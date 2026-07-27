package metrics

// postgres_test.go — m99 static guards for the hybrid fleetUptimeQuery
// (rollup middle + two bounded raw edge reads, see its doc comment in
// postgres.go). These inspect the actual fleetUptimeQuery constant/
// fleetUptimeParams function that QueryFleetUptime executes at runtime — no
// DB needed — so they catch a regression (e.g. someone widening a raw read
// back into an unbounded scan, or dropping the rollup/status references)
// even in environments without Docker. The numeric equivalence property
// (hybrid == old exact rolling-window aggregate) is proven against a real
// Postgres in tests/uptime_rollup_integration_test.go; the EXPLAIN-based
// boundedness proof lives in postgres_explain_test.go (same package, real
// Postgres via testcontainers).

import (
	"strings"
	"testing"
	"time"
)

// TestFleetUptimeQuery_ReferencesRollupAndBoundedRawEdges locks in the m99
// hybrid shape: the rollup tables must be read for the window's middle, and
// site_uptime_probes must be read ONLY for the two edge days, each with an
// explicit lower AND upper probed_at bound (never an open-ended scan back to
// the start of the window).
func TestFleetUptimeQuery_ReferencesRollupAndBoundedRawEdges(t *testing.T) {
	if !strings.Contains(fleetUptimeQuery, "site_uptime_daily") {
		t.Fatal("fleetUptimeQuery no longer references site_uptime_daily — has the rollup middle been removed?")
	}
	if !strings.Contains(fleetUptimeQuery, "site_uptime_status") {
		t.Fatal("fleetUptimeQuery no longer references site_uptime_status — has the latest-status join been removed?")
	}
	if !strings.Contains(fleetUptimeQuery, "site_uptime_probes") {
		t.Fatal("fleetUptimeQuery no longer references site_uptime_probes — the two bounded edge-day raw reads are required for EXACT equivalence")
	}

	// Exactly two site_uptime_probes sub-selects (the boundary tail and
	// today), each bounded on BOTH sides — never an unbounded
	// "probed_at >= X" scan back through the whole window.
	if n := strings.Count(fleetUptimeQuery, "FROM site_uptime_probes"); n != 2 {
		t.Fatalf("fleetUptimeQuery has %d site_uptime_probes sub-selects, want exactly 2 (boundary tail + today)", n)
	}
	// A leading space distinguishes the raw table's bare "probed_at >=" from
	// site_uptime_status's unrelated "last_probed_at >=" freshness guard,
	// which contains "probed_at >=" as a substring but is a different column.
	if n := strings.Count(fleetUptimeQuery, " probed_at >="); n != 2 {
		t.Fatalf("fleetUptimeQuery has %d lower-bounded probed_at predicates, want 2", n)
	}
	// One upper-bounded with '<' (boundary tail, exclusive) and one with
	// '<=' (today, inclusive of now) — both are upper bounds, so together
	// with the two lower bounds above every raw read is a closed range.
	upperBounds := strings.Count(fleetUptimeQuery, "probed_at < $6::timestamptz") + strings.Count(fleetUptimeQuery, "probed_at <= $9::timestamptz")
	if upperBounds != 2 {
		t.Fatalf("fleetUptimeQuery has %d upper-bounded probed_at predicates (want 2: boundary tail '< $6', today '<= $9') — a raw read may have regressed to an unbounded scan", upperBounds)
	}

	// The rollup middle must EXCLUDE both edge days (day > boundaryDay AND
	// day < today) — it must never overlap the two raw reads.
	if !strings.Contains(fleetUptimeQuery, "day > $3::date AND day < $4::date") {
		t.Fatal("fleetUptimeQuery's rollup middle predicate changed shape — must stay strictly between boundaryDay and today to avoid overlapping the raw edge reads")
	}
}

// TestFleetUptimeQuery_SelectsAppHealthColumns is the static regression guard
// for the GH #291 Phase 2 review finding: the Postgres fleet-uptime read
// never selected st.latest_app_up / st.app_probe_reason, so every sweep wrote
// the application-health verdict and QueryFleetUptime silently never
// surfaced it on Postgres - the default metrics backend and the one hosted
// production runs. This inspects the fleetUptimeQuery constant directly (no
// DB needed), the same style as
// TestFleetUptimeQuery_ReferencesRollupAndBoundedRawEdges above, so a future
// edit that drops these two columns from the SELECT list fails a fast,
// DB-less test rather than only a real-Postgres one. See
// TestQueryFleetUptime_SurfacesAppHealthColumns in postgres_explain_test.go
// for the real-Postgres, read-path-specific proof.
func TestFleetUptimeQuery_SelectsAppHealthColumns(t *testing.T) {
	if !strings.Contains(fleetUptimeQuery, "st.latest_app_up") {
		t.Fatal("fleetUptimeQuery no longer selects st.latest_app_up - the application-health verdict would be invisible on Postgres again")
	}
	if !strings.Contains(fleetUptimeQuery, "st.app_probe_reason") {
		t.Fatal("fleetUptimeQuery no longer selects st.app_probe_reason - QueryFleetUptime cannot distinguish \"never app-probed\" from \"probed, verdict unknown\" without it")
	}
}

// TestFleetUptimeParams_DecomposesWithNoGapNoOverlap is a pure (no DB) unit
// test of the window decomposition math: for a range of windows and "now"
// instants (including degenerate sub-day windows and windows that land
// exactly on a day boundary), the three parts — rollup middle
// (boundaryDay, today), raw tail [tailLower, tailUpper), raw today
// [todayLower, now] — must together cover [now-window, now] with NO gap and
// NO overlap.
func TestFleetUptimeParams_DecomposesWithNoGapNoOverlap(t *testing.T) {
	base := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		now    time.Time
		window time.Duration
	}{
		{"7d window, mid-afternoon now", base.Add(14*time.Hour + 37*time.Minute), 7 * 24 * time.Hour},
		{"30d window, mid-afternoon now", base.Add(14*time.Hour + 37*time.Minute), 30 * 24 * time.Hour},
		{"exactly midnight now", base, 7 * 24 * time.Hour},
		{"sub-day window (degenerate: boundaryDay == today)", base.Add(2 * time.Hour), 30 * time.Minute},
		{"window landing exactly on a day boundary", base.Add(6 * time.Hour), 6 * time.Hour},
		{"window one day short of a full day (boundaryDay == today-1)", base.Add(1 * time.Hour), 23 * time.Hour},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			boundaryDay, today, tailLower, tailUpper, _, todayLower, now := fleetUptimeParams(tc.now, tc.window)

			wantStart := tc.now.UTC().Add(-tc.window)
			if !tailLower.Equal(wantStart) {
				t.Fatalf("tailLower = %v, want %v (now-window)", tailLower, wantStart)
			}
			if !now.Equal(tc.now.UTC()) {
				t.Fatalf("now = %v, want %v", now, tc.now.UTC())
			}

			// No gap, no overlap: tailUpper must equal todayLower EXACTLY —
			// the raw tail ends precisely where raw today begins (rollup
			// middle, if non-empty, fills everything from tailUpper's day
			// boundary up to today's day boundary, which by construction is
			// also todayLower when today > boundaryDay+1).
			if boundaryDay.Before(today.Add(-24 * time.Hour)) {
				// Rollup middle is non-empty: tailUpper must be the start of
				// boundaryDay+1 and todayLower must be today's midnight —
				// the rollup fills the gap between them.
				if !tailUpper.Equal(boundaryDay.Add(24 * time.Hour)) {
					t.Fatalf("tailUpper = %v, want boundaryDay+1 = %v", tailUpper, boundaryDay.Add(24*time.Hour))
				}
				if !todayLower.Equal(today) {
					t.Fatalf("todayLower = %v, want today midnight = %v", todayLower, today)
				}
			} else {
				// Degenerate: boundaryDay == today (or today-1 with no room
				// for a rollup day) — tailUpper and todayLower must meet with
				// no gap between the raw tail and raw today.
				if !tailUpper.Equal(todayLower) {
					t.Fatalf("degenerate case: tailUpper = %v, todayLower = %v, want equal (no gap)", tailUpper, todayLower)
				}
			}

			// Bounds sanity: tailLower <= tailUpper <= todayLower <= now.
			if tailLower.After(tailUpper) {
				t.Fatalf("tailLower %v > tailUpper %v", tailLower, tailUpper)
			}
			if tailUpper.After(todayLower) {
				t.Fatalf("tailUpper %v > todayLower %v", tailUpper, todayLower)
			}
			if todayLower.After(now) {
				t.Fatalf("todayLower %v > now %v", todayLower, now)
			}

			// The two raw reads are each bounded to at most ~1 UTC day.
			if d := tailUpper.Sub(tailLower); d > 24*time.Hour {
				t.Fatalf("raw boundary tail spans %v, want <= 24h", d)
			}
			if d := now.Sub(todayLower); d > 24*time.Hour {
				t.Fatalf("raw today spans %v, want <= 24h", d)
			}
		})
	}
}
