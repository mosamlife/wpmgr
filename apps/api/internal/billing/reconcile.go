package billing

// reconcile.go — M16 Phase B daily drift-repair sweep. A missed/lost webhook
// delivery (a provider outage, a deploy window, a dropped 5xx) must never
// leave a tenant's stored plan/status permanently wrong; this sweep re-derives
// every tenant with a live provider subscription reference from the SAME
// nextBillingState pipeline the webhook consumer uses, so drift repair is
// AUTOMATICALLY "fail toward the customer": an upgrade/active state applies
// immediately (nextBillingState's active/trialing branch), while a
// downgrade only ever moves through the existing graded ladder — past_due's
// 7-day grace, canceled's non-destructive plan=free — never a hard,
// immediate cutoff. There is no separate "downgrade" code path to get wrong.

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ReconcileResult summarizes one sweep for logging/observability.
type ReconcileResult struct {
	Checked  int
	Repaired int
}

// Reconcile lists every tenant with a live provider subscription reference
// (excluding comped tenants, which are immune to any provider-driven
// mutation) and re-derives its billing state from the provider. A tenant
// whose provider is not registered, or whose subscription fetch fails, is
// logged and skipped — one tenant's provider hiccup must never abort the
// rest of the sweep. Returns cleanly (zero work) when hosted billing is
// disabled or no provider is registered.
func (s *Service) Reconcile(ctx context.Context) (ReconcileResult, error) {
	var out ReconcileResult
	if !s.enabled || s.registry == nil || !s.registry.Any() {
		return out, nil
	}

	rows, err := sqlc.New(s.pool.Pool).ListTenantsWithProviderSubscription(ctx)
	if err != nil {
		return out, domain.Internal("billing_reconcile_list_failed", "failed to list tenants for billing reconcile").WithCause(err)
	}

	for _, row := range rows {
		out.Checked++
		if row.BillingProvider == nil || row.ProviderSubscriptionID == nil {
			continue
		}
		if repaired, err := s.reconcileOne(ctx, row.ID, *row.BillingProvider, *row.ProviderSubscriptionID); err != nil {
			s.logger.Warn("billing reconcile: tenant skipped", slog.String("tenant_id", row.ID.String()), slog.Any("error", err))
		} else if repaired {
			out.Repaired++
		}
	}

	return out, nil
}

// ReconcileOneNow immediately re-derives tenantID's billing state from its
// live provider subscription (see reconcileOne) — the same drift-repair
// pipeline the daily sweep and webhook consumer use. Used by the superadmin
// billing panel's (internal/admin, M16 Phase C1) "revoke comp" action to
// adopt whatever the provider currently reports the instant a comp override
// is lifted, rather than waiting for the next scheduled sweep.
// hadSubscription=false means tenantID has no live provider subscription
// reference to reconcile against — the caller should then fall back to
// plan=free (see AdminRevokeCompToFree).
func (s *Service) ReconcileOneNow(ctx context.Context, tenantID uuid.UUID) (repaired bool, hadSubscription bool, err error) {
	if !s.enabled || s.registry == nil || !s.registry.Any() {
		return false, false, nil
	}
	profile, err := s.getBillingProfile(ctx, tenantID)
	if err != nil {
		return false, false, err
	}
	if profile.BillingProvider == "" || profile.ProviderSubscriptionID == "" {
		return false, false, nil
	}
	repaired, err = s.reconcileOne(ctx, tenantID, profile.BillingProvider, profile.ProviderSubscriptionID)
	return repaired, true, err
}

// reconcileOne reconciles a single tenant. Returns repaired=true when a drift
// was found and applied.
func (s *Service) reconcileOne(ctx context.Context, tenantID uuid.UUID, providerName, providerSubscriptionID string) (bool, error) {
	provider, ok := s.registry.Provider(providerName)
	if !ok {
		return false, domain.ServiceUnavailable("billing_provider_unavailable", "provider not registered")
	}

	profile, err := s.getBillingProfile(ctx, tenantID)
	if err != nil {
		return false, err
	}

	sub, err := provider.GetSubscription(ctx, providerSubscriptionID)
	if err != nil {
		return false, domain.Internal("billing_subscription_fetch_failed", "failed to fetch subscription").WithCause(err)
	}

	if !sub.PlanResolved && statusAppliesPlan(sub.Status) {
		return false, domain.Validation("billing_unknown_price", "subscription price does not map to a known tier")
	}

	next := nextBillingState(profile, sub, s.clock.Now())
	if next.Plan == profile.Plan && next.Status == profile.Status {
		return false, nil // no drift
	}

	if err := s.applySubscriptionState(ctx, tenantID, next); err != nil {
		return false, err
	}
	s.invalidateCache(ctx, tenantID)
	s.recordAudit(ctx, tenantID, audit.ActorSystem, "billing_reconcile", "billing.subscription.changed", map[string]any{
		"old_plan":   string(profile.Plan),
		"new_plan":   string(next.Plan),
		"old_status": string(profile.Status),
		"new_status": string(next.Status),
		"source":     "reconcile",
	})
	s.logger.Info("billing reconcile: repaired drift",
		slog.String("tenant_id", tenantID.String()),
		slog.String("old_plan", string(profile.Plan)), slog.String("new_plan", string(next.Plan)),
		slog.String("old_status", string(profile.Status)), slog.String("new_status", string(next.Status)))
	return true, nil
}
