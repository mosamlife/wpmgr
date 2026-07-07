package site

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
)

// ---------------------------------------------------------------------------
// Unit tests for cachedMetricsStore (m85)
// ---------------------------------------------------------------------------

// countingStore wraps a fixed result and counts QueryFleetUptime calls.
type countingStore struct {
	result map[uuid.UUID]metrics.FleetUptimeRow
	calls  atomic.Int64
}

func (s *countingStore) Enabled() bool { return true }
func (s *countingStore) Close() error  { return nil }
func (s *countingStore) InsertChecks(_ context.Context, _ []metrics.Check) error { return nil }
func (s *countingStore) QueryAggregate(_ context.Context, _, _ uuid.UUID, _ time.Duration) (metrics.Aggregate, error) {
	return metrics.Aggregate{}, nil
}
func (s *countingStore) QueryLatest(_ context.Context, _, _ uuid.UUID) (metrics.Latest, error) {
	return metrics.Latest{}, nil
}
func (s *countingStore) QuerySeries(_ context.Context, _, _ uuid.UUID, _ time.Duration, _ int) ([]metrics.Point, error) {
	return nil, nil
}
func (s *countingStore) QueryFleetUptime(_ context.Context, _ uuid.UUID, _ []uuid.UUID, _ time.Duration) (map[uuid.UUID]metrics.FleetUptimeRow, error) {
	s.calls.Add(1)
	return s.result, nil
}
func (s *countingStore) QueryProbeWindow(_ context.Context, _, _ uuid.UUID, _, _ time.Time, _ int) ([]metrics.ProbeSample, error) {
	return nil, nil
}

func TestUptimeCache_Hit(t *testing.T) {
	inner := &countingStore{result: map[uuid.UUID]metrics.FleetUptimeRow{}}
	cached := newCachedMetricsStore(inner)

	tenantID := uuid.New()
	siteIDs := []uuid.UUID{uuid.New(), uuid.New()}
	ctx := context.Background()

	// First call: cache miss → inner called once.
	_, err := cached.QueryFleetUptime(ctx, tenantID, siteIDs, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("expected 1 inner call, got %d", got)
	}

	// Second call immediately: cache hit → inner still called only once.
	_, err = cached.QueryFleetUptime(ctx, tenantID, siteIDs, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("expected 1 inner call after cache hit, got %d", got)
	}
}

func TestUptimeCache_Expiry(t *testing.T) {
	inner := &countingStore{result: map[uuid.UUID]metrics.FleetUptimeRow{}}
	// Use the raw cache so we can manipulate expiry directly.
	c := newUptimeCache()
	tenantID := uuid.New()
	siteIDs := []uuid.UUID{uuid.New()}
	window := 30 * 24 * time.Hour
	key := c.buildKey(tenantID, siteIDs, window)

	// Store with an already-expired TTL.
	c.mu.Lock()
	c.entries[key] = uptimeCacheEntry{
		result:    inner.result,
		expiresAt: time.Now().Add(-time.Second),
	}
	c.mu.Unlock()

	// get should return miss (expired).
	if _, ok := c.get(key); ok {
		t.Fatal("expected cache miss for expired entry")
	}
	// Entry should have been deleted.
	c.mu.Lock()
	_, stillPresent := c.entries[key]
	c.mu.Unlock()
	if stillPresent {
		t.Fatal("expired entry should have been removed from map")
	}
}

func TestUptimeCache_KeyOrderIndependent(t *testing.T) {
	c := newUptimeCache()
	tenantID := uuid.New()
	id1 := uuid.New()
	id2 := uuid.New()
	window := 30 * 24 * time.Hour

	k1 := c.buildKey(tenantID, []uuid.UUID{id1, id2}, window)
	k2 := c.buildKey(tenantID, []uuid.UUID{id2, id1}, window)

	if k1 != k2 {
		t.Fatalf("key should be order-independent: %q != %q", k1.siteKey, k2.siteKey)
	}
}

func TestUptimeCache_Cap(t *testing.T) {
	c := newUptimeCache()
	window := 30 * 24 * time.Hour

	// Fill to the cap.
	for i := 0; i < maxUptimeCacheEntries; i++ {
		key := c.buildKey(uuid.New(), []uuid.UUID{uuid.New()}, window)
		c.set(key, map[uuid.UUID]metrics.FleetUptimeRow{})
	}
	if got := len(c.entries); got != maxUptimeCacheEntries {
		t.Fatalf("expected %d entries, got %d", maxUptimeCacheEntries, got)
	}

	// Adding one more must evict one entry so the map stays at cap.
	key := c.buildKey(uuid.New(), []uuid.UUID{uuid.New()}, window)
	c.set(key, map[uuid.UUID]metrics.FleetUptimeRow{})
	if got := len(c.entries); got != maxUptimeCacheEntries {
		t.Fatalf("expected %d entries after cap eviction, got %d", maxUptimeCacheEntries, got)
	}
}

func TestSetUptimeStore_WrapsWithCache(t *testing.T) {
	inner := &countingStore{result: map[uuid.UUID]metrics.FleetUptimeRow{}}
	svc := NewService(nil, nil, nil)
	svc.SetUptimeStore(inner)

	// The store wired into the service should be the caching wrapper.
	if _, ok := svc.uptimeStore.(*cachedMetricsStore); !ok {
		t.Fatal("SetUptimeStore should wrap the store with cachedMetricsStore")
	}
}
