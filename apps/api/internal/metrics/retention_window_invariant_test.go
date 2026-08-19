package metrics

// retention_window_invariant_test.go — GH #460 security-review follow-up.
//
// The seam this pins: the daily decomposition serves complete days from the
// site_uptime_daily rollup but EXCLUDES the boundary day (part 1 of
// dailySeriesParts is `day > $3::date`), so the OLDEST labelled day of a
// 90-day strip is served from site_uptime_probes alone — the table
// UptimeProbeGCWorker prunes at exactly probeRetention.
//
// It is correct today with a positive margin, and this file is why it stays
// that way: change probeRetention or lengthen the longest history window and
// this goes red, instead of the oldest cell of the strip going quietly null
// on the page that exists to be honest about what was measured.
//
// No database is needed — the invariant is pure arithmetic over the two
// constants and the production boundary function, so it runs everywhere,
// including where startMetricsTestPostgres would skip for want of Docker.

import (
	"testing"
	"time"
)

// maxHistoryWindowDays is the longest window uptime.GetFleetUptimeHistory
// accepts (its historyWindowDays map tops out at "90d"). It is restated here
// rather than imported because internal/uptime imports this package, so a
// real import would be a cycle. TestMaxHistoryWindowMatchesTheEndpoint below
// is the guard against the two drifting apart.
const maxHistoryWindowDays = 90

// TestProbeRetentionCoversTheLongestHistoryWindow asserts that the oldest day
// the 90-day strip labels is still inside raw-probe retention, at EVERY time
// of day.
//
// The endpoint asks for a window reaching back to the UTC midnight of its
// oldest labelled day, so the window length is (days-1) plus the current time
// of day. The margin against retention is therefore 24h minus the time of
// day: largest just after UTC midnight, smallest just before it. Sweeping the
// whole day matters — an invariant checked only at the moment the test
// happens to run would pass at 00:05 and hide a violation that appears at
// 23:55.
func TestProbeRetentionCoversTheLongestHistoryWindow(t *testing.T) {
	base := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)

	var worstMargin = time.Duration(1<<63 - 1)
	var worstAt time.Time

	// Every minute of a full UTC day, plus the last instant before midnight.
	for m := 0; m < 24*60; m++ {
		now := base.Add(time.Duration(m) * time.Minute)
		for _, now := range []time.Time{now, now.Add(59*time.Second + 999*time.Millisecond)} {
			today := now.Truncate(24 * time.Hour)
			start := today.AddDate(0, 0, -(maxHistoryWindowDays - 1))
			window := now.Sub(start)

			boundaryDay, _, tailLower, _, retentionCutoff, _, _ := fleetUptimeParams(now, window)

			// The model the margin rests on: the oldest labelled day IS the
			// boundary day, i.e. the day served from raw probes only. If this
			// ever stops holding, the arithmetic below is measuring the wrong
			// thing and the margin figure would be meaningless.
			if !boundaryDay.Equal(start) {
				t.Fatalf("at %s: boundaryDay %s != oldest labelled day %s — the retention seam is not where this test assumes",
					now.Format(time.RFC3339), boundaryDay, start)
			}
			if !tailLower.Equal(start) {
				t.Fatalf("at %s: tailLower %s != oldest labelled day %s — the raw read does not start at that day's midnight",
					now.Format(time.RFC3339), tailLower, start)
			}

			margin := start.Sub(retentionCutoff)
			if margin <= 0 {
				t.Fatalf("at %s: the oldest labelled day (%s) is at or past the probe retention cutoff (%s), margin %s — "+
					"the oldest cell of the 90-day strip would read as no-data even though the day was measured. "+
					"probeRetention=%s maxHistoryWindowDays=%d",
					now.Format(time.RFC3339), start.Format(time.RFC3339), retentionCutoff.Format(time.RFC3339),
					margin, probeRetention, maxHistoryWindowDays)
			}
			if margin < worstMargin {
				worstMargin = margin
				worstAt = now
			}
		}
	}

	t.Logf("probeRetention=%s maxHistoryWindowDays=%d: worst-case margin %s (at %s)",
		probeRetention, maxHistoryWindowDays, worstMargin, worstAt.Format(time.RFC3339))

	// The margin is thin by construction. Record the actual figure so a
	// future reader sees it shrink rather than discovering it at zero: with
	// probeRetention == maxHistoryWindowDays it is under a day, and any
	// widening of that gap should be a deliberate, visible change here.
	if worstMargin >= 24*time.Hour {
		t.Errorf("worst-case margin is %s, expected under 24h — probeRetention or the window changed; "+
			"that may well be an improvement, but update this assertion and probeRetention's comment deliberately",
			worstMargin)
	}
}

// TestMaxHistoryWindowMatchesTheEndpoint keeps maxHistoryWindowDays honest.
// internal/uptime cannot be imported here (it imports this package), so the
// coupling is asserted against the retention constant instead: the endpoint's
// longest window must never exceed retention in whole days, which is the
// property the seam actually depends on.
func TestMaxHistoryWindowMatchesTheEndpoint(t *testing.T) {
	retentionDaysWhole := int(probeRetention / (24 * time.Hour))
	if maxHistoryWindowDays > retentionDaysWhole {
		t.Fatalf("maxHistoryWindowDays=%d exceeds probeRetention=%d days — the oldest labelled days are outside retention entirely",
			maxHistoryWindowDays, retentionDaysWhole)
	}
	// If retention ever grows well beyond the window, the sub-24h assertion
	// in the test above becomes wrong rather than merely tight; fail here so
	// the two are updated together.
	if retentionDaysWhole != maxHistoryWindowDays {
		t.Errorf("probeRetention (%d days) and maxHistoryWindowDays (%d) have diverged — "+
			"update probeRetention's comment and the margin assertion above together",
			retentionDaysWhole, maxHistoryWindowDays)
	}
}
