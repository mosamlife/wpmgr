// m174 database proofs for approval tiers, Merge A, design section 11: the
// two backfills, the sites and mcp_grants guards, the approval backstop on
// both request tables, ai_mode_allows, mcp_grant_runs_by_setting and
// set_ability_change_class.
//
// The backfills are proved as the production migrator role (wpmgr_owner:
// NOSUPERUSER NOBYPASSRLS, the owner of every table) over rows that role
// wrote in two tenants before m174 applied, then read back as wpmgr_app.
// Every other statement under test runs as wpmgr_app through the db
// package's tenant transactions, using the generated sqlc method or the
// policy repository where one exists. A raw statement is used only for a
// write no shipped statement makes, which is what a guard exists to refuse.
// The bootstrap superuser seeds users and, with the triggers out of the way,
// shows a CHECK holding on its own.
//
// Each test names, in its doc comment, the mutations it is built to catch.
package tests

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilityrequest"
	"github.com/mosamlife/wpmgr/apps/api/internal/aipolicy"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

const m174gVersion = "20261009070000_m174_approval_tiers"

var errM174gRollback = errors.New("m174g: roll back")

// m174gWant records one case: code "" wants success, any other value wants
// exactly that SQLSTATE. It reports with Errorf so one run lists every case.
func m174gWant(t *testing.T, what string, err error, code string) {
	t.Helper()
	if code == "" {
		if err != nil {
			t.Errorf("%s: err = %v (code %q), want success", what, err, m174Code(err))
			return
		}
		t.Logf("ok   %s: accepted", what)
		return
	}
	if got := m174Code(err); got != code {
		t.Errorf("%s: err = %v (code %q), want SQLSTATE %s", what, err, got, code)
		return
	}
	t.Logf("ok   %s: refused %s", what, code)
}

// m174gExec runs one statement as wpmgr_app in a tenant transaction, with
// no user when user is nil and with app.user_id = *user otherwise.
func m174gExec(t *testing.T, pool *db.Pool, tenant uuid.UUID, user *uuid.UUID, stmt string, args ...any) error {
	t.Helper()
	ctx := context.Background()
	fn := func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "m174g statement")
		_, err := tx.Exec(ctx, stmt, args...)
		return err
	}
	if user == nil {
		return pool.InTenantTx(ctx, tenant, fn)
	}
	return pool.InTenantTxAsUser(ctx, tenant, *user, fn)
}

// m174gMode reads a site's mode through the shipped GetSiteAIMode as
// wpmgr_app, org-wide in its tenant.
func m174gMode(t *testing.T, pool *db.Pool, tenant, site uuid.UUID) sqlc.GetSiteAIModeRow {
	t.Helper()
	ctx := context.Background()
	var out sqlc.GetSiteAIModeRow
	if err := pool.RunTenantTx(ctx, acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m174g read mode)")
		var err error
		out, err = sqlc.New(tx).GetSiteAIMode(ctx, sqlc.GetSiteAIModeParams{TenantID: tenant, SiteID: site})
		return err
	}); err != nil {
		t.Fatalf("read the mode of site %s: %v", site, err)
	}
	return out
}

// m174gConnection reads a connection's switch through the shipped
// GetAIConnectionAuto as wpmgr_app, org-wide in the given tenant.
func m174gConnection(t *testing.T, pool *db.Pool, tenant, grant uuid.UUID) (sqlc.GetAIConnectionAutoRow, error) {
	t.Helper()
	ctx := context.Background()
	var out sqlc.GetAIConnectionAutoRow
	err := pool.RunTenantTx(ctx, acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m174g read connection)")
		var err error
		out, err = sqlc.New(tx).GetAIConnectionAuto(ctx, sqlc.GetAIConnectionAutoParams{TenantID: tenant, GrantID: grant})
		return err
	})
	return out, err
}

// m174gSwitch sets a connection's switch through the shipped
// SetAIConnectionAuto: 'never' with no person, 'site_setting' as *by.
func m174gSwitch(t *testing.T, pool *db.Pool, tenant, grant uuid.UUID, auto string, by *uuid.UUID) (sqlc.SetAIConnectionAutoRow, error) {
	t.Helper()
	p := acprOrgPrincipal(tenant)
	arg := sqlc.SetAIConnectionAutoParams{AiAuto: auto, TenantID: tenant, GrantID: grant}
	if by != nil {
		arg.SetBy = m174UUID(*by)
	}
	return m174gSwitchAs(t, pool, p, arg)
}

func m174gSwitchAs(t *testing.T, pool *db.Pool, p domain.Principal, arg sqlc.SetAIConnectionAutoParams) (sqlc.SetAIConnectionAutoRow, error) {
	t.Helper()
	ctx := context.Background()
	var out sqlc.SetAIConnectionAutoRow
	err := pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m174g switch)")
		var err error
		out, err = sqlc.New(tx).SetAIConnectionAuto(ctx, arg)
		return err
	})
	return out, err
}

// ---------------------------------------------------------------------------
// The backfills, as the production migrator role
// ---------------------------------------------------------------------------

type m174gTenantRows struct {
	tenant, enabler, gone           uuid.UUID
	onWithSetter, onSetterGone, off uuid.UUID
	personGrant, keyGrant           uuid.UUID
}

type m174gBackfill struct {
	admin, owner, app *db.Pool
	rows              [2]m174gTenantRows
}

// m174gAssertMigrator fails unless tx runs as wpmgr_owner with neither
// SUPERUSER nor BYPASSRLS: the role the backfill must work as.
func m174gAssertMigrator(t *testing.T, tx pgx.Tx, where string) {
	t.Helper()
	var role string
	var super, bypass bool
	if err := tx.QueryRow(context.Background(),
		`SELECT current_user, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&role, &super, &bypass); err != nil {
		t.Fatalf("%s: read the connection's role: %v", where, err)
	}
	if role != "wpmgr_owner" || super || bypass {
		t.Fatalf("%s: running as %q rolsuper=%t rolbypassrls=%t; want wpmgr_owner with neither", where, role, super, bypass)
	}
}

// m174gStartBeforeM174 starts a database migrated, as wpmgr_owner, up to but
// not including m174, and returns the bootstrap superuser, the migrator and
// a wpmgr_app pool.
func m174gStartBeforeM174(t *testing.T) (admin, owner, app *db.Pool) {
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
	admin, err = db.Connect(ctx, adminDSN)
	if err != nil {
		setupFatalf(t, err, "postgres: connect as bootstrap superuser")
	}
	t.Cleanup(admin.Close)
	for _, stmt := range []string{
		"CREATE ROLE wpmgr_owner LOGIN PASSWORD 'owner' NOSUPERUSER NOBYPASSRLS CREATEROLE",
		"ALTER DATABASE wpmgr OWNER TO wpmgr_owner",
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			setupFatalf(t, err, "postgres: provision owner role ("+stmt+")")
		}
	}
	owner, err = db.Connect(ctx, strings.Replace(adminDSN, "wpmgr:wpmgr@", "wpmgr_owner:owner@", 1))
	if err != nil {
		setupFatalf(t, err, "postgres: connect as wpmgr_owner")
	}
	t.Cleanup(owner.Close)

	applyMigrationsBefore(t, owner, m174gVersion)

	if _, err := owner.Exec(ctx, "ALTER ROLE wpmgr_app LOGIN PASSWORD 'app'"); err != nil {
		t.Fatalf("give wpmgr_app a login: %v", err)
	}
	app, err = db.Connect(ctx, strings.Replace(adminDSN, "wpmgr:wpmgr@", "wpmgr_app:app@", 1))
	if err != nil {
		setupFatalf(t, err, "postgres: connect as wpmgr_app")
	}
	t.Cleanup(app.Close)
	return admin, owner, app
}

// m174gSeedBeforeM174 writes, as the migrator role inside its tenant's
// transaction, the rows the backfills act on: a site with AI editing on
// whose enabling account exists, one whose enabling account is then
// deleted, one with AI editing off, a connection a person created and one
// minted with an API key. Row security records no writer, so these rows are
// the same stored state the application writes.
func m174gSeedBeforeM174(t *testing.T, w *m174gBackfill, label string) m174gTenantRows {
	t.Helper()
	ctx := context.Background()
	var r m174gTenantRows
	r.tenant = seedTenant(t, w.owner, "m174g-"+label+"-"+uuid.NewString()[:8])
	r.enabler = seedUserRow(t, w.admin, "m174g-"+label+"-"+uuid.NewString()[:8]+"@example.com")
	r.gone = seedUserRow(t, w.admin, "m174g-gone-"+label+"-"+uuid.NewString()[:8]+"@example.com")

	site := func() uuid.UUID {
		var id uuid.UUID
		if err := w.owner.InTenantTx(ctx, r.tenant, func(tx pgx.Tx) error {
			m174gAssertMigrator(t, tx, "seed site")
			return tx.QueryRow(ctx, `INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, 'm174g') RETURNING id`,
				r.tenant, "https://m174g-"+uuid.NewString()[:12]+".example.com").Scan(&id)
		}); err != nil {
			t.Fatalf("seed a site as the migrator role: %v", err)
		}
		return id
	}
	enable := func(site, by uuid.UUID) {
		if err := w.owner.InTenantTxAsUser(ctx, r.tenant, by, func(tx pgx.Tx) error {
			m174gAssertMigrator(t, tx, "turn AI editing on")
			_, err := sqlc.New(tx).MarkSiteContentEditingEnabled(ctx, sqlc.MarkSiteContentEditingEnabledParams{
				PrincipalUserID: 7, EnabledBy: by, SiteID: site, TenantID: r.tenant,
			})
			return err
		}); err != nil {
			t.Fatalf("turn AI editing on as the migrator role: %v", err)
		}
	}
	grant := func(createdBy *uuid.UUID) uuid.UUID {
		var id uuid.UUID
		if err := w.owner.InTenantTx(ctx, r.tenant, func(tx pgx.Tx) error {
			m174gAssertMigrator(t, tx, "seed connection")
			return tx.QueryRow(ctx, `
INSERT INTO mcp_grants (tenant_id, name, status, site_scope_mode, capabilities, oauth_scopes,
                        created_by_user_id, expires_at)
VALUES ($1, 'm174g laptop', 'active', 'all', '{mcp.sites.read}', '{mcp:read}', $2, now() + interval '30 days')
RETURNING id`, r.tenant, createdBy).Scan(&id)
		}); err != nil {
			t.Fatalf("seed a connection as the migrator role: %v", err)
		}
		return id
	}

	r.onWithSetter = site()
	r.onSetterGone = site()
	r.off = site()
	enable(r.onWithSetter, r.enabler)
	enable(r.onSetterGone, r.gone)
	creator := r.enabler
	r.personGrant = grant(&creator)
	r.keyGrant = grant(nil)
	return r
}

// m174gBackfillWorld seeds two tenants before m174, proves that an UPDATE
// run as the migrator with no tenant setting reaches none of those rows
// while FORCE ROW LEVEL SECURITY holds (the hazard the backfills must
// beat), deletes the accounts that turned AI editing on for one site per
// tenant, and applies m174 as the migrator through db.Pool.Migrate.
func m174gBackfillWorld(t *testing.T) *m174gBackfill {
	t.Helper()
	ctx := context.Background()
	w := &m174gBackfill{}
	w.admin, w.owner, w.app = m174gStartBeforeM174(t)
	w.rows[0] = m174gSeedBeforeM174(t, w, "a")
	w.rows[1] = m174gSeedBeforeM174(t, w, "b")
	for _, r := range w.rows {
		if _, err := w.admin.Exec(ctx, `DELETE FROM users WHERE id = $1`, r.gone); err != nil {
			t.Fatalf("delete the account that turned AI editing on: %v", err)
		}
	}

	var naiveSites, liftedSites, naiveGrants, liftedGrants int64
	err := pgx.BeginFunc(ctx, w.owner.Pool, func(tx pgx.Tx) error {
		m174gAssertMigrator(t, tx, "control")
		tag, err := tx.Exec(ctx, `UPDATE sites SET updated_at = updated_at WHERE content_editing_enabled_at IS NOT NULL`)
		if err != nil {
			return err
		}
		naiveSites = tag.RowsAffected()
		if tag, err = tx.Exec(ctx, `UPDATE mcp_grants SET name = name WHERE created_by_user_id IS NOT NULL`); err != nil {
			return err
		}
		naiveGrants = tag.RowsAffected()
		for _, stmt := range []string{
			`ALTER TABLE sites NO FORCE ROW LEVEL SECURITY`,
			`ALTER TABLE mcp_grants NO FORCE ROW LEVEL SECURITY`,
		} {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return err
			}
		}
		if tag, err = tx.Exec(ctx, `UPDATE sites SET updated_at = updated_at WHERE content_editing_enabled_at IS NOT NULL`); err != nil {
			return err
		}
		liftedSites = tag.RowsAffected()
		if tag, err = tx.Exec(ctx, `UPDATE mcp_grants SET name = name WHERE created_by_user_id IS NOT NULL`); err != nil {
			return err
		}
		liftedGrants = tag.RowsAffected()
		return errM174gRollback
	})
	if !errors.Is(err, errM174gRollback) {
		t.Fatalf("control transaction: %v", err)
	}
	t.Logf("control as wpmgr_owner before m174: sites %d under FORCE, %d with FORCE lifted; connections %d under FORCE, %d with FORCE lifted",
		naiveSites, liftedSites, naiveGrants, liftedGrants)
	// Two sites with AI editing on and one person-created connection per
	// tenant. If the naive count is not zero this harness does not reproduce
	// the hazard and nothing below would prove the backfill beats it.
	if naiveSites != 0 || liftedSites != 4 || naiveGrants != 0 || liftedGrants != 2 {
		t.Fatalf("control: sites %d/%d, connections %d/%d; want 0/4 and 0/2", naiveSites, liftedSites, naiveGrants, liftedGrants)
	}

	if err := w.owner.Migrate(ctx); err != nil {
		t.Fatalf("apply m174 as wpmgr_owner through db.Pool.Migrate: %v (code %s)", err, m174Code(err))
	}
	var applied bool
	if err := w.admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, m174gVersion).Scan(&applied); err != nil || !applied {
		t.Fatalf("m174 recorded as applied: %t, %v", applied, err)
	}
	return w
}

// m174gForceHeld reads relrowsecurity AND relforcerowsecurity for a table.
func m174gForceHeld(t *testing.T, pool *db.Pool, table string) bool {
	t.Helper()
	var held bool
	if err := pool.QueryRow(context.Background(),
		`SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid = $1::regclass`, table).Scan(&held); err != nil {
		t.Fatalf("read row security of %s: %v", table, err)
	}
	return held
}

// TestLaunchBackfill (section 11 test 5) proves m174's site backfill as the
// production migrator role, in two tenants, over rows that existed before
// it: AI editing on with its enabling person gives ai_drafts, launch_default,
// that person, version 1; on with that account deleted gives ask, migration,
// no setter, version 1; off keeps ask, unset, version 0. sites ends ENABLE
// and FORCE, each tenant reads none of the other's sites, the guard is in
// place after the backfill, and the launch notice claim finds the site.
//
// Mutations: delete the NO FORCE and row_security lines from the sites DO
// block (the sites stay unset, or Migrate fails); create sites_ai_mode_guard
// before the DO block (Migrate fails: the guard refuses launch_default);
// drop the "content_editing_enabled_by IS NOT NULL" filter from the first
// UPDATE (Migrate fails on the CHECKs).
func TestLaunchBackfill(t *testing.T) {
	ctx := context.Background()
	w := m174gBackfillWorld(t)

	if !m174gForceHeld(t, w.app, "public.sites") {
		t.Errorf("sites is not ENABLE and FORCE ROW LEVEL SECURITY after m174")
	}
	for i, r := range w.rows {
		on := m174gMode(t, w.app, r.tenant, r.onWithSetter)
		t.Logf("tenant %d on, setter kept: %s/%s set_by=%v v%d", i, on.AiMode, on.AiModeSource, on.AiModeSetBy, on.AiModeVersion)
		if on.AiMode != "ai_drafts" || on.AiModeSource != "launch_default" || !on.AiModeSetBy.Valid ||
			uuid.UUID(on.AiModeSetBy.Bytes) != r.enabler || on.AiModeVersion != 1 || !on.AiModeSetAt.Valid {
			t.Errorf("tenant %d: AI editing on with its person: %+v; want ai_drafts/launch_default by %s, v1", i, on, r.enabler)
		}
		gone := m174gMode(t, w.app, r.tenant, r.onSetterGone)
		t.Logf("tenant %d on, setter gone: %s/%s set_by=%v v%d", i, gone.AiMode, gone.AiModeSource, gone.AiModeSetBy, gone.AiModeVersion)
		if gone.AiMode != "ask" || gone.AiModeSource != "migration" || gone.AiModeSetBy.Valid || gone.AiModeVersion != 1 {
			t.Errorf("tenant %d: AI editing on, account deleted: %+v; want ask/migration, no setter, v1", i, gone)
		}
		off := m174gMode(t, w.app, r.tenant, r.off)
		t.Logf("tenant %d off: %s/%s set_by=%v v%d", i, off.AiMode, off.AiModeSource, off.AiModeSetBy, off.AiModeVersion)
		if off.AiMode != "ask" || off.AiModeSource != "unset" || off.AiModeSetBy.Valid || off.AiModeVersion != 0 || off.AiModeSetAt.Valid {
			t.Errorf("tenant %d: AI editing off: %+v; want ask/unset, no setter, v0", i, off)
		}
	}

	err := w.app.RunTenantTx(ctx, acprOrgPrincipal(w.rows[1].tenant), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m174g cross-tenant read)")
		_, err := sqlc.New(tx).GetSiteAIMode(ctx, sqlc.GetSiteAIModeParams{TenantID: w.rows[0].tenant, SiteID: w.rows[0].onWithSetter})
		return err
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("tenant B reading tenant A's site: err = %v, want no row", err)
	}

	// The guard exists once the backfill is done: launch_default is never
	// written at run time.
	a := w.rows[0]
	_, err = m174SetMode(t, w.app, m174UserPrincipal(a.tenant, a.enabler), sqlc.SetSiteAIModeParams{
		Mode: "ask", Source: "launch_default", TenantID: a.tenant, SiteID: a.off, ExpectedVersion: 0,
	})
	m174gWant(t, "a run-time write of launch_default after the backfill", err, "42501")

	if err := w.app.InTenantTx(ctx, a.tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m174g launch claim)")
		rows, err := sqlc.New(tx).ClaimSiteAILaunchNotices(ctx, a.tenant)
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0].ID != a.onWithSetter {
			t.Errorf("launch notice claim in tenant A: %+v; want only the backfilled site %s", rows, a.onWithSetter)
		}
		return nil
	}); err != nil {
		t.Fatalf("launch notice claim: %v", err)
	}
}

// TestGrantBackfillKeyMintedStaysNever (B4) proves m174's connection
// backfill as the production migrator role, in two tenants: a connection a
// person created ends site_setting with that person as its setter, one
// minted with an API key stays never with no setter. mcp_grants ends ENABLE
// and FORCE, each tenant reads none of the other's connections, and the
// guard is in place after the backfill.
//
// Mutations: delete the NO FORCE and row_security lines from the
// mcp_grants DO block (the person's connection stays never); drop
// "created_by_user_id IS NOT NULL" from the backfill (Migrate fails on
// mcp_grants_ai_auto_names_setter_check); create mcp_grants_ai_auto_guard
// before the DO block (Migrate fails: the guard refuses the backfill).
func TestGrantBackfillKeyMintedStaysNever(t *testing.T) {
	w := m174gBackfillWorld(t)

	if !m174gForceHeld(t, w.app, "public.mcp_grants") {
		t.Errorf("mcp_grants is not ENABLE and FORCE ROW LEVEL SECURITY after m174")
	}
	for i, r := range w.rows {
		person, err := m174gConnection(t, w.app, r.tenant, r.personGrant)
		if err != nil {
			t.Fatalf("tenant %d: read the person's connection: %v", i, err)
		}
		t.Logf("tenant %d person-created: %s set_by=%v", i, person.AiAuto, person.AiAutoSetBy)
		if person.AiAuto != "site_setting" || !person.AiAutoSetBy.Valid || uuid.UUID(person.AiAutoSetBy.Bytes) != r.enabler || !person.AiAutoSetAt.Valid {
			t.Errorf("tenant %d: person-created connection %+v; want site_setting set by its creator %s", i, person, r.enabler)
		}
		key, err := m174gConnection(t, w.app, r.tenant, r.keyGrant)
		if err != nil {
			t.Fatalf("tenant %d: read the key-minted connection: %v", i, err)
		}
		t.Logf("tenant %d key-minted: %s set_by=%v", i, key.AiAuto, key.AiAutoSetBy)
		if key.AiAuto != "never" || key.AiAutoSetBy.Valid || key.AiAutoSetAt.Valid {
			t.Errorf("tenant %d: key-minted connection %+v; want never with no setter", i, key)
		}
	}
	if _, err := m174gConnection(t, w.app, w.rows[0].tenant, w.rows[1].personGrant); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("tenant A reading tenant B's connection: err = %v, want no row", err)
	}

	a := w.rows[0]
	_, err := m174gSwitch(t, w.app, a.tenant, a.keyGrant, "site_setting", &a.enabler)
	m174gWant(t, "a key-minted connection allowed with no signed-in person, after the backfill", err, "42501")
}

// ---------------------------------------------------------------------------
// The guards, as wpmgr_app
// ---------------------------------------------------------------------------

type m174gWorld struct {
	pool               *db.Pool
	tenant, tenantB    uuid.UUID
	site, site2, siteB uuid.UUID
	alice, bob         uuid.UUID
	grant              uuid.UUID
	mode               sqlc.ApplySiteAIModeEnableDefaultRow
}

// m174gNewWorld is a migrated database with two tenants. In tenant A, alice
// created a connection (site_setting, by alice) and turned AI editing on
// for site, which therefore runs Auto for AI drafts by alice's enable
// default at version 1. site2 and tenant B's siteB carry the defaults.
func m174gNewWorld(t *testing.T) *m174gWorld {
	t.Helper()
	pool := startPostgres(t)
	w := &m174gWorld{pool: pool}
	w.tenant = seedTenant(t, pool, "m174g-a-"+uuid.NewString()[:8])
	w.tenantB = seedTenant(t, pool, "m174g-b-"+uuid.NewString()[:8])
	w.site = seedSite(t, pool, w.tenant, "")
	w.site2 = seedSite(t, pool, w.tenant, "")
	w.siteB = seedSite(t, pool, w.tenantB, "")
	w.alice = m174User(t, pool)
	w.bob = m174User(t, pool)
	g, err := m174CreateGrant(t, pool, m174UserPrincipal(w.tenant, w.alice), m174GrantParams(w.tenant, &w.alice, true))
	if err != nil {
		t.Fatalf("alice creates a connection: %v", err)
	}
	if g.AiAuto != "site_setting" {
		t.Fatalf("alice's connection starts on %s, want site_setting", g.AiAuto)
	}
	w.grant = g.ID
	w.mode = m174EnableAI(t, pool, w.tenant, w.site, w.alice)
	if w.mode.AiMode != "ai_drafts" || w.mode.AiModeSource != "enable_default" || w.mode.AiModeVersion != 1 || !w.mode.AiModeSetAt.Valid {
		t.Fatalf("enable default: %+v; want ai_drafts/enable_default v1", w.mode)
	}
	return w
}

func (w *m174gWorld) policyArg(id uuid.UUID, class string) sqlc.ApproveAbilityRequestByPolicyParams {
	return sqlc.ApproveAbilityRequestByPolicyParams{
		SiteMode: w.mode.AiMode, ModeSource: w.mode.AiModeSource, ModeVersion: w.mode.AiModeVersion,
		SetterUserID: w.alice, SetterSetAt: w.mode.AiModeSetAt.Time,
		BaseChangeClass: class, ChangeClass: class, DispatchWindowSeconds: 600,
		TenantID: w.tenant, ID: id, SiteID: w.site,
	}
}

func (w *m174gWorld) abilityRequest(t *testing.T, seed string) sqlc.AssistantAbilityRequest {
	t.Helper()
	return aarInsert(t, w.pool, acprSitePrincipal(w.tenant, w.site), aarParams(w.tenant, w.site, w.grant, seed))
}

func (w *m174gWorld) purgeRequest(t *testing.T, seed string) sqlc.AssistantCachePurgeRequest {
	t.Helper()
	return acprInsert(t, w.pool, acprSitePrincipal(w.tenant, w.site), acprParams(w.tenant, w.site, w.grant, seed))
}

// m174gRawPolicyApproval approves an ability request by the site's setting
// without the compare-and-set's predicates, so the backstop alone decides:
// $2 mode, $3 mode source, $4 version, $5 setter, $6 set time, $7 class.
const m174gRawPolicyApproval = `
UPDATE assistant_ability_requests
SET state = 'approved', approval_source = 'policy', approval_site_mode = $2,
    approval_mode_source = $3, approval_mode_version = $4, approval_setter_user_id = $5,
    approval_setter_set_at = $6, base_change_class = $7, change_class = $7,
    decided_at = now(), dispatch_deadline_at = now() + interval '10 minutes',
    policy_checked_at = now()
WHERE id = $1`

// TestMigrationSourceAboveAskRefused (B2) proves source 'migration' never
// rides above Ask or with a setter: the guard refuses it at run time, and
// with the triggers out of the way sites_ai_mode_unattributed_is_ask_check
// refuses it on its own (23514), while the backfill's own state (ask, no
// setter) is accepted.
//
// Mutation: drop sites_ai_mode_unattributed_is_ask_check (both trigger-less
// UPDATEs are then accepted).
func TestMigrationSourceAboveAskRefused(t *testing.T) {
	ctx := context.Background()
	w := m174gNewWorld(t)

	_, err := m174SetMode(t, w.pool, m174UserPrincipal(w.tenant, w.alice), sqlc.SetSiteAIModeParams{
		Mode: "ai_drafts", Source: "migration", SetBy: m174UUID(w.alice),
		TenantID: w.tenant, SiteID: w.site, ExpectedVersion: w.mode.AiModeVersion,
	})
	m174gWant(t, "a person writing source migration above ask, through SetSiteAIMode", err, "42501")

	admin := connectAdmin(t, w.pool)
	defer admin.Close()
	withoutTriggers := func(stmt string) error {
		return admin.InTenantTx(ctx, w.tenant, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, stmt, w.site)
			return err
		})
	}
	m174gWant(t, "source migration on ai_drafts with a setter, triggers off",
		withoutTriggers(`UPDATE sites SET ai_mode_source = 'migration' WHERE id = $1`), "23514")
	m174gWant(t, "source migration on ask with a setter, triggers off",
		withoutTriggers(`UPDATE sites SET ai_mode = 'ask', ai_mode_source = 'migration' WHERE id = $1`), "23514")
	m174gWant(t, "source migration on ask with no setter, triggers off (the backfill's own state)",
		withoutTriggers(`UPDATE sites SET ai_mode = 'ask', ai_mode_source = 'migration', ai_mode_set_by = NULL WHERE id = $1`), "")
}

// TestLaunchDefaultNotWritableAtRuntime (B2, T-B2c) proves no run-time write
// produces a source only the migration writes, with or without a signed-in
// person, through the shipped SetSiteAIMode, and the site is unchanged.
//
// Mutation: drop the guard's "source % is never written at run time" clause
// (the three Ask cases written by a person then pass the guard: unset and
// migration are accepted, launch_default fails 23514 instead of 42501).
func TestLaunchDefaultNotWritableAtRuntime(t *testing.T) {
	w := m174gNewWorld(t)
	person := m174UserPrincipal(w.tenant, w.alice)
	nobody := acprOrgPrincipal(w.tenant)
	cases := []struct {
		what         string
		p            domain.Principal
		mode, source string
		setBy        bool
	}{
		{"launch_default above ask, by the person it names", person, "ai_drafts", "launch_default", true},
		{"launch_default above ask, with no signed-in person", nobody, "ai_drafts", "launch_default", true},
		{"launch_default on ask, by a person", person, "ask", "launch_default", false},
		{"launch_default on ask, with no signed-in person", nobody, "ask", "launch_default", false},
		{"back to unset, by a person", person, "ask", "unset", false},
		{"migration on ask, by a person", person, "ask", "migration", false},
	}
	for _, c := range cases {
		arg := sqlc.SetSiteAIModeParams{Mode: c.mode, Source: c.source, TenantID: w.tenant, SiteID: w.site, ExpectedVersion: w.mode.AiModeVersion}
		if c.setBy {
			arg.SetBy = m174UUID(w.alice)
		}
		_, err := m174SetMode(t, w.pool, c.p, arg)
		m174gWant(t, c.what, err, "42501")
	}
	if got := m174gMode(t, w.pool, w.tenant, w.site); got.AiMode != "ai_drafts" || got.AiModeSource != "enable_default" || got.AiModeVersion != w.mode.AiModeVersion {
		t.Errorf("after the refused writes: %s/%s v%d; want ai_drafts/enable_default v%d", got.AiMode, got.AiModeSource, got.AiModeVersion, w.mode.AiModeVersion)
	}
}

// TestModeRaiseWithoutUserRefused (B2) proves a mode above Ask is written
// only by the signed-in person it names, and that with no signed-in person
// (including the all-zero user id) the only change is down to Ask, recorded
// as tightened, naming no one.
//
// Mutations: drop the guard's "recorded setter is the signed-in person"
// clause (bob naming alice is accepted); drop its "with no signed-in
// person" clause (the person-sourced lowering with no user is accepted);
// drop the all-zero exclusion (the zero-user lowering is accepted).
func TestModeRaiseWithoutUserRefused(t *testing.T) {
	ctx := context.Background()
	w := m174gNewWorld(t)

	r, err := m174SetMode(t, w.pool, acprOrgPrincipal(w.tenant), sqlc.SetSiteAIModeParams{
		Mode: "ask", Source: "tightened", TenantID: w.tenant, SiteID: w.site, ExpectedVersion: 1,
	})
	if err != nil || !r.Applied || r.AiModeVersion != 2 || r.AiModeSetBy.Valid {
		t.Fatalf("lowering to ask with no person: %+v, %v; want applied at v2 with no setter", r, err)
	}

	raise := sqlc.SetSiteAIModeParams{Mode: "ai_drafts", Source: "person", SetBy: m174UUID(w.alice), TenantID: w.tenant, SiteID: w.site, ExpectedVersion: 2}
	_, err = m174SetMode(t, w.pool, acprOrgPrincipal(w.tenant), raise)
	m174gWant(t, "a raise naming alice with no signed-in person", err, "42501")
	_, err = m174SetMode(t, w.pool, m174UserPrincipal(w.tenant, w.bob), raise)
	m174gWant(t, "a raise naming alice in bob's transaction", err, "42501")

	askAsPerson := sqlc.SetSiteAIModeParams{Mode: "ask", Source: "person", TenantID: w.tenant, SiteID: w.site, ExpectedVersion: 2}
	_, err = m174SetMode(t, w.pool, acprOrgPrincipal(w.tenant), askAsPerson)
	m174gWant(t, "an Ask recorded as a person's choice with no signed-in person", err, "42501")
	_, err = m174SetMode(t, w.pool, acprOrgPrincipal(w.tenant), sqlc.SetSiteAIModeParams{
		Mode: "ask", Source: "tightened", SetBy: m174UUID(w.alice), TenantID: w.tenant, SiteID: w.site, ExpectedVersion: 2,
	})
	m174gWant(t, "a tightening that names a setter, with no signed-in person", err, "42501")
	err = w.pool.InTenantTxAsUser(ctx, w.tenant, uuid.Nil, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTxAsUser (m174g all-zero user)")
		_, err := sqlc.New(tx).SetSiteAIMode(ctx, askAsPerson)
		return err
	})
	m174gWant(t, "an Ask recorded as a person's choice under the all-zero user id", err, "42501")

	r, err = m174SetMode(t, w.pool, m174UserPrincipal(w.tenant, w.alice), raise)
	if err != nil || !r.Applied || r.AiMode != "ai_drafts" || r.AiModeVersion != 3 || uuid.UUID(r.AiModeSetBy.Bytes) != w.alice {
		t.Errorf("alice raising, naming herself: %+v, %v; want applied ai_drafts v3 by alice", r, err)
	}
}

// TestPersonApprovalWithoutUserRefused (B2) proves, on both request tables,
// that a person's approval through the shipped statement is made only by
// that person signed in, and that an approval naming no one fails its CHECK.
//
// Mutation: drop clause 6 of ai_approval_backstop (the no-user and the
// other-user approvals are accepted on both tables).
func TestPersonApprovalWithoutUserRefused(t *testing.T) {
	ctx := context.Background()
	w := m174gNewWorld(t)

	r := w.abilityRequest(t, "pa-1")
	approve := func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "m174g person approval (ability)")
		_, err := sqlc.New(tx).ApproveAbilityRequest(ctx, sqlc.ApproveAbilityRequestParams{
			DecidedByUserID: w.alice, DispatchWindowSeconds: 600, TenantID: w.tenant, ID: r.ID, SiteID: w.site,
			PresentedDigest: r.PresentedDigest,
		})
		return err
	}
	m174gWant(t, "ability: alice's approval with no user in the transaction", w.pool.InTenantTx(ctx, w.tenant, approve), "42501")
	m174gWant(t, "ability: alice's approval in bob's transaction", w.pool.InTenantTxAsUser(ctx, w.tenant, w.bob, approve), "42501")
	m174gWant(t, "ability: a person's approval naming no one", m174gExec(t, w.pool, w.tenant, &w.alice, `
UPDATE assistant_ability_requests
SET state = 'approved', decided_at = now(), dispatch_deadline_at = now() + interval '10 minutes'
WHERE id = $1`, r.ID), "23514")
	m174gWant(t, "ability: alice's approval, signed in as alice", w.pool.InTenantTxAsUser(ctx, w.tenant, w.alice, approve), "")

	p := w.purgeRequest(t, "pa-2")
	approveP := func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "m174g person approval (cache clear)")
		_, err := sqlc.New(tx).ApproveAssistantCachePurgeRequest(ctx, sqlc.ApproveAssistantCachePurgeRequestParams{
			DecidedByUserID: w.alice, TenantID: w.tenant, ID: p.ID, SiteID: w.site, PresentedDigest: p.PresentedDigest,
		})
		return err
	}
	m174gWant(t, "cache clear: alice's approval with no user in the transaction", w.pool.InTenantTx(ctx, w.tenant, approveP), "42501")
	m174gWant(t, "cache clear: alice's approval in bob's transaction", w.pool.InTenantTxAsUser(ctx, w.tenant, w.bob, approveP), "42501")
	m174gWant(t, "cache clear: a person's approval naming no one", m174gExec(t, w.pool, w.tenant, &w.alice,
		`UPDATE assistant_cache_purge_requests SET state = 'approved_undispatched', decided_at = now() WHERE id = $1`, p.ID), "23514")
	m174gWant(t, "cache clear: alice's approval, signed in as alice", w.pool.InTenantTxAsUser(ctx, w.tenant, w.alice, approveP), "")
}

// m174gCopyAbilityRequest inserts a copy of request $1 with the given state
// ($2), approval source ($3), change class ($4) and check time ($5).
const m174gCopyAbilityRequest = `
INSERT INTO assistant_ability_requests (
    tenant_id, site_id, proposed_by_grant_id, entry_id, entry_sha256, ability_name, operator_permission,
    input_json, input_sha256, target_post_id, precheck_digest, preview_digest, base_fingerprint,
    site_label, site_host, grant_label, grant_via, setup_client, title_excerpt, editor, post_type,
    effect_copy, snapshot, card_copy_version, digest_nonce, presented_digest, expires_at,
    state, approval_source, change_class, policy_checked_at)
SELECT tenant_id, site_id, proposed_by_grant_id, entry_id, entry_sha256, ability_name, operator_permission,
    input_json, input_sha256, target_post_id, precheck_digest, preview_digest, base_fingerprint,
    site_label, site_host, grant_label, grant_via, setup_client, title_excerpt, editor, post_type,
    effect_copy, snapshot, card_copy_version, digest_nonce, presented_digest, expires_at,
    $2::text, $3::text, $4::text, $5::timestamptz
FROM assistant_ability_requests
WHERE id = $1`

// m174gInsertPurgeRequest inserts a cache clear request with the given state
// ($5), approval source ($6), change class ($7) and check time ($8).
const m174gInsertPurgeRequest = `
INSERT INTO assistant_cache_purge_requests (
    tenant_id, site_id, proposed_by_grant_id, scope, site_label, site_host, grant_label, grant_via,
    digest_nonce, presented_digest, expires_at, state, approval_source, change_class, policy_checked_at)
VALUES ($1, $2, $3, 'all', 'Shop', 'shop.example.com', 'Laptop', 'token',
    $4, $4, now() + interval '1 day', $5::text, $6::text, $7::text, $8::timestamptz)`

// TestRequestInsertMustBePending (B2) proves, on both request tables, that a
// request is created waiting, with a person's approval source and no class
// or check time: an INSERT that is already approved, or carries a decision,
// fails 55000, while the shipped insert (the setup rows) succeeds.
//
// Mutation: drop clause 1 of ai_approval_backstop (each INSERT reaches the
// table's own constraints instead: the codes change).
func TestRequestInsertMustBePending(t *testing.T) {
	w := m174gNewWorld(t)
	scope := acprSitePrincipal(w.tenant, w.site)
	ctx := context.Background()
	now := time.Now().UTC()

	r := w.abilityRequest(t, "ins-1")
	run := func(stmt string, args ...any) error {
		return w.pool.RunTenantTx(ctx, scope, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "RunTenantTx (m174g insert)")
			_, err := tx.Exec(ctx, stmt, args...)
			return err
		})
	}
	type ins struct {
		what          string
		state, source string
		class         *string
		checked       *time.Time
	}
	for _, c := range []ins{
		{"created approved", "approved", "person", nil, nil},
		{"created pending with a policy source", "pending", "policy", nil, nil},
		{"created pending with a change class", "pending", "person", acprStr("ai_draft"), nil},
		{"created pending with a check time", "pending", "person", nil, &now},
	} {
		m174gWant(t, "ability: "+c.what, run(m174gCopyAbilityRequest, r.ID, c.state, c.source, c.class, c.checked), "55000")
	}

	w.purgeRequest(t, "ins-2")
	for _, c := range []ins{
		{"created approved", "approved_undispatched", "person", nil, nil},
		{"created pending with a policy source", "pending", "policy", nil, nil},
		{"created pending with a change class", "pending", "person", acprStr("operational"), nil},
		{"created pending with a check time", "pending", "person", nil, &now},
	} {
		m174gWant(t, "cache clear: "+c.what, run(m174gInsertPurgeRequest, w.tenant, w.site, w.grant,
			acprHex("m174g-"+uuid.NewString()), c.state, c.source, c.class, c.checked), "55000")
	}
}

// TestByTargetStatusNeverEffective (D3) proves by_target_status is a stored
// class only: ai_mode_allows is false for it in every mode, a request whose
// effective class is by_target_status fails 23514 on both tables, the
// compare-and-set approves nothing with it, and the Ask write through the
// policy repository records it as the stored class beside an effective one.
//
// Mutations: allow 'by_target_status' in either change_class CHECK (that
// table's write is accepted); make ai_mode_allows true for it in any mode
// (the SQL loop goes red, and the compare-and-set approves).
func TestByTargetStatusNeverEffective(t *testing.T) {
	ctx := context.Background()
	w := m174gNewWorld(t)

	if err := w.pool.InTenantTx(ctx, w.tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m174g allows)")
		for _, m := range aipolicy.Modes() {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT ai_mode_allows($1, 'by_target_status')`, string(m)).Scan(&ok); err != nil {
				return err
			}
			if ok {
				t.Errorf("ai_mode_allows(%s, by_target_status) = true, want false", m)
			}
		}
		var control bool
		if err := tx.QueryRow(ctx, `SELECT ai_mode_allows('full', 'live')`).Scan(&control); err != nil {
			return err
		}
		if !control {
			t.Errorf("positive control ai_mode_allows(full, live) = false; the function answers nothing")
		}
		return nil
	}); err != nil {
		t.Fatalf("ai_mode_allows: %v", err)
	}

	r := w.abilityRequest(t, "bts-1")
	m174gWant(t, "ability: an effective class of by_target_status", m174gExec(t, w.pool, w.tenant, nil, `
UPDATE assistant_ability_requests
SET base_change_class = 'by_target_status', change_class = 'by_target_status',
    ask_reason = 'not_checked', policy_checked_at = now()
WHERE id = $1`, r.ID), "23514")
	p := w.purgeRequest(t, "bts-2")
	m174gWant(t, "cache clear: an effective class of by_target_status", m174gExec(t, w.pool, w.tenant, nil, `
UPDATE assistant_cache_purge_requests
SET base_change_class = 'by_target_status', change_class = 'by_target_status',
    ask_reason = 'not_checked', policy_checked_at = now()
WHERE id = $1`, p.ID), "23514")

	if _, err := m174ApproveByPolicy(t, w.pool, w.policyArg(r.ID, "by_target_status")); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("compare-and-set with class by_target_status: err = %v (code %s), want no row", err, m174Code(err))
	}

	repo := abilityrequest.NewPolicyRepo(w.pool, audit.NewRecorder(w.pool, domain.SystemClock{}))
	m174gWant(t, "the Ask write of a stored by_target_status beside an effective class", repo.Approving(ctx, w.tenant, func(tx abilityrequest.PolicyTx) error {
		return tx.RecordAsk(ctx, abilityrequest.PolicyAsk{
			TenantID: w.tenant, RequestID: r.ID,
			BaseClass: aipolicy.StoredByTargetStatus, Class: aipolicy.ClassUnpublished, Reason: aipolicy.AskKindNotInMode,
		})
	}), "")
}

// TestM174AutomaticApprovalWithAnyUserRefused (D4, beside
// TestAutomaticApprovalWithUserSetRefused) proves, on both request tables, that an approval by the site's setting is refused in a
// transaction that carries any user id, the all-zero one included, and that
// the same approval with no user is accepted.
//
// Mutation: drop the backstop's "runs with no user in the transaction"
// clause (the approvals with a user are accepted).
func TestM174AutomaticApprovalWithAnyUserRefused(t *testing.T) {
	ctx := context.Background()
	w := m174gNewWorld(t)

	r := w.abilityRequest(t, "au-1")
	arg := w.policyArg(r.ID, "ai_draft")
	as := func(user uuid.UUID) error {
		return w.pool.InTenantTxAsUser(ctx, w.tenant, user, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "InTenantTxAsUser (m174g policy approval with a user)")
			_, err := sqlc.New(tx).ApproveAbilityRequestByPolicy(ctx, arg)
			return err
		})
	}
	m174gWant(t, "ability: approval by the setting with its setter signed in", as(w.alice), "42501")
	m174gWant(t, "ability: approval by the setting under the all-zero user id", as(uuid.Nil), "42501")
	got, err := m174ApproveByPolicy(t, w.pool, arg)
	m174gWant(t, "ability: the same approval with no user", err, "")
	if err == nil && (got.State != "approved" || got.ApprovalSource != "policy" || got.DecidedByUserID.Valid) {
		t.Errorf("approved row: %s/%s decider %v; want approved/policy with no decider", got.State, got.ApprovalSource, got.DecidedByUserID)
	}

	p := w.purgeRequest(t, "au-2")
	const purgePolicy = `
UPDATE assistant_cache_purge_requests
SET state = 'approved_undispatched', approval_source = 'policy', approval_site_mode = 'ai_drafts',
    approval_mode_source = $2, approval_mode_version = $3, approval_setter_user_id = $4,
    approval_setter_set_at = $5, base_change_class = 'operational', change_class = 'operational',
    decided_at = now(), policy_checked_at = now()
WHERE id = $1`
	args := []any{p.ID, w.mode.AiModeSource, w.mode.AiModeVersion, w.alice, w.mode.AiModeSetAt.Time}
	m174gWant(t, "cache clear: approval by the setting with its setter signed in",
		m174gExec(t, w.pool, w.tenant, &w.alice, purgePolicy, args...), "42501")
	m174gWant(t, "cache clear: the same approval with no user",
		m174gExec(t, w.pool, w.tenant, nil, purgePolicy, args...), "")
}

// TestM174BackstopRefusesAPolicyApprovalTheSettingDoesNotHold proves the
// backstop's own check of an approval by a site's setting, with no
// compare-and-set in front of it: a recorded mode, version, setter or set
// time the site does not carry, a class the mode does not allow, and a
// connection set to never each fail 42501; the setting as it is succeeds.
//
// Mutations: drop any one of "s.ai_mode = NEW.approval_site_mode",
// "s.ai_mode_version = NEW.approval_mode_version", "s.ai_mode_set_by =
// NEW.approval_setter_user_id", "s.ai_mode_set_at IS NOT DISTINCT FROM
// NEW.approval_setter_set_at", "ai_mode_allows(...)" or
// "mcp_grant_runs_by_setting(...)" from ai_approval_backstop (that case is
// accepted). The mode source is TestM174PolicyApprovalRecordsModeSource's.
func TestM174BackstopRefusesAPolicyApprovalTheSettingDoesNotHold(t *testing.T) {
	w := m174gNewWorld(t)
	r := w.abilityRequest(t, "bs-1")
	mode, src, ver, setAt := w.mode.AiMode, w.mode.AiModeSource, w.mode.AiModeVersion, w.mode.AiModeSetAt.Time
	try := func(m string, v int64, setter uuid.UUID, at time.Time, class string) error {
		return m174gExec(t, w.pool, w.tenant, nil, m174gRawPolicyApproval, r.ID, m, src, v, setter, at, class)
	}

	m174gWant(t, "a mode the site is not on", try("full", ver, w.alice, setAt, "ai_draft"), "42501")
	m174gWant(t, "a version the site is not on", try(mode, ver+1, w.alice, setAt, "ai_draft"), "42501")
	m174gWant(t, "a setter who is not the site's", try(mode, ver, w.bob, setAt, "ai_draft"), "42501")
	m174gWant(t, "a set time that is not the site's", try(mode, ver, w.alice, setAt.Add(time.Second), "ai_draft"), "42501")
	m174gWant(t, "a class the mode does not allow", try(mode, ver, w.alice, setAt, "live"), "42501")

	if _, err := m174gSwitch(t, w.pool, w.tenant, w.grant, "never", nil); err != nil {
		t.Fatalf("set the connection to never: %v", err)
	}
	m174gWant(t, "a connection set to never", try(mode, ver, w.alice, setAt, "ai_draft"), "42501")
	arg := sqlc.SetAIConnectionAutoParams{AiAuto: "site_setting", SetBy: m174UUID(w.alice), TenantID: w.tenant, GrantID: w.grant}
	if _, err := m174gSwitchAs(t, w.pool, m174UserPrincipal(w.tenant, w.alice), arg); err != nil {
		t.Fatalf("alice allows the connection again: %v", err)
	}

	m174gWant(t, "the setting as recorded, current, allowing the change, connection on",
		try(mode, ver, w.alice, setAt, "ai_draft"), "")
}

// TestM174DecideApprovalSatisfiesTheBackstop proves the approving statement
// the policy repository runs for Decide approves a request the site's
// setting covers, and records the mode source the site carries: the
// approval Decide makes is one the backstop and the policy shape CHECK
// accept.
//
// Mutations: drop approval_mode_source from the repository's approving
// statement, ApproveAbilityRequestByPolicy (the backstop refuses it, 42501);
// leave ModeSource out of the approval (the compare-and-set touches no row).
func TestM174DecideApprovalSatisfiesTheBackstop(t *testing.T) {
	ctx := context.Background()
	w := m174gNewWorld(t)
	r := w.abilityRequest(t, "dec-1")

	repo := abilityrequest.NewPolicyRepo(w.pool, audit.NewRecorder(w.pool, domain.SystemClock{}))
	var got sqlc.AssistantAbilityRequest
	err := repo.Approving(ctx, w.tenant, func(tx abilityrequest.PolicyTx) error {
		var err error
		got, err = tx.ApproveByPolicy(ctx, abilityrequest.PolicyApproval{
			TenantID: w.tenant, RequestID: r.ID, SiteID: w.site,
			SiteMode: aipolicy.ModeAIDrafts, ModeSource: w.mode.AiModeSource, ModeVersion: w.mode.AiModeVersion,
			SetterUserID: w.alice, SetterSetAt: w.mode.AiModeSetAt.Time,
			BaseClass: aipolicy.ClassAIDraft, Class: aipolicy.ClassAIDraft, DispatchWindowSeconds: 600,
		})
		return err
	})
	if err != nil {
		t.Fatalf("the approving statement Decide runs: err = %v (code %s), want the request approved", err, m174Code(err))
	}
	if got.State != "approved" || got.ApprovalSource != "policy" {
		t.Fatalf("approved row: %s/%s; want approved/policy", got.State, got.ApprovalSource)
	}
	var source *string
	if err := w.pool.InTenantTx(ctx, w.tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m174g read approval)")
		return tx.QueryRow(ctx, `SELECT approval_mode_source FROM assistant_ability_requests WHERE id = $1`, r.ID).Scan(&source)
	}); err != nil {
		t.Fatalf("read the approval: %v", err)
	}
	if source == nil || *source != w.mode.AiModeSource {
		t.Errorf("approval_mode_source = %v, want %q", source, w.mode.AiModeSource)
	}
}

// TestGrantAutoRaiseNeedsUser (N2, and D1's database half) proves a
// connection runs by the site's setting only when the signed-in person
// recorded as its setter says so: allowing it, or moving its setter, with
// no person or naming someone else fails 42501; another person re-saving it
// as themself becomes its setter; stopping it needs no person.
//
// Mutation: drop "NEW.ai_auto_set_by IS DISTINCT FROM OLD.ai_auto_set_by"
// from mcp_grants_ai_auto_guard (both setter moves with no person are then
// accepted).
func TestGrantAutoRaiseNeedsUser(t *testing.T) {
	w := m174gNewWorld(t)
	key, err := m174CreateGrant(t, w.pool, acprOrgPrincipal(w.tenant), m174GrantParams(w.tenant, nil, false))
	if err != nil || key.AiAuto != "never" {
		t.Fatalf("key-minted connection: %+v, %v; want never", key, err)
	}
	set := func(p domain.Principal, auto string, by *uuid.UUID) (sqlc.SetAIConnectionAutoRow, error) {
		arg := sqlc.SetAIConnectionAutoParams{AiAuto: auto, TenantID: w.tenant, GrantID: key.ID}
		if by != nil {
			arg.SetBy = m174UUID(*by)
		}
		return m174gSwitchAs(t, w.pool, p, arg)
	}

	_, err = set(acprOrgPrincipal(w.tenant), "site_setting", &w.alice)
	m174gWant(t, "allowed with no signed-in person", err, "42501")
	_, err = set(m174UserPrincipal(w.tenant, w.bob), "site_setting", &w.alice)
	m174gWant(t, "allowed naming alice in bob's transaction", err, "42501")
	_, err = set(m174UserPrincipal(w.tenant, w.alice), "site_setting", &w.alice)
	m174gWant(t, "allowed by alice, naming herself", err, "")

	_, err = set(acprOrgPrincipal(w.tenant), "site_setting", &w.bob)
	m174gWant(t, "its setter moved to bob with no signed-in person", err, "42501")
	m174gWant(t, "its setter moved by a raw write with no signed-in person",
		m174gExec(t, w.pool, w.tenant, nil, `UPDATE mcp_grants SET ai_auto_set_by = $2 WHERE id = $1`, key.ID, w.bob), "42501")

	resaved, err := set(m174UserPrincipal(w.tenant, w.bob), "site_setting", &w.bob)
	m174gWant(t, "bob re-saves it as himself", err, "")
	if err == nil && (!resaved.AiAutoSetBy.Valid || uuid.UUID(resaved.AiAutoSetBy.Bytes) != w.bob) {
		t.Errorf("after bob's re-save the setter is %v, want %s", resaved.AiAutoSetBy, w.bob)
	}
	off, err := set(acprOrgPrincipal(w.tenant), "never", nil)
	m174gWant(t, "stopped with no signed-in person", err, "")
	if err == nil && (off.AiAuto != "never" || off.AiAutoSetBy.Valid) {
		t.Errorf("after stopping: %+v; want never with no setter", off)
	}
}

// TestClassColumnsSetOnceFromNull (N-d) proves, on both request tables, that
// the class, the reason a request asked and the check time are written
// once, together, while it waits: the Ask write succeeds, and any second
// write of any of them, or an approval after it, fails 55000.
//
// Mutation: drop "OLD.policy_checked_at IS NOT NULL OR ... OLD.ask_reason IS
// NOT NULL" from clause 3 of ai_approval_backstop (the second writes are
// accepted).
func TestClassColumnsSetOnceFromNull(t *testing.T) {
	ctx := context.Background()
	w := m174gNewWorld(t)
	repo := abilityrequest.NewPolicyRepo(w.pool, audit.NewRecorder(w.pool, domain.SystemClock{}))
	ask := func(id uuid.UUID, reason aipolicy.AskReason) error {
		return repo.Approving(ctx, w.tenant, func(tx abilityrequest.PolicyTx) error {
			return tx.RecordAsk(ctx, abilityrequest.PolicyAsk{
				TenantID: w.tenant, RequestID: id, BaseClass: aipolicy.ClassAIDraft, Class: aipolicy.ClassAIDraft, Reason: reason,
			})
		})
	}

	r := w.abilityRequest(t, "once-1")
	m174gWant(t, "ability: the Ask write on a waiting request", ask(r.ID, aipolicy.AskNotChecked), "")
	if err := ask(r.ID, aipolicy.AskSiteModeAsk); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("ability: a second Ask write through the repository: err = %v, want no row", err)
	}
	for _, c := range []struct{ what, stmt string }{
		{"ask_reason", `UPDATE assistant_ability_requests SET ask_reason = 'site_mode_ask' WHERE id = $1`},
		{"change_class", `UPDATE assistant_ability_requests SET change_class = 'live' WHERE id = $1`},
		{"base_change_class", `UPDATE assistant_ability_requests SET base_change_class = 'live' WHERE id = $1`},
		{"policy_checked_at", `UPDATE assistant_ability_requests SET policy_checked_at = now() + interval '1 second' WHERE id = $1`},
	} {
		m174gWant(t, "ability: a second write of "+c.what, m174gExec(t, w.pool, w.tenant, nil, c.stmt, r.ID), "55000")
	}
	m174gWant(t, "ability: an approval by the setting after the request asked",
		m174gExec(t, w.pool, w.tenant, nil, m174gRawPolicyApproval, r.ID, w.mode.AiMode, w.mode.AiModeSource,
			w.mode.AiModeVersion, w.alice, w.mode.AiModeSetAt.Time, "ai_draft"), "55000")

	r2 := w.abilityRequest(t, "once-2")
	m174gWant(t, "ability: a class and reason written without the check time", m174gExec(t, w.pool, w.tenant, nil,
		`UPDATE assistant_ability_requests SET ask_reason = 'site_mode_ask', change_class = 'ai_draft' WHERE id = $1`, r2.ID), "55000")

	p := w.purgeRequest(t, "once-3")
	m174gWant(t, "cache clear: the Ask write on a waiting request", m174gExec(t, w.pool, w.tenant, nil, `
UPDATE assistant_cache_purge_requests
SET ask_reason = 'site_mode_ask', base_change_class = 'operational', change_class = 'operational',
    policy_checked_at = now()
WHERE id = $1`, p.ID), "")
	m174gWant(t, "cache clear: a second write of ask_reason", m174gExec(t, w.pool, w.tenant, nil,
		`UPDATE assistant_cache_purge_requests SET ask_reason = 'not_checked' WHERE id = $1`, p.ID), "55000")
}

// TestSitesGuardVersionOnlyWithChange (N-f) proves ai_mode_version moves by
// exactly one and only with a change of mode, setter or source; the set time
// and step-up move only with such a change; a setter moving from NULL to a
// value is a change; and saving the stored setting when its setter is NULL
// is no change.
//
// Mutations: drop the guard's no-change block (the three no-change writes
// are accepted); accept any version above the old one (the +2 raise is
// accepted); compare the setter with <> instead of IS DISTINCT FROM (saving
// the stored setting with a NULL setter is refused).
func TestSitesGuardVersionOnlyWithChange(t *testing.T) {
	w := m174gNewWorld(t)
	tb, sb, alice := w.tenantB, w.siteB, &w.alice

	steps := []struct {
		what string
		user *uuid.UUID
		stmt string
		with bool
		want string
	}{
		{"a version bump with no change", nil, `UPDATE sites SET ai_mode_version = ai_mode_version + 1 WHERE id = $1`, false, "55000"},
		{"a new set time with no change", nil, `UPDATE sites SET ai_mode_set_at = now() WHERE id = $1`, false, "55000"},
		{"a new step-up with no change", nil, `UPDATE sites SET ai_mode_step_up = 'password' WHERE id = $1`, false, "55000"},
		{"a raise that does not move the version", alice, `UPDATE sites SET ai_mode = 'ai_drafts', ai_mode_source = 'person', ai_mode_set_by = $2, ai_mode_set_at = now() WHERE id = $1`, true, "55000"},
		{"a raise that moves the version by two", alice, `UPDATE sites SET ai_mode = 'ai_drafts', ai_mode_source = 'person', ai_mode_set_by = $2, ai_mode_set_at = now(), ai_mode_version = ai_mode_version + 2 WHERE id = $1`, true, "55000"},
		{"a person records their own Ask, version +1", alice, `UPDATE sites SET ai_mode_source = 'person', ai_mode_set_at = now(), ai_mode_version = ai_mode_version + 1 WHERE id = $1`, false, ""},
	}
	run := func(user *uuid.UUID, stmt string, with bool) error {
		if with {
			return m174gExec(t, w.pool, tb, user, stmt, sb, w.alice)
		}
		return m174gExec(t, w.pool, tb, user, stmt, sb)
	}
	for _, s := range steps {
		m174gWant(t, s.what, run(s.user, s.stmt, s.with), s.want)
	}

	// Saving the stored setting (ask, person, no setter) is no change.
	r, err := m174SetMode(t, w.pool, m174UserPrincipal(tb, w.alice), sqlc.SetSiteAIModeParams{
		Mode: "ask", Source: "person", TenantID: tb, SiteID: sb, ExpectedVersion: 1,
	})
	m174gWant(t, "saving the stored setting whose setter is NULL", err, "")
	if err == nil && (!r.Applied || r.AiModeVersion != 1) {
		t.Errorf("saving the stored setting: %+v; want applied with the version still 1", r)
	}
	m174gWant(t, "a setter from NULL to a value with no version move",
		run(alice, `UPDATE sites SET ai_mode_set_by = $2 WHERE id = $1`, true), "55000")
	m174gWant(t, "a setter from NULL to a value with the version +1",
		run(alice, `UPDATE sites SET ai_mode_set_by = $2, ai_mode_set_at = now(), ai_mode_version = ai_mode_version + 1 WHERE id = $1`, true), "")
	m174gWant(t, "a raise with the version +1",
		run(alice, `UPDATE sites SET ai_mode = 'ai_drafts', ai_mode_set_at = now(), ai_mode_version = ai_mode_version + 1 WHERE id = $1`, false), "")

	got := m174gMode(t, w.pool, tb, sb)
	if got.AiMode != "ai_drafts" || got.AiModeSource != "person" || got.AiModeVersion != 3 ||
		!got.AiModeSetBy.Valid || uuid.UUID(got.AiModeSetBy.Bytes) != w.alice {
		t.Errorf("end state: %s/%s v%d by %v; want ai_drafts/person v3 by alice", got.AiMode, got.AiModeSource, got.AiModeVersion, got.AiModeSetBy)
	}
}

// TestM174ReEnableGuardKeepsChosenMode (N3, the database half of
// TestReEnableKeepsChosenMode) proves turning AI editing on again never
// resets a mode a person chose: Full auto and Ask are both kept by the
// shipped enable statements, and a raw enable default over a person's
// choice is refused by the guard.
//
// Mutations: drop "ai_mode_source = 'unset'" from
// ApplySiteAIModeEnableDefault (the re-enable then fails 55000 instead of
// returning no row); drop the guard's enable_default clause (the raw write
// is accepted).
func TestM174ReEnableGuardKeepsChosenMode(t *testing.T) {
	ctx := context.Background()
	w := m174gNewWorld(t)
	stepUp := "password"
	full, err := m174SetMode(t, w.pool, m174UserPrincipal(w.tenant, w.alice), sqlc.SetSiteAIModeParams{
		Mode: "full", Source: "person", SetBy: m174UUID(w.alice), StepUp: &stepUp,
		TenantID: w.tenant, SiteID: w.site, ExpectedVersion: 1,
	})
	if err != nil || !full.Applied || full.AiMode != "full" || full.AiModeVersion != 2 {
		t.Fatalf("alice chooses Full auto: %+v, %v; want applied full v2", full, err)
	}

	reEnable := func() error {
		return w.pool.RunTenantTx(ctx, m174UserPrincipal(w.tenant, w.alice), func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "RunTenantTx (m174g enable again)")
			q := sqlc.New(tx)
			if _, err := q.MarkSiteContentEditingEnabled(ctx, sqlc.MarkSiteContentEditingEnabledParams{
				PrincipalUserID: 7, EnabledBy: w.alice, SiteID: w.site, TenantID: w.tenant,
			}); err != nil {
				return err
			}
			_, err := q.ApplySiteAIModeEnableDefault(ctx, sqlc.ApplySiteAIModeEnableDefaultParams{
				EnabledBy: w.alice, TenantID: w.tenant, SiteID: w.site,
			})
			return err
		})
	}
	if err := reEnable(); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("turning AI editing on again over Full auto: err = %v (code %s), want no row", err, m174Code(err))
	}
	if got := m174gMode(t, w.pool, w.tenant, w.site); got.AiMode != "full" || got.AiModeVersion != 2 {
		t.Errorf("after enabling again: %s v%d, want full v2", got.AiMode, got.AiModeVersion)
	}
	m174gWant(t, "the enable default written over a person's Full auto", m174gExec(t, w.pool, w.tenant, &w.alice, `
UPDATE sites
SET ai_mode = 'ai_drafts', ai_mode_source = 'enable_default', ai_mode_step_up = NULL,
    ai_mode_set_at = now(), ai_mode_version = ai_mode_version + 1
WHERE id = $1`, w.site), "55000")

	lowered, err := m174SetMode(t, w.pool, m174UserPrincipal(w.tenant, w.alice), sqlc.SetSiteAIModeParams{
		Mode: "ask", Source: "person", SetBy: m174UUID(w.alice), TenantID: w.tenant, SiteID: w.site, ExpectedVersion: 2,
	})
	if err != nil || !lowered.Applied || lowered.AiModeVersion != 3 {
		t.Fatalf("alice chooses Ask: %+v, %v; want applied v3", lowered, err)
	}
	if err := reEnable(); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("turning AI editing on again over Ask: err = %v (code %s), want no row", err, m174Code(err))
	}
	if got := m174gMode(t, w.pool, w.tenant, w.site); got.AiMode != "ask" || got.AiModeSource != "person" || got.AiModeVersion != 3 {
		t.Errorf("after enabling again: %s/%s v%d, want ask/person v3", got.AiMode, got.AiModeSource, got.AiModeVersion)
	}
}

// TestM174MatrixParityIsNotVacuous (section 11 test 19, beside
// TestMatrixParity) compares every (mode, class) pair, unknown values
// included, between aipolicy.Allows and ai_mode_allows, as
// wpmgr_app.
//
// Mutation: flip any one cell of ai_mode_allows, or of the Go matrix.
func TestM174MatrixParityIsNotVacuous(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m174g-matrix-"+uuid.NewString()[:8])

	modes := []string{"bogus", ""}
	for _, m := range aipolicy.Modes() {
		modes = append(modes, string(m))
	}
	classes := []string{"bogus", ""}
	for _, c := range aipolicy.StoredClasses() {
		classes = append(classes, string(c))
	}
	pairs, allowed := 0, 0
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m174g matrix)")
		for _, m := range modes {
			for _, c := range classes {
				var sqlSays bool
				if err := tx.QueryRow(ctx, `SELECT ai_mode_allows($1, $2)`, m, c).Scan(&sqlSays); err != nil {
					return err
				}
				goSays := aipolicy.Allows(aipolicy.Mode(m), aipolicy.Class(c))
				if goSays != sqlSays {
					t.Errorf("(%q, %q): Go %t, SQL %t", m, c, goSays, sqlSays)
				}
				pairs++
				if sqlSays {
					allowed++
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("ai_mode_allows: %v", err)
	}
	t.Logf("compared %d (mode, class) pairs; SQL allows %d", pairs, allowed)
	if allowed == 0 || allowed == pairs || !aipolicy.Allows(aipolicy.ModeAIDrafts, aipolicy.ClassAIDraft) {
		t.Errorf("positive control: %d of %d pairs allowed, ai_drafts/ai_draft = %t; the comparison is vacuous",
			allowed, pairs, aipolicy.Allows(aipolicy.ModeAIDrafts, aipolicy.ClassAIDraft))
	}
}

// TestM174GrantRunsBySettingStaysInTenant proves mcp_grant_runs_by_setting
// answers only inside the grant's tenant, answers inside a site-scoped
// transaction (where the grant itself is hidden), puts the site scope back
// on before it returns, and reads never after the switch is stopped.
//
// Mutation: drop the set_config that restores app.site_scope (the scope
// reads empty after the call, and the site-scoped count widens).
func TestM174GrantRunsBySettingStaysInTenant(t *testing.T) {
	ctx := context.Background()
	w := m174gNewWorld(t)
	runs := func(tenant uuid.UUID) bool {
		var ok bool
		if err := w.pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "InTenantTx (m174g runs by setting)")
			return tx.QueryRow(ctx, `SELECT mcp_grant_runs_by_setting($1, $2)`, w.tenant, w.grant).Scan(&ok)
		}); err != nil {
			t.Fatalf("mcp_grant_runs_by_setting: %v", err)
		}
		return ok
	}
	if !runs(w.tenant) {
		t.Errorf("under its own tenant: false, want true")
	}
	if runs(w.tenantB) {
		t.Errorf("under another tenant: true, want false")
	}

	var ok bool
	var scopeAfter string
	var sitesSeen, sitesAfter, grantsSeen int
	if err := w.pool.InScopedTenantTx(ctx, w.tenant, w.alice, []uuid.UUID{w.site}, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InScopedTenantTx (m174g runs by setting)")
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM sites`).Scan(&sitesSeen); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT mcp_grant_runs_by_setting($1, $2)`, w.tenant, w.grant).Scan(&ok); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT coalesce(current_setting('app.site_scope', true), '<null>')`).Scan(&scopeAfter); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM sites`).Scan(&sitesAfter); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM mcp_grants`).Scan(&grantsSeen)
	}); err != nil {
		t.Fatalf("site-scoped transaction: %v", err)
	}
	t.Logf("site-scoped {site}: runs=%t scope after=%q sites before=%d after=%d grants visible=%d", ok, scopeAfter, sitesSeen, sitesAfter, grantsSeen)
	if !ok || scopeAfter != "on" || sitesSeen != 1 || sitesAfter != 1 || grantsSeen != 0 {
		t.Errorf("in a site-scoped transaction: want runs=true, the scope back on, 1 site before and after, 0 grants")
	}

	if _, err := m174gSwitch(t, w.pool, w.tenant, w.grant, "never", nil); err != nil {
		t.Fatalf("stop the connection: %v", err)
	}
	if runs(w.tenant) {
		t.Errorf("after the switch is set to never: true, want false")
	}
}

// TestM174ChangeClassOnlyBySuperadmin proves set_ability_change_class, the
// only run-time path that re-classes a route, refuses anyone who is not a
// superadmin, refuses a value that is not a class, and writes the audit row
// naming the old and new class with the change.
//
// Mutation: drop the definer's superadmin check (alice's re-class is
// accepted).
func TestM174ChangeClassOnlyBySuperadmin(t *testing.T) {
	ctx := context.Background()
	w := m174gNewWorld(t)
	admin := connectAdmin(t, w.pool)
	defer admin.Close()
	if _, err := admin.Exec(ctx, `UPDATE users SET is_superadmin = true WHERE id = $1`, w.bob); err != nil {
		t.Fatalf("make bob a superadmin: %v", err)
	}
	const route = "wp-v2-posts-update-fields"
	var before string
	if err := admin.QueryRow(ctx, `SELECT change_class FROM rest_route_catalogue WHERE route_id = $1`, route).Scan(&before); err != nil {
		t.Fatalf("read the route's class: %v", err)
	}
	call := func(actor uuid.UUID, class string) (string, error) {
		var out string
		err := w.pool.InTenantTx(ctx, w.tenant, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "InTenantTx (m174g re-class)")
			return tx.QueryRow(ctx, `SELECT set_ability_change_class($1, 'rest_route', $2, $3)`, actor, route, class).Scan(&out)
		})
		return out, err
	}

	_, err := call(w.alice, "ai_draft")
	m174gWant(t, "a re-class by a person who is not a superadmin", err, "42501")
	_, err = call(w.bob, "sometimes")
	m174gWant(t, "a re-class to a value that is not a class", err, "22023")
	got, err := call(w.bob, "always_ask")
	m174gWant(t, "a re-class by a superadmin", err, "")
	if err == nil && got != "always_ask" {
		t.Errorf("the definer returned %q, want always_ask", got)
	}

	var n int
	var auditBefore, auditAfter *string
	var actor uuid.UUID
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM rest_route_catalogue_audit WHERE route_id = $1 AND after_change_class IS NOT NULL`, route).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("re-class audit rows: %d, want 1", n)
	}
	if err := admin.QueryRow(ctx, `SELECT before_change_class, after_change_class, actor_user_id FROM rest_route_catalogue_audit WHERE route_id = $1 AND after_change_class IS NOT NULL`, route).Scan(&auditBefore, &auditAfter, &actor); err != nil {
		t.Fatalf("read the audit row: %v", err)
	}
	if auditBefore == nil || *auditBefore != before || auditAfter == nil || *auditAfter != "always_ask" || actor != w.bob {
		t.Errorf("audit row: before %v after %v actor %s; want %s -> always_ask by bob", auditBefore, auditAfter, actor, before)
	}
}

// TestM174UserIDDoesNotLeakAcrossPooledTransactions (R26) proves app.user_id
// is local to its transaction: on one pooled connection, a transaction that
// signed in alice is followed by one with no user, and there a write naming
// alice as the setter is refused.
//
// Mutation: make InTenantTxAsUser set app.user_id for the session instead of
// the transaction (the second transaction then reads alice and the write is
// accepted).
func TestM174UserIDDoesNotLeakAcrossPooledTransactions(t *testing.T) {
	ctx := context.Background()
	w := m174gNewWorld(t)
	adminDSNsMu.Lock()
	dsn, ok := adminDSNs[w.pool]
	adminDSNsMu.Unlock()
	if !ok {
		t.Fatal("SETUP FAILURE: no DSN recorded for this pool")
	}
	appDSN := strings.Replace(dsn, "wpmgr:wpmgr@", "wpmgr_app:app@", 1)
	sep := "?"
	if strings.Contains(appDSN, "?") {
		sep = "&"
	}
	one, err := db.Connect(ctx, appDSN+sep+"pool_max_conns=1")
	if err != nil {
		t.Fatalf("connect a one-connection wpmgr_app pool: %v", err)
	}
	defer one.Close()

	var pid1, pid2 int32
	if err := one.InTenantTxAsUser(ctx, w.tenantB, w.alice, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTxAsUser (m174g first)")
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid1); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE sites SET ai_mode_source = 'person', ai_mode_set_at = now(), ai_mode_version = ai_mode_version + 1 WHERE id = $1`, w.siteB)
		return err
	}); err != nil {
		t.Fatalf("alice records her own Ask: %v", err)
	}
	var seen string
	err = one.InTenantTx(ctx, w.tenantB, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m174g second)")
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid(), coalesce(current_setting('app.user_id', true), '')`).Scan(&pid2, &seen); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE sites SET ai_mode_set_by = $2, ai_mode_set_at = now(), ai_mode_version = ai_mode_version + 1 WHERE id = $1`, w.siteB, w.alice)
		return err
	})
	if pid1 == 0 || pid1 != pid2 {
		t.Fatalf("the two transactions ran on connections %d and %d; this proof needs one", pid1, pid2)
	}
	if seen != "" {
		t.Errorf("app.user_id in the later transaction = %q, want empty", seen)
	}
	m174gWant(t, "naming alice in a later transaction on the same connection, with no user", err, "42501")
}

// TestM174SiteModeHonoursSiteScope (R26) proves the mode columns inherit
// sites' row security: with the site scope on and allowlist {site}, site2's
// mode is neither readable nor writable through the shipped statements, an
// empty allowlist reads nothing, and another tenant reads nothing.
//
// Mutation: drop sites_site_scope (site2 becomes readable and writable).
func TestM174SiteModeHonoursSiteScope(t *testing.T) {
	ctx := context.Background()
	w := m174gNewWorld(t)
	read := func(p domain.Principal, site uuid.UUID) error {
		return w.pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "RunTenantTx (m174g scoped read)")
			_, err := sqlc.New(tx).GetSiteAIMode(ctx, sqlc.GetSiteAIModeParams{TenantID: w.tenant, SiteID: site})
			return err
		})
	}
	only := acprSitePrincipal(w.tenant, w.site)
	if err := read(only, w.site); err != nil {
		t.Errorf("reading the allowed site: %v", err)
	}
	if err := read(only, w.site2); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("reading a site outside the allowlist: err = %v, want no row", err)
	}
	if err := read(acprOrgPrincipal(w.tenantB), w.site); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("another tenant reading the site: err = %v, want no row", err)
	}
	var n int
	if err := w.pool.InScopedTenantTx(ctx, w.tenant, w.alice, []uuid.UUID{}, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InScopedTenantTx (m174g empty allowlist)")
		return tx.QueryRow(ctx, `SELECT count(*) FROM sites WHERE ai_mode IS NOT NULL`).Scan(&n)
	}); err != nil {
		t.Fatalf("empty allowlist: %v", err)
	}
	if n != 0 {
		t.Errorf("an empty allowlist reads %d sites, want 0", n)
	}

	lower := func(site uuid.UUID, expected int64) (sqlc.SetSiteAIModeRow, error) {
		return m174SetMode(t, w.pool, only, sqlc.SetSiteAIModeParams{
			Mode: "ask", Source: "tightened", TenantID: w.tenant, SiteID: site, ExpectedVersion: expected,
		})
	}
	if _, err := lower(w.site2, 0); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("writing a site outside the allowlist: err = %v, want no row", err)
	}
	if got := m174gMode(t, w.pool, w.tenant, w.site2); got.AiModeVersion != 0 || got.AiModeSource != "unset" {
		t.Errorf("site2 after the refused write: %s v%d, want unset v0", got.AiModeSource, got.AiModeVersion)
	}
	if r, err := lower(w.site, w.mode.AiModeVersion); err != nil || !r.Applied || r.AiMode != "ask" {
		t.Errorf("lowering the allowed site: %+v, %v; want applied ask", r, err)
	}
}
