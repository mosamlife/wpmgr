package billing

import "time"

// pastDueGracePeriod is how long a past_due tenant keeps its paid limits
// (internal/billing's Entitlements status gate reads tenants.grace_until —
// see effectiveTier in entitlements.go) before falling back to free.
const pastDueGracePeriod = 7 * 24 * time.Hour

// tenantBillingProfile is the provider-agnostic subset of a tenant's billing
// row the state machine reads and writes. Decoupled from the sqlc row shape
// so nextBillingState stays a pure, table-free function (mirrors resolve()'s
// tenantBillingRow in entitlements.go).
type tenantBillingProfile struct {
	Plan                   Tier
	Status                 Status
	GraceUntil             *time.Time
	BillingProvider        string
	ProviderCustomerID     string
	ProviderSubscriptionID string
	CurrentPeriodEnd       *time.Time
}

// statusAppliesPlan reports whether a normalized provider Status is one where
// the freshly fetched Subscription.Plan is actually consulted by
// nextBillingState (active/trialing/past_due all keep the subscribed tier;
// every other status forces the tenant to free regardless of Plan). Used to
// scope the "unknown price" no-op guard: an unresolved price only matters
// when it would otherwise be applied.
func statusAppliesPlan(status Status) bool {
	switch status {
	case StatusActive, StatusTrialing, StatusPastDue:
		return true
	}
	return false
}

// nextBillingState computes a tenant's NEXT billing profile from a freshly
// fetched provider Subscription ("pull is the truth" — the caller must have
// already called Provider.GetSubscription; nothing here trusts a webhook
// payload's own claims). current is the tenant's billing row BEFORE this
// event; now is the clock used to anchor a fresh past_due grace window.
//
// Full transition matrix:
//
//	active/trialing  -> plan=sub.Plan, status=sub.Status, grace cleared.
//	past_due         -> plan=sub.Plan (kept), status=past_due; grace_until is
//	                    set to now+7d ONLY on first entry into past_due (the
//	                    prior status was not already past_due, or no grace was
//	                    recorded yet) — a repeat invoice.payment_failed retry
//	                    notification does NOT push the window further out.
//	canceled         -> NON-DESTRUCTIVE downgrade: plan=free, status=canceled,
//	                    grace cleared. Sites/backups/history are untouched —
//	                    only NEW growth is blocked, via the existing
//	                    CheckSiteCreate free-tier cap (Phase A).
//	paused           -> status=paused, grace cleared. plan is left as the
//	                    subscription reports it (informational only): the
//	                    Phase A entitlements gate (effectiveTier) already
//	                    resolves ANY non-active/trialing/comped/in-grace
//	                    status to free, so a paused tenant is already
//	                    capacity-limited regardless of the stored plan value.
//	anything else    -> (incomplete, incomplete_expired, unpaid, unknown) is
//	                    treated as "not a real subscription yet/anymore":
//	                    plan=free, status=none, grace cleared.
//
// billingProvider/providerCustomerID/providerSubscriptionID are carried
// forward from the freshly fetched subscription; the caller is responsible
// for having already set tenants.billing_provider on first checkout (see
// Service.CreateCheckout) — nextBillingState does not invent a provider name.
func nextBillingState(current tenantBillingProfile, sub Subscription, now time.Time) tenantBillingProfile {
	next := current
	next.ProviderSubscriptionID = sub.ID
	if sub.CustomerID != "" {
		next.ProviderCustomerID = sub.CustomerID
	}
	cpe := sub.CurrentPeriodEnd
	next.CurrentPeriodEnd = &cpe

	switch sub.Status {
	case StatusActive, StatusTrialing:
		if sub.PlanResolved {
			next.Plan = sub.Plan
		}
		next.Status = sub.Status
		next.GraceUntil = nil

	case StatusPastDue:
		if sub.PlanResolved {
			next.Plan = sub.Plan
		}
		if current.Status != StatusPastDue || current.GraceUntil == nil {
			grace := now.Add(pastDueGracePeriod)
			next.GraceUntil = &grace
		}
		next.Status = StatusPastDue

	case StatusPaused:
		next.Status = StatusPaused
		next.GraceUntil = nil

	case StatusCanceled:
		next.Plan = TierFree
		next.Status = StatusCanceled
		next.GraceUntil = nil

	default:
		next.Plan = TierFree
		next.Status = StatusNone
		next.GraceUntil = nil
	}

	return next
}
