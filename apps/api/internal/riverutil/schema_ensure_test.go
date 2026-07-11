package riverutil

// schema_ensure_test.go — GH #207 Bug 1 regression lock: v0.61.54 flipped the
// binary default of river.media_schema from "" to "media_encoder", which
// activated the previously-skipped EnsureSchema block on the media-encoder
// boot path. EnsureSchema's privileged CREATE SCHEMA/GRANT statements require
// DATABASE-level CREATE even when the schema already exists (Postgres checks
// the privilege before the IF-NOT-EXISTS short-circuit), so an encoder
// connecting with the unprivileged app role (no WPMGR_DB_MIGRATION_DSN owner
// DSN configured on that service) crash-looped on every boot with SQLSTATE
// 42501 even though the schema was already fully set up. These tests prove
// the fix: a readiness fast-path that needs no privileged DDL once the schema
// exists, the happy-path first-time creation staying green, and a genuine
// permission failure surfacing as a wrapped, actionable error.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// schemaTestEnv holds the two roles EnsureSchema's fast-path/first-creation
// split needs to be exercised end-to-end: an owner (the testcontainers
// superuser, full CREATE) and wpmgr_app, provisioned exactly like the real
// deployment (migrations/20260527130000_auth_multitenancy.sql:
// NOSUPERUSER NOBYPASSRLS, no database-level CREATE ever granted) with LOGIN
// added only so the test can connect as it directly (production grants
// LOGIN+password externally).
type schemaTestEnv struct {
	owner *pgxpool.Pool
	app   *pgxpool.Pool
}

// startSchemaTestPostgres spins up an ephemeral Postgres and provisions the
// owner + wpmgr_app roles. Trimmed, package-local copy of the pattern in
// cmd/media-encoder/reconcile_test.go's startReconcileTestPostgres (that
// helper is unexported in a different package and pulls in the full app
// schema migration, which EnsureSchema does not need).
func startSchemaTestPostgres(t *testing.T) *schemaTestEnv {
	t.Helper()
	ctx := context.Background()

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
	if err != nil {
		t.Skipf("skipping: cannot start postgres container (docker unavailable?): %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	ownerDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	ownerPool, err := pgxpool.New(ctx, ownerDSN)
	if err != nil {
		t.Fatalf("connect owner: %v", err)
	}
	t.Cleanup(ownerPool.Close)

	// wpmgr_app: same shape as the real app role, plus LOGIN so the test can
	// connect directly. Explicitly revoke database-level CREATE from both
	// PUBLIC and the role so the test does not depend on the base image's
	// ambient defaults — the real wpmgr_app role never receives this grant.
	for _, stmt := range []string{
		"CREATE ROLE wpmgr_app LOGIN PASSWORD 'app' NOSUPERUSER NOBYPASSRLS",
		"REVOKE CREATE ON DATABASE wpmgr FROM PUBLIC",
		"REVOKE CREATE ON DATABASE wpmgr FROM wpmgr_app",
	} {
		if _, err := ownerPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("provision wpmgr_app (%q): %v", stmt, err)
		}
	}

	appDSN := strings.Replace(ownerDSN, "wpmgr:wpmgr@", "wpmgr_app:app@", 1)
	appPool, err := pgxpool.New(ctx, appDSN)
	if err != nil {
		t.Fatalf("connect wpmgr_app: %v", err)
	}
	t.Cleanup(appPool.Close)

	var canCreate bool
	if err := appPool.QueryRow(ctx, "SELECT has_database_privilege('wpmgr_app', current_database(), 'CREATE')").Scan(&canCreate); err != nil {
		t.Fatalf("check wpmgr_app CREATE privilege: %v", err)
	}
	if canCreate {
		t.Fatal("test setup invariant broken: wpmgr_app must NOT have database-level CREATE (it never does in production)")
	}

	return &schemaTestEnv{owner: ownerPool, app: appPool}
}

// TestEnsureSchema_ReadinessFastPath_NoCreatePrivilegeNeeded is the direct
// GH #207 Bug 1 regression lock: once a schema is created and migrated (here,
// by the owner — mirroring the API's own boot, which always runs
// EnsureSchema with owner creds and owns migrations), every subsequent call
// made with a connection that provably CANNOT run CREATE SCHEMA must still
// succeed, proving the readiness fast-path — not the privileged path — is
// what ran.
func TestEnsureSchema_ReadinessFastPath_NoCreatePrivilegeNeeded(t *testing.T) {
	env := startSchemaTestPostgres(t)
	ctx := context.Background()
	const schema = "media_encoder"

	if err := EnsureSchema(ctx, env.owner, schema, "wpmgr_app"); err != nil {
		t.Fatalf("owner EnsureSchema (first creation): %v", err)
	}

	// Steady-state boot, simulated by calling EnsureSchema again as the
	// unprivileged wpmgr_app role — exactly what the media-encoder does when
	// WPMGR_DB_MIGRATION_DSN is unset. Since the role provably has no
	// database CREATE privilege, a nil error here can only mean the
	// readiness fast-path ran (the privileged CREATE SCHEMA statement would
	// have failed with 42501).
	if err := EnsureSchema(ctx, env.app, schema, "wpmgr_app"); err != nil {
		t.Fatalf("app-role EnsureSchema (steady-state boot) = %v, want nil (must use the readiness fast-path, not CREATE SCHEMA)", err)
	}
}

// TestEnsureSchema_FreshSchema_OwnerCreatesMigratesAndGrants is the existing
// happy path staying green: an owner connection on a never-created schema
// still creates it, runs the River migration, and grants the app role
// read/write access.
func TestEnsureSchema_FreshSchema_OwnerCreatesMigratesAndGrants(t *testing.T) {
	env := startSchemaTestPostgres(t)
	ctx := context.Background()
	const schema = "media_encoder"

	if err := EnsureSchema(ctx, env.owner, schema, "wpmgr_app"); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	var ready bool
	if err := env.owner.QueryRow(ctx, `SELECT to_regclass('"media_encoder"."river_job"') IS NOT NULL`).Scan(&ready); err != nil {
		t.Fatalf("check river_job exists: %v", err)
	}
	if !ready {
		t.Fatal("river_job table was not created/migrated in the dedicated schema")
	}

	// wpmgr_app must have been granted USAGE + table access by the
	// privileged path — proven independently of the fast-path by having the
	// app role actually read the freshly migrated table.
	if _, err := env.app.Exec(ctx, `SELECT count(*) FROM "media_encoder"."river_job"`); err != nil {
		t.Fatalf("wpmgr_app cannot read the migrated table after EnsureSchema: %v", err)
	}
}

// TestEnsureSchema_PermissionDenied_WrapsActionableError verifies that a
// genuine first-time-creation permission failure (the schema was never
// created, and the connection lacks database CREATE) surfaces as a wrapped,
// actionable error instead of a raw pg error, per the fix's requirement to
// never swallow the underlying error.
func TestEnsureSchema_PermissionDenied_WrapsActionableError(t *testing.T) {
	env := startSchemaTestPostgres(t)
	ctx := context.Background()
	const schema = "media_encoder"

	// No owner EnsureSchema call precedes this: the schema genuinely does
	// not exist, so the readiness fast-path returns false and the call falls
	// into the privileged CREATE SCHEMA path, which wpmgr_app cannot satisfy.
	err := EnsureSchema(ctx, env.app, schema, "wpmgr_app")
	if err == nil {
		t.Fatal("EnsureSchema with an unprivileged connection on a never-created schema = nil, want an error")
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error does not wrap the underlying *pgconn.PgError (raw pg error swallowed?): %v", err)
	}
	if pgErr.Code != pgErrInsufficientPrivilege {
		t.Fatalf("underlying pg error code = %q, want %q (insufficient_privilege)", pgErr.Code, pgErrInsufficientPrivilege)
	}
	if !strings.Contains(err.Error(), "WPMGR_DB_MIGRATION_DSN") {
		t.Fatalf("error message is not actionable, want mention of WPMGR_DB_MIGRATION_DSN: %v", err)
	}
}
