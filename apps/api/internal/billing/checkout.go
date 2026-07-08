package billing

// checkout.go — M16 Phase B tenant-facing billing operations: start a hosted
// checkout, mint a billing-management portal session, and summarize a
// tenant's current billing state for the dashboard's Billing page.

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Actor identifies who is performing a tenant-facing billing action, for
// audit attribution. Kept as a tiny local type (rather than accepting
// domain.Principal directly) so this package's public surface does not widen
// to depend on the exact Principal shape.
type Actor struct {
	Type string // audit.ActorUser or audit.ActorAPIKey
	ID   string
}

// validPurchasableTier reports whether t is one of the three paid tiers a
// checkout may target. Free is never purchasable (it is the no-subscription
// default, not a plan a payment provider bills for).
func validPurchasableTier(t Tier) bool {
	switch t {
	case TierStarter, TierAgency, TierScale:
		return true
	}
	return false
}

// CreateCheckout starts a hosted checkout session for tenantID targeting
// tier. requestedProvider is the caller's preferred provider ("stripe" |
// "razorpay"; empty defers to whatever this tenant is already pinned to, then
// this instance's configured default — see the "one tenant = one provider"
// resolution below). currency is meaningful ONLY to a provider whose
// CreateCheckout resolves a price/plan PER (tier, currency) — e.g. Razorpay's
// dual-currency plan model; Stripe ignores it entirely (its own Price already
// encodes a currency). The caller (the HTTP handler) supplies customerEmail
// (best-effort prefill, may be empty) and the success/cancel redirect URLs —
// the price itself is resolved SERVER-SIDE by the provider adapter's
// tier(+currency)->price map; nothing the caller supplies can select a price
// directly.
func (s *Service) CreateCheckout(ctx context.Context, tenantID uuid.UUID, tier Tier, requestedProvider, currency, customerEmail, successURL, cancelURL string, actor Actor) (CheckoutSession, error) {
	if !validPurchasableTier(tier) {
		return CheckoutSession{}, domain.Validation("billing_invalid_tier", "tier must be one of: starter, agency, scale")
	}
	if !s.enabled {
		return CheckoutSession{}, domain.Unavailable("billing_disabled", "hosted billing is not enabled on this instance")
	}
	if s.registry == nil || !s.registry.Any() {
		return CheckoutSession{}, domain.ServiceUnavailable("billing_not_configured", "no payment provider is configured on this instance yet")
	}

	profile, err := s.getBillingProfile(ctx, tenantID)
	if err != nil {
		return CheckoutSession{}, err
	}

	// "One tenant = one provider at a time (set at first checkout)": an
	// already-pinned provider ALWAYS wins over the caller's request — a
	// returning customer's checkout can never accidentally split their
	// subscription across two providers. requestedProvider is only consulted
	// on a tenant's first-ever checkout; s.defaultProvider (configured at
	// boot, today always "stripe") is the final fallback, so an omitted
	// "provider" field in the request keeps working exactly as before.
	providerName := profile.BillingProvider
	if providerName == "" {
		providerName = requestedProvider
	}
	if providerName == "" {
		providerName = s.defaultProvider
	}
	provider, ok := s.registry.Provider(providerName)
	if !ok {
		return CheckoutSession{}, domain.ServiceUnavailable("billing_provider_unavailable",
			"the configured payment provider for this workspace is not available")
	}

	sess, err := provider.CreateCheckout(ctx, CheckoutInput{
		TenantID:           tenantID,
		Plan:               tier,
		Currency:           currency,
		CustomerEmail:      customerEmail,
		ProviderCustomerID: profile.ProviderCustomerID,
		SuccessURL:         successURL,
		CancelURL:          cancelURL,
	})
	if err != nil {
		return CheckoutSession{}, err
	}

	// "One tenant = one provider at a time (set at first checkout)": this is
	// a no-op once already set, so a returning customer's second-ever
	// checkout never re-points them at a different provider. Logged loudly
	// (not just a warning) on failure: the checkout itself already succeeded
	// with the provider, so we must not fail the response over this, but a
	// silent failure here would degrade CreatePortalSession later.
	if err := s.setBillingProviderIfUnset(ctx, tenantID, providerName); err != nil {
		s.logger.Error("billing: failed to record billing_provider after checkout creation", "tenant_id", tenantID, "provider", providerName, "error", err)
	}

	s.recordAudit(ctx, tenantID, actor.Type, actor.ID, "billing.checkout.started", map[string]any{
		"tier":     string(tier),
		"provider": providerName,
	})

	return sess, nil
}

// VerifyCheckoutCallback authenticates a browser-returned checkout-completion
// callback for tenantID's CURRENT billing provider (e.g. Razorpay's
// Checkout.js onSuccess payload: razorpay_payment_id/razorpay_subscription_id/
// razorpay_signature). This is ONLY a UX confirmation that the client-side
// modal succeeded — it NEVER mutates tenants.plan/plan_status itself; the
// frontend is expected to poll GetBillingSummary afterward and wait for the
// webhook (Service.ProcessWebhook) to actually flip the plan, exactly like
// Stripe's existing redirect-then-poll success flow.
//
// Returns a "billing_callback_not_supported" error for a provider that has no
// such callback to verify (Stripe's redirect-based Checkout Session has no
// equivalent) — see CheckoutCallbackVerifier's doc comment.
func (s *Service) VerifyCheckoutCallback(ctx context.Context, tenantID uuid.UUID, payload map[string]string) error {
	if !s.enabled {
		return domain.Unavailable("billing_disabled", "hosted billing is not enabled on this instance")
	}
	profile, err := s.getBillingProfile(ctx, tenantID)
	if err != nil {
		return err
	}
	if profile.BillingProvider == "" {
		return domain.Conflict("billing_no_customer",
			"start a checkout before verifying a checkout callback — this workspace has no billing provider yet")
	}
	if s.registry == nil {
		return domain.ServiceUnavailable("billing_not_configured", "no payment provider is configured on this instance yet")
	}
	provider, ok := s.registry.Provider(profile.BillingProvider)
	if !ok {
		return domain.ServiceUnavailable("billing_provider_unavailable",
			"the configured payment provider for this workspace is not available")
	}
	verifier, ok := provider.(CheckoutCallbackVerifier)
	if !ok {
		return domain.Unavailable("billing_callback_not_supported", "this payment provider has no browser checkout callback to verify")
	}
	return verifier.VerifyCheckoutCallback(payload)
}

// CreatePortalSession mints a short-lived billing-management portal session
// for tenantID, routed via the tenant's OWN provider (a tenant's
// subscription/portal operations always go through whichever provider it
// started its first checkout with).
func (s *Service) CreatePortalSession(ctx context.Context, tenantID uuid.UUID, actor Actor) (PortalSession, error) {
	if !s.enabled {
		return PortalSession{}, domain.Unavailable("billing_disabled", "hosted billing is not enabled on this instance")
	}

	profile, err := s.getBillingProfile(ctx, tenantID)
	if err != nil {
		return PortalSession{}, err
	}
	if profile.BillingProvider == "" || profile.ProviderCustomerID == "" {
		return PortalSession{}, domain.Conflict("billing_no_customer",
			"start a checkout before managing billing — this workspace has no billing provider customer yet")
	}
	if s.registry == nil {
		return PortalSession{}, domain.ServiceUnavailable("billing_not_configured", "no payment provider is configured on this instance yet")
	}
	provider, ok := s.registry.Provider(profile.BillingProvider)
	if !ok {
		return PortalSession{}, domain.ServiceUnavailable("billing_provider_unavailable",
			"the configured payment provider for this workspace is not available")
	}

	sess, err := provider.CreatePortalSession(ctx, profile.ProviderCustomerID)
	if err != nil {
		return PortalSession{}, err
	}

	s.recordAudit(ctx, tenantID, actor.Type, actor.ID, "billing.portal.opened", map[string]any{
		"provider": profile.BillingProvider,
	})

	return sess, nil
}

// CancelSubscription tells tenantID's CURRENT billing provider to cancel its
// live subscription — the provider-agnostic backend for the dashboard's
// "Cancel subscription" action. Every provider is reachable through this ONE
// method regardless of whether it also has a hosted portal: Razorpay tenants
// (HasPortal()==false) have NO other way to cancel; Stripe tenants may use
// either this or the portal.
//
// Cancellation is scheduled for the END of the current billing period by
// every adapter (see Provider.CancelSubscription's doc comment) — the
// customer keeps paid access through what they already paid for.
//
// This method NEVER mutates tenants.plan/plan_status itself: "push is a
// hint, pull is the truth" applies here exactly as it does to every other
// billing mutation in this package — the provider's own cancellation webhook
// is what actually drives the non-destructive downgrade, through the EXACT
// SAME ProcessWebhook/state-machine path as any other event. Callers must
// not assume the plan has changed the instant this call returns; the
// frontend should poll GetBillingSummary afterward, same as the checkout
// success flow.
//
// Returns a clean "billing_no_subscription" error when the tenant has no
// live provider subscription to cancel (never checked out, or a provider
// name is set with no subscription id on file).
func (s *Service) CancelSubscription(ctx context.Context, tenantID uuid.UUID, actor Actor) error {
	if !s.enabled {
		return domain.Unavailable("billing_disabled", "hosted billing is not enabled on this instance")
	}
	profile, err := s.getBillingProfile(ctx, tenantID)
	if err != nil {
		return err
	}
	if profile.BillingProvider == "" || profile.ProviderSubscriptionID == "" {
		return domain.Conflict("billing_no_subscription", "this workspace has no active subscription to cancel")
	}
	if s.registry == nil {
		return domain.ServiceUnavailable("billing_not_configured", "no payment provider is configured on this instance yet")
	}
	provider, ok := s.registry.Provider(profile.BillingProvider)
	if !ok {
		return domain.ServiceUnavailable("billing_provider_unavailable",
			"the configured payment provider for this workspace is not available")
	}

	if err := provider.CancelSubscription(ctx, profile.ProviderSubscriptionID); err != nil {
		return err
	}

	s.recordAudit(ctx, tenantID, actor.Type, actor.ID, "billing.subscription.cancel_requested", map[string]any{
		"provider": profile.BillingProvider,
	})
	return nil
}

// SiteMeter is a single usage/limit pair for the Billing page's meters block.
type SiteMeter struct {
	Used  int `json:"used"`
	Limit int `json:"limit"`
}

// Meters bundles every metered resource the Billing page shows. Only Sites is
// populated in Phase B; the shape leaves room for a future storage/seats
// meter without another contract change.
type Meters struct {
	Sites SiteMeter `json:"sites"`
}

// Summary is the fully-resolved billing state for GET /api/v1/billing.
type Summary struct {
	Plan             Tier       `json:"plan"`
	PlanStatus       Status     `json:"plan_status"`
	CurrentPeriodEnd *time.Time `json:"current_period_end,omitempty"`
	Provider         string     `json:"provider,omitempty"`
	GraceUntil       *time.Time `json:"grace_until,omitempty"`
	Meters           Meters     `json:"meters"`
	PortalAvailable  bool       `json:"portal_available"`
}

// GetBillingSummary resolves the full Summary the Billing page renders.
func (s *Service) GetBillingSummary(ctx context.Context, tenantID uuid.UUID) (Summary, error) {
	if !s.enabled {
		return Summary{}, domain.Unavailable("billing_disabled", "hosted billing is not enabled on this instance")
	}

	profile, err := s.getBillingProfile(ctx, tenantID)
	if err != nil {
		return Summary{}, err
	}
	ent, err := s.Entitlements(ctx, tenantID)
	if err != nil {
		return Summary{}, err
	}

	var used int64
	txErr := s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		n, cerr := sqlc.New(tx).CountActiveSitesForBilling(ctx, tenantID)
		if cerr != nil {
			return cerr
		}
		used = n
		return nil
	})
	if txErr != nil {
		return Summary{}, domain.Internal("billing_usage_count_failed", "failed to count active sites").WithCause(txErr)
	}

	// PortalAvailable defaults to "has a provider customer at all" and is only
	// suppressed when the tenant's OWN provider is registered and positively
	// reports HasPortal()==false (e.g. Razorpay). An unresolvable provider
	// (registry nil/not-yet-registered) errs toward the prior, simpler
	// behavior rather than hiding a legitimate Stripe portal link over an
	// unrelated lookup gap.
	portalAvailable := profile.ProviderCustomerID != ""
	if portalAvailable && s.registry != nil {
		if provider, ok := s.registry.Provider(profile.BillingProvider); ok && !provider.HasPortal() {
			portalAvailable = false
		}
	}

	return Summary{
		Plan:             profile.Plan,
		PlanStatus:       profile.Status,
		CurrentPeriodEnd: profile.CurrentPeriodEnd,
		Provider:         profile.BillingProvider,
		GraceUntil:       profile.GraceUntil,
		Meters: Meters{
			Sites: SiteMeter{Used: int(used), Limit: ent.MaxSites},
		},
		PortalAvailable: portalAvailable,
	}, nil
}

// actorTypeFor maps a domain.Principal-derived actor type through to the
// audit package's constants — a thin adapter so callers outside this package
// never need to import audit themselves just to build an Actor.
func actorTypeFor(isAPIKey bool) string {
	if isAPIKey {
		return audit.ActorAPIKey
	}
	return audit.ActorUser
}
