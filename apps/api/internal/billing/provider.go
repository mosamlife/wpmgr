package billing

import (
	"context"
	"net/http"
	"sort"
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
	// EventTaxIDUpdated is a change to a customer's saved tax ID. It never
	// changes billing state; the apply worker records it and raises a warning
	// when the ID is unverified.
	EventTaxIDUpdated EventKind = "tax_id_updated"
)

// Ownership says whether a verified webhook event belongs to WPMgr. A payment
// account can be shared with other products, so an adapter classifies every
// verified event before intake records anything.
//
// The zero value means owned: an adapter that never sets the field (Razorpay,
// the integration fake) produces owned events, which intake records and the
// apply worker resolves to a tenant or reports as a mismatch.
type Ownership uint8

const (
	// OwnershipOwned is the zero value: the event is WPMgr's.
	OwnershipOwned Ownership = iota
	// OwnershipForeign marks an event another product on the same account
	// produced. Intake acknowledges it with 200 and records nothing.
	OwnershipForeign
	// OwnershipByCustomer marks an event that carries only a customer id.
	// Intake treats it as owned when a tenant stores that customer for this
	// provider, and as foreign otherwise.
	OwnershipByCustomer
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
	// CustomerEmail is the signed-in owner's email. Best-effort: may be
	// empty. A provider that creates its checkout on a stored customer sets
	// the email on that customer instead.
	CustomerEmail string
	// ProviderCustomerID is the stored provider customer the checkout is
	// created on, as returned by the checkout binding. A provider that needs
	// one (Stripe) refuses an empty value.
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
	// SessionID is the hosted-redirect provider's session id. It never
	// reaches the wire; the service uses it to expire or supersede the
	// session it just created.
	SessionID string `json:"-"`
}

// CustomerCreator is an OPTIONAL capability of a provider whose checkout is
// created on a stored provider customer (Stripe). CreateCustomer creates a
// new customer for tenantID marked as WPMgr's; it never looks a customer up
// by email.
type CustomerCreator interface {
	CreateCustomer(ctx context.Context, tenantID uuid.UUID, email string) (string, error)
}

// SubscriptionLister is an OPTIONAL capability of a provider that can prove,
// from the provider side, that nothing is pending or live for a customer.
// A provider without it cannot be switched away from self-serve.
//
// Every list an implementation sends carries the customer filter. An empty
// customerID has nothing findable: HasPendingOrLive answers "nothing
// pending" without any provider request, and PendingOrLiveSubscription
// returns an error without any provider request.
type SubscriptionLister interface {
	// HasPendingOrLive expires customerID's open checkout sessions, then
	// reports one pending or live subscription id, if any.
	HasPendingOrLive(ctx context.Context, customerID string) (subscriptionID string, pending bool, err error)
	// PendingOrLiveSubscription reports one pending or live subscription id
	// of customerID, if any, and changes nothing.
	PendingOrLiveSubscription(ctx context.Context, customerID string) (subscriptionID string, pending bool, err error)
}

// CheckoutSessionSuperseder is an OPTIONAL capability of a hosted-redirect
// provider that keeps at most one open checkout session per customer.
type CheckoutSessionSuperseder interface {
	// SupersedeOpenCheckoutSessions expires every open session of customerID
	// except the greatest by (created, id). ownSuperseded reports that
	// ownSessionID was one of the sessions that is not the greatest.
	SupersedeOpenCheckoutSessions(ctx context.Context, customerID, ownSessionID string) (ownSuperseded bool, err error)
	// ExpireCheckoutSession expires one session by id.
	ExpireCheckoutSession(ctx context.Context, sessionID string) error
}

// CheckoutSessionInfo is what the confirm path reads from a provider's
// checkout session.
type CheckoutSessionInfo struct {
	ID                string
	ClientReferenceID string
	CustomerID        string
	SubscriptionID    string
	Mode              string
	Status            string
}

// CheckoutSessionConfirmer is an OPTIONAL capability of a hosted-redirect
// provider whose browser returns with a session id the service can check.
type CheckoutSessionConfirmer interface {
	RetrieveCheckoutSession(ctx context.Context, sessionID string) (CheckoutSessionInfo, error)
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
	// CancelAt is the instant the provider scheduled the subscription to end,
	// or ended it. The zero value means no end is scheduled.
	CancelAt time.Time
	// CancelScheduleReported is true when the provider reports its cancel
	// schedule on the subscription itself, so CancelAtPeriodEnd and CancelAt
	// are authoritative. When false the provider reports none, and the
	// schedule stored by the in-app cancel is kept.
	CancelScheduleReported bool
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

	// Ownership is the adapter's classification (see Ownership). The zero
	// value means owned.
	Ownership Ownership

	// TaxIDTypes and BillingCountry are read from a completed checkout
	// session's customer details: the types of the tax IDs the buyer gave
	// (never the values) and the billing address country. Intake stores them
	// in the ledger payload, so later work reads them from the ledger row.
	TaxIDTypes     []string
	BillingCountry string

	// TaxIDType and TaxIDVerificationStatus describe a tax_id_updated event's
	// ID: its type and its verification status (e.g. "verified",
	// "unverified", "pending"). The ID value itself is never read.
	TaxIDType               string
	TaxIDVerificationStatus string
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

// CheckoutSessionExpirer is an OPTIONAL capability a Provider may implement
// when its hosted checkout leaves session objects open after a subscription
// has started. The apply worker calls it after an activation commits, outside
// any transaction, so a customer is left with no second payable checkout.
//
// Implementations must refuse an empty providerCustomerID with an error and
// send no request, must list only that customer's sessions, and must expire
// only sessions whose own customer equals it.
type CheckoutSessionExpirer interface {
	ExpireOpenCheckoutSessions(ctx context.Context, providerCustomerID string) (expired int, err error)
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

// Names returns the registered provider names, "stripe" first and the rest
// in lexical order. It never returns nil.
func (r *Registry) Names() []string {
	out := []string{}
	if r == nil {
		return out
	}
	for name := range r.providers {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i] == "stripe") != (out[j] == "stripe") {
			return out[i] == "stripe"
		}
		return out[i] < out[j]
	})
	return out
}

// DefaultCheckoutProvider is the provider a checkout that names none uses:
// "stripe" when it is registered, else the only registered provider, else "".
func (r *Registry) DefaultCheckoutProvider() string {
	names := r.Names()
	if len(names) == 0 {
		return ""
	}
	if names[0] == "stripe" || len(names) == 1 {
		return names[0]
	}
	return ""
}
