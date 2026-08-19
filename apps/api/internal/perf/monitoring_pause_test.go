package perf

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// ---------------------------------------------------------------------------
// GH #493 — monitoring pause on the SCHEDULED db-clean path.
//
// db_clean deletes rows from the customer's live WordPress database. Pause
// governs the schedule, never the operator, so:
//   - the five-minute sweep must not enqueue a paused site,
//   - the dispatch worker must re-ask at fire time (a site paused in the
//     window between selection and dispatch is the case that matters),
//   - and a manual clean must still reach the agent.
//
// Every assertion below is on the AGENT CALL, not on a status: "was the
// destructive command sent" is the only question worth asking.
// ---------------------------------------------------------------------------

// cleanRecordingAgent counts db_clean commands and remembers which sites got one.
// Only DBClean is meaningful here; the rest of AgentPerfClient is satisfied by
// the embedded fakeAgent.
type cleanRecordingAgent struct {
	fakeAgent
	mu    sync.Mutex
	sites []uuid.UUID
}

func (a *cleanRecordingAgent) DBClean(_ context.Context, siteID uuid.UUID, _ string, req agentcmd.DBCleanRequest) (agentcmd.DBCleanResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sites = append(a.sites, siteID)
	return agentcmd.DBCleanResult{OK: true, JobID: req.JobID}, nil
}

func (a *cleanRecordingAgent) cleaned() []uuid.UUID {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]uuid.UUID(nil), a.sites...)
}

// pauseRepo is a fakeRepo that answers the pause question from a fixed set and
// serves a due list to the sweeper. pausedAtFire is consulted only by
// IsMonitoringPaused, which lets a test pause a site strictly between
// selection and dispatch.
type pauseRepo struct {
	fakeRepo
	mu           sync.Mutex
	due          []DueDBCleanSite
	pausedAtScan map[uuid.UUID]bool
	pausedAtFire map[uuid.UUID]bool
	scanErr      error
	fireErr      error
	advanced     []uuid.UUID
}

func (r *pauseRepo) GetDueDBCleanSites(_ context.Context, _ int) ([]DueDBCleanSite, error) {
	return r.due, nil
}

func (r *pauseRepo) PausedSiteIDs(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	if r.scanErr != nil {
		return nil, r.scanErr
	}
	out := map[uuid.UUID]bool{}
	for _, id := range ids {
		if r.pausedAtScan[id] {
			out[id] = true
		}
	}
	return out, nil
}

func (r *pauseRepo) IsMonitoringPaused(_ context.Context, id uuid.UUID) (bool, error) {
	if r.fireErr != nil {
		return false, r.fireErr
	}
	return r.pausedAtFire[id], nil
}

func (r *pauseRepo) UpdateNextDBCleanAt(_ context.Context, id uuid.UUID, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.advanced = append(r.advanced, id)
	return nil
}

// captureEnqueuer records the DBCleanArgs the sweeper hands to River.
type captureEnqueuer struct {
	args []DBCleanArgs
}

func (e *captureEnqueuer) EnqueueDBClean(_ context.Context, a DBCleanArgs) error {
	e.args = append(e.args, a)
	return nil
}

func (e *captureEnqueuer) siteIDs() []uuid.UUID {
	out := make([]uuid.UUID, 0, len(e.args))
	for _, a := range e.args {
		out = append(out, a.SiteID)
	}
	return out
}

// pauseFixture wires a perf Service with a recording agent so that "the agent
// was called" is observable, over the same repo the workers consult.
type pauseFixture struct {
	svc      *Service
	repo     *pauseRepo
	agent    *cleanRecordingAgent
	enqueuer *captureEnqueuer
}

func newPauseFixture(t *testing.T, repo *pauseRepo) *pauseFixture {
	t.Helper()
	repo.configFound = true
	// One task must be on, otherwise the clean has nothing to send.
	repo.config = Config{DBPostRevisions: true}

	ag := &cleanRecordingAgent{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := NewService(repo, nil, &fakeEvents{}, logger)
	svc.SetAgentClient(ag, &fakeSites{url: "https://example.test"})

	return &pauseFixture{svc: svc, repo: repo, agent: ag, enqueuer: &captureEnqueuer{}}
}

func (f *pauseFixture) sweeper(t *testing.T) *DBCleanScheduleWorker {
	t.Helper()
	w := NewDBCleanScheduleWorker(f.svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.SetEnqueuer(f.enqueuer, "https://cp.test")
	return w
}

func (f *pauseFixture) dispatcher(t *testing.T) *DBCleanWorker {
	t.Helper()
	return NewDBCleanWorker(f.svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func dueSite(tenant, site uuid.UUID) DueDBCleanSite {
	return DueDBCleanSite{SiteID: site, TenantID: tenant, DBAutoCleanInterval: "weekly"}
}

func cleanJob(a DBCleanArgs) *river.Job[DBCleanArgs] {
	return &river.Job[DBCleanArgs]{Args: a}
}

func containsSite(ids []uuid.UUID, want uuid.UUID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// A paused site is never enqueued, and its ACTIVE sibling in the same sweep
// still is. The second half is the assertion that matters: a fix that skipped
// the whole sweep would satisfy the first half alone.
func TestScheduledSweepSkipsPausedSiteAndStillCleansTheActiveSibling(t *testing.T) {
	tenant, pausedID, activeID := uuid.New(), uuid.New(), uuid.New()
	repo := &pauseRepo{
		due:          []DueDBCleanSite{dueSite(tenant, pausedID), dueSite(tenant, activeID)},
		pausedAtScan: map[uuid.UUID]bool{pausedID: true},
		pausedAtFire: map[uuid.UUID]bool{pausedID: true},
	}
	f := newPauseFixture(t, repo)

	if err := f.sweeper(t).Work(context.Background(), &river.Job[DBCleanScheduleArgs]{}); err != nil {
		t.Fatalf("sweep must succeed: %v", err)
	}

	got := f.enqueuer.siteIDs()
	if containsSite(got, pausedID) {
		t.Fatalf("a paused site must not be enqueued for a scheduled clean, but %s was", pausedID)
	}
	if !containsSite(got, activeID) {
		t.Fatalf("the active sibling must still be cleaned in the same sweep, but only %v were enqueued", got)
	}
	if len(got) != 1 {
		t.Fatalf("exactly one site must be enqueued, got %d: %v", len(got), got)
	}

	// The paused site's schedule is still advanced. Leaving it perpetually due
	// would let a fleet of paused sites fill the selection window and starve
	// the active sites behind them.
	if !containsSite(repo.advanced, pausedID) {
		t.Fatalf("a skipped paused site must still have next_db_clean_at advanced, advanced=%v", repo.advanced)
	}
}

// The case the sweep filter cannot catch: the site was ACTIVE at selection and
// paused before the dispatch job ran. Nothing drains River on pause, so the
// dispatcher must ask again — and no db_clean command may reach the agent.
func TestSitePausedAfterSelectionIsDeclinedAtFireTime(t *testing.T) {
	tenant, siteID := uuid.New(), uuid.New()
	repo := &pauseRepo{
		// Unpaused at scan…
		pausedAtScan: map[uuid.UUID]bool{},
		// …paused by the time the job runs.
		pausedAtFire: map[uuid.UUID]bool{siteID: true},
	}
	f := newPauseFixture(t, repo)

	err := f.dispatcher(t).Work(context.Background(), cleanJob(DBCleanArgs{
		TenantID: tenant, SiteID: siteID, Trigger: "scheduled",
	}))
	if err != nil {
		t.Fatalf("a declined clean must succeed, not fail the job: %v", err)
	}
	if got := f.agent.cleaned(); len(got) != 0 {
		t.Fatalf("no db_clean command may reach a paused site's agent, but %v were sent", got)
	}
}

// Pause governs the schedule, never the operator: a manual clean on a paused
// site still sends the command.
func TestManualCleanOnAPausedSiteStillReachesTheAgent(t *testing.T) {
	tenant, siteID := uuid.New(), uuid.New()
	repo := &pauseRepo{
		pausedAtScan: map[uuid.UUID]bool{siteID: true},
		pausedAtFire: map[uuid.UUID]bool{siteID: true},
	}
	f := newPauseFixture(t, repo)

	// The production manual path is perf/handler.go's POST handler calling
	// Service.DBClean directly — it never goes through River. Calling the same
	// service method here is that path; the worker's "manual" trigger is
	// covered separately below so a future manual enqueue stays unfiltered.
	if _, err := f.svc.DBClean(context.Background(), tenant, siteID, "https://cp.test"); err != nil {
		t.Fatalf("an operator's manual clean must not be blocked by pause: %v", err)
	}
	if got := f.agent.cleaned(); len(got) != 1 || got[0] != siteID {
		t.Fatalf("the manual clean must send exactly one db_clean to %s, got %v", siteID, got)
	}
}

// The same, through the dispatch worker's manual trigger.
func TestManualTriggeredJobOnAPausedSiteIsNotFiltered(t *testing.T) {
	tenant, siteID := uuid.New(), uuid.New()
	repo := &pauseRepo{
		pausedAtScan: map[uuid.UUID]bool{siteID: true},
		pausedAtFire: map[uuid.UUID]bool{siteID: true},
	}
	f := newPauseFixture(t, repo)

	if err := f.dispatcher(t).Work(context.Background(), cleanJob(DBCleanArgs{
		TenantID: tenant, SiteID: siteID, Trigger: "manual",
	})); err != nil {
		t.Fatalf("a manual job must not be blocked by pause: %v", err)
	}
	if got := f.agent.cleaned(); len(got) != 1 || got[0] != siteID {
		t.Fatalf("the manual job must send exactly one db_clean to %s, got %v", siteID, got)
	}
}

// A pause lookup that errors must FAIL CLOSED. This is the half that separates
// db_clean from the reversible vuln rescan: rescanning on an unknown pause
// state costs a wasted read, cleaning on one deletes the customer's rows.
//
// Fail-closed and fail-visible are separate properties: the clean must not
// happen (closed), and the failure must not be swallowed into a successful
// job (visible) — a sustained pause-lookup outage must show up as a failing
// River job, not a warning nobody reads while every job reports success.
func TestAPauseCheckErrorDeclinesTheScheduledClean(t *testing.T) {
	tenant, siteID := uuid.New(), uuid.New()
	wantErr := errors.New("pause lookup unavailable")
	repo := &pauseRepo{fireErr: wantErr}
	f := newPauseFixture(t, repo)

	err := f.dispatcher(t).Work(context.Background(), cleanJob(DBCleanArgs{
		TenantID: tenant, SiteID: siteID, Trigger: "scheduled",
	}))
	if !errors.Is(err, wantErr) {
		t.Fatalf("a pause-check failure must be returned so River retries and the job is visibly failed, got %v", err)
	}
	if got := f.agent.cleaned(); len(got) != 0 {
		t.Fatalf("an unknown pause state must decline the destructive clean, but %v were sent", got)
	}
}

// Same, at the sweep: an unavailable pause lookup declines the whole sweep
// rather than cleaning through it, and now fails the sweep job visibly too.
func TestASweepPauseLookupErrorEnqueuesNothing(t *testing.T) {
	tenant := uuid.New()
	wantErr := errors.New("pause lookup unavailable")
	repo := &pauseRepo{
		due:     []DueDBCleanSite{dueSite(tenant, uuid.New()), dueSite(tenant, uuid.New())},
		scanErr: wantErr,
	}
	f := newPauseFixture(t, repo)

	err := f.sweeper(t).Work(context.Background(), &river.Job[DBCleanScheduleArgs]{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("a pause-lookup failure must be returned so River retries and the sweep is visibly failed, got %v", err)
	}
	if got := f.enqueuer.siteIDs(); len(got) != 0 {
		t.Fatalf("an unknown pause state must enqueue nothing, got %v", got)
	}
}

// A resumed site resumes: once unpaused, the same site is enqueued again.
func TestAResumedSiteIsCleanedAgain(t *testing.T) {
	tenant, siteID := uuid.New(), uuid.New()
	repo := &pauseRepo{
		due:          []DueDBCleanSite{dueSite(tenant, siteID)},
		pausedAtScan: map[uuid.UUID]bool{siteID: true},
	}
	f := newPauseFixture(t, repo)
	w := f.sweeper(t)

	if err := w.Work(context.Background(), &river.Job[DBCleanScheduleArgs]{}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := f.enqueuer.siteIDs(); len(got) != 0 {
		t.Fatalf("precondition: the paused site must not be enqueued, got %v", got)
	}

	delete(repo.pausedAtScan, siteID)
	if err := w.Work(context.Background(), &river.Job[DBCleanScheduleArgs]{}); err != nil {
		t.Fatalf("sweep after resume: %v", err)
	}
	if got := f.enqueuer.siteIDs(); len(got) != 1 || got[0] != siteID {
		t.Fatalf("a resumed site must be cleaned again, got %v", got)
	}
}
