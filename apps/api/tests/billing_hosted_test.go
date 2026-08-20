package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// assertSiteLimitReached asserts err is the M16 Phase A 402 shape:
// {code: "site_limit_reached", limit, usage, plan} carried in the domain
// error's Details.
func assertSiteLimitReached(t *testing.T, err error, wantLimit, wantUsage int, wantPlan string) {
	t.Helper()
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindPaymentRequired {
		t.Fatalf("want a 402 site_limit_reached error, got: %v", err)
	}
	if de.Code != "site_limit_reached" {
		t.Fatalf("Code = %q, want site_limit_reached", de.Code)
	}
	if got := de.Details["limit"]; got != wantLimit {
		t.Fatalf("Details[limit] = %v, want %d", got, wantLimit)
	}
	if got := de.Details["usage"]; got != wantUsage {
		t.Fatalf("Details[usage] = %v, want %d", got, wantUsage)
	}
	if got := de.Details["plan"]; got != wantPlan {
		t.Fatalf("Details[plan] = %v, want %q", got, wantPlan)
	}
}

// newHostedBillingService builds a billing.Service with no Redis (every call
// resolves fresh from Postgres — correct, just uncached, which is exactly
// what these tests want to observe).
func newHostedBillingService(pool *db.Pool, enabled bool) *billing.Service {
	return billing.New(pool, nil, enabled, domain.SystemClock{}, slog.Default())
}

// ---- Path 1: site.Service.Create (internal/site/service.go) --------------

func TestSiteCap_BlocksDirectCreate(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "cap-direct-create")

	repo := site.NewRepo(pool)
	svc := site.NewService(repo, domain.NewValidator(), domain.SystemClock{})

	for i := 0; i < 3; i++ {
		if _, err := svc.Create(ctx, site.CreateInput{
			TenantID: tenant, URL: fmt.Sprintf("https://direct-%d.example.com", i), Name: "s",
		}); err != nil {
			t.Fatalf("seed site %d: %v", i, err)
		}
	}

	svc.SetBillingGate(newHostedBillingService(pool, true))

	_, err := svc.Create(ctx, site.CreateInput{TenantID: tenant, URL: "https://direct-4.example.com", Name: "s4"})
	assertSiteLimitReached(t, err, 3, 3, "free")
}

// ---- Path 2: CreatePending via MintEnrollmentCode (site-first "Add site") -

func TestSiteCap_BlocksMintEnrollmentCode(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "cap-mint")

	repo := site.NewRepo(pool)
	conn := site.NewConnectionService(repo, domain.NewValidator(), nil, nil, domain.SystemClock{}, nil)

	for i := 0; i < 3; i++ {
		if _, err := conn.MintEnrollmentCode(ctx, site.MintEnrollmentInput{
			TenantID: tenant, URL: fmt.Sprintf("https://mint-%d.example.com", i), Name: "s",
		}); err != nil {
			t.Fatalf("seed site %d: %v", i, err)
		}
	}

	site.SetBillingGate(repo, newHostedBillingService(pool, true))

	_, err := conn.MintEnrollmentCode(ctx, site.MintEnrollmentInput{TenantID: tenant, URL: "https://mint-4.example.com", Name: "s4"})
	assertSiteLimitReached(t, err, 3, 3, "free")
}

// ---- Path 3: legacy repo.Enroll -> CreateSiteForEnroll (public /enroll) ---
//
// The #1 bypass risk if left unchecked: this is the create-at-enroll branch
// reached whenever a pairing code is NOT site-bound (the legacy tenant-scoped
// code path, still reachable even with the lifecycle service wired — see
// site.Service.Enroll).

func TestSiteCap_BlocksLegacyEnrollCreate(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "cap-legacy-enroll")

	repo := site.NewRepo(pool)
	svc := site.NewService(repo, domain.NewValidator(), domain.SystemClock{})
	// svc.conn is intentionally left nil: every code takes the legacy
	// repo.Enroll path, exercising CreateSiteForEnroll directly.

	for i := 0; i < 3; i++ {
		code, err := svc.CreatePairingCode(ctx, site.CreatePairingCodeInput{TenantID: tenant})
		if err != nil {
			t.Fatalf("create code %d: %v", i, err)
		}
		_, _, pub := genKey(t)
		if _, err := svc.Enroll(ctx, site.EnrollRequest{
			PairingCode: code.Plaintext, SiteURL: fmt.Sprintf("https://legacy-%d.example.com", i), AgentPublicKey: pub,
		}); err != nil {
			t.Fatalf("seed enroll %d: %v", i, err)
		}
	}

	svc.SetBillingGate(newHostedBillingService(pool, true))

	code, err := svc.CreatePairingCode(ctx, site.CreatePairingCodeInput{TenantID: tenant})
	if err != nil {
		t.Fatalf("create code 4: %v", err)
	}
	_, _, pub := genKey(t)
	_, err = svc.Enroll(ctx, site.EnrollRequest{PairingCode: code.Plaintext, SiteURL: "https://legacy-4.example.com", AgentPublicKey: pub})
	assertSiteLimitReached(t, err, 3, 3, "free")

	// The public /enroll endpoint must surface a clean upgrade message, not a
	// generic internal error string.
	de, _ := domain.AsDomain(err)
	if de.Message == "" {
		t.Fatal("expected a non-empty caller-facing message on the enroll-path 402")
	}
}

// ---- Paths 4/5: the site-bound consume (ConsumeEnrollmentCode / ---------
// ---- ConsumeSiteBoundCode) never re-checks the cap -----------------------
//
// By design: the site row was already created (and counted) by
// MintEnrollmentCode/CreatePending. Re-checking here would be both redundant
// and WRONG — it must not block a pending site's agent from finishing
// enrollment just because the tenant has since reached its cap.

func TestConsumeEnrollmentCode_SucceedsEvenAtCap(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "cap-consume")

	repo := site.NewRepo(pool)
	conn := site.NewConnectionService(repo, domain.NewValidator(), nil, nil, domain.SystemClock{}, nil)
	svc := site.NewService(repo, domain.NewValidator(), domain.SystemClock{})
	svc.SetConnectionService(conn)

	billingSvc := newHostedBillingService(pool, true)
	site.SetBillingGate(repo, billingSvc)
	svc.SetBillingGate(billingSvc)

	var lastCode site.EnrollmentCode
	for i := 0; i < 3; i++ {
		code, err := conn.MintEnrollmentCode(ctx, site.MintEnrollmentInput{
			TenantID: tenant, URL: fmt.Sprintf("https://consume-%d.example.com", i), Name: "s",
		})
		if err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
		lastCode = code
	}
	// The tenant is now AT cap (3 non-archived sites); the 3rd — lastCode's
	// site — is still pending_enrollment and its code has not been consumed.

	_, _, pub := genKey(t)
	s, err := svc.Enroll(ctx, site.EnrollRequest{
		PairingCode: lastCode.Plaintext, SiteURL: "https://consume-2.example.com", AgentPublicKey: pub,
	})
	if err != nil {
		t.Fatalf("consuming the site-bound code for an ALREADY-COUNTED pending site "+
			"should succeed even though the tenant is at cap, got: %v", err)
	}
	if s.ConnectionState != site.StateConnected {
		t.Fatalf("expected connected, got %s", s.ConnectionState)
	}
}

// ---- Path 6: Restore (archived -> disconnected un-archive) ---------------

func TestSiteCap_BlocksRestore(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "cap-restore")

	repo := site.NewRepo(pool)
	conn := site.NewConnectionService(repo, domain.NewValidator(), nil, nil, domain.SystemClock{}, nil)

	// The site that will be archived then restored.
	target, err := conn.MintEnrollmentCode(ctx, site.MintEnrollmentInput{TenantID: tenant, URL: "https://restore-target.example.com", Name: "target"})
	if err != nil {
		t.Fatalf("mint target: %v", err)
	}
	if err := conn.Archive(ctx, site.ActorSiteInput{TenantID: tenant, SiteID: target.SiteID}); err != nil {
		t.Fatalf("archive target: %v", err)
	}

	// Fill the cap with 3 OTHER active sites (archiving excludes the target
	// from the count, so these 3 alone reach the free-tier cap).
	for i := 0; i < 3; i++ {
		if _, err := conn.MintEnrollmentCode(ctx, site.MintEnrollmentInput{
			TenantID: tenant, URL: fmt.Sprintf("https://restore-filler-%d.example.com", i), Name: "s",
		}); err != nil {
			t.Fatalf("seed filler %d: %v", i, err)
		}
	}

	site.SetBillingGate(repo, newHostedBillingService(pool, true))

	_, err = conn.Restore(ctx, site.ActorSiteInput{TenantID: tenant, SiteID: target.SiteID})
	assertSiteLimitReached(t, err, 3, 3, "free")
}

// ---- Path 7: BeginReEnrollment (archived -> pending_enrollment re-enroll) -
//
// Security review Finding B: BeginReEnrollment is a SIBLING archived-exit path
// to Restore (archived -> pending_enrollment vs archived -> disconnected) but
// went through the plain `transition` helper with no CheckSiteQuota, so a free
// tenant already at cap could reactivate an archived site past its limit
// through this path even though Restore was correctly gated. The fix derives
// the site-cap re-check directly from the (from, to) state pair in
// pgRepo.Transition — any exit from archived re-checks the cap — rather than
// trusting a caller-supplied flag, so this path (and any future archived-exit
// path) cannot bypass the cap by simply not setting one.

func TestSiteCap_BlocksBeginReEnrollment(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "cap-reenroll")

	repo := site.NewRepo(pool)
	conn := site.NewConnectionService(repo, domain.NewValidator(), nil, nil, domain.SystemClock{}, nil)

	// The site that will be archived then re-enrolled.
	target, err := conn.MintEnrollmentCode(ctx, site.MintEnrollmentInput{TenantID: tenant, URL: "https://reenroll-target.example.com", Name: "target"})
	if err != nil {
		t.Fatalf("mint target: %v", err)
	}
	if err := conn.Archive(ctx, site.ActorSiteInput{TenantID: tenant, SiteID: target.SiteID}); err != nil {
		t.Fatalf("archive target: %v", err)
	}

	// Fill the cap with 3 OTHER active sites (archiving excludes the target
	// from the count, so these 3 alone reach the free-tier cap).
	for i := 0; i < 3; i++ {
		if _, err := conn.MintEnrollmentCode(ctx, site.MintEnrollmentInput{
			TenantID: tenant, URL: fmt.Sprintf("https://reenroll-filler-%d.example.com", i), Name: "s",
		}); err != nil {
			t.Fatalf("seed filler %d: %v", i, err)
		}
	}

	site.SetBillingGate(repo, newHostedBillingService(pool, true))

	// Before the Finding-B fix this succeeded (bypassing the cap): the archived
	// site would reactivate straight to pending_enrollment past the limit.
	_, err = conn.BeginReEnrollment(ctx, site.ActorSiteInput{TenantID: tenant, SiteID: target.SiteID})
	assertSiteLimitReached(t, err, 3, 3, "free")
}

// TestSiteCap_HostedDisabled_BeginReEnrollmentUncapped proves BeginReEnrollment
// is uncapped when hosted billing is off, mirroring every other path's
// hosted=OFF behavior (TestSiteCap_HostedDisabled_AllPathsUncapped).
func TestSiteCap_HostedDisabled_BeginReEnrollmentUncapped(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "cap-reenroll-disabled")

	repo := site.NewRepo(pool)
	conn := site.NewConnectionService(repo, domain.NewValidator(), nil, nil, domain.SystemClock{}, nil)

	target, err := conn.MintEnrollmentCode(ctx, site.MintEnrollmentInput{TenantID: tenant, URL: "https://reenroll-disabled-target.example.com", Name: "target"})
	if err != nil {
		t.Fatalf("mint target: %v", err)
	}
	if err := conn.Archive(ctx, site.ActorSiteInput{TenantID: tenant, SiteID: target.SiteID}); err != nil {
		t.Fatalf("archive target: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := conn.MintEnrollmentCode(ctx, site.MintEnrollmentInput{
			TenantID: tenant, URL: fmt.Sprintf("https://reenroll-disabled-filler-%d.example.com", i), Name: "s",
		}); err != nil {
			t.Fatalf("seed filler %d: %v", i, err)
		}
	}

	site.SetBillingGate(repo, newHostedBillingService(pool, false)) // hosted OFF

	if _, err := conn.BeginReEnrollment(ctx, site.ActorSiteInput{TenantID: tenant, SiteID: target.SiteID}); err != nil {
		t.Fatalf("BeginReEnrollment should succeed when hosted billing is disabled: %v", err)
	}
}

// ---- hosted=OFF: every path is uncapped -----------------------------------

func TestSiteCap_HostedDisabled_AllPathsUncapped(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "cap-disabled")

	repo := site.NewRepo(pool)
	svc := site.NewService(repo, domain.NewValidator(), domain.SystemClock{})
	conn := site.NewConnectionService(repo, domain.NewValidator(), nil, nil, domain.SystemClock{}, nil)
	svc.SetConnectionService(conn)

	billingSvc := newHostedBillingService(pool, false) // hosted OFF
	svc.SetBillingGate(billingSvc)
	site.SetBillingGate(repo, billingSvc)

	// Direct create, well past the free-tier cap of 3.
	for i := 0; i < 5; i++ {
		if _, err := svc.Create(ctx, site.CreateInput{TenantID: tenant, URL: fmt.Sprintf("https://disabled-direct-%d.example.com", i), Name: "s"}); err != nil {
			t.Fatalf("direct create %d should succeed when hosted billing is disabled: %v", i, err)
		}
	}
	// Site-first mint, also past cap.
	for i := 0; i < 5; i++ {
		if _, err := conn.MintEnrollmentCode(ctx, site.MintEnrollmentInput{TenantID: tenant, URL: fmt.Sprintf("https://disabled-mint-%d.example.com", i), Name: "s"}); err != nil {
			t.Fatalf("mint %d should succeed when hosted billing is disabled: %v", i, err)
		}
	}
}

// ---- Concurrent-enroll race: the advisory lock is the TOCTOU fix ---------

func TestSiteCap_ConcurrentCreateRace_ExactlyOneWins(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "cap-race")

	repo := site.NewRepo(pool)
	svc := site.NewService(repo, domain.NewValidator(), domain.SystemClock{})

	// Seed cap-1 (2) existing sites, unwired (gate not yet attached).
	for i := 0; i < 2; i++ {
		if _, err := svc.Create(ctx, site.CreateInput{TenantID: tenant, URL: fmt.Sprintf("https://race-seed-%d.example.com", i), Name: "s"}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	svc.SetBillingGate(newHostedBillingService(pool, true))

	const n = 2
	var wg sync.WaitGroup
	results := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := svc.Create(ctx, site.CreateInput{
				TenantID: tenant, URL: fmt.Sprintf("https://race-attempt-%d.example.com", i), Name: "s",
			})
			results[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	successes, rejections := 0, 0
	for _, err := range results {
		if err == nil {
			successes++
			continue
		}
		if de, ok := domain.AsDomain(err); ok && de.Kind == domain.KindPaymentRequired {
			rejections++
			continue
		}
		t.Fatalf("unexpected error: %v", err)
	}
	if successes != 1 || rejections != 1 {
		t.Fatalf("want exactly 1 success and 1 rejection out of %d concurrent creates at cap-1, got %d successes, %d rejections",
			n, successes, rejections)
	}
}

// ---- Grandfather backfill (m91 migration-time data fix) ------------------

// m91MigrationVersion is the embedded migration filename (sans .sql) that
// adds the hosted-billing columns/table/function and runs the grandfather
// backfill. The harness below stops short of applying it so the test can
// seed the exact pre-m91 over-cap scenario the backfill must heal.
const m91MigrationVersion = "20260724000000_m91_hosted_billing_substrate"

// startPostgresBeforeM91 mirrors startPostgresBeforeM88 (see
// update_m88_dedup_test.go, same package): boots a fresh container, applies
// every embedded migration up to (not including) m91 as the bootstrap
// superuser, and returns that admin pool so the caller can seed pre-m91 data
// before finishing the boot with pool.Migrate(ctx).
func startPostgresBeforeM91(t *testing.T) *db.Pool {
	t.Helper()
	ctx := context.Background()

	skipIfDockerUnavailable(t, ctx, "postgres")

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("wpmgr"),
		tcpostgres.WithUsername("wpmgr"),
		tcpostgres.WithPassword("wpmgr"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	// container can be non-nil even when err != nil (partial start); register
	// cleanup before the error check so a failure path cannot leak it (see
	// rls_integration_test.go's startPostgres).
	if container != nil {
		t.Cleanup(func() { _ = container.Terminate(ctx) })
	}
	if err != nil {
		setupFatalfOrSkipIfDaemonDied(t, ctx, err, "postgres: container start")
	}

	adminDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	pool, err := db.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(pool.Close)

	applyMigrationsBefore(t, pool, m91MigrationVersion)
	return pool
}

// TestGrandfatherBackfill_OverCapTenantKeepsOperating is the non-destructive
// prime-directive regression test: a tenant that already had 5 non-archived
// sites before hosted billing existed must NOT be silently over-cap the
// instant m91 lands. The backfill must write plan_overrides.max_sites=5, so
// the tenant keeps operating; only a 6th (new) site is blocked once hosted
// billing is actually turned on.
func TestGrandfatherBackfill_OverCapTenantKeepsOperating(t *testing.T) {
	pool := startPostgresBeforeM91(t)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "m91-grandfather")

	// Seed 5 non-archived sites — over the free-tier cap of 3 — against the
	// pre-m91 schema (no plan/plan_overrides columns exist on tenants yet).
	for i := 0; i < 5; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, $3)`,
			tenant, fmt.Sprintf("https://grandfather-%d.example.com", i), "site",
		); err != nil {
			t.Fatalf("seed site %d: %v", i, err)
		}
	}

	// Finish the boot: applies m91 (schema + grandfather backfill).
	if err := pool.Migrate(ctx); err != nil {
		t.Fatalf("m91 migration failed: %v", err)
	}

	var overridesJSON []byte
	if err := pool.QueryRow(ctx, `SELECT plan_overrides FROM tenants WHERE id = $1`, tenant).Scan(&overridesJSON); err != nil {
		t.Fatalf("read plan_overrides: %v", err)
	}
	var overrides struct {
		MaxSites int `json:"max_sites"`
	}
	if err := json.Unmarshal(overridesJSON, &overrides); err != nil {
		t.Fatalf("unmarshal plan_overrides: %v", err)
	}
	if overrides.MaxSites != 5 {
		t.Fatalf("plan_overrides.max_sites = %d, want 5 (grandfathered to the tenant's existing count)", overrides.MaxSites)
	}

	// Hosted billing turned on: the tenant is AT its grandfathered cap (5
	// active sites, override max_sites=5) — the 5 existing sites are
	// untouched, but a 6th is blocked.
	billingSvc := newHostedBillingService(pool, true)
	err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		return billingSvc.CheckSiteCreate(ctx, tx, tenant)
	})
	assertSiteLimitReached(t, err, 5, 5, "free")
}

// TestGrandfatherBackfill_IsNoopUnderCap proves the common case (a tenant at
// or under the free cap when m91 lands) is unaffected: no max_sites override
// is written, so future growth is still gated by the real ladder cap (3).
//
// pool.Migrate applies every embedded migration, not just m91 — including m95
// (M16 Phase B), which unconditionally grandfathers
// plan_overrides.managed_backup_storage=true onto EVERY tenant that exists at
// migration time, regardless of site count. That is a deliberate, unrelated
// backfill (see m95's own migration comment) and is asserted for explicitly
// below; it does not make this tenant's max_sites backfill any less of a
// no-op, so the assertion checks for the ABSENCE of a max_sites key rather
// than requiring the whole plan_overrides document to be empty.
func TestGrandfatherBackfill_IsNoopUnderCap(t *testing.T) {
	pool := startPostgresBeforeM91(t)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "m91-noop")
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, $3)`,
			tenant, fmt.Sprintf("https://noop-%d.example.com", i), "site",
		); err != nil {
			t.Fatalf("seed site %d: %v", i, err)
		}
	}

	if err := pool.Migrate(ctx); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	var overridesJSON []byte
	if err := pool.QueryRow(ctx, `SELECT plan_overrides FROM tenants WHERE id = $1`, tenant).Scan(&overridesJSON); err != nil {
		t.Fatalf("read plan_overrides: %v", err)
	}
	var overrides map[string]any
	if err := json.Unmarshal(overridesJSON, &overrides); err != nil {
		t.Fatalf("unmarshal plan_overrides: %v", err)
	}
	if _, hasMaxSites := overrides["max_sites"]; hasMaxSites {
		t.Fatalf("plan_overrides = %s, want no max_sites key for an under-cap tenant", overridesJSON)
	}
	// m95 (M16 Phase B) grandfather: every pre-existing tenant, capped or not,
	// gets managed_backup_storage=true so nobody loses managed-storage access
	// the instant the gate activates.
	if managed, ok := overrides["managed_backup_storage"].(bool); !ok || !managed {
		t.Fatalf("plan_overrides = %s, want managed_backup_storage=true (m95 grandfather)", overridesJSON)
	}
}
