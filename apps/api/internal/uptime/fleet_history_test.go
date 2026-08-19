package uptime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
)

// TestGetFleetUptimeHistory_SeededGapIsNullNotZero is the GH #460 proof that
// the day strip reports only what was measured.
//
// The seed is deliberately lopsided: the site has stored counters for the
// OLDEST 45 days of a 90-day window (D-89..D-45) and nothing for the most
// recent 45 (D-44..D-0). That shape catches the failure this endpoint is most
// likely to have — returning the right NUMBER of entries whose values are
// defaults. An assertion that only counts 90 entries passes against a
// zero-filled array; an assertion that only checks "some days are null"
// passes against an array that nulled the wrong end. So this asserts, per
// index, which days carry a measurement and which do not, and that the
// measured ones carry the seeded percentage rather than a placeholder.
//
// It also pins the two things a client depends on positionally: the array is
// exactly windowDays long, oldest first, and dates are consecutive UTC days
// with no gaps — the store emits nothing for an unprobed day, so an endpoint
// that mapped its output straight through would shift a young site's history
// into the wrong dates.
func TestGetFleetUptimeHistory_SeededGapIsNullNotZero(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	const windowDays = 90
	const measuredDays = 45 // D-89..D-45 inclusive

	today := time.Now().UTC().Truncate(24 * time.Hour)
	start := today.AddDate(0, 0, -(windowDays - 1))

	// Seed the oldest 45 days with real, DISTINCT counters. Distinct values
	// matter: identical ones would let an implementation that returns a
	// constant pass.
	points := make([]metrics.Point, 0, measuredDays)
	wantPct := make(map[string]float64, measuredDays)
	for i := 0; i < measuredDays; i++ {
		day := start.AddDate(0, 0, i)
		total := uint64(1440)
		up := total - uint64(i) // 1440/1440, 1439/1440, ...
		points = append(points, metrics.Point{
			Bucket:       day,
			Checks:       total,
			UpChecks:     up,
			AvgLatencyMs: float64(100 + i),
		})
		wantPct[day.Format(dateLayout)] = float64(up) / float64(total) * 100
	}

	repo := &stubRepo{infos: []FleetSiteInfo{{
		SiteID: siteID, Name: "gapped", URL: "https://gapped.example",
		ConnectionState: "connected", HealthStatus: "healthy",
	}}}
	store := &stubStore{dailySeries: map[uuid.UUID][]metrics.Point{siteID: points}}

	svc := NewService(repo, store, nil)
	resp, err := svc.GetFleetUptimeHistory(context.Background(), tenantID, []uuid.UUID{siteID}, "90d")
	if err != nil {
		t.Fatalf("GetFleetUptimeHistory: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(resp.Items))
	}
	item := resp.Items[0]
	if len(item.Days) != windowDays {
		t.Fatalf("got %d days, want %d", len(item.Days), windowDays)
	}
	if resp.Days != windowDays || resp.Window != "90d" {
		t.Errorf("resp.Days=%d resp.Window=%q, want %d and \"90d\"", resp.Days, resp.Window, windowDays)
	}
	if item.MeasuredDays != measuredDays {
		t.Errorf("MeasuredDays = %d, want %d — the coverage figure is what stops the UI implying 90 days of history",
			item.MeasuredDays, measuredDays)
	}

	// Dates: consecutive UTC days, oldest first, matching the response bounds.
	if item.Days[0].Date != resp.StartDate || item.Days[windowDays-1].Date != resp.EndDate {
		t.Errorf("date bounds: days[0]=%s days[last]=%s, resp says %s..%s",
			item.Days[0].Date, item.Days[windowDays-1].Date, resp.StartDate, resp.EndDate)
	}
	for i, d := range item.Days {
		want := start.AddDate(0, 0, i).Format(dateLayout)
		if d.Date != want {
			t.Fatalf("days[%d].Date = %s, want %s — the strip is not densified by UTC date", i, d.Date, want)
		}
	}

	// The measured half carries the seeded percentages; the gap half is nil.
	for i, d := range item.Days {
		measured := i < measuredDays
		switch {
		case measured && d.UptimePct == nil:
			t.Errorf("days[%d] (%s) is null, want the seeded measurement", i, d.Date)
		case measured && *d.UptimePct != wantPct[d.Date]:
			t.Errorf("days[%d] (%s) uptime_pct = %v, want %v — a default value would also produce 90 entries",
				i, d.Date, *d.UptimePct, wantPct[d.Date])
		case measured && d.Checks != 1440:
			t.Errorf("days[%d] (%s) checks = %d, want 1440", i, d.Date, d.Checks)
		case !measured && d.UptimePct != nil:
			t.Errorf("days[%d] (%s) uptime_pct = %v, want null — no counters were stored for that day",
				i, d.Date, *d.UptimePct)
		case !measured && d.Checks != 0:
			t.Errorf("days[%d] (%s) checks = %d, want 0", i, d.Date, d.Checks)
		}
	}

	// Wire level: an unmeasured day must encode as literal null. Decoded into
	// a raw map so "key present, value null" stays distinguishable from an
	// absent key.
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire struct {
		Items []struct {
			Days []map[string]json.RawMessage `json:"days"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	gapDay := wire.Items[0].Days[windowDays-1]
	raw, present := gapDay["uptime_pct"]
	if !present {
		t.Fatal("uptime_pct key absent on an unmeasured day — the client cannot tell no-data from a dropped field")
	}
	if string(raw) != "null" {
		t.Errorf("unmeasured day serialised uptime_pct as %s, want null", string(raw))
	}
}

// TestGetFleetUptimeHistory_NoHistoryIsAllNull covers the site the issue was
// filed about: never probed at all. Every day must be null — not zero, and
// not an empty array that a client would render as "no outages".
func TestGetFleetUptimeHistory_NoHistoryIsAllNull(t *testing.T) {
	siteID := uuid.New()
	repo := &stubRepo{infos: []FleetSiteInfo{{
		SiteID: siteID, Name: "fresh", URL: "https://fresh.example",
		ConnectionState: "connected", HealthStatus: "unknown",
	}}}
	// Store returns nothing for this site at all.
	store := &stubStore{dailySeries: map[uuid.UUID][]metrics.Point{}}

	svc := NewService(repo, store, nil)
	resp, err := svc.GetFleetUptimeHistory(context.Background(), uuid.New(), []uuid.UUID{siteID}, "90d")
	if err != nil {
		t.Fatalf("GetFleetUptimeHistory: %v", err)
	}
	item := resp.Items[0]
	if len(item.Days) != 90 {
		t.Fatalf("got %d days, want 90 — the window must still be described, just with no data in it", len(item.Days))
	}
	if item.MeasuredDays != 0 {
		t.Errorf("MeasuredDays = %d, want 0", item.MeasuredDays)
	}
	for i, d := range item.Days {
		if d.UptimePct != nil {
			t.Fatalf("days[%d] (%s) = %v, want null for a never-probed site", i, d.Date, *d.UptimePct)
		}
	}
}

// TestGetFleetUptimeHistory_SingleDayOutage: a site up every day except one
// produces exactly ONE non-100% cell, on that date. This is the assertion
// that the strip localises an outage rather than smearing it, which the
// heuristic it replaces could not do.
func TestGetFleetUptimeHistory_SingleDayOutage(t *testing.T) {
	siteID := uuid.New()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	start := today.AddDate(0, 0, -6) // 7d window

	outageDay := start.AddDate(0, 0, 3)
	points := make([]metrics.Point, 0, 7)
	for i := 0; i < 7; i++ {
		day := start.AddDate(0, 0, i)
		p := metrics.Point{Bucket: day, Checks: 1440, UpChecks: 1440}
		if day.Equal(outageDay) {
			p.UpChecks = 720 // half the day down
		}
		points = append(points, p)
	}

	repo := &stubRepo{infos: []FleetSiteInfo{{
		SiteID: siteID, Name: "outage", URL: "https://outage.example",
		ConnectionState: "connected", HealthStatus: "healthy",
	}}}
	store := &stubStore{dailySeries: map[uuid.UUID][]metrics.Point{siteID: points}}

	svc := NewService(repo, store, nil)
	resp, err := svc.GetFleetUptimeHistory(context.Background(), uuid.New(), []uuid.UUID{siteID}, "7d")
	if err != nil {
		t.Fatalf("GetFleetUptimeHistory: %v", err)
	}
	item := resp.Items[0]
	if len(item.Days) != 7 {
		t.Fatalf("got %d days, want 7", len(item.Days))
	}

	var degraded []string
	for _, d := range item.Days {
		if d.UptimePct == nil {
			t.Fatalf("day %s is null, want a measurement (every day was seeded)", d.Date)
		}
		if *d.UptimePct != 100 {
			degraded = append(degraded, d.Date)
		}
	}
	if len(degraded) != 1 {
		t.Fatalf("got %d non-100%% days (%v), want exactly 1", len(degraded), degraded)
	}
	if degraded[0] != outageDay.Format(dateLayout) {
		t.Errorf("outage landed on %s, want %s", degraded[0], outageDay.Format(dateLayout))
	}
	if got := *item.Days[3].UptimePct; got != 50 {
		t.Errorf("outage day uptime_pct = %v, want 50", got)
	}
}

// TestGetFleetUptimeHistory_RejectsUnknownWindow: the window is an enum
// because the day count is also the response size. An arbitrary string must
// be a 400, never a silently-substituted default that returns a strip
// describing a different period than the caller asked for.
func TestGetFleetUptimeHistory_RejectsUnknownWindow(t *testing.T) {
	svc := NewService(&stubRepo{}, &stubStore{}, nil)
	for _, w := range []string{"1d", "365d", "", "90", "90D", "all"} {
		_, err := svc.GetFleetUptimeHistory(context.Background(), uuid.New(), []uuid.UUID{uuid.New()}, w)
		if err == nil {
			t.Errorf("window %q was accepted, want a validation error", w)
			continue
		}
		de, ok := domain.AsDomain(err)
		if !ok || de.Code != "invalid_window" {
			t.Errorf("window %q: err = %v, want a domain validation error with code invalid_window", w, err)
		}
	}
	for _, w := range []string{"7d", "30d", "90d"} {
		if _, err := svc.GetFleetUptimeHistory(context.Background(), uuid.New(), nil, w); err != nil {
			t.Errorf("window %q was rejected: %v", w, err)
		}
	}
}
