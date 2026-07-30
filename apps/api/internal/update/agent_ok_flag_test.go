package update

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// This file covers one class of control-plane blind spot: an agent command that
// returns HTTP 200 (so the transport error is nil) with a body whose ok field is
// false, meaning the agent REFUSED the command. A caller that branches only on
// the transport error records such a refusal as a success. That is how a
// refresh_inventory broken since it shipped stayed invisible in production, and
// the same shape existed on the update worker's dry-run and rollback paths.

// scriptedUpdateCommander is a Commander returning a fixed Update/Rollback
// response pair, so runDry and rollback can be driven with an agent that answers
// 200 while refusing the command.
type scriptedUpdateCommander struct {
	updateResp   agentcmd.UpdateResponse
	rollbackResp agentcmd.RollbackResponse
	rollbackHit  bool
}

func (c *scriptedUpdateCommander) Update(context.Context, uuid.UUID, string, agentcmd.UpdateRequest) (agentcmd.UpdateResponse, error) {
	return c.updateResp, nil
}

func (c *scriptedUpdateCommander) Rollback(context.Context, uuid.UUID, string, agentcmd.RollbackRequest) (agentcmd.RollbackResponse, error) {
	c.rollbackHit = true
	return c.rollbackResp, nil
}

func newScriptedWorker(repo *probeFakeRepo, cmd *scriptedUpdateCommander) *Worker {
	w := NewWorker(repo, nil, cmd, &scriptedProber{}, nil, nil, nil, 5, 0)
	w.SetProbeRetryDelays([]time.Duration{time.Millisecond})
	return w
}

// TestRunDry_AgentOkFalseIsNotSuccess proves a dry run the agent refused is not
// recorded as a succeeded task. Before the fix runDry never inspected resp.OK
// and hardcoded status = TaskSucceeded, so this landed as "no change".
func TestRunDry_AgentOkFalseIsNotSuccess(t *testing.T) {
	repo := &probeFakeRepo{}
	cmd := &scriptedUpdateCommander{updateResp: agentcmd.UpdateResponse{OK: false}}
	w := newScriptedWorker(repo, cmd)

	if err := w.runDry(context.Background(), testTask(), "https://example.test", updateItem()); err != nil {
		t.Fatalf("runDry: %v", err)
	}
	if len(repo.finished) != 1 {
		t.Fatalf("expected exactly one finish, got %+v", repo.finished)
	}
	if got := repo.finished[0].Status; got != TaskFailed {
		t.Fatalf("a dry run the agent refused must be TaskFailed, got %q with detail %q",
			got, repo.finished[0].Detail)
	}
}

// TestRunDry_AgentReturnedNoItemResultIsNotSuccess proves the OTHER half of the
// same hole: firstResult synthesises an ItemFailed when the agent sends back an
// empty results array, and runDry ignored that status entirely.
func TestRunDry_AgentReturnedNoItemResultIsNotSuccess(t *testing.T) {
	repo := &probeFakeRepo{}
	// ok=true but no per-item result: the agent acked yet reported nothing.
	cmd := &scriptedUpdateCommander{updateResp: agentcmd.UpdateResponse{OK: true}}
	w := newScriptedWorker(repo, cmd)

	if err := w.runDry(context.Background(), testTask(), "https://example.test", updateItem()); err != nil {
		t.Fatalf("runDry: %v", err)
	}
	if len(repo.finished) != 1 {
		t.Fatalf("expected exactly one finish, got %+v", repo.finished)
	}
	if got := repo.finished[0].Status; got != TaskFailed {
		t.Fatalf("a dry run with no item result must be TaskFailed, got %q with detail %q",
			got, repo.finished[0].Detail)
	}
}

// TestRunDry_HappyPathStillSucceeds guards the fix against over-reach: a normal
// dry run must still be recorded as succeeded.
func TestRunDry_HappyPathStillSucceeds(t *testing.T) {
	repo := &probeFakeRepo{}
	cmd := &scriptedUpdateCommander{updateResp: agentcmd.UpdateResponse{
		OK: true,
		Results: []agentcmd.ItemResult{{
			Type: TargetPlugin, Slug: "suremail",
			FromVersion: "1.9.9", ToVersion: "2.0.0",
			Status: agentcmd.ItemWouldUpdate,
		}},
	}}
	w := newScriptedWorker(repo, cmd)

	if err := w.runDry(context.Background(), testTask(), "https://example.test", updateItem()); err != nil {
		t.Fatalf("runDry: %v", err)
	}
	if len(repo.finished) != 1 || repo.finished[0].Status != TaskSucceeded {
		t.Fatalf("a healthy dry run must still succeed, got %+v", repo.finished)
	}
	if !strings.Contains(repo.finished[0].Detail, "would update") {
		t.Fatalf("expected the would-update detail, got %q", repo.finished[0].Detail)
	}
}

// TestRollback_AgentOkFalseIsNotRecordedAsRolledBack is the most consequential
// case of the class. When the agent answers 200 with ok=false it REFUSED the
// rollback: nothing was restored and the site is still broken. The transport
// error is nil, so before the fix the task was recorded TaskRolledBack, telling
// the operator the site had been recovered when it had not.
func TestRollback_AgentOkFalseIsNotRecordedAsRolledBack(t *testing.T) {
	repo := &probeFakeRepo{}
	cmd := &scriptedUpdateCommander{rollbackResp: agentcmd.RollbackResponse{
		OK:  false,
		Log: "snapshot snap-1 is missing",
	}}
	w := newScriptedWorker(repo, cmd)

	task := testTask()
	item := updateItem()
	res := agentcmd.ItemResult{
		Type: item.Type, Slug: item.Slug,
		FromVersion: "1.9.9", ToVersion: "2.0.0",
		Status: agentcmd.ItemSucceeded, SnapshotID: "snap-1",
	}

	err := w.rollback(context.Background(), task, "https://example.test", item, res,
		agentcmd.ProbeResult{}, false, "post-update health failed")
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !cmd.rollbackHit {
		t.Fatal("expected the rollback command to be dispatched")
	}
	if len(repo.finished) != 1 {
		t.Fatalf("expected exactly one finish, got %+v", repo.finished)
	}
	got := repo.finished[0]
	if got.Status == TaskRolledBack {
		t.Fatal("a rollback the agent REFUSED must never be recorded as TaskRolledBack; the site was not restored")
	}
	if got.Status != TaskFailed {
		t.Fatalf("expected TaskFailed, got %q", got.Status)
	}
	if !strings.Contains(got.Error, "snapshot snap-1 is missing") {
		t.Fatalf("the agent's own rollback log must be preserved, got %q", got.Error)
	}
}

// TestRollback_HappyPathStillRecordsRolledBack guards the fix against over-reach.
func TestRollback_HappyPathStillRecordsRolledBack(t *testing.T) {
	repo := &probeFakeRepo{}
	cmd := &scriptedUpdateCommander{rollbackResp: agentcmd.RollbackResponse{
		OK: true, RestoredVersion: "1.9.9",
	}}
	w := newScriptedWorker(repo, cmd)

	res := agentcmd.ItemResult{FromVersion: "1.9.9", ToVersion: "2.0.0", SnapshotID: "snap-1"}
	if err := w.rollback(context.Background(), testTask(), "https://example.test", updateItem(), res,
		agentcmd.ProbeResult{}, false, "post-update health failed"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if len(repo.finished) != 1 || repo.finished[0].Status != TaskRolledBack {
		t.Fatalf("a rollback the agent accepted must still record TaskRolledBack, got %+v", repo.finished)
	}
}
