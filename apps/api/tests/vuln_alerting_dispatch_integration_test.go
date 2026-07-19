// vuln_alerting_dispatch_integration_test.go — m103 (GH #247) vulnerability
// alerting: the claim/dispatch transactional-outbox correctness against a
// real Postgres (RLS + row locks matter here, so fakes alone cannot prove
// this). Requires Docker; skips when unavailable (via startPostgres).
//
// Fixture writes/reads that are NOT the behavior under test (seeding a
// finding row, reading back notified_at/status for assertions) go through the
// ADMIN (superuser) pool, exactly like seedSiteFor — site_vulnerabilities has
// FORCE ROW LEVEL SECURITY, so an unscoped read/write from the non-superuser
// app pool would itself fail RLS. The actual claim/dispatch logic under test
// always goes through the real Repo/Service methods, which correctly open
// their own InTenantTx/InAgentTx against the app pool.
package tests

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/vuln"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeVulnAlertConfigReader returns a fixed per-tenant AlertChannelConfig.
type fakeVulnAlertConfigReader struct {
	mu  sync.Mutex
	cfg map[uuid.UUID]vuln.AlertChannelConfig
}

func newFakeVulnAlertConfigReader() *fakeVulnAlertConfigReader {
	return &fakeVulnAlertConfigReader{cfg: map[uuid.UUID]vuln.AlertChannelConfig{}}
}

func (f *fakeVulnAlertConfigReader) set(tenantID uuid.UUID, cfg vuln.AlertChannelConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg[tenantID] = cfg
}

func (f *fakeVulnAlertConfigReader) GetVulnAlertChannel(_ context.Context, tenantID uuid.UUID) (vuln.AlertChannelConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg[tenantID], nil
}

// fakeVulnAlertMailer records EnqueueTx calls; failNext makes the NEXT call
// return an error (to exercise the outbox rollback), then clears itself.
type fakeVulnAlertMailer struct {
	mu        sync.Mutex
	calls     int
	failNext  bool
	lastRecip []string
	lastData  map[string]any
}

func (f *fakeVulnAlertMailer) EnqueueTx(_ context.Context, _ pgx.Tx, _ uuid.UUID, recipients []string, _ string, data map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext {
		f.failNext = false
		return errors.New("fake mailer failure (outbox test)")
	}
	f.calls++
	f.lastRecip = recipients
	f.lastData = data
	return nil
}

func (f *fakeVulnAlertMailer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeVulnAlertMailer) lastNewCount() (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.lastData["NewCount"].(int)
	return n, ok
}

// fakeVulnWebhookPoster records PostSignedWebhook calls.
type fakeVulnWebhookPoster struct {
	calls int32
}

func (f *fakeVulnWebhookPoster) PostSignedWebhook(_ context.Context, _, _ string, _ any) error {
	atomic.AddInt32(&f.calls, 1)
	return nil
}

func (f *fakeVulnWebhookPoster) callCount() int32 { return atomic.LoadInt32(&f.calls) }

// ---------------------------------------------------------------------------
// Fixture helpers (all via the admin/superuser pool — see file doc comment)
// ---------------------------------------------------------------------------

func seedFindingAdmin(t *testing.T, admin *db.Pool, tenant, site uuid.UUID, vulnID, severity, status string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := admin.QueryRow(context.Background(), `
		INSERT INTO site_vulnerabilities
			(tenant_id, site_id, vuln_id, kind, slug, name, installed_version, severity, title, status)
		VALUES ($1, $2, $3, 'plugin', $3, $3, '1.0.0', $4, 'seed finding', $5)
		RETURNING id`,
		tenant, site, vulnID, severity, status,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed finding %s: %v", vulnID, err)
	}
	return id
}

func notifiedAtOfAdmin(t *testing.T, admin *db.Pool, id uuid.UUID) *time.Time {
	t.Helper()
	var v *time.Time
	if err := admin.QueryRow(context.Background(), `SELECT notified_at FROM site_vulnerabilities WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("query notified_at: %v", err)
	}
	return v
}

func statusOfAdmin(t *testing.T, admin *db.Pool, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := admin.QueryRow(context.Background(), `SELECT status FROM site_vulnerabilities WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("query status: %v", err)
	}
	return s
}

// ---------------------------------------------------------------------------
// Repo-level: claim idempotency + concurrency
// ---------------------------------------------------------------------------

// TestClaimUnnotifiedFindings_IdempotentRetry proves a second claim attempt
// (e.g. a River job retry after some OTHER failure) claims zero rows once the
// first claim has committed — findings are never double-claimed/double-sent.
func TestClaimUnnotifiedFindings_IdempotentRetry(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()
	tenant := seedTenant(t, pool, "m103-claim-"+uuid.NewString()[:8])
	site := seedSiteFor(t, admin, tenant, "https://m103-claim.example.com")
	repo := vuln.NewRepo(pool)

	seedFindingAdmin(t, admin, tenant, site, "v1", "critical", "open")
	seedFindingAdmin(t, admin, tenant, site, "v2", "high", "open")

	claim := func() int {
		var n int
		err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			rows, cErr := repo.ClaimUnnotifiedFindings(ctx, tx, tenant)
			n = len(rows)
			return cErr
		})
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		return n
	}

	if got := claim(); got != 2 {
		t.Fatalf("first claim: expected 2 rows, got %d", got)
	}
	if got := claim(); got != 0 {
		t.Fatalf("second (retry) claim: expected 0 rows (already claimed), got %d", got)
	}
}

// TestClaimUnnotifiedFindings_ConcurrentClaim_OneWinner races two goroutines
// claiming the SAME tenant's findings simultaneously and asserts the total
// claimed across both is exactly the seeded count — never double-counted,
// never lost.
func TestClaimUnnotifiedFindings_ConcurrentClaim_OneWinner(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()
	tenant := seedTenant(t, pool, "m103-race-"+uuid.NewString()[:8])
	site := seedSiteFor(t, admin, tenant, "https://m103-race.example.com")
	repo := vuln.NewRepo(pool)

	const n = 5
	for i := 0; i < n; i++ {
		seedFindingAdmin(t, admin, tenant, site, uuid.NewString(), "high", "open")
	}

	var wg sync.WaitGroup
	var totalClaimed int32
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
				rows, err := repo.ClaimUnnotifiedFindings(ctx, tx, tenant)
				if err != nil {
					return err
				}
				atomic.AddInt32(&totalClaimed, int32(len(rows)))
				return nil
			})
		}()
	}
	wg.Wait()

	if totalClaimed != n {
		t.Fatalf("total claimed across concurrent claimers = %d, want exactly %d (no double-claim, no lost claim)", totalClaimed, n)
	}
}

// TestUpsertFinding_ReappearingVuln_ResetsNotifiedAt proves a resolved->open
// transition resets notified_at (so it alerts again), while a routine
// re-match of an already-open finding (the common rescan case) does NOT
// touch notified_at once claimed.
func TestUpsertFinding_ReappearingVuln_ResetsNotifiedAt(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()
	tenant := seedTenant(t, pool, "m103-reopen-"+uuid.NewString()[:8])
	site := seedSiteFor(t, admin, tenant, "https://m103-reopen.example.com")
	repo := vuln.NewRepo(pool)

	id := seedFindingAdmin(t, admin, tenant, site, "v-reopen", "high", "open")

	// Claim it (simulates a completed alert dispatch).
	err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		_, cErr := repo.ClaimUnnotifiedFindings(ctx, tx, tenant)
		return cErr
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if notifiedAtOfAdmin(t, admin, id) == nil {
		t.Fatal("expected notified_at to be set after claim")
	}

	upsert := vuln.FindingUpsert{
		TenantID: tenant, SiteID: site, VulnID: "v-reopen", Kind: "plugin", Slug: "v-reopen",
		Name: "v-reopen", InstalledVersion: "1.0.0", Severity: "high", Title: "seed finding",
	}

	// A routine re-match while still 'open' (e.g. every subsequent rescan)
	// must NOT reset notified_at.
	err = pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		return repo.UpsertFinding(ctx, tx, upsert)
	})
	if err != nil {
		t.Fatalf("routine re-upsert: %v", err)
	}
	if notifiedAtOfAdmin(t, admin, id) == nil {
		t.Fatal("a routine re-match of an already-open finding must NOT reset notified_at")
	}

	// Resolve it (simulates ResolveStaleFindings after the vuln disappears).
	if _, err := admin.Exec(ctx, `UPDATE site_vulnerabilities SET status='resolved', resolved_at=now() WHERE id=$1`, id); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Re-upsert (the vuln re-appears — resolved -> open transition).
	err = pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		return repo.UpsertFinding(ctx, tx, upsert)
	})
	if err != nil {
		t.Fatalf("reopen re-upsert: %v", err)
	}
	if statusOfAdmin(t, admin, id) != "open" {
		t.Fatal("expected status back to 'open' after re-appearing")
	}
	if notifiedAtOfAdmin(t, admin, id) != nil {
		t.Fatal("a REAPPEARING vulnerability (resolved -> open) must reset notified_at to NULL so it alerts again")
	}
}

// ---------------------------------------------------------------------------
// Service-level: DispatchVulnAlerts (gate, threshold, outbox, webhook)
// ---------------------------------------------------------------------------

func newDispatchService(pool *db.Pool, alertCfg *fakeVulnAlertConfigReader, mailer *fakeVulnAlertMailer, webhook *fakeVulnWebhookPoster) *vuln.Service {
	svc := vuln.NewService(vuln.NewRepo(pool), pool, nil, nil, nil, slog.Default())
	svc.SetAlertConfigReader(alertCfg)
	svc.SetMailer(mailer)
	svc.SetWebhookPoster(webhook)
	svc.SetPublicBase("https://manage.wpmgr.app")
	return svc
}

// TestDispatchVulnAlerts_DisabledTenant_RowsStillStamped proves findings are
// claimed (notified_at stamped) even when notify_vulns is off — so enabling
// it later never floods the tenant with this backlog — while the
// mailer/webhook are never invoked.
func TestDispatchVulnAlerts_DisabledTenant_RowsStillStamped(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()
	tenant := seedTenant(t, pool, "m103-disabled-"+uuid.NewString()[:8])
	site := seedSiteFor(t, admin, tenant, "https://m103-disabled.example.com")

	id := seedFindingAdmin(t, admin, tenant, site, "v-disabled", "critical", "open")

	alertCfg := newFakeVulnAlertConfigReader()
	alertCfg.set(tenant, vuln.AlertChannelConfig{
		Enabled: true, NotifyVulns: false, // gate OFF
		EmailRecipients: []string{"ops@example.com"}, VulnMinSeverity: "high",
	})
	mailer := &fakeVulnAlertMailer{}
	webhook := &fakeVulnWebhookPoster{}
	svc := newDispatchService(pool, alertCfg, mailer, webhook)

	if err := svc.DispatchVulnAlerts(ctx); err != nil {
		t.Fatalf("DispatchVulnAlerts: %v", err)
	}

	if notifiedAtOfAdmin(t, admin, id) == nil {
		t.Fatal("finding must be stamped (claimed) even when notify_vulns is off")
	}
	if mailer.callCount() != 0 {
		t.Errorf("mailer must not be called when notify_vulns is off, got %d calls", mailer.callCount())
	}
	if webhook.callCount() != 0 {
		t.Errorf("webhook must not be called when notify_vulns is off, got %d calls", webhook.callCount())
	}
}

// TestDispatchVulnAlerts_ThresholdFilter_DismissedNeverClaimed_WebhookFired
// covers the threshold gate (unknown always included, low excluded below a
// 'high' threshold but still claimed), a dismissed finding never being
// claimed at all, and the webhook firing with the filtered set.
func TestDispatchVulnAlerts_ThresholdFilter_DismissedNeverClaimed_WebhookFired(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()
	tenant := seedTenant(t, pool, "m103-threshold-"+uuid.NewString()[:8])
	site := seedSiteFor(t, admin, tenant, "https://m103-threshold.example.com")

	critID := seedFindingAdmin(t, admin, tenant, site, "v-crit", "critical", "open")
	lowID := seedFindingAdmin(t, admin, tenant, site, "v-low", "low", "open")
	unkID := seedFindingAdmin(t, admin, tenant, site, "v-unk", "unknown", "open")
	dismissedID := seedFindingAdmin(t, admin, tenant, site, "v-dismissed", "critical", "dismissed")

	alertCfg := newFakeVulnAlertConfigReader()
	alertCfg.set(tenant, vuln.AlertChannelConfig{
		Enabled: true, NotifyVulns: true,
		EmailRecipients: []string{"ops@example.com"},
		WebhookURL:      "https://hooks.example.com/wpmgr",
		VulnMinSeverity: "high",
	})
	mailer := &fakeVulnAlertMailer{}
	webhook := &fakeVulnWebhookPoster{}
	svc := newDispatchService(pool, alertCfg, mailer, webhook)

	if err := svc.DispatchVulnAlerts(ctx); err != nil {
		t.Fatalf("DispatchVulnAlerts: %v", err)
	}

	// All THREE open findings get claimed (regardless of whether they pass
	// the threshold) — only 'dismissed' is excluded by the claim's own WHERE
	// status='open' clause.
	for _, id := range []uuid.UUID{critID, lowID, unkID} {
		if notifiedAtOfAdmin(t, admin, id) == nil {
			t.Errorf("finding %s must be claimed (notified_at set)", id)
		}
	}
	if notifiedAtOfAdmin(t, admin, dismissedID) != nil {
		t.Error("a dismissed finding must NEVER be claimed (status filter), notified_at must stay NULL")
	}

	if mailer.callCount() != 1 {
		t.Fatalf("expected exactly 1 batched email, got %d", mailer.callCount())
	}
	newCount, ok := mailer.lastNewCount()
	if !ok || newCount != 2 {
		t.Errorf("email NewCount = %v (ok=%v), want 2 (critical + unknown; low excluded by threshold)", newCount, ok)
	}

	if webhook.callCount() != 1 {
		t.Fatalf("expected exactly 1 webhook POST, got %d", webhook.callCount())
	}
}

// TestDispatchVulnAlerts_WebhookNotFiredWhenUnconfigured proves the webhook
// leg is skipped (not called) when no webhook_url is configured, while the
// email leg still proceeds normally.
func TestDispatchVulnAlerts_WebhookNotFiredWhenUnconfigured(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()
	tenant := seedTenant(t, pool, "m103-nowebhook-"+uuid.NewString()[:8])
	site := seedSiteFor(t, admin, tenant, "https://m103-nowebhook.example.com")
	seedFindingAdmin(t, admin, tenant, site, "v1", "critical", "open")

	alertCfg := newFakeVulnAlertConfigReader()
	alertCfg.set(tenant, vuln.AlertChannelConfig{
		Enabled: true, NotifyVulns: true,
		EmailRecipients: []string{"ops@example.com"},
		WebhookURL:      "", // not configured
		VulnMinSeverity: "high",
	})
	mailer := &fakeVulnAlertMailer{}
	webhook := &fakeVulnWebhookPoster{}
	svc := newDispatchService(pool, alertCfg, mailer, webhook)

	if err := svc.DispatchVulnAlerts(ctx); err != nil {
		t.Fatalf("DispatchVulnAlerts: %v", err)
	}
	if mailer.callCount() != 1 {
		t.Errorf("expected 1 email, got %d", mailer.callCount())
	}
	if webhook.callCount() != 0 {
		t.Errorf("webhook must not be called when webhook_url is empty, got %d calls", webhook.callCount())
	}
}

// TestDispatchVulnAlerts_Outbox_RollbackLeavesUnclaimed_RetrySucceeds is the
// transactional-outbox correctness proof: when the email enqueue fails, the
// claim (notified_at stamp) rolls back with it, so the finding is retried on
// the NEXT dispatch cycle instead of being silently claimed-but-unemailed.
func TestDispatchVulnAlerts_Outbox_RollbackLeavesUnclaimed_RetrySucceeds(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()
	tenant := seedTenant(t, pool, "m103-outbox-"+uuid.NewString()[:8])
	site := seedSiteFor(t, admin, tenant, "https://m103-outbox.example.com")
	id := seedFindingAdmin(t, admin, tenant, site, "v-outbox", "critical", "open")

	alertCfg := newFakeVulnAlertConfigReader()
	alertCfg.set(tenant, vuln.AlertChannelConfig{
		Enabled: true, NotifyVulns: true,
		EmailRecipients: []string{"ops@example.com"},
		VulnMinSeverity: "high",
	})
	mailer := &fakeVulnAlertMailer{failNext: true} // fail the first EnqueueTx call
	webhook := &fakeVulnWebhookPoster{}
	svc := newDispatchService(pool, alertCfg, mailer, webhook)

	// First run: the mailer fails -> the whole tx (claim + enqueue) rolls back.
	if err := svc.DispatchVulnAlerts(ctx); err != nil {
		t.Fatalf("DispatchVulnAlerts (first run): %v", err)
	}
	if notifiedAtOfAdmin(t, admin, id) != nil {
		t.Fatal("outbox rollback: notified_at must stay NULL when the email enqueue fails (claim undone)")
	}
	if mailer.callCount() != 0 {
		t.Errorf("expected 0 successful mailer calls after the rollback, got %d", mailer.callCount())
	}
	if webhook.callCount() != 0 {
		t.Errorf("webhook must not fire when the tx rolled back (nothing to send), got %d calls", webhook.callCount())
	}

	// Second run (retry — the debounced dispatch job re-runs, or a fresh
	// dispatch cycle picks it up): the mailer now succeeds.
	if err := svc.DispatchVulnAlerts(ctx); err != nil {
		t.Fatalf("DispatchVulnAlerts (retry): %v", err)
	}
	if notifiedAtOfAdmin(t, admin, id) == nil {
		t.Fatal("retry: expected the finding to be claimed once the email enqueue succeeds")
	}
	if mailer.callCount() != 1 {
		t.Errorf("expected exactly 1 successful mailer call after retry, got %d", mailer.callCount())
	}
}

// TestDispatchVulnAlerts_RepeatRun_NoNewSendsWithoutNewFindings proves
// idempotency at the service level: running DispatchVulnAlerts again
// immediately after a successful dispatch (no new findings in between) sends
// nothing further.
func TestDispatchVulnAlerts_RepeatRun_NoNewSendsWithoutNewFindings(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()
	tenant := seedTenant(t, pool, "m103-repeat-"+uuid.NewString()[:8])
	site := seedSiteFor(t, admin, tenant, "https://m103-repeat.example.com")
	seedFindingAdmin(t, admin, tenant, site, "v1", "critical", "open")

	alertCfg := newFakeVulnAlertConfigReader()
	alertCfg.set(tenant, vuln.AlertChannelConfig{
		Enabled: true, NotifyVulns: true,
		EmailRecipients: []string{"ops@example.com"},
		VulnMinSeverity: "high",
	})
	mailer := &fakeVulnAlertMailer{}
	webhook := &fakeVulnWebhookPoster{}
	svc := newDispatchService(pool, alertCfg, mailer, webhook)

	if err := svc.DispatchVulnAlerts(ctx); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	if mailer.callCount() != 1 {
		t.Fatalf("expected 1 email after the first dispatch, got %d", mailer.callCount())
	}
	if err := svc.DispatchVulnAlerts(ctx); err != nil {
		t.Fatalf("second dispatch: %v", err)
	}
	if mailer.callCount() != 1 {
		t.Errorf("a repeat dispatch with no new findings must send nothing further, got %d total calls", mailer.callCount())
	}
}
