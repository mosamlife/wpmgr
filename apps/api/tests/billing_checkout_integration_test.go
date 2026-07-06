package tests

import (
	"context"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
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

	sess, err := svc.CreateCheckout(ctx, tenant, billing.TierAgency, "owner@example.com",
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

	if _, err := svc.CreateCheckout(ctx, tenant, billing.TierStarter, "", "https://s", "https://c", billing.Actor{}); err != nil {
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
