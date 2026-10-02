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
// migrations as wpmgr_owner — a NOSUPERUSER NOBYPASSRLS role that owns the
// database, mirroring production's WPMGR_DB_MIGRATION_DSN role (see
// DBConfig.MigrateDSN and cmd/wpmgr/main.go) — then provisions a dedicated
// NON-superuser application role and returns a pool connected as that role.
//
// This mirrors two production requirements, not one:
//
//   - Postgres superusers (and roles with BYPASSRLS) ignore RLS policies
//     entirely, so the application MUST connect as a plain, non-superuser role
//     for the sites_tenant_isolation policy to take effect.
//   - Production does not MIGRATE as a superuser either. WPMGR_DB_MIGRATION_DSN
//     is a NOSUPERUSER NOBYPASSRLS owner, so every FORCE ROW LEVEL SECURITY
//     table (e.g. mcp_grants) applies its policies to the migrator itself. A
//     migration whose UPDATE backfill relies on an ambient GUC the migrator
//     never sets touches zero rows under RLS and then fails its own following
//     SET NOT NULL — exactly what happened in production on 2026-09-28 (m136,
//     fixed by #770's m139/m140 prefill). Migrating as the container's
//     bootstrap superuser here, as this helper did before, bypasses RLS during
//     the apply and cannot reproduce that failure, so a migration shaped that
//     way passed every test in this package while it failed in production.
//     apps/api/tests/mcp_scope_columns_prefill_migration_test.go proved the
//     production shape is reproducible this way for m136/m137 specifically;
//     this makes it the DEFAULT for every test in the package rather than a
//     one-off harness.
//
// The container's bootstrap superuser connection is still opened first, but
// only to provision wpmgr_owner and hand it the database — it never runs a
// migration body itself, and its DSN is kept (via adminDSNs) purely as the
// existing connectAdmin() escape hatch for tests that must tamper with data
// outside RLS/privilege constraints entirely (e.g. the append-only audit_log).
//
// What this change does and does not catch, precisely, because it is easy to
// overstate: every startPostgres(t) call migrates a freshly created, EMPTY
// database, same as a fresh install. On an empty database this still catches
// a migration that needs superuser, ownership or role privileges the real
// migrator lacks, or that writes to a FORCE ROW LEVEL SECURITY table in a way
// that fails regardless of whether any rows exist yet (a bad GRANT, a
// privilege-gated DDL statement). It does NOT, by itself, catch m136's actual
// failure shape — a backfill whose UPDATE/INSERT silently touches zero rows
// under RLS because the migrator sets no GUC. On an empty table that is
// indistinguishable from correct behaviour: zero rows were the right answer
// either way. Catching that class requires a test that seeds rows BEFORE
// calling Migrate, so the migration has real pre-existing data to (fail to)
// act on — see e.g. update_m88_dedup_test.go's startPostgresBeforeM88 and its
// siblings, which is where this change actually found several such bugs.
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

	// Connect as the bootstrap superuser ONLY to provision wpmgr_owner below.
	// It never applies a migration body itself (see the doc comment above).
	adminPool, err := db.Connect(ctx, adminDSN)
	if err != nil {
		setupFatalf(t, err, "postgres: connect as bootstrap superuser")
	}

	// wpmgr_owner mirrors production's WPMGR_DB_MIGRATION_DSN role: LOGIN,
	// NOSUPERUSER, NOBYPASSRLS, CREATEROLE (m1's migration does `CREATE ROLE
	// wpmgr_app`, so the migrator needs that attribute), and it owns the
	// database so it owns every table its own migrations create — PostgreSQL
	// 16's "public" schema is owned by pg_database_owner, so owning the
	// database is owning the schema every migration creates into. Same shape
	// as apps/api/tests/mcp_scope_columns_prefill_migration_test.go's
	// startPostgresAsOwner.
	for _, stmt := range []string{
		"CREATE ROLE wpmgr_owner LOGIN PASSWORD 'owner' NOSUPERUSER NOBYPASSRLS CREATEROLE",
		"ALTER DATABASE wpmgr OWNER TO wpmgr_owner",
	} {
		if _, err := adminPool.Exec(ctx, stmt); err != nil {
			setupFatalf(t, err, "postgres: provision owner role ("+stmt+")")
		}
	}
	adminPool.Close()

	ownerDSN := strings.Replace(adminDSN, "wpmgr:wpmgr@", "wpmgr_owner:owner@", 1)
	ownerPool, err := db.Connect(ctx, ownerDSN)
	if err != nil {
		setupFatalf(t, err, "postgres: connect as wpmgr_owner")
	}

	// The premise of every test in this package: the migrating role is subject
	// to row security like any other. If this ever reads true, every RLS proof
	// below it is inert (m112's failure shape) and a migration that only works
	// as superuser or BYPASSRLS would pass here undetected.
	var ownerSuper, ownerBypass bool
	if err := ownerPool.QueryRow(ctx,
		"SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user").
		Scan(&ownerSuper, &ownerBypass); err != nil {
		setupFatalf(t, err, "postgres: read owner role attributes")
	}
	if ownerSuper || ownerBypass {
		t.Fatalf("wpmgr_owner has rolsuper=%t rolbypassrls=%t; startPostgres's premise is a role row security applies to",
			ownerSuper, ownerBypass)
	}

	// Apply migrations as the owner, exactly as cmd/wpmgr does with
	// WPMGR_DB_MIGRATION_DSN.
	if err := ownerPool.Migrate(ctx); err != nil {
		setupFatalf(t, err, "postgres: run embedded migrations")
	}

	// The auth migration already created the wpmgr_app role (NOLOGIN, no
	// password) and granted it table privileges. Here we just give it a login +
	// password so the test can connect AS that non-superuser role. wpmgr_owner
	// can do this without superuser: it owns every table via the ALTER DATABASE
	// above, so it holds grant option on all of them. (Grants for any
	// pre-auth-migration tables are reasserted to be safe.)
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
		// m133's assistant_update_proposals is immutable-after-insert in its
		// FACTS but not in its workflow columns, so it needs the finer form of
		// the same re-revoke: drop the table-level UPDATE the blanket grant
		// above re-added, then hand back only the columns that may move.
		//
		// Without these two lines expires_at is updatable in tests and is not
		// in production, so "an expired proposal cannot be approved" would be
		// provable here while being false for every real install - one UPDATE
		// moves the window forward to meet decided_at. That is the vacuous
		// shape the comment above describes, and it is why the two statements
		// are ordered revoke-then-grant: a table-level UPDATE covers every
		// column and PostgreSQL will not carve a single column out of it.
		"REVOKE UPDATE ON assistant_update_proposals FROM wpmgr_app",
		"GRANT UPDATE (state, decided_at, decided_by_user_id, dispatched_update_run_id, note) ON assistant_update_proposals TO wpmgr_app",
		// And the DELETE, for the same reason as the three tables above: m133
		// DECISION 9 revokes it in the migration, the blanket GRANT re-adds it,
		// and a proof that the approval record cannot be destroyed would pass
		// here against a privilege no real install has. An immutable column
		// inside a deletable row is not immutable.
		"REVOKE DELETE, TRUNCATE ON assistant_update_proposals FROM wpmgr_app",
		// m151's assistant_cache_purge_requests is m133's shape and needs the
		// same three statements for the same reasons: without them url,
		// site_host, digest_nonce and presented_digest are updatable in tests
		// and not in production, and the row is deletable in tests and not in
		// production, so every immutability and undeletability proof would
		// pass against privileges no real install has.
		"REVOKE UPDATE ON assistant_cache_purge_requests FROM wpmgr_app",
		"GRANT UPDATE (state, decided_at, decided_by_user_id, withdrawn_at, claimed_at, cache_purge_audit_id, dispatch_attempts, last_attempt_at, last_attempt_code, outcome, not_sent_reason, outcome_at, hosting_caches_cleared, hosting_caches_skipped, origin_only_confirmed, wpmgr_cdn, site_reported_text) ON assistant_cache_purge_requests TO wpmgr_app",
		"REVOKE DELETE, TRUNCATE ON assistant_cache_purge_requests FROM wpmgr_app",
		// m147's install_owner is insert-once: the record of who set up the
		// install must never be re-pointed or removed. Same re-revoke, same
		// reason: without it the blanket GRANT above lets a test re-point the
		// row, which no real install can do.
		"REVOKE UPDATE, DELETE, TRUNCATE ON install_owner FROM wpmgr_app",
		// m153's content_integrations and its audit are SELECT-only for
		// wpmgr_app: the migration revokes every write, and the one write path
		// is the SECURITY DEFINER admin_upsert_content_integration. Without
		// this line the blanket GRANT above hands wpmgr_app INSERT, UPDATE and
		// DELETE that no real install has, and the write-fence proof tests a
		// database nobody runs.
		"REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON content_integrations, content_integrations_audit FROM wpmgr_app",
		// m153 site_content_inventory_runs: no DELETE for wpmgr_app in the migration.
		"REVOKE DELETE, TRUNCATE ON site_content_inventory_runs FROM wpmgr_app",
		// m155's ability_catalogue and its audit, for the same reason as m153's
		// content_integrations: SELECT-only for wpmgr_app, written only through
		// the SECURITY DEFINER admin_upsert_ability_catalogue_entry.
		"REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON ability_catalogue, ability_catalogue_audit FROM wpmgr_app",
		// m160's ability_read_side_effect_reports is definer-only: the
		// migration revokes ALL from wpmgr_app, and the one path in is the
		// SECURITY DEFINER record_ability_read_side_effect. Without this line
		// the blanket GRANT above hands wpmgr_app the four privileges no real
		// install has, and the refusal proof tests a database nobody runs.
		"REVOKE ALL ON ability_read_side_effect_reports FROM wpmgr_app",
		// m160's ability_tenant_disables: SELECT and UPDATE of the two
		// re-enable columns only. No INSERT (the definer writes it), no DELETE
		// (the record of a disable is kept). Revoke-then-grant, as m151.
		"REVOKE ALL ON ability_tenant_disables FROM wpmgr_app",
		"GRANT SELECT ON ability_tenant_disables TO wpmgr_app",
		"GRANT UPDATE (reenabled_at, reenabled_by_user_id) ON ability_tenant_disables TO wpmgr_app",
		// m161's rest_route_catalogue and its audit, for the same reason as
		// m155's ability_catalogue: SELECT-only for wpmgr_app, written only
		// through the SECURITY DEFINER admin_upsert_rest_route and
		// stamp_wpmgr_rest_route_hash.
		"REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON rest_route_catalogue, rest_route_catalogue_audit FROM wpmgr_app",
		// m155 site_ability_inventory_runs: no DELETE for wpmgr_app in the migration.
		"REVOKE DELETE, TRUNCATE ON site_ability_inventory_runs FROM wpmgr_app",
		// m155 site_ability_inventory: TRUNCATE revoked in the migration.
		"REVOKE TRUNCATE ON site_ability_inventory FROM wpmgr_app",
		// m156's assistant_ability_requests is m151's shape: the same three
		// statements, revoke-then-grant, so its immutability and
		// undeletability proofs run against a real install's privileges.
		"REVOKE UPDATE ON assistant_ability_requests FROM wpmgr_app",
		"GRANT UPDATE (state, decided_at, decided_by_user_id, withdrawn_at, dispatch_deadline_at, claimed_at, dispatch_attempts, last_attempt_at, last_attempt_code, unknown_since, ledger_checked_at, outcome, outcome_at, outcome_code, not_sent_reason, created_post_id, restored, trashed, site_reported_text, undo_state, undo_available_until, undo_by_user_id, undo_started_at, undo_finished_at) ON assistant_ability_requests TO wpmgr_app",
		"REVOKE DELETE, TRUNCATE ON assistant_ability_requests FROM wpmgr_app",
	} {
		if _, err := ownerPool.Exec(ctx, stmt); err != nil {
			setupFatalf(t, err, "postgres: provision app role ("+stmt+")")
		}
	}
	ownerPool.Close()

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

// connectOwner opens a pool for the most recently started container AS
// wpmgr_owner — the same NOSUPERUSER NOBYPASSRLS role production's migrator
// uses (see startPostgres's doc comment above) — rather than the bootstrap
// superuser connectAdmin returns. startPostgres already created wpmgr_owner
// and handed it the database; that role and its password ('owner') persist
// in the container after startPostgres's own owner connection closes, so this
// just derives the same DSN startPostgres itself used. Used by tests that
// re-run a migration file against an already-migrated database (idempotency
// / boot-safety checks): re-applying it as the bootstrap superuser would not
// reproduce the RLS exposure the real migrator has, exactly the gap this
// harness change (#775) exists to close.
func connectOwner(t *testing.T, app *db.Pool) *db.Pool {
	t.Helper()
	adminDSNsMu.Lock()
	adminDSN, ok := adminDSNs[app]
	adminDSNsMu.Unlock()
	if !ok {
		t.Fatal("SETUP FAILURE (test helper misuse, not the test's own assertion): connectOwner called with a pool startPostgres never returned")
	}
	ownerDSN := strings.Replace(adminDSN, "wpmgr:wpmgr@", "wpmgr_owner:owner@", 1)
	pool, err := db.Connect(context.Background(), ownerDSN)
	if err != nil {
		setupFatalf(t, err, "postgres: connectOwner reconnect as wpmgr_owner")
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
