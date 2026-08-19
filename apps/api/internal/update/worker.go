package update

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentplugin"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
)

// Audit action names for the update lifecycle.
const (
	ActionRunCreated = "update.run.created"
	// ActionRunRetried records a retry (GH #336) against the SOURCE run, so the
	// run that failed carries the evidence that someone acted on it. The new run
	// it produced is in the metadata, alongside the requested/created counts.
	ActionRunRetried     = "update.run.retried"
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
	// claimStaleAfter is the staleness bound handed to the claim CAS,
	// derived from this worker's own configured job timeout at construction
	// (see ClaimStaleAfter). A field, not a constant, because the budget it
	// must exceed is config-driven.
	claimStaleAfter time.Duration
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
	// busyBackoff overrides siteBusyBackoff's default jittered schedule. Nil
	// uses the default. Tests set this to a tiny fixed delay (see
	// SetSiteBusyBackoff) so a busy-site retry test does not actually sleep
	// the production 5s-60s window.
	busyBackoff func(Task) time.Duration
	// agent holds the agent self-update channel's dependencies and its
	// fleet-wide kill switch. The ZERO VALUE IS DISABLED: a deployment that
	// never calls SetAgentSelfUpdate never sends an agent self-update command
	// to any site.
	agent AgentSelfUpdateDeps
	// jobTimeout overrides River's default 60s per-job context deadline (see
	// DeriveApplyJobTimeout, and NewBackupWorker's identical jobTimeout field for
	// the same pattern applied to backups). runApply makes one apply/rollback
	// command round trip plus the full GH #291 Phase 4 post-update health check
	// (the agent-first reachability ladder and the public probe ladder, both of
	// which can retry with backoff), all inside the same River job, comfortably
	// longer than the 60s default. Zero falls back to river.Config.JobTimeout.
	jobTimeout time.Duration
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

// NewWorker builds the update task worker. jobTimeout overrides River's
// default 60s per-job deadline; pass update.DeriveApplyJobTimeout(cfg.Update.
// ApplyHTTPTimeout, cfg.Update.HTTPTimeout) (see that function's doc comment
// for the arithmetic). Zero keeps River's default.
func NewWorker(repo Repo, sites SiteLookup, cmd Commander, prober HealthProber, hub *Hub, rec *audit.Recorder, logger *slog.Logger, perTenantLimit int, jobTimeout time.Duration) *Worker {
	if perTenantLimit <= 0 {
		perTenantLimit = 5
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{repo: repo, sites: sites, cmd: cmd, prober: prober, hub: hub, audit: rec, logger: logger, perTenantLimit: perTenantLimit, jobTimeout: jobTimeout, claimStaleAfter: ClaimStaleAfter(jobTimeout)}
}

// Timeout overrides River's default per-job context deadline (60s) for the
// update-task worker, the same way BackupWorker.Timeout does. Returning a
// positive duration makes River use it instead of river.Config.JobTimeout;
// returning 0 keeps the default (see cmp.Or in river's job executor, which
// falls through to river.Config.JobTimeout, itself defaulting to River's own
// 60s river.JobTimeoutDefault when that is also unset, exactly as it is in
// this codebase's river.Config today). River documents that returning -1
// disables the deadline entirely; we intentionally do NOT do that: a wedged
// update task must eventually error out so River can retry/reap it.
func (w *Worker) Timeout(*river.Job[TaskArgs]) time.Duration { return w.jobTimeout }

// DeriveApplyJobTimeout computes the River job-level Timeout() budget for the
// update-apply worker (see Worker.Timeout and NewWorker). It reads the actual
// ladder schedules (probeRetryDelays, agentVerifyTimeout) rather than assuming
// them, so this stays correct if either is ever tuned.
//
// runApply's worst-case wall-clock budget, in the order the work happens:
//
//  1. applyHTTPTimeout: the ONE apply/rollback command round trip
//     (cfg.Update.ApplyHTTPTimeout; see UpdateConfig's doc comment in
//     internal/config for how its 8m default was picked).
//  2. the agent-first reachability ladder (verifyAgentHealthWithRetry, GH #291
//     Phase 4): up to len(probeRetryDelays)+1 attempts, each hard-capped at
//     agentVerifyTimeout via context.WithTimeout, plus the sleep between
//     attempts:
//     (len(probeRetryDelays)+1)*agentVerifyTimeout + sum(probeRetryDelays)
//  3. the public homepage probe ladder (probeHealthWithRetry), which reuses
//     the SAME probeRetryDelays schedule; each attempt is one prober.Get call
//     capped at probeHTTPTimeout (cfg.Update.HTTPTimeout, the timeout the
//     shared SSRF client that backs the prober is built with):
//     (len(probeRetryDelays)+1)*probeHTTPTimeout + sum(probeRetryDelays)
//  4. headroom, mirroring the backup worker's `+2*time.Minute` (see
//     NewBackupWorker/backupJobTimeout in cmd/wpmgr/main.go) for scheduling
//     jitter and the small per-attempt backoff the shared HTTP client's own
//     retry-on-5xx layer can add on top of a single probeHTTPTimeout window
//     (internal/httpclient.Client.Do; not separately itemized above to keep
//     this arithmetic reviewable).
//
// With the production defaults (applyHTTPTimeout=8m, probeHTTPTimeout=30s,
// agentVerifyTimeout=8s, probeRetryDelays summing to ~21s across 4 delays)
// this computes to 8m + 61s + 171s + 2m = 13m52s, comfortably under
// staleTaskThreshold (45m), so the periodic reaper still cannot terminalize a
// task that is legitimately still running.
//
// Returns 0 (defer to river.Config.JobTimeout) when applyHTTPTimeout is not
// positive, so a zero/misconfigured input never produces a misleadingly small
// nonzero timeout that omits the apply round trip it is meant to cover.
func DeriveApplyJobTimeout(applyHTTPTimeout, probeHTTPTimeout time.Duration) time.Duration {
	if applyHTTPTimeout <= 0 {
		return 0
	}
	if probeHTTPTimeout <= 0 {
		probeHTTPTimeout = agentVerifyTimeout
	}

	attempts := time.Duration(len(probeRetryDelays) + 1)
	var delaySum time.Duration
	for _, d := range probeRetryDelays {
		delaySum += d
	}

	agentLadder := attempts*agentVerifyTimeout + delaySum
	probeLadder := attempts*probeHTTPTimeout + delaySum
	const headroom = 2 * time.Minute

	return applyHTTPTimeout + agentLadder + probeLadder + headroom
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

	// The agent's OWN upgrade travels a wholly separate branch: a wave-gated
	// three-beat protocol with no health probe and no rollback (see
	// runAgentSelfUpdate for why each of those is absent). It shares nothing
	// with the plugin/theme/core path below except this dispatch point, so no
	// code that snapshots, probes or rolls back a site can ever be reached
	// with the agent as its target.
	if task.TargetType == TargetAgent {
		return w.runAgentSelfUpdate(ctx, a, task)
	}

	// Last line of defense before anything is sent to a site: a task targeting
	// the agent's own plugin is never dispatched. validateItems refuses the
	// target at the planning boundary, so a row can only reach here if it was
	// created before that guard existed. This first pass matches the recognized
	// slug forms and needs no site lookup; the inventory-aware pass below runs
	// once the site has been resolved.
	if task.TargetType == TargetPlugin && agentplugin.Is(task.TargetSlug) {
		return w.refuseAgentSelfUpdate(ctx, task)
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

	// The guard above sees only a slug, which is the directory the agent's
	// archive happened to be unpacked into. Now that the site's own inventory
	// is in hand, re-check the target against it: the plugin-header name
	// identifies the agent whatever its folder is called. This runs before
	// MarkTaskRunning, so nothing has been sent to the site yet.
	if targetsAgentPlugin(site.Components, task.TargetType, task.TargetSlug) {
		return w.refuseAgentSelfUpdate(ctx, task)
	}

	// GH #328 per-site serialisation pre-check (INVARIANT R): is another
	// command already dispatched and unresolved against this same site? Best
	// effort — the agent's own site-update lock is the authoritative bound;
	// this just avoids the wasted HTTP round trip on the common case. Placed
	// AFTER the agent-plugin re-check (so a mis-targeted task is still
	// refused outright rather than deferred) and BEFORE MarkTaskRunning (so a
	// task the gate defers never transitions to 'running' at all — the row is
	// still 'pending' here, and DeferTaskToPending's status write is then a
	// no-op that only updates the wait bookkeeping). See
	// SiteHasRunningUpdateTask's own comment in db/query/updates.sql for the
	// no-deadlock proof and why agent-target rows are excluded.
	if busy, gerr := w.repo.SiteHasRunningTask(ctx, a.TenantID, task.SiteID, a.TaskID, siteWriterHoldMax); gerr == nil && busy {
		return w.deferForBusySite(ctx, task, "another update is running on this site")
	}

	// The claim. Everything above this line is a read; from here on this
	// worker asserts it is the one talking to this site about this item, and
	// the assertion is decided by the row itself under its own row lock, not
	// by the GetTask above. The bound is derived from this worker's own
	// configured apply budget (see ClaimStaleAfter), NOT shared with the
	// per-site gate above: for the gate an over-age row is safe to ignore,
	// for the claim an over-short bound is exactly the double-dispatch defect.
	running, err := w.repo.MarkTaskRunning(ctx, a.TenantID, a.TaskID, w.claimStaleAfter)
	if errors.Is(err, ErrTaskNotClaimed) {
		return w.yieldContendedClaim(ctx, a, task)
	}
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

// targetsAgentPlugin reports whether a task's target resolves, in the site's
// OWN inventory, to the agent's plugin. It is the name-aware companion to the
// slug-only pre-check: the inventory entry carries the plugin-header name,
// which the agent's archive supplies and a rename of the plugin directory
// cannot change. An entry the site does not list at all is not the agent.
func targetsAgentPlugin(components []Component, targetType, targetSlug string) bool {
	if targetType != TargetPlugin {
		return false
	}
	for _, c := range components {
		if c.Type == TargetPlugin && c.Slug == targetSlug && agentplugin.IsComponent(c.Slug, c.Name) {
			return true
		}
	}
	return false
}

// refuseAgentSelfUpdate finishes a task that names the agent as skipped, never
// failed: the operator asked for something the control plane will not do, which
// is not a site error.
func (w *Worker) refuseAgentSelfUpdate(ctx context.Context, task Task) error {
	w.logger.Warn("update task targets the agent's own plugin; refusing to dispatch",
		slog.String("task_id", task.ID.String()),
		slog.String("site_id", task.SiteID.String()),
		slog.String("target_slug", task.TargetSlug))
	return w.finish(ctx, task, TaskSkipped, task.FromVersion, "", "the site agent updates itself over its own signed channel", "")
}

// ----------------------------------------------------------------------------
// GH #328 per-site serialisation: the busy-site gate and its one handler.
// ----------------------------------------------------------------------------

// siteWriterHoldMax bounds how long a 'running' non-agent update_tasks row is
// trusted as evidence of a live writer by the per-site serialisation gate
// (SiteHasRunningUpdateTask). A row whose command went out longer ago than
// this cannot have a live worker behind it, because River cancels the job's
// context at its own job timeout (see Worker.Timeout / DeriveApplyJobTimeout,
// about 13m52s on production defaults) whether the site answers or not. 20
// minutes adds margin over that for scheduling/clock skew and stays
// comfortably under staleTaskThreshold (45m), so the gate stops trusting a
// row strictly before the periodic reaper would terminalize it. Ignoring an
// over-age row only makes the gate MORE PERMISSIVE (it lets a sibling
// dispatch a command the gate would otherwise have deferred), never less,
// which is safe: the agent's own site-update lock is the authoritative bound
// in every case, this gate is only a round-trip optimisation.
const siteWriterHoldMax = 20 * time.Minute

// claimStaleMargin is the headroom the CLAIM's staleness bound keeps over the
// worst-case apply job budget, covering scheduling delay and clock skew
// between the control plane and Postgres.
const claimStaleMargin = 6 * time.Minute

// ClaimStaleAfter is the staleness bound the update-task claim
// (MarkUpdateTaskRunning's reclaim arm) is given, derived from THIS install's
// configured apply job budget rather than fixed.
//
// It is deliberately NOT siteWriterHoldMax, even though the two are equal on
// default configuration. They are different judgements that happen to agree
// today, and the reasoning does not transfer between them:
//
//   - For the per-site GATE (SiteHasRunningUpdateTask), being over-permissive
//     is SAFE. Ignoring an over-age row only lets a sibling send a command the
//     gate would otherwise have deferred, and the agent's own site-update lock
//     is the authoritative bound. See siteWriterHoldMax's own comment.
//   - For the CLAIM, being over-permissive is THE DEFECT. A bound shorter than
//     the time a worker can legitimately still be applying lets a second
//     worker claim a row whose holder is alive, and both dispatch. The claim
//     therefore has to track the apply budget; a constant cannot.
//
// The apply budget is config-driven (DeriveApplyJobTimeout over
// cfg.Update.ApplyHTTPTimeout and cfg.Update.HTTPTimeout, both env-settable),
// so a constant 20m silently stops exceeding it once ApplyHTTPTimeout is
// raised past ~14m. Deriving from the same number that produced the job
// timeout keeps the relationship true at any configuration.
//
// The siteWriterHoldMax floor keeps the historical value on default
// configuration (13m52s + 6m = 19m52s rounds up to 20m) so this is not also a
// silent retune of production behaviour, and makes the bound monotone in the
// apply budget.
//
// applyJobTimeout of 0 means "no derived budget; River's own default applies"
// (see DeriveApplyJobTimeout), which the floor covers.
//
// ValidateClaimTimings must hold for the result; call it at boot.
func ClaimStaleAfter(applyJobTimeout time.Duration) time.Duration {
	if bound := applyJobTimeout + claimStaleMargin; bound > siteWriterHoldMax {
		return bound
	}
	return siteWriterHoldMax
}

// ValidateClaimTimings reports whether the claim's staleness bound still sits
// in the window it must occupy for this install's configuration:
//
//	applyJobTimeout  <  ClaimStaleAfter(applyJobTimeout)  <  staleTaskThreshold
//
// The left inequality is what stops a second worker reclaiming a row whose
// holder is still legitimately applying. The right one keeps the periodic
// reaper from terminalizing a row the claim would still treat as live, which
// would let the reaper and a retrying worker act on one task at once.
//
// Both are config-dependent, and neither is checked by the compiler or by any
// test that runs on default configuration, so this is asserted AT BOOT and the
// process refuses to start when it fails. That is the intended severity: a
// configuration that re-opens a double-dispatch window must not run. The
// remedy is in the message, because the operator who raised the timeout is the
// one who has to read it.
func ValidateClaimTimings(applyJobTimeout time.Duration) error {
	bound := ClaimStaleAfter(applyJobTimeout)
	if applyJobTimeout >= bound {
		return fmt.Errorf("update: claim staleness bound (%v) must exceed the apply job budget (%v); "+
			"a worker could reclaim a task another worker is still applying", bound, applyJobTimeout)
	}
	if bound >= staleTaskThreshold {
		return fmt.Errorf("update: claim staleness bound (%v) must stay under the stale-task reaper threshold (%v), "+
			"but the configured apply budget (%v) pushes it past: lower update.apply_http_timeout "+
			"(WPMGR_UPDATE_APPLY_HTTP_TIMEOUT) or update.http_timeout (WPMGR_UPDATE_HTTP_TIMEOUT). "+
			"The reaper threshold is a compile-time constant and cannot be raised by configuration",
			bound, staleTaskThreshold, applyJobTimeout)
	}
	return nil
}

const (
	// siteBusyBackoffMin/Max bound the jittered snooze between busy-site
	// retries. See siteBusyBackoff.
	siteBusyBackoffMin = 5 * time.Second
	siteBusyBackoffMax = 60 * time.Second

	// siteBusyMaxWait is the AUTHORITATIVE bound on how long a task may keep
	// being deferred for a busy site, measured from the task's own CreatedAt
	// (wall clock, not a retry count — see busyBoundExceeded). A task
	// deferred continuously for longer than this is terminalized as
	// TaskSkipped rather than deferred again: nothing was sent, nothing
	// changed, and the operator is told to re-run when the site is idle.
	// This is the bound that keeps a permanently busy site from holding a
	// task, and therefore its (tenant, site, target_type, target_slug)
	// in-flight dedup slot (m88's partial unique index), open forever.
	siteBusyMaxWait = 6 * time.Hour
)

// SetSiteBusyBackoff overrides the busy-site retry backoff schedule. Exposed
// for tests so a bulk-update test does not actually sleep the production
// 5s-60s window; production code should leave this unset (nil restores the
// default, siteBusyBackoff).
func (w *Worker) SetSiteBusyBackoff(f func(Task) time.Duration) {
	w.busyBackoff = f
}

// siteBusyBackoff computes the snooze delay for a task deferred because its
// site is busy: a jittered delay in [siteBusyBackoffMin, siteBusyBackoffMax).
// The jitter (crypto-unpredictable, via math/rand/v2's auto-seeded default
// source) is what keeps many siblings waiting on the same busy site from
// waking in lockstep and colliding again the moment the current writer
// releases the lock.
func (w *Worker) siteBusyBackoff(task Task) time.Duration {
	if w.busyBackoff != nil {
		return w.busyBackoff(task)
	}
	span := siteBusyBackoffMax - siteBusyBackoffMin
	return siteBusyBackoffMin + time.Duration(rand.Int64N(int64(span)))
}

// busyBoundExceeded reports the reason a busy-deferred task must stop being
// retried and be terminalized instead, or "" when it may still be deferred
// again. The wall-clock bound (siteBusyMaxWait, measured from CreatedAt) is
// AUTHORITATIVE; see the constant's own doc comment.
func busyBoundExceeded(task Task) string {
	if waited := time.Since(task.CreatedAt); waited > siteBusyMaxWait {
		return fmt.Sprintf("for over %s", siteBusyMaxWait)
	}
	return ""
}

// deferForBusySite is the ONE path into the waiting state for a task whose
// site is busy with another update, rollback, or agent upgrade. It ALWAYS
// exits with the task back in 'pending' (never a terminal status), unless the
// task has been waiting longer than siteBusyMaxWait, in which case it is
// terminalized as TaskSkipped — never TaskFailed — because nothing was ever
// sent to the site for this item and nothing on it changed.
//
// A BUSY SITE MUST NEVER PRODUCE A TERMINAL "FAILED" STATUS, and a WAITING
// TASK MUST NEVER LOOK "RUNNING". Both halves live here so neither can be
// broken independently: the bound check below only ever chooses between
// 'pending' (via finish=false) and TaskSkipped, never TaskFailed; and
// DeferTaskToPending is the only write in this codebase that moves a task
// from 'running' back to 'pending'.
func (w *Worker) deferForBusySite(ctx context.Context, task Task, why string) error {
	if bound := busyBoundExceeded(task); bound != "" {
		return w.finish(ctx, task, TaskSkipped, task.FromVersion, "",
			fmt.Sprintf("not attempted: this site was continuously busy with another update, rollback, or agent upgrade %s. "+
				"Nothing was sent to the site for this item and nothing on the site was changed. Re-run this update when the site is idle.", bound), "")
	}

	deferred, err := w.repo.DeferTaskToPending(ctx, DeferTaskInput{
		TenantID: task.TenantID,
		TaskID:   task.ID,
		Detail:   "waiting: " + why,
	})
	if errors.Is(err, ErrTaskNotOpen) {
		// A halt (or a concurrent finisher) already terminalized this task
		// while the site was being asked. The recorded outcome wins; there
		// is nothing left to wait for.
		return nil
	}
	if err != nil {
		// Could not record the wait: snooze SHORT and retry the demotion
		// rather than terminalize a task for a CP-side DB hiccup. Still
		// bounded even so: the gate's own staleness clause (siteWriterHoldMax)
		// stops trusting a row left 'running' after that, so a sibling can
		// still make progress, and the periodic reaper (staleTaskThreshold)
		// claims this row itself after that regardless of how this branch
		// resolves.
		w.logger.Warn("update: failed to defer task for a busy site; snoozing short",
			slog.String("task_id", task.ID.String()), slog.Any("error", err))
		return river.JobSnooze(siteBusyBackoffMin)
	}
	w.publish(deferred, RunRunning)
	return river.JobSnooze(w.siteBusyBackoff(deferred))
}

// yieldContendedClaim handles the one outcome the claim CAS cannot explain
// itself: zero rows matched, so this worker did NOT get the task and must not
// dispatch. The statement does not say why, so re-read the row and decide.
//
// The two returns here are the whole point, and neither is the obvious one:
//
//   - nil, ONLY for a terminal row. Someone else already recorded the outcome;
//     that outcome wins and there is nothing left to do. This is exactly what
//     Work already does for a terminal read at its top.
//   - river.JobSnooze otherwise. NOT an error: an error consumes a retry
//     attempt, and enough of them dead-letter a task that never actually
//     failed. NOT nil either: that drops the work silently while the row sits
//     'pending' forever, holding its (tenant, site, target_type, target_slug)
//     in-flight slot in m88's partial unique index and 409-ing every future
//     attempt at that target. Snooze reschedules without spending an attempt,
//     so the row stays reclaimable if the current holder dies.
//
// Termination does NOT rest on the CAS reclaim arm. That arm is bounded by age
// alone, is disabled entirely for target_type = 'agent', and depends on a
// config-derived bound; leaning on it would make this path's termination a
// property of somebody else's tuning. The authoritative bound is the same
// wall-clock one every other waiting path here uses (siteBusyMaxWait, measured
// from the task's own CreatedAt), applied below before anything else.
func (w *Worker) yieldContendedClaim(ctx context.Context, a TaskArgs, task Task) error {
	// The wall-clock bound comes FIRST, before the re-read, so it holds on
	// every path out of this function including the one where the re-read
	// itself fails. Every other waiting path in this worker is bounded this
	// way (see deferForBusySite and siteBusyMaxWait's own comment); without it
	// this is the only place that can snooze indefinitely, and an
	// indefinitely-snoozing job is invisible: no error, no attempt consumed,
	// no terminal state, nothing to alert on.
	//
	// TaskSkipped, never TaskFailed: this worker never got the claim, so it
	// sent nothing to the site and changed nothing on it. If the holder is in
	// fact still alive and finishes later, FinishTask refuses to overwrite a
	// terminal row (ErrTaskNotOpen), so the recorded outcome still wins.
	if bound := busyBoundExceeded(task); bound != "" {
		return w.finish(ctx, task, TaskSkipped, task.FromVersion, "",
			fmt.Sprintf("not attempted: another worker held this task continuously %s. "+
				"Nothing was sent to the site for this item and nothing on the site was changed. "+
				"Re-run this update.", bound), "")
	}

	current, err := w.repo.GetTask(ctx, a.TenantID, a.TaskID)
	if err != nil {
		// Cannot tell terminal from contended. Snooze rather than error: the
		// safe reading of an unknown row is "someone else may hold it", and a
		// CP-side DB hiccup must not spend this task's retry budget. Bounded
		// by the busyBoundExceeded check above, which is exactly why that
		// check does not depend on this read succeeding.
		w.logger.Warn("update: claim was refused and the row could not be re-read; snoozing",
			slog.String("task_id", a.TaskID.String()), slog.Any("error", err))
		return river.JobSnooze(w.siteBusyBackoff(task))
	}
	if terminal(current.Status) {
		return nil
	}
	w.logger.Info("update: another worker holds this task; snoozing without consuming an attempt",
		slog.String("task_id", a.TaskID.String()), slog.String("status", current.Status))
	return river.JobSnooze(w.siteBusyBackoff(current))
}

// runDry asks the agent what WOULD change without mutating the site.
func (w *Worker) runDry(ctx context.Context, task Task, siteURL string, item agentcmd.UpdateItem) error {
	resp, err := w.cmd.Update(ctx, task.SiteID, siteURL, agentcmd.UpdateRequest{DryRun: true, Snapshot: false, Items: []agentcmd.UpdateItem{item}})
	if err != nil {
		return w.finish(ctx, task, TaskFailed, task.FromVersion, "", "dry-run command failed", err.Error())
	}
	// A 200 only means the transport worked. resp.OK is the agent's own verdict on
	// whether it accepted the command at all, and firstResult synthesises an
	// ItemFailed when the agent sent back no per-item result. Both are failures,
	// and neither was checked here before: every dry run took the TaskSucceeded
	// path below, so an agent that had rejected the command outright was still
	// recorded as a successful "no change".
	res := firstResult(resp.Results)

	// GH #328: a busy site refuses BEFORE the resp.OK check below, exactly
	// like runApply (see that function for why the ordering is load-bearing).
	// A stock agent takes no lock for a dry run, but a foreign maintenance
	// window (e.g. a human's wp-admin bulk update in progress) can still
	// refuse one.
	if res.Status == agentcmd.ItemSiteBusy {
		return w.deferForBusySite(ctx, task, detailOr(res.Log, "another update is running on this site"))
	}
	if !resp.OK {
		return w.finish(ctx, task, TaskFailed, task.FromVersion, "",
			"agent rejected the dry-run command", detailOr(res.Log, "agent returned ok=false"))
	}
	if res.Status == agentcmd.ItemFailed {
		return w.finish(ctx, task, TaskFailed, fromOr(res.FromVersion, task.FromVersion), res.ToVersion,
			"agent reported dry-run failure", res.Log)
	}
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

	// GH #328: a busy site refuses BEFORE the resp.OK check below and BEFORE
	// the ItemFailed check further down. Ordering here is LOAD BEARING: the
	// agent sets ok=false whenever any item reports a non-success status
	// (site_busy included, same as a failure), so checking resp.OK first
	// would send this down the generic TaskFailed path and make every busy
	// refusal a permanent recorded failure instead of a retried wait. See
	// deferForBusySite for the no-deadlock invariant this preserves
	// (INVARIANT R) and for why this can never be tallied as Failed or halt a
	// wave (agent_wave.go's tallyWave/haltReasonFor operate ONLY over
	// target_type='agent' tasks — see waveOrder's filter — so a plugin/theme/
	// core task deferred here is never even visible to that code; and the
	// 'pending' status this produces falls into tallyWave's default case,
	// InFlight, on the rare occasion a task of any type is inspected there).
	if res.Status == agentcmd.ItemSiteBusy {
		return w.deferForBusySite(ctx, task, detailOr(res.Log, "another update is running on this site"))
	}

	// The agent's own verdict on the command dispatch. An ok=false here means it
	// never ran the update, so the per-item results below cannot be trusted to
	// describe what happened on disk; fail the task rather than health-probing a
	// site that was never touched and then recording it as updated.
	if !resp.OK {
		return w.finish(ctx, task, TaskFailed, fromOr(res.FromVersion, task.FromVersion), res.ToVersion,
			"agent rejected the update command", detailOr(res.Log, "agent returned ok=false"))
	}

	if res.Status == agentcmd.ItemFailed {
		return w.finish(ctx, task, TaskFailed, fromOr(res.FromVersion, task.FromVersion), res.ToVersion, "agent reported update failure", res.Log)
	}
	if res.Status == agentcmd.ItemUpToDate || res.Status == agentcmd.ItemSkipped {
		return w.finish(ctx, task, TaskSkipped, fromOr(res.FromVersion, task.FromVersion), res.ToVersion, "already up to date", "")
	}

	// GH #291 Phase 4 (post-review fix): ask the agent FIRST, over a signed and
	// therefore uncacheable round trip. The control plane has JUST spoken to
	// this exact agent to apply the update; a fresh signed check is
	// authoritative proof PHP booted and the plugin loaded (the signed command
	// route does not exist otherwise), and no page cache can fake it. See
	// verifyAgentHealth for the per-attempt decision table and
	// verifyAgentHealthWithRetry for why a single unhealthy sample is never
	// enough on its own (the same transient-migration window that can make the
	// public homepage 503 for a few seconds can just as easily hit the agent's
	// own PHP route, since it is served by the same stack). An inconclusive
	// verdict (no signing key configured, the agent is absent/uninstalled, or
	// the check timed out or hit a TLS/transport error) does NOT roll back on
	// its own, because plenty of legitimate sites have no agent reachable this
	// way; it falls through to the public probe below exactly like a healthy
	// verdict does.
	verdict, verifyDetail, agentAttempts := w.verifyAgentHealthWithRetry(ctx, task.SiteID, siteURL)
	if verdict == agentHealthUnhealthy {
		return w.rollback(ctx, task, siteURL, item, res, agentcmd.ProbeResult{}, true,
			fmt.Sprintf("post-update agent reachability check failed after %d attempt(s): %s", agentAttempts, verifyDetail))
	}

	// The public homepage probe still runs even when the agent-first check
	// came back healthy. The signed agent route only proves PHP booted and the
	// WPMgr plugin loaded; it proves nothing about whether the site RENDERS. A
	// theme update, or a plugin that only fatals on a front-end hook (e.g.
	// wp_head or the_content), can leave the agent route perfectly healthy
	// while the homepage is fatal, so an agent-healthy verdict must not by
	// itself end the check for any target type, plugin included.
	//
	// Reusing probeHealthWithRetry here, rather than a bespoke single-shot
	// check, keeps the added latency close to zero on the common path:
	// probeHealthWithRetry only invokes its backoff schedule when the FIRST
	// attempt is not already postUpdateHealthy, so an agent-confirmed-healthy
	// update whose front end also renders fine (the overwhelming common case)
	// costs exactly one extra request. Only a front end that is ALSO failing
	// pays the bounded (about 21s worst case) retry cost, which is the same
	// budget a broken update already paid before this phase existed, and which
	// exists for the same reason as verifyAgentHealthWithRetry: a single failed
	// sample right after activation must not be mistaken for a broken update.
	probe, perr, attempts := w.probeHealthWithRetry(ctx, siteURL)
	if perr != nil {
		// Still unreachable after retrying: treat as unhealthy and roll back.
		return w.rollback(ctx, task, siteURL, item, res, probe, false, fmt.Sprintf("post-update probe error after %d attempt(s): %v", attempts, perr))
	}
	switch classifyPostUpdateProbe(probe) {
	case postUpdateHealthy:
		detail := "updated and healthy"
		if verdict == agentHealthHealthy {
			detail = "updated and healthy (signed agent check and public probe both confirmed)"
		}
		return w.finish(ctx, task, TaskSucceeded, fromOr(res.FromVersion, task.FromVersion), res.ToVersion, detail, "")
	case postUpdateInconclusive:
		// A cached response (or, per the design doc's baseline note below, a
		// 404/410 on the site root with no pre-update baseline to compare
		// against) is NOT proof of health. Do not roll back on it alone, but
		// record the uncertainty plainly rather than claiming "healthy".
		detail := fmt.Sprintf("updated; post-update probe inconclusive after %d attempt(s): %s, so this could not confirm the update did not break the site", attempts, probe.Detail)
		if verdict == agentHealthHealthy {
			detail = fmt.Sprintf("updated; signed agent check confirmed the backend healthy, but the public probe was inconclusive after %d attempt(s): %s, so the front end could not be separately confirmed", attempts, probe.Detail)
		}
		return w.finish(ctx, task, TaskSucceeded, fromOr(res.FromVersion, task.FromVersion), res.ToVersion, detail, "")
	default: // postUpdateUnhealthy
		reason := fmt.Sprintf("post-update health failed after %d attempt(s): status=%d %s", attempts, probe.StatusCode, probe.Detail)
		if verdict == agentHealthHealthy {
			reason = fmt.Sprintf("signed agent check reported healthy, but the public homepage failed after %d attempt(s): status=%d %s (a front-end-only failure the agent route cannot see)", attempts, probe.StatusCode, probe.Detail)
		}
		return w.rollback(ctx, task, siteURL, item, res, probe, false, reason)
	}
}

// agentVerifyTimeout bounds ONE attempt of the agent-first post-update
// reachability check (GH #291 Phase 4). Short and independent of the update
// command's own (possibly multi-minute) timeout: this is a lightweight signed
// ping/metadata round trip, not the heavy update operation itself. Applied
// per attempt inside verifyAgentHealthWithRetry, not to the retry loop as a
// whole.
const agentVerifyTimeout = 8 * time.Second

// agentHealthVerdict is the outcome of the agent-first post-update
// reachability check (see Worker.verifyAgentHealth and
// Worker.verifyAgentHealthWithRetry). The zero value is agentHealthInconclusive
// so a mistakenly-unset verdict never triggers either a false "healthy" or a
// false rollback.
type agentHealthVerdict int

const (
	// agentHealthInconclusive covers both "the check could not be run at all"
	// (no signing key configured, or a Commander test double that does not
	// implement agentVerifier) and "the check ran but the result does not
	// prove anything either way" (agent absent/uninstalled, a non-404/5xx
	// HTTP status, a timeout, or a TLS/transport error). It deliberately does
	// NOT roll back on its own: a great many legitimate sites will have no
	// agent reachable from this specific check, and rolling back a good
	// update on that basis alone would be its own outage. The caller falls
	// through to the public probe, exactly like agentHealthHealthy does.
	agentHealthInconclusive agentHealthVerdict = iota
	// agentHealthHealthy means the signed command route answered: PHP booted
	// and the plugin loaded. This is authoritative and cannot be a cache
	// artifact (the route only exists once PHP has booted). It proves nothing
	// about the front end, though, so the caller still runs the public probe
	// (see runApply); it only proves the backend is alive, which lets that
	// probe skip its own retry ladder on the common happy path.
	agentHealthHealthy
	// agentHealthUnhealthy means the signed command route itself returned a
	// server error on EVERY attempt across verifyAgentHealthWithRetry's retry
	// window, not merely once: PHP is persistently fatal-ing on every request,
	// including the agent's own route. This is the strongest signal available
	// short of total unreachability, and because it came back over a signed
	// round trip rather than a cacheable public GET, it cannot be a stale
	// cached read. A single unhealthy sample is deliberately NOT enough to
	// reach this verdict (see verifyAgentHealthWithRetry): the agent's ping
	// route is served by the same PHP and WordPress stack as the homepage, so
	// it is exposed to the same transient migration-on-activation window,
	// php-fpm restart, opcache reset, or WAF/CDN origin error that
	// probeRetryDelays already exists to ride out for the public probe.
	agentHealthUnhealthy
)

// agentVerifier is the subset of *agentcmd.Client used for the agent-first
// post-update reachability check. Defined locally, matching the method
// agentcmd.Client already exposes, so this package does not need to import
// internal/site's identically-shaped AgentVerifier, and so any Commander test
// double that does not implement it (most of worker_test.go's fakes) simply
// fails the type assertion in verifyAgentHealth rather than needing a stub
// method, which itself resolves to the correct agentHealthInconclusive
// behaviour.
type agentVerifier interface {
	VerifyReachableWithReason(ctx context.Context, siteID uuid.UUID, siteURL string) (alive bool, fallbackUsed bool, reason agentcmd.ReachabilityReason, err error)
}

// verifyAgentHealth implements GH #291 Phase 4's per-attempt agent-first
// decision table (see verifyAgentHealthWithRetry for the retry wrapper that
// callers actually use). It never returns an error: any failure to even run
// the check collapses to agentHealthInconclusive, exactly like an ambiguous
// check result, so a caller can never mistake "we couldn't ask" for "we
// asked and it's fine" or "we asked and it's broken".
func (w *Worker) verifyAgentHealth(ctx context.Context, siteID uuid.UUID, siteURL string) (agentHealthVerdict, string) {
	verifier, ok := w.cmd.(agentVerifier)
	if !ok {
		return agentHealthInconclusive, "agent reachability check unavailable (no signing key configured)"
	}
	verifyCtx, cancel := context.WithTimeout(ctx, agentVerifyTimeout)
	defer cancel()
	alive, _, reason, err := verifier.VerifyReachableWithReason(verifyCtx, siteID, siteURL)
	if err != nil {
		// An infrastructure failure minting/sending the check (e.g. a JWT-mint
		// error) proves nothing about the site itself: ambiguous, not a signal.
		return agentHealthInconclusive, fmt.Sprintf("agent reachability check error: %v", err)
	}
	if alive {
		return agentHealthHealthy, "signed agent route reachable"
	}
	if reason == agentcmd.ReasonHTTP5xx {
		return agentHealthUnhealthy, "signed agent route returned a server error (PHP fatal)"
	}
	return agentHealthInconclusive, fmt.Sprintf("agent reachability ambiguous (%s)", reason)
}

// verifyAgentHealthWithRetry retries the agent-first reachability check (see
// verifyAgentHealth) using the SAME backoff schedule as the public probe
// (probeRetryDelays, or w.probeDelays in tests), rather than a second bespoke
// schedule. A single unhealthy sample must never conclude agentHealthUnhealthy
// on its own: the agent's ping route is served by the same PHP and WordPress
// stack as the homepage, so a transient DB-migration-on-activation window (or
// a php-fpm restart, opcache reset, or WAF/CDN origin error) can make it 5xx
// for a few seconds exactly like the public homepage. Only a verdict that
// STAYS agentHealthUnhealthy across the whole retry window reaches that
// conclusion here; a verdict that resolves to healthy or inconclusive at any
// attempt returns immediately, mirroring probeHealthWithRetry's early return
// on postUpdateHealthy.
func (w *Worker) verifyAgentHealthWithRetry(ctx context.Context, siteID uuid.UUID, siteURL string) (verdict agentHealthVerdict, detail string, attempts int) {
	delays := probeRetryDelays
	if w.probeDelays != nil {
		delays = w.probeDelays
	}
	for i := 0; ; i++ {
		attempts = i + 1
		verdict, detail = w.verifyAgentHealth(ctx, siteID, siteURL)
		if verdict != agentHealthUnhealthy {
			return verdict, detail, attempts
		}
		if i >= len(delays) {
			return verdict, detail, attempts
		}
		select {
		case <-ctx.Done():
			return verdict, detail, attempts
		case <-time.After(delays[i]):
		}
	}
}

// postUpdateVerdict classifies a public homepage ProbeResult specifically for
// the post-update rollback decision. This is intentionally NOT the same rule
// as ProbeResult.Healthy(): see classifyPostUpdateProbe's doc comment for why
// a separate, explicit classification exists instead of changing Healthy()
// itself.
type postUpdateVerdict int

const (
	postUpdateUnhealthy postUpdateVerdict = iota
	postUpdateHealthy
	postUpdateInconclusive
)

// classifyPostUpdateProbe applies the post-update-specific health rule.
//
// A response flagged CacheHit is checked FIRST, before any status-code based
// classification: a cached response proves nothing about the CURRENT backend
// state, so evaluating Fatal/5xx/404 first would let a stale cached error (or
// a stale cached 404) drive a rollback, or a stale cached 200 claim health,
// for a site the update may never have touched. A CacheHit result is always
// classified inconclusive, regardless of its status code.
//
// A 5xx status or a fatal-error body signature is unhealthy (unchanged from
// Healthy()). A 401 or 403 is healthy, because those are common and
// legitimate on the homepage (staging HTTP auth, a members-only site, a
// security plugin) and rolling back a good update because of one would be its
// own outage.
//
// A 404 or 410 on the site root is classified inconclusive, NOT unhealthy and
// NOT healthy (see docs/security/uptime-app-health-design-2026-07-27.md
// section 4 item 2, which explicitly forbids treating 4xx as a rollback
// trigger). There is no pre-update baseline recorded for this task, so a site
// that legitimately already returned 404 on its root BEFORE the update (a
// headless install, a site whose root is intentionally empty, a redirect-only
// domain) cannot be told apart from one broken BY the update; rolling back the
// former would be its own outage. Classifying it inconclusive rather than
// healthy still stops the caller from claiming the update was confirmed fine.
// Follow-up: record a pre-update probe of the site root so a 404 that appears
// ONLY after the update can be promoted to a real unhealthy signal.
func classifyPostUpdateProbe(probe agentcmd.ProbeResult) postUpdateVerdict {
	if probe.CacheHit {
		return postUpdateInconclusive
	}
	if probe.StatusCode <= 0 || probe.Fatal || probe.StatusCode >= 500 {
		return postUpdateUnhealthy
	}
	if probe.StatusCode == http.StatusNotFound || probe.StatusCode == http.StatusGone {
		return postUpdateInconclusive
	}
	return postUpdateHealthy
}

// probeHealthWithRetry probes siteURL and, if the site is unhealthy,
// inconclusive, or unreachable, retries with backoff (probeDelays, defaulting
// to probeRetryDelays) before giving up. It returns as soon as any attempt
// classifies postUpdateHealthy (see classifyPostUpdateProbe; NOT the same
// rule as ProbeResult.Healthy(), see classifyPostUpdateProbe's doc comment for
// why); otherwise it returns the LAST probe result/error after exhausting the
// schedule, plus the number of attempts made (folded into the rollback reason
// so an operator can see the update was given a fair chance). Retrying on an
// inconclusive (cache-hit) result too, rather than exiting early on it, gives a
// cache-busted retry a real chance to land on a genuine cache MISS within the
// window.
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
		if perr == nil && classifyPostUpdateProbe(probe) == postUpdateHealthy {
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
// zero value when rollback was reached via a probe TRANSPORT error, or via
// the GH #291 Phase 4 agent-first check, rather than a reachable-but-unhealthy
// public-probe response), and is used, together with agentConfirmedFatal,
// only to classify a rollback-transport failure below (GH #210).
// agentConfirmedFatal is true when the rollback decision came from the
// signed agent-first reachability check itself returning a server error
// (agentHealthUnhealthy), which is exactly as strong a "site-wide PHP fatal"
// signal as probe.Fatal or probe.StatusCode >= 500, but arrives with no
// ProbeResult to carry it.
func (w *Worker) rollback(ctx context.Context, task Task, siteURL string, item agentcmd.UpdateItem, res agentcmd.ItemResult, probe agentcmd.ProbeResult, agentConfirmedFatal bool, reason string) error {
	from := fromOr(res.FromVersion, task.FromVersion)
	rbResp, rbErr := w.cmd.Rollback(ctx, task.SiteID, siteURL, agentcmd.RollbackRequest{
		Type:       item.Type,
		Slug:       item.Slug,
		SnapshotID: res.SnapshotID,
		ToVersion:  from,
	})
	// A 200 with ok=false is the agent REFUSING the rollback (e.g. the snapshot
	// is gone or unreadable). The transport succeeded, so rbErr is nil and the
	// task used to land on the TaskRolledBack line below, telling the operator
	// the site had been restored when nothing had been restored at all. That is
	// the most dangerous shape of this bug: the site is still broken and the
	// record says it was recovered. Report it as a failed rollback instead.
	if rbErr == nil && !rbResp.OK {
		return w.finish(ctx, task, TaskFailed, from, res.ToVersion,
			"rollback REFUSED by agent after unhealthy update: "+reason,
			detailOr(rbResp.Log, "agent returned ok=false"))
	}
	if rbErr != nil {
		// Rollback itself failed: this is the worst case. Record as failed with
		// both the health reason and the rollback error so the operator is alerted.
		detail := "rollback FAILED after unhealthy update: " + reason
		// GH #210: when the post-update health check ITSELF detected a
		// site-wide PHP fatal (a fatal-error body signature, a 5xx probe
		// response, or the GH #291 Phase 4 signed agent-first check itself
		// getting a 5xx, agentConfirmedFatal) AND the rollback command's
		// transport also errored, the site is very likely serving a
		// site-wide PHP fatal that makes the agent's own REST endpoint
		// undeliverable, a distinct, more actionable failure mode than a
		// generic "rollback failed" (which could also mean e.g. a
		// transient network blip unrelated to the update). Record a distinct
		// detail so the operator knows the automatic filesystem-level
		// recovery on the agent side is the remaining recovery path.
		if probe.Fatal || probe.StatusCode >= 500 || agentConfirmedFatal {
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
		if !errors.Is(err, ErrTaskNotOpen) {
			return err
		}
		// The task already has a terminal state, typically 'cancelled',
		// written by a halt while this worker was in flight. First outcome
		// recorded wins: reporting this one would overwrite the halt with a
		// success and hide the fact that the run was stopped. The job itself
		// succeeded; there is simply nothing left to record.
		w.logger.Info("update task already finished; leaving the recorded outcome alone",
			slog.String("task_id", task.ID.String()),
			slog.String("recorded_status", finished.Status),
			slog.String("discarded_status", status))
		return nil
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
// the full apply + post-update health-check budget the River job itself is
// now given (see Worker.Timeout / DeriveApplyJobTimeout, with production
// defaults that budget is ~10m52s, itself already inclusive of both
// post-update retry ladders) and the per-tenant parallelism snooze backoff: a
// worker crash between MarkTaskRunning and finish(), or an EnqueueTask
// failure that leaves a task pending (Service.CreateRun's best-effort enqueue
// after CreateRunWithTasks), would otherwise permanently occupy the
// update_tasks_inflight_target_idx slot for that (tenant, site, target), // every future update attempt for it 409s targets_in_flight forever (m88).
// If DeriveApplyJobTimeout's budget is ever tuned up near or past this value,
// this threshold must grow with it: the reaper terminalizing a task that is
// still legitimately within its own job deadline would itself be a bug.
const staleTaskThreshold = 45 * time.Minute

// staleTaskReapLimit bounds how many stale tasks one sweep reaps, so an
// unbounded backlog cannot make a single periodic tick unbounded; any
// remainder is caught by the next sweep (see the periodic job interval, wired
// in cmd/wpmgr/main.go).
const staleTaskReapLimit = 500

// agentStaleTaskThreshold is the reaper's threshold for an AGENT self-update
// task. It is far longer than staleTaskThreshold because such a task is
// legitimately slow in two ways the ordinary threshold never anticipates: a
// task in a later wave sits pending until every earlier wave has confirmed
// (which for a large fleet is several confirmation windows back to back), and
// a task on a site with external cron waits up to
// agentConfirmDeadlineExternalCron for beat 2 on its own. Reaping either as
// "stale" would fail a task that is behaving exactly as designed, and, worse
// , feed a false failure into the wave gate, halting a healthy rollout.
//
// It is still finite: a genuinely stuck agent task must eventually release its
// (tenant, site, target_type, target_slug) slot in the
// update_tasks_inflight_target_idx partial unique index (m88), or every future
// agent upgrade for that site would 409 forever.
const agentStaleTaskThreshold = 6 * time.Hour

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
		// An agent self-update task is legitimately slow (wave gating +
		// external-cron confirmation windows); it gets its own, much longer
		// threshold. The SQL sweep cannot express two thresholds, so the
		// narrower one is applied here.
		if task.TargetType == TargetAgent && time.Since(task.UpdatedAt) < agentStaleTaskThreshold {
			continue
		}
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
