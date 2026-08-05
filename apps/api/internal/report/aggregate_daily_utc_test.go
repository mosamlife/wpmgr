package report

// aggregate_daily_utc_test.go: 0.61.125. The uptime series feeding a client
// report is now one point per UTC day for any window of a day or more
// (metrics.pgStore.querySeriesDaily), and each of those points sits EXACTLY on
// a UTC midnight. aggregateDailyBuckets used to read the calendar day off the
// driver-returned local-zone value, which is harmless for the arbitrary
// instants the old fixed-width bucketing produced but puts every
// UTC-midnight point in the previous calendar day on any deployment west of
// UTC. This test runs the grouping against a deliberately negative-offset
// zone, so it fails if the UTC grouping is ever removed.

import (
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
)

func TestAggregateDailyBuckets_GroupsByUTCDayRegardlessOfServerZone(t *testing.T) {
	// A zone well west of UTC: a UTC midnight is 17:00 the PREVIOUS day here.
	west := time.FixedZone("UTC-7", -7*60*60)

	days := []time.Time{
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC),
	}
	points := make([]metrics.Point, 0, len(days))
	for i, d := range days {
		points = append(points, metrics.Point{
			// Same instant, expressed in the west zone: exactly what the pgx
			// driver hands back on a process whose local zone is not UTC.
			Bucket:       d.In(west),
			Checks:       1440,
			UpChecks:     uint64(1440 - i),
			AvgLatencyMs: float64(100 + i),
		})
	}

	got := aggregateDailyBuckets(points)
	if len(got) != len(days) {
		t.Fatalf("got %d daily buckets, want %d", len(got), len(days))
	}
	for i, day := range got {
		want := days[i]
		if !day.Day.Equal(want) {
			t.Fatalf("bucket %d has day %v, want %v. The grouping is reading the calendar day in the server's local zone, so every UTC-midnight point lands in the previous day", i, day.Day, want)
		}
		if day.Day.Location() != time.UTC {
			t.Fatalf("bucket %d day %v is not in UTC", i, day.Day)
		}
	}

	// The counters must survive the regrouping untouched.
	if got[0].UptimePct != 100 {
		t.Fatalf("first day UptimePct = %v, want 100 (1440/1440 up)", got[0].UptimePct)
	}
	if got[2].AvgLatencyMs != 102 {
		t.Fatalf("third day AvgLatencyMs = %v, want 102", got[2].AvgLatencyMs)
	}
}
