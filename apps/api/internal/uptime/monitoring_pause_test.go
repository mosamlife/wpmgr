// monitoring_pause_test.go — GH #414 phase 2, the parts that need no database.
//
// The DB-backed half (the query predicate itself, under RLS, as the wpmgr_app
// role) lives in apps/api/tests/gh414_monitoring_probe_pause_integration_test.go.
// What is pinned here is the WIRING: which enumeration the sweep asks for,
// which enumeration the cron kicker asks for, and whether the dispatch-side
// re-read actually withholds an alert. The prober and the dispatcher are the
// REAL types (they are concrete, not interfaces), driven against httptest
// servers, so the alert really is composed and really would be delivered.
package uptime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
)

// pauseFakeRepo services exactly the calls a Sweep makes and records them.
// Anything it does not implement is inherited from the embedded nil Repo and
// panics, which is deliberate: a sweep that starts calling something new fails
// loudly rather than silently widening what pause touches.
type pauseFakeRepo struct {
	Repo

	monitored []EnrolledSite // what ListEnrolledForMonitoringProbe returns
	all       []EnrolledSite // what the UNFILTERED ListEnrolledForProbe returns
	paused    map[uuid.UUID]bool

	mu               sync.Mutex
	calledMonitored  int
	calledAll        int
	pauseChecks      []uuid.UUID
	healthWrites     []uuid.UUID
	transitionWrites []uuid.UUID
}

func (r *pauseFakeRepo) ListEnrolledForMonitoringProbe(_ context.Context) ([]EnrolledSite, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calledMonitored++
	return r.monitored, nil
}

func (r *pauseFakeRepo) ListEnrolledForProbe(_ context.Context) ([]EnrolledSite, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calledAll++
	return r.all, nil
}

func (r *pauseFakeRepo) IsMonitoringPaused(_ context.Context, siteID uuid.UUID) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pauseChecks = append(r.pauseChecks, siteID)
	return r.paused[siteID], nil
}

func (r *pauseFakeRepo) SetSiteHealth(_ context.Context, siteID uuid.UUID, _ string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.healthWrites = append(r.healthWrites, siteID)
	return true, nil
}

func (r *pauseFakeRepo) TransitionAlertState(_ context.Context, siteID, _ uuid.UUID, up bool, _ int, _ time.Time, _ int, _ string,
	_ bool, _ *bool, _ string, _ int) (Transition, AppTransition, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transitionWrites = append(r.transitionWrites, siteID)
	// A down probe fires a down transition: the strongest case, because it is
	// the one that would page someone.
	return Transition{FireDown: !up}, AppTransition{}, nil
}

func (r *pauseFakeRepo) GetAlertConfig(_ context.Context, tenantID uuid.UUID) (AlertConfig, bool, error) {
	return AlertConfig{
		TenantID:        tenantID,
		EmailRecipients: []string{"ops@example.com"},
		Enabled:         true,
	}, true, nil
}

func (r *pauseFakeRepo) ListTrippedAppAlertBreakerTenants(_ context.Context) ([]uuid.UUID, error) {
	return nil, nil
}

func (r *pauseFakeRepo) counts() (monitored, all int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calledMonitored, r.calledAll
}

// discardStore is a metrics.Store that accepts writes and answers nothing.
// The time-series is not what these tests are about; the alert is.
type discardStore struct{ metrics.Store }

func (discardStore) Enabled() bool { return false }
func (discardStore) InsertChecks(_ context.Context, _ []metrics.Check) error {
	return nil
}

// pauseTestRig wires the real Prober and the real Dispatcher. The probe target
// always answers 500, so every probed site is DOWN and therefore reaches the
// dispatch gate; the mailer counts what actually got delivered.
type pauseTestRig struct {
	worker *ProbeWorker
	mailer *stubMailer
	down   *httptest.Server
}

func newPauseRig(t *testing.T, repo Repo) *pauseTestRig {
	t.Helper()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)

	mailer := &stubMailer{}
	dispatcher := NewDispatcher(mailer, nil, nil, nil)
	prober := NewProber(testClient(), 2*time.Second)
	// threshold 1: one down probe is enough to fire, so the test does not
	// depend on the state machine (the fake repo returns the transition).
	w := NewProbeWorker(repo, prober, discardStore{}, dispatcher, nil, nil, 4, 1)
	return &pauseTestRig{worker: w, mailer: mailer, down: down}
}

func (r *pauseTestRig) delivered() int {
	r.mailer.mu.Lock()
	defer r.mailer.mu.Unlock()
	return r.mailer.calls
}

func pauseSite(tenant uuid.UUID, url string) EnrolledSite {
	return EnrolledSite{ID: uuid.New(), TenantID: tenant, URL: url, HealthStatus: "healthy", ConnectionState: "connected"}
}

// TestSweepSelectsOnlyMonitoredSites pins the SELECTION gate: the sweep asks
// for the monitoring-filtered enumeration and never for the unfiltered one,
// and the ACTIVE site in the same tenant still alerts.
//
// RED: change Sweep back to w.repo.ListEnrolledForProbe.
func TestSweepSelectsOnlyMonitoredSites(t *testing.T) {
	tenant := uuid.New()
	repo := &pauseFakeRepo{paused: map[uuid.UUID]bool{}}
	rig := newPauseRig(t, repo)

	active := pauseSite(tenant, rig.down.URL)
	paused := pauseSite(tenant, rig.down.URL)
	repo.monitored = []EnrolledSite{active}   // what the DB predicate returns
	repo.all = []EnrolledSite{active, paused} // what the unfiltered query returns
	repo.paused[paused.ID] = true

	n, err := rig.worker.Sweep(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	monitoredCalls, allCalls := repo.counts()
	if allCalls != 0 {
		t.Fatalf("the probe sweep must NOT use the unfiltered enumeration: ListEnrolledForProbe called %d times", allCalls)
	}
	if monitoredCalls != 1 {
		t.Fatalf("the probe sweep must use ListEnrolledForMonitoringProbe exactly once, got %d", monitoredCalls)
	}
	if n != 1 {
		t.Fatalf("a paused site must not be probed: probed %d sites, want 1 (the active one)", n)
	}
	for _, id := range repo.healthWrites {
		if id == paused.ID {
			t.Fatalf("a paused site must not have its health_status rewritten by the scheduled sweep")
		}
	}
	// The over-fire control, and the case that matters most: the ACTIVE site
	// in the SAME tenant still alerts exactly as before.
	if got := rig.delivered(); got != 1 {
		t.Fatalf("the active site must still alert: %d alerts delivered, want 1", got)
	}
}

// TestQueuedProbeProducesNoAlertAfterPause pins the DISPATCH gate: a probe that
// was already admitted when the pause landed still runs, still records, and is
// still evaluated - but its alert is withheld.
//
// The fake models exactly that race: the site IS in the monitored enumeration
// (the snapshot was taken before the pause), and IsMonitoringPaused - the fresh
// read - says paused.
//
// RED: delete the `if w.monitoringPaused(...)` guard from fire().
func TestQueuedProbeProducesNoAlertAfterPause(t *testing.T) {
	tenant := uuid.New()
	repo := &pauseFakeRepo{paused: map[uuid.UUID]bool{}}
	rig := newPauseRig(t, repo)

	inflight := pauseSite(tenant, rig.down.URL)
	repo.monitored = []EnrolledSite{inflight}
	repo.paused[inflight.ID] = true

	if _, err := rig.worker.Sweep(context.Background(), time.Now()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if got := rig.delivered(); got != 0 {
		t.Fatalf("a probe queued before the pause must produce NO alert, %d were delivered", got)
	}
	if len(repo.pauseChecks) != 1 || repo.pauseChecks[0] != inflight.ID {
		t.Fatalf("the fire path must re-read the pause state fresh for the firing site, got %v", repo.pauseChecks)
	}
	// The record must stay TRUE. Pause withholds the notification; it never
	// rewrites what the site actually did (see monitoringPaused's doc comment,
	// and Sweep's on the open-incident decision).
	if len(repo.transitionWrites) != 1 {
		t.Fatalf("the in-flight probe's transition must still be persisted (the incident stays real), got %d writes", len(repo.transitionWrites))
	}
	if len(repo.healthWrites) != 1 {
		t.Fatalf("the in-flight probe's health_status must still be recorded truthfully, got %d writes", len(repo.healthWrites))
	}
}

// TestActiveSitesStillAlertWithPauseCheckPresent is the OVER-FIRE case on its
// own: nothing paused anywhere, and every alert still goes out. A pause feature
// that silences everyone is worse than no pause feature.
//
// RED: make monitoringPaused return true unconditionally.
func TestActiveSitesStillAlertWithPauseCheckPresent(t *testing.T) {
	tenant := uuid.New()
	repo := &pauseFakeRepo{paused: map[uuid.UUID]bool{}}
	rig := newPauseRig(t, repo)

	repo.monitored = []EnrolledSite{pauseSite(tenant, rig.down.URL), pauseSite(tenant, rig.down.URL)}

	if _, err := rig.worker.Sweep(context.Background(), time.Now()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := rig.delivered(); got != 2 {
		t.Fatalf("with nothing paused, both sites must alert exactly as before: got %d, want 2", got)
	}
}

// pauseErrorRepo makes the pause lookup fail.
type pauseErrorRepo struct{ pauseFakeRepo }

func (r *pauseErrorRepo) IsMonitoringPaused(_ context.Context, _ uuid.UUID) (bool, error) {
	return false, context.DeadlineExceeded
}

// TestPauseLookupFailureStillAlerts pins the fail-open direction: a database
// blip on the pause read must not silence a real alert.
//
// RED: in monitoringPaused, `return false` on the error branch -> `return true`.
func TestPauseLookupFailureStillAlerts(t *testing.T) {
	tenant := uuid.New()
	repo := &pauseErrorRepo{pauseFakeRepo: pauseFakeRepo{paused: map[uuid.UUID]bool{}}}
	rig := newPauseRig(t, repo)

	repo.monitored = []EnrolledSite{pauseSite(tenant, rig.down.URL)}

	if _, err := rig.worker.Sweep(context.Background(), time.Now()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := rig.delivered(); got != 1 {
		t.Fatalf("a failed pause lookup must fail OPEN and still alert: got %d, want 1", got)
	}
}

// TestCronKickerKicksPausedSites is the promise this design makes loudest:
// pause governs MONITORING, never data protection. The cron kick boots PHP so
// the site's own WP-Cron queue drains, and that queue runs the agent's
// heartbeats and its scheduled BACKUPS. A paused site must still be kicked.
//
// This drives the real CronKicker.Kick against a real httptest wp-cron target.
//
// RED: switch CronKicker.Kick to k.repo.ListEnrolledForMonitoringProbe. The
// paused site drops out and the hit count falls to 1 - and the panic in
// cron_kick_test.go's fakeCronRepo catches the same switch from the other side.
func TestCronKickerKicksPausedSites(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tenant := uuid.New()
	active := pauseSite(tenant, srv.URL)
	paused := pauseSite(tenant, srv.URL)
	repo := &pauseFakeRepo{
		monitored: []EnrolledSite{active},
		all:       []EnrolledSite{active, paused},
		paused:    map[uuid.UUID]bool{paused.ID: true},
	}

	kicker := NewCronKicker(repo, testClient(), 5*time.Second, 5)
	if err := kicker.Kick(context.Background()); err != nil {
		t.Fatalf("kick: %v", err)
	}

	mu.Lock()
	got := hits
	mu.Unlock()
	if got != 2 {
		t.Fatalf("the cron kick must still fire for a PAUSED site (its backups depend on WP-Cron draining): %d hits, want 2", got)
	}
	if monitored, all := repo.counts(); monitored != 0 || all != 1 {
		t.Fatalf("the cron kicker must use the UNFILTERED enumeration exactly once: monitored=%d unfiltered=%d", monitored, all)
	}
}
