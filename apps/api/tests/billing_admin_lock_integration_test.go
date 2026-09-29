package tests

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// TestBillingAdminWrites_WaitForTheTenantBillingLock: while another
// transaction holds a tenant's billing lock, an operator billing write for
// that tenant blocks, and it completes once the lock is released.
func TestBillingAdminWrites_WaitForTheTenantBillingLock(t *testing.T) {
	app := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, app, "billing-admin-lock")

	fp := newFakeProvider("fake")
	svc := newAdminBillingService(app, newTestBillingService(app, fp), audit.NewRecorder(app, domain.SystemClock{}))

	holder, err := app.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if err := billing.LockTenantBilling(ctx, holder, tenant); err != nil {
		t.Fatalf("take lock: %v", err)
	}

	var done atomic.Bool
	errc := make(chan error, 1)
	go func() {
		err := svc.ForceAccountState(ctx, uuid.New(), tenant, billing.TierAgency, billing.StatusActive, "lock proof")
		done.Store(true)
		errc <- err
	}()

	time.Sleep(500 * time.Millisecond)
	if done.Load() {
		t.Fatal("an operator billing write completed while another transaction held the tenant billing lock")
	}
	if plan, _ := getTenantPlanStatus(t, app, tenant); plan != "free" {
		t.Fatalf("plan changed to %s while the lock was held", plan)
	}

	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release lock: %v", err)
	}
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("ForceAccountState after release: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the operator write did not complete after the lock was released")
	}
	if plan, status := getTenantPlanStatus(t, app, tenant); plan != string(billing.TierAgency) || status != "active" {
		t.Fatalf("plan/status = %s/%s, want agency/active", plan, status)
	}
}

// TestAdminBilling_RevokeComp_WithSubscriptionEnqueuesRefresh: revoking a
// comp for a tenant with a stored subscription makes no provider call; it
// sets free/none and enqueues a refresh, and the refresh adopts the live
// subscription.
func TestAdminBilling_RevokeComp_WithSubscriptionEnqueuesRefresh(t *testing.T) {
	app := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, app, "revoke-comp-sub-"+uuid.NewString()[:8])
	setTenantPlan(t, app, tenant, string(billing.TierScale), "comped")
	setTenantProvider(t, app, tenant, "fake", "cus_rc", "sub_rc")

	fp := newFakeProvider("fake")
	fp.subscriptions["sub_rc"] = billing.Subscription{
		ID: "sub_rc", CustomerID: "cus_rc", Plan: billing.TierStarter, PlanResolved: true,
		Status: billing.StatusActive, CurrentPeriodEnd: time.Now().Add(30 * 24 * time.Hour),
	}
	h := newBillingHarness(t, app, fp)
	if err := h.client.Stop(ctx); err != nil {
		t.Fatalf("stop river: %v", err)
	}
	svc := newAdminBillingService(app, h.svc, audit.NewRecorder(app, domain.SystemClock{}))

	if err := svc.RevokeComp(ctx, uuid.New(), tenant, "comp ended"); err != nil {
		t.Fatalf("RevokeComp: %v", err)
	}
	if calls := atomic.LoadInt32(&fp.getSubscriptionCalls); calls != 0 {
		t.Fatalf("GetSubscription calls during RevokeComp = %d, want 0", calls)
	}
	if plan, status := getTenantPlanStatus(t, app, tenant); plan != "free" || status != "none" {
		t.Fatalf("plan/status right after RevokeComp = %s/%s, want free/none", plan, status)
	}
	var source string
	if err := app.QueryRow(ctx,
		`SELECT args->>'source' FROM river_job WHERE kind = 'billing_refresh' AND (args->>'tenant_id')::uuid = $1`,
		tenant).Scan(&source); err != nil {
		t.Fatalf("read enqueued refresh: %v", err)
	}
	if source != billing.RefreshSourceRevokeComp {
		t.Fatalf("refresh source = %q, want %q", source, billing.RefreshSourceRevokeComp)
	}
	entry := lastAuditEntry(t, app, tenant, audit.ActionAdminBillingCompRevoked)
	if entry == nil {
		t.Fatalf("no audit entry recorded for %s", audit.ActionAdminBillingCompRevoked)
	}

	// A second revoke finds the tenant no longer comped.
	err := svc.RevokeComp(ctx, uuid.New(), tenant, "again")
	if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindConflict {
		t.Fatalf("second RevokeComp = %v, want a conflict", err)
	}

	// Work the refresh: it adopts the live subscription.
	h2 := newBillingHarness(t, app, fp)
	h2.drain(t)
	if plan, status := getTenantPlanStatus(t, app, tenant); plan != string(billing.TierStarter) || status != "active" {
		t.Fatalf("plan/status after the refresh = %s/%s, want starter/active", plan, status)
	}
}
