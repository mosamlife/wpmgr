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

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
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
	var sub any
	if subscriptionID != "" {
		sub = subscriptionID
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE tenants SET billing_provider = $1, provider_customer_id = $2, provider_subscription_id = $3 WHERE id = $4`,
		provider, customerID, sub, tenantID); err != nil {
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
// this is the same cross-tenant "system observer" context intake itself
// writes under.
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

// billingEventState reads one ledger row's stored tenant and whether it was
// processed, under InAgentTx.
func billingEventState(t *testing.T, pool *db.Pool, provider, providerEventID string) (tenant *uuid.UUID, processed bool, payload map[string]any) {
	t.Helper()
	err := pool.InAgentTx(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT tenant_id, processed_at IS NOT NULL, payload FROM billing_events WHERE provider = $1 AND provider_event_id = $2`,
			provider, providerEventID).Scan(&tenant, &processed, &payload)
	})
	if err != nil {
		t.Fatalf("billingEventState: %v", err)
	}
	return tenant, processed, payload
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
// Intake acknowledges fast; the apply worker does the work.
// ---------------------------------------------------------------------------

func TestBillingWebhook_IntakeOnlyRecordsAndEnqueues(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-webhook-fastack")
	setTenantProvider(t, pool, tenant, "fake", "cus_fa", "")

	fp := newFakeProvider("fake")
	fp.subscriptions["sub_fa"] = billing.Subscription{
		ID: "sub_fa", CustomerID: "cus_fa", Plan: billing.TierStarter, PlanResolved: true,
		Status: billing.StatusActive, CurrentPeriodEnd: time.Now().Add(30 * 24 * time.Hour),
	}
	// The River client is stopped, so nothing works the job: intake alone
	// must record and enqueue, and must not call the provider or change the
	// tenant.
	h := newBillingHarness(t, pool, fp)
	if err := h.client.Stop(ctx); err != nil {
		t.Fatalf("stop river: %v", err)
	}

	body := fakeEventBody(fakeEventPayload{
		ID: "evt_fa_1", Type: "customer.subscription.created", Kind: "activated", Handled: true,
		TenantID: tenant, ProviderCustomerID: "cus_fa", ProviderSubscriptionID: "sub_fa", OccurredAt: time.Now(),
	})
	if err := h.svc.ProcessWebhook(ctx, "fake", body, http.Header{}); err != nil {
		t.Fatalf("ProcessWebhook: %v", err)
	}
	if calls := atomic.LoadInt32(&fp.getSubscriptionCalls); calls != 0 {
		t.Fatalf("GetSubscription calls during intake = %d, want 0", calls)
	}
	if plan, status := getTenantPlanStatus(t, pool, tenant); plan != "free" || status != "none" {
		t.Fatalf("intake changed the tenant: plan=%s status=%s, want free/none", plan, status)
	}
	if n := countBillingJobs(t, pool, "billing_apply", "available"); n != 1 {
		t.Fatalf("available billing_apply jobs = %d, want 1", n)
	}
	stored, processed, payload := billingEventState(t, pool, "fake", "evt_fa_1")
	if stored == nil || *stored != tenant || processed {
		t.Fatalf("ledger row tenant=%v processed=%t, want %s and unprocessed", stored, processed, tenant)
	}
	if payload["claimed_tenant_id"] != tenant.String() {
		t.Fatalf("payload claimed_tenant_id = %v, want %s", payload["claimed_tenant_id"], tenant)
	}
}

func TestBillingWebhook_ActivationAppliesAndAuditsOnce(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-webhook-activate")
	setTenantProvider(t, pool, tenant, "fake", "cus_act", "")

	fp := newFakeProvider("fake")
	fp.subscriptions["sub_act"] = billing.Subscription{
		ID: "sub_act", CustomerID: "cus_act", Plan: billing.TierAgency, PlanResolved: true,
		Status: billing.StatusActive, CurrentPeriodEnd: time.Now().Add(30 * 24 * time.Hour),
	}
	h := newBillingHarness(t, pool, fp)
	h.svc.SetAudit(audit.NewRecorder(pool, domain.SystemClock{}))

	body := fakeEventBody(fakeEventPayload{
		ID: "evt_act_1", Type: "customer.subscription.created", Kind: "activated", Handled: true,
		TenantID: tenant, ProviderCustomerID: "cus_act", ProviderSubscriptionID: "sub_act", OccurredAt: time.Now(),
	})
	if err := h.svc.ProcessWebhook(ctx, "fake", body, http.Header{}); err != nil {
		t.Fatalf("ProcessWebhook: %v", err)
	}
	h.drain(t)

	if plan, status := getTenantPlanStatus(t, pool, tenant); plan != string(billing.TierAgency) || status != "active" {
		t.Fatalf("plan/status = %s/%s, want agency/active", plan, status)
	}
	if _, processed, _ := billingEventState(t, pool, "fake", "evt_act_1"); !processed {
		t.Fatal("a successfully applied event must be marked processed")
	}
	if n := countAuditEntries(t, pool, tenant, "billing.subscription.changed"); n != 1 {
		t.Fatalf("billing.subscription.changed audit entries = %d, want 1", n)
	}

	// Re-running the audit job with the same key appends nothing.
	if _, err := h.client.Insert(ctx, billing.BillingAuditArgs{
		TenantID: tenant, ActorID: "fake", Action: "billing.subscription.changed",
		AuditKey: "billing_event:fake:evt_act_1", Metadata: map[string]any{"source": "webhook"},
	}, nil); err != nil {
		t.Fatalf("insert duplicate audit job: %v", err)
	}
	h.drain(t)
	if n := countAuditEntries(t, pool, tenant, "billing.subscription.changed"); n != 1 {
		t.Fatalf("audit entries after a repeated audit job = %d, want still 1", n)
	}
}

func countAuditEntries(t *testing.T, pool *db.Pool, tenantID uuid.UUID, action string) int {
	t.Helper()
	var n int
	err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2`, tenantID, action).Scan(&n)
	})
	if err != nil {
		t.Fatalf("countAuditEntries: %v", err)
	}
	return n
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
	h := newBillingHarness(t, pool, fp)

	body := fakeEventBody(fakeEventPayload{
		ID: "evt_dup_1", Type: "customer.subscription.updated", Kind: "activated", Handled: true,
		TenantID: tenant, ProviderCustomerID: "cus_dup", ProviderSubscriptionID: "sub_dup", OccurredAt: time.Now(),
	})

	if err := h.svc.ProcessWebhook(ctx, "fake", body, http.Header{}); err != nil {
		t.Fatalf("first ProcessWebhook: %v", err)
	}
	h.drain(t)
	if err := h.svc.ProcessWebhook(ctx, "fake", body, http.Header{}); err != nil {
		t.Fatalf("duplicate delivery should be a clean idempotent no-op, got error: %v", err)
	}
	h.drain(t)

	if got := countBillingEvents(t, pool, "fake", "evt_dup_1"); got != 1 {
		t.Fatalf("billing_events rows for evt_dup_1 = %d, want exactly 1 (ON CONFLICT DO NOTHING)", got)
	}
	if calls := atomic.LoadInt32(&fp.getSubscriptionCalls); calls != 1 {
		t.Fatalf("GetSubscription calls = %d, want 1 — a duplicate of a processed event must not be applied again", calls)
	}
}

// ---------------------------------------------------------------------------
// Delivery order: every event re-reads current provider state.
// ---------------------------------------------------------------------------

// TestBillingWebhook_OlderEventStillAppliesCurrentState replaces the former
// out-of-order test. An event whose occurred_at is older than one already
// applied is still applied, and what it applies is the provider's CURRENT
// state, so delivery order cannot move a tenant backwards.
func TestBillingWebhook_OlderEventStillAppliesCurrentState(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-webhook-order")
	setTenantProvider(t, pool, tenant, "fake", "cus_ooo", "")

	fp := newFakeProvider("fake")
	fp.subscriptions["sub_ooo"] = billing.Subscription{
		ID: "sub_ooo", CustomerID: "cus_ooo", Plan: billing.TierStarter, PlanResolved: true, Status: billing.StatusPastDue,
	}
	h := newBillingHarness(t, pool, fp)

	newer := time.Now()
	older := newer.Add(-time.Hour)

	bodyNewer := fakeEventBody(fakeEventPayload{
		ID: "evt_ooo_newer", Type: "invoice.payment_failed", Kind: "past_due", Handled: true,
		TenantID: tenant, ProviderCustomerID: "cus_ooo", ProviderSubscriptionID: "sub_ooo", OccurredAt: newer,
	})
	if err := h.svc.ProcessWebhook(ctx, "fake", bodyNewer, http.Header{}); err != nil {
		t.Fatalf("newer event: %v", err)
	}
	h.drain(t)
	if _, status := getTenantPlanStatus(t, pool, tenant); status != "past_due" {
		t.Fatalf("status after the newer event = %q, want past_due", status)
	}

	// The provider has since recovered the payment.
	fp.subscriptions["sub_ooo"] = billing.Subscription{
		ID: "sub_ooo", CustomerID: "cus_ooo", Plan: billing.TierStarter, PlanResolved: true, Status: billing.StatusActive,
	}
	bodyOlder := fakeEventBody(fakeEventPayload{
		ID: "evt_ooo_older", Type: "invoice.payment_failed", Kind: "past_due", Handled: true,
		TenantID: tenant, ProviderCustomerID: "cus_ooo", ProviderSubscriptionID: "sub_ooo", OccurredAt: older,
	})
	if err := h.svc.ProcessWebhook(ctx, "fake", bodyOlder, http.Header{}); err != nil {
		t.Fatalf("older event: %v", err)
	}
	h.drain(t)

	if calls := atomic.LoadInt32(&fp.getSubscriptionCalls); calls != 2 {
		t.Fatalf("GetSubscription calls = %d, want 2: every event re-reads the provider", calls)
	}
	if _, status := getTenantPlanStatus(t, pool, tenant); status != "active" {
		t.Fatalf("status after the older event = %q, want active (the provider's current state)", status)
	}
	if _, processed, _ := billingEventState(t, pool, "fake", "evt_ooo_older"); !processed {
		t.Fatal("the older event must be marked processed")
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
	fp.subscriptions["sub_comped"] = billing.Subscription{ID: "sub_comped", Status: billing.StatusCanceled}
	h := newBillingHarness(t, pool, fp)

	body := fakeEventBody(fakeEventPayload{
		ID: "evt_comped_1", Type: "customer.subscription.deleted", Kind: "canceled", Handled: true,
		TenantID: tenant, ProviderCustomerID: "cus_comped", ProviderSubscriptionID: "sub_comped", OccurredAt: time.Now(),
	})
	if err := h.svc.ProcessWebhook(ctx, "fake", body, http.Header{}); err != nil {
		t.Fatalf("ProcessWebhook: %v", err)
	}
	h.drain(t)

	plan, status := getTenantPlanStatus(t, pool, tenant)
	if plan != string(billing.TierAgency) || status != "comped" {
		t.Fatalf("a comped tenant was mutated by a webhook: plan=%s status=%s, want agency/comped unchanged", plan, status)
	}
	if calls := atomic.LoadInt32(&fp.getSubscriptionCalls); calls != 0 {
		t.Fatalf("GetSubscription calls = %d, want 0 — a comped tenant must never even reach the provider fetch", calls)
	}
	if _, processed, _ := billingEventState(t, pool, "fake", "evt_comped_1"); !processed {
		t.Fatal("the comped tenant's event must be marked processed")
	}
}

// ---------------------------------------------------------------------------
// Price guard: every status.
// ---------------------------------------------------------------------------

func TestBillingWebhook_UnknownPriceNoOpForEveryStatus(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	fp := newFakeProvider("fake")
	h := newBillingHarness(t, pool, fp)

	statuses := []billing.Status{billing.StatusCanceled, billing.StatusPaused, billing.StatusNone, billing.StatusActive}
	for i, st := range statuses {
		tenant := seedTenant(t, pool, "billing-unknownprice-"+string(st))
		setTenantPlan(t, pool, tenant, string(billing.TierStarter), "active")
		cus, sub := "cus_up_"+string(st), "sub_up_"+string(st)
		setTenantProvider(t, pool, tenant, "fake", cus, sub)
		// PlanResolved=false: the subscription's price is not one of ours.
		fp.subscriptions[sub] = billing.Subscription{ID: sub, CustomerID: cus, Status: st, PlanResolved: false}

		evID := "evt_unknownprice_" + string(st)
		body := fakeEventBody(fakeEventPayload{
			ID: evID, Type: "customer.subscription.updated", Kind: "updated", Handled: true,
			TenantID: tenant, ProviderCustomerID: cus, ProviderSubscriptionID: sub, OccurredAt: time.Now(),
		})
		if err := h.svc.ProcessWebhook(ctx, "fake", body, http.Header{}); err != nil {
			t.Fatalf("ProcessWebhook(%s): %v", st, err)
		}
		h.drain(t)

		plan, status := getTenantPlanStatus(t, pool, tenant)
		if plan != string(billing.TierStarter) || status != "active" {
			t.Fatalf("status %s: tenant mutated despite an unknown price: plan=%s status=%s, want starter/active", st, plan, status)
		}
		if _, processed, _ := billingEventState(t, pool, "fake", evID); !processed {
			t.Fatalf("status %s: event not marked processed", st)
		}
		got := 0
		for _, a := range h.alerts.names() {
			if a == "billing_unknown_price" {
				got++
			}
		}
		if got != i+1 {
			t.Fatalf("status %s: unknown-price alerts = %d, want %d", st, got, i+1)
		}
	}
}

// ---------------------------------------------------------------------------
// Ownership: foreign events leave nothing; owned events always record.
// ---------------------------------------------------------------------------

func TestBillingWebhook_ForeignEventsLeaveNoTrace(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-foreign")
	setTenantProvider(t, pool, tenant, "fake", "cus_ours", "")

	fp := newFakeProvider("fake")
	h := newBillingHarness(t, pool, fp)

	events := []fakeEventPayload{
		// Another product's checkout, even one naming a real tenant.
		{ID: "evt_foreign_checkout", Type: "checkout.session.completed", Kind: "activated", Handled: true,
			TenantID: tenant, ProviderCustomerID: "cus_other", Ownership: billing.OwnershipForeign},
		{ID: "evt_foreign_refund", Type: "charge.refunded", Ownership: billing.OwnershipForeign, ProviderCustomerID: "cus_other"},
		{ID: "evt_foreign_invoice", Type: "invoice.paid", Kind: "payment_succeeded", Handled: true,
			ProviderCustomerID: "cus_other", ProviderSubscriptionID: "sub_other", Ownership: billing.OwnershipForeign},
		// A tax-ID change for a customer no tenant stores.
		{ID: "evt_foreign_taxid", Type: "customer.tax_id.updated", Kind: "tax_id_updated", Handled: true,
			ProviderCustomerID: "cus_unknown", Ownership: billing.OwnershipByCustomer},
	}
	for _, ev := range events {
		ev.OccurredAt = time.Now()
		if err := h.svc.ProcessWebhook(ctx, "fake", fakeEventBody(ev), http.Header{}); err != nil {
			t.Fatalf("%s: ProcessWebhook = %v, want nil (HTTP 200)", ev.ID, err)
		}
		if got := countBillingEvents(t, pool, "fake", ev.ID); got != 0 {
			t.Fatalf("%s: ledger rows = %d, want 0", ev.ID, got)
		}
	}
	h.drain(t)
	if n := countBillingJobs(t, pool, "billing_apply"); n != 0 {
		t.Fatalf("billing_apply jobs = %d, want 0", n)
	}
	if a := h.alerts.names(); len(a) != 0 {
		t.Fatalf("alerts = %v, want none", a)
	}
	if calls := atomic.LoadInt32(&fp.getSubscriptionCalls); calls != 0 {
		t.Fatalf("GetSubscription calls = %d, want 0", calls)
	}
}

func TestBillingWebhook_TaxIDUpdatedForStoredCustomer(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-taxid")
	setTenantPlan(t, pool, tenant, string(billing.TierStarter), "active")
	setTenantProvider(t, pool, tenant, "fake", "cus_tax", "sub_tax")

	fp := newFakeProvider("fake")
	h := newBillingHarness(t, pool, fp)

	body := fakeEventBody(fakeEventPayload{
		ID: "evt_taxid_1", Type: "customer.tax_id.updated", Kind: "tax_id_updated", Handled: true,
		ProviderCustomerID: "cus_tax", Ownership: billing.OwnershipByCustomer, OccurredAt: time.Now(),
	})
	if err := h.svc.ProcessWebhook(ctx, "fake", body, http.Header{}); err != nil {
		t.Fatalf("ProcessWebhook: %v", err)
	}
	h.drain(t)

	stored, processed, _ := billingEventState(t, pool, "fake", "evt_taxid_1")
	if stored == nil || *stored != tenant || !processed {
		t.Fatalf("tax-ID event row tenant=%v processed=%t, want %s and processed", stored, processed, tenant)
	}
	if calls := atomic.LoadInt32(&fp.getSubscriptionCalls); calls != 0 {
		t.Fatalf("GetSubscription calls = %d, want 0: a tax-ID change never changes billing state", calls)
	}
	if plan, status := getTenantPlanStatus(t, pool, tenant); plan != string(billing.TierStarter) || status != "active" {
		t.Fatalf("tenant changed by a tax-ID event: %s/%s", plan, status)
	}
}

func TestBillingWebhook_UnknownCustomerIsRecordedAndAlerted(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	fp := newFakeProvider("fake")
	h := newBillingHarness(t, pool, fp)

	body := fakeEventBody(fakeEventPayload{
		ID: "evt_unknown_customer", Type: "invoice.paid", Kind: "payment_succeeded", Handled: true,
		// No claim, and no tenant stores this customer.
		ProviderCustomerID: "cus_ghost", ProviderSubscriptionID: "sub_ghost", OccurredAt: time.Now(),
	})
	if err := h.svc.ProcessWebhook(ctx, "fake", body, http.Header{}); err != nil {
		t.Fatalf("ProcessWebhook should not error for an unresolvable customer: %v", err)
	}
	h.drain(t)

	if calls := atomic.LoadInt32(&fp.getSubscriptionCalls); calls != 0 {
		t.Fatalf("GetSubscription calls = %d, want 0 — an unresolvable tenant must never reach the provider fetch", calls)
	}
	if got := countBillingEvents(t, pool, "fake", "evt_unknown_customer"); got != 1 {
		t.Fatalf("billing_events rows = %d, want 1 — an owned event is always recorded", got)
	}
	if _, processed, _ := billingEventState(t, pool, "fake", "evt_unknown_customer"); !processed {
		t.Fatal("an unresolvable event must be marked processed")
	}
	if !h.alerts.has("billing_tenant_mismatch") {
		t.Fatalf("alerts = %v, want billing_tenant_mismatch", h.alerts.names())
	}
}

// TestBillingWebhook_ClaimForMissingTenantIsRecordedAndAlerted: an owned
// event naming a tenant that does not exist (never created, or hard-deleted)
// is recorded with a NULL tenant instead of failing intake, and alerts.
func TestBillingWebhook_ClaimForMissingTenantIsRecordedAndAlerted(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	fp := newFakeProvider("fake")
	h := newBillingHarness(t, pool, fp)

	gone := uuid.New()
	body := fakeEventBody(fakeEventPayload{
		ID: "evt_gone_tenant", Type: "invoice.paid", Kind: "payment_succeeded", Handled: true,
		TenantID: gone, ProviderCustomerID: "cus_gone", ProviderSubscriptionID: "sub_gone", OccurredAt: time.Now(),
	})
	if err := h.svc.ProcessWebhook(ctx, "fake", body, http.Header{}); err != nil {
		t.Fatalf("ProcessWebhook for a missing tenant = %v, want nil", err)
	}
	h.drain(t)

	stored, processed, payload := billingEventState(t, pool, "fake", "evt_gone_tenant")
	if stored != nil {
		t.Fatalf("stored tenant_id = %v, want NULL for a tenant that does not exist", *stored)
	}
	if !processed {
		t.Fatal("the event must be marked processed")
	}
	if payload["claimed_tenant_id"] != gone.String() {
		t.Fatalf("payload claimed_tenant_id = %v, want %s", payload["claimed_tenant_id"], gone)
	}
	if !h.alerts.has("billing_tenant_mismatch") {
		t.Fatalf("alerts = %v, want billing_tenant_mismatch", h.alerts.names())
	}
}

func TestBillingWebhook_CustomerMismatchChangesNothing(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-cust-mismatch")
	setTenantPlan(t, pool, tenant, string(billing.TierStarter), "active")
	setTenantProvider(t, pool, tenant, "fake", "cus_mine", "sub_mine")

	fp := newFakeProvider("fake")
	// The subscription the event names belongs to another customer.
	fp.subscriptions["sub_theirs"] = billing.Subscription{
		ID: "sub_theirs", CustomerID: "cus_theirs", Plan: billing.TierScale, PlanResolved: true, Status: billing.StatusCanceled,
	}
	h := newBillingHarness(t, pool, fp)

	body := fakeEventBody(fakeEventPayload{
		ID: "evt_mismatch", Type: "customer.subscription.deleted", Kind: "canceled", Handled: true,
		TenantID: tenant, ProviderCustomerID: "cus_mine", ProviderSubscriptionID: "sub_theirs", OccurredAt: time.Now(),
	})
	if err := h.svc.ProcessWebhook(ctx, "fake", body, http.Header{}); err != nil {
		t.Fatalf("ProcessWebhook: %v", err)
	}
	h.drain(t)

	if plan, status := getTenantPlanStatus(t, pool, tenant); plan != string(billing.TierStarter) || status != "active" {
		t.Fatalf("tenant changed by another customer's subscription: %s/%s", plan, status)
	}
	if !h.alerts.has("billing_customer_mismatch") {
		t.Fatalf("alerts = %v, want billing_customer_mismatch", h.alerts.names())
	}
}

// ---------------------------------------------------------------------------
// Request-path refresh uniqueness.
// ---------------------------------------------------------------------------

func TestBillingRefresh_RequestPathUniqueWithinOneMinute(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-refresh-unique")

	fp := newFakeProvider("fake")
	h := newBillingHarness(t, pool, fp)

	// Keep the test inside one clock minute: River truncates to the period.
	if s := time.Now().Second(); s > 50 {
		time.Sleep(time.Duration(61-s) * time.Second)
	}

	first, err := h.svc.EnqueueRequestRefresh(ctx, tenant, "sub_x", billing.RefreshSourceConfirm)
	if err != nil || !first {
		t.Fatalf("first enqueue: inserted=%t err=%v, want inserted", first, err)
	}
	second, err := h.svc.EnqueueRequestRefresh(ctx, tenant, "sub_x", billing.RefreshSourceConfirm)
	if err != nil || second {
		t.Fatalf("second enqueue while the first is queued: inserted=%t err=%v, want a duplicate", second, err)
	}
	h.drain(t)
	if n := countBillingJobs(t, pool, "billing_refresh", "completed"); n != 1 {
		t.Fatalf("completed billing_refresh jobs = %d, want 1", n)
	}
	third, err := h.svc.EnqueueRequestRefresh(ctx, tenant, "sub_x", billing.RefreshSourceConfirm)
	if err != nil || third {
		t.Fatalf("enqueue after the first completed, same minute: inserted=%t err=%v, want a duplicate", third, err)
	}
	if n := countBillingJobs(t, pool, "billing_refresh"); n != 1 {
		t.Fatalf("billing_refresh jobs = %d, want exactly 1", n)
	}
}
