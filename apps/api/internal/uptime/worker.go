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

// Work runs one sweep.
func (w *ProbeWorker) Work(ctx context.Context, _ *river.Job[ProbeArgs]) error {
	_, err := w.Sweep(ctx, time.Now())
	return err
}

// Sweep probes every enrolled site, records the results, and processes alerts.
// It returns the number of sites probed. Exposed (not just Work) so it is
// directly testable without River. A per-site failure is logged and does not
// abort the sweep.
func (w *ProbeWorker) Sweep(ctx context.Context, now time.Time) (int, error) {
	sites, err := w.repo.ListEnrolledForProbe(ctx)
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

	for _, s := range sites {
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
// GetTenantAppAlertRatio's THIRD eligibility criterion, ever_app_up, is
// deliberately NOT restated here: EvaluateApp itself already refuses to fire
// (FireDown or FireRecovery) for a site with EverAppUp=false, so such a site
// can never reach `pending` in the first place - restating the same gate
// here would be redundant, never a second source of truth to drift out of
// sync. See TestAppAlertEligibleMatchesRatioQueryPredicate, which pins this
// function's three inputs against the SQL predicate's literal text.
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

// fire resolves the tenant's alert config and dispatches the transition alert.
func (w *ProbeWorker) fire(ctx context.Context, s EnrolledSite, res ProbeResult, tr Transition, now time.Time) {
	if w.dispatcher == nil {
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

	ratio := w.appAlertBreakerRatio
	if ratio <= 0 {
		ratio = defaultAppAlertBreakerRatio
	}

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
		wantTrip := eligible > 0 && down >= minAppAlertBreakerDownCount && float64(down) > ratio*float64(eligible)

		brTr, err := w.repo.TransitionAppAlertBreaker(ctx, tenantID, wantTrip, down, now)
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
				// naming everything suppressed on this first tick (at this
				// exact moment nothing has been suppressed yet, so `fires`
				// IS the complete affected population).
				w.fireAppAggregate(ctx, tenantID, AppAggregateAlert{
					Recovered:       false,
					TenantID:        tenantID,
					DownCount:       down,
					EligibleCount:   eligible,
					SuppressedSites: w.appFireDisplayNames(ctx, fires),
					FiredAt:         now,
				})
			case brTr.FireUpdate:
				// Fix 3: still tripped, but materially worse than the last
				// notification (and at least appAlertBreakerUpdateMinInterval
				// has elapsed) - tell the operator instead of staying silent
				// until the eventual recovery, which would otherwise leave
				// them believing nothing has changed since the original trip.
				// DownCount/EligibleCount are the TRUE current counts
				// (always live, from GetTenantAppAlertRatio just above).
				// SuppressedSites is deliberately sourced from
				// ListTenantAppDownSites (the LIVE, current down set) rather
				// than this tick's `fires`: an update can fire several ticks
				// after the sites it describes actually went down (the
				// min-interval throttle delays it), so "this tick's pending
				// transitions" can be empty or incomplete even when the
				// notification is fully warranted - see the query's own doc
				// comment.
				suppressed, sErr := w.repo.ListTenantAppDownSites(ctx, tenantID, 0)
				if sErr != nil {
					w.logger.Warn("uptime: list tenant app down sites failed",
						slog.String("tenant_id", tenantID.String()), slog.Any("error", sErr))
				}
				w.fireAppAggregate(ctx, tenantID, AppAggregateAlert{
					Recovered:       false,
					Updated:         true,
					TenantID:        tenantID,
					DownCount:       down,
					EligibleCount:   eligible,
					SuppressedSites: suppressed,
					FiredAt:         now,
				})
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
				DownCount:     down,
				EligibleCount: eligible,
				FiredAt:       now,
			})
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

// fireAppAggregate resolves the tenant's alert config and dispatches the
// fleet circuit breaker's aggregate notification. Same AppAlertsEnabled gate
// as fireApp - the breaker's own notification is still an app-health alert.
func (w *ProbeWorker) fireAppAggregate(ctx context.Context, tenantID uuid.UUID, alert AppAggregateAlert) {
	if w.dispatcher == nil {
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
