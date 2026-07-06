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
// tier. The caller (the HTTP handler) supplies customerEmail (best-effort
// prefill, may be empty) and the success/cancel redirect URLs — the price
// itself is resolved SERVER-SIDE by the provider adapter's tier->price map;
// nothing the caller supplies can select a price directly.
func (s *Service) CreateCheckout(ctx context.Context, tenantID uuid.UUID, tier Tier, customerEmail, successURL, cancelURL string, actor Actor) (CheckoutSession, error) {
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

	providerName := profile.BillingProvider
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

	return Summary{
		Plan:             profile.Plan,
		PlanStatus:       profile.Status,
		CurrentPeriodEnd: profile.CurrentPeriodEnd,
		Provider:         profile.BillingProvider,
		GraceUntil:       profile.GraceUntil,
		Meters: Meters{
			Sites: SiteMeter{Used: int(used), Limit: ent.MaxSites},
		},
		PortalAvailable: profile.ProviderCustomerID != "",
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
