package metrics

// postgres_persite_pg_test.go: 0.61.125, real-Postgres proofs for the per-site
// window reads. White-box (same package) so the boundary math can be taken from
// the very fleetUptimeParams call the production code makes, rather than
// re-derived here where it could drift.
//
// What each test is for:
//
//	T1  TestQueryAggregate_RollupMatchesRawRollingWindow
//	    the load-bearing equivalence: counts and uptime % from the rollup
//	    decomposition are IDENTICAL to a Go-computed aggregate over the exact
//	    same rolling [now-window, now] range of raw probes.
//	T1b TestQueryAggregate_ServesCompleteMiddleDaysFromRollup
//	    the rollup is genuinely the source for the window's middle (raw rows
//	    for those days are deleted and the answer is unchanged).
//	T2  TestQuerySeries_ThirtyDayWindowReturnsDailyUTCPoints
//	T3  TestQuerySeries_SubDayWindowStillReadsRawMinuteBuckets
//	T4  TestPerSiteWindow_BoundaryDayCountedExactlyOnce
//	T5  TestQueryAggregate_RollupGapIsNotBackfilledFromRawProbes  (the C2 case)
//
// The container/seed helpers (startMetricsTestPostgres, metricsSeedTenant,
// metricsSeedSite, expectedFromChecks, absDiff) live in postgres_explain_test.go
// in this same package.

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
)

// oldStyleAvgLatency reproduces the PRE-0.61.125 per-site average latency
// exactly: AVG(NULLIF(total_ms, 0)) over every probe in the window, up or
// down. It exists so the one documented semantic change in this work is
// asserted rather than assumed - see the block comment above siteAggregateQuery.
func oldStyleAvgLatency(checks []Check, start, upTo time.Time) float64 {
	var sum float64
	var n int
	for _, c := range checks {
		if c.CheckedAt.Before(start) || c.CheckedAt.After(upTo) {
			continue
		}
		if c.TotalMs == 0 {
			continue // NULLIF(total_ms, 0) excludes it from sum AND divisor.
		}
		sum += c.TotalMs
		n++
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// utcDay is the UTC midnight the instant belongs to (the site_uptime_daily key).
func utcDay(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

// inBoundaryDay returns n distinct instants spread strictly inside
// [lower, upper), and boundaryDayBefore returns n strictly inside
// [dayStart, lower).
//
// WHY THESE EXIST, because getting this wrong cost a day of investigation.
//
// For a whole-day window, timeOfDay(tailLower) == timeOfDay(now), since
// tailLower is now minus a whole number of days. So a seed written as
// tailLower.Add(90 * time.Minute) leaves the boundary day entirely whenever
// the suite runs at or after 22:30 UTC, and a seed at tailLower.Add(-4 *
// time.Hour) leaves it in the other direction before 04:00 UTC.
//
// That made two tests here fail for a 90-minute band every day while passing
// in CI, which had simply never run inside the band. The production query was
// correct throughout: it reported the boundary day's in-window slice exactly,
// and put the crossed probe on the day it actually belongs to. The fixtures
// were asserting a day membership they had assumed rather than computed.
//
// Placing seeds proportionally inside the real interval makes the fixture
// independent of the wall clock, which is the only way a boundary test is
// worth anything as a regression guard.
func inBoundaryDay(lower, upper time.Time, n int) []time.Time {
	span := upper.Sub(lower)
	out := make([]time.Time, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, lower.Add(span*time.Duration(i)/time.Duration(n+1)))
	}
	return out
}

func boundaryDayBefore(dayStart, lower time.Time, n int) []time.Time {
	return inBoundaryDay(dayStart, lower, n)
}

// seedWithRollup writes checks through BOTH production write paths, exactly as
// a live sweep does: the raw probe insert first, then the best-effort rollup
// upsert.
func seedWithRollup(t *testing.T, store Store, checks []Check) {
	t.Helper()
	ctx := context.Background()
	if err := store.InsertChecks(ctx, checks); err != nil {
		t.Fatalf("insert checks: %v", err)
	}
	if err := store.(RollupWriter).UpsertRollup(ctx, checks); err != nil {
		t.Fatalf("upsert rollup: %v", err)
	}
}

// ---------------------------------------------------------------------------
// T1: equivalence
// ---------------------------------------------------------------------------

// TestQueryAggregate_RollupMatchesRawRollingWindow seeds a realistic mixed
// history (probes just outside and just inside the window start, a complete
// mid-window day, a zero-latency-while-up probe, DOWN probes carrying a real
// total_ms, and a second site's noise) through the real write paths, then
// asserts the rollup-backed aggregate is numerically identical to the exact
// rolling-window answer computed in Go from the very same Check values.
//
// The avg-latency assertion at the end is the one place this test also pins
// the DOCUMENTED semantic change: down probes with a real response time used
// to be folded into the per-site average and no longer are, which is what the
// fleet read has always done for the same site.
func TestQueryAggregate_RollupMatchesRawRollingWindow(t *testing.T) {
	app, admin := startMetricsTestPostgres(t)
	ctx := context.Background()
	store := NewPostgres(app, nil)

	for _, window := range []time.Duration{7 * 24 * time.Hour, 30 * 24 * time.Hour} {
		t.Run(window.String(), func(t *testing.T) {
			now := time.Now()
			boundaryDay, today, tailLower, _, _, _, nowUTC := fleetUptimeParams(now, window)

			tenant := metricsSeedTenant(t, admin, "persite-eq-"+uuid.NewString()[:8])
			siteID := metricsSeedSite(t, admin, tenant, "https://"+uuid.NewString()+".example.com")
			noiseSiteID := metricsSeedSite(t, admin, tenant, "https://"+uuid.NewString()+".example.com")

			var checks []Check
			add := func(at time.Time, up bool, totalMs float64) {
				checks = append(checks, Check{TenantID: tenant, SiteID: siteID, CheckedAt: at, Up: up, TotalMs: totalMs})
			}

			// Boundary day, OUT of window: must be excluded. A day-granular
			// rollup that swallowed the whole boundary day would count these.
			add(tailLower.Add(-90*time.Minute), true, 400)
			add(tailLower.Add(-2*time.Minute), false, 900)
			// Boundary day, IN window: served by the raw boundary tail.
			add(tailLower.Add(2*time.Minute), false, 850) // down WITH latency
			add(tailLower.Add(3*time.Hour), true, 150)

			// A complete mid-window day: served by the rollup.
			midDay := boundaryDay.Add(2 * 24 * time.Hour)
			if midDay.Before(today) {
				add(midDay.Add(4*time.Hour), false, 700) // down WITH latency
				add(midDay.Add(5*time.Hour), true, 0)    // up, zero latency: excluded from the average
				add(midDay.Add(6*time.Hour), true, 200)
			}

			// Today: read live from raw.
			add(nowUTC.Add(-30*time.Minute), false, 650)
			add(nowUTC.Add(-20*time.Minute), true, 0)
			add(nowUTC.Add(-10*time.Minute), true, 90)

			checks = append(checks, Check{TenantID: tenant, SiteID: noiseSiteID, CheckedAt: nowUTC, Up: false, TotalMs: 999})
			seedWithRollup(t, store, checks)

			siteChecks := make([]Check, 0, len(checks))
			for _, c := range checks {
				if c.SiteID == siteID {
					siteChecks = append(siteChecks, c)
				}
			}
			wantUp, wantTotal, wantSumLatency, wantSamples, _, _, hasAny :=
				expectedFromChecks(siteChecks, tailLower, nowUTC)
			if !hasAny {
				t.Fatal("fixture bug: no checks seeded")
			}

			agg, err := store.QueryAggregate(ctx, tenant, siteID, window)
			if err != nil {
				t.Fatalf("QueryAggregate: %v", err)
			}

			if agg.Checks != uint64(wantTotal) {
				t.Fatalf("Checks = %d, want %d (exact rolling-window count over raw probes)", agg.Checks, wantTotal)
			}
			if agg.UpChecks != uint64(wantUp) {
				t.Fatalf("UpChecks = %d, want %d", agg.UpChecks, wantUp)
			}
			wantPct := float64(wantUp) / float64(wantTotal) * 100
			if absDiff(agg.UptimePct, wantPct) > 1e-9 {
				t.Fatalf("UptimePct = %v, want %v EXACTLY (up=%d total=%d)", agg.UptimePct, wantPct, wantUp, wantTotal)
			}

			// Fixture cross-check against the database itself, independent of
			// both the production query and expectedFromChecks: this is what
			// pins that the two out-of-window boundary-day probes really are
			// excluded and the in-window one on the SAME day is included.
			var dbCount int64
			if err := admin.QueryRow(ctx,
				`SELECT count(*) FROM site_uptime_probes WHERE site_id=$1 AND probed_at>=$2 AND probed_at<=$3`,
				siteID, tailLower, nowUTC).Scan(&dbCount); err != nil {
				t.Fatalf("fixture cross-check: %v", err)
			}
			if dbCount != wantTotal {
				t.Fatalf("fixture cross-check: raw rows in [tailLower, now] = %d, want %d", dbCount, wantTotal)
			}

			wantAvg := wantSumLatency / float64(wantSamples)
			if absDiff(agg.AvgLatencyMs, wantAvg) > 1e-9 {
				t.Fatalf("AvgLatencyMs = %v, want %v (sum_latency_ms / latency_samples over SUCCESSFUL probes)", agg.AvgLatencyMs, wantAvg)
			}
			// The documented change: the old definition folded the DOWN probes'
			// real response times into the same average, and produced a
			// different number for this exact history.
			oldAvg := oldStyleAvgLatency(siteChecks, tailLower, nowUTC)
			if absDiff(oldAvg, wantAvg) < 1e-9 {
				t.Fatalf("fixture bug: the old and new latency definitions agree (%v), so this test cannot detect the documented semantic change; seed a DOWN probe with a non-zero total_ms", oldAvg)
			}
		})
	}
}

// TestQueryAggregate_ServesCompleteMiddleDaysFromRollup proves the middle of
// the window really is served by site_uptime_daily and not by a raw scan: the
// raw probe rows for every complete mid-window day are DELETED after the
// rollup has been written, and the aggregate must be unchanged. Against the
// pre-change implementation (a single raw scan across the whole window) the
// deleted days simply vanish from the answer.
func TestQueryAggregate_ServesCompleteMiddleDaysFromRollup(t *testing.T) {
	app, admin := startMetricsTestPostgres(t)
	ctx := context.Background()
	store := NewPostgres(app, nil)

	const window = 30 * 24 * time.Hour
	now := time.Now()
	boundaryDay, today, tailLower, _, _, _, nowUTC := fleetUptimeParams(now, window)

	tenant := metricsSeedTenant(t, admin, "persite-mid-"+uuid.NewString()[:8])
	siteID := metricsSeedSite(t, admin, tenant, "https://"+uuid.NewString()+".example.com")

	var checks []Check
	add := func(at time.Time, up bool, totalMs float64) {
		checks = append(checks, Check{TenantID: tenant, SiteID: siteID, CheckedAt: at, Up: up, TotalMs: totalMs})
	}
	add(tailLower.Add(30*time.Minute), true, 100)
	for d := boundaryDay.Add(24 * time.Hour); d.Before(today); d = d.Add(24 * time.Hour) {
		add(d.Add(6*time.Hour), true, 120)
		add(d.Add(18*time.Hour), false, 800)
	}
	add(nowUTC.Add(-5*time.Minute), true, 110)
	seedWithRollup(t, store, checks)

	before, err := store.QueryAggregate(ctx, tenant, siteID, window)
	if err != nil {
		t.Fatalf("QueryAggregate (before delete): %v", err)
	}
	if before.Checks < 30 {
		t.Fatalf("fixture bug: only %d checks in the window", before.Checks)
	}

	// Drop every raw probe belonging to a COMPLETE mid-window day. The rollup
	// rows for those days stay.
	tag, err := admin.Exec(ctx,
		`DELETE FROM site_uptime_probes
		 WHERE site_id = $1 AND probed_at >= $2 AND probed_at < $3`,
		siteID, boundaryDay.Add(24*time.Hour), today)
	if err != nil {
		t.Fatalf("delete mid-window raw probes: %v", err)
	}
	if tag.RowsAffected() == 0 {
		t.Fatal("fixture bug: no mid-window raw probes were deleted")
	}

	after, err := store.QueryAggregate(ctx, tenant, siteID, window)
	if err != nil {
		t.Fatalf("QueryAggregate (after delete): %v", err)
	}
	if after.Checks != before.Checks || after.UpChecks != before.UpChecks {
		t.Fatalf("aggregate changed when only the mid-window RAW rows were removed: before %d/%d, after %d/%d. The window's middle is not being served from site_uptime_daily",
			before.UpChecks, before.Checks, after.UpChecks, after.Checks)
	}
	if absDiff(after.UptimePct, before.UptimePct) > 1e-9 {
		t.Fatalf("UptimePct changed after deleting only mid-window raw rows: %v then %v", before.UptimePct, after.UptimePct)
	}
}

// ---------------------------------------------------------------------------
// T2 / T3: series granularity
// ---------------------------------------------------------------------------

// TestQuerySeries_ThirtyDayWindowReturnsDailyUTCPoints asserts the 30-day
// series is now one point per UTC day: every bucket is exactly a UTC midnight,
// buckets are strictly increasing whole days apart, and the point count equals
// the number of distinct UTC days that actually have in-window probes (never
// the ~100 fixed-width buckets the old implementation produced). It also
// checks the per-day counts against a Go-computed grouping of the same seeds.
func TestQuerySeries_ThirtyDayWindowReturnsDailyUTCPoints(t *testing.T) {
	app, admin := startMetricsTestPostgres(t)
	ctx := context.Background()
	store := NewPostgres(app, nil)

	const window = 30 * 24 * time.Hour
	now := time.Now()
	boundaryDay, today, tailLower, tailUpper, _, _, nowUTC := fleetUptimeParams(now, window)

	tenant := metricsSeedTenant(t, admin, "persite-series-"+uuid.NewString()[:8])
	siteID := metricsSeedSite(t, admin, tenant, "https://"+uuid.NewString()+".example.com")

	var checks []Check
	add := func(at time.Time, up bool, totalMs float64) {
		checks = append(checks, Check{TenantID: tenant, SiteID: siteID, CheckedAt: at, Up: up, TotalMs: totalMs})
	}
	// Boundary day: one probe before the window start (must not appear in the
	// boundary point) and two inside it.
	//
	// Positioned PROPORTIONALLY inside the real intervals, never at a fixed
	// offset from tailLower: see inBoundaryDay for why a fixed 90 minutes
	// silently leaves the boundary day for part of every day.
	before1 := boundaryDayBefore(boundaryDay, tailLower, 1)
	inWin1 := inBoundaryDay(tailLower, tailUpper, 2)
	add(before1[0], true, 500)
	add(inWin1[0], true, 100)
	add(inWin1[1], false, 700)
	// Every complete day in between.
	for d := boundaryDay.Add(24 * time.Hour); d.Before(today); d = d.Add(24 * time.Hour) {
		add(d.Add(3*time.Hour), true, 120)
		add(d.Add(15*time.Hour), true, 160)
	}
	// Today, anchored to `now` so the test cannot fall off the day boundary
	// when it happens to run just after UTC midnight.
	add(nowUTC.Add(-2*time.Second), true, 140)
	add(nowUTC.Add(-time.Second), false, 640)
	seedWithRollup(t, store, checks)

	// buckets=100 is what the HTTP handler passes; the daily path ignores it,
	// which is exactly what the point count below proves.
	points, err := store.QuerySeries(ctx, tenant, siteID, window, 100)
	if err != nil {
		t.Fatalf("QuerySeries: %v", err)
	}
	if len(points) == 0 {
		t.Fatal("QuerySeries returned no points")
	}

	// Independent Go grouping of the in-window seeds by UTC day.
	type dayAgg struct{ checks, upChecks int }
	wantByDay := map[time.Time]*dayAgg{}
	for _, c := range checks {
		if c.CheckedAt.Before(tailLower) || c.CheckedAt.After(nowUTC) {
			continue
		}
		d := utcDay(c.CheckedAt)
		if wantByDay[d] == nil {
			wantByDay[d] = &dayAgg{}
		}
		wantByDay[d].checks++
		if c.Up {
			wantByDay[d].upChecks++
		}
	}
	if len(points) != len(wantByDay) {
		t.Fatalf("QuerySeries returned %d points, want %d (one per UTC day with in-window probes). ~100 fixed-width buckets means the daily path did not run", len(points), len(wantByDay))
	}
	// Sanity on the absolute size, so a future change that silently reverts to
	// fixed-width bucketing is caught even if the day-count assertion above
	// were weakened: a 30-day window can only ever span 30 or 31 UTC days.
	if len(points) < 29 || len(points) > 31 {
		t.Fatalf("30-day window produced %d points, want 30 or 31 daily points", len(points))
	}

	var prev time.Time
	for i, p := range points {
		b := p.Bucket.UTC()
		if !b.Equal(b.Truncate(24 * time.Hour)) {
			t.Fatalf("point %d bucket %v is not a UTC midnight: buckets must align to the rollup's day grain", i, b)
		}
		if i > 0 {
			if !b.After(prev) {
				t.Fatalf("points are not strictly increasing: %v then %v", prev, b)
			}
			if gap := b.Sub(prev); gap%(24*time.Hour) != 0 {
				t.Fatalf("gap between point %d and %d is %v, not a whole number of days", i-1, i, gap)
			}
		}
		prev = b

		w, ok := wantByDay[b]
		if !ok {
			t.Fatalf("point %d has bucket %v, which has no in-window probes at all", i, b)
		}
		if p.Checks != uint64(w.checks) || p.UpChecks != uint64(w.upChecks) {
			t.Fatalf("bucket %v: got %d/%d up/checks, want %d/%d", b, p.UpChecks, p.Checks, w.upChecks, w.checks)
		}
	}

	// The boundary day's point must cover ONLY the in-window slice: the probe
	// seeded an hour before the window start must not be in it.
	first := points[0]
	if !first.Bucket.UTC().Equal(boundaryDay) {
		t.Fatalf("first point bucket = %v, want the boundary day %v", first.Bucket.UTC(), boundaryDay)
	}
	if first.Checks != 2 {
		t.Fatalf("boundary day point has %d checks, want 2 (the out-of-window probe on the same day must be excluded)", first.Checks)
	}
}

// TestQuerySeries_SubDayWindowStillReadsRawMinuteBuckets is the no-regression
// guard for C4: below the threshold nothing changes. A 1h window must still
// come back as per-minute raw buckets, not a single daily point.
func TestQuerySeries_SubDayWindowStillReadsRawMinuteBuckets(t *testing.T) {
	app, admin := startMetricsTestPostgres(t)
	ctx := context.Background()
	store := NewPostgres(app, nil)

	now := time.Now().UTC()
	tenant := metricsSeedTenant(t, admin, "persite-subday-"+uuid.NewString()[:8])
	siteID := metricsSeedSite(t, admin, tenant, "https://"+uuid.NewString()+".example.com")

	// Ten probes, each in its own minute, comfortably inside a 1h window.
	var checks []Check
	minutes := map[time.Time]bool{}
	for i := 1; i <= 10; i++ {
		at := now.Add(-time.Duration(i*3) * time.Minute)
		checks = append(checks, Check{TenantID: tenant, SiteID: siteID, CheckedAt: at, Up: i%4 != 0, TotalMs: float64(100 + i)})
		minutes[at.Truncate(time.Minute)] = true
	}
	seedWithRollup(t, store, checks)

	points, err := store.QuerySeries(ctx, tenant, siteID, time.Hour, 100)
	if err != nil {
		t.Fatalf("QuerySeries: %v", err)
	}
	if len(points) != len(minutes) {
		t.Fatalf("1h window returned %d points, want %d (one per distinct minute): the sub-day path must keep reading raw probes at minute granularity", len(points), len(minutes))
	}
	midnights := 0
	var total uint64
	for _, p := range points {
		b := p.Bucket.UTC()
		if !b.Equal(b.Truncate(time.Minute)) {
			t.Fatalf("bucket %v is not minute-aligned", b)
		}
		if b.Equal(b.Truncate(24 * time.Hour)) {
			midnights++
		}
		total += p.Checks
	}
	if midnights == len(points) {
		t.Fatal("every 1h-window bucket is a UTC midnight: the daily path ran for a sub-day window")
	}
	if total != uint64(len(checks)) {
		t.Fatalf("sub-day series covers %d checks, want %d", total, len(checks))
	}
}

// ---------------------------------------------------------------------------
// T4: the boundary partial day
// ---------------------------------------------------------------------------

// TestPerSiteWindow_BoundaryDayCountedExactlyOnce is the double-count /
// dropped-day guard. The oldest day in the window is seeded with probes on
// BOTH sides of the window start and the rollup row for that day therefore
// holds the WHOLE day's counters, including the out-of-window part. Both the
// aggregate and the series must report only the in-window slice: counting the
// boundary day's rollup row as well would inflate it, and excluding the raw
// tail would drop it entirely.
func TestPerSiteWindow_BoundaryDayCountedExactlyOnce(t *testing.T) {
	app, admin := startMetricsTestPostgres(t)
	ctx := context.Background()
	store := NewPostgres(app, nil)

	const window = 7 * 24 * time.Hour
	now := time.Now()
	boundaryDay, today, tailLower, tailUpper, _, _, nowUTC := fleetUptimeParams(now, window)

	tenant := metricsSeedTenant(t, admin, "persite-boundary-"+uuid.NewString()[:8])
	siteID := metricsSeedSite(t, admin, tenant, "https://"+uuid.NewString()+".example.com")

	var checks []Check
	add := func(at time.Time, up bool, totalMs float64) {
		checks = append(checks, Check{TenantID: tenant, SiteID: siteID, CheckedAt: at, Up: up, TotalMs: totalMs})
	}
	// Boundary day, BEFORE the window start: 3 probes, all up. Present in that
	// day's rollup row, and must never reach the answer.
	// Positioned PROPORTIONALLY inside the real intervals, never at fixed
	// offsets from tailLower: see inBoundaryDay. A fixed -4h leaves the
	// boundary day before 04:00 UTC and a fixed +2h leaves it after 22:00,
	// either of which breaks the "whole day is 5 probes" premise below while
	// the query under test is behaving correctly.
	pre := boundaryDayBefore(boundaryDay, tailLower, 3)
	post := inBoundaryDay(tailLower, tailUpper, 2)
	add(pre[0], true, 300)
	add(pre[1], true, 310)
	add(pre[2], true, 320)
	// Boundary day, AFTER the window start: 2 probes, one down.
	add(post[0], false, 800)
	add(post[1], true, 130)
	// One complete mid-window day and today, so the other two parts are
	// non-empty and a mistake in the boundary part cannot hide.
	midDay := boundaryDay.Add(2 * 24 * time.Hour)
	if !midDay.Before(today) {
		t.Fatalf("fixture bug: midDay %v is not inside the window (today=%v)", midDay, today)
	}
	add(midDay.Add(8*time.Hour), true, 140)
	add(nowUTC.Add(-time.Minute), true, 150)
	seedWithRollup(t, store, checks)

	// The boundary day's rollup row really does hold the whole day (5 probes),
	// which is what makes this test meaningful.
	var dailyTotal int64
	if err := admin.QueryRow(ctx,
		`SELECT total_checks FROM site_uptime_daily WHERE site_id = $1 AND day = $2::date`,
		siteID, boundaryDay).Scan(&dailyTotal); err != nil {
		t.Fatalf("read boundary day rollup row: %v", err)
	}
	if dailyTotal != 5 {
		t.Fatalf("fixture bug: boundary day rollup total_checks = %d, want 5", dailyTotal)
	}

	// Aggregate: 2 (in-window boundary) + 1 (mid day) + 1 (today) = 4.
	agg, err := store.QueryAggregate(ctx, tenant, siteID, window)
	if err != nil {
		t.Fatalf("QueryAggregate: %v", err)
	}
	if agg.Checks != 4 {
		t.Fatalf("Checks = %d, want 4. 7 would mean the boundary day's rollup row was added on top of the raw tail (double counted); 2 would mean the raw tail was dropped", agg.Checks)
	}
	if agg.UpChecks != 3 {
		t.Fatalf("UpChecks = %d, want 3", agg.UpChecks)
	}

	// Series: exactly one point for the boundary day, carrying the in-window
	// slice only.
	points, err := store.QuerySeries(ctx, tenant, siteID, window, 100)
	if err != nil {
		t.Fatalf("QuerySeries: %v", err)
	}
	var boundaryPoints []Point
	var seriesTotal uint64
	for _, p := range points {
		seriesTotal += p.Checks
		if p.Bucket.UTC().Equal(boundaryDay) {
			boundaryPoints = append(boundaryPoints, p)
		}
	}
	if len(boundaryPoints) != 1 {
		t.Fatalf("series has %d points for the boundary day %v, want exactly 1", len(boundaryPoints), boundaryDay)
	}
	if boundaryPoints[0].Checks != 2 || boundaryPoints[0].UpChecks != 1 {
		t.Fatalf("boundary day point = %d up / %d checks, want 1/2 (the in-window slice only, not the whole day's 5)", boundaryPoints[0].UpChecks, boundaryPoints[0].Checks)
	}
	if seriesTotal != agg.Checks {
		t.Fatalf("series covers %d checks but the aggregate reports %d: the two per-site reads disagree about the same window", seriesTotal, agg.Checks)
	}
}

// ---------------------------------------------------------------------------
// T5: the C2 case, probes with no rollup row
// ---------------------------------------------------------------------------

// TestQueryAggregate_RollupGapIsNotBackfilledFromRawProbes asserts the
// DELIBERATE behaviour chosen for C2, rather than leaving it undiscovered.
//
// The rollup upsert is best effort: uptime.ProbeWorker.Sweep logs and
// continues when it fails, so a complete mid-window day can hold raw probes
// with no matching site_uptime_daily row. For such a day the per-site read now
// reports the rollup's answer (nothing) rather than rescanning raw probes, and
// this test pins exactly that, in both directions:
//
//   - the missing day's checks are NOT counted, and
//   - the number the per-site card shows is then the SAME number the fleet
//     read (QueryFleetUptime, unchanged since m99) shows for the same site and
//     window - which is the real reason a raw-probe fallback is not wanted
//     here. A fallback would also be indistinguishable from a site that
//     genuinely had no probes those days (paused, newly added), and would put
//     the full-window scan back for exactly the sites it was removed from.
func TestQueryAggregate_RollupGapIsNotBackfilledFromRawProbes(t *testing.T) {
	app, admin := startMetricsTestPostgres(t)
	ctx := context.Background()
	store := NewPostgres(app, nil)

	const window = 30 * 24 * time.Hour
	now := time.Now()
	boundaryDay, today, tailLower, _, _, _, nowUTC := fleetUptimeParams(now, window)

	tenant := metricsSeedTenant(t, admin, "persite-gap-"+uuid.NewString()[:8])
	siteID := metricsSeedSite(t, admin, tenant, "https://"+uuid.NewString()+".example.com")

	dayA := boundaryDay.Add(2 * 24 * time.Hour) // rolled up normally
	dayB := boundaryDay.Add(4 * 24 * time.Hour) // the gap: raw probes only
	for _, d := range []time.Time{dayA, dayB} {
		if !d.Before(today) {
			t.Fatalf("fixture bug: %v is not a complete mid-window day (today=%v)", d, today)
		}
	}

	// Everything except dayB goes through both write paths.
	rolled := []Check{
		{TenantID: tenant, SiteID: siteID, CheckedAt: tailLower.Add(time.Minute), Up: true, TotalMs: 100},
		{TenantID: tenant, SiteID: siteID, CheckedAt: dayA.Add(6 * time.Hour), Up: true, TotalMs: 110},
		{TenantID: tenant, SiteID: siteID, CheckedAt: dayA.Add(18 * time.Hour), Up: true, TotalMs: 120},
		{TenantID: tenant, SiteID: siteID, CheckedAt: nowUTC.Add(-time.Minute), Up: true, TotalMs: 130},
	}
	seedWithRollup(t, store, rolled)

	// dayB: the raw insert succeeded and the rollup upsert did not. Four down
	// probes, so a fallback that silently rescanned raw rows would be obvious
	// in the uptime percentage, not just the count.
	var gapped []Check
	for i := 0; i < 4; i++ {
		gapped = append(gapped, Check{
			TenantID: tenant, SiteID: siteID,
			CheckedAt: dayB.Add(time.Duration(i+1) * 3 * time.Hour),
			Up:        false, TotalMs: 900,
		})
	}
	if err := store.InsertChecks(ctx, gapped); err != nil {
		t.Fatalf("insert gapped checks: %v", err)
	}
	var gapRollupRows int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM site_uptime_daily WHERE site_id = $1 AND day = $2::date`,
		siteID, dayB).Scan(&gapRollupRows); err != nil {
		t.Fatalf("count dayB rollup rows: %v", err)
	}
	if gapRollupRows != 0 {
		t.Fatalf("fixture bug: dayB has %d rollup rows, want 0", gapRollupRows)
	}

	agg, err := store.QueryAggregate(ctx, tenant, siteID, window)
	if err != nil {
		t.Fatalf("QueryAggregate: %v", err)
	}
	// 4 rolled-up checks, all up. dayB's 4 down probes are invisible.
	if agg.Checks != 4 || agg.UpChecks != 4 {
		t.Fatalf("Checks/UpChecks = %d/%d, want 4/4. 8/4 would mean the read fell back to raw probes for a day the rollup does not cover, which is not the documented behaviour",
			agg.Checks, agg.UpChecks)
	}
	if absDiff(agg.UptimePct, 100) > 1e-9 {
		t.Fatalf("UptimePct = %v, want 100", agg.UptimePct)
	}

	// And the fleet read, which has behaved this way since m99, agrees.
	fleet, err := store.QueryFleetUptime(ctx, tenant, []uuid.UUID{siteID}, window)
	if err != nil {
		t.Fatalf("QueryFleetUptime: %v", err)
	}
	row, ok := fleet[siteID]
	if !ok || row.UptimePct7d == nil {
		t.Fatalf("fleet read returned no uptime for the site: %+v ok=%v", row, ok)
	}
	if absDiff(*row.UptimePct7d, agg.UptimePct) > 1e-9 {
		t.Fatalf("per-site UptimePct %v disagrees with the fleet read's %v for the same site and window: the two cards on the same screen would show different numbers",
			agg.UptimePct, *row.UptimePct7d)
	}

	// The series shows the same gap, in the same place: a point for the
	// boundary day, dayA and today, and none for dayB.
	points, err := store.QuerySeries(ctx, tenant, siteID, window, 100)
	if err != nil {
		t.Fatalf("QuerySeries: %v", err)
	}
	got := make([]time.Time, 0, len(points))
	for _, p := range points {
		got = append(got, p.Bucket.UTC())
	}
	sort.Slice(got, func(i, j int) bool { return got[i].Before(got[j]) })
	want := []time.Time{boundaryDay, dayA, today}
	if len(got) != len(want) {
		t.Fatalf("series buckets = %v, want %v (dayB has no rollup row, so it has no point)", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("series buckets = %v, want %v", got, want)
		}
	}
}
