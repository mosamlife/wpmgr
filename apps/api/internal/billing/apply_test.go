package billing

import (
	"testing"
	"time"
)

func TestOwnershipMismatch_PriceGuardForEveryStatus(t *testing.T) {
	profile := tenantBillingProfile{
		Plan: TierStarter, Status: StatusActive,
		BillingProvider: providerStripe, ProviderCustomerID: "cus_1", ProviderSubscriptionID: "sub_1",
	}
	for _, st := range []Status{StatusActive, StatusTrialing, StatusPastDue, StatusCanceled, StatusPaused, StatusNone} {
		sub := Subscription{ID: "sub_1", CustomerID: "cus_1", Status: st, PlanResolved: false}
		if got := ownershipMismatch(providerStripe, profile, sub); got != alertUnknownPrice {
			t.Errorf("status %s with an unresolved price: mismatch = %q, want %q", st, got, alertUnknownPrice)
		}
		sub.PlanResolved = true
		sub.Plan = TierAgency
		if got := ownershipMismatch(providerStripe, profile, sub); got != "" {
			t.Errorf("status %s with a resolved price: mismatch = %q, want none", st, got)
		}
	}
}

func TestOwnershipMismatch_Binding(t *testing.T) {
	base := tenantBillingProfile{
		Status: StatusActive, BillingProvider: providerStripe,
		ProviderCustomerID: "cus_1", ProviderSubscriptionID: "sub_1",
	}
	good := Subscription{ID: "sub_1", CustomerID: "cus_1", Status: StatusActive, PlanResolved: true, Plan: TierStarter}

	cases := []struct {
		name     string
		provider string
		profile  func(p tenantBillingProfile) tenantBillingProfile
		sub      func(s Subscription) Subscription
		want     string
	}{
		{"match", providerStripe, nil, nil, ""},
		{"pinned to another provider", providerStripe,
			func(p tenantBillingProfile) tenantBillingProfile { p.BillingProvider = providerRazorpay; return p }, nil, alertProviderMismatch},
		{"stripe with no stored customer", providerStripe,
			func(p tenantBillingProfile) tenantBillingProfile { p.ProviderCustomerID = ""; return p }, nil, alertCustomerMismatch},
		{"stripe with another customer", providerStripe, nil,
			func(s Subscription) Subscription { s.CustomerID = "cus_2"; return s }, alertCustomerMismatch},
		{"razorpay with no stored customer", providerRazorpay,
			func(p tenantBillingProfile) tenantBillingProfile {
				p.BillingProvider = providerRazorpay
				p.ProviderCustomerID = ""
				return p
			}, nil, ""},
		{"second live subscription", providerStripe, nil,
			func(s Subscription) Subscription { s.ID = "sub_2"; return s }, alertDoubleSubscription},
		{"new subscription after a cancel", providerStripe,
			func(p tenantBillingProfile) tenantBillingProfile { p.Status = StatusCanceled; return p },
			func(s Subscription) Subscription { s.ID = "sub_2"; return s }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, s := base, good
			if tc.profile != nil {
				p = tc.profile(p)
			}
			if tc.sub != nil {
				s = tc.sub(s)
			}
			if got := ownershipMismatch(tc.provider, p, s); got != tc.want {
				t.Fatalf("mismatch = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNextBillingState_CustomerIsWriteOnce(t *testing.T) {
	now := time.Now()
	sub := Subscription{ID: "sub_1", CustomerID: "cus_new", Status: StatusActive, PlanResolved: true, Plan: TierStarter}

	kept := nextBillingState(tenantBillingProfile{ProviderCustomerID: "cus_stored"}, sub, now)
	if kept.ProviderCustomerID != "cus_stored" {
		t.Fatalf("stored customer replaced: got %q, want cus_stored", kept.ProviderCustomerID)
	}
	filled := nextBillingState(tenantBillingProfile{}, sub, now)
	if filled.ProviderCustomerID != "cus_new" {
		t.Fatalf("empty customer not filled: got %q, want cus_new", filled.ProviderCustomerID)
	}
}

func TestInsertOpts_Uniqueness(t *testing.T) {
	apply := BillingApplyArgs{}.InsertOpts()
	if apply.Queue != BillingQueue || !apply.UniqueOpts.ByArgs || apply.UniqueOpts.ByPeriod != 0 || apply.UniqueOpts.ByState != nil {
		t.Fatalf("billing_apply opts = %+v; want queue %q, ByArgs only, no period, default states", apply, BillingQueue)
	}

	req := RequestRefreshInsertOpts()
	if req.Queue != BillingQueue || !req.UniqueOpts.ByArgs || req.UniqueOpts.ByPeriod != time.Minute {
		t.Fatalf("request refresh opts = %+v; want queue %q, ByArgs, one-minute period", req, BillingQueue)
	}
	// nil ByState means River's default, which includes Completed: a job
	// finished within the same minute still counts as a duplicate.
	if req.UniqueOpts.ByState != nil {
		t.Fatalf("request refresh ByState = %v, want nil (River's default states)", req.UniqueOpts.ByState)
	}

	worker := BillingRefreshArgs{}.InsertOpts()
	if worker.Queue != BillingQueue || worker.UniqueOpts.ByArgs {
		t.Fatalf("worker-side refresh opts = %+v; want queue %q and no uniqueness", worker, BillingQueue)
	}
}
