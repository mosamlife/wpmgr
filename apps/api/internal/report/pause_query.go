package report

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
)

// AuditPauseLister is the one audit-trail read
// QueryMonitoringPauseIntervalsFromAudit needs. *audit.Recorder satisfies it
// via ListFiltered — see cmd/wpmgr/main.go's wiring of
// Sources.QueryMonitoringPauseIntervals.
type AuditPauseLister interface {
	ListFiltered(ctx context.Context, tenantID uuid.UUID, f audit.Filter, limit, offset int32) ([]audit.Entry, error)
}

// monitoringActionPrefix scopes ListFiltered to the two site.monitoring.*
// actions PauseIntervalsFromEvents understands: audit.ActionSiteMonitoringPaused
// and audit.ActionSiteMonitoringResumed.
const monitoringActionPrefix = "site.monitoring."

// monitoringPauseWindowPageSize/MaxPages bound the window read below.
// maxPeriodDays (service.go) caps a report window at 92 days; at 200
// events/page the loop only runs more than once if a site logged over 100
// pause/resume cycles inside that window — more than one full cycle per day,
// sustained for the entire period. Ordinary use (a nightly maintenance
// toggle) is 2 events/day, ~184 over 92 days: one page. The cap exists for a
// flapping or scripted toggle, not routine operation; it is not needed at all
// for the carry-in read below, which only ever wants the single newest row.
const (
	monitoringPauseWindowPageSize = 200
	monitoringPauseWindowMaxPages = 5
)

// QueryMonitoringPauseIntervalsFromAudit builds a
// Sources.QueryMonitoringPauseIntervals implementation over lister. It
// replaces paging backward through a tenant's whole audit history with two
// bounded, non-overlapping reads per call (GH #414 follow-up):
//
//	window read : CreatedFrom=&from, CreatedTo=&to   -> [from, to), the
//	              events that fall inside the reporting window itself.
//	carry-in    : CreatedFrom=nil,   CreatedTo=&from, limit 1 -> the newest
//	              monitoring event strictly before the window, i.e. the state
//	              the window opened in.
//
// The carry-in read is not an optimization, it is required for correctness: a
// pause that opened BEFORE the window and was never resumed writes no row
// inside [from, to) at all, so a window-only read reconstructs no pause and
// the report would claim full coverage for a period the site was
// demonstrably dark. See db/query/audit_log.sql's ListAuditEntriesFiltered
// doc, which makes the same argument from the query side.
//
// The two reads cannot double-count a boundary row: the window read's lower
// bound and the carry-in read's upper bound are both `from`, and the
// underlying query is half-open on both ends ([from, to) and (-inf, from)),
// so a row stamped exactly at `from` lands in the window read only.
//
// A carried-in `resumed` row needs no special case: PauseIntervalsFromEvents
// already drops a resume with no matching open pause once
// resumedAt.After(windowStart) is false, and the carry-in row's CreatedAt is
// always strictly before `from` by construction, so that condition is always
// false for it — it is dropped exactly as if it had never been read.
func QueryMonitoringPauseIntervalsFromAudit(lister AuditPauseLister) func(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time) ([]PauseInterval, error) {
	return func(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time) ([]PauseInterval, error) {
		events, err := monitoringPauseEvents(ctx, lister, tenantID, siteID, from, to)
		if err != nil {
			return nil, err
		}
		return PauseIntervalsFromEvents(events, from), nil
	}
}

// monitoringPauseEvents performs the window read (paged, see
// monitoringPauseWindowMaxPages) and the single carry-in read, and reduces
// both to PauseEvent for PauseIntervalsFromEvents.
func monitoringPauseEvents(ctx context.Context, lister AuditPauseLister, tenantID, siteID uuid.UUID, from, to time.Time) ([]PauseEvent, error) {
	var events []PauseEvent
	for page := int32(0); page < monitoringPauseWindowMaxPages; page++ {
		batch, err := lister.ListFiltered(ctx, tenantID, audit.Filter{
			ActionPrefix: monitoringActionPrefix,
			SiteID:       &siteID,
			CreatedFrom:  &from,
			CreatedTo:    &to,
		}, monitoringPauseWindowPageSize, page*monitoringPauseWindowPageSize)
		if err != nil {
			return nil, err
		}
		events = appendPauseEvents(events, batch)
		if int32(len(batch)) < monitoringPauseWindowPageSize {
			break
		}
	}

	// Carry-in: the newest monitoring event strictly before the window,
	// limit 1. See the doc above for why this second read exists.
	carryIn, err := lister.ListFiltered(ctx, tenantID, audit.Filter{
		ActionPrefix: monitoringActionPrefix,
		SiteID:       &siteID,
		CreatedTo:    &from,
	}, 1, 0)
	if err != nil {
		return nil, err
	}
	events = appendPauseEvents(events, carryIn)
	return events, nil
}

// appendPauseEvents reduces a batch of audit entries to the PauseEvents
// PauseIntervalsFromEvents understands, ignoring any action outside the two
// this package recognizes (ListFiltered's ActionPrefix already restricts the
// batch to "site.monitoring.*", but a third action under that prefix is not
// this function's business to interpret).
func appendPauseEvents(events []PauseEvent, batch []audit.Entry) []PauseEvent {
	for _, e := range batch {
		switch e.Action {
		case audit.ActionSiteMonitoringPaused:
			events = append(events, PauseEvent{At: e.CreatedAt, Paused: true})
		case audit.ActionSiteMonitoringResumed:
			events = append(events, PauseEvent{At: e.CreatedAt})
		}
	}
	return events
}
