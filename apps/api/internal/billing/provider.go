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
	// Currency is an ISO 4217 currency code (e.g. "USD", "INR"), meaningful
	// ONLY to a provider whose CreateCheckout resolves a price/plan PER
	// (tier, currency) — e.g. Razorpay's dual-currency plan model (one
	// Razorpay Plan per currency per tier, since Razorpay has no single
	// multi-currency price object the way Stripe does). Stripe's own Price
	// already encodes its currency and ignores this field entirely. Empty is
	// legal for any provider that does not need it.
	Currency string
	// CustomerEmail prefills the checkout form. Best-effort: may be empty.
	CustomerEmail string
	// ProviderCustomerID reuses an existing provider customer record when the
	// tenant has one (e.g. a lapsed subscription being restarted). Empty for a
	// tenant's first-ever checkout.
	ProviderCustomerID string
	SuccessURL         string
	CancelURL          string
}

// RazorpayCheckoutData is the data WPMgr's in-app Checkout.js modal needs to
// open a Razorpay subscription checkout — Razorpay has no hosted Checkout
// Session object (unlike Stripe's redirect URL), so the CP must create the
// Subscription server-side and hand the browser just enough to open the
// modal itself. Declared here (in the core billing package, not
// internal/billing/razorpay) so CheckoutSession's shape — and therefore the
// HTTP wire contract the frontend depends on — never requires importing the
// razorpay adapter package.
type RazorpayCheckoutData struct {
	// SubscriptionID is the just-created Razorpay subscription id
	// (Checkout.js's "subscription_id" option).
	SubscriptionID string `json:"subscription_id"`
	// KeyID is Razorpay's PUBLIC key id (Checkout.js's "key" option) — never
	// the key secret.
	KeyID string `json:"key_id"`
	// Currency is the resolved plan's ISO 4217 currency code, echoing back
	// in.Currency for display.
	Currency string `json:"currency"`
	// AmountMinor is the per-billing-cycle charge amount in the currency's
	// smallest unit (paise for INR, cents for USD) — the exact shape
	// Checkout.js's own "amount" option expects, read authoritatively off the
	// Razorpay Plan (never computed/guessed CP-side, so it can never drift
	// from what Razorpay will actually charge).
	AmountMinor int64 `json:"amount"`
}

// CheckoutSession is the result of starting a hosted checkout. Exactly one of
// URL or Razorpay is populated, depending on the provider's checkout style:
//
//   - A hosted-redirect provider (Stripe) sets URL and leaves Razorpay nil —
//     the caller redirects the browser to URL.
//   - An in-app-modal provider (Razorpay) sets Razorpay and leaves URL empty —
//     the caller hands Razorpay to the frontend's Checkout.js modal.
type CheckoutSession struct {
	URL      string                `json:"url,omitempty"`
	Razorpay *RazorpayCheckoutData `json:"razorpay,omitempty"`
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

	// CancelSubscription tells the provider to cancel providerSubscriptionID.
	// Both adapters cancel AT THE END OF THE CURRENT BILLING PERIOD, never
	// immediately — the customer keeps paid-tier access through what they
	// already paid for, matching the state machine's existing
	// non-destructive-downgrade intent (state_machine.go's StatusCanceled
	// case just downgrades to free; it never deletes anything).
	//
	// This method NEVER mutates a tenant's stored plan/status itself —
	// "push is a hint, pull is the truth" applies here exactly as it does to
	// every other billing mutation: the resulting subscription.cancelled (or
	// Stripe's customer.subscription.updated with cancel_at_period_end=true)
	// webhook is what actually drives the downgrade, through the EXACT SAME
	// ProcessWebhook/state-machine path as any other event. Callers
	// (Service.CancelSubscription) must not assume the plan has changed the
	// instant this call returns.
	CancelSubscription(ctx context.Context, providerSubscriptionID string) error

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

	// HasPortal reports whether this provider offers a hosted self-service
	// billing-management portal (Stripe: yes. Razorpay: no — Razorpay has no
	// equivalent of Stripe's Billing Portal; its CreatePortalSession returns a
	// domain KindUnavailable error rather than fabricating a URL). Callers
	// (GetBillingSummary's PortalAvailable, the billing Handler) use this to
	// decide whether to advertise/attempt a portal link at all, rather than
	// discovering "not supported" only after calling CreatePortalSession.
	HasPortal() bool
}

// CheckoutCallbackVerifier is an OPTIONAL capability a Provider may implement
// when its checkout flow returns a browser-side completion callback that
// needs its own signature check — e.g. Razorpay's Checkout.js onSuccess
// handler, which hands the browser {razorpay_payment_id,
// razorpay_subscription_id, razorpay_signature} that must be HMAC-verified
// before the frontend trusts "the modal succeeded". Stripe's redirect-based
// Checkout Session has no equivalent and does not implement this interface.
//
// Declared as a SEPARATE, optional interface (type-asserted by
// Service.VerifyCheckoutCallback) rather than folded into Provider itself:
// a browser-callback-verify step is not a capability every payment provider
// needs, so widening the core Provider interface for it would force every
// adapter (including test doubles) to carry a method most of them never use.
//
// CRITICAL: this is ONLY a UX confirmation that the client-side modal
// succeeded. The webhook (Service.ProcessWebhook) remains the SOLE source of
// truth for granting a plan change — an implementation of this method must
// NEVER be treated as authorization to mutate a tenant's billing state, and
// none of the callers in this codebase do.
type CheckoutCallbackVerifier interface {
	VerifyCheckoutCallback(payload map[string]string) error
}

// StripePriceReader is an OPTIONAL capability a Provider may implement to
// expose its live per-tier list price WITHOUT creating a subscription — the
// read backing the public GET /api/v1/pricing endpoint (internal/pricing),
// which the marketing site polls to show accurate prices. Declared as a
// SEPARATE, optional interface (mirrors CheckoutCallbackVerifier immediately
// above) rather than folded into Provider itself: a price-introspection
// capability is not something every payment-provider adapter or test double
// needs to carry.
type StripePriceReader interface {
	// GetPrice reads tier's price via a side-effect-free provider lookup (no
	// subscription created) and returns the per-billing-cycle amount in the
	// currency's smallest unit, its ISO 4217 currency code, and the billing
	// interval (e.g. "month").
	GetPrice(ctx context.Context, tier Tier) (amountMinor int64, currency string, interval string, err error)
}

// RazorpayPlanReader is the Razorpay-shaped equivalent of StripePriceReader:
// Razorpay has no single multi-currency price object the way Stripe does —
// it maintains one Plan PER CURRENCY PER TIER (see the razorpay package's own
// doc comment) — so its price read is additionally keyed by currency.
type RazorpayPlanReader interface {
	// GetPlanAmount reads the (tier, currency) Plan's authoritative amount via
	// a side-effect-free provider lookup (no subscription created).
	GetPlanAmount(ctx context.Context, tier Tier, currency string) (amountMinor int64, resolvedCurrency string, interval string, err error)
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
