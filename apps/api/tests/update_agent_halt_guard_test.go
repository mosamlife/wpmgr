package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/update"
)

// GH #255 Phase 2, the two SQL preconditions that keep a halted agent
// self-update run honest, exercised against a real Postgres through the real
// generated queries. The in-package unit tests drive the same rules through
// in-memory doubles; these prove the rules live where they have to be atomic,
// in db/query/updates.sql.
//
//   - CancelPendingUpdateTask carries `AND status = 'pending'`, so a halt can
//     only cancel a task nothing was ever sent for. update_tasks.status
//     'cancelled' means exactly "nothing was ever sent to this site"; applying
//     it to a running task would record a falsehood AND stop the confirmation
//     poll (AgentConfirmWorker.Work short-circuits on a terminal status) from
//     ever establishing what happened to a site the control plane had already
//     touched, at the moment an operator hit the kill switch and most needs to
//     know.
//   - FinishUpdateTask carries `AND status IN ('pending','running')`, so a
//     worker that was in flight when the halt landed cannot come back and
//     overwrite 'cancelled' with 'succeeded'. Without it the kill switch looks
//     like it stopped a rollout that then reported itself a success.

// seedAgentRun creates one agent self-update run with n tasks over n freshly
// enrolled sites, through the real repo, so RLS, the in-flight unique index and
// every column default are the production ones.
func seedAgentRun(t *testing.T, pool *db.Pool, tenant uuid.UUID, repo update.Repo, n int) (update.Run, []update.Task) {
	t.Helper()

	newTasks := make([]update.NewTask, 0, n)
	for i := 0; i < n; i++ {
		s := enrollFakeSite(t, pool, tenant, "https://"+uuid.NewString()+".example.com")
		newTasks = append(newTasks, update.NewTask{
			SiteID:     s.ID,
			TargetType: update.TargetAgent,
			TargetSlug: update.AgentTargetSlug,
			// The RESOLVED target the planner records, not the literal
			// "latest": it is the run's premise and the only fixed thing a
			// later "up_to_date" answer can be checked against.
			DesiredVersion: "0.62.0",
			FromVersion:    "0.61.80",
		})
	}

	run, tasks, err := repo.CreateRunWithTasks(context.Background(),
		update.CreateRunInput{TenantID: tenant}, newTasks)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if len(tasks) != n {
		t.Fatalf("created %d tasks, want %d", len(tasks), n)
	}
	return run, tasks
}

// TestAgentHaltCancelsOnlyPendingTasks: a halt cancels what was never
// dispatched and leaves a RUNNING task alone for its own confirmation job.
func TestAgentHaltCancelsOnlyPendingTasks(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "agent-halt-pending")

	repo := update.NewRepo(pool)
	waves := update.NewAgentWaveRepo(pool)
	run, tasks := seedAgentRun(t, pool, tenant, repo, 4)

	// Task 0 has been dispatched: its command is delivered and (in production)
	// a cron event is spawned on the site. Tasks 1..3 were never sent anything.
	if _, err := repo.MarkTaskRunning(ctx, tenant, tasks[0].ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	ev, err := waves.HaltAgentRun(ctx, tenant, run.ID, "stopped by the operator")
	if err != nil {
		t.Fatalf("halt: %v", err)
	}
	if !ev.Halted || !ev.Changed {
		t.Fatalf("the first halt must report the transition: %+v", ev)
	}
	if ev.Cancelled != 3 {
		t.Fatalf("cancelled %d tasks, want the 3 that were still pending", ev.Cancelled)
	}

	got, err := repo.ListTasks(ctx, tenant, run.ID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	byID := map[uuid.UUID]update.Task{}
	for _, task := range got {
		byID[task.ID] = task
	}
	if s := byID[tasks[0].ID].Status; s != update.TaskRunning {
		t.Fatalf("the dispatched task is %q, want %q: a halt must not claim nothing was sent to a site that was contacted",
			s, update.TaskRunning)
	}
	for i := 1; i < 4; i++ {
		if s := byID[tasks[i].ID].Status; s != update.TaskCancelled {
			t.Fatalf("task %d is %q, want %q", i, s, update.TaskCancelled)
		}
	}

	// Halting again is idempotent and still leaves the running task alone.
	second, err := waves.HaltAgentRun(ctx, tenant, run.ID, "stopped by the operator")
	if err != nil {
		t.Fatalf("second halt: %v", err)
	}
	if second.Changed || second.Cancelled != 0 {
		t.Fatalf("a repeated halt must be a no-op: %+v", second)
	}
	after, err := repo.GetTask(ctx, tenant, tasks[0].ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if after.Status != update.TaskRunning {
		t.Fatalf("the dispatched task is %q after a second halt, want %q", after.Status, update.TaskRunning)
	}
}

// TestCancelPendingUpdateTaskRefusesEveryNonPendingRow exercises the SQL
// precondition on its own, which the test above cannot.
//
// TestAgentHaltCancelsOnlyPendingTasks drives the cancel through
// HaltAgentRun, and haltLocked ALREADY filters to pending rows in Go before it
// issues the statement. Both writers also hold the same per-run advisory lock,
// so the Go filter is never even racing anything there. Delete `AND status =
// 'pending'` from db/query/updates.sql and that test still passes: the guard
// this file exists to protect can be removed without a single failure.
//
// So this one calls the generated query DIRECTLY, with no Go filter in front of
// it, once for every status a task can already hold. That is also the shape of
// the race the precondition is really for: a worker claims a task (pending ->
// running) between the halt's snapshot read and its UPDATE, and only the
// database can settle who wins.
//
// The property: the statement must touch NO row that is not pending, and it must
// leave the recorded outcome exactly as it found it.
func TestCancelPendingUpdateTaskRefusesEveryNonPendingRow(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "agent-cancel-precondition")

	repo := update.NewRepo(pool)

	// Every non-pending status a row can hold when a halt reaches it, plus the
	// pending control that proves the statement is a precondition and not a
	// blanket refusal.
	cases := []struct {
		name string
		// setup moves the seeded task into the status under test and returns
		// the detail the row must still carry afterwards.
		setup      func(t *testing.T, task update.Task) string
		wantCancel bool
	}{
		{
			// The one the design is actually about: a dispatched task, whose
			// command is delivered and whose cron event is spawned on the site.
			name: "running",
			setup: func(t *testing.T, task update.Task) string {
				t.Helper()
				if _, err := repo.MarkTaskRunning(ctx, tenant, task.ID); err != nil {
					t.Fatalf("mark running: %v", err)
				}
				return ""
			},
		},
		{
			name: "succeeded",
			setup: func(t *testing.T, task update.Task) string {
				t.Helper()
				finishTaskAs(t, repo, tenant, task.ID, update.TaskSucceeded, "upgraded and confirmed")
				return "upgraded and confirmed"
			},
		},
		{
			name: "failed",
			setup: func(t *testing.T, task update.Task) string {
				t.Helper()
				finishTaskAs(t, repo, tenant, task.ID, update.TaskFailed, "the agent could not arm its self-update")
				return "the agent could not arm its self-update"
			},
		},
		{
			name: "skipped",
			setup: func(t *testing.T, task update.Task) string {
				t.Helper()
				finishTaskAs(t, repo, tenant, task.ID, update.TaskSkipped, "not confirmed: the site did not reach this run's target")
				return "not confirmed: the site did not reach this run's target"
			},
		},
		{
			// An earlier halt already cancelled this row. The detail is
			// deliberately a DIFFERENT reason from the one this cancel would
			// write, so "nothing was written" is distinguishable from "the same
			// thing was written twice".
			name: "cancelled already",
			setup: func(t *testing.T, task update.Task) string {
				t.Helper()
				finishTaskAs(t, repo, tenant, task.ID, update.TaskCancelled, "cancelled: an earlier halt, for its own reason")
				return "cancelled: an earlier halt, for its own reason"
			},
		},
		{
			name:       "pending (the control)",
			setup:      func(*testing.T, update.Task) string { return "" },
			wantCancel: true,
		},
	}

	const haltDetail = "cancelled: stopped by the operator"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, tasks := seedAgentRun(t, pool, tenant, repo, 1)
			task := tasks[0]
			wantDetail := tc.setup(t, task)

			before, err := repo.GetTask(ctx, tenant, task.ID)
			if err != nil {
				t.Fatalf("get task: %v", err)
			}

			var cancelled bool
			err = pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
				_, qerr := sqlc.New(tx).CancelPendingUpdateTask(ctx, sqlc.CancelPendingUpdateTaskParams{
					ID:       task.ID,
					TenantID: tenant,
					Detail:   haltDetail,
				})
				if errors.Is(qerr, pgx.ErrNoRows) {
					return nil // refused: no row matched the precondition.
				}
				if qerr != nil {
					return qerr
				}
				cancelled = true
				return nil
			})
			if err != nil {
				t.Fatalf("cancel: %v", err)
			}

			after, err := repo.GetTask(ctx, tenant, task.ID)
			if err != nil {
				t.Fatalf("get task after: %v", err)
			}

			if cancelled != tc.wantCancel {
				t.Fatalf("the statement %s a %s task; want %v. Status is now %q.\n"+
					"This is the `AND status = 'pending'` precondition in db/query/updates.sql: without it a halt "+
					"can record \"nothing was ever sent to this site\" about a site that was contacted, and stop the "+
					"confirmation poll from ever establishing what happened there.",
					map[bool]string{true: "cancelled", false: "refused"}[cancelled], tc.name, tc.wantCancel, after.Status)
			}
			if tc.wantCancel {
				if after.Status != update.TaskCancelled || after.Detail != haltDetail {
					t.Fatalf("a pending task must still be cancellable: status %q detail %q", after.Status, after.Detail)
				}
				return
			}
			if after.Status != before.Status {
				t.Fatalf("status moved from %q to %q", before.Status, after.Status)
			}
			if after.Detail != wantDetail {
				t.Fatalf("detail = %q, want the outcome already recorded (%q): a refused cancel must write nothing at all",
					after.Detail, wantDetail)
			}
			if after.Detail == haltDetail {
				t.Fatalf("a %s task acquired the halt's cancellation detail", tc.name)
			}
		})
	}
}

// finishTaskAs drives one task to a terminal status through the real repo.
func finishTaskAs(t *testing.T, repo update.Repo, tenant, taskID uuid.UUID, status, detail string) {
	t.Helper()
	if _, err := repo.FinishTask(context.Background(), update.FinishTaskInput{
		TenantID: tenant,
		TaskID:   taskID,
		Status:   status,
		Detail:   detail,
	}); err != nil {
		t.Fatalf("finish as %s: %v", status, err)
	}
}

// TestFinishUpdateTaskCannotOverwriteATerminalTask: the in-flight worker comes
// back after the halt. Its outcome must not land on a task that already has
// one, and the caller must be told so rather than being handed a lie.
func TestFinishUpdateTaskCannotOverwriteATerminalTask(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "agent-halt-overwrite")

	repo := update.NewRepo(pool)
	waves := update.NewAgentWaveRepo(pool)
	halted, haltedTasks := seedAgentRun(t, pool, tenant, repo, 2)

	// A second, untouched run: the control that proves the guard below is a
	// precondition and not a blanket refusal.
	_, openTasks := seedAgentRun(t, pool, tenant, repo, 1)
	if _, err := repo.MarkTaskRunning(ctx, tenant, openTasks[0].ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	if _, err := waves.HaltAgentRun(ctx, tenant, halted.ID, "stopped by the operator"); err != nil {
		t.Fatalf("halt: %v", err)
	}

	// The worker that was already in flight reports its success afterwards.
	out, err := repo.FinishTask(ctx, update.FinishTaskInput{
		TenantID:    tenant,
		TaskID:      haltedTasks[0].ID,
		Status:      update.TaskSucceeded,
		FromVersion: "0.61.80",
		ToVersion:   "0.62.0",
		Detail:      "upgraded and confirmed",
	})
	if !errors.Is(err, update.ErrTaskNotOpen) {
		t.Fatalf("finishing an already-terminal task must report ErrTaskNotOpen, got %v", err)
	}
	if out.Status != update.TaskCancelled {
		t.Fatalf("the returned row is %q, want the recorded outcome %q", out.Status, update.TaskCancelled)
	}

	stored, err := repo.GetTask(ctx, tenant, haltedTasks[0].ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if stored.Status != update.TaskCancelled {
		t.Fatalf("status = %q, want %q: the first recorded outcome wins", stored.Status, update.TaskCancelled)
	}
	if stored.ToVersion == "0.62.0" {
		t.Fatal("a cancelled task must not acquire the version a late worker reported")
	}
	if stored.Detail == "upgraded and confirmed" {
		t.Fatal("a cancelled task must keep the halt's reason, not a late worker's detail")
	}

	// The control: a task that is still open finishes exactly as before.
	finished, err := repo.FinishTask(ctx, update.FinishTaskInput{
		TenantID:    tenant,
		TaskID:      openTasks[0].ID,
		Status:      update.TaskSucceeded,
		FromVersion: "0.61.80",
		ToVersion:   "0.62.0",
		Detail:      "upgraded and confirmed",
	})
	if err != nil {
		t.Fatalf("an open task must still finish: %v", err)
	}
	if finished.Status != update.TaskSucceeded {
		t.Fatalf("status = %q, want %q", finished.Status, update.TaskSucceeded)
	}

	// And a task that does not exist at all is still a clean not-found, not
	// mistaken for a task that merely closed.
	if _, err := repo.FinishTask(ctx, update.FinishTaskInput{
		TenantID: tenant,
		TaskID:   uuid.New(),
		Status:   update.TaskSucceeded,
	}); err == nil || errors.Is(err, update.ErrTaskNotOpen) {
		t.Fatalf("finishing an unknown task must be a not-found, got %v", err)
	}
}
