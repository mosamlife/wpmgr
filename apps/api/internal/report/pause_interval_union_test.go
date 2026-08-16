package report

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// GH #414 follow-up, defect 1. overlapForIntervals summed each interval's
// clipped length into one accumulator with no union, so two OVERLAPPING pause
// intervals double-counted their overlap and a mostly-measured month was
// classified overlapFull: the uptime section suppressed entirely, the site
// dropped out of ReportTotals.UptimeSiteCount, and the customer told
// monitoring was off all period.
//
// The overlaps are not hypothetical. They are what the audit-trail
// reconstruction produced whenever a site.monitoring.resumed write was lost —
// which the auto-resume path can do and log past
// (internal/site/monitoring_resume_worker.go).

var (
	unionWindowStart = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	unionWindowEnd   = unionWindowStart.AddDate(0, 0, 30)
)

func day(n int) time.Time { return unionWindowStart.AddDate(0, 0, n) }

func ptr(t time.Time) *time.Time { return &t }

// TestOverlappingIntervalsMeasureUnionNotSum is the adversary's fixture: two
// pauses [day0, day16) and [day2, day18) over a 30-day window. Their UNION is
// 18 days; their SUM is 32, which exceeds the window and was classified full.
// Twelve measured days were discarded.
func TestOverlappingIntervalsMeasureUnionNotSum(t *testing.T) {
	intervals := []PauseInterval{
		{Start: day(0), End: ptr(day(16))},
		{Start: day(2), End: ptr(day(18))},
	}
	overlap, unmonitored := overlapForIntervals(intervals, unionWindowStart, unionWindowEnd)
	if overlap != overlapPartial {
		t.Errorf("overlap = %v, want overlapPartial — 18 of 30 days were paused, 12 were measured", overlap)
	}
	if want := 18 * 24 * time.Hour; unmonitored != want {
		t.Errorf("unmonitored = %v, want %v (the union, not the %v sum)", unmonitored, want, 32*24*time.Hour)
	}
}

// TestOverlappingIntervalsKeepUptimeSection walks the same fixture through
// BuildReportData, because the damage is not in the classifier — it is that
// the section vanishes from the document and the site leaves the fleet
// average's denominator.
func TestOverlappingIntervalsKeepUptimeSection(t *testing.T) {
	siteID := uuid.New()
	s := site.Site{ID: siteID, Name: "overlapping-pauses", URL: "https://o.example"}
	sources := Sources{
		ListClientSites: func(context.Context, uuid.UUID, uuid.UUID) ([]site.Site, error) {
			return []site.Site{s}, nil
		},
		QueryUptimeAggregateRange: func(context.Context, uuid.UUID, uuid.UUID, time.Time, time.Time) (metrics.Aggregate, error) {
			return metrics.Aggregate{Checks: 300, UpChecks: 297, UptimePct: 99.0}, nil
		},
		QueryMonitoringPauseIntervals: func(context.Context, uuid.UUID, uuid.UUID, time.Time, time.Time) ([]PauseInterval, error) {
			return []PauseInterval{
				{Start: day(0), End: ptr(day(16))},
				{Start: day(2), End: ptr(day(18))},
			}, nil
		},
	}
	rd, err := BuildReportData(context.Background(), sources, BuildInput{
		TenantID: uuid.New(), ClientID: uuid.New(), Client: ClientInfo{Name: "Acme"}, AgencyName: "Agency",
		PeriodStart: unionWindowStart, PeriodEnd: unionWindowEnd,
	})
	if err != nil {
		t.Fatalf("BuildReportData: %v", err)
	}
	sr := rd.Sites[0]
	if sr.Uptime == nil {
		t.Fatal("uptime section suppressed: 12 measured days were discarded because two overlapping pauses summed past the window length")
	}
	if !sr.Uptime.PartialCoverage {
		t.Error("PartialCoverage = false, want true — 18 of 30 days really were unmonitored")
	}
	if want := float64(18 * 24); sr.Uptime.UnmonitoredHours != want {
		t.Errorf("UnmonitoredHours = %v, want %v — this is the number both renderers print", sr.Uptime.UnmonitoredHours, want)
	}
	if rd.Totals.UptimeSiteCount != 1 {
		t.Errorf("UptimeSiteCount = %d, want 1 — a measured site must stay in the average's denominator", rd.Totals.UptimeSiteCount)
	}
}

// TestFullyPausedWindowStillSuppressed is the honest case the union fix must
// not break: a genuine pause covering the entire window still suppresses the
// section and still leaves the site out of the denominator.
func TestFullyPausedWindowStillSuppressed(t *testing.T) {
	intervals := []PauseInterval{{Start: day(-5), End: ptr(day(40))}}
	overlap, unmonitored := overlapForIntervals(intervals, unionWindowStart, unionWindowEnd)
	if overlap != overlapFull {
		t.Errorf("overlap = %v, want overlapFull", overlap)
	}
	if want := 30 * 24 * time.Hour; unmonitored != want {
		t.Errorf("unmonitored = %v, want %v", unmonitored, want)
	}
	// Contiguous halves that tile the window exactly are also full: the union
	// fix must not turn an adjacent pair into a partial.
	tiled := []PauseInterval{
		{Start: day(0), End: ptr(day(15))},
		{Start: day(15), End: ptr(day(30))},
	}
	if o, _ := overlapForIntervals(tiled, unionWindowStart, unionWindowEnd); o != overlapFull {
		t.Errorf("two adjacent intervals tiling the window classified %v, want overlapFull", o)
	}
}

// TestNoPauseIsUntouched pins the case that must produce a byte-identical
// report section: no pause history at all, no current pause.
func TestNoPauseIsUntouched(t *testing.T) {
	overlap, unmonitored := overlapForIntervals(nil, unionWindowStart, unionWindowEnd)
	if overlap != overlapNone || unmonitored != 0 {
		t.Errorf("overlapForIntervals(nil) = %v, %v; want overlapNone, 0", overlap, unmonitored)
	}
	s := site.Site{ID: uuid.New(), Name: "never-paused", URL: "https://n.example"}
	got := normalizePauseIntervals(nil, s.MonitoringPausedAt)
	if got != nil {
		t.Errorf("normalizePauseIntervals(nil, nil) = %v, want nil — a never-paused site must gain nothing", got)
	}
}

// ---------------------------------------------------------------------------
// Invariant (b): no unbounded interval for a site that is not paused now
// ---------------------------------------------------------------------------

// TestMissingResumeDoesNotProduceUnboundedInterval is the second adversary
// fixture. One lost site.monitoring.resumed audit write leaves an interval
// with End nil; overlapForIntervals runs a nil End to the window end, so that
// single interval classified EVERY future window as fully paused and the
// site's uptime silently vanished from every report from then on. The site's
// own row says it is not paused, which is the evidence that the open interval
// is a data gap and not a state.
func TestMissingResumeDoesNotProduceUnboundedInterval(t *testing.T) {
	siteID := uuid.New()
	s := site.Site{ID: siteID, Name: "lost-resume", URL: "https://l.example"} // MonitoringPausedAt nil
	sources := Sources{
		ListClientSites: func(context.Context, uuid.UUID, uuid.UUID) ([]site.Site, error) {
			return []site.Site{s}, nil
		},
		QueryUptimeAggregateRange: func(context.Context, uuid.UUID, uuid.UUID, time.Time, time.Time) (metrics.Aggregate, error) {
			return metrics.Aggregate{Checks: 900, UpChecks: 894, UptimePct: 99.3}, nil
		},
		QueryMonitoringPauseIntervals: func(context.Context, uuid.UUID, uuid.UUID, time.Time, time.Time) ([]PauseInterval, error) {
			// A pause that opened long before the window and never recorded a
			// resume, exactly as the audit reconstruction emitted it.
			return []PauseInterval{{Start: day(-40)}}, nil
		},
	}
	rd, err := BuildReportData(context.Background(), sources, BuildInput{
		TenantID: uuid.New(), ClientID: uuid.New(), Client: ClientInfo{Name: "Acme"}, AgencyName: "Agency",
		PeriodStart: unionWindowStart, PeriodEnd: unionWindowEnd,
	})
	if err != nil {
		t.Fatalf("BuildReportData: %v", err)
	}
	sr := rd.Sites[0]
	if sr.Uptime == nil {
		t.Fatal("uptime section suppressed: an open-ended interval for a site that is NOT currently paused classified the whole window as paused")
	}
	if sr.Uptime.UptimePct != 99.3 {
		t.Errorf("UptimePct = %v, want 99.3", sr.Uptime.UptimePct)
	}
	if rd.Totals.UptimeSiteCount != 1 {
		t.Errorf("UptimeSiteCount = %d, want 1", rd.Totals.UptimeSiteCount)
	}
}

// TestCurrentlyPausedSiteKeepsItsOpenInterval is the guard against
// over-correcting: when the site IS paused right now, the trailing open
// interval is legitimate and a full-window pause must still suppress.
func TestCurrentlyPausedSiteKeepsItsOpenInterval(t *testing.T) {
	pausedAt := day(-3)
	got := normalizePauseIntervals([]PauseInterval{{Start: pausedAt}}, &pausedAt)
	if len(got) != 1 || got[0].End != nil {
		t.Fatalf("normalize closed the interval of a site that IS paused now: %+v", got)
	}
	if o, _ := overlapForIntervals(got, unionWindowStart, unionWindowEnd); o != overlapFull {
		t.Errorf("overlap = %v, want overlapFull — this site really was paused for the whole window", o)
	}
}

// TestTruncatedHistoryStillSeesTheCurrentPause: when the audit read reaches
// back far enough to show older, closed pauses but not the currently-open one,
// the current pause is merged in. Under the old sum this could not be done
// without risking a false full; under a union it is free.
func TestCurrentPauseMergedWhenHistoryMissesIt(t *testing.T) {
	pausedAt := day(20)
	history := []PauseInterval{{Start: day(2), End: ptr(day(4))}}
	got := normalizePauseIntervals(history, &pausedAt)
	if len(got) != 2 {
		t.Fatalf("want the current pause appended, got %+v", got)
	}
	overlap, unmonitored := overlapForIntervals(got, unionWindowStart, unionWindowEnd)
	if overlap != overlapPartial {
		t.Errorf("overlap = %v, want overlapPartial", overlap)
	}
	// 2 days of closed pause + 10 days from day 20 to the window end.
	if want := 12 * 24 * time.Hour; unmonitored != want {
		t.Errorf("unmonitored = %v, want %v", unmonitored, want)
	}
}

// TestDuplicateIntervalsAreNotDoubleCounted: merging the current pause with an
// audit-derived interval describing the SAME pause must be idempotent. This is
// the double-counting the old code refused to merge in order to avoid, and the
// union makes it a no-op.
func TestDuplicateIntervalsAreNotDoubleCounted(t *testing.T) {
	pausedAt := day(20)
	got := normalizePauseIntervals([]PauseInterval{{Start: pausedAt}}, &pausedAt)
	_, unmonitored := overlapForIntervals(got, unionWindowStart, unionWindowEnd)
	if want := 10 * 24 * time.Hour; unmonitored != want {
		t.Errorf("unmonitored = %v, want %v — the same pause counted twice", unmonitored, want)
	}
}

// ---------------------------------------------------------------------------
// Invariant (c): a failed history read is qualified, never confidently covered
// ---------------------------------------------------------------------------

// TestFailedPauseHistoryQualifiesTheSection. Returning nil on error rendered
// the window as a complete, fully-covered month on the strength of an error —
// a stronger claim than the data supports. Suppressing it would discard real
// checks. The section is kept and marked.
func TestFailedPauseHistoryQualifiesTheSection(t *testing.T) {
	s := site.Site{ID: uuid.New(), Name: "history-unreadable", URL: "https://h.example"}
	sources := Sources{
		ListClientSites: func(context.Context, uuid.UUID, uuid.UUID) ([]site.Site, error) {
			return []site.Site{s}, nil
		},
		QueryUptimeAggregateRange: func(context.Context, uuid.UUID, uuid.UUID, time.Time, time.Time) (metrics.Aggregate, error) {
			return metrics.Aggregate{Checks: 800, UpChecks: 799, UptimePct: 99.9}, nil
		},
		QueryMonitoringPauseIntervals: func(context.Context, uuid.UUID, uuid.UUID, time.Time, time.Time) ([]PauseInterval, error) {
			return nil, errors.New("audit read failed")
		},
	}
	rd, err := BuildReportData(context.Background(), sources, BuildInput{
		TenantID: uuid.New(), ClientID: uuid.New(), Client: ClientInfo{Name: "Acme"}, AgencyName: "Agency",
		PeriodStart: unionWindowStart, PeriodEnd: unionWindowEnd,
	})
	if err != nil {
		t.Fatalf("BuildReportData: %v", err)
	}
	sr := rd.Sites[0]
	if sr.Uptime == nil {
		t.Fatal("uptime section suppressed by a FAILED history read — real checks discarded")
	}
	if !sr.Uptime.CoverageUnknown {
		t.Error("CoverageUnknown = false: a failed pause-history read renders as a confidently complete month")
	}
	if sr.Uptime.PartialCoverage {
		t.Error("PartialCoverage must stay false — no pause of known size was measured")
	}
}

// TestSuccessfulHistoryIsNotQualified is the over-fire guard for the above: a
// healthy read must never set CoverageUnknown.
func TestSuccessfulHistoryIsNotQualified(t *testing.T) {
	s := site.Site{ID: uuid.New(), Name: "healthy", URL: "https://ok.example"}
	sources := Sources{
		ListClientSites: func(context.Context, uuid.UUID, uuid.UUID) ([]site.Site, error) {
			return []site.Site{s}, nil
		},
		QueryUptimeAggregateRange: func(context.Context, uuid.UUID, uuid.UUID, time.Time, time.Time) (metrics.Aggregate, error) {
			return metrics.Aggregate{Checks: 800, UpChecks: 800, UptimePct: 100}, nil
		},
		QueryMonitoringPauseIntervals: func(context.Context, uuid.UUID, uuid.UUID, time.Time, time.Time) ([]PauseInterval, error) {
			return nil, nil
		},
	}
	rd, err := BuildReportData(context.Background(), sources, BuildInput{
		TenantID: uuid.New(), ClientID: uuid.New(), Client: ClientInfo{Name: "Acme"}, AgencyName: "Agency",
		PeriodStart: unionWindowStart, PeriodEnd: unionWindowEnd,
	})
	if err != nil {
		t.Fatalf("BuildReportData: %v", err)
	}
	u := rd.Sites[0].Uptime
	if u == nil {
		t.Fatal("uptime section missing")
	}
	if u.CoverageUnknown || u.PartialCoverage {
		t.Errorf("a clean read produced CoverageUnknown=%v PartialCoverage=%v; want both false", u.CoverageUnknown, u.PartialCoverage)
	}
}

// ---------------------------------------------------------------------------
// The reconstruction itself (the logic lifted out of cmd/wpmgr/main.go)
// ---------------------------------------------------------------------------

// TestReconstructionClosesAStalePauseAtItsOwnStart. The production source of
// the overlapping intervals above: a paused event arriving while one was
// already open appended the open interval UNCHANGED, End still nil, so it ran
// to the window end and subsumed every later interval — despite the code's own
// comment claiming it closed it.
func TestReconstructionClosesAStalePauseAtItsOwnStart(t *testing.T) {
	events := []PauseEvent{
		{At: day(0), Paused: true},
		// The resume for the pause above was lost.
		{At: day(2), Paused: true},
		{At: day(18)},
	}
	got := PauseIntervalsFromEvents(events, unionWindowStart)
	for i, iv := range got {
		if iv.End == nil {
			t.Fatalf("interval %d is unbounded: %+v — one lost resume poisons every future window", i, iv)
		}
	}
	overlap, unmonitored := overlapForIntervals(got, unionWindowStart, unionWindowEnd)
	if overlap != overlapPartial {
		t.Errorf("overlap = %v, want overlapPartial", overlap)
	}
	// Only [day2, day18) is honestly known to be paused; the stale pause is
	// closed at its own start rather than swallowing the period before it.
	if want := 16 * 24 * time.Hour; unmonitored != want {
		t.Errorf("unmonitored = %v, want %v", unmonitored, want)
	}
}

// TestReconstructionKeepsAnOrphanedResume. A history read that stops between a
// paused row and its resumed row used to drop the resume on the floor
// (`open == nil`), so the interval vanished and the report claimed coverage it
// did not have. The pause is instead taken to have been open since the window
// began.
func TestReconstructionKeepsAnOrphanedResume(t *testing.T) {
	got := PauseIntervalsFromEvents([]PauseEvent{{At: day(6)}}, unionWindowStart)
	if len(got) != 1 {
		t.Fatalf("orphaned resume produced %d intervals, want 1: %+v", len(got), got)
	}
	if got[0].End == nil || !got[0].End.Equal(day(6)) || !got[0].Start.Equal(unionWindowStart) {
		t.Errorf("interval = %+v, want [windowStart, day6)", got[0])
	}
	_, unmonitored := overlapForIntervals(got, unionWindowStart, unionWindowEnd)
	if want := 6 * 24 * time.Hour; unmonitored != want {
		t.Errorf("unmonitored = %v, want %v", unmonitored, want)
	}
}

// TestReconstructionOrdinaryPairing is the over-fire guard: an ordinary,
// complete paused/resumed history still reconstructs exactly, and events
// arriving newest-first (as ListFiltered returns them) are handled.
func TestReconstructionOrdinaryPairing(t *testing.T) {
	newestFirst := []PauseEvent{
		{At: day(12)},
		{At: day(10), Paused: true},
		{At: day(4)},
		{At: day(1), Paused: true},
	}
	got := PauseIntervalsFromEvents(newestFirst, unionWindowStart)
	if len(got) != 2 {
		t.Fatalf("want 2 intervals, got %d: %+v", len(got), got)
	}
	if !got[0].Start.Equal(day(1)) || !got[0].End.Equal(day(4)) {
		t.Errorf("interval 0 = %+v, want [day1, day4)", got[0])
	}
	if !got[1].Start.Equal(day(10)) || !got[1].End.Equal(day(12)) {
		t.Errorf("interval 1 = %+v, want [day10, day12)", got[1])
	}
	_, unmonitored := overlapForIntervals(got, unionWindowStart, unionWindowEnd)
	if want := 5 * 24 * time.Hour; unmonitored != want {
		t.Errorf("unmonitored = %v, want %v", unmonitored, want)
	}
}

// TestReconstructionStillOpenPauseStaysOpen: a genuinely current pause is the
// one case an unbounded interval is correct at this layer. Closing it here
// would hide a live pause; the aggregator, which knows the site's current
// state, is the layer that decides.
func TestReconstructionStillOpenPauseStaysOpen(t *testing.T) {
	got := PauseIntervalsFromEvents([]PauseEvent{{At: day(20), Paused: true}}, unionWindowStart)
	if len(got) != 1 || got[0].End != nil {
		t.Fatalf("a still-open pause must stay open at this layer: %+v", got)
	}
}
