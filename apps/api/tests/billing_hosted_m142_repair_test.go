// billing_hosted_m142_repair_test.go - m142 (PR #786) repairs m91's
// grandfather backfill under the production migrator role: FORCE ROW LEVEL
// SECURITY on sites hid every row from m91's own UPDATE (no app.* GUC set by
// the production migrator), so no over-cap free tenant ever got
// plan_overrides.max_sites written (20260724120000_m142_hosted_billing_grandfather_repair.sql).
//
// migrate.go applies an unapplied version even when later versions are
// already applied (internal/db/migrate.go:52-63), so on hosted this repair
// runs LATE, at the next deploy, on a database already long past m91. Every
// test in this file proves that late-run shape: reach a state where m91 has
// already run (unaided, reproducing the original defect), then let m142 land
// afterward and prove it grandfathers correctly, touches nobody it must not,
// and is a no-op on a second run.
//
// TestGrandfatherBackfill_OverCapTenantKeepsOperating in billing_hosted_test.go
// covers the OTHER shape m142 fixes: the SAME-BOOT case, where m91 and m142
// apply together on a database that had never run m91 before.
package tests

import (
	"context"
	"encoding/json"
	"fmt"
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

// m142MigrationVersion is the embedded migration filename (sans .sql) that
// repairs m91's grandfather backfill under the production migrator role.
const m142MigrationVersion = "20260724120000_m142_hosted_billing_grandfather_repair"

// startPostgresBeforeM142 mirrors startPostgresBeforeM91 (billing_hosted_test.go)
// but stops short of m142 instead of m91, so m91 has ALREADY RUN — and,
// being unaided, already reproduced the original defect (no
// plan_overrides.max_sites written for anyone) — before the caller seeds
// rows and finishes the boot with owner.Migrate, which is where m142
// actually runs.
func startPostgresBeforeM142(t *testing.T) (admin *db.Pool, owner *db.Pool) {
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

	applyMigrationsBefore(t, owner, m142MigrationVersion)
	return admin, owner
}

// m91AppliedAt reads m91's own applied_at from schema_migrations — the
// cutoff m142 grandfathers against.
func m91AppliedAt(t *testing.T, pool *db.Pool) time.Time {
	t.Helper()
	var ts time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT applied_at FROM schema_migrations WHERE version = $1`, m91MigrationVersion,
	).Scan(&ts); err != nil {
		t.Fatalf("read m91 applied_at: %v", err)
	}
	return ts
}

// assertSitesForceIntact fails the test unless sites still carries both
// relrowsecurity and relforcerowsecurity: m142 lifts FORCE for the length of
// its backfill and must restore it before it returns, and it raises on its
// own if it doesn't — this is the test-side check on top of that.
func assertSitesForceIntact(t *testing.T, pool *db.Pool) {
	t.Helper()
	ctx := context.Background()
	var rowsec, forcesec bool
	if err := pool.QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = 'public.sites'::regclass`,
	).Scan(&rowsec, &forcesec); err != nil {
		t.Fatalf("read sites pg_class: %v", err)
	}
	if !rowsec || !forcesec {
		t.Fatalf("sites relrowsecurity=%t relforcerowsecurity=%t, want true, true", rowsec, forcesec)
	}
}

// planOverridesMaxSites reads plan_overrides->>'max_sites' for tenant, and
// whether the key is present at all.
func planOverridesMaxSites(t *testing.T, pool *db.Pool, tenant uuid.UUID) (value int, present bool) {
	t.Helper()
	ctx := context.Background()
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT plan_overrides FROM tenants WHERE id = $1`, tenant).Scan(&raw); err != nil {
		t.Fatalf("read plan_overrides: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal plan_overrides: %v", err)
	}
	v, ok := doc["max_sites"]
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("plan_overrides.max_sites is not a number: %#v", v)
	}
	return int(f), true
}

// seedSiteAt inserts one site for tenant with an explicit created_at, so a
// test can control which side of m91's cutoff the site falls on regardless
// of insertion order.
func seedSiteAt(t *testing.T, pool *db.Pool, tenant uuid.UUID, url string, createdAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO sites (tenant_id, url, name, created_at) VALUES ($1, $2, $3, $4)`,
		tenant, url, "site", createdAt,
	); err != nil {
		t.Fatalf("seed site %s: %v", url, err)
	}
}

// ---- Late-run behavior -----------------------------------------------

// TestM142LateRun_OnlyPreCutoffSitesCounted covers two of the brief's four
// late-run assertions: a site created AFTER m91 applied is never counted
// toward the grandfather, and a second run changes nothing.
func TestM142LateRun_OnlyPreCutoffSitesCounted(t *testing.T) {
	admin, owner := startPostgresBeforeM142(t)
	ctx := context.Background()
	cutoff := m91AppliedAt(t, admin)

	// A tenant that already had 5 non-archived sites BEFORE m91 applied:
	// over cap, must be grandfathered to 5.
	overCapPreCutoff := seedTenant(t, admin, "m142-laterun-precutoff")
	for i := 0; i < 5; i++ {
		seedSiteAt(t, admin, overCapPreCutoff,
			fmt.Sprintf("https://m142-precutoff-%d.example.com", i), cutoff.Add(-time.Hour))
	}

	// A tenant with only 2 sites BEFORE m91 applied, plus 3 MORE added
	// afterward (5 total today, but only 2 pre-cutoff): must NOT be
	// grandfathered — the post-cutoff growth is exactly what free-tier
	// enforcement should now gate normally.
	grewAfterCutoff := seedTenant(t, admin, "m142-laterun-grew-after")
	for i := 0; i < 2; i++ {
		seedSiteAt(t, admin, grewAfterCutoff,
			fmt.Sprintf("https://m142-grew-old-%d.example.com", i), cutoff.Add(-time.Hour))
	}
	for i := 0; i < 3; i++ {
		seedSiteAt(t, admin, grewAfterCutoff,
			fmt.Sprintf("https://m142-grew-new-%d.example.com", i), cutoff.Add(time.Hour))
	}

	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate (m142 late run): %v", err)
	}

	if value, present := planOverridesMaxSites(t, admin, overCapPreCutoff); !present || value != 5 {
		t.Fatalf("over-cap pre-cutoff tenant: max_sites present=%v value=%d, want present=true value=5", present, value)
	}
	if _, present := planOverridesMaxSites(t, admin, grewAfterCutoff); present {
		t.Fatal("a tenant only over cap due to POST-cutoff growth must not be grandfathered")
	}
	assertSitesForceIntact(t, admin)

	// Second run: idempotent, changes nothing.
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if value, present := planOverridesMaxSites(t, admin, overCapPreCutoff); !present || value != 5 {
		t.Fatalf("after second run: max_sites present=%v value=%d, want present=true value=5 (unchanged)", present, value)
	}
	if _, present := planOverridesMaxSites(t, admin, grewAfterCutoff); present {
		t.Fatal("after second run: the post-cutoff-growth tenant must still have no max_sites key")
	}
}

// TestM142LateRun_PayingTenantsUntouched covers the brief's second
// assertion: a tenant on plan='agency', or with plan_status='active', is
// left untouched even when over cap with pre-cutoff sites — an override
// REPLACES MaxSites (entitlements.go:286-287), so writing one to a paying
// tenant would silently override its real plan-derived cap.
func TestM142LateRun_PayingTenantsUntouched(t *testing.T) {
	admin, owner := startPostgresBeforeM142(t)
	ctx := context.Background()
	cutoff := m91AppliedAt(t, admin)

	agencyTenant := seedTenant(t, admin, "m142-laterun-agency")
	if _, err := admin.Exec(ctx, `UPDATE tenants SET plan = 'agency' WHERE id = $1`, agencyTenant); err != nil {
		t.Fatalf("set plan=agency: %v", err)
	}
	for i := 0; i < 5; i++ {
		seedSiteAt(t, admin, agencyTenant, fmt.Sprintf("https://m142-agency-%d.example.com", i), cutoff.Add(-time.Hour))
	}

	activeTenant := seedTenant(t, admin, "m142-laterun-active")
	if _, err := admin.Exec(ctx, `UPDATE tenants SET plan_status = 'active' WHERE id = $1`, activeTenant); err != nil {
		t.Fatalf("set plan_status=active: %v", err)
	}
	for i := 0; i < 5; i++ {
		seedSiteAt(t, admin, activeTenant, fmt.Sprintf("https://m142-active-%d.example.com", i), cutoff.Add(-time.Hour))
	}

	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate (m142 late run): %v", err)
	}

	if _, present := planOverridesMaxSites(t, admin, agencyTenant); present {
		t.Fatal("a plan='agency' tenant must never receive a max_sites grandfather override")
	}
	if _, present := planOverridesMaxSites(t, admin, activeTenant); present {
		t.Fatal("a plan_status='active' tenant must never receive a max_sites grandfather override")
	}
	assertSitesForceIntact(t, admin)
}

// TestM142LateRun_ExistingOverrideUntouched covers the brief's third
// assertion: a tenant that already carries a max_sites override — whatever
// its value, and however it got there — keeps it exactly as-is.
func TestM142LateRun_ExistingOverrideUntouched(t *testing.T) {
	admin, owner := startPostgresBeforeM142(t)
	ctx := context.Background()
	cutoff := m91AppliedAt(t, admin)

	tenant := seedTenant(t, admin, "m142-laterun-override")
	if _, err := admin.Exec(ctx,
		`UPDATE tenants SET plan_overrides = jsonb_set(plan_overrides, '{max_sites}', '10'::jsonb, true) WHERE id = $1`,
		tenant); err != nil {
		t.Fatalf("seed existing override: %v", err)
	}
	for i := 0; i < 5; i++ {
		seedSiteAt(t, admin, tenant, fmt.Sprintf("https://m142-override-%d.example.com", i), cutoff.Add(-time.Hour))
	}

	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate (m142 late run): %v", err)
	}

	value, present := planOverridesMaxSites(t, admin, tenant)
	if !present || value != 10 {
		t.Fatalf("existing override: present=%v value=%d, want present=true value=10 (untouched, NOT recomputed to 5)", present, value)
	}
	assertSitesForceIntact(t, admin)
}

// ---- Fires proof --------------------------------------------------------

// TestM142FiresWhenSkipped_GrandfatherStaysMissing proves m142 is
// load-bearing for the late-run repair: with m142 recorded as applied but
// never actually run (a binary that predates PR #786's fix, exactly as
// production saw it before this repair), the original m91 defect must
// persist — no grandfather override written for an over-cap pre-cutoff
// tenant. Unmarking m142 and migrating again must then fix it.
func TestM142FiresWhenSkipped_GrandfatherStaysMissing(t *testing.T) {
	admin, owner := startPostgresBeforeM142(t)
	ctx := context.Background()
	cutoff := m91AppliedAt(t, admin)

	tenant := seedTenant(t, admin, "m142-fires")
	for i := 0; i < 5; i++ {
		seedSiteAt(t, admin, tenant, fmt.Sprintf("https://m142-fires-%d.example.com", i), cutoff.Add(-time.Hour))
	}

	// Simulate the stale binary: m142 recorded as applied, never run.
	scopePrefillMark(t, owner, m142MigrationVersion)
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate with m142 withheld: %v", err)
	}
	if _, present := planOverridesMaxSites(t, admin, tenant); present {
		t.Fatal("setup invariant broken: the original m91 defect (no grandfather) should still reproduce with m142 skipped")
	}

	// The fix: unmark m142 and migrate again.
	scopePrefillUnmark(t, owner, m142MigrationVersion)
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate with m142 present: %v", err)
	}
	if value, present := planOverridesMaxSites(t, admin, tenant); !present || value != 5 {
		t.Fatalf("after unmarking m142: max_sites present=%v value=%d, want present=true value=5", present, value)
	}
	assertSitesForceIntact(t, admin)
}

// ---- Mutation proofs (never touch the committed migration file) --------

// TestM142MutationDropsFreePlanPredicate_GrandfathersPayingTenant proves the
// `t.plan = 'free'` predicate is load-bearing: an IN-MEMORY copy of m142
// with that predicate stripped (never the committed file — migrations are
// database-engineer's territory) wrongly grandfathers an agency-plan tenant.
// This is the same defect TestM142LateRun_PayingTenantsUntouched exists to
// catch; this test proves that test would actually go red if the predicate
// were ever accidentally dropped.
func TestM142MutationDropsFreePlanPredicate_GrandfathersPayingTenant(t *testing.T) {
	admin, owner := startPostgresBeforeM142(t)
	ctx := context.Background()
	cutoff := m91AppliedAt(t, admin)

	tenant := seedTenant(t, admin, "m142-mutation-plan")
	if _, err := admin.Exec(ctx, `UPDATE tenants SET plan = 'agency' WHERE id = $1`, tenant); err != nil {
		t.Fatalf("set plan=agency: %v", err)
	}
	for i := 0; i < 5; i++ {
		seedSiteAt(t, admin, tenant, fmt.Sprintf("https://m142-mutation-plan-%d.example.com", i), cutoff.Add(-time.Hour))
	}

	body, err := fs.ReadFile(migrations.FS, m142MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m142 migration body: %v", err)
	}
	mutated := stripOnce(t, string(body), "      AND t.plan = 'free'\n")

	if _, err := owner.Exec(ctx, mutated); err != nil {
		t.Fatalf("apply mutated m142 (plan predicate dropped): %v", err)
	}

	value, present := planOverridesMaxSites(t, admin, tenant)
	if !present || value != 5 {
		t.Fatalf("mutation did not fire: dropping the plan='free' predicate should have WRONGLY grandfathered "+
			"an agency-plan tenant (max_sites=5), got present=%v value=%d — the real migration's plan "+
			"predicate is not proven load-bearing by this check", present, value)
	}
	assertSitesForceIntact(t, admin)
}

// TestM142LateRun_ArchivedSitesExcludedFromCount proves the
// `connection_state <> 'archived'` predicate is load-bearing on the COUNT
// itself, not just on which tenants get touched at all: a tenant with 4
// active (non-archived) and 2 archived pre-cutoff sites must be grandfathered
// to 4 (the true active count), never to 6 (every site regardless of state).
//
// Free-tier MaxSites is 3 (internal/billing/entitlements.go:135,
// "MaxSites values are LOCKED: free=3" — mirrored by this migration's own
// literal `> 3`), so the seeded active count must be over that cap on its
// own merits (4, not 3) for a write to happen at all; a tenant with exactly
// 3 active sites is AT the free cap, not over it, and would correctly
// receive no override regardless of how the archived ones are counted — that
// case cannot distinguish this predicate one way or the other.
func TestM142LateRun_ArchivedSitesExcludedFromCount(t *testing.T) {
	admin, owner := startPostgresBeforeM142(t)
	ctx := context.Background()
	cutoff := m91AppliedAt(t, admin)

	tenant := seedTenant(t, admin, "m142-laterun-archived")
	for i := 0; i < 4; i++ {
		seedSiteAt(t, admin, tenant, fmt.Sprintf("https://m142-archived-active-%d.example.com", i), cutoff.Add(-time.Hour))
	}
	for i := 0; i < 2; i++ {
		var siteID uuid.UUID
		if err := admin.QueryRow(ctx,
			`INSERT INTO sites (tenant_id, url, name, created_at) VALUES ($1, $2, $3, $4) RETURNING id`,
			tenant, fmt.Sprintf("https://m142-archived-archived-%d.example.com", i), "site", cutoff.Add(-time.Hour),
		).Scan(&siteID); err != nil {
			t.Fatalf("seed pre-cutoff archived site %d: %v", i, err)
		}
		if _, err := admin.Exec(ctx,
			`UPDATE sites SET connection_state = 'archived' WHERE id = $1`, siteID); err != nil {
			t.Fatalf("archive site %d: %v", i, err)
		}
	}

	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate (m142 late run): %v", err)
	}

	if value, present := planOverridesMaxSites(t, admin, tenant); !present || value != 4 {
		t.Fatalf("max_sites present=%v value=%d, want present=true value=4 (4 active, not 6 counting the 2 archived)", present, value)
	}
	assertSitesForceIntact(t, admin)
}

// TestM142MutationDropsArchivedFilter_CountsArchivedSites proves the
// `connection_state <> 'archived'` predicate load-bearing by mutation,
// completing TestM142LateRun_ArchivedSitesExcludedFromCount's positive
// proof: an IN-MEMORY copy of m142 with that predicate stripped (never the
// committed file — migrations are database-engineer's territory) wrongly
// grandfathers the same tenant to 6 (every site, archived included) instead
// of 4.
func TestM142MutationDropsArchivedFilter_CountsArchivedSites(t *testing.T) {
	admin, owner := startPostgresBeforeM142(t)
	ctx := context.Background()
	cutoff := m91AppliedAt(t, admin)

	tenant := seedTenant(t, admin, "m142-mutation-archived")
	for i := 0; i < 4; i++ {
		seedSiteAt(t, admin, tenant, fmt.Sprintf("https://m142-mutation-archived-active-%d.example.com", i), cutoff.Add(-time.Hour))
	}
	for i := 0; i < 2; i++ {
		var siteID uuid.UUID
		if err := admin.QueryRow(ctx,
			`INSERT INTO sites (tenant_id, url, name, created_at) VALUES ($1, $2, $3, $4) RETURNING id`,
			tenant, fmt.Sprintf("https://m142-mutation-archived-archived-%d.example.com", i), "site", cutoff.Add(-time.Hour),
		).Scan(&siteID); err != nil {
			t.Fatalf("seed pre-cutoff archived site %d: %v", i, err)
		}
		if _, err := admin.Exec(ctx,
			`UPDATE sites SET connection_state = 'archived' WHERE id = $1`, siteID); err != nil {
			t.Fatalf("archive site %d: %v", i, err)
		}
	}

	body, err := fs.ReadFile(migrations.FS, m142MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m142 migration body: %v", err)
	}
	mutated := stripOnce(t, string(body), "connection_state <> 'archived'\n          AND ")

	if _, err := owner.Exec(ctx, mutated); err != nil {
		t.Fatalf("apply mutated m142 (archived filter dropped): %v", err)
	}

	value, present := planOverridesMaxSites(t, admin, tenant)
	if !present || value != 6 {
		t.Fatalf("mutation did not fire: dropping the connection_state <> 'archived' predicate should have WRONGLY "+
			"grandfathered this tenant to 6 (every site, archived included), got present=%v value=%d — the real "+
			"migration's archived filter is not proven load-bearing by this check", present, value)
	}
	assertSitesForceIntact(t, admin)
}

// ---- Skip path: outside the server's runner (mirrors
//      uptime_m144_backfill_repair_test.go's own two sibling tests) --------

// readM142Body returns m142's own, UNMUTATED SQL text from the embedded FS.
func readM142Body(t *testing.T) string {
	t.Helper()
	body, err := fs.ReadFile(migrations.FS, m142MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m142 migration body: %v", err)
	}
	return string(body)
}

// m142AccessExclusiveLockCount runs body (m142's own SQL — the real,
// unmutated file, or an in-memory mutated copy) inside a fresh explicit
// transaction on pool, counts this backend's ACCESS EXCLUSIVE lock on sites
// in pg_locks, then rolls the transaction back — nothing body does is ever
// committed — and returns the count. Mirrors
// uptime_m144_backfill_repair_test.go's own m144AccessExclusiveLockCount,
// narrowed to the one table m142 touches.
func m142AccessExclusiveLockCount(t *testing.T, pool *db.Pool, body string) int {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explicit tx to apply m142's body: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, body); err != nil {
		t.Fatalf("apply m142's body inside an explicit tx: %v", err)
	}
	var n int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM pg_locks
		 WHERE pid = pg_backend_pid()
		   AND mode = 'AccessExclusiveLock'
		   AND relation = 'public.sites'::regclass`,
	).Scan(&n); err != nil {
		t.Fatalf("read pg_locks for sites: %v", err)
	}
	return n
}

// m142NoM91RowSkipBlock is the IF v_cutoff IS NULL ... END IF; block (plus
// its trailing blank line) that skips the whole backfill — never lifting
// FORCE or taking a lock on sites — when schema_migrations exists but
// carries no row for m91. Deleting it is the fires proof that
// TestM142SkipsOutsideServerRunner_NoM91Row's "no ACCESS EXCLUSIVE lock
// taken" assertion is load-bearing: without it, v_cutoff stays NULL, but the
// ALTER TABLE statement below runs anyway.
const m142NoM91RowSkipBlock = `    IF v_cutoff IS NULL THEN
        RAISE NOTICE 'm142: backfill skipped: schema_migrations has no row for 20260724000000_m91_hosted_billing_substrate, so m91 was not applied by the server''s migration runner and its apply time is unknown; free tenants that had more than 3 non-archived sites when m91 applied get no max_sites override from this file';
        RETURN;
    END IF;

`

// TestM142SkipsOutsideServerRunner_NoSchemaMigrationsTable proves m142's
// OUTSIDE THE SERVER'S RUNNER skip: applied the way atlas (or any runner
// that does not use schema_migrations) would — with that table never
// present at all — m142 raises its NOTICE and returns before touching a
// single row or lifting FORCE on sites, even though an over-cap
// pre-cutoff-shaped tenant exists that a real backfill would otherwise
// grandfather.
func TestM142SkipsOutsideServerRunner_NoSchemaMigrationsTable(t *testing.T) {
	admin, owner := startPostgresBeforeM142(t)
	ctx := context.Background()

	tenant := seedTenant(t, admin, "m142-no-tracking-table")
	for i := 0; i < 5; i++ {
		seedSiteAt(t, admin, tenant, fmt.Sprintf("https://m142-no-tracking-table-%d.example.com", i), time.Now().UTC().Add(-time.Hour))
	}

	// Simulate a runner that does not use schema_migrations at all (atlas
	// records its own applied versions in its own table): drop it. Nothing
	// else about the schema changes — every OTHER migration up to m142 is
	// already applied, from startPostgresBeforeM142's own bootstrap.
	if _, err := admin.Exec(ctx, `DROP TABLE schema_migrations`); err != nil {
		t.Fatalf("drop schema_migrations: %v", err)
	}

	// Call the file directly (never through owner.Migrate, which would
	// itself try to recreate schema_migrations as part of the server's own
	// runner — the exact thing this test needs to NOT be present).
	if _, err := owner.Exec(ctx, readM142Body(t)); err != nil {
		t.Fatalf("m142 body failed with no schema_migrations table present, want a clean no-op: %v", err)
	}

	if _, present := planOverridesMaxSites(t, admin, tenant); present {
		t.Fatal("max_sites override present after m142 ran with no schema_migrations table, want none (should have skipped before touching anything)")
	}
	assertSitesForceIntact(t, admin)

	// Lock assertion: the skip path takes NO ACCESS EXCLUSIVE lock on sites.
	// schema_migrations is still absent here, so this re-runs the same skip
	// path a second time, inside its own rolled-back transaction — no side
	// effects carry forward.
	if n := m142AccessExclusiveLockCount(t, owner, readM142Body(t)); n != 0 {
		t.Fatalf("m142's no-schema_migrations-table skip took %d ACCESS EXCLUSIVE lock(s) on sites, want 0", n)
	}
	t.Logf("real m142 correctly took 0 ACCESS EXCLUSIVE locks on sites on the no-table skip path")

	// Restore proof: with schema_migrations back and m91's applied_at row
	// present — the state startPostgresBeforeM142 actually left it in — the
	// REAL repair (called the same direct way) runs and grandfathers the
	// seeded tenant. Recreated as OWNER (wpmgr_owner), exactly as the
	// server's own runner creates it (internal/db/migrate.go): recreating it
	// as admin, the bootstrap superuser, would leave the table owned by a
	// role m142 itself has no privilege on.
	if _, err := owner.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text        PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		t.Fatalf("recreate schema_migrations: %v", err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT (version) DO NOTHING`,
		m91MigrationVersion); err != nil {
		t.Fatalf("re-seed m91's applied_at row: %v", err)
	}
	if _, err := owner.Exec(ctx, readM142Body(t)); err != nil {
		t.Fatalf("m142 body failed with schema_migrations present: %v", err)
	}
	if value, present := planOverridesMaxSites(t, admin, tenant); !present || value != 5 {
		t.Fatalf("after restoring schema_migrations and m91's row: max_sites present=%v value=%d, want present=true value=5", present, value)
	}
	assertSitesForceIntact(t, admin)
}

// TestM142SkipsOutsideServerRunner_NoM91Row is
// TestM142SkipsOutsideServerRunner_NoSchemaMigrationsTable's sibling for the
// OTHER outside-the-runner skip: schema_migrations exists — every other
// migration up to m142 applied normally, through startPostgresBeforeM142's
// own bootstrap — but carries no row for m91, the shape atlas's own
// out-of-order refusal and non-linear exec-order leave behind. m142 raises
// its NOTICE and returns before touching a row or taking a lock on sites,
// even though an over-cap pre-cutoff-shaped tenant exists that a real
// backfill would otherwise grandfather.
//
// Also the fires proof for the "no m91 row" RETURN block itself
// (m142NoM91RowSkipBlock): an in-memory copy with that block deleted
// proceeds past the skip — v_cutoff stays NULL, which the ALTER TABLE
// statement that follows doesn't depend on — and DOES take an ACCESS
// EXCLUSIVE lock on sites, proving the "no lock taken" assertion below is
// load-bearing rather than vacuous.
func TestM142SkipsOutsideServerRunner_NoM91Row(t *testing.T) {
	admin, owner := startPostgresBeforeM142(t)
	ctx := context.Background()

	tenant := seedTenant(t, admin, "m142-no-m91-row")
	for i := 0; i < 5; i++ {
		seedSiteAt(t, admin, tenant, fmt.Sprintf("https://m142-no-m91-row-%d.example.com", i), time.Now().UTC().Add(-time.Hour))
	}

	// schema_migrations exists (startPostgresBeforeM142's own bootstrap
	// applied m91 for real, through the owner role, and inserted its row)
	// but drop JUST m91's row: the "table present, no m91 row" skip.
	if _, err := admin.Exec(ctx,
		`DELETE FROM schema_migrations WHERE version = $1`, m91MigrationVersion); err != nil {
		t.Fatalf("delete m91's schema_migrations row: %v", err)
	}

	if _, err := owner.Exec(ctx, readM142Body(t)); err != nil {
		t.Fatalf("m142 body failed with schema_migrations present but no m91 row, want a clean no-op: %v", err)
	}

	if _, present := planOverridesMaxSites(t, admin, tenant); present {
		t.Fatal("max_sites override present after m142 ran with no m91 row, want none (should have skipped before touching anything)")
	}
	assertSitesForceIntact(t, admin)

	// DOES NOT OVER-FIRE: the real skip path takes NO ACCESS EXCLUSIVE lock
	// on sites.
	if n := m142AccessExclusiveLockCount(t, owner, readM142Body(t)); n != 0 {
		t.Fatalf("m142's no-m91-row skip took %d ACCESS EXCLUSIVE lock(s) on sites, want 0", n)
	}
	t.Logf("real m142 correctly took 0 ACCESS EXCLUSIVE locks on sites on the no-m91-row skip path")

	// FIRES: an in-memory copy of m142 with the "no m91 row" RETURN block
	// deleted proceeds past the skip and DOES take an ACCESS EXCLUSIVE lock
	// on sites.
	mutated := stripOnce(t, readM142Body(t), m142NoM91RowSkipBlock)
	if n := m142AccessExclusiveLockCount(t, owner, mutated); n == 0 {
		t.Fatal("block-stripped m142 (no-m91-row RETURN removed) took 0 ACCESS EXCLUSIVE locks; expected it to proceed past the skip and lock sites (this mutation must fire for the proof above to mean anything)")
	} else {
		t.Logf("block-stripped m142 correctly took %d ACCESS EXCLUSIVE lock(s) on sites (guard-is-load-bearing)", n)
	}

	// Restore proof: with m91's row back — the state startPostgresBeforeM142
	// actually left it in — the REAL repair runs and grandfathers the
	// seeded tenant.
	if _, err := admin.Exec(ctx,
		`INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT (version) DO NOTHING`,
		m91MigrationVersion); err != nil {
		t.Fatalf("re-seed m91's applied_at row: %v", err)
	}
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("m142 migration: %v", err)
	}
	if value, present := planOverridesMaxSites(t, admin, tenant); !present || value != 5 {
		t.Fatalf("after restoring m91's row: max_sites present=%v value=%d, want present=true value=5", present, value)
	}
	assertSitesForceIntact(t, admin)
}
