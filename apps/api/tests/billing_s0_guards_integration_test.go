package tests

// billing_s0_guards_integration_test.go — proofs for billing guards that
// protect money: a new subscription never inherits an old cancel schedule,
// a second live subscription is refused, a duplicate delivery of an event
// that was never processed is applied, reconcile adopts a returning Stripe
// customer's new subscription, and Cancel now refuses every state it must.
// Every test runs as wpmgr_app through the production service and workers.

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// s0gWebhook delivers one owned, handled event for sub to the fake provider
// named by fp.name.
func s0gWebhook(t *testing.T, h *billingHarness, provider string, tenant uuid.UUID, customer, sub, kind string) {
	t.Helper()
	body := fakeEventBody(fakeEventPayload{
		ID: "evt_" + uuid.NewString()[:12], Type: "subscription." + kind, Kind: kind, Handled: true,
		TenantID: tenant, ProviderCustomerID: customer, ProviderSubscriptionID: sub, OccurredAt: time.Now(),
	})
	if err := h.svc.ProcessWebhook(context.Background(), provider, body, http.Header{}); err != nil {
		t.Fatalf("ProcessWebhook %s %s: %v", kind, sub, err)
	}
	h.drain(t)
}

// TestBillingS0G_RazorpayResubscribeRefusesDelete: a Razorpay subscription
// cancelled in-app ends, the owner subscribes again, and the new
// subscription goes active. The org delete must then be refused with
// cancel_required, because nothing cancelled the new subscription.
func TestBillingS0G_RazorpayResubscribeRefusesDelete(t *testing.T) {
	pool := startPostgres(t)
	s0pbeRequireAppRole(t, pool)
	admin := connectAdmin(t, pool)
	ctx := context.Background()

	target, slug, p := odgSeedOrg(t, pool, admin, "s0g-rzp-resub")
	cust := "cust_s0g" + uuid.NewString()[:8]
	sub1, sub2 := "sub_s0g1"+uuid.NewString()[:8], "sub_s0g2"+uuid.NewString()[:8]
	setTenantPlan(t, pool, target, "free", "none")
	setTenantProvider(t, pool, target, "razorpay", cust, "")

	fp := newFakeProvider("razorpay")
	fp.noPortal = true
	periodEnd := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	fp.subscriptions[sub1] = billing.Subscription{
		ID: sub1, CustomerID: cust, Plan: billing.TierAgency, PlanResolved: true,
		Status: billing.StatusActive, CurrentPeriodEnd: periodEnd,
	}
	h := newBillingHarness(t, pool, fp)
	actor := billing.Actor{Type: "user", ID: p.UserID.String()}

	s0gWebhook(t, h, "razorpay", target, cust, sub1, "activated")
	if _, status, sub := s0pbeBilling(t, pool, target); status != "active" || sub == nil || *sub != sub1 {
		t.Fatalf("after sub_1 activated: status=%s sub=%v, want active/%s", status, sub, sub1)
	}

	// In-app cancel: the provider reports no schedule, so it is stored locally.
	if err := h.svc.CancelSubscription(ctx, target, actor); err != nil {
		t.Fatalf("CancelSubscription: %v", err)
	}
	if atEnd, _ := s0pbeCancelState(t, pool, target); !atEnd {
		t.Fatal("in-app cancel did not store cancel_at_period_end")
	}

	// sub_1 ends.
	ended := fp.subscriptions[sub1]
	ended.Status = billing.StatusCanceled
	fp.subscriptions[sub1] = ended
	s0gWebhook(t, h, "razorpay", target, cust, sub1, "canceled")
	if _, status, _ := s0pbeBilling(t, pool, target); status != "canceled" {
		t.Fatalf("after sub_1 ended: status=%s, want canceled", status)
	}

	// The owner subscribes again through the real checkout path.
	if _, err := h.svc.CreateCheckout(ctx, target, billing.TierAgency, "razorpay", "INR", "owner@example.com",
		"https://cp.test/ok", "https://cp.test/cancel", actor); err != nil {
		t.Fatalf("re-subscribe CreateCheckout: %v", err)
	}
	fp.subscriptions[sub2] = billing.Subscription{
		ID: sub2, CustomerID: cust, Plan: billing.TierAgency, PlanResolved: true,
		Status: billing.StatusActive, CurrentPeriodEnd: time.Now().Add(30 * 24 * time.Hour),
	}
	s0gWebhook(t, h, "razorpay", target, cust, sub2, "activated")

	_, status, sub := s0pbeBilling(t, pool, target)
	atEnd, cancelAt := s0pbeCancelState(t, pool, target)
	if status != "active" || sub == nil || *sub != sub2 {
		t.Fatalf("after sub_2 activated: status=%s sub=%v, want active/%s", status, sub, sub2)
	}
	if atEnd || cancelAt != nil {
		t.Fatalf("sub_2 inherited sub_1's cancel schedule: cancel_at_period_end=%v cancel_at=%v", atEnd, cancelAt)
	}

	w := odgDelete(odgEngine(t, pool, h.svc, p), target, slug)
	if w.Code != http.StatusConflict {
		t.Fatalf("delete with a live, uncancelled sub_2: want 409, got %d body=%s", w.Code, w.Body.String())
	}
	if code, reason := odgReason(t, w); code != billing.OrgDeleteBlockedCode || reason != billing.OrgDeleteReasonCancelRequired {
		t.Fatalf("code/reason = %s/%s, want %s/%s", code, reason, billing.OrgDeleteBlockedCode, billing.OrgDeleteReasonCancelRequired)
	}
	if odTenantDeletedAt(t, admin, target) != nil {
		t.Fatal("the org was soft-deleted while sub_2 is live")
	}
}

// TestBillingS0G_ProviderSwitchClearsCancelSchedule: a Stripe tenant whose
// subscription was cancelled at period end, and has ended, starts a checkout
// with Razorpay. The bind that moves the tenant to Razorpay clears the stored
// cancel schedule along with the subscription id.
func TestBillingS0G_ProviderSwitchClearsCancelSchedule(t *testing.T) {
	r := newStripeCheckoutRig(t, "s0g-switch-cancel")
	s0pbeRequireAppRole(t, r.pool)
	r.setPin(t, "stripe", "cus_A", "sub_OLD", "canceled")
	if _, err := r.pool.Exec(r.baseCtx,
		`UPDATE tenants SET cancel_at_period_end = true, cancel_at = $1 WHERE id = $2`,
		time.Now().Add(-24*time.Hour), r.tenant); err != nil {
		t.Fatalf("seed cancel schedule: %v", err)
	}

	if _, err := r.checkout("razorpay"); err != nil {
		t.Fatalf("switch checkout: %v", err)
	}
	provider, _, sub := r.pin(t)
	if provider != "razorpay" || sub != "" {
		t.Fatalf("pin after switch = %s/%q, want razorpay with no subscription", provider, sub)
	}
	if atEnd, cancelAt := s0pbeCancelState(t, r.pool, r.tenant); atEnd || cancelAt != nil {
		t.Fatalf("switch kept the old provider's cancel schedule: cancel_at_period_end=%v cancel_at=%v", atEnd, cancelAt)
	}
}

// TestBillingS0G_SecondLiveSubscriptionIsRefused: while the stored
// subscription is live, an event for a different subscription of the same
// customer raises billing_double_subscription and changes nothing.
func TestBillingS0G_SecondLiveSubscriptionIsRefused(t *testing.T) {
	pool := startPostgres(t)
	s0pbeRequireAppRole(t, pool)
	tenant := seedTenant(t, pool, "s0g-double-"+uuid.NewString()[:8])
	const cust, subA, subB = "cus_s0g_dbl", "sub_s0g_dbl_A", "sub_s0g_dbl_B"
	setTenantPlan(t, pool, tenant, string(billing.TierStarter), "active")
	setTenantProvider(t, pool, tenant, "fake", cust, subA)

	fp := newFakeProvider("fake")
	fp.subscriptions[subA] = billing.Subscription{
		ID: subA, CustomerID: cust, Plan: billing.TierStarter, PlanResolved: true,
		Status: billing.StatusActive, CurrentPeriodEnd: time.Now().Add(30 * 24 * time.Hour),
	}
	fp.subscriptions[subB] = billing.Subscription{
		ID: subB, CustomerID: cust, Plan: billing.TierScale, PlanResolved: true,
		Status: billing.StatusActive, CurrentPeriodEnd: time.Now().Add(30 * 24 * time.Hour),
	}
	h := newBillingHarness(t, pool, fp)

	s0gWebhook(t, h, "fake", tenant, cust, subB, "activated")

	if !h.alerts.has("billing_double_subscription") {
		t.Fatalf("alerts = %v, want billing_double_subscription", h.alerts.names())
	}
	plan, status, sub := s0pbeBilling(t, pool, tenant)
	if plan != string(billing.TierStarter) || status != "active" || sub == nil || *sub != subA {
		t.Fatalf("state = %s/%s/%v, want starter/active/%s unchanged", plan, status, sub, subA)
	}
}

// TestBillingS0G_DuplicateOfUnprocessedEventIsApplied: the first delivery is
// recorded but its apply job never runs (it is cancelled). A later duplicate
// delivery of the same event must enqueue it again, and the event is then
// applied.
func TestBillingS0G_DuplicateOfUnprocessedEventIsApplied(t *testing.T) {
	pool := startPostgres(t)
	s0pbeRequireAppRole(t, pool)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "s0g-dup-unprocessed-"+uuid.NewString()[:8])
	const cust, sub = "cus_s0g_dup", "sub_s0g_dup"
	setTenantProvider(t, pool, tenant, "fake", cust, "")

	fp := newFakeProvider("fake")
	fp.subscriptions[sub] = billing.Subscription{
		ID: sub, CustomerID: cust, Plan: billing.TierAgency, PlanResolved: true,
		Status: billing.StatusActive, CurrentPeriodEnd: time.Now().Add(30 * 24 * time.Hour),
	}
	body := fakeEventBody(fakeEventPayload{
		ID: "evt_s0g_dup_1", Type: "customer.subscription.updated", Kind: "activated", Handled: true,
		TenantID: tenant, ProviderCustomerID: cust, ProviderSubscriptionID: sub, OccurredAt: time.Now(),
	})

	// First delivery through an insert-only client: the apply job is queued
	// and then cancelled before any worker runs it.
	migrateRiver(t, pool)
	insertOnly, err := river.NewClient(riverpgxv5.New(pool.Pool), &river.Config{
		Logger: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	if err != nil {
		t.Fatalf("insert-only river client: %v", err)
	}
	first := billing.New(pool, nil, true, domain.SystemClock{}, slog.Default())
	first.SetProviders(billing.NewRegistry(fp), "fake")
	first.SetRiver(insertOnly)
	if err := first.ProcessWebhook(ctx, "fake", body, http.Header{}); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	var jobID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM river_job WHERE kind = 'billing_apply'`).Scan(&jobID); err != nil {
		t.Fatalf("read the first apply job: %v", err)
	}
	if _, err := insertOnly.JobCancel(ctx, jobID); err != nil {
		t.Fatalf("cancel the first apply job: %v", err)
	}
	if n := countBillingJobs(t, pool, "billing_apply", "cancelled"); n != 1 {
		t.Fatalf("cancelled apply jobs = %d, want 1", n)
	}

	// The duplicate arrives while workers run.
	h := newBillingHarness(t, pool, fp)
	if err := h.svc.ProcessWebhook(ctx, "fake", body, http.Header{}); err != nil {
		t.Fatalf("duplicate delivery: %v", err)
	}
	h.drain(t)

	if n := countBillingJobs(t, pool, "billing_apply", "completed"); n != 1 {
		t.Fatalf("completed apply jobs = %d, want 1: the duplicate did not re-enqueue the unprocessed event", n)
	}
	if calls := atomic.LoadInt32(&fp.getSubscriptionCalls); calls != 1 {
		t.Fatalf("GetSubscription calls = %d, want 1", calls)
	}
	plan, status := getTenantPlanStatus(t, pool, tenant)
	if plan != string(billing.TierAgency) || status != "active" {
		t.Fatalf("state = %s/%s, want agency/active from the re-enqueued event", plan, status)
	}
}

// s0gStripeLister is a Stripe-named fake that answers the customer-filtered
// subscription lookup.
type s0gStripeLister struct {
	*fakeProvider
	liveByCustomer map[string]string
	lookups        []string
}

func (p *s0gStripeLister) HasPendingOrLive(_ context.Context, customerID string) (string, bool, error) {
	id, ok := p.liveByCustomer[customerID]
	return id, ok, nil
}

func (p *s0gStripeLister) PendingOrLiveSubscription(_ context.Context, customerID string) (string, bool, error) {
	p.lookups = append(p.lookups, customerID)
	id, ok := p.liveByCustomer[customerID]
	return id, ok, nil
}

// TestBillingS0G_ReconcileAdoptsReturningStripeCustomer: a Stripe tenant
// whose stored subscription has ended pays for a new one, and every webhook
// and the confirm are lost. Reconcile finds the new subscription by the
// stored customer and adopts it.
func TestBillingS0G_ReconcileAdoptsReturningStripeCustomer(t *testing.T) {
	pool := startPostgres(t)
	s0pbeRequireAppRole(t, pool)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "s0g-recon-return-"+uuid.NewString()[:8])
	const cust, sub0, sub1 = "cus_s0g_ret", "sub_s0g_ret_0", "sub_s0g_ret_1"
	setTenantPlan(t, pool, tenant, "free", "canceled")
	setTenantProvider(t, pool, tenant, "stripe", cust, sub0)

	fp := &s0gStripeLister{fakeProvider: newFakeProvider("stripe"), liveByCustomer: map[string]string{cust: sub1}}
	fp.subscriptions[sub0] = billing.Subscription{
		ID: sub0, CustomerID: cust, Plan: billing.TierAgency, PlanResolved: true, Status: billing.StatusCanceled,
	}
	fp.subscriptions[sub1] = billing.Subscription{
		ID: sub1, CustomerID: cust, Plan: billing.TierAgency, PlanResolved: true,
		Status: billing.StatusActive, CurrentPeriodEnd: time.Now().Add(30 * 24 * time.Hour),
	}
	h := newBillingHarness(t, pool, fp)

	if _, err := h.svc.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	h.drain(t)

	if len(fp.lookups) != 1 || fp.lookups[0] != cust {
		t.Fatalf("customer lookups = %v, want [%s]", fp.lookups, cust)
	}
	plan, status, sub := s0pbeBilling(t, pool, tenant)
	if plan != string(billing.TierAgency) || status != "active" || sub == nil || *sub != sub1 {
		t.Fatalf("after reconcile: %s/%s/%v, want agency/active/%s", plan, status, sub, sub1)
	}
}

// TestBillingS0G_ReconcileLeavesLiveStripeSubscriptionAlone: a Stripe tenant
// whose stored subscription is live is refreshed by that id, and no lookup
// by customer is made.
func TestBillingS0G_ReconcileLeavesLiveStripeSubscriptionAlone(t *testing.T) {
	pool := startPostgres(t)
	s0pbeRequireAppRole(t, pool)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "s0g-recon-live-"+uuid.NewString()[:8])
	const cust, sub0 = "cus_s0g_live", "sub_s0g_live_0"
	setTenantPlan(t, pool, tenant, string(billing.TierAgency), "active")
	setTenantProvider(t, pool, tenant, "stripe", cust, sub0)

	fp := &s0gStripeLister{fakeProvider: newFakeProvider("stripe"), liveByCustomer: map[string]string{cust: "sub_other"}}
	fp.subscriptions[sub0] = billing.Subscription{
		ID: sub0, CustomerID: cust, Plan: billing.TierAgency, PlanResolved: true,
		Status: billing.StatusActive, CurrentPeriodEnd: time.Now().Add(30 * 24 * time.Hour),
	}
	h := newBillingHarness(t, pool, fp)

	if _, err := h.svc.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	h.drain(t)
	if len(fp.lookups) != 0 {
		t.Fatalf("customer lookups = %v, want none for a live stored subscription", fp.lookups)
	}
	if _, _, sub := s0pbeBilling(t, pool, tenant); sub == nil || *sub != sub0 {
		t.Fatalf("stored subscription = %v, want %s", sub, sub0)
	}
}

// TestBillingS0G_CancelNowRefusals: Cancel now refuses, and never calls the
// provider's cancel, when the provider no longer reports past_due, when the
// provider's subscription belongs to another customer, for a Razorpay
// tenant, and when the stored status is not past_due.
func TestBillingS0G_CancelNowRefusals(t *testing.T) {
	pool := startPostgres(t)
	s0pbeRequireAppRole(t, pool)
	ctx := context.Background()

	cases := []struct {
		name         string
		provider     string
		storedStatus string
		subCustomer  string // "" means the stored customer
		subStatus    billing.Status
		wantKind     domain.Kind
		wantCode     string
	}{
		{"provider status moved", "stripe", "past_due", "", billing.StatusActive,
			domain.KindValidation, "billing_cancel_now_not_allowed"},
		{"customer mismatch", "stripe", "past_due", "cus_someone_else", billing.StatusPastDue,
			domain.KindConflict, "billing_subscription_mismatch"},
		{"razorpay tenant", "razorpay", "past_due", "", billing.StatusPastDue,
			domain.KindValidation, "billing_cancel_now_not_allowed"},
		{"stored status not past_due", "stripe", "active", "", billing.StatusPastDue,
			domain.KindValidation, "billing_cancel_now_not_allowed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenant := seedTenant(t, pool, "s0g-now-"+uuid.NewString()[:8])
			cust, sub := "cus_s0g_now"+uuid.NewString()[:8], "sub_s0g_now"+uuid.NewString()[:8]
			setTenantPlan(t, pool, tenant, string(billing.TierAgency), tc.storedStatus)
			setTenantProvider(t, pool, tenant, tc.provider, cust, sub)

			subCustomer := cust
			if tc.subCustomer != "" {
				subCustomer = tc.subCustomer
			}
			fp := &s0pbeImmediateProvider{fakeProvider: newFakeProvider(tc.provider)}
			fp.subscriptions[sub] = billing.Subscription{
				ID: sub, CustomerID: subCustomer, Plan: billing.TierAgency, PlanResolved: true,
				Status: tc.subStatus, CurrentPeriodEnd: time.Now().Add(10 * 24 * time.Hour),
			}
			svc := billing.New(pool, nil, true, domain.SystemClock{}, slog.Default())
			svc.SetProviders(billing.NewRegistry(fp), tc.provider)

			err := svc.CancelSubscriptionNow(ctx, tenant, billing.Actor{Type: "user", ID: "user-1"})
			de, ok := domain.AsDomain(err)
			if !ok || de.Kind != tc.wantKind || de.Code != tc.wantCode {
				t.Fatalf("err = %v, want kind %v code %s", err, tc.wantKind, tc.wantCode)
			}
			if len(fp.nowCalls) != 0 {
				t.Fatalf("provider cancel-now calls = %v, want none", fp.nowCalls)
			}
			if atEnd, cancelAt := s0pbeCancelState(t, pool, tenant); atEnd || cancelAt != nil {
				t.Fatalf("a refused cancel now stored a marker: %v/%v", atEnd, cancelAt)
			}
		})
	}
}
