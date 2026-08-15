// app_aggregate_population_test.go — GH #414 phase 3, round 3 on
// fireAppAggregate. Every earlier round shut a hole where a paused site
// silenced an unpaused one; those are pinned in app_alert_pause_test.go and
// nothing here may loosen them. This file covers the opposite failure: the
// aggregate notification saying something it cannot substantiate.
//
// The shape under test is one population, decided once
// (ProbeWorker.appAggregatePopulation), from which the count, the denominator
// and the names all come — and a send gate that asks only whether that
// population has something true to say.
package uptime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// countingRatioRepo wraps the shared fake to (a) count ratio reads and (b) let
// a test fail the Nth one. The dispatch-time re-read is the only read that can
// fail in the window this file is about, so failing it by index is how the
// "population cannot be determined" case is reached at all.
type countingRatioRepo struct {
	*appPauseRepo

	mu       sync.Mutex
	calls    int
	failFrom int // 0 = never fail; N = fail the Nth call onward
}

var errRatioReRead = errors.New("ratio re-read failed")

func (r *countingRatioRepo) GetTenantAppAlertRatio(ctx context.Context, tenantID uuid.UUID) (int, int, error) {
	r.mu.Lock()
	r.calls++
	n := r.calls
	fail := r.failFrom > 0 && n >= r.failFrom
	r.mu.Unlock()
	if fail {
		return 0, 0, errRatioReRead
	}
	return r.appPauseRepo.GetTenantAppAlertRatio(ctx, tenantID)
}

func (r *countingRatioRepo) ratioReads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// newAggregateRig mirrors newAppPauseRig but accepts any Repo, so the counting
// wrapper above can stand in front of the shared fake.
func newAggregateRig(t *testing.T, repo Repo) (*ProbeWorker, *recordingMailer) {
	t.Helper()
	mailer := &recordingMailer{}
	w := NewProbeWorker(repo, nil, nil, NewDispatcher(mailer, nil, nil, nil), nil, nil, 4, 2)
	return w, mailer
}

// ---------------------------------------------------------------------------
// FINDING A — a page that names nothing.
// ---------------------------------------------------------------------------

// TestAggregateWithZeroSubstantiatedDownSitesIsNotSent reproduces the observed
// mail: "APP ALERTS SUPPRESSED: 0/4 sites are simultaneously app-down", body
// "0 of 4 alert-eligible sites ... are simultaneously failing", naming nothing.
//
// Fleet: two sites down and paused between the ratio query and the dispatch,
// plus four healthy unpaused sites. The first ratio read counts 2 of 6 and
// trips; by dispatch time both down sites are paused, so the substantiated
// population is 0 down of 4 eligible — a notification whose own numbers say
// nothing is wrong. It must not be sent, and the breaker must not trip on a
// population that does not clear its own bar.
//
// RED: drop the `!appBreakerBarMet(...)` half of fireAppAggregate's send gate
// AND the `wantTrip = appBreakerBarMet(...)` recomputation in resolveAppAlerts
// — one mail arrives reading 0/4.
func TestAggregateWithZeroSubstantiatedDownSitesIsNotSent(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	downA := appPauseFleetSite("https://zeronamed-down-a.example.com")
	downA.inIncident, downA.pausedAfterTheRow = true, true
	downB := appPauseFleetSite("https://zeronamed-down-b.example.com")
	downB.inIncident, downB.pausedAfterTheRow = true, true

	fleet := []appPauseSite{downA, downB}
	for _, u := range []string{"h1", "h2", "h3", "h4"} {
		fleet = append(fleet, appPauseFleetSite("https://zeronamed-"+u+".example.com"))
	}

	base := &appPauseRepo{fleet: fleet, breaker: map[uuid.UUID]AppBreakerState{}}
	repo := &countingRatioRepo{appPauseRepo: base}
	w, mailer := newAggregateRig(t, repo)

	fires := []pendingAppFire{fireFor(downA, true), fireFor(downB, true)}
	for i := range fires {
		fires[i].site.TenantID = tenant
	}
	w.resolveAppAlerts(ctx, fires, time.Now())

	subjects, bodies := mailer.sent()
	if len(subjects) != 0 {
		t.Fatalf("an aggregate whose substantiated population is 0 down must not be sent, got subjects=%v bodies=%v", subjects, bodies)
	}
	if base.breaker[tenant].Tripped {
		t.Fatalf("the breaker must not stay tripped on a population that does not clear its own bar: %+v", base.breaker[tenant])
	}
}

// ---------------------------------------------------------------------------
// FINDING B — the phase 2 fix must not be contingent on a read succeeding.
// ---------------------------------------------------------------------------

// TestAggregateWithheldWhenTheRatioReReadFails reproduces the observed mail:
// with the dispatch-time ratio re-read erroring, a tenant whose every site is
// paused received "APP ALERTS SUPPRESSED: 2/2 sites are simultaneously
// app-down" over a body reading "Suppressed sites: none named". That is the
// exact hole phase 2 closed, re-opened by a query blip, and it fails in the
// LOUD direction: it pages someone who deliberately asked for silence.
//
// This path is a notification, not a safety interlock — the per-site alerts for
// genuinely unpaused sites are governed by fire and fireApp, each of which
// re-reads the pause itself. So a population that cannot be determined is
// withheld and logged, never sent with counts nothing substantiates.
//
// RED: restore the old `if err != nil { log } else { counts = ... }` in
// appAggregatePopulation, so the pre-pause counts stand — one mail arrives
// reading 2/2 and naming nothing.
func TestAggregateWithheldWhenTheRatioReReadFails(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	a := appPauseFleetSite("https://reread-a.example.com")
	a.paused, a.inIncident = true, true
	b := appPauseFleetSite("https://reread-b.example.com")
	b.paused, b.inIncident = true, true

	base := &appPauseRepo{fleet: []appPauseSite{a, b}, breaker: map[uuid.UUID]AppBreakerState{}}
	repo := &countingRatioRepo{appPauseRepo: base, failFrom: 1}
	w, mailer := newAggregateRig(t, repo)

	fires := []pendingAppFire{fireFor(a, true), fireFor(b, true)}
	for i := range fires {
		fires[i].site.TenantID = tenant
	}
	// Driven directly, exactly as TestFullyPausedTenantAggregateNamesNothing
	// does, so the tenant-level ratio leg cannot be what saves it: these are
	// the pre-pause counts a trip would have carried in.
	w.fireAppAggregate(ctx, tenant, AppAggregateAlert{
		TenantID: tenant, DownCount: 2, EligibleCount: 2, FiredAt: time.Now(),
	}, fires)

	subjects, bodies := mailer.sent()
	if len(subjects) != 0 {
		t.Fatalf("an aggregate whose population could not be determined must be withheld, got subjects=%v bodies=%v", subjects, bodies)
	}
}

// TestAggregateWithheldWhenPopulationFallsBelowTheBar is the same rule from the
// other side: the re-read SUCCEEDS but drops the numbers below the threshold
// that justified tripping. The observed mail read "1/3 sites are simultaneously
// app-down ... far more likely to be a shared host issue than 1 unrelated sites
// breaking at once", which is nonsense on its face. A substantiated population
// that no longer clears the breaker's bar is a signal not to send, not a signal
// to send smaller numbers.
//
// Fleet: two down sites, one of which is paused after the ratio query, plus two
// healthy. First read is 2 of 4 (trips, 2 > 0.25*4); substantiated it is 1 of 3,
// which is below minAppAlertBreakerDownCount.
//
// RED: drop the `!appBreakerBarMet(...)` half of fireAppAggregate's send gate
// and the wantTrip recomputation in resolveAppAlerts.
func TestAggregateWithheldWhenPopulationFallsBelowTheBar(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	stays := appPauseFleetSite("https://belowbar-stays.example.com")
	stays.inIncident = true
	late := appPauseFleetSite("https://belowbar-late.example.com")
	late.inIncident, late.pausedAfterTheRow = true, true
	up1 := appPauseFleetSite("https://belowbar-up1.example.com")
	up2 := appPauseFleetSite("https://belowbar-up2.example.com")

	base := &appPauseRepo{
		fleet:   []appPauseSite{stays, late, up1, up2},
		breaker: map[uuid.UUID]AppBreakerState{},
	}
	repo := &countingRatioRepo{appPauseRepo: base}
	w, mailer := newAggregateRig(t, repo)

	fires := []pendingAppFire{fireFor(stays, true), fireFor(late, true)}
	for i := range fires {
		fires[i].site.TenantID = tenant
	}
	w.resolveAppAlerts(ctx, fires, time.Now())

	subjects, bodies := mailer.sent()
	for _, s := range subjects {
		if strings.Contains(s, "APP ALERTS SUPPRESSED") {
			t.Fatalf("a population of 1 down of 3 must not produce a fleet-wide aggregate, got subjects=%v bodies=%v", subjects, bodies)
		}
	}
	if base.breaker[tenant].Tripped {
		t.Fatalf("1 down of 3 is below the breaker's own bar; it must not stay tripped: %+v", base.breaker[tenant])
	}
	// The one genuinely down, unpaused site keeps its ordinary per-site alert:
	// withholding the aggregate must not silence it.
	if len(subjects) != 1 || !strings.Contains(subjects[0], stays.url) {
		t.Fatalf("the unpaused down site must still get its individual alert, got subjects=%v", subjects)
	}
}

// ---------------------------------------------------------------------------
// The breaker row must store the count the mail quoted.
// ---------------------------------------------------------------------------

// TestBreakerStoresTheCountTheMailQuoted pins tenant_app_alert_breaker.
// last_down_count to what the notification actually said. Observed: the mail
// quoted the post-pause "2/3" while the row kept the pre-pause 3, and a genuine
// later worsening to 3-of-3 then produced ZERO mails, because
// wantBreakerUpdate's `down <= prev.LastDownCount` was true.
//
// Both halves are asserted: the stored count after the trip, and the update
// mail that a real worsening must still produce.
//
// RED: pass the pre-pause `down` to TransitionAppAlertBreaker again (move the
// substantiation back below it in resolveAppAlerts) — the stored count is 3 and
// the second half sends nothing.
func TestBreakerStoresTheCountTheMailQuoted(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	a := appPauseFleetSite("https://stored-a.example.com")
	a.inIncident = true
	b := appPauseFleetSite("https://stored-b.example.com")
	b.inIncident = true
	late := appPauseFleetSite("https://stored-late.example.com")
	late.inIncident, late.pausedAfterTheRow = true, true
	up := appPauseFleetSite("https://stored-up.example.com")

	base := &appPauseRepo{
		fleet:   []appPauseSite{a, b, late, up},
		breaker: map[uuid.UUID]AppBreakerState{},
	}
	repo := &countingRatioRepo{appPauseRepo: base}
	w, mailer := newAggregateRig(t, repo)

	fires := []pendingAppFire{fireFor(a, true), fireFor(b, true), fireFor(late, true)}
	for i := range fires {
		fires[i].site.TenantID = tenant
	}
	start := time.Now()
	w.resolveAppAlerts(ctx, fires, start)

	subjects, bodies := mailer.sent()
	if len(subjects) != 1 {
		t.Fatalf("expected exactly one aggregate, got %d: %v", len(subjects), subjects)
	}
	if !strings.Contains(subjects[0], "2/3") {
		t.Fatalf("the subject must quote the substantiated counts (2/3), got %q with body:\n%s", subjects[0], bodies[0])
	}
	if got := base.breaker[tenant].LastDownCount; got != 2 {
		t.Errorf("the breaker must store the count the mail quoted (2), stored %d — a later worsening to 3 would be swallowed", got)
	}

	// Half 2: `up` breaks too. The live population is now 3 down of 3
	// eligible, a genuine worsening over the 2 the operator was told about,
	// and past appAlertBreakerUpdateMinInterval. It must produce an update.
	mailer.reset()
	base.setIncident(up.id, true)
	base.tripped = []uuid.UUID{tenant}
	w.resolveAppAlerts(ctx, nil, start.Add(2*time.Hour))

	subjects, bodies = mailer.sent()
	if len(subjects) != 1 {
		t.Fatalf("a worsening from 2 to 3 down must send an update aggregate, got %d: %v", len(subjects), subjects)
	}
	if !strings.Contains(subjects[0], "3/3") {
		t.Fatalf("the update must quote the worsened counts (3/3), got %q with body:\n%s", subjects[0], bodies[0])
	}
}

// ---------------------------------------------------------------------------
// The over-fire control. A gate that reddens correct work guards nothing.
// ---------------------------------------------------------------------------

// TestAggregateUnchangedWhenNothingIsPaused is the control for every withhold
// above: with nothing paused anywhere, the breaker trips at exactly the same
// threshold, the mail quotes exactly the same numbers, names exactly the same
// sites, and the breaker row stores exactly the same count as before this
// change. It also pins the cost: with no site dropped there is NO re-read, so
// the ordinary path still costs one ratio query per tenant per tick.
//
// RED: make monitoringPaused return true unconditionally, or widen either half
// of the send gate into a general silencer. The aggregate disappears.
func TestAggregateUnchangedWhenNothingIsPaused(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	a := appPauseFleetSite("https://control-a.example.com")
	a.inIncident = true
	b := appPauseFleetSite("https://control-b.example.com")
	b.inIncident = true
	fleet := []appPauseSite{a, b}
	for _, u := range []string{"c", "d", "e", "f"} {
		fleet = append(fleet, appPauseFleetSite("https://control-"+u+".example.com"))
	}

	base := &appPauseRepo{fleet: fleet, breaker: map[uuid.UUID]AppBreakerState{}}
	repo := &countingRatioRepo{appPauseRepo: base}
	w, mailer := newAggregateRig(t, repo)

	fires := []pendingAppFire{fireFor(a, true), fireFor(b, true)}
	for i := range fires {
		fires[i].site.TenantID = tenant
	}
	w.resolveAppAlerts(ctx, fires, time.Now())

	subjects, bodies := mailer.sent()
	if len(subjects) != 1 {
		t.Fatalf("with nothing paused the breaker must trip and send exactly one aggregate, got %d: %v", len(subjects), subjects)
	}
	if !strings.Contains(subjects[0], "2/6") {
		t.Fatalf("the subject must still read 2/6, got %q", subjects[0])
	}
	if !strings.Contains(bodies[0], "2 of 6") {
		t.Fatalf("the body must still read 2 of 6, body:\n%s", bodies[0])
	}
	for _, want := range []string{a.url, b.url} {
		if !strings.Contains(bodies[0], want) {
			t.Fatalf("the aggregate must name %s exactly as before, body:\n%s", want, bodies[0])
		}
	}
	if !base.breaker[tenant].Tripped {
		t.Fatalf("the breaker must be tripped: %+v", base.breaker[tenant])
	}
	if got := base.breaker[tenant].LastDownCount; got != 2 {
		t.Fatalf("the breaker must store 2 exactly as before, stored %d", got)
	}
	if got := repo.ratioReads(); got != 1 {
		t.Fatalf("with nothing dropped there must be NO dispatch-time re-read: %d ratio reads", got)
	}
}
