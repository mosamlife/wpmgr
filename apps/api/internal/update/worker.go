package update

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
)

// Audit action names for the update lifecycle.
const (
	ActionRunCreated     = "update.run.created"
	ActionTaskSucceeded  = "update.task.succeeded"
	ActionTaskFailed     = "update.task.failed"
	ActionTaskRolledBack = "update.task.rolled_back"
)

// TaskArgs is the River job payload for one update task. It carries only IDs;
// the worker re-reads authoritative state (tenant-scoped) from the DB.
type TaskArgs struct {
	TenantID uuid.UUID `json:"tenant_id"`
	RunID    uuid.UUID `json:"run_id"`
	TaskID   uuid.UUID `json:"task_id"`
	DryRun   bool      `json:"dry_run"`
}

// Kind implements river.JobArgs.
func (TaskArgs) Kind() string { return "update_task" }

// InsertOpts pins each task to a per-tenant queue so River's per-queue
// MaxWorkers bounds a single tenant's concurrency — one tenant cannot starve
// others. The queue name is derived from the tenant id; the worker pool
// registers a bounded number of these queues (see QueueForTenant).
func (a TaskArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueForTenant(a.TenantID)}
}

// tenantQueueShards is the number of per-tenant queue shards. Each tenant maps
// to exactly one shard; River's MaxWorkers on each shard caps concurrency so a
// burst from one tenant fills only its own shard. A fixed shard count keeps the
// queue set bounded (River needs queues configured at client start).
const tenantQueueShards = 8

// QueueForTenant maps a tenant to its River queue name. Deterministic so the
// enqueuer and the worker pool agree.
func QueueForTenant(tenantID uuid.UUID) string {
	// First byte of the UUID is enough entropy for shard selection.
	shard := int(tenantID[0]) % tenantQueueShards
	return fmt.Sprintf("update_t%d", shard)
}

// QueueNames returns every per-tenant queue shard name (for River client config).
func QueueNames() []string {
	names := make([]string, tenantQueueShards)
	for i := 0; i < tenantQueueShards; i++ {
		names[i] = fmt.Sprintf("update_t%d", i)
	}
	return names
}

// Commander sends signed CP->agent update/rollback commands. siteID is the
// target site's stable enrollment UUID, bound into the command JWT's aud claim
// so a captured token cannot be replayed against a different tenant's site.
type Commander interface {
	Update(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.UpdateRequest) (agentcmd.UpdateResponse, error)
	Rollback(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.RollbackRequest) (agentcmd.RollbackResponse, error)
}

// HealthProber probes a site's homepage for post-update health.
type HealthProber interface {
	Get(ctx context.Context, targetURL string) (agentcmd.ProbeResult, error)
}

// Worker executes one update task end-to-end.
type Worker struct {
	river.WorkerDefaults[TaskArgs]
	repo   Repo
	sites  SiteLookup
	cmd    Commander
	prober HealthProber
	hub    *Hub
	audit  *audit.Recorder
	logger *slog.Logger
	// perTenantLimit bounds concurrent running tasks per tenant as a
	// belt-and-suspenders guard alongside the per-tenant queue sharding. When the
	// limit is reached the job snoozes and retries shortly.
	perTenantLimit int
	// refresher enqueues a CP->agent inventory-refresh job after each task
	// reaches a terminal state (debounced per site). Optional: a nil refresher
	// keeps the legacy behaviour (no post-update refresh).
	refresher   RefreshEnqueuer
	refreshSkip *RefreshDebouncer
	// probeDelays overrides the default post-update health-probe backoff
	// schedule (probeRetryDelays). Nil uses the default. Tests set this to a
	// tiny schedule so the retry loop does not actually sleep ~21s.
	probeDelays []time.Duration
}

// probeRetryDelays is the backoff schedule between post-update health-probe
// attempts (see probeHealthWithRetry). A plugin/theme update — particularly a
// major-version upgrade that runs a synchronous DB migration on activation
// (e.g. SureMail 1.x -> 2.0.0) — can legitimately return a transient 503, or
// be briefly unreachable, for several seconds while the migration runs. A
// single immediate probe misclassifies that as a broken update and rolls back
// a site that in fact updated successfully. Total worst-case retry window:
// sum(probeRetryDelays) ~= 21s, on top of the first (immediate) attempt.
var probeRetryDelays = []time.Duration{
	3 * time.Second,
	4 * time.Second,
	6 * time.Second,
	8 * time.Second,
}

// SetProbeRetryDelays overrides the post-update health-probe backoff
// schedule. Exposed for tests so the retry loop does not actually sleep the
// production ~21s window; production code should rely on the default
// (probeRetryDelays). Passing nil restores the default; pass a non-nil empty
// slice ([]time.Duration{}) to disable retries entirely (single probe).
func (w *Worker) SetProbeRetryDelays(delays []time.Duration) {
	w.probeDelays = delays
}

// NewWorker builds the update task worker.
func NewWorker(repo Repo, sites SiteLookup, cmd Commander, prober HealthProber, hub *Hub, rec *audit.Recorder, logger *slog.Logger, perTenantLimit int) *Worker {
	if perTenantLimit <= 0 {
		perTenantLimit = 5
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{repo: repo, sites: sites, cmd: cmd, prober: prober, hub: hub, audit: rec, logger: logger, perTenantLimit: perTenantLimit}
}

// SetRefreshEnqueuer wires the post-update inventory-refresh enqueuer + its
// per-site debouncer. Call once at boot after the River client is up. A nil
// refresher disables the post-update refresh entirely.
func (w *Worker) SetRefreshEnqueuer(r RefreshEnqueuer, d *RefreshDebouncer) {
	w.refresher = r
	w.refreshSkip = d
}

// Work runs one update task. It is idempotent-ish: a task already in a terminal
// state is skipped. On a transient infrastructure error it returns the error so
// River retries; per-item update failures are recorded as terminal task states
// (not job errors).
func (w *Worker) Work(ctx context.Context, job *river.Job[TaskArgs]) error {
	a := job.Args

	task, err := w.repo.GetTask(ctx, a.TenantID, a.TaskID)
	if err != nil {
		return err
	}
	if terminal(task.Status) {
		return nil // already finished (retry/dup); nothing to do.
	}

	// Per-tenant parallelism guard: if too many of this tenant's tasks are
	// already running, snooze and let River retry shortly. Best-effort (a small
	// race window is acceptable; the queue sharding is the primary bound).
	if running, cerr := w.repo.CountRunningTasksForTenant(ctx, a.TenantID); cerr == nil {
		if int(running) >= w.perTenantLimit {
			return river.JobSnooze(2 * time.Second)
		}
	}

	site, err := w.sites.GetSiteInfo(ctx, a.TenantID, task.SiteID)
	if err != nil {
		// Site gone/unresolvable: terminal failure for this task.
		return w.finish(ctx, task, TaskFailed, task.FromVersion, "", "site unresolved", err.Error())
	}

	running, err := w.repo.MarkTaskRunning(ctx, a.TenantID, a.TaskID)
	if err != nil {
		return err
	}
	w.publish(running, RunRunning)
	w.ensureRunRunning(ctx, a.TenantID, a.RunID)

	item := agentcmd.UpdateItem{Type: task.TargetType, Slug: task.TargetSlug, Version: task.DesiredVersion}

	if a.DryRun {
		return w.runDry(ctx, task, site.URL, item)
	}
	return w.runApply(ctx, task, site.URL, item)
}

// runDry asks the agent what WOULD change without mutating the site.
func (w *Worker) runDry(ctx context.Context, task Task, siteURL string, item agentcmd.UpdateItem) error {
	resp, err := w.cmd.Update(ctx, task.SiteID, siteURL, agentcmd.UpdateRequest{DryRun: true, Snapshot: false, Items: []agentcmd.UpdateItem{item}})
	if err != nil {
		return w.finish(ctx, task, TaskFailed, task.FromVersion, "", "dry-run command failed", err.Error())
	}
	res := firstResult(resp.Results)
	detail := "no change"
	status := TaskSucceeded
	if res.Status == agentcmd.ItemWouldUpdate {
		detail = fmt.Sprintf("would update %s -> %s", res.FromVersion, res.ToVersion)
	} else if res.Status == agentcmd.ItemUpToDate {
		detail = "already up to date"
	}
	return w.finish(ctx, task, status, res.FromVersion, res.ToVersion, detail, "")
}

// runApply executes the real update: snapshot + apply, then health-probe and
// auto-rollback on a broken site.
func (w *Worker) runApply(ctx context.Context, task Task, siteURL string, item agentcmd.UpdateItem) error {
	resp, err := w.cmd.Update(ctx, task.SiteID, siteURL, agentcmd.UpdateRequest{DryRun: false, Snapshot: true, Items: []agentcmd.UpdateItem{item}})
	if err != nil {
		return w.finish(ctx, task, TaskFailed, task.FromVersion, "", "update command failed", err.Error())
	}
	res := firstResult(resp.Results)

	if res.Status == agentcmd.ItemFailed {
		return w.finish(ctx, task, TaskFailed, fromOr(res.FromVersion, task.FromVersion), res.ToVersion, "agent reported update failure", res.Log)
	}
	if res.Status == agentcmd.ItemUpToDate || res.Status == agentcmd.ItemSkipped {
		return w.finish(ctx, task, TaskSkipped, fromOr(res.FromVersion, task.FromVersion), res.ToVersion, "already up to date", "")
	}

	// Post-update health probe of the site homepage, retried with backoff
	// before declaring the update unhealthy (see probeHealthWithRetry): a
	// normal DB-migration 503 immediately after activation must not trigger an
	// unnecessary rollback of a site that in fact updated successfully.
	probe, perr, attempts := w.probeHealthWithRetry(ctx, siteURL)
	if perr != nil {
		// Still unreachable after retrying — treat as unhealthy and roll back.
		return w.rollback(ctx, task, siteURL, item, res, probe, fmt.Sprintf("post-update probe error after %d attempt(s): %v", attempts, perr))
	}
	if !probe.Healthy() {
		return w.rollback(ctx, task, siteURL, item, res, probe, fmt.Sprintf("post-update health failed after %d attempt(s): status=%d %s", attempts, probe.StatusCode, probe.Detail))
	}

	return w.finish(ctx, task, TaskSucceeded, fromOr(res.FromVersion, task.FromVersion), res.ToVersion, "updated and healthy", "")
}

// probeHealthWithRetry probes siteURL and, if the site is unhealthy or
// unreachable, retries with backoff (probeDelays, defaulting to
// probeRetryDelays) before giving up. It returns as soon as any attempt
// reports Healthy(); otherwise it returns the LAST probe result/error after
// exhausting the schedule, plus the number of attempts made (folded into the
// rollback reason so an operator can see the update was given a fair chance).
//
// A transient transport error (perr != nil) is retried the same as an
// unhealthy-but-reachable response: during a post-update migration window a
// momentary connect/timeout error is indistinguishable from — and no less
// transient than — a 503. A permanent transport error (e.g. SSRF-blocked or
// an invalid URL) will simply fail the same way on every attempt and still
// exhausts within the same bounded window, so no separate classification is
// required to stay conservative here.
func (w *Worker) probeHealthWithRetry(ctx context.Context, siteURL string) (probe agentcmd.ProbeResult, perr error, attempts int) {
	delays := probeRetryDelays
	if w.probeDelays != nil {
		delays = w.probeDelays
	}
	for i := 0; ; i++ {
		attempts = i + 1
		probe, perr = w.prober.Get(ctx, siteURL)
		if perr == nil && probe.Healthy() {
			return probe, nil, attempts
		}
		if i >= len(delays) {
			return probe, perr, attempts
		}
		select {
		case <-ctx.Done():
			return probe, ctx.Err(), attempts
		case <-time.After(delays[i]):
		}
	}
}

// rollback issues the signed rollback command and records the rolled_back
// state. probe is the ProbeResult that triggered the rollback decision (the
// zero value when rollback was reached via a probe TRANSPORT error rather
// than a reachable-but-unhealthy response) — it is used only to classify a
// rollback-transport failure below (GH #210).
func (w *Worker) rollback(ctx context.Context, task Task, siteURL string, item agentcmd.UpdateItem, res agentcmd.ItemResult, probe agentcmd.ProbeResult, reason string) error {
	from := fromOr(res.FromVersion, task.FromVersion)
	_, rbErr := w.cmd.Rollback(ctx, task.SiteID, siteURL, agentcmd.RollbackRequest{
		Type:       item.Type,
		Slug:       item.Slug,
		SnapshotID: res.SnapshotID,
		ToVersion:  from,
	})
	if rbErr != nil {
		// Rollback itself failed: this is the worst case. Record as failed with
		// both the health reason and the rollback error so the operator is alerted.
		detail := "rollback FAILED after unhealthy update: " + reason
		// GH #210: when the post-update probe ITSELF detected a site-wide PHP
		// fatal (a fatal-error body signature, or a 5xx response) AND the
		// rollback command's transport also errored, the site is very likely
		// serving a site-wide PHP fatal that makes the agent's own REST
		// endpoint undeliverable — a distinct, more actionable failure mode
		// than a generic "rollback failed" (which could also mean e.g. a
		// transient network blip unrelated to the update). Record a distinct
		// detail so the operator knows the automatic filesystem-level
		// recovery on the agent side is the remaining recovery path.
		if probe.Fatal || probe.StatusCode >= 500 {
			detail = "site not responding: site-wide PHP fatal after update; rollback command undeliverable. " +
				"The agent update watchdog will attempt automatic filesystem recovery; if it cannot, manual filesystem recovery is required."
		}
		return w.finish(ctx, task, TaskFailed, from, res.ToVersion, detail, rbErr.Error())
	}
	return w.finish(ctx, task, TaskRolledBack, from, from, "rolled back: "+reason, "")
}

// finish records a terminal task state, publishes it, records an audit event,
// and completes the run if this was the last outstanding task. It returns nil
// (the task reached a terminal state; the River job succeeds).
func (w *Worker) finish(ctx context.Context, task Task, status, fromVersion, toVersion, detail, errMsg string) error {
	finished, err := w.repo.FinishTask(ctx, FinishTaskInput{
		TenantID:    task.TenantID,
		TaskID:      task.ID,
		Status:      status,
		FromVersion: fromVersion,
		ToVersion:   toVersion,
		Detail:      detail,
		Error:       errMsg,
	})
	if err != nil {
		return err
	}

	runStatus := RunRunning
	if done := w.maybeCompleteRun(ctx, task.TenantID, task.RunID); done {
		runStatus = RunCompleted
	}
	w.publish(finished, runStatus)
	w.recordAudit(ctx, finished)

	// Post-update inventory refresh: ask the agent to re-read its plugin/theme
	// inventory and update transients now that the site state has changed. Only
	// fires for state-changing terminals (succeeded/rolled_back); skipped/failed
	// would not have moved the site forward. Debounced per-site (30s window) so
	// a bulk run does not enqueue N refresh jobs back-to-back.
	w.maybeEnqueueRefresh(ctx, finished)
	return nil
}

// maybeEnqueueRefresh enqueues a refresh-inventory job for the task's site, if
// the refresher is wired and the per-site debouncer allows it. Best-effort: an
// enqueue failure is logged but never bubbled out of Work (the task already
// reached a terminal state).
func (w *Worker) maybeEnqueueRefresh(ctx context.Context, task Task) {
	if w.refresher == nil {
		return
	}
	// Only state-changing outcomes warrant a fresh inventory pull.
	switch task.Status {
	case TaskSucceeded, TaskRolledBack:
	default:
		return
	}
	if w.refreshSkip != nil && !w.refreshSkip.Allow(task.SiteID) {
		return
	}
	site, err := w.sites.GetSiteInfo(ctx, task.TenantID, task.SiteID)
	if err != nil {
		w.logger.Debug("post-update refresh: site lookup failed; skipping",
			slog.String("site_id", task.SiteID.String()), slog.Any("error", err))
		return
	}
	if !site.Enrolled || site.URL == "" {
		return
	}
	if err := w.refresher.EnqueueRefresh(ctx, RefreshInventoryArgs{
		TenantID: task.TenantID,
		SiteID:   task.SiteID,
		SiteURL:  site.URL,
		Source:   "post_update",
	}); err != nil {
		w.logger.Warn("post-update refresh: enqueue failed",
			slog.String("site_id", task.SiteID.String()), slog.Any("error", err))
	}
}

// maybeCompleteRun marks the run completed when no tasks remain unfinished.
func (w *Worker) maybeCompleteRun(ctx context.Context, tenantID, runID uuid.UUID) bool {
	n, err := w.repo.CountUnfinishedTasks(ctx, tenantID, runID)
	if err != nil {
		w.logger.Warn("update: count unfinished tasks", slog.Any("error", err))
		return false
	}
	if n > 0 {
		return false
	}
	if _, err := w.repo.SetRunStatus(ctx, tenantID, runID, RunCompleted); err != nil {
		w.logger.Warn("update: set run completed", slog.Any("error", err))
		return false
	}
	return true
}

// ensureRunRunning transitions a pending run to running on the first task start.
func (w *Worker) ensureRunRunning(ctx context.Context, tenantID, runID uuid.UUID) {
	run, err := w.repo.GetRun(ctx, tenantID, runID)
	if err != nil {
		return
	}
	if run.Status == RunPending {
		if _, err := w.repo.SetRunStatus(ctx, tenantID, runID, RunRunning); err != nil {
			w.logger.Warn("update: set run running", slog.Any("error", err))
		}
	}
}

func (w *Worker) publish(task Task, runStatus string) {
	if w.hub == nil {
		return
	}
	w.hub.Publish(Event{
		RunID:       task.RunID,
		TaskID:      task.ID,
		SiteID:      task.SiteID,
		TargetType:  task.TargetType,
		TargetSlug:  task.TargetSlug,
		Status:      task.Status,
		FromVersion: task.FromVersion,
		ToVersion:   task.ToVersion,
		Detail:      task.Detail,
		RunStatus:   runStatus,
	})
}

func (w *Worker) recordAudit(ctx context.Context, task Task) {
	if w.audit == nil {
		return
	}
	var action string
	switch task.Status {
	case TaskSucceeded, TaskSkipped:
		action = ActionTaskSucceeded
	case TaskRolledBack:
		action = ActionTaskRolledBack
	default:
		action = ActionTaskFailed
	}
	_, _ = w.audit.Record(ctx, audit.Event{
		TenantID:   task.TenantID,
		ActorType:  audit.ActorSystem,
		Action:     action,
		TargetType: "update_task",
		TargetID:   task.ID.String(),
		Metadata: map[string]any{
			"run_id":       task.RunID.String(),
			"site_id":      task.SiteID.String(),
			"target_type":  task.TargetType,
			"target_slug":  task.TargetSlug,
			"from_version": task.FromVersion,
			"to_version":   task.ToVersion,
			"status":       task.Status,
		},
	})
}

// ----------------------------------------------------------------------------
// ReapStaleTasksArgs — periodic stale-task reaper sweep (#131 follow-up)
// ----------------------------------------------------------------------------

// ReapStaleTasksArgs is the River job payload for the periodic stale-task
// reaper sweep. No fields — it enumerates stale rows itself.
type ReapStaleTasksArgs struct{}

// Kind implements river.JobArgs.
func (ReapStaleTasksArgs) Kind() string { return "update_task_reaper" }

// staleTaskThreshold is how long a task may sit in pending/running before the
// reaper terminalizes it. Comfortably longer than any real update, including
// the post-update health-probe retry window (worst case ~21s, see
// probeRetryDelays) and the per-tenant parallelism snooze backoff: a worker
// crash between MarkTaskRunning and finish(), or an EnqueueTask failure that
// leaves a task pending (Service.CreateRun's best-effort enqueue after
// CreateRunWithTasks), would otherwise permanently occupy the
// update_tasks_inflight_target_idx slot for that (tenant, site, target) —
// every future update attempt for it 409s targets_in_flight forever (m88).
const staleTaskThreshold = 45 * time.Minute

// staleTaskReapLimit bounds how many stale tasks one sweep reaps, so an
// unbounded backlog cannot make a single periodic tick unbounded; any
// remainder is caught by the next sweep (see the periodic job interval, wired
// in cmd/wpmgr/main.go).
const staleTaskReapLimit = 500

// ReaperWorker is the periodic River job that sweeps update_tasks for rows
// stuck in pending/running past staleTaskThreshold and terminalizes them as
// failed, freeing the (tenant, site, target_type, target_slug) slot the
// update_tasks_inflight_target_idx partial unique index (m88) would otherwise
// hold forever. It wraps the same Worker used to execute update tasks and
// reuses Worker.finish, so a reaped task gets the exact same treatment as a
// task the agent itself finished (publish to the SSE hub + audit record + the
// run-completion check that would otherwise never re-run for an orphaned run
// whose last outstanding task never called finish()).
type ReaperWorker struct {
	river.WorkerDefaults[ReapStaleTasksArgs]
	w *Worker
}

// NewReaperWorker builds the stale-task reaper, backed by the same Worker
// (and therefore the same repo/hub/audit/logger) used to execute update
// tasks.
func NewReaperWorker(w *Worker) *ReaperWorker {
	return &ReaperWorker{w: w}
}

// Work runs one reaper sweep: list stale tasks and terminalize each one.
// Errors are logged and swallowed — the reaper must not fail the periodic
// River job (that would just retry the exact same stale rows sooner).
func (rw *ReaperWorker) Work(ctx context.Context, _ *river.Job[ReapStaleTasksArgs]) error {
	stale, err := rw.w.repo.ListStaleUpdateTasks(ctx, staleTaskThreshold, staleTaskReapLimit)
	if err != nil {
		rw.w.logger.Warn("update task reaper: failed to list stale tasks", slog.Any("error", err))
		return nil
	}
	for _, task := range stale {
		if ferr := rw.w.finish(ctx, task, TaskFailed, task.FromVersion, task.ToVersion,
			"stale: task exceeded max runtime", "reaped by the periodic stale-task sweep after no progress within the threshold"); ferr != nil {
			rw.w.logger.Warn("update task reaper: failed to terminalize stale task",
				slog.String("task_id", task.ID.String()), slog.Any("error", ferr))
			continue
		}
		rw.w.logger.Warn("update task reaper: terminalized stale task",
			slog.String("task_id", task.ID.String()),
			slog.String("run_id", task.RunID.String()),
			slog.String("site_id", task.SiteID.String()),
			slog.String("target", task.TargetType+"/"+task.TargetSlug),
			slog.String("prior_status", task.Status))
	}
	return nil
}

func firstResult(rs []agentcmd.ItemResult) agentcmd.ItemResult {
	if len(rs) == 0 {
		return agentcmd.ItemResult{Status: agentcmd.ItemFailed, Log: "agent returned no item result"}
	}
	return rs[0]
}

func fromOr(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}

var _ = errors.New // reserved for future typed-error handling
