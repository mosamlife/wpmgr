package update

// agent_worker_redirect_test.go — GH #755 F4: a site that redirects its
// command address for agent_self_update is recorded TaskSkipped with the
// operator message naming the redirect target (see agent_worker.go's
// runAgentSelfUpdate, the `agentcmd.AsRedirect(err)` check right after the
// beat-1 AgentSelfUpdate arm call). Before this file, deleting that check
// left the update package green: the arm error fell through to the
// old-agent/timeout handling below it, which records a DIFFERENT terminal
// status/detail for a DIFFERENT reason, so this test catches the regression
// on the exact status and wording rather than just on "the task finished".

import (
	"context"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// TestRunAgentSelfUpdate_RedirectIsSkippedNotFailed: a site that redirects its
// command address for agent_self_update is recorded TaskSkipped (never
// TaskFailed, never retried), naming the redirect target, exactly like a
// redirect on any other command channel.
func TestRunAgentSelfUpdate_RedirectIsSkippedNotFailed(t *testing.T) {
	cmd := &recordingSelfUpdater{err: &agentcmd.RedirectError{
		Command:          "agent_self_update",
		Status:           301,
		From:             "https://example.com/wp-json/wpmgr/v1/command/agent_self_update",
		To:               "https://www.example.com/wp-json/wpmgr/v1/command/agent_self_update",
		SuggestedSiteURL: "https://www.example.com",
	}}
	h := newAgentHarness(t, 10, cmd, &fakeVersions{}, true)

	if err := h.work(context.Background(), 0); err != nil {
		t.Fatalf("Work: %v", err)
	}

	task := h.store.Task(0)
	if task.Status == TaskFailed {
		t.Fatalf("a redirected command address is not a site failure: %+v", task)
	}
	if task.Status != TaskSkipped {
		t.Fatalf("status = %q, want %q (neither confirmed nor failed)", task.Status, TaskSkipped)
	}
	if !strings.HasPrefix(task.Detail, "Agent self-update not started.") {
		t.Errorf("detail = %q, want prefix %q", task.Detail, "Agent self-update not started.")
	}
	if !strings.Contains(task.Detail, "https://www.example.com") {
		t.Errorf("detail %q does not name the redirect target", task.Detail)
	}
	if len(cmd.calls) != 1 {
		t.Errorf("agent_self_update command sent %d times, want 1", len(cmd.calls))
	}
}
