package uptime

// retention_window_invariant_test.go — GH #460 security-review follow-up.
//
// The seam this pins: the daily decomposition serves complete days from the
// site_uptime_daily rollup but EXCLUDES the boundary day (part 1 of
// metrics.dailySeriesParts is `day > $3::date`), so the OLDEST labelled day of
// the longest strip is served from site_uptime_probes alone — the table
// metrics.UptimeProbeGCWorker prunes at exactly metrics.ProbeRetention.
//
// It is correct today with a positive margin, and this file is why it stays
// that way: lengthen the endpoint's longest window or shrink retention and
// this goes red, instead of the oldest cell of the strip going quietly null
// on the page that exists to be honest about what was measured.
//
// WHY IT LIVES HERE. The first version sat in internal/metrics and restated
// the longest window as its own literal (`maxHistoryWindowDays = 90`), on the
// mistaken reasoning that importing internal/uptime from internal/metrics
// would be a cycle. It would be — but the dependency only runs one way:
// internal/uptime already imports internal/metrics, so the guard belongs on
// THIS side, where it can read historyWindowDays directly. With the literal,
// widening the endpoint to 120d would have left the test green while the
// invariant it exists to protect broke silently. A guard that cannot detect
// the drift it was written for is worse than none, because it is trusted.
//
// Neither bound is written down here. The maximum comes from
// historyWindowDays — the map the handler actually enforces — and retention
// from metrics.ProbeRetention. No database is needed, so this also runs where
// the container-backed proofs would skip for want of Docker.

import (
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
)

// longestHistoryWindowDays derives the endpoint's longest accepted window
// from historyWindowDays itself. Deliberately a scan, not a constant: adding
// a "120d" entry to that map is what must redden this test, with nothing else
// to remember to edit.
func longestHistoryWindowDays(t *testing.T) int {
	t.Helper()
	if len(historyWindowDays) == 0 {
		t.Fatal("historyWindowDays is empty — the endpoint accepts no window, or this guard is reading the wrong thing")
	}
	max := 0
	for w, days := range historyWindowDays {
		if days <= 0 {
			t.Fatalf("historyWindowDays[%q] = %d, which is not a usable window length", w, days)
		}
		if days > max {
			max = days
		}
	}
	return max
}

// TestProbeRetentionCoversTheLongestHistoryWindow asserts that the oldest day
// the longest strip labels is still inside raw-probe retention, at EVERY time
// of day.
//
// GetFleetUptimeHistory asks the store for a window reaching back to the UTC
// midnight of its oldest labelled day, so the window length is (days-1) plus
// the current time of day. The margin against retention is therefore 24h
// minus the time of day: largest just after UTC midnight, smallest just
// before it. Sweeping the whole day matters — an invariant checked only at
// the moment the test happens to run would pass at 00:05 and hide a violation
// that appears at 23:55.
func TestProbeRetentionCoversTheLongestHistoryWindow(t *testing.T) {
	days := longestHistoryWindowDays(t)
	base := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)

	worstMargin := time.Duration(1<<63 - 1)
	var worstAt time.Time

	for m := 0; m < 24*60; m++ {
		minute := base.Add(time.Duration(m) * time.Minute)
		// The minute boundary and the last instant before the next one: the
		// worst case sits at the very end of the UTC day.
		for _, now := range []time.Time{minute, minute.Add(59*time.Second + 999*time.Millisecond)} {
			today := now.Truncate(24 * time.Hour)
			start := today.AddDate(0, 0, -(days - 1))
			retentionCutoff := now.Add(-metrics.ProbeRetention)

			margin := start.Sub(retentionCutoff)
			if margin <= 0 {
				t.Fatalf("at %s: the oldest labelled day (%s) is at or past the probe retention cutoff (%s), margin %s — "+
					"the oldest cell of the %d-day strip would read as no-data even though the day was measured. "+
					"metrics.ProbeRetention=%s longest window=%dd",
					now.Format(time.RFC3339), start.Format(time.RFC3339), retentionCutoff.Format(time.RFC3339),
					margin, days, metrics.ProbeRetention, days)
			}
			if margin < worstMargin {
				worstMargin = margin
				worstAt = now
			}
		}
	}

	t.Logf("metrics.ProbeRetention=%s longest accepted window=%dd: worst-case margin %s (at %s)",
		metrics.ProbeRetention, days, worstMargin, worstAt.Format(time.RFC3339))

	// The margin is thin by construction. Record the actual figure so a future
	// reader watches it shrink rather than discovering it at zero: while
	// retention and the longest window are the same number of days it is under
	// 24h, and any widening of that gap should be a deliberate, visible change.
	if worstMargin >= 24*time.Hour {
		t.Errorf("worst-case margin is %s, expected under 24h — retention or the window changed; "+
			"that may well be an improvement, but update this assertion and metrics.probeRetention's comment deliberately",
			worstMargin)
	}
}

// TestLongestHistoryWindowFitsWithinRetention states the coupling directly, in
// whole days, so a violation names the two numbers rather than a duration
// arithmetic result.
func TestLongestHistoryWindowFitsWithinRetention(t *testing.T) {
	days := longestHistoryWindowDays(t)
	retentionDays := int(metrics.ProbeRetention / (24 * time.Hour))

	if days > retentionDays {
		t.Fatalf("the endpoint accepts a %dd window but raw probes are retained only %dd — "+
			"the oldest labelled days are outside retention entirely, and the strip would show measured history as no-data",
			days, retentionDays)
	}
	if days != retentionDays {
		t.Errorf("longest window (%dd) and retention (%dd) have diverged — "+
			"update metrics.probeRetention's comment and the sub-24h margin assertion together",
			days, retentionDays)
	}
}
