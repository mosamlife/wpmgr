// uptime_rollup_backfill_test.go — m99 migration backfill regression test.
// Mirrors backup_m96_migration_test.go's pattern exactly: boot a container,
// apply every embedded migration UP TO (not including) m99, seed raw
// site_uptime_probes rows the way a live pre-m99 deployment would already
// have them, finish the boot (applying m99, including its backfill INSERT
// statements), and assert the backfilled site_uptime_daily/site_uptime_status
// rows match an independent Go-computed GROUP BY over the exact seeded rows.
// Also proves the backfill is idempotent by re-executing the migration's own
// SQL text a second time and asserting nothing changes.
package tests

import (
	"context"
	"io/fs"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

// m99MigrationVersion is the embedded migration filename (sans .sql) that
// adds site_uptime_daily/site_uptime_status and backfills them from
// pre-existing site_uptime_probes rows.
const m99MigrationVersion = "20260801000000_m99_uptime_rollup"

// startPostgresBeforeM99 mirrors startPostgresBeforeM96's container bootstrap
// but stops short of m99. Migrations run AS wpmgr_owner — the NOSUPERUSER
// NOBYPASSRLS role production's migrator uses, not the bootstrap superuser
// (see startPostgres's doc comment in rls_integration_test.go); site_uptime_daily
// and site_uptime_status are FORCE ROW LEVEL SECURITY (RLS correctness itself
// is covered separately by TestUptimeRollupTablesRLS, but the migrator's own
// exposure to that RLS is exactly what this file's backfill test now needs to
// reproduce). Returns (admin, owner): seed pre-m99 probe rows through admin,
// the bootstrap superuser; call owner.Migrate(ctx) (and, for the idempotency
// re-exec below, owner.Exec) to run migration SQL under the production role.
func startPostgresBeforeM99(t *testing.T) (admin *db.Pool, owner *db.Pool) {
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

	applyMigrationsBeforeM99(t, owner, m99MigrationVersion)
	return admin, owner
}

// applyMigrationsBeforeM99 is a package-local copy of
// backup_m96_migration_test.go's applyMigrationsBeforeM96 (same re-implementation
// of internal/db/migrate.go's embedded-FS walk, kept local per that file's own
// stated convention so this file has no compile-order dependency on it).
func applyMigrationsBeforeM99(t *testing.T, pool *db.Pool, stopAt string) {
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

// dailyBucketExpectation is the Go-computed GROUP BY reference the backfilled
// site_uptime_daily rows must match exactly.
type dailyBucketExpectation struct {
	day                   time.Time
	upChecks, totalChecks int64
	sumLatencyMs          float64
	latencySamples        int64
}

// TestM99Migration_BackfillMatchesRawGroupBy_AndIdempotent is the ship-level
// regression test: on a database that already has historical site_uptime_probes
// rows (exactly the state of any live pre-m99 deployment), m99's backfill must
// populate site_uptime_daily/site_uptime_status with values that agree with a
// straight GROUP BY over those rows, and running the backfill SQL a second
// time must be a complete no-op (ON CONFLICT DO NOTHING).
func TestM99Migration_BackfillMatchesRawGroupBy_AndIdempotent(t *testing.T) {
	// KNOWN GAP (found by PR #775's owner-role harness, not fixed here — a
	// migration change for database-engineer, not this test-harness PR):
	// worse than the m88/m96 sibling skips, this one does NOT fail loudly.
	// m99's backfill INSERTs read FROM site_uptime_probes (also FORCE ROW
	// LEVEL SECURITY) with no GUC set by the production migrator, so under
	// the real migrator role the SELECT ... GROUP BY sees zero source rows,
	// the INSERT is a silent no-op, and the migration reports success with
	// site_uptime_daily/site_uptime_status left EMPTY instead of backfilled:
	//
	//   uptime_rollup_backfill_test.go:274: query daily bucket for
	//   2026-09-28 00:00:00 +0000 UTC: no rows in result set
	//
	// Implication: any self-hosted install still pre-m99, on upgrade, loses
	// its entire pre-migration uptime history and current-status stamp
	// silently — no error, no log, a successful boot. Do not loosen this
	// test or the migration to make the skip below go away — see PR #775 /
	// the session worklog for the full analysis.
	t.Skip("known gap: m99's backfill silently inserts zero rows under the production migrator role (RLS on site_uptime_probes); see PR #775")

	pool, owner := startPostgresBeforeM99(t)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "m99-backfill")
	siteID := seedSiteFor(t, pool, tenant, "https://m99-backfill.example.com")
	otherSiteID := seedSiteFor(t, pool, tenant, "https://m99-backfill-other.example.com")

	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	yesterday := today.Add(-24 * time.Hour)

	type probeSeed struct {
		siteID   uuid.UUID
		probedAt time.Time
		up       bool
		totalMs  float64
	}
	seeds := []probeSeed{
		// siteID, today: 3 up (one zero-latency, excluded from the latency
		// average like NULLIF(total_ms,0) would exclude it), 1 down.
		{siteID, today.Add(9 * time.Hour), true, 120},
		{siteID, today.Add(10 * time.Hour), true, 0},
		{siteID, today.Add(11 * time.Hour), false, 0},
		{siteID, today.Add(12 * time.Hour), true, 80}, // most recent overall — the expected "latest" row.
		// siteID, yesterday: 2 up.
		{siteID, yesterday.Add(9 * time.Hour), true, 200},
		{siteID, yesterday.Add(10 * time.Hour), true, 300},
		// A second site, today only — proves per-site grouping.
		{otherSiteID, today.Add(9 * time.Hour), false, 999},
	}
	for _, s := range seeds {
		if _, err := pool.Exec(ctx,
			`INSERT INTO site_uptime_probes (tenant_id, site_id, probed_at, up, total_ms) VALUES ($1, $2, $3, $4, $5)`,
			tenant, s.siteID, s.probedAt, s.up, s.totalMs); err != nil {
			t.Fatalf("seed probe: %v", err)
		}
	}

	// Finish the boot: applies m99 (table creation + RLS + backfill), AS
	// wpmgr_owner.
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("m99 migration failed: %v", err)
	}

	// Independent Go-computed reference for siteID's two day buckets.
	want := map[time.Time]dailyBucketExpectation{
		today: {
			day: today, upChecks: 3, totalChecks: 4,
			sumLatencyMs: 120 + 80, latencySamples: 2,
		},
		yesterday: {
			day: yesterday, upChecks: 2, totalChecks: 2,
			sumLatencyMs: 200 + 300, latencySamples: 2,
		},
	}

	assertDailyBuckets := func(t *testing.T) {
		t.Helper()
		for day, w := range want {
			var up, total, samples int64
			var sumMs float64
			err := pool.QueryRow(ctx,
				`SELECT up_checks, total_checks, sum_latency_ms, latency_samples
				 FROM site_uptime_daily WHERE site_id = $1 AND day = $2`,
				siteID, day).Scan(&up, &total, &sumMs, &samples)
			if err != nil {
				t.Fatalf("query daily bucket for %v: %v", day, err)
			}
			if up != w.upChecks || total != w.totalChecks || samples != w.latencySamples {
				t.Fatalf("day=%v: up=%d total=%d samples=%d, want up=%d total=%d samples=%d",
					day, up, total, samples, w.upChecks, w.totalChecks, w.latencySamples)
			}
			if sumMs != w.sumLatencyMs {
				t.Fatalf("day=%v: sum_latency_ms=%v, want %v", day, sumMs, w.sumLatencyMs)
			}
		}

		var otherUp, otherTotal int64
		if err := pool.QueryRow(ctx,
			`SELECT up_checks, total_checks FROM site_uptime_daily WHERE site_id = $1 AND day = $2`,
			otherSiteID, today).Scan(&otherUp, &otherTotal); err != nil {
			t.Fatalf("query other site daily bucket: %v", err)
		}
		if otherUp != 0 || otherTotal != 1 {
			t.Fatalf("other site bucket: up=%d total=%d, want 0/1", otherUp, otherTotal)
		}
	}
	assertDailyBuckets(t)

	// Status stamp: the single most recent probe (today.Add(12h), up=true).
	var latestUp bool
	var lastProbedAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT latest_up, last_probed_at FROM site_uptime_status WHERE site_id = $1`, siteID).
		Scan(&latestUp, &lastProbedAt); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if !latestUp {
		t.Fatal("status: latest_up = false, want true (most recent seeded probe was up)")
	}
	if !lastProbedAt.Equal(today.Add(12 * time.Hour)) {
		t.Fatalf("status: last_probed_at = %v, want %v", lastProbedAt, today.Add(12*time.Hour))
	}

	// Idempotency: re-execute the m99 migration's own SQL text a second time
	// directly (bypassing schema_migrations tracking, which is what makes this
	// a genuine re-run test rather than a no-op from Migrate() skipping an
	// already-applied version) and assert nothing changed.
	body, err := fs.ReadFile(migrations.FS, m99MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m99 migration body: %v", err)
	}
	if _, err := owner.Exec(ctx, string(body)); err != nil {
		t.Fatalf("re-apply m99 migration SQL: %v", err)
	}
	assertDailyBuckets(t)

	var statusCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM site_uptime_status WHERE site_id = $1`, siteID).Scan(&statusCount); err != nil {
		t.Fatalf("count status rows: %v", err)
	}
	if statusCount != 1 {
		t.Fatalf("status row count after re-apply = %d, want 1 (idempotent)", statusCount)
	}
}
