package tests

// billing_cancel_integration_test.go — Service.CancelSubscription and the
// POST /billing/cancel route: the provider-agnostic "Cancel subscription"
// backend added so a Razorpay tenant (no hosted portal — HasPortal()==false)
// has SOME way to cancel.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// TestCancelSubscription_CallsProviderCancel proves the happy path: the
// tenant's OWN provider (not some other registered one) is called, with the
// tenant's OWN stored provider_subscription_id — never a caller-suppliable
// value.
func TestCancelSubscription_CallsProviderCancel(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-cancel-happy")
	setTenantProvider(t, pool, tenant, "fake", "cus_cancel", "sub_cancel_1")

	fp := newFakeProvider("fake")
	svc := newTestBillingService(pool, fp)

	if err := svc.CancelSubscription(ctx, tenant, billing.Actor{Type: "user", ID: "user-1"}); err != nil {
		t.Fatalf("CancelSubscription: %v", err)
	}
	if fp.lastCancelSubscriptionID != "sub_cancel_1" {
		t.Fatalf("provider.CancelSubscription called with %q, want sub_cancel_1", fp.lastCancelSubscriptionID)
	}
}

// TestCancelSubscription_ProviderErrorPropagates proves a provider-side
// cancel failure (e.g. the provider API rejects it) surfaces to the caller
// rather than being swallowed.
func TestCancelSubscription_ProviderErrorPropagates(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-cancel-providererr")
	setTenantProvider(t, pool, tenant, "fake", "cus_cancel_err", "sub_cancel_err")

	fp := newFakeProvider("fake")
	fp.cancelErr = domain.Internal("fake_cancel_failed", "the fake provider rejected the cancellation")
	svc := newTestBillingService(pool, fp)

	if err := svc.CancelSubscription(ctx, tenant, billing.Actor{}); err == nil {
		t.Fatal("expected the provider's cancel error to propagate")
	}
}

// TestCancelSubscription_NoActiveSubscriptionReturnsCleanError proves a
// tenant that never checked out (no provider, no subscription id) gets a
// clean domain error rather than attempting a nonsensical provider call.
func TestCancelSubscription_NoActiveSubscriptionReturnsCleanError(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-cancel-none")

	fp := newFakeProvider("fake")
	svc := newTestBillingService(pool, fp)

	err := svc.CancelSubscription(ctx, tenant, billing.Actor{})
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindConflict {
		t.Fatalf("want a KindConflict 'no active subscription' error, got %v", err)
	}
	if fp.lastCancelSubscriptionID != "" {
		t.Fatalf("the provider should never have been called, got lastCancelSubscriptionID=%q", fp.lastCancelSubscriptionID)
	}
}

// TestCancelSubscription_DoesNotMutateTenantPlanDirectly is the "pull is the
// truth" regression guard: CancelSubscription tells the provider to cancel,
// but tenants.plan/plan_status must remain EXACTLY as they were immediately
// afterward — only a SEPARATE webhook delivery is allowed to change them.
func TestCancelSubscription_DoesNotMutateTenantPlanDirectly(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-cancel-nomutate")
	setTenantPlan(t, pool, tenant, string(billing.TierAgency), "active")
	setTenantProvider(t, pool, tenant, "fake", "cus_nomutate", "sub_nomutate")

	fp := newFakeProvider("fake")
	svc := newTestBillingService(pool, fp)

	if err := svc.CancelSubscription(ctx, tenant, billing.Actor{}); err != nil {
		t.Fatalf("CancelSubscription: %v", err)
	}

	plan, status := getTenantPlanStatus(t, pool, tenant)
	if plan != string(billing.TierAgency) || status != "active" {
		t.Fatalf("plan/status changed to %s/%s immediately after CancelSubscription — it must stay agency/active until a webhook arrives", plan, status)
	}
}

// ---------------------------------------------------------------------------
// Route-level: owner-gated, tenant-scoped.
// ---------------------------------------------------------------------------

// TestBillingCancelRoute_NonOwnerForbidden proves POST /billing/cancel
// carries the SAME owner-only gate as every other /billing route.
func TestBillingCancelRoute_NonOwnerForbidden(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "billing-cancel-route-nonowner")
	setTenantProvider(t, pool, tenant, "fake", "cus_1", "sub_1")

	fp := newFakeProvider("fake")
	svc := newTestBillingService(pool, fp)
	h := billing.NewHandler(svc, nil, "https://cp.example.com")

	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		p := domain.Principal{
			Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant,
			Role: "admin", Scope: domain.ScopeOrg,
		}
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	v1 := engine.Group("/api/v1")
	h.Register(v1)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/billing/cancel", nil)
	engine.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("POST /api/v1/billing/cancel (admin role) = %d, want 403", w.Code)
	}
	if fp.lastCancelSubscriptionID != "" {
		t.Fatal("a forbidden request must never reach the provider")
	}
}

// TestBillingCancelRoute_OwnerReachesHandlerAndCallsProvider proves the owner
// path end to end: 200 + {"ok":true}, and the fake provider's
// CancelSubscription was actually invoked with the tenant's own subscription
// id.
func TestBillingCancelRoute_OwnerReachesHandlerAndCallsProvider(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "billing-cancel-route-owner")
	setTenantProvider(t, pool, tenant, "fake", "cus_owner", "sub_owner_1")

	fp := newFakeProvider("fake")
	svc := newTestBillingService(pool, fp)
	h := billing.NewHandler(svc, nil, "https://cp.example.com")

	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		p := domain.Principal{
			Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant,
			Role: "owner", Scope: domain.ScopeOrg,
		}
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	v1 := engine.Group("/api/v1")
	h.Register(v1)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/billing/cancel", nil)
	engine.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/billing/cancel (owner) = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if want := `{"ok":true}`; w.Body.String() != want {
		t.Fatalf("body = %q, want %q (PINNED contract)", w.Body.String(), want)
	}
	if fp.lastCancelSubscriptionID != "sub_owner_1" {
		t.Fatalf("provider.CancelSubscription called with %q, want sub_owner_1", fp.lastCancelSubscriptionID)
	}
}
