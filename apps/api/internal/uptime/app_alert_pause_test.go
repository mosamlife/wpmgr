// app_alert_pause_test.go — GH #414 phase 2, the APP-HEALTH half of the pause
// gate: the fleet circuit breaker's ratio and its aggregate notification.
//
// Everything here is driven through the REAL resolveAppAlerts, the REAL
// EvaluateAppBreaker (the fake repo's TransitionAppAlertBreaker delegates
// straight to it over stored state) and the REAL Dispatcher, so a "no alert"
// assertion means an email was genuinely never composed, not that a stub was
// never poked.
//
// The one thing that is modelled rather than executed is the SQL: appRatio
// below reproduces GetTenantAppAlertRatio's WHERE clause in Go. That coupling
// is deliberate and is the RED handle for the exploit test — drop `&& !s.paused`
// from appRatio and you have the query exactly as it shipped, and
// TestPausedIncidentDoesNotSilenceActiveSite goes red. The predicate itself,
// against a real Postgres as wpmgr_app, is pinned in
// apps/api/tests/gh414_monitoring_probe_pause_integration_test.go.
package uptime

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// appPauseSite is one row of the modelled fleet: the columns
// GetTenantAppAlertRatio and ListTenantAppDownSites actually read.
type appPauseSite struct {
	id                uuid.UUID
	url               string
	name              string
	everAppUp         bool
	inIncident        bool // site_app_alert_state.in_incident
	alertsDisabled    bool
	connectionState   string
	paused            bool // sites.monitoring_paused_at IS NOT NULL
	pausedAfterTheRow bool // paused between the ratio query and the dispatch
}

// appRatio is GetTenantAppAlertRatio's WHERE clause, in Go.
//
//	sas.ever_app_up = true
//	AND s.app_alerts_disabled = false
//	AND s.connection_state NOT IN ('revoked','archived')
//	AND s.monitoring_paused_at IS NULL
//
// `down` is that same population with in_incident = true — the pause predicate
// applies to BOTH sides, which is the whole fix: filtering the numerator alone
// leaves the paused row padding the denominator.
func appRatio(fleet []appPauseSite) (eligible, down int) {
	for _, s := range fleet {
		if !s.everAppUp || s.alertsDisabled || s.paused {
			continue
		}
		if s.connectionState == "revoked" || s.connectionState == "archived" {
			continue
		}
		eligible++
		if s.inIncident {
			down++
		}
	}
	return eligible, down
}

// appPauseRepo services exactly the calls resolveAppAlerts makes. Everything
// else is inherited from the embedded nil Repo and panics, so a widening of
// what the breaker path touches fails loudly instead of silently.
type appPauseRepo struct {
	Repo

	mu      sync.Mutex
	fleet   []appPauseSite
	breaker map[uuid.UUID]AppBreakerState
	tripped []uuid.UUID
}

// GetTenantAppAlertRatio answers from the fleet as it stands, then commits any
// pending "paused between the ratio query and the dispatch" pause. That is what
// the race actually is: the FIRST read of a tick happens before the operator's
// UPDATE commits and still counts the site; every read after it sees the pause.
// Modelling the flip is what lets the fire path's re-read (fireAppAggregate,
// finding 3) be tested at all - without it the fake would insist a late pause
// is invisible to every query forever, which is not how Postgres behaves.
func (r *appPauseRepo) GetTenantAppAlertRatio(_ context.Context, _ uuid.UUID) (int, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	eligible, down := appRatio(r.fleet)
	for i := range r.fleet {
		if r.fleet[i].pausedAfterTheRow {
			r.fleet[i].paused = true
		}
	}
	return eligible, down, nil
}

// ListTenantAppDownSites mirrors the query's own predicate: the currently
// down, alert-eligible, NOT-paused sites, by display name.
func (r *appPauseRepo) ListTenantAppDownSites(_ context.Context, _ uuid.UUID, _ int) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, s := range r.fleet {
		if !s.everAppUp || s.alertsDisabled || s.paused || !s.inIncident {
			continue
		}
		out = append(out, s.name)
	}
	return out, nil
}

func (r *appPauseRepo) ListTrippedAppAlertBreakerTenants(_ context.Context) ([]uuid.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tripped, nil
}

// TransitionAppAlertBreaker delegates to the REAL transition logic over the
// stored state, exactly as the pgRepo does inside its tx.
func (r *appPauseRepo) TransitionAppAlertBreaker(_ context.Context, tenantID uuid.UUID, wantTrip bool, down int, now time.Time) (AppBreakerTransition, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tr := EvaluateAppBreaker(r.breaker[tenantID], wantTrip, down, now)
	r.breaker[tenantID] = tr.NewState
	return tr, nil
}

// IsMonitoringPaused is the DISPATCH-side re-read. It reports a site paused
// when the column says so OR when the test marked it paused-after-the-row:
// the race the fire-time read exists for.
func (r *appPauseRepo) IsMonitoringPaused(_ context.Context, siteID uuid.UUID) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.fleet {
		if s.id == siteID {
			return s.paused || s.pausedAfterTheRow, nil
		}
	}
	return false, nil
}

func (r *appPauseRepo) GetAlertConfig(_ context.Context, tenantID uuid.UUID) (AlertConfig, bool, error) {
	return AlertConfig{
		TenantID:         tenantID,
		EmailRecipients:  []string{"ops@example.com"},
		Enabled:          true,
		AppAlertsEnabled: true,
	}, true, nil
}

func (r *appPauseRepo) setIncident(id uuid.UUID, in bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.fleet {
		if r.fleet[i].id == id {
			r.fleet[i].inIncident = in
		}
	}
}

// recordingMailer keeps EVERY subject and body, unlike stubMailer which keeps
// only the last — "which sites did the mail name" is the assertion this file
// is built around.
type recordingMailer struct {
	mu       sync.Mutex
	subjects []string
	bodies   []string
}

func (m *recordingMailer) Send(_ context.Context, _ []string, subject, body string) (SendResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subjects = append(m.subjects, subject)
	m.bodies = append(m.bodies, body)
	return SendResult{Status: SendResultSent}, nil
}

func (m *recordingMailer) sent() ([]string, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.subjects...), append([]string(nil), m.bodies...)
}

func (m *recordingMailer) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subjects = nil
	m.bodies = nil
}

func newAppPauseRig(t *testing.T, repo *appPauseRepo) (*ProbeWorker, *recordingMailer) {
	t.Helper()
	mailer := &recordingMailer{}
	w := NewProbeWorker(repo, nil, nil, NewDispatcher(mailer, nil, nil, nil), nil, nil, 4, 2)
	return w, mailer
}

func appPauseFleetSite(url string) appPauseSite {
	return appPauseSite{
		id: uuid.New(), url: url, name: url,
		everAppUp: true, connectionState: "connected",
	}
}

func fireFor(s appPauseSite, down bool) pendingAppFire {
	return pendingAppFire{
		site: EnrolledSite{
			ID: s.id, TenantID: uuid.Nil, URL: s.url,
			HealthStatus: "healthy", ConnectionState: s.connectionState,
		},
		tr:        AppTransition{FireDown: down, FireRecovery: !down},
		appReason: "app_http_500",
	}
}

// ---------------------------------------------------------------------------
// HOLE 1 — the regression that would have shipped.
// ---------------------------------------------------------------------------

// TestPausedIncidentDoesNotSilenceActiveSite is the exploit, end to end.
//
// Two sites are paused WHILE their app-health incident is open — the ordinary
// reason anyone pauses a site: it is broken. Phase 2 stops probing them, so
// site_app_alert_state.in_incident is frozen at true forever. Without the pause
// predicate on GetTenantAppAlertRatio those two frozen rows keep counting in
// BOTH the numerator and the denominator, the fleet circuit breaker stays
// tripped permanently, and every individual alert for the tenant's remaining
// ACTIVE site is suppressed — a site nobody paused going silent.
//
// The tenant's breaker is seeded already-tripped with LastDownCount=3, which is
// the honest history: all three sites went down, the breaker tripped and
// notified naming three, and the operator then paused the two that were broken.
//
// RED: delete `|| s.paused` from appRatio above (the query exactly as it
// shipped). Both halves fall to zero mails.
func TestPausedIncidentDoesNotSilenceActiveSite(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	pausedA := appPauseFleetSite("https://paused-a.example.com")
	pausedA.inIncident, pausedA.paused = true, true
	pausedB := appPauseFleetSite("https://paused-b.example.com")
	pausedB.inIncident, pausedB.paused = true, true
	active := appPauseFleetSite("https://active.example.com")
	active.inIncident = true // it went down with the others

	lastAlert := time.Now().Add(-48 * time.Hour)
	repo := &appPauseRepo{
		fleet: []appPauseSite{pausedA, pausedB, active},
		breaker: map[uuid.UUID]AppBreakerState{
			tenant: {Tripped: true, TrippedAt: &lastAlert, LastAlertAt: &lastAlert, LastDownCount: 3},
		},
		tripped: []uuid.UUID{tenant},
	}
	w, mailer := newAppPauseRig(t, repo)

	// --- Half 1: the ACTIVE site recovers. -----------------------------
	repo.setIncident(active.id, false)
	recovery := fireFor(active, false)
	recovery.site.TenantID = tenant
	w.resolveAppAlerts(ctx, []pendingAppFire{recovery}, time.Now())

	subjects, _ := mailer.sent()
	if len(subjects) == 0 {
		t.Fatalf("the ACTIVE site's recovery must reach the tenant: subjects=%v (two paused sites' frozen incidents are suppressing an unpaused site)", subjects)
	}
	mailer.reset()

	// --- Half 2: 24 hours later it goes down again. --------------------
	later := time.Now().Add(24 * time.Hour)
	repo.setIncident(active.id, true)
	down := fireFor(active, true)
	down.site.TenantID = tenant
	w.resolveAppAlerts(ctx, []pendingAppFire{down}, later)

	subjects, _ = mailer.sent()
	if len(subjects) == 0 {
		t.Fatalf("the ACTIVE site's next outage must alert: subjects=%v", subjects)
	}
	joined := strings.Join(subjects, " | ")
	if !strings.Contains(joined, active.url) {
		t.Fatalf("the outage alert must name the ACTIVE site %q, got subjects=%v", active.url, subjects)
	}
	// And it must never name a site the operator paused.
	for _, paused := range []string{pausedA.url, pausedB.url} {
		if strings.Contains(joined, paused) {
			t.Fatalf("a paused site (%s) must never be named in a notification: subjects=%v", paused, subjects)
		}
	}
}

// ---------------------------------------------------------------------------
// HOLE 2 — a fully paused tenant must never be paged.
// ---------------------------------------------------------------------------

// TestFullyPausedTenantSendsNoAggregate: every site paused, breaker already
// tripped from before the pause. The ratio now reports 0 eligible / 0 down, so
// the breaker recovers — and a "your fleet recovered" aggregate for a tenant
// with nothing being monitored is still a page nobody asked for. It is
// withheld.
//
// RED: delete the `else if alert.EligibleCount == 0` branch from
// fireAppAggregate. One aggregate email is delivered.
func TestFullyPausedTenantSendsNoAggregate(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	fleet := make([]appPauseSite, 0, 3)
	for _, u := range []string{"https://p1.example.com", "https://p2.example.com", "https://p3.example.com"} {
		s := appPauseFleetSite(u)
		s.paused = true
		s.inIncident = u != "https://p3.example.com" // two frozen mid-incident
		fleet = append(fleet, s)
	}
	last := time.Now().Add(-2 * time.Hour)
	repo := &appPauseRepo{
		fleet: fleet,
		breaker: map[uuid.UUID]AppBreakerState{
			tenant: {Tripped: true, TrippedAt: &last, LastAlertAt: &last, LastDownCount: 2},
		},
		tripped: []uuid.UUID{tenant},
	}
	w, mailer := newAppPauseRig(t, repo)

	w.resolveAppAlerts(ctx, nil, time.Now())

	subjects, bodies := mailer.sent()
	if len(subjects) != 0 {
		t.Fatalf("a tenant with every site paused must receive NOTHING, got subjects=%v bodies=%v", subjects, bodies)
	}
}

// TestFullyPausedTenantAggregateNamesNothing is the same hole approached from
// the body rather than the dispatch: even if a fire slipped through with an
// entirely paused affected population, the aggregate must not go out naming
// paused sites. This drives fireAppAggregate directly with a non-empty
// EligibleCount, so the tenant-level leg cannot be what saves it.
//
// RED: delete the `if len(affected) > 0 { ... }` block from fireAppAggregate.
func TestFullyPausedTenantAggregateNamesNothing(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	a := appPauseFleetSite("https://gone-quiet-a.example.com")
	a.paused, a.inIncident = true, true
	b := appPauseFleetSite("https://gone-quiet-b.example.com")
	b.paused, b.inIncident = true, true

	repo := &appPauseRepo{fleet: []appPauseSite{a, b}, breaker: map[uuid.UUID]AppBreakerState{}}
	w, mailer := newAppPauseRig(t, repo)

	fires := []pendingAppFire{fireFor(a, true), fireFor(b, true)}
	for i := range fires {
		fires[i].site.TenantID = tenant
	}
	w.fireAppAggregate(ctx, tenant, AppAggregateAlert{
		TenantID: tenant, DownCount: 2, EligibleCount: 2, FiredAt: time.Now(),
	}, fires)

	subjects, bodies := mailer.sent()
	if len(subjects) != 0 {
		t.Fatalf("an aggregate whose entire affected population is paused must not be sent, got subjects=%v bodies=%v", subjects, bodies)
	}
}

// ---------------------------------------------------------------------------
// The mixed tenant: what the notification is allowed to say.
// ---------------------------------------------------------------------------

// TestAggregateNamesOnlyUnpausedDownSites is the reported bug's exact shape,
// inverted: it claimed "3/3 sites are simultaneously app-down" while naming
// paused sites, when only one had actually been observed down.
//
// Fleet: A and B unpaused and down this tick, C and D paused and frozen
// mid-incident, E unpaused and up. The truthful statement is 2 down of 3
// eligible, naming A and B — the count must equal what it names, and the two
// paused sites must appear in neither.
//
// RED: delete `|| s.paused` from appRatio (the shipped query) — the mail
// becomes 4/5 and the body names C and D.
func TestAggregateNamesOnlyUnpausedDownSites(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	a := appPauseFleetSite("https://mix-a.example.com")
	a.inIncident = true
	b := appPauseFleetSite("https://mix-b.example.com")
	b.inIncident = true
	c := appPauseFleetSite("https://mix-paused-c.example.com")
	c.paused, c.inIncident = true, true
	d := appPauseFleetSite("https://mix-paused-d.example.com")
	d.paused, d.inIncident = true, true
	e := appPauseFleetSite("https://mix-e.example.com")

	repo := &appPauseRepo{
		fleet:   []appPauseSite{a, b, c, d, e},
		breaker: map[uuid.UUID]AppBreakerState{},
	}
	w, mailer := newAppPauseRig(t, repo)

	fires := []pendingAppFire{fireFor(a, true), fireFor(b, true)}
	for i := range fires {
		fires[i].site.TenantID = tenant
	}
	w.resolveAppAlerts(ctx, fires, time.Now())

	subjects, bodies := mailer.sent()
	if len(subjects) != 1 {
		t.Fatalf("the breaker must trip and send exactly ONE aggregate, got %d: %v", len(subjects), subjects)
	}
	body := bodies[0]
	if !strings.Contains(body, "2 of 3") && !strings.Contains(body, "2/3") {
		t.Fatalf("the aggregate must state 2 down of 3 ELIGIBLE (paused sites count on neither side), body:\n%s", body)
	}
	for _, want := range []string{a.url, b.url} {
		if !strings.Contains(body, want) {
			t.Fatalf("the aggregate must name the unpaused down site %s, body:\n%s", want, body)
		}
	}
	for _, never := range []string{c.url, d.url} {
		if strings.Contains(body, never) {
			t.Fatalf("the aggregate must NOT name the paused site %s, body:\n%s", never, body)
		}
	}
	// The count must match what it names: 2 named, 2 claimed.
	named := strings.Count(body, "mix-a.example.com") + strings.Count(body, "mix-b.example.com")
	if named == 0 {
		t.Fatalf("expected the two unpaused sites named in the body:\n%s", body)
	}
}

// TestAggregateDropsSitePausedAfterTheQuery is the belt to the query's braces
// (task 2): the ratio and down-list queries exclude paused sites server-side,
// but a site paused a SECOND after those queries ran is still in their result
// set. Only the fire-time re-read sees it, and it must not be named.
//
// RED: delete the `alert.SuppressedSites = w.appFireDisplayNames(ctx, live)`
// line from fireAppAggregate — the late-paused site is named again.
func TestAggregateDropsSitePausedAfterTheQuery(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	a := appPauseFleetSite("https://race-a.example.com")
	a.inIncident = true
	b := appPauseFleetSite("https://race-b.example.com")
	b.inIncident = true
	// Paused between the ratio query and the dispatch: still counted by the
	// query (it ran first), already paused by the time the alert fires.
	late := appPauseFleetSite("https://race-late.example.com")
	late.inIncident, late.pausedAfterTheRow = true, true
	up := appPauseFleetSite("https://race-up.example.com")

	repo := &appPauseRepo{
		fleet:   []appPauseSite{a, b, late, up},
		breaker: map[uuid.UUID]AppBreakerState{},
	}
	w, mailer := newAppPauseRig(t, repo)

	fires := []pendingAppFire{fireFor(a, true), fireFor(b, true), fireFor(late, true)}
	for i := range fires {
		fires[i].site.TenantID = tenant
	}
	w.resolveAppAlerts(ctx, fires, time.Now())

	subjects, bodies := mailer.sent()
	if len(subjects) != 1 {
		t.Fatalf("expected exactly one aggregate, got %d: %v", len(subjects), subjects)
	}
	if strings.Contains(bodies[0], late.url) {
		t.Fatalf("a site paused after the ratio query ran must not be named, body:\n%s", bodies[0])
	}
	for _, want := range []string{a.url, b.url} {
		if !strings.Contains(bodies[0], want) {
			t.Fatalf("the aggregate must still name %s, body:\n%s", want, bodies[0])
		}
	}
}

// ---------------------------------------------------------------------------
// HOLE 3 — the tick's transitions are paused, the sites holding the ratio up
// are not, and the breaker suppresses BOTH halves.
// ---------------------------------------------------------------------------

// TestAggregateStillFiresWhenEveryTransitionIsPaused is the third total-silence
// hole, a different shape from the first two.
//
// Two sites transition this tick and are paused before the sweep resolves;
// TWO OTHER SITES, unpaused and genuinely app-down, are what actually holds the
// ratio above the trip threshold. The breaker trips — which suppresses every
// per-site alert for the tenant — and the aggregate that is supposed to replace
// them must therefore go out. Withholding it because THIS TICK's transitions
// happened to be paused ones produces total silence over two unpaused, broken
// sites, which is the same class of bug as the two holes above.
//
// The pause must remove the paused sites from the BODY and nothing else. What
// the body names instead is the live down set, which is exactly the population
// the notification is speaking for.
//
// RED: restore `if len(live) == 0 { ...; return }` as the first thing
// fireAppAggregate does with `affected` — subjects comes back empty.
func TestAggregateStillFiresWhenEveryTransitionIsPaused(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	// This tick's transitions, both paused before resolveAppAlerts ran.
	pausedA := appPauseFleetSite("https://tick-paused-a.example.com")
	pausedA.paused, pausedA.inIncident = true, true
	pausedB := appPauseFleetSite("https://tick-paused-b.example.com")
	pausedB.paused, pausedB.inIncident = true, true
	// The sites that actually hold the ratio above threshold: unpaused, down,
	// and about to have every one of their individual alerts suppressed.
	liveC := appPauseFleetSite("https://tick-live-c.example.com")
	liveC.inIncident = true
	liveD := appPauseFleetSite("https://tick-live-d.example.com")
	liveD.inIncident = true

	repo := &appPauseRepo{
		fleet:   []appPauseSite{pausedA, pausedB, liveC, liveD},
		breaker: map[uuid.UUID]AppBreakerState{},
	}
	w, mailer := newAppPauseRig(t, repo)

	fires := []pendingAppFire{fireFor(pausedA, true), fireFor(pausedB, true)}
	for i := range fires {
		fires[i].site.TenantID = tenant
	}
	w.resolveAppAlerts(ctx, fires, time.Now())

	subjects, bodies := mailer.sent()
	if !repo.breaker[tenant].Tripped {
		t.Fatalf("two unpaused down sites out of two eligible must trip the breaker: %+v", repo.breaker[tenant])
	}
	if len(subjects) != 1 {
		t.Fatalf("the breaker suppressed every per-site alert, so the aggregate MUST be sent: got %d subjects=%v bodies=%v", len(subjects), subjects, bodies)
	}
	body := bodies[0]
	if !strings.Contains(body, "2 of 2") {
		t.Fatalf("the aggregate must state the unpaused truth, 2 down of 2 eligible, body:\n%s", body)
	}
	for _, want := range []string{liveC.url, liveD.url} {
		if !strings.Contains(body, want) {
			t.Fatalf("the aggregate must name the unpaused down site %s, body:\n%s", want, body)
		}
	}
	for _, never := range []string{pausedA.url, pausedB.url} {
		if strings.Contains(body, never) {
			t.Fatalf("a paused site (%s) must never be named, body:\n%s", never, body)
		}
	}
}

// TestAggregateCountMatchesTheNamesAfterALatePause pins the subject to the
// body. The ratio query runs, THEN one of the sites it counted is paused, and
// the fire-time re-read drops that site from the list — so the counts the
// subject quotes were taken before the pause and the list was not. The mail
// then reads "3/4 sites are simultaneously app-down" over a body naming two,
// which is the shape of the originally reported symptom.
//
// RED: delete the `GetTenantAppAlertRatio` re-read block from fireAppAggregate
// — the subject says 3/4 while the body names 2.
func TestAggregateCountMatchesTheNamesAfterALatePause(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	a := appPauseFleetSite("https://count-a.example.com")
	a.inIncident = true
	b := appPauseFleetSite("https://count-b.example.com")
	b.inIncident = true
	late := appPauseFleetSite("https://count-late.example.com")
	late.inIncident, late.pausedAfterTheRow = true, true
	up := appPauseFleetSite("https://count-up.example.com")

	repo := &appPauseRepo{
		fleet:   []appPauseSite{a, b, late, up},
		breaker: map[uuid.UUID]AppBreakerState{},
	}
	w, mailer := newAppPauseRig(t, repo)

	fires := []pendingAppFire{fireFor(a, true), fireFor(b, true), fireFor(late, true)}
	for i := range fires {
		fires[i].site.TenantID = tenant
	}
	w.resolveAppAlerts(ctx, fires, time.Now())

	subjects, bodies := mailer.sent()
	if len(subjects) != 1 {
		t.Fatalf("expected exactly one aggregate, got %d: %v", len(subjects), subjects)
	}
	if !strings.Contains(subjects[0], "2/3") {
		t.Fatalf("the subject must quote the pause-filtered counts (2/3), got %q with body:\n%s", subjects[0], bodies[0])
	}
	if !strings.Contains(bodies[0], "2 of 3") {
		t.Fatalf("the body's count must agree with the sites it names, body:\n%s", bodies[0])
	}
	if strings.Contains(bodies[0], late.url) {
		t.Fatalf("the late-paused site must not be named, body:\n%s", bodies[0])
	}
	for _, want := range []string{a.url, b.url} {
		if !strings.Contains(bodies[0], want) {
			t.Fatalf("the aggregate must still name %s, body:\n%s", want, bodies[0])
		}
	}
}

// ---------------------------------------------------------------------------
// The over-fire control. A gate that reddens correct work guards nothing.
// ---------------------------------------------------------------------------

// TestBreakerTripsUnchangedWhenNothingIsPaused: identical fleet, nothing
// paused, and the breaker trips on exactly the same population and sends
// exactly the same aggregate it always did. This is the assertion that stops
// the pause predicate from being widened into a general silencer.
//
// RED: make monitoringPaused return true unconditionally, or make appRatio
// treat every site as paused. The aggregate disappears.
func TestBreakerTripsUnchangedWhenNothingIsPaused(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	a := appPauseFleetSite("https://plain-a.example.com")
	a.inIncident = true
	b := appPauseFleetSite("https://plain-b.example.com")
	b.inIncident = true
	c := appPauseFleetSite("https://plain-c.example.com")

	repo := &appPauseRepo{fleet: []appPauseSite{a, b, c}, breaker: map[uuid.UUID]AppBreakerState{}}
	w, mailer := newAppPauseRig(t, repo)

	fires := []pendingAppFire{fireFor(a, true), fireFor(b, true)}
	for i := range fires {
		fires[i].site.TenantID = tenant
	}
	w.resolveAppAlerts(ctx, fires, time.Now())

	subjects, bodies := mailer.sent()
	if len(subjects) != 1 {
		t.Fatalf("with nothing paused the breaker must trip and send exactly one aggregate, got %d: %v", len(subjects), subjects)
	}
	for _, want := range []string{a.url, b.url} {
		if !strings.Contains(bodies[0], want) {
			t.Fatalf("the aggregate must name %s exactly as before, body:\n%s", want, bodies[0])
		}
	}
	if !repo.breaker[tenant].Tripped {
		t.Fatalf("the breaker must be tripped: %+v", repo.breaker[tenant])
	}
}

// TestUnpausedTenantBelowThresholdStillAlertsIndividually is the second
// over-fire control: a tenant nowhere near the trip threshold keeps getting its
// ordinary per-site app alerts, one per site, with nothing collapsed.
//
// RED: make monitoringPaused return true unconditionally.
func TestUnpausedTenantBelowThresholdStillAlertsIndividually(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()

	fleet := make([]appPauseSite, 0, 6)
	for i := 0; i < 6; i++ {
		fleet = append(fleet, appPauseFleetSite("https://solo-"+uuid.NewString()[:8]+".example.com"))
	}
	fleet[0].inIncident = true

	repo := &appPauseRepo{fleet: fleet, breaker: map[uuid.UUID]AppBreakerState{}}
	w, mailer := newAppPauseRig(t, repo)

	fire := fireFor(fleet[0], true)
	fire.site.TenantID = tenant
	w.resolveAppAlerts(ctx, []pendingAppFire{fire}, time.Now())

	subjects, _ := mailer.sent()
	if len(subjects) != 1 {
		t.Fatalf("one down site out of six must produce exactly one INDIVIDUAL alert, got %d: %v", len(subjects), subjects)
	}
	if repo.breaker[tenant].Tripped {
		t.Fatalf("one down site out of six must not trip the breaker: %+v", repo.breaker[tenant])
	}
}
