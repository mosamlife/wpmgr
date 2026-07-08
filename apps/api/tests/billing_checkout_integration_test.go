package tests

import (
	"context"
	"log/slog"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// TestCreateCheckout_BindsToCallerTenantAndPersistsProvider proves the
// checkout flow is bound to the AUTHENTICATED caller's tenant (never a
// client-suppliable value — CreateCheckout takes tenantID as an explicit Go
// parameter sourced by the HTTP handler from the request's Principal, never
// from the request body) and that the provider adapter receives EXACTLY that
// tenant/tier, with no way for the caller to have named a price directly.
// It also proves "one tenant = one provider at a time (set at first
// checkout)": billing_provider is persisted after the first successful
// checkout.
func TestCreateCheckout_BindsToCallerTenantAndPersistsProvider(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-checkout-binding")

	fp := newFakeProvider("fake")
	svc := newTestBillingService(pool, fp)

	sess, err := svc.CreateCheckout(ctx, tenant, billing.TierAgency, "", "", "owner@example.com",
		"https://cp.example.com/billing?checkout=success", "https://cp.example.com/billing?checkout=cancel",
		billing.Actor{Type: "user", ID: "user-1"})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	if sess.URL == "" {
		t.Fatal("expected a non-empty checkout URL")
	}

	if fp.lastCheckoutInput.TenantID != tenant {
		t.Fatalf("provider received TenantID=%s, want the caller's own tenant %s", fp.lastCheckoutInput.TenantID, tenant)
	}
	if fp.lastCheckoutInput.Plan != billing.TierAgency {
		t.Fatalf("provider received Plan=%q, want agency", fp.lastCheckoutInput.Plan)
	}
	if fp.lastCheckoutInput.CustomerEmail != "owner@example.com" {
		t.Fatalf("provider received CustomerEmail=%q", fp.lastCheckoutInput.CustomerEmail)
	}

	var billingProvider string
	if err := pool.QueryRow(ctx, `SELECT billing_provider FROM tenants WHERE id = $1`, tenant).Scan(&billingProvider); err != nil {
		t.Fatalf("read billing_provider: %v", err)
	}
	if billingProvider != "fake" {
		t.Fatalf("tenants.billing_provider = %q, want %q to be recorded after the first checkout", billingProvider, "fake")
	}
}

// TestCreateCheckout_SecondCheckoutDoesNotRepointProvider proves "one tenant
// = one provider at a time": a second checkout call never re-points an
// already-provider-bound tenant at a different provider name.
func TestCreateCheckout_SecondCheckoutDoesNotRepointProvider(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-checkout-repoint")
	setTenantProvider(t, pool, tenant, "fake", "", "")

	fp := newFakeProvider("fake")
	svc := newTestBillingService(pool, fp)

	if _, err := svc.CreateCheckout(ctx, tenant, billing.TierStarter, "", "", "", "https://s", "https://c", billing.Actor{}); err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}

	var billingProvider string
	if err := pool.QueryRow(ctx, `SELECT billing_provider FROM tenants WHERE id = $1`, tenant).Scan(&billingProvider); err != nil {
		t.Fatalf("read billing_provider: %v", err)
	}
	if billingProvider != "fake" {
		t.Fatalf("billing_provider = %q, want it to remain %q (already set before this checkout)", billingProvider, "fake")
	}
}

// TestCreateCheckout_RequestedProviderIsUsedOnFirstCheckout proves the
// customer-chooses-provider-at-checkout contract: a tenant with NO pinned
// provider yet gets the CALLER-REQUESTED provider (not silently the
// instance's default), and only the requested provider's adapter is ever
// invoked.
func TestCreateCheckout_RequestedProviderIsUsedOnFirstCheckout(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-checkout-requested-provider")

	fpDefault := newFakeProvider("fake-default")
	fpChosen := newFakeProvider("fake-chosen")
	svc := billing.New(pool, nil, true, domain.SystemClock{}, slog.Default())
	svc.SetProviders(billing.NewRegistry(fpDefault, fpChosen), fpDefault.Name())

	sess, err := svc.CreateCheckout(ctx, tenant, billing.TierStarter, "fake-chosen", "", "", "https://s", "https://c", billing.Actor{})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	if sess.URL == "" {
		t.Fatal("expected a non-empty checkout URL")
	}
	if fpChosen.lastCheckoutInput.TenantID != tenant {
		t.Fatal("the REQUESTED provider should have received the checkout call")
	}
	if fpDefault.lastCheckoutInput.TenantID == tenant {
		t.Fatal("the default provider should NOT have received the checkout call when a different provider was requested")
	}

	var billingProvider string
	if err := pool.QueryRow(ctx, `SELECT billing_provider FROM tenants WHERE id = $1`, tenant).Scan(&billingProvider); err != nil {
		t.Fatalf("read billing_provider: %v", err)
	}
	if billingProvider != "fake-chosen" {
		t.Fatalf("billing_provider = %q, want fake-chosen", billingProvider)
	}
}

// TestCreateCheckout_PinnedProviderWinsOverRequestedProvider proves "one
// tenant = one provider at a time" holds even against an explicit
// caller-requested provider: an already-pinned tenant can never be
// re-pointed at a different provider via the request body.
func TestCreateCheckout_PinnedProviderWinsOverRequestedProvider(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-checkout-pinned-wins")
	setTenantProvider(t, pool, tenant, "fake-default", "", "")

	fpDefault := newFakeProvider("fake-default")
	fpOther := newFakeProvider("fake-other")
	svc := billing.New(pool, nil, true, domain.SystemClock{}, slog.Default())
	svc.SetProviders(billing.NewRegistry(fpDefault, fpOther), fpDefault.Name())

	if _, err := svc.CreateCheckout(ctx, tenant, billing.TierStarter, "fake-other", "", "", "https://s", "https://c", billing.Actor{}); err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}

	if fpOther.lastCheckoutInput.TenantID == tenant {
		t.Fatal("an already-pinned provider must win over a caller-requested one — 'fake-other' should never have been called")
	}
	var billingProvider string
	if err := pool.QueryRow(ctx, `SELECT billing_provider FROM tenants WHERE id = $1`, tenant).Scan(&billingProvider); err != nil {
		t.Fatalf("read billing_provider: %v", err)
	}
	if billingProvider != "fake-default" {
		t.Fatalf("billing_provider = %q, want it to remain fake-default (pinned before this checkout)", billingProvider)
	}
}

// TestGetBillingSummary_PortalAvailable_TrueForPortalHavingProvider proves
// the default (Stripe-like) case: a provider that HasPortal()==true, with an
// existing provider customer id, reports PortalAvailable=true.
func TestGetBillingSummary_PortalAvailable_TrueForPortalHavingProvider(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-summary-portal-true")
	setTenantProvider(t, pool, tenant, "fake", "cus_1", "")

	fp := newFakeProvider("fake")
	svc := newTestBillingService(pool, fp)

	summary, err := svc.GetBillingSummary(ctx, tenant)
	if err != nil {
		t.Fatalf("GetBillingSummary: %v", err)
	}
	if !summary.PortalAvailable {
		t.Fatal("PortalAvailable should be true for a provider with HasPortal()==true and an existing customer id")
	}
}

// TestGetBillingSummary_PortalAvailable_FalseForPortallessProvider is the
// Razorpay regression guard: a provider that HasPortal()==false must NEVER
// be advertised as having a portal, even with an existing provider customer
// id on file.
func TestGetBillingSummary_PortalAvailable_FalseForPortallessProvider(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-summary-portal-false")
	setTenantProvider(t, pool, tenant, "fake", "cus_1", "")

	fp := newFakeProvider("fake")
	fp.noPortal = true
	svc := newTestBillingService(pool, fp)

	summary, err := svc.GetBillingSummary(ctx, tenant)
	if err != nil {
		t.Fatalf("GetBillingSummary: %v", err)
	}
	if summary.PortalAvailable {
		t.Fatal("PortalAvailable should be false for a provider whose HasPortal()==false (e.g. Razorpay)")
	}
}
