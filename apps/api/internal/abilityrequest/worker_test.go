package abilityrequest

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/org"
)

func TestLifecycleLockKeyMatchesOrg(t *testing.T) {
	if lifecycleLockKey != org.LifecycleLockKey {
		t.Fatalf("lifecycleLockKey %q != org.LifecycleLockKey %q", lifecycleLockKey, org.LifecycleLockKey)
	}
}

func TestClassifyWrite(t *testing.T) {
	now := time.Now()
	oc := classifyWrite(agentcmd.AbilityRunResponse{OK: true, Outcome: "created", PostID: 7}, nil, now)
	if oc.outcome != OutcomeCreated || oc.createdPostID == nil || *oc.createdPostID != 7 || !oc.undoUntil.Valid {
		t.Fatalf("created: %+v", oc)
	}
	if oc := classifyWrite(agentcmd.AbilityRunResponse{OK: true, Outcome: "created"}, nil, now); oc.outcome != OutcomeFailed {
		t.Fatalf("created without a post id must not be recorded as created: %+v", oc)
	}
	vm := classifyWrite(agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{Code: "verify_mismatch", PostID: 9, Trashed: true}, now)
	if vm.outcome != OutcomeVerifyMismatch || vm.trashed == nil || !*vm.trashed || *vm.createdPostID != 9 {
		t.Fatalf("verify_mismatch: %+v", vm)
	}
	if oc := classifyWrite(agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{Code: "preview_changed"}, now); oc.outcome != OutcomeRefused {
		t.Fatalf("refusal: %+v", oc)
	}
	if oc := classifyWrite(agentcmd.AbilityRunResponse{}, fmt.Errorf("x: %w", agentcmd.ErrCommandNotSent), now); oc.outcome != OutcomeNotSent {
		t.Fatalf("not sent: %+v", oc)
	}
	// Anything else is unknown: never resent, resolved by the ledger.
	for _, err := range []error{errors.New("timeout"), &agentcmd.AbilityRunRefusal{Code: "request_in_flight"}} {
		if oc := classifyWrite(agentcmd.AbilityRunResponse{}, err, now); oc.outcome != "" {
			t.Fatalf("%v must be unknown: %+v", err, oc)
		}
	}
	stored, _ := json.Marshal(map[string]any{"ok": true, "outcome": "created", "post_id": 5})
	if oc := classifyWrite(agentcmd.AbilityRunResponse{OK: true, Outcome: "already_applied", Result: stored}, nil, now); oc.outcome != OutcomeCreated || *oc.createdPostID != 5 {
		t.Fatalf("already_applied: %+v", oc)
	}
}

func TestLedgerVerdict(t *testing.T) {
	now := time.Now()
	if _, done := ledgerVerdict(agentcmd.AbilityRunResponse{Inflight: true, Found: true}, time.Hour, now); done {
		t.Fatal("an in-flight write was decided")
	}
	if _, done := ledgerVerdict(agentcmd.AbilityRunResponse{}, 30*time.Second, now); done {
		t.Fatal("not found inside the token window was decided")
	}
	if oc, done := ledgerVerdict(agentcmd.AbilityRunResponse{}, 3*time.Minute, now); !done || oc.outcome != OutcomeFailed {
		t.Fatalf("not found after the token window: %+v", oc)
	}
	stored, _ := json.Marshal(map[string]any{"ok": true, "outcome": "created", "post_id": 11})
	if oc, done := ledgerVerdict(agentcmd.AbilityRunResponse{Found: true, Result: stored}, time.Minute, now); !done || oc.outcome != OutcomeCreated {
		t.Fatalf("found created: %+v", oc)
	}
	id := int64(12)
	if oc, done := ledgerVerdict(agentcmd.AbilityRunResponse{Found: true, CreatedPostID: &id}, time.Minute, now); !done || oc.outcome != OutcomeFailed || *oc.createdPostID != 12 {
		t.Fatalf("interrupted: %+v", oc)
	}
}

func TestUndoResultFor(t *testing.T) {
	cases := map[string]string{"created_post_published": UndoRefusedPublished, "conflict": UndoRefusedConflict, "created_post_touched": UndoRefusedConflict, "revert_failed": UndoFailed}
	for code, want := range cases {
		if got := undoResultFor(agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{Code: code}); got != want {
			t.Errorf("%s: got %s want %s", code, got, want)
		}
	}
	if undoResultFor(agentcmd.AbilityRunResponse{Outcome: "reverted"}, nil) != UndoDone {
		t.Fatal("reverted is not undone")
	}
}
