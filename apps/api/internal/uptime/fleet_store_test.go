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
func (r *stubRepo) ListEnrolledForMonitoringProbe(_ context.Context) ([]EnrolledSite, error) {
	panic("not called")
}
func (r *stubRepo) IsMonitoringPaused(_ context.Context, _ uuid.UUID) (bool, error) {
	panic("not called")
}
func (r *stubRepo) SetSiteHealth(_ context.Context, _ uuid.UUID, _ string) (bool, error) {
	panic("not called")
}
func (r *stubRepo) GetAlertState(_ context.Context, _ uuid.UUID) (AlertState, bool, error) {
	panic("not called")
}
func (r *stubRepo) UpsertAlertState(_ context.Context, _ AlertState) error { panic("not called") }
func (r *stubRepo) TransitionAlertState(_ context.Context, _, _ uuid.UUID, _ bool, _ int, _ time.Time, _ int, _ string,
	_ bool, _ *bool, _ string, _ int) (Transition, AppTransition, error) {
	panic("not called")
}
func (r *stubRepo) GetTenantAppAlertRatio(_ context.Context, _ uuid.UUID) (int, int, error) {
	panic("not called")
}
func (r *stubRepo) TransitionAppAlertBreaker(_ context.Context, _ uuid.UUID, _ bool, _ int, _ time.Time) (AppBreakerTransition, error) {
	panic("not called")
}
func (r *stubRepo) ListTrippedAppAlertBreakerTenants(_ context.Context) ([]uuid.UUID, error) {
	panic("not called")
}
func (r *stubRepo) ListTenantAppDownSites(_ context.Context, _ uuid.UUID, _ int) ([]string, error) {
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
func (r *stubRepo) GetAppAlertRolloutDefault(_ context.Context) (bool, error) {
	panic("not called")
}
func (r *stubRepo) GetAppHealthSettings(_ context.Context, _, _ uuid.UUID) (AppHealthSettings, bool, error) {
	panic("not called")
}
func (r *stubRepo) UpdateAppHealthSettings(_ context.Context, _, _ uuid.UUID, _ string, _ bool) (AppHealthSettings, bool, error) {
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
	uptimeMap   map[uuid.UUID]metrics.FleetUptimeRow
	dailySeries map[uuid.UUID][]metrics.Point
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

// dailySeries is returned by QueryFleetDailySeries. Nil is a valid value and
// means "no site has any measurement", which is what the fleet-status tests
// (which never call it) leave it as.
func (s *stubStore) QueryFleetDailySeries(_ context.Context, _ uuid.UUID, _ []uuid.UUID, _ time.Duration) (map[uuid.UUID][]metrics.Point, error) {
	return s.dailySeries, nil
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
	if it.UptimePct7d == nil || *it.UptimePct7d != 99.5 {
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
	if it.UptimePct7d == nil || *it.UptimePct7d != pct {
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

// TestGetFleetStatus_DisconnectedCachedUpStaysUpDisplayOnlyChanges is the GH
// #291 golden no-regression test (Task 4). It pins the phase's core promise:
// a site whose latest probe is up=true (what a page cache keeps serving even
// while the backend is fatal-ing) MUST still report up=true, with an
// unchanged 7-day uptime percentage, unchanged latency, and unchanged
// Postgres-resident health_status. The ONLY thing allowed to move is the
// DERIVED display status, from Up to Degraded, once sites.connection_state
// has been marked "disconnected" by the connection sweeper's independent,
// signed, uncacheable active-verify.
//
// Two sites are compared side by side with IDENTICAL probe/store data and
// IDENTICAL Postgres health_status; the only difference is connection_state.
// If this test ever fails because up/uptime_pct_7d/avg_latency_ms/
// health_status differ between the two sites, this phase has stopped being
// display-only and started rewriting what "up" means, which is the one thing
// explicitly forbidden by the design.
func TestGetFleetStatus_DisconnectedCachedUpStaysUpDisplayOnlyChanges(t *testing.T) {
	tenantID := uuid.New()
	healthySite := uuid.New() // connection_state=connected
	brokenSite := uuid.New()  // connection_state=disconnected, same probe data

	probedAt := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	up := true
	const pct = 100.0    // a fully page-cached site answers every probe
	const latency = 45.0 // cached responses are fast

	repo := &stubRepo{
		infos: []FleetSiteInfo{
			{
				SiteID:          healthySite,
				Name:            "healthy-site",
				URL:             "https://healthy.example.com",
				ConnectionState: "connected",
				HealthStatus:    "healthy",
				InIncident:      false,
			},
			{
				SiteID:          brokenSite,
				Name:            "broken-site",
				URL:             "https://broken.example.com",
				ConnectionState: "disconnected",
				// health_status is written ONLY by the M5 reachability probe
				// worker (SetSiteHealthStatus), which this phase does not
				// touch. It stays "healthy" here on purpose, exactly like
				// the reported incident, to prove this function does not
				// derive or overwrite it.
				HealthStatus: "healthy",
				InIncident:   false,
				// disconnected_reason names the sweeper's own active-verify
				// failure (internal/site/sweeper.go Sweep), matching the
				// reported incident: the sweeper's signed POST to the agent
				// failed, not an operator-initiated last-will disconnect. See
				// TestDeriveFleetStatus_DisconnectedReasonDisambiguation for
				// the last-will side of this distinction.
				DisconnectedReason: "agent_unreachable",
			},
		},
	}
	// Identical store-sourced probe data for both sites: same up, same
	// uptime %, same latency, same last-probe timestamp. Only Postgres
	// connection_state differs between the two repo infos above.
	store := &stubStore{
		uptimeMap: map[uuid.UUID]metrics.FleetUptimeRow{
			healthySite: {Up: &up, LastProbeAt: &probedAt, UptimePct7d: ptrF64(pct), AvgLatencyMs: ptrF64(latency)},
			brokenSite:  {Up: &up, LastProbeAt: &probedAt, UptimePct7d: ptrF64(pct), AvgLatencyMs: ptrF64(latency)},
		},
	}

	svc := NewService(repo, store, nil)
	resp, err := svc.GetFleetStatus(context.Background(), tenantID, []uuid.UUID{healthySite, brokenSite})
	if err != nil {
		t.Fatalf("GetFleetStatus: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(resp.Items))
	}

	var healthy, broken FleetStatusItem
	for _, it := range resp.Items {
		switch it.SiteID {
		case healthySite:
			healthy = it
		case brokenSite:
			broken = it
		}
	}

	// FROZEN: up, uptime_pct_7d, avg_latency_ms, health_status must be
	// bit-identical between the two sites. connection_state must not leak
	// into any of them.
	if broken.Up == nil || !*broken.Up {
		t.Fatalf("broken site Up = %v, want true (a cached 200 is still up=true)", broken.Up)
	}
	if healthy.Up == nil || *healthy.Up != *broken.Up {
		t.Fatalf("Up mismatch: healthy=%v broken=%v, want identical", healthy.Up, broken.Up)
	}
	if broken.UptimePct7d == nil || *broken.UptimePct7d != pct ||
		healthy.UptimePct7d == nil || *healthy.UptimePct7d != *broken.UptimePct7d {
		t.Fatalf("UptimePct7d mismatch: healthy=%v broken=%v, want both %v", healthy.UptimePct7d, broken.UptimePct7d, pct)
	}
	if broken.AvgLatencyMs == nil || *broken.AvgLatencyMs != latency {
		t.Fatalf("broken site AvgLatencyMs = %v, want %v", broken.AvgLatencyMs, latency)
	}
	if healthy.AvgLatencyMs == nil || *healthy.AvgLatencyMs != *broken.AvgLatencyMs {
		t.Fatalf("AvgLatencyMs mismatch: healthy=%v broken=%v, want identical", healthy.AvgLatencyMs, broken.AvgLatencyMs)
	}
	if broken.HealthStatus != "healthy" || healthy.HealthStatus != broken.HealthStatus {
		t.Fatalf("HealthStatus mismatch: healthy=%q broken=%q, want both %q", healthy.HealthStatus, broken.HealthStatus, "healthy")
	}

	// The ONLY thing allowed to move: the derived display status.
	if healthy.Status != FleetStatusUp {
		t.Errorf("healthy site Status = %q, want %q (unaffected by this fix)", healthy.Status, FleetStatusUp)
	}
	if healthy.StatusReason != "" {
		t.Errorf("healthy site StatusReason = %q, want empty", healthy.StatusReason)
	}
	if broken.Status != FleetStatusDegraded {
		t.Errorf("broken site Status = %q, want %q (GH #291: was Up before this fix)", broken.Status, FleetStatusDegraded)
	}
	if broken.StatusReason != FleetReasonAgentUnreachable {
		t.Errorf("broken site StatusReason = %q, want %q", broken.StatusReason, FleetReasonAgentUnreachable)
	}

	// Summary counts must reflect exactly one Up and one Degraded, not two Up.
	if resp.Summary.Up != 1 {
		t.Errorf("Summary.Up = %d, want 1", resp.Summary.Up)
	}
	if resp.Summary.Degraded != 1 {
		t.Errorf("Summary.Degraded = %d, want 1", resp.Summary.Degraded)
	}
}

func ptrF64(f float64) *float64 { return &f }
