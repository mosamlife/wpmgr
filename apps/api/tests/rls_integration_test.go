// Package tests holds integration tests that exercise the real Postgres schema
// (migrations + RLS) via testcontainers-go. They require Docker; if Docker is
// unavailable the tests skip rather than fail.
package tests

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// setupFatalf fails the test with a message that cannot be mistaken for an
// assertion made by the test body. A full-package run starts hundreds of
// testcontainers over roughly ten minutes, and under that load an
// infrastructure hiccup at one of these stages (container start, connection
// string, migrate, connect, role provisioning) otherwise surfaces as a bare
// "%v" attached to whatever test happened to be running when it hit — which
// reads exactly like that test's own assertion failed. The SETUP FAILURE
// prefix plus the named stage let the next reader classify it in one glance
// instead of re-running the investigation that produced this comment.
func setupFatalf(t testing.TB, err error, stage string) {
	t.Helper()
	t.Fatalf("SETUP FAILURE (infrastructure, not the test's own assertion) at stage=%q: %v", stage, err)
}

// setupSkipf is setupFatalf's skip counterpart, used only for "Docker is not
// available on this machine at all" — the one setup failure that is not a
// mid-run flake and that every other test in the package would hit
// identically, so skipping (rather than failing) is the honest signal.
func setupSkipf(t testing.TB, err error, stage string) {
	t.Helper()
	t.Skipf("SETUP SKIP (infrastructure, not the test's own assertion) at stage=%q: %v", stage, err)
}

// skipIfDockerUnavailable positively detects whether the Docker/OCI provider
// testcontainers will use is reachable at all, and skips (via setupSkipf) if
// it is not. This is checked directly, up front, before any container is
// asked to start, rather than being inferred from whatever error a later
// container-start call happens to return.
//
// That distinction matters because a container that fails to START despite
// Docker being reachable (bad image tag, no space left, registry pull
// failure, resource limits) is a real setup failure, not "Docker is not
// installed here" — it must go to setupFatalf and turn the run red, never
// setupSkipf. Routing a start failure to Skip is exactly the failure mode
// this test package exists to eliminate: a package-level "ok" over a suite
// that asserted nothing.
//
// setupFatalfOrSkipIfDaemonDied below is the one other path that may resolve
// to a skip, for the narrow case of a container-start error specifically. It
// still only skips on a second, independent positive Health() probe — never
// by inspecting the start error's text — so it cannot reclassify an ordinary
// start failure as unavailability.
func skipIfDockerUnavailable(t testing.TB, ctx context.Context, stage string) {
	t.Helper()
	provider, err := testcontainers.ProviderDocker.GetProvider()
	if err != nil {
		setupSkipf(t, err, stage+" (docker provider unavailable)")
		return
	}
	if err := provider.Health(ctx); err != nil {
		setupSkipf(t, err, stage+" (docker daemon unreachable)")
	}
}

// setupFatalfOrSkipIfDaemonDied handles an error from a container-START call
// specifically (tcpostgres.Run, testcontainers.GenericContainer). At that
// point skipIfDockerUnavailable already proved the daemon was reachable
// immediately before this container's start began, so an error here is
// usually a genuine start failure (bad image tag, no space left, registry
// pull failure) with the daemon perfectly healthy — that MUST stay fatal, or
// this package regresses to exactly the "skip on anything" failure mode it
// was built to eliminate.
//
// The one case that should skip instead is the daemon dying in the window
// this container's own startup (image pull plus up to a 60s wait strategy)
// spans: Docker Desktop crashing, an OOM kill, disk pressure taking the
// daemon down mid-run. That is told apart from an ordinary start failure with
// a SECOND positive Health() probe, the same mechanism skipIfDockerUnavailable
// already trusts — never by pattern-matching startErr's text. An ordinary
// start failure re-probes healthy and falls straight through to setupFatalf.
func setupFatalfOrSkipIfDaemonDied(t testing.TB, ctx context.Context, startErr error, stage string) {
	t.Helper()
	provider, provErr := testcontainers.ProviderDocker.GetProvider()
	if provErr == nil {
		if healthErr := provider.Health(ctx); healthErr != nil {
			setupSkipf(t, fmt.Errorf("start error: %v; daemon health re-probe now fails: %w", startErr, healthErr),
				stage+" (docker daemon died mid-start)")
			return
		}
	}
	setupFatalf(t, startErr, stage)
}

// startPostgres spins up an ephemeral Postgres, applies the embedded
// migrations as the bootstrap superuser, then provisions a dedicated
// NON-superuser application role and returns a pool connected as that role.
//
// This mirrors the production requirement: Postgres superusers (and roles with
// BYPASSRLS) ignore RLS policies entirely, so the application MUST connect as a
// plain, non-superuser role for the sites_tenant_isolation policy to take
// effect. The default container user is a superuser, hence the extra role.
func startPostgres(t testing.TB) *db.Pool {
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
	// Register cleanup BEFORE inspecting err: tcpostgres.Run can return a
	// non-nil container alongside a non-nil error on a partial start (the
	// container was created, or even started, and a later step in Run
	// failed) — testcontainers-go's own GenericContainer says as much: "At
	// this point `c` might not be nil. Give the caller an opportunity to call
	// Destroy on the container." setupFatalf below calls t.Fatalf, which
	// never returns (runtime.Goexit), so anything after it never runs; if
	// cleanup registration came after that call, a partially-started
	// container would leak for the life of the machine on every failure path.
	if container != nil {
		t.Cleanup(func() { _ = container.Terminate(ctx) })
	}
	if err != nil {
		setupFatalfOrSkipIfDaemonDied(t, ctx, err, "postgres: container start")
	}

	adminDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		setupFatalf(t, err, "postgres: connection string")
	}

	// Apply migrations as the bootstrap superuser.
	adminPool, err := db.Connect(ctx, adminDSN)
	if err != nil {
		setupFatalf(t, err, "postgres: connect as bootstrap superuser")
	}
	if err := adminPool.Migrate(ctx); err != nil {
		setupFatalf(t, err, "postgres: run embedded migrations")
	}

	// The auth migration already created the wpmgr_app role (NOLOGIN, no
	// password) and granted it table privileges. Here we just give it a login +
	// password so the test can connect AS that non-superuser role. (Grants for
	// any pre-auth-migration tables are reasserted to be safe.)
	for _, stmt := range []string{
		"ALTER ROLE wpmgr_app LOGIN PASSWORD 'app'",
		"GRANT USAGE ON SCHEMA public TO wpmgr_app",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO wpmgr_app",
		// audit_log is append-only: re-revoke mutation in case the blanket grant
		// above re-added it.
		"REVOKE UPDATE, DELETE, TRUNCATE ON audit_log FROM wpmgr_app",
		// The m122/ADR-064 context tables are append-only for the same reason
		// and by the same mechanism, so they need the same re-revoke. Without
		// these two lines the blanket GRANT above hands wpmgr_app an UPDATE it
		// does not hold in production, and every test in this package would run
		// against privileges no real install has -- including the append-only
		// proofs, which would then pass by testing nothing.
		"REVOKE UPDATE, DELETE, TRUNCATE ON org_context_versions FROM wpmgr_app",
		"REVOKE UPDATE, DELETE, TRUNCATE ON site_context_versions FROM wpmgr_app",
	} {
		if _, err := adminPool.Exec(ctx, stmt); err != nil {
			setupFatalf(t, err, "postgres: provision app role ("+stmt+")")
		}
	}
	adminPool.Close()

	appDSN := strings.Replace(adminDSN, "wpmgr:wpmgr@", "wpmgr_app:app@", 1)
	pool, err := db.Connect(ctx, appDSN)
	if err != nil {
		setupFatalf(t, err, "postgres: connect as wpmgr_app")
	}
	t.Cleanup(pool.Close)

	adminDSNsMu.Lock()
	adminDSNs[pool] = adminDSN
	adminDSNsMu.Unlock()
	t.Cleanup(func() {
		adminDSNsMu.Lock()
		delete(adminDSNs, pool)
		adminDSNsMu.Unlock()
	})
	return pool
}

// adminDSNs maps an app pool to its container's superuser DSN, so tests that
// must act outside the app's RLS/privilege constraints (e.g. tampering with the
// append-only audit_log) can open a superuser connection.
//
// adminDSNsMu is not tidiness. startPostgres is called from t.Parallel() tests
// (scan_plugin_checksums_regression_test.go holds the package's only two), so
// two goroutines write this map at once, and a concurrent map write is a FATAL
// Go runtime error: it aborts the whole test binary, taking every unrelated
// test with it, with a stack that points at whichever test happened to be
// running. That is the shape of an intermittent full-suite failure that never
// reproduces in isolation.
var (
	adminDSNsMu sync.Mutex
	adminDSNs   = map[*db.Pool]string{}
)

// connectAdmin opens a superuser pool for the most recently started container.
// It is only used to simulate out-of-band tampering in tests.
func connectAdmin(t *testing.T, app *db.Pool) *db.Pool {
	t.Helper()
	adminDSNsMu.Lock()
	dsn, ok := adminDSNs[app]
	adminDSNsMu.Unlock()
	if !ok {
		// Caller misuse, not a container flake: connectAdmin only knows a DSN
		// for a pool that startPostgres itself returned.
		t.Fatal("SETUP FAILURE (test helper misuse, not the test's own assertion): connectAdmin called with a pool startPostgres never returned")
	}
	pool, err := db.Connect(context.Background(), dsn)
	if err != nil {
		setupFatalf(t, err, "postgres: connectAdmin reconnect as bootstrap superuser")
	}
	return pool
}

// seedTenant inserts a tenant row directly (tenants are not RLS-scoped).
func seedTenant(t testing.TB, pool *db.Pool, slug string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(),
		"INSERT INTO tenants (name, slug) VALUES ($1, $2) RETURNING id", slug, slug).Scan(&id)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

// TestRLSIsolation proves that the sites_tenant_isolation policy prevents a
// query running under one tenant's app.tenant_id from seeing another tenant's
// rows, even when the explicit WHERE filter is bypassed.
func TestRLSIsolation(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenantA := seedTenant(t, pool, "tenant-a")
	tenantB := seedTenant(t, pool, "tenant-b")

	repo := site.NewRepo(pool)

	siteA, err := repo.Create(ctx, site.CreateInput{TenantID: tenantA, URL: "https://a.example.com", Name: "A"})
	if err != nil {
		t.Fatalf("create site A: %v", err)
	}
	if _, err := repo.Create(ctx, site.CreateInput{TenantID: tenantB, URL: "https://b.example.com", Name: "B"}); err != nil {
		t.Fatalf("create site B: %v", err)
	}

	// Tenant A lists: must see only its own site.
	listA, err := repo.List(ctx, site.ListInput{TenantID: tenantA, Limit: 100})
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(listA) != 1 || listA[0].TenantID != tenantA {
		t.Fatalf("tenant A leaked rows: %+v", listA)
	}

	// Tenant B cannot fetch tenant A's site by ID (RLS hides it -> not found).
	if _, err := repo.Get(ctx, tenantB, siteA.ID); err == nil {
		t.Fatalf("tenant B was able to read tenant A's site")
	}

	// Direct cross-tenant SELECT under tenant B's GUC returns zero rows even
	// though we query for tenant A's id with no tenant filter at all.
	err = pool.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM sites WHERE id = $1", siteA.ID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("RLS failed: cross-tenant SELECT returned %d rows", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cross-tenant select tx: %v", err)
	}
}
