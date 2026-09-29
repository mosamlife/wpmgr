package update

// worker_redirect_test.go — GH #755 F4: runDry and runApply each answer a
// command-address redirect from the agent the same way (see worker.go's
// runDry/runApply, both around the `agentcmd.AsRedirect(err)` check just
// after the w.cmd.Update call): the task is finished terminal (TaskFailed),
// never retried, with the operator message naming the redirect target. Before
// this file, deleting either check left the package green — the Update call
// erroring on a redirect took the generic "command failed" path instead,
// which finish()es the task with a DIFFERENT detail than the redirect one, so
// the tests below catch the regression on the exact wording, not just on the
// terminal status.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// redirectUpdateCommander answers every Update call with the command-address
// redirect RedirectError, wired exactly like agentcmd would report it: a
// "www." toggle on the saved host, which is the siteaddr.Adopt shape
// OperatorMessage renders with the "updates ... automatically" explanation
// (mirrors internal/backup/worker_redirect_test.go's redirectCommander).
type redirectUpdateCommander struct{ calls int }

func (c *redirectUpdateCommander) Update(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.UpdateRequest) (agentcmd.UpdateResponse, error) {
	c.calls++
	return agentcmd.UpdateResponse{}, &agentcmd.RedirectError{
		Command:          "update",
		Status:           301,
		From:             "https://example.com/wp-json/wpmgr/v1/command/update",
		To:               "https://www.example.com/wp-json/wpmgr/v1/command/update",
		SuggestedSiteURL: "https://www.example.com",
	}
}

func (c *redirectUpdateCommander) Rollback(context.Context, uuid.UUID, string, agentcmd.RollbackRequest) (agentcmd.RollbackResponse, error) {
	panic("rollback must never be reached on a redirected command")
}

// TestRunDry_RedirectFailsTerminalWithTargetNamed: a site that redirects its
// command address fails the dry-run task terminally on the first attempt
// (never a retry/snooze), naming the redirect target and the "Dry run"
// action word the way runApply's own redirect branch names "Update".
func TestRunDry_RedirectFailsTerminalWithTargetNamed(t *testing.T) {
	repo := &probeFakeRepo{}
	cmd := &redirectUpdateCommander{}
	w := newApplyTestWorker(repo, cmd, &scriptedProber{})

	task := testTask()
	item := updateItem()
	if err := w.runDry(context.Background(), task, "https://example.com", item); err != nil {
		t.Fatalf("runDry() returned %v; a redirect must be a terminal failure, not a retry", err)
	}
	if cmd.calls != 1 {
		t.Errorf("update command sent %d times, want 1", cmd.calls)
	}
	if len(repo.finished) != 1 {
		t.Fatalf("expected exactly one terminal finish, got %d: %+v", len(repo.finished), repo.finished)
	}
	got := repo.finished[0]
	if got.Status != TaskFailed {
		t.Errorf("status = %q, want %q", got.Status, TaskFailed)
	}
	if !strings.HasPrefix(got.Detail, "Dry run not started.") {
		t.Errorf("detail = %q, want prefix %q", got.Detail, "Dry run not started.")
	}
	if !strings.Contains(got.Detail, "https://www.example.com") {
		t.Errorf("detail %q does not name the redirect target", got.Detail)
	}
	if got.Error == "" {
		t.Error("expected the underlying redirect error text to be recorded, got empty Error")
	}
}

// TestRunApply_RedirectFailsTerminalWithTargetNamed is runApply's mirror of
// TestRunDry_RedirectFailsTerminalWithTargetNamed above.
func TestRunApply_RedirectFailsTerminalWithTargetNamed(t *testing.T) {
	repo := &probeFakeRepo{}
	cmd := &redirectUpdateCommander{}
	w := newApplyTestWorker(repo, cmd, &scriptedProber{})

	task := testTask()
	item := updateItem()
	if err := w.runApply(context.Background(), task, "https://example.com", item); err != nil {
		t.Fatalf("runApply() returned %v; a redirect must be a terminal failure, not a retry", err)
	}
	if cmd.calls != 1 {
		t.Errorf("update command sent %d times, want 1", cmd.calls)
	}
	if len(repo.finished) != 1 {
		t.Fatalf("expected exactly one terminal finish, got %d: %+v", len(repo.finished), repo.finished)
	}
	got := repo.finished[0]
	if got.Status != TaskFailed {
		t.Errorf("status = %q, want %q", got.Status, TaskFailed)
	}
	if !strings.HasPrefix(got.Detail, "Update not started.") {
		t.Errorf("detail = %q, want prefix %q", got.Detail, "Update not started.")
	}
	if !strings.Contains(got.Detail, "https://www.example.com") {
		t.Errorf("detail %q does not name the redirect target", got.Detail)
	}
	if got.Error == "" {
		t.Error("expected the underlying redirect error text to be recorded, got empty Error")
	}
}
