package billing

// checkout.go — M16 Phase B tenant-facing billing operations: start a hosted
// checkout, mint a billing-management portal session, and summarize a
// tenant's current billing state for the dashboard's Billing page.

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
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

// billing_provider_locked reasons, carried in the error's details.reason.
// The web picks its copy from the reason, never from the message.
const (
	ReasonNeedsSupport      = "needs_support"
	ReasonPendingAtProvider = "pending_at_provider"
	ReasonProviderChanged   = "provider_changed"
)

// providerLocked is the 409 billing_provider_locked with its reason.
func providerLocked(reason, msg string) error {
	return domain.Conflict("billing_provider_locked", msg).WithDetails(map[string]any{"reason": reason})
}

// CheckoutTestHooks are seams the integration tests use to change a tenant's
// row at a fixed point of CreateCheckout, to prove how concurrent requests
// resolve. Production never sets them.
type CheckoutTestHooks struct {
	// BeforeBind runs after every check that precedes the locked bind.
	BeforeBind func()
	// AfterCreate runs after the provider call, before the re-read.
	AfterCreate func()
	// AfterReread runs after the re-read, before the supersede.
	AfterReread func()
}

// SetCheckoutTestHooks installs h. Tests only.
func (s *Service) SetCheckoutTestHooks(h CheckoutTestHooks) { s.checkoutHooks = h }

func runHook(f func()) {
	if f != nil {
		f()
	}
}

// checkoutRefusal answers whether a tenant in state p may start a checkout.
// It returns nil for 'canceled', and for 'none' with no stored subscription
// id; every other state is refused. refreshSubID is set when the refusal is
// billing_subscription_pending, so the caller enqueues a refresh that
// settles the stored subscription.
func checkoutRefusal(p tenantBillingProfile) (err error, refreshSubID string) {
	switch p.Status {
	case StatusComped:
		return domain.Conflict("billing_comped", "this workspace is on a complimentary plan"), ""
	case StatusActive, StatusTrialing, StatusPastDue, StatusPaused:
		return domain.Conflict("billing_subscription_exists", "this workspace already has a subscription; manage it instead"), ""
	case StatusNone:
		if p.ProviderSubscriptionID != "" {
			return domain.Conflict("billing_subscription_pending",
				"a subscription for this workspace is still settling; check back shortly"), p.ProviderSubscriptionID
		}
		return nil, ""
	case StatusCanceled:
		return nil, ""
	}
	return domain.Conflict("billing_subscription_exists", "this workspace already has a subscription; manage it instead"), ""
}

// refuseCheckout returns err, first enqueueing a request-path refresh for
// refreshSubID when one is set. An enqueue failure is logged: the refusal
// stands either way.
func (s *Service) refuseCheckout(ctx context.Context, tenantID uuid.UUID, err error, refreshSubID string) error {
	if refreshSubID != "" {
		if _, qerr := s.EnqueueRequestRefresh(ctx, tenantID, refreshSubID, "checkout"); qerr != nil {
			s.logger.Warn("billing: enqueue refresh on a refused checkout failed",
				slog.String("tenant_id", tenantID.String()), slog.Any("error", qerr))
		}
	}
	return err
}

// ProviderNothingPending proves, from providerName's side and outside any
// transaction, that nothing is pending or live for customerID, so a tenant
// pinned to providerName may be moved off it. It returns nil when that is
// proven, and 409 billing_provider_locked otherwise: reason
// pending_at_provider when the provider reports a pending or live
// subscription, needs_support when this provider cannot prove it
// self-serve.
//
// An empty customerID answers "nothing pending" without any provider call:
// nothing is findable by an empty customer, and an unfiltered list would read
// other products' objects on a shared account.
func (s *Service) ProviderNothingPending(ctx context.Context, providerName, customerID string) error {
	provider, ok := s.registry.Provider(providerName)
	if !ok {
		return providerLocked(ReasonNeedsSupport, "this workspace's current payment provider is not available; contact support to switch")
	}
	lister, ok := provider.(SubscriptionLister)
	if !ok {
		return providerLocked(ReasonNeedsSupport, "switching away from this payment provider needs support in this release")
	}
	if customerID == "" {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, requestProviderTimeout)
	defer cancel()
	_, pending, err := lister.HasPendingOrLive(callCtx, customerID)
	if err != nil {
		return err
	}
	if pending {
		return providerLocked(ReasonPendingAtProvider, "a subscription is still pending or live with this workspace's current payment provider")
	}
	return nil
}

// requestProviderTimeout bounds each provider call a request path makes.
const requestProviderTimeout = 8 * time.Second

// CreateCheckout starts a hosted checkout for tenantID targeting tier.
//
// requestedProvider ("stripe" | "razorpay") is the provider the caller asks
// for; empty means Stripe when it is registered, else the only registered
// provider. The request wins over the tenant's current pin, subject to the
// switch rule: a tenant pinned to another provider moves only when it may
// start a checkout at all, and that provider proves nothing is pending or
// live (ProviderNothingPending). currency matters only to Razorpay. The
// price is resolved server-side from tier; nothing the caller supplies
// selects a price.
//
// No provider call runs inside a transaction. The only write is one locked
// compare-and-set (BindCheckoutProvider) that succeeds only from the pin and
// customer this request checked, or when the pin already equals the
// requested provider; anything else is a 409.
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

	// Pre-checks, no lock. P0 and C0 are the pin and customer every later
	// decision is made on; the locked bind re-checks them.
	profile, err := s.getBillingProfile(ctx, tenantID)
	if err != nil {
		return CheckoutSession{}, err
	}
	if rerr, sub := checkoutRefusal(profile); rerr != nil {
		return CheckoutSession{}, s.refuseCheckout(ctx, tenantID, rerr, sub)
	}
	p0, c0 := profile.BillingProvider, profile.ProviderCustomerID

	// Provider choice: the request wins, subject to the switch rule.
	providerName := requestedProvider
	if providerName == "" {
		providerName = s.registry.DefaultCheckoutProvider()
	}
	provider, ok := s.registry.Provider(providerName)
	if !ok {
		return CheckoutSession{}, domain.ServiceUnavailable("billing_provider_unavailable",
			"the requested payment provider is not available on this instance")
	}

	// Switch rule: moving off another provider needs that provider to prove
	// nothing is pending or live for the stored customer.
	switching := p0 != "" && p0 != providerName
	if switching {
		if err := s.ProviderNothingPending(ctx, p0, c0); err != nil {
			return CheckoutSession{}, err
		}
	}

	// Provider customer, created outside any transaction. A stored id counts
	// only when it belongs to the requested provider.
	var newCustomer *string
	if creator, ok := provider.(CustomerCreator); ok {
		if p0 == providerName && c0 != "" {
			newCustomer = &c0
		} else {
			callCtx, cancel := context.WithTimeout(ctx, requestProviderTimeout)
			id, cerr := creator.CreateCustomer(callCtx, tenantID, customerEmail)
			cancel()
			if cerr != nil {
				return CheckoutSession{}, cerr
			}
			newCustomer = &id
		}
	}

	runHook(s.checkoutHooks.BeforeBind)

	boundCustomer, err := s.bindCheckoutProvider(ctx, tenantID, p0, c0, providerName, newCustomer)
	if err != nil {
		return CheckoutSession{}, err
	}

	// After a switch commits, sweep the previous customer's open sessions
	// again. Best effort: a session that slips through is refused when its
	// events are applied.
	if switching && c0 != "" {
		if prev, ok := s.registry.Provider(p0); ok {
			s.expireOpenSessions(ctx, tenantID, prev, c0)
		}
	}

	// One subscription per customer: a pending or live subscription at the
	// provider that is not stored yet is adopted, not doubled.
	if lister, ok := provider.(SubscriptionLister); ok && boundCustomer != "" {
		callCtx, cancel := context.WithTimeout(ctx, requestProviderTimeout)
		subID, pending, lerr := lister.PendingOrLiveSubscription(callCtx, boundCustomer)
		cancel()
		if lerr != nil {
			return CheckoutSession{}, lerr
		}
		if pending {
			return CheckoutSession{}, s.refuseCheckout(ctx, tenantID, domain.Conflict("billing_subscription_pending",
				"a subscription for this workspace is still settling; check back shortly"), subID)
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, requestProviderTimeout)
	sess, err := provider.CreateCheckout(callCtx, CheckoutInput{
		TenantID:           tenantID,
		Plan:               tier,
		Currency:           currency,
		CustomerEmail:      customerEmail,
		ProviderCustomerID: boundCustomer,
		SuccessURL:         successURL,
		CancelURL:          cancelURL,
	})
	cancel()
	if err != nil {
		return CheckoutSession{}, err
	}

	runHook(s.checkoutHooks.AfterCreate)

	// Re-read: if the pin or customer moved since the bind, this session
	// must not be handed out.
	after, err := s.getBillingProfile(ctx, tenantID)
	if err != nil {
		s.expireOwnSession(ctx, tenantID, provider, sess.SessionID)
		return CheckoutSession{}, err
	}
	_, needsCustomer := provider.(CustomerCreator)
	if after.BillingProvider != providerName || (needsCustomer && after.ProviderCustomerID != boundCustomer) {
		s.expireOwnSession(ctx, tenantID, provider, sess.SessionID)
		return CheckoutSession{}, providerLocked(ReasonProviderChanged,
			"this workspace's payment provider changed during checkout; start again")
	}

	runHook(s.checkoutHooks.AfterReread)

	// Keep one open session per customer: the greatest by (created, id).
	if sup, ok := provider.(CheckoutSessionSuperseder); ok && boundCustomer != "" && sess.SessionID != "" {
		callCtx, cancel := context.WithTimeout(ctx, requestProviderTimeout)
		superseded, serr := sup.SupersedeOpenCheckoutSessions(callCtx, boundCustomer, sess.SessionID)
		cancel()
		if serr != nil {
			s.logger.Warn("billing: superseding older checkout sessions failed",
				slog.String("tenant_id", tenantID.String()), slog.Any("error", serr))
		}
		if superseded {
			return CheckoutSession{}, domain.Conflict("billing_checkout_superseded",
				"a newer checkout was started for this workspace; start again")
		}
	}

	s.recordAudit(ctx, tenantID, actor.Type, actor.ID, "billing.checkout.started", map[string]any{
		"tier":     string(tier),
		"provider": providerName,
		"switched": switching,
	})

	return sess, nil
}

// expireOwnSession expires the session this request created and must not
// hand out. Best effort, logged at Warn.
func (s *Service) expireOwnSession(ctx context.Context, tenantID uuid.UUID, provider Provider, sessionID string) {
	sup, ok := provider.(CheckoutSessionSuperseder)
	if !ok || sessionID == "" {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, requestProviderTimeout)
	defer cancel()
	if err := sup.ExpireCheckoutSession(callCtx, sessionID); err != nil {
		s.alert(ctx, slog.LevelWarn, alertSessionExpiry,
			slog.String("tenant_id", tenantID.String()), slog.Any("error", err))
	}
}

// bindCheckoutProvider is the one locked write of a checkout. Under the
// per-tenant billing lock it compare-and-sets the pin to newProvider from
// (p0, c0), or keeps a pin that already equals newProvider, and returns the
// stored customer id. When nothing was written, a read in the same
// transaction picks the 409 code and decides nothing.
func (s *Service) bindCheckoutProvider(ctx context.Context, tenantID uuid.UUID, p0, c0, newProvider string, customer *string) (string, error) {
	var stored string
	var refusal error
	var refreshSub string
	err := pgx.BeginFunc(ctx, s.pool.Pool, func(tx pgx.Tx) error {
		if err := LockTenantBilling(ctx, tx, tenantID); err != nil {
			return err
		}
		q := sqlc.New(tx)
		got, err := q.BindCheckoutProvider(ctx, sqlc.BindCheckoutProviderParams{
			NewProvider:      newProvider,
			CustomerID:       customer,
			TenantID:         tenantID,
			ExpectedProvider: nonEmptyPtr(p0),
			ExpectedCustomer: nonEmptyPtr(c0),
		})
		if err == nil {
			if got != nil {
				stored = *got
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.Internal("billing_bind_failed", "failed to bind the checkout provider").WithCause(err)
		}
		row, rerr := q.GetTenantBillingProfile(ctx, tenantID)
		if rerr != nil {
			return domain.Internal("billing_profile_lookup_failed", "failed to load tenant billing profile").WithCause(rerr)
		}
		cur := toBillingProfile(row)
		// A pin that already equals newProvider fails only on the state
		// predicate. Otherwise the pin, or the customer under it, moved.
		moved := cur.BillingProvider != newProvider &&
			(cur.BillingProvider != p0 || cur.ProviderCustomerID != c0)
		if !moved {
			refusal, refreshSub = checkoutRefusal(cur)
		}
		if refusal == nil {
			refusal = providerLocked(ReasonProviderChanged,
				"this workspace's payment provider changed during checkout; start again")
			refreshSub = ""
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if refusal != nil {
		return "", s.refuseCheckout(ctx, tenantID, refusal, refreshSub)
	}
	return stored, nil
}

// checkoutSessionIDPattern is the shape of a Stripe Checkout Session id.
var checkoutSessionIDPattern = regexp.MustCompile(`^cs_[A-Za-z0-9_]+$`)

// ConfirmResult says whether the plan change had already landed when a
// checkout was confirmed.
type ConfirmResult struct {
	Landed bool
}

// ConfirmCheckout checks the checkout session the browser returned with and
// enqueues a refresh so activation does not wait for the webhook. It is a
// binding check for a session the caller names: the session must be a
// completed subscription-mode session of the caller's own tenant, on the
// tenant's stored customer. It never changes billing state itself.
func (s *Service) ConfirmCheckout(ctx context.Context, tenantID uuid.UUID, sessionID string) (ConfirmResult, error) {
	if !s.enabled {
		return ConfirmResult{}, domain.Unavailable("billing_disabled", "hosted billing is not enabled on this instance")
	}
	if !checkoutSessionIDPattern.MatchString(sessionID) {
		return ConfirmResult{}, domain.Validation("billing_invalid_session", "session_id is not a checkout session id")
	}
	profile, err := s.getBillingProfile(ctx, tenantID)
	if err != nil {
		return ConfirmResult{}, err
	}
	notConfirmable := domain.Validation("billing_checkout_not_confirmable",
		"this checkout session is not a completed checkout for this workspace")
	if profile.BillingProvider == "" || profile.ProviderCustomerID == "" || s.registry == nil {
		return ConfirmResult{}, notConfirmable
	}
	provider, ok := s.registry.Provider(profile.BillingProvider)
	if !ok {
		return ConfirmResult{}, notConfirmable
	}
	confirmer, ok := provider.(CheckoutSessionConfirmer)
	if !ok {
		return ConfirmResult{}, notConfirmable
	}
	callCtx, cancel := context.WithTimeout(ctx, requestProviderTimeout)
	info, err := confirmer.RetrieveCheckoutSession(callCtx, sessionID)
	cancel()
	if errors.Is(err, ErrCheckoutSessionNotFound) {
		return ConfirmResult{}, notConfirmable
	}
	if err != nil {
		return ConfirmResult{}, err
	}
	// A session another workspace started is not this workspace's to
	// confirm, and is answered as not found.
	if info.ClientReferenceID != tenantID.String() {
		return ConfirmResult{}, domain.NotFound("billing_checkout_session_not_found",
			"no checkout session with this id exists for this workspace")
	}
	if info.CustomerID != profile.ProviderCustomerID ||
		info.Mode != "subscription" || info.Status != "complete" ||
		info.SubscriptionID == "" {
		return ConfirmResult{}, notConfirmable
	}
	if profile.ProviderSubscriptionID == info.SubscriptionID && isLiveStatus(profile.Status) {
		return ConfirmResult{Landed: true}, nil
	}
	if _, err := s.EnqueueRequestRefresh(ctx, tenantID, info.SubscriptionID, "confirm"); err != nil {
		return ConfirmResult{}, domain.Internal("billing_refresh_enqueue_failed", "failed to enqueue the billing refresh").WithCause(err)
	}
	return ConfirmResult{}, nil
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

	// Razorpay reports no cancel schedule back, so the in-app cancel is
	// recorded locally: the flag, and the period end as cancel_at only while
	// that is still in the future.
	if profile.BillingProvider == providerRazorpay {
		if _, err := s.setCancelRequested(ctx, tenantID, profile.ProviderSubscriptionID, false, profile.CurrentPeriodEnd); err != nil {
			return err
		}
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
	// CancelAtPeriodEnd is true once a cancel is scheduled, for the period's
	// end or through Cancel now. CancelAt is the scheduled or actual end, when
	// one is known; a past instant marks Cancel now.
	CancelAtPeriodEnd bool       `json:"cancel_at_period_end"`
	CancelAt          *time.Time `json:"cancel_at,omitempty"`

	Plan             Tier       `json:"plan"`
	PlanStatus       Status     `json:"plan_status"`
	CurrentPeriodEnd *time.Time `json:"current_period_end,omitempty"`
	Provider         string     `json:"provider,omitempty"`
	GraceUntil       *time.Time `json:"grace_until,omitempty"`
	Meters           Meters     `json:"meters"`
	PortalAvailable  bool       `json:"portal_available"`
	// AvailableProviders lists the registered payment providers, "stripe"
	// first. Never null.
	AvailableProviders []string `json:"available_providers"`
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
		// Either stored field means a cancel is scheduled.
		CancelAtPeriodEnd: cancelScheduled(profile),
		CancelAt:          profile.CancelAt,

		Plan:             profile.Plan,
		PlanStatus:       profile.Status,
		CurrentPeriodEnd: profile.CurrentPeriodEnd,
		Provider:         profile.BillingProvider,
		GraceUntil:       profile.GraceUntil,
		Meters: Meters{
			Sites: SiteMeter{Used: int(used), Limit: ent.MaxSites},
		},
		PortalAvailable:    portalAvailable,
		AvailableProviders: s.registry.Names(),
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
