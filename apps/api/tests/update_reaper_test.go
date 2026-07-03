package tests

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/update"
)

// TestUpdateReaperTerminalizesStaleTasks proves the #131 follow-up reaper: a
// task stuck in pending/running past the stale-task threshold (a worker crash
// between MarkTaskRunning and finish(), or a failed enqueue that leaves a task
// pending) is terminalized as failed, its run completes if that was the last
// outstanding task, and a task that is NOT yet stale is left untouched.
func TestUpdateReaperTerminalizesStaleTasks(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "upd-reaper")
	fa := newFakeAgent(t)
	fa.setHomepage(http.StatusOK, "<html>ok</html>")

	h := buildHarness(t, pool, newTestCommander(t), newTestProber(t))
	s := enrollFakeSite(t, pool, tenant, fa.url())
	fa.setExpectAud(s.ID.String())

	// update_runs/update_tasks carry FORCE ROW LEVEL SECURITY: seeding rows
	// directly (bypassing Service.CreateRun, which would run the update
	// through the worker instead of leaving it "stuck") requires the
	// superuser connection, same as any other out-of-band-tampering test.
	admin := connectAdmin(t, pool)
	defer admin.Close()

	// --- Run A: one task stuck 'running' for 2 hours (well past the 45-minute
	// stale-task threshold) — simulates a worker that crashed after
	// MarkTaskRunning but before it ever called finish().
	var staleRunID uuid.UUID
	if err := admin.QueryRow(ctx,
		`INSERT INTO update_runs (tenant_id, status) VALUES ($1, 'running') RETURNING id`,
		tenant,
	).Scan(&staleRunID); err != nil {
		t.Fatalf("seed stale run: %v", err)
	}
	staleSince := time.Now().Add(-2 * time.Hour)
	var staleTaskID uuid.UUID
	if err := admin.QueryRow(ctx,
		`INSERT INTO update_tasks
		   (run_id, tenant_id, site_id, target_type, target_slug, status, started_at, created_at, updated_at)
		 VALUES ($1, $2, $3, 'plugin', 'stuck-plugin', 'running', $4, $4, $4)
		 RETURNING id`,
		staleRunID, tenant, s.ID, staleSince,
	).Scan(&staleTaskID); err != nil {
		t.Fatalf("seed stale task: %v", err)
	}

	// --- Run B: one task stuck 'pending' for 2 hours — simulates a failed
	// EnqueueTask that left the row created but never picked up by a worker.
	var stalePendingRunID uuid.UUID
	if err := admin.QueryRow(ctx,
		`INSERT INTO update_runs (tenant_id, status) VALUES ($1, 'pending') RETURNING id`,
		tenant,
	).Scan(&stalePendingRunID); err != nil {
		t.Fatalf("seed stale-pending run: %v", err)
	}
	var stalePendingTaskID uuid.UUID
	if err := admin.QueryRow(ctx,
		`INSERT INTO update_tasks
		   (run_id, tenant_id, site_id, target_type, target_slug, status, created_at, updated_at)
		 VALUES ($1, $2, $3, 'plugin', 'never-enqueued-plugin', 'pending', $4, $4)
		 RETURNING id`,
		stalePendingRunID, tenant, s.ID, staleSince,
	).Scan(&stalePendingTaskID); err != nil {
		t.Fatalf("seed stale-pending task: %v", err)
	}

	// --- Run C: one task 'running' with a RECENT updated_at (well inside the
	// threshold) — must be left untouched by the sweep (no false positives).
	var freshRunID uuid.UUID
	if err := admin.QueryRow(ctx,
		`INSERT INTO update_runs (tenant_id, status) VALUES ($1, 'running') RETURNING id`,
		tenant,
	).Scan(&freshRunID); err != nil {
		t.Fatalf("seed fresh run: %v", err)
	}
	fresh := time.Now().Add(-1 * time.Minute)
	var freshTaskID uuid.UUID
	if err := admin.QueryRow(ctx,
		`INSERT INTO update_tasks
		   (run_id, tenant_id, site_id, target_type, target_slug, status, started_at, created_at, updated_at)
		 VALUES ($1, $2, $3, 'plugin', 'in-progress-plugin', 'running', $4, $4, $4)
		 RETURNING id`,
		freshRunID, tenant, s.ID, fresh,
	).Scan(&freshTaskID); err != nil {
		t.Fatalf("seed fresh task: %v", err)
	}

	// Run one reaper sweep directly (no need to wait for the 10-minute
	// periodic interval).
	reaper := update.NewReaperWorker(h.worker)
	if err := reaper.Work(ctx, &river.Job[update.ReapStaleTasksArgs]{}); err != nil {
		t.Fatalf("reaper sweep: %v", err)
	}

	// The stale 'running' task is terminalized as failed.
	staleTask, err := h.repo.GetTask(ctx, tenant, staleTaskID)
	if err != nil {
		t.Fatalf("get stale task: %v", err)
	}
	if staleTask.Status != update.TaskFailed {
		t.Fatalf("stale running task status = %s, want failed", staleTask.Status)
	}
	if staleTask.FinishedAt == nil {
		t.Fatal("reaped task must have finished_at set")
	}
	if staleTask.Detail != "stale: task exceeded max runtime" {
		t.Fatalf("stale task detail = %q, want the stale-reap reason", staleTask.Detail)
	}

	// The stale 'pending' task is ALSO terminalized (the reaper must catch
	// both pending and running, per the failed-enqueue scenario).
	stalePendingTask, err := h.repo.GetTask(ctx, tenant, stalePendingTaskID)
	if err != nil {
		t.Fatalf("get stale-pending task: %v", err)
	}
	if stalePendingTask.Status != update.TaskFailed {
		t.Fatalf("stale pending task status = %s, want failed", stalePendingTask.Status)
	}

	// The fresh, still-in-progress task is untouched.
	freshTask, err := h.repo.GetTask(ctx, tenant, freshTaskID)
	if err != nil {
		t.Fatalf("get fresh task: %v", err)
	}
	if freshTask.Status != update.TaskRunning {
		t.Fatalf("fresh task status = %s, want still running (untouched)", freshTask.Status)
	}

	// Reaping the last outstanding task of a run must also complete that run
	// (mirrors the normal Worker.finish behaviour) — otherwise the run would
	// stay "running"/"pending" forever even after its only task terminalized.
	staleRun, _, err := h.svc.GetRun(ctx, tenant, staleRunID)
	if err != nil {
		t.Fatalf("get stale run: %v", err)
	}
	if staleRun.Status != update.RunCompleted {
		t.Fatalf("stale run status = %s, want completed", staleRun.Status)
	}
	stalePendingRun, _, err := h.svc.GetRun(ctx, tenant, stalePendingRunID)
	if err != nil {
		t.Fatalf("get stale-pending run: %v", err)
	}
	if stalePendingRun.Status != update.RunCompleted {
		t.Fatalf("stale-pending run status = %s, want completed", stalePendingRun.Status)
	}

	// The fresh run must NOT have been marked completed.
	freshRun, _, err := h.svc.GetRun(ctx, tenant, freshRunID)
	if err != nil {
		t.Fatalf("get fresh run: %v", err)
	}
	if freshRun.Status == update.RunCompleted {
		t.Fatal("fresh run must not be completed by the reaper sweep")
	}

	// A second sweep is a no-op for the already-reaped tasks (idempotent):
	// their status is now terminal so ListStaleUpdateTasks no longer selects
	// them, regardless of how stale their (now-refreshed) updated_at is.
	if err := reaper.Work(ctx, &river.Job[update.ReapStaleTasksArgs]{}); err != nil {
		t.Fatalf("second reaper sweep: %v", err)
	}
	staleTaskAgain, err := h.repo.GetTask(ctx, tenant, staleTaskID)
	if err != nil {
		t.Fatalf("get stale task (second sweep): %v", err)
	}
	if staleTaskAgain.Status != update.TaskFailed {
		t.Fatalf("stale task status after second sweep = %s, want unchanged failed", staleTaskAgain.Status)
	}
}
