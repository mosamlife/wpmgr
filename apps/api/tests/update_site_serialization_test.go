package tests

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/update"
)

// GH #328 PIECE 2 (serialisation), end to end: the control plane's per-site
// gate (SiteHasRunningUpdateTask / DeferUpdateTaskToPending, INVARIANT R) and
// the agent's own site-update lock (simulated here by fakeAgent.
// enableSiteLock) together keep concurrent applies against ONE site's
// wp-content/upgrade/ down to exactly one at a time, and a busy refusal is
// retried to completion rather than ever becoming a permanent TaskFailed or
// a deadlock.
//
// No test here may assert that the CP-side gate ALONE prevents a collision:
// it is deliberately best-effort (see SiteHasRunningUpdateTask's own comment
// in db/query/updates.sql). The mutual-exclusion guarantee is the agent's
// lock, simulated by fakeAgent's internal mutex; concurrentPeak is measured
// against what the fake AGENT observed, never against how many commands the
// control plane sent.

// seedPendingPluginsBulk records N distinct pending plugin advisories on one
// site in a SINGLE ApplyMetadata call (site.Service.ApplyMetadata replaces
// the whole plugins list per call, so seeding them one at a time via
// seedPendingPlugin would each overwrite the last) and returns the matching
// update.Item slice ready for CreateRunInput.Items.
func seedPendingPluginsBulk(t *testing.T, h *updateTestHarness, tenant uuid.UUID, s site.Site, n int) []update.Item {
	t.Helper()
	comps := make([]site.Component, 0, n)
	items := make([]update.Item, 0, n)
	for i := 0; i < n; i++ {
		slug := fmt.Sprintf("plugin-%02d", i)
		comps = append(comps, site.Component{
			Slug: slug, Version: "1.0.0",
			AvailableUpdate: &site.AvailableUpdate{NewVersion: "1.1.0"},
		})
		items = append(items, update.Item{Type: "plugin", Slug: slug, Version: "latest"})
	}
	if _, err := h.siteSvc.ApplyMetadata(context.Background(), tenant, s.ID, site.Metadata{Plugins: comps}); err != nil {
		t.Fatalf("seed pending plugins bulk: %v", err)
	}
	return items
}

// waitRunCompletedWithin is waitRunCompleted with a caller-supplied deadline,
// for tests whose serialised workload legitimately takes longer than the
// package default (25s).
func waitRunCompletedWithin(t *testing.T, h *updateTestHarness, tenant, runID uuid.UUID, timeout time.Duration) (update.Run, []update.Task) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	for {
		run, tasks, err := h.svc.GetRun(ctx, tenant, runID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status == update.RunCompleted {
			return run, tasks
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not complete within %s (status=%s tasks=%+v)", timeout, run.Status, tasks)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestSingleSiteBulkOfThirtyDoesNotDeadlockAndSerializes is the required
// 30-plugin single-site bulk case: it must complete (no deadlock) and the
// agent must never observe more than one update in flight against the site
// at once.
func TestSingleSiteBulkOfThirtyDoesNotDeadlockAndSerializes(t *testing.T) {
	const n = 30

	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "upd-bulk30")
	fa := newFakeAgent(t)
	fa.setHomepage(http.StatusOK, "<html>ok</html>")
	// A short, real hold so the test proves actual overlap-avoidance rather
	// than passing by luck: with river's per-tenant-shard MaxWorkers=3 (see
	// startUpdateRiver), the first wave WILL send up to 3 concurrent apply
	// commands to this one site; the simulated lock is what turns that into
	// "one wins, the rest are refused busy" rather than three concurrent
	// writers.
	fa.enableSiteLock(20 * time.Millisecond)

	h := buildHarness(t, pool, newTestCommander(t), newTestProber(t))
	// Tiny, deterministic backoff so this test does not spend the production
	// 5s-60s jittered window 29 times over.
	h.worker.SetSiteBusyBackoff(func(update.Task) time.Duration { return 15 * time.Millisecond })

	s := enrollFakeSite(t, pool, tenant, fa.url())
	fa.setExpectAud(s.ID.String())
	items := seedPendingPluginsBulk(t, h, tenant, s, n)

	run, tasks, err := h.svc.CreateRun(context.Background(), update.CreateRunInput{
		TenantID: tenant,
		SiteIDs:  []uuid.UUID{s.ID},
		Items:    items,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if len(tasks) != n {
		t.Fatalf("want %d tasks, got %d", n, len(tasks))
	}

	// Generous bound: n serialised holds at 20ms is 600ms of pure "work", plus
	// per-task DB/River overhead and (deliberately, per the design) some
	// wasted first-wave collisions. 60s comfortably absorbs CI jitter while
	// still catching an actual deadlock (which would otherwise hang forever).
	finalRun, finalTasks := waitRunCompletedWithin(t, h, tenant, run.ID, 60*time.Second)
	if finalRun.Status != update.RunCompleted {
		t.Fatalf("run status = %s, want completed", finalRun.Status)
	}
	if len(finalTasks) != n {
		t.Fatalf("want %d final tasks, got %d", n, len(finalTasks))
	}
	for _, tk := range finalTasks {
		if tk.Status != update.TaskSucceeded {
			t.Fatalf("task %s (%s) status = %s, want succeeded (detail=%s err=%s)",
				tk.ID, tk.TargetSlug, tk.Status, tk.Detail, tk.Error)
		}
	}

	peak := atomic.LoadInt32(&fa.concurrentPeak)
	if peak != 1 {
		t.Fatalf("agent observed peak concurrency %d against one site, want exactly 1: serialisation failed", peak)
	}
	won := atomic.LoadInt32(&fa.lockWon)
	if won != n {
		t.Fatalf("agent recorded %d successful lock acquisitions, want exactly %d (one per task, eventually)", won, n)
	}
	// The busy path must actually have been exercised (not accidentally
	// bypassed by the CP-side gate alone, which is best-effort): with 3
	// concurrent River workers dispatching against one site's first wave,
	// at least some requests must have collided with the agent's lock.
	if atomic.LoadInt32(&fa.busyRefusals) == 0 {
		t.Fatalf("expected at least one site_busy refusal from the agent (the busy-defer path was never exercised)")
	}
}

// TestEveryUpdateRefusedThenAcceptedStillCompletes is the deadlock witness:
// the agent refuses EVERY request as busy for a short window, then starts
// accepting. Under the prior ("running means dispatched-and-waiting")
// design this would hang forever, because a refused task never stopped
// occupying the gate. Under INVARIANT R a refused task goes straight back to
// 'pending' and is retried, so the run still completes once the window
// clears.
func TestEveryUpdateRefusedThenAcceptedStillCompletes(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "upd-allbusy")
	fa := newFakeAgent(t)
	fa.setHomepage(http.StatusOK, "<html>ok</html>")

	h := buildHarness(t, pool, newTestCommander(t), newTestProber(t))
	h.worker.SetSiteBusyBackoff(func(update.Task) time.Duration { return 20 * time.Millisecond })

	s := enrollFakeSite(t, pool, tenant, fa.url())
	fa.setExpectAud(s.ID.String())
	seedPendingPlugin(t, h, tenant, s, "akismet", "1.0.0", "1.1.0")

	// Refuse everything for the first ~300ms, then let the scripted success
	// response through (fa.updateResp defaults to a plain ItemSucceeded).
	fa.mu.Lock()
	fa.updateResp = agentcmd.UpdateResponse{OK: false, Results: []agentcmd.ItemResult{{
		Status: agentcmd.ItemSiteBusy, Log: "another update is running on this site",
	}}}
	fa.mu.Unlock()
	deadline := time.Now().Add(300 * time.Millisecond)
	go func() {
		time.Sleep(time.Until(deadline))
		fa.mu.Lock()
		fa.updateResp = agentcmd.UpdateResponse{OK: true, Results: []agentcmd.ItemResult{{
			Status: agentcmd.ItemSucceeded, FromVersion: "1.0.0", ToVersion: "1.1.0", SnapshotID: "snap-1",
		}}}
		fa.mu.Unlock()
	}()

	run, _, err := h.svc.CreateRun(context.Background(), update.CreateRunInput{
		TenantID: tenant,
		SiteIDs:  []uuid.UUID{s.ID},
		Items:    []update.Item{{Type: "plugin", Slug: "akismet", Version: "latest"}},
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	_, finalTasks := waitRunCompletedWithin(t, h, tenant, run.ID, 30*time.Second)
	tk := finalTasks[0]
	if tk.Status != update.TaskSucceeded {
		t.Fatalf("task status = %s, want succeeded once the busy window cleared (detail=%s)", tk.Status, tk.Detail)
	}
	if atomic.LoadInt32(&fa.updateCalls) < 2 {
		t.Fatalf("expected at least one busy refusal before the eventual success, got only %d update call(s)", fa.updateCalls)
	}
}
