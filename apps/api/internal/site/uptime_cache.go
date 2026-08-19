package site

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
)

// ---------------------------------------------------------------------------
// In-process short-TTL cache for QueryFleetUptime results (m85)
// ---------------------------------------------------------------------------
//
// Context: site.Service.List() calls metrics.Store.QueryFleetUptime on every
// /api/v1/sites request. The 30-day aggregate over site_uptime_probes is cheap
// when the Postgres buffer cache is warm, but on a cold cache (Cloud SQL
// db-g1-small after an idle period) the heap I/O takes ~8s. Even after the m85
// covering index eliminates the per-row heap fetch, adding a 60-second TTL
// cache means subsequent dashboard loads within the TTL window skip the DB
// round-trip entirely — important for multi-tab or quick refresh patterns.
//
// Design constraints:
//   - Simple mutex-guarded map; no external dependency.
//   - Keyed by (tenantID, sorted siteID set, window). Sorting the IDs makes the
//     key stable regardless of the order List() returns sites (which is
//     ORDER BY created_at, id DESC and is stable, but making the key
//     order-independent is cheap insurance).
//   - Capped at maxUptimeCacheEntries entries to prevent unbounded growth across
//     tenants. When the cap is hit, a random eviction removes one entry (O(1),
//     acceptable for a small cache whose entries should naturally expire within
//     the TTL before the cap matters in any realistic deployment).
//   - TTL of fleetUptimeCacheTTL (60s). The 30-day aggregate changes by at most
//     one probe per site per minute, so a 60s stale window is imperceptible.

const (
	// fleetUptimeCacheTTL is how long a cached QueryFleetUptime result is
	// considered fresh. One minute is conservative — the underlying aggregate
	// changes by ~1 probe per site per probe interval (also ~1 min).
	fleetUptimeCacheTTL = 60 * time.Second

	// maxUptimeCacheEntries caps the number of distinct (tenant, siteSet,
	// window) combinations kept in memory. Each entry holds a map of up to
	// ~fleet-size FleetUptimeRow values (small structs). 256 entries covers
	// any realistic single-instance tenant count.
	maxUptimeCacheEntries = 256
)

// uptimeCacheKey is the map key for a cached fleet-uptime result.
type uptimeCacheKey struct {
	tenantID string // uuid.UUID.String() — comparable
	siteKey  string // sorted, comma-joined site UUIDs
	window   time.Duration
}

// uptimeCacheEntry holds one cached result with its expiry.
type uptimeCacheEntry struct {
	result    map[uuid.UUID]metrics.FleetUptimeRow
	expiresAt time.Time
}

// uptimeCache is a short-TTL in-process cache for QueryFleetUptime results.
type uptimeCache struct {
	mu      sync.Mutex
	entries map[uptimeCacheKey]uptimeCacheEntry
}

func newUptimeCache() *uptimeCache {
	return &uptimeCache{entries: make(map[uptimeCacheKey]uptimeCacheEntry)}
}

// buildKey constructs a stable, comparable cache key from the inputs.
func (c *uptimeCache) buildKey(tenantID uuid.UUID, siteIDs []uuid.UUID, window time.Duration) uptimeCacheKey {
	// Sort IDs so key is order-independent.
	strs := make([]string, len(siteIDs))
	for i, id := range siteIDs {
		strs[i] = id.String()
	}
	sort.Strings(strs)
	return uptimeCacheKey{
		tenantID: tenantID.String(),
		siteKey:  strings.Join(strs, ","),
		window:   window,
	}
}

// get returns a cached result and true if a fresh entry exists, or nil and
// false if the entry is absent or expired.
func (c *uptimeCache) get(key uptimeCacheKey) (map[uuid.UUID]metrics.FleetUptimeRow, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expiresAt) {
		delete(c.entries, key)
		return nil, false
	}
	return e.result, true
}

// set stores a result under key with a TTL of fleetUptimeCacheTTL.
// When the cache is at capacity, one arbitrary entry is evicted first.
func (c *uptimeCache) set(key uptimeCacheKey, result map[uuid.UUID]metrics.FleetUptimeRow) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; !exists && len(c.entries) >= maxUptimeCacheEntries {
		// Evict one entry at random (map iteration order is randomised in Go).
		for k := range c.entries {
			delete(c.entries, k)
			break
		}
	}
	c.entries[key] = uptimeCacheEntry{
		result:    result,
		expiresAt: time.Now().Add(fleetUptimeCacheTTL),
	}
}

// cachedMetricsStore wraps a metrics.Store and adds a per-instance in-process
// cache for QueryFleetUptime. All other methods are delegated unchanged.
type cachedMetricsStore struct {
	inner metrics.Store
	cache *uptimeCache
}

// newCachedMetricsStore wraps store with the fleet-uptime TTL cache.
// Callers should replace their metrics.Store reference with the returned value.
func newCachedMetricsStore(store metrics.Store) metrics.Store {
	return &cachedMetricsStore{
		inner: store,
		cache: newUptimeCache(),
	}
}

func (s *cachedMetricsStore) Enabled() bool { return s.inner.Enabled() }
func (s *cachedMetricsStore) Close() error  { return s.inner.Close() }
func (s *cachedMetricsStore) InsertChecks(ctx context.Context, checks []metrics.Check) error {
	return s.inner.InsertChecks(ctx, checks)
}
func (s *cachedMetricsStore) QueryAggregate(ctx context.Context, tenantID, siteID uuid.UUID, window time.Duration) (metrics.Aggregate, error) {
	return s.inner.QueryAggregate(ctx, tenantID, siteID, window)
}
func (s *cachedMetricsStore) QueryLatest(ctx context.Context, tenantID, siteID uuid.UUID) (metrics.Latest, error) {
	return s.inner.QueryLatest(ctx, tenantID, siteID)
}
func (s *cachedMetricsStore) QuerySeries(ctx context.Context, tenantID, siteID uuid.UUID, window time.Duration, buckets int) ([]metrics.Point, error) {
	return s.inner.QuerySeries(ctx, tenantID, siteID, window, buckets)
}

// QueryFleetUptime returns a cached result when one exists and is fresh;
// otherwise calls the underlying store and caches the result.
func (s *cachedMetricsStore) QueryFleetUptime(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID, window time.Duration) (map[uuid.UUID]metrics.FleetUptimeRow, error) {
	key := s.cache.buildKey(tenantID, siteIDs, window)
	if cached, ok := s.cache.get(key); ok {
		return cached, nil
	}
	result, err := s.inner.QueryFleetUptime(ctx, tenantID, siteIDs, window)
	if err != nil {
		return nil, err
	}
	s.cache.set(key, result)
	return result, nil
}

// QueryFleetDailySeries delegates unchanged, deliberately NOT cached. The
// uptime cache above is keyed on (tenant, sites, window) and stores only
// FleetUptimeRow; reusing it for a day series would need a second typed cache
// for a page that is loaded far less often than the sites list this cache was
// built for (m85). More importantly, this endpoint is the one an operator
// exports into a client report, so a stale strip is a wrong number in
// somebody else's document rather than a slightly old dashboard tile.
func (s *cachedMetricsStore) QueryFleetDailySeries(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID, window time.Duration) (map[uuid.UUID][]metrics.Point, error) {
	return s.inner.QueryFleetDailySeries(ctx, tenantID, siteIDs, window)
}

// QueryProbeWindow delegates unchanged (not cached — the incident-detail
// endpoint is a low-traffic, single-incident lookup, not a repeated
// dashboard-refresh path like QueryFleetUptime).
func (s *cachedMetricsStore) QueryProbeWindow(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time, limitN int) ([]metrics.ProbeSample, error) {
	return s.inner.QueryProbeWindow(ctx, tenantID, siteID, from, to, limitN)
}
