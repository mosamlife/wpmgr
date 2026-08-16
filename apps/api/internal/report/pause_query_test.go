package report

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
)

// fakeAuditLister is an in-memory stand-in for *audit.Recorder that applies
// the same half-open CreatedFrom/CreatedTo filtering and newest-first
// ordering db/query/audit_log.sql's ListAuditEntriesFiltered does, so a test
// against it exercises the real boundary behaviour rather than a canned
// per-call response.
type fakeAuditLister struct {
	entries []audit.Entry
	calls   []audit.Filter
}

func (f *fakeAuditLister) ListFiltered(_ context.Context, _ uuid.UUID, filt audit.Filter, limit, offset int32) ([]audit.Entry, error) {
	f.calls = append(f.calls, filt)
	var matched []audit.Entry
	for _, e := range f.entries {
		if filt.CreatedFrom != nil && e.CreatedAt.Before(*filt.CreatedFrom) {
			continue
		}
		if filt.CreatedTo != nil && !e.CreatedAt.Before(*filt.CreatedTo) {
			continue
		}
		matched = append(matched, e)
	}
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].CreatedAt.After(matched[j].CreatedAt) })
	start := int(offset)
	if start > len(matched) {
		start = len(matched)
	}
	end := start + int(limit)
	if end > len(matched) {
		end = len(matched)
	}
	return matched[start:end], nil
}

func pausedAt(at time.Time) audit.Entry {
	return audit.Entry{Action: audit.ActionSiteMonitoringPaused, CreatedAt: at}
}

func resumedAt(at time.Time) audit.Entry {
	return audit.Entry{Action: audit.ActionSiteMonitoringResumed, CreatedAt: at}
}

// TestQueryMonitoringPauseIntervalsFromAudit_CarryInNeverResumed is the case
// that motivates the whole two-read design: a pause opened BEFORE the window
// and never resumed writes no row inside [from, to) at all. A window-only
// read (what a naive "just add CreatedFrom/CreatedTo to the one query"
// version would do) reconstructs no interval and the report claims full
// coverage for a period the site was demonstrably dark. Only the carry-in
// read can see it.
func TestQueryMonitoringPauseIntervalsFromAudit_CarryInNeverResumed(t *testing.T) {
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 30)

	lister := &fakeAuditLister{entries: []audit.Entry{
		pausedAt(from.AddDate(0, 0, -10)), // opened 10 days before the window, never resumed
	}}

	got, err := QueryMonitoringPauseIntervalsFromAudit(lister)(context.Background(), uuid.New(), uuid.New(), from, to)
	if err != nil {
		t.Fatalf("QueryMonitoringPauseIntervalsFromAudit: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("intervals = %v, want exactly 1 reconstructed from the carry-in row", got)
	}
	if got[0].End != nil {
		t.Fatalf("interval end = %v, want nil (never resumed, still open)", *got[0].End)
	}

	overlap, unmonitored := overlapForIntervals(got, from, to)
	if overlap != overlapFull {
		t.Fatalf("overlap = %v, want overlapFull — the whole window falls inside a pause that opened before it and was never resumed", overlap)
	}
	if want := to.Sub(from); unmonitored != want {
		t.Fatalf("unmonitored = %v, want the full window %v", unmonitored, want)
	}
}

// TestQueryMonitoringPauseIntervalsFromAudit_BoundaryCountedOnce pins the
// half-open contract between the window read ([from, to)) and the carry-in
// read ((-inf, from)): an event stamped exactly at `from` must be counted by
// the window read and must NOT also surface from the carry-in read (which
// would double it) and must not be dropped by either (which would lose it).
func TestQueryMonitoringPauseIntervalsFromAudit_BoundaryCountedOnce(t *testing.T) {
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 30)

	lister := &fakeAuditLister{entries: []audit.Entry{
		pausedAt(from), // stamped exactly at the window start
	}}

	events, err := monitoringPauseEvents(context.Background(), lister, uuid.New(), uuid.New(), from, to)
	if err != nil {
		t.Fatalf("monitoringPauseEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %v (len %d), want exactly 1 — the from-stamped row must be counted once, not by both reads and not by neither", events, len(events))
	}
	if !events[0].At.Equal(from) || !events[0].Paused {
		t.Fatalf("event = %+v, want the paused row stamped at `from`", events[0])
	}

	// Confirm it was the window read that picked it up, not the carry-in
	// read silently including a boundary row it should exclude.
	got, err := QueryMonitoringPauseIntervalsFromAudit(lister)(context.Background(), uuid.New(), uuid.New(), from, to)
	if err != nil {
		t.Fatalf("QueryMonitoringPauseIntervalsFromAudit: %v", err)
	}
	if len(got) != 1 || got[0].End != nil || !got[0].Start.Equal(from) {
		t.Fatalf("intervals = %v, want exactly one open interval starting at `from`", got)
	}
}

// TestQueryMonitoringPauseIntervalsFromAudit_NoHistory is the over-fire
// control: a site with no monitoring-pause audit rows at all must produce the
// same result it does today — no intervals, no false pause.
func TestQueryMonitoringPauseIntervalsFromAudit_NoHistory(t *testing.T) {
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 30)

	lister := &fakeAuditLister{} // no entries at all

	got, err := QueryMonitoringPauseIntervalsFromAudit(lister)(context.Background(), uuid.New(), uuid.New(), from, to)
	if err != nil {
		t.Fatalf("QueryMonitoringPauseIntervalsFromAudit: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("intervals = %v, want none for a site with no pause history", got)
	}
	overlap, _ := overlapForIntervals(got, from, to)
	if overlap != overlapNone {
		t.Fatalf("overlap = %v, want overlapNone", overlap)
	}

	// Exactly two reads: the window and the single carry-in row. Never the
	// old 25-page backward scan.
	if len(lister.calls) != 2 {
		t.Fatalf("ListFiltered called %d times, want exactly 2 (window + carry-in)", len(lister.calls))
	}
}

// TestQueryMonitoringPauseIntervalsFromAudit_WindowFullyInsideLongPause is
// the second over-fire control: a pause that opened before the window AND
// was resumed after it closed must still classify the window as fully
// paused, even though NEITHER edge of the pause falls inside [from, to) —
// the window read alone returns zero rows, and correctness rests entirely on
// the carry-in read.
func TestQueryMonitoringPauseIntervalsFromAudit_WindowFullyInsideLongPause(t *testing.T) {
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 30)

	lister := &fakeAuditLister{entries: []audit.Entry{
		pausedAt(from.AddDate(0, 0, -90)),
		resumedAt(to.AddDate(0, 0, 90)),
	}}

	got, err := QueryMonitoringPauseIntervalsFromAudit(lister)(context.Background(), uuid.New(), uuid.New(), from, to)
	if err != nil {
		t.Fatalf("QueryMonitoringPauseIntervalsFromAudit: %v", err)
	}
	overlap, unmonitored := overlapForIntervals(got, from, to)
	if overlap != overlapFull {
		t.Fatalf("overlap = %v, want overlapFull — window sits entirely inside a pause spanning well before and after it", overlap)
	}
	if want := to.Sub(from); unmonitored != want {
		t.Fatalf("unmonitored = %v, want the full window %v", unmonitored, want)
	}
}
