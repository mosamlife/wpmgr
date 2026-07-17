// uptime_rollup_integration_test.go — m99: the durable fix for the
// /api/v1/sites cold-uptime-scan latency. Proves the site_uptime_daily /
// site_uptime_status rollup (metrics.pgStore.UpsertRollup, wired into
// uptime.ProbeWorker.Sweep) is correctly maintained and that the rewritten
// QueryFleetUptime, which now reads ONLY the rollup, is numerically
// equivalent to a direct aggregate over the raw site_uptime_probes rows —
// the load-bearing property this whole change depends on. Also proves the
// two new tables carry the same RLS shape as every other tenant-scoped,
// direct site-keyed table (mirrors perf_rls_integration_test.go).
package tests

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
	"github.com/mosamlife/wpmgr/apps/api/internal/uptime"
)

// ---------------------------------------------------------------------------
// Equivalence: rollup-backed QueryFleetUptime === direct raw-probe aggregate
// ---------------------------------------------------------------------------

// rawFleetUptimeAggregate independently reproduces the OLD (pre-m99)
// QueryFleetUptime's single-site aggregate directly against the raw
// site_uptime_probes table, via the admin (superuser, RLS-bypassing)
// connection. This is the reference the rollup-backed implementation must
// match — it is a copy of the exact SQL QueryFleetUptime used to compute
// before this change (see git history of internal/metrics/postgres.go), not
// a re-derivation, so a real behavioral drift between the two would show up
// as a test failure rather than being masked by both sides sharing a bug.
func rawFleetUptimeAggregate(t *testing.T, admin *db.Pool, tenantID, siteID uuid.UUID, since time.Time) (latestUp *bool, latestAt, latestTLS *time.Time, uptimePct, avgLatency *float64) {
	t.Helper()
	ctx := context.Background()

	row := admin.QueryRow(ctx, `
SELECT
    lat.up, lat.probed_at, lat.tls_expiry,
    agg.checks, agg.up_checks, agg.avg_latency
FROM (SELECT $2::uuid AS id) AS s
LEFT JOIN LATERAL (
    SELECT up, probed_at, tls_expiry
    FROM site_uptime_probes
    WHERE site_id = s.id AND tenant_id = $1
    ORDER BY probed_at DESC
    LIMIT 1
) lat ON true
LEFT JOIN LATERAL (
    SELECT
        COUNT(*)                                   AS checks,
        COUNT(*) FILTER (WHERE up)                 AS up_checks,
        AVG(NULLIF(total_ms, 0)) FILTER (WHERE up)  AS avg_latency
    FROM site_uptime_probes
    WHERE site_id = s.id AND tenant_id = $1 AND probed_at >= $3
) agg ON true`, tenantID, siteID, since)

	var (
		up            *bool
		probedAt      *time.Time
		tlsExpiry     *time.Time
		checks        *int64
		upChecks      *int64
		avgLatencyRaw *float64
	)
	if err := row.Scan(&up, &probedAt, &tlsExpiry, &checks, &upChecks, &avgLatencyRaw); err != nil {
		t.Fatalf("raw fleet uptime aggregate: %v", err)
	}
	if up == nil {
		return nil, nil, nil, nil, nil
	}
	if checks != nil && *checks > 0 && upChecks != nil {
		pct := float64(*upChecks) / float64(*checks) * 100
		uptimePct = &pct
	}
	return up, probedAt, tlsExpiry, uptimePct, avgLatencyRaw
}

func floatsClose(a, b float64) bool {
	return math.Abs(a-b) < 0.01
}

// TestQueryFleetUptime_RollupMatchesRawAggregate is the load-bearing
// equivalence test: for a seeded, mixed (up/down, some zero-latency-while-up,
// two different UTC days) set of raw probes, the rollup-backed
// QueryFleetUptime returns the SAME uptime %, avg latency, and latest status
// as a direct aggregate over the raw rows.
func TestQueryFleetUptime_RollupMatchesRawAggregate(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	store := metrics.NewPostgres(pool, nil)
	rw, ok := store.(metrics.RollupWriter)
	if !ok {
		t.Fatal("metrics.NewPostgres must return a RollupWriter")
	}

	tenantID := seedTenant(t, pool, "uptime-rollup-eq-"+uuid.NewString()[:8])
	siteID := seedSiteFor(t, admin, tenantID, "https://"+uuid.NewString()+".example.com")
	// A second, noise site: must not leak into siteID's aggregate.
	noiseSiteID := seedSiteFor(t, admin, tenantID, "https://"+uuid.NewString()+".example.com")

	now := time.Now().UTC()
	yesterday := now.Add(-26 * time.Hour) // a different UTC day, safely inside a 30d window.

	checks := []metrics.Check{
		// Today: 3 up (one with zero latency — must be EXCLUDED from the
		// latency average, mirroring the old AVG(NULLIF(total_ms,0)) FILTER
		// (WHERE up) semantics), 1 down.
		{TenantID: tenantID, SiteID: siteID, CheckedAt: now.Add(-3 * time.Minute), Up: true, TotalMs: 120},
		{TenantID: tenantID, SiteID: siteID, CheckedAt: now.Add(-2 * time.Minute), Up: true, TotalMs: 0}, // excluded from avg
		{TenantID: tenantID, SiteID: siteID, CheckedAt: now.Add(-1 * time.Minute), Up: false, TotalMs: 0},
		{TenantID: tenantID, SiteID: siteID, CheckedAt: now, Up: true, TotalMs: 80},
		// Yesterday (a different day bucket): 2 up.
		{TenantID: tenantID, SiteID: siteID, CheckedAt: yesterday, Up: true, TotalMs: 200},
		{TenantID: tenantID, SiteID: siteID, CheckedAt: yesterday.Add(time.Minute), Up: true, TotalMs: 300},
		// Noise for a different site — must not affect siteID's result.
		{TenantID: tenantID, SiteID: noiseSiteID, CheckedAt: now, Up: false, TotalMs: 999},
	}

	if err := store.InsertChecks(ctx, checks); err != nil {
		t.Fatalf("insert checks: %v", err)
	}
	if err := rw.UpsertRollup(ctx, checks); err != nil {
		t.Fatalf("upsert rollup: %v", err)
	}

	const window30d = 30 * 24 * time.Hour
	since := time.Now().Add(-window30d)

	wantUp, wantAt, wantTLS, wantPct, wantAvg := rawFleetUptimeAggregate(t, admin, tenantID, siteID, since)
	if wantUp == nil {
		t.Fatal("raw aggregate reference found no data — test fixture bug")
	}

	got, err := store.QueryFleetUptime(ctx, tenantID, []uuid.UUID{siteID, noiseSiteID}, window30d)
	if err != nil {
		t.Fatalf("QueryFleetUptime: %v", err)
	}
	row, ok := got[siteID]
	if !ok {
		t.Fatal("QueryFleetUptime: siteID missing from result (should have rollup data)")
	}

	// Latest status must match exactly (the most recent check was "now",
	// Up: true).
	if row.Up == nil || *row.Up != *wantUp {
		t.Fatalf("Up mismatch: rollup=%v raw=%v", row.Up, wantUp)
	}
	// Compare at microsecond granularity: Postgres timestamptz truncates the
	// nanosecond time.Time, so a Go-native expected value would otherwise diverge.
	if row.LastProbeAt == nil || wantAt == nil ||
		!row.LastProbeAt.Truncate(time.Microsecond).Equal(wantAt.Truncate(time.Microsecond)) {
		t.Fatalf("LastProbeAt mismatch: rollup=%v raw=%v", row.LastProbeAt, wantAt)
	}
	if (row.TLSExpiry == nil) != (wantTLS == nil) {
		t.Fatalf("TLSExpiry nil-ness mismatch: rollup=%v raw=%v", row.TLSExpiry, wantTLS)
	}

	// 6 checks total for siteID (4 today + 2 yesterday), 5 up, 1 down ⇒
	// 83.33% uptime.
	if row.UptimePct7d == nil || wantPct == nil {
		t.Fatalf("UptimePct7d missing: rollup=%v raw=%v", row.UptimePct7d, wantPct)
	}
	if !floatsClose(*row.UptimePct7d, *wantPct) {
		t.Fatalf("UptimePct7d mismatch: rollup=%.4f raw=%.4f", *row.UptimePct7d, *wantPct)
	}
	wantPctExact := float64(5) / float64(6) * 100
	if !floatsClose(*row.UptimePct7d, wantPctExact) {
		t.Fatalf("UptimePct7d = %.4f, want %.4f (5/6 up)", *row.UptimePct7d, wantPctExact)
	}

	// avg latency over up checks with total_ms != 0: (120 + 80 + 200 + 300) / 4
	// = 175. The zero-latency up check must be excluded (matches NULLIF).
	if row.AvgLatencyMs == nil || wantAvg == nil {
		t.Fatalf("AvgLatencyMs missing: rollup=%v raw=%v", row.AvgLatencyMs, wantAvg)
	}
	if !floatsClose(*row.AvgLatencyMs, *wantAvg) {
		t.Fatalf("AvgLatencyMs mismatch: rollup=%.4f raw=%.4f", *row.AvgLatencyMs, *wantAvg)
	}
	wantAvgExact := (120.0 + 80.0 + 200.0 + 300.0) / 4.0
	if !floatsClose(*row.AvgLatencyMs, wantAvgExact) {
		t.Fatalf("AvgLatencyMs = %.4f, want %.4f", *row.AvgLatencyMs, wantAvgExact)
	}

	// The noise site's own down probe must not appear under siteID, and
	// siteID's data must not leak into it.
	noiseRow, ok := got[noiseSiteID]
	if !ok {
		t.Fatal("noise site missing from result")
	}
	if noiseRow.Up == nil || *noiseRow.Up {
		t.Fatalf("noise site Up = %v, want false (site scoping leak?)", noiseRow.Up)
	}
	if noiseRow.UptimePct7d == nil || !floatsClose(*noiseRow.UptimePct7d, 0) {
		t.Fatalf("noise site UptimePct7d = %v, want 0", noiseRow.UptimePct7d)
	}
}

// TestQueryFleetUptime_NoRollupData_SiteOmittedFromMap proves a site with no
// probe/rollup history is absent from the result map — the same contract the
// old raw-probe query had ("missing == no data").
func TestQueryFleetUptime_NoRollupData_SiteOmittedFromMap(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	store := metrics.NewPostgres(pool, nil)
	tenantID := seedTenant(t, pool, "uptime-rollup-empty-"+uuid.NewString()[:8])
	siteID := seedSiteFor(t, admin, tenantID, "https://"+uuid.NewString()+".example.com")

	got, err := store.QueryFleetUptime(ctx, tenantID, []uuid.UUID{siteID}, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("QueryFleetUptime: %v", err)
	}
	if _, ok := got[siteID]; ok {
		t.Fatal("expected never-probed site to be absent from the result map")
	}
}

// ---------------------------------------------------------------------------
// Worker upsert: exactly-once-per-sweep, additive across sweeps, new-day bucket
// ---------------------------------------------------------------------------

func queryDailyBucket(t *testing.T, admin *db.Pool, siteID uuid.UUID, day time.Time) (upChecks, totalChecks, latencySamples int64, sumLatency float64, found bool) {
	t.Helper()
	ctx := context.Background()
	err := admin.QueryRow(ctx,
		`SELECT up_checks, total_checks, latency_samples, sum_latency_ms
		 FROM site_uptime_daily WHERE site_id = $1 AND day = $2`,
		siteID, day.UTC().Truncate(24*time.Hour)).
		Scan(&upChecks, &totalChecks, &latencySamples, &sumLatency)
	if err != nil {
		if err == pgx.ErrNoRows {
			return 0, 0, 0, 0, false
		}
		t.Fatalf("query daily bucket: %v", err)
	}
	return upChecks, totalChecks, latencySamples, sumLatency, true
}

func queryStatusRow(t *testing.T, admin *db.Pool, siteID uuid.UUID) (latestUp bool, lastProbedAt time.Time, found bool) {
	t.Helper()
	ctx := context.Background()
	err := admin.QueryRow(ctx,
		`SELECT latest_up, last_probed_at FROM site_uptime_status WHERE site_id = $1`, siteID).
		Scan(&latestUp, &lastProbedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return false, time.Time{}, false
		}
		t.Fatalf("query status row: %v", err)
	}
	return latestUp, lastProbedAt, true
}

// TestProbeWorker_RollupUpsert_ExactlyOncePerSweep proves the ProbeWorker's
// per-sweep rollup write (metrics.pgStore.UpsertRollup, called from
// uptime.ProbeWorker.Sweep) increments the today bucket by exactly 1 per
// sweep, accumulates additively across sweeps (no overwrite), and keeps
// site_uptime_status fresh — the exact contract QueryFleetUptime depends on.
func TestProbeWorker_RollupUpsert_ExactlyOncePerSweep(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	var status int32 = http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(atomic.LoadInt32(&status)))
	}))
	defer srv.Close()

	tenant := seedTenant(t, pool, "uptime-rollup-worker-"+uuid.NewString()[:8])
	s := enrollFakeSite(t, pool, tenant, srv.URL)

	store := metrics.NewPostgres(pool, nil)
	repo := uptime.NewRepo(pool)
	prober := uptime.NewProber(loopbackClient(), 5*time.Second)
	w := uptime.NewProbeWorker(repo, prober, store, nil, nil, nil, 5, 2)

	now := time.Now()

	// Sweep 1 (up): today's bucket goes from absent to 1/1.
	if _, err := w.Sweep(ctx, now); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	up, total, _, _, found := queryDailyBucket(t, admin, s.ID, now)
	if !found {
		t.Fatal("sweep 1: expected a daily bucket row to exist")
	}
	if up != 1 || total != 1 {
		t.Fatalf("sweep 1: up_checks=%d total_checks=%d, want 1/1", up, total)
	}
	latestUp, lastProbedAt, found := queryStatusRow(t, admin, s.ID)
	if !found || !latestUp {
		t.Fatalf("sweep 1: status row missing or latest_up=false, found=%v up=%v", found, latestUp)
	}
	if time.Since(lastProbedAt) > time.Minute {
		t.Fatalf("sweep 1: last_probed_at looks stale: %v", lastProbedAt)
	}

	// Sweep 2 (still up), same UTC day: additive increment, not overwrite.
	if _, err := w.Sweep(ctx, now); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	up, total, _, _, found = queryDailyBucket(t, admin, s.ID, now)
	if !found || up != 2 || total != 2 {
		t.Fatalf("sweep 2: up_checks=%d total_checks=%d found=%v, want 2/2", up, total, found)
	}

	// Sweep 3: site goes DOWN. total increments, up_checks does NOT.
	atomic.StoreInt32(&status, http.StatusInternalServerError)
	if _, err := w.Sweep(ctx, now); err != nil {
		t.Fatalf("sweep 3: %v", err)
	}
	up, total, _, _, found = queryDailyBucket(t, admin, s.ID, now)
	if !found || up != 2 || total != 3 {
		t.Fatalf("sweep 3: up_checks=%d total_checks=%d found=%v, want 2/3 (down probe must not increment up_checks)", up, total, found)
	}
	latestUp, _, found = queryStatusRow(t, admin, s.ID)
	if !found || latestUp {
		t.Fatalf("sweep 3: status row latest_up=%v found=%v, want false (freshest probe was down)", latestUp, found)
	}

	// A different UTC day creates a SEPARATE bucket; today's bucket is
	// untouched. Exercised directly via UpsertRollup (the worker itself has
	// no way to fake "tomorrow" — this proves the day-keying, not the
	// worker's own timekeeping).
	rw := store.(metrics.RollupWriter)
	yesterday := now.Add(-25 * time.Hour)
	if err := rw.UpsertRollup(ctx, []metrics.Check{
		{TenantID: tenant, SiteID: s.ID, CheckedAt: yesterday, Up: true, TotalMs: 50},
	}); err != nil {
		t.Fatalf("upsert rollup (yesterday): %v", err)
	}
	yUp, yTotal, _, _, yFound := queryDailyBucket(t, admin, s.ID, yesterday)
	if !yFound || yUp != 1 || yTotal != 1 {
		t.Fatalf("yesterday bucket: up=%d total=%d found=%v, want 1/1", yUp, yTotal, yFound)
	}
	// Today's bucket must be completely unaffected by the new-day write.
	up, total, _, _, found = queryDailyBucket(t, admin, s.ID, now)
	if !found || up != 2 || total != 3 {
		t.Fatalf("today bucket after cross-day write: up_checks=%d total_checks=%d found=%v, want unchanged 2/3", up, total, found)
	}
}

// TestProbeWorker_RollupUpsert_NoOpOnEmptyInput proves UpsertRollup no-ops
// cleanly on an empty batch (the ProbeWorker calls it every sweep even when
// no sites are enrolled).
func TestProbeWorker_RollupUpsert_NoOpOnEmptyInput(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	store := metrics.NewPostgres(pool, nil)
	rw := store.(metrics.RollupWriter)
	if err := rw.UpsertRollup(ctx, nil); err != nil {
		t.Fatalf("UpsertRollup(nil) should no-op, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// RLS: site_uptime_daily / site_uptime_status carry the standard tenant
// isolation + agent + WITH CHECK shape (mirrors TestPerfConfigRLS).
// ---------------------------------------------------------------------------

func TestUptimeRollupTablesRLS(t *testing.T) {
	app := startPostgres(t)
	admin := connectAdmin(t, app)
	defer admin.Close()
	ctx := context.Background()

	tenantA := seedTenant(t, app, "uptime-rollup-rls-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, app, "uptime-rollup-rls-b-"+uuid.NewString()[:8])
	siteA := seedSiteFor(t, admin, tenantA, "https://"+uuid.NewString()+".example.com")
	siteA2 := seedSiteFor(t, admin, tenantA, "https://"+uuid.NewString()+".example.com")

	today := time.Now().UTC().Truncate(24 * time.Hour)

	if _, err := admin.Exec(ctx,
		`INSERT INTO site_uptime_daily (tenant_id, site_id, day, up_checks, total_checks, sum_latency_ms, latency_samples)
		 VALUES ($1, $2, $3, 1, 1, 100, 1)`,
		tenantA, siteA, today); err != nil {
		t.Fatalf("seed site_uptime_daily: %v", err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO site_uptime_status (site_id, tenant_id, latest_up, last_probed_at)
		 VALUES ($1, $2, true, now())`,
		siteA, tenantA); err != nil {
		t.Fatalf("seed site_uptime_status: %v", err)
	}

	countUnder := func(table string, run func(fn func(pgx.Tx) error) error) int {
		var n int
		if err := run(func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE site_id = $1`, siteA).Scan(&n)
		}); err != nil {
			t.Fatalf("count query on %s: %v", table, err)
		}
		return n
	}

	for _, table := range []string{"site_uptime_daily", "site_uptime_status"} {
		if got := countUnder(table, func(fn func(pgx.Tx) error) error { return app.InTenantTx(ctx, tenantA, fn) }); got != 1 {
			t.Fatalf("%s: tenant A must see its own row, got %d", table, got)
		}
		if got := countUnder(table, func(fn func(pgx.Tx) error) error { return app.InTenantTx(ctx, tenantB, fn) }); got != 0 {
			t.Fatalf("%s: tenant B must NOT see tenant A's row (tenant_isolation), got %d", table, got)
		}
		if got := countUnder(table, func(fn func(pgx.Tx) error) error { return app.InAgentTx(ctx, fn) }); got != 1 {
			t.Fatalf("%s: agent scope must see the row cross-tenant (agent policy), got %d", table, got)
		}
	}

	// WITH CHECK: tenant B cannot write a row carrying tenant A's id, even for
	// a real site of A's (siteA2 has no rows yet).
	errDaily := app.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO site_uptime_daily (tenant_id, site_id, day, up_checks, total_checks) VALUES ($1, $2, $3, 1, 1)`,
			tenantA, siteA2, today)
		return e
	})
	if errDaily == nil {
		t.Fatal("tenant B must NOT be able to write a site_uptime_daily row for tenant A (WITH CHECK)")
	}
	errStatus := app.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO site_uptime_status (site_id, tenant_id, latest_up, last_probed_at) VALUES ($1, $2, true, now())`,
			siteA2, tenantA)
		return e
	})
	if errStatus == nil {
		t.Fatal("tenant B must NOT be able to write a site_uptime_status row for tenant A (WITH CHECK)")
	}

	var totalDaily, totalStatus int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM site_uptime_daily`).Scan(&totalDaily); err != nil {
		t.Fatalf("total daily count: %v", err)
	}
	if totalDaily != 1 {
		t.Fatalf("expected exactly 1 site_uptime_daily row overall, got %d", totalDaily)
	}
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM site_uptime_status`).Scan(&totalStatus); err != nil {
		t.Fatalf("total status count: %v", err)
	}
	if totalStatus != 1 {
		t.Fatalf("expected exactly 1 site_uptime_status row overall, got %d", totalStatus)
	}
}
