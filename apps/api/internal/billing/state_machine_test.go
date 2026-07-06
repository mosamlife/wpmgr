package billing

import (
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

func TestNextBillingState_ActiveAppliesSubscribedPlanAndClearsGrace(t *testing.T) {
	grace := fixedNow.Add(-time.Hour)
	current := tenantBillingProfile{Plan: TierFree, Status: StatusPastDue, GraceUntil: &grace}
	sub := Subscription{ID: "sub_1", CustomerID: "cus_1", Plan: TierAgency, PlanResolved: true, Status: StatusActive, CurrentPeriodEnd: fixedNow.Add(30 * 24 * time.Hour)}

	got := nextBillingState(current, sub, fixedNow)

	if got.Plan != TierAgency {
		t.Fatalf("Plan = %q, want agency", got.Plan)
	}
	if got.Status != StatusActive {
		t.Fatalf("Status = %q, want active", got.Status)
	}
	if got.GraceUntil != nil {
		t.Fatalf("GraceUntil = %v, want nil (cleared on active)", got.GraceUntil)
	}
	if got.ProviderSubscriptionID != "sub_1" || got.ProviderCustomerID != "cus_1" {
		t.Fatalf("provider ids not carried forward: %+v", got)
	}
}

func TestNextBillingState_TrialingAppliesPlan(t *testing.T) {
	current := tenantBillingProfile{Plan: TierFree, Status: StatusNone}
	sub := Subscription{ID: "sub_2", Plan: TierStarter, PlanResolved: true, Status: StatusTrialing}

	got := nextBillingState(current, sub, fixedNow)
	if got.Plan != TierStarter || got.Status != StatusTrialing {
		t.Fatalf("got %+v", got)
	}
	if got.GraceUntil != nil {
		t.Fatal("trialing should not carry a grace window")
	}
}

func TestNextBillingState_PastDue_FirstEntrySetsSevenDayGrace(t *testing.T) {
	current := tenantBillingProfile{Plan: TierAgency, Status: StatusActive}
	sub := Subscription{Plan: TierAgency, PlanResolved: true, Status: StatusPastDue}

	got := nextBillingState(current, sub, fixedNow)

	if got.Status != StatusPastDue {
		t.Fatalf("Status = %q, want past_due", got.Status)
	}
	if got.Plan != TierAgency {
		t.Fatalf("Plan = %q, want the subscribed tier kept (agency)", got.Plan)
	}
	if got.GraceUntil == nil {
		t.Fatal("expected a grace_until to be set on first entry into past_due")
	}
	wantGrace := fixedNow.Add(pastDueGracePeriod)
	if !got.GraceUntil.Equal(wantGrace) {
		t.Fatalf("GraceUntil = %v, want %v (now+7d)", got.GraceUntil, wantGrace)
	}
}

func TestNextBillingState_PastDue_RepeatNotificationDoesNotExtendGrace(t *testing.T) {
	originalGrace := fixedNow.Add(2 * 24 * time.Hour) // already 2 days into a 7-day grace
	current := tenantBillingProfile{Plan: TierAgency, Status: StatusPastDue, GraceUntil: &originalGrace}
	sub := Subscription{Plan: TierAgency, PlanResolved: true, Status: StatusPastDue}

	// A LATER "now" (e.g. a repeat invoice.payment_failed retry webhook, or a
	// reconcile sweep run days later) must NOT push grace_until further out.
	later := fixedNow.Add(3 * 24 * time.Hour)
	got := nextBillingState(current, sub, later)

	if got.GraceUntil == nil || !got.GraceUntil.Equal(originalGrace) {
		t.Fatalf("GraceUntil = %v, want the ORIGINAL %v unchanged (repeat past_due must not extend the window)", got.GraceUntil, originalGrace)
	}
}

func TestNextBillingState_PastDue_UnresolvedPriceKeepsExistingPlan(t *testing.T) {
	current := tenantBillingProfile{Plan: TierScale, Status: StatusActive}
	sub := Subscription{PlanResolved: false, Status: StatusPastDue} // unknown price

	got := nextBillingState(current, sub, fixedNow)
	if got.Plan != TierScale {
		t.Fatalf("Plan = %q, want the tenant's EXISTING plan (scale) kept when the price is unresolved", got.Plan)
	}
	if got.Status != StatusPastDue {
		t.Fatalf("Status = %q, want past_due (status is trusted even when the price is not)", got.Status)
	}
}

func TestNextBillingState_Canceled_NonDestructiveDowngradeToFree(t *testing.T) {
	current := tenantBillingProfile{Plan: TierAgency, Status: StatusActive, ProviderCustomerID: "cus_1"}
	sub := Subscription{ID: "sub_3", CustomerID: "cus_1", Status: StatusCanceled}

	got := nextBillingState(current, sub, fixedNow)

	if got.Plan != TierFree {
		t.Fatalf("Plan = %q, want free (non-destructive downgrade)", got.Plan)
	}
	if got.Status != StatusCanceled {
		t.Fatalf("Status = %q, want canceled", got.Status)
	}
	if got.GraceUntil != nil {
		t.Fatal("canceled should carry no grace window")
	}
	// nextBillingState never touches anything site-related — there is no
	// sites/backups field on tenantBillingProfile at all, so a canceled
	// downgrade is structurally incapable of deleting anything; only the
	// EFFECTIVE cap changes (enforced separately by CheckSiteCreate/
	// Entitlements, covered by Phase A's own tests and the reconcile
	// integration test in tests/billing_webhook_integration_test.go).
	if got.ProviderCustomerID != "cus_1" {
		t.Fatalf("ProviderCustomerID should be preserved across cancellation for portal history access, got %q", got.ProviderCustomerID)
	}
}

func TestNextBillingState_Paused(t *testing.T) {
	current := tenantBillingProfile{Plan: TierStarter, Status: StatusActive}
	sub := Subscription{Plan: TierStarter, PlanResolved: true, Status: StatusPaused}

	got := nextBillingState(current, sub, fixedNow)
	if got.Status != StatusPaused {
		t.Fatalf("Status = %q, want paused", got.Status)
	}
	if got.GraceUntil != nil {
		t.Fatal("paused should carry no grace window")
	}
}

func TestNextBillingState_UnrecognizedStatusFallsBackToNone(t *testing.T) {
	current := tenantBillingProfile{Plan: TierAgency, Status: StatusActive}
	sub := Subscription{Status: Status("some_future_unrecognized_status")}

	got := nextBillingState(current, sub, fixedNow)
	if got.Plan != TierFree || got.Status != StatusNone {
		t.Fatalf("got %+v, want free/none", got)
	}
}

func TestStatusAppliesPlan(t *testing.T) {
	tests := []struct {
		status Status
		want   bool
	}{
		{StatusActive, true},
		{StatusTrialing, true},
		{StatusPastDue, true},
		{StatusCanceled, false},
		{StatusPaused, false},
		{StatusNone, false},
		{StatusComped, false},
	}
	for _, tt := range tests {
		if got := statusAppliesPlan(tt.status); got != tt.want {
			t.Errorf("statusAppliesPlan(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}
