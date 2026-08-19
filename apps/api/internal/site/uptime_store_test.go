package site

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
)

// ---------------------------------------------------------------------------
// Stubs
// ---------------------------------------------------------------------------

// listOnlyRepo wraps fakeRepo and overrides List() to return a configurable
// set of sites (with no uptime fields pre-populated, simulating what the repo
// returns when site_uptime_probes is empty on a ClickHouse install).
type listOnlyRepo struct {
	fakeRepo
	sites []Site
}

func (r *listOnlyRepo) List(_ context.Context, _ ListInput) ([]Site, error) {
	return r.sites, nil
}

// uptimeStubStore is a metrics.Store that returns a fixed map from
// QueryFleetUptime. All other methods panic — the site List path must only
// call QueryFleetUptime.
type uptimeStubStore struct {
	uptimeMap map[uuid.UUID]metrics.FleetUptimeRow
}

func (s *uptimeStubStore) Enabled() bool { return true }
func (s *uptimeStubStore) Close() error  { return nil }
func (s *uptimeStubStore) InsertChecks(_ context.Context, _ []metrics.Check) error {
	panic("site list must not call InsertChecks")
}
func (s *uptimeStubStore) QueryAggregate(_ context.Context, _, _ uuid.UUID, _ time.Duration) (metrics.Aggregate, error) {
	panic("site list must not call QueryAggregate")
}
func (s *uptimeStubStore) QueryLatest(_ context.Context, _, _ uuid.UUID) (metrics.Latest, error) {
	panic("site list must not call QueryLatest")
}
func (s *uptimeStubStore) QuerySeries(_ context.Context, _, _ uuid.UUID, _ time.Duration, _ int) ([]metrics.Point, error) {
	panic("site list must not call QuerySeries")
}
func (s *uptimeStubStore) QueryFleetUptime(_ context.Context, _ uuid.UUID, _ []uuid.UUID, _ time.Duration) (map[uuid.UUID]metrics.FleetUptimeRow, error) {
	return s.uptimeMap, nil
}
func (s *uptimeStubStore) QueryProbeWindow(_ context.Context, _, _ uuid.UUID, _, _ time.Time, _ int) ([]metrics.ProbeSample, error) {
	panic("site list must not call QueryProbeWindow")
}
func (s *uptimeStubStore) QueryFleetDailySeries(_ context.Context, _ uuid.UUID, _ []uuid.UUID, _ time.Duration) (map[uuid.UUID][]metrics.Point, error) {
	panic("site list must not call QueryFleetDailySeries")
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestSiteListUptimeFromStore is the sibling of the fleet-status regression
// (GitHub issue #74). It asserts that after the fix, the site-list uptime
// fields (UptimeUp, UptimePct30d, AvgLatencyMs, TLSExpiresAt) are populated
// from the metrics.Store — NOT from a direct read of site_uptime_probes.
//
// The repo returns sites with no uptime fields (simulating an empty
// site_uptime_probes table, as on ClickHouse installs), while the store
// returns real data. The service must merge them so the response carries
// populated uptime fields.
func TestSiteListUptimeFromStore(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	tlsExpiry := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	up := true
	pct := 98.5
	latency := 280.0

	repo := &listOnlyRepo{
		sites: []Site{
			{
				ID:       siteID,
				TenantID: tenantID,
				URL:      "https://example.com",
				Name:     "example",
				Tags:     []string{},
				// No uptime fields — simulates empty site_uptime_probes (ClickHouse mode).
			},
		},
	}
	store := &uptimeStubStore{
		uptimeMap: map[uuid.UUID]metrics.FleetUptimeRow{
			siteID: {
				Up:           &up,
				UptimePct7d:  &pct,
				AvgLatencyMs: &latency,
				TLSExpiry:    &tlsExpiry,
			},
		},
	}

	svc := NewService(repo, domain.NewValidator(), domain.FixedClock{T: time.Unix(0, 0)})
	svc.SetUptimeStore(store)

	sites, err := svc.List(context.Background(), ListInput{TenantID: tenantID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sites) != 1 {
		t.Fatalf("expected 1 site, got %d", len(sites))
	}
	s := sites[0]

	// UptimeUp must be non-nil and true (from store).
	if s.UptimeUp == nil {
		t.Fatal("UptimeUp is nil — store data not merged into site list (sibling bug #74)")
	}
	if !*s.UptimeUp {
		t.Errorf("UptimeUp = false, want true")
	}
	// UptimePct30d must reflect store value.
	if s.UptimePct30d == nil {
		t.Fatal("UptimePct30d is nil — store data not merged")
	}
	if *s.UptimePct30d != pct {
		t.Errorf("UptimePct30d = %v, want %v", *s.UptimePct30d, pct)
	}
	// AvgLatencyMs must be non-nil.
	if s.AvgLatencyMs == nil {
		t.Fatal("AvgLatencyMs is nil — store data not merged")
	}
	if *s.AvgLatencyMs != latency {
		t.Errorf("AvgLatencyMs = %v, want %v", *s.AvgLatencyMs, latency)
	}
	// TLSExpiresAt must be set.
	if s.TLSExpiresAt == nil {
		t.Fatal("TLSExpiresAt is nil — store data not merged")
	}
	if !s.TLSExpiresAt.Equal(tlsExpiry) {
		t.Errorf("TLSExpiresAt = %v, want %v", *s.TLSExpiresAt, tlsExpiry)
	}
}

// TestSiteListUptimeAbsentWhenUnprobed verifies that when a site has no data
// in the store (not yet probed), the uptime fields remain nil (not zero-value
// populated), so they serialize as absent in the JSON response.
func TestSiteListUptimeAbsentWhenUnprobed(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := &listOnlyRepo{
		sites: []Site{
			{ID: siteID, TenantID: tenantID, URL: "https://new.example.com", Name: "new", Tags: []string{}},
		},
	}
	// Store returns an empty map — no data for any site.
	store := &uptimeStubStore{
		uptimeMap: map[uuid.UUID]metrics.FleetUptimeRow{},
	}

	svc := NewService(repo, domain.NewValidator(), domain.FixedClock{T: time.Unix(0, 0)})
	svc.SetUptimeStore(store)

	sites, err := svc.List(context.Background(), ListInput{TenantID: tenantID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sites) != 1 {
		t.Fatalf("expected 1 site, got %d", len(sites))
	}
	s := sites[0]

	if s.UptimeUp != nil {
		t.Errorf("UptimeUp should be nil for unprobed site, got %v", *s.UptimeUp)
	}
	if s.UptimePct30d != nil {
		t.Errorf("UptimePct30d should be nil for unprobed site, got %v", *s.UptimePct30d)
	}
	if s.AvgLatencyMs != nil {
		t.Errorf("AvgLatencyMs should be nil for unprobed site, got %v", *s.AvgLatencyMs)
	}
	if s.TLSExpiresAt != nil {
		t.Errorf("TLSExpiresAt should be nil for unprobed site, got %v", *s.TLSExpiresAt)
	}
}

// TestSiteListUptimeNoStoreWired verifies backward-compat: when no uptime
// store is wired (e.g. test callers that use NewService without SetUptimeStore),
// List returns sites without uptime fields rather than erroring.
func TestSiteListUptimeNoStoreWired(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := &listOnlyRepo{
		sites: []Site{
			{ID: siteID, TenantID: tenantID, URL: "https://example.com", Name: "example", Tags: []string{}},
		},
	}

	// No SetUptimeStore call — uptimeStore remains nil.
	svc := NewService(repo, domain.NewValidator(), domain.FixedClock{T: time.Unix(0, 0)})

	sites, err := svc.List(context.Background(), ListInput{TenantID: tenantID})
	if err != nil {
		t.Fatalf("List without store wired must not error: %v", err)
	}
	if len(sites) != 1 {
		t.Fatalf("expected 1 site, got %d", len(sites))
	}
	// Uptime fields absent — no store, no enrichment.
	if sites[0].UptimeUp != nil {
		t.Errorf("expected UptimeUp=nil when no store wired, got %v", sites[0].UptimeUp)
	}
}
