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
)

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
