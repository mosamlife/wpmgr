package metrics

// postgres_explain_test.go — m99 follow-up (adversarial review): the product
// decision is EXACT uptime numbers, not a day-granular approximation. This
// file proves two things against a real Postgres (white-box, same package,
// so it can call the unexported fleetUptimeParams directly for deterministic
// boundary math regardless of wall-clock time at test-run):
//
//  1. TestQueryFleetUptime_HybridExactlyMatchesRawRollingWindow — the hybrid
//     decomposition (rollup middle + two bounded raw edge reads) is
//     numerically IDENTICAL to a direct Go-computed aggregate over the exact
//     same probed_at >= now-window rolling range, for an outage placed
//     exactly straddling the oldest boundary day's cutoff instant (the case
//     a day-granular rollup gets wrong), an outage today, an outage in a
//     complete mid-window day, multi-site noise, and the zero-latency-while-
//     up exclusion — for both the 7d and 30d windows the codebase actually
//     uses.
//  2. TestQueryFleetUptime_RawReadsAreIndexBounded — EXPLAIN proof that
//     neither raw edge read scans more than its bounded ~1-day slice, even
//     with tens of thousands of probe rows spanning the full window.
//
// The bootstrap helper below is a package-local copy of the pattern used
// throughout apps/api/tests (see tests/rls_integration_test.go's
// startPostgres / internal/org/purge_worker_test.go's startOrgTestPostgres)
// — kept local, per that file's own stated convention, so this package has
// no test-only dependency on the external tests package.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

func startMetricsTestPostgres(t *testing.T) (app *db.Pool, admin *db.Pool) {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("wpmgr"),
		tcpostgres.WithUsername("wpmgr"),
		tcpostgres.WithPassword("wpmgr"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Skipf("skipping: cannot start postgres container (docker unavailable?): %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	adminDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	adminPool, err := db.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if err := adminPool.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, stmt := range []string{
		"ALTER ROLE wpmgr_app LOGIN PASSWORD 'app'",
		"GRANT USAGE ON SCHEMA public TO wpmgr_app",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO wpmgr_app",
		"REVOKE UPDATE, DELETE, TRUNCATE ON audit_log FROM wpmgr_app",
	} {
		if _, err := adminPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("provision app role (%q): %v", stmt, err)
		}
	}
	t.Cleanup(adminPool.Close)

	appDSN := strings.Replace(adminDSN, "wpmgr:wpmgr@", "wpmgr_app:app@", 1)
	appPool, err := db.Connect(ctx, appDSN)
	if err != nil {
		t.Fatalf("connect app: %v", err)
	}
	t.Cleanup(appPool.Close)
	return appPool, adminPool
}

func metricsSeedTenant(t *testing.T, admin *db.Pool, slug string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := admin.QueryRow(context.Background(),
		"INSERT INTO tenants (name, slug) VALUES ($1, $2) RETURNING id", slug, slug).Scan(&id); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

func metricsSeedSite(t *testing.T, admin *db.Pool, tenant uuid.UUID, url string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := admin.QueryRow(context.Background(),
		`INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, 'seed') RETURNING id`,
		tenant, url).Scan(&id); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	return id
}

// expectedFromChecks reproduces the OLD, exact rolling-window aggregate
// (probed_at >= start, unbounded above — no future rows exist in practice, so
// bounding at `upTo` here is equivalent) directly from the Check values the
// test seeded, entirely in Go — the most direct possible ground truth, with
// no SQL text to drift out of sync with what "old" actually meant.
func expectedFromChecks(checks []Check, start, upTo time.Time) (up, total int64, sumLatency float64, samples int64, latestUp bool, latestAt time.Time, hasAny bool) {
	for _, c := range checks {
		if !hasAny || c.CheckedAt.After(latestAt) {
			latestUp, latestAt, hasAny = c.Up, c.CheckedAt, true
		}
		if c.CheckedAt.Before(start) || c.CheckedAt.After(upTo) {
			continue
		}
		total++
		if c.Up {
			up++
			if c.TotalMs != 0 {
				sumLatency += c.TotalMs
				samples++
			}
		}
	}
	return
}

// TestQueryFleetUptime_HybridExactlyMatchesRawRollingWindow is the load-
// bearing exactness test the m99 adversarial review requires: the hybrid
// decomposition must be numerically IDENTICAL (not merely close) to the
// exact old rolling-window aggregate, including an outage placed exactly
// straddling the oldest boundary day's cutoff instant.
func TestQueryFleetUptime_HybridExactlyMatchesRawRollingWindow(t *testing.T) {
	app, admin := startMetricsTestPostgres(t)
	ctx := context.Background()
	store := NewPostgres(app, nil)
	rw := store.(RollupWriter)

	for _, window := range []time.Duration{7 * 24 * time.Hour, 30 * 24 * time.Hour} {
		t.Run(fmt.Sprintf("window=%s", window), func(t *testing.T) {
			now := time.Now()
			boundaryDay, today, tailLower, _, _, _, nowUTC := fleetUptimeParams(now, window)

			tenant := metricsSeedTenant(t, admin, "m99-exact-"+uuid.NewString()[:8])
			siteID := metricsSeedSite(t, admin, tenant, "https://"+uuid.NewString()+".example.com")
			noiseSiteID := metricsSeedSite(t, admin, tenant, "https://"+uuid.NewString()+".example.com")

			var checks []Check
			add := func(at time.Time, up bool, totalMs float64) {
				checks = append(checks, Check{TenantID: tenant, SiteID: siteID, CheckedAt: at, Up: up, TotalMs: totalMs})
			}

			// 1. JUST BEFORE the exact window start (tailLower), same UTC
			//    boundary day: a down probe that MUST be excluded. This is
			//    the case a day-granular rollup (bucket the whole boundary
			//    day in-or-out) would get wrong.
			add(tailLower.Add(-2*time.Minute), false, 0)
			// 2. JUST AFTER tailLower, same boundary day: a down probe that
			//    MUST be included — proves the raw boundary-tail read (not
			//    the rollup) is what serves this partial day.
			add(tailLower.Add(2*time.Minute), false, 0)
			// 3. A healthy probe later the same boundary day.
			add(tailLower.Add(3*time.Hour), true, 150)

			// 4. A complete mid-window day outage (only when the window has
			//    room for one — the 7d window barely does; boundaryDay+2 is
			//    always < today for both 7d and 30d in this test).
			midDay := boundaryDay.Add(2 * 24 * time.Hour)
			if midDay.Before(today) {
				add(midDay.Add(4*time.Hour), false, 0)
				add(midDay.Add(5*time.Hour), false, 0)
				add(midDay.Add(6*time.Hour), true, 200)
			}

			// 5. Today: an outage, a zero-latency-while-up probe (must be
			//    excluded from the latency average, matching
			//    NULLIF(total_ms,0)), and a healthy probe — read live from
			//    RAW today, not the rollup.
			add(nowUTC.Add(-30*time.Minute), false, 0)
			add(nowUTC.Add(-20*time.Minute), true, 0)
			add(nowUTC.Add(-10*time.Minute), true, 90)

			// Noise for a second site: must not leak into siteID's result.
			checks = append(checks, Check{TenantID: tenant, SiteID: noiseSiteID, CheckedAt: nowUTC, Up: false, TotalMs: 999})

			if err := store.InsertChecks(ctx, checks); err != nil {
				t.Fatalf("insert checks: %v", err)
			}
			if err := rw.UpsertRollup(ctx, checks); err != nil {
				t.Fatalf("upsert rollup: %v", err)
			}

			siteChecks := make([]Check, 0, len(checks))
			for _, c := range checks {
				if c.SiteID == siteID {
					siteChecks = append(siteChecks, c)
				}
			}
			wantUp, wantTotal, wantSumLatency, wantSamples, wantLatestUp, wantLatestAt, hasAny :=
				expectedFromChecks(siteChecks, tailLower, nowUTC)
			if !hasAny {
				t.Fatal("test fixture bug: no checks seeded for siteID")
			}

			got, err := store.QueryFleetUptime(ctx, tenant, []uuid.UUID{siteID, noiseSiteID}, window)
			if err != nil {
				t.Fatalf("QueryFleetUptime: %v", err)
			}
			row, ok := got[siteID]
			if !ok {
				t.Fatal("siteID missing from result")
			}

			if row.Up == nil || *row.Up != wantLatestUp {
				t.Fatalf("Up = %v, want %v", row.Up, wantLatestUp)
			}
			// Postgres timestamptz has microsecond precision, so the round-tripped
			// LastProbeAt is truncated from the seeded nanosecond time.Time; compare
			// at microsecond granularity (the production value is microsecond-exact).
			if row.LastProbeAt == nil ||
				!row.LastProbeAt.Truncate(time.Microsecond).Equal(wantLatestAt.Truncate(time.Microsecond)) {
				t.Fatalf("LastProbeAt = %v, want %v", row.LastProbeAt, wantLatestAt)
			}

			var gotPct float64
			if row.UptimePct7d != nil {
				gotPct = *row.UptimePct7d
			}
			wantPct := float64(0)
			if wantTotal > 0 {
				wantPct = float64(wantUp) / float64(wantTotal) * 100
			}
			if (row.UptimePct7d == nil) != (wantTotal == 0) {
				t.Fatalf("UptimePct7d nil-ness mismatch: got=%v wantTotal=%d", row.UptimePct7d, wantTotal)
			}
			if row.UptimePct7d != nil && absDiff(gotPct, wantPct) > 1e-9 {
				t.Fatalf("UptimePct7d = %v, want %v EXACTLY (up=%d total=%d)", gotPct, wantPct, wantUp, wantTotal)
			}

			wantAvg := float64(0)
			if wantSamples > 0 {
				wantAvg = wantSumLatency / float64(wantSamples)
			}
			if (row.AvgLatencyMs == nil) != (wantSamples == 0) {
				t.Fatalf("AvgLatencyMs nil-ness mismatch: got=%v wantSamples=%d", row.AvgLatencyMs, wantSamples)
			}
			if row.AvgLatencyMs != nil && absDiff(*row.AvgLatencyMs, wantAvg) > 1e-9 {
				t.Fatalf("AvgLatencyMs = %v, want %v EXACTLY", *row.AvgLatencyMs, wantAvg)
			}

			// Fixture cross-check, independent of both the production code
			// path and expectedFromChecks: the raw row count actually in the
			// DB within [tailLower, nowUTC] must equal wantTotal. This
			// specifically locks in that the pre-window down probe
			// (tailLower - 2min) is excluded and the post-tailLower one
			// (tailLower + 2min, same UTC boundary day) is included — the
			// exact boundary-day case a day-granular rollup gets wrong.
			var dbCount int64
			if err := admin.QueryRow(ctx,
				`SELECT count(*) FROM site_uptime_probes WHERE site_id=$1 AND probed_at>=$2 AND probed_at<=$3`,
				siteID, tailLower, nowUTC).Scan(&dbCount); err != nil {
				t.Fatalf("fixture cross-check count: %v", err)
			}
			if dbCount != wantTotal {
				t.Fatalf("fixture cross-check: raw DB count in [tailLower,nowUTC] = %d, want %d (matches expectedFromChecks) — boundary exclusion bug in the TEST fixture, not necessarily production code", dbCount, wantTotal)
			}

			noiseRow, ok := got[noiseSiteID]
			if !ok || noiseRow.Up == nil || *noiseRow.Up {
				t.Fatalf("noise site result wrong: %+v ok=%v", noiseRow, ok)
			}
		})
	}
}

func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

// TestQueryFleetUptime_RawReadsAreIndexBounded proves, via EXPLAIN ANALYZE
// against a realistically large site_uptime_probes table (well beyond the
// ~2 edge days), that the two raw sub-selects inside fleetUptimeQuery are
// served by an index scan on the m85 covering index and touch only their
// bounded ~1-day slice — never the full window's worth of rows.
func TestQueryFleetUptime_RawReadsAreIndexBounded(t *testing.T) {
	app, admin := startMetricsTestPostgres(t)
	ctx := context.Background()
	store := NewPostgres(app, nil)
	rw := store.(RollupWriter)

	tenant := metricsSeedTenant(t, admin, "m99-explain-"+uuid.NewString()[:8])
	siteID := metricsSeedSite(t, admin, tenant, "https://"+uuid.NewString()+".example.com")
	// Several noise sites with their OWN dense history over the SAME time
	// range. Without these, site_id has zero selectivity in a single-site
	// table and the planner reasonably prefers the narrower probed_at-only
	// index (site_uptime_probes_probed_at_idx) over the wider covering index
	// — a fine choice for a one-site table, but not representative of a real
	// multi-site fleet, which is exactly what the m85 covering index
	// (site_id, tenant_id, probed_at DESC) targets. Seeding noise sites
	// makes site_id genuinely selective, matching production.
	const noiseSiteCount = 5
	noiseSiteIDs := make([]uuid.UUID, noiseSiteCount)
	for i := range noiseSiteIDs {
		noiseSiteIDs[i] = metricsSeedSite(t, admin, tenant, "https://"+uuid.NewString()+".example.com")
	}

	window := 30 * 24 * time.Hour
	now := time.Now()
	_, _, tailLower, tailUpper, _, todayLower, nowUTC := fleetUptimeParams(now, window)

	// Seed a REALISTIC volume: dense probes for the full 30-day window (plus
	// history before it) for siteID AND every noise site — the exact
	// scenario m99 fixes (many sites, each with tens of thousands of probe
	// rows).
	const probesPerDay = 200 // dense enough for the planner to prefer the index; still far fewer than 1,440/day for test speed.
	seedDense := func(id uuid.UUID) []Check {
		var out []Check
		cursor := tailLower.Add(-25 * 24 * time.Hour) // start well before the window too, so a full-scan alternative would visibly cost more.
		for cursor.Before(nowUTC) {
			out = append(out, Check{TenantID: tenant, SiteID: id, CheckedAt: cursor, Up: true, TotalMs: 50})
			cursor = cursor.Add(24 * time.Hour / probesPerDay)
		}
		return out
	}
	all := seedDense(siteID)
	if err := store.InsertChecks(ctx, all); err != nil {
		t.Fatalf("insert checks: %v", err)
	}
	if err := rw.UpsertRollup(ctx, all); err != nil {
		t.Fatalf("upsert rollup: %v", err)
	}
	for _, id := range noiseSiteIDs {
		noiseChecks := seedDense(id)
		if err := store.InsertChecks(ctx, noiseChecks); err != nil {
			t.Fatalf("insert noise checks: %v", err)
		}
	}
	if _, err := admin.Exec(ctx, `ANALYZE site_uptime_probes`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	// Count how many raw rows actually fall in each bounded edge range —
	// this is the "should be scanned" ceiling the EXPLAIN actual-rows count
	// must stay near, and is always << the full window's row count.
	var tailRows, todayRows, totalRows int64
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM site_uptime_probes WHERE site_id=$1 AND probed_at>=$2 AND probed_at<$3`, siteID, tailLower, tailUpper).Scan(&tailRows); err != nil {
		t.Fatalf("count tail rows: %v", err)
	}
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM site_uptime_probes WHERE site_id=$1 AND probed_at>=$2 AND probed_at<=$3`, siteID, todayLower, nowUTC).Scan(&todayRows); err != nil {
		t.Fatalf("count today rows: %v", err)
	}
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM site_uptime_probes WHERE site_id=$1`, siteID).Scan(&totalRows); err != nil {
		t.Fatalf("count total rows: %v", err)
	}
	if totalRows < int64(len(all))/2 {
		t.Fatalf("fixture bug: only %d of %d seeded rows landed", totalRows, len(all))
	}
	if tailRows >= totalRows/3 || todayRows >= totalRows/3 {
		t.Fatalf("fixture bug: edge ranges not small relative to total (tail=%d today=%d total=%d)", tailRows, todayRows, totalRows)
	}

	explainRaw := func(t *testing.T, label string, lower, upper time.Time, upperOp string) string {
		t.Helper()
		sql := fmt.Sprintf(`EXPLAIN (ANALYZE, FORMAT TEXT)
SELECT count(*) FILTER (WHERE up), count(*),
       coalesce(sum(total_ms) FILTER (WHERE up AND total_ms<>0),0),
       count(*) FILTER (WHERE up AND total_ms<>0)
FROM site_uptime_probes
WHERE site_id = $1 AND tenant_id = $2
  AND probed_at >= $3::timestamptz AND probed_at %s $4::timestamptz`, upperOp)
		rows, err := admin.Query(ctx, sql, siteID, tenant, lower, upper)
		if err != nil {
			t.Fatalf("%s explain: %v", label, err)
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("%s explain scan: %v", label, err)
			}
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n")
	}

	tailPlan := explainRaw(t, "boundary tail", tailLower, tailUpper, "<")
	todayPlan := explainRaw(t, "today", todayLower, nowUTC, "<=")

	// actualRowsRe pulls the "actual rows=N" figure off the driving scan
	// node's EXPLAIN ANALYZE line — the ground truth for how many rows
	// Postgres actually visited, independent of which index it picked.
	actualRowsRe := regexp.MustCompile(`actual time=[0-9.]+\.\.[0-9.]+ rows=(\d+)`)

	for label, plan := range map[string]string{"boundary tail": tailPlan, "today": todayPlan} {
		t.Logf("%s plan:\n%s", label, plan)
		if strings.Contains(plan, "Seq Scan") {
			t.Fatalf("%s: EXPLAIN shows a Seq Scan on site_uptime_probes — the raw edge read is scanning the whole table, not an index-bounded slice:\n%s", label, plan)
		}
		if !strings.Contains(plan, "Index") {
			t.Fatalf("%s: EXPLAIN does not show any Index (Only) Scan — expected the raw edge read to use an index:\n%s", label, plan)
		}
		// Boundedness: every "actual rows=" figure in the plan (there may be
		// several — the scan node and the Aggregate wrapping it) must be
		// small relative to the full table (totalRows), never anywhere near
		// the full window's worth of rows. This is the real proof the m99
		// hybrid fixes the O(43k rows/site) scan: whichever index Postgres
		// picks, it must not be visiting anywhere close to the whole table.
		matches := actualRowsRe.FindAllStringSubmatch(plan, -1)
		if len(matches) == 0 {
			t.Fatalf("%s: could not find an 'actual rows=' figure in the EXPLAIN ANALYZE output:\n%s", label, plan)
		}
		for _, m := range matches {
			var n int64
			if _, err := fmt.Sscanf(m[1], "%d", &n); err != nil {
				t.Fatalf("%s: parse actual rows %q: %v", label, m[1], err)
			}
			if n > totalRows/4 {
				t.Fatalf("%s: EXPLAIN actual rows=%d, which is not small relative to the full table (%d rows) — the raw edge read is not bounded:\n%s", label, n, totalRows, plan)
			}
		}
	}
}
