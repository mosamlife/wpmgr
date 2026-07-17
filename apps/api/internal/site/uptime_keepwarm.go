package site

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
)

// ---------------------------------------------------------------------------
// INTERIM uptime keep-warm refresher (stopgap — see WPMGR_UPTIME_KEEPWARM)
// ---------------------------------------------------------------------------
//
// Confirmed root cause: GET /api/v1/sites enriches the sites list with uptime
// data via metrics.Store.QueryFleetUptime — a per-site LATERAL aggregate over
// the raw site_uptime_probes table for the last 30 days (~43k rows/site/
// request). QueryFleetUptime's result is cached for fleetUptimeCacheTTL (60s,
// see uptime_cache.go), but the recurring ~7-8s stall happens because BOTH
// warm layers expire after 15-30 minutes of idle traffic: the 60s result
// cache misses AND Postgres evicts the probes index from its buffer cache on
// the small db-g1-small instance. A warm query is ~30ms; a cold one is ~8s.
//
// This refresher periodically re-runs QueryFleetUptime for every tenant with
// enrolled sites, on a tick shorter than fleetUptimeCacheTTL, through the SAME
// (cache-wrapped) metrics.Store the /sites handler reads — so a refresh here
// populates the exact cache entry List() will hit, AND keeps the Postgres
// buffer cache backing the query resident. This is a deliberate, bounded
// "keep the DB busy on purpose" tradeoff: it turns an idle DB into one that
// runs the uptime aggregate ~every 45s, which is fine short-term for a small
// fleet.
//
// REMOVE this file (uptime_keepwarm.go), its wiring in cmd/wpmgr/main.go, and
// the WPMGR_UPTIME_KEEPWARM config flag once the site_uptime_daily rollup
// lands and becomes the authoritative source for /sites uptime enrichment —
// the rollup turns QueryFleetUptime into a cheap read over a small
// pre-aggregated table and this warming is no longer needed.

const (
	// uptimeKeepWarmInterval is intentionally under fleetUptimeCacheTTL (60s)
	// so the result cache is always refreshed before it would otherwise expire
	// unpopulated.
	uptimeKeepWarmInterval = 45 * time.Second

	// uptimeKeepWarmWindow MUST match the window List() queries (window30d in
	// service.go) — a different window is a different cache key and warms an
	// entry the real request path never reads.
	uptimeKeepWarmWindow = 30 * 24 * time.Hour
)

// TenantSiteID is one (tenant, site) pair from the enrolled-site list.
type TenantSiteID struct {
	TenantID uuid.UUID
	SiteID   uuid.UUID
}

// EnrolledSiteLister lists every currently-enrolled site across all tenants,
// cross-tenant. Satisfied via a thin adapter over uptime.Repo.
// ListEnrolledForProbe in cmd/wpmgr — the SAME cross-tenant list the uptime
// probe worker already sweeps every ~60s, so the keep-warm refresher adds no
// new query shape. Kept as a narrow interface (rather than importing the
// uptime package directly) to avoid a cross-domain import — see repo.go's
// ConnectionService/BillingGate/ScreenshotEnricher seams for the same
// pattern.
type EnrolledSiteLister interface {
	ListEnrolledSiteIDs(ctx context.Context) ([]TenantSiteID, error)
}

// UptimeKeepWarmer periodically re-runs QueryFleetUptime for every tenant
// with enrolled sites, keeping both the 60s in-process result cache and the
// underlying Postgres buffer cache warm. See the file doc comment above —
// this is an interim stopgap, env-gated for clean removal.
type UptimeKeepWarmer struct {
	store    metrics.Store
	lister   EnrolledSiteLister
	interval time.Duration
	window   time.Duration
	logger   *slog.Logger
}

// NewUptimeKeepWarmer builds a keep-warm refresher. store should be the SAME
// cache-wrapped store instance wired via Service.SetUptimeStore (i.e. what
// SetUptimeStore was called with), so a refresh here populates the exact
// cache entry List() will read. lister enumerates enrolled sites cross-tenant.
func NewUptimeKeepWarmer(store metrics.Store, lister EnrolledSiteLister, logger *slog.Logger) *UptimeKeepWarmer {
	if logger == nil {
		logger = slog.Default()
	}
	return &UptimeKeepWarmer{
		store:    store,
		lister:   lister,
		interval: uptimeKeepWarmInterval,
		window:   uptimeKeepWarmWindow,
		logger:   logger,
	}
}

// StartUptimeKeepWarm starts the refresher in its own goroutine when enabled
// is true, and returns nil without starting anything when enabled is false
// (the WPMGR_UPTIME_KEEPWARM=false / default-off case). ctx controls the
// refresher's lifetime — cancel it on shutdown for a clean stop. The returned
// warmer is nil when disabled, mainly so callers/tests can observe whether the
// gate started the loop.
func StartUptimeKeepWarm(ctx context.Context, enabled bool, store metrics.Store, lister EnrolledSiteLister, logger *slog.Logger) *UptimeKeepWarmer {
	if logger == nil {
		logger = slog.Default()
	}
	if !enabled {
		logger.Info("uptime keep-warm refresher disabled (WPMGR_UPTIME_KEEPWARM=false)")
		return nil
	}
	if store == nil || lister == nil {
		logger.Warn("uptime keep-warm refresher not started: store or lister not wired")
		return nil
	}
	w := NewUptimeKeepWarmer(store, lister, logger)
	go w.Run(ctx)
	return w
}

// Run starts the periodic refresh loop and blocks until ctx is cancelled.
// Call it in its own goroutine (StartUptimeKeepWarm does this). It refreshes
// once immediately on start (so a fresh deploy doesn't wait a full interval
// before the cache is warm), then on every tick thereafter. Each tick is
// best-effort: a failure listing enrolled sites, or refreshing any one
// tenant, is logged and never stops the loop or crashes the process.
func (w *UptimeKeepWarmer) Run(ctx context.Context) {
	w.logger.Info("uptime keep-warm refresher started",
		slog.Duration("interval", w.interval))

	w.tick(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("uptime keep-warm refresher stopped")
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// tick groups enrolled sites by tenant and refreshes each tenant's fleet-
// uptime cache entry once.
func (w *UptimeKeepWarmer) tick(ctx context.Context) {
	t0 := time.Now()
	sites, err := w.lister.ListEnrolledSiteIDs(ctx)
	if err != nil {
		w.logger.Warn("uptime keep-warm: list enrolled sites failed", slog.Any("error", err))
		return
	}
	byTenant := groupByTenant(sites)
	for tenantID, siteIDs := range byTenant {
		w.refreshTenant(ctx, tenantID, siteIDs)
	}
	w.logger.Debug("uptime keep-warm tick",
		slog.Int("tenants", len(byTenant)),
		slog.Int("sites", len(sites)),
		slog.Int64("elapsed_ms", time.Since(t0).Milliseconds()),
	)
}

// refreshTenant re-runs QueryFleetUptime for one tenant, through the SAME
// tenant-scoped path the /sites handler uses (QueryFleetUptime runs under
// InAgentTx with an explicit tenant_id predicate — RLS is never bypassed).
// Best-effort: a panic is recovered and an error is logged, never propagated
// — a single tenant's failure must never stop the loop or affect others.
func (w *UptimeKeepWarmer) refreshTenant(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID) {
	defer func() {
		if r := recover(); r != nil {
			w.logger.Warn("uptime keep-warm: tenant refresh panic",
				slog.String("tenant_id", tenantID.String()),
				slog.Any("panic", r))
		}
	}()
	if _, err := w.store.QueryFleetUptime(ctx, tenantID, siteIDs, w.window); err != nil {
		w.logger.Debug("uptime keep-warm: tenant refresh failed",
			slog.String("tenant_id", tenantID.String()),
			slog.Any("error", err))
	}
}

// groupByTenant buckets (tenant, site) pairs into tenant -> siteIDs, skipping
// any pair with a nil tenant or site ID (defensive; the lister should never
// produce these).
func groupByTenant(sites []TenantSiteID) map[uuid.UUID][]uuid.UUID {
	out := make(map[uuid.UUID][]uuid.UUID)
	for _, s := range sites {
		if s.TenantID == uuid.Nil || s.SiteID == uuid.Nil {
			continue
		}
		out[s.TenantID] = append(out[s.TenantID], s.SiteID)
	}
	return out
}
