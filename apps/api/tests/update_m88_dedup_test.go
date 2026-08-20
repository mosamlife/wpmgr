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

// m88MigrationVersion is the embedded migration filename (sans .sql) that adds
// update_tasks_inflight_target_idx. The harness below stops short of applying
// it so the test can seed the exact pre-m88 duplicate-row scenario the
// migration must heal before creating the unique index.
const m88MigrationVersion = "20260721000000_m88_update_tasks_inflight_dedup"

// startPostgresBeforeM88 mirrors startPostgres's container bootstrap but
// applies every embedded migration UP TO (not including) m88, using the
// bootstrap superuser connection (matching cmd/wpmgr/main.go's real migration
// DSN, which is also the owner/superuser). The caller seeds pre-m88 data, then
// finishes the boot with pool.Migrate(ctx), which applies exactly m88 (and any
// later migration, of which there are currently none).
func startPostgresBeforeM88(t *testing.T) *db.Pool {
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
	pool, err := db.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(pool.Close)

	applyMigrationsBefore(t, pool, m88MigrationVersion)
	return pool
}

// applyMigrationsBefore re-implements the (unexported) Pool.Migrate loop from
// internal/db/migrate.go so this test can stop short of one named version:
// same embedded FS, same lexical ordering, same one-tx-per-migration +
// schema_migrations bookkeeping. Fails the test if stopAt is not found among
// the embedded migrations (a version rename would otherwise silently apply
// everything and defeat the test).
func applyMigrationsBefore(t *testing.T, pool *db.Pool, stopAt string) {
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

// TestM88MigrationDedupesPreexistingInFlightDuplicates is the ship-blocker
// regression test: on a LIVE database that already ran the buggy pre-m88 code,
// multiple pending/running update_tasks rows can already exist for the same
// (tenant, site, target_type, target_slug) — that is the exact bug m88 fixes,
// and prior to the reaper there was nothing to clean them up, so they persist
// indefinitely. CREATE UNIQUE INDEX would fail outright on that duplicate
// data, and since the migration runner wraps each migration in a tx and
// aborts boot on error, that would hard-block the API from booting for every
// affected deployment. This test seeds the exact collision, then proves the
// migration heals it before creating the index.
func TestM88MigrationDedupesPreexistingInFlightDuplicates(t *testing.T) {
	pool := startPostgresBeforeM88(t)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "m88-dedup")

	var siteID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, $3) RETURNING id`,
		tenant, "https://m88-dedup.example.com", "m88 dedup site",
	).Scan(&siteID); err != nil {
		t.Fatalf("seed site: %v", err)
	}

	var runID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO update_runs (tenant_id, status) VALUES ($1, 'running') RETURNING id`,
		tenant,
	).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	// Seed THREE duplicate in-flight tasks for the SAME (tenant, site, target)
	// — the exact pre-m88 bug (#131): a scheduled auto-update, an operator
	// bulk "Update all", and a client-portal trigger could each create a task
	// for the same (site, plugin) concurrently, since nothing before m88
	// prevented it. Stagger created_at so "keep the newest" is unambiguous,
	// and mix pending/running so the dedup statement's status IN (...) filter
	// is exercised on both.
	base := time.Now().Add(-time.Hour)
	var newestID uuid.UUID
	for i, status := range []string{"pending", "running", "pending"} {
		createdAt := base.Add(time.Duration(i) * time.Minute)
		var id uuid.UUID
		if err := pool.QueryRow(ctx,
			`INSERT INTO update_tasks
			   (run_id, tenant_id, site_id, target_type, target_slug, status, created_at, updated_at)
			 VALUES ($1, $2, $3, 'plugin', 'akismet', $4, $5, $5)
			 RETURNING id`,
			runID, tenant, siteID, status, createdAt,
		).Scan(&id); err != nil {
			t.Fatalf("seed duplicate task %d: %v", i, err)
		}
		newestID = id // last iteration has the latest created_at
	}

	// A DIFFERENT target on the same site is not a duplicate and must survive
	// the dedup statement untouched.
	var unrelatedID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO update_tasks (run_id, tenant_id, site_id, target_type, target_slug, status)
		 VALUES ($1, $2, $3, 'plugin', 'woocommerce', 'pending')
		 RETURNING id`,
		runID, tenant, siteID,
	).Scan(&unrelatedID); err != nil {
		t.Fatalf("seed unrelated task: %v", err)
	}

	// Finish the boot: this applies m88. It must succeed even though the
	// table already carries the duplicate in-flight rows above.
	if err := pool.Migrate(ctx); err != nil {
		t.Fatalf("m88 migration failed on a table with pre-existing in-flight duplicates: %v", err)
	}

	// Exactly one in-flight (pending/running) row remains for the deduped
	// target, and it is the newest of the three duplicates.
	rows, err := pool.Query(ctx,
		`SELECT id FROM update_tasks
		 WHERE tenant_id = $1 AND site_id = $2 AND target_type = 'plugin' AND target_slug = 'akismet'
		   AND status IN ('pending', 'running')`,
		tenant, siteID)
	if err != nil {
		t.Fatalf("query survivors: %v", err)
	}
	var survivors []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan survivor: %v", err)
		}
		survivors = append(survivors, id)
	}
	rows.Close()
	if len(survivors) != 1 {
		t.Fatalf("want exactly 1 in-flight survivor, got %d: %v", len(survivors), survivors)
	}
	if survivors[0] != newestID {
		t.Fatalf("survivor = %s, want the newest duplicate %s", survivors[0], newestID)
	}

	// The two superseded duplicates were terminalized (history preserved, not
	// deleted) as 'skipped'.
	var skippedCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM update_tasks
		 WHERE tenant_id = $1 AND site_id = $2 AND target_type = 'plugin' AND target_slug = 'akismet'
		   AND status = 'skipped'`,
		tenant, siteID).Scan(&skippedCount); err != nil {
		t.Fatalf("count skipped: %v", err)
	}
	if skippedCount != 2 {
		t.Fatalf("want 2 skipped duplicates, got %d", skippedCount)
	}

	// The unrelated target is untouched.
	var unrelatedStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM update_tasks WHERE id = $1`, unrelatedID).
		Scan(&unrelatedStatus); err != nil {
		t.Fatalf("get unrelated task: %v", err)
	}
	if unrelatedStatus != "pending" {
		t.Fatalf("unrelated task status = %q, want untouched pending", unrelatedStatus)
	}

	// The partial unique index now exists.
	var idxCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'update_tasks_inflight_target_idx'`,
	).Scan(&idxCount); err != nil {
		t.Fatalf("check index: %v", err)
	}
	if idxCount != 1 {
		t.Fatal("update_tasks_inflight_target_idx was not created")
	}

	// The index is actually enforcing going forward: a fresh duplicate insert
	// for the same in-flight target now fails.
	_, err = pool.Exec(ctx,
		`INSERT INTO update_tasks (run_id, tenant_id, site_id, target_type, target_slug, status)
		 VALUES ($1, $2, $3, 'plugin', 'akismet', 'pending')`,
		runID, tenant, siteID)
	if err == nil {
		t.Fatal("expected the partial unique index to reject a second in-flight duplicate after m88")
	}
}

// TestM88MigrationIsNoopWhenNoDuplicatesExist proves the common case (a
// database with no pre-existing collisions, e.g. every fresh install and
// every deployment that never hit the pre-m88 race) is unaffected: the
// pre-dedup UPDATE touches nothing and the index is created normally.
func TestM88MigrationIsNoopWhenNoDuplicatesExist(t *testing.T) {
	pool := startPostgresBeforeM88(t)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "m88-noop")
	var siteID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, $3) RETURNING id`,
		tenant, "https://m88-noop.example.com", "m88 noop site",
	).Scan(&siteID); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	var runID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO update_runs (tenant_id, status) VALUES ($1, 'running') RETURNING id`,
		tenant,
	).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	var taskID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO update_tasks (run_id, tenant_id, site_id, target_type, target_slug, status)
		 VALUES ($1, $2, $3, 'plugin', 'akismet', 'running')
		 RETURNING id`,
		runID, tenant, siteID,
	).Scan(&taskID); err != nil {
		t.Fatalf("seed single in-flight task: %v", err)
	}

	if err := pool.Migrate(ctx); err != nil {
		t.Fatalf("m88 migration failed on a clean table: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM update_tasks WHERE id = $1`, taskID).Scan(&status); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if status != "running" {
		t.Fatalf("single in-flight task status = %q, want untouched running", status)
	}

	var idxCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'update_tasks_inflight_target_idx'`,
	).Scan(&idxCount); err != nil {
		t.Fatalf("check index: %v", err)
	}
	if idxCount != 1 {
		t.Fatal("update_tasks_inflight_target_idx was not created")
	}
}
