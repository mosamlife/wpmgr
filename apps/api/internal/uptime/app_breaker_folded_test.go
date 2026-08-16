// app_breaker_folded_test.go — GH #414 phase 2, round 6. The tenant folded
// into resolveAppAlerts from ListTrippedAppAlertBreakerTenants carries NO fires
// this tick, and every earlier round left at least one path that read the
// aggregate population without having resolved it for exactly that tenant.
//
// The rule these pin: pause means "do not tell me", never "lie to me", and
// nothing in this file pauses anything. A tenant nobody paused must never be
// silenced, and the count written to tenant_app_alert_breaker.last_down_count
// must be the count the mail quotes — on every path, because a row and a mail
// that disagree is how the silence became permanent rather than one tick long.
//
// Driven through the REAL resolveAppAlerts, the REAL EvaluateAppBreaker and the
// REAL Dispatcher over the shared fake from app_alert_pause_test.go.
package uptime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// downListFailingRepo fails ListTenantAppDownSites so the name-query fallback
// is reachable. Every other call goes to the shared fake.
type downListFailingRepo struct {
	*appPauseRepo
}

func (r *downListFailingRepo) ListTenantAppDownSites(context.Context, uuid.UUID, int) ([]string, error) {
	return nil, errRatioReRead
}

// ---------------------------------------------------------------------------
// THE BUG. A tenant nobody paused, silent for the whole outage.
// ---------------------------------------------------------------------------

// TestFoldedInTenantTrippingSendsTheAggregate is the reproduction.
//
// The tenant is folded in from ListTrippedAppAlertBreakerTenants, so `fires` is
// nil. Between that read and the LOCKING re-read inside
// TransitionAppAlertBreaker, an overlapping sweep clears the breaker row — an
// ordinary event, not a contrived one: the probe sweep is a River periodic with
// RunOnStart whose ProbeArgs declares no InsertOpts, so it runs on QueueDefault
// with MaxWorkers 5, and a sweep that exceeds its 60s interval overlaps the
// next. Two overlapping resolveAppAlerts on one tenant is what a fleet whose
// sweep takes over a minute does every minute. The fake models it exactly:
// `tripped` names the tenant while the breaker map has no row for it.
//
// EvaluateAppBreaker then returns FireTrip, and the FireTrip branch used to read
// a `pop` that had only ever been resolved under `wantTrip && len(fires) > 0`.
// With no fires it was the ZERO VALUE, so the alert was built as 0 down of 0
// eligible and fireAppAggregate's empty-population gate withheld it — while the
// breaker row, written from the LIVE down count, was left tripped. Every
// per-site app alert for the tenant is suppressed behind that row, no aggregate
// replaced them, and wantBreakerUpdate cannot re-open the path because it needs
// `down` to RISE above the LastDownCount the row already holds.
//
// RED: restore `if wantTrip && len(fires) > 0` around the substantiation in
// resolveAppAlerts (and `var pop appAggregatePopulation` above it) —
//
//	fleet is 2 down of 3 eligible; nothing is paused
//	breaker after the tick: tripped=true last_down_count=2
//	mails sent: 0 []
func TestFoldedInTenantTrippingSendsTheAggregate(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	a := appPauseFleetSite("https://folded-a.example.com")
	a.inIncident = true
	b := appPauseFleetSite("https://folded-b.example.com")
	b.inIncident = true
	up := appPauseFleetSite("https://folded-up.example.com")

	// `tripped` names the tenant; `breaker` has no row for it. That IS the
	// race: the list read saw a tripped row, the locking re-read a moment
	// later does not.
	base := &appPauseRepo{
		fleet:   []appPauseSite{a, b, up},
		breaker: map[uuid.UUID]AppBreakerState{},
		tripped: []uuid.UUID{tenant},
	}
	repo := &countingRatioRepo{appPauseRepo: base}
	w, mailer := newAggregateRig(t, repo)

	eligible, down := appRatio(base.fleet)
	if eligible != 3 || down != 2 {
		t.Fatalf("rig check: fleet must be 2 down of 3 eligible with nothing paused, got %d down of %d", down, eligible)
	}

	w.resolveAppAlerts(ctx, nil, time.Now())

	subjects, bodies := mailer.sent()
	if len(subjects) != 1 {
		t.Fatalf("the breaker tripped and is suppressing every per-site alert, so the aggregate MUST go out; fleet is %d down of %d eligible and nothing is paused. breaker=%+v mails sent: %d %v",
			down, eligible, base.breaker[tenant], len(subjects), subjects)
	}
	if !strings.Contains(subjects[0], "APP ALERTS SUPPRESSED") || !strings.Contains(subjects[0], "2/3") {
		t.Fatalf("expected the aggregate quoting the live 2/3, got %q with body:\n%s", subjects[0], bodies[0])
	}
	for _, want := range []string{a.url, b.url} {
		if !strings.Contains(bodies[0], want) {
			t.Fatalf("a tenant folded in with no fires must still name its live down sites; %s missing from body:\n%s", want, bodies[0])
		}
	}
	if strings.Contains(bodies[0], up.url) {
		t.Fatalf("the healthy site must not be named, body:\n%s", bodies[0])
	}
	if !base.breaker[tenant].Tripped {
		t.Fatalf("the breaker must be tripped: %+v", base.breaker[tenant])
	}
	if got := base.breaker[tenant].LastDownCount; got != 2 {
		t.Fatalf("the row must store the count the mail quoted (2), stored %d — a disagreement here is what made the silence permanent", got)
	}
}

// TestFoldedInTenantRowAndMailAgreeAfterALatePause is the same folded-in tenant
// with a pause landing mid-dispatch, which is the only way the row and the mail
// can be given different numbers on this path. It pins them equal.
//
// The tenant is folded in AND has one fire, so the dispatch-time re-read is
// reachable: `late` is paused between the ratio query and the dispatch, taking
// the population from 3 down of 4 to 2 down of 3. The mail must quote 2/3 and
// the row must store 2 — if the row kept the pre-pause 3, the genuine later
// worsening to 3 would be swallowed by wantBreakerUpdate's
// `down <= prev.LastDownCount` and the tenant would go quiet for the rest of
// the outage.
//
// RED: pass the pre-pause `down` (or the live `eligible`/`down`) to
// TransitionAppAlertBreaker and the FireTrip alert instead of pop's — the
// subject reads 3/4 or the stored count is 3.
func TestFoldedInTenantRowAndMailAgreeAfterALatePause(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	a := appPauseFleetSite("https://foldlate-a.example.com")
	a.inIncident = true
	b := appPauseFleetSite("https://foldlate-b.example.com")
	b.inIncident = true
	late := appPauseFleetSite("https://foldlate-late.example.com")
	late.inIncident, late.pausedAfterTheRow = true, true
	up := appPauseFleetSite("https://foldlate-up.example.com")

	base := &appPauseRepo{
		fleet:   []appPauseSite{a, b, late, up},
		breaker: map[uuid.UUID]AppBreakerState{},
		tripped: []uuid.UUID{tenant},
	}
	repo := &countingRatioRepo{appPauseRepo: base}
	w, mailer := newAggregateRig(t, repo)

	fires := []pendingAppFire{fireFor(late, true)}
	fires[0].site.TenantID = tenant
	w.resolveAppAlerts(ctx, fires, time.Now())

	subjects, bodies := mailer.sent()
	if len(subjects) != 1 {
		t.Fatalf("expected exactly one aggregate, got %d: %v", len(subjects), subjects)
	}
	if !strings.Contains(subjects[0], "2/3") {
		t.Fatalf("the subject must quote the substantiated 2/3, got %q with body:\n%s", subjects[0], bodies[0])
	}
	if strings.Contains(bodies[0], late.url) {
		t.Fatalf("the site paused mid-dispatch must not be named, body:\n%s", bodies[0])
	}
	if got := base.breaker[tenant].LastDownCount; got != 2 {
		t.Fatalf("the row must store the 2 the mail quoted, stored %d", got)
	}
}

// TestUpdateNamesTheSameSitesItCounts is the second, lower finding: on the
// FireUpdate path the counts came from GetTenantAppAlertRatio while the names
// came from a separate ListTenantAppDownSites read, so the mail quoted a number
// its own body did not substantiate — "3/3 sites are simultaneously app-down"
// over a body naming 2.
//
// Here the update is warranted (2 -> 3 down, past the min interval) and the
// mail must both quote 3/3 and name all three.
//
// RED: source SuppressedSites from a separate ListTenantAppDownSites call taken
// AFTER a site is paused, or quote the live `down`/`eligible` while the names
// come from pop — the count and the list disagree.
func TestUpdateNamesTheSameSitesItCounts(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	a := appPauseFleetSite("https://upd-a.example.com")
	a.inIncident = true
	b := appPauseFleetSite("https://upd-b.example.com")
	b.inIncident = true
	c := appPauseFleetSite("https://upd-c.example.com")
	c.inIncident = true

	long := time.Now().Add(-2 * time.Hour)
	base := &appPauseRepo{
		fleet: []appPauseSite{a, b, c},
		breaker: map[uuid.UUID]AppBreakerState{
			tenant: {Tripped: true, TrippedAt: &long, LastAlertAt: &long, LastDownCount: 2},
		},
		tripped: []uuid.UUID{tenant},
	}
	repo := &countingRatioRepo{appPauseRepo: base}
	w, mailer := newAggregateRig(t, repo)

	w.resolveAppAlerts(ctx, nil, time.Now())

	subjects, bodies := mailer.sent()
	if len(subjects) != 1 {
		t.Fatalf("a worsening from 2 to 3 down must send exactly one update, got %d: %v", len(subjects), subjects)
	}
	if !strings.Contains(subjects[0], "3/3") {
		t.Fatalf("the update must quote 3/3, got %q", subjects[0])
	}
	for _, want := range []string{a.url, b.url, c.url} {
		if !strings.Contains(bodies[0], want) {
			t.Fatalf("the update quotes 3 down, so it must name all 3; %s missing from body:\n%s", want, bodies[0])
		}
	}
	if got := base.breaker[tenant].LastDownCount; got != 3 {
		t.Fatalf("the row must store the 3 the mail quoted, stored %d", got)
	}
}

// TestNameQueryFailureStillSendsTheSubstantiatedCounts pins the fallback: the
// counts are substantiated, only the LIST could not be read. Withholding the
// mail there would silence a tenant nobody paused over a query blip, which is
// the same defect class this whole file exists to close. The mail goes out with
// the counts it can stand behind.
//
// RED: set pop.resolved = false when ListTenantAppDownSites errors — the
// aggregate disappears and every per-site alert stays suppressed behind the
// tripped breaker.
func TestNameQueryFailureStillSendsTheSubstantiatedCounts(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	a := appPauseFleetSite("https://noname-a.example.com")
	a.inIncident = true
	b := appPauseFleetSite("https://noname-b.example.com")
	b.inIncident = true
	up := appPauseFleetSite("https://noname-up.example.com")

	base := &appPauseRepo{
		fleet:   []appPauseSite{a, b, up},
		breaker: map[uuid.UUID]AppBreakerState{},
	}
	repo := &downListFailingRepo{appPauseRepo: base}
	w, mailer := newAggregateRig(t, repo)

	fires := []pendingAppFire{fireFor(a, true), fireFor(b, true)}
	for i := range fires {
		fires[i].site.TenantID = tenant
	}
	w.resolveAppAlerts(ctx, fires, time.Now())

	subjects, bodies := mailer.sent()
	if len(subjects) != 1 {
		t.Fatalf("a name-query failure must not withhold a substantiated aggregate, got %d: %v", len(subjects), subjects)
	}
	if !strings.Contains(subjects[0], "2/3") {
		t.Fatalf("the counts are substantiated and must still be quoted, got %q", subjects[0])
	}
	// The fallback list is this tick's unpaused fires - honest, and every site
	// in it is genuinely down and part of the counted population.
	for _, want := range []string{a.url, b.url} {
		if !strings.Contains(bodies[0], want) {
			t.Fatalf("the fallback must name this tick's unpaused fires; %s missing from body:\n%s", want, bodies[0])
		}
	}
	if got := base.breaker[tenant].LastDownCount; got != 2 {
		t.Fatalf("the row must store the 2 the mail quoted, stored %d", got)
	}
}

// TestFoldedInTenantUnchangedWhenItShouldStaySilent is the over-fire control
// for the folded-in path. A gate that reddens correct work guards nothing, and
// the change above resolves the population for tenants that previously took a
// cheaper path — so the two things it must NOT do are pinned here.
//
// Half 1: a folded-in tenant that is steadily tripped, with no worsening and no
// recovery, stays silent exactly as it always did (that is the breaker's whole
// point: one mail on the way in, one on the way out).
//
// Half 2: a folded-in tenant whose fleet has recovered sends its ONE recovery
// aggregate quoting the live counts, and the population is NOT substantiated on
// that path, so it costs no extra ratio read.
func TestFoldedInTenantUnchangedWhenItShouldStaySilent(t *testing.T) {
	ctx := context.Background()

	// --- Half 1: steadily tripped, nothing has changed. ------------------
	steady := uuid.New()
	a := appPauseFleetSite("https://steady-a.example.com")
	a.inIncident = true
	b := appPauseFleetSite("https://steady-b.example.com")
	b.inIncident = true
	c := appPauseFleetSite("https://steady-c.example.com")

	recent := time.Now().Add(-time.Minute)
	steadyBase := &appPauseRepo{
		fleet: []appPauseSite{a, b, c},
		breaker: map[uuid.UUID]AppBreakerState{
			steady: {Tripped: true, TrippedAt: &recent, LastAlertAt: &recent, LastDownCount: 2},
		},
		tripped: []uuid.UUID{steady},
	}
	steadyRepo := &countingRatioRepo{appPauseRepo: steadyBase}
	wSteady, steadyMailer := newAggregateRig(t, steadyRepo)

	wSteady.resolveAppAlerts(ctx, nil, time.Now())

	if subjects, bodies := steadyMailer.sent(); len(subjects) != 0 {
		t.Fatalf("a steadily tripped tenant with no worsening must stay silent, got %v bodies=%v", subjects, bodies)
	}
	if !steadyBase.breaker[steady].Tripped {
		t.Fatalf("the breaker must remain tripped: %+v", steadyBase.breaker[steady])
	}

	// --- Half 2: recovered. One aggregate, live counts, no re-read. ------
	recovered := uuid.New()
	r1 := appPauseFleetSite("https://recov-1.example.com")
	r2 := appPauseFleetSite("https://recov-2.example.com")
	r3 := appPauseFleetSite("https://recov-3.example.com")

	old := time.Now().Add(-3 * time.Hour)
	recovBase := &appPauseRepo{
		fleet: []appPauseSite{r1, r2, r3},
		breaker: map[uuid.UUID]AppBreakerState{
			recovered: {Tripped: true, TrippedAt: &old, LastAlertAt: &old, LastDownCount: 3},
		},
		tripped: []uuid.UUID{recovered},
	}
	recovRepo := &countingRatioRepo{appPauseRepo: recovBase}
	wRecov, recovMailer := newAggregateRig(t, recovRepo)

	wRecov.resolveAppAlerts(ctx, nil, time.Now())

	subjects, bodies := recovMailer.sent()
	if len(subjects) != 1 {
		t.Fatalf("a recovered fleet must send exactly one recovery aggregate, got %d: %v", len(subjects), subjects)
	}
	if !strings.Contains(bodies[0], "0 of 3") && !strings.Contains(subjects[0], "0/3") {
		t.Fatalf("the recovery must quote the live 0 of 3, got %q body:\n%s", subjects[0], bodies[0])
	}
	if recovBase.breaker[recovered].Tripped {
		t.Fatalf("the breaker must have recovered: %+v", recovBase.breaker[recovered])
	}
	if got := recovRepo.ratioReads(); got != 1 {
		t.Fatalf("a tenant that does not clear the bar must not substantiate a population: %d ratio reads", got)
	}
}
