package tests

// billing_s0p_callers_integration_test.go — the billing callers moved onto the
// S0 queries, proved through the production paths as wpmgr_app:
//
//   - webhook attribution by customer tells one tenant from several;
//   - a cancel schedule the provider reports is stored by the apply worker,
//     and then allows the org delete;
//   - Cancel now stores its marker, which allows the org delete while the
//     subscription is still past due locally;
//   - a Razorpay in-app cancel stores the local flag, with a future cancel_at
//     only, which allows the org delete;
//   - confirm answers 404 for another workspace's session and 422 for an
//     unknown session id.

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// s0pbeRequireAppRole fails unless pool is connected as a role with neither
// SUPERUSER nor BYPASSRLS.
func s0pbeRequireAppRole(t *testing.T, pool *db.Pool) {
	t.Helper()
	var role string
	var super, bypass bool
	if err := pool.QueryRow(context.Background(),
		`SELECT current_user, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&role, &super, &bypass); err != nil {
		t.Fatalf("read current role: %v", err)
	}
	if super || bypass {
		t.Fatalf("running as %q with rolsuper=%v rolbypassrls=%v; these proofs must run as the application role", role, super, bypass)
	}
}

// s0pbeCancelState reads the stored cancel schedule.
func s0pbeCancelState(t *testing.T, pool *db.Pool, tenant uuid.UUID) (atEnd bool, cancelAt *time.Time) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT cancel_at_period_end, cancel_at FROM tenants WHERE id = $1`, tenant).Scan(&atEnd, &cancelAt); err != nil {
		t.Fatalf("read cancel state: %v", err)
	}
	return atEnd, cancelAt
}

// s0pbeBilling reads plan, status and the stored subscription id.
func s0pbeBilling(t *testing.T, pool *db.Pool, tenant uuid.UUID) (plan, status string, sub *string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT plan, plan_status, provider_subscription_id FROM tenants WHERE id = $1`, tenant).Scan(&plan, &status, &sub); err != nil {
		t.Fatalf("read billing: %v", err)
	}
	return plan, status, sub
}

// TestBillingS0PBE_RazorpaySharedCustomerIsRefused: a Razorpay event with no
// claim whose customer is stored by two tenants resolves to neither. It is
// recorded, alerted, marked processed, and applied to no tenant.
func TestBillingS0PBE_RazorpaySharedCustomerIsRefused(t *testing.T) {
	pool := startPostgres(t)
	s0pbeRequireAppRole(t, pool)
	ctx := context.Background()

	const cust, sub = "cust_s0pbe_shared", "sub_s0pbe_shared"
	a := seedTenant(t, pool, "s0pbe-rzp-a-"+uuid.NewString()[:8])
	b := seedTenant(t, pool, "s0pbe-rzp-b-"+uuid.NewString()[:8])
	setTenantProvider(t, pool, a, "razorpay", cust, "")
	setTenantProvider(t, pool, b, "razorpay", cust, "")
	beforeA, beforeStatusA, _ := s0pbeBilling(t, pool, a)
	beforeB, beforeStatusB, _ := s0pbeBilling(t, pool, b)

	fp := newFakeProvider("razorpay")
	fp.noPortal = true
	fp.subscriptions[sub] = billing.Subscription{
		ID: sub, CustomerID: cust, Plan: billing.TierAgency, PlanResolved: true,
		Status: billing.StatusActive, CurrentPeriodEnd: time.Now().Add(30 * 24 * time.Hour),
	}
	h := newBillingHarness(t, pool, fp)

	body := fakeEventBody(fakeEventPayload{
		ID: "evt_s0pbe_shared", Type: "subscription.activated", Kind: string(billing.EventActivated), Handled: true,
		ProviderCustomerID: cust, ProviderSubscriptionID: sub, OccurredAt: time.Now(),
	})
	if err := h.svc.ProcessWebhook(ctx, "razorpay", body, http.Header{}); err != nil {
		t.Fatalf("ProcessWebhook: %v", err)
	}
	h.drain(t)

	if calls := atomic.LoadInt32(&fp.getSubscriptionCalls); calls != 0 {
		t.Fatalf("GetSubscription calls = %d, want 0: an event naming two tenants' customer must not be applied to either", calls)
	}
	for _, c := range []struct {
		id           uuid.UUID
		plan, status string
		name         string
	}{{a, beforeA, beforeStatusA, "a"}, {b, beforeB, beforeStatusB, "b"}} {
		plan, status, stored := s0pbeBilling(t, pool, c.id)
		if plan != c.plan || status != c.status || stored != nil {
			t.Fatalf("tenant %s changed to %s/%s sub=%v, want %s/%s and no subscription", c.name, plan, status, stored, c.plan, c.status)
		}
	}
	stored, processed, _ := billingEventState(t, pool, "razorpay", "evt_s0pbe_shared")
	if !processed {
		t.Fatal("the refused event must be marked processed")
	}
	if stored != nil {
		t.Fatalf("the refused event was attributed to tenant %s", *stored)
	}
	if !h.alerts.has("billing_tenant_mismatch") {
		t.Fatalf("alerts = %v, want billing_tenant_mismatch", h.alerts.names())
	}
}

// TestBillingS0PBE_RazorpaySingleCustomerApplies is the control for the test
// above: the same event, with only one tenant storing the customer, is
// applied to that tenant.
func TestBillingS0PBE_RazorpaySingleCustomerApplies(t *testing.T) {
	pool := startPostgres(t)
	s0pbeRequireAppRole(t, pool)
	ctx := context.Background()

	const cust, sub = "cust_s0pbe_single", "sub_s0pbe_single"
	a := seedTenant(t, pool, "s0pbe-rzp-one-"+uuid.NewString()[:8])
	setTenantProvider(t, pool, a, "razorpay", cust, "")

	fp := newFakeProvider("razorpay")
	fp.noPortal = true
	fp.subscriptions[sub] = billing.Subscription{
		ID: sub, CustomerID: cust, Plan: billing.TierAgency, PlanResolved: true,
		Status: billing.StatusActive, CurrentPeriodEnd: time.Now().Add(30 * 24 * time.Hour),
	}
	h := newBillingHarness(t, pool, fp)

	body := fakeEventBody(fakeEventPayload{
		ID: "evt_s0pbe_single", Type: "subscription.activated", Kind: string(billing.EventActivated), Handled: true,
		ProviderCustomerID: cust, ProviderSubscriptionID: sub, OccurredAt: time.Now(),
	})
	if err := h.svc.ProcessWebhook(ctx, "razorpay", body, http.Header{}); err != nil {
		t.Fatalf("ProcessWebhook: %v", err)
	}
	h.drain(t)

	plan, status, stored := s0pbeBilling(t, pool, a)
	if plan != string(billing.TierAgency) || status != "active" || stored == nil || *stored != sub {
		t.Fatalf("tenant = %s/%s sub=%v, want agency/active with %s", plan, status, stored, sub)
	}
	if h.alerts.has("billing_tenant_mismatch") {
		t.Fatalf("alerts = %v, want no billing_tenant_mismatch for a customer one tenant stores", h.alerts.names())
	}
}

// TestBillingS0PBE_ReportedCancelScheduleAllowsDelete: Stripe reports a
// period-end cancel; the apply worker stores it, and the org delete, refused
// before, is allowed after.
func TestBillingS0PBE_ReportedCancelScheduleAllowsDelete(t *testing.T) {
	pool := startPostgres(t)
	s0pbeRequireAppRole(t, pool)
	admin := connectAdmin(t, pool)
	ctx := context.Background()

	target, slug, p := odgSeedOrg(t, pool, admin, "s0pbe-sched")
	cust, sub := "cus_s0pbe_"+uuid.NewString()[:8], "sub_s0pbe_"+uuid.NewString()[:8]
	setTenantPlan(t, pool, target, string(billing.TierAgency), "active")
	setTenantProvider(t, pool, target, "stripe", cust, sub)

	periodEnd := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	fp := newFakeProvider("stripe")
	fp.subscriptions[sub] = billing.Subscription{
		ID: sub, CustomerID: cust, Plan: billing.TierAgency, PlanResolved: true,
		Status: billing.StatusActive, CurrentPeriodEnd: periodEnd,
		CancelScheduleReported: true, CancelAtPeriodEnd: true, CancelAt: periodEnd,
	}
	h := newBillingHarness(t, pool, fp)

	if w := odgDelete(odgEngine(t, pool, h.svc, p), target, slug); w.Code != http.StatusConflict {
		t.Fatalf("before the event: want 409, got %d body=%s", w.Code, w.Body.String())
	} else if _, reason := odgReason(t, w); reason != billing.OrgDeleteReasonCancelRequired {
		t.Fatalf("before the event: reason = %s, want %s", reason, billing.OrgDeleteReasonCancelRequired)
	}

	body := fakeEventBody(fakeEventPayload{
		ID: "evt_s0pbe_sched_" + uuid.NewString()[:8], Type: "customer.subscription.updated",
		Kind: string(billing.EventUpdated), Handled: true,
		TenantID: target, ProviderCustomerID: cust, ProviderSubscriptionID: sub, OccurredAt: time.Now(),
	})
	if err := h.svc.ProcessWebhook(ctx, "stripe", body, http.Header{}); err != nil {
		t.Fatalf("ProcessWebhook: %v", err)
	}
	h.drain(t)

	atEnd, cancelAt := s0pbeCancelState(t, pool, target)
	if !atEnd || cancelAt == nil || !cancelAt.Equal(periodEnd) {
		t.Fatalf("stored cancel state = %v/%v, want true/%v", atEnd, cancelAt, periodEnd)
	}
	w := odgDelete(odgEngine(t, pool, h.svc, p), target, slug)
	if w.Code != http.StatusOK {
		t.Fatalf("after the event: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if odTenantDeletedAt(t, admin, target) == nil {
		t.Fatal("delete returned 200 but the org is not soft-deleted")
	}
}

// s0pbeImmediateProvider is the fake with Cancel now.
type s0pbeImmediateProvider struct {
	*fakeProvider
	nowCalls []string
}

func (p *s0pbeImmediateProvider) CancelSubscriptionNow(_ context.Context, id string) error {
	p.nowCalls = append(p.nowCalls, id)
	return nil
}

// TestBillingS0PBE_CancelNowMarkerAllowsDelete: a past-due Stripe tenant uses
// Cancel now. The marker is stored under the billing lock, and the org
// delete, refused before, is allowed while the local status is still
// past_due (no refresh has run).
func TestBillingS0PBE_CancelNowMarkerAllowsDelete(t *testing.T) {
	pool := startPostgres(t)
	s0pbeRequireAppRole(t, pool)
	admin := connectAdmin(t, pool)
	ctx := context.Background()

	target, slug, p := odgSeedOrg(t, pool, admin, "s0pbe-now")
	cust, sub := "cus_s0pbe_"+uuid.NewString()[:8], "sub_s0pbe_"+uuid.NewString()[:8]
	setTenantPlan(t, pool, target, string(billing.TierAgency), "past_due")
	setTenantProvider(t, pool, target, "stripe", cust, sub)

	fp := &s0pbeImmediateProvider{fakeProvider: newFakeProvider("stripe")}
	fp.subscriptions[sub] = billing.Subscription{
		ID: sub, CustomerID: cust, Plan: billing.TierAgency, PlanResolved: true,
		Status: billing.StatusPastDue, CurrentPeriodEnd: time.Now().Add(10 * 24 * time.Hour),
	}
	svc := billing.New(pool, nil, true, domain.SystemClock{}, slog.Default())
	svc.SetProviders(billing.NewRegistry(fp), "stripe")

	if w := odgDelete(odgEngine(t, pool, svc, p), target, slug); w.Code != http.StatusConflict {
		t.Fatalf("before cancel now: want 409, got %d body=%s", w.Code, w.Body.String())
	} else if _, reason := odgReason(t, w); reason != billing.OrgDeleteReasonPastDue {
		t.Fatalf("before cancel now: reason = %s, want %s", reason, billing.OrgDeleteReasonPastDue)
	}

	before := time.Now()
	if err := svc.CancelSubscriptionNow(ctx, target, billing.Actor{Type: "user", ID: p.UserID.String()}); err != nil {
		t.Fatalf("CancelSubscriptionNow: %v", err)
	}
	if len(fp.nowCalls) != 1 || fp.nowCalls[0] != sub {
		t.Fatalf("provider cancel-now calls = %v, want [%s]", fp.nowCalls, sub)
	}
	atEnd, cancelAt := s0pbeCancelState(t, pool, target)
	if !atEnd || cancelAt == nil || cancelAt.After(time.Now()) || cancelAt.Before(before.Add(-time.Minute)) {
		t.Fatalf("stored cancel state = %v/%v, want true and a cancel_at at the cancel", atEnd, cancelAt)
	}
	if _, status, _ := s0pbeBilling(t, pool, target); status != "past_due" {
		t.Fatalf("status = %s, want past_due: Cancel now writes only the marker", status)
	}

	w := odgDelete(odgEngine(t, pool, svc, p), target, slug)
	if w.Code != http.StatusOK {
		t.Fatalf("after cancel now: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if odTenantDeletedAt(t, admin, target) == nil {
		t.Fatal("delete returned 200 but the org is not soft-deleted")
	}
}

// TestBillingS0PBE_RazorpayInAppCancelFlag: a Razorpay in-app cancel stores
// the local flag. A future period end is stored as cancel_at; a past one is
// not. Either way the org delete is then allowed.
func TestBillingS0PBE_RazorpayInAppCancelFlag(t *testing.T) {
	pool := startPostgres(t)
	s0pbeRequireAppRole(t, pool)
	admin := connectAdmin(t, pool)
	ctx := context.Background()

	for _, tc := range []struct {
		name         string
		periodOffset time.Duration
		wantCancelAt bool
	}{
		{"future period end", 12 * 24 * time.Hour, true},
		{"past period end", -time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, slug, p := odgSeedOrg(t, pool, admin, "s0pbe-rzpc")
			sub := "sub_s0pbe" + uuid.NewString()[:8]
			setTenantPlan(t, pool, target, string(billing.TierAgency), "active")
			setTenantProvider(t, pool, target, "razorpay", "cust_s0pbe"+uuid.NewString()[:8], sub)
			periodEnd := time.Now().Add(tc.periodOffset).UTC().Truncate(time.Second)
			if _, err := pool.Exec(ctx, `UPDATE tenants SET current_period_end = $1 WHERE id = $2`, periodEnd, target); err != nil {
				t.Fatalf("seed period end: %v", err)
			}

			fp := newFakeProvider("razorpay")
			fp.noPortal = true
			svc := billing.New(pool, nil, true, domain.SystemClock{}, slog.Default())
			svc.SetProviders(billing.NewRegistry(fp), "razorpay")

			if w := odgDelete(odgEngine(t, pool, svc, p), target, slug); w.Code != http.StatusConflict {
				t.Fatalf("before the cancel: want 409, got %d body=%s", w.Code, w.Body.String())
			}

			if err := svc.CancelSubscription(ctx, target, billing.Actor{Type: "user", ID: p.UserID.String()}); err != nil {
				t.Fatalf("CancelSubscription: %v", err)
			}
			if fp.lastCancelSubscriptionID != sub {
				t.Fatalf("provider cancel called with %q, want %q", fp.lastCancelSubscriptionID, sub)
			}
			atEnd, cancelAt := s0pbeCancelState(t, pool, target)
			if !atEnd {
				t.Fatal("cancel_at_period_end = false, want true after an in-app Razorpay cancel")
			}
			if tc.wantCancelAt && (cancelAt == nil || !cancelAt.Equal(periodEnd)) {
				t.Fatalf("cancel_at = %v, want %v", cancelAt, periodEnd)
			}
			if !tc.wantCancelAt && cancelAt != nil {
				t.Fatalf("cancel_at = %v, want NULL for a period end in the past", *cancelAt)
			}

			w := odgDelete(odgEngine(t, pool, svc, p), target, slug)
			if w.Code != http.StatusOK {
				t.Fatalf("after the cancel: want 200, got %d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// s0pbeConfirmProvider is the fake with checkout-session retrieval.
type s0pbeConfirmProvider struct {
	*fakeProvider
	sessions map[string]billing.CheckoutSessionInfo
}

func (p *s0pbeConfirmProvider) RetrieveCheckoutSession(_ context.Context, id string) (billing.CheckoutSessionInfo, error) {
	s, ok := p.sessions[id]
	if !ok {
		return billing.CheckoutSessionInfo{}, billing.ErrCheckoutSessionNotFound
	}
	return s, nil
}

// TestBillingS0PBE_ConfirmOtherWorkspaceIs404: confirm with another
// workspace's session is 404, an unknown session id is 422, and the tenant's
// own completed session is accepted.
func TestBillingS0PBE_ConfirmOtherWorkspaceIs404(t *testing.T) {
	pool := startPostgres(t)
	s0pbeRequireAppRole(t, pool)
	ctx := context.Background()

	mine := seedTenant(t, pool, "s0pbe-conf-mine-"+uuid.NewString()[:8])
	other := seedTenant(t, pool, "s0pbe-conf-other-"+uuid.NewString()[:8])
	setTenantProvider(t, pool, mine, "stripe", "cus_s0pbe_mine", "")
	setTenantProvider(t, pool, other, "stripe", "cus_s0pbe_other", "")

	fp := &s0pbeConfirmProvider{fakeProvider: newFakeProvider("stripe"), sessions: map[string]billing.CheckoutSessionInfo{
		"cs_test_other": {ID: "cs_test_other", ClientReferenceID: other.String(), CustomerID: "cus_s0pbe_other",
			SubscriptionID: "sub_s0pbe_other", Mode: "subscription", Status: "complete"},
		"cs_test_mine": {ID: "cs_test_mine", ClientReferenceID: mine.String(), CustomerID: "cus_s0pbe_mine",
			SubscriptionID: "sub_s0pbe_mine", Mode: "subscription", Status: "complete"},
	}}
	h := newBillingHarness(t, pool, fp)

	_, err := h.svc.ConfirmCheckout(ctx, mine, "cs_test_other")
	if got := domain.HTTPStatus(err); got != http.StatusNotFound {
		t.Fatalf("another workspace's session: status %d (%v), want 404", got, err)
	}
	if de, ok := domain.AsDomain(err); !ok || de.Code != "billing_checkout_session_not_found" {
		t.Fatalf("another workspace's session: error %v, want billing_checkout_session_not_found", err)
	}

	_, err = h.svc.ConfirmCheckout(ctx, mine, "cs_test_unknown")
	if got := domain.HTTPStatus(err); got != http.StatusUnprocessableEntity {
		t.Fatalf("unknown session: status %d (%v), want 422", got, err)
	}

	if _, err := h.svc.ConfirmCheckout(ctx, mine, "cs_test_mine"); err != nil {
		t.Fatalf("own completed session: %v", err)
	}
}
