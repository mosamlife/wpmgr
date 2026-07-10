// Package razorpay implements billing.Provider for Razorpay — the SECOND
// payment provider (alongside internal/billing/stripe), added for India
// pricing. Nothing outside this package (and cmd/wpmgr/main.go's boot wiring)
// ever imports a Razorpay-specific type — the core internal/billing package,
// its state machine, and its HTTP handlers know only the billing.Provider
// interface (plus the two small optional capability interfaces this adapter
// also implements: billing.CheckoutCallbackVerifier, and HasPortal() as part
// of billing.Provider itself).
//
// Two things make this adapter shaped differently from Stripe's:
//
//  1. DUAL-CURRENCY plans: Razorpay has no single multi-currency price object
//     the way Stripe does, so this control plane maintains ONE Razorpay Plan
//     PER CURRENCY PER TIER (USD for international, INR for India) — see
//     Config's six Plan* fields and (Provider).planID.
//  2. IN-APP CHECKOUT: Razorpay has no hosted Checkout Session/redirect URL.
//     CreateCheckout creates a Razorpay Subscription server-side and returns
//     just enough data (billing.RazorpayCheckoutData) for the frontend's
//     Checkout.js modal to open in-app.
package razorpay

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Currency codes this adapter accepts. Exported so callers (and tests)
// building a CheckoutInput/checkout request never have to spell out the
// literal strings themselves.
const (
	CurrencyUSD = "USD"
	CurrencyINR = "INR"
)

// Config is the Razorpay adapter's construction-time configuration, sourced
// from config.BillingConfig.Razorpay (WPMGR_BILLING_RAZORPAY_*).
type Config struct {
	// KeyID is Razorpay's PUBLIC key id — safe to hand to the browser
	// (Checkout.js's "key" option).
	KeyID string
	// KeySecret is Razorpay's secret API key: used for HTTP Basic Auth on
	// every Subscriptions/Plans REST call, AND to verify the browser checkout
	// callback's signature (VerifyCheckoutCallback) — DISTINCT from
	// WebhookSecret.
	KeySecret string
	// WebhookSecret is the Razorpay webhook signing secret used to verify
	// POST /webhooks/billing/razorpay (X-Razorpay-Signature). DISTINCT from
	// KeySecret.
	WebhookSecret string

	// PlanStarterUSD/PlanStarterINR/PlanAgencyUSD/PlanAgencyINR/
	// PlanScaleUSD/PlanScaleINR are the six Razorpay Plan ids — ONE PER
	// (tier, currency) pair — created once in the Razorpay Dashboard/API.
	// This control plane never creates Plans itself.
	PlanStarterUSD string
	PlanStarterINR string
	PlanAgencyUSD  string
	PlanAgencyINR  string
	PlanScaleUSD   string
	PlanScaleINR   string

	// BaseURL overrides the Razorpay API base URL. Empty uses the real
	// production API (https://api.razorpay.com/v1). Exposed for tests only.
	BaseURL string
	// HTTPClient overrides the default *http.Client (timeout etc). Nil uses a
	// sane default. Exposed for tests — never used to change TLS/security
	// behavior.
	HTTPClient *http.Client
}

// Configured reports whether cfg has everything this adapter needs to
// operate: the three credentials, and all SIX dual-currency plan ids.
// cmd/wpmgr/main.go only registers this provider in the billing.Registry when
// Configured() is true — a partially-set Razorpay config is refused at boot
// by internal/config.Validate, never silently half-wired into a Provider
// that would fail confusingly on first checkout/webhook in whichever
// currency was left unconfigured.
func (c Config) Configured() bool {
	return c.KeyID != "" && c.KeySecret != "" && c.WebhookSecret != "" &&
		c.PlanStarterUSD != "" && c.PlanStarterINR != "" &&
		c.PlanAgencyUSD != "" && c.PlanAgencyINR != "" &&
		c.PlanScaleUSD != "" && c.PlanScaleINR != ""
}

// planKey indexes Provider.planIndex by (tier, currency).
type planKey struct {
	tier     billing.Tier
	currency string
}

// Provider implements billing.Provider (and billing.CheckoutCallbackVerifier)
// for Razorpay.
type Provider struct {
	keyID         string
	keySecret     string
	webhookSecret string
	rest          *restClient

	// planIndex resolves (tier, currency) -> Razorpay plan id (CreateCheckout).
	planIndex map[planKey]string
	// planToPlan resolves a Razorpay plan id -> tier (MapPriceToPlan), built
	// as the reverse of planIndex over all six configured plan ids.
	planToTier map[string]billing.Tier
}

// New builds a Razorpay Provider. Callers should check cfg.Configured()
// first (or rely on the registry-building code in cmd/wpmgr/main.go, which
// already does).
func New(cfg Config) *Provider {
	planIndex := map[planKey]string{
		{billing.TierStarter, CurrencyUSD}: cfg.PlanStarterUSD,
		{billing.TierStarter, CurrencyINR}: cfg.PlanStarterINR,
		{billing.TierAgency, CurrencyUSD}:  cfg.PlanAgencyUSD,
		{billing.TierAgency, CurrencyINR}:  cfg.PlanAgencyINR,
		{billing.TierScale, CurrencyUSD}:   cfg.PlanScaleUSD,
		{billing.TierScale, CurrencyINR}:   cfg.PlanScaleINR,
	}
	planToTier := make(map[string]billing.Tier, len(planIndex))
	for k, planID := range planIndex {
		if planID != "" {
			planToTier[planID] = k.tier
		}
	}
	return &Provider{
		keyID:         cfg.KeyID,
		keySecret:     cfg.KeySecret,
		webhookSecret: cfg.WebhookSecret,
		rest:          newRestClient(cfg),
		planIndex:     planIndex,
		planToTier:    planToTier,
	}
}

// Name implements billing.Provider.
func (p *Provider) Name() string { return "razorpay" }

// HasPortal implements billing.Provider: Razorpay has no hosted
// billing-management portal — see CreatePortalSession.
func (p *Provider) HasPortal() bool { return false }

// MapPriceToPlan implements billing.Provider: resolves a Razorpay plan id to
// a tier over ALL SIX configured (tier, currency) plan ids — a USD and an
// INR plan for the same tier both resolve to that same Tier.
func (p *Provider) MapPriceToPlan(planID string) (billing.Tier, bool) {
	t, ok := p.planToTier[planID]
	return t, ok
}

// planID resolves (tier, currency) to a configured Razorpay plan id.
// ok=false when no plan is configured for that exact pair (e.g. a currency
// this instance never wired for that tier).
func (p *Provider) planID(tier billing.Tier, currency string) (string, bool) {
	id, ok := p.planIndex[planKey{tier, currency}]
	return id, ok && id != ""
}

// CreateCheckout implements billing.Provider. Razorpay has NO Checkout
// Session object — this creates a Razorpay SUBSCRIPTION server-side and
// returns the data the frontend's IN-APP Checkout.js modal needs to open it
// (billing.RazorpayCheckoutData), rather than a redirect URL.
//
// in.ProviderCustomerID/in.CustomerEmail are NOT used by this adapter:
// unlike Stripe's Checkout Session (which takes an existing Customer id or an
// email to prefill), Razorpay's Subscription Create API resolves/creates the
// customer during the Checkout.js authorization flow itself, not as a create
// parameter. This is a deliberate no-op, not an oversight.
func (p *Provider) CreateCheckout(ctx context.Context, in billing.CheckoutInput) (billing.CheckoutSession, error) {
	currency := strings.ToUpper(strings.TrimSpace(in.Currency))
	if currency != CurrencyUSD && currency != CurrencyINR {
		return billing.CheckoutSession{}, domain.Validation("billing_invalid_currency",
			"currency must be USD or INR for a Razorpay checkout")
	}

	planID, ok := p.planID(in.Plan, currency)
	if !ok {
		return billing.CheckoutSession{}, domain.Validation("billing_unknown_tier",
			fmt.Sprintf("no Razorpay plan is configured for tier %q in currency %q", in.Plan, currency))
	}

	// Read the plan's authoritative amount/currency for the Checkout.js
	// modal — never computed/guessed CP-side, so it can never drift from
	// what Razorpay will actually charge.
	plan, err := p.fetchPlan(ctx, planID)
	if err != nil {
		return billing.CheckoutSession{}, err
	}

	tenantID := in.TenantID.String()
	sub, err := p.createSubscription(ctx, subscriptionCreateRequest{
		PlanID:         planID,
		TotalCount:     subscriptionTotalCycles,
		CustomerNotify: 0,
		Notes:          map[string]string{tenantNotesKey: tenantID},
	})
	if err != nil {
		return billing.CheckoutSession{}, err
	}

	return billing.CheckoutSession{
		Razorpay: &billing.RazorpayCheckoutData{
			SubscriptionID: sub.ID,
			KeyID:          p.keyID,
			Currency:       plan.Item.Currency,
			AmountMinor:    plan.Item.Amount,
		},
	}, nil
}

// GetPlanAmount implements billing.RazorpayPlanReader: reads the (tier,
// currency) Plan's authoritative amount via the SAME side-effect-free
// GET /plans/{id} call CreateCheckout uses to read the Checkout.js "amount" —
// no subscription is created. Backs the public GET /api/v1/pricing endpoint
// (internal/pricing). Every Razorpay Plan this adapter creates bills
// MONTHLY (see subscriptionTotalCycles's doc comment), so interval is always
// "month".
func (p *Provider) GetPlanAmount(ctx context.Context, tier billing.Tier, currency string) (amountMinor int64, resolvedCurrency string, interval string, err error) {
	planID, ok := p.planID(tier, currency)
	if !ok {
		return 0, "", "", domain.NotFound("razorpay_plan_not_configured",
			fmt.Sprintf("no Razorpay plan is configured for tier %q in currency %q", tier, currency))
	}
	plan, err := p.fetchPlan(ctx, planID)
	if err != nil {
		return 0, "", "", err
	}
	return plan.Item.Amount, plan.Item.Currency, "month", nil
}

// CreatePortalSession implements billing.Provider: Razorpay has no hosted
// billing-management portal (unlike Stripe's Billing Portal). Returns a
// clear domain KindUnavailable (HTTP 501) error rather than fabricating a
// URL — see billing.Provider.HasPortal, which lets a caller avoid this call
// entirely and show a cancel action instead of a portal link.
func (p *Provider) CreatePortalSession(ctx context.Context, providerCustomerID string) (billing.PortalSession, error) {
	return billing.PortalSession{}, domain.Unavailable("billing_portal_not_supported",
		"Razorpay has no hosted billing-management portal — manage or cancel the subscription from the WPMgr dashboard instead")
}

// CancelSubscription implements billing.Provider: schedules cancellation for
// the END of the current billing cycle (cancel_at_cycle_end=1) rather than
// immediately — the customer keeps access through what they already paid
// for, matching the state machine's non-destructive-downgrade intent (and
// mirroring the Stripe adapter's cancel_at_period_end=true choice exactly).
// Since Razorpay has no hosted portal (HasPortal()==false), THIS is the
// tenant's only way to cancel — see billing.Service.CancelSubscription,
// which is the backend for the dashboard's "Cancel subscription" action.
//
// The tenant's stored plan/status is NEVER mutated here: the resulting
// subscription.cancelled (or, for a scheduled cancel-at-cycle-end,
// subscription.completed at the cycle boundary) webhook drives the
// downgrade, through the exact same ProcessWebhook path as any other event.
func (p *Provider) CancelSubscription(ctx context.Context, providerSubscriptionID string) error {
	return p.cancelSubscription(ctx, providerSubscriptionID)
}

// GetSubscription implements billing.Provider — the sole source of truth the
// state machine acts on ("pull is the truth").
func (p *Provider) GetSubscription(ctx context.Context, providerSubscriptionID string) (billing.Subscription, error) {
	sub, err := p.fetchSubscription(ctx, providerSubscriptionID)
	if err != nil {
		return billing.Subscription{}, err
	}
	return p.toSubscription(sub), nil
}

// toSubscription normalizes a Razorpay subscriptionEntity to billing.Subscription.
func (p *Provider) toSubscription(sub subscriptionEntity) billing.Subscription {
	out := billing.Subscription{
		ID:         sub.ID,
		CustomerID: sub.CustomerID,
		Status:     mapStatus(sub.Status),
	}
	if sub.CurrentEnd > 0 {
		out.CurrentPeriodEnd = time.Unix(sub.CurrentEnd, 0).UTC()
	}
	if tier, ok := p.planToTier[sub.PlanID]; ok {
		out.Plan = tier
		out.PlanResolved = true
	}
	return out
}

// mapStatus normalizes Razorpay's subscription status vocabulary
// (https://razorpay.com/docs/api/payments/subscriptions/subscription-entity/#subscription-status)
// to the project's provider-agnostic billing.Status.
//
//   - active maps directly.
//   - pending (a payment attempt is retrying) maps to past_due — grace, do
//     NOT revoke (mirrors Stripe's past_due).
//   - halted (Razorpay's retry schedule is exhausted — mirrors Stripe's
//     "unpaid") ALSO maps to past_due: the existing grace-window logic in
//     state_machine.go (anchored off the FIRST past_due transition) decides
//     if/when this eventually downgrades to free, exactly as it already
//     does for Stripe's "unpaid".
//   - cancelled AND completed (a fixed total_count subscription ran its full
//     course — should not occur in practice given subscriptionTotalCycles,
//     but handled defensively) both map to canceled (non-destructive
//     downgrade to free — see state_machine.go).
//   - paused maps directly.
//   - created and authenticated (the mandate is set up/authorized but not
//     yet a live, charging subscription) and anything unrecognized map to
//     StatusNone — mirrors Stripe's "incomplete" treatment.
func mapStatus(s string) billing.Status {
	switch s {
	case "active":
		return billing.StatusActive
	case "pending":
		return billing.StatusPastDue
	case "halted":
		return billing.StatusPastDue
	case "cancelled", "completed":
		return billing.StatusCanceled
	case "paused":
		return billing.StatusPaused
	default: // created, authenticated, or any future/unrecognized value.
		return billing.StatusNone
	}
}

// VerifyCheckoutCallback implements billing.CheckoutCallbackVerifier: checks
// the signature Razorpay's Checkout.js onSuccess handler hands the browser —
// HMAC-SHA256 of "razorpay_payment_id|razorpay_subscription_id", keyed by the
// API KEY SECRET (NOT the webhook secret), hex-compared to
// payload["razorpay_signature"].
// https://razorpay.com/docs/payments/subscriptions/verify-signature/
//
// This is ONLY a UX confirmation that the client-side modal succeeded — see
// the interface doc comment. It never mutates any billing state itself.
func (p *Provider) VerifyCheckoutCallback(payload map[string]string) error {
	paymentID := payload["razorpay_payment_id"]
	subscriptionID := payload["razorpay_subscription_id"]
	signature := payload["razorpay_signature"]
	if paymentID == "" || subscriptionID == "" || signature == "" {
		return domain.Validation("billing_callback_invalid",
			"missing razorpay_payment_id, razorpay_subscription_id, or razorpay_signature")
	}
	message := paymentID + "|" + subscriptionID
	if !verifyHMACSHA256Hex([]byte(message), signature, p.keySecret) {
		return domain.Unauthorized("billing_callback_signature_invalid",
			"razorpay checkout callback signature verification failed")
	}
	return nil
}
