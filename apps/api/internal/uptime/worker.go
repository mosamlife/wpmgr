package uptime

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
)

// SiteLookup resolves a site's name for alert rendering (tenant-scoped). The
// probe job already has URL+IDs from the cross-tenant enrolled list; the name is
// a nicety, so a lookup failure degrades to using the URL.
type SiteLookup interface {
	SiteName(ctx context.Context, tenantID, siteID uuid.UUID) string
}

// ProbeArgs is the River job payload for the periodic probe sweep. It carries no
// per-run data; the cadence is on the periodic job.
type ProbeArgs struct{}

// Kind implements river.JobArgs.
func (ProbeArgs) Kind() string { return "uptime_probe" }

// ProbeWorker runs one probe sweep across all enrolled sites: it probes each
// site over the SSRF client, batch-writes the results to ClickHouse, refreshes
// each site's Postgres health_status, and evaluates the alert state machine,
// firing downtime/recovery alerts on transition (de-duped). It bounds
// concurrency with a worker pool so one sweep cannot stampede the network.
type ProbeWorker struct {
	river.WorkerDefaults[ProbeArgs]
	repo        Repo
	prober      *Prober
	store       metrics.Store
	dispatcher  *Dispatcher
	sites       SiteLookup
	logger      *slog.Logger
	concurrency int
	threshold   int

	// GH #291 Phase 2 (application health). All three are zero-value/nil
	// until SetAppProber is called: appProber stays nil, so Sweep's app-probe
	// branch below never runs and every check this worker writes carries
	// AppUp=nil / AppProbeReason="" - bit-identical to this worker's
	// behavior before this feature existed. See SetAppProber.
	appProber        *AppProber
	probeInterval    time.Duration
	appProbeInterval time.Duration

	// GH #291 Phase 3 (application-health ALERTING). Zero-value-safe: when
	// SetAppAlertConfig is never called, defaultAppAlertThreshold /
	// defaultAppAlertBreakerRatio apply (see resolveAppAlerts/processSite),
	// so this worker still evaluates+persists app-health transitions
	// whenever the app probe attempts a verdict (state tracking is
	// unconditional - see Repo.TransitionAlertState's doc comment); only
	// DISPATCH is gated, per-tenant, on AlertConfig.AppAlertsEnabled.
	appAlertThreshold    int
	appAlertBreakerRatio float64

	// jobTimeout overrides River's default 60s per-job context deadline (see
	// Timeout, DeriveProbeJobTimeout, and the identical jobTimeout field on
	// update.Worker / backup.BackupWorker for the same pattern applied to
	// their own jobs). Sweep's worst-case wall-clock cost scales with the
	// size of the fleet being probed (see DeriveProbeJobTimeout's doc
	// comment), so the 60s default is only correct for a small fleet. Past
	// that, River silently cancels the job mid-sweep: some sites get probed
	// and recorded, the rest simply are not, with no error explaining the
	// gap. Zero falls back to river.Config.JobTimeout, see Timeout's doc
	// comment.
	jobTimeout time.Duration
}

// defaultAppAlertThreshold is ~25 minutes at the documented 300s app-probe
// cadence (design doc section 1).
const defaultAppAlertThreshold = 5

// defaultAppAlertBreakerRatio is 25% (design doc section 2).
const defaultAppAlertBreakerRatio = 0.25

// minAppAlertBreakerDownCount is the absolute floor, ANDed with the ratio,
// below which the fleet circuit breaker can never trip - see
// resolveAppAlerts's wantTrip computation for the full reasoning: "many
// sites breaking at once" is meaningless below 2 simultaneously-down sites,
// so a 1-site tenant's single site going down (100% of its own eligible
// population) must never be treated as a fleet-wide event.
const minAppAlertBreakerDownCount = 2

// NewProbeWorker builds the probe worker. concurrency caps simultaneous probes;
// threshold is the consecutive-down count that fires a downtime alert.
func NewProbeWorker(repo Repo, prober *Prober, store metrics.Store, dispatcher *Dispatcher, sites SiteLookup, logger *slog.Logger, concurrency, threshold int) *ProbeWorker {
	if logger == nil {
		logger = slog.Default()
	}
	if concurrency <= 0 {
		concurrency = 10
	}
	if threshold < 1 {
		threshold = 2
	}
	return &ProbeWorker{repo: repo, prober: prober, store: store, dispatcher: dispatcher, sites: sites, logger: logger, concurrency: concurrency, threshold: threshold}
}

// SetAppProber wires the GH #291 Phase 2 application-health prober into the
// existing reachability sweep. Optional: a ProbeWorker this is never called
// on (every existing caller/test, and any deployment with
// WPMGR_UPTIME_APP_PROBE_ENABLED=false) makes zero app-probe network
// requests and writes exactly what it wrote before this feature existed -
// this is the mechanism behind the design's bit-identical-on-upgrade
// requirement, not a separate code path.
//
// probeInterval is this worker's OWN reachability cadence (the same value
// main.go registers its periodic job under); appProbeInterval is the desired
// app-probe cadence. Both feed appProbeDue, the stateless cadence check that
// decides, per sweep tick and per site, whether THIS tick also attempts an
// app probe.
func (w *ProbeWorker) SetAppProber(ap *AppProber, probeInterval, appProbeInterval time.Duration) {
	w.appProber = ap
	w.probeInterval = probeInterval
	w.appProbeInterval = appProbeInterval
}

// SetAppAlertConfig wires the GH #291 Phase 3 numeric knobs: threshold is
// the consecutive CONCLUSIVE-false count that fires an app-down alert
// (default defaultAppAlertThreshold when <1); breakerRatio is the fleet
// circuit breaker's trip ratio (default defaultAppAlertBreakerRatio when
// <=0). Optional - never calling this leaves both at their defaults, which
// is exactly what every existing caller/test gets.
func (w *ProbeWorker) SetAppAlertConfig(threshold int, breakerRatio float64) {
	w.appAlertThreshold = threshold
	w.appAlertBreakerRatio = breakerRatio
}

// SetJobTimeout overrides River's default per-job context deadline for the
// probe sweep (see Timeout). Call once at boot with
// uptime.DeriveProbeJobTimeout(...), see that function's doc comment for the
// arithmetic. Optional: a ProbeWorker this is never called on keeps
// jobTimeout at its zero value, which Timeout reports back as 0 (defer to
// river.Config.JobTimeout), so every existing caller/test is unaffected.
func (w *ProbeWorker) SetJobTimeout(d time.Duration) {
	w.jobTimeout = d
}

// Timeout overrides River's default per-job context deadline (60s) for the
// uptime probe sweep, the same way update.Worker.Timeout and
// backup.BackupWorker.Timeout do for their own jobs. Returning a positive
// duration makes River use it instead of river.Config.JobTimeout; returning 0
// keeps the default. River documents that returning -1 disables the deadline
// entirely; we intentionally do NOT do that: a genuinely wedged sweep must
// eventually error out so River can retry it. Sweep's own admission-control
// backstop (see sweepBudgetExhausted) is meant to degrade a large-fleet sweep
// gracefully well before this deadline is ever reached; Timeout is the last
// resort behind it, not the primary mechanism.
func (w *ProbeWorker) Timeout(*river.Job[ProbeArgs]) time.Duration { return w.jobTimeout }

// DefaultMaxFleetSizeForProbeTimeout is the fleet size DeriveProbeJobTimeout
// assumes when sizing the probe sweep's job-level Timeout() budget and no
// more specific value is configured (see UptimeConfig.MaxFleetSize in
// internal/config).
//
// The sweep's real worst case is not a fixed number: it scales with however
// many sites ListEnrolledForProbe returns, which grows with the fleet and is
// never a compile-time constant. Two ways to size a job Timeout() budget
// against that moving target were considered:
//
//   - Derive it from the CURRENTLY enrolled site count (a live COUNT query at
//     boot, or refreshed periodically). Rejected: the budget would then go
//     stale as the fleet grows between boots/refreshes, silently
//     reintroducing exactly the bug this function exists to fix, and a
//     fleet-wide incident (the scenario that actually needs the larger
//     budget) is also the scenario least likely to coincide with a recent
//     redeploy.
//   - A generous, deliberately FIXED ceiling, decoupled from the actual
//     current fleet size and therefore immune to that staleness failure
//     mode, configurable (WPMGR_UPTIME_MAX_FLEET_SIZE /
//     UptimeConfig.MaxFleetSize) for an operator who legitimately runs a
//     larger fleet than the default covers. This is what
//     DefaultMaxFleetSizeForProbeTimeout does.
//
// 2000 is chosen to comfortably exceed any fleet size this deployment has
// been operated against to date while still keeping the resulting Timeout()
// (see DeriveProbeJobTimeout's worked example) under an hour, so a genuinely
// wedged sweep is still caught in a reasonable window rather than only after
// a multi-hour deadline. A fleet that outgrows this ceiling, configured or
// default, is not silently broken by that: Sweep's own admission-control
// backstop (sweepBudgetExhausted) stops admitting new sites once the job's
// remaining budget can no longer safely cover another one, so the sweep still
// finishes with a partial-but-recorded result and a logged skip count instead
// of an abrupt River cancellation. Raising this value only widens the window
// before that backstop would ever need to engage.
const DefaultMaxFleetSizeForProbeTimeout = 2000

// DeriveProbeJobTimeout computes the River job-level Timeout() budget for the
// uptime probe sweep (see ProbeWorker.Timeout / SetJobTimeout). It reads the
// actual sweep knobs (concurrency, both probe timeouts, and the app-probe
// cadence ratio) rather than assuming them, so this stays correct if any of
// them is ever tuned.
//
// Sweep's worst-case wall-clock budget, in the order the work happens (see
// Sweep):
//
//  1. reachability pass: every one of maxFleetSize sites is probed under the
//     `sem` semaphore (bounded to `concurrency` in flight at once), each
//     probe hard-capped at probeTimeout:
//     ceil(maxFleetSize / concurrency) * probeTimeout
//     (this term alone reproduces the mismatch this function exists to fix:
//     at the production defaults, concurrency=10 and probeTimeout=15s, a
//     500-site fleet already costs ceil(500/10)*15s = 750s, twelve and a
//     half times River's 60s default.)
//  2. app-health pass (only when appProbeTimeout > 0, i.e. SetAppProber was
//     called, see NewProbeWorker/SetAppProber's zero-value-safe doc
//     comments): appProbeDue buckets sites into `ratio` = max(1,
//     appProbeInterval / probeInterval) roughly-even groups keyed on site
//     ID (mirroring appProbeDue's own defaults and integer arithmetic
//     exactly, so this budget always matches what Sweep actually does at
//     runtime), so AT MOST ceil(maxFleetSize / ratio) sites attempt an app
//     probe on any single tick, each bounded by appProbeTimeout under the
//     separate `appSem` semaphore (also sized `concurrency`):
//     ceil(ceil(maxFleetSize / ratio) / concurrency) * appProbeTimeout
//  3. headroom, mirroring update.DeriveApplyJobTimeout's `+2*time.Minute`:
//     scheduling jitter, the sequential per-site TransitionAlertState /
//     SetSiteHealth writes that run after wg.Wait() (cheap per site, but not
//     exactly zero at fleet scale), and appProbeDue's bucketing being only
//     APPROXIMATELY even (a real hash is not a perfectly balanced
//     partition): a protective margin, not a separately itemized worst
//     case.
//
// maxFleetSize is deliberately NOT "however many sites are enrolled right
// now", see DefaultMaxFleetSizeForProbeTimeout's doc comment for why a live
// count would be the wrong input here. A value <= 0 falls back to
// DefaultMaxFleetSizeForProbeTimeout.
//
// With the production defaults (concurrency=10, probeTimeout=15s,
// appProbeTimeout=10s, probeInterval=60s, appProbeInterval=300s so ratio=5,
// maxFleetSize=DefaultMaxFleetSizeForProbeTimeout=2000) this computes to
// 50m0s (reachability) + 6m40s (app) + 2m (headroom) = 58m40s.
//
// Returns 0 (defer to river.Config.JobTimeout) when concurrency or
// probeTimeout is not positive, so a zero/misconfigured input never produces
// a misleadingly small nonzero timeout that omits the reachability pass it is
// meant to cover.
func DeriveProbeJobTimeout(concurrency int, probeTimeout, probeInterval, appProbeInterval, appProbeTimeout time.Duration, maxFleetSize int) time.Duration {
	if concurrency <= 0 || probeTimeout <= 0 {
		return 0
	}
	if maxFleetSize <= 0 {
		maxFleetSize = DefaultMaxFleetSizeForProbeTimeout
	}

	reachabilityPass := time.Duration(ceilDivInt(maxFleetSize, concurrency)) * probeTimeout

	var appPass time.Duration
	if appProbeTimeout > 0 {
		// Mirror appProbeDue's own interval defaults and ratio arithmetic
		// exactly (see its doc comment) so this budget always matches what
		// Sweep will actually do at runtime.
		pi := probeInterval
		if pi <= 0 {
			pi = time.Minute
		}
		api := appProbeInterval
		if api <= 0 {
			api = 5 * time.Minute
		}
		ratio := int(api / pi)
		if ratio < 1 {
			ratio = 1
		}
		appSites := ceilDivInt(maxFleetSize, ratio)
		appPass = time.Duration(ceilDivInt(appSites, concurrency)) * appProbeTimeout
	}

	const headroom = 2 * time.Minute
	return reachabilityPass + appPass + headroom
}

// ceilDivInt returns ceil(n/d) for n >= 0 and d >= 1, without floating point.
// d <= 0 is treated as 1 (never divides by zero); n <= 0 returns 0.
func ceilDivInt(n, d int) int {
	if d <= 0 {
		d = 1
	}
	if n <= 0 {
		return 0
	}
	return (n + d - 1) / d
}

// probeAdmissionBudget is the worst-case wall-clock ONE already-admitted
// site's goroutine can still take to finish (see Sweep): the reachability
// probe (w.prober's own configured timeout) plus, when the app-health probe
// is wired (SetAppProber), its own timeout too. These are additive rather
// than overlapping for a single site because the app probe only starts AFTER
// the reachability probe finishes and releases `sem` (see the release-order
// comment in Sweep).
func (w *ProbeWorker) probeAdmissionBudget() time.Duration {
	if w.prober == nil {
		return 0
	}
	budget := w.prober.timeout
	if w.appProber != nil {
		budget += w.appProber.timeout
	}
	return budget
}

// sweepBudgetExhausted reports whether the sweep's job context is close
// enough to its own deadline that admitting one more site risks that site's
// probe being abruptly cancelled mid-flight instead of completing and being
// recorded. This is Sweep's graceful-degradation backstop for the case
// DeriveProbeJobTimeout budgets for (see its doc comment): a fleet that has
// grown past the configured/default ceiling. A context with no deadline
// (context.Background(), every existing unit/integration test in this
// package) never trips this: it is not a change to normal-sized-fleet
// behavior, only a backstop for when the job's own deadline is genuinely
// close. admissionBudget <= 0 (an unconfigured prober, never happens via
// NewProbeWorker, but guarded defensively) also never trips this, since there
// is then no meaningful worst case to compare against.
func sweepBudgetExhausted(ctx context.Context, admissionBudget time.Duration) bool {
	if admissionBudget <= 0 {
		return false
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return false
	}
	return time.Until(deadline) < admissionBudget
}

// Work runs one sweep.
func (w *ProbeWorker) Work(ctx context.Context, _ *river.Job[ProbeArgs]) error {
	_, err := w.Sweep(ctx, time.Now())
	return err
}

// Sweep probes every enrolled site, records the results, and processes alerts.
// It returns the number of sites probed. Exposed (not just Work) so it is
// directly testable without River. A per-site failure is logged and does not
// abort the sweep.
// GH #414 phase 2: the sweep enumerates only MONITORED sites
// (monitoring_paused_at IS NULL). This is the first of the two gates; the
// second is the fresh re-read in fire()/fireApp(), which catches a probe that
// was already admitted when the pause landed.
//
// WHAT HAPPENS TO A SITE THAT IS ALREADY IN AN OPEN INCIDENT WHEN IT IS
// PAUSED. Decided deliberately; the choice is LEAVE IT OPEN AND SILENT, and it
// is implemented by NOT writing any code here at all - which is the point.
//
//   - Not resolved. Closing the incident would stamp a recovery that never
//     happened: site_incidents' closing write is what the dashboard and the
//     incident history read as "the site came back". Pausing a notification
//     is not evidence about the site. An incident history that claims a
//     recovery is a lie that outlives the pause, and this feature's whole
//     promise is "do not tell me", never "lie to me".
//   - Not kept alerting. That is simply the feature not working.
//   - So: the incident row stays open and untouched, and no further alert is
//     dispatched for it. The state machine is not advanced either, because the
//     site is no longer probed - there are no observations to advance it with.
//
// The operator can tell exactly what happened afterwards, from three durable
// facts that need no extra bookkeeping: sites.monitoring_paused_at /
// _paused_by / _paused_reason say who paused it, when and why; the incident
// stays open with its real opened-at and its last real probe data; and the gap
// in site_uptime_probes over the paused window is visible in the same UI. On
// resume the state machine picks up from the state it was left in: if the site
// is still down, ConsecutiveDown is already past the threshold and no
// duplicate down-alert fires; if it recovered while paused, the first probe
// after resume produces a genuine FireRecovery and closes the incident with a
// timestamp that is real - late, but real. Neither path invents an observation.
func (w *ProbeWorker) Sweep(ctx context.Context, now time.Time) (int, error) {
	sites, err := w.repo.ListEnrolledForMonitoringProbe(ctx)
	if err != nil {
		return 0, err
	}
	if len(sites) == 0 {
		return 0, nil
	}

	var (
		mu         sync.Mutex
		checks     []metrics.Check
		results    = make(map[uuid.UUID]ProbeResult, len(sites))
		appResults = make(map[uuid.UUID]AppProbeResult, len(sites))
		sem        = make(chan struct{}, w.concurrency)
		// appSem is a SEPARATE, equally-sized semaphore for the GH #291
		// Phase 2 app probe - see the release-order comment below for why
		// this exists instead of reusing sem.
		appSem = make(chan struct{}, w.concurrency)
		wg     sync.WaitGroup
	)

	// GH follow-up (job-timeout mismatch): stop ADMITTING new sites once the
	// job's own remaining context budget can no longer safely cover one more
	// site's worst case (see sweepBudgetExhausted / probeAdmissionBudget /
	// DeriveProbeJobTimeout). Sites admitted before the cutoff are completely
	// unaffected by this: they run to completion and are recorded exactly as
	// before. `sites` is reassigned to the admitted prefix below so every
	// later use in this function (InsertChecks/UpsertRollup already only ever
	// see admitted results via `checks`; the alert-processing loop and the
	// returned count both range over `sites` directly) automatically covers
	// only what was actually probed. A skipped site is left completely
	// untouched (no health_status write, no alert-state transition) rather
	// than misread as "down", which reading its zero-value ProbeResult would
	// otherwise produce.
	admissionBudget := w.probeAdmissionBudget()
	admitted := len(sites)
	for i, s := range sites {
		if sweepBudgetExhausted(ctx, admissionBudget) {
			admitted = i
			break
		}
		s := s
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			res := w.prober.Probe(ctx, s.URL)
			// Release the reachability slot as soon as the reachability
			// probe itself finishes, BEFORE the (optional) app probe below
			// - not via a single defer covering the whole goroutine. sem
			// exists to bound how many probes hit the network at once for
			// the reachability sweep specifically ("bounds concurrency ...
			// so one sweep cannot stampede the network", ProbeWorker's own
			// doc comment); if the app probe held this same slot for
			// however long it additionally takes (review finding: up to
			// 2*appProber.timeout after the FIX 4 change), a fleet-wide app
			// probe would proportionally slow the reachability sweep for
			// sites that have nothing to do with it. appSem below bounds
			// the app probe's own concurrency instead, so it still cannot
			// stampede the network, but on its OWN budget.
			<-sem
			checkedAt := time.Now()

			// GH #291 Phase 2: piggyback an application-health probe onto
			// THIS reachability check when the worker has one configured
			// (SetAppProber) AND this site is due this tick (appProbeDue).
			// Attaching it to the SAME Check below - never a separate write
			// - is what keeps site_uptime_probes/site_uptime_daily free of
			// the "second row corrupts every aggregate" trap (see the
			// design doc and metrics.pgStore.UpsertRollup's doc comment).
			var appUp *bool
			var appReason string
			if w.appProber != nil && appProbeDue(s.ID, now, w.probeInterval, w.appProbeInterval) {
				appSem <- struct{}{}
				ar := w.appProber.Probe(ctx, s.URL, s.LastSeenAt, w.appProbeInterval, s.AppProbePath)
				<-appSem
				appUp = ar.Up
				appReason = ar.Reason
			}

			mu.Lock()
			results[s.ID] = res
			// GH #291 Phase 3: carried separately from `checks` (which is
			// batch-written to the metrics store and never read back this
			// tick) so the sequential alert-transition loop below knows
			// per-site whether the app probe attempted a verdict THIS tick
			// (appResults[s.ID].Reason != "") without re-deriving it from
			// appProbeDue a second time.
			appResults[s.ID] = AppProbeResult{Up: appUp, Reason: appReason}
			checks = append(checks, metrics.Check{
				CheckedAt:      checkedAt,
				TenantID:       s.TenantID,
				SiteID:         s.ID,
				Up:             res.Up,
				HTTPStatus:     uint16(res.HTTPStatus),
				DNSMs:          res.DNSMs,
				ConnectMs:      res.ConnectMs,
				TLSMs:          res.TLSMs,
				TTFBMs:         res.TTFBMs,
				TotalMs:        res.TotalMs,
				TLSExpiry:      res.TLSExpiry,
				TLSIssuer:      res.TLSIssuer,
				TLSSubject:     res.TLSSubject,
				Error:          res.Error,
				AppUp:          appUp,
				AppProbeReason: appReason,
			})
			mu.Unlock()
		}()
	}
	wg.Wait()

	if admitted < len(sites) {
		// The gap this closes: previously River would simply cancel the
		// whole job at the Timeout() deadline, leaving no error and no
		// record explaining which sites were skipped. This log line, plus
		// the fact that `sites` below only covers what was actually
		// admitted, is what turns that into a documented partial result
		// instead of a silent hole in uptime history. The next periodic
		// tick (ProbeInterval, ~60s) picks up every site regardless, so this
		// sweep does not need to remember which ones it skipped.
		w.logger.Warn("uptime sweep: job budget nearly exhausted; stopped admitting new sites mid-sweep",
			slog.Int("sites_total", len(sites)),
			slog.Int("sites_admitted", admitted),
			slog.Int("sites_skipped", len(sites)-admitted))
		sites = sites[:admitted]
	}

	// Batch-write the time-series (no-op when ClickHouse is disabled).
	if err := w.store.InsertChecks(ctx, checks); err != nil {
		w.logger.Warn("uptime: clickhouse insert failed", slog.Any("error", err))
	}

	// m99: fold this sweep into the site_uptime_daily/site_uptime_status
	// rollup so QueryFleetUptime (the /api/v1/sites uptime enrichment) never
	// has to scan site_uptime_probes. Only the Postgres backend implements
	// RollupWriter (ClickHouse deployments don't need it — see the
	// RollupWriter doc comment); the type assertion makes this a clean no-op
	// on ClickHouse and on every test double that doesn't implement it. A
	// SEPARATE, best-effort step from InsertChecks above: a rollup failure
	// must never affect probing/alerting, which is why it is only logged.
	if rw, ok := w.store.(metrics.RollupWriter); ok {
		if err := rw.UpsertRollup(ctx, checks); err != nil {
			w.logger.Warn("uptime: rollup upsert failed", slog.Any("error", err))
		}
	}

	// Update health_status + alert state per site (Postgres). Sequential: these
	// are cheap RLS-scoped writes and avoid hammering the pool. GH #291 Phase
	// 3: each site's app-health transition (when the app probe attempted a
	// verdict this tick) is ALSO evaluated+persisted here, inside the SAME
	// TransitionAlertState call/transaction as the reachability transition -
	// but its DISPATCH is deferred (collected into pending) until every
	// site's state for this tick has been persisted, so the fleet circuit
	// breaker (resolveAppAlerts, below) evaluates a ratio that reflects the
	// FULL tick rather than a partial one.
	var pending []pendingAppFire
	for _, s := range sites {
		res := results[s.ID]
		app := appResults[s.ID]
		w.processSite(ctx, s, res, app, now, &pending)
	}
	// GH #291 Phase 3 Fix 4: always call resolveAppAlerts, even with zero
	// pending transitions this tick - a tenant whose breaker is currently
	// tripped must still be re-evaluated so it can converge (see
	// resolveAppAlerts's own doc comment). resolveAppAlerts itself keeps this
	// cheap: it is a single bounded query (never per-tenant) plus, only for
	// tenants that actually need it, the existing per-tenant ratio/transition
	// work.
	w.resolveAppAlerts(ctx, pending, now)
	return len(sites), nil
}

// appAlertEligible reports whether a site may participate in app-health
// alerting at all - the SAME predicate GetTenantAppAlertRatio's WHERE clause
// enforces server-side for its eligible/down counts (app_alerts_disabled =
// false AND connection_state NOT IN ('revoked','archived')), applied here so
// the fire path (processSite) and the fleet circuit breaker's ratio can
// never disagree about which sites count (GH #291 Phase 3 Fix 2: previously
// processSite gated ONLY on AppAlertsDisabled, so a revoked/archived site
// could still open/close its own app-health incident and dispatch an
// individual alert while GetTenantAppAlertRatio silently excluded it from
// both the eligible and down counts - contributing to the fire path but not
// the denominator, which could either bypass the breaker for a real
// multi-site incident diluted by an uncounted revoked site, or trip it on a
// ratio that does not match what was actually dispatched).
//
// TWO of GetTenantAppAlertRatio's legs are deliberately NOT restated here,
// each because a layer BELOW this one already makes them unreachable:
//
//   - ever_app_up: EvaluateApp itself refuses to fire (FireDown or
//     FireRecovery) for a site with EverAppUp=false, so such a site can never
//     reach `pending` in the first place.
//   - monitoring_paused_at IS NULL (m117, GH #414 phase 2): a paused site is
//     not enumerated by ListEnrolledForMonitoringProbe, so it is never
//     probed, never evaluated and never lands in `pending`; and the one case
//     that gets past that - a probe already in flight when the pause landed -
//     is caught by monitoringPaused at dispatch, with a read fresher than
//     anything a per-site predicate here could do. EnrolledSite does not even
//     carry the column, deliberately (see the note on
//     ListEnrolledSitesForProbe): a snapshot value would be a STALE pause,
//     which is the one thing this feature cannot afford.
//
// Restating either here would be redundant, never a second source of truth to
// drift out of sync. See TestAppAlertEligibleMatchesRatioQueryPredicate, which
// pins this function's inputs against the SQL predicate's literal text.
func appAlertEligible(s EnrolledSite) bool {
	return !s.AppAlertsDisabled && s.ConnectionState != "revoked" && s.ConnectionState != "archived"
}

// appFireDownOnly filters pending fires down to the ones that opened a NEW
// incident this tick (tr.FireDown), discarding the recovery-typed ones. Used
// by resolveAppAlerts's FireRecovery branch (Fix 1): the aggregate recovery
// notification already speaks for every RECOVERY transition collected this
// tick, but a same-tick NEW incident is not covered by that statement and
// must still reach fireAppIndividually - see Fix 1's doc comment on the
// FireRecovery branch for the full reasoning.
func appFireDownOnly(fires []pendingAppFire) []pendingAppFire {
	out := make([]pendingAppFire, 0, len(fires))
	for _, p := range fires {
		if p.tr.FireDown {
			out = append(out, p)
		}
	}
	return out
}

// pendingAppFire is one site's app-health transition awaiting dispatch,
// collected across the whole sweep so the fleet circuit breaker
// (resolveAppAlerts) can be evaluated ONCE per tenant, after every site's
// alert-state transition for this tick has already been persisted.
type pendingAppFire struct {
	site      EnrolledSite
	tr        AppTransition
	appReason string
}

// processSite refreshes the site's health_status and runs the alert state
// machine, firing a transition alert when warranted. GH #291 Phase 3: also
// runs the app-health transition (site_app_alert_state) in the SAME
// TransitionAlertState call, and queues a pending app-alert dispatch onto
// `pending` when it fired - the caller resolves pending fires after the
// whole sweep's per-site loop completes (see resolveAppAlerts).
func (w *ProbeWorker) processSite(ctx context.Context, s EnrolledSite, res ProbeResult, app AppProbeResult, now time.Time, pending *[]pendingAppFire) {
	// Refresh Postgres health_status from the probe (the M5 refinement of M2's
	// freshness-based status).
	status := HealthHealthy
	if !res.Up {
		status = HealthUnreachable
	}
	if _, err := w.repo.SetSiteHealth(ctx, s.ID, status); err != nil {
		w.logger.Warn("uptime: set health failed", slog.String("site_id", s.ID.String()), slog.Any("error", err))
	}

	// appAttempted is false whenever the app probe did not produce a verdict
	// this tick at all (app.Reason == "" - the common case, since the app
	// probe runs on a slower cadence than the reachability probe; see
	// AppProbeResult's doc comment: Reason is ALWAYS populated when a probe
	// actually ran) OR the site is not app-alert-eligible (appAlertEligible -
	// GH #291 Phase 3 Fix 2: the SAME predicate GetTenantAppAlertRatio
	// enforces server-side, so the fire path and the breaker's ratio can
	// never disagree about which sites count). The probe itself keeps
	// running and the dashboard stays accurate either way; only this
	// alerting step is skipped, and site_app_alert_state is left completely
	// untouched so a later opt-in/un-revoke starts from a clean slate rather
	// than a primed counter.
	appAttempted := appAlertEligible(s) && app.Reason != ""
	appThreshold := w.appAlertThreshold
	if appThreshold < 1 {
		appThreshold = defaultAppAlertThreshold
	}

	// Atomically read (locked), evaluate, and persist BOTH the reachability
	// and app-health alert-state transitions in one transaction - see
	// TransitionAlertState for why this must NOT be split into a separate
	// get + upsert (lost-update race under overlapping sweeps).
	tr, appTr, err := w.repo.TransitionAlertState(ctx, s.ID, s.TenantID, res.Up, w.threshold, now, res.HTTPStatus, res.Error,
		appAttempted, app.Up, app.Reason, appThreshold)
	if err != nil {
		w.logger.Warn("uptime: transition alert state failed", slog.String("site_id", s.ID.String()), slog.Any("error", err))
		return
	}

	if tr.FireDown || tr.FireRecovery {
		w.fire(ctx, s, res, tr, now)
	}

	if appAttempted && (appTr.FireDown || appTr.FireRecovery) && pending != nil {
		*pending = append(*pending, pendingAppFire{site: s, tr: appTr, appReason: app.Reason})
	}
}

// monitoringPaused is the DISPATCH-side half of the pause gate (m117, GH #414
// phase 2): it re-reads sites.monitoring_paused_at at fire time and reports
// whether this site's uptime alert must be withheld.
//
// WHY A SECOND CHECK EXISTS AT ALL. Sweep already refuses to enumerate a paused
// site, so in the steady state this never returns true. It exists for the one
// window the selection filter cannot cover: phase 1 deliberately does not drain
// jobs that are already queued, so a probe admitted a second before the pause
// landed runs to completion and arrives here carrying a snapshot taken while
// the site was still active. Only a fresh read sees the pause. Without this,
// pausing during an in-flight sweep still pages someone.
//
// WHY IT WITHHOLDS THE DISPATCH AND NOTHING ELSE. Everything upstream of this
// point has already run and is left exactly as it was: the probe result is
// recorded, health_status is refreshed, and TransitionAlertState has persisted
// the transition and opened or closed the incident. That is deliberate. Pause
// means "do not tell me", never "lie to me" - the record of what the site
// actually did stays true, and only the notification is withheld. An incident
// that opens in this window is therefore open, real, visible in the UI, and
// silent, which is the same end state as an incident that was already open when
// the pause landed (see Sweep's doc comment).
//
// HOW IT COMPOSES WITH THE FLEET CIRCUIT BREAKER. The breaker's counts come
// from GetTenantAppAlertRatio, and that query now carries the pause predicate
// too - on BOTH sides of the ratio, numerator and denominator. It has to: a
// paused site is not probed, so its site_app_alert_state.in_incident is frozen
// at whatever it was when the pause landed. Pause a site while its app-health
// incident is OPEN and, without the predicate, that frozen `true` counts as
// down forever, and two of them permanently trip the breaker for any tenant
// with seven or fewer eligible sites - which suppresses the individual alerts
// of every ACTIVE site in that tenant, indefinitely. That is the worst outcome
// this feature can produce: a site nobody paused going silent. Filtering the
// numerator alone would not fix it, only move it - the paused row would keep
// padding the denominator. This gate (fire, fireApp) and fireAppAggregate's
// population gate are the DISPATCH-side complement of that query: they cover
// the window between the query running and the notification going out.
//
// A DB error fails OPEN: log it and dispatch. The alternative - swallowing an
// alert because a pause lookup failed - turns a transient database blip into
// silent monitoring, which is the failure this whole feature is supposed to
// make deliberate and visible rather than accidental.
func (w *ProbeWorker) monitoringPaused(ctx context.Context, s EnrolledSite, kind string) bool {
	paused, err := w.repo.IsMonitoringPaused(ctx, s.ID)
	if err != nil {
		w.logger.Warn("uptime: monitoring pause check failed, dispatching anyway",
			slog.String("site_id", s.ID.String()),
			slog.String("alert_kind", kind),
			slog.Any("error", err))
		return false
	}
	if paused {
		w.logger.Info("uptime: alert withheld, monitoring paused",
			slog.String("site_id", s.ID.String()),
			slog.String("tenant_id", s.TenantID.String()),
			slog.String("alert_kind", kind))
	}
	return paused
}

// fire resolves the tenant's alert config and dispatches the transition alert.
func (w *ProbeWorker) fire(ctx context.Context, s EnrolledSite, res ProbeResult, tr Transition, now time.Time) {
	if w.dispatcher == nil {
		return
	}
	if w.monitoringPaused(ctx, s, "reachability") {
		return
	}
	cfg, found, err := w.repo.GetAlertConfig(ctx, s.TenantID)
	if err != nil {
		w.logger.Warn("uptime: get alert config failed", slog.String("tenant_id", s.TenantID.String()), slog.Any("error", err))
		return
	}
	if !found || !cfg.Enabled {
		return // no channel configured (or disabled): nothing to deliver, but the
		// transition state was still recorded above so we don't re-fire later.
	}

	name := s.URL
	if w.sites != nil {
		if n := w.sites.SiteName(ctx, s.TenantID, s.ID); n != "" {
			name = n
		}
	}
	alert := Alert{
		TenantID:   s.TenantID,
		SiteID:     s.ID,
		SiteURL:    s.URL,
		SiteName:   name,
		HTTPStatus: res.HTTPStatus,
		Error:      res.Error,
		FiredAt:    now,
	}
	if tr.FireRecovery {
		alert.Kind = AlertRecovery
	} else {
		alert.Kind = AlertDown
	}
	w.dispatcher.Fire(ctx, cfg, alert)
}

// ---------------------------------------------------------------------------
// GH #291 Phase 3 - app-health alert dispatch + fleet circuit breaker.
// ---------------------------------------------------------------------------

// resolveAppAlerts evaluates the fleet circuit breaker for every tenant that
// either had at least one app-health transition this sweep tick OR whose
// breaker is currently tripped (Fix 4, below), and dispatches EITHER each
// tenant's individual per-site app alert(s) OR, when the breaker is tripped,
// collapses them into ONE aggregate notification instead (design doc section
// 2: "far more likely to be our own monitoring, or a shared host/network,
// having a bad day than N unrelated clients breaking at once"). Called ONCE
// per sweep, after every site's TransitionAlertState call has already
// persisted its own state for this tick - so the ratio GetTenantAppAlertRatio
// reads reflects the FULL tick, not a partial one.
//
// GH #291 Phase 3 Fix 4 (stale trip): a tenant with zero pending transitions
// this tick is NOT excluded here merely for that reason - if its breaker is
// CURRENTLY tripped, it is folded into the same per-tenant loop below with an
// empty fires list. Without this, a tenant whose down sites simply stop
// transitioning (already down and staying down - nothing ever lands in
// `pending` for it again) could never re-converge, and the ratio CAN move for
// reasons that never touch an individual site's app-alert state at all (a
// down site gets archived/revoked, or per-site alerting gets disabled,
// shrinking the eligible/down counts with no AppTransition). This costs
// exactly ONE extra query per sweep tick (ListTrippedAppAlertBreakerTenants) -
// never a per-tenant query - because the tripped set is small by definition
// (a breaker only trips when a meaningful fraction of a tenant's sites are
// simultaneously down). A tenant that is neither pending nor tripped costs
// nothing beyond that one bounded query.
func (w *ProbeWorker) resolveAppAlerts(ctx context.Context, pending []pendingAppFire, now time.Time) {
	if w.dispatcher == nil {
		return
	}
	byTenant := make(map[uuid.UUID][]pendingAppFire, len(pending))
	for _, p := range pending {
		byTenant[p.site.TenantID] = append(byTenant[p.site.TenantID], p)
	}

	tripped, err := w.repo.ListTrippedAppAlertBreakerTenants(ctx)
	if err != nil {
		w.logger.Warn("uptime: list tripped app alert breaker tenants failed", slog.Any("error", err))
	}
	for _, tenantID := range tripped {
		if _, ok := byTenant[tenantID]; !ok {
			byTenant[tenantID] = nil
		}
	}
	if len(byTenant) == 0 {
		return
	}

	ratio := w.breakerRatio()

	for tenantID, fires := range byTenant {
		eligible, down, err := w.repo.GetTenantAppAlertRatio(ctx, tenantID)
		if err != nil {
			w.logger.Warn("uptime: get tenant app alert ratio failed",
				slog.String("tenant_id", tenantID.String()), slog.Any("error", err))
			// Fail safe toward the ORIGINAL per-site behavior rather than
			// silently dropping the alert: a ratio-query failure must not
			// turn into a swallowed incident notification.
			w.fireAppIndividually(ctx, fires, now)
			continue
		}
		// Strict ">" - "MORE than a configurable ratio" (design doc section
		// 2), not ">=". eligible can legitimately be 0 here for a tenant
		// folded in ONLY via the tripped-tenant list above (Fix 4) with an
		// empty `fires` - e.g. every one of its down sites got archived
		// between ticks - so the guard is load-bearing, not merely
		// defensive, and avoids a division by zero.
		//
		// minAppAlertBreakerDownCount is a SECOND, absolute condition, ANDed
		// with the ratio: a lone site going down is 100% of a 1-site
		// tenant's eligible population, which would otherwise trip the
		// breaker on every single-site down event - exactly the ordinary,
		// expected case this feature must NOT interfere with. "Many sites
		// breaking at once" is not a meaningful concept below 2 sites, so
		// the breaker never engages until at least 2 are simultaneously
		// down, regardless of how small the eligible population is.
		wantTrip := appBreakerBarMet(eligible, down, ratio)

		// GH #414 phase 3. A trip about to NAME this tick's fires is the one
		// path whose counts can be contradicted between the ratio read above
		// and the mail: an operator pausing a site in that window leaves the
		// numbers pre-pause and the names post-pause. Substantiate the
		// population HERE, before the breaker records what we are about to
		// say, so the stored last_down_count is the count the mail quotes
		// (phase 3 finding: the mail said "2/3" while the row kept 3, and the
		// genuine later worsening to 3-of-3 was then swallowed by
		// wantBreakerUpdate's `down <= prev.LastDownCount`). Re-deciding
		// wantTrip on the substantiated numbers is the same act: a population
		// that no longer clears the bar must not trip the breaker at all,
		// rather than trip it and then send "1/3 sites are simultaneously
		// app-down", which reads as nonsense and suppresses every per-site
		// alert behind it.
		//
		// `pop.resolved == false` means the pause landed and the re-read that
		// would price it failed, so this tick cannot say how many sites are
		// down. Fall back to the ORIGINAL per-site behaviour exactly as the
		// ratio-query failure above does: never trip on numbers we could not
		// substantiate, and let the per-site gates (each of which re-reads the
		// pause itself) decide what goes out.
		//
		// `pop` is SEEDED with the live counts and only ever REPLACED by a
		// resolved population, so from here down it is the single source every
		// branch reads - the breaker row, the trip mail, the update mail and
		// the recovery mail all quote the same struct, and there is no state in
		// which a branch can read a zero value. It used to be declared `var pop
		// appAggregatePopulation` and filled in only under `wantTrip &&
		// len(fires) > 0`, while the FireTrip branch below read pop.down /
		// pop.eligible / pop.names unconditionally. A tenant folded in from
		// ListTrippedAppAlertBreakerTenants carries no fires, so it took the
		// zero value: if the locking re-read inside TransitionAppAlertBreaker
		// then found the breaker NOT tripped (an overlapping sweep having just
		// cleared it - ProbeArgs runs on QueueDefault with MaxWorkers 5 and a
		// sweep longer than its interval overlaps the next), EvaluateAppBreaker
		// returned FireTrip, the alert was built as 0 down of 0 eligible, and
		// fireAppAggregate's empty-population gate withheld it. The breaker was
		// left tripped - suppressing every per-site app alert for the tenant -
		// with nothing sent and no way back, because wantBreakerUpdate only
		// fires again once `down` RISES above the LastDownCount the row had
		// already been given from the live count. A tenant nobody paused, silent
		// for the whole outage.
		//
		// The population is therefore substantiated for EVERY tenant that
		// clears the bar, with or without fires this tick. It is skipped only
		// when wantTrip is false, which is exactly the set of branches that
		// cannot fire a trip or an update (see EvaluateAppBreaker: both require
		// wantTrip), and the seeded live counts serve the recovery mail there.
		pop := appAggregatePopulation{eligible: eligible, down: down, resolved: true}
		if wantTrip {
			pop = w.appAggregatePopulation(ctx, tenantID, eligible, down, fires)
			if !pop.resolved {
				w.fireAppIndividually(ctx, fires, now)
				continue
			}
			wantTrip = appBreakerBarMet(pop.eligible, pop.down, ratio)
		}

		brTr, err := w.repo.TransitionAppAlertBreaker(ctx, tenantID, wantTrip, pop.down, now)
		if err != nil {
			w.logger.Warn("uptime: transition app alert breaker failed",
				slog.String("tenant_id", tenantID.String()), slog.Any("error", err))
			w.fireAppIndividually(ctx, fires, now)
			continue
		}

		switch {
		case brTr.NewState.Tripped:
			// Suppressed: no individual per-site dispatch while tripped,
			// regardless of whether this tick's fires are new incidents or
			// recoveries - that is the entire point of the breaker. Within
			// that, THREE distinct notifications are possible:
			switch {
			case brTr.FireTrip:
				// The transition INTO tripped: fire the initial aggregate,
				// naming the population being suppressed.
				//
				// Counts AND names both come from the `pop` resolved above -
				// one population, decided once, so the subject cannot
				// contradict the body and the breaker row cannot contradict
				// either. `affected` is passed nil precisely because the
				// population is already resolved; fireAppAggregate re-resolves
				// only for a caller that hands it raw fires.
				w.fireAppAggregate(ctx, tenantID, AppAggregateAlert{
					Recovered:       false,
					TenantID:        tenantID,
					DownCount:       pop.down,
					EligibleCount:   pop.eligible,
					SuppressedSites: pop.names,
					FiredAt:         now,
				}, nil)
			case brTr.FireUpdate:
				// Fix 3: still tripped, but materially worse than the last
				// notification (and at least appAlertBreakerUpdateMinInterval
				// has elapsed) - tell the operator instead of staying silent
				// until the eventual recovery, which would otherwise leave
				// them believing nothing has changed since the original trip.
				//
				// Counts and names come from the SAME `pop` the trip above
				// uses and the breaker row was just written from. This branch
				// used to quote the live counts over a list read separately
				// from ListTenantAppDownSites a moment later, which is two
				// populations at two instants: a folded-in tenant sent "3/3
				// sites are simultaneously app-down" over a body naming 2.
				// appAggregatePopulation now names the live down set for every
				// caller, so an update several ticks after the sites it
				// describes went down still names them - the reason this branch
				// read the live set in the first place - without the count and
				// the list being able to disagree.
				w.fireAppAggregate(ctx, tenantID, AppAggregateAlert{
					Recovered:       false,
					Updated:         true,
					TenantID:        tenantID,
					DownCount:       pop.down,
					EligibleCount:   pop.eligible,
					SuppressedSites: pop.names,
					FiredAt:         now,
				}, nil)
			}
			// Steady tripped tick with neither FireTrip nor FireUpdate:
			// silent, exactly like an already-open site incident does not
			// re-alert.
		case brTr.FireRecovery:
			// Fix 1: the aggregate speaks for every RECOVERY transition
			// collected this tick (they were suppressed while tripped -
			// dispatching them individually here too would double-notify the
			// exact recovery the aggregate just announced). A transition NOT
			// covered by that statement - a site that crossed newly INTO an
			// incident on this exact tick, coexisting with enough OTHER
			// sites recovering to pull the ratio itself back under threshold -
			// is a genuinely new, unrelated outage the aggregate says
			// nothing about. AppTransition is transition-only (it never
			// re-fires once consumed), so failing to dispatch it here would
			// swallow that incident PERMANENTLY, not merely delay it to the
			// next tick - it MUST still be dispatched individually.
			w.fireAppAggregate(ctx, tenantID, AppAggregateAlert{
				Recovered:     true,
				TenantID:      tenantID,
				DownCount:     pop.down,
				EligibleCount: pop.eligible,
				FiredAt:       now,
			}, nil)
			w.fireAppIndividually(ctx, appFireDownOnly(fires), now)
		default:
			// Never tripped, or recovered on a PRIOR tick and steady since:
			// dispatch this tick's individual fires normally.
			w.fireAppIndividually(ctx, fires, now)
		}
	}
}

// appFireDisplayNames resolves a display name (site name, falling back to
// URL) for each pending fire, for the aggregate notification's "what was
// suppressed" list.
func (w *ProbeWorker) appFireDisplayNames(ctx context.Context, fires []pendingAppFire) []string {
	names := make([]string, 0, len(fires))
	for _, p := range fires {
		name := p.site.URL
		if w.sites != nil {
			if n := w.sites.SiteName(ctx, p.site.TenantID, p.site.ID); n != "" {
				name = n
			}
		}
		names = append(names, name)
	}
	return names
}

// fireAppIndividually dispatches each pending fire's per-site app alert
// normally (the breaker is not suppressing this tenant this tick).
func (w *ProbeWorker) fireAppIndividually(ctx context.Context, fires []pendingAppFire, now time.Time) {
	for _, p := range fires {
		w.fireApp(ctx, p.site, p.tr, p.appReason, now)
	}
}

// fireApp resolves the tenant's alert config and dispatches ONE app-health
// alert. Gated on cfg.AppAlertsEnabled IN ADDITION to cfg.Enabled (mirrors
// the NotifySecurity/NotifyVulns precedent - see
// cmd/wpmgr/siteadapter.go's activitySecurityAlerter.NotifySecurity): a
// tenant that already has reachability alerts on does not silently start
// receiving app-health alerts too.
func (w *ProbeWorker) fireApp(ctx context.Context, s EnrolledSite, tr AppTransition, appReason string, now time.Time) {
	if w.dispatcher == nil {
		return
	}
	if w.monitoringPaused(ctx, s, "app") {
		return
	}
	cfg, found, err := w.repo.GetAlertConfig(ctx, s.TenantID)
	if err != nil {
		w.logger.Warn("uptime: get alert config failed (app)", slog.String("tenant_id", s.TenantID.String()), slog.Any("error", err))
		return
	}
	if !found || !cfg.Enabled || !cfg.AppAlertsEnabled {
		return // transition state was still recorded above so we don't re-fire later.
	}

	name := s.URL
	if w.sites != nil {
		if n := w.sites.SiteName(ctx, s.TenantID, s.ID); n != "" {
			name = n
		}
	}
	alert := Alert{
		TenantID: s.TenantID,
		SiteID:   s.ID,
		SiteURL:  s.URL,
		SiteName: name,
		Error:    appReason,
		FiredAt:  now,
	}
	if tr.FireRecovery {
		alert.Kind = AlertAppRecovery
	} else {
		alert.Kind = AlertAppDown
	}
	w.dispatcher.Fire(ctx, cfg, alert)
}

// appFireUnpaused returns the subset of fires whose site is NOT currently
// monitoring-paused, using the SAME dispatch-side re-read (monitoringPaused)
// that fire and fireApp use. It is the belt to the query's braces: the ratio
// and down-list queries exclude paused sites server-side, but a site paused a
// second AFTER those queries ran is still in their result set, and this is the
// only read late enough to see it.
func (w *ProbeWorker) appFireUnpaused(ctx context.Context, fires []pendingAppFire) []pendingAppFire {
	live := make([]pendingAppFire, 0, len(fires))
	for _, p := range fires {
		if w.monitoringPaused(ctx, p.site, "app_aggregate") {
			continue
		}
		live = append(live, p)
	}
	return live
}

// breakerRatio is the configured fleet circuit breaker trip ratio, or the
// default when unset. One accessor so the bar cannot be computed from a
// different ratio in two places.
func (w *ProbeWorker) breakerRatio() float64 {
	if w.appAlertBreakerRatio <= 0 {
		return defaultAppAlertBreakerRatio
	}
	return w.appAlertBreakerRatio
}

// appBreakerBarMet reports whether `down` sites out of `eligible` clear the
// fleet circuit breaker's bar - the SINGLE definition of "many sites are
// breaking at once", used both to decide the trip and to decide whether the
// aggregate notification still has anything true to say.
//
// Strict ">" - "MORE than a configurable ratio" (design doc section 2), not
// ">=". eligible can legitimately be 0, for a tenant folded in ONLY via the
// tripped-tenant list (resolveAppAlerts Fix 4) with an empty `fires` - e.g.
// every one of its down sites got archived between ticks - so the guard is
// load-bearing, not merely defensive, and avoids a division by zero.
//
// minAppAlertBreakerDownCount is a SECOND, absolute condition, ANDed with the
// ratio: a lone site going down is 100% of a 1-site tenant's eligible
// population, which would otherwise trip the breaker on every single-site down
// event - exactly the ordinary, expected case this feature must NOT interfere
// with. "Many sites breaking at once" is not a meaningful concept below 2
// sites, so the breaker never engages until at least 2 are simultaneously
// down, regardless of how small the eligible population is.
func appBreakerBarMet(eligible, down int, ratio float64) bool {
	return eligible > 0 && down >= minAppAlertBreakerDownCount && float64(down) > ratio*float64(eligible)
}

// appAggregatePopulation is the ONE thing the aggregate notification speaks
// for. Every number and every name in the mail comes from this struct, and the
// struct is decided once - the shape GH #414 phase 3 replaced four overlapping
// conditionals with, after each of the previous rounds bolted another one on
// and the mail kept finding a new way to say something untrue.
//
// resolved is the honest "I could not tell": a pause landed mid-dispatch and
// the read that would price it failed. There is no such thing as a partly
// substantiated population - a caller that gets resolved == false must not
// send, and must not record a count either.
type appAggregatePopulation struct {
	eligible int
	down     int
	names    []string
	resolved bool
}

// appAggregatePopulation resolves the population from `affected` - the tick's
// pending fires, i.e. the transitions the aggregate is standing in for.
//
// PAUSE (m117, GH #414). The aggregate is tenant-wide, so it has no single
// site to hand monitoringPaused. It re-reads the pause for each affected site
// at dispatch time, which is the only read late enough to see a pause that
// landed after the ratio query ran. A drop has two consequences, and both are
// answered here rather than at the point each symptom surfaces:
//
//   - The COUNTS must be re-derived, or the subject contradicts the body: the
//     ratio was read before the pause landed, so it still counts a site the
//     body no longer names ("3/4 sites are simultaneously app-down" over a body
//     naming 2 - phase 2 finding 3). A drop is rare (it needs a pause inside the
//     window between the ratio query and the dispatch), so re-reading the ratio
//     then, and only then, costs nothing in the ordinary case and gives counts
//     and names pause-filtered at the same instant. The re-read cannot be
//     substituted with arithmetic on the fires: a fire whose pause landed
//     BEFORE the ratio query was never in the count to subtract from, and this
//     path cannot tell the two apart.
//
//     If that re-read FAILS the population is unresolved, full stop (phase 3
//     finding B). Letting the pre-pause counts stand was the previous
//     behaviour, and it made the phase 2 fix contingent on a query succeeding:
//     with the re-read erroring, a tenant whose every site was paused was sent
//     "2/2 sites are simultaneously app-down" over a body naming none - the
//     exact page the operator had asked not to receive, produced by the loud
//     failure direction of a NOTIFICATION path. This is not a safety interlock;
//     the per-site alerts for genuinely unpaused sites are governed by fire and
//     fireApp, each of which re-reads the pause itself. So the safe direction
//     when the population cannot be determined is to withhold and log loudly.
//
//   - The NAMES are the live down set (ListTenantAppDownSites), never this
//     tick's fires. `affected` is a set of TRANSITIONS; the counts count the
//     whole down POPULATION, so naming the fires lets the mail quote a number
//     nothing in its own body substantiates. Three ways that happened: a tenant
//     folded in from the tripped list has no fires at all (round 6, the
//     FireTrip silence); an Updated notification can fire several ticks after
//     the sites it describes went down, so the fires are empty or stale (the
//     reason this branch already read the live set); and even at the instant of
//     a trip, sites that entered their incident on an EARLIER tick are in the
//     count while nothing this tick transitioned them. The live set is the same
//     server-side pause-filtered population the counts come from, so it is the
//     only list that cannot contradict them. It is bounded by the query's own
//     limit, so on a large fleet it names fewer sites than `down` - by
//     construction, not by disagreement.
//
//     This also subsumes the phase 2 finding-2 case, where the tick's
//     transitions were all paused sites while OTHER, unpaused sites held the
//     ratio above threshold: the breaker is tripped and suppressing their
//     individual alerts, so the aggregate must go out and must name them.
//
// A failure of the NAME query alone does not unresolve the population: the
// counts are still substantiated, and this tick's unpaused fires are a strictly
// worse but honest list to fall back to (every site in it is genuinely down,
// unpaused and part of the counted population). A substantiated count with a
// partial list, or with "Suppressed sites: none named", beats no mail at all.
func (w *ProbeWorker) appAggregatePopulation(ctx context.Context, tenantID uuid.UUID, eligible, down int, affected []pendingAppFire) appAggregatePopulation {
	pop := appAggregatePopulation{eligible: eligible, down: down, resolved: true}

	live := w.appFireUnpaused(ctx, affected)
	if dropped := len(affected) - len(live); dropped > 0 {
		eligible, down, err := w.repo.GetTenantAppAlertRatio(ctx, tenantID)
		if err != nil {
			w.logger.Warn("uptime: app aggregate withheld, a pause landed and the ratio re-read failed",
				slog.String("tenant_id", tenantID.String()),
				slog.Int("dropped", dropped), slog.Any("error", err))
			pop.resolved = false
			return pop
		}
		pop.eligible, pop.down = eligible, down
	}

	names, err := w.repo.ListTenantAppDownSites(ctx, tenantID, 0)
	if err != nil {
		w.logger.Warn("uptime: list tenant app down sites failed, naming this tick's unpaused fires instead",
			slog.String("tenant_id", tenantID.String()), slog.Any("error", err))
		pop.names = w.appFireDisplayNames(ctx, live)
		return pop
	}
	pop.names = names
	return pop
}

// fireAppAggregate resolves the tenant's alert config and dispatches the
// fleet circuit breaker's aggregate notification. Same AppAlertsEnabled gate
// as fireApp - the breaker's own notification is still an app-health alert.
//
// It asks ONE question, of ONE population: does this notification have
// something true to say? `affected` non-nil means the population has not been
// resolved yet and this call resolves it (see appAggregatePopulation);
// resolveAppAlerts's trip path passes nil because it resolved the same
// population a moment earlier, before recording the count in the breaker row.
// Either way the counts and the names that go out are the same population's,
// never two populations read at two instants.
//
// The send gate, in its two halves:
//
//	EMPTY POPULATION. EligibleCount is GetTenantAppAlertRatio's denominator and
//	it excludes paused sites, so zero means the aggregate speaks for no sites at
//	all - every site of the tenant is paused, archived or revoked. A
//	notification about no sites is not a safer notification, it is noise, and
//	for the paused case it is precisely the mail the operator asked not to
//	receive (phase 2 HOLE 2: a fully paused tenant received an aggregate naming
//	its paused sites).
//
//	NOTHING WORTH SAYING. A down aggregate asserts that many sites are breaking
//	at once. If the substantiated population no longer clears appBreakerBarMet,
//	that assertion is false and the mail must not go - phase 3 finding A had it
//	sending "0/4 sites are simultaneously app-down" naming nothing at all, after
//	two down sites were paused mid-dispatch, and the related low finding had it
//	sending "1/3 sites ... far more likely to be a shared host issue than 1
//	unrelated sites breaking at once". The bar is the SAME function that decided
//	the trip, so the notification can never claim a fleet-wide event on numbers
//	that would not have tripped the breaker. A recovery aggregate asserts the
//	opposite and is deliberately not subject to it: its down count is small or
//	zero by definition, which is the whole news.
func (w *ProbeWorker) fireAppAggregate(ctx context.Context, tenantID uuid.UUID, alert AppAggregateAlert, affected []pendingAppFire) {
	if w.dispatcher == nil {
		return
	}
	if len(affected) > 0 {
		pop := w.appAggregatePopulation(ctx, tenantID, alert.EligibleCount, alert.DownCount, affected)
		if !pop.resolved {
			return
		}
		alert.EligibleCount, alert.DownCount, alert.SuppressedSites = pop.eligible, pop.down, pop.names
	}
	if alert.EligibleCount == 0 {
		w.logger.Info("uptime: app aggregate withheld, no alert-eligible site left in tenant",
			slog.String("tenant_id", tenantID.String()),
			slog.Bool("recovered", alert.Recovered))
		return
	}
	if !alert.Recovered && !appBreakerBarMet(alert.EligibleCount, alert.DownCount, w.breakerRatio()) {
		w.logger.Info("uptime: app aggregate withheld, the substantiated population no longer clears the breaker bar",
			slog.String("tenant_id", tenantID.String()),
			slog.Int("down", alert.DownCount), slog.Int("eligible", alert.EligibleCount))
		return
	}
	cfg, found, err := w.repo.GetAlertConfig(ctx, tenantID)
	if err != nil {
		w.logger.Warn("uptime: get alert config failed (app aggregate)",
			slog.String("tenant_id", tenantID.String()), slog.Any("error", err))
		return
	}
	if !found || !cfg.Enabled || !cfg.AppAlertsEnabled {
		return
	}
	w.dispatcher.FireAppAggregate(ctx, cfg, alert)
}
