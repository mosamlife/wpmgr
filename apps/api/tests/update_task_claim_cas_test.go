package tests

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
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
// claimStaleAfter is READ FROM the production derivation, never re-declared.
// A literal here would keep passing against a stale number after the real
// bound was retuned, asserting nothing. Zero means "no derived apply budget",
// which is what update.NewWorker is handed in a default-config install.
var claimStaleAfter = update.ClaimStaleAfter(0)

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

	// claimWithBound issues the claim with an explicit staleness bound. Since
	// the Go wiring landed, update.Repo.MarkTaskRunning carries that bound
	// itself, so this is now EXACTLY the production path — the same repo
	// method Worker.Work calls, over pool.InTenantTx as wpmgr_app, no bespoke
	// connection. A refusal must surface as update.ErrTaskNotClaimed and must
	// NOT be a domain error: reporting the losing claimant as NotFound sends
	// an operator hunting for a row that is still there, held by the winner.
	claimWithBound := func(t *testing.T, taskID uuid.UUID, staleAfter time.Duration) (update.Task, bool) {
		t.Helper()
		got, err := repo.MarkTaskRunning(ctx, tenant, taskID, staleAfter)
		if err == nil {
			return got, true
		}
		if !errors.Is(err, update.ErrTaskNotClaimed) {
			t.Fatalf("a refused claim must report ErrTaskNotClaimed, got %v (%T)", err, err)
		}
		if de, ok := domain.AsDomain(err); ok {
			t.Fatalf("a refused claim must not be a domain error, got %v", de)
		}
		return update.Task{}, false
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
				_, err := repo.MarkTaskRunning(ctx, tenant, task.ID, claimStaleAfter)
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
				// The loser's error is part of the contract, not just noise.
				// It must say "you did not get the claim", never "no such
				// task": the row is right there, held by the winner, and
				// NotFound would send an operator hunting for a deleted row.
				if !errors.Is(err, update.ErrTaskNotClaimed) {
					t.Errorf("the losing claimant must be refused with ErrTaskNotClaimed, got %v (%T)", err, err)
				}
				if de, ok := domain.AsDomain(err); ok {
					t.Errorf("the losing claimant must not get a domain error (it was reported as NotFound before this fix), got %v", de)
				}
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
		if _, err := repo.MarkTaskRunning(ctx, tenant, task.ID, claimStaleAfter); err != nil {
			t.Fatalf("the ordinary claim of a pending task must succeed: %v", err)
		}
		if got := statusOf(t, task.ID); got != "running" {
			t.Fatalf("task status = %q, want \"running\"", got)
		}
	})

	t.Run("a River retry of an ABANDONED in-flight task reclaims it", func(t *testing.T) {
		site := newSite(t)
		task := mkTask(t, site, "plugin", "cas-abandoned")
		if _, err := repo.MarkTaskRunning(ctx, tenant, task.ID, claimStaleAfter); err != nil {
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
		if _, err := repo.MarkTaskRunning(ctx, tenant, task.ID, claimStaleAfter); err != nil {
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
		if _, err := repo.MarkTaskRunning(ctx, tenant, task.ID, claimStaleAfter); err != nil {
			t.Fatalf("the agent self-update wave claim of a pending task must still succeed: %v", err)
		}
		if got := statusOf(t, task.ID); got != "running" {
			t.Fatalf("agent task status = %q, want \"running\"", got)
		}
	})

	t.Run("an aged RUNNING agent task is never reclaimed", func(t *testing.T) {
		site := newSite(t)
		task := mkTask(t, site, "agent", "fleet-agent-site-manager")
		if _, err := repo.MarkTaskRunning(ctx, tenant, task.ID, claimStaleAfter); err != nil {
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

	t.Run("another tenant can never claim this tenant's task", func(t *testing.T) {
		site := newSite(t)
		task := mkTask(t, site, "plugin", "cas-cross-tenant")

		// A second tenant, claiming with the victim's real task id. The id is
		// not a secret — it appears in URLs and audit rows — so the boundary
		// has to be the tenant scoping itself, not the caller's ignorance of
		// the id. MarkTaskRunning goes through InTenantTx as wpmgr_app, so
		// RLS is live for this statement exactly as it is in production.
		attacker := seedTenant(t, pool, "upd-cas-other")
		_, err := repo.MarkTaskRunning(ctx, attacker, task.ID, claimStaleAfter)
		if err == nil {
			t.Fatal("a tenant claimed another tenant's update task: the claim must be refused")
		}
		if !errors.Is(err, update.ErrTaskNotClaimed) {
			t.Fatalf("a cross-tenant claim must be refused as ErrTaskNotClaimed, got %v (%T)", err, err)
		}

		// The victim's row must be untouched: still claimable by its OWN
		// tenant. A refusal that had already flipped the row to 'running'
		// would be a denial of service even though no data leaked.
		if got := statusOf(t, task.ID); got != "pending" {
			t.Fatalf("the victim task status = %q, want \"pending\": a refused cross-tenant claim must not modify the row", got)
		}
		if _, err := repo.MarkTaskRunning(ctx, tenant, task.ID, claimStaleAfter); err != nil {
			t.Fatalf("the owning tenant must still be able to claim its own task: %v", err)
		}
	})

	t.Run("a terminal task is never claimable", func(t *testing.T) {
		site := newSite(t)
		task := mkTask(t, site, "plugin", "cas-terminal")
		if _, err := repo.MarkTaskRunning(ctx, tenant, task.ID, claimStaleAfter); err != nil {
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
