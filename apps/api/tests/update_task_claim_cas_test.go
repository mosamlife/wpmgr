package tests

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/update"
)

// TestMarkUpdateTaskRunning_CompareAndSwap pins the claim precondition on
// MarkUpdateTaskRunning directly against a real Postgres, as wpmgr_app, and
// through the same tenant-scoped RLS transaction production uses.
//
// The defect it guards: Worker.Work loads a task, returns early only for
// TERMINAL statuses, and claims some way further down. The claim carried no
// status precondition, so two River jobs for the same task both observed a
// non-terminal row in that gap and BOTH dispatched, applying one item to one
// site twice. The precondition makes the transition a compare-and-swap decided
// by the row under its own lock.
//
// Every claim here goes through update.Repo (InTenantTx -> sqlc), or through
// sqlc inside pool.InTenantTx where the repo signature cannot yet carry the
// staleness bound. Nothing opens its own connection, so the RLS policies are
// live for every statement below rather than inert.
func TestMarkUpdateTaskRunning_CompareAndSwap(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "upd-cas")
	repo := update.NewRepo(pool)
	ctx := context.Background()

	// A site per subtest, never shared: a row deliberately left 'running' to
	// set up one scenario would otherwise leak into the next.
	newSite := func(t *testing.T) uuid.UUID {
		t.Helper()
		url := fmt.Sprintf("https://cas-test-%s.example", uuid.New().String())
		return enrollFakeSite(t, pool, tenant, url).ID
	}
	mkTask := func(t *testing.T, site uuid.UUID, targetType, slug string) update.Task {
		t.Helper()
		_, tasks, err := repo.CreateRunWithTasks(ctx, update.CreateRunInput{TenantID: tenant}, []update.NewTask{
			{SiteID: site, TargetType: targetType, TargetSlug: slug, DesiredVersion: "latest", FromVersion: "1.0.0"},
		})
		if err != nil {
			t.Fatalf("create task: %v", err)
		}
		return tasks[0]
	}

	// claimWithBound issues the claim with an explicit staleness bound, which
	// update.Repo.MarkTaskRunning cannot yet supply (that is the follow-up Go
	// change). Still the production path in every way that matters to the
	// policy: pool.InTenantTx as wpmgr_app, generated sqlc, no bespoke
	// connection.
	claimWithBound := func(t *testing.T, taskID uuid.UUID, staleAfter time.Duration) (update.Task, bool) {
		t.Helper()
		var got sqlc.UpdateTask
		claimed := false
		err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			row, err := sqlc.New(tx).MarkUpdateTaskRunning(ctx, sqlc.MarkUpdateTaskRunningParams{
				ID:         taskID,
				TenantID:   tenant,
				StaleAfter: pgtype.Interval{Microseconds: staleAfter.Microseconds(), Valid: true},
			})
			if err != nil {
				if err == pgx.ErrNoRows {
					return nil // no claim; not an error for this helper
				}
				return err
			}
			got, claimed = row, true
			return nil
		})
		if err != nil {
			t.Fatalf("claim with bound: %v", err)
		}
		_ = got
		return update.Task{}, claimed
	}

	// statusOf reads a row back through the same tenant-scoped path.
	statusOf := func(t *testing.T, taskID uuid.UUID) string {
		t.Helper()
		task, err := repo.GetTask(ctx, tenant, taskID)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		return task.Status
	}

	// ---- FIRES -------------------------------------------------------------

	t.Run("two concurrent claims of one task: exactly one wins", func(t *testing.T) {
		site := newSite(t)
		task := mkTask(t, site, "plugin", "cas-race")

		// Both goroutines model a River job that has already passed
		// Work's terminal check and is about to dispatch.
		const claimants = 2
		var wg sync.WaitGroup
		results := make([]error, claimants)
		start := make(chan struct{})
		for i := 0; i < claimants; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, err := repo.MarkTaskRunning(ctx, tenant, task.ID)
				results[i] = err
			}(i)
		}
		close(start) // release both at once
		wg.Wait()

		winners := 0
		for i, err := range results {
			if err == nil {
				winners++
			} else {
				t.Logf("claimant %d was refused, as it must be: %v", i, err)
			}
		}
		if winners != 1 {
			t.Fatalf("expected EXACTLY ONE claimant to win, got %d of %d. "+
				"More than one winner means two workers both dispatch this item to this site.", winners, claimants)
		}
		if got := statusOf(t, task.ID); got != "running" {
			t.Fatalf("task status = %q, want \"running\" after the winning claim", got)
		}
	})

	// ---- DOES NOT OVER-FIRE ------------------------------------------------

	t.Run("a normal single claim of a pending task still succeeds", func(t *testing.T) {
		site := newSite(t)
		task := mkTask(t, site, "plugin", "cas-normal")
		if _, err := repo.MarkTaskRunning(ctx, tenant, task.ID); err != nil {
			t.Fatalf("the ordinary claim of a pending task must succeed: %v", err)
		}
		if got := statusOf(t, task.ID); got != "running" {
			t.Fatalf("task status = %q, want \"running\"", got)
		}
	})

	t.Run("a River retry of an ABANDONED in-flight task reclaims it", func(t *testing.T) {
		site := newSite(t)
		task := mkTask(t, site, "plugin", "cas-abandoned")
		if _, err := repo.MarkTaskRunning(ctx, tenant, task.ID); err != nil {
			t.Fatalf("initial claim: %v", err)
		}
		// The worker behind it died: its command went out longer ago than the
		// bound, so River has already cancelled that job's context.
		backdateTaskTimestamps(t, pool, task.ID, 30*time.Minute)

		if _, ok := claimWithBound(t, task.ID, 20*time.Minute); !ok {
			t.Fatal("a 'running' task older than the staleness bound MUST be reclaimable. " +
				"Refusing it turns a duplicate-work bug into a dropped-work bug: no live worker " +
				"is behind this row and nothing else will re-dispatch it before the reaper.")
		}
	})

	t.Run("a retry does NOT steal a task another worker is actively dispatching", func(t *testing.T) {
		site := newSite(t)
		task := mkTask(t, site, "plugin", "cas-fresh")
		if _, err := repo.MarkTaskRunning(ctx, tenant, task.ID); err != nil {
			t.Fatalf("initial claim: %v", err)
		}
		// Not backdated: the holder claimed moments ago and is mid-dispatch.
		if _, ok := claimWithBound(t, task.ID, 20*time.Minute); ok {
			t.Fatal("a FRESH 'running' task must not be re-claimable: the worker holding it is " +
				"still within its own job timeout and is talking to the site right now")
		}
	})

	t.Run("the agent wave path still claims its pending task", func(t *testing.T) {
		site := newSite(t)
		task := mkTask(t, site, "agent", "fleet-agent-site-manager")
		// ClaimAgentWaveTask reaches MarkUpdateTaskRunning only with the row
		// still 'pending' (it returns ClaimAlreadyClaimed for anything else
		// while holding the run's advisory lock). That claim must still work.
		if _, err := repo.MarkTaskRunning(ctx, tenant, task.ID); err != nil {
			t.Fatalf("the agent self-update wave claim of a pending task must still succeed: %v", err)
		}
		if got := statusOf(t, task.ID); got != "running" {
			t.Fatalf("agent task status = %q, want \"running\"", got)
		}
	})

	t.Run("an aged RUNNING agent task is never reclaimed", func(t *testing.T) {
		site := newSite(t)
		task := mkTask(t, site, "agent", "fleet-agent-site-manager")
		if _, err := repo.MarkTaskRunning(ctx, tenant, task.ID); err != nil {
			t.Fatalf("initial claim: %v", err)
		}
		// An agent task legitimately stays 'running' for its whole
		// confirmation window (20m, or 90m on external cron) with no live
		// worker behind it: the apply happens after the ARM response is
		// released. Age is therefore NOT evidence of abandonment here.
		backdateTaskTimestamps(t, pool, task.ID, 30*time.Minute)

		if _, ok := claimWithBound(t, task.ID, 20*time.Minute); ok {
			t.Fatal("an agent-target 'running' row must NEVER be reclaimed on age alone: " +
				"it is mid-confirmation by design, and re-dispatching would upgrade the agent twice")
		}
	})

	t.Run("a terminal task is never claimable", func(t *testing.T) {
		site := newSite(t)
		task := mkTask(t, site, "plugin", "cas-terminal")
		if _, err := repo.MarkTaskRunning(ctx, tenant, task.ID); err != nil {
			t.Fatalf("initial claim: %v", err)
		}
		if _, err := repo.FinishTask(ctx, update.FinishTaskInput{
			TenantID: tenant, TaskID: task.ID, Status: "succeeded",
			FromVersion: "1.0.0", ToVersion: "1.1.0",
		}); err != nil {
			t.Fatalf("finish: %v", err)
		}
		if _, ok := claimWithBound(t, task.ID, time.Nanosecond); ok {
			t.Fatal("a terminal task must never be re-claimed, at any staleness bound: " +
				"its outcome is already recorded")
		}
	})
}

var _ = db.Pool{}
