package billing

// reconcile.go — the daily drift-repair sweep. It only lists and enqueues:
// every tenant with a stored provider subscription (comped tenants excluded)
// gets a billing_refresh, and the refresh worker re-derives its state through
// the same locked apply function webhooks use (apply.go). A Stripe tenant
// with a stored customer and either no stored subscription or a stored one
// that is no longer live is also looked up at Stripe by that customer, and a
// pending or live subscription found there is refreshed by id, so an
// activation that never reached this database is adopted.
//
// Other providers are refreshed by the stored subscription id only, so a
// subscription of theirs that this database never stored is not found by the
// sweep.

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ReconcileResult summarizes one sweep for logging.
type ReconcileResult struct {
	Checked  int
	Enqueued int
}

// reconcileRefresh is one billing_refresh the sweep enqueues.
type reconcileRefresh struct {
	tenantID       uuid.UUID
	subscriptionID string
}

// Reconcile enqueues one billing_refresh per tenant in the reconcile set
// (ListTenantsForReconcile). It changes no billing state. The only provider
// calls are the customer-filtered subscription lists for Stripe tenants with
// a customer and no live stored subscription, made outside any transaction.
// A failed lookup is logged; the tenant then gets only the refresh of its
// stored subscription, when it has one. Returns cleanly
// (zero work) when hosted billing is disabled or no provider is registered.
func (s *Service) Reconcile(ctx context.Context) (ReconcileResult, error) {
	var out ReconcileResult
	if !s.enabled || s.registry == nil || !s.registry.Any() {
		return out, nil
	}
	if s.river == nil {
		return out, errQueueNotWired
	}

	rows, err := sqlc.New(s.pool.Pool).ListTenantsForReconcile(ctx)
	if err != nil {
		return out, domain.Internal("billing_reconcile_list_failed", "failed to list tenants for billing reconcile").WithCause(err)
	}

	var refreshes []reconcileRefresh
	for _, row := range rows {
		out.Checked++
		if row.BillingProvider == nil {
			continue
		}
		stored := ""
		if row.ProviderSubscriptionID != nil {
			stored = *row.ProviderSubscriptionID
		}
		lookup := *row.BillingProvider == providerStripe &&
			row.ProviderCustomerID != nil && *row.ProviderCustomerID != "" &&
			(stored == "" || !isLiveStatus(Status(row.PlanStatus)))
		if lookup {
			if subID, ok := s.reconcileLookupByCustomer(ctx, row.ID, *row.ProviderCustomerID); ok && subID != stored {
				refreshes = append(refreshes, reconcileRefresh{tenantID: row.ID, subscriptionID: subID})
				continue
			}
		}
		if stored != "" {
			refreshes = append(refreshes, reconcileRefresh{tenantID: row.ID})
		}
	}

	err = pgx.BeginFunc(ctx, s.pool.Pool, func(tx pgx.Tx) error {
		for _, r := range refreshes {
			if _, err := s.river.InsertTx(ctx, tx, BillingRefreshArgs{
				TenantID: r.tenantID, SubscriptionID: r.subscriptionID, Source: RefreshSourceReconcile,
			}, nil); err != nil {
				return err
			}
			out.Enqueued++
		}
		return nil
	})
	if err != nil {
		return ReconcileResult{}, domain.Internal("billing_reconcile_enqueue_failed", "failed to enqueue billing refreshes").WithCause(err)
	}
	return out, nil
}

// reconcileLookupByCustomer asks Stripe, filtered by customerID, for a
// pending or live subscription the tenant has not stored. It returns the
// subscription id and true when one exists. Nothing found, no lister, or a
// failed call returns false; a failure is logged and retried next sweep.
func (s *Service) reconcileLookupByCustomer(ctx context.Context, tenantID uuid.UUID, customerID string) (string, bool) {
	provider, ok := s.registry.Provider(providerStripe)
	if !ok {
		return "", false
	}
	lister, ok := provider.(SubscriptionLister)
	if !ok {
		return "", false
	}
	callCtx, cancel := context.WithTimeout(ctx, providerCallTimeout)
	subID, pending, err := lister.PendingOrLiveSubscription(callCtx, customerID)
	cancel()
	if err != nil {
		s.logger.Warn("billing: reconcile lookup by customer failed",
			slog.String("tenant_id", tenantID.String()), slog.Any("error", err))
		return "", false
	}
	if !pending || subID == "" {
		return "", false
	}
	return subID, true
}
