package perf

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// ----------------------------------------------------------------------------
// DBCleanRiverEnqueuer
// ----------------------------------------------------------------------------

// DBCleanRiverEnqueuer enqueues DBCleanArgs onto River. It satisfies
// DBCleanEnqueuer.
type DBCleanRiverEnqueuer struct {
	client *river.Client[pgx.Tx]
}

// NewDBCleanRiverEnqueuer builds an enqueuer around the started River client.
func NewDBCleanRiverEnqueuer(client *river.Client[pgx.Tx]) *DBCleanRiverEnqueuer {
	return &DBCleanRiverEnqueuer{client: client}
}

// EnqueueDBClean inserts one db-clean dispatch job.
func (e *DBCleanRiverEnqueuer) EnqueueDBClean(ctx context.Context, args DBCleanArgs) error {
	if _, err := e.client.Insert(ctx, args, nil); err != nil {
		return fmt.Errorf("enqueue db-clean: %w", err)
	}
	return nil
}

// ----------------------------------------------------------------------------
// DBCleanArgs — per-site db-clean River job
// ----------------------------------------------------------------------------

// DBCleanArgs is the River job payload for one site's database cleanup. It
// carries tenant + site IDs; the worker re-reads authoritative config from the
// DB (so a stale enqueue can never dispatch a misconfigured job).
type DBCleanArgs struct {
	TenantID  uuid.UUID `json:"tenant_id"`
	SiteID    uuid.UUID `json:"site_id"`
	Trigger   string    `json:"trigger"` // "manual" | "scheduled"
	CPBaseURL string    `json:"cp_base_url"`
}

// Kind implements river.JobArgs.
func (DBCleanArgs) Kind() string { return "perf_db_clean" }

// DBCleanWorker sends the db_clean command to the site's agent. The agent ACKs
// immediately; progress flows back via /agent/v1/db-clean/progress.
type DBCleanWorker struct {
	river.WorkerDefaults[DBCleanArgs]
	svc    *Service
	logger *slog.Logger
}

// NewDBCleanWorker builds the db-clean dispatch worker.
func NewDBCleanWorker(svc *Service, logger *slog.Logger) *DBCleanWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &DBCleanWorker{svc: svc, logger: logger}
}

// Work dispatches one db-clean job to the site's agent.
func (w *DBCleanWorker) Work(ctx context.Context, job *river.Job[DBCleanArgs]) error {
	a := job.Args
	trigger := a.Trigger
	if trigger == "" {
		trigger = "scheduled"
	}

	// GH #493 — point-of-action pause re-check. The sweeper already filters
	// paused sites out of its fan-out, but nothing drains River, so a job
	// enqueued in the window between selection and dispatch must ask again.
	//
	// db_clean DELETES ROWS FROM THE CUSTOMER'S LIVE DATABASE, so unlike the
	// vuln rescan's reversible read this one FAILS CLOSED: a pause-check error
	// declines the clean rather than proceeding on an unknown — that half is
	// unconditional and does not change below. What changes is that the error
	// is also returned instead of swallowed, so River retries this job with
	// backoff and the failure surfaces as a failed job rather than a silent
	// success. If every retry hits the same failure the job is eventually
	// discarded, and the site falls back to its already-advanced
	// next_db_clean_at for the next interval.
	//
	// Only the scheduled trigger is gated. Pause governs the schedule, never
	// the operator: a manual clean is the operator, and is never filtered.
	if trigger != "manual" {
		paused, perr := w.svc.repo.IsMonitoringPaused(ctx, a.SiteID)
		if perr != nil {
			w.logger.Warn("db-clean: pause check failed, declining scheduled clean",
				slog.String("site_id", a.SiteID.String()),
				slog.String("tenant_id", a.TenantID.String()),
				slog.Any("error", perr))
			return perr
		}
		if paused {
			w.logger.Info("db-clean: scheduled clean declined, monitoring paused",
				slog.String("site_id", a.SiteID.String()),
				slog.String("tenant_id", a.TenantID.String()))
			return nil
		}
	}

	var (
		jobID string
		err   error
	)
	if trigger == "manual" {
		jobID, err = w.svc.DBClean(ctx, a.TenantID, a.SiteID, a.CPBaseURL)
	} else {
		jobID, err = w.svc.DBCleanScheduled(ctx, a.TenantID, a.SiteID, a.CPBaseURL)
	}
	if err != nil {
		w.logger.Warn("db-clean job failed",
			slog.String("site_id", a.SiteID.String()),
			slog.String("tenant_id", a.TenantID.String()),
			slog.String("trigger", trigger),
			slog.Any("error", err))
		// Return the error so River can retry on transport failures. Semantic
		// refusals (ok=false from agent) are already surfaced via db.clean.failed
		// SSE by the service; returning nil here would hide them from River metrics.
		return err
	}
	w.logger.Info("db-clean dispatched",
		slog.String("job_id", jobID),
		slog.String("site_id", a.SiteID.String()),
		slog.String("tenant_id", a.TenantID.String()),
		slog.String("trigger", trigger))
	return nil
}

// ----------------------------------------------------------------------------
// DBCleanScheduleArgs — periodic sweep River job
// ----------------------------------------------------------------------------

// DBCleanScheduleArgs is the River job payload for the periodic schedule sweep.
// It has no fields; the worker enumerates due sites itself.
type DBCleanScheduleArgs struct{}

// Kind implements river.JobArgs.
func (DBCleanScheduleArgs) Kind() string { return "perf_db_clean_scheduler" }

// DBCleanScheduleWorker is the periodic River job that sweeps site_perf_config
// for sites where db_auto_clean=true and next_db_clean_at IS NULL or past due,
// enqueues a DBCleanArgs River job per site, and advances next_db_clean_at.
// This makes the CP — not agent wp-cron — the owner of the auto-clean schedule
// (Defect 4 fix, M38).
type DBCleanScheduleWorker struct {
	river.WorkerDefaults[DBCleanScheduleArgs]
	svc       *Service
	enqueuer  DBCleanEnqueuer
	cpBaseURL string
	logger    *slog.Logger
}

// DBCleanEnqueuer enqueues a DBCleanArgs River job. The concrete implementation
// uses river.Client[pgx.Tx].Insert; declared as an interface so tests can
// substitute a fake.
type DBCleanEnqueuer interface {
	EnqueueDBClean(ctx context.Context, args DBCleanArgs) error
}

// NewDBCleanScheduleWorker builds the schedule-sweep worker. enqueuer must be
// wired after River starts (via SetEnqueuer). cpBaseURL is the CP origin passed
// to the dispatched job's progress_endpoint.
func NewDBCleanScheduleWorker(svc *Service, logger *slog.Logger) *DBCleanScheduleWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &DBCleanScheduleWorker{svc: svc, logger: logger}
}

// SetEnqueuer wires the River enqueuer after the client has started.
func (w *DBCleanScheduleWorker) SetEnqueuer(e DBCleanEnqueuer, cpBaseURL string) {
	w.enqueuer = e
	w.cpBaseURL = cpBaseURL
}

// Work runs one sweep: fetch due sites, enqueue a dispatch job per site, and
// advance next_db_clean_at. Each site is processed independently; an error on
// one site does not skip the rest.
func (w *DBCleanScheduleWorker) Work(ctx context.Context, _ *river.Job[DBCleanScheduleArgs]) error {
	if w.enqueuer == nil {
		w.logger.Warn("db-clean schedule worker: enqueuer not wired; skipping sweep")
		return nil
	}

	due, err := w.svc.repo.GetDueDBCleanSites(ctx, 200)
	if err != nil {
		return err
	}
	if len(due) == 0 {
		return nil
	}

	// GH #493 — selection-time pause filter. One round trip for the whole
	// batch. On error the sweep declines entirely — no site in this batch is
	// enqueued and none has next_db_clean_at advanced — rather than cleaning
	// through an unknown pause state: db_clean deletes rows from the
	// customer's live database. The dispatcher re-checks per site at fire
	// time regardless.
	//
	// The error is also returned instead of swallowed, so River retries this
	// sweep job with backoff and the failure surfaces as a failed job rather
	// than a silent success. Nothing here advanced any site's schedule, so
	// the next periodic tick (every 5 min) re-selects the same due sites
	// independently of that retry.
	paused, perr := w.svc.repo.PausedSiteIDs(ctx, dueSiteIDs(due))
	if perr != nil {
		w.logger.Warn("db-clean schedule: pause lookup failed, declining this sweep",
			slog.Int("due", len(due)), slog.Any("error", perr))
		return perr
	}

	dispatched, skipped := 0, 0
	for _, s := range due {
		// Advance next_db_clean_at BEFORE enqueuing so a crash after Enqueue does not
		// create duplicate runs. The window is bounded by the sweep interval (5 min).
		nextAt := nextCleanTime(s.DBAutoCleanInterval)
		if advErr := w.svc.repo.UpdateNextDBCleanAt(ctx, s.SiteID, nextAt); advErr != nil {
			w.logger.Warn("db-clean schedule: failed to advance next_db_clean_at",
				slog.String("site_id", s.SiteID.String()),
				slog.Any("error", advErr))
			continue
		}
		// GH #493 — a paused site is skipped AFTER its schedule is advanced,
		// not before. Skipping without advancing would leave it permanently
		// due, and a fleet with many paused sites would then fill the limit=200
		// selection window and starve the active sites behind it. Advancing
		// also keeps the cadence, so a site paused for three weeks does not
		// clean the instant it resumes.
		if paused[s.SiteID] {
			skipped++
			w.logger.Info("db-clean schedule: site skipped, monitoring paused",
				slog.String("site_id", s.SiteID.String()),
				slog.String("tenant_id", s.TenantID.String()))
			continue
		}
		if enqErr := w.enqueuer.EnqueueDBClean(ctx, DBCleanArgs{
			TenantID:  s.TenantID,
			SiteID:    s.SiteID,
			Trigger:   "scheduled",
			CPBaseURL: w.cpBaseURL,
		}); enqErr != nil {
			w.logger.Warn("db-clean schedule: failed to enqueue dispatch",
				slog.String("site_id", s.SiteID.String()),
				slog.Any("error", enqErr))
			// Roll next_db_clean_at back to now so the next 5-minute sweep retries,
			// rather than silently skipping this site for a whole interval. Best-effort:
			// if the rollback itself fails the site just waits one interval (the prior
			// behaviour), which is acceptable.
			if rbErr := w.svc.repo.UpdateNextDBCleanAt(ctx, s.SiteID, time.Now().UTC()); rbErr != nil {
				w.logger.Warn("db-clean schedule: failed to roll back next_db_clean_at after enqueue failure",
					slog.String("site_id", s.SiteID.String()),
					slog.Any("error", rbErr))
			}
			continue
		}
		dispatched++
	}

	if dispatched > 0 || skipped > 0 {
		w.logger.Info("db-clean schedule sweep",
			slog.Int("due", len(due)),
			slog.Int("dispatched", dispatched),
			slog.Int("skipped_paused", skipped))
	}
	return nil
}

// dueSiteIDs projects the sweep's due list down to its site ids, for the
// single batched pause lookup.
func dueSiteIDs(due []DueDBCleanSite) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(due))
	for _, s := range due {
		ids = append(ids, s.SiteID)
	}
	return ids
}

// nextCleanTime returns the next scheduled run time based on the interval string.
// Interval strings mirror the backup scheduler: "daily", "weekly", "monthly".
func nextCleanTime(interval string) time.Time {
	now := time.Now().UTC()
	switch interval {
	case "daily":
		return now.Add(24 * time.Hour)
	case "monthly":
		return now.AddDate(0, 1, 0)
	default: // "weekly" and any unrecognised value
		return now.Add(7 * 24 * time.Hour)
	}
}

// ----------------------------------------------------------------------------
// DBSizeHistoryGCWorker — periodic GC for site_db_size_history (M42)
// ----------------------------------------------------------------------------

// DBSizeHistoryGCArgs is the River job payload for the periodic size-history
// GC. No fields — the worker uses a fixed 120-day retention.
type DBSizeHistoryGCArgs struct{}

// Kind implements river.JobArgs.
func (DBSizeHistoryGCArgs) Kind() string { return "perf_db_size_history_gc" }

// DBSizeHistoryGCWorker deletes site_db_size_history rows older than 120 days.
// It runs cross-tenant under InAgentTx (app.agent = 'on'), matching the
// php_errors_retention_gc and backup_retention_gc patterns exactly.
type DBSizeHistoryGCWorker struct {
	river.WorkerDefaults[DBSizeHistoryGCArgs]
	repo   *Repo
	logger *slog.Logger
}

// NewDBSizeHistoryGCWorker builds the GC worker.
func NewDBSizeHistoryGCWorker(repo *Repo, logger *slog.Logger) *DBSizeHistoryGCWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &DBSizeHistoryGCWorker{repo: repo, logger: logger}
}

// Work deletes rows older than 120 days in a single cross-tenant pass.
func (w *DBSizeHistoryGCWorker) Work(ctx context.Context, _ *river.Job[DBSizeHistoryGCArgs]) error {
	deleted, err := w.repo.PruneDBSizeHistory(ctx, 120*24*time.Hour)
	if err != nil {
		w.logger.Warn("db size history GC error", slog.Any("error", err))
		return err
	}
	if deleted > 0 {
		w.logger.Info("db size history GC", slog.Int64("rows_deleted", deleted))
	}
	return nil
}

// ----------------------------------------------------------------------------
// CacheHitRatioHistoryGCWorker — M52 / #162
// ----------------------------------------------------------------------------

// CacheHitRatioHistoryGCArgs is the River job-args type for the cache
// hit-ratio history GC worker.
type CacheHitRatioHistoryGCArgs struct{}

// Kind implements river.JobArgs.
func (CacheHitRatioHistoryGCArgs) Kind() string { return "perf_cache_hit_ratio_history_gc" }

// CacheHitRatioHistoryGCWorker deletes site_cache_hit_ratio_history rows older
// than 120 days. It runs cross-tenant under InAgentTx (app.agent = 'on'),
// matching the DBSizeHistoryGCWorker pattern exactly.
type CacheHitRatioHistoryGCWorker struct {
	river.WorkerDefaults[CacheHitRatioHistoryGCArgs]
	repo   *Repo
	logger *slog.Logger
}

// NewCacheHitRatioHistoryGCWorker builds the GC worker.
func NewCacheHitRatioHistoryGCWorker(repo *Repo, logger *slog.Logger) *CacheHitRatioHistoryGCWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &CacheHitRatioHistoryGCWorker{repo: repo, logger: logger}
}

// Work deletes rows older than 120 days in a single cross-tenant pass.
func (w *CacheHitRatioHistoryGCWorker) Work(ctx context.Context, _ *river.Job[CacheHitRatioHistoryGCArgs]) error {
	deleted, err := w.repo.PruneCacheHitRatioHistory(ctx, 120*24*time.Hour)
	if err != nil {
		w.logger.Warn("cache hit-ratio history GC error", slog.Any("error", err))
		return err
	}
	if deleted > 0 {
		w.logger.Info("cache hit-ratio history GC", slog.Int64("rows_deleted", deleted))
	}
	return nil
}

// ----------------------------------------------------------------------------
// SSE publish helper for the worker layer (no DB access needed)
// ----------------------------------------------------------------------------

// PublishDBCleanFailed emits db.clean.failed for a job that the worker
// could not dispatch (e.g. site not enrolled). The service's publish method
// is not accessible from the worker layer directly; this shim calls the event
// publisher the service wraps.
func (w *DBCleanWorker) publishFailed(ctx context.Context, tenantID, siteID uuid.UUID, jobID, detail string) {
	if w.svc.events == nil {
		return
	}
	_ = w.svc.events.Publish(ctx, site.ConnectionEvent{
		Type:     site.EventDbCleanFailed,
		TenantID: tenantID,
		SiteID:   siteID,
		Data: map[string]any{
			"job_id": jobID,
			"detail": detail,
		},
	})
}

// ----------------------------------------------------------------------------
// DBCleanWatchdogArgs — periodic stall-detection sweep (M39)
// ----------------------------------------------------------------------------

// DBCleanWatchdogArgs is the River job payload for the watchdog periodic sweep.
// No fields — it enumerates stalled rows itself.
type DBCleanWatchdogArgs struct{}

// Kind implements river.JobArgs.
func (DBCleanWatchdogArgs) Kind() string { return "perf_db_clean_watchdog" }

// cleanWatchdogThreshold is the stall window for db_clean jobs. A category that
// takes 5 minutes to run still gets one progress frame before the 10-minute mark.
const cleanWatchdogThreshold = 10 * time.Minute

// scanWatchdogThreshold is the stall window for db_scan jobs. The scan is
// synchronous READ-only, so a 3-minute window covers CP restarts mid-flight.
const scanWatchdogThreshold = 3 * time.Minute

// DBCleanWatchdogWorker is the periodic River job that sweeps site_perf_config
// for stalled in-flight db_clean and db_scan jobs, clears the watchdog columns,
// and emits db.clean.failed / db.scan.failed SSE to un-stick the UI.
type DBCleanWatchdogWorker struct {
	river.WorkerDefaults[DBCleanWatchdogArgs]
	svc    *Service
	logger *slog.Logger
}

// NewDBCleanWatchdogWorker builds the watchdog worker.
func NewDBCleanWatchdogWorker(svc *Service, logger *slog.Logger) *DBCleanWatchdogWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &DBCleanWatchdogWorker{svc: svc, logger: logger}
}

// Work runs one sweep: detect stalled db_clean + db_scan jobs, clear their
// watchdog columns, and emit the appropriate failed SSE events.
func (w *DBCleanWatchdogWorker) Work(ctx context.Context, _ *river.Job[DBCleanWatchdogArgs]) error {
	w.sweepClean(ctx)
	w.sweepScan(ctx)
	return nil
}

func (w *DBCleanWatchdogWorker) sweepClean(ctx context.Context) {
	stalled, err := w.svc.repo.GetStalledDBCleanJobs(ctx, cleanWatchdogThreshold)
	if err != nil {
		w.logger.Warn("db-clean watchdog: failed to query stalled clean jobs", slog.Any("error", err))
		return
	}
	for _, s := range stalled {
		if clrErr := w.svc.repo.ClearActiveDBCleanJob(ctx, s.SiteID); clrErr != nil {
			w.logger.Warn("db-clean watchdog: failed to clear stalled clean job",
				slog.String("site_id", s.SiteID.String()), slog.Any("error", clrErr))
		}
		w.svc.publish(ctx, s.TenantID, s.SiteID, site.EventDbCleanFailed, map[string]any{
			"job_id": s.JobID,
			"detail": "stalled — no progress for >10 minutes",
		})
		w.logger.Warn("db-clean watchdog: marked stalled clean job failed",
			slog.String("site_id", s.SiteID.String()),
			slog.String("job_id", s.JobID))
	}
}

func (w *DBCleanWatchdogWorker) sweepScan(ctx context.Context) {
	stalled, err := w.svc.repo.GetStalledDBScanJobs(ctx, scanWatchdogThreshold)
	if err != nil {
		w.logger.Warn("db-clean watchdog: failed to query stalled scan jobs", slog.Any("error", err))
		return
	}
	for _, s := range stalled {
		if clrErr := w.svc.repo.ClearActiveDBScanJob(ctx, s.SiteID); clrErr != nil {
			w.logger.Warn("db-clean watchdog: failed to clear stalled scan job",
				slog.String("site_id", s.SiteID.String()), slog.Any("error", clrErr))
		}
		w.svc.publish(ctx, s.TenantID, s.SiteID, site.EventDbScanFailed, map[string]any{
			"job_id": s.JobID,
			"detail": "stalled — no result within timeout",
		})
		w.logger.Warn("db-clean watchdog: marked stalled scan job failed",
			slog.String("site_id", s.SiteID.String()),
			slog.String("job_id", s.JobID))
	}
}

// ----------------------------------------------------------------------------
// DBOrphanDeleteWatchdogArgs — periodic stall-detection sweep (P3.8)
// ----------------------------------------------------------------------------

// DBOrphanDeleteWatchdogArgs is the River job payload for the orphan-delete
// watchdog periodic sweep. No fields — it enumerates stalled rows itself.
type DBOrphanDeleteWatchdogArgs struct{}

// Kind implements river.JobArgs.
func (DBOrphanDeleteWatchdogArgs) Kind() string { return "db_orphan_delete_watchdog" }

// orphanDeleteWatchdogThreshold is the stall window for db_orphan_delete jobs.
// 5 minutes is shorter than db_clean's 10-minute threshold because orphan-delete
// operates on at most 500 small items.
const orphanDeleteWatchdogThreshold = 5 * time.Minute

// DBOrphanDeleteWatchdogWorker is the periodic River job that sweeps
// site_perf_config for stalled in-flight db_orphan_delete jobs, clears the
// watchdog columns, and emits db.orphan.delete.failed SSE to un-stick the UI.
type DBOrphanDeleteWatchdogWorker struct {
	river.WorkerDefaults[DBOrphanDeleteWatchdogArgs]
	svc    *Service
	logger *slog.Logger
}

// NewDBOrphanDeleteWatchdogWorker builds the orphan-delete watchdog worker.
func NewDBOrphanDeleteWatchdogWorker(svc *Service, logger *slog.Logger) *DBOrphanDeleteWatchdogWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &DBOrphanDeleteWatchdogWorker{svc: svc, logger: logger}
}

// Work runs one sweep: detect stalled db_orphan_delete jobs, clear their
// watchdog columns, and emit db.orphan.delete.failed SSE events.
func (w *DBOrphanDeleteWatchdogWorker) Work(ctx context.Context, _ *river.Job[DBOrphanDeleteWatchdogArgs]) error {
	stalled, err := w.svc.repo.GetStalledDBOrphanDeleteJobs(ctx, orphanDeleteWatchdogThreshold)
	if err != nil {
		w.logger.Warn("db-orphan-delete watchdog: failed to query stalled jobs", slog.Any("error", err))
		return nil // swallow — watchdog must not break the River periodic job
	}
	for _, s := range stalled {
		if clrErr := w.svc.repo.ClearActiveDBOrphanDeleteJob(ctx, s.SiteID); clrErr != nil {
			w.logger.Warn("db-orphan-delete watchdog: failed to clear stalled job",
				slog.String("site_id", s.SiteID.String()), slog.Any("error", clrErr))
		}
		w.svc.publish(ctx, s.TenantID, s.SiteID, site.EventDbOrphanDeleteFailed, map[string]any{
			"job_id":  s.JobID,
			"site_id": s.SiteID.String(),
			"detail":  "watchdog: job stalled",
		})
		w.logger.Warn("db-orphan-delete watchdog: marked stalled job failed",
			slog.String("site_id", s.SiteID.String()),
			slog.String("job_id", s.JobID))
	}
	return nil
}

// ----------------------------------------------------------------------------
// RumBeaconReconcileArgs — ack-based beacon-key re-mint (GH #174)
// ----------------------------------------------------------------------------

// RumBeaconReconcileArgs is the River job payload for one site's RUM
// beacon-key reconcile. Enqueued from Service.MarkConfigApplied when a
// config-ack reports rum_beacon_present=false on a site that is rum-enabled
// with a hash already minted — i.e. the CP believes the agent should have a
// key but the agent says it does not (the exact "the one best-effort
// mint+push was lost" stuck state GH #174 describes). The worker calls the
// SAME RotateBeaconKey primitive the operator rotate-key endpoint uses, so a
// transiently-down agent self-heals on its next successful config-ack without
// any operator action.
type RumBeaconReconcileArgs struct {
	TenantID uuid.UUID `json:"tenant_id"`
	SiteID   uuid.UUID `json:"site_id"`
}

// Kind implements river.JobArgs.
func (RumBeaconReconcileArgs) Kind() string { return "perf_rum_beacon_reconcile" }

// rumBeaconReconcileUniqueWindow collapses back-to-back acks from the same
// site's cron heartbeat into a single in-flight rotation, so a chatty or
// mis-signaling agent cannot spam rotate-and-push in a tight loop.
const rumBeaconReconcileUniqueWindow = 5 * time.Minute

// RumBeaconReconcileRiverEnqueuer enqueues RumBeaconReconcileArgs onto River.
// It satisfies perf.RumBeaconReconcileEnqueuer.
type RumBeaconReconcileRiverEnqueuer struct {
	client *river.Client[pgx.Tx]
}

// NewRumBeaconReconcileRiverEnqueuer builds an enqueuer around the started
// River client.
func NewRumBeaconReconcileRiverEnqueuer(client *river.Client[pgx.Tx]) *RumBeaconReconcileRiverEnqueuer {
	return &RumBeaconReconcileRiverEnqueuer{client: client}
}

// EnqueueRumBeaconReconcile inserts one reconcile job, deduplicated per site
// within rumBeaconReconcileUniqueWindow.
func (e *RumBeaconReconcileRiverEnqueuer) EnqueueRumBeaconReconcile(ctx context.Context, args RumBeaconReconcileArgs) error {
	if _, err := e.client.Insert(ctx, args, &river.InsertOpts{
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByPeriod: rumBeaconReconcileUniqueWindow,
		},
	}); err != nil {
		return fmt.Errorf("enqueue rum beacon reconcile: %w", err)
	}
	return nil
}

// RumBeaconReconcileWorker calls Service.RotateBeaconKey to unconditionally
// re-mint + push a fresh RUM beacon key for one site (GH #174 self-heal).
type RumBeaconReconcileWorker struct {
	river.WorkerDefaults[RumBeaconReconcileArgs]
	svc    *Service
	logger *slog.Logger
}

// NewRumBeaconReconcileWorker builds the reconcile worker.
func NewRumBeaconReconcileWorker(svc *Service, logger *slog.Logger) *RumBeaconReconcileWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &RumBeaconReconcileWorker{svc: svc, logger: logger}
}

// Work rotates the beacon key for one site. Returns the error so River
// retries on a transient failure (e.g. the agent is still unreachable) — the
// whole point of this worker is to keep trying until a push actually lands.
func (w *RumBeaconReconcileWorker) Work(ctx context.Context, job *river.Job[RumBeaconReconcileArgs]) error {
	a := job.Args
	if err := w.svc.RotateBeaconKey(ctx, a.TenantID, a.SiteID); err != nil {
		w.logger.Warn("rum beacon reconcile failed",
			slog.String("site_id", a.SiteID.String()),
			slog.String("tenant_id", a.TenantID.String()),
			slog.Any("error", err))
		return err
	}
	w.logger.Info("rum beacon reconcile: rotated + pushed",
		slog.String("site_id", a.SiteID.String()),
		slog.String("tenant_id", a.TenantID.String()))
	return nil
}
