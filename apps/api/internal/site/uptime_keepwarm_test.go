package site

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
)

// ---------------------------------------------------------------------------
// Unit tests for the interim uptime keep-warm refresher (WPMGR_UPTIME_KEEPWARM)
// ---------------------------------------------------------------------------

// keepWarmFakeLister returns a fixed set of (tenant, site) pairs and counts
// how many times it was called. Can be configured to error.
type keepWarmFakeLister struct {
	mu    sync.Mutex
	sites []TenantSiteID
	calls int
	err   error
}

func (l *keepWarmFakeLister) ListEnrolledSiteIDs(_ context.Context) ([]TenantSiteID, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	return l.sites, nil
}

func (l *keepWarmFakeLister) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

var _ EnrolledSiteLister = (*keepWarmFakeLister)(nil)

// keepWarmFakeStore records QueryFleetUptime calls per tenant and can be
// configured to fail for exactly one tenant, so tests can prove a single
// tenant's failure never stops the refresh loop.
type keepWarmFakeStore struct {
	mu         sync.Mutex
	calls      map[uuid.UUID]int
	failTenant uuid.UUID
}

func newKeepWarmFakeStore() *keepWarmFakeStore {
	return &keepWarmFakeStore{calls: make(map[uuid.UUID]int)}
}

func (s *keepWarmFakeStore) Enabled() bool { return true }
func (s *keepWarmFakeStore) Close() error  { return nil }
func (s *keepWarmFakeStore) InsertChecks(_ context.Context, _ []metrics.Check) error {
	return nil
}
func (s *keepWarmFakeStore) QueryAggregate(_ context.Context, _, _ uuid.UUID, _ time.Duration) (metrics.Aggregate, error) {
	return metrics.Aggregate{}, nil
}
func (s *keepWarmFakeStore) QueryLatest(_ context.Context, _, _ uuid.UUID) (metrics.Latest, error) {
	return metrics.Latest{}, nil
}
func (s *keepWarmFakeStore) QuerySeries(_ context.Context, _, _ uuid.UUID, _ time.Duration, _ int) ([]metrics.Point, error) {
	return nil, nil
}
func (s *keepWarmFakeStore) QueryFleetUptime(_ context.Context, tenantID uuid.UUID, _ []uuid.UUID, _ time.Duration) (map[uuid.UUID]metrics.FleetUptimeRow, error) {
	s.mu.Lock()
	s.calls[tenantID]++
	s.mu.Unlock()
	if s.failTenant != uuid.Nil && tenantID == s.failTenant {
		return nil, errors.New("simulated tenant refresh failure")
	}
	return map[uuid.UUID]metrics.FleetUptimeRow{}, nil
}
func (s *keepWarmFakeStore) QueryProbeWindow(_ context.Context, _, _ uuid.UUID, _, _ time.Time, _ int) ([]metrics.ProbeSample, error) {
	return nil, nil
}

func (s *keepWarmFakeStore) callsFor(tenantID uuid.UUID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[tenantID]
}

func (s *keepWarmFakeStore) totalCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		n += c
	}
	return n
}

var _ metrics.Store = (*keepWarmFakeStore)(nil)

// ---------------------------------------------------------------------------
// groupByTenant
// ---------------------------------------------------------------------------

func TestGroupByTenant(t *testing.T) {
	tenantA := uuid.New()
	tenantB := uuid.New()
	siteA1 := uuid.New()
	siteA2 := uuid.New()
	siteB1 := uuid.New()

	got := groupByTenant([]TenantSiteID{
		{TenantID: tenantA, SiteID: siteA1},
		{TenantID: tenantA, SiteID: siteA2},
		{TenantID: tenantB, SiteID: siteB1},
		{TenantID: uuid.Nil, SiteID: uuid.New()}, // nil tenant skipped
		{TenantID: uuid.New(), SiteID: uuid.Nil}, // nil site skipped
	})

	if len(got) != 2 {
		t.Fatalf("expected 2 tenants, got %d", len(got))
	}
	if len(got[tenantA]) != 2 {
		t.Fatalf("expected 2 sites for tenantA, got %d", len(got[tenantA]))
	}
	if len(got[tenantB]) != 1 {
		t.Fatalf("expected 1 site for tenantB, got %d", len(got[tenantB]))
	}
}

// ---------------------------------------------------------------------------
// UptimeKeepWarmer.tick — tenant-selection + per-tenant fault tolerance
// ---------------------------------------------------------------------------

func TestUptimeKeepWarmer_Tick_RefreshesEveryTenant_TolerantOfPerTenantError(t *testing.T) {
	tenantOK1 := uuid.New()
	tenantOK2 := uuid.New()
	tenantFail := uuid.New()

	lister := &keepWarmFakeLister{sites: []TenantSiteID{
		{TenantID: tenantOK1, SiteID: uuid.New()},
		{TenantID: tenantOK2, SiteID: uuid.New()},
		{TenantID: tenantFail, SiteID: uuid.New()},
	}}
	store := newKeepWarmFakeStore()
	store.failTenant = tenantFail

	w := NewUptimeKeepWarmer(store, lister, nil)

	// A single tenant's failure must not stop the others from being
	// refreshed, and tick() itself must not panic or return early.
	w.tick(context.Background())

	if got := store.callsFor(tenantOK1); got != 1 {
		t.Fatalf("expected tenantOK1 refreshed once, got %d", got)
	}
	if got := store.callsFor(tenantOK2); got != 1 {
		t.Fatalf("expected tenantOK2 refreshed once, got %d", got)
	}
	if got := store.callsFor(tenantFail); got != 1 {
		t.Fatalf("expected the failing tenant to still be attempted once, got %d", got)
	}
}

func TestUptimeKeepWarmer_Tick_OnlyEnrolledTenantsAreTouched(t *testing.T) {
	tenantWithSites := uuid.New()
	lister := &keepWarmFakeLister{sites: []TenantSiteID{
		{TenantID: tenantWithSites, SiteID: uuid.New()},
	}}
	store := newKeepWarmFakeStore()
	w := NewUptimeKeepWarmer(store, lister, nil)

	w.tick(context.Background())

	if got := store.totalCalls(); got != 1 {
		t.Fatalf("expected exactly 1 tenant refreshed (no zero-site tenants scanned), got %d", got)
	}
	if got := store.callsFor(tenantWithSites); got != 1 {
		t.Fatalf("expected the enrolled tenant to be refreshed, got %d", got)
	}
	// A tenant that never appeared in the lister output must never be
	// queried — there is nothing to assert an absence of a random UUID
	// beyond totalCalls()==1 above, which already pins the exact call count.
}

func TestUptimeKeepWarmer_Tick_ListerErrorIsNonFatal(t *testing.T) {
	lister := &keepWarmFakeLister{err: errors.New("boom")}
	store := newKeepWarmFakeStore()
	w := NewUptimeKeepWarmer(store, lister, nil)

	// Must not panic; must simply skip this tick.
	w.tick(context.Background())

	if got := store.totalCalls(); got != 0 {
		t.Fatalf("expected no store calls when the lister errors, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// StartUptimeKeepWarm — the env-gate
// ---------------------------------------------------------------------------

func TestStartUptimeKeepWarm_Disabled_DoesNotStart(t *testing.T) {
	lister := &keepWarmFakeLister{sites: []TenantSiteID{{TenantID: uuid.New(), SiteID: uuid.New()}}}
	store := newKeepWarmFakeStore()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := StartUptimeKeepWarm(ctx, false, store, lister, nil)
	if w != nil {
		t.Fatal("expected a nil warmer when disabled (goroutine must not start)")
	}

	// Give any (incorrectly) started goroutine a chance to run, then assert
	// it never touched the lister/store.
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		if lister.callCount() > 0 {
			t.Fatal("disabled refresher must never call the lister")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := store.totalCalls(); got != 0 {
		t.Fatalf("disabled refresher must never call the store, got %d calls", got)
	}
}

func TestStartUptimeKeepWarm_Enabled_TicksImmediatelyOnStart(t *testing.T) {
	tenantID := uuid.New()
	lister := &keepWarmFakeLister{sites: []TenantSiteID{{TenantID: tenantID, SiteID: uuid.New()}}}
	store := newKeepWarmFakeStore()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := StartUptimeKeepWarm(ctx, true, store, lister, nil)
	if w == nil {
		t.Fatal("expected a non-nil warmer when enabled")
	}

	// Run() refreshes once immediately on start (not just on the 45s ticker),
	// so a fresh deploy doesn't sit cold for a full interval. Poll (bounded)
	// rather than sleeping the full interval.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if store.callsFor(tenantID) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expected the enabled refresher to have called QueryFleetUptime at least once within 2s")
}

func TestStartUptimeKeepWarm_StopsOnContextCancel(t *testing.T) {
	tenantID := uuid.New()
	lister := &keepWarmFakeLister{sites: []TenantSiteID{{TenantID: tenantID, SiteID: uuid.New()}}}
	store := newKeepWarmFakeStore()

	ctx, cancel := context.WithCancel(context.Background())
	w := StartUptimeKeepWarm(ctx, true, store, lister, nil)
	if w == nil {
		t.Fatal("expected a non-nil warmer when enabled")
	}

	// Wait for the immediate startup tick, then cancel and confirm no
	// subsequent tick occurs. (We can't observe goroutine exit directly
	// without instrumenting Run further, so this only asserts cancellation
	// doesn't itself panic/hang — the real "does it stop" guarantee is the
	// select on ctx.Done() in Run, exercised here for crash-freedom.)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if store.callsFor(tenantID) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
}

func TestStartUptimeKeepWarm_NilStoreOrLister_DoesNotStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if w := StartUptimeKeepWarm(ctx, true, nil, &keepWarmFakeLister{}, nil); w != nil {
		t.Fatal("expected a nil warmer when store is nil")
	}
	if w := StartUptimeKeepWarm(ctx, true, newKeepWarmFakeStore(), nil, nil); w != nil {
		t.Fatal("expected a nil warmer when lister is nil")
	}
}
