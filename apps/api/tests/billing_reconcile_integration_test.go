package tests

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// TestBillingReconcile_UpgradeAppliesImmediately proves the "fail toward the
// customer" bias: when the provider's CURRENT truth is a better state than
// what is stored (a missed webhook left a free/none tenant on record even
// though the provider shows an active paid subscription), the daily
// reconcile sweep applies the upgrade immediately — no waiting, no grace
// window, no separate "upgrade" code path to get wrong (nextBillingState is
// the exact same function the webhook consumer uses).
func TestBillingReconcile_UpgradeAppliesImmediately(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-reconcile-upgrade")
	setTenantPlan(t, pool, tenant, "free", "none")
	setTenantProvider(t, pool, tenant, "fake", "cus_recon_up", "sub_recon_up")

	fp := newFakeProvider("fake")
	fp.subscriptions["sub_recon_up"] = billing.Subscription{
		ID: "sub_recon_up", CustomerID: "cus_recon_up", Plan: billing.TierAgency, PlanResolved: true,
		Status: billing.StatusActive, CurrentPeriodEnd: time.Now().Add(30 * 24 * time.Hour),
	}
	svc := newTestBillingService(pool, fp)

	result, err := svc.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.Checked != 1 || result.Repaired != 1 {
		t.Fatalf("Reconcile result = %+v, want Checked=1 Repaired=1", result)
	}

	plan, status := getTenantPlanStatus(t, pool, tenant)
	if plan != string(billing.TierAgency) || status != "active" {
		t.Fatalf("upgrade was not applied immediately: plan=%s status=%s, want agency/active", plan, status)
	}
}

// TestBillingReconcile_DowngradeGoesThroughGradedLadderNonDestructively proves
// the other half of "fail toward the customer": a MISSED cancellation is
// repaired through the SAME graded, non-destructive downgrade the webhook
// path uses (plan->free, status->canceled) — never an immediate hard cutoff,
// and the tenant's existing sites are never touched; only NEW growth is
// blocked (via the pre-existing Phase A CheckSiteCreate cap).
func TestBillingReconcile_DowngradeGoesThroughGradedLadderNonDestructively(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-reconcile-downgrade")
	setTenantPlan(t, pool, tenant, string(billing.TierAgency), "active")
	setTenantProvider(t, pool, tenant, "fake", "cus_recon_down", "sub_recon_down")

	// Seed 5 sites — above the free-tier cap of 3 — to prove the downgrade
	// repair never deletes anything.
	repo := site.NewRepo(pool)
	for i := 0; i < 5; i++ {
		if _, err := repo.Create(ctx, site.CreateInput{
			TenantID: tenant, URL: fmt.Sprintf("https://reconcile-down-%d.example.com", i), Name: "s",
		}); err != nil {
			t.Fatalf("seed site %d: %v", i, err)
		}
	}

	fp := newFakeProvider("fake")
	fp.subscriptions["sub_recon_down"] = billing.Subscription{ID: "sub_recon_down", CustomerID: "cus_recon_down", Status: billing.StatusCanceled}
	svc := newTestBillingService(pool, fp)

	result, err := svc.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.Repaired != 1 {
		t.Fatalf("Repaired = %d, want 1", result.Repaired)
	}

	plan, status := getTenantPlanStatus(t, pool, tenant)
	if plan != "free" || status != "canceled" {
		t.Fatalf("downgrade did not apply the graded, non-destructive ladder: plan=%s status=%s, want free/canceled", plan, status)
	}

	// Read under InTenantTx: sites carries tenant-isolation RLS, so a raw
	// unguarded query (no app.tenant_id GUC) would see zero rows regardless
	// of what actually exists.
	var siteCount int
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM sites WHERE tenant_id = $1`, tenant).Scan(&siteCount)
	}); err != nil {
		t.Fatalf("count sites: %v", err)
	}
	if siteCount != 5 {
		t.Fatalf("site count after the downgrade repair = %d, want 5 (non-destructive — reconcile must never delete a site)", siteCount)
	}

	// Growth is blocked (the existing Phase A site cap), but nothing existing
	// was touched.
	err = pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		return svc.CheckSiteCreate(ctx, tx, tenant)
	})
	assertSiteLimitReached(t, err, 3, 5, "free")
}

// TestBillingReconcile_NoDriftIsANoOp proves a tenant whose stored state
// already matches the provider is left untouched (Repaired=0) — the sweep
// must not re-audit/re-invalidate-cache for every already-correct tenant on
// every run.
func TestBillingReconcile_NoDriftIsANoOp(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-reconcile-nodrift")
	setTenantPlan(t, pool, tenant, string(billing.TierStarter), "active")
	setTenantProvider(t, pool, tenant, "fake", "cus_recon_same", "sub_recon_same")

	fp := newFakeProvider("fake")
	fp.subscriptions["sub_recon_same"] = billing.Subscription{
		ID: "sub_recon_same", CustomerID: "cus_recon_same", Plan: billing.TierStarter, PlanResolved: true,
		Status: billing.StatusActive, CurrentPeriodEnd: time.Now().Add(30 * 24 * time.Hour),
	}
	svc := newTestBillingService(pool, fp)

	result, err := svc.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.Checked != 1 || result.Repaired != 0 {
		t.Fatalf("Reconcile result = %+v, want Checked=1 Repaired=0 (no drift)", result)
	}
}

// TestBillingReconcile_CompedTenantNeverListed proves a comped tenant is
// excluded from the sweep's tenant set entirely (see
// ListTenantsWithProviderSubscription's plan_status <> 'comped' filter) —
// immunity applies to reconcile exactly as it does to the webhook path.
func TestBillingReconcile_CompedTenantNeverListed(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-reconcile-comped")
	setTenantPlan(t, pool, tenant, string(billing.TierAgency), "comped")
	setTenantProvider(t, pool, tenant, "fake", "cus_recon_comped", "sub_recon_comped")

	fp := newFakeProvider("fake")
	fp.subscriptions["sub_recon_comped"] = billing.Subscription{ID: "sub_recon_comped", Status: billing.StatusCanceled}
	svc := newTestBillingService(pool, fp)

	result, err := svc.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.Checked != 0 {
		t.Fatalf("Checked = %d, want 0 — a comped tenant must never even be listed for reconcile", result.Checked)
	}

	plan, status := getTenantPlanStatus(t, pool, tenant)
	if plan != string(billing.TierAgency) || status != "comped" {
		t.Fatalf("comped tenant was mutated by reconcile: plan=%s status=%s", plan, status)
	}
}
