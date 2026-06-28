package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// ---------------------------------------------------------------------------
// Uptime probes retention GC (periodic River job) — m85
// ---------------------------------------------------------------------------

// probeRetention is the age after which site_uptime_probes rows are eligible
// for deletion. Must be strictly greater than the longest query window used by
// QueryFleetUptime (currently 30 days) to avoid deleting rows that are still
// within a live dashboard window. 90 days gives a 3× safety margin and
// matches the ClickHouse TTL already defined in retentionDays.
const probeRetention = 90 * 24 * time.Hour

// probeGCBatchSize caps the number of rows deleted per GC pass to keep the
// DELETE transaction short and avoid long lock contention on a large table.
// At ≤100 sites × 1 probe/min the table grows ~52 M rows/year; a daily
// 500 k-row batch prunes the day's ~144 k new rows plus handles any backlog.
const probeGCBatchSize = 500_000

// UptimeProbeGCArgs is the River job payload for the periodic uptime-probe
// retention GC. It carries no fields; the worker operates cross-tenant.
type UptimeProbeGCArgs struct{}

// Kind implements river.JobArgs.
func (UptimeProbeGCArgs) Kind() string { return "uptime_probe_retention_gc" }

// UptimeProbeGCWorker deletes site_uptime_probes rows whose probed_at is
// older than probeRetention (90 days). Runs cross-tenant under InAgentTx
// (app.agent = 'on') so the single job sweeps the whole table without needing
// to enumerate tenant IDs — mirroring the backup GC and PHP-error GC pattern.
//
// The DELETE uses a single-statement cut (no loop) with LIMIT probeGCBatchSize.
// On a table with months of backlog the job may leave rows older than the
// window on its first run; subsequent daily runs converge it. The LIMIT keeps
// each individual transaction under a second even on large tables.
type UptimeProbeGCWorker struct {
	river.WorkerDefaults[UptimeProbeGCArgs]
	pool   *db.Pool
	logger *slog.Logger
}

// NewUptimeProbeGCWorker builds the uptime-probe retention GC worker.
func NewUptimeProbeGCWorker(pool *db.Pool, logger *slog.Logger) *UptimeProbeGCWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &UptimeProbeGCWorker{pool: pool, logger: logger}
}

// Work deletes site_uptime_probes rows older than probeRetention in a single
// bounded DELETE. The job is enqueued daily by the River periodic scheduler.
func (w *UptimeProbeGCWorker) Work(ctx context.Context, _ *river.Job[UptimeProbeGCArgs]) error {
	cutoff := time.Now().Add(-probeRetention)
	var deleted int64
	err := w.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			fmt.Sprintf(`DELETE FROM site_uptime_probes
WHERE id IN (
    SELECT id FROM site_uptime_probes
    WHERE probed_at < $1
    LIMIT %d
)`, probeGCBatchSize),
			cutoff,
		)
		if err != nil {
			return fmt.Errorf("uptime probe gc delete: %w", err)
		}
		deleted = tag.RowsAffected()
		return nil
	})
	if err != nil {
		w.logger.Warn("uptime probe retention GC error", slog.Any("error", err))
		return err
	}
	if deleted > 0 {
		w.logger.Info("uptime probe retention GC",
			slog.Int64("rows_deleted", deleted),
			slog.Duration("retention", probeRetention),
		)
	}
	return nil
}
