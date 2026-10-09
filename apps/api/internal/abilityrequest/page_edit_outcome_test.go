package abilityrequest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

var editSnapshot = strings.Repeat("ab", 32)

// TestPageEditAppliedRecordsTheSnapshotHash: an applied page edit records
// the hash its answer carries and opens the undo; the same holds for the
// stored result a replay or a ledger answer carries after a lost reply.
func TestPageEditAppliedRecordsTheSnapshotHash(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	stored := json.RawMessage(`{"ok":true,"outcome":"applied","mode":"write","ability":"wpmgr/page-edit","post_id":120,` +
		`"before_fp":"x","after_fp":"y","snapshot_sha256":"` + editSnapshot + `","changes_applied":2}`)
	cases := map[string]writeOutcome{
		"direct": classifyWrite(mcp.AbilityPageEdit, agentcmd.AbilityRunResponse{
			OK: true, Outcome: "applied", Mode: "write", PostID: 120, SnapshotSHA256: editSnapshot,
		}, nil, now),
		"replay": classifyWrite(mcp.AbilityPageEdit, agentcmd.AbilityRunResponse{
			OK: true, Outcome: "already_applied", Result: stored,
		}, nil, now),
	}
	ledger, done := ledgerVerdict(mcp.AbilityPageEdit, agentcmd.AbilityRunResponse{Found: true, Result: stored}, time.Minute, now)
	if !done {
		t.Fatal("a ledger row with a stored result decides the request")
	}
	cases["ledger"] = ledger
	for name, oc := range cases {
		if oc.outcome != OutcomeApplied || oc.snapshotSHA256 == nil || *oc.snapshotSHA256 != editSnapshot ||
			!oc.undoUntil.Valid || oc.snapshotHashMissing || oc.createdPostID != nil {
			t.Errorf("%s: %+v", name, oc)
		}
	}
}

// TestPageEditAppliedWithoutAHashOffersNoUndo: an applied answer with no
// hash, or one that is not 64 lowercase hex characters, is recorded applied
// with no undo and flagged for the audit row; no hash is made up.
func TestPageEditAppliedWithoutAHashOffersNoUndo(t *testing.T) {
	now := time.Now()
	for _, hash := range []string{"", "ABAB", strings.Repeat("AB", 32), strings.Repeat("a", 63), strings.Repeat("a", 65)} {
		oc := classifyWrite(mcp.AbilityPageEdit, agentcmd.AbilityRunResponse{OK: true, Outcome: "applied", SnapshotSHA256: hash}, nil, now)
		if oc.outcome != OutcomeApplied || oc.snapshotSHA256 != nil || oc.undoUntil.Valid || !oc.snapshotHashMissing {
			t.Errorf("hash %q: %+v", hash, oc)
		}
		stored := json.RawMessage(`{"ok":true,"outcome":"applied","snapshot_sha256":"` + hash + `"}`)
		got, ok := outcomeFromStored(mcp.AbilityPageEdit, stored, now)
		if !ok || got.outcome != OutcomeApplied || got.snapshotSHA256 != nil || got.undoUntil.Valid || !got.snapshotHashMissing {
			t.Errorf("stored hash %q: %+v", hash, got)
		}
	}
}

// TestPageEditFailureRecordsNoCreatedPost: a failed page edit names its own
// post, which is the request's target; it is never recorded as a post the
// write created or trashed, and the put-back report is kept.
func TestPageEditFailureRecordsNoCreatedPost(t *testing.T) {
	now := time.Now()
	restored := true
	for _, code := range []string{"verify_mismatch", "snapshot_failed", "builder_crashed", "restore_mismatch"} {
		oc := classifyWrite(mcp.AbilityPageEdit, agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{
			Code: code, PostID: 120, Restored: &restored,
		}, now)
		if oc.createdPostID != nil || oc.trashed != nil || oc.code == nil || *oc.code != code {
			t.Errorf("%s: %+v", code, oc)
		}
		if oc.restored == nil || !*oc.restored {
			t.Errorf("%s: the put-back report is lost: %+v", code, oc)
		}
		stored, ok := outcomeFromStored(mcp.AbilityPageEdit,
			json.RawMessage(`{"ok":false,"outcome":"refused","code":"`+code+`","post_id":120,"restored":true}`), now)
		if !ok || stored.createdPostID != nil || stored.trashed != nil {
			t.Errorf("stored %s: %+v", code, stored)
		}
	}
	// The control: a page-create's failed write still names the draft it made.
	vm := classifyWrite(mcp.AbilityPageCreate, agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{
		Code: "verify_mismatch", PostID: 9, Trashed: true,
	}, now)
	if vm.createdPostID == nil || *vm.createdPostID != 9 {
		t.Fatalf("page-create verify_mismatch: %+v", vm)
	}
}

// TestAppliedIsAPageEditAnswerOnly: only wpmgr/page-edit answers "applied";
// for any other ability it is not a definite answer.
func TestAppliedIsAPageEditAnswerOnly(t *testing.T) {
	for _, ability := range []string{mcp.AbilityPageCreate, mcp.AbilityRestWrite} {
		oc := classifyWrite(ability, agentcmd.AbilityRunResponse{OK: true, Outcome: "applied", SnapshotSHA256: editSnapshot}, nil, time.Now())
		if oc.outcome != "" {
			t.Errorf("%s: %+v", ability, oc)
		}
	}
}
