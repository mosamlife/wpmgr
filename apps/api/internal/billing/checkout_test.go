package billing

// checkout_test.go — pure, DB-free unit tests: CreateCheckout's tier
// validation and its disabled/unconfigured guards all short-circuit BEFORE
// touching the database, so a nil pool is safe here.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func TestCreateCheckout_RejectsNonPurchasableTier(t *testing.T) {
	svc := New(nil, nil, true, domain.SystemClock{}, nil)

	tests := []Tier{TierFree, Tier(""), Tier("enterprise"), Tier("price_agency_123")}
	for _, tier := range tests {
		_, err := svc.CreateCheckout(context.Background(), uuid.New(), tier, "", "", "", "", "", Actor{})
		de, ok := domain.AsDomain(err)
		if !ok || de.Kind != domain.KindValidation {
			t.Errorf("tier %q: want a KindValidation error, got %v", tier, err)
		}
	}
}

// TestCreateCheckout_RejectsNonPurchasableTier covers "the client can't name
// a price id": the ONLY thing a checkout request supplies is a tier string,
// and anything outside the fixed {starter, agency, scale} vocabulary
// (including something that LOOKS like a Stripe price id) is rejected here,
// before any provider is ever consulted.
func TestCreateCheckout_AcceptsEveryPaidTier(t *testing.T) {
	for _, tier := range []Tier{TierStarter, TierAgency, TierScale} {
		if !validPurchasableTier(tier) {
			t.Errorf("tier %q should be purchasable", tier)
		}
	}
}

func TestCreateCheckout_DisabledReturnsUnavailable(t *testing.T) {
	svc := New(nil, nil, false, domain.SystemClock{}, nil) // hosted billing OFF
	_, err := svc.CreateCheckout(context.Background(), uuid.New(), TierStarter, "", "", "", "", "", Actor{})
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindUnavailable {
		t.Fatalf("want KindUnavailable when hosted billing is disabled, got %v", err)
	}
}

func TestCreateCheckout_NoProviderRegisteredReturnsServiceUnavailable(t *testing.T) {
	svc := New(nil, nil, true, domain.SystemClock{}, nil)
	svc.SetProviders(NewRegistry(), "stripe") // registry built, but empty

	_, err := svc.CreateCheckout(context.Background(), uuid.New(), TierStarter, "", "", "", "", "", Actor{})
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindServiceUnavailable {
		t.Fatalf("want KindServiceUnavailable when no provider is configured, got %v", err)
	}
}

func TestCreatePortalSession_DisabledReturnsUnavailable(t *testing.T) {
	svc := New(nil, nil, false, domain.SystemClock{}, nil)
	_, err := svc.CreatePortalSession(context.Background(), uuid.New(), Actor{})
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindUnavailable {
		t.Fatalf("want KindUnavailable when hosted billing is disabled, got %v", err)
	}
}

func TestVerifyCheckoutCallback_DisabledReturnsUnavailable(t *testing.T) {
	svc := New(nil, nil, false, domain.SystemClock{}, nil)
	err := svc.VerifyCheckoutCallback(context.Background(), uuid.New(), map[string]string{})
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindUnavailable {
		t.Fatalf("want KindUnavailable when hosted billing is disabled, got %v", err)
	}
}

func TestCancelSubscription_DisabledReturnsUnavailable(t *testing.T) {
	svc := New(nil, nil, false, domain.SystemClock{}, nil)
	err := svc.CancelSubscription(context.Background(), uuid.New(), Actor{})
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindUnavailable {
		t.Fatalf("want KindUnavailable when hosted billing is disabled, got %v", err)
	}
}

func TestGetBillingSummary_DisabledReturnsUnavailable(t *testing.T) {
	svc := New(nil, nil, false, domain.SystemClock{}, nil)
	_, err := svc.GetBillingSummary(context.Background(), uuid.New())
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindUnavailable {
		t.Fatalf("want KindUnavailable when hosted billing is disabled, got %v", err)
	}
}
