package billing

// cancel.go — Cancel now: ending a past-due card subscription at once, so
// the customer is not left waiting for the retry schedule to run out. A
// period-end cancel is Service.CancelSubscription (checkout.go).

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// RefreshSourceCancel is the billing_refresh source enqueued after Cancel now.
const RefreshSourceCancel = "cancel"

// CancelWhen is when a requested cancel takes effect.
type CancelWhen string

const (
	// CancelAtPeriodEnd ends the subscription when the paid period ends.
	CancelAtPeriodEnd CancelWhen = "period_end"
	// CancelNow ends the subscription at once. Allowed only while a payment
	// is past due, and only for a provider that implements
	// ImmediateCanceller.
	CancelNow CancelWhen = "now"
)

// ParseCancelWhen maps the request's "when" to a CancelWhen. Empty means
// period_end. Anything else is a 422.
func ParseCancelWhen(s string) (CancelWhen, error) {
	switch CancelWhen(s) {
	case "", CancelAtPeriodEnd:
		return CancelAtPeriodEnd, nil
	case CancelNow:
		return CancelNow, nil
	}
	return "", domain.Validation("billing_invalid_cancel_when", "when must be one of: period_end, now")
}

// ImmediateCanceller is an OPTIONAL capability a Provider implements when it
// can end a subscription at once. Implementations must refuse an empty id
// and send nothing. Like CancelSubscription, it never changes a tenant's
// stored state: the apply worker does that from the provider's answer.
type ImmediateCanceller interface {
	CancelSubscriptionNow(ctx context.Context, providerSubscriptionID string) error
}

// errCancelNowNotAllowed is the 422 for a Cancel now the state does not
// allow.
func errCancelNowNotAllowed() error {
	return domain.Validation("billing_cancel_now_not_allowed",
		"cancel now is available only while a card payment is past due; cancel at the end of the period instead")
}

// CancelSubscriptionNow ends tenantID's subscription at once. It is allowed
// only while the stored status is past_due and the provider is Stripe, and
// the provider must confirm, just before the cancel, that the stored
// subscription still belongs to the stored customer and is still past due.
// The provider calls run outside any transaction and under
// providerCallTimeout.
//
// Afterwards a billing_refresh is enqueued so the tenant's state settles
// without waiting for the provider's webhook. Like every other billing
// request path, this never writes the tenant's plan or status itself.
func (s *Service) CancelSubscriptionNow(ctx context.Context, tenantID uuid.UUID, actor Actor) error {
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
	if profile.BillingProvider != providerStripe || profile.Status != StatusPastDue {
		return errCancelNowNotAllowed()
	}
	if s.registry == nil {
		return domain.ServiceUnavailable("billing_not_configured", "no payment provider is configured on this instance yet")
	}
	provider, ok := s.registry.Provider(profile.BillingProvider)
	if !ok {
		return domain.ServiceUnavailable("billing_provider_unavailable",
			"the configured payment provider for this workspace is not available")
	}
	canceller, ok := provider.(ImmediateCanceller)
	if !ok {
		return errCancelNowNotAllowed()
	}

	fetchCtx, cancelFetch := context.WithTimeout(ctx, providerCallTimeout)
	sub, err := provider.GetSubscription(fetchCtx, profile.ProviderSubscriptionID)
	cancelFetch()
	if err != nil {
		return err
	}
	if profile.ProviderCustomerID == "" || sub.CustomerID != profile.ProviderCustomerID || sub.ID != profile.ProviderSubscriptionID {
		s.alert(ctx, slog.LevelError, alertCustomerMismatch,
			slog.String("tenant_id", tenantID.String()), slog.String("provider", profile.BillingProvider),
			slog.String("provider_subscription_id", sub.ID), slog.String("source", RefreshSourceCancel))
		return domain.Conflict("billing_subscription_mismatch",
			"this workspace's subscription could not be confirmed; contact support")
	}
	if sub.Status != StatusPastDue {
		return errCancelNowNotAllowed()
	}

	callCtx, cancelCall := context.WithTimeout(ctx, providerCallTimeout)
	err = canceller.CancelSubscriptionNow(callCtx, profile.ProviderSubscriptionID)
	cancelCall()
	if err != nil {
		return err
	}

	s.recordAudit(ctx, tenantID, actor.Type, actor.ID, "billing.subscription.cancel_requested", map[string]any{
		"provider": profile.BillingProvider,
		"when":     string(CancelNow),
	})

	if _, err := s.EnqueueRequestRefresh(ctx, tenantID, profile.ProviderSubscriptionID, RefreshSourceCancel); err != nil {
		// The cancel already happened at the provider; its webhook and the
		// daily reconcile settle the state without this refresh.
		s.logger.Warn("billing: failed to enqueue the refresh after cancel now",
			slog.String("tenant_id", tenantID.String()), slog.Any("error", err))
	}
	return nil
}
