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
}

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
		mu      sync.Mutex
		checks  []metrics.Check
		results = make(map[uuid.UUID]ProbeResult, len(sites))
		sem     = make(chan struct{}, w.concurrency)
		wg      sync.WaitGroup
	)

	for _, s := range sites {
		s := s
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			res := w.prober.Probe(ctx, s.URL)
			checkedAt := time.Now()
			mu.Lock()
			results[s.ID] = res
			checks = append(checks, metrics.Check{
				CheckedAt:  checkedAt,
				TenantID:   s.TenantID,
				SiteID:     s.ID,
				Up:         res.Up,
				HTTPStatus: uint16(res.HTTPStatus),
				DNSMs:      res.DNSMs,
				ConnectMs:  res.ConnectMs,
				TLSMs:      res.TLSMs,
				TTFBMs:     res.TTFBMs,
				TotalMs:    res.TotalMs,
				TLSExpiry:  res.TLSExpiry,
				TLSIssuer:  res.TLSIssuer,
				TLSSubject: res.TLSSubject,
				Error:      res.Error,
			})
			mu.Unlock()
		}()
	}
	wg.Wait()

	// Batch-write the time-series (no-op when ClickHouse is disabled).
	if err := w.store.InsertChecks(ctx, checks); err != nil {
		w.logger.Warn("uptime: clickhouse insert failed", slog.Any("error", err))
	}

	// Update health_status + alert state per site (Postgres). Sequential: these
	// are cheap RLS-scoped writes and avoid hammering the pool.
	for _, s := range sites {
		res := results[s.ID]
		w.processSite(ctx, s, res, now)
	}
	return len(sites), nil
}

// processSite refreshes the site's health_status and runs the alert state
// machine, firing a transition alert when warranted.
func (w *ProbeWorker) processSite(ctx context.Context, s EnrolledSite, res ProbeResult, now time.Time) {
	// Refresh Postgres health_status from the probe (the M5 refinement of M2's
	// freshness-based status).
	status := HealthHealthy
	if !res.Up {
		status = HealthUnreachable
	}
	if _, err := w.repo.SetSiteHealth(ctx, s.ID, status); err != nil {
		w.logger.Warn("uptime: set health failed", slog.String("site_id", s.ID.String()), slog.Any("error", err))
	}

	// Load prior alert state (default zero-value when none yet).
	prev, _, err := w.repo.GetAlertState(ctx, s.ID)
	if err != nil {
		w.logger.Warn("uptime: get alert state failed", slog.String("site_id", s.ID.String()), slog.Any("error", err))
		return
	}
	prev.SiteID = s.ID
	prev.TenantID = s.TenantID
	if prev.LastStatus == "" {
		prev.LastStatus = StatusUnknown
	}

	tr := Evaluate(prev, res.Up, w.threshold, now)
	if err := w.repo.UpsertAlertState(ctx, tr.NewState); err != nil {
		w.logger.Warn("uptime: upsert alert state failed", slog.String("site_id", s.ID.String()), slog.Any("error", err))
		return
	}

	if !tr.FireDown && !tr.FireRecovery {
		return
	}
	w.fire(ctx, s, res, tr, now)
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
