package abilityrequest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// GH #826: a recovery undo is offered, and accepted, only when the site's
// recorded agent version ships the recovery revert. An older agent answers
// not_revertible and the undo would fail for good.
func TestUndoKindFor_RecoveryNeedsAgentVersion(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	post := int64(42)
	failed := OutcomeFailed
	avail := "available"
	recovery := sqlc.AssistantAbilityRequest{State: "failed", Outcome: &failed, CreatedPostID: &post, OutcomeAt: tsAt(now.Add(-time.Hour))}
	done := sqlc.AssistantAbilityRequest{State: "done", UndoState: &avail, UndoAvailableUntil: tsAt(now.Add(time.Hour))}

	for _, c := range []struct {
		version string
		want    undoKind
	}{
		{"", undoKindNone},
		{"0.61.155", undoKindNone},
		{"0.61.156", undoKindNone},
		{agentcmd.MinAgentVersionForRecoveryUndo, undoKindRecovery},
		{"0.61.158", undoKindRecovery},
		{"0.62.0", undoKindRecovery},
	} {
		if got := undoKindFor(recovery, c.version, now); got != c.want {
			t.Errorf("recovery row, agent %q: got %d, want %d", c.version, got, c.want)
		}
		if UndoOffered(recovery, c.version, now) != (c.want != undoKindNone) {
			t.Errorf("recovery row, agent %q: undo_offered disagrees", c.version)
		}
	}
	// The normal undo of a done row is not gated on the recovery floor.
	for _, v := range []string{"", "0.61.156", "0.61.157"} {
		if got := undoKindFor(done, v, now); got != undoKindDone {
			t.Errorf("done row, agent %q: got %d, want the normal undo", v, got)
		}
	}
}

// GH #824: an answer a resend cannot change finishes the undo as failed
// instead of releasing it for a retry that can never succeed.
func TestUndoRetryable_PermanentErrors(t *testing.T) {
	final := map[string]error{
		"403 token rejected": &agentcmd.CommandError{Command: agentcmd.CmdAbilityRun, Status: 403},
		"401":                &agentcmd.CommandError{Command: agentcmd.CmdAbilityRun, Status: 401},
		"404 plugin gone":    &agentcmd.CommandError{Command: agentcmd.CmdAbilityRun, Status: 404},
		"400":                &agentcmd.CommandError{Command: agentcmd.CmdAbilityRun, Status: 400},
		"decode error":       fmt.Errorf("decode ability_run response: %w", agentcmd.ErrAbilityRunMalformed),
	}
	for name, err := range final {
		if undoRetryable(err) {
			t.Errorf("%s: retryable, so the undo is released and fails the same way on every press", name)
		}
		if got := undoResultFor(agentcmd.AbilityRunResponse{}, err); got != UndoFailed {
			t.Errorf("%s: result %q, want %q", name, got, UndoFailed)
		}
	}
	retry := map[string]error{
		"500":         &agentcmd.CommandError{Command: agentcmd.CmdAbilityRun, Status: 500},
		"502 proxy":   &agentcmd.CommandError{Command: agentcmd.CmdAbilityRun, Status: 502},
		"503":         &agentcmd.CommandError{Command: agentcmd.CmdAbilityRun, Status: 503},
		"408":         &agentcmd.CommandError{Command: agentcmd.CmdAbilityRun, Status: 408},
		"425":         &agentcmd.CommandError{Command: agentcmd.CmdAbilityRun, Status: 425},
		"429":         &agentcmd.CommandError{Command: agentcmd.CmdAbilityRun, Status: 429},
		"transport":   errors.New("ability_run command transport: connection reset"),
		"timeout":     context.DeadlineExceeded,
		"in flight":   &agentcmd.AbilityRunRefusal{Code: "request_in_flight"},
		"read failed": errors.New("read ability_run response: unexpected EOF"),
	}
	for name, err := range retry {
		if !undoRetryable(err) {
			t.Errorf("%s: not retryable, so one blip closes undo for good", name)
		}
	}
}

// GH #826: snapshot_failed after the insert names the draft; it is recorded
// so the card shows it and, when the site did not trash it, offers the
// recovery undo.
func TestRefusedOutcome_SnapshotFailedKeepsTheDraft(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	oc := classifyWrite("", agentcmd.AbilityRunResponse{},
		&agentcmd.AbilityRunRefusal{Code: "snapshot_failed", PostID: 42, Trashed: false}, now)
	if oc.outcome != OutcomeRefused || oc.code == nil || *oc.code != "snapshot_failed" {
		t.Fatalf("outcome = %q code = %v", oc.outcome, oc.code)
	}
	if oc.createdPostID == nil || *oc.createdPostID != 42 {
		t.Fatalf("created post id = %v, want 42", oc.createdPostID)
	}
	if oc.trashed == nil || *oc.trashed {
		t.Fatalf("trashed = %v, want false", oc.trashed)
	}

	// The row this records offers the recovery undo.
	failed := OutcomeRefused
	row := sqlc.AssistantAbilityRequest{State: "failed", Outcome: &failed, CreatedPostID: oc.createdPostID,
		Trashed: oc.trashed, OutcomeAt: tsAt(now)}
	if undoKindFor(row, agentcmd.MinAgentVersionForRecoveryUndo, now) != undoKindRecovery {
		t.Fatal("an untrashed snapshot_failed draft offers no recovery undo")
	}

	trashed := refusedOutcome("snapshot_failed", "", 43, true)
	if trashed.createdPostID == nil || *trashed.createdPostID != 43 || trashed.trashed == nil || !*trashed.trashed {
		t.Fatalf("trashed draft: post %v trashed %v", trashed.createdPostID, trashed.trashed)
	}

	// Before the insert the site created nothing and names no post.
	none := refusedOutcome("snapshot_failed", "", 0, false)
	if none.createdPostID != nil || none.trashed != nil {
		t.Fatalf("pre-insert snapshot_failed recorded post %v trashed %v", none.createdPostID, none.trashed)
	}

	// The same answer stored in the ledger (resolved later) is read the same way.
	stored, ok := outcomeFromStored("", []byte(`{"ok":false,"code":"snapshot_failed","post_id":44,"trashed":false}`), now)
	if !ok || stored.createdPostID == nil || *stored.createdPostID != 44 {
		t.Fatalf("stored snapshot_failed: ok=%v post=%v", ok, stored.createdPostID)
	}
}
