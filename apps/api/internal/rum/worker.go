package rum

import (
	"context"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/config"
)

// ---------------------------------------------------------------------------
// RUM Raw-Event GC worker
// ---------------------------------------------------------------------------

// RumGCArgs is the River job payload for the RUM retention-GC periodic sweep.
type RumGCArgs struct{}

// Kind implements river.JobArgs.
func (RumGCArgs) Kind() string { return "rum_gc" }

// RumGCWorker deletes expired raw events, hourly rollups, and daily rollups.
// Retention windows (configurable via RetentionConfig):
//
//   - Raw events:     48h (SaaS) / 24h (self-host)
//   - Hourly rollups: 14 days (SaaS) / 7 days (self-host)
//   - Daily rollups:  13 months (SaaS) / 90 days (self-host)
//
// The worker runs cross-tenant under InAgentTx. It is a simple periodic sweep
// registered in startRiver; no per-tenant fan-out is needed because the GC
// queries span ALL tenants in a single DELETE.
type RumGCWorker struct {
	river.WorkerDefaults[RumGCArgs]
	store     Store
	retention RetentionConfig
	logger    *slog.Logger
}

// RetentionConfig holds the per-tier retention windows for RUM data.
type RetentionConfig struct {
	// RawEventsTTL is how long raw events are kept. Default 48h (SaaS), 24h (self-host).
	RawEventsTTL time.Duration
	// HourlyRollupTTL is how long hourly rollup rows are kept. Default 14d.
	HourlyRollupTTL time.Duration
	// DailyRollupTTL is how long daily rollup rows are kept. Default 13*30d (≈13 months).
	DailyRollupTTL time.Duration
}

// DefaultRetention returns the SaaS retention defaults.
func DefaultRetention(cfg config.Config) RetentionConfig {
	if cfg.IsProduction() {
		// SaaS hosted defaults (generous — rollups are cheap; raw events are the only pressure).
		return RetentionConfig{
			RawEventsTTL:    48 * time.Hour,
			HourlyRollupTTL: 14 * 24 * time.Hour,
			DailyRollupTTL:  13 * 30 * 24 * time.Hour,
		}
	}
	// Self-host defaults: tighter to save disk on single-node installs.
	return RetentionConfig{
		RawEventsTTL:    24 * time.Hour,
		HourlyRollupTTL: 7 * 24 * time.Hour,
		DailyRollupTTL:  90 * 24 * time.Hour,
	}
}

// NewRumGCWorker constructs a RumGCWorker.
func NewRumGCWorker(store Store, retention RetentionConfig, logger *slog.Logger) *RumGCWorker {
	return &RumGCWorker{store: store, retention: retention, logger: logger}
}

// Work runs the RUM GC sweep: raw events → hourly rollups → daily rollups.
func (w *RumGCWorker) Work(ctx context.Context, _ *river.Job[RumGCArgs]) error {
	now := time.Now().UTC()

	rawCutoff := now.Add(-w.retention.RawEventsTTL)
	n, err := w.store.PruneRawEvents(ctx, rawCutoff)
	if err != nil {
		w.logger.Error("rum gc: prune raw events failed", slog.String("err", err.Error()))
		return err
	}
	if n > 0 {
		w.logger.Info("rum gc: pruned raw events", slog.Int64("count", n))
	}

	hourlyCutoff := now.Add(-w.retention.HourlyRollupTTL)
	n, err = w.store.PruneHourlyRollups(ctx, hourlyCutoff)
	if err != nil {
		w.logger.Error("rum gc: prune hourly rollups failed", slog.String("err", err.Error()))
		return err
	}
	if n > 0 {
		w.logger.Info("rum gc: pruned hourly rollups", slog.Int64("count", n))
	}

	dailyCutoff := now.Add(-w.retention.DailyRollupTTL)
	n, err = w.store.PruneDailyRollups(ctx, dailyCutoff)
	if err != nil {
		w.logger.Error("rum gc: prune daily rollups failed", slog.String("err", err.Error()))
		return err
	}
	if n > 0 {
		w.logger.Info("rum gc: pruned daily rollups", slog.Int64("count", n))
	}

	return nil
}

// ---------------------------------------------------------------------------
// RUM Rollup worker
// ---------------------------------------------------------------------------

// RumRollupArgs is the River job payload for rolling raw events into rollups
// for a specific (site_id, tenant_id, bucket_hour).
type RumRollupArgs struct {
	SiteID     string `json:"site_id"`
	TenantID   string `json:"tenant_id"`
	BucketHour string `json:"bucket_hour"` // RFC3339
}

// Kind implements river.JobArgs.
func (RumRollupArgs) Kind() string { return "rum_rollup" }

// RumRollupWorker is a no-op retained so the "rum_rollup" River job kind stays
// known and any previously enqueued jobs drain cleanly.
//
// V1 mechanism: StorePostgres.WriteEvent writes the raw event AND additively
// upserts the hourly and daily rollup rows in the same InRumIngestTx on every
// beacon, so rollups are populated in real-time without any worker involved.
// See Work below for the accurate description of what this worker does.
type RumRollupWorker struct {
	river.WorkerDefaults[RumRollupArgs]
	store  *StorePostgres
	logger *slog.Logger
}

// NewRumRollupWorker constructs a RumRollupWorker. store must be the concrete
// *StorePostgres because it exposes UpsertRollupHourly/UpsertRollupDaily
// directly; the Store interface does not expose them.
func NewRumRollupWorker(store *StorePostgres, logger *slog.Logger) *RumRollupWorker {
	return &RumRollupWorker{store: store, logger: logger}
}

// Work is a no-op fold retained for Phase 2 batch-aggregation.
//
// V1 mechanism: StorePostgres.WriteEvent writes the raw event AND additively
// upserts the hourly and daily rollup rows in the same InRumIngestTx on every
// beacon. This means rollups are populated in real-time without any worker. The
// throttled rum.rollup_updated SSE emit is also fired by the ingest handler
// (see Handler.maybeEmitRollupUpdated).
//
// This worker remains registered so the River job kind is known and any
// previously enqueued jobs (from prior deployments) drain cleanly. Phase 2
// may repurpose it to batch-fold raw events when ingest volume warrants a
// separate aggregation step.
func (w *RumRollupWorker) Work(ctx context.Context, job *river.Job[RumRollupArgs]) error {
	bh, err := time.Parse(time.RFC3339, job.Args.BucketHour)
	if err != nil {
		w.logger.Error("rum rollup: parse bucket_hour", slog.String("err", err.Error()))
		return err
	}
	w.logger.Debug("rum rollup worker: no-op (V1 per-beacon upsert handles rollups in WriteEvent)",
		slog.String("site_id", job.Args.SiteID),
		slog.String("bucket_hour", bh.Format(time.RFC3339)))
	return nil
}
