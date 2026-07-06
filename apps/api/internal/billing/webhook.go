package billing

// webhook.go — M16 Phase B webhook ingestion. This is the ONLY place a
// provider webhook payload is trusted for anything beyond identifying which
// tenant/subscription an event concerns: every actual state change re-fetches
// the subscription from the provider before applying it ("push is a hint,
// pull is the truth"), and every step here is provider-agnostic — a second
// adapter (e.g. Razorpay) plugs into this exact same flow.

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ProcessWebhook is the full webhook-ingestion flow:
//
//  1. Resolve the named provider from the registry (404-equivalent — an
//     unknown provider path segment — if absent).
//  2. VerifyWebhook (signature + replay tolerance). A failure here is the
//     ONLY path that returns an error the HTTP layer maps to 401 — nothing
//     before this point is trusted.
//  3. Ledger the event (INSERT ... ON CONFLICT DO NOTHING). A duplicate
//     delivery (same provider + provider_event_id) returns immediately —
//     idempotent no-op, 200.
//  4. Unhandled event types (a provider event this CP does not act on) are
//     ledgered and skip every remaining step.
//  5. Resolve the tenant: the event's own metadata attribution, falling back
//     to a (provider, provider_customer_id) lookup. An unresolvable tenant
//     ("unknown customer") is ledgered and dropped — no error, no mutation.
//  6. Comped-tenant immunity: a plan_status=comped tenant is NEVER mutated by
//     a webhook, regardless of what the provider reports.
//  7. Out-of-order guard: an event older than the newest ALREADY-APPLIED
//     event for this tenant is ledgered but not applied.
//  8. Re-fetch the subscription's current state (GetSubscription) when the
//     event carries a subscription id. "Unknown price" (the subscription's
//     price does not map to a tier) is ledgered and dropped, same as unknown
//     customer.
//  9. Apply the state machine (nextBillingState) and persist it.
//  10. Invalidate the tenant's cached Entitlements and record an audit entry.
//
// Every path ends by marking the ledgered event processed_at, so a later
// duplicate delivery of the SAME event is recognized as already-handled at
// step 3 without redoing any of this work.
func (s *Service) ProcessWebhook(ctx context.Context, providerName string, rawBody []byte, headers http.Header) error {
	// Belt-and-braces: cmd/wpmgr/main.go only mounts the webhook route at all
	// when hosted billing is enabled, but this guard mirrors every other
	// tenant-facing method in this package (CreateCheckout/CreatePortalSession/
	// GetBillingSummary) so ProcessWebhook is never a way to mutate a tenant's
	// plan on a build that has hosted billing turned off.
	if !s.enabled {
		return domain.Unavailable("billing_disabled", "hosted billing is not enabled on this instance")
	}
	if s.registry == nil {
		return domain.NotFound("billing_provider_unknown", "no payment provider is configured")
	}
	provider, ok := s.registry.Provider(providerName)
	if !ok {
		return domain.NotFound("billing_provider_unknown", "unrecognized payment provider")
	}

	ev, err := provider.VerifyWebhook(rawBody, headers)
	if err != nil {
		return domain.Unauthorized("billing_webhook_signature_invalid", "webhook signature verification failed").WithCause(err)
	}

	eventID, inserted, err := s.insertBillingEvent(ctx, billingEventInsert{
		Provider:        providerName,
		ProviderEventID: ev.ProviderEventID,
		Kind:            ev.ProviderEventType,
		TenantID:        ev.TenantID,
		OccurredAt:      ev.OccurredAt,
		Payload: map[string]any{
			"normalized_kind":          string(ev.Kind),
			"provider_customer_id":     ev.ProviderCustomerID,
			"provider_subscription_id": ev.ProviderSubscriptionID,
		},
	})
	if err != nil {
		return err
	}
	if !inserted {
		// Duplicate delivery of an event we have already recorded (and, if it
		// was ever going to be applied, already applied). Idempotent no-op.
		s.logger.Info("billing: duplicate webhook event ignored",
			slog.String("provider", providerName), slog.String("event_id", ev.ProviderEventID))
		return nil
	}

	if !ev.Handled {
		// Checked BEFORE tenant resolution: a provider event type this CP does
		// not act on has nothing useful to attribute or reconcile — ledger it
		// and stop, rather than attempting (and possibly failing) a customer
		// lookup for an event nobody will ever read.
		s.logger.Debug("billing: webhook event type not acted on — ledgered only",
			slog.String("provider", providerName), slog.String("event_type", ev.ProviderEventType))
		return s.markBillingEventProcessed(ctx, eventID)
	}

	tenantID := ev.TenantID
	if tenantID == uuid.Nil && ev.ProviderCustomerID != "" {
		found, ok, ferr := s.findTenantByProviderCustomer(ctx, providerName, ev.ProviderCustomerID)
		if ferr != nil {
			return ferr
		}
		if ok {
			tenantID = found
			if serr := s.setBillingEventTenant(ctx, eventID, tenantID); serr != nil {
				s.logger.Warn("billing: failed to backfill billing_events.tenant_id", slog.Any("error", serr))
			}
		}
	}

	if tenantID == uuid.Nil {
		s.logger.Warn("billing: webhook event has no resolvable tenant (unknown customer) — recording only",
			slog.String("provider", providerName), slog.String("event_id", ev.ProviderEventID),
			slog.String("provider_customer_id", ev.ProviderCustomerID))
		return s.markBillingEventProcessed(ctx, eventID)
	}

	profile, err := s.getBillingProfile(ctx, tenantID)
	if err != nil {
		return err
	}

	// Comped-tenant immunity: never mutated by a webhook.
	if profile.Status == StatusComped {
		s.logger.Info("billing: webhook event for a comped tenant ignored (immunity)",
			slog.String("tenant_id", tenantID.String()), slog.String("event_type", ev.ProviderEventType))
		return s.markBillingEventProcessed(ctx, eventID)
	}

	outOfOrder, err := s.isOutOfOrder(ctx, tenantID, eventID, ev.OccurredAt)
	if err != nil {
		return err
	}
	if outOfOrder {
		s.logger.Info("billing: out-of-order webhook event ignored",
			slog.String("tenant_id", tenantID.String()), slog.String("event_id", ev.ProviderEventID))
		return s.markBillingEventProcessed(ctx, eventID)
	}

	if ev.ProviderSubscriptionID == "" {
		// No subscription reference to reconcile against (e.g. a bare
		// charge.refunded with no recurring context). Ledgered only.
		return s.markBillingEventProcessed(ctx, eventID)
	}

	sub, err := provider.GetSubscription(ctx, ev.ProviderSubscriptionID)
	if err != nil {
		return domain.Internal("billing_subscription_fetch_failed", "failed to fetch current subscription state from the payment provider").WithCause(err)
	}

	// "Unknown price": the subscription's price does not map to a tier this
	// CP knows about. Only matters for a status that would otherwise apply
	// sub.Plan (active/trialing/past_due) — record + warn, change nothing.
	if !sub.PlanResolved && statusAppliesPlan(sub.Status) {
		s.logger.Warn("billing: subscription price does not map to a known tier (unknown price) — recording only",
			slog.String("tenant_id", tenantID.String()), slog.String("provider_subscription_id", sub.ID))
		return s.markBillingEventProcessed(ctx, eventID)
	}

	next := nextBillingState(profile, sub, s.clock.Now())
	if err := s.applySubscriptionState(ctx, tenantID, next); err != nil {
		return err
	}

	s.invalidateCache(ctx, tenantID)

	if profile.Plan != next.Plan || profile.Status != next.Status {
		s.recordAudit(ctx, tenantID, audit.ActorSystem, providerName, "billing.subscription.changed", map[string]any{
			"old_plan":   string(profile.Plan),
			"new_plan":   string(next.Plan),
			"old_status": string(profile.Status),
			"new_status": string(next.Status),
			"source":     "webhook",
			"event_type": ev.ProviderEventType,
		})
	}

	return s.markBillingEventProcessed(ctx, eventID)
}
