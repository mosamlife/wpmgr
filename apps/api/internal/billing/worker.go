package billing

// worker.go — the M16 Phase B daily reconcile River worker. Registered only
// when hosted billing is enabled (see cmd/wpmgr/main.go); a no-op boot
// (WPMGR_HOSTED unset) never registers this worker or its periodic job.

import (
	"context"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
)

// ReconcileQueue is the River queue the daily reconcile job runs on.
const ReconcileQueue = "billing_reconcile"

// ReconcileArgs is the (empty) periodic job payload.
type ReconcileArgs struct{}

// Kind implements river.JobArgs.
func (ReconcileArgs) Kind() string { return "billing_reconcile" }

// ReconcileWorker drives the daily drift-repair sweep (see reconcile.go).
type ReconcileWorker struct {
	river.WorkerDefaults[ReconcileArgs]
	svc    *Service
	logger *slog.Logger
}

// NewReconcileWorker builds a ReconcileWorker.
func NewReconcileWorker(svc *Service, logger *slog.Logger) *ReconcileWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &ReconcileWorker{svc: svc, logger: logger}
}

// Timeout gives the sweep 10 minutes — generous headroom for a provider API
// round-trip per tenant plus Postgres latency, well above the expected wall
// time for the tenant counts this early-stage feature will see.
func (w *ReconcileWorker) Timeout(*river.Job[ReconcileArgs]) time.Duration {
	return 10 * time.Minute
}

// Work runs one reconcile sweep.
func (w *ReconcileWorker) Work(ctx context.Context, _ *river.Job[ReconcileArgs]) error {
	result, err := w.svc.Reconcile(ctx)
	if err != nil {
		return err
	}
	w.logger.Info("billing reconcile: sweep complete",
		slog.Int("checked", result.Checked), slog.Int("repaired", result.Repaired))
	return nil
}
