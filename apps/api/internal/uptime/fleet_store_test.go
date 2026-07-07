package uptime

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
)

// ---------------------------------------------------------------------------
// Stubs
// ---------------------------------------------------------------------------

// stubRepo is a minimal Repo that only services GetFleetSiteInfo (returns the
// provided infos). All other methods panic — the fleet-status path under test
// must not touch them.
type stubRepo struct {
	infos []FleetSiteInfo
}

func (r *stubRepo) ListEnrolledForProbe(_ context.Context) ([]EnrolledSite, error) {
	panic("not called")
}
func (r *stubRepo) SetSiteHealth(_ context.Context, _ uuid.UUID, _ string) (bool, error) {
	panic("not called")
}
func (r *stubRepo) GetAlertState(_ context.Context, _ uuid.UUID) (AlertState, bool, error) {
	panic("not called")
}
func (r *stubRepo) UpsertAlertState(_ context.Context, _ AlertState) error { panic("not called") }
func (r *stubRepo) TransitionAlertState(_ context.Context, _, _ uuid.UUID, _ bool, _ int, _ time.Time, _ int, _ string) (Transition, error) {
	panic("not called")
}
func (r *stubRepo) ListAlertConfigsAllTenants(_ context.Context) ([]AlertConfig, error) {
	panic("not called")
}
func (r *stubRepo) GetAlertConfig(_ context.Context, _ uuid.UUID) (AlertConfig, bool, error) {
	panic("not called")
}
func (r *stubRepo) UpsertAlertConfig(_ context.Context, _ AlertConfig) (AlertConfig, error) {
	panic("not called")
}
func (r *stubRepo) GetFleetSiteInfo(_ context.Context, _ uuid.UUID, _ []uuid.UUID) ([]FleetSiteInfo, error) {
	return r.infos, nil
}
func (r *stubRepo) GetFleetIncidents(_ context.Context, _ uuid.UUID, _ []uuid.UUID, _ time.Time, _ int) ([]FleetIncidentItem, error) {
	panic("not called")
}
func (r *stubRepo) GetIncidentByID(_ context.Context, _, _ uuid.UUID) (IncidentSummary, bool, error) {
	panic("not called")
}
func (r *stubRepo) CountRecentIncidents(_ context.Context, _, _ uuid.UUID) (int64, error) {
	panic("not called")
}

// stubStore is a metrics.Store that returns a fixed map from QueryFleetUptime
// and panics on any other method — the fleet-status path must not call them.
type stubStore struct {
	uptimeMap map[uuid.UUID]metrics.FleetUptimeRow
}

func (s *stubStore) Enabled() bool { return true }
func (s *stubStore) Close() error  { return nil }
func (s *stubStore) InsertChecks(_ context.Context, _ []metrics.Check) error {
	panic("not called")
}
func (s *stubStore) QueryAggregate(_ context.Context, _, _ uuid.UUID, _ time.Duration) (metrics.Aggregate, error) {
	panic("not called")
}
func (s *stubStore) QueryLatest(_ context.Context, _, _ uuid.UUID) (metrics.Latest, error) {
	panic("not called")
}
func (s *stubStore) QuerySeries(_ context.Context, _, _ uuid.UUID, _ time.Duration, _ int) ([]metrics.Point, error) {
	panic("not called")
}
func (s *stubStore) QueryFleetUptime(_ context.Context, _ uuid.UUID, _ []uuid.UUID, _ time.Duration) (map[uuid.UUID]metrics.FleetUptimeRow, error) {
	return s.uptimeMap, nil
}
func (s *stubStore) QueryProbeWindow(_ context.Context, _, _ uuid.UUID, _, _ time.Time, _ int) ([]metrics.ProbeSample, error) {
	panic("not called")
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestGetFleetStatus_StoreDataWhenPostgresEmpty is the primary regression test
// for GitHub issue #74. It simulates a ClickHouse deployment by wiring a store
// that returns real data while the repo (simulating an empty site_uptime_probes
// table) returns only site metadata. The service must return non-null uptime
// fields sourced from the store.
func TestGetFleetStatus_StoreDataWhenPostgresEmpty(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	probedAt := time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC)
	tlsExpiry := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	up := true
	pct := 99.5
	latency := 312.4

	repo := &stubRepo{
		infos: []FleetSiteInfo{
			{
				SiteID:          siteID,
				Name:            "example",
				URL:             "https://example.com",
				ConnectionState: "connected",
				HealthStatus:    "healthy",
				InIncident:      false,
			},
		},
	}
	store := &stubStore{
		uptimeMap: map[uuid.UUID]metrics.FleetUptimeRow{
			siteID: {
				Up:           &up,
				LastProbeAt:  &probedAt,
				UptimePct7d:  &pct,
				AvgLatencyMs: &latency,
				TLSExpiry:    &tlsExpiry,
			},
		},
	}

	svc := NewService(repo, store, nil /* verifier not used by GetFleetStatus */)
	resp, err := svc.GetFleetStatus(context.Background(), tenantID, []uuid.UUID{siteID})
	if err != nil {
		t.Fatalf("GetFleetStatus: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(resp.Items))
	}
	it := resp.Items[0]

	// up must be non-nil and true.
	if it.Up == nil {
		t.Fatal("Up is nil — store data not merged (regression: ClickHouse null bug)")
	}
	if !*it.Up {
		t.Errorf("Up = false, want true")
	}
	// uptime_pct_7d must reflect store value.
	if it.UptimePct7d != 99.5 {
		t.Errorf("UptimePct7d = %v, want 99.5", it.UptimePct7d)
	}
	// avg_latency_ms must be non-nil and match.
	if it.AvgLatencyMs == nil {
		t.Fatal("AvgLatencyMs is nil — store data not merged")
	}
	if *it.AvgLatencyMs != latency {
		t.Errorf("AvgLatencyMs = %v, want %v", *it.AvgLatencyMs, latency)
	}
	// last_probe_at must be set.
	if it.LastProbeAt == nil {
		t.Fatal("LastProbeAt is nil — store data not merged")
	}
	if !it.LastProbeAt.Equal(probedAt) {
		t.Errorf("LastProbeAt = %v, want %v", it.LastProbeAt, probedAt)
	}
	// tls_expiry must be set.
	if it.TLSExpiry == nil {
		t.Fatal("TLSExpiry is nil — store data not merged")
	}
	if !it.TLSExpiry.Equal(tlsExpiry) {
		t.Errorf("TLSExpiry = %v, want %v", it.TLSExpiry, tlsExpiry)
	}
	// Status must not be "unknown" for a probed, up site.
	if it.Status == FleetStatusUnknown {
		t.Errorf("Status = unknown — deriveFleetStatus not called with store data")
	}
	if it.Status != FleetStatusUp {
		t.Errorf("Status = %q, want %q", it.Status, FleetStatusUp)
	}
	// Tenant scoping: name/url must come from the repo (Postgres).
	if it.Name != "example" {
		t.Errorf("Name = %q, want %q", it.Name, "example")
	}
}

// TestGetFleetStatus_SummaryCountsFromStore verifies that the up/degraded/down
// summary counts are computed from store-sourced data, not from all-unknown
// defaults. Previously they all bucketed as "unknown" because the probe columns
// were nil when site_uptime_probes was empty.
func TestGetFleetStatus_SummaryCountsFromStore(t *testing.T) {
	tenantID := uuid.New()
	siteUp := uuid.New()
	siteDown := uuid.New()
	siteUnknown := uuid.New()

	up := true
	down := false
	pct100 := 100.0
	pct0 := 0.0
	lat := 200.0

	repo := &stubRepo{
		infos: []FleetSiteInfo{
			{SiteID: siteUp, Name: "up-site", URL: "https://up.example.com", ConnectionState: "connected", HealthStatus: "healthy"},
			{SiteID: siteDown, Name: "down-site", URL: "https://down.example.com", ConnectionState: "connected", HealthStatus: "unreachable"},
			{SiteID: siteUnknown, Name: "unknown-site", URL: "https://unknown.example.com", ConnectionState: "connected", HealthStatus: "unknown"},
		},
	}
	store := &stubStore{
		uptimeMap: map[uuid.UUID]metrics.FleetUptimeRow{
			siteUp:   {Up: &up, UptimePct7d: &pct100, AvgLatencyMs: &lat},
			siteDown: {Up: &down, UptimePct7d: &pct0},
			// siteUnknown is absent — no probe data (never probed)
		},
	}

	svc := NewService(repo, store, nil)
	resp, err := svc.GetFleetStatus(context.Background(), tenantID, []uuid.UUID{siteUp, siteDown, siteUnknown})
	if err != nil {
		t.Fatalf("GetFleetStatus: %v", err)
	}

	if resp.Summary.Up != 1 {
		t.Errorf("Summary.Up = %d, want 1", resp.Summary.Up)
	}
	if resp.Summary.Down != 1 {
		t.Errorf("Summary.Down = %d, want 1", resp.Summary.Down)
	}
	if resp.Summary.Unknown != 1 {
		t.Errorf("Summary.Unknown = %d, want 1 (the unprobed site)", resp.Summary.Unknown)
	}
	if resp.Summary.Degraded != 0 {
		t.Errorf("Summary.Degraded = %d, want 0", resp.Summary.Degraded)
	}
}

// TestGetFleetStatus_PostgresModeParity verifies that when the store returns
// the same data that site_uptime_probes would have provided (i.e. the pgStore
// path), the merged result is identical to the pre-fix output. This ensures no
// regression on Postgres deployments.
func TestGetFleetStatus_PostgresModeParity(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	probedAt := time.Date(2026, 6, 19, 9, 0, 0, 0, time.UTC)
	tlsExpiry := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	up := true
	pct := 98.6
	latency := 450.0

	repo := &stubRepo{
		infos: []FleetSiteInfo{
			{
				SiteID:          siteID,
				Name:            "postgres-site",
				URL:             "https://postgres.example.com",
				ConnectionState: "connected",
				HealthStatus:    "healthy",
				InIncident:      false,
			},
		},
	}
	// pgStore would return exactly these values; we simulate it via stubStore.
	store := &stubStore{
		uptimeMap: map[uuid.UUID]metrics.FleetUptimeRow{
			siteID: {
				Up:           &up,
				LastProbeAt:  &probedAt,
				UptimePct7d:  &pct,
				AvgLatencyMs: &latency,
				TLSExpiry:    &tlsExpiry,
			},
		},
	}

	svc := NewService(repo, store, nil)
	resp, err := svc.GetFleetStatus(context.Background(), tenantID, []uuid.UUID{siteID})
	if err != nil {
		t.Fatalf("GetFleetStatus: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(resp.Items))
	}
	it := resp.Items[0]

	// Verify parity: same fields the old repo SQL would have returned.
	if it.Up == nil || !*it.Up {
		t.Errorf("Up mismatch: got %v", it.Up)
	}
	if it.UptimePct7d != pct {
		t.Errorf("UptimePct7d = %v, want %v", it.UptimePct7d, pct)
	}
	if it.AvgLatencyMs == nil || *it.AvgLatencyMs != latency {
		t.Errorf("AvgLatencyMs = %v, want %v", it.AvgLatencyMs, latency)
	}
	if it.LastProbeAt == nil || !it.LastProbeAt.Equal(probedAt) {
		t.Errorf("LastProbeAt mismatch: got %v, want %v", it.LastProbeAt, probedAt)
	}
	if it.TLSExpiry == nil || !it.TLSExpiry.Equal(tlsExpiry) {
		t.Errorf("TLSExpiry mismatch: got %v, want %v", it.TLSExpiry, tlsExpiry)
	}
	if it.Name != "postgres-site" {
		t.Errorf("Name = %q, want %q", it.Name, "postgres-site")
	}
	if it.Status != FleetStatusUp {
		t.Errorf("Status = %q, want %q", it.Status, FleetStatusUp)
	}
}
