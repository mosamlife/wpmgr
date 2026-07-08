// Package stripe implements billing.Provider for Stripe. It is the FIRST of
// what M16 Phase B's architecture is explicit about being a multi-provider
// system: nothing outside this package (and cmd/wpmgr/main.go's boot wiring)
// ever imports github.com/stripe/stripe-go — the core internal/billing
// package, its state machine, and its HTTP handlers know only the
// billing.Provider interface. A second adapter (e.g. Razorpay, for India)
// is a new sibling package implementing the same interface; nothing here
// changes.
package stripe

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	stripesdk "github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// tenantMetadataKey is the Stripe metadata/custom-field key this adapter
// stamps on every Checkout Session AND the resulting Subscription (via
// SubscriptionData.Metadata), so a `customer.subscription.*` webhook is
// attributable to a tenant WITHOUT waiting on a customer-id lookup. Also read
// (as a snapshot) off Invoice.Parent.SubscriptionDetails.Metadata for
// invoice.* events.
const tenantMetadataKey = "wpmgr_tenant_id"

// Config is the Stripe adapter's construction-time configuration, sourced
// from config.BillingConfig.Stripe (WPMGR_BILLING_STRIPE_*).
type Config struct {
	SecretKey     string
	WebhookSecret string
	PriceStarter  string
	PriceAgency   string
	PriceScale    string
	// PortalReturnURL is the URL Stripe's Billing Portal offers as "Return to
	// <business>". Optional: when empty, Stripe falls back to the Customer
	// Portal configuration's own default return URL (set once in the Stripe
	// Dashboard), so an empty value is a legal, working configuration.
	PortalReturnURL string
	// HTTPClient overrides the Stripe SDK's default HTTP client. Nil uses the
	// SDK default. Exposed for tests (never used to change TLS/security
	// behavior — only to point at a test double).
	HTTPClient *http.Client
}

// Configured reports whether cfg has everything this adapter needs to
// operate: the secret key, the webhook signing secret, and all three price
// ids. cmd/wpmgr/main.go only registers this provider in the billing.Registry
// when Configured() is true — a partially-set Stripe config is refused at
// boot by internal/config.Validate, never silently half-wired into a
// Provider that would panic or misbehave on first use.
func (c Config) Configured() bool {
	return c.SecretKey != "" && c.WebhookSecret != "" &&
		c.PriceStarter != "" && c.PriceAgency != "" && c.PriceScale != ""
}

// Provider implements billing.Provider for Stripe.
type Provider struct {
	client          *stripesdk.Client
	webhookSecret   string
	portalReturnURL string
	priceToPlan     map[string]billing.Tier
	planToPrice     map[billing.Tier]string
}

// New builds a Stripe Provider. Callers should check cfg.Configured() first
// (or rely on the registry-building code in cmd/wpmgr/main.go, which already
// does).
func New(cfg Config) *Provider {
	var opts []stripesdk.ClientOption
	if cfg.HTTPClient != nil {
		opts = append(opts, stripesdk.WithBackends(stripesdk.NewBackends(cfg.HTTPClient)))
	}
	return &Provider{
		client:          stripesdk.NewClient(cfg.SecretKey, opts...),
		webhookSecret:   cfg.WebhookSecret,
		portalReturnURL: cfg.PortalReturnURL,
		priceToPlan: map[string]billing.Tier{
			cfg.PriceStarter: billing.TierStarter,
			cfg.PriceAgency:  billing.TierAgency,
			cfg.PriceScale:   billing.TierScale,
		},
		planToPrice: map[billing.Tier]string{
			billing.TierStarter: cfg.PriceStarter,
			billing.TierAgency:  cfg.PriceAgency,
			billing.TierScale:   cfg.PriceScale,
		},
	}
}

// Name implements billing.Provider.
func (p *Provider) Name() string { return "stripe" }

// HasPortal implements billing.Provider: Stripe's Billing Portal is always
// available once a customer exists.
func (p *Provider) HasPortal() bool { return true }

// MapPriceToPlan implements billing.Provider.
func (p *Provider) MapPriceToPlan(priceID string) (billing.Tier, bool) {
	t, ok := p.priceToPlan[priceID]
	return t, ok
}

// CreateCheckout implements billing.Provider. The price id is resolved
// SERVER-SIDE from in.Plan via planToPrice — the caller (ultimately, the
// HTTP request body) never supplies a price id.
func (p *Provider) CreateCheckout(ctx context.Context, in billing.CheckoutInput) (billing.CheckoutSession, error) {
	price, ok := p.planToPrice[in.Plan]
	if !ok || price == "" {
		return billing.CheckoutSession{}, domain.Validation("billing_unknown_tier", "no Stripe price is configured for this tier")
	}

	tenantID := in.TenantID.String()
	params := &stripesdk.CheckoutSessionCreateParams{
		Mode:              stripesdk.String(string(stripesdk.CheckoutSessionModeSubscription)),
		SuccessURL:        stripesdk.String(in.SuccessURL),
		CancelURL:         stripesdk.String(in.CancelURL),
		ClientReferenceID: stripesdk.String(tenantID),
		Metadata:          map[string]string{tenantMetadataKey: tenantID},
		LineItems: []*stripesdk.CheckoutSessionCreateLineItemParams{
			{Price: stripesdk.String(price), Quantity: stripesdk.Int64(1)},
		},
		// Stamping the SAME tenant id onto the subscription's own metadata
		// (not just the checkout session's) means every future
		// customer.subscription.* webhook is self-attributing — no customer-id
		// lookup fallback is needed for the common case.
		SubscriptionData: &stripesdk.CheckoutSessionCreateSubscriptionDataParams{
			Metadata: map[string]string{tenantMetadataKey: tenantID},
		},
	}
	if in.ProviderCustomerID != "" {
		params.Customer = stripesdk.String(in.ProviderCustomerID)
	} else if in.CustomerEmail != "" {
		params.CustomerEmail = stripesdk.String(in.CustomerEmail)
	}

	sess, err := p.client.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return billing.CheckoutSession{}, wrapErr("stripe_checkout_create_failed", "failed to create Stripe checkout session", err)
	}
	return billing.CheckoutSession{URL: sess.URL}, nil
}

// CreatePortalSession implements billing.Provider.
func (p *Provider) CreatePortalSession(ctx context.Context, providerCustomerID string) (billing.PortalSession, error) {
	params := &stripesdk.BillingPortalSessionCreateParams{
		Customer: stripesdk.String(providerCustomerID),
	}
	if p.portalReturnURL != "" {
		params.ReturnURL = stripesdk.String(p.portalReturnURL)
	}
	sess, err := p.client.V1BillingPortalSessions.Create(ctx, params)
	if err != nil {
		return billing.PortalSession{}, wrapErr("stripe_portal_create_failed", "failed to create Stripe billing portal session", err)
	}
	return billing.PortalSession{URL: sess.URL}, nil
}

// CancelSubscription implements billing.Provider: schedules cancellation at
// the END of the current billing period (cancel_at_period_end=true) rather
// than an immediate delete — the customer keeps access through what they
// already paid for, and the tenant's own downgrade-to-free happens later,
// driven by the resulting customer.subscription.updated webhook through the
// normal ProcessWebhook path (never mutated directly here). Stripe customers
// can also reach this same effect via the Billing Portal
// (CreatePortalSession); this method is the programmatic equivalent for a
// provider (like Razorpay) that has no portal at all.
func (p *Provider) CancelSubscription(ctx context.Context, providerSubscriptionID string) error {
	if _, err := p.client.V1Subscriptions.Update(ctx, providerSubscriptionID, cancelSubscriptionParams()); err != nil {
		return wrapErr("stripe_subscription_cancel_failed", "failed to schedule Stripe subscription cancellation", err)
	}
	return nil
}

// cancelSubscriptionParams builds the SubscriptionUpdateParams
// CancelSubscription sends. Extracted as a pure function (mirrors
// toSubscription/mapStatus below) so the cancel-at-period-end — NEVER
// immediate — choice is unit-testable without a live Stripe API call.
func cancelSubscriptionParams() *stripesdk.SubscriptionUpdateParams {
	return &stripesdk.SubscriptionUpdateParams{
		CancelAtPeriodEnd: stripesdk.Bool(true),
	}
}

// GetSubscription implements billing.Provider — the sole source of truth the
// state machine acts on ("pull is the truth").
func (p *Provider) GetSubscription(ctx context.Context, providerSubscriptionID string) (billing.Subscription, error) {
	sub, err := p.client.V1Subscriptions.Retrieve(ctx, providerSubscriptionID, nil)
	if err != nil {
		return billing.Subscription{}, wrapErr("stripe_subscription_fetch_failed", "failed to fetch the Stripe subscription", err)
	}
	return p.toSubscription(sub), nil
}

// toSubscription normalizes a Stripe Subscription to billing.Subscription.
// current_period_end and price now live on the FIRST subscription item (the
// Stripe API moved them off the subscription object itself) — see
// SubscriptionItem.CurrentPeriodEnd / SubscriptionItem.Price.
func (p *Provider) toSubscription(sub *stripesdk.Subscription) billing.Subscription {
	out := billing.Subscription{
		ID:                sub.ID,
		Status:            mapStatus(sub.Status),
		CancelAtPeriodEnd: sub.CancelAtPeriodEnd,
	}
	if sub.Customer != nil {
		out.CustomerID = sub.Customer.ID
	}
	if sub.Items != nil && len(sub.Items.Data) > 0 {
		item := sub.Items.Data[0]
		out.CurrentPeriodEnd = time.Unix(item.CurrentPeriodEnd, 0).UTC()
		if item.Price != nil {
			if tier, ok := p.priceToPlan[item.Price.ID]; ok {
				out.Plan = tier
				out.PlanResolved = true
			}
		}
	}
	return out
}

// mapStatus normalizes Stripe's subscription status vocabulary to the
// project's provider-agnostic billing.Status.
//
//   - active / trialing map directly.
//   - past_due AND unpaid both map to past_due: "unpaid" is what Stripe calls
//     a subscription whose configured payment-retry schedule has been
//     exhausted without an explicit cancellation — treating it as past_due
//     (rather than an immediate hard cancel) keeps the "fail toward the
//     customer" bias: they still get the existing 7-day grace window.
//   - canceled AND incomplete_expired both map to canceled (non-destructive
//     downgrade to free — see state_machine.go).
//   - paused maps directly.
//   - incomplete (a subscription whose FIRST invoice was never paid — not a
//     real subscription yet) and anything unrecognized map to StatusNone.
func mapStatus(s stripesdk.SubscriptionStatus) billing.Status {
	switch s {
	case stripesdk.SubscriptionStatusActive:
		return billing.StatusActive
	case stripesdk.SubscriptionStatusTrialing:
		return billing.StatusTrialing
	case stripesdk.SubscriptionStatusPastDue, stripesdk.SubscriptionStatusUnpaid:
		return billing.StatusPastDue
	case stripesdk.SubscriptionStatusCanceled, stripesdk.SubscriptionStatusIncompleteExpired:
		return billing.StatusCanceled
	case stripesdk.SubscriptionStatusPaused:
		return billing.StatusPaused
	default: // incomplete, or any future/unrecognized value.
		return billing.StatusNone
	}
}

// wrapErr maps a Stripe SDK error to a domain error. Stripe API errors
// (stripesdk.Error) carry a user-safe Msg; anything else (network/timeout) is
// wrapped as an opaque Internal error so no transport detail leaks.
func wrapErr(code, msg string, err error) error {
	var stripeErr *stripesdk.Error
	if errors.As(err, &stripeErr) {
		return domain.Internal(code, fmt.Sprintf("%s: %s", msg, stripeErr.Msg)).WithCause(err)
	}
	return domain.Internal(code, msg).WithCause(err)
}

// VerifyWebhook implements billing.Provider: verifies the Stripe-Signature
// header (raw body + the configured webhook signing secret, using Stripe's
// own constant-time HMAC verification with its default 5-minute replay
// tolerance) and normalizes the event. Returns an error on ANY signature
// failure — the caller (Service.ProcessWebhook) maps that to HTTP 401 without
// this function having touched the database.
//
// webhook.ConstructEvent ALSO refuses an event whose api_version does not
// match this stripe-go build's compiled stripesdk.APIVersion — deliberately
// NOT suppressed here (no IgnoreAPIVersionMismatch): a version mismatch can
// mean Stripe shaped the payload differently for an older/newer API version
// (e.g. current_period_end lived on the subscription object itself before it
// moved to the line item — see toSubscription), and silently misparsing a
// shape this code does not expect is worse than a loud, actionable failure.
// OPERATIONAL REQUIREMENT: the Stripe webhook endpoint for
// /webhooks/billing/stripe must be configured (Stripe supports pinning this
// per-endpoint, independent of the account's default API version) to send
// events at exactly stripesdk.APIVersion.
func (p *Provider) VerifyWebhook(rawBody []byte, headers http.Header) (billing.Event, error) {
	sigHeader := headers.Get("Stripe-Signature")
	ev, err := webhook.ConstructEvent(rawBody, sigHeader, p.webhookSecret)
	if err != nil {
		return billing.Event{}, err
	}

	out := billing.Event{
		ProviderEventID:   ev.ID,
		ProviderEventType: string(ev.Type),
		OccurredAt:        time.Unix(ev.Created, 0).UTC(),
		Raw:               ev.Data.Raw,
	}

	switch ev.Type {
	case stripesdk.EventTypeCheckoutSessionCompleted:
		p.applyCheckoutSession(&out, ev.Data.Object)
	case stripesdk.EventTypeCustomerSubscriptionCreated:
		p.applySubscriptionEvent(&out, ev.Data.Object, billing.EventActivated)
	case stripesdk.EventTypeCustomerSubscriptionUpdated:
		p.applySubscriptionEvent(&out, ev.Data.Object, billing.EventUpdated)
	case stripesdk.EventTypeCustomerSubscriptionDeleted:
		p.applySubscriptionEvent(&out, ev.Data.Object, billing.EventCanceled)
	case stripesdk.EventTypeInvoicePaymentFailed:
		p.applyInvoiceEvent(&out, ev.Data.Object, billing.EventPaymentFailed)
	case stripesdk.EventTypeInvoicePaid:
		p.applyInvoiceEvent(&out, ev.Data.Object, billing.EventPaymentSucceeded)
	case stripesdk.EventTypeChargeRefunded:
		p.applyChargeEvent(&out, ev.Data.Object, billing.EventRefunded)
	default:
		out.Handled = false
	}

	return out, nil
}

// stringField reads a string field out of a raw event-object map, returning
// "" for a missing/non-string/nil value. Stripe's EventData.Object is always
// map[string]interface{} (see stripe.EventData's doc comment); every field we
// need here is a plain JSON string or nested object, never requiring the full
// typed struct unmarshal, so using the map directly keeps this file free of
// brittle re-marshal/unmarshal round-trips.
func stringField(obj map[string]interface{}, key string) string {
	v, ok := obj[key]
	if !ok || v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// nestedID reads obj[key] as either a bare string id (an unexpanded
// expandable reference) or a nested object's own "id" field (an expanded
// reference), returning "" if neither shape matches or the field is absent.
func nestedID(obj map[string]interface{}, key string) string {
	v, ok := obj[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if nested, ok := v.(map[string]interface{}); ok {
		return stringField(nested, "id")
	}
	return ""
}

// stringMapField reads obj[key] as a map[string]string (Stripe's "metadata"
// field shape), returning nil if absent or of the wrong shape.
func stringMapField(obj map[string]interface{}, key string) map[string]string {
	v, ok := obj[key]
	if !ok || v == nil {
		return nil
	}
	raw, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, vv := range raw {
		if s, ok := vv.(string); ok {
			out[k] = s
		}
	}
	return out
}

// applyCheckoutSession fills out from a checkout.session.completed event's
// raw object.
func (p *Provider) applyCheckoutSession(out *billing.Event, obj map[string]interface{}) {
	if stringField(obj, "mode") != "subscription" {
		out.Handled = false
		return
	}
	out.Handled = true
	out.Kind = billing.EventActivated
	out.ProviderCustomerID = nestedID(obj, "customer")
	out.ProviderSubscriptionID = nestedID(obj, "subscription")
	out.TenantID = tenantIDFromMetadataOrClientRef(
		stringMapField(obj, "metadata"), stringField(obj, "client_reference_id"))
}

// applySubscriptionEvent fills out from a customer.subscription.* event's raw
// object. defaultKind is the caller's best static guess for this event TYPE;
// it is refined to trial_started/past_due/paused when the subscription's own
// status field says so (still just a ledger label — the actual state
// transition always comes from Service.ProcessWebhook's authoritative
// GetSubscription refetch).
func (p *Provider) applySubscriptionEvent(out *billing.Event, obj map[string]interface{}, defaultKind billing.EventKind) {
	out.Handled = true
	out.Kind = defaultKind
	switch stringField(obj, "status") {
	case string(stripesdk.SubscriptionStatusTrialing):
		if defaultKind == billing.EventActivated {
			out.Kind = billing.EventTrialStarted
		}
	case string(stripesdk.SubscriptionStatusPastDue), string(stripesdk.SubscriptionStatusUnpaid):
		out.Kind = billing.EventPastDue
	case string(stripesdk.SubscriptionStatusPaused):
		out.Kind = billing.EventPaused
	case string(stripesdk.SubscriptionStatusCanceled), string(stripesdk.SubscriptionStatusIncompleteExpired):
		out.Kind = billing.EventCanceled
	}
	out.ProviderCustomerID = nestedID(obj, "customer")
	out.ProviderSubscriptionID = stringField(obj, "id")
	out.TenantID = tenantIDFromMetadataOrClientRef(stringMapField(obj, "metadata"), "")
}

// applyInvoiceEvent fills out from an invoice.* event's raw object. Tenant
// attribution reads Parent.SubscriptionDetails.Metadata — Stripe's own
// immutable snapshot of the subscription's metadata taken at invoice
// finalization — so no expanded Subscription fetch is needed just to
// attribute the event.
func (p *Provider) applyInvoiceEvent(out *billing.Event, obj map[string]interface{}, kind billing.EventKind) {
	out.Handled = true
	out.Kind = kind
	out.ProviderCustomerID = nestedID(obj, "customer")

	var subMeta map[string]string
	if parent, ok := obj["parent"].(map[string]interface{}); ok {
		if details, ok := parent["subscription_details"].(map[string]interface{}); ok {
			out.ProviderSubscriptionID = nestedID(details, "subscription")
			subMeta = stringMapField(details, "metadata")
		}
	}
	out.TenantID = tenantIDFromMetadataOrClientRef(subMeta, "")
}

// applyChargeEvent fills out from a charge.refunded event's raw object.
// Charges carry no subscription reference, so ProviderSubscriptionID is left
// empty — Service.ProcessWebhook ledgers the event and does not attempt a
// subscription refetch/state-machine pass when that is empty, matching the
// spec's "charge.refunded → Refunded, no state mutation" behavior.
func (p *Provider) applyChargeEvent(out *billing.Event, obj map[string]interface{}, kind billing.EventKind) {
	out.Handled = true
	out.Kind = kind
	out.ProviderCustomerID = nestedID(obj, "customer")
	out.TenantID = tenantIDFromMetadataOrClientRef(stringMapField(obj, "metadata"), "")
}

// tenantIDFromMetadataOrClientRef parses billing.Event.TenantID from a
// Stripe metadata map's tenantMetadataKey entry, falling back to a raw
// client_reference_id string (checkout sessions only). Returns uuid.Nil when
// neither is present or parseable — the caller then falls back to the
// (provider, provider_customer_id) lookup.
func tenantIDFromMetadataOrClientRef(metadata map[string]string, clientRef string) uuid.UUID {
	if metadata != nil {
		if v, ok := metadata[tenantMetadataKey]; ok && v != "" {
			if parsed, err := uuid.Parse(v); err == nil {
				return parsed
			}
		}
	}
	if clientRef != "" {
		if parsed, err := uuid.Parse(clientRef); err == nil {
			return parsed
		}
	}
	return uuid.Nil
}
