package tests

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// tenants-table test setup helpers (tenants carries no RLS — plain SQL).
// ---------------------------------------------------------------------------

func setTenantPlan(t *testing.T, pool *db.Pool, tenantID uuid.UUID, plan, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE tenants SET plan = $1, plan_status = $2 WHERE id = $3`, plan, status, tenantID); err != nil {
		t.Fatalf("setTenantPlan: %v", err)
	}
}

func setTenantProvider(t *testing.T, pool *db.Pool, tenantID uuid.UUID, provider, customerID, subscriptionID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE tenants SET billing_provider = $1, provider_customer_id = $2, provider_subscription_id = $3 WHERE id = $4`,
		provider, customerID, subscriptionID, tenantID); err != nil {
		t.Fatalf("setTenantProvider: %v", err)
	}
}

func getTenantPlanStatus(t *testing.T, pool *db.Pool, tenantID uuid.UUID) (plan, status string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT plan, plan_status FROM tenants WHERE id = $1`, tenantID).Scan(&plan, &status); err != nil {
		t.Fatalf("getTenantPlanStatus: %v", err)
	}
	return plan, status
}

// countBillingEvents reads under InAgentTx: billing_events carries the m91
// tenant/system RLS pairing, and a raw unguarded query (no app.tenant_id or
// app.agent GUC) sees ZERO rows regardless of what was actually inserted —
// this is the same cross-tenant "system observer" context
// Service.insertBillingEvent itself writes under.
func countBillingEvents(t *testing.T, pool *db.Pool, provider, providerEventID string) int {
	t.Helper()
	var n int
	err := pool.InAgentTx(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM billing_events WHERE provider = $1 AND provider_event_id = $2`,
			provider, providerEventID).Scan(&n)
	})
	if err != nil {
		t.Fatalf("countBillingEvents: %v", err)
	}
	return n
}

func newTestBillingService(pool *db.Pool, provider *fakeProvider) *billing.Service {
	svc := billing.New(pool, nil, true, domain.SystemClock{}, slog.Default())
	svc.SetProviders(billing.NewRegistry(provider), provider.Name())
	return svc
}

// ---------------------------------------------------------------------------
// Signature / provider-resolution failures (no DB access needed for these —
// the checks happen before any billing_events write).
// ---------------------------------------------------------------------------

func TestProcessWebhook_SignatureFailureMapsToUnauthorized(t *testing.T) {
	fp := newFakeProvider("fake")
	fp.verifyErr = errFakeSubscriptionNotFound // any non-nil error stands in for "bad signature"
	svc := newTestBillingService(nil, fp)

	err := svc.ProcessWebhook(context.Background(), "fake", []byte(`{}`), http.Header{})
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindUnauthorized {
		t.Fatalf("want a KindUnauthorized error for a signature-verification failure, got %v", err)
	}
}

func TestProcessWebhook_UnknownProviderIsNotFound(t *testing.T) {
	svc := billing.New(nil, nil, true, domain.SystemClock{}, slog.Default())
	svc.SetProviders(billing.NewRegistry(), "fake") // registry built, nothing registered

	err := svc.ProcessWebhook(context.Background(), "some-unknown-provider", []byte(`{}`), http.Header{})
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindNotFound {
		t.Fatalf("want a KindNotFound error for an unrecognized provider, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Duplicate event_id: idempotent.
// ---------------------------------------------------------------------------

func TestBillingWebhook_DuplicateEventIDIsIdempotent(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-webhook-dup")
	setTenantProvider(t, pool, tenant, "fake", "cus_dup", "")

	fp := newFakeProvider("fake")
	fp.subscriptions["sub_dup"] = billing.Subscription{
		ID: "sub_dup", CustomerID: "cus_dup", Plan: billing.TierStarter, PlanResolved: true,
		Status: billing.StatusActive, CurrentPeriodEnd: time.Now().Add(30 * 24 * time.Hour),
	}
	svc := newTestBillingService(pool, fp)

	body := fakeEventBody(fakeEventPayload{
		ID: "evt_dup_1", Type: "customer.subscription.updated", Kind: "activated", Handled: true,
		TenantID: tenant, ProviderCustomerID: "cus_dup", ProviderSubscriptionID: "sub_dup", OccurredAt: time.Now(),
	})

	if err := svc.ProcessWebhook(ctx, "fake", body, http.Header{}); err != nil {
		t.Fatalf("first ProcessWebhook: %v", err)
	}
	if err := svc.ProcessWebhook(ctx, "fake", body, http.Header{}); err != nil {
		t.Fatalf("duplicate delivery should be a clean idempotent no-op, got error: %v", err)
	}

	if got := countBillingEvents(t, pool, "fake", "evt_dup_1"); got != 1 {
		t.Fatalf("billing_events rows for evt_dup_1 = %d, want exactly 1 (ON CONFLICT DO NOTHING)", got)
	}
	if calls := atomic.LoadInt32(&fp.getSubscriptionCalls); calls != 1 {
		t.Fatalf("GetSubscription calls = %d, want 1 — the duplicate delivery must be recognized and dropped BEFORE any re-fetch/re-apply", calls)
	}
}

// ---------------------------------------------------------------------------
// Out-of-order delivery: ignored.
// ---------------------------------------------------------------------------

func TestBillingWebhook_OutOfOrderEventIgnored(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-webhook-ooo")
	setTenantProvider(t, pool, tenant, "fake", "cus_ooo", "")

	fp := newFakeProvider("fake")
	fp.subscriptions["sub_ooo"] = billing.Subscription{
		ID: "sub_ooo", CustomerID: "cus_ooo", Plan: billing.TierStarter, PlanResolved: true, Status: billing.StatusPastDue,
	}
	svc := newTestBillingService(pool, fp)

	newer := time.Now()
	older := newer.Add(-time.Hour)

	// The NEWER event is delivered and processed first.
	bodyNewer := fakeEventBody(fakeEventPayload{
		ID: "evt_ooo_newer", Type: "invoice.payment_failed", Kind: "past_due", Handled: true,
		TenantID: tenant, ProviderCustomerID: "cus_ooo", ProviderSubscriptionID: "sub_ooo", OccurredAt: newer,
	})
	if err := svc.ProcessWebhook(ctx, "fake", bodyNewer, http.Header{}); err != nil {
		t.Fatalf("newer event: %v", err)
	}
	if calls := atomic.LoadInt32(&fp.getSubscriptionCalls); calls != 1 {
		t.Fatalf("GetSubscription calls after the newer event = %d, want 1", calls)
	}
	_, status := getTenantPlanStatus(t, pool, tenant)
	if status != "past_due" {
		t.Fatalf("status after the newer event = %q, want past_due", status)
	}

	// An OLDER event (a different provider_event_id — a genuinely delayed,
	// out-of-order delivery, NOT a duplicate) arrives second.
	bodyOlder := fakeEventBody(fakeEventPayload{
		ID: "evt_ooo_older", Type: "invoice.payment_failed", Kind: "past_due", Handled: true,
		TenantID: tenant, ProviderCustomerID: "cus_ooo", ProviderSubscriptionID: "sub_ooo", OccurredAt: older,
	})
	if err := svc.ProcessWebhook(ctx, "fake", bodyOlder, http.Header{}); err != nil {
		t.Fatalf("older (out-of-order) event: %v", err)
	}

	if calls := atomic.LoadInt32(&fp.getSubscriptionCalls); calls != 1 {
		t.Fatalf("GetSubscription calls after the out-of-order event = %d, want STILL 1 — "+
			"the out-of-order guard must short-circuit before ever re-fetching/re-applying", calls)
	}
}

// ---------------------------------------------------------------------------
// Comped-tenant immunity.
// ---------------------------------------------------------------------------

func TestBillingWebhook_CompedTenantImmunity(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-webhook-comped")
	setTenantPlan(t, pool, tenant, string(billing.TierAgency), "comped")
	setTenantProvider(t, pool, tenant, "fake", "cus_comped", "")

	fp := newFakeProvider("fake")
	// A subscription state that WOULD downgrade a normal tenant to free/canceled.
	fp.subscriptions["sub_comped"] = billing.Subscription{ID: "sub_comped", Status: billing.StatusCanceled}
	svc := newTestBillingService(pool, fp)

	body := fakeEventBody(fakeEventPayload{
		ID: "evt_comped_1", Type: "customer.subscription.deleted", Kind: "canceled", Handled: true,
		TenantID: tenant, ProviderCustomerID: "cus_comped", ProviderSubscriptionID: "sub_comped", OccurredAt: time.Now(),
	})
	if err := svc.ProcessWebhook(ctx, "fake", body, http.Header{}); err != nil {
		t.Fatalf("ProcessWebhook: %v", err)
	}

	plan, status := getTenantPlanStatus(t, pool, tenant)
	if plan != string(billing.TierAgency) || status != "comped" {
		t.Fatalf("a comped tenant was mutated by a webhook: plan=%s status=%s, want agency/comped unchanged", plan, status)
	}
	if calls := atomic.LoadInt32(&fp.getSubscriptionCalls); calls != 0 {
		t.Fatalf("GetSubscription calls = %d, want 0 — a comped tenant must never even reach the provider fetch", calls)
	}
}

// ---------------------------------------------------------------------------
// Unknown price: no-op.
// ---------------------------------------------------------------------------

func TestBillingWebhook_UnknownPriceNoOp(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-webhook-unknownprice")
	setTenantPlan(t, pool, tenant, string(billing.TierStarter), "active")
	setTenantProvider(t, pool, tenant, "fake", "cus_up", "sub_up")

	fp := newFakeProvider("fake")
	// PlanResolved=false: the subscription's price does not map to any tier.
	fp.subscriptions["sub_up"] = billing.Subscription{ID: "sub_up", CustomerID: "cus_up", Status: billing.StatusActive, PlanResolved: false}
	svc := newTestBillingService(pool, fp)

	body := fakeEventBody(fakeEventPayload{
		ID: "evt_unknownprice_1", Type: "customer.subscription.updated", Kind: "updated", Handled: true,
		TenantID: tenant, ProviderCustomerID: "cus_up", ProviderSubscriptionID: "sub_up", OccurredAt: time.Now(),
	})
	if err := svc.ProcessWebhook(ctx, "fake", body, http.Header{}); err != nil {
		t.Fatalf("ProcessWebhook: %v", err)
	}

	plan, status := getTenantPlanStatus(t, pool, tenant)
	if plan != string(billing.TierStarter) || status != "active" {
		t.Fatalf("tenant was mutated despite an unresolved price: plan=%s status=%s, want starter/active unchanged", plan, status)
	}
}

// ---------------------------------------------------------------------------
// Unknown customer: no-op (recorded, never resolved to any tenant).
// ---------------------------------------------------------------------------

func TestBillingWebhook_UnknownCustomerNoOp(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	fp := newFakeProvider("fake")
	svc := newTestBillingService(pool, fp)

	body := fakeEventBody(fakeEventPayload{
		ID: "evt_unknown_customer", Type: "invoice.paid", Kind: "payment_succeeded", Handled: true,
		// TenantID left as uuid.Nil, and no tenant in the DB has this customer id.
		ProviderCustomerID: "cus_ghost", ProviderSubscriptionID: "sub_ghost", OccurredAt: time.Now(),
	})
	if err := svc.ProcessWebhook(ctx, "fake", body, http.Header{}); err != nil {
		t.Fatalf("ProcessWebhook should not error for an unresolvable customer: %v", err)
	}

	if calls := atomic.LoadInt32(&fp.getSubscriptionCalls); calls != 0 {
		t.Fatalf("GetSubscription calls = %d, want 0 — an unresolvable tenant must never reach the provider fetch", calls)
	}
	if got := countBillingEvents(t, pool, "fake", "evt_unknown_customer"); got != 1 {
		t.Fatalf("billing_events rows = %d, want 1 — still ledgered even though it could not be attributed", got)
	}
}
