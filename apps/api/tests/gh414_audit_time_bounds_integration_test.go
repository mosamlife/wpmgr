package tests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// GH #414 — created_at bounds on ListAuditEntriesFiltered.
//
// Every assertion below goes through audit.Recorder (the production type) on
// the pool startPostgres returns, which connects as the NON-superuser,
// NOBYPASSRLS wpmgr_app role, so ListFiltered's InTenantTx and the audit_log
// RLS policies are live for all of it. No test-owned connection, no superuser.
//
// The defect these bound: the monthly report reads the audit trail to decide
// whether a site's monitoring was paused during the window it reports on. The
// first version took the newest 200 site.monitoring.* rows and ignored the
// window; a site paused nightly reaches 200 rows in about three months, after
// which an older report reconstructs no pause and prints the uptime section as
// fully covered for a period the site was demonstrably paused.
//
// The subtle half is the LOWER bound. A plain created_at >= from drops exactly
// the interval that matters most: a pause that OPENED BEFORE the window and was
// never resumed writes no row inside the window at all. TestAuditTimeBounds_
// CarryInReadSeesPauseOpenedBeforeWindow is that case.

// auditAt records one entry stamped at exactly `at`, by handing the Recorder a
// FixedClock. Entries must be recorded in chronological order: the hash chain
// seeds from GetLastAuditHash, which is ordered by created_at DESC.
func auditAt(t *testing.T, pool *db.Pool, tenant uuid.UUID, at time.Time, action, targetID string) audit.Entry {
	t.Helper()
	rec := audit.NewRecorder(pool, domain.FixedClock{T: at})
	e, err := rec.Record(context.Background(), audit.Event{
		TenantID:   tenant,
		ActorType:  audit.ActorSystem,
		Action:     action,
		TargetType: "site",
		TargetID:   targetID,
	})
	if err != nil {
		t.Fatalf("record %s at %s: %v", action, at.Format(time.RFC3339), err)
	}
	return e
}

// TestAuditTimeBounds_WindowReadIsHalfOpen proves the range predicate itself:
// created_at >= CreatedFrom and created_at < CreatedTo. Half-open is
// load-bearing rather than stylistic — it is what makes the window read and the
// carry-in read below non-overlapping, so a row sitting exactly on the boundary
// is counted once and only once.
func TestAuditTimeBounds_WindowReadIsHalfOpen(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	rec := audit.NewRecorder(pool, domain.SystemClock{})

	tenant := seedTenant(t, pool, "gh414-audit-bounds-halfopen")
	siteID := uuid.New()

	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	before := auditAt(t, pool, tenant, from.Add(-24*time.Hour), audit.ActionSiteMonitoringPaused, siteID.String())
	atFrom := auditAt(t, pool, tenant, from, audit.ActionSiteMonitoringResumed, siteID.String())
	inside := auditAt(t, pool, tenant, from.Add(10*24*time.Hour), audit.ActionSiteMonitoringPaused, siteID.String())
	atTo := auditAt(t, pool, tenant, to, audit.ActionSiteMonitoringResumed, siteID.String())
	after := auditAt(t, pool, tenant, to.Add(24*time.Hour), audit.ActionSiteMonitoringPaused, siteID.String())

	got, err := rec.ListFiltered(ctx, tenant, audit.Filter{
		ActionPrefix: "site.monitoring.",
		SiteID:       &siteID,
		CreatedFrom:  &from,
		CreatedTo:    &to,
	}, 200, 0)
	if err != nil {
		t.Fatalf("bounded ListFiltered errored: %v", err)
	}

	ids := map[uuid.UUID]bool{}
	for _, e := range got {
		ids[e.ID] = true
	}

	// Lower bound is INCLUSIVE: the row stamped exactly at `from` is in.
	if !ids[atFrom.ID] {
		t.Errorf("row at exactly CreatedFrom was excluded; the lower bound must be >= (inclusive), not >")
	}
	if !ids[inside.ID] {
		t.Errorf("row inside the window was excluded")
	}
	// Upper bound is EXCLUSIVE: the row stamped exactly at `to` is out.
	if ids[atTo.ID] {
		t.Errorf("row at exactly CreatedTo was included; the upper bound must be < (exclusive), not <=. "+
			"An inclusive upper bound double-counts this row against the carry-in read of the NEXT window. id=%s", atTo.ID)
	}
	if ids[before.ID] {
		t.Errorf("row before the window was included by a bounded read")
	}
	if ids[after.ID] {
		t.Errorf("row after the window was included by a bounded read")
	}
	if len(got) != 2 {
		t.Errorf("bounded read returned %d rows, want 2 (the row at `from` and the row inside)", len(got))
	}
}

// TestAuditTimeBounds_CarryInReadSeesPauseOpenedBeforeWindow is the case the
// brief calls out as the one that matters most, and the one a plain
// created_at >= from silently loses.
//
// The site was paused BEFORE the reporting window opened and never resumed. It
// was therefore paused for the ENTIRE window, and the audit trail contains not
// one row inside the window saying so. A window-only read reconstructs no pause
// and the report claims full uptime coverage for a month the site was dark.
//
// The carry-in read — no lower bound, upper bound = the window start, limit 1 —
// is the read that recovers it, and it is the same query with different bounds,
// not a second query.
func TestAuditTimeBounds_CarryInReadSeesPauseOpenedBeforeWindow(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	rec := audit.NewRecorder(pool, domain.SystemClock{})

	tenant := seedTenant(t, pool, "gh414-audit-bounds-carryin")
	siteID := uuid.New()

	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	// A long-closed pause/resume pair, then the pause that is still open when
	// the window opens. The carry-in must return the NEWEST row before `from`,
	// which is the still-open pause, not the older resume.
	auditAt(t, pool, tenant, from.Add(-90*24*time.Hour), audit.ActionSiteMonitoringPaused, siteID.String())
	auditAt(t, pool, tenant, from.Add(-89*24*time.Hour), audit.ActionSiteMonitoringResumed, siteID.String())
	openPause := auditAt(t, pool, tenant, from.Add(-45*24*time.Hour), audit.ActionSiteMonitoringPaused, siteID.String())

	// The window itself is empty of monitoring rows — that is the whole point.
	windowRows, err := rec.ListFiltered(ctx, tenant, audit.Filter{
		ActionPrefix: "site.monitoring.",
		SiteID:       &siteID,
		CreatedFrom:  &from,
		CreatedTo:    &to,
	}, 200, 0)
	if err != nil {
		t.Fatalf("window read errored: %v", err)
	}
	if len(windowRows) != 0 {
		t.Fatalf("precondition broken: window read returned %d rows, want 0", len(windowRows))
	}

	// Carry-in: unbounded below, strictly before the window, newest first,
	// limit 1. This is the read that must not come back empty.
	carry, err := rec.ListFiltered(ctx, tenant, audit.Filter{
		ActionPrefix: "site.monitoring.",
		SiteID:       &siteID,
		CreatedTo:    &from,
	}, 1, 0)
	if err != nil {
		t.Fatalf("carry-in read errored: %v", err)
	}
	if len(carry) != 1 {
		t.Fatalf("carry-in read returned %d rows, want 1. Without it the report reconstructs no pause "+
			"and prints the uptime section as fully covered for a window the site was paused for all of.", len(carry))
	}
	if carry[0].ID != openPause.ID {
		t.Errorf("carry-in returned the wrong row: got %s, want the newest pre-window row %s (the still-open pause)",
			carry[0].ID, openPause.ID)
	}
	if carry[0].Action != audit.ActionSiteMonitoringPaused {
		t.Errorf("carry-in row action = %q, want %q — the window opened with monitoring PAUSED",
			carry[0].Action, audit.ActionSiteMonitoringPaused)
	}

	// The two reads must not overlap: nothing the carry-in returned may also
	// appear in the window read. (Vacuous here since the window is empty, but
	// it is the invariant the caller depends on, so assert it explicitly.)
	for _, w := range windowRows {
		if w.ID == carry[0].ID {
			t.Errorf("row %s appeared in BOTH the window read and the carry-in read", w.ID)
		}
	}
}

// TestAuditTimeBounds_NoBoundsIsUnchanged is the over-fire control. Every
// pre-existing ListFiltered caller (internal/audit/handler.go, and the report
// closure before it is converted) passes a Filter with no time bounds. Such a
// call must return exactly what it returned before the bounds existed: the nil
// *time.Time maps to a zero pgtype.Timestamptz (Valid:false -> SQL NULL), and
// the query's `... IS NULL OR ...` short-circuits the predicate away.
//
// A guard that reddens correct work gets switched off, so this asserts the
// honest case the new predicate must NOT block.
func TestAuditTimeBounds_NoBoundsIsUnchanged(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	rec := audit.NewRecorder(pool, domain.SystemClock{})

	tenant := seedTenant(t, pool, "gh414-audit-bounds-nobounds")
	siteID := uuid.New()

	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	var want []uuid.UUID
	for i := range 6 {
		action := audit.ActionSiteMonitoringPaused
		if i%2 == 1 {
			action = audit.ActionSiteMonitoringResumed
		}
		e := auditAt(t, pool, tenant, base.Add(time.Duration(i*24)*time.Hour), action, siteID.String())
		want = append(want, e.ID)
	}

	// Spread over 5 days from `base`, so no single bounded window used
	// elsewhere in this file could account for all of them.
	unbounded, err := rec.ListFiltered(ctx, tenant, audit.Filter{
		ActionPrefix: "site.monitoring.",
		SiteID:       &siteID,
	}, 200, 0)
	if err != nil {
		t.Fatalf("unbounded ListFiltered errored: %v", err)
	}
	if len(unbounded) != len(want) {
		t.Fatalf("unbounded ListFiltered returned %d rows, want %d — a no-bounds call must behave "+
			"exactly as it did before created_at bounds existed", len(unbounded), len(want))
	}

	// Newest-first ordering is part of the contract and must survive too.
	for i := 1; i < len(unbounded); i++ {
		if unbounded[i-1].CreatedAt.Before(unbounded[i].CreatedAt) {
			t.Fatalf("unbounded ListFiltered lost its newest-first ordering at index %d", i)
		}
	}

	// Explicitly NULL-ing only one end must also leave the other end open.
	onlyUpper, err := rec.ListFiltered(ctx, tenant, audit.Filter{
		ActionPrefix: "site.monitoring.",
		SiteID:       &siteID,
		CreatedTo:    ptrTime(base.Add(200 * 24 * time.Hour)),
	}, 200, 0)
	if err != nil {
		t.Fatalf("upper-bound-only ListFiltered errored: %v", err)
	}
	if len(onlyUpper) != len(want) {
		t.Errorf("upper-bound-only read returned %d rows, want %d — a nil CreatedFrom must leave the "+
			"lower end unbounded, not clamp it", len(onlyUpper), len(want))
	}

	onlyLower, err := rec.ListFiltered(ctx, tenant, audit.Filter{
		ActionPrefix: "site.monitoring.",
		SiteID:       &siteID,
		CreatedFrom:  ptrTime(base.Add(-200 * 24 * time.Hour)),
	}, 200, 0)
	if err != nil {
		t.Fatalf("lower-bound-only ListFiltered errored: %v", err)
	}
	if len(onlyLower) != len(want) {
		t.Errorf("lower-bound-only read returned %d rows, want %d — a nil CreatedTo must leave the "+
			"upper end unbounded, not clamp it", len(onlyLower), len(want))
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
