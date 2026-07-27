// uptime_app_health_integration_test.go - GH #291 Phase 2: the
// application-health probe. Two load-bearing properties this whole change
// depends on:
//
//   - The COALESCE-guarded rollup: the app probe runs on a slower cadence
//     than the reachability probe, so most sweeps carry no app-health
//     opinion at all. A naive upsert would clobber a known app_up value with
//     NULL on those sweeps - TestAppHealthRollup_CoalesceGuard proves it
//     never does.
//   - The GOLDEN, bit-identical-on-upgrade property the design doc requires:
//     uptime percentages, sites.health_status, and site_incidents must be
//     EXACTLY the same whether or not the app probe ran, even when the app
//     probe conclusively finds the backend dead. "up" (reachability) is
//     frozen forever; only the new, orthogonal app_up signal may move.
package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
	"github.com/mosamlife/wpmgr/apps/api/internal/uptime"
)

// queryAppStatusRow reads the site_uptime_status app-health columns via the
// admin (RLS-bypassing) connection.
func queryAppStatusRow(t *testing.T, admin *db.Pool, siteID uuid.UUID) (latestAppUp *bool, reason string, lastAppProbedAt *time.Time, found bool) {
	t.Helper()
	ctx := context.Background()
	var (
		up      *bool
		reason2 *string
		at      *time.Time
	)
	err := admin.QueryRow(ctx,
		`SELECT latest_app_up, app_probe_reason, last_app_probed_at FROM site_uptime_status WHERE site_id = $1`,
		siteID).Scan(&up, &reason2, &at)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, "", nil, false
		}
		t.Fatalf("query app status row: %v", err)
	}
	r := ""
	if reason2 != nil {
		r = *reason2
	}
	return up, r, at, true
}

// queryDailyAppBucket reads the site_uptime_daily app-health counters via the
// admin connection. NULL (never touched) reads back as 0/false-found.
func queryDailyAppBucket(t *testing.T, admin *db.Pool, siteID uuid.UUID, day time.Time) (appUpChecks, appTotalChecks int32, found bool) {
	t.Helper()
	ctx := context.Background()
	var (
		up    *int32
		total *int32
	)
	err := admin.QueryRow(ctx,
		`SELECT app_up_checks, app_total_checks FROM site_uptime_daily WHERE site_id = $1 AND day = $2`,
		siteID, day.UTC().Truncate(24*time.Hour)).Scan(&up, &total)
	if err != nil {
		if err == pgx.ErrNoRows {
			return 0, 0, false
		}
		t.Fatalf("query daily app bucket: %v", err)
	}
	if up != nil {
		appUpChecks = *up
	}
	if total != nil {
		appTotalChecks = *total
	}
	return appUpChecks, appTotalChecks, true
}

// TestAppHealthRollup_CoalesceGuard proves metrics.pgStore.UpsertRollup never
// lets a NULL (no app-probe attempt this check - the common case, since the
// app probe runs on a slower cadence than the reachability probe) overwrite
// a previously-recorded known app_up value, and that it DOES advance once a
// genuinely fresher app-probe attempt arrives.
func TestAppHealthRollup_CoalesceGuard(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	store := metrics.NewPostgres(pool, nil)
	rw, ok := store.(metrics.RollupWriter)
	if !ok {
		t.Fatal("metrics.NewPostgres must return a RollupWriter")
	}

	tenantID := seedTenant(t, pool, "app-health-coalesce-"+uuid.NewString()[:8])
	siteID := seedSiteFor(t, admin, tenantID, "https://"+uuid.NewString()+".example.com")

	now := time.Now().UTC()
	appTrue := true
	appFalse := false

	// Check 1: a reachability probe that ALSO attempted (and succeeded) an
	// app probe.
	check1 := metrics.Check{
		TenantID: tenantID, SiteID: siteID, CheckedAt: now, Up: true, TotalMs: 50,
		AppUp: &appTrue, AppProbeReason: uptime.AppProbeReasonRestOK,
	}
	if err := rw.UpsertRollup(ctx, []metrics.Check{check1}); err != nil {
		t.Fatalf("upsert rollup (check1): %v", err)
	}

	upAfter1, reasonAfter1, atAfter1, found := queryAppStatusRow(t, admin, siteID)
	if !found || upAfter1 == nil || !*upAfter1 || reasonAfter1 != uptime.AppProbeReasonRestOK {
		t.Fatalf("after check1: latest_app_up=%v reason=%q found=%v, want true/rest_ok", upAfter1, reasonAfter1, found)
	}
	if atAfter1 == nil {
		t.Fatal("after check1: expected last_app_probed_at to be set")
	}

	// Checks 2-5: FOUR consecutive reachability-only sweeps (no app-probe
	// attempt this tick - mirrors the ~4-of-5 cadence ratio at the documented
	// defaults). Each one must NOT clobber the known app_up=true.
	for i := 2; i <= 5; i++ {
		check := metrics.Check{
			TenantID: tenantID, SiteID: siteID, CheckedAt: now.Add(time.Duration(i) * time.Minute), Up: true, TotalMs: 50,
			// AppUp/AppProbeReason left zero-value: no app-probe attempt.
		}
		if err := rw.UpsertRollup(ctx, []metrics.Check{check}); err != nil {
			t.Fatalf("upsert rollup (check%d): %v", i, err)
		}
		up, reason, at, found := queryAppStatusRow(t, admin, siteID)
		if !found || up == nil || !*up || reason != uptime.AppProbeReasonRestOK {
			t.Fatalf("after check%d (no app-probe attempt): latest_app_up=%v reason=%q found=%v - a NULL/absent app opinion must NEVER clobber the known value", i, up, reason, found)
		}
		if at == nil || !at.Equal(*atAfter1) {
			t.Fatalf("after check%d: last_app_probed_at moved (%v) even though no app probe was attempted - want unchanged (%v)", i, at, atAfter1)
		}
	}

	// Check 6: a genuinely fresher app-probe attempt (conclusively false)
	// must advance the stored verdict, reason, and timestamp.
	check6At := now.Add(6 * time.Minute)
	check6 := metrics.Check{
		TenantID: tenantID, SiteID: siteID, CheckedAt: check6At, Up: true, TotalMs: 50,
		AppUp: &appFalse, AppProbeReason: uptime.AppProbeReasonRest5xx,
	}
	if err := rw.UpsertRollup(ctx, []metrics.Check{check6}); err != nil {
		t.Fatalf("upsert rollup (check6): %v", err)
	}
	upAfter6, reasonAfter6, atAfter6, found := queryAppStatusRow(t, admin, siteID)
	if !found || upAfter6 == nil || *upAfter6 || reasonAfter6 != uptime.AppProbeReasonRest5xx {
		t.Fatalf("after check6: latest_app_up=%v reason=%q found=%v, want false/rest_5xx", upAfter6, reasonAfter6, found)
	}
	if atAfter6 == nil || !atAfter6.Truncate(time.Microsecond).Equal(check6At.Truncate(time.Microsecond)) {
		t.Fatalf("after check6: last_app_probed_at = %v, want %v", atAfter6, check6At)
	}

	// site_uptime_daily: app_total_checks counts every ATTEMPT (check1 and
	// check6 - 2 attempts), app_up_checks counts only the conclusive true
	// (check1 - 1). The 4 no-attempt checks (2-5) contribute 0/0.
	appUpChecks, appTotalChecks, found := queryDailyAppBucket(t, admin, siteID, now)
	if !found {
		t.Fatal("expected a site_uptime_daily row")
	}
	if appTotalChecks != 2 {
		t.Fatalf("app_total_checks = %d, want 2 (only check1 and check6 attempted)", appTotalChecks)
	}
	if appUpChecks != 1 {
		t.Fatalf("app_up_checks = %d, want 1 (only check1 was conclusively true)", appUpChecks)
	}

	// The reachability counters must be untouched by any of this: all 6
	// checks were Up=true.
	upChecks, totalChecks, _, _, found := queryDailyBucket(t, admin, siteID, now)
	if !found || upChecks != 6 || totalChecks != 6 {
		t.Fatalf("reachability up_checks=%d total_checks=%d found=%v, want 6/6 (app-health writes must never affect these)", upChecks, totalChecks, found)
	}
}

// appHealthGoldenScenario seeds a tenant+site whose reachability probe target
// (site root) always answers a cached 200 and whose app-probe target
// (/wp-json/) always answers 500 - the literal GH #291 incident shape: a
// page cache masking a completely dead PHP backend. withAppProbe controls
// whether the app prober is wired at all.
func appHealthGoldenScenario(t *testing.T, withAppProbe bool) (healthStatus string, uptimePct float64, incidentCount int64, latestAppUp *bool) {
	t.Helper()
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/wp-json/" {
			// The app probe's target: a dead PHP backend.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// The reachability probe's target (site root): a cached 200 - a
		// visitor IS being served, which is the honest, unchanged meaning
		// of "up".
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>cached page, dead backend</html>"))
	}))
	defer srv.Close()

	tenant := seedTenant(t, pool, "app-health-golden-"+uuid.NewString()[:8])
	s := enrollFakeSite(t, pool, tenant, srv.URL)
	// Enrollment itself stamps last_seen_at = now(), which would otherwise
	// make B0 (agent ground truth) short-circuit every sweep in this test
	// and never actually reach the /wp-json/ target below. Backdate it to
	// simulate a site whose heartbeat is stale enough that B0 correctly
	// falls through to the direct measurement - the exact situation Phase 2
	// exists for.
	if _, err := admin.Exec(ctx, "UPDATE sites SET last_seen_at = now() - interval '1 hour' WHERE id = $1", s.ID); err != nil {
		t.Fatalf("backdate last_seen_at: %v", err)
	}

	store := metrics.NewPostgres(pool, nil)
	repo := uptime.NewRepo(pool)
	prober := uptime.NewProber(loopbackClient(), 5*time.Second)
	w := uptime.NewProbeWorker(repo, prober, store, nil, nil, nil, 5, 2)
	if withAppProbe {
		appProber := uptime.NewAppProber(loopbackClient(), 5*time.Second)
		// probeInterval == appProbeInterval (ratio 1): every sweep also
		// attempts an app probe, so the golden comparison does not depend on
		// timing/hashing luck.
		w.SetAppProber(appProber, time.Minute, time.Minute)
	}

	now := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := w.Sweep(ctx, now); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}

	var health string
	if err := admin.QueryRow(ctx, "SELECT health_status FROM sites WHERE id = $1", s.ID).Scan(&health); err != nil {
		t.Fatalf("read health_status: %v", err)
	}

	agg, err := store.QueryAggregate(ctx, tenant, s.ID, 24*time.Hour)
	if err != nil {
		t.Fatalf("query aggregate: %v", err)
	}

	var incidents int64
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM site_incidents WHERE site_id = $1", s.ID).Scan(&incidents); err != nil {
		t.Fatalf("count incidents: %v", err)
	}

	var appUp *bool
	if withAppProbe {
		appUp, _, _, _ = queryAppStatusRow(t, admin, s.ID)
	}

	return health, agg.UptimePct, incidents, appUp
}

// TestAppHealthGolden_FrozenSignalsBitIdentical is the design doc's required
// golden test: a site serving cached 200s over a dead backend must report
// EXACTLY the same sites.health_status, uptime percentage, and
// site_incidents count whether or not the app probe ran - even though the
// app probe conclusively (and correctly) determines the backend is dead.
// Only the new, orthogonal app_up signal may move.
func TestAppHealthGolden_FrozenSignalsBitIdentical(t *testing.T) {
	baselineHealth, baselinePct, baselineIncidents, baselineAppUp := appHealthGoldenScenario(t, false)
	withAppHealth, withAppPct, withAppIncidents, withAppAppUp := appHealthGoldenScenario(t, true)

	if baselineAppUp != nil {
		t.Fatalf("baseline (app probe disabled) scenario unexpectedly recorded an app_up value: %v", baselineAppUp)
	}

	// The frozen triad: bit-identical regardless of the app probe.
	if baselineHealth != withAppHealth {
		t.Fatalf("sites.health_status diverged: baseline=%q with-app-probe=%q - the app probe must NEVER affect this frozen signal", baselineHealth, withAppHealth)
	}
	if baselineHealth != "healthy" {
		t.Fatalf("sanity: expected health_status=healthy for a cached-200 reachability target, got %q", baselineHealth)
	}
	if baselinePct != withAppPct {
		t.Fatalf("uptime percentage diverged: baseline=%.4f with-app-probe=%.4f - the app probe must NEVER affect this frozen signal", baselinePct, withAppPct)
	}
	if baselinePct != 100 {
		t.Fatalf("sanity: expected 100%% uptime (every reachability probe was a cached 200), got %.4f", baselinePct)
	}
	if baselineIncidents != withAppIncidents {
		t.Fatalf("site_incidents count diverged: baseline=%d with-app-probe=%d - the app probe must NEVER affect this frozen signal", baselineIncidents, withAppIncidents)
	}
	if baselineIncidents != 0 {
		t.Fatalf("sanity: expected 0 incidents (reachability never went down), got %d", baselineIncidents)
	}

	// Proof the test scenario actually exercised something real: the app
	// probe DID record a conclusive false verdict - the whole point of
	// Phase 2, orthogonal to (and not reflected in) any of the frozen
	// signals asserted above.
	if withAppAppUp == nil {
		t.Fatal("expected the app-probe scenario to record a conclusive app_up value, got none")
	}
	if *withAppAppUp {
		t.Fatal("expected the app-probe scenario to record app_up=false (dead backend behind the cache), got true")
	}
}
