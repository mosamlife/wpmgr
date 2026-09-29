// uptime_app_alert_rollout_m146_repair_test.go - m146 (PR #786) repairs
// m108's rollout decision under the production migrator role: FORCE ROW
// LEVEL SECURITY on sites hid every row from m108's own "does a site
// already exist" check (no app.* GUC set by the production migrator), so
// every upgrade deployment was wrongly recorded as a fresh install
// (20260810120000_m146_app_alert_rollout_repair.sql).
//
// migrate.go applies an unapplied version even when later versions are
// already applied (internal/db/migrate.go:52-63), so on hosted this repair
// runs LATE, at the next deploy, on a database already long past m108.
// Every test in this file proves that late-run shape.
//
// TestM108Migration_UpgradeDeployment_DefaultsAppAlertsOff in
// uptime_app_alert_rollout_m108_test.go covers the OTHER shape m146 fixes:
// the SAME-BOOT case, where m108 and m146 apply together on a database that
// had never run m108 before.
package tests

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

// m146MigrationVersion is the embedded migration filename (sans .sql) that
// repairs m108's rollout decision under the production migrator role.
const m146MigrationVersion = "20260810120000_m146_app_alert_rollout_repair"

// startPostgresBeforeM146 mirrors startPostgresBeforeM108 (same file) but
// stops short of m146 instead of m108, so m108 has ALREADY RUN by the time
// the caller gets control back. Reuses applyMigrationsBeforeM108's embedded-
// FS walk (it takes stopAt as a parameter; there is nothing m108-specific
// about it beyond the name).
func startPostgresBeforeM146(t *testing.T) (admin *db.Pool, owner *db.Pool) {
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
		t.Fatalf("connection string: %v", err)
	}
	admin, err = db.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
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

	ownerDSN := strings.Replace(adminDSN, "wpmgr:wpmgr@", "wpmgr_owner:owner@", 1)
	owner, err = db.Connect(ctx, ownerDSN)
	if err != nil {
		setupFatalf(t, err, "postgres: connect as wpmgr_owner")
	}
	t.Cleanup(owner.Close)

	var ownerSuper, ownerBypass bool
	if err := owner.QueryRow(ctx,
		"SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user").
		Scan(&ownerSuper, &ownerBypass); err != nil {
		setupFatalf(t, err, "postgres: read owner role attributes")
	}
	if ownerSuper || ownerBypass {
		t.Fatalf("wpmgr_owner has rolsuper=%t rolbypassrls=%t; this harness's premise is a role row security applies to",
			ownerSuper, ownerBypass)
	}

	applyMigrationsBeforeM108(t, owner, m146MigrationVersion)
	return admin, owner
}

// m108AppliedAt reads m108's own applied_at from schema_migrations — the
// cutoff m146 turns off stale alert_configs rows against.
func m108AppliedAt(t *testing.T, pool *db.Pool) time.Time {
	t.Helper()
	var ts time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT applied_at FROM schema_migrations WHERE version = $1`, m108MigrationVersion,
	).Scan(&ts); err != nil {
		t.Fatalf("read m108 applied_at: %v", err)
	}
	return ts
}

// assertM146TablesForceIntact fails the test unless sites, alert_configs and
// site_app_alert_state all still carry both relrowsecurity and
// relforcerowsecurity: m146 lifts FORCE on all three for the length of its
// repair and must restore it before it returns, and it raises on its own if
// it doesn't — this is the test-side check on top of that.
func assertM146TablesForceIntact(t *testing.T, pool *db.Pool) {
	t.Helper()
	ctx := context.Background()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_class
		 WHERE oid IN ('public.sites'::regclass, 'public.alert_configs'::regclass, 'public.site_app_alert_state'::regclass)
		   AND relrowsecurity AND relforcerowsecurity`,
	).Scan(&n); err != nil {
		t.Fatalf("read pg_class force-rls state: %v", err)
	}
	if n != 3 {
		t.Fatalf("sites/alert_configs/site_app_alert_state FORCE-intact count = %d, want 3", n)
	}
}

// alertConfigsAppAlertsEnabledDefault reads the column's own DEFAULT
// expression via pg_attrdef — proof the DEFAULT itself changed, not just
// existing rows.
func alertConfigsAppAlertsEnabledDefault(t *testing.T, pool *db.Pool) string {
	t.Helper()
	ctx := context.Background()
	var def string
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_expr(d.adbin, d.adrelid)
		 FROM pg_attrdef d
		 JOIN pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum
		 WHERE d.adrelid = 'public.alert_configs'::regclass AND a.attname = 'app_alerts_enabled'`,
	).Scan(&def); err != nil {
		t.Fatalf("read alert_configs.app_alerts_enabled default via pg_attrdef: %v", err)
	}
	return def
}

// replaceOnce replaces exactly one occurrence of old with new in body and
// fails the test if old was not found exactly once — the positive control
// for a mutation that must stay syntactically valid SQL (stripOnce, its
// sibling in migration_late_run_lock_test.go, only ever removes text).
func replaceOnce(t *testing.T, body, old, new string) string {
	t.Helper()
	if strings.Count(body, old) != 1 {
		t.Fatalf("expected exactly one occurrence of substring in migration body; the wording likely changed — update this test's copy:\n%s", old)
	}
	return strings.Replace(body, old, new, 1)
}

// ---- Late-run behavior -----------------------------------------------

// TestM146LateRun_TurnsOffStaleRowsKeepsRecentlySaved is the main late-run
// proof: sites and an alert_configs row seeded BEFORE m108 runs at all (so
// m108 itself reproduces the original "always fresh" bug on real,
// pre-existing data), reaching head with m146 withheld, then a second
// tenant's alert_configs row saved AFTER m108 (updated_at > cutoff) and a
// site_app_alert_state row (the "this is a late run" signal) before m146
// finally lands.
func TestM146LateRun_TurnsOffStaleRowsKeepsRecentlySaved(t *testing.T) {
	// startPostgresBeforeM108 (uptime_app_alert_rollout_m108_test.go), NOT
	// startPostgresBeforeM146: it stops BEFORE m108 itself, so the site and
	// alert_configs row seeded below are in place before m108 actually
	// executes below — startPostgresBeforeM146 stops before m146, which
	// means m108 has ALREADY run (on an empty database) by the time it
	// returns, too late to reproduce the "already had a site" case.
	admin, owner := startPostgresBeforeM108(t)
	ctx := context.Background()

	tenant := seedTenant(t, admin, "m146-laterun-stale")
	if _, err := admin.Exec(ctx,
		`INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, $3)`,
		tenant, "https://m146-laterun-stale.example.com", "seed"); err != nil {
		t.Fatalf("seed pre-m108 site: %v", err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO alert_configs (tenant_id, enabled) VALUES ($1, true)`, tenant); err != nil {
		t.Fatalf("seed pre-m108 alert_configs: %v", err)
	}

	// Reach head with m146 withheld: m108 runs, unaided, on a database that
	// already had a site — reproducing the original bug (fresh_install=true
	// despite a pre-existing deployment).
	scopePrefillMark(t, owner, m146MigrationVersion)
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate to head with m146 withheld: %v", err)
	}

	var freshAfterM108 bool
	if err := admin.QueryRow(ctx, `SELECT fresh_install FROM app_alert_rollout WHERE singleton = true`).Scan(&freshAfterM108); err != nil {
		t.Fatalf("query app_alert_rollout: %v", err)
	}
	if !freshAfterM108 {
		t.Fatal("setup invariant broken: fresh_install should still be true (m108's own bug) before m146 runs")
	}

	// A site_app_alert_state row: one of the three "this is a late run"
	// signals (the state-row-exists signal; see TestM146LateRun_
	// LaterVersionSignal_TurnsOffStaleRow and TestM146LateRun_
	// SavedSinceM108Signal_KeepsRecentTurnsOffStale for the other two,
	// exercised alone).
	var siteID uuid.UUID
	if err := admin.QueryRow(ctx, `SELECT id FROM sites WHERE tenant_id = $1`, tenant).Scan(&siteID); err != nil {
		t.Fatalf("read seeded site id: %v", err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO site_app_alert_state (site_id, tenant_id) VALUES ($1, $2)`, siteID, tenant); err != nil {
		t.Fatalf("seed site_app_alert_state: %v", err)
	}

	// A SECOND tenant's alert_configs row, saved AFTER m108 ran (updated_at
	// defaults to now(), which is after m108's applied_at since real time
	// has passed) — this row must survive m146 untouched, per owner ruling A.
	savedAfterTenant := seedTenant(t, admin, "m146-laterun-saved-after")
	if _, err := admin.Exec(ctx,
		`INSERT INTO alert_configs (tenant_id, app_alerts_enabled) VALUES ($1, true)`, savedAfterTenant); err != nil {
		t.Fatalf("seed post-m108 alert_configs: %v", err)
	}

	// The fix: unmark m146 and migrate again.
	scopePrefillUnmark(t, owner, m146MigrationVersion)
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate with m146 present: %v", err)
	}

	var freshAfterM146 bool
	if err := admin.QueryRow(ctx, `SELECT fresh_install FROM app_alert_rollout WHERE singleton = true`).Scan(&freshAfterM146); err != nil {
		t.Fatalf("query app_alert_rollout after m146: %v", err)
	}
	if freshAfterM146 {
		t.Fatal("app_alert_rollout.fresh_install must be false after m146 repairs an upgrade deployment")
	}

	if def := alertConfigsAppAlertsEnabledDefault(t, admin); def != "false" {
		t.Fatalf("alert_configs.app_alerts_enabled column default = %q, want \"false\" after m146", def)
	}

	var staleEnabled bool
	if err := admin.QueryRow(ctx, `SELECT app_alerts_enabled FROM alert_configs WHERE tenant_id = $1`, tenant).Scan(&staleEnabled); err != nil {
		t.Fatalf("read the never-saved-since-m108 row: %v", err)
	}
	if staleEnabled {
		t.Fatal("the alert_configs row never saved since m108 (updated_at <= cutoff) must be turned off by m146")
	}

	var savedAfterEnabled bool
	if err := admin.QueryRow(ctx, `SELECT app_alerts_enabled FROM alert_configs WHERE tenant_id = $1`, savedAfterTenant).Scan(&savedAfterEnabled); err != nil {
		t.Fatalf("read the saved-after-m108 row: %v", err)
	}
	if !savedAfterEnabled {
		t.Fatal("the alert_configs row saved AFTER m108 (updated_at > cutoff) must be left on by m146")
	}

	assertM146TablesForceIntact(t, admin)
}

// TestM146LateRun_FreshAtM108_SitesEnrolledLater_Noop covers the "fresh at
// m108, sites enrolled later" case: a genuinely fresh install correctly
// recorded fresh_install=true, and a site enrolled afterward must not make
// m146 (arriving even later) treat the deployment as an upgrade. A second
// run changes nothing either.
func TestM146LateRun_FreshAtM108_SitesEnrolledLater_Noop(t *testing.T) {
	admin, owner := startPostgresBeforeM146(t)
	ctx := context.Background()

	// No sites seeded before m108 — a genuinely fresh install.
	scopePrefillMark(t, owner, m146MigrationVersion)
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate to head with m146 withheld: %v", err)
	}

	var freshAfterM108 bool
	if err := admin.QueryRow(ctx, `SELECT fresh_install FROM app_alert_rollout WHERE singleton = true`).Scan(&freshAfterM108); err != nil {
		t.Fatalf("query app_alert_rollout: %v", err)
	}
	if !freshAfterM108 {
		t.Fatal("setup invariant broken: fresh_install should be true (genuinely fresh install)")
	}

	// A site enrolled AFTER m108 ran (created_at defaults to now(), which is
	// after m108's applied_at) — this must NOT make m146 treat the
	// deployment as an upgrade.
	tenant := seedTenant(t, admin, "m146-fresh-sites-later")
	if _, err := admin.Exec(ctx,
		`INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, $3)`,
		tenant, "https://m146-fresh-sites-later.example.com", "site"); err != nil {
		t.Fatalf("seed post-m108 site: %v", err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO alert_configs (tenant_id, app_alerts_enabled) VALUES ($1, true)`, tenant); err != nil {
		t.Fatalf("seed alert_configs: %v", err)
	}

	scopePrefillUnmark(t, owner, m146MigrationVersion)
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate with m146 present: %v", err)
	}

	var freshAfterM146 bool
	if err := admin.QueryRow(ctx, `SELECT fresh_install FROM app_alert_rollout WHERE singleton = true`).Scan(&freshAfterM146); err != nil {
		t.Fatalf("query app_alert_rollout after m146: %v", err)
	}
	if !freshAfterM146 {
		t.Fatal("m146 must leave a genuinely fresh install's fresh_install=true even once a site is enrolled later")
	}
	if def := alertConfigsAppAlertsEnabledDefault(t, admin); def != "true" {
		t.Fatalf("alert_configs.app_alerts_enabled column default = %q, want \"true\" (fresh install untouched)", def)
	}
	var enabled bool
	if err := admin.QueryRow(ctx, `SELECT app_alerts_enabled FROM alert_configs WHERE tenant_id = $1`, tenant).Scan(&enabled); err != nil {
		t.Fatalf("read alert_configs row: %v", err)
	}
	if !enabled {
		t.Fatal("m146 must not touch alert_configs on a genuinely fresh install")
	}

	// Second run: still a no-op.
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var freshSecond bool
	if err := admin.QueryRow(ctx, `SELECT fresh_install FROM app_alert_rollout WHERE singleton = true`).Scan(&freshSecond); err != nil {
		t.Fatalf("query app_alert_rollout after second run: %v", err)
	}
	if !freshSecond {
		t.Fatal("a second run must not change fresh_install on a fresh install")
	}
	assertM146TablesForceIntact(t, admin)
}

// TestM146LateRun_ConvergedDeployment_TakesNoLock proves the doc comment's
// "a late run on a correct database takes no lock" claim: with
// fresh_install already false, m146's early-return check (the very first
// statement in its DO block) must return before ever touching sites,
// alert_configs or site_app_alert_state — no ALTER TABLE, no lock.
//
// Proof structure mirrors TestM141LateRunAfterM88AlreadyApplied
// (migration_late_run_lock_test.go's shared helpers): reach head with m146
// withheld so the bounded Migrate() call below only has m146 itself left to
// apply, force the rollout to converged (false), hold ROW EXCLUSIVE on sites
// from a second connection, then prove an IN-MEMORY copy of m146 with the
// early-return guard stripped WOULD block behind that hold (fires), while
// the REAL file returns fast and takes no lock at all (does not over-fire).
func TestM146LateRun_ConvergedDeployment_TakesNoLock(t *testing.T) {
	admin, owner := startPostgresBeforeM146(t)
	ctx := context.Background()

	scopePrefillMark(t, owner, m146MigrationVersion)
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate to head with m146 withheld: %v", err)
	}

	// Simulate a database where the rollout has already converged to false
	// (e.g. a prior working run of this same repair) — what the
	// early-return guard keys on.
	if _, err := admin.Exec(ctx, `UPDATE app_alert_rollout SET fresh_install = false`); err != nil {
		t.Fatalf("set fresh_install=false: %v", err)
	}
	scopePrefillUnmark(t, owner, m146MigrationVersion)

	body, err := fs.ReadFile(migrations.FS, m146MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m146 migration body: %v", err)
	}
	mutated := stripOnce(t, string(body),
		"    IF v_fresh IS DISTINCT FROM true THEN\n        RETURN;\n    END IF;\n")

	release := holdRowExclusiveOpen(t, owner, "sites", "updated_at")
	defer release()

	// Fires: without the early-return guard, m146 would try to lift FORCE
	// on sites (ALTER TABLE, ACCESS EXCLUSIVE) and block behind the held
	// ROW EXCLUSIVE until its own 5s lock_timeout fires — SQLSTATE 55P03.
	mutatedMigrationMustBlockOrError(t, owner, mutated, sqlStateLockNotAvailable)

	// Does not over-fire: the REAL m146 must return before taking any lock,
	// despite the ROW EXCLUSIVE holder still open.
	assertMigrateStaysUnderLockBound(t, owner, ctx)

	// Re-apply the real file directly and prove it took no lock on sites at
	// all, still with the ROW EXCLUSIVE holder open.
	assertFileTakesNoLockOnRelation(t, owner, "public.sites", string(body))

	release()

	var fresh bool
	if err := admin.QueryRow(ctx, `SELECT fresh_install FROM app_alert_rollout WHERE singleton = true`).Scan(&fresh); err != nil {
		t.Fatalf("query app_alert_rollout: %v", err)
	}
	if fresh {
		t.Fatal("fresh_install should remain false")
	}
	assertM146TablesForceIntact(t, admin)
}

// ---- Mutation proofs (never touch the committed migration file) --------

// TestM146MutationDropsCutoffPredicate_TurnsOffRecentlySavedRow proves the
// `updated_at <= v_cutoff` predicate is load-bearing: an IN-MEMORY copy of
// m146 with that predicate stripped (never the committed file — migrations
// are database-engineer's territory) wrongly turns off an alert_configs row
// saved AFTER m108.
func TestM146MutationDropsCutoffPredicate_TurnsOffRecentlySavedRow(t *testing.T) {
	admin, owner := startPostgresBeforeM146(t)
	ctx := context.Background()
	cutoff := m108AppliedAt(t, admin)

	tenant := seedTenant(t, admin, "m146-mutation-cutoff")
	var siteID uuid.UUID
	if err := admin.QueryRow(ctx,
		`INSERT INTO sites (tenant_id, url, name, created_at) VALUES ($1, $2, $3, $4) RETURNING id`,
		tenant, "https://m146-mutation-cutoff.example.com", "site", cutoff.Add(-time.Hour),
	).Scan(&siteID); err != nil {
		t.Fatalf("seed pre-cutoff site: %v", err)
	}
	// Forces v_live = true so the mutated body takes the cutoff-checked
	// branch (rather than the "fresh at m108" blanket turn-off branch).
	if _, err := admin.Exec(ctx,
		`INSERT INTO site_app_alert_state (site_id, tenant_id) VALUES ($1, $2)`, siteID, tenant); err != nil {
		t.Fatalf("seed site_app_alert_state: %v", err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO alert_configs (tenant_id, app_alerts_enabled, updated_at) VALUES ($1, true, $2)`,
		tenant, cutoff.Add(time.Hour)); err != nil {
		t.Fatalf("seed alert_configs saved after cutoff: %v", err)
	}

	body, err := fs.ReadFile(migrations.FS, m146MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m146 migration body: %v", err)
	}
	mutated := replaceOnce(t, string(body),
		"            WHERE app_alerts_enabled\n              AND updated_at <= v_cutoff;\n",
		"            WHERE app_alerts_enabled;\n",
	)

	if _, err := owner.Exec(ctx, mutated); err != nil {
		t.Fatalf("apply mutated m146 (cutoff predicate dropped): %v", err)
	}

	var enabled bool
	if err := admin.QueryRow(ctx, `SELECT app_alerts_enabled FROM alert_configs WHERE tenant_id = $1`, tenant).Scan(&enabled); err != nil {
		t.Fatalf("read app_alerts_enabled: %v", err)
	}
	if enabled {
		t.Fatal("mutation did not fire: dropping the updated_at <= cutoff predicate should have WRONGLY turned off " +
			"a row saved AFTER m108, but it is still on — the real migration's cutoff predicate is not proven " +
			"load-bearing by this check")
	}
	assertM146TablesForceIntact(t, admin)
}

// TestM146MutationForgetsUnforceAlertConfigs proves lifting FORCE on
// alert_configs is load-bearing: an IN-MEMORY copy of m146 with that ALTER
// TABLE line stripped fails outright — under FORCE ROW LEVEL SECURITY with
// row_security='off' and no BYPASSRLS, Postgres refuses the query rather
// than silently touching fewer rows — instead of quietly leaving every
// alert_configs row un-repaired.
func TestM146MutationForgetsUnforceAlertConfigs(t *testing.T) {
	admin, owner := startPostgresBeforeM146(t)
	ctx := context.Background()
	cutoff := m108AppliedAt(t, admin)

	tenant := seedTenant(t, admin, "m146-mutation-unforce")
	if _, err := admin.Exec(ctx,
		`INSERT INTO sites (tenant_id, url, name, created_at) VALUES ($1, $2, $3, $4)`,
		tenant, "https://m146-mutation-unforce.example.com", "site", cutoff.Add(-time.Hour)); err != nil {
		t.Fatalf("seed pre-cutoff site: %v", err)
	}

	body, err := fs.ReadFile(migrations.FS, m146MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m146 migration body: %v", err)
	}
	mutated := stripOnce(t, string(body),
		"    ALTER TABLE \"public\".\"alert_configs\" NO FORCE ROW LEVEL SECURITY;\n")

	_, err = owner.Exec(ctx, mutated)
	if err == nil {
		t.Fatal("mutation did not fire: applying m146 without lifting FORCE on alert_configs should have failed " +
			"with the RLS-blocks-the-owner error, but it succeeded — FORCE is not proven load-bearing on alert_configs")
	}
	if !strings.Contains(err.Error(), "row-level security policy") || !strings.Contains(err.Error(), "alert_configs") {
		t.Fatalf("got error %v, want a row-level-security-policy error naming alert_configs", err)
	}

	// alert_configs must still be FORCE ROW LEVEL SECURITY: the failed
	// statement rolled back its own implicit transaction.
	var forcesec bool
	if err := admin.QueryRow(ctx,
		`SELECT relforcerowsecurity FROM pg_class WHERE oid = 'public.alert_configs'::regclass`,
	).Scan(&forcesec); err != nil {
		t.Fatalf("read alert_configs pg_class: %v", err)
	}
	if !forcesec {
		t.Fatal("alert_configs lost FORCE ROW LEVEL SECURITY after the failed mutated run")
	}

	// The failure must not have knocked FORCE off sites or
	// site_app_alert_state either: check every table this file toggles, not
	// just the one whose ALTER TABLE line was stripped.
	assertM146TablesForceIntact(t, admin)
}

// ---- Each late-run signal alone (PR #786 r2) ---------------------------
//
// TestM146LateRun_TurnsOffStaleRowsKeepsRecentlySaved (above) seeds BOTH a
// site_app_alert_state row and an alert_configs row saved after m108, so it
// never isolates any one of the three `v_live` signals: dropping either the
// schema_migrations later-version clause or (before this PR) an equivalent
// of the alert_configs updated_at clause left that test, and the whole
// suite, green. The two tests below each seed exactly one signal and no
// other, so a regression to that signal's own clause is the only thing that
// can make them fail.

// fakeLaterSchemaMigrationsVersion is a schema_migrations version string
// that sorts after m146's own version (20260810120000) but is never a real
// embedded migration file — inserted directly so
// TestM146LateRun_LaterVersionSignal_TurnsOffStaleRow can fake "a later
// release already booted" without depending on what m146+1 happens to be
// named today. internal/db/migrate.go's runner only ever looks up FS-derived
// version strings in this table (Migrate, m146MigrationVersion above); a row
// with no matching file is simply inert to it.
const fakeLaterSchemaMigrationsVersion = "20260810120001_test_fake_later_signal"

// TestM146LateRun_LaterVersionSignal_TurnsOffStaleRow isolates the SECOND
// v_live signal: no site_app_alert_state row and no alert_configs row saved
// since m108, but a schema_migrations version later than m146's own
// recorded. Proves that signal alone still drives the stale-row turn-off and
// the fresh_install/column-default repair.
//
// This test cannot also prove the schema_migrations clause is load-bearing
// by mutation: v_live only ever changes the alert_configs UPDATE's WHERE
// clause between "every enabled row" and "every enabled row not saved since
// m108", and with no row saved since m108 in scope anywhere, those two
// predicates select the identical set. Any row that WOULD distinguish them
// (one saved after the cutoff) itself satisfies the third signal
// (TestM146LateRun_SavedSinceM108Signal_KeepsRecentTurnsOffStale) regardless
// of this one, so a fires/does-not-fire proof isolated to the
// schema_migrations clause alone is not constructible against the current
// three-signal body. Verified by actually stripping the clause and
// re-running this test's own scenario: the result did not change.
func TestM146LateRun_LaterVersionSignal_TurnsOffStaleRow(t *testing.T) {
	admin, owner := startPostgresBeforeM108(t)
	ctx := context.Background()

	tenant := seedTenant(t, admin, "m146-laterun-laterversion")
	if _, err := admin.Exec(ctx,
		`INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, $3)`,
		tenant, "https://m146-laterun-laterversion.example.com", "seed"); err != nil {
		t.Fatalf("seed pre-m108 site: %v", err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO alert_configs (tenant_id, enabled) VALUES ($1, true)`, tenant); err != nil {
		t.Fatalf("seed pre-m108 alert_configs: %v", err)
	}

	scopePrefillMark(t, owner, m146MigrationVersion)
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate to head with m146 withheld: %v", err)
	}

	var freshAfterM108 bool
	if err := admin.QueryRow(ctx, `SELECT fresh_install FROM app_alert_rollout WHERE singleton = true`).Scan(&freshAfterM108); err != nil {
		t.Fatalf("query app_alert_rollout: %v", err)
	}
	if !freshAfterM108 {
		t.Fatal("setup invariant broken: fresh_install should still be true (m108's own bug) before m146 runs")
	}

	// NO site_app_alert_state row, NO alert_configs row saved since m108 -
	// the only signal present is a later schema_migrations version.
	if _, err := admin.Exec(ctx,
		`INSERT INTO schema_migrations (version) VALUES ($1)`, fakeLaterSchemaMigrationsVersion); err != nil {
		t.Fatalf("seed fake later schema_migrations version: %v", err)
	}

	scopePrefillUnmark(t, owner, m146MigrationVersion)
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate with m146 present: %v", err)
	}

	var freshAfterM146 bool
	if err := admin.QueryRow(ctx, `SELECT fresh_install FROM app_alert_rollout WHERE singleton = true`).Scan(&freshAfterM146); err != nil {
		t.Fatalf("query app_alert_rollout after m146: %v", err)
	}
	if freshAfterM146 {
		t.Fatal("app_alert_rollout.fresh_install must be false after m146 repairs an upgrade deployment")
	}
	if def := alertConfigsAppAlertsEnabledDefault(t, admin); def != "false" {
		t.Fatalf("alert_configs.app_alerts_enabled column default = %q, want \"false\" after m146", def)
	}
	var enabled bool
	if err := admin.QueryRow(ctx, `SELECT app_alerts_enabled FROM alert_configs WHERE tenant_id = $1`, tenant).Scan(&enabled); err != nil {
		t.Fatalf("read the never-saved-since-m108 row: %v", err)
	}
	if enabled {
		t.Fatal("the alert_configs row never saved since m108 must be turned off by m146, with only the later-version signal present")
	}
	assertM146TablesForceIntact(t, admin)
}

// TestM146LateRun_SavedSinceM108Signal_KeepsRecentTurnsOffStale isolates the
// THIRD v_live signal (database-engineer's addition, PR #786 r2): no
// site_app_alert_state row and no later schema_migrations version recorded,
// but one tenant's alert_configs row was saved AFTER m108 ran. That row must
// survive untouched, and a second, never-saved-since tenant's row must still
// be turned off.
func TestM146LateRun_SavedSinceM108Signal_KeepsRecentTurnsOffStale(t *testing.T) {
	admin, owner := startPostgresBeforeM108(t)
	ctx := context.Background()

	staleTenant := seedTenant(t, admin, "m146-laterun-thirdsignal-stale")
	if _, err := admin.Exec(ctx,
		`INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, $3)`,
		staleTenant, "https://m146-thirdsignal-stale.example.com", "seed"); err != nil {
		t.Fatalf("seed pre-m108 site: %v", err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO alert_configs (tenant_id, enabled) VALUES ($1, true)`, staleTenant); err != nil {
		t.Fatalf("seed pre-m108 alert_configs: %v", err)
	}

	scopePrefillMark(t, owner, m146MigrationVersion)
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate to head with m146 withheld: %v", err)
	}

	var freshAfterM108 bool
	if err := admin.QueryRow(ctx, `SELECT fresh_install FROM app_alert_rollout WHERE singleton = true`).Scan(&freshAfterM108); err != nil {
		t.Fatalf("query app_alert_rollout: %v", err)
	}
	if !freshAfterM108 {
		t.Fatal("setup invariant broken: fresh_install should still be true (m108's own bug) before m146 runs")
	}

	// NO site_app_alert_state row, NO later schema_migrations version - the
	// only signal present is a SECOND tenant's alert_configs row saved after
	// m108 (updated_at defaults to now(), after m108's applied_at since real
	// time has passed).
	savedTenant := seedTenant(t, admin, "m146-laterun-thirdsignal-saved")
	if _, err := admin.Exec(ctx,
		`INSERT INTO alert_configs (tenant_id, app_alerts_enabled) VALUES ($1, true)`, savedTenant); err != nil {
		t.Fatalf("seed post-m108 alert_configs: %v", err)
	}

	scopePrefillUnmark(t, owner, m146MigrationVersion)
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate with m146 present: %v", err)
	}

	var staleEnabled bool
	if err := admin.QueryRow(ctx, `SELECT app_alerts_enabled FROM alert_configs WHERE tenant_id = $1`, staleTenant).Scan(&staleEnabled); err != nil {
		t.Fatalf("read the never-saved-since-m108 row: %v", err)
	}
	if staleEnabled {
		t.Fatal("the alert_configs row never saved since m108 must be turned off, with the third signal present only on a different tenant's row")
	}
	var savedEnabled bool
	if err := admin.QueryRow(ctx, `SELECT app_alerts_enabled FROM alert_configs WHERE tenant_id = $1`, savedTenant).Scan(&savedEnabled); err != nil {
		t.Fatalf("read the saved-after-m108 row: %v", err)
	}
	if !savedEnabled {
		t.Fatal("the alert_configs row saved AFTER m108 must be left on by m146 - it IS ITSELF the third signal that proves this is a late run")
	}
	assertM146TablesForceIntact(t, admin)
}

// TestM146MutationDropsThirdSignalClause_TurnsOffRecentlySavedRow proves the
// third `EXISTS` clause (any alert_configs row updated after m108's cutoff -
// database-engineer's addition, PR #786 r2) is load-bearing on its own: with
// no site_app_alert_state row and no later schema_migrations version, an
// IN-MEMORY copy of m146 with that clause stripped wrongly turns off a row
// saved AFTER m108 - the same row
// TestM146LateRun_SavedSinceM108Signal_KeepsRecentTurnsOffStale relies on the
// REAL migration to keep on.
func TestM146MutationDropsThirdSignalClause_TurnsOffRecentlySavedRow(t *testing.T) {
	admin, owner := startPostgresBeforeM146(t)
	ctx := context.Background()
	cutoff := m108AppliedAt(t, admin)

	tenant := seedTenant(t, admin, "m146-mutation-thirdsignal")
	var siteID uuid.UUID
	if err := admin.QueryRow(ctx,
		`INSERT INTO sites (tenant_id, url, name, created_at) VALUES ($1, $2, $3, $4) RETURNING id`,
		tenant, "https://m146-mutation-thirdsignal.example.com", "site", cutoff.Add(-time.Hour),
	).Scan(&siteID); err != nil {
		t.Fatalf("seed pre-cutoff site: %v", err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO alert_configs (tenant_id, app_alerts_enabled, updated_at) VALUES ($1, true, $2)`,
		tenant, cutoff.Add(time.Hour)); err != nil {
		t.Fatalf("seed alert_configs saved after cutoff: %v", err)
	}

	body, err := fs.ReadFile(migrations.FS, m146MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m146 migration body: %v", err)
	}
	mutated := stripOnce(t, string(body),
		"\n            OR EXISTS (\n                SELECT 1 FROM \"public\".\"alert_configs\"\n                WHERE updated_at > v_cutoff\n            )")

	if _, err := owner.Exec(ctx, mutated); err != nil {
		t.Fatalf("apply mutated m146 (third signal clause dropped): %v", err)
	}

	var enabled bool
	if err := admin.QueryRow(ctx, `SELECT app_alerts_enabled FROM alert_configs WHERE tenant_id = $1`, tenant).Scan(&enabled); err != nil {
		t.Fatalf("read app_alerts_enabled: %v", err)
	}
	if enabled {
		t.Fatal("mutation did not fire: dropping the third EXISTS clause should have WRONGLY turned off a row saved AFTER m108 " +
			"(with no other signal present), but it is still on - the real migration's third clause is not proven load-bearing " +
			"by this check")
	}
	assertM146TablesForceIntact(t, admin)
}
