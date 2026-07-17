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
// older than probeRetention (90 days), AND (m99 follow-up) the
// site_uptime_daily / site_uptime_status rollup rows that have aged past the
// same retention — folded into this SAME daily job rather than a separate
// worker/periodic-job registration, since it already runs cross-tenant under
// InAgentTx (app.agent = 'on') on the same cadence. Mirrors the backup GC and
// PHP-error GC pattern.
//
// Retention MUST stay >= the longest QueryFleetUptime window (30 days) with
// margin, or a live dashboard window would lose data out from under it — 90
// days (shared with the raw-probe retention below) gives a 3x margin.
// QueryFleetUptime's site_uptime_status join uses this SAME probeRetention
// constant as its "surface the latest status only if still within retention"
// cutoff (see fleetUptimeParams in postgres.go), so the query-time behavior
// and this GC's deletion are always in lockstep — a site's status can never
// appear "stale" in a query result for longer than it takes this job to
// actually delete the row, and never sooner (both read the same constant).
//
// The site_uptime_probes DELETE uses a single-statement cut (no loop) with
// LIMIT probeGCBatchSize — on a table with months of backlog the job may
// leave rows older than the window on its first run; subsequent daily runs
// converge it. The rollup tables are orders of magnitude smaller (one row
// per site per day, or one row per site) so their DELETEs need no batching.
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
// bounded DELETE, then prunes site_uptime_daily/site_uptime_status rows past
// the same cutoff. The job is enqueued daily by the River periodic scheduler.
// All three deletes run in ONE transaction so the rollup pruning and the raw-
// probe pruning are always consistent with each other (never a state where
// one has advanced past a boot/retry and the other hasn't).
func (w *UptimeProbeGCWorker) Work(ctx context.Context, _ *river.Job[UptimeProbeGCArgs]) error {
	cutoff := time.Now().Add(-probeRetention)
	cutoffDay := cutoff.UTC().Truncate(24 * time.Hour)
	var deletedProbes, deletedDaily, deletedStatus int64
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
		deletedProbes = tag.RowsAffected()

		// site_uptime_daily: prune whole days entirely past retention. Bound
		// by cutoffDay (a UTC calendar date) rather than cutoff (an instant)
		// so a day that is only PARTIALLY past retention is kept whole —
		// QueryFleetUptime's rollup middle only ever reads days it can prove
		// are fully outside any live window anyway (day > boundaryDay), so
		// this is conservative, never a correctness risk.
		tagDaily, err := tx.Exec(ctx, `DELETE FROM site_uptime_daily WHERE day < $1::date`, cutoffDay)
		if err != nil {
			return fmt.Errorf("uptime daily rollup gc delete: %w", err)
		}
		deletedDaily = tagDaily.RowsAffected()

		// site_uptime_status: drop the stale current-status stamp for any
		// site that has not been probed within retention (a paused/
		// disconnected/deleted-but-not-cascaded site) — matches the OLD
		// raw-probe query's behavior, where a GC'd site's last probe row
		// disappearing made QueryFleetUptime omit it as "no data".
		tagStatus, err := tx.Exec(ctx, `DELETE FROM site_uptime_status WHERE last_probed_at < $1`, cutoff)
		if err != nil {
			return fmt.Errorf("uptime status rollup gc delete: %w", err)
		}
		deletedStatus = tagStatus.RowsAffected()
		return nil
	})
	if err != nil {
		w.logger.Warn("uptime probe retention GC error", slog.Any("error", err))
		return err
	}
	if deletedProbes > 0 || deletedDaily > 0 || deletedStatus > 0 {
		w.logger.Info("uptime probe retention GC",
			slog.Int64("probes_deleted", deletedProbes),
			slog.Int64("daily_rollup_deleted", deletedDaily),
			slog.Int64("status_deleted", deletedStatus),
			slog.Duration("retention", probeRetention),
		)
	}
	return nil
}
