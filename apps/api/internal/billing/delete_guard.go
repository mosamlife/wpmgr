package billing

// delete_guard.go — whether a tenant's billing state lets its organisation be
// deleted. The organisation delete asks twice: once before its lifecycle
// lock, for the message, and again inside its locked transaction under the
// billing lock, which is the check that decides. Lock order is always the
// organisation lifecycle lock first, then the billing lock. Nothing here
// cancels a subscription.

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// OrgDeleteBlockedCode is the error code of a delete refused for billing.
const OrgDeleteBlockedCode = "billing_active"

// Values of details.reason on a delete refused for billing.
const (
	// OrgDeleteReasonCancelRequired: a live subscription with no cancel
	// scheduled.
	OrgDeleteReasonCancelRequired = "cancel_required"
	// OrgDeleteReasonPastDue: a payment is past due. Only Cancel now (card
	// payments) or the subscription ending unblocks the delete.
	OrgDeleteReasonPastDue = "past_due"
	// OrgDeleteReasonComped: a complimentary plan with a subscription
	// attached.
	OrgDeleteReasonComped = "comped_subscription"
	// OrgDeleteReasonPending: a subscription is attached while its state is
	// still settling.
	OrgDeleteReasonPending = "subscription_pending"
)

// cancelScheduled reports whether a cancel is scheduled, from either field.
func cancelScheduled(p tenantBillingProfile) bool {
	return p.CancelAtPeriodEnd || p.CancelAt != nil
}

// orgDeleteBlock returns nil when the billing state allows the organisation
// to be deleted, and a 409 billing_active error with details.reason when it
// does not.
//
// Allowed: no subscription id stored; an id stored and the subscription
// canceled; active, trialing or paused with a cancel scheduled; past due on
// Stripe with the Cancel now marker (cancel_at at or before now).
// Everything else is refused, including every status this function does not
// name.
func orgDeleteBlock(p tenantBillingProfile, now time.Time) error {
	if p.ProviderSubscriptionID == "" {
		return nil
	}
	switch p.Status {
	case StatusCanceled:
		return nil
	case StatusActive, StatusTrialing, StatusPaused:
		if cancelScheduled(p) {
			return nil
		}
		return orgDeleteRefusal(OrgDeleteReasonCancelRequired,
			"cancel the subscription before deleting this organisation; deletion is allowed once a cancellation is scheduled")
	case StatusPastDue:
		if p.BillingProvider == providerStripe && p.CancelAt != nil && !p.CancelAt.After(now) {
			return nil
		}
		return orgDeleteRefusal(OrgDeleteReasonPastDue,
			"a payment is past due; cancel the subscription now, or wait for it to end, before deleting this organisation")
	case StatusComped:
		return orgDeleteRefusal(OrgDeleteReasonComped,
			"this organisation has a complimentary plan with a subscription attached; contact support to delete it")
	default:
		return orgDeleteRefusal(OrgDeleteReasonPending,
			"this organisation's subscription is still settling; try again in a few minutes, or contact support")
	}
}

func orgDeleteRefusal(reason, msg string) error {
	return domain.Conflict(OrgDeleteBlockedCode, msg).WithDetails(map[string]any{"reason": reason})
}

// CheckOrgDeletable is the check before the organisation lifecycle lock. It
// reads the tenant row without a lock and exists for the message: the
// deciding check is CheckOrgDeletableLocked. Returns nil when hosted billing
// is disabled.
func (s *Service) CheckOrgDeletable(ctx context.Context, tenantID uuid.UUID) error {
	if !s.enabled {
		return nil
	}
	row, err := sqlc.New(s.pool.Pool).GetTenantBillingProfile(ctx, tenantID)
	return s.orgDeleteVerdict(row, err)
}

// CheckOrgDeletableLocked is the deciding check. It runs inside the caller's
// organisation-delete transaction, after the caller took the organisation
// lifecycle lock, and takes the per-tenant billing lock before reading, so
// no billing write for this tenant can land between this read and the
// delete's commit. Returns nil when hosted billing is disabled.
func (s *Service) CheckOrgDeletableLocked(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	if !s.enabled {
		return nil
	}
	if err := LockTenantBilling(ctx, tx, tenantID); err != nil {
		return err
	}
	row, err := sqlc.New(tx).GetTenantBillingProfile(ctx, tenantID)
	return s.orgDeleteVerdict(row, err)
}

func (s *Service) orgDeleteVerdict(row sqlc.GetTenantBillingProfileRow, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NotFound("org_not_found", "organisation not found")
	}
	if err != nil {
		return domain.Internal("billing_profile_lookup_failed", "failed to load tenant billing profile").WithCause(err)
	}
	return orgDeleteBlock(toBillingProfile(row), s.clock.Now())
}
