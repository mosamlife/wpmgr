package tests

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/uptime"
)

// loopbackClient is an SSRF client that may reach the loopback httptest server
// (test-only), mirroring the update integration tests.
func loopbackClient() *httpclient.Client {
	return httpclient.New(httpclient.Config{Timeout: 5 * time.Second, AllowPrivateNetworks: true})
}

// countingMailer records the recipients of each alert email.
type countingMailer struct {
	mu         sync.Mutex
	calls      int
	recipients [][]string
}

func (m *countingMailer) Send(_ context.Context, recipients []string, _, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	cp := append([]string(nil), recipients...)
	m.recipients = append(m.recipients, cp)
	return nil
}

// nameLookup is a static SiteLookup for the probe worker.
type nameLookup struct{ name string }

func (n nameLookup) SiteName(context.Context, uuid.UUID, uuid.UUID) string { return n.name }

// TestUptimeProbeAlertsTransitionDedupe runs real probe sweeps against a fake
// site whose status is toggled, asserting exactly ONE down alert fires (after
// the threshold of consecutive downs and de-duped thereafter) and exactly ONE
// recovery on the next up — with the right email recipients and a signed
// webhook POST.
func TestUptimeProbeAlertsTransitionDedupe(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "uptime-alerts")

	// Fake site whose homepage status is test-controlled.
	var status int32 = http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(atomic.LoadInt32(&status)))
	}))
	defer srv.Close()

	s := enrollFakeSite(t, pool, tenant, srv.URL)

	// Configure the tenant's alert channel: email recipients + a webhook.
	var (
		hookMu    sync.Mutex
		hookCount int
		hookSig   string
		hookBody  []byte
	)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hookMu.Lock()
		hookCount++
		hookSig = r.Header.Get("X-WPMgr-Signature")
		hookBody = body
		hookMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer hook.Close()

	repo := uptime.NewRepo(pool)
	if _, err := repo.UpsertAlertConfig(ctx, uptime.AlertConfig{
		TenantID:        tenant,
		EmailRecipients: []string{"ops@example.com"},
		WebhookURL:      hook.URL,
		WebhookSecret:   "s3cr3t",
		Enabled:         true,
	}); err != nil {
		t.Fatalf("upsert alert config: %v", err)
	}

	mailer := &countingMailer{}
	poster := uptime.NewSSRFWebhookPoster(loopbackClient())
	dispatcher := uptime.NewDispatcher(mailer, poster, nil, nil)
	disabledStore, _ := metrics.New(ctx, metrics.Config{Addr: ""}, nil) // ClickHouse not needed for alerts.
	prober := uptime.NewProber(loopbackClient(), 5*time.Second)

	// threshold=2 ⇒ down fires on the 2nd consecutive down.
	w := uptime.NewProbeWorker(repo, prober, disabledStore, dispatcher, nameLookup{name: "Fake Site"}, nil, 5, 2)

	// Bring the site DOWN.
	atomic.StoreInt32(&status, http.StatusInternalServerError)

	// Sweep 1: 1st down — below threshold, no alert.
	if _, err := w.Sweep(ctx, time.Now()); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	if mailer.calls != 0 {
		t.Fatalf("after sweep 1 expected 0 emails, got %d", mailer.calls)
	}

	// Sweep 2: 2nd down — crosses threshold, fire ONE down alert.
	if _, err := w.Sweep(ctx, time.Now()); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	// Sweep 3: 3rd down — already in incident, de-duped (no new alert).
	if _, err := w.Sweep(ctx, time.Now()); err != nil {
		t.Fatalf("sweep 3: %v", err)
	}
	if mailer.calls != 1 {
		t.Fatalf("expected exactly ONE down alert after 3 downs (transition+dedupe), got %d", mailer.calls)
	}
	mailer.mu.Lock()
	if len(mailer.recipients[0]) != 1 || mailer.recipients[0][0] != "ops@example.com" {
		t.Fatalf("down alert recipients = %v, want [ops@example.com]", mailer.recipients[0])
	}
	mailer.mu.Unlock()

	// The webhook fired once with a signature header and a parseable body.
	hookMu.Lock()
	if hookCount != 1 {
		t.Fatalf("expected 1 webhook POST, got %d", hookCount)
	}
	if hookSig == "" || len(hookBody) == 0 {
		t.Fatalf("expected a signed webhook body, sig=%q len=%d", hookSig, len(hookBody))
	}
	hookMu.Unlock()

	// Site health_status must now be unreachable.
	assertHealth(t, pool, s.ID, "unreachable")

	// Bring the site back UP.
	atomic.StoreInt32(&status, http.StatusOK)
	if _, err := w.Sweep(ctx, time.Now()); err != nil {
		t.Fatalf("recovery sweep: %v", err)
	}
	if mailer.calls != 2 {
		t.Fatalf("expected ONE recovery alert (total 2), got %d", mailer.calls)
	}
	assertHealth(t, pool, s.ID, "healthy")

	// Another up sweep must NOT re-alert (no recovery spam).
	if _, err := w.Sweep(ctx, time.Now()); err != nil {
		t.Fatalf("steady-up sweep: %v", err)
	}
	if mailer.calls != 2 {
		t.Fatalf("expected no further alerts on steady-up, got %d", mailer.calls)
	}
}

// TestUptimeAlertStateTransitionRace reproduces the lost-update race behind
// issue #124 (downtime alerts never fired, even for sustained outages): when
// a probe sweep runs longer than the probe interval — exactly what happens
// during a real, fleet-wide outage, since down probes each take up to the
// probe timeout — the NEXT periodic sweep can start before the first
// finishes, so two overlapping sweeps observe and persist a site's alert
// state concurrently. A plain "read state, evaluate, write state" sequence
// loses updates under this overlap: both reads see the same prior
// consecutive_down, so it never accumulates past 1 and the down threshold is
// never crossed — silently, with no error logged. TransitionAlertState closes
// this via a single locked (SELECT ... FOR UPDATE) transaction, so this test
// fires two concurrent transitions for the SAME site and asserts NEITHER
// update is lost and EXACTLY one crosses the threshold.
func TestUptimeAlertStateTransitionRace(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "uptime-race")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	s := enrollFakeSite(t, pool, tenant, srv.URL)

	repo := uptime.NewRepo(pool)

	const threshold = 2
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []uptime.Transition
	)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			tr, err := repo.TransitionAlertState(ctx, s.ID, tenant, false, threshold, time.Now())
			if err != nil {
				t.Errorf("transition: %v", err)
				return
			}
			mu.Lock()
			results = append(results, tr)
			mu.Unlock()
		}()
	}
	wg.Wait()

	final, found, err := repo.GetAlertState(ctx, s.ID)
	if err != nil || !found {
		t.Fatalf("get final alert state: found=%v err=%v", found, err)
	}
	if final.ConsecutiveDown != 2 {
		t.Fatalf("consecutive_down = %d, want 2 (lost update under overlapping sweeps)", final.ConsecutiveDown)
	}
	if !final.InIncident {
		t.Fatal("expected in_incident=true after crossing the threshold")
	}

	fireCount := 0
	for _, tr := range results {
		if tr.FireDown {
			fireCount++
		}
	}
	if fireCount != 1 {
		t.Fatalf("expected exactly 1 FireDown across the two overlapping transitions, got %d", fireCount)
	}
}

// TestUptimeAlertStateTransitionRaceDeterministic forces the EXACT
// interleaving a lost-update race requires (rather than hoping goroutine
// scheduling happens to produce it, which is unreliable on a fast local test
// DB): transaction 1 takes the row lock and holds it open; transaction 2's
// locking read is proven to be BLOCKED on that lock before transaction 1 is
// released to commit. This directly proves FOR UPDATE — not scheduling luck —
// is what prevents transaction 2 from computing "consecutive_down + 1" off a
// value that transaction 1 is about to overwrite.
func TestUptimeAlertStateTransitionRaceDeterministic(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "uptime-race-det")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	s := enrollFakeSite(t, pool, tenant, srv.URL)

	repo := uptime.NewRepo(pool)
	// Seed consecutive_down=1 (threshold high so it doesn't fire yet) so both
	// racing transactions increment an EXISTING row — the case a bare read +
	// separate write can lose (the very-first insert is already race-safe via
	// plain ON CONFLICT DO UPDATE).
	if _, err := repo.TransitionAlertState(ctx, s.ID, tenant, false, 100, time.Now()); err != nil {
		t.Fatalf("seed transition: %v", err)
	}

	tx1Locked := make(chan struct{})
	releaseTx1 := make(chan struct{})
	tx2ReadStarted := make(chan struct{})
	errs := make(chan error, 2)

	lockAndIncrement := func(startedCh chan struct{}, lockedCh chan struct{}, waitCh chan struct{}) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			errs <- err
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SELECT set_config('app.agent', 'on', true)"); err != nil {
			errs <- err
			return
		}
		if startedCh != nil {
			close(startedCh)
		}
		q := sqlc.New(tx)
		row, err := q.GetSiteAlertStateForUpdate(ctx, s.ID) // blocks if another tx holds the lock
		if err != nil {
			errs <- err
			return
		}
		if lockedCh != nil {
			close(lockedCh)
		}
		if waitCh != nil {
			<-waitCh
		}
		if _, err := q.UpsertSiteAlertState(ctx, sqlc.UpsertSiteAlertStateParams{
			SiteID:          s.ID,
			TenantID:        tenant,
			LastStatus:      "down",
			ConsecutiveDown: row.ConsecutiveDown + 1,
			InIncident:      row.InIncident,
		}); err != nil {
			errs <- err
			return
		}
		errs <- tx.Commit(ctx)
	}

	// tx1 acquires the lock and holds it until told to proceed.
	go lockAndIncrement(nil, tx1Locked, releaseTx1)
	<-tx1Locked

	// tx2 starts and issues its own locking read, which must block on tx1's
	// held lock rather than reading tx1's stale (already-superseded) value.
	go lockAndIncrement(tx2ReadStarted, nil, nil)
	<-tx2ReadStarted
	// Give tx2's SELECT ... FOR UPDATE time to actually reach Postgres and
	// start waiting on the lock before we release tx1 (best-effort; the
	// consecutive_down assertion below is what actually proves the ordering).
	time.Sleep(200 * time.Millisecond)
	close(releaseTx1)

	if err := <-errs; err != nil {
		t.Fatalf("tx1: %v", err)
	}
	if err := <-errs; err != nil {
		t.Fatalf("tx2: %v", err)
	}

	final, found, err := repo.GetAlertState(ctx, s.ID)
	if err != nil || !found {
		t.Fatalf("get final alert state: found=%v err=%v", found, err)
	}
	// Seeded at 1; both transactions increment by 1. A transaction that reads
	// the STALE pre-lock value would land the final count on 2 (a lost
	// update); the lock forces tx2 to read tx1's committed 2 and land on 3.
	if final.ConsecutiveDown != 3 {
		t.Fatalf("consecutive_down = %d, want 3 (FOR UPDATE failed to serialize the two transitions — lost update)", final.ConsecutiveDown)
	}
}

func assertHealth(t *testing.T, pool *db.Pool, siteID uuid.UUID, want string) {
	t.Helper()
	// sites has FORCE RLS; the app pool with no app.tenant_id GUC sees zero rows.
	// Read via the superuser admin connection (bypasses RLS) for the assertion.
	admin := connectAdmin(t, pool)
	defer admin.Close()
	var got string
	if err := admin.QueryRow(context.Background(),
		"SELECT health_status FROM sites WHERE id = $1", siteID).Scan(&got); err != nil {
		t.Fatalf("read health_status: %v", err)
	}
	if got != want {
		t.Fatalf("health_status = %q, want %q", got, want)
	}
}

// TestUptimeAPITenantIsolation asserts the uptime service refuses a site from
// another tenant (404), so a ClickHouse query is never issued for it.
func TestUptimeAPITenantIsolation(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenantA := seedTenant(t, pool, "uptime-iso-a")
	tenantB := seedTenant(t, pool, "uptime-iso-b")

	svcA := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	siteA, err := svcA.Create(ctx, site.CreateInput{TenantID: tenantA, URL: "https://a.example.com", Name: "A"})
	if err != nil {
		t.Fatalf("create site A: %v", err)
	}

	verifier := &isoAdapter{svc: svcA}
	disabledStore, _ := metrics.New(ctx, metrics.Config{Addr: ""}, nil)
	usvc := uptime.NewService(uptime.NewRepo(pool), disabledStore, verifier)

	// Tenant B asking for tenant A's site ⇒ 404 (RLS hides it; VerifySite ok=false).
	_, err = usvc.Uptime(ctx, tenantB, siteA.ID, 7*24*time.Hour, 100)
	if err == nil {
		t.Fatal("expected not-found for cross-tenant uptime query")
	}
	if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindNotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}

	// Tenant A can read its own site (no error; empty metrics).
	if _, err := usvc.Uptime(ctx, tenantA, siteA.ID, 7*24*time.Hour, 100); err != nil {
		t.Fatalf("tenant A own-site uptime: %v", err)
	}
}

// isoAdapter is a minimal SiteVerifier over the site service for the isolation
// test (mirrors the production cmd adapter).
type isoAdapter struct{ svc *site.Service }

func (a *isoAdapter) VerifySite(ctx context.Context, tenantID, siteID uuid.UUID) (string, bool, error) {
	s, err := a.svc.Get(ctx, tenantID, siteID)
	if err != nil {
		if de, ok := domain.AsDomain(err); ok && de.Kind == domain.KindNotFound {
			return "", false, nil
		}
		return "", false, err
	}
	return s.Name, true, nil
}

func (a *isoAdapter) ListSiteIDs(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error) {
	sites, err := a.svc.List(ctx, site.ListInput{TenantID: tenantID, Limit: 500})
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(sites))
	for _, s := range sites {
		ids = append(ids, s.ID)
	}
	return ids, nil
}

// TestAlertConfigRLS proves the alert_configs + site_alert_state tables are
// tenant-isolated by RLS: a config written under tenant A is invisible to
// tenant B's scoped read.
func TestAlertConfigRLS(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenantA := seedTenant(t, pool, "alert-rls-a")
	tenantB := seedTenant(t, pool, "alert-rls-b")

	repo := uptime.NewRepo(pool)
	if _, err := repo.UpsertAlertConfig(ctx, uptime.AlertConfig{
		TenantID:        tenantA,
		EmailRecipients: []string{"a@example.com"},
		Enabled:         true,
	}); err != nil {
		t.Fatalf("upsert config A: %v", err)
	}

	// Tenant A reads its own config.
	cfgA, foundA, err := repo.GetAlertConfig(ctx, tenantA)
	if err != nil || !foundA {
		t.Fatalf("tenant A get config: found=%v err=%v", foundA, err)
	}
	if len(cfgA.EmailRecipients) != 1 || cfgA.EmailRecipients[0] != "a@example.com" {
		t.Fatalf("tenant A config wrong: %+v", cfgA)
	}

	// Tenant B sees no config (RLS isolation).
	_, foundB, err := repo.GetAlertConfig(ctx, tenantB)
	if err != nil {
		t.Fatalf("tenant B get config: %v", err)
	}
	if foundB {
		t.Fatal("tenant B must NOT see tenant A's alert config (RLS leak)")
	}
}
