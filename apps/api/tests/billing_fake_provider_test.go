package tests

// billing_fake_provider_test.go — a billing.Provider test double shared by
// every M16 Phase B webhook/reconcile integration test in this package. It
// deliberately does NOT touch a live Stripe account: VerifyWebhook just
// decodes a plain JSON test payload (bypassing signature schemes entirely —
// the Stripe adapter's OWN signature verification is unit-tested against
// stripe-go's testhelpers in internal/billing/stripe/provider_test.go), and
// GetSubscription reads from a static in-memory map that stands in for "the
// provider's current truth" ("pull is the truth": whatever this map holds IS
// what a GetSubscription call returns, regardless of which webhook event
// triggered it — exactly like a real payment provider).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
)

var errFakeSubscriptionNotFound = errors.New("fake provider: subscription not found")

// fakeEventPayload is the wire shape fakeEventBody marshals and
// fakeProvider.VerifyWebhook unmarshals — a direct stand-in for a normalized
// billing.Event, so a test can drive Service.ProcessWebhook's DB-backed logic
// (dedup, out-of-order, comped immunity, unknown-price/customer) precisely.
type fakeEventPayload struct {
	ID                     string    `json:"id"`
	Type                   string    `json:"type"`
	Kind                   string    `json:"kind"`
	Handled                bool      `json:"handled"`
	TenantID               uuid.UUID `json:"tenant_id"`
	ProviderCustomerID     string    `json:"provider_customer_id"`
	ProviderSubscriptionID string    `json:"provider_subscription_id"`
	OccurredAt             time.Time `json:"occurred_at"`
}

func fakeEventBody(p fakeEventPayload) []byte {
	b, err := json.Marshal(p)
	if err != nil {
		panic(err) // test-only helper; a marshal failure here is a test bug.
	}
	return b
}

// fakeProvider implements billing.Provider.
type fakeProvider struct {
	name          string
	subscriptions map[string]billing.Subscription

	getSubscriptionCalls int32 // atomic

	lastCheckoutInput billing.CheckoutInput
	checkoutErr       error
	portalErr         error
	verifyErr         error
	cancelErr         error

	// lastCancelSubscriptionID records what CancelSubscription was called
	// with, so a test can assert the Service actually told THIS provider to
	// cancel THIS subscription id.
	lastCancelSubscriptionID string

	// noPortal flips HasPortal() to false (a Razorpay-like provider). Zero
	// value (false) keeps every existing test's Stripe-like (portal-having)
	// assumption working unchanged.
	noPortal bool
}

func newFakeProvider(name string) *fakeProvider {
	return &fakeProvider{name: name, subscriptions: map[string]billing.Subscription{}}
}

func (f *fakeProvider) Name() string { return f.name }

// HasPortal implements billing.Provider.
func (f *fakeProvider) HasPortal() bool { return !f.noPortal }

func (f *fakeProvider) CreateCheckout(_ context.Context, in billing.CheckoutInput) (billing.CheckoutSession, error) {
	f.lastCheckoutInput = in
	if f.checkoutErr != nil {
		return billing.CheckoutSession{}, f.checkoutErr
	}
	return billing.CheckoutSession{URL: "https://fake.test/checkout/" + in.TenantID.String()}, nil
}

func (f *fakeProvider) CreatePortalSession(_ context.Context, providerCustomerID string) (billing.PortalSession, error) {
	if f.portalErr != nil {
		return billing.PortalSession{}, f.portalErr
	}
	return billing.PortalSession{URL: "https://fake.test/portal/" + providerCustomerID}, nil
}

func (f *fakeProvider) GetSubscription(_ context.Context, id string) (billing.Subscription, error) {
	atomic.AddInt32(&f.getSubscriptionCalls, 1)
	sub, ok := f.subscriptions[id]
	if !ok {
		return billing.Subscription{}, errFakeSubscriptionNotFound
	}
	return sub, nil
}

func (f *fakeProvider) VerifyWebhook(rawBody []byte, _ http.Header) (billing.Event, error) {
	if f.verifyErr != nil {
		return billing.Event{}, f.verifyErr
	}
	var in fakeEventPayload
	if err := json.Unmarshal(rawBody, &in); err != nil {
		return billing.Event{}, err
	}
	return billing.Event{
		ProviderEventID:        in.ID,
		ProviderEventType:      in.Type,
		Kind:                   billing.EventKind(in.Kind),
		Handled:                in.Handled,
		TenantID:               in.TenantID,
		ProviderCustomerID:     in.ProviderCustomerID,
		ProviderSubscriptionID: in.ProviderSubscriptionID,
		OccurredAt:             in.OccurredAt,
	}, nil
}

func (f *fakeProvider) MapPriceToPlan(string) (billing.Tier, bool) {
	return "", false // not exercised by these tests
}

// CancelSubscription implements billing.Provider. Records the id it was
// called with (never mutates any state — mirrors real adapters, which only
// tell the provider to cancel; the tenant's plan/status changes ONLY via a
// subsequent webhook, driven separately in tests that need it).
func (f *fakeProvider) CancelSubscription(_ context.Context, providerSubscriptionID string) error {
	f.lastCancelSubscriptionID = providerSubscriptionID
	return f.cancelErr
}
