// app_breaker_fallback_test.go — GH #414 phase 3, the two lines the mutation
// run found unproven. Nothing here changes production code; both tests pin
// behaviour that already ships and that no existing test reddens on.
//
// Both are driven through the REAL resolveAppAlerts over the shared fake from
// app_alert_pause_test.go, because both defects live in resolveAppAlerts'
// control flow rather than in a leaf function: a test that calls the leaf
// directly cannot reach either. TestAggregateWithheldWhenTheRatioReReadFails is
// the near miss — it calls fireAppAggregate directly, so it never enters the
// `!pop.resolved` branch of resolveAppAlerts at all, and deleting that branch's
// body left every test in the package green.
package uptime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// The per-site fallback when the population cannot be substantiated.
// ---------------------------------------------------------------------------

// TestUnresolvedPopulationStillAlertsUnpausedDownSitesIndividually pins the
// `w.fireAppIndividually(ctx, fires, now)` inside resolveAppAlerts'
// `if !pop.resolved` branch.
//
// Withholding the aggregate is the correct answer to "a pause landed and the
// ratio re-read failed, so I cannot say how many sites are down" — but the
// aggregate is a notification, not the alert. The sites that are genuinely,
// unpausedly down still have to be told about individually, exactly as the
// ratio-query failure a few lines above already falls back to doing. Without
// that one line, a transient blip on ONE re-read turns into total silence for
// every unpaused down site in the tenant: AppTransition is transition-only, so
// the swallowed incident is swallowed permanently, not merely delayed a tick.
// That is the same defect class the pause work exists to close, sitting inside
// the fix for it.
//
// The rig reaches the branch the only way production does. `late` is paused in
// the window between the ratio query and the dispatch, so the population needs
// a re-read; failFrom: 2 fails that second read (the first, tenant-level read
// must succeed or the earlier ratio-failure fallback is what runs and the
// branch under test is never entered).
//
// RED: delete the fireAppIndividually call from the `!pop.resolved` branch —
// `stays` is genuinely down, unpaused and alert-eligible, and hears nothing.
func TestUnresolvedPopulationStillAlertsUnpausedDownSitesIndividually(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	stays := appPauseFleetSite("https://unresolved-stays.example.com")
	stays.inIncident = true
	late := appPauseFleetSite("https://unresolved-late.example.com")
	late.inIncident, late.pausedAfterTheRow = true, true
	up1 := appPauseFleetSite("https://unresolved-up1.example.com")
	up2 := appPauseFleetSite("https://unresolved-up2.example.com")

	base := &appPauseRepo{
		fleet:   []appPauseSite{stays, late, up1, up2},
		breaker: map[uuid.UUID]AppBreakerState{},
	}
	repo := &countingRatioRepo{appPauseRepo: base, failFrom: 2}
	w, mailer := newAggregateRig(t, repo)

	fires := []pendingAppFire{fireFor(stays, true), fireFor(late, true)}
	for i := range fires {
		fires[i].site.TenantID = tenant
	}
	w.resolveAppAlerts(ctx, fires, time.Now())

	// The branch was actually entered: the tenant read succeeded (2 down of 4
	// clears the bar and wants to trip), and the dispatch-time re-read that the
	// dropped site forces is the one that failed. Without this, a future change
	// that never reaches the re-read at all could satisfy every assertion below
	// for the wrong reason.
	if got := repo.ratioReads(); got != 2 {
		t.Fatalf("expected the tenant read plus the failing dispatch-time re-read (2 reads), got %d", got)
	}

	subjects, bodies := mailer.sent()
	if len(subjects) != 1 {
		t.Fatalf("an unsubstantiated population withholds the AGGREGATE only; the unpaused down site must still be alerted individually, got %d mails: %v (bodies %v)", len(subjects), subjects, bodies)
	}
	if !strings.Contains(subjects[0], "[WPMgr] APP DOWN:") || !strings.Contains(subjects[0], stays.url) {
		t.Fatalf("expected the ordinary per-site app-down alert for %s, got %q", stays.url, subjects[0])
	}
	// The fallback is per-site, so it re-reads the pause per site: it must not
	// become a blanket "send everything" that pages for the paused site too.
	if strings.Contains(subjects[0], late.url) || strings.Contains(bodies[0], late.url) {
		t.Fatalf("the site paused mid-dispatch must stay silent, got subject %q body:\n%s", subjects[0], bodies[0])
	}
	if strings.Contains(subjects[0], "APP ALERTS SUPPRESSED") {
		t.Fatalf("the aggregate itself must still be withheld, got %q", subjects[0])
	}
	if base.breaker[tenant].Tripped {
		t.Fatalf("a population that could not be substantiated must not trip the breaker: %+v", base.breaker[tenant])
	}
}

// ---------------------------------------------------------------------------
// The breaker's threshold boundary, from both sides.
// ---------------------------------------------------------------------------

// TestBreakerBarIsStrictAtItsThreshold pins appBreakerBarMet's strict `>`
// against the exact ratio, from both directions, so an off-by-one either way
// reddens. Changing that `>` to `>=` survived every other test in the package.
//
// The boundary carries more weight since the pause predicate landed: pausing a
// site shrinks `eligible`, which MOVES a tenant across the bar. The two halves
// below are the same fleet either side of one pause, and they are the exact
// numbers an operator can produce by pausing one healthy site.
//
//	8 eligible, 2 down: 2 > 0.25*8 == 2 is FALSE. No trip; both down sites get
//	their ordinary per-site alerts.
//	7 eligible, 2 down: 2 > 0.25*7 == 1.75 is TRUE. Trips; one aggregate,
//	naming both.
//
// RED, loosening: `>` to `>=` — half 1 trips and collapses two per-site alerts
// into an aggregate nobody should have received.
// RED, tightening: any change that raises the bar (a `+1`, a `>=` on the count
// floor turned into `>`) — half 2 stops tripping and the aggregate disappears.
func TestBreakerBarIsStrictAtItsThreshold(t *testing.T) {
	ctx := context.Background()

	// --- Half 1: exactly AT the ratio. Must NOT trip. --------------------
	tenantA := uuid.New()
	downA := appPauseFleetSite("https://bar-at-down-a.example.com")
	downA.inIncident = true
	downB := appPauseFleetSite("https://bar-at-down-b.example.com")
	downB.inIncident = true
	atFleet := []appPauseSite{downA, downB}
	for _, s := range []string{"h1", "h2", "h3", "h4", "h5", "h6"} {
		atFleet = append(atFleet, appPauseFleetSite("https://bar-at-"+s+".example.com"))
	}

	atBase := &appPauseRepo{fleet: atFleet, breaker: map[uuid.UUID]AppBreakerState{}}
	atRepo := &countingRatioRepo{appPauseRepo: atBase}
	wAt, atMailer := newAggregateRig(t, atRepo)

	atFires := []pendingAppFire{fireFor(downA, true), fireFor(downB, true)}
	for i := range atFires {
		atFires[i].site.TenantID = tenantA
	}
	wAt.resolveAppAlerts(ctx, atFires, time.Now())

	atSubjects, atBodies := atMailer.sent()
	for _, s := range atSubjects {
		if strings.Contains(s, "APP ALERTS SUPPRESSED") {
			t.Fatalf("2 down of 8 eligible is exactly AT the 25%% ratio, not more than it: the breaker must not trip, got subjects=%v bodies=%v", atSubjects, atBodies)
		}
	}
	if atBase.breaker[tenantA].Tripped {
		t.Fatalf("2 of 8 must leave the breaker untripped: %+v", atBase.breaker[tenantA])
	}
	if len(atSubjects) != 2 {
		t.Fatalf("both down sites must get their ordinary per-site alert, got %d: %v", len(atSubjects), atSubjects)
	}
	for _, want := range []string{downA.url, downB.url} {
		found := false
		for _, s := range atSubjects {
			if strings.Contains(s, "[WPMgr] APP DOWN:") && strings.Contains(s, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected an individual app-down alert naming %s, got %v", want, atSubjects)
		}
	}

	// --- Half 2: one healthy site paused, so OVER the ratio. Must trip. --
	tenantB := uuid.New()
	overDownA := appPauseFleetSite("https://bar-over-down-a.example.com")
	overDownA.inIncident = true
	overDownB := appPauseFleetSite("https://bar-over-down-b.example.com")
	overDownB.inIncident = true
	// The single difference from half 1: one of the eight is monitoring-paused
	// before the tick, so GetTenantAppAlertRatio counts 7, not 8.
	pausedHealthy := appPauseFleetSite("https://bar-over-h1.example.com")
	pausedHealthy.paused = true
	overFleet := []appPauseSite{overDownA, overDownB, pausedHealthy}
	for _, s := range []string{"h2", "h3", "h4", "h5", "h6"} {
		overFleet = append(overFleet, appPauseFleetSite("https://bar-over-"+s+".example.com"))
	}

	overBase := &appPauseRepo{fleet: overFleet, breaker: map[uuid.UUID]AppBreakerState{}}
	overRepo := &countingRatioRepo{appPauseRepo: overBase}
	wOver, overMailer := newAggregateRig(t, overRepo)

	overFires := []pendingAppFire{fireFor(overDownA, true), fireFor(overDownB, true)}
	for i := range overFires {
		overFires[i].site.TenantID = tenantB
	}
	wOver.resolveAppAlerts(ctx, overFires, time.Now())

	overSubjects, overBodies := overMailer.sent()
	if len(overSubjects) != 1 {
		t.Fatalf("2 down of 7 eligible is over the 25%% ratio: exactly one aggregate must go out, got %d: %v", len(overSubjects), overSubjects)
	}
	if !strings.Contains(overSubjects[0], "APP ALERTS SUPPRESSED") || !strings.Contains(overSubjects[0], "2/7") {
		t.Fatalf("expected the aggregate quoting the pause-filtered 2/7, got %q", overSubjects[0])
	}
	for _, want := range []string{overDownA.url, overDownB.url} {
		if !strings.Contains(overBodies[0], want) {
			t.Fatalf("the aggregate must name %s, body:\n%s", want, overBodies[0])
		}
	}
	if !overBase.breaker[tenantB].Tripped {
		t.Fatalf("2 of 7 must trip the breaker: %+v", overBase.breaker[tenantB])
	}
}
