package update

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// refreshCommandError is the typed error agentcmd returns for an HTTP 500 from
// the refresh_inventory command, with the given body code.
func refreshCommandError(code string) error {
	return fmt.Errorf("wrapped: %w", &agentcmd.CommandError{
		Command:     "refresh_inventory",
		Status:      500,
		Code:        code,
		Message:     "Command execution failed: RuntimeException: transient unavailable",
		DataCommand: "refresh_inventory",
		Exception:   "RuntimeException",
		At:          "includes/commands/class-refresh-command.php:12",
		DataStatus:  500,
	})
}

func runRefresh(t *testing.T, err error) (error, *fakeRefreshCmd) {
	t.Helper()
	cmd := &fakeRefreshCmd{err: err}
	w := NewRefreshInventoryWorker(cmd, nil, nil)
	return w.Work(context.Background(), &river.Job[RefreshInventoryArgs]{
		Args: RefreshInventoryArgs{TenantID: uuid.New(), SiteID: uuid.New(), SiteURL: "https://example.com"},
	}), cmd
}

// TestRefreshInventoryWorker_AgentFailedCancels (GH #791): an agent-reported
// command failure cancels the job, keeping the typed error, instead of
// returning it for River to retry.
func TestRefreshInventoryWorker_AgentFailedCancels(t *testing.T) {
	err, cmd := runRefresh(t, refreshCommandError("wpmgr_command_failed"))
	var cancelErr *river.JobCancelError
	if !errors.As(err, &cancelErr) {
		t.Fatalf("Work() returned %T (%v), want *river.JobCancelError", err, err)
	}
	if ce, ok := agentcmd.AsCommandError(err); !ok || !ce.AgentFailed() {
		t.Errorf("cancel error %v no longer carries the agent's CommandError", err)
	}
	if cmd.calls != 1 {
		t.Errorf("refresh command sent %d times, want 1", cmd.calls)
	}
}

// TestRefreshInventoryWorker_Other500Retries is the control: a 500 that is not
// the agent's own failure shape stays retryable (a plain error, not a cancel).
func TestRefreshInventoryWorker_Other500Retries(t *testing.T) {
	err, _ := runRefresh(t, refreshCommandError("internal_server_error"))
	if err == nil {
		t.Fatal("Work() = nil; a 500 that is not an agent failure must be retried")
	}
	var cancelErr *river.JobCancelError
	if errors.As(err, &cancelErr) {
		t.Fatalf("Work() cancelled on %v; only an agent failure is terminal", err)
	}
}
