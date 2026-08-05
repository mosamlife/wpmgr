package metrics

// postgres_persite_test.go: 0.61.125 static guards for the per-site window
// reads (siteAggregateQuery / siteDailySeriesQuery), mirroring the style of
// postgres_test.go's guards on fleetUptimeQuery: they inspect the actual SQL
// constants and the actual threshold constant the production code executes, so
// a regression that widens a bounded read back into a full-window scan, drops
// the rollup, or silently moves the daily/raw cutover fails a fast, DB-less
// test. The numeric equivalence and granularity properties are proven against
// a real Postgres in postgres_persite_pg_test.go.

import (
	"strings"
	"testing"
	"time"
)

// TestSitePerWindowQueries_ReadRollupAndOnlyBoundedRawEdges is the per-site
// twin of TestFleetUptimeQuery_ReferencesRollupAndBoundedRawEdges. Before this
// change QueryAggregate and QuerySeries each ran a single
// "probed_at >= now - window" scan over site_uptime_probes, which is what made
// the endpoint cost up to 32.5s on a cold buffer cache. Both queries must now
// read the rollup for the window's middle and touch raw probes ONLY through
// two ranges that are bounded on BOTH sides.
func TestSitePerWindowQueries_ReadRollupAndOnlyBoundedRawEdges(t *testing.T) {
	for name, q := range map[string]string{
		"siteAggregateQuery":   siteAggregateQuery,
		"siteDailySeriesQuery": siteDailySeriesQuery,
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(q, "site_uptime_daily") {
				t.Fatalf("%s no longer reads site_uptime_daily: the whole window would be served from raw probes again", name)
			}
			if n := strings.Count(q, "FROM site_uptime_probes"); n != 2 {
				t.Fatalf("%s has %d site_uptime_probes sub-selects, want exactly 2 (boundary tail + today)", name, n)
			}
			if n := strings.Count(q, " probed_at >="); n != 2 {
				t.Fatalf("%s has %d lower-bounded probed_at predicates, want 2", name, n)
			}
			upper := strings.Count(q, "probed_at < $6::timestamptz") + strings.Count(q, "probed_at <= $8::timestamptz")
			if upper != 2 {
				t.Fatalf("%s has %d upper-bounded probed_at predicates (want 2: boundary tail '< $6', today '<= $8'): a raw read may have regressed to an unbounded scan", name, upper)
			}
			// The rollup middle must EXCLUDE both edge days, or it would double
			// count the two raw reads.
			if !strings.Contains(q, "day > $3::date AND day < $4::date") {
				t.Fatalf("%s's rollup middle predicate changed shape: it must stay strictly between boundaryDay and today so it can never overlap the raw edge reads", name)
			}
			// Every tenant-scoped read keeps its explicit tenant_id predicate
			// (defense in depth in front of RLS, and index coverage).
			if n := strings.Count(q, "tenant_id = $1"); n != 3 {
				t.Fatalf("%s has %d explicit tenant_id predicates, want 3 (one per window part)", name, n)
			}
		})
	}
}

// TestSiteDailySeriesQuery_BucketsAreUTCAndNotSessionDependent guards the one
// timezone trap in the daily series: site_uptime_daily.day is a `date`, and a
// bare day::timestamptz would be resolved against the Postgres session's
// TimeZone GUC, shifting every bucket by the server's offset on a non-UTC
// self-host. The conversion must be pinned to UTC, exactly like every other
// boundary in the decomposition.
func TestSiteDailySeriesQuery_BucketsAreUTCAndNotSessionDependent(t *testing.T) {
	if !strings.Contains(siteDailySeriesQuery, "(day::timestamp AT TIME ZONE 'UTC')") {
		t.Fatal("siteDailySeriesQuery no longer converts the rollup's date column with an explicit AT TIME ZONE 'UTC': daily buckets would shift with the server's session timezone")
	}
	// The two raw edge parts label themselves from bound timestamptz params
	// ($9/$10), never from a server-side now()/CURRENT_DATE.
	for _, forbidden := range []string{"CURRENT_DATE", "now()"} {
		if strings.Contains(siteDailySeriesQuery, forbidden) {
			t.Fatalf("siteDailySeriesQuery uses %s: every boundary must be computed in Go from now.UTC() and bound as a parameter", forbidden)
		}
	}
	if !strings.Contains(siteDailySeriesQuery, "$9::timestamptz") || !strings.Contains(siteDailySeriesQuery, "$10::timestamptz") {
		t.Fatal("siteDailySeriesQuery no longer labels its two raw edge parts from the bound $9/$10 bucket params")
	}
	// Days with no probes must not appear, matching the raw path's GROUP BY.
	if !strings.Contains(siteDailySeriesQuery, "WHERE total_checks > 0") {
		t.Fatal("siteDailySeriesQuery dropped the total_checks > 0 filter: an empty edge range would emit a zero-check point the raw path never emitted")
	}
}

// TestDailySeriesMinWindow_IsExactlyOneDay pins the documented threshold. It is
// not a taste knob: 24h is exactly the smallest window for which the
// decomposition guarantees boundaryDay < today, which is what makes the
// boundary-tail day, the rollup middle days and today three disjoint day sets.
// Changing it without changing that reasoning is a bug.
func TestDailySeriesMinWindow_IsExactlyOneDay(t *testing.T) {
	if dailySeriesMinWindow != 24*time.Hour {
		t.Fatalf("dailySeriesMinWindow = %v, want exactly 24h (see its doc comment for why this exact value)", dailySeriesMinWindow)
	}
}

// TestDailySeriesThreshold_GuaranteesDisjointDayParts is the proof behind the
// threshold: at or above dailySeriesMinWindow, for ANY instant within a UTC
// day, the oldest (partial) day is strictly before today, so the three parts
// each own a distinct set of days and no day can be counted twice or dropped.
// Below the threshold that guarantee does not hold, which is why the sub-day
// path keeps reading raw probes.
func TestDailySeriesThreshold_GuaranteesDisjointDayParts(t *testing.T) {
	base := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	offsets := []time.Duration{
		0,
		time.Second,
		time.Minute,
		6 * time.Hour,
		12*time.Hour + 37*time.Minute,
		23*time.Hour + 59*time.Minute + 59*time.Second,
	}
	windows := []time.Duration{
		dailySeriesMinWindow,
		dailySeriesMinWindow + time.Minute,
		25 * time.Hour,
		48 * time.Hour,
		7 * 24 * time.Hour,
		30 * 24 * time.Hour,
		90 * 24 * time.Hour,
	}

	for _, off := range offsets {
		for _, window := range windows {
			now := base.Add(off)
			boundaryDay, today, tailLower, tailUpper, _, todayLower, nowUTC := fleetUptimeParams(now, window)

			if !boundaryDay.Before(today) {
				t.Fatalf("window=%v now=%v: boundaryDay %v is not strictly before today %v, so the boundary tail and today would fight over the same day", window, now, boundaryDay, today)
			}
			// The boundary tail never reaches into today, and today's part
			// never reaches back before today's midnight: no overlap.
			if tailUpper.After(today) {
				t.Fatalf("window=%v now=%v: tailUpper %v spills past today's midnight %v", window, now, tailUpper, today)
			}
			if todayLower.Before(today) {
				t.Fatalf("window=%v now=%v: todayLower %v starts before today's midnight %v", window, now, todayLower, today)
			}
			// And no gap: the tail ends exactly where the rollup middle picks
			// up, and the rollup middle ends exactly where today's part starts.
			if !tailUpper.Equal(boundaryDay.Add(24*time.Hour)) && !tailUpper.Equal(today) {
				t.Fatalf("window=%v now=%v: tailUpper %v is neither boundaryDay+1 nor today: the tail no longer meets the next part", window, now, tailUpper)
			}
			if tailLower.After(tailUpper) || todayLower.After(nowUTC) {
				t.Fatalf("window=%v now=%v: bounds inverted (tailLower=%v tailUpper=%v todayLower=%v now=%v)", window, now, tailLower, tailUpper, todayLower, nowUTC)
			}
		}
	}

	// Below the threshold the guarantee genuinely does not hold: a sub-day
	// window sitting inside today collapses boundaryDay onto today. This is
	// the case the raw path exists to serve, and asserting it here keeps the
	// threshold's justification honest rather than merely asserted in a
	// comment.
	boundaryDay, today, _, _, _, _, _ := fleetUptimeParams(base.Add(14*time.Hour), time.Hour)
	if !boundaryDay.Equal(today) {
		t.Fatalf("sub-day window: boundaryDay %v != today %v, so the stated reason for the 24h threshold no longer holds", boundaryDay, today)
	}
}
