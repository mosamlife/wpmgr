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
// but stops short of m99, using the bootstrap superuser connection directly
// (this test is about migration/backfill correctness, not RLS — RLS on the
// two new tables is covered separately by TestUptimeRollupTablesRLS).
func startPostgresBeforeM99(t *testing.T) *db.Pool {
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

	adminDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	pool, err := db.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(pool.Close)

	applyMigrationsBeforeM99(t, pool, m99MigrationVersion)
	return pool
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
	pool := startPostgresBeforeM99(t)
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

	// Finish the boot: applies m99 (table creation + RLS + backfill).
	if err := pool.Migrate(ctx); err != nil {
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
	if _, err := pool.Exec(ctx, string(body)); err != nil {
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
