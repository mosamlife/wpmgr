package metrics

// postgres_fleet_series_pg_test.go: real-Postgres proofs for
// QueryFleetDailySeries, the batched fleet daily strip behind
// GET /api/v1/fleet/uptime-history (GH #460).
//
// These run against a live Postgres as wpmgr_app — the non-superuser,
// non-BYPASSRLS role every install actually runs as — through the same
// db.Pool the request path uses, and seed via the production write paths
// (InsertChecks + UpsertRollup) rather than hand-written INSERTs. A proof
// that opened its own superuser connection would leave the RLS policies
// inert and pass regardless.
//
// Container/seed helpers (startMetricsTestPostgres, metricsSeedTenant,
// metricsSeedSite, absDiff) live in postgres_explain_test.go; seedWithRollup
// lives in postgres_persite_pg_test.go.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestQueryFleetDailySeries_SeededGapProducesNoPoints is the load-bearing
// GH #460 proof at the SQL level: a site measured for the OLDEST half of a
// 90-day window and never probed for the most recent half must produce points
// for the measured days only, and NOTHING for the gap.
//
// The seed is lopsided on purpose. The failure this endpoint is most likely
// to have is returning the right number of days with default values in them,
// which a count assertion cannot see. So this asserts which specific UTC dates
// carry points, that the gap dates carry none, and that each measured day's
// counters match what was written — not merely that they are non-zero.
func TestQueryFleetDailySeries_SeededGapProducesNoPoints(t *testing.T) {
	app, admin := startMetricsTestPostgres(t)
	ctx := context.Background()
	store := NewPostgres(app, nil)

	const window = 90 * 24 * time.Hour
	now := time.Now()
	_, today, _, _, _, _, _ := fleetUptimeParams(now, window)

	tenant := metricsSeedTenant(t, admin, "fleetseries-"+uuid.NewString()[:8])
	gapped := metricsSeedSite(t, admin, tenant, "https://"+uuid.NewString()+".example.com")
	// A second site in the same tenant, measured on a DISJOINT set of days.
	// It is what proves the batch keys each point to the right site rather
	// than smearing one site's history across the fleet.
	other := metricsSeedSite(t, admin, tenant, "https://"+uuid.NewString()+".example.com")

	// gapped: D-89..D-45 measured, D-44..D-0 silent.
	// The counters differ per day so a constant-returning implementation
	// cannot pass.
	var checks []Check
	wantGapped := map[string]struct{ up, total int }{}
	for d := 89; d >= 45; d-- {
		day := today.AddDate(0, 0, -d)
		total := 4
		up := 4
		if d%10 == 0 {
			up = 3 // a few partial days, so not every value is 100%
		}
		for i := 0; i < total; i++ {
			checks = append(checks, Check{
				TenantID: tenant, SiteID: gapped,
				CheckedAt: day.Add(time.Duration(i+1) * time.Hour),
				Up:        i < up, TotalMs: 120,
			})
		}
		wantGapped[day.Format("2006-01-02")] = struct{ up, total int }{up, total}
	}

	// other: measured only on D-10, squarely inside gapped's silent stretch.
	otherDay := today.AddDate(0, 0, -10)
	checks = append(checks, Check{
		TenantID: tenant, SiteID: other,
		CheckedAt: otherDay.Add(6 * time.Hour), Up: true, TotalMs: 200,
	})

	seedWithRollup(t, store, checks)

	got, err := store.QueryFleetDailySeries(ctx, tenant, []uuid.UUID{gapped, other}, window)
	if err != nil {
		t.Fatalf("QueryFleetDailySeries: %v", err)
	}

	// --- gapped -------------------------------------------------------
	gotDays := map[string]Point{}
	for _, p := range got[gapped] {
		key := p.Bucket.UTC().Format("2006-01-02")
		if _, dup := gotDays[key]; dup {
			t.Fatalf("day %s appears twice for the gapped site — the decomposition double-counted it", key)
		}
		gotDays[key] = p
	}
	if len(gotDays) != len(wantGapped) {
		t.Errorf("gapped site returned %d days, want %d", len(gotDays), len(wantGapped))
	}

	// Every measured day is present with its exact counters.
	for day, want := range wantGapped {
		p, ok := gotDays[day]
		if !ok {
			t.Errorf("measured day %s is missing from the series", day)
			continue
		}
		if int(p.Checks) != want.total || int(p.UpChecks) != want.up {
			t.Errorf("day %s: checks=%d up=%d, want checks=%d up=%d",
				day, p.Checks, p.UpChecks, want.total, want.up)
		}
	}

	// Every gap day is ABSENT. This is the assertion that catches a
	// zero-filled series: such an implementation returns 90 days here, all
	// present, and only this loop notices.
	for d := 44; d >= 0; d-- {
		day := today.AddDate(0, 0, -d).Format("2006-01-02")
		if p, ok := gotDays[day]; ok {
			t.Errorf("gap day %s produced a point (checks=%d up=%d) — a day with no probes must produce NO point, so the API can report it as null rather than 0%%",
				day, p.Checks, p.UpChecks)
		}
	}

	// Points are oldest-first, which the service relies on being able to scan
	// once.
	for i := 1; i < len(got[gapped]); i++ {
		if !got[gapped][i-1].Bucket.Before(got[gapped][i].Bucket) {
			t.Fatalf("series not ascending at index %d: %v then %v",
				i, got[gapped][i-1].Bucket, got[gapped][i].Bucket)
		}
	}

	// --- other: per-site keying ---------------------------------------
	if len(got[other]) != 1 {
		t.Fatalf("other site returned %d days, want 1", len(got[other]))
	}
	if k := got[other][0].Bucket.UTC().Format("2006-01-02"); k != otherDay.Format("2006-01-02") {
		t.Errorf("other site's point landed on %s, want %s", k, otherDay.Format("2006-01-02"))
	}
	// And that day must NOT have leaked into the gapped site's series.
	if _, leaked := gotDays[otherDay.Format("2006-01-02")]; leaked {
		t.Error("the other site's measured day appears in the gapped site's series — the batch is not keying points per site")
	}
}

// TestQueryFleetDailySeries_NeverProbedSiteIsAbsent: a site with no probes at
// all is absent from the map entirely, never present with a zero-filled
// slice. The service turns absence into 90 explicit nulls; a zero-filled
// slice here would instead become 90 days of measured 0% — the exact
// user-visible lie GH #460 is about, reintroduced one layer down.
func TestQueryFleetDailySeries_NeverProbedSiteIsAbsent(t *testing.T) {
	app, admin := startMetricsTestPostgres(t)
	ctx := context.Background()
	store := NewPostgres(app, nil)

	tenant := metricsSeedTenant(t, admin, "fleetseries-none-"+uuid.NewString()[:8])
	fresh := metricsSeedSite(t, admin, tenant, "https://"+uuid.NewString()+".example.com")

	got, err := store.QueryFleetDailySeries(ctx, tenant, []uuid.UUID{fresh}, 90*24*time.Hour)
	if err != nil {
		t.Fatalf("QueryFleetDailySeries: %v", err)
	}
	if pts, present := got[fresh]; present {
		t.Errorf("never-probed site is present with %d points, want absent from the map", len(pts))
	}
	if len(got) != 0 {
		t.Errorf("map has %d entries, want 0", len(got))
	}
}

// TestQueryFleetDailySeries_IsOneBatchedQueryNotAPerSiteScan is the
// performance-regression guard.
//
// m99 exists because the per-site window scan cost 6-8s cold; a fleet strip
// that looped a per-site query would pay that once per site, which is a
// performance regression wearing a feature's clothes. Two independent
// properties are asserted, because either alone can be satisfied by the wrong
// implementation:
//
//  1. EXPLAIN shows the unnest anchor and a LATERAL join — i.e. all sites are
//     served by ONE plan — and shows no Seq Scan on site_uptime_probes, so
//     the raw edge reads stay index-bounded exactly as
//     TestQueryFleetUptime_RawReadsAreIndexBounded proves for the fleet
//     aggregate.
//  2. Actual rows for a many-site batch come back in a single execution: the
//     plan is executed once (loops on the anchor, not repeated top-level
//     statements).
func TestQueryFleetDailySeries_IsOneBatchedQueryNotAPerSiteScan(t *testing.T) {
	app, admin := startMetricsTestPostgres(t)
	ctx := context.Background()
	store := NewPostgres(app, nil)

	const window = 30 * 24 * time.Hour
	_, today, _, _, _, _, _ := fleetUptimeParams(time.Now(), window)

	tenant := metricsSeedTenant(t, admin, "fleetseries-plan-"+uuid.NewString()[:8])
	var siteIDs []uuid.UUID
	var checks []Check
	for s := 0; s < 12; s++ {
		id := metricsSeedSite(t, admin, tenant, "https://"+uuid.NewString()+".example.com")
		siteIDs = append(siteIDs, id)
		for d := 25; d >= 1; d-- {
			day := today.AddDate(0, 0, -d)
			checks = append(checks, Check{
				TenantID: tenant, SiteID: id,
				CheckedAt: day.Add(3 * time.Hour), Up: true, TotalMs: 100,
			})
		}
	}
	seedWithRollup(t, store, checks)

	// Property 2 first: one call, every site served.
	got, err := store.QueryFleetDailySeries(ctx, tenant, siteIDs, window)
	if err != nil {
		t.Fatalf("QueryFleetDailySeries: %v", err)
	}
	if len(got) != len(siteIDs) {
		t.Fatalf("got %d sites in one call, want %d", len(got), len(siteIDs))
	}
	for _, id := range siteIDs {
		if len(got[id]) != 25 {
			t.Errorf("site %s returned %d days, want 25", id, len(got[id]))
		}
	}

	// Property 1: the plan. EXPLAIN runs on the admin pool for the same
	// reason TestQueryFleetUptime_RawReadsAreIndexBounded does — the plan
	// SHAPE is what is under test here, and the behavioural assertions above
	// already went through the app pool and its RLS policies.
	boundaryDay, todayP, tailLower, tailUpper, _, todayLower, nowP := fleetUptimeParams(time.Now(), window)
	explainSQL := "EXPLAIN (ANALYZE, FORMAT TEXT) " + fleetDailySeriesQuery
	rows, err := admin.Query(ctx, explainSQL,
		tenant, siteIDs, boundaryDay, todayP, tailLower, tailUpper, todayLower, nowP,
		boundaryDay, todayP)
	if err != nil {
		t.Fatalf("explain fleet daily series: %v", err)
	}
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			t.Fatalf("explain scan: %v", err)
		}
		lines = append(lines, line)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("EXPLAIN returned no plan lines — the guard would otherwise pass by finding nothing")
	}
	plan := strings.Join(lines, "\n")

	if !strings.Contains(plan, "Function Scan on unnest") {
		t.Errorf("plan has no unnest anchor — the sites are not being batched into one query:\n%s", plan)
	}
	if !strings.Contains(plan, "Nested Loop") {
		t.Errorf("plan has no lateral nested loop over the anchor:\n%s", plan)
	}
	if strings.Contains(plan, "Seq Scan on site_uptime_probes") {
		t.Errorf("plan sequentially scans site_uptime_probes — the raw edge reads must stay index-bounded (m85 covering index):\n%s", plan)
	}
}
