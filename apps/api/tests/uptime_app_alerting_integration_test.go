// uptime_app_alerting_integration_test.go - GH #291 Phase 3: application-
// health ALERTING. End-to-end coverage (real Postgres + a real
// uptime.ProbeWorker.Sweep + a real uptime.Dispatcher over a capturing
// Mailer) for the three load-bearing properties the design demands:
//
//   - A site that has never been conclusively observed healthy must NEVER
//     fire an app alert, no matter how many sweeps observe it conclusively
//     down (TestAppAlerting_NeverHealthySiteNeverFires).
//   - A site that proves itself healthy, then conclusively breaks, fires
//     exactly ONE down alert and, on recovery, exactly ONE recovery alert
//     (TestAppAlerting_IndividualDownAndRecovery).
//   - When more than the configured ratio of a tenant's alert-eligible sites
//     are simultaneously app-down, the fleet circuit breaker collapses every
//     individual alert into exactly ONE aggregate notification, and exactly
//     ONE more on recovery (TestAppAlerting_BreakerTripsAndCollapses).
package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
	"github.com/mosamlife/wpmgr/apps/api/internal/uptime"
)

// appAlertCapturedSend is one email the appAlertCapturingMailer recorded.
type appAlertCapturedSend struct {
	recipients []string
	subject    string
	body       string
}

// appAlertCapturingMailer is a uptime.Mailer stub that records every send instead of
// delivering it, so a test can assert exactly how many alerts (and of what
// kind) actually dispatched.
type appAlertCapturingMailer struct {
	mu    sync.Mutex
	sends []appAlertCapturedSend
}

func (m *appAlertCapturingMailer) Send(_ context.Context, recipients []string, subject, body string) (uptime.SendResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sends = append(m.sends, appAlertCapturedSend{recipients: recipients, subject: subject, body: body})
	return uptime.SendResult{Status: uptime.SendResultSent}, nil
}

func (m *appAlertCapturingMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sends)
}

func (m *appAlertCapturingMailer) snapshot() []appAlertCapturedSend {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]appAlertCapturedSend, len(m.sends))
	copy(out, m.sends)
	return out
}

// appAlertNoopAuditSink discards every audit record - these tests assert on
// dispatched EMAILS, not the audit trail (already covered by
// internal/uptime/alerts_test.go's Dispatcher tests).
type appAlertNoopAuditSink struct{}

func (appAlertNoopAuditSink) Record(_ context.Context, _ audit.Event) (audit.Entry, error) {
	return audit.Entry{}, nil
}

// appAlertTestSite is one fake enrolled site whose /wp-json/ response can be
// flipped between conclusively-healthy (200 JSON) and conclusively-broken
// (HTTP 500) mid-test.
type appAlertTestSite struct {
	siteID  uuid.UUID
	healthy atomic.Bool
	srv     *httptest.Server
}

func (s *appAlertTestSite) setHealthy(v bool) { s.healthy.Store(v) }

// newAppAlertTestSite enrolls a site whose reachability target (site root)
// always answers a plain 200 (keeping the FROZEN reachability signal
// unambiguously up throughout, so only the app-health signal is under test)
// and whose app-probe target (/wp-json/) answers according to the returned
// site's toggleable `healthy` flag. last_seen_at is backdated (mirrors the
// GH #291 Phase 2 golden test) so B0 (agent ground truth) never
// short-circuits the direct measurement these tests are exercising.
func newAppAlertTestSite(t *testing.T, pool *db.Pool, admin *db.Pool, tenant uuid.UUID) *appAlertTestSite {
	t.Helper()
	ts := &appAlertTestSite{}
	ts.healthy.Store(true)
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/wp-json/" {
			if ts.healthy.Load() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.srv.Close)

	s := enrollFakeSite(t, pool, tenant, ts.srv.URL)
	ts.siteID = s.ID
	ctx := context.Background()
	if _, err := admin.Exec(ctx, "UPDATE sites SET last_seen_at = now() - interval '1 hour' WHERE id = $1", s.ID); err != nil {
		t.Fatalf("backdate last_seen_at: %v", err)
	}
	return ts
}

// appAlertHarness bundles a real ProbeWorker (real Postgres repo, real app
// prober, real Dispatcher) wired for fast, deterministic app-health alert
// testing: reachability threshold is irrelevant (every site always answers a
// cached-style 200 on "/"), the app-probe cadence ratio is 1 (every sweep
// attempts a verdict - mirrors the GH #291 Phase 2 golden test), and the
// app-alert threshold is 1 (fires on the FIRST conclusive-false once
// ever_app_up is already true), so a handful of Sweep calls is enough to
// exercise every transition deterministically.
type appAlertHarness struct {
	pool   *db.Pool
	worker *uptime.ProbeWorker
	repo   uptime.Repo
	mailer *appAlertCapturingMailer
}

func newAppAlertHarness(t *testing.T, breakerRatio float64) *appAlertHarness {
	t.Helper()
	pool := startPostgres(t)
	store := metrics.NewPostgres(pool, nil)
	repo := uptime.NewRepo(pool)
	prober := uptime.NewProber(loopbackClient(), 5*time.Second)
	appProber := uptime.NewAppProber(loopbackClient(), 5*time.Second)
	mailer := &appAlertCapturingMailer{}
	dispatcher := uptime.NewDispatcher(mailer, nil, appAlertNoopAuditSink{}, nil)

	w := uptime.NewProbeWorker(repo, prober, store, dispatcher, nil, nil, 5, 2)
	// ratio 1: probeInterval == appProbeInterval, so every sweep tick
	// attempts an app-probe verdict for every site (no cadence-skip luck).
	w.SetAppProber(appProber, time.Minute, time.Minute)
	w.SetAppAlertConfig(1, breakerRatio)

	return &appAlertHarness{pool: pool, worker: w, repo: repo, mailer: mailer}
}

// enableAlerting saves an AlertConfig with both the reachability channel and
// the NEW app-health gate turned on - the default-off rollout decision is
// tested separately (see the migration-default test); these tests exercise
// the alerting MACHINERY once it is on.
func (h *appAlertHarness) enableAlerting(t *testing.T, tenant uuid.UUID) {
	t.Helper()
	if _, err := h.repo.UpsertAlertConfig(context.Background(), uptime.AlertConfig{
		TenantID:         tenant,
		EmailRecipients:  []string{"ops@example.com"},
		Enabled:          true,
		AppAlertsEnabled: true,
		VulnMinSeverity:  "high",
	}); err != nil {
		t.Fatalf("enable alerting: %v", err)
	}
}

func (h *appAlertHarness) sweep(t *testing.T) {
	t.Helper()
	h.sweepAt(t, time.Now())
}

// sweepAt runs one sweep tick at an EXPLICIT, caller-controlled `now` -
// ProbeWorker.Sweep threads this straight through to
// TransitionAlertState/TransitionAppAlertBreaker, so a test can simulate
// elapsed time (e.g. crossing the fleet circuit breaker's update
// min-interval, GH #291 Phase 3 Fix 3) deterministically, without an actual
// wall-clock wait.
func (h *appAlertHarness) sweepAt(t *testing.T, now time.Time) {
	t.Helper()
	if _, err := h.worker.Sweep(context.Background(), now); err != nil {
		t.Fatalf("sweep: %v", err)
	}
}

// TestAppAlerting_NeverHealthySiteNeverFires is the core "never page on a
// site we've never seen healthy" guarantee (design doc section 1), proven
// end-to-end: a site whose /wp-json/ returns HTTP 500 from the very first
// sweep never fires an app-down alert no matter how many conclusive-false
// sweeps accumulate - almost always a blocked/disabled REST route, not a
// broken site.
func TestAppAlerting_NeverHealthySiteNeverFires(t *testing.T) {
	h := newAppAlertHarness(t, 0.25)
	admin := connectAdmin(t, h.pool)
	defer admin.Close()

	tenant := seedTenant(t, h.pool, "app-alert-never-healthy-"+uuid.NewString()[:8])
	h.enableAlerting(t, tenant)

	site := newAppAlertTestSite(t, h.pool, admin, tenant)
	site.setHealthy(false) // conclusively broken from sweep 1 onward

	for i := 0; i < 6; i++ {
		h.sweep(t)
	}

	if got := h.mailer.count(); got != 0 {
		t.Fatalf("expected 0 emails for a site that has never been conclusively healthy, got %d: %+v", got, h.mailer.snapshot())
	}
}

// TestAppAlerting_IndividualDownAndRecovery proves the full per-site
// lifecycle once ever_app_up is true: exactly one down alert, then exactly
// one recovery alert - never a repeat on either side.
func TestAppAlerting_IndividualDownAndRecovery(t *testing.T) {
	h := newAppAlertHarness(t, 0.25)
	admin := connectAdmin(t, h.pool)
	defer admin.Close()

	tenant := seedTenant(t, h.pool, "app-alert-lifecycle-"+uuid.NewString()[:8])
	h.enableAlerting(t, tenant)

	site := newAppAlertTestSite(t, h.pool, admin, tenant)

	// Sweep 1: healthy - proves EverAppUp, no alert.
	h.sweep(t)
	if got := h.mailer.count(); got != 0 {
		t.Fatalf("after the first healthy sweep: expected 0 emails, got %d", got)
	}

	// Sweep 2: conclusively broken - threshold=1, EverAppUp already true -
	// fires exactly one down alert.
	site.setHealthy(false)
	h.sweep(t)
	if got := h.mailer.count(); got != 1 {
		t.Fatalf("after going down: expected exactly 1 email, got %d: %+v", got, h.mailer.snapshot())
	}
	if subj := h.mailer.snapshot()[0].subject; !strings.Contains(subj, "APP DOWN") {
		t.Fatalf("expected an APP DOWN subject, got %q", subj)
	}

	// Steady-state down: no repeat.
	h.sweep(t)
	h.sweep(t)
	if got := h.mailer.count(); got != 1 {
		t.Fatalf("steady-state down: expected no repeat alert, still 1 email, got %d", got)
	}

	// Recovery: exactly one more email.
	site.setHealthy(true)
	h.sweep(t)
	if got := h.mailer.count(); got != 2 {
		t.Fatalf("after recovery: expected exactly 2 emails total, got %d: %+v", got, h.mailer.snapshot())
	}
	if subj := h.mailer.snapshot()[1].subject; !strings.Contains(subj, "APP RECOVERED") {
		t.Fatalf("expected an APP RECOVERED subject, got %q", subj)
	}

	// Steady-state up: no repeat recovery.
	h.sweep(t)
	if got := h.mailer.count(); got != 2 {
		t.Fatalf("steady-state up: expected no repeat recovery, still 2 emails, got %d", got)
	}
}

// TestAppAlerting_BreakerTripsAndCollapses is the anti-storm guard (design
// doc section 2): 4 sites in ONE tenant all go simultaneously app-down after
// each has proven itself healthy - 100% of the tenant's alert-eligible
// sites, comfortably over the 25% default ratio. Exactly ONE aggregate
// email fires, never 4 individual ones; recovery collapses to exactly one
// more.
func TestAppAlerting_BreakerTripsAndCollapses(t *testing.T) {
	h := newAppAlertHarness(t, 0.25)
	admin := connectAdmin(t, h.pool)
	defer admin.Close()

	tenant := seedTenant(t, h.pool, "app-alert-breaker-"+uuid.NewString()[:8])
	h.enableAlerting(t, tenant)

	sites := make([]*appAlertTestSite, 4)
	for i := range sites {
		sites[i] = newAppAlertTestSite(t, h.pool, admin, tenant)
	}

	// Sweep 1: every site healthy - proves EverAppUp for all four, no alert.
	h.sweep(t)
	if got := h.mailer.count(); got != 0 {
		t.Fatalf("after the first healthy sweep: expected 0 emails, got %d", got)
	}

	// All four go conclusively down on the SAME tick: ratio = 4/4 = 100% >
	// 25% - the breaker trips and collapses what would otherwise be 4
	// individual down alerts into exactly 1 aggregate notification.
	for _, s := range sites {
		s.setHealthy(false)
	}
	h.sweep(t)
	if got := h.mailer.count(); got != 1 {
		t.Fatalf("breaker trip: expected exactly 1 aggregate email (not one per site), got %d: %+v", got, h.mailer.snapshot())
	}
	sent := h.mailer.snapshot()[0]
	if !strings.Contains(sent.subject, "SUPPRESSED") {
		t.Fatalf("expected the aggregate SUPPRESSED subject, got %q", sent.subject)
	}
	if !strings.Contains(sent.body, "4 of 4") {
		t.Fatalf("expected the aggregate body to state the exact count (4 of 4), got body: %q", sent.body)
	}

	// Steady-state tripped: no repeat aggregate, and the individually-
	// suppressed sites still do not leak their own alert.
	h.sweep(t)
	if got := h.mailer.count(); got != 1 {
		t.Fatalf("steady-state tripped: expected no repeat, still 1 email, got %d: %+v", got, h.mailer.snapshot())
	}

	// All four recover together: exactly ONE aggregate recovery email.
	for _, s := range sites {
		s.setHealthy(true)
	}
	h.sweep(t)
	if got := h.mailer.count(); got != 2 {
		t.Fatalf("breaker recovery: expected exactly 2 emails total, got %d: %+v", got, h.mailer.snapshot())
	}
	if subj := h.mailer.snapshot()[1].subject; !strings.Contains(subj, "RESUMED") {
		t.Fatalf("expected the aggregate RESUMED subject, got %q", subj)
	}
}

// TestAppAlerting_BreakerRecoveryTickStillFiresNewIndividualDown is GH #291
// Phase 3 Fix 1: on the EXACT tick the fleet circuit breaker recovers, a
// site that crosses newly INTO an incident (unrelated to the sites that just
// recovered) must still be dispatched individually - the aggregate recovery
// notification speaks only for the RECOVERY transitions it collapsed while
// tripped, never for a same-tick, unrelated NEW incident. Before this fix,
// that new incident's alert was PERMANENTLY swallowed: AppTransition is
// transition-only, so once the FireDown was persisted without being
// dispatched, it could never re-fire.
func TestAppAlerting_BreakerRecoveryTickStillFiresNewIndividualDown(t *testing.T) {
	h := newAppAlertHarness(t, 0.25)
	admin := connectAdmin(t, h.pool)
	defer admin.Close()

	tenant := seedTenant(t, h.pool, "app-alert-fix1-"+uuid.NewString()[:8])
	h.enableAlerting(t, tenant)

	sites := make([]*appAlertTestSite, 4)
	for i := range sites {
		sites[i] = newAppAlertTestSite(t, h.pool, admin, tenant)
	}
	down0, down1, down2, staysUp := sites[0], sites[1], sites[2], sites[3]

	// Sweep 1: every site healthy - proves EverAppUp for all four.
	h.sweep(t)
	if got := h.mailer.count(); got != 0 {
		t.Fatalf("after the first healthy sweep: expected 0 emails, got %d", got)
	}

	// Three of four go down: 3/4 = 75% > 25%, down=3 >= 2 - the breaker
	// trips, collapsing all three into exactly 1 aggregate email.
	down0.setHealthy(false)
	down1.setHealthy(false)
	down2.setHealthy(false)
	h.sweep(t)
	if got := h.mailer.count(); got != 1 {
		t.Fatalf("breaker trip: expected exactly 1 aggregate email, got %d: %+v", got, h.mailer.snapshot())
	}
	if subj := h.mailer.snapshot()[0].subject; !strings.Contains(subj, "SUPPRESSED") {
		t.Fatalf("expected the aggregate SUPPRESSED subject, got %q", subj)
	}

	// On the SAME tick: all three previously-down sites recover AND the
	// fourth, previously-untouched site goes conclusively down for the
	// FIRST time. Net down count = 1/4 = 25% (not strictly > 25%, and below
	// the absolute floor of 2) - the breaker recovers. The fourth site's
	// FireDown transition is NOT a recovery and is NOT covered by the
	// aggregate recovery notification - it must still be dispatched
	// individually, in ADDITION to the one aggregate recovery email.
	down0.setHealthy(true)
	down1.setHealthy(true)
	down2.setHealthy(true)
	staysUp.setHealthy(false)
	h.sweep(t)

	if got := h.mailer.count(); got != 3 {
		t.Fatalf("breaker-recovery tick: expected exactly 3 emails total (1 trip + 1 aggregate recovery + 1 individual new-down), got %d: %+v",
			got, h.mailer.snapshot())
	}
	snap := h.mailer.snapshot()
	foundRecoveryAggregate, foundIndividualDown := false, false
	for _, s := range snap[1:] {
		switch {
		case strings.Contains(s.subject, "RESUMED"):
			foundRecoveryAggregate = true
		case strings.Contains(s.subject, "APP DOWN") && strings.Contains(s.body, staysUp.srv.URL):
			foundIndividualDown = true
		}
	}
	if !foundRecoveryAggregate {
		t.Fatalf("expected an aggregate RESUMED email among the last two, got: %+v", snap[1:])
	}
	if !foundIndividualDown {
		t.Fatalf("expected an INDIVIDUAL APP DOWN email for the newly-down site (GH #291 Phase 3 Fix 1 - this is the alert that used to be silently swallowed), got: %+v", snap[1:])
	}

	// The three sites that recovered must NOT ALSO get an individual
	// recovery email (that would be the double-dispatch bug this fix must
	// not reintroduce) - already implied by the exact count of 3 above, but
	// assert explicitly that neither an "APP RECOVERED" subject appears.
	for _, s := range snap[1:] {
		if strings.Contains(s.subject, "APP RECOVERED") {
			t.Fatalf("unexpected individual APP RECOVERED email - the aggregate recovery must speak for these, not a per-site one: %+v", s)
		}
	}
}

// TestAppAlerting_RevokedSiteExcludedFromRatioAndFire is GH #291 Phase 3 Fix
// 2: a revoked site must be excluded from app-health alerting ENTIRELY - it
// must never dispatch its own individual alert, and it must never be
// counted in the fleet circuit breaker's eligible/down denominator, exactly
// matching GetTenantAppAlertRatio's own exclusion. Before this fix, the fire
// path (processSite) checked ONLY AppAlertsDisabled, so a revoked site could
// still open its own incident and dispatch an alert while being invisible to
// the ratio query - the two disagreeing about which sites count.
func TestAppAlerting_RevokedSiteExcludedFromRatioAndFire(t *testing.T) {
	h := newAppAlertHarness(t, 0.25)
	admin := connectAdmin(t, h.pool)
	defer admin.Close()

	tenant := seedTenant(t, h.pool, "app-alert-fix2-"+uuid.NewString()[:8])
	h.enableAlerting(t, tenant)

	siteA := newAppAlertTestSite(t, h.pool, admin, tenant)
	siteB := newAppAlertTestSite(t, h.pool, admin, tenant)

	// Sweep 1: both healthy - proves EverAppUp for both.
	h.sweep(t)
	if got := h.mailer.count(); got != 0 {
		t.Fatalf("after the first healthy sweep: expected 0 emails, got %d", got)
	}

	// Revoke siteB (operator-initiated "stop managing this site" - mirrors
	// the exact exclusion GetTenantAppAlertRatio's WHERE clause enforces),
	// then make it conclusively break.
	if _, err := admin.Exec(context.Background(),
		"UPDATE sites SET connection_state = 'revoked' WHERE id = $1", siteB.siteID); err != nil {
		t.Fatalf("revoke siteB: %v", err)
	}
	siteB.setHealthy(false)
	h.sweep(t)

	if got := h.mailer.count(); got != 0 {
		t.Fatalf("a revoked site going conclusively down must NEVER dispatch its own alert, got %d email(s): %+v",
			got, h.mailer.snapshot())
	}

	// The ratio query must ALSO exclude the revoked site: eligible=1 (siteA
	// only), down=0 (siteA is still healthy) - never eligible=2/down=1.
	eligible, down, err := h.repo.GetTenantAppAlertRatio(context.Background(), tenant)
	if err != nil {
		t.Fatalf("GetTenantAppAlertRatio: %v", err)
	}
	if eligible != 1 || down != 0 {
		t.Fatalf("expected eligible=1 down=0 (revoked siteB excluded from both), got eligible=%d down=%d", eligible, down)
	}

	// siteA, still eligible, alerts normally: proves the fix did not
	// collaterally break an UNRELATED, still-eligible site's own alerting.
	siteA.setHealthy(false)
	h.sweep(t)
	if got := h.mailer.count(); got != 1 {
		t.Fatalf("expected exactly 1 email total (siteA's own down alert; siteB must never contribute), got %d: %+v",
			got, h.mailer.snapshot())
	}
	if subj := h.mailer.snapshot()[0].subject; !strings.Contains(subj, "APP DOWN") {
		t.Fatalf("expected an APP DOWN subject for siteA, got %q", subj)
	}
}

// TestAppAlerting_BreakerSendsUpdateOnMaterialWorseningPastMinInterval is GH
// #291 Phase 3 Fix 3: while the breaker stays tripped, a LATER, materially
// worse down count must not go completely unreported until the eventual
// recovery - an "updated" aggregate fires once the down count has worsened
// past the last-notified checkpoint AND at least the minimum interval has
// elapsed, and never more often than that (no flood).
func TestAppAlerting_BreakerSendsUpdateOnMaterialWorseningPastMinInterval(t *testing.T) {
	h := newAppAlertHarness(t, 0.25)
	admin := connectAdmin(t, h.pool)
	defer admin.Close()

	tenant := seedTenant(t, h.pool, "app-alert-fix3-"+uuid.NewString()[:8])
	h.enableAlerting(t, tenant)

	sites := make([]*appAlertTestSite, 5)
	for i := range sites {
		sites[i] = newAppAlertTestSite(t, h.pool, admin, tenant)
	}

	t0 := time.Now()

	// Sweep 1 (t0): every site healthy - proves EverAppUp for all five.
	h.sweepAt(t, t0)
	if got := h.mailer.count(); got != 0 {
		t.Fatalf("after the first healthy sweep: expected 0 emails, got %d", got)
	}

	// Two of five go down: 2/5 = 40% > 25%, down=2 >= 2 - trips.
	sites[0].setHealthy(false)
	sites[1].setHealthy(false)
	tTrip := t0.Add(time.Minute)
	h.sweepAt(t, tTrip)
	if got := h.mailer.count(); got != 1 {
		t.Fatalf("breaker trip: expected exactly 1 aggregate email, got %d: %+v", got, h.mailer.snapshot())
	}
	if subj := h.mailer.snapshot()[0].subject; !strings.Contains(subj, "SUPPRESSED") || strings.Contains(subj, "STILL") {
		t.Fatalf("expected the INITIAL aggregate SUPPRESSED subject, got %q", subj)
	}

	// A third site goes down too, but BEFORE the update min-interval has
	// elapsed since the trip: no update yet (flood guard).
	sites[2].setHealthy(false)
	tTooSoon := tTrip.Add(10 * time.Minute)
	h.sweepAt(t, tTooSoon)
	if got := h.mailer.count(); got != 1 {
		t.Fatalf("materially worse but before the min interval: expected still 1 email, got %d: %+v", got, h.mailer.snapshot())
	}

	// Same worsened state, now PAST the min interval since the trip: fires
	// exactly one "updated" aggregate. This tick itself has ZERO pending
	// transitions (sites[2] already transitioned on the earlier, suppressed
	// tTooSoon tick and is now steady-down) - proving the update's site list
	// must come from the LIVE current down set (ListTenantAppDownSites), not
	// from this tick's (empty) `fires`, or the update would misleadingly
	// name nobody despite correctly stating a worsened count.
	tPastInterval := tTrip.Add(31 * time.Minute)
	h.sweepAt(t, tPastInterval)
	if got := h.mailer.count(); got != 2 {
		t.Fatalf("materially worse past the min interval: expected exactly 2 emails total, got %d: %+v", got, h.mailer.snapshot())
	}
	updateMail := h.mailer.snapshot()[1]
	if !strings.Contains(updateMail.subject, "STILL SUPPRESSED") {
		t.Fatalf("expected the UPDATED aggregate's distinct subject, got %q", updateMail.subject)
	}
	if !strings.Contains(updateMail.body, "3 of 5") {
		t.Fatalf("expected the updated aggregate body to state the TRUE current count (3 of 5), got body: %q", updateMail.body)
	}
	for _, s := range []*appAlertTestSite{sites[0], sites[1], sites[2]} {
		if !strings.Contains(updateMail.body, s.srv.URL) {
			t.Fatalf("expected the updated aggregate to name every currently-down site (including %s, which transitioned on an EARLIER, throttled tick, not this one), got body: %q",
				s.srv.URL, updateMail.body)
		}
	}

	// Immediately after the update (well inside its own min interval), no
	// further transition occurs - no third email (steady tripped tick).
	h.sweepAt(t, tPastInterval.Add(time.Second))
	if got := h.mailer.count(); got != 2 {
		t.Fatalf("steady tripped tick right after an update: expected still 2 emails, got %d: %+v", got, h.mailer.snapshot())
	}

	// All three down sites recover together: exactly ONE aggregate recovery
	// email, no repeat of the update logic.
	sites[0].setHealthy(true)
	sites[1].setHealthy(true)
	sites[2].setHealthy(true)
	h.sweepAt(t, tPastInterval.Add(2*time.Hour))
	if got := h.mailer.count(); got != 3 {
		t.Fatalf("breaker recovery: expected exactly 3 emails total, got %d: %+v", got, h.mailer.snapshot())
	}
	if subj := h.mailer.snapshot()[2].subject; !strings.Contains(subj, "RESUMED") {
		t.Fatalf("expected the aggregate RESUMED subject, got %q", subj)
	}
}

// TestAppAlerting_StaleTrippedBreakerConvergesWithoutTransitions is GH #291
// Phase 3 Fix 4: a tripped breaker must still converge (recover) even when
// NOTHING transitions for the tenant on a given sweep tick - the ratio can
// shift for reasons entirely unrelated to any individual site's own
// app-alert transition (here: a down site gets revoked, shrinking the
// eligible/down denominator out from under the breaker with zero
// AppTransition involved).
func TestAppAlerting_StaleTrippedBreakerConvergesWithoutTransitions(t *testing.T) {
	h := newAppAlertHarness(t, 0.25)
	admin := connectAdmin(t, h.pool)
	defer admin.Close()

	tenant := seedTenant(t, h.pool, "app-alert-fix4-"+uuid.NewString()[:8])
	h.enableAlerting(t, tenant)

	sites := make([]*appAlertTestSite, 4)
	for i := range sites {
		sites[i] = newAppAlertTestSite(t, h.pool, admin, tenant)
	}

	// Sweep 1: every site healthy - proves EverAppUp for all four.
	h.sweep(t)
	if got := h.mailer.count(); got != 0 {
		t.Fatalf("after the first healthy sweep: expected 0 emails, got %d", got)
	}

	// Two of four go down: 2/4 = 50% > 25%, down=2 >= 2 - trips.
	sites[0].setHealthy(false)
	sites[1].setHealthy(false)
	h.sweep(t)
	if got := h.mailer.count(); got != 1 {
		t.Fatalf("breaker trip: expected exactly 1 aggregate email, got %d: %+v", got, h.mailer.snapshot())
	}

	// Revoke ONE of the two down sites WITHOUT touching either site's own
	// probe outcome (both stay exactly as broken/healthy as before) - this
	// shrinks eligible from 4 to 3 and down from 2 to 1, pulling the ratio
	// (1/3 = 33%... but the ABSOLUTE floor of 2 is what actually matters
	// here: down=1 < minAppAlertBreakerDownCount=2) below the trip
	// condition, with ZERO AppTransition for ANY site this tick: sites[0]
	// is now revoked (appAlertEligible=false, so it is skipped entirely -
	// no read, no write, no transition), sites[1] stays steady-down (no
	// transition, already in_incident), sites[2]/sites[3] stay steady-up
	// (no transition). `pending` for this tenant is therefore completely
	// EMPTY this tick - the exact "down sites stop transitioning" case Fix
	// 4 exists for.
	if _, err := admin.Exec(context.Background(),
		"UPDATE sites SET connection_state = 'revoked' WHERE id = $1", sites[0].siteID); err != nil {
		t.Fatalf("revoke sites[0]: %v", err)
	}
	h.sweep(t)

	if got := h.mailer.count(); got != 2 {
		t.Fatalf("a tripped breaker whose ratio fell below threshold via an unrelated revoke (zero transitions this tick) must still converge and fire exactly one recovery email; got %d total: %+v",
			got, h.mailer.snapshot())
	}
	if subj := h.mailer.snapshot()[1].subject; !strings.Contains(subj, "RESUMED") {
		t.Fatalf("expected the aggregate RESUMED subject, got %q", subj)
	}

	// Confirm the breaker's OWN persisted state actually converged (not
	// merely that an email happened to fire): a fresh ratio read agrees, and
	// steady state afterward produces no further email.
	eligible, down, err := h.repo.GetTenantAppAlertRatio(context.Background(), tenant)
	if err != nil {
		t.Fatalf("GetTenantAppAlertRatio: %v", err)
	}
	if eligible != 3 || down != 1 {
		t.Fatalf("expected eligible=3 down=1 after the revoke, got eligible=%d down=%d", eligible, down)
	}
	h.sweep(t)
	if got := h.mailer.count(); got != 2 {
		t.Fatalf("steady state after convergence: expected no further email, got %d: %+v", got, h.mailer.snapshot())
	}
}
