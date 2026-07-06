package billing

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// EventKind is the normalized shape a payment-provider webhook event is
// mapped to. Every provider adapter (Stripe today, Razorpay next) translates
// its own provider-native event taxonomy into these ten values so the state
// machine in state_machine.go never has to know a provider exists.
type EventKind string

const (
	EventActivated        EventKind = "activated"
	EventTrialStarted     EventKind = "trial_started"
	EventPastDue          EventKind = "past_due"
	EventCanceled         EventKind = "canceled"
	EventPaused           EventKind = "paused"
	EventResumed          EventKind = "resumed"
	EventPaymentSucceeded EventKind = "payment_succeeded"
	EventPaymentFailed    EventKind = "payment_failed"
	EventRefunded         EventKind = "refunded"
	EventUpdated          EventKind = "updated"
)

// CheckoutInput describes a request to start a hosted checkout for one
// tenant. Plan is resolved server-side to a provider price/plan ID by the
// Provider implementation (see Stripe's planToPrice map) — the caller (and
// therefore the HTTP client) never names a price ID directly.
type CheckoutInput struct {
	TenantID uuid.UUID
	Plan     Tier
	// CustomerEmail prefills the checkout form. Best-effort: may be empty.
	CustomerEmail string
	// ProviderCustomerID reuses an existing provider customer record when the
	// tenant has one (e.g. a lapsed subscription being restarted). Empty for a
	// tenant's first-ever checkout.
	ProviderCustomerID string
	SuccessURL         string
	CancelURL          string
}

// CheckoutSession is the result of starting a hosted checkout: a URL the
// caller redirects the browser to.
type CheckoutSession struct {
	URL string
}

// PortalSession is the result of minting a billing-management portal session:
// a short-lived URL the caller redirects the browser to.
type PortalSession struct {
	URL string
}

// Subscription is a provider subscription's CURRENT state, as freshly
// fetched via Provider.GetSubscription. This is the "pull" half of "push is a
// hint, pull is the truth": the webhook consumer never trusts a webhook
// payload's own plan/status claims — it re-fetches this shape and reconciles
// from it.
type Subscription struct {
	ID         string
	CustomerID string
	// Plan is the tier resolved from the subscription's price ID via
	// MapPriceToPlan. Meaningful only when PlanResolved is true.
	Plan Tier
	// PlanResolved is false when the subscription's price ID does not match
	// any tier this control plane knows about (a misconfiguration — a price
	// was created/changed in the provider dashboard without updating the
	// CP's price-to-tier config). Callers must NOT apply Plan when this is
	// false; see the "unknown price" handling in Service.ProcessWebhook.
	PlanResolved bool
	// Status is the provider's subscription status, normalized to the
	// project's Status vocabulary (see entitlements.go).
	Status            Status
	CurrentPeriodEnd  time.Time
	CancelAtPeriodEnd bool
}

// Event is a normalized payment-provider webhook event, as returned by
// Provider.VerifyWebhook.
//
// Plan/Status/CurrentPeriodEnd are best-effort hints lifted directly from the
// webhook payload (when the provider's event body happens to carry them) —
// they exist for logging/audit context ONLY. Service.ProcessWebhook never
// applies them to a tenant's billing state; it always re-fetches the
// authoritative Subscription via GetSubscription before mutating anything
// ("push is a hint, pull is the truth").
type Event struct {
	// ProviderEventID is the provider's own event id (e.g. Stripe's "evt_...").
	// Paired with the provider name, this is the dedup key in billing_events.
	ProviderEventID string
	// ProviderEventType is the provider-native event-type string (e.g.
	// "invoice.payment_failed"), stored verbatim in billing_events.kind for
	// debuggability. Distinct from the normalized Kind.
	ProviderEventType string
	Kind              EventKind
	// Handled is false for a provider event type this control plane does not
	// act on (still ledgered for completeness, but no tenant resolution or
	// state-machine work is attempted).
	Handled bool
	// TenantID is set when the webhook payload itself carries tenant
	// attribution (checkout session client_reference_id/metadata, or
	// subscription metadata). uuid.Nil means the caller must fall back to a
	// (provider, provider_customer_id) lookup.
	TenantID               uuid.UUID
	ProviderCustomerID     string
	ProviderSubscriptionID string
	Plan                   Tier
	Status                 Status
	CurrentPeriodEnd       time.Time
	OccurredAt             time.Time
	Raw                    []byte
}

// Provider is the payment-provider integration surface. internal/billing's
// state machine, webhook consumer, and HTTP handlers depend ONLY on this
// interface — never on a provider SDK type — so a second provider (Razorpay,
// for India) is a new adapter package, not a change to this package.
type Provider interface {
	// Name is the provider's stable identifier, stored in
	// tenants.billing_provider and billing_events.provider (e.g. "stripe").
	Name() string

	// CreateCheckout starts a hosted checkout for one tenant/tier and returns
	// the URL to redirect the browser to.
	CreateCheckout(ctx context.Context, in CheckoutInput) (CheckoutSession, error)

	// CreatePortalSession mints a short-lived billing-management portal
	// session for an existing provider customer.
	CreatePortalSession(ctx context.Context, providerCustomerID string) (PortalSession, error)

	// GetSubscription fetches a subscription's CURRENT state from the
	// provider. This is the sole source of truth the state machine acts on.
	GetSubscription(ctx context.Context, providerSubscriptionID string) (Subscription, error)

	// VerifyWebhook authenticates a raw webhook request (signature + replay
	// tolerance) and normalizes it to an Event. Returns an error (which the
	// HTTP layer maps to 401) on a forged or malformed signature — no
	// processing of any kind occurs before this succeeds.
	VerifyWebhook(rawBody []byte, headers http.Header) (Event, error)

	// MapPriceToPlan resolves a provider price/plan ID to a tier. Returns
	// (_, false) when the price is not one this control plane's config maps
	// to a tier (see the "unknown price" no-op path).
	MapPriceToPlan(priceID string) (Tier, bool)
}

// Registry is the set of payment providers wired at boot (from config). A
// provider only appears here when its configuration is actually present
// (e.g. Stripe's secret key + webhook secret + all three price IDs) — an
// unconfigured provider is simply absent, never a registered-but-broken
// entry. "Hosted with zero providers configured" is a legal boot state (the
// Phase A no-op behavior extends to Phase B: every checkout/portal call
// degrades to a clean 503 rather than a crash).
type Registry struct {
	providers map[string]Provider
}

// NewRegistry builds a Registry from zero or more providers. Providers with a
// duplicate Name() overwrite earlier ones (last wins) — callers should not
// register the same name twice.
func NewRegistry(providers ...Provider) *Registry {
	m := make(map[string]Provider, len(providers))
	for _, p := range providers {
		if p == nil {
			continue
		}
		m[p.Name()] = p
	}
	return &Registry{providers: m}
}

// Provider returns the named provider, or (_, false) when it is not
// registered (either never wired, or its configuration was incomplete at boot).
func (r *Registry) Provider(name string) (Provider, bool) {
	if r == nil {
		return nil, false
	}
	p, ok := r.providers[name]
	return p, ok
}

// Any reports whether at least one provider is registered.
func (r *Registry) Any() bool {
	return r != nil && len(r.providers) > 0
}
