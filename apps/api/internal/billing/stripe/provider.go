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
	"strings"
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
	// PortalConfigurationID is the id (bpc_...) of the Customer Portal
	// configuration WPMgr's portal sessions use. Required: without it Stripe
	// would fall back to the account's default configuration, which on a
	// shared account belongs to no single product.
	PortalConfigurationID string
	// HTTPClient overrides the Stripe SDK's default HTTP client. Nil uses the
	// SDK default. Exposed for tests (never used to change TLS/security
	// behavior — only to point at a test double).
	HTTPClient *http.Client
}

// Configured reports whether cfg has everything this adapter needs to
// operate: the secret key, the webhook signing secret, all three price ids
// and the portal configuration id. cmd/wpmgr/main.go only registers this provider in the billing.Registry
// when Configured() is true — a partially-set Stripe config is refused at
// boot by internal/config.Validate, never silently half-wired into a
// Provider that would panic or misbehave on first use.
func (c Config) Configured() bool {
	return c.SecretKey != "" && c.WebhookSecret != "" &&
		c.PriceStarter != "" && c.PriceAgency != "" && c.PriceScale != "" &&
		c.PortalConfigurationID != ""
}

// IsTestModeKey reports whether secretKey is a test-mode key: a secret key
// (sk_test_) or a restricted key (rk_test_).
func IsTestModeKey(secretKey string) bool {
	return strings.HasPrefix(secretKey, "sk_test_") || strings.HasPrefix(secretKey, "rk_test_")
}

// Provider implements billing.Provider for Stripe.
type Provider struct {
	client          *stripesdk.Client
	webhookSecret   string
	portalReturnURL string
	portalConfigID  string
	priceToPlan     map[string]billing.Tier
	planToPrice     map[billing.Tier]string
	// now is the clock checkout expiry is computed from. Tests replace it.
	now func() time.Time
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
		portalConfigID:  cfg.PortalConfigurationID,
		now:             time.Now,
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

// checkoutSessionLifetime is how long a Checkout Session stays payable.
const checkoutSessionLifetime = time.Hour

// CreateCheckout implements billing.Provider. The price id is resolved
// SERVER-SIDE from in.Plan via planToPrice — the caller (ultimately, the
// HTTP request body) never supplies a price id. The session is always
// created on the stored customer in.ProviderCustomerID; an empty customer is
// refused before any request is sent.
func (p *Provider) CreateCheckout(ctx context.Context, in billing.CheckoutInput) (billing.CheckoutSession, error) {
	price, ok := p.planToPrice[in.Plan]
	if !ok || price == "" {
		return billing.CheckoutSession{}, domain.Validation("billing_unknown_tier", "no Stripe price is configured for this tier")
	}
	if in.ProviderCustomerID == "" {
		return billing.CheckoutSession{}, domain.Internal("billing_customer_required", "a Stripe checkout requires a stored customer")
	}
	sess, err := p.client.V1CheckoutSessions.Create(ctx, p.checkoutSessionParams(in, price))
	if err != nil {
		return billing.CheckoutSession{}, wrapErr("stripe_checkout_create_failed", "failed to create Stripe checkout session", err)
	}
	return billing.CheckoutSession{URL: sess.URL, SessionID: sess.ID}, nil
}

// checkoutSessionParams builds the Checkout Session create parameters. It is
// a pure function of its inputs and the adapter clock, so every parameter is
// unit-testable without a Stripe call:
//
//   - the session is created on the stored customer, never by email;
//   - card is the only payment method type;
//   - tax is calculated automatically, a billing address is required, the
//     customer's address and name are saved back, and a tax ID is collected
//     and required where Stripe supports requiring one;
//   - the session and the resulting subscription carry the tenant id and
//     app=wpmgr, which is how events are recognised as WPMgr's on an account
//     shared with other products;
//   - the session expires after checkoutSessionLifetime.
func (p *Provider) checkoutSessionParams(in billing.CheckoutInput, price string) *stripesdk.CheckoutSessionCreateParams {
	tenantID := in.TenantID.String()
	meta := func() map[string]string {
		return map[string]string{tenantMetadataKey: tenantID, appMetadataKey: appMetadataValue}
	}
	return &stripesdk.CheckoutSessionCreateParams{
		Mode:               stripesdk.String(string(stripesdk.CheckoutSessionModeSubscription)),
		SuccessURL:         stripesdk.String(in.SuccessURL),
		CancelURL:          stripesdk.String(in.CancelURL),
		ClientReferenceID:  stripesdk.String(tenantID),
		Customer:           stripesdk.String(in.ProviderCustomerID),
		Metadata:           meta(),
		PaymentMethodTypes: []*string{stripesdk.String("card")},
		AutomaticTax:       &stripesdk.CheckoutSessionCreateAutomaticTaxParams{Enabled: stripesdk.Bool(true)},
		BillingAddressCollection: stripesdk.String(
			string(stripesdk.CheckoutSessionBillingAddressCollectionRequired)),
		CustomerUpdate: &stripesdk.CheckoutSessionCreateCustomerUpdateParams{
			Address: stripesdk.String("auto"),
			Name:    stripesdk.String("auto"),
		},
		TaxIDCollection: &stripesdk.CheckoutSessionCreateTaxIDCollectionParams{
			Enabled:  stripesdk.Bool(true),
			Required: stripesdk.String(string(stripesdk.CheckoutSessionTaxIDCollectionRequiredIfSupported)),
		},
		ExpiresAt: stripesdk.Int64(p.now().Add(checkoutSessionLifetime).Unix()),
		LineItems: []*stripesdk.CheckoutSessionCreateLineItemParams{
			{Price: stripesdk.String(price), Quantity: stripesdk.Int64(1)},
		},
		// The subscription carries the same metadata as the session, so
		// every later customer.subscription.* event attributes itself.
		SubscriptionData: &stripesdk.CheckoutSessionCreateSubscriptionDataParams{
			Metadata: meta(),
		},
	}
}

// CreateCustomer implements billing.CustomerCreator: it creates a Stripe
// Customer for tenantID carrying the tenant id and app=wpmgr, with the SDK's
// own random idempotency key. It never looks up or reuses a customer by
// email.
func (p *Provider) CreateCustomer(ctx context.Context, tenantID uuid.UUID, email string) (string, error) {
	params := &stripesdk.CustomerCreateParams{
		Metadata: map[string]string{
			tenantMetadataKey: tenantID.String(),
			appMetadataKey:    appMetadataValue,
		},
	}
	if email != "" {
		params.Email = stripesdk.String(email)
	}
	c, err := p.client.V1Customers.Create(ctx, params)
	if err != nil {
		return "", wrapErr("stripe_customer_create_failed", "failed to create Stripe customer", err)
	}
	return c.ID, nil
}

// GetPrice implements billing.StripePriceReader: reads tier's live price via
// a side-effect-free GET /v1/prices/:id — no subscription is created. Backs
// the public GET /api/v1/pricing endpoint (internal/pricing).
func (p *Provider) GetPrice(ctx context.Context, tier billing.Tier) (amountMinor int64, currency string, interval string, err error) {
	price, ok := p.planToPrice[tier]
	if !ok || price == "" {
		return 0, "", "", domain.Validation("billing_unknown_tier", "no Stripe price is configured for this tier")
	}
	pr, ferr := p.client.V1Prices.Retrieve(ctx, price, nil)
	if ferr != nil {
		return 0, "", "", wrapErr("stripe_price_fetch_failed", "failed to fetch the Stripe price", ferr)
	}
	interval = "month"
	if pr.Recurring != nil && pr.Recurring.Interval != "" {
		interval = string(pr.Recurring.Interval)
	}
	return pr.UnitAmount, string(pr.Currency), interval, nil
}

// CreatePortalSession implements billing.Provider.
func (p *Provider) CreatePortalSession(ctx context.Context, providerCustomerID string) (billing.PortalSession, error) {
	params := p.portalSessionParams(providerCustomerID)
	if p.portalReturnURL != "" {
		params.ReturnURL = stripesdk.String(p.portalReturnURL)
	}
	sess, err := p.client.V1BillingPortalSessions.Create(ctx, params)
	if err != nil {
		return billing.PortalSession{}, wrapErr("stripe_portal_create_failed", "failed to create Stripe billing portal session", err)
	}
	return billing.PortalSession{URL: sess.URL}, nil
}

// portalSessionParams builds the portal session parameters. The portal
// configuration is always WPMgr's own, never the account default.
func (p *Provider) portalSessionParams(providerCustomerID string) *stripesdk.BillingPortalSessionCreateParams {
	return &stripesdk.BillingPortalSessionCreateParams{
		Customer:      stripesdk.String(providerCustomerID),
		Configuration: stripesdk.String(p.portalConfigID),
	}
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

// CancelSubscriptionNow implements billing.ImmediateCanceller: it ends the
// subscription at once (DELETE /v1/subscriptions/{id}), with no proration
// credit and no final invoice. The caller decides when this is allowed; the
// tenant's own state still changes only through the apply worker.
func (p *Provider) CancelSubscriptionNow(ctx context.Context, providerSubscriptionID string) error {
	if providerSubscriptionID == "" {
		return domain.Validation("stripe_subscription_id_required", "a subscription id is required")
	}
	if _, err := p.client.V1Subscriptions.Cancel(ctx, providerSubscriptionID, cancelNowParams()); err != nil {
		return wrapErr("stripe_subscription_cancel_now_failed", "failed to cancel the Stripe subscription", err)
	}
	return nil
}

// cancelNowParams builds the SubscriptionCancelParams CancelSubscriptionNow
// sends: no proration credit and no final invoice.
func cancelNowParams() *stripesdk.SubscriptionCancelParams {
	return &stripesdk.SubscriptionCancelParams{
		InvoiceNow: stripesdk.Bool(false),
		Prorate:    stripesdk.Bool(false),
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
		ID:     sub.ID,
		Status: mapStatus(sub.Status),
		// A cancel scheduled for a set date (cancel_at) is a scheduled cancel
		// just as a period-end one is, whichever way it was requested.
		CancelAtPeriodEnd:      sub.CancelAtPeriodEnd || sub.CancelAt > 0,
		CancelScheduleReported: true,
	}
	if sub.CancelAt > 0 {
		out.CancelAt = time.Unix(sub.CancelAt, 0).UTC()
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

	obj := ev.Data.Object
	out.Ownership = p.classify(string(ev.Type), obj)
	if out.Ownership == billing.OwnershipForeign {
		// Another product's event on a shared account: nothing below reads
		// it, and intake acknowledges it without recording anything.
		return out, nil
	}

	switch ev.Type {
	case stripesdk.EventTypeCheckoutSessionCompleted:
		p.applyCheckoutSession(&out, obj)
	case stripesdk.EventTypeCustomerSubscriptionCreated:
		p.applySubscriptionEvent(&out, obj, billing.EventActivated)
	case stripesdk.EventTypeCustomerSubscriptionUpdated:
		p.applySubscriptionEvent(&out, obj, billing.EventUpdated)
	case stripesdk.EventTypeCustomerSubscriptionDeleted:
		p.applySubscriptionEvent(&out, obj, billing.EventCanceled)
	case stripesdk.EventTypeInvoicePaymentFailed:
		p.applyInvoiceEvent(&out, obj, billing.EventPaymentFailed)
	case stripesdk.EventTypeInvoicePaid:
		p.applyInvoiceEvent(&out, obj, billing.EventPaymentSucceeded)
	case stripesdk.EventTypeCustomerTaxIDUpdated:
		p.applyTaxIDEvent(&out, obj)
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
	out.TenantID = tenantIDFromMetadata(stringMapField(obj, "metadata"))
	out.TaxIDTypes, out.BillingCountry = sessionTaxDetails(obj)
}

// sessionTaxDetails reads a completed checkout session's customer_details:
// the TYPE of every tax ID the buyer gave (never its value) and the billing
// address country. Both are empty when the session carries none. The types
// slice is never nil, so the ledger payload always holds an array.
func sessionTaxDetails(obj map[string]interface{}) (types []string, country string) {
	types = []string{}
	details, ok := obj["customer_details"].(map[string]interface{})
	if !ok {
		return types, ""
	}
	if ids, ok := details["tax_ids"].([]interface{}); ok {
		for _, raw := range ids {
			if id, ok := raw.(map[string]interface{}); ok {
				if t := stringField(id, "type"); t != "" {
					types = append(types, t)
				}
			}
		}
	}
	if addr, ok := details["address"].(map[string]interface{}); ok {
		country = stringField(addr, "country")
	}
	return types, country
}

// applyTaxIDEvent fills out from a customer.tax_id.updated event's raw
// object: the owning customer, the ID's type and its verification status.
// The ID's value is never read.
func (p *Provider) applyTaxIDEvent(out *billing.Event, obj map[string]interface{}) {
	out.Handled = true
	out.Kind = billing.EventTaxIDUpdated
	out.ProviderCustomerID = nestedID(obj, "customer")
	out.TaxIDType = stringField(obj, "type")
	if v, ok := obj["verification"].(map[string]interface{}); ok {
		out.TaxIDVerificationStatus = stringField(v, "status")
	}
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
	out.TenantID = tenantIDFromMetadata(stringMapField(obj, "metadata"))
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
	if details := invoiceSubscriptionDetails(obj); details != nil {
		out.ProviderSubscriptionID = nestedID(details, "subscription")
		subMeta = stringMapField(details, "metadata")
	}
	out.TenantID = tenantIDFromMetadata(subMeta)
}

// invoiceSubscriptionDetails returns an invoice object's
// parent.subscription_details, or nil when the invoice has none.
func invoiceSubscriptionDetails(obj map[string]interface{}) map[string]interface{} {
	parent, ok := obj["parent"].(map[string]interface{})
	if !ok {
		return nil
	}
	details, _ := parent["subscription_details"].(map[string]interface{})
	return details
}

// appMetadataKey and appMetadataValue mark an object WPMgr created, for an
// object that carries no tenant id.
const (
	appMetadataKey   = "app"
	appMetadataValue = "wpmgr"
)

// tenantIDFromMetadata parses billing.Event.TenantID from a Stripe metadata
// map's tenantMetadataKey entry. Returns uuid.Nil when it is absent or not a
// UUID; the apply worker then resolves the tenant from the customer.
func tenantIDFromMetadata(metadata map[string]string) uuid.UUID {
	if v, ok := metadata[tenantMetadataKey]; ok && v != "" {
		if parsed, err := uuid.Parse(v); err == nil {
			return parsed
		}
	}
	return uuid.Nil
}

// metadataIsOurs reports whether a Stripe metadata map marks its object as
// WPMgr's: a tenant id, or app=wpmgr.
func metadataIsOurs(metadata map[string]string) bool {
	return metadata[tenantMetadataKey] != "" || metadata[appMetadataKey] == appMetadataValue
}

// classify decides whether a verified event belongs to WPMgr. The Stripe
// account may be shared with other products, so only these count as owned:
//
//   - a checkout.session.* event whose session metadata marks it as WPMgr's;
//   - a customer.subscription.* event whose subscription metadata marks it,
//     or one of whose items uses a price this adapter maps to a tier;
//   - an invoice.* event whose parent subscription_details metadata marks it.
//
// customer.tax_id.updated carries only a customer, so it is
// OwnershipByCustomer and intake decides it against the stored customers.
// Every other event is foreign. client_reference_id is never consulted.
func (p *Provider) classify(evType string, obj map[string]interface{}) billing.Ownership {
	switch {
	case evType == string(stripesdk.EventTypeCustomerTaxIDUpdated):
		return billing.OwnershipByCustomer
	case strings.HasPrefix(evType, "checkout.session."):
		if metadataIsOurs(stringMapField(obj, "metadata")) {
			return billing.OwnershipOwned
		}
	case strings.HasPrefix(evType, "customer.subscription."):
		if metadataIsOurs(stringMapField(obj, "metadata")) || p.subscriptionUsesOurPrice(obj) {
			return billing.OwnershipOwned
		}
	case strings.HasPrefix(evType, "invoice."):
		if details := invoiceSubscriptionDetails(obj); details != nil &&
			metadataIsOurs(stringMapField(details, "metadata")) {
			return billing.OwnershipOwned
		}
	}
	return billing.OwnershipForeign
}

// subscriptionUsesOurPrice reports whether any item of a raw subscription
// object uses a price in priceToPlan.
func (p *Provider) subscriptionUsesOurPrice(obj map[string]interface{}) bool {
	items, ok := obj["items"].(map[string]interface{})
	if !ok {
		return false
	}
	data, ok := items["data"].([]interface{})
	if !ok {
		return false
	}
	for _, raw := range data {
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if id := nestedID(item, "price"); id != "" {
			if _, ok := p.priceToPlan[id]; ok {
				return true
			}
		}
	}
	return false
}

// errListNeedsCustomer is returned by every list helper called with an empty
// customer. A list without a customer filter would read the whole account.
var errListNeedsCustomer = errors.New("stripe: a list call requires a non-empty customer")

// listOpenSessions returns every open Checkout Session of customerID,
// following every page. The request always carries the customer filter, an
// empty customerID is refused before any request is sent, and a returned
// session whose own customer differs from customerID is dropped.
func (p *Provider) listOpenSessions(ctx context.Context, customerID string) ([]*stripesdk.CheckoutSession, error) {
	if customerID == "" {
		return nil, errListNeedsCustomer
	}
	params := &stripesdk.CheckoutSessionListParams{
		Customer: stripesdk.String(customerID),
		Status:   stripesdk.String(string(stripesdk.CheckoutSessionStatusOpen)),
	}
	var out []*stripesdk.CheckoutSession
	for sess, err := range p.client.V1CheckoutSessions.List(ctx, params).All(ctx) {
		if err != nil {
			return nil, wrapErr("billing_session_list_failed", "failed to list checkout sessions", err)
		}
		if sess == nil || sess.Customer == nil || sess.Customer.ID != customerID {
			continue
		}
		if sess.Status != stripesdk.CheckoutSessionStatusOpen {
			continue
		}
		out = append(out, sess)
	}
	return out, nil
}

// listSubscriptions returns every subscription of customerID in any status,
// following every page, under the same rules as listOpenSessions.
func (p *Provider) listSubscriptions(ctx context.Context, customerID string) ([]*stripesdk.Subscription, error) {
	if customerID == "" {
		return nil, errListNeedsCustomer
	}
	params := &stripesdk.SubscriptionListParams{
		Customer: stripesdk.String(customerID),
		Status:   stripesdk.String("all"),
	}
	var out []*stripesdk.Subscription
	for sub, err := range p.client.V1Subscriptions.List(ctx, params).All(ctx) {
		if err != nil {
			return nil, wrapErr("billing_subscription_list_failed", "failed to list subscriptions", err)
		}
		if sub == nil || sub.Customer == nil || sub.Customer.ID != customerID {
			continue
		}
		out = append(out, sub)
	}
	return out, nil
}

// ExpireOpenCheckoutSessions implements billing.CheckoutSessionExpirer. It
// lists customerID's open Checkout Sessions, following every page, and
// expires each one whose own customer equals customerID. An empty customerID
// is refused before any request is sent.
func (p *Provider) ExpireOpenCheckoutSessions(ctx context.Context, customerID string) (int, error) {
	sessions, err := p.listOpenSessions(ctx, customerID)
	if err != nil {
		return 0, err
	}
	expired := 0
	for _, sess := range sessions {
		if _, err := p.client.V1CheckoutSessions.Expire(ctx, sess.ID, nil); err != nil {
			return expired, wrapErr("billing_session_expire_failed", "failed to expire checkout session", err)
		}
		expired++
	}
	return expired, nil
}

// subscriptionPendingOrLive reports whether a Stripe subscription status
// still holds, or may still become, a paid subscription.
func subscriptionPendingOrLive(st stripesdk.SubscriptionStatus) bool {
	switch st {
	case stripesdk.SubscriptionStatusActive, stripesdk.SubscriptionStatusTrialing,
		stripesdk.SubscriptionStatusPastDue, stripesdk.SubscriptionStatusUnpaid,
		stripesdk.SubscriptionStatusPaused, stripesdk.SubscriptionStatusIncomplete:
		return true
	}
	return false
}

// PendingOrLiveSubscription implements billing.SubscriptionLister: the id of
// one of customerID's subscriptions that is pending or live, if any.
func (p *Provider) PendingOrLiveSubscription(ctx context.Context, customerID string) (string, bool, error) {
	subs, err := p.listSubscriptions(ctx, customerID)
	if err != nil {
		return "", false, err
	}
	for _, sub := range subs {
		if subscriptionPendingOrLive(sub.Status) {
			return sub.ID, true, nil
		}
	}
	return "", false, nil
}

// HasPendingOrLive implements billing.SubscriptionLister. An empty
// customerID has nothing findable, so it answers "nothing pending" with no
// request at all. Otherwise it expires customerID's open Checkout Sessions,
// then reports whether any of its subscriptions is pending or live.
func (p *Provider) HasPendingOrLive(ctx context.Context, customerID string) (string, bool, error) {
	if customerID == "" {
		return "", false, nil
	}
	if _, err := p.ExpireOpenCheckoutSessions(ctx, customerID); err != nil {
		return "", false, err
	}
	return p.PendingOrLiveSubscription(ctx, customerID)
}

// SupersedeOpenCheckoutSessions implements billing.CheckoutSessionSuperseder.
// It lists customerID's open sessions (every page) and expires each one
// except the greatest by (created, id). ownSuperseded is true when
// ownSessionID was listed and is not the greatest. Every expiry is attempted;
// the first failure is returned.
func (p *Provider) SupersedeOpenCheckoutSessions(ctx context.Context, customerID, ownSessionID string) (ownSuperseded bool, err error) {
	sessions, err := p.listOpenSessions(ctx, customerID)
	if err != nil {
		return false, err
	}
	if len(sessions) == 0 {
		return false, nil
	}
	greatest := sessions[0]
	for _, s := range sessions[1:] {
		if s.Created > greatest.Created || (s.Created == greatest.Created && s.ID > greatest.ID) {
			greatest = s
		}
	}
	var firstErr error
	for _, s := range sessions {
		if s.ID == greatest.ID {
			continue
		}
		if s.ID == ownSessionID {
			ownSuperseded = true
		}
		if _, xerr := p.client.V1CheckoutSessions.Expire(ctx, s.ID, nil); xerr != nil && firstErr == nil {
			firstErr = wrapErr("billing_session_expire_failed", "failed to expire checkout session", xerr)
		}
	}
	return ownSuperseded, firstErr
}

// ExpireCheckoutSession implements billing.CheckoutSessionSuperseder: it
// expires one session by id.
func (p *Provider) ExpireCheckoutSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return errors.New("stripe: expire requires a session id")
	}
	if _, err := p.client.V1CheckoutSessions.Expire(ctx, sessionID, nil); err != nil {
		return wrapErr("billing_session_expire_failed", "failed to expire checkout session", err)
	}
	return nil
}

// RetrieveCheckoutSession implements billing.CheckoutSessionConfirmer.
func (p *Provider) RetrieveCheckoutSession(ctx context.Context, sessionID string) (billing.CheckoutSessionInfo, error) {
	sess, err := p.client.V1CheckoutSessions.Retrieve(ctx, sessionID, nil)
	if err != nil {
		return billing.CheckoutSessionInfo{}, wrapErr("stripe_checkout_fetch_failed", "failed to fetch the Stripe checkout session", err)
	}
	out := billing.CheckoutSessionInfo{
		ID:                sess.ID,
		ClientReferenceID: sess.ClientReferenceID,
		Mode:              string(sess.Mode),
		Status:            string(sess.Status),
	}
	if sess.Customer != nil {
		out.CustomerID = sess.Customer.ID
	}
	if sess.Subscription != nil {
		out.SubscriptionID = sess.Subscription.ID
	}
	return out, nil
}
