package billing

// reconcile.go — the daily drift-repair sweep. It only lists and enqueues:
// every tenant with a stored provider subscription (comped tenants excluded)
// gets a billing_refresh, and the refresh worker re-derives its state through
// the same locked apply function webhooks use (apply.go). A missed webhook
// therefore never leaves a tenant's stored plan wrong for longer than one
// sweep.

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ReconcileResult summarizes one sweep for logging.
type ReconcileResult struct {
	Checked  int
	Enqueued int
}

// Reconcile enqueues one billing_refresh per tenant with a stored provider
// subscription. It makes no provider call and changes no billing state.
// Returns cleanly (zero work) when hosted billing is disabled or no provider
// is registered.
func (s *Service) Reconcile(ctx context.Context) (ReconcileResult, error) {
	var out ReconcileResult
	if !s.enabled || s.registry == nil || !s.registry.Any() {
		return out, nil
	}
	if s.river == nil {
		return out, errQueueNotWired
	}

	rows, err := sqlc.New(s.pool.Pool).ListTenantsWithProviderSubscription(ctx)
	if err != nil {
		return out, domain.Internal("billing_reconcile_list_failed", "failed to list tenants for billing reconcile").WithCause(err)
	}

	err = pgx.BeginFunc(ctx, s.pool.Pool, func(tx pgx.Tx) error {
		for _, row := range rows {
			out.Checked++
			if row.BillingProvider == nil || row.ProviderSubscriptionID == nil {
				continue
			}
			if _, err := s.river.InsertTx(ctx, tx, BillingRefreshArgs{
				TenantID: row.ID, Source: RefreshSourceReconcile,
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
