// mcp_scope_columns_prefill_migration_test.go: m136 and m137 must apply to a
// database whose mcp_grants and mcp_oauth_clients already hold rows, when the
// migrations are applied the way a managed-Postgres install applies them: by a
// role that owns the tables and is neither a superuser nor BYPASSRLS.
//
// Every other migration test in this package migrates as the container's
// bootstrap superuser. That role is not what production uses, and a migration
// that works as a superuser can fail as the owner. So these tests provision a
// NOSUPERUSER NOBYPASSRLS owner, hand it the database, run every migration as
// that role, seed rows as wpmgr_app through the transaction helpers that set the
// GUCs the tables' policies require, and then boot the real migrator
// (db.Pool.Migrate) as the owner, exactly as cmd/wpmgr does.
//
// m139 and m140 are the migrations that fill the two columns ahead of m136 and
// m137. "Without them" is simulated by recording them in schema_migrations
// without running them, which is what a binary that does not carry them sees.
package tests

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

const (
	scopePrefillM135 = "20260906000000_m135_mcp_cache_purge_capability"
	scopePrefillM139 = "20260906120000_m139_mcp_grant_oauth_scopes_prefill"
	scopePrefillM136 = "20260907000000_m136_mcp_grant_oauth_scopes"
	scopePrefillM140 = "20260907120000_m140_mcp_client_registered_scopes_prefill"
	scopePrefillM137 = "20260908000000_m137_mcp_client_registered_scopes"
)

// scopePrefillDB is one container with three connections: the bootstrap
// superuser (inspection only, it sees every row), the non-superuser owner that
// applies migrations, and wpmgr_app, the role the application writes as.
type scopePrefillDB struct {
	admin *db.Pool
	owner *db.Pool
	app   *db.Pool
}

// startPostgresAsOwner boots a container, provisions wpmgr_owner (LOGIN,
// CREATEROLE, NOSUPERUSER, NOBYPASSRLS) as the owner of the database, and
// applies every embedded migration as that role whose version is in apply
// (nil means all), recording each in schema_migrations exactly as applyOne
// does. It then gives wpmgr_app a login and returns all three pools.
func startPostgresAsOwner(t *testing.T, apply func(version string) bool) scopePrefillDB {
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
	// Cleanup before the error check: a partial start can return a container
	// alongside the error (see startPostgres in rls_integration_test.go).
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
	admin, err := db.Connect(ctx, adminDSN)
	if err != nil {
		setupFatalf(t, err, "postgres: connect as bootstrap superuser")
	}
	t.Cleanup(admin.Close)

	for _, stmt := range []string{
		`CREATE ROLE wpmgr_owner LOGIN PASSWORD 'owner' NOSUPERUSER NOBYPASSRLS CREATEROLE`,
		// PostgreSQL 16's public schema is owned by pg_database_owner, so owning
		// the database is owning the schema every migration creates into.
		`ALTER DATABASE wpmgr OWNER TO wpmgr_owner`,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			setupFatalf(t, err, "postgres: provision owner role ("+stmt+")")
		}
	}

	owner, err := db.Connect(ctx, strings.Replace(adminDSN, "wpmgr:wpmgr@", "wpmgr_owner:owner@", 1))
	if err != nil {
		setupFatalf(t, err, "postgres: connect as wpmgr_owner")
	}
	t.Cleanup(owner.Close)

	// The premise of every test in this file: the migrating role is subject to
	// row security. If this ever reads true, the tests below prove nothing.
	var super, bypass bool
	if err := owner.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&super, &bypass); err != nil {
		setupFatalf(t, err, "postgres: read owner role attributes")
	}
	if super || bypass {
		t.Fatalf("wpmgr_owner has rolsuper=%t rolbypassrls=%t; this file's premise is a role row security applies to", super, bypass)
	}

	scopePrefillApply(t, owner, apply)

	if _, err := owner.Exec(ctx, `ALTER ROLE wpmgr_app LOGIN PASSWORD 'app'`); err != nil {
		setupFatalf(t, err, "postgres: give wpmgr_app a login")
	}
	app, err := db.Connect(ctx, strings.Replace(adminDSN, "wpmgr:wpmgr@", "wpmgr_app:app@", 1))
	if err != nil {
		setupFatalf(t, err, "postgres: connect as wpmgr_app")
	}
	t.Cleanup(app.Close)

	return scopePrefillDB{admin: admin, owner: owner, app: app}
}

// scopePrefillApply walks the embedded migrations in the runner's lexical
// order and applies, as pool's role, every version apply accepts, one
// transaction per file with its schema_migrations row, as applyOne does.
func scopePrefillApply(t *testing.T, pool *db.Pool, apply func(version string) bool) {
	t.Helper()
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text        PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		t.Fatalf("ensure schema_migrations: %v", err)
	}

	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	var versions []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			versions = append(versions, strings.TrimSuffix(e.Name(), ".sql"))
		}
	}
	sort.Strings(versions)

	// Every version this file names must exist, or a rename would turn the
	// filters below into "apply everything" and the tests into no-ops.
	known := map[string]bool{}
	for _, v := range versions {
		known[v] = true
	}
	for _, v := range []string{scopePrefillM135, scopePrefillM139, scopePrefillM136, scopePrefillM140, scopePrefillM137} {
		if !known[v] {
			t.Fatalf("migration %q is not embedded (renamed or removed?); this file's premise is wrong", v)
		}
	}

	for _, version := range versions {
		if apply != nil && !apply(version) {
			continue
		}
		body, err := fs.ReadFile(migrations.FS, version+".sql")
		if err != nil {
			t.Fatalf("read migration %s: %v", version, err)
		}
		if err := pgx.BeginFunc(ctx, pool.Pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version)
			return err
		}); err != nil {
			t.Fatalf("apply migration %s as the owner: %v", version, err)
		}
	}
}

// upToM135 applies everything that sorts at or before m135: the schema every
// production database had when m136 first failed.
func upToM135(version string) bool { return version <= scopePrefillM135 }

// scopePrefillSeed inserts two grants and one OAuth client per tenant, for two
// tenants, as wpmgr_app through the helpers that set each table's GUC. With
// withScopes the rows name the scope columns, which only exist once m136/m137
// (or m139/m140) have applied. It returns the tenants and client ids.
func scopePrefillSeed(t *testing.T, d scopePrefillDB, withScopes bool, grantScopes, clientScopes []string) ([]uuid.UUID, []string) {
	t.Helper()
	ctx := context.Background()

	var tenants []uuid.UUID
	var clients []string
	for i := 0; i < 2; i++ {
		tenant := seedTenant(t, d.app, "scope-prefill-"+uuid.NewString()[:8])
		tenants = append(tenants, tenant)

		for j := 0; j < 2; j++ {
			err := d.app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
				q := `INSERT INTO mcp_grants (tenant_id, name, status, site_scope_mode, capabilities, expires_at)
				      VALUES ($1, $2, 'active', 'all', ARRAY['mcp.sites.read']::text[], now() + interval '30 days')`
				args := []any{tenant, "grant-" + uuid.NewString()[:8]}
				if withScopes {
					q = `INSERT INTO mcp_grants (tenant_id, name, status, site_scope_mode, capabilities, expires_at, oauth_scopes)
					     VALUES ($1, $2, 'active', 'all', ARRAY['mcp.sites.read']::text[], now() + interval '30 days', $3)`
					args = append(args, grantScopes)
				}
				_, err := tx.Exec(ctx, q, args...)
				return err
			})
			if err != nil {
				t.Fatalf("seed mcp_grants row for tenant %s: %v", tenant, err)
			}
		}

		clientID := "client-" + uuid.NewString()
		clients = append(clients, clientID)
		err := d.app.InMCPClientRegisterTx(ctx, func(tx pgx.Tx) error {
			q := `INSERT INTO mcp_oauth_clients (client_id, token_endpoint_auth_method, redirect_uris)
			      VALUES ($1, 'none', ARRAY['https://client.example/cb']::text[])`
			args := []any{clientID}
			if withScopes {
				q = `INSERT INTO mcp_oauth_clients (client_id, token_endpoint_auth_method, redirect_uris, registered_scopes)
				     VALUES ($1, 'none', ARRAY['https://client.example/cb']::text[], $2)`
				args = append(args, clientScopes)
			}
			_, err := tx.Exec(ctx, q, args...)
			return err
		})
		if err != nil {
			t.Fatalf("seed mcp_oauth_clients row: %v", err)
		}
	}

	// The seed is only a premise if the rows are there. Counted as the
	// superuser, which sees every row.
	if got := scopePrefillCount(t, d.admin, `SELECT count(*) FROM mcp_grants`); got != 4 {
		t.Fatalf("seeded %d mcp_grants rows, want 4; this test's premise is wrong", got)
	}
	if got := scopePrefillCount(t, d.admin, `SELECT count(*) FROM mcp_oauth_clients`); got != 2 {
		t.Fatalf("seeded %d mcp_oauth_clients rows, want 2; this test's premise is wrong", got)
	}
	return tenants, clients
}

func scopePrefillCount(t *testing.T, pool *db.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", q, err)
	}
	return n
}

func scopePrefillMark(t *testing.T, pool *db.Pool, versions ...string) {
	t.Helper()
	for _, v := range versions {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO schema_migrations (version) VALUES ($1)`, v); err != nil {
			t.Fatalf("mark %s applied: %v", v, err)
		}
	}
}

func scopePrefillUnmark(t *testing.T, pool *db.Pool, versions ...string) {
	t.Helper()
	for _, v := range versions {
		tag, err := pool.Exec(context.Background(),
			`DELETE FROM schema_migrations WHERE version = $1`, v)
		if err != nil {
			t.Fatalf("unmark %s: %v", v, err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("unmark %s removed %d rows, want 1", v, tag.RowsAffected())
		}
	}
}

// requireNotNullViolation asserts err is the boot failure production saw:
// the named migration, SQLSTATE 23502, on the named column and table.
func requireNotNullViolation(t *testing.T, err error, version, table, column string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Migrate succeeded; want 23502 on %s.%s from %s", table, column, version)
	}
	t.Logf("Migrate error (expected): %v", err)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("Migrate error is not a PostgreSQL error: %v", err)
	}
	// The message production logged, verbatim.
	wantMsg := `column "` + column + `" of relation "` + table + `" contains null values`
	if pgErr.Code != "23502" || pgErr.Message != wantMsg {
		t.Fatalf("got SQLSTATE %s %q, want 23502 %q", pgErr.Code, pgErr.Message, wantMsg)
	}
	if !strings.Contains(err.Error(), "apply migration "+version+":") {
		t.Fatalf("error does not name migration %s: %v", version, err)
	}
}

// requireScopeColumnEndState asserts the column is m136's/m137's: text[],
// NOT NULL, no default, the three CHECK constraints present and validated,
// and every row, counted as the superuser, carrying want.
func requireScopeColumnEndState(t *testing.T, admin *db.Pool, table, column string, constraints []string, want []string) {
	t.Helper()
	ctx := context.Background()

	var dataType, nullable string
	var def *string
	if err := admin.QueryRow(ctx,
		`SELECT data_type, is_nullable, column_default FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`,
		table, column).Scan(&dataType, &nullable, &def); err != nil {
		t.Fatalf("read %s.%s: %v", table, column, err)
	}
	if dataType != "ARRAY" || nullable != "NO" || def != nil {
		t.Errorf("%s.%s: data_type=%s is_nullable=%s default=%v; want ARRAY, NO, no default",
			table, column, dataType, nullable, def)
	}

	for _, name := range constraints {
		var validated bool
		err := admin.QueryRow(ctx,
			`SELECT convalidated FROM pg_constraint
			 WHERE conrelid = ('public.' || $1)::regclass AND conname = $2`,
			table, name).Scan(&validated)
		if err != nil {
			t.Errorf("constraint %s on %s: %v", name, table, err)
			continue
		}
		if !validated {
			t.Errorf("constraint %s on %s is not validated", name, table)
		}
	}

	total := scopePrefillCount(t, admin, `SELECT count(*) FROM `+table)
	matching := scopePrefillCount(t, admin,
		`SELECT count(*) FROM `+table+` WHERE `+column+` = $1::text[]`, want)
	if total == 0 || matching != total {
		t.Errorf("%s: %d of %d rows carry %s = %v; want every row, and at least one",
			table, matching, total, column, want)
	}
}

var (
	grantScopeConstraints = []string{
		"mcp_grants_oauth_scopes_shape_check",
		"mcp_grants_oauth_scopes_not_empty_check",
		"mcp_grants_oauth_scopes_vocabulary_check",
	}
	clientScopeConstraints = []string{
		"mcp_oauth_clients_registered_scopes_shape_check",
		"mcp_oauth_clients_registered_scopes_present_check",
		"mcp_oauth_clients_registered_scopes_vocabulary_check",
	}
	mcpRead = []string{"mcp:read"}
)

// The plant, then the fix, on one database in the state production was in:
// m135 applied, rows in both tables, m136 not applied.
//
//  1. m139 and m140 recorded but not run (the binary that failed in
//     production): the boot stops at m136 with 23502 on oauth_scopes.
//  2. m139 allowed to run, m140 still withheld: m139 and m136 apply, and the
//     boot stops at m137 with 23502 on registered_scopes, the next failure.
//  3. m140 allowed to run: it applies after m136 and before m137, m137
//     applies, and the boot completes.
func TestMCPScopeColumnMigrations_FailWithoutPrefillAndApplyWithIt(t *testing.T) {
	d := startPostgresAsOwner(t, upToM135)
	ctx := context.Background()
	scopePrefillSeed(t, d, false, nil, nil)

	scopePrefillMark(t, d.owner, scopePrefillM139, scopePrefillM140)
	err := d.owner.Migrate(ctx)
	requireNotNullViolation(t, err, scopePrefillM136, "mcp_grants", "oauth_scopes")

	scopePrefillUnmark(t, d.owner, scopePrefillM139)
	err = d.owner.Migrate(ctx)
	requireNotNullViolation(t, err, scopePrefillM137, "mcp_oauth_clients", "registered_scopes")
	requireScopeColumnEndState(t, d.admin, "mcp_grants", "oauth_scopes", grantScopeConstraints, mcpRead)

	scopePrefillUnmark(t, d.owner, scopePrefillM140)
	if err := d.owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate with m140 present: %v", err)
	}
	requireScopeColumnEndState(t, d.admin, "mcp_oauth_clients", "registered_scopes", clientScopeConstraints, mcpRead)
}

// The fix from production's state in one boot, then everything the fix must
// leave true: every existing row carries the intended value (read back through
// the repository layer as wpmgr_app, as well as counted as the superuser), the
// end state is exactly m136's and m137's, no default survived, and a second
// boot is a no-op.
func TestMCPScopeColumnMigrations_FillExistingRowsForEveryTenant(t *testing.T) {
	d := startPostgresAsOwner(t, upToM135)
	ctx := context.Background()
	tenants, clients := scopePrefillSeed(t, d, false, nil, nil)

	if err := d.owner.Migrate(ctx); err != nil {
		t.Fatalf("boot migration as the owner: %v", err)
	}

	requireScopeColumnEndState(t, d.admin, "mcp_grants", "oauth_scopes", grantScopeConstraints, mcpRead)
	requireScopeColumnEndState(t, d.admin, "mcp_oauth_clients", "registered_scopes", clientScopeConstraints, mcpRead)

	for _, v := range []string{scopePrefillM139, scopePrefillM136, scopePrefillM140, scopePrefillM137} {
		if n := scopePrefillCount(t, d.admin, `SELECT count(*) FROM schema_migrations WHERE version = $1`, v); n != 1 {
			t.Errorf("schema_migrations has %d rows for %s, want 1", n, v)
		}
	}

	// Through the product's read paths, as the application role.
	repo := mcp.NewRepo(d.app)
	for _, tenant := range tenants {
		grants, err := repo.ListGrants(ctx, domain.Principal{
			Type: domain.PrincipalAPIKey, TenantID: tenant, Role: "owner", Scope: "org",
		})
		if err != nil {
			t.Fatalf("ListGrants for tenant %s: %v", tenant, err)
		}
		if len(grants) != 2 {
			t.Fatalf("tenant %s: ListGrants returned %d grants, want 2", tenant, len(grants))
		}
		for _, g := range grants {
			if g.TenantID != tenant || !reflect.DeepEqual(g.OauthScopes, mcpRead) {
				t.Errorf("tenant %s grant %s: tenant_id=%s oauth_scopes=%v, want %v",
					tenant, g.ID, g.TenantID, g.OauthScopes, mcpRead)
			}
		}
	}
	for _, clientID := range clients {
		c, err := repo.LookupClient(ctx, clientID)
		if err != nil {
			t.Fatalf("LookupClient %s: %v", clientID, err)
		}
		if !reflect.DeepEqual(c.RegisteredScopes, mcpRead) {
			t.Errorf("client %s: registered_scopes=%v, want %v", clientID, c.RegisteredScopes, mcpRead)
		}
	}

	// No default survived m139/m140: an INSERT that omits the column is still
	// the loud 23502 m136 and m137 designed.
	err := d.app.InTenantTx(ctx, tenants[0], func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO mcp_grants (tenant_id, name, status, site_scope_mode, capabilities, expires_at)
			 VALUES ($1, 'no-scope', 'active', 'all', ARRAY['mcp.sites.read']::text[], now() + interval '30 days')`,
			tenants[0])
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23502" || pgErr.ColumnName != "oauth_scopes" {
		t.Errorf("mcp_grants INSERT omitting oauth_scopes: got %v, want 23502 on oauth_scopes", err)
	}
	err = d.app.InMCPClientRegisterTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO mcp_oauth_clients (client_id, token_endpoint_auth_method, redirect_uris)
			 VALUES ($1, 'none', ARRAY['https://client.example/cb']::text[])`,
			"client-"+uuid.NewString())
		return err
	})
	if !errors.As(err, &pgErr) || pgErr.Code != "23502" || pgErr.ColumnName != "registered_scopes" {
		t.Errorf("mcp_oauth_clients INSERT omitting registered_scopes: got %v, want 23502 on registered_scopes", err)
	}

	// A second boot applies nothing and changes nothing.
	before := scopePrefillCount(t, d.admin, `SELECT count(*) FROM schema_migrations`)
	if err := d.owner.Migrate(ctx); err != nil {
		t.Fatalf("second boot migration: %v", err)
	}
	if after := scopePrefillCount(t, d.admin, `SELECT count(*) FROM schema_migrations`); after != before {
		t.Errorf("second boot recorded %d new migrations, want 0", after-before)
	}
	requireScopeColumnEndState(t, d.admin, "mcp_grants", "oauth_scopes", grantScopeConstraints, mcpRead)
	requireScopeColumnEndState(t, d.admin, "mcp_oauth_clients", "registered_scopes", clientScopeConstraints, mcpRead)
}

// A database that already ran m136 and m137 (CI and dev databases built from
// main before m139/m140 existed) applies m139 and m140 on its next boot,
// because the runner applies any unapplied version however it sorts. There
// they must change nothing: the column is not re-added, no default appears,
// and a value that is not the fill value is left exactly as it was.
func TestMCPScopeColumnMigrations_PrefillIsANoOpWhereM136AndM137Ran(t *testing.T) {
	d := startPostgresAsOwner(t, func(v string) bool {
		return v != scopePrefillM139 && v != scopePrefillM140
	})
	ctx := context.Background()

	// A duplicated scope is legal under the shape and vocabulary checks and is
	// not what the fill would write, so any rewrite by m139/m140 shows.
	sentinel := []string{"mcp:read", "mcp:read"}
	scopePrefillSeed(t, d, true, sentinel, sentinel)

	attnum := func(table, column string) int {
		return scopePrefillCount(t, d.admin,
			`SELECT attnum FROM pg_attribute
			 WHERE attrelid = ('public.' || $1)::regclass AND attname = $2 AND NOT attisdropped`,
			table, column)
	}
	grantAttnum := attnum("mcp_grants", "oauth_scopes")
	clientAttnum := attnum("mcp_oauth_clients", "registered_scopes")

	if n := scopePrefillCount(t, d.admin,
		`SELECT count(*) FROM schema_migrations WHERE version IN ($1, $2)`,
		scopePrefillM139, scopePrefillM140); n != 0 {
		t.Fatalf("m139/m140 already recorded (%d rows); this test's premise is wrong", n)
	}

	if err := d.owner.Migrate(ctx); err != nil {
		t.Fatalf("boot migration applying m139/m140 after m136/m137: %v", err)
	}
	if n := scopePrefillCount(t, d.admin,
		`SELECT count(*) FROM schema_migrations WHERE version IN ($1, $2)`,
		scopePrefillM139, scopePrefillM140); n != 2 {
		t.Fatalf("m139/m140 recorded %d times after boot, want 2", n)
	}

	requireScopeColumnEndState(t, d.admin, "mcp_grants", "oauth_scopes", grantScopeConstraints, sentinel)
	requireScopeColumnEndState(t, d.admin, "mcp_oauth_clients", "registered_scopes", clientScopeConstraints, sentinel)
	if got := attnum("mcp_grants", "oauth_scopes"); got != grantAttnum {
		t.Errorf("mcp_grants.oauth_scopes attnum moved %d -> %d: the column was re-added", grantAttnum, got)
	}
	if got := attnum("mcp_oauth_clients", "registered_scopes"); got != clientAttnum {
		t.Errorf("mcp_oauth_clients.registered_scopes attnum moved %d -> %d: the column was re-added", clientAttnum, got)
	}
}
