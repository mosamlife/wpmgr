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

// redirectErr is what agentcmd returns when the site redirects a command, with
// a To whose path a sloppy text match could mistake for a 404.
func redirectErr(cmd string) error {
	return fmt.Errorf("wrapped: %w", &agentcmd.RedirectError{
		Command: cmd,
		Status:  301,
		From:    "https://example.com/wp-json/wpmgr/v1/command/" + cmd,
		To:      "https://www.example.com/status-404/rejected-by-agent",
	})
}

// TestIsOldAgentRouteMissing_FalseForRedirectError: a redirect is a wrong
// saved address and is never read as an agent without the route.
func TestIsOldAgentRouteMissing_FalseForRedirectError(t *testing.T) {
	if isOldAgentRouteMissing(redirectErr("refresh_inventory")) {
		t.Fatal("isOldAgentRouteMissing(RedirectError) = true, want false")
	}
	// Positive control: the canonical old-agent error still matches.
	if !isOldAgentRouteMissing(errors.New("refresh_inventory command rejected by agent: status 404 body={}")) {
		t.Fatal("positive control failed: the canonical 404 no longer matches")
	}
}

// TestRefreshInventoryWorker_RedirectCancels: a redirect cancels the job rather
// than being retried (every retry is refused the same way) or recorded as an
// old agent's soft success.
func TestRefreshInventoryWorker_RedirectCancels(t *testing.T) {
	cmd := &fakeRefreshCmd{err: redirectErr("refresh_inventory")}
	w := NewRefreshInventoryWorker(cmd, nil, nil)
	err := w.Work(context.Background(), &river.Job[RefreshInventoryArgs]{
		Args: RefreshInventoryArgs{TenantID: uuid.New(), SiteID: uuid.New(), SiteURL: "https://example.com"},
	})
	var cancelErr *river.JobCancelError
	if !errors.As(err, &cancelErr) {
		t.Fatalf("Work() returned %T (%v), want *river.JobCancelError", err, err)
	}
	if _, ok := agentcmd.AsRedirect(err); !ok {
		t.Errorf("cancel error %v no longer carries the RedirectError", err)
	}
}
