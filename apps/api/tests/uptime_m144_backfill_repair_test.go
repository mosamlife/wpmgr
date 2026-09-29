// uptime_m144_backfill_repair_test.go — m144's own regression coverage. m144
// (20260801120000_m144_uptime_rollup_backfill_repair, sorts between m99 and
// m100) repairs the rollup rows m99's own backfill silently failed to write
// under the production migrator role (see uptime_rollup_backfill_test.go's
// now-fixed KNOWN GAP): it re-aggregates site_uptime_probes directly, under a
// NO FORCE / row_security='off' toggle it lifts and restores itself, for
// every (site, UTC day) up to and including the day m99 applied.
//
// Covers, each against the exact production code path and role:
//   - the late-run backfill itself: pre-cutover days match an independent
//     GROUP BY, the cutover day merges with whatever the probe worker already
//     wrote for it, a later day is untouched, app_up_checks/app_total_checks
//     (unrelated to this repair) are never read or written, and re-applying
//     m144 a second time changes nothing;
//   - the monotone guard: a row that already holds MORE checks than a fresh
//     raw aggregate (the GC-pruned case) is never replaced;
//   - the lock bound: m144's own 5s lock_timeout, not an open-ended wait,
//     bounds a run contending for ACCESS EXCLUSIVE on site_uptime_daily;
//   - the historical-state defect this migration exists to fix: with m144
//     recorded applied but never actually run, the pre-cutover days it would
//     have repaired stay missing;
//   - FORCE ROW LEVEL SECURITY intact on all three tables after every run.
//
// Each mutation below is an IN-MEMORY copy of m144's SQL (via migrations.FS)
// with one specific guard stripped — never the committed file, which is
// database-engineer's territory (see CLAUDE.md's routing table).
package tests

import (
	"context"
	"errors"
	"io/fs"
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
	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

// m144MigrationVersion is the embedded migration filename (sans .sql) that
// repairs the rollup rows m99's backfill silently failed to write under the
// production migrator role.
const m144MigrationVersion = "20260801120000_m144_uptime_rollup_backfill_repair"

// startPostgresBeforeM144 mirrors startPostgresBeforeM99's container
// bootstrap (uptime_rollup_backfill_test.go) but stops short of m144 instead
// of m99 — i.e. it applies m99 for real (its backfill still inserts nothing,
// exactly as production saw) plus everything else up to, not including, m144.
// Migrations run AS wpmgr_owner, the NOSUPERUSER NOBYPASSRLS role production's
// migrator uses. Returns (admin, owner): seed and assert through admin, the
// bootstrap superuser; call owner.Migrate(ctx) / owner.Exec to run m144 under
// the production migrator role.
func startPostgresBeforeM144(t *testing.T) (admin *db.Pool, owner *db.Pool) {
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

	applyMigrationsBeforeM144(t, owner, m144MigrationVersion)
	return admin, owner
}

// applyMigrationsBeforeM144 is a package-local copy of
// uptime_rollup_backfill_test.go's applyMigrationsBeforeM99 (same
// re-implementation of internal/db/migrate.go's embedded-FS walk, kept local
// per that file's own stated convention so this file has no compile-order
// dependency on it).
func applyMigrationsBeforeM144(t *testing.T, pool *db.Pool, stopAt string) {
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
	files := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		version := strings.TrimSuffix(name, ".sql")
		versions = append(versions, version)
		files[version] = name
	}
	sort.Strings(versions)

	found := false
	for _, version := range versions {
		if version == stopAt {
			found = true
			break
		}
		body, err := fs.ReadFile(migrations.FS, files[version])
		if err != nil {
			t.Fatalf("read migration %s: %v", version, err)
		}
		err = pgx.BeginFunc(ctx, pool.Pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version)
			return err
		})
		if err != nil {
			t.Fatalf("apply migration %s: %v", version, err)
		}
	}
	if !found {
		t.Fatalf("stopAt version %q not found among embedded migrations (renamed/removed?)", stopAt)
	}
}

// readM144Body returns m144's own, UNMUTATED SQL text from the embedded FS.
func readM144Body(t *testing.T) string {
	t.Helper()
	body, err := fs.ReadFile(migrations.FS, m144MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m144 migration body: %v", err)
	}
	return string(body)
}

// m144's own mutation targets, quoted verbatim from the committed file so
// stripOnce's positive control (it fails the test if the text is not found)
// catches drift if the migration's wording ever changes.
const (
	// m144LockTimeoutLine is the top-level SET LOCAL that bounds how long
	// m144 waits for its ACCESS EXCLUSIVE locks. Dropping it turns a
	// contended run into an open-ended hang instead of a clean SQLSTATE
	// 55P03 abort.
	m144LockTimeoutLine = "SET LOCAL lock_timeout = '5s';\n"

	// m144StatusNoForceLine lifts FORCE on site_uptime_status specifically.
	// Dropping just this one (leaving its restoring ALTER at the end in
	// place, which is a harmless no-op on a table never taken out of FORCE)
	// leaves the status INSERT running under row_security='off' with FORCE
	// still on, which Postgres refuses outright rather than silently
	// filtering.
	m144StatusNoForceLine = `    ALTER TABLE "public"."site_uptime_status" NO FORCE ROW LEVEL SECURITY;` + "\n"

	// m144MonotoneGuardClause is the WHERE clause (without its trailing
	// semicolon, which stays attached to the statement once this is
	// stripped) that refuses to replace a site_uptime_daily row unless the
	// fresh raw aggregate holds strictly more checks than it already does.
	m144MonotoneGuardClause = "\n    WHERE EXCLUDED.\"total_checks\" > \"site_uptime_daily\".\"total_checks\""

	// m144DailyBoundClause is the WHERE clause (its trailing newline stays
	// attached to GROUP BY once this is stripped) that keeps the
	// site_uptime_daily repair inside the pre-cutover window. Dropping it
	// alone (leaving the ON CONFLICT monotone guard in place) still lets a
	// day AFTER the one m99 was applied get overwritten whenever its raw
	// count happens to exceed its existing rollup — the monotone guard says
	// nothing about which day a row belongs to.
	m144DailyBoundClause = "    WHERE \"probed_at\" < v_bound\n"

	// m144StatusCutoffClause is the WHERE clause (its trailing newline stays
	// attached to ORDER BY once this is stripped) that keeps the
	// site_uptime_status seed to probes from before m99 was applied.
	m144StatusCutoffClause = "    WHERE \"probed_at\" < v_cutoff\n"

	// m144NoM99RowSkipBlock is the IF v_cutoff IS NULL ... END IF; block
	// (plus its trailing blank line) that skips the whole repair — never
	// lifting FORCE or taking a lock on any of the three tables — when
	// schema_migrations exists but carries no row for m99. Deleting it is
	// the fires proof that TestM144SkipsOutsideServerRunner_NoM99Row's "no
	// ACCESS EXCLUSIVE lock taken" assertion is load-bearing: without it,
	// v_cutoff stays NULL, but the ALTER TABLE statements below run anyway.
	m144NoM99RowSkipBlock = `    IF v_cutoff IS NULL THEN
        RAISE NOTICE 'm144: repair skipped: schema_migrations has no row for 20260801000000_m99_uptime_rollup, so m99 was not applied by the server''s migration runner and its apply time is unknown; days before m99 are not rebuilt';
        RETURN;
    END IF;

`

	// m144EarliestProbeTenantOrderBy is the array_agg ORDER BY (its closing
	// ")[1]" stays attached once this is replaced) that stamps a mixed-tenant
	// day's row with the tenant of its EARLIEST probe — the same tenant
	// metrics.pgStore.UpsertRollup would itself have stamped had it run that
	// day.
	m144EarliestProbeTenantOrderBy = `"probed_at", "id"`

	// m144DailyGroupByClause is m144's own (site_id, day)-only grouping.
	// Replacing it with m99's own three-column grouping (which also groups
	// by tenant_id — see m99's migration) makes a mixed-tenant day propose
	// the same (site_id, day) conflict target twice within one INSERT.
	m144DailyGroupByClause = `GROUP BY "site_id", (("probed_at" AT TIME ZONE 'UTC')::date)`
)

// replaceOnce replaces exactly one occurrence of needle with replacement in
// body and fails the test if needle was not found — replaceOnce's own
// positive control, mirroring stripOnce's: if a later edit to the real
// migration's wording makes this silently match zero times, the fires proof
// would otherwise mutate nothing and pass for the wrong reason.
func replaceOnce(t *testing.T, body, needle, replacement string) string {
	t.Helper()
	if !strings.Contains(body, needle) {
		t.Fatalf("expected substring not found in migration body; the guard text likely changed — update this test's copy:\n%s", needle)
	}
	mutated := strings.Replace(body, needle, replacement, 1)
	if mutated == body {
		t.Fatalf("replacing the guard text made no change to the migration body")
	}
	return mutated
}

// assertUptimeRollupTablesForceIntact fails the test unless
// site_uptime_probes, site_uptime_daily and site_uptime_status all still
// carry both relrowsecurity and relforcerowsecurity: m144 lifts FORCE on all
// three for the length of the repair and must restore it before it returns,
// and it raises on its own if it doesn't (see its own final IF/RAISE
// EXCEPTION) — this is the test-side check on top of that, mirroring
// backup_m96_migration_test.go's assertBackupSnapshotsForceIntact.
func assertUptimeRollupTablesForceIntact(t *testing.T, pool *db.Pool) {
	t.Helper()
	ctx := context.Background()
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_class
		WHERE oid IN (
			'public.site_uptime_probes'::regclass,
			'public.site_uptime_daily'::regclass,
			'public.site_uptime_status'::regclass
		) AND relrowsecurity AND relforcerowsecurity`).Scan(&n); err != nil {
		t.Fatalf("read pg_class for the uptime rollup tables: %v", err)
	}
	if n != 3 {
		t.Fatalf("FORCE ROW LEVEL SECURITY intact on %d/3 of site_uptime_probes/site_uptime_daily/site_uptime_status, want 3", n)
	}
}

// m144AccessExclusiveLockCount runs body (m144's own SQL — the real,
// unmutated file, or an in-memory mutated copy) inside a fresh explicit
// transaction on pool, counts this backend's ACCESS EXCLUSIVE locks on
// site_uptime_probes/site_uptime_daily/site_uptime_status in pg_locks, then
// rolls the transaction back — nothing body does is ever committed — and
// returns the count. Mirrors migration_late_run_lock_test.go's own
// assertFileTakesNoLockOnRelation, generalized to ACCESS EXCLUSIVE mode
// specifically and to all three tables at once, since m144's skip path must
// take none of its own ALTER TABLE ... NO FORCE ROW LEVEL SECURITY locks
// (each of which is ACCESS EXCLUSIVE) rather than merely "no lock on one
// relation".
func m144AccessExclusiveLockCount(t *testing.T, pool *db.Pool, body string) int {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explicit tx to apply m144's body: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, body); err != nil {
		t.Fatalf("apply m144's body inside an explicit tx: %v", err)
	}
	var n int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM pg_locks
		WHERE pid = pg_backend_pid()
		  AND mode = 'AccessExclusiveLock'
		  AND relation IN (
		      'public.site_uptime_probes'::regclass,
		      'public.site_uptime_daily'::regclass,
		      'public.site_uptime_status'::regclass
		  )`).Scan(&n); err != nil {
		t.Fatalf("read pg_locks for the uptime rollup tables: %v", err)
	}
	return n
}

// siteUptimeDailyChecksum returns a deterministic hash of every
// site_uptime_daily row, used to prove a migration run touched no data.
func siteUptimeDailyChecksum(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var sum string
	if err := pool.QueryRow(context.Background(),
		`SELECT md5(coalesce(string_agg(row(d.*)::text, '|' ORDER BY d.site_id, d.day), '')) FROM site_uptime_daily d`,
	).Scan(&sum); err != nil {
		t.Fatalf("checksum site_uptime_daily: %v", err)
	}
	return sum
}

// siteUptimeStatusChecksum is siteUptimeDailyChecksum's sibling for
// site_uptime_status.
func siteUptimeStatusChecksum(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var sum string
	if err := pool.QueryRow(context.Background(),
		`SELECT md5(coalesce(string_agg(row(s.*)::text, '|' ORDER BY s.site_id), '')) FROM site_uptime_status s`,
	).Scan(&sum); err != nil {
		t.Fatalf("checksum site_uptime_status: %v", err)
	}
	return sum
}

// dailyExpect is the Go-computed GROUP BY reference a backfilled
// site_uptime_daily row must match exactly: same shape as
// uptime_rollup_backfill_test.go's dailyBucketExpectation, without the day
// field (callers key it by day themselves).
type dailyExpect struct {
	up, total, samples int64
	sumMs              float64
}

// computeDailyExpect independently reproduces m144's own per-check math
// (count(*) [FILTER (WHERE up)]; sum/count of total_ms for up AND
// total_ms<>0 checks only) over a Go-side list of probes, so the comparison
// is against an independent reference rather than the migration re-checking
// its own arithmetic.
func computeDailyExpect(probedUp []bool, totalMs []float64) dailyExpect {
	var e dailyExpect
	for i, up := range probedUp {
		e.total++
		if up {
			e.up++
			if totalMs[i] != 0 {
				e.samples++
				e.sumMs += totalMs[i]
			}
		}
	}
	return e
}

// ---------------------------------------------------------------------------
// 1. Late run: pre-cutover days, the cutover day, a later day, app_* columns,
//    idempotency.
// ---------------------------------------------------------------------------

// TestM144LateRun_BackfillsExactlyThePreM99Window seeds raw site_uptime_probes
// across three distinct pre-cutover days, reaches head with m144 withheld
// (m99 runs for real and, as production saw, inserts nothing), simulates the
// probe worker continuing to run — through metrics.pgStore.UpsertRollup, the
// SAME InAgentTx-scoped code path production uses — on the cutover day itself
// and on a later day, then lets m144 arrive late (unmark + Migrate again,
// mirroring m141/m143/m145's own late-run tests).
func TestM144LateRun_BackfillsExactlyThePreM99Window(t *testing.T) {
	pool, owner := startPostgresBeforeM99(t)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "m144-laterun")
	siteID := seedSiteFor(t, pool, tenant, "https://m144-laterun.example.com")

	// Truncated to the microsecond: see uptime_rollup_backfill_test.go's own
	// fix for why an untruncated time.Now() (nanosecond-precision on Linux)
	// cannot round-trip a Postgres timestamptz (microsecond-precision).
	now := time.Now().UTC().Truncate(time.Microsecond)
	today := now.Truncate(24 * time.Hour)
	dayMinus1 := today.Add(-24 * time.Hour)
	dayMinus2 := today.Add(-48 * time.Hour)
	dayMinus3 := today.Add(-72 * time.Hour)
	tomorrow := today.Add(24 * time.Hour)

	type probeSeed struct {
		probedAt time.Time
		up       bool
		totalMs  float64
	}
	preCutover := map[time.Time][]probeSeed{
		dayMinus3: {
			{dayMinus3.Add(9 * time.Hour), true, 100},
			{dayMinus3.Add(10 * time.Hour), false, 0},
		},
		dayMinus2: {
			{dayMinus2.Add(9 * time.Hour), true, 150},
			{dayMinus2.Add(10 * time.Hour), true, 0}, // zero-latency-while-up: excluded from the average.
			{dayMinus2.Add(11 * time.Hour), true, 250},
		},
		dayMinus1: {
			{dayMinus1.Add(9 * time.Hour), false, 0},
		},
	}
	// The cutover day's PRE-existing history, seeded now — minutes before
	// now, not a fixed hour-of-day: must land before whatever instant
	// owner.Migrate stamps as m99's applied_at moments from now (see
	// uptime_rollup_backfill_test.go's own fix for the same hazard).
	// anchorToToday keeps both on today's date even when the suite runs in
	// the six minutes after UTC midnight (uptime_rollup_backfill_test.go's
	// own fix for the same hazard).
	cutoverPre := []probeSeed{
		{anchorToToday(now.Add(-6*time.Minute), today), true, 120},
		{anchorToToday(now.Add(-5*time.Minute), today), true, 80},
	}

	seed := func(at time.Time, up bool, totalMs float64) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO site_uptime_probes (tenant_id, site_id, probed_at, up, total_ms) VALUES ($1, $2, $3, $4, $5)`,
			tenant, siteID, at, up, totalMs); err != nil {
			t.Fatalf("seed probe at %v: %v", at, err)
		}
	}
	for _, day := range []time.Time{dayMinus3, dayMinus2, dayMinus1} {
		for _, s := range preCutover[day] {
			seed(s.probedAt, s.up, s.totalMs)
		}
	}
	for _, s := range cutoverPre {
		seed(s.probedAt, s.up, s.totalMs)
	}

	expectFor := func(rows []probeSeed) dailyExpect {
		ups := make([]bool, len(rows))
		mss := make([]float64, len(rows))
		for i, r := range rows {
			ups[i], mss[i] = r.up, r.totalMs
		}
		return computeDailyExpect(ups, mss)
	}
	wantPreCutover := map[time.Time]dailyExpect{
		dayMinus3: expectFor(preCutover[dayMinus3]),
		dayMinus2: expectFor(preCutover[dayMinus2]),
		dayMinus1: expectFor(preCutover[dayMinus1]),
	}

	// Reach head with m144 withheld: m99 runs for real here (its backfill
	// inserts nothing, as production saw), and everything else through the
	// current head (including m107's app_up_checks/app_total_checks columns)
	// applies normally.
	scopePrefillMark(t, owner, m144MigrationVersion)
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate to head with m144 withheld: %v", err)
	}
	var dailyCountAfterM99 int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM site_uptime_daily WHERE site_id = $1`, siteID).Scan(&dailyCountAfterM99); err != nil {
		t.Fatalf("count site_uptime_daily after m99: %v", err)
	}
	if dailyCountAfterM99 != 0 {
		t.Fatalf("site_uptime_daily has %d row(s) right after m99, want 0 (m99's backfill is a silent no-op under the production migrator role)", dailyCountAfterM99)
	}

	// Simulate the probe worker continuing to run, through the SAME
	// InAgentTx-scoped path production uses. The cutover-day check carries an
	// app-probe verdict so app_up_checks/app_total_checks land non-zero — the
	// value m144 must leave untouched.
	store := metrics.NewPostgres(owner, nil)
	rw, ok := store.(metrics.RollupWriter)
	if !ok {
		t.Fatal("metrics.NewPostgres must return a RollupWriter")
	}
	appUp := true
	cutoverAgentCheckedAt := anchorToToday(now.Add(-1*time.Minute), today)
	cutoverAgentChecks := []metrics.Check{
		{TenantID: tenant, SiteID: siteID, CheckedAt: cutoverAgentCheckedAt, Up: true, TotalMs: 60, AppUp: &appUp, AppProbeReason: "ok"},
	}
	if err := store.InsertChecks(ctx, cutoverAgentChecks); err != nil {
		t.Fatalf("insert cutover-day agent checks: %v", err)
	}
	if err := rw.UpsertRollup(ctx, cutoverAgentChecks); err != nil {
		t.Fatalf("upsert cutover-day agent rollup: %v", err)
	}
	laterChecks := []metrics.Check{
		{TenantID: tenant, SiteID: siteID, CheckedAt: tomorrow.Add(9 * time.Hour), Up: true, TotalMs: 90},
		{TenantID: tenant, SiteID: siteID, CheckedAt: tomorrow.Add(10 * time.Hour), Up: false, TotalMs: 0},
	}
	if err := store.InsertChecks(ctx, laterChecks); err != nil {
		t.Fatalf("insert later-day agent checks: %v", err)
	}
	if err := rw.UpsertRollup(ctx, laterChecks); err != nil {
		t.Fatalf("upsert later-day agent rollup: %v", err)
	}

	// The cutover day's full raw aggregate now the worker has run: the
	// pre-existing history PLUS the one post-migration agent-simulated check
	// — m144 buckets by calendar day, not by the exact m99 applied_at
	// instant, so both land in the same day.
	cutoverAll := append(append([]probeSeed{}, cutoverPre...), probeSeed{cutoverAgentCheckedAt, true, 60})
	wantCutover := expectFor(cutoverAll)

	beforeCutoverUp, beforeCutoverTotal, _, _, foundCutover := queryDailyBucket(t, pool, siteID, today)
	if !foundCutover {
		t.Fatal("expected the agent's cutover-day rollup write to have created a row")
	}
	if beforeCutoverTotal >= wantCutover.total {
		t.Fatalf("test fixture bug: agent-only cutover total_checks=%d already >= the full raw day total %d; the replace-vs-keep distinction below would be meaningless",
			beforeCutoverTotal, wantCutover.total)
	}
	_ = beforeCutoverUp
	var beforeCutoverAppTotal int64
	if err := pool.QueryRow(ctx, `SELECT app_total_checks FROM site_uptime_daily WHERE site_id = $1 AND day = $2`, siteID, today).Scan(&beforeCutoverAppTotal); err != nil {
		t.Fatalf("query cutover-day app_total_checks before m144: %v", err)
	}
	if beforeCutoverAppTotal != 1 {
		t.Fatalf("cutover-day app_total_checks before m144 = %d, want 1 (one app-probe attempt seeded)", beforeCutoverAppTotal)
	}
	laterUp, laterTotal, laterSamples, laterSumMs, foundLater := queryDailyBucket(t, pool, siteID, tomorrow)
	if !foundLater {
		t.Fatal("expected the agent's later-day rollup write to have created a row")
	}

	// m144 arrives late: unmark and migrate again.
	scopePrefillUnmark(t, owner, m144MigrationVersion)
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("late m144 run: %v", err)
	}

	for day, want := range wantPreCutover {
		up, total, samples, sumMs, found := queryDailyBucket(t, pool, siteID, day)
		if !found {
			t.Fatalf("day=%v: expected m144 to have inserted a row, found none", day)
		}
		if up != want.up || total != want.total || samples != want.samples {
			t.Fatalf("day=%v: up=%d total=%d samples=%d, want up=%d total=%d samples=%d",
				day, up, total, samples, want.up, want.total, want.samples)
		}
		if sumMs != want.sumMs {
			t.Fatalf("day=%v: sum_latency_ms=%v, want %v", day, sumMs, want.sumMs)
		}
		var appTotal *int64
		if err := pool.QueryRow(ctx, `SELECT app_total_checks FROM site_uptime_daily WHERE site_id = $1 AND day = $2`, siteID, day).Scan(&appTotal); err != nil {
			t.Fatalf("day=%v: query app_total_checks: %v", day, err)
		}
		if appTotal != nil {
			t.Fatalf("day=%v: app_total_checks = %v on a brand-new row m144 inserted, want NULL (m144 never writes this column)", day, *appTotal)
		}
	}

	cutoverUp, cutoverTotal, cutoverSamples, cutoverSumMs, foundAfter := queryDailyBucket(t, pool, siteID, today)
	if !foundAfter {
		t.Fatal("cutover day row missing after m144")
	}
	if cutoverUp != wantCutover.up || cutoverTotal != wantCutover.total || cutoverSamples != wantCutover.samples {
		t.Fatalf("cutover day: up=%d total=%d samples=%d, want up=%d total=%d samples=%d",
			cutoverUp, cutoverTotal, cutoverSamples, wantCutover.up, wantCutover.total, wantCutover.samples)
	}
	if cutoverSumMs != wantCutover.sumMs {
		t.Fatalf("cutover day: sum_latency_ms=%v, want %v", cutoverSumMs, wantCutover.sumMs)
	}
	var afterCutoverAppTotal int64
	if err := pool.QueryRow(ctx, `SELECT app_total_checks FROM site_uptime_daily WHERE site_id = $1 AND day = $2`, siteID, today).Scan(&afterCutoverAppTotal); err != nil {
		t.Fatalf("query cutover-day app_total_checks after m144: %v", err)
	}
	if afterCutoverAppTotal != beforeCutoverAppTotal {
		t.Fatalf("cutover-day app_total_checks changed: before=%d after=%d, want unchanged (m144 never writes app_* columns)", beforeCutoverAppTotal, afterCutoverAppTotal)
	}

	afterLaterUp, afterLaterTotal, afterLaterSamples, afterLaterSumMs, foundLaterAfter := queryDailyBucket(t, pool, siteID, tomorrow)
	if !foundLaterAfter {
		t.Fatal("later day row disappeared after m144")
	}
	if afterLaterUp != laterUp || afterLaterTotal != laterTotal || afterLaterSamples != laterSamples || afterLaterSumMs != laterSumMs {
		t.Fatalf("later day changed by m144: before up=%d total=%d samples=%d sum=%v, after up=%d total=%d samples=%d sum=%v",
			laterUp, laterTotal, laterSamples, laterSumMs, afterLaterUp, afterLaterTotal, afterLaterSamples, afterLaterSumMs)
	}

	// Idempotent: re-apply m144's own SQL text a second time, directly
	// (bypassing schema_migrations — a genuine re-run, mirroring m99's own
	// idempotency check).
	beforeDaily := siteUptimeDailyChecksum(t, pool)
	beforeStatus := siteUptimeStatusChecksum(t, pool)
	if _, err := owner.Exec(ctx, readM144Body(t)); err != nil {
		t.Fatalf("re-apply m144 migration SQL: %v", err)
	}
	if after := siteUptimeDailyChecksum(t, pool); after != beforeDaily {
		t.Fatalf("m144's second run changed site_uptime_daily: before=%s after=%s", beforeDaily, after)
	}
	if after := siteUptimeStatusChecksum(t, pool); after != beforeStatus {
		t.Fatalf("m144's second run changed site_uptime_status: before=%s after=%s", beforeStatus, after)
	}

	assertUptimeRollupTablesForceIntact(t, pool)
}

// ---------------------------------------------------------------------------
// 2. Guard: a row with MORE checks than raw (the GC-pruned case) survives.
// ---------------------------------------------------------------------------

// TestM144GuardDoesNotReplaceRowWithMoreChecksThanRaw is the guard test: a
// site_uptime_daily row that already holds MORE checks than a fresh GROUP BY
// over site_uptime_probes finds for that day — exactly what retention GC
// having pruned some of that day's raw probes out from under an
// otherwise-correct rollup looks like — must not be replaced. Also the fires
// proof for the monotone guard clause itself: an in-memory copy of m144 with
// that WHERE dropped DOES overwrite the same row.
func TestM144GuardDoesNotReplaceRowWithMoreChecksThanRaw(t *testing.T) {
	pool, owner := startPostgresBeforeM144(t)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "m144-guard")
	siteID := seedSiteFor(t, pool, tenant, "https://m144-guard.example.com")

	today := time.Now().UTC().Truncate(24 * time.Hour)
	for _, at := range []time.Time{today.Add(1 * time.Hour), today.Add(2 * time.Hour)} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO site_uptime_probes (tenant_id, site_id, probed_at, up, total_ms) VALUES ($1, $2, $3, true, 50)`,
			tenant, siteID, at); err != nil {
			t.Fatalf("seed raw probe: %v", err)
		}
	}
	// Raw GROUP BY for today is now 2/2. Plant a GC-pruned-shaped row: MORE
	// checks than the raw probes currently show.
	const plantedTotal = 999
	if _, err := pool.Exec(ctx,
		`INSERT INTO site_uptime_daily (tenant_id, site_id, day, up_checks, total_checks, sum_latency_ms, latency_samples)
		 VALUES ($1, $2, $3, 900, $4, 45000, 900)`,
		tenant, siteID, today, plantedTotal); err != nil {
		t.Fatalf("plant GC-pruned row: %v", err)
	}

	// FIRES: an in-memory copy of m144 with the monotone WHERE stripped WOULD
	// overwrite the planted row with the (smaller) raw count.
	mutated := stripOnce(t, readM144Body(t), m144MonotoneGuardClause)
	if _, err := owner.Exec(ctx, mutated); err != nil {
		t.Fatalf("apply guard-stripped m144: %v", err)
	}
	var mutatedTotal int64
	if err := pool.QueryRow(ctx, `SELECT total_checks FROM site_uptime_daily WHERE site_id = $1 AND day = $2`, siteID, today).Scan(&mutatedTotal); err != nil {
		t.Fatalf("query total_checks after guard-stripped run: %v", err)
	}
	if mutatedTotal != 2 {
		t.Fatalf("guard-stripped m144 left total_checks=%d, want 2 (it must overwrite the planted row for this to be a real proof)", mutatedTotal)
	}
	t.Logf("guard-stripped m144 correctly overwrote the planted row (guard-is-load-bearing)")

	// Restore the planted state and run the REAL file: the row must survive
	// unchanged.
	if _, err := pool.Exec(ctx,
		`UPDATE site_uptime_daily SET up_checks = 900, total_checks = $3, sum_latency_ms = 45000, latency_samples = 900
		 WHERE site_id = $1 AND day = $2`,
		siteID, today, plantedTotal); err != nil {
		t.Fatalf("restore planted row: %v", err)
	}
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("m144 migration: %v", err)
	}
	var up, total, samples int64
	var sumMs float64
	if err := pool.QueryRow(ctx,
		`SELECT up_checks, total_checks, sum_latency_ms, latency_samples FROM site_uptime_daily WHERE site_id = $1 AND day = $2`,
		siteID, today).Scan(&up, &total, &sumMs, &samples); err != nil {
		t.Fatalf("query planted row after real m144: %v", err)
	}
	if up != 900 || total != plantedTotal || sumMs != 45000 || samples != 900 {
		t.Fatalf("real m144 replaced the GC-pruned-shaped row: up=%d total=%d sum=%v samples=%d, want unchanged 900/%d/45000/900",
			up, total, sumMs, samples, plantedTotal)
	}

	assertUptimeRollupTablesForceIntact(t, pool)
}

// ---------------------------------------------------------------------------
// 3. Lock bound: m144's own 5s lock_timeout, not an open-ended wait.
// ---------------------------------------------------------------------------

// TestM144RealLockTimeoutBoundsRunWait is the lock-bound proof. Unlike
// m141/m143/m145 (which probe for convergence and take NO lock at all once
// their target column/index already exists), m144 does real work on every
// FIRST run regardless of when it runs: its own ALTER TABLE ... NO FORCE ROW
// LEVEL SECURITY statements always contend for ACCESS EXCLUSIVE on all three
// tables. There is no "converged, skips its own body" branch to test running
// late against, so this proves the bound in m144's natural (right-after-m99)
// position — mirroring TestM145RealLockTimeoutBoundsEarlyRunWait's own
// reasoning for the not-yet-converged case.
//
// A second connection holds ROW EXCLUSIVE on site_uptime_daily throughout.
// FIRES: an in-memory copy of m144 with its own SET LOCAL lock_timeout line
// dropped is run against the SAME hold, bounded by this harness's own 3s
// context — it must NOT produce the real file's SQLSTATE 55P03 inside that
// bound, proving the real line (not some other timeout) is what produces the
// clean abort. DOES NOT OVER-FIRE: the REAL, unmutated file — through
// owner.Migrate, the exact path and role production uses — aborts with
// SQLSTATE 55P03 in roughly five seconds, its own transaction fully rolled
// back (no partial rows, m144 not recorded applied, FORCE intact on all three
// tables), and a retry after the hold releases succeeds and backfills the
// seeded row.
func TestM144RealLockTimeoutBoundsRunWait(t *testing.T) {
	pool, owner := startPostgresBeforeM144(t)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "m144-lockwait")
	siteID := seedSiteFor(t, pool, tenant, "https://m144-lockwait.example.com")
	probedAt := time.Now().UTC().Add(-10 * time.Minute)
	if _, err := pool.Exec(ctx,
		`INSERT INTO site_uptime_probes (tenant_id, site_id, probed_at, up, total_ms) VALUES ($1, $2, $3, true, 40)`,
		tenant, siteID, probedAt); err != nil {
		t.Fatalf("seed raw probe: %v", err)
	}

	// FIRES: without its own lock_timeout, m144 would hang on the held
	// ROW EXCLUSIVE past any bound of its own. Proven against a SHORT
	// harness-side context: the mutated body must not produce SQLSTATE
	// 55P03 inside it.
	mutated := stripOnce(t, readM144Body(t), m144LockTimeoutLine)
	holdForMutant := holdRowExclusiveOpen(t, owner, "site_uptime_daily", "updated_at")
	mutCtx, mutCancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, mutErr := owner.Exec(mutCtx, mutated)
	mutCancel()
	holdForMutant()
	if mutErr == nil {
		t.Fatal("guard-stripped m144 (no lock_timeout) succeeded despite a concurrent ROW EXCLUSIVE holder; expected it to still be waiting when this harness's own 3s bound cut it off")
	}
	var mutPgErr *pgconn.PgError
	if errors.As(mutErr, &mutPgErr) && mutPgErr.Code == sqlStateLockNotAvailable {
		t.Fatalf("guard-stripped m144 (no lock_timeout) still failed with SQLSTATE %s inside 3s; expected it to just be hanging, not aborting cleanly (mutation may not have taken effect)", sqlStateLockNotAvailable)
	}
	t.Logf("guard-stripped m144 correctly did NOT produce a clean SQLSTATE %s inside 3s (still hanging on the hold, no lock_timeout of its own): %v", sqlStateLockNotAvailable, mutErr)

	// DOES NOT OVER-FIRE (real behavior): the real file, held against the
	// same lock shape, aborts cleanly via its OWN 5s lock_timeout.
	release := holdRowExclusiveOpen(t, owner, "site_uptime_daily", "updated_at")
	defer release()

	start := time.Now()
	err := owner.Migrate(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("Migrate succeeded in %s despite a concurrent ROW EXCLUSIVE holder on site_uptime_daily; m144's own lock_timeout should have aborted it", elapsed)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != sqlStateLockNotAvailable {
		t.Fatalf("Migrate failed after %s, but not with SQLSTATE %s (lock_not_available): %v", elapsed, sqlStateLockNotAvailable, err)
	}
	t.Logf("Migrate correctly aborted after %s with SQLSTATE %s (guard-is-load-bearing): %v", elapsed, pgErr.Code, err)

	const (
		wantMin = 4500 * time.Millisecond
		wantMax = 8 * time.Second
	)
	if elapsed < wantMin || elapsed > wantMax {
		t.Fatalf("Migrate aborted after %s, want roughly %s-%s (m144's own 5s lock_timeout)", elapsed, wantMin, wantMax)
	}

	// No partial rows: the transaction rolled back in full.
	var dailyCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM site_uptime_daily`).Scan(&dailyCount); err != nil {
		t.Fatalf("count site_uptime_daily: %v", err)
	}
	if dailyCount != 0 {
		t.Fatalf("site_uptime_daily has %d row(s) after the lock_timeout abort; m144's transaction should have rolled back entirely", dailyCount)
	}
	var m144Recorded bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, m144MigrationVersion,
	).Scan(&m144Recorded); err != nil {
		t.Fatalf("check schema_migrations: %v", err)
	}
	if m144Recorded {
		t.Fatalf("m144 is recorded applied in schema_migrations despite aborting on the lock_timeout")
	}
	assertUptimeRollupTablesForceIntact(t, pool)

	// Release and retry: must now succeed and backfill the seeded row.
	release()
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate after releasing the hold: %v", err)
	}
	up, total, _, _, found := queryDailyBucket(t, pool, siteID, probedAt.Truncate(24*time.Hour))
	if !found || up != 1 || total != 1 {
		t.Fatalf("after the retry: up=%d total=%d found=%v, want 1/1 backfilled", up, total, found)
	}
	assertUptimeRollupTablesForceIntact(t, pool)
}

// ---------------------------------------------------------------------------
// 4. FORCE lifted on the wrong subset fails outright, never silently
//    under-filters.
// ---------------------------------------------------------------------------

// TestM144MutatedStatusForceNotLifted_FailsUnderRLS is the fires proof for
// m144 lifting FORCE on site_uptime_status specifically, not just probes and
// daily: an in-memory copy with that ONE "NO FORCE ROW LEVEL SECURITY" line
// dropped (its matching restore ALTER at the end is left in place — a
// harmless no-op on a table never taken out of FORCE) must fail outright,
// under row_security='off' with FORCE still active on that one table, rather
// than silently filtering the status INSERT down to nothing.
func TestM144MutatedStatusForceNotLifted_FailsUnderRLS(t *testing.T) {
	pool, owner := startPostgresBeforeM144(t)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "m144-status-force")
	siteID := seedSiteFor(t, pool, tenant, "https://m144-status-force.example.com")
	if _, err := pool.Exec(ctx,
		`INSERT INTO site_uptime_probes (tenant_id, site_id, probed_at, up, total_ms) VALUES ($1, $2, now() - interval '1 hour', true, 30)`,
		tenant, siteID); err != nil {
		t.Fatalf("seed raw probe: %v", err)
	}

	mutated := stripOnce(t, readM144Body(t), m144StatusNoForceLine)
	_, err := owner.Exec(ctx, mutated)
	if err == nil {
		t.Fatal("guard-stripped m144 (site_uptime_status left under FORCE) succeeded; expected it to fail under row_security='off' with FORCE still active")
	}
	if !strings.Contains(err.Error(), "row-level security policy") {
		t.Fatalf("guard-stripped m144 failed, but not with the expected RLS error: %v", err)
	}
	t.Logf("guard-stripped m144 correctly failed (guard-is-load-bearing): %v", err)
	assertUptimeRollupTablesForceIntact(t, pool)

	// Restore proof: the REAL file succeeds on the same fixture.
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("real m144 migration: %v", err)
	}
	assertUptimeRollupTablesForceIntact(t, pool)
}

// ---------------------------------------------------------------------------
// 5. The defect this migration exists to fix, reproduced historically.
// ---------------------------------------------------------------------------

// TestM144FiresWhenNeverRun_PreCutoverDaysStayMissing reproduces the defect
// m144 exists to repair: with m144 recorded applied but never actually run —
// a stale binary, or a schema pre-marked from a partial apply — the
// pre-cutover day m99's own backfill silently dropped stays missing.
// Unmarking m144 and migrating again is the fix.
func TestM144FiresWhenNeverRun_PreCutoverDaysStayMissing(t *testing.T) {
	pool, owner := startPostgresBeforeM99(t)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "m144-fires")
	siteID := seedSiteFor(t, pool, tenant, "https://m144-fires.example.com")

	yesterday := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	if _, err := pool.Exec(ctx,
		`INSERT INTO site_uptime_probes (tenant_id, site_id, probed_at, up, total_ms) VALUES ($1, $2, $3, true, 70)`,
		tenant, siteID, yesterday.Add(9*time.Hour)); err != nil {
		t.Fatalf("seed pre-cutover raw probe: %v", err)
	}

	// The defect: m144 recorded applied, never actually run.
	scopePrefillMark(t, owner, m144MigrationVersion)
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate with m144 marked (never run): %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM site_uptime_daily WHERE site_id = $1 AND day = $2`, siteID, yesterday).Scan(&count); err != nil {
		t.Fatalf("count pre-cutover day: %v", err)
	}
	if count != 0 {
		t.Fatalf("pre-cutover day already has %d row(s) with m144 skipped; test fixture bug (this is supposed to reproduce the still-missing defect)", count)
	}
	t.Logf("confirmed the defect: pre-cutover day is still missing with m144 recorded applied but never run")

	// The fix: unmark and migrate again.
	scopePrefillUnmark(t, owner, m144MigrationVersion)
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate with m144 present: %v", err)
	}
	up, total, _, _, found := queryDailyBucket(t, pool, siteID, yesterday)
	if !found || up != 1 || total != 1 {
		t.Fatalf("after m144 runs: up=%d total=%d found=%v, want 1/1 backfilled", up, total, found)
	}
	assertUptimeRollupTablesForceIntact(t, pool)
}

// ---------------------------------------------------------------------------
// 6. Later day: m144's window bound, not just its ON CONFLICT monotone
//    guard, is what keeps it from touching days after the one m99 applied.
// ---------------------------------------------------------------------------

// TestM144LeavesLaterDayAlone_WithFewerRollupChecksThanRaw proves m144 never
// touches a day AFTER the one m99 was applied, even when that day's raw
// probe count is HIGHER than its existing rollup — the shape of an ordinary
// missed rollup sweep, not the GC-pruned shape
// TestM144GuardDoesNotReplaceRowWithMoreChecksThanRaw covers. Without
// m144DailyBoundClause, the ON CONFLICT monotone guard alone (fresh
// total_checks > existing total_checks) would happily "repair" this day
// too, which is wrong: it is not m144's window to touch at all.
func TestM144LeavesLaterDayAlone_WithFewerRollupChecksThanRaw(t *testing.T) {
	pool, owner := startPostgresBeforeM144(t)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "m144-laterday")
	siteID := seedSiteFor(t, pool, tenant, "https://m144-laterday.example.com")

	today := time.Now().UTC().Truncate(24 * time.Hour)
	later := today.Add(24 * time.Hour)

	for i := 0; i < 5; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO site_uptime_probes (tenant_id, site_id, probed_at, up, total_ms) VALUES ($1, $2, $3, true, 50)`,
			tenant, siteID, later.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatalf("seed later-day raw probe %d: %v", i, err)
		}
	}
	// A missed-rollup-shaped row: fewer checks (3) than the 5 raw probes
	// above, which is what a skipped sweep or two looks like — the raw
	// count here is intentionally the LARGER one, so this row is exactly
	// the shape the ON CONFLICT monotone guard alone would replace.
	plant := func() {
		if _, err := pool.Exec(ctx,
			`INSERT INTO site_uptime_daily (tenant_id, site_id, day, up_checks, total_checks, sum_latency_ms, latency_samples)
			 VALUES ($1, $2, $3, 3, 3, 150, 3)
			 ON CONFLICT (site_id, day) DO UPDATE SET
			   up_checks = 3, total_checks = 3, sum_latency_ms = 150, latency_samples = 3`,
			tenant, siteID, later); err != nil {
			t.Fatalf("plant later-day rollup: %v", err)
		}
	}
	plant()

	// FIRES: an in-memory copy of m144 with the daily window bound stripped
	// (its ON CONFLICT monotone guard left intact) still overwrites the
	// later day, because raw (5) > existing (3) satisfies that guard too.
	mutated := stripOnce(t, readM144Body(t), m144DailyBoundClause)
	if _, err := owner.Exec(ctx, mutated); err != nil {
		t.Fatalf("apply bound-stripped m144: %v", err)
	}
	if _, total, _, _, _ := queryDailyBucket(t, pool, siteID, later); total != 5 {
		t.Fatalf("bound-stripped m144 left later-day total_checks=%d, want 5 (it must overwrite the planted row for this to be a real proof)", total)
	}
	t.Logf("bound-stripped m144 correctly touched the later day (guard-is-load-bearing)")

	// Restore the planted state and run the REAL file: the later day must
	// survive unchanged.
	plant()
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("m144 migration: %v", err)
	}
	up, total, samples, sumMs, found := queryDailyBucket(t, pool, siteID, later)
	if !found {
		t.Fatal("later-day row disappeared after real m144")
	}
	if up != 3 || total != 3 || samples != 3 || sumMs != 150 {
		t.Fatalf("real m144 touched a day after its window: up=%d total=%d samples=%d sum=%v, want unchanged 3/3/150/3",
			up, total, samples, sumMs)
	}
	assertUptimeRollupTablesForceIntact(t, pool)
}

// ---------------------------------------------------------------------------
// 7. Status cutoff: a site probed only AFTER v_cutoff gets no status row.
// ---------------------------------------------------------------------------

// TestM144StatusCutoff_ProbesAfterCutoffGetNoStatusRow proves m144's status
// seed respects v_cutoff, not "any known probe": a site whose only raw probe
// was recorded strictly AFTER the instant m99 was applied gets no status row
// out of m144. Backfilling one would be wrong — that probe is the probe
// worker's own job to stamp, and m144 doing it here could race (or silently
// duplicate, since ON CONFLICT DO NOTHING never overwrites) whatever the
// worker writes for it.
func TestM144StatusCutoff_ProbesAfterCutoffGetNoStatusRow(t *testing.T) {
	pool, owner := startPostgresBeforeM144(t)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "m144-status-cutoff")
	siteID := seedSiteFor(t, pool, tenant, "https://m144-status-cutoff.example.com")

	afterCutoff := time.Now().UTC().Add(1 * time.Hour)
	if _, err := pool.Exec(ctx,
		`INSERT INTO site_uptime_probes (tenant_id, site_id, probed_at, up, total_ms) VALUES ($1, $2, $3, true, 40)`,
		tenant, siteID, afterCutoff); err != nil {
		t.Fatalf("seed after-cutoff raw probe: %v", err)
	}

	// FIRES: an in-memory copy of m144 with the status cutoff stripped seeds
	// a status row from this after-cutoff-only probe anyway.
	mutated := stripOnce(t, readM144Body(t), m144StatusCutoffClause)
	if _, err := owner.Exec(ctx, mutated); err != nil {
		t.Fatalf("apply cutoff-stripped m144: %v", err)
	}
	if _, _, found := queryStatusRow(t, pool, siteID); !found {
		t.Fatal("cutoff-stripped m144 left no status row; expected it to seed one from the after-cutoff probe (this mutation must fire for the proof below to mean anything)")
	}
	t.Logf("cutoff-stripped m144 correctly seeded a status row from an after-cutoff-only probe (guard-is-load-bearing)")

	// Clean the mutant's write and run the REAL file: no status row.
	if _, err := pool.Exec(ctx, `DELETE FROM site_uptime_status WHERE site_id = $1`, siteID); err != nil {
		t.Fatalf("clean up mutant status row: %v", err)
	}
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("m144 migration: %v", err)
	}
	if _, _, found := queryStatusRow(t, pool, siteID); found {
		t.Fatal("real m144 seeded a status row for a site whose only probe is after v_cutoff, want none")
	}
	assertUptimeRollupTablesForceIntact(t, pool)
}

// ---------------------------------------------------------------------------
// 8. Multi-tenant: one row per (site, day), stamped with the day's EARLIEST
//    probe's tenant, and never leaked across sites.
// ---------------------------------------------------------------------------

// TestM144MultiTenant_StampsEarliestProbesTenant_NeverLeaksAcrossSites covers
// a site transferred between tenants mid-day: its raw probes for one day
// carry tenant A early and tenant B late, as a live transfer would leave
// them. m144's own GROUP BY has no tenant_id in it (unlike m99's), so a
// mixed-tenant day still proposes exactly one (site_id, day) row, and the
// array_agg ORDER BY "probed_at", "id" picks that day's EARLIEST probe's
// tenant for it — the same tenant metrics.pgStore.UpsertRollup would have
// stamped had it run that day. A second, single-tenant site in tenant B
// alone must never see tenant A on its own row.
func TestM144MultiTenant_StampsEarliestProbesTenant_NeverLeaksAcrossSites(t *testing.T) {
	pool, owner := startPostgresBeforeM144(t)
	ctx := context.Background()

	tenantA := seedTenant(t, pool, "m144-multitenant-a")
	tenantB := seedTenant(t, pool, "m144-multitenant-b")
	// earlyTenant is guaranteed NOT to be the lexically lower of the two
	// UUIDs, so "stamp every row with the lowest tenant id" (the first
	// planted mutation below) is guaranteed to disagree with the correct
	// answer instead of passing by a coin-flip on how uuid.New() happened to
	// order tenantA/tenantB.
	earlyTenant, lateTenant := tenantA, tenantB
	if earlyTenant.String() < lateTenant.String() {
		earlyTenant, lateTenant = lateTenant, earlyTenant
	}

	transferredSite := seedSiteFor(t, pool, lateTenant, "https://m144-transferred.example.com")
	tenantBOnlySite := seedSiteFor(t, pool, lateTenant, "https://m144-tenant-b-only.example.com")

	yesterday := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)

	seedProbe := func(tenant, siteID uuid.UUID, at time.Time) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO site_uptime_probes (tenant_id, site_id, probed_at, up, total_ms) VALUES ($1, $2, $3, true, 50)`,
			tenant, siteID, at); err != nil {
			t.Fatalf("seed probe: %v", err)
		}
	}
	// transferredSite, yesterday: 2 checks under earlyTenant, then 3 under
	// lateTenant — as if the site moved tenants partway through the day.
	seedProbe(earlyTenant, transferredSite, yesterday.Add(1*time.Hour))
	seedProbe(earlyTenant, transferredSite, yesterday.Add(2*time.Hour))
	seedProbe(lateTenant, transferredSite, yesterday.Add(3*time.Hour))
	seedProbe(lateTenant, transferredSite, yesterday.Add(4*time.Hour))
	seedProbe(lateTenant, transferredSite, yesterday.Add(5*time.Hour))
	// tenantBOnlySite, yesterday: lateTenant only.
	seedProbe(lateTenant, tenantBOnlySite, yesterday.Add(1*time.Hour))
	seedProbe(lateTenant, tenantBOnlySite, yesterday.Add(2*time.Hour))

	queryDailyTenant := func(siteID uuid.UUID) (tenant uuid.UUID, total int64, found bool) {
		t.Helper()
		err := pool.QueryRow(ctx,
			`SELECT tenant_id, total_checks FROM site_uptime_daily WHERE site_id = $1 AND day = $2`,
			siteID, yesterday).Scan(&tenant, &total)
		if err != nil {
			if err == pgx.ErrNoRows {
				return uuid.Nil, 0, false
			}
			t.Fatalf("query daily tenant for %s: %v", siteID, err)
		}
		return tenant, total, true
	}

	// FIRES #1: an in-memory copy of m144 that picks the LOWEST tenant_id
	// per group instead of the earliest probe's disagrees with the correct
	// answer on the transferred site (guaranteed by the earlyTenant/
	// lateTenant ordering above).
	mutatedLowest := replaceOnce(t, readM144Body(t), m144EarliestProbeTenantOrderBy, `"tenant_id"`)
	if _, err := owner.Exec(ctx, mutatedLowest); err != nil {
		t.Fatalf("apply lowest-tenant-id m144: %v", err)
	}
	if gotTenant, _, found := queryDailyTenant(transferredSite); !found || gotTenant != lateTenant {
		t.Fatalf("lowest-tenant-id m144 produced tenant=%v found=%v, want lateTenant=%v (it must disagree with the correct answer for this to be a real proof)",
			gotTenant, found, lateTenant)
	}
	t.Logf("lowest-tenant-id m144 correctly stamped the wrong tenant (guard-is-load-bearing)")
	if _, err := pool.Exec(ctx,
		`DELETE FROM site_uptime_daily WHERE site_id IN ($1, $2) AND day = $3`,
		transferredSite, tenantBOnlySite, yesterday); err != nil {
		t.Fatalf("clean up mutant daily rows: %v", err)
	}

	// FIRES #2: an in-memory copy of m144 grouped like m99 (also by
	// tenant_id) proposes the transferred site's day twice within one
	// INSERT, which ON CONFLICT DO UPDATE refuses outright rather than
	// picking one silently.
	mutatedGrouped := replaceOnce(t, readM144Body(t), m144DailyGroupByClause,
		`GROUP BY "tenant_id", "site_id", (("probed_at" AT TIME ZONE 'UTC')::date)`)
	_, groupErr := owner.Exec(ctx, mutatedGrouped)
	if groupErr == nil {
		t.Fatal("tenant-grouped m144 succeeded on a mixed-tenant day; expected the ON CONFLICT DO UPDATE double-affect error")
	}
	const wantConflictErr = "ON CONFLICT DO UPDATE command cannot affect row a second time"
	if !strings.Contains(groupErr.Error(), wantConflictErr) {
		t.Fatalf("tenant-grouped m144 failed, but not with the expected ON CONFLICT error (want %q): %v", wantConflictErr, groupErr)
	}
	t.Logf("tenant-grouped m144 correctly hit the ON CONFLICT double-affect error (guard-is-load-bearing): %v", groupErr)

	// Restore proof: the REAL file gives exactly one row per (site, day),
	// stamped with the EARLIEST probe's tenant, and never leaks across
	// sites.
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("m144 migration: %v", err)
	}
	gotTenant, total, found := queryDailyTenant(transferredSite)
	if !found {
		t.Fatal("transferred site's day row missing after m144")
	}
	if total != 5 {
		t.Fatalf("transferred site: total_checks=%d, want 5 (one row covering both tenants' probes)", total)
	}
	if gotTenant != earlyTenant {
		t.Fatalf("transferred site: tenant_id=%v, want earlyTenant=%v (the day's earliest probe's tenant)", gotTenant, earlyTenant)
	}
	otherTenant, otherTotal, otherFound := queryDailyTenant(tenantBOnlySite)
	if !otherFound {
		t.Fatal("tenant-B-only site's day row missing after m144")
	}
	if otherTotal != 2 {
		t.Fatalf("tenant-B-only site: total_checks=%d, want 2", otherTotal)
	}
	if otherTenant != lateTenant {
		t.Fatalf("tenant-B-only site: tenant_id=%v, want lateTenant=%v, and specifically must never be earlyTenant=%v (a single-tenant site must never pick up another site's tenant)",
			otherTenant, lateTenant, earlyTenant)
	}
	assertUptimeRollupTablesForceIntact(t, pool)
}

// ---------------------------------------------------------------------------
// 9. Skip path: no schema_migrations table at all, m144 changes nothing.
// ---------------------------------------------------------------------------

// TestM144SkipsOutsideServerRunner_NoSchemaMigrationsTable proves m144's
// OUTSIDE THE SERVER'S RUNNER skip: applied the way atlas (or any runner
// that does not use schema_migrations) would — with that table never
// present at all — m144 raises its NOTICE and returns before touching a
// single row or lifting FORCE on any of the three tables, even though raw
// probes exist that a real repair would otherwise aggregate.
func TestM144SkipsOutsideServerRunner_NoSchemaMigrationsTable(t *testing.T) {
	admin, owner := startPostgresBeforeM144(t)
	ctx := context.Background()

	tenant := seedTenant(t, admin, "m144-no-tracking-table")
	siteID := seedSiteFor(t, admin, tenant, "https://m144-no-tracking-table.example.com")

	// Truncated to the microsecond (see uptime_rollup_backfill_test.go's own
	// fix for why an untruncated time.Now() cannot round-trip a Postgres
	// timestamptz) and anchored to today with anchorToToday, so a probe
	// seeded as "SQL now() minus an hour" and the Go-computed day bucket
	// asserted on below agree even when the suite runs in the hour after UTC
	// midnight — the same hazard uptime_rollup_backfill_test.go's own fix
	// covers, and which a bare `now() - interval '1 hour'` seed compared
	// against a fresh time.Now().Truncate(24h) does not.
	now := time.Now().UTC().Truncate(time.Microsecond)
	today := now.Truncate(24 * time.Hour)
	probedAt := anchorToToday(now.Add(-1*time.Hour), today)
	if _, err := admin.Exec(ctx,
		`INSERT INTO site_uptime_probes (tenant_id, site_id, probed_at, up, total_ms) VALUES ($1, $2, $3, true, 30)`,
		tenant, siteID, probedAt); err != nil {
		t.Fatalf("seed raw probe: %v", err)
	}

	// Simulate a runner that does not use schema_migrations at all (atlas
	// records its own applied versions in its own table): drop it. Nothing
	// else about the schema changes — every OTHER migration up to m144 is
	// already applied, from startPostgresBeforeM144's own bootstrap.
	if _, err := admin.Exec(ctx, `DROP TABLE schema_migrations`); err != nil {
		t.Fatalf("drop schema_migrations: %v", err)
	}

	// Call the file directly (never through owner.Migrate, which would
	// itself try to recreate schema_migrations as part of the server's own
	// runner — the exact thing this test needs to NOT be present).
	if _, err := owner.Exec(ctx, readM144Body(t)); err != nil {
		t.Fatalf("m144 body failed with no schema_migrations table present, want a clean no-op: %v", err)
	}

	var dailyCount, statusCount int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM site_uptime_daily WHERE site_id = $1`, siteID).Scan(&dailyCount); err != nil {
		t.Fatalf("count site_uptime_daily: %v", err)
	}
	if dailyCount != 0 {
		t.Fatalf("site_uptime_daily has %d row(s) after m144 ran with no schema_migrations table, want 0 (should have skipped before touching anything)", dailyCount)
	}
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM site_uptime_status WHERE site_id = $1`, siteID).Scan(&statusCount); err != nil {
		t.Fatalf("count site_uptime_status: %v", err)
	}
	if statusCount != 0 {
		t.Fatalf("site_uptime_status has %d row(s) after m144 ran with no schema_migrations table, want 0", statusCount)
	}
	assertUptimeRollupTablesForceIntact(t, admin)

	// Lock assertion: the skip path takes NO ACCESS EXCLUSIVE lock on any of
	// the three tables (mirrors migration_late_run_lock_test.go's own
	// pg_locks-based proofs). schema_migrations is still absent here, so
	// this re-runs the same skip path a second time, inside its own
	// rolled-back transaction — no side effects carry forward.
	if n := m144AccessExclusiveLockCount(t, owner, readM144Body(t)); n != 0 {
		t.Fatalf("m144's no-schema_migrations-table skip took %d ACCESS EXCLUSIVE lock(s) on the uptime rollup tables, want 0", n)
	}
	t.Logf("real m144 correctly took 0 ACCESS EXCLUSIVE locks on the uptime rollup tables on the no-table skip path")

	// Restore proof: with schema_migrations back and m99's applied_at row
	// present — the state startPostgresBeforeM144 actually left it in — the
	// REAL repair (called the same direct way) runs and backfills the
	// seeded probe. Recreated as OWNER (wpmgr_owner), exactly as the
	// server's own runner creates it (internal/db/migrate.go): recreating it
	// as admin, the bootstrap superuser, would leave the table owned by a
	// role m144 itself has no privilege on, and the repair below would fail
	// with "permission denied for table schema_migrations" the moment it
	// tries to SELECT applied_at from it as wpmgr_owner.
	if _, err := owner.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text        PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		t.Fatalf("recreate schema_migrations: %v", err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT (version) DO NOTHING`,
		m99MigrationVersion); err != nil {
		t.Fatalf("re-seed m99's applied_at row: %v", err)
	}
	if _, err := owner.Exec(ctx, readM144Body(t)); err != nil {
		t.Fatalf("m144 body failed with schema_migrations present: %v", err)
	}
	up, total, _, _, found := queryDailyBucket(t, admin, siteID, today)
	if !found || up != 1 || total != 1 {
		t.Fatalf("after restoring schema_migrations and m99's row: up=%d total=%d found=%v, want 1/1 backfilled", up, total, found)
	}
	assertUptimeRollupTablesForceIntact(t, admin)
}

// ---------------------------------------------------------------------------
// 10. Skip path: schema_migrations exists but has no m99 row, m144 changes
//     nothing and takes NO lock.
// ---------------------------------------------------------------------------

// TestM144SkipsOutsideServerRunner_NoM99Row is
// TestM144SkipsOutsideServerRunner_NoSchemaMigrationsTable's sibling for the
// OTHER outside-the-runner skip: schema_migrations exists — every other
// migration up to m144 applied normally, through startPostgresBeforeM144's
// own bootstrap — but carries no row for m99, the shape atlas's own
// out-of-order refusal and non-linear exec-order leave behind (see m144's own
// migration comment). m144 raises its NOTICE and returns before touching a
// row or taking a lock on any of the three tables, even though a raw probe
// exists that a real repair would otherwise aggregate.
//
// Also the fires proof for the "no m99 row" RETURN block itself
// (m144NoM99RowSkipBlock): an in-memory copy with that block deleted
// proceeds past the skip — v_cutoff stays NULL, which the ALTER TABLE
// statements that follow don't depend on — and DOES take ACCESS EXCLUSIVE
// locks on all three tables, proving the "no lock taken" assertion below is
// load-bearing rather than vacuous.
func TestM144SkipsOutsideServerRunner_NoM99Row(t *testing.T) {
	pool, owner := startPostgresBeforeM144(t)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "m144-no-m99-row")
	siteID := seedSiteFor(t, pool, tenant, "https://m144-no-m99-row.example.com")

	// Truncated to the microsecond and anchored to today with anchorToToday
	// — see TestM144SkipsOutsideServerRunner_NoSchemaMigrationsTable's own
	// comment for why.
	now := time.Now().UTC().Truncate(time.Microsecond)
	today := now.Truncate(24 * time.Hour)
	probedAt := anchorToToday(now.Add(-1*time.Hour), today)
	if _, err := pool.Exec(ctx,
		`INSERT INTO site_uptime_probes (tenant_id, site_id, probed_at, up, total_ms) VALUES ($1, $2, $3, true, 30)`,
		tenant, siteID, probedAt); err != nil {
		t.Fatalf("seed raw probe: %v", err)
	}

	// schema_migrations exists (startPostgresBeforeM144's own bootstrap
	// applied m99 for real, through the owner role, and inserted its row)
	// but drop JUST m99's row: the "table present, no m99 row" skip.
	if _, err := pool.Exec(ctx,
		`DELETE FROM schema_migrations WHERE version = $1`, m99MigrationVersion); err != nil {
		t.Fatalf("delete m99's schema_migrations row: %v", err)
	}

	if _, err := owner.Exec(ctx, readM144Body(t)); err != nil {
		t.Fatalf("m144 body failed with schema_migrations present but no m99 row, want a clean no-op: %v", err)
	}

	var dailyCount, statusCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM site_uptime_daily WHERE site_id = $1`, siteID).Scan(&dailyCount); err != nil {
		t.Fatalf("count site_uptime_daily: %v", err)
	}
	if dailyCount != 0 {
		t.Fatalf("site_uptime_daily has %d row(s) after m144 ran with no m99 row, want 0 (should have skipped before touching anything)", dailyCount)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM site_uptime_status WHERE site_id = $1`, siteID).Scan(&statusCount); err != nil {
		t.Fatalf("count site_uptime_status: %v", err)
	}
	if statusCount != 0 {
		t.Fatalf("site_uptime_status has %d row(s) after m144 ran with no m99 row, want 0", statusCount)
	}
	assertUptimeRollupTablesForceIntact(t, pool)

	// DOES NOT OVER-FIRE: the real skip path takes NO ACCESS EXCLUSIVE lock
	// on any of the three tables.
	if n := m144AccessExclusiveLockCount(t, owner, readM144Body(t)); n != 0 {
		t.Fatalf("m144's no-m99-row skip took %d ACCESS EXCLUSIVE lock(s) on the uptime rollup tables, want 0", n)
	}
	t.Logf("real m144 correctly took 0 ACCESS EXCLUSIVE locks on the uptime rollup tables on the no-m99-row skip path")

	// FIRES: an in-memory copy of m144 with the "no m99 row" RETURN block
	// deleted proceeds past the skip and DOES take ACCESS EXCLUSIVE locks on
	// all three tables.
	mutated := stripOnce(t, readM144Body(t), m144NoM99RowSkipBlock)
	if n := m144AccessExclusiveLockCount(t, owner, mutated); n == 0 {
		t.Fatal("block-stripped m144 (no-m99-row RETURN removed) took 0 ACCESS EXCLUSIVE locks; expected it to proceed past the skip and lock all three tables (this mutation must fire for the proof above to mean anything)")
	} else {
		t.Logf("block-stripped m144 correctly took %d ACCESS EXCLUSIVE lock(s) on the uptime rollup tables (guard-is-load-bearing)", n)
	}

	// Restore proof: with m99's row back — the state startPostgresBeforeM144
	// actually left it in — the REAL repair runs and backfills the seeded
	// probe.
	if _, err := pool.Exec(ctx,
		`INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT (version) DO NOTHING`,
		m99MigrationVersion); err != nil {
		t.Fatalf("re-seed m99's applied_at row: %v", err)
	}
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("m144 migration: %v", err)
	}
	up, total, _, _, found := queryDailyBucket(t, pool, siteID, today)
	if !found || up != 1 || total != 1 {
		t.Fatalf("after restoring m99's row: up=%d total=%d found=%v, want 1/1 backfilled", up, total, found)
	}
	assertUptimeRollupTablesForceIntact(t, pool)
}
