package tests

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/update"
)

// TestSiteHasRunningUpdateTask_Gate pins the SQL behaviour of the GH #328
// per-site serialisation gate (SiteHasRunningUpdateTask / INVARIANT R)
// directly against a real Postgres, independent of the worker/River harness:
// what "busy" means, that a task never sees itself as busy, that a stale
// 'running' row is ignored (the gate's own staleness clause), and that a
// running agent-target row is excluded entirely (Block D never gates a
// plugin/theme/core dispatch, and vice versa).
func TestSiteHasRunningUpdateTask_Gate(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "upd-gate")
	repo := update.NewRepo(pool)
	ctx := context.Background()

	// Each subtest gets its OWN site (never shared across t.Run blocks). A
	// task deliberately left 'running' by one subtest (to set up its
	// scenario) would otherwise leak into every later subtest sharing the
	// same site and permanently poison SiteHasRunningTask's answer for it,
	// since the gate is scoped to a site, not to a subtest.
	newSite := func(t *testing.T) uuid.UUID {
		t.Helper()
		return seedGateSite(t, pool, tenant)
	}
	mkTaskOn := func(t *testing.T, site uuid.UUID, targetType, slug string) update.Task {
		t.Helper()
		_, tasks, err := repo.CreateRunWithTasks(ctx, update.CreateRunInput{TenantID: tenant}, []update.NewTask{
			{SiteID: site, TargetType: targetType, TargetSlug: slug, DesiredVersion: "latest", FromVersion: "1.0.0"},
		})
		if err != nil {
			t.Fatalf("create task: %v", err)
		}
		return tasks[0]
	}

	t.Run("a running sibling makes the gate busy", func(t *testing.T) {
		site := newSite(t)
		a := mkTaskOn(t, site, "plugin", "a")
		b := mkTaskOn(t, site, "plugin", "b")
		if _, err := repo.MarkTaskRunning(ctx, tenant, a.ID, claimStaleAfter); err != nil {
			t.Fatalf("mark running: %v", err)
		}
		busy, err := repo.SiteHasRunningTask(ctx, tenant, site, b.ID, 20*time.Minute)
		if err != nil {
			t.Fatalf("gate: %v", err)
		}
		if !busy {
			t.Fatal("expected the gate to report busy while a sibling task is running")
		}
	})

	t.Run("a task never sees itself as busy", func(t *testing.T) {
		site := newSite(t)
		a := mkTaskOn(t, site, "plugin", "c")
		if _, err := repo.MarkTaskRunning(ctx, tenant, a.ID, claimStaleAfter); err != nil {
			t.Fatalf("mark running: %v", err)
		}
		busy, err := repo.SiteHasRunningTask(ctx, tenant, site, a.ID, 20*time.Minute)
		if err != nil {
			t.Fatalf("gate: %v", err)
		}
		if busy {
			t.Fatal("a task must never see its OWN running row as a busy sibling")
		}
	})

	t.Run("a pending sibling does not make the gate busy", func(t *testing.T) {
		site := newSite(t)
		// mkTaskOn leaves the row 'pending' (never marked running): under
		// INVARIANT R a waiter must never look busy to another waiter, or the
		// whole no-deadlock proof collapses.
		mkTaskOn(t, site, "plugin", "d")
		b := mkTaskOn(t, site, "plugin", "e")
		busy, err := repo.SiteHasRunningTask(ctx, tenant, site, b.ID, 20*time.Minute)
		if err != nil {
			t.Fatalf("gate: %v", err)
		}
		if busy {
			t.Fatal("a merely-pending sibling must never make the gate busy (INVARIANT R: only 'running' counts)")
		}
	})

	t.Run("a stale running row is ignored", func(t *testing.T) {
		site := newSite(t)
		a := mkTaskOn(t, site, "plugin", "f")
		b := mkTaskOn(t, site, "plugin", "g")
		if _, err := repo.MarkTaskRunning(ctx, tenant, a.ID, claimStaleAfter); err != nil {
			t.Fatalf("mark running: %v", err)
		}
		backdateTaskTimestamps(t, pool, a.ID, 30*time.Minute)

		busy, err := repo.SiteHasRunningTask(ctx, tenant, site, b.ID, 20*time.Minute)
		if err != nil {
			t.Fatalf("gate: %v", err)
		}
		if busy {
			t.Fatal("a 'running' row older than holdMax cannot have a live worker behind it and must not be trusted")
		}

		// The SAME row, well within holdMax, DOES gate: proves the staleness
		// clause (not some other reason) was what ignored it above.
		fresh := mkTaskOn(t, site, "plugin", "h")
		if _, err := repo.MarkTaskRunning(ctx, tenant, fresh.ID, claimStaleAfter); err != nil {
			t.Fatalf("mark running: %v", err)
		}
		i := mkTaskOn(t, site, "plugin", "i")
		busy, err = repo.SiteHasRunningTask(ctx, tenant, site, i.ID, 20*time.Minute)
		if err != nil {
			t.Fatalf("gate: %v", err)
		}
		if !busy {
			t.Fatal("a fresh 'running' row within holdMax must still gate")
		}
	})

	t.Run("a running agent-target row never gates a plugin dispatch", func(t *testing.T) {
		site := newSite(t)
		a := mkTaskOn(t, site, "agent", update.AgentTargetSlug)
		b := mkTaskOn(t, site, "plugin", "j")
		if _, err := repo.MarkTaskRunning(ctx, tenant, a.ID, claimStaleAfter); err != nil {
			t.Fatalf("mark running: %v", err)
		}
		busy, err := repo.SiteHasRunningTask(ctx, tenant, site, b.ID, 20*time.Minute)
		if err != nil {
			t.Fatalf("gate: %v", err)
		}
		if busy {
			t.Fatal("a running agent-target task must be excluded from the gate entirely (target_type <> 'agent')")
		}
	})
}

// TestDeferUpdateTaskToPending_Semantics pins DeferTaskToPending's contract:
// it flips the row back to 'pending', clears started_at, records the wait
// detail, refreshes updated_at, is idempotent from either open state, and
// reports ErrTaskNotOpen (rather than silently no-op'ing) once the task has
// already reached a terminal state.
func TestDeferUpdateTaskToPending_Semantics(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "upd-defer")
	repo := update.NewRepo(pool)
	ctx := context.Background()
	site := seedGateSite(t, pool, tenant)

	_, tasks, err := repo.CreateRunWithTasks(ctx, update.CreateRunInput{TenantID: tenant}, []update.NewTask{
		{SiteID: site, TargetType: "plugin", TargetSlug: "a", DesiredVersion: "latest", FromVersion: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	task := tasks[0]

	if _, err := repo.MarkTaskRunning(ctx, tenant, task.ID, claimStaleAfter); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	deferred, err := repo.DeferTaskToPending(ctx, update.DeferTaskInput{TenantID: tenant, TaskID: task.ID, Detail: "waiting: another update is running on this site"})
	if err != nil {
		t.Fatalf("defer: %v", err)
	}
	if deferred.Status != update.TaskPending {
		t.Fatalf("status = %s, want pending", deferred.Status)
	}
	if deferred.StartedAt != nil {
		t.Fatalf("started_at = %v, want cleared (nil)", deferred.StartedAt)
	}
	if deferred.Detail != "waiting: another update is running on this site" {
		t.Fatalf("detail = %q, want the wait sentence", deferred.Detail)
	}

	// Idempotent from the already-pending state (the pre-dispatch gate case:
	// nothing was ever sent, DeferTaskToPending is called anyway).
	again, err := repo.DeferTaskToPending(ctx, update.DeferTaskInput{TenantID: tenant, TaskID: task.ID, Detail: "waiting: attempt 2"})
	if err != nil {
		t.Fatalf("defer again from pending: %v", err)
	}
	if again.Status != update.TaskPending || again.Detail != "waiting: attempt 2" {
		t.Fatalf("expected a second successful defer from 'pending', got status=%s detail=%q", again.Status, again.Detail)
	}

	// Once terminal, DeferTaskToPending must refuse rather than silently
	// resurrect a finished task.
	if _, err := repo.FinishTask(ctx, update.FinishTaskInput{TenantID: tenant, TaskID: task.ID, Status: update.TaskSucceeded, FromVersion: "1.0.0", ToVersion: "1.1.0"}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if _, err := repo.DeferTaskToPending(ctx, update.DeferTaskInput{TenantID: tenant, TaskID: task.ID, Detail: "waiting: too late"}); err == nil {
		t.Fatal("expected ErrTaskNotOpen once the task is terminal")
	}
}

// TestBusyDeferredTask_ReapedOnlyWhenAbandoned is the GH #328 reaper proof
// (task requirement 4): a task DeferTaskToPending just touched is NOT stale
// (a live River job is evidently still minding it, since only Worker.Work
// issues this statement), but the SAME row, once its updated_at watermark
// stops advancing (the worker crashed or its queue was lost), IS claimed by
// the periodic reaper after the threshold — a busy task must never become
// immortal.
func TestBusyDeferredTask_ReapedOnlyWhenAbandoned(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "upd-reap-busy")
	repo := update.NewRepo(pool)
	ctx := context.Background()
	site := seedGateSite(t, pool, tenant)

	_, tasks, err := repo.CreateRunWithTasks(ctx, update.CreateRunInput{TenantID: tenant}, []update.NewTask{
		{SiteID: site, TargetType: "plugin", TargetSlug: "a", DesiredVersion: "latest", FromVersion: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	task := tasks[0]
	if _, err := repo.MarkTaskRunning(ctx, tenant, task.ID, claimStaleAfter); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if _, err := repo.DeferTaskToPending(ctx, update.DeferTaskInput{TenantID: tenant, TaskID: task.ID, Detail: "waiting: busy"}); err != nil {
		t.Fatalf("defer: %v", err)
	}

	// Fresh off a defer: updated_at is now(), so a short reaper threshold must
	// NOT claim it yet.
	stale, err := repo.ListStaleUpdateTasks(ctx, time.Hour, 100)
	if err != nil {
		t.Fatalf("list stale: %v", err)
	}
	if containsTaskID(stale, task.ID) {
		t.Fatal("a task just deferred (fresh updated_at) must not be reaped as stale")
	}

	// Simulate the worker/queue having been lost: the watermark freezes.
	backdateTaskTimestamps(t, pool, task.ID, 2*time.Hour)

	stale, err = repo.ListStaleUpdateTasks(ctx, time.Hour, 100)
	if err != nil {
		t.Fatalf("list stale: %v", err)
	}
	if !containsTaskID(stale, task.ID) {
		t.Fatal("a busy-deferred task whose watermark stopped advancing must eventually be reaped: it must not become immortal")
	}
}

func containsTaskID(tasks []update.Task, id uuid.UUID) bool {
	for _, t := range tasks {
		if t.ID == id {
			return true
		}
	}
	return false
}

// seedGateSite enrolls a minimal site (no live agent needed: these tests
// exercise the repo/SQL layer directly, never an HTTP round trip). Reuses
// enrollFakeSite (update_integration_test.go), which only writes DB rows and
// never dials the given URL. The URL is made unique per call: sites carries a
// UNIQUE (tenant_id, url) index (sites_tenant_id_url_key), and this helper is
// called more than once per tenant across a test's subtests.
func seedGateSite(t *testing.T, pool *db.Pool, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	url := fmt.Sprintf("https://gate-test-%s.example", uuid.New().String())
	return enrollFakeSite(t, pool, tenant, url).ID
}

// backdateTaskTimestamps rewinds an update_tasks row's started_at and
// updated_at by d, out of band via a superuser connection, to simulate a row
// a live worker is no longer minding (a crashed worker, a lost queue) without
// waiting d in real time.
func backdateTaskTimestamps(t *testing.T, pool *db.Pool, taskID uuid.UUID, d time.Duration) {
	t.Helper()
	admin := connectAdmin(t, pool)
	defer admin.Close()
	// make_interval(secs => $2) avoids any ambiguity in parsing a Go duration
	// string as a Postgres interval literal.
	if _, err := admin.Exec(context.Background(),
		`UPDATE update_tasks SET started_at = started_at - make_interval(secs := $2), updated_at = updated_at - make_interval(secs := $2) WHERE id = $1`,
		taskID, d.Seconds()); err != nil {
		t.Fatalf("backdate task timestamps: %v", err)
	}
}
