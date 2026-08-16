// app_breaker_silence_test.go — GH #414, the app-alert breaker's SILENCE paths.
//
// Two things live here, and they are different in kind.
//
// PART 1 settles, by execution rather than by argument, whether pausing healthy
// sites can collapse the per-site alerts of sites nobody paused. It reproduces:
// see TestPausingHealthySitesCollapsesUnpausedAlertsIntoTheAggregate for the
// numbers and for why the outcome is bounded to a loss of GRANULARITY and can
// never become silence.
//
// PART 2 pins the three per-site fallbacks that make that bound hold. Each is a
// single `fireAppIndividually` call in an error or recovery branch of
// resolveAppAlerts, each carries a comment saying why it must exist, and a
// mutation run deleted all three with the whole package still green. Every test
// below names the exact line it guards and what its removal looks like.
//
// Everything runs through the REAL resolveAppAlerts over the shared fake from
// app_alert_pause_test.go and the REAL Dispatcher, so "no alert" means an email
// was never composed rather than a stub never poked.
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

// ---------------------------------------------------------------------------
// PART 1 — the pause-shrunken denominator.
// ---------------------------------------------------------------------------

// TestPausingHealthySitesCollapsesUnpausedAlertsIntoTheAggregate executes the
// claim that pausing healthy sites shrinks appBreakerBarMet's denominator
// (worker.go:1113) and trips the fleet breaker for a tenant whose down sites
// nobody paused.
//
// IT REPRODUCES. Both halves share one fleet of 100 alert-eligible sites with
// the SAME 5 sites down, and differ only in an operator action taken against 81
// OTHER, HEALTHY sites:
//
//	nothing paused:   5 down of 100 eligible. 5 > 0.25*100 == 25 is false.
//	                  No trip. Five individual APP DOWN alerts, one per site.
//	81 healthy paused: 5 down of 19 eligible. 5 > 0.25*19 == 4.75 is true.
//	                  Trips. Zero individual alerts; one aggregate.
//
// So the invariant "pausing a site must not change whether an unpaused site
// gets its own individual alert" is NOT held, and this test pins that as the
// shipped behaviour rather than asserting it away.
//
// WHY IT IS NOT TREATED AS A SILENCE DEFECT, and why no production line was
// changed for it:
//
//   - The pause is not special. resolveAppAlerts' own Fix 4 comment
//     (worker.go:786-789) already records that the ratio moves for operator
//     actions that never touch an individual site's alert state — a down site
//     archived or revoked, or per-site alerting disabled. Archiving 81 healthy
//     sites produces this outcome identically and has since GH #291 Phase 3.
//     Pause joined an existing family; it did not create the behaviour. Under
//     the semantics phase 2 established — a paused site is not being monitored
//     — 5 of the 19 sites actually under monitoring being down IS more than a
//     quarter of them, and the breaker is answering the question it was built
//     to answer.
//
//   - The outcome is bounded. The aggregate still goes out, and it NAMES every
//     unpaused down site (pop.names comes from ListTenantAppDownSites, the live
//     pause-filtered down set). The operator learns the same five URLs in one
//     mail instead of five. That is a loss of granularity, not of information,
//     and it is asserted below rather than assumed.
//
//   - Every cheap fix defeats the breaker or reddens correct work. Ignoring the
//     pause in the denominator restores the phase 2 HOLE 1 shape (paused rows
//     padding the population) and reddens TestFullyPausedTenantSendsNoAggregate,
//     which requires a fully paused tenant to receive NOTHING. A denominator
//     that counts paused sites while the numerator does not needs a count this
//     package cannot obtain: GetTenantAppAlertRatio returns one pair, and a
//     pause-blind denominator is a new query, i.e. db/query and sqlc, which is
//     not this package's to write.
//
// The residual cost, stated plainly: while tripped, later per-site app alerts
// for the tenant are suppressed, and the operator hears again only via
// FireUpdate, which is rate-limited to appAlertBreakerUpdateMinInterval and
// requires the down count to RISE. Delay and coarseness, bounded by that
// interval; never permanent silence, because the three fallbacks pinned in
// PART 2 below hold every error path open.
//
// RED, in the direction that matters: if a future change makes the trip path
// withhold the aggregate, or name none of the down sites, half 2's assertions
// fail and the degradation has become the silence this feature must never
// produce.
func TestPausingHealthySitesCollapsesUnpausedAlertsIntoTheAggregate(t *testing.T) {
	ctx := context.Background()

	// The five sites nobody pauses, in either half.
	downURLs := []string{
		"https://denom-down-1.example.com",
		"https://denom-down-2.example.com",
		"https://denom-down-3.example.com",
		"https://denom-down-4.example.com",
		"https://denom-down-5.example.com",
	}

	// buildFleet returns 100 alert-eligible sites: the five above, down, plus
	// 95 healthy ones of which the first `pauseHealthy` are monitoring-paused.
	buildFleet := func(prefix string, pauseHealthy int) ([]appPauseSite, []appPauseSite) {
		fleet := make([]appPauseSite, 0, 100)
		down := make([]appPauseSite, 0, len(downURLs))
		for _, u := range downURLs {
			s := appPauseFleetSite(prefix + u)
			s.inIncident = true
			down = append(down, s)
			fleet = append(fleet, s)
		}
		for i := 0; i < 95; i++ {
			s := appPauseFleetSite(prefix + "https://denom-healthy.example.com/" + uuid.NewString()[:8])
			s.paused = i < pauseHealthy
			fleet = append(fleet, s)
		}
		return fleet, down
	}

	// --- Half 1: nothing paused. Five individual alerts. -----------------
	tenantA := uuid.New()
	fleetA, downA := buildFleet("a", 0)
	baseA := &appPauseRepo{fleet: fleetA, breaker: map[uuid.UUID]AppBreakerState{}}
	repoA := &countingRatioRepo{appPauseRepo: baseA}
	wA, mailerA := newAggregateRig(t, repoA)

	eligibleA, downCountA := appRatio(fleetA)
	if eligibleA != 100 || downCountA != 5 {
		t.Fatalf("half 1 setup: expected 5 down of 100 eligible, got %d of %d", downCountA, eligibleA)
	}
	t.Logf("half 1: %d down of %d eligible, bar met = %v", downCountA, eligibleA, appBreakerBarMet(eligibleA, downCountA, defaultAppAlertBreakerRatio))

	wA.resolveAppAlerts(ctx, firesFor(t, downA, tenantA), time.Now())

	subjectsA, bodiesA := mailerA.sent()
	if len(subjectsA) != 5 {
		t.Fatalf("5 down of 100 is under the 25%% bar: each down site must get its OWN alert, got %d: %v (bodies %v)", len(subjectsA), subjectsA, bodiesA)
	}
	for _, s := range subjectsA {
		if strings.Contains(s, "APP ALERTS SUPPRESSED") {
			t.Fatalf("the breaker must not trip on 5 of 100, got %v", subjectsA)
		}
	}
	if baseA.breaker[tenantA].Tripped {
		t.Fatalf("5 of 100 must leave the breaker untripped: %+v", baseA.breaker[tenantA])
	}

	// --- Half 2: 81 HEALTHY sites paused. Same five down. ----------------
	tenantB := uuid.New()
	fleetB, downB := buildFleet("b", 81)
	baseB := &appPauseRepo{fleet: fleetB, breaker: map[uuid.UUID]AppBreakerState{}}
	repoB := &countingRatioRepo{appPauseRepo: baseB}
	wB, mailerB := newAggregateRig(t, repoB)

	eligibleB, downCountB := appRatio(fleetB)
	if eligibleB != 19 || downCountB != 5 {
		t.Fatalf("half 2 setup: expected 5 down of 19 eligible after pausing 81 healthy sites, got %d of %d", downCountB, eligibleB)
	}
	t.Logf("half 2: %d down of %d eligible, bar met = %v", downCountB, eligibleB, appBreakerBarMet(eligibleB, downCountB, defaultAppAlertBreakerRatio))

	wB.resolveAppAlerts(ctx, firesFor(t, downB, tenantB), time.Now())

	subjectsB, bodiesB := mailerB.sent()

	// The reproduction itself: the same five sites no longer get their own
	// alerts. If this ever stops holding, the defect has been fixed and this
	// test's verdict — and the trade-off recorded above — must be re-derived.
	if len(subjectsB) != 1 {
		t.Fatalf("pausing 81 healthy sites moves the same 5 down sites over the bar: expected exactly one aggregate, got %d: %v", len(subjectsB), subjectsB)
	}
	if !strings.Contains(subjectsB[0], "APP ALERTS SUPPRESSED") || !strings.Contains(subjectsB[0], "5/19") {
		t.Fatalf("expected the aggregate quoting the pause-shrunken 5/19, got %q", subjectsB[0])
	}

	// THE BOUND. Granularity may degrade; information may not disappear. Every
	// one of the five sites nobody paused must be named in the mail that
	// replaced its individual alert.
	for _, s := range downB {
		if !strings.Contains(bodiesB[0], s.name) {
			t.Fatalf("the aggregate replaced %s's individual alert, so it MUST name it — otherwise a site nobody paused went silent. Body:\n%s", s.name, bodiesB[0])
		}
	}
	if !baseB.breaker[tenantB].Tripped {
		t.Fatalf("5 of 19 must trip the breaker: %+v", baseB.breaker[tenantB])
	}
}

// firesFor turns fleet rows into this tick's down transitions for one tenant.
func firesFor(t *testing.T, sites []appPauseSite, tenant uuid.UUID) []pendingAppFire {
	t.Helper()
	fires := make([]pendingAppFire, 0, len(sites))
	for _, s := range sites {
		f := fireFor(s, true)
		f.site.TenantID = tenant
		fires = append(fires, f)
	}
	return fires
}

// ---------------------------------------------------------------------------
// PART 2 — the three unguarded fail-safes.
// ---------------------------------------------------------------------------

// TestRatioQueryFailureStillAlertsEachDownSiteIndividually pins
// `w.fireAppIndividually(ctx, fires, now)` in resolveAppAlerts'
// GetTenantAppAlertRatio-error branch (worker.go:827), whose own comment reads
// "a ratio-query failure must not turn into a swallowed incident notification".
//
// The breaker is a mail-volume optimisation layered ON TOP of per-site
// alerting. When the query that decides whether to engage it fails, the honest
// answer is the ORIGINAL behaviour — alert each site — not to drop the tick.
// AppTransition is transition-only, so a dropped tick is a dropped incident,
// permanently.
//
// The near miss that left this unproven: TestAggregateWithheldWhenTheRatioReRead
// Fails uses the same failFrom knob but calls fireAppAggregate DIRECTLY, so it
// never enters resolveAppAlerts and never reaches this branch at all.
//
// RED: delete the fireAppIndividually call from the ratio-error branch — two
// genuinely down, unpaused, alert-eligible sites hear nothing.
func TestRatioQueryFailureStillAlertsEachDownSiteIndividually(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	downA := appPauseFleetSite("https://ratiofail-down-a.example.com")
	downA.inIncident = true
	downB := appPauseFleetSite("https://ratiofail-down-b.example.com")
	downB.inIncident = true
	up := appPauseFleetSite("https://ratiofail-up.example.com")

	base := &appPauseRepo{
		fleet:   []appPauseSite{downA, downB, up},
		breaker: map[uuid.UUID]AppBreakerState{},
	}
	// failFrom: 1 — the FIRST read, the tenant-level ratio read, fails. That is
	// the branch under test; the dispatch-time re-read is a different one and
	// is pinned separately in app_breaker_fallback_test.go.
	repo := &countingRatioRepo{appPauseRepo: base, failFrom: 1}
	w, mailer := newAggregateRig(t, repo)

	w.resolveAppAlerts(ctx, firesFor(t, []appPauseSite{downA, downB}, tenant), time.Now())

	// Prove the branch was entered, so no future change can satisfy the
	// assertions below by never failing a read in the first place.
	if got := repo.ratioReads(); got != 1 {
		t.Fatalf("expected exactly the one failing tenant-level ratio read, got %d", got)
	}

	subjects, bodies := mailer.sent()
	if len(subjects) != 2 {
		t.Fatalf("a ratio-query failure must fall back to the ORIGINAL per-site behaviour: expected 2 individual alerts, got %d: %v (bodies %v)", len(subjects), subjects, bodies)
	}
	for _, want := range []string{downA.url, downB.url} {
		found := false
		for _, s := range subjects {
			if strings.Contains(s, "[WPMgr] APP DOWN:") && strings.Contains(s, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected an individual app-down alert naming %s, got %v", want, subjects)
		}
	}
	// The fallback must not also send an aggregate: nothing substantiated one.
	for _, s := range subjects {
		if strings.Contains(s, "APP ALERTS") {
			t.Fatalf("no aggregate can be sent on counts that could not be read, got %v", subjects)
		}
	}
	if base.breaker[tenant].Tripped {
		t.Fatalf("a ratio read that failed must not trip the breaker: %+v", base.breaker[tenant])
	}
}

var errBreakerTransition = errors.New("transition app alert breaker failed")

// breakerTransitionFailRepo fails TransitionAppAlertBreaker and counts the
// attempts, so the test can prove it reached the branch rather than assuming it.
type breakerTransitionFailRepo struct {
	*appPauseRepo

	mu    sync.Mutex
	calls int
}

func (r *breakerTransitionFailRepo) TransitionAppAlertBreaker(_ context.Context, _ uuid.UUID, _ bool, _ int, _ time.Time) (AppBreakerTransition, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return AppBreakerTransition{}, errBreakerTransition
}

func (r *breakerTransitionFailRepo) transitions() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// TestBreakerTransitionFailureStillAlertsEachDownSiteIndividually pins
// `w.fireAppIndividually(ctx, fires, now)` in resolveAppAlerts'
// TransitionAppAlertBreaker-error branch (worker.go:909).
//
// This is the harsher of the two error branches. The population HAS been
// substantiated here — the tenant genuinely clears the bar and the breaker was
// about to trip — and the only thing that failed is the write that records it.
// Falling through silently would mean the tick decided the alerts should be
// collapsed into an aggregate, then failed to send the aggregate too, so a real
// fleet-wide outage produces not one mail. Since AppTransition is consumed, the
// next tick has nothing left to re-fire from.
//
// The fleet is 2 down of 3, which clears the bar (2 > 0.25*3 == 0.75) and
// carries the population all the way through appAggregatePopulation before the
// transition write is attempted — so the assertion below is about the failure
// of the write specifically, not about never having wanted to trip.
//
// RED: delete the fireAppIndividually call from the transition-error branch —
// zero mails for a tenant with two unpaused sites down.
func TestBreakerTransitionFailureStillAlertsEachDownSiteIndividually(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	downA := appPauseFleetSite("https://trfail-down-a.example.com")
	downA.inIncident = true
	downB := appPauseFleetSite("https://trfail-down-b.example.com")
	downB.inIncident = true
	up := appPauseFleetSite("https://trfail-up.example.com")

	base := &appPauseRepo{
		fleet:   []appPauseSite{downA, downB, up},
		breaker: map[uuid.UUID]AppBreakerState{},
	}
	repo := &breakerTransitionFailRepo{appPauseRepo: base}
	w, mailer := newAggregateRig(t, repo)

	eligible, down := appRatio(base.fleet)
	if !appBreakerBarMet(eligible, down, defaultAppAlertBreakerRatio) {
		t.Fatalf("setup: %d down of %d must clear the bar, or the transition write is never attempted", down, eligible)
	}

	w.resolveAppAlerts(ctx, firesFor(t, []appPauseSite{downA, downB}, tenant), time.Now())

	if got := repo.transitions(); got != 1 {
		t.Fatalf("expected exactly one attempted breaker transition, got %d", got)
	}

	subjects, bodies := mailer.sent()
	if len(subjects) != 2 {
		t.Fatalf("a failed breaker transition must fall back to per-site alerts, or the incident is swallowed permanently: expected 2, got %d: %v (bodies %v)", len(subjects), subjects, bodies)
	}
	for _, want := range []string{downA.url, downB.url} {
		found := false
		for _, s := range subjects {
			if strings.Contains(s, "[WPMgr] APP DOWN:") && strings.Contains(s, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected an individual app-down alert naming %s, got %v", want, subjects)
		}
	}
	for _, s := range subjects {
		if strings.Contains(s, "APP ALERTS") {
			t.Fatalf("the breaker state was never recorded, so no aggregate may claim it, got %v", subjects)
		}
	}
}

// TestRecoveryTickStillAlertsANewIncidentIndividually pins
// `w.fireAppIndividually(ctx, appFireDownOnly(fires), now)` in
// resolveAppAlerts' FireRecovery branch (worker.go:989), whose comment
// (worker.go:976-981) states the consequence exactly: "AppTransition is
// transition-only (it never re-fires once consumed), so failing to dispatch it
// here would swallow that incident PERMANENTLY, not merely delay it to the next
// tick."
//
// The tick under test is the one where enough sites recover to pull the ratio
// back under the bar WHILE a different site crosses newly into an incident. The
// recovery aggregate speaks for the recoveries and says nothing whatever about
// the new outage, so the new outage must still be dispatched on its own.
//
// Fleet: recA and recB recovered this tick, fresh went down this tick, plus two
// healthy — 1 down of 5, below minAppAlertBreakerDownCount, so the breaker
// recovers. Exactly two mails must result:
//
//	the recovery aggregate, for recA and recB;
//	fresh's own APP DOWN alert, which nothing else can carry.
//
// The appFireDownOnly filter is asserted from both sides: recA and recB must
// NOT get individual recovery mails on top of the aggregate that just announced
// them.
//
// RED: delete the fireAppIndividually call from the FireRecovery branch — one
// mail arrives, the recovery aggregate, and a genuinely down site nobody paused
// is silent for the entire duration of its outage.
func TestRecoveryTickStillAlertsANewIncidentIndividually(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	recA := appPauseFleetSite("https://rectick-recovered-a.example.com")
	recB := appPauseFleetSite("https://rectick-recovered-b.example.com")
	fresh := appPauseFleetSite("https://rectick-fresh-down.example.com")
	fresh.inIncident = true
	h1 := appPauseFleetSite("https://rectick-h1.example.com")
	h2 := appPauseFleetSite("https://rectick-h2.example.com")

	// The honest history: three sites were down, the breaker tripped naming
	// three, and this tick two of them come back.
	long := time.Now().Add(-6 * time.Hour)
	base := &appPauseRepo{
		fleet: []appPauseSite{recA, recB, fresh, h1, h2},
		breaker: map[uuid.UUID]AppBreakerState{
			tenant: {Tripped: true, TrippedAt: &long, LastAlertAt: &long, LastDownCount: 3},
		},
	}
	repo := &countingRatioRepo{appPauseRepo: base}
	w, mailer := newAggregateRig(t, repo)

	eligible, down := appRatio(base.fleet)
	if appBreakerBarMet(eligible, down, defaultAppAlertBreakerRatio) {
		t.Fatalf("setup: %d down of %d must fall BELOW the bar so the breaker recovers", down, eligible)
	}

	// This tick's transitions: two recoveries and one new incident.
	fires := []pendingAppFire{fireFor(recA, false), fireFor(recB, false), fireFor(fresh, true)}
	for i := range fires {
		fires[i].site.TenantID = tenant
	}
	w.resolveAppAlerts(ctx, fires, time.Now())

	if base.breaker[tenant].Tripped {
		t.Fatalf("the breaker must have recovered on this tick, or the FireRecovery branch was never entered: %+v", base.breaker[tenant])
	}

	subjects, bodies := mailer.sent()
	if len(subjects) != 2 {
		t.Fatalf("expected the recovery aggregate PLUS the new incident's own alert, got %d: %v (bodies %v)", len(subjects), subjects, bodies)
	}

	joined := strings.Join(subjects, " | ")
	if !strings.Contains(joined, "APP ALERTS RESUMED") {
		t.Fatalf("the recovery aggregate must go out, got %v", subjects)
	}
	// The line under test. The aggregate says nothing about `fresh`.
	freshAlerted := false
	for _, s := range subjects {
		if strings.Contains(s, "[WPMgr] APP DOWN:") && strings.Contains(s, fresh.url) {
			freshAlerted = true
		}
	}
	if !freshAlerted {
		t.Fatalf("a site that crossed newly INTO an incident on a recovery tick is not covered by the recovery aggregate: its individual alert is the only notification it will ever get, and AppTransition is consumed. Got %v", subjects)
	}
	// The other side of appFireDownOnly: the aggregate already announced these
	// two, so an individual recovery mail on top of it double-notifies.
	for _, never := range []string{recA.url, recB.url} {
		if strings.Contains(joined, "[WPMgr] APP RECOVERED:") && strings.Contains(joined, never) {
			t.Fatalf("the recovery aggregate already spoke for %s: it must not also get an individual recovery mail, got %v", never, subjects)
		}
	}
}

// ---------------------------------------------------------------------------
// The over-fire control. A guard that reddens correct work guards nothing.
// ---------------------------------------------------------------------------

// TestBreakerPathsUnchangedWhenNothingFailsAndNothingIsPaused is the control
// for all four tests above: the same shape of fleet, no pause, no failing
// query, no seeded breaker state. The ordinary paths must behave exactly as
// they always did — one individual alert per down site below the bar, one
// aggregate above it — so none of the fallbacks above can be widened into a
// blanket "send everything" that pages on ticks it should stay quiet on.
//
// RED: make fireAppIndividually fire unconditionally in every branch, or make
// resolveAppAlerts skip the breaker entirely. Half 2 gains four extra mails.
func TestBreakerPathsUnchangedWhenNothingFailsAndNothingIsPaused(t *testing.T) {
	ctx := context.Background()

	// --- Below the bar: individual alerts, no aggregate. -----------------
	tenantA := uuid.New()
	lowDown := appPauseFleetSite("https://control-low-down.example.com")
	lowDown.inIncident = true
	lowFleet := []appPauseSite{lowDown}
	for i := 0; i < 9; i++ {
		lowFleet = append(lowFleet, appPauseFleetSite("https://control-low-h.example.com/"+uuid.NewString()[:8]))
	}
	lowBase := &appPauseRepo{fleet: lowFleet, breaker: map[uuid.UUID]AppBreakerState{}}
	wLow, lowMailer := newAggregateRig(t, &countingRatioRepo{appPauseRepo: lowBase})

	wLow.resolveAppAlerts(ctx, firesFor(t, []appPauseSite{lowDown}, tenantA), time.Now())

	lowSubjects, _ := lowMailer.sent()
	if len(lowSubjects) != 1 || !strings.Contains(lowSubjects[0], "[WPMgr] APP DOWN:") || !strings.Contains(lowSubjects[0], lowDown.url) {
		t.Fatalf("1 down of 10 must produce exactly its own individual alert, got %v", lowSubjects)
	}
	if lowBase.breaker[tenantA].Tripped {
		t.Fatalf("1 down of 10 must not trip: %+v", lowBase.breaker[tenantA])
	}

	// --- Above the bar: one aggregate, no individual alerts. -------------
	tenantB := uuid.New()
	hiDown := make([]appPauseSite, 0, 4)
	hiFleet := make([]appPauseSite, 0, 8)
	for i := 0; i < 4; i++ {
		s := appPauseFleetSite("https://control-hi-down.example.com/" + uuid.NewString()[:8])
		s.inIncident = true
		hiDown = append(hiDown, s)
		hiFleet = append(hiFleet, s)
	}
	for i := 0; i < 4; i++ {
		hiFleet = append(hiFleet, appPauseFleetSite("https://control-hi-h.example.com/"+uuid.NewString()[:8]))
	}
	hiBase := &appPauseRepo{fleet: hiFleet, breaker: map[uuid.UUID]AppBreakerState{}}
	wHi, hiMailer := newAggregateRig(t, &countingRatioRepo{appPauseRepo: hiBase})

	wHi.resolveAppAlerts(ctx, firesFor(t, hiDown, tenantB), time.Now())

	hiSubjects, hiBodies := hiMailer.sent()
	if len(hiSubjects) != 1 {
		t.Fatalf("4 down of 8 must collapse into exactly one aggregate, got %d: %v", len(hiSubjects), hiSubjects)
	}
	if !strings.Contains(hiSubjects[0], "APP ALERTS SUPPRESSED") || !strings.Contains(hiSubjects[0], "4/8") {
		t.Fatalf("expected the aggregate quoting 4/8, got %q", hiSubjects[0])
	}
	for _, s := range hiDown {
		if !strings.Contains(hiBodies[0], s.name) {
			t.Fatalf("the aggregate must name every down site it replaced, missing %s, body:\n%s", s.name, hiBodies[0])
		}
	}
	if !hiBase.breaker[tenantB].Tripped {
		t.Fatalf("4 of 8 must trip: %+v", hiBase.breaker[tenantB])
	}
}
