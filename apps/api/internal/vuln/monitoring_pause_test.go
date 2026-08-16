package vuln_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/vuln"
)

// ---------------------------------------------------------------------------
// m117 (GH #414) phase 3: vuln_rescan_site point-of-action pause re-check.
//
// The rescan fan-out filters paused sites out of its enumeration, but nothing
// drains River, so a job queued before the pause must ask again when it runs.
// The gate is RescanSiteArgs.Scheduled: an operator's "rescan now" is never
// gated.
// ---------------------------------------------------------------------------

// pauseLoader is a SiteLoader that records whether the rescan actually reached
// the database. GetSiteForVuln is the FIRST thing Service.RescanSite does, so
// "was it called" is exactly "did the rescan take action".
type pauseLoader struct {
	called   int
	siteSeen uuid.UUID
}

func (l *pauseLoader) GetSiteForVuln(_ context.Context, _, siteID uuid.UUID) (vuln.SiteSnapshot, error) {
	l.called++
	l.siteSeen = siteID
	// A domain error is fine: the assertion is on reaching this call at all.
	return vuln.SiteSnapshot{}, errors.New("pauseLoader: site load stopped here on purpose")
}

// fakePauseChecker answers the pause question from a fixed map.
type fakePauseChecker struct {
	paused map[uuid.UUID]bool
	err    error
	asked  int
}

func (c *fakePauseChecker) IsMonitoringPaused(_ context.Context, siteID uuid.UUID) (bool, error) {
	c.asked++
	if c.err != nil {
		return false, c.err
	}
	return c.paused[siteID], nil
}

func newPauseWorker(t *testing.T, loader *pauseLoader, checker *fakePauseChecker) *vuln.RescanSiteWorker {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := vuln.NewService(nil, nil, loader, nil, nil, logger)
	w := vuln.NewRescanSiteWorker(svc, logger)
	w.SetMonitoringPauseChecker(checker)
	return w
}

func rescanJob(args vuln.RescanSiteArgs) *river.Job[vuln.RescanSiteArgs] {
	return &river.Job[vuln.RescanSiteArgs]{Args: args}
}

// A scheduled job that was queued before the pause takes NO action after it.
func TestQueuedScheduledRescanTakesNoActionAfterAPause(t *testing.T) {
	paused := uuid.New()
	loader := &pauseLoader{}
	checker := &fakePauseChecker{paused: map[uuid.UUID]bool{paused: true}}
	w := newPauseWorker(t, loader, checker)

	err := w.Work(context.Background(), rescanJob(vuln.RescanSiteArgs{
		TenantID: uuid.New(), SiteID: paused, Scheduled: true,
	}))
	if err != nil {
		t.Fatalf("a skipped rescan must succeed, not fail the job: %v", err)
	}
	if loader.called != 0 {
		t.Fatalf("a paused site's queued scheduled rescan must take no action, but the rescan ran (%d loads)", loader.called)
	}
	if checker.asked != 1 {
		t.Fatalf("the scheduled job must re-check the pause exactly once, asked %d", checker.asked)
	}
}

// The active sibling in the same wave is not skipped.
func TestActiveSiblingStillRescansInTheSameWave(t *testing.T) {
	pausedID, activeID := uuid.New(), uuid.New()
	loader := &pauseLoader{}
	checker := &fakePauseChecker{paused: map[uuid.UUID]bool{pausedID: true}}
	w := newPauseWorker(t, loader, checker)
	tenant := uuid.New()

	_ = w.Work(context.Background(), rescanJob(vuln.RescanSiteArgs{TenantID: tenant, SiteID: pausedID, Scheduled: true}))
	_ = w.Work(context.Background(), rescanJob(vuln.RescanSiteArgs{TenantID: tenant, SiteID: activeID, Scheduled: true}))

	if loader.called != 1 {
		t.Fatalf("exactly the active sibling must rescan, got %d rescans", loader.called)
	}
	if loader.siteSeen != activeID {
		t.Fatalf("the site that rescanned must be the active one %s, got %s", activeID, loader.siteSeen)
	}
}

// A resumed site resumes: the same site, once unpaused, rescans again.
func TestResumedSiteRescansAgain(t *testing.T) {
	site := uuid.New()
	loader := &pauseLoader{}
	checker := &fakePauseChecker{paused: map[uuid.UUID]bool{site: true}}
	w := newPauseWorker(t, loader, checker)
	args := vuln.RescanSiteArgs{TenantID: uuid.New(), SiteID: site, Scheduled: true}

	_ = w.Work(context.Background(), rescanJob(args))
	if loader.called != 0 {
		t.Fatalf("precondition: the paused site must not rescan, got %d", loader.called)
	}

	checker.paused[site] = false // resume
	_ = w.Work(context.Background(), rescanJob(args))
	if loader.called != 1 {
		t.Fatalf("a resumed site must rescan again, got %d rescans after resume", loader.called)
	}
}

// A hand-triggered rescan still works on a paused site, and does not even ask.
func TestOperatorRescanStillRunsOnAPausedSite(t *testing.T) {
	site := uuid.New()
	loader := &pauseLoader{}
	checker := &fakePauseChecker{paused: map[uuid.UUID]bool{site: true}}
	w := newPauseWorker(t, loader, checker)

	// Scheduled is false — this is what handler.go's rescan route enqueues.
	_ = w.Work(context.Background(), rescanJob(vuln.RescanSiteArgs{
		TenantID: uuid.New(), SiteID: site, Scheduled: false,
	}))

	if loader.called != 1 {
		t.Fatalf("a person clicking Rescan must get a rescan on a paused site, got %d", loader.called)
	}
	if checker.asked != 0 {
		t.Fatalf("an operator rescan must not consult the pause at all, asked %d", checker.asked)
	}
}

// A pre-m117 job (no `scheduled` key in its JSON) unmarshals to Scheduled=false
// and therefore runs. The zero value must be the safe one.
func TestUnmarkedJobIsTreatedAsOperatorInitiated(t *testing.T) {
	site := uuid.New()
	loader := &pauseLoader{}
	checker := &fakePauseChecker{paused: map[uuid.UUID]bool{site: true}}
	w := newPauseWorker(t, loader, checker)

	var args vuln.RescanSiteArgs // zero value, as a pre-m117 payload decodes
	args.TenantID, args.SiteID = uuid.New(), site
	_ = w.Work(context.Background(), rescanJob(args))

	if loader.called != 1 {
		t.Fatalf("an unmarked job must run rather than be silently dropped, got %d", loader.called)
	}
}

// A pause-check failure does not skip. Rescanning is reversible (the alert
// dispatch has its own independent pause filter); dropping is not.
func TestPauseCheckFailureStillRescans(t *testing.T) {
	site := uuid.New()
	loader := &pauseLoader{}
	checker := &fakePauseChecker{err: errors.New("db down")}
	w := newPauseWorker(t, loader, checker)

	_ = w.Work(context.Background(), rescanJob(vuln.RescanSiteArgs{
		TenantID: uuid.New(), SiteID: site, Scheduled: true,
	}))

	if loader.called != 1 {
		t.Fatalf("a pause-check failure must not silently drop the rescan, got %d", loader.called)
	}
}
