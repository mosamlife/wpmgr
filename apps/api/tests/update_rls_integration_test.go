package tests

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/update"
)

// noopEnqueuer counts enqueue calls without touching River, so CreateRun can be
// exercised without a running worker (the planning + RLS tests don't execute
// tasks).
type noopEnqueuer struct{ count int32 }

func (e *noopEnqueuer) EnqueueTask(_ context.Context, _, _, _ uuid.UUID, _ bool) error {
	atomic.AddInt32(&e.count, 1)
	return nil
}

// TestUpdateRunCreatesTasksAndEnqueues: a run over one site + two items creates
// two tasks and enqueues two jobs.
func TestUpdateRunCreatesTasksAndEnqueues(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "upd-create")
	ctx := context.Background()

	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	enq := &noopEnqueuer{}
	svc := update.NewService(update.NewRepo(pool), &svcSiteLookup{svc: siteSvc}, enq, domain.NewValidator(), domain.SystemClock{})

	// A site enrolled (no live agent needed; we don't run tasks here).
	s := enrollFakeSite(t, pool, tenant, "https://create.example.com")

	run, tasks, err := svc.CreateRun(ctx, update.CreateRunInput{
		TenantID: tenant,
		SiteIDs:  []uuid.UUID{s.ID},
		Items: []update.Item{
			{Type: "plugin", Slug: "akismet", Version: "latest"},
			{Type: "core", Version: "latest"},
		},
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("want 2 tasks, got %d", len(tasks))
	}
	if run.Status != update.RunPending {
		t.Fatalf("run status = %s, want pending", run.Status)
	}
	if atomic.LoadInt32(&enq.count) != 2 {
		t.Fatalf("enqueued %d jobs, want 2", enq.count)
	}
}

// TestUpdateRLSCrossTenantDenied proves a run/tasks created for tenant A are
// invisible to tenant B (RLS on update_runs + update_tasks).
func TestUpdateRLSCrossTenantDenied(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenantA := seedTenant(t, pool, "upd-rls-a")
	tenantB := seedTenant(t, pool, "upd-rls-b")

	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	svc := update.NewService(update.NewRepo(pool), &svcSiteLookup{svc: siteSvc}, &noopEnqueuer{}, domain.NewValidator(), domain.SystemClock{})

	sA := enrollFakeSite(t, pool, tenantA, "https://rls-a.example.com")
	runA, _, err := svc.CreateRun(ctx, update.CreateRunInput{
		TenantID: tenantA,
		SiteIDs:  []uuid.UUID{sA.ID},
		Items:    []update.Item{{Type: "plugin", Slug: "akismet"}},
	})
	if err != nil {
		t.Fatalf("create run A: %v", err)
	}

	// Tenant A can read its own run.
	if _, _, err := svc.GetRun(ctx, tenantA, runA.ID); err != nil {
		t.Fatalf("tenant A reading own run: %v", err)
	}

	// Tenant B must NOT see tenant A's run (RLS → not found).
	if _, _, err := svc.GetRun(ctx, tenantB, runA.ID); err == nil {
		t.Fatal("tenant B must not read tenant A's update run (RLS breach)")
	} else if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindNotFound {
		t.Fatalf("expected NotFound for cross-tenant read, got %v", err)
	}

	// Tenant B's run list must not include tenant A's run.
	runsB, err := svc.ListRuns(ctx, tenantB, 50, 0)
	if err != nil {
		t.Fatalf("list runs B: %v", err)
	}
	for _, r := range runsB {
		if r.ID == runA.ID {
			t.Fatal("tenant B's run list leaked tenant A's run (RLS breach)")
		}
	}

	// Tenant B's task listing for A's run must be empty (RLS on update_tasks).
	tasksB, err := update.NewRepo(pool).ListTasks(ctx, tenantB, runA.ID)
	if err != nil {
		t.Fatalf("list tasks B: %v", err)
	}
	if len(tasksB) != 0 {
		t.Fatalf("tenant B saw %d of tenant A's tasks (RLS breach)", len(tasksB))
	}
}
