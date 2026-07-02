package uptime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeCronRepo is a minimal Repo stub for the cron-kick tests. It only
// implements ListEnrolledForProbe; all other methods panic if called (the
// kicker must NOT touch health_status, alert state, or metrics).
type fakeCronRepo struct {
	sites []EnrolledSite
	err   error
}

func (r *fakeCronRepo) ListEnrolledForProbe(_ context.Context) ([]EnrolledSite, error) {
	return r.sites, r.err
}
func (r *fakeCronRepo) SetSiteHealth(_ context.Context, _ uuid.UUID, _ string) (bool, error) {
	panic("CronKicker must not call SetSiteHealth")
}
func (r *fakeCronRepo) GetAlertState(_ context.Context, _ uuid.UUID) (AlertState, bool, error) {
	panic("CronKicker must not call GetAlertState")
}
func (r *fakeCronRepo) UpsertAlertState(_ context.Context, _ AlertState) error {
	panic("CronKicker must not call UpsertAlertState")
}
func (r *fakeCronRepo) TransitionAlertState(_ context.Context, _, _ uuid.UUID, _ bool, _ int, _ time.Time) (Transition, error) {
	panic("CronKicker must not call TransitionAlertState")
}
func (r *fakeCronRepo) ListAlertConfigsAllTenants(_ context.Context) ([]AlertConfig, error) {
	panic("CronKicker must not call ListAlertConfigsAllTenants")
}
func (r *fakeCronRepo) GetAlertConfig(_ context.Context, _ uuid.UUID) (AlertConfig, bool, error) {
	panic("CronKicker must not call GetAlertConfig")
}
func (r *fakeCronRepo) UpsertAlertConfig(_ context.Context, _ AlertConfig) (AlertConfig, error) {
	panic("CronKicker must not call UpsertAlertConfig")
}
func (r *fakeCronRepo) GetFleetSiteInfo(_ context.Context, _ uuid.UUID, _ []uuid.UUID) ([]FleetSiteInfo, error) {
	panic("CronKicker must not call GetFleetSiteInfo")
}
func (r *fakeCronRepo) GetFleetIncidents(_ context.Context, _ uuid.UUID, _ []uuid.UUID, _ time.Time, _ int) ([]FleetIncidentItem, error) {
	panic("CronKicker must not call GetFleetIncidents")
}

// TestCronKickFiresWPCronEndpoint verifies that the kicker fires a GET to
// /wp-cron.php for every enrolled site.
func TestCronKickFiresWPCronEndpoint(t *testing.T) {
	var hitCount atomic.Int64
	var lastPath atomic.Value

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitCount.Add(1)
		lastPath.Store(r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	repo := &fakeCronRepo{
		sites: []EnrolledSite{
			{ID: uuid.New(), TenantID: uuid.New(), URL: srv.URL},
		},
	}
	kicker := NewCronKicker(repo, testClient(), 5*time.Second, 5)
	if err := kicker.Kick(context.Background()); err != nil {
		t.Fatalf("Kick returned unexpected error: %v", err)
	}

	if hitCount.Load() != 1 {
		t.Fatalf("expected 1 hit on the test server, got %d", hitCount.Load())
	}
	p, _ := lastPath.Load().(string)
	if p != "/wp-cron.php" {
		t.Fatalf("expected path /wp-cron.php, got %q", p)
	}
}

// TestCronKickDoingWPCronParam verifies the doing_wp_cron query parameter is
// included in the kick URL. WP uses this parameter to detect an authoritative
// cron invocation and avoids recursively spawning another cron request.
func TestCronKickDoingWPCronParam(t *testing.T) {
	var rawQuery atomic.Value

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQuery.Store(r.URL.RawQuery)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	repo := &fakeCronRepo{
		sites: []EnrolledSite{
			{ID: uuid.New(), TenantID: uuid.New(), URL: srv.URL},
		},
	}
	kicker := NewCronKicker(repo, testClient(), 5*time.Second, 5)
	if err := kicker.Kick(context.Background()); err != nil {
		t.Fatalf("Kick returned unexpected error: %v", err)
	}

	qs, _ := rawQuery.Load().(string)
	if !strings.Contains(qs, "doing_wp_cron=") {
		t.Fatalf("expected doing_wp_cron query param, got raw query %q", qs)
	}
}

// TestCronKickRecordsNothing verifies that Kick does NOT call any
// health-status or metrics-writing repo methods. The fakeCronRepo panics on
// any method other than ListEnrolledForProbe, so a panic here means the kicker
// called a forbidden method.
func TestCronKickRecordsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	repo := &fakeCronRepo{
		sites: []EnrolledSite{
			{ID: uuid.New(), TenantID: uuid.New(), URL: srv.URL},
		},
	}
	kicker := NewCronKicker(repo, testClient(), 5*time.Second, 5)
	// If the kicker touches SetSiteHealth/GetAlertState/etc., the fake panics.
	if err := kicker.Kick(context.Background()); err != nil {
		t.Fatalf("Kick returned unexpected error: %v", err)
	}
}

// TestCronKickDisabledEnv verifies that an empty enrolled-site list results in
// no HTTP activity. In production the kicker is gated behind
// WPMGR_CRON_KICK_ENABLED (default true); when false, main.go simply does not
// wire or schedule the CronKicker, so the kick is never called.
func TestCronKickDisabledEnv(t *testing.T) {
	var hitCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hitCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	repo := &fakeCronRepo{sites: nil}
	kicker := NewCronKicker(repo, testClient(), 5*time.Second, 5)
	if err := kicker.Kick(context.Background()); err != nil {
		t.Fatalf("Kick on empty site list returned unexpected error: %v", err)
	}
	if hitCount.Load() != 0 {
		t.Fatalf("expected no HTTP hits when site list is empty, got %d", hitCount.Load())
	}
}

// TestCronKickUsesSSRFClient verifies that the kicker's request is blocked by
// the SSRF guard when constructed with a production-posture client. A loopback
// test server must NOT receive a hit. This mirrors TestProbeSSRFBlocked.
func TestCronKickUsesSSRFClient(t *testing.T) {
	var hitCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hitCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	repo := &fakeCronRepo{
		sites: []EnrolledSite{
			{ID: uuid.New(), TenantID: uuid.New(), URL: srv.URL},
		},
	}
	// guardedClient() has SSRF guard ON — loopback (127.x) is blocked.
	kicker := NewCronKicker(repo, guardedClient(), 5*time.Second, 5)
	// Fire-and-forget: no error returned even on SSRF block.
	if err := kicker.Kick(context.Background()); err != nil {
		t.Fatalf("Kick returned unexpected error: %v", err)
	}
	if hitCount.Load() != 0 {
		t.Fatalf("expected SSRF guard to block the loopback kick; server received %d hit(s)", hitCount.Load())
	}
}

// TestCronKickMultipleSites verifies that the kicker fans out to multiple
// enrolled sites and each receives exactly one kick.
func TestCronKickMultipleSites(t *testing.T) {
	var hitCount atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hitCount.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	srv1 := httptest.NewServer(handler)
	defer srv1.Close()
	srv2 := httptest.NewServer(handler)
	defer srv2.Close()
	srv3 := httptest.NewServer(handler)
	defer srv3.Close()

	repo := &fakeCronRepo{
		sites: []EnrolledSite{
			{ID: uuid.New(), TenantID: uuid.New(), URL: srv1.URL},
			{ID: uuid.New(), TenantID: uuid.New(), URL: srv2.URL},
			{ID: uuid.New(), TenantID: uuid.New(), URL: srv3.URL},
		},
	}
	kicker := NewCronKicker(repo, testClient(), 5*time.Second, 10)
	if err := kicker.Kick(context.Background()); err != nil {
		t.Fatalf("Kick returned unexpected error: %v", err)
	}
	if got := hitCount.Load(); got != 3 {
		t.Fatalf("expected 3 kicks (one per site), got %d", got)
	}
}

// TestCronKickURLBuilder unit-tests cronKickURL without a network round-trip.
func TestCronKickURLBuilder(t *testing.T) {
	cases := []struct {
		name     string
		siteURL  string
		wantPath string
	}{
		{
			name:     "plain http",
			siteURL:  "http://example.com",
			wantPath: "/wp-cron.php",
		},
		{
			name:     "trailing slash",
			siteURL:  "https://example.com/",
			wantPath: "/wp-cron.php",
		},
		{
			name:     "subdir install",
			siteURL:  "https://example.com/blog",
			wantPath: "/blog/wp-cron.php",
		},
		{
			name:     "subdir trailing slash",
			siteURL:  "https://example.com/blog/",
			wantPath: "/blog/wp-cron.php",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := cronKickURL(tc.siteURL)
			u, err := url.Parse(result)
			if err != nil {
				t.Fatalf("cronKickURL returned unparseable URL %q: %v", result, err)
			}
			if u.Path != tc.wantPath {
				t.Errorf("expected path %q, got %q (full URL: %s)", tc.wantPath, u.Path, result)
			}
			if !strings.Contains(u.RawQuery, "doing_wp_cron=") {
				t.Errorf("expected doing_wp_cron query param in %q", u.RawQuery)
			}
		})
	}
}
