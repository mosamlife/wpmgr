package billing

import (
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// TestOrgDeleteBlock_Matrix is the full org-delete matrix over the pure
// decision: every allowed row returns nil, and every refused row returns a
// 409 billing_active carrying the named reason.
func TestOrgDeleteBlock_Matrix(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute)
	future := now.Add(24 * time.Hour)

	tests := []struct {
		name       string
		p          tenantBillingProfile
		wantReason string // "" means allowed
	}{
		// Allowed.
		{"no subscription stored, active", tenantBillingProfile{Status: StatusActive, BillingProvider: providerStripe}, ""},
		{"no subscription stored, past due", tenantBillingProfile{Status: StatusPastDue, BillingProvider: providerStripe}, ""},
		{"no subscription stored, comped", tenantBillingProfile{Status: StatusComped}, ""},
		{"no subscription stored, none", tenantBillingProfile{Status: StatusNone}, ""},
		{"canceled", tenantBillingProfile{Status: StatusCanceled, ProviderSubscriptionID: "sub_1", BillingProvider: providerStripe}, ""},
		{"active, period-end cancel", tenantBillingProfile{Status: StatusActive, ProviderSubscriptionID: "sub_1", CancelAtPeriodEnd: true}, ""},
		{"active, cancel_at only", tenantBillingProfile{Status: StatusActive, ProviderSubscriptionID: "sub_1", CancelAt: &future}, ""},
		{"trialing, period-end cancel", tenantBillingProfile{Status: StatusTrialing, ProviderSubscriptionID: "sub_1", CancelAtPeriodEnd: true}, ""},
		{"paused, cancel_at only", tenantBillingProfile{Status: StatusPaused, ProviderSubscriptionID: "sub_1", CancelAt: &future}, ""},
		{"stripe past due, cancel now marker", tenantBillingProfile{Status: StatusPastDue, BillingProvider: providerStripe, ProviderSubscriptionID: "sub_1", CancelAtPeriodEnd: true, CancelAt: &past}, ""},
		{"stripe past due, marker exactly now", tenantBillingProfile{Status: StatusPastDue, BillingProvider: providerStripe, ProviderSubscriptionID: "sub_1", CancelAt: &now}, ""},

		// Refused.
		{"active, no cancel", tenantBillingProfile{Status: StatusActive, ProviderSubscriptionID: "sub_1"}, OrgDeleteReasonCancelRequired},
		{"trialing, no cancel", tenantBillingProfile{Status: StatusTrialing, ProviderSubscriptionID: "sub_1"}, OrgDeleteReasonCancelRequired},
		{"paused, no cancel", tenantBillingProfile{Status: StatusPaused, ProviderSubscriptionID: "sub_1"}, OrgDeleteReasonCancelRequired},
		{"stripe past due, no marker", tenantBillingProfile{Status: StatusPastDue, BillingProvider: providerStripe, ProviderSubscriptionID: "sub_1"}, OrgDeleteReasonPastDue},
		{"stripe past due, period-end cancel only", tenantBillingProfile{Status: StatusPastDue, BillingProvider: providerStripe, ProviderSubscriptionID: "sub_1", CancelAtPeriodEnd: true, CancelAt: &future}, OrgDeleteReasonPastDue},
		{"razorpay past due, past cancel_at", tenantBillingProfile{Status: StatusPastDue, BillingProvider: providerRazorpay, ProviderSubscriptionID: "sub_1", CancelAtPeriodEnd: true, CancelAt: &past}, OrgDeleteReasonPastDue},
		{"comped, subscription attached", tenantBillingProfile{Status: StatusComped, ProviderSubscriptionID: "sub_1", CancelAtPeriodEnd: true}, OrgDeleteReasonComped},
		{"none, subscription attached", tenantBillingProfile{Status: StatusNone, ProviderSubscriptionID: "sub_1", CancelAtPeriodEnd: true}, OrgDeleteReasonPending},
		{"unknown status, subscription attached", tenantBillingProfile{Status: Status("mystery"), ProviderSubscriptionID: "sub_1", CancelAtPeriodEnd: true}, OrgDeleteReasonPending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := orgDeleteBlock(tt.p, now)
			if tt.wantReason == "" {
				if err != nil {
					t.Fatalf("want allowed, got %v", err)
				}
				return
			}
			de, ok := domain.AsDomain(err)
			if !ok {
				t.Fatalf("want a 409 %s (%s), got %v", OrgDeleteBlockedCode, tt.wantReason, err)
			}
			if de.Kind != domain.KindConflict || de.Code != OrgDeleteBlockedCode {
				t.Fatalf("kind/code = %v/%s, want conflict/%s", de.Kind, de.Code, OrgDeleteBlockedCode)
			}
			if got := de.Details["reason"]; got != tt.wantReason {
				t.Fatalf("details.reason = %v, want %s", got, tt.wantReason)
			}
		})
	}
}

// TestNextBillingState_CancelSchedule proves the cancel schedule follows a
// provider that reports one, including a resume that clears it, and is kept
// for a provider that reports none.
func TestNextBillingState_CancelSchedule(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	at := now.Add(30 * 24 * time.Hour)
	stored := now.Add(10 * 24 * time.Hour)
	current := tenantBillingProfile{
		Status: StatusActive, ProviderSubscriptionID: "sub_1",
		CancelAtPeriodEnd: true, CancelAt: &stored,
	}

	reported := Subscription{ID: "sub_1", Status: StatusActive, CancelScheduleReported: true, CancelAtPeriodEnd: true, CancelAt: at}
	next := nextBillingState(current, reported, now)
	if !next.CancelAtPeriodEnd || next.CancelAt == nil || !next.CancelAt.Equal(at) {
		t.Fatalf("reported schedule not applied: %+v", next)
	}

	resumed := Subscription{ID: "sub_1", Status: StatusActive, CancelScheduleReported: true}
	next = nextBillingState(current, resumed, now)
	if next.CancelAtPeriodEnd || next.CancelAt != nil {
		t.Fatalf("a reported resume must clear the schedule, got %+v", next)
	}

	unreported := Subscription{ID: "sub_1", Status: StatusActive}
	next = nextBillingState(current, unreported, now)
	if !next.CancelAtPeriodEnd || next.CancelAt == nil || !next.CancelAt.Equal(stored) {
		t.Fatalf("an unreported schedule must keep the stored one, got %+v", next)
	}
}

// TestNextBillingState_NewSubscriptionStartsUncancelled proves a stored
// cancel schedule belongs to the subscription it was stored for: adopting a
// different subscription from a provider that reports no schedule starts it
// with no cancel scheduled, so the org delete is refused while it is live.
func TestNextBillingState_NewSubscriptionStartsUncancelled(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	past := now.Add(-24 * time.Hour)
	for _, storedSub := range []string{"sub_1", ""} {
		current := tenantBillingProfile{
			Plan: TierFree, Status: StatusCanceled, BillingProvider: providerRazorpay,
			ProviderSubscriptionID: storedSub, CancelAtPeriodEnd: true, CancelAt: &past,
		}
		sub := Subscription{ID: "sub_2", Status: StatusActive, Plan: TierAgency, PlanResolved: true}
		next := nextBillingState(current, sub, now)
		if next.CancelAtPeriodEnd || next.CancelAt != nil {
			t.Fatalf("stored sub %q: the new subscription inherited the old schedule: %+v", storedSub, next)
		}
		err := orgDeleteBlock(next, now)
		de, ok := domain.AsDomain(err)
		if !ok || de.Details["reason"] != OrgDeleteReasonCancelRequired {
			t.Fatalf("stored sub %q: org delete with a live uncancelled subscription = %v, want %s", storedSub, err, OrgDeleteReasonCancelRequired)
		}
	}
}

// TestParseCancelWhen covers the request's when values.
func TestParseCancelWhen(t *testing.T) {
	for in, want := range map[string]CancelWhen{"": CancelAtPeriodEnd, "period_end": CancelAtPeriodEnd, "now": CancelNow} {
		got, err := ParseCancelWhen(in)
		if err != nil || got != want {
			t.Errorf("ParseCancelWhen(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"NOW", "immediately", "period-end", " now"} {
		_, err := ParseCancelWhen(in)
		de, ok := domain.AsDomain(err)
		if !ok || de.Kind != domain.KindValidation {
			t.Errorf("ParseCancelWhen(%q) = %v, want a 422 validation error", in, err)
		}
	}
}
