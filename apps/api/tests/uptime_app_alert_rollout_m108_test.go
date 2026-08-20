// uptime_app_alert_rollout_m108_test.go - m108 (GH #291 Phase 3) migration
// rollout-default test. Mirrors vuln_alerting_m103_test.go's pattern: boot a
// container, apply every embedded migration UP TO (not including) m108, seed
// `sites` rows the way a live pre-m108 deployment would already have them (or
// don't, for the fresh-install case), finish the boot (applying m108), and
// assert the deployment-fresh decision landed correctly and identically in
// BOTH places it is recorded: the app_alert_rollout singleton row and the
// alert_configs.app_alerts_enabled column's own DEFAULT (applied to every
// pre-existing alert_configs row AND every future INSERT on that
// deployment).
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

// m108MigrationVersion is the embedded migration filename (sans .sql) that
// adds app-health alerting columns/tables and decides the rollout default.
const m108MigrationVersion = "20260810000000_m108_uptime_app_alerting"

// startPostgresBeforeM108 mirrors startPostgresBeforeM103's container
// bootstrap but stops short of m108.
func startPostgresBeforeM108(t *testing.T) *db.Pool {
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

	applyMigrationsBeforeM108(t, pool, m108MigrationVersion)
	return pool
}

// applyMigrationsBeforeM108 is a package-local copy of
// applyMigrationsBeforeM103's embedded-FS walk (kept local per that file's
// own stated convention so this file has no compile-order dependency on it).
func applyMigrationsBeforeM108(t *testing.T, pool *db.Pool, stopAt string) {
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

// TestM108Migration_UpgradeDeployment_DefaultsAppAlertsOff seeds a site
// BEFORE m108 runs (the "this deployment already manages sites" case) and
// asserts BOTH the app_alert_rollout singleton and the alert_configs.
// app_alerts_enabled column's own DEFAULT land on false - an operator who
// has not asked for app-health alerting must never have it silently turned
// on by an upgrade.
func TestM108Migration_UpgradeDeployment_DefaultsAppAlertsOff(t *testing.T) {
	pool := startPostgresBeforeM108(t)
	ctx := context.Background()

	var tenant uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO tenants (name, slug) VALUES ($1, $1) RETURNING id`,
		"m108-upgrade-"+uuid.NewString()[:8]).Scan(&tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, 'seed')`,
		tenant, "https://m108-upgrade-site.example.com"); err != nil {
		t.Fatalf("seed site: %v", err)
	}

	// A pre-existing alert_configs row (pre-m108 shape: no app_alerts_enabled
	// column yet) - the column's DEFAULT must backfill it to false.
	if _, err := pool.Exec(ctx,
		`INSERT INTO alert_configs (tenant_id, enabled) VALUES ($1, true)`, tenant); err != nil {
		t.Fatalf("seed alert_configs: %v", err)
	}

	// Finish the boot: applies m108.
	if err := pool.Migrate(ctx); err != nil {
		t.Fatalf("m108 migration failed: %v", err)
	}

	var freshInstall bool
	if err := pool.QueryRow(ctx, `SELECT fresh_install FROM app_alert_rollout WHERE singleton = true`).Scan(&freshInstall); err != nil {
		t.Fatalf("query app_alert_rollout: %v", err)
	}
	if freshInstall {
		t.Fatal("app_alert_rollout.fresh_install must be false: a site already existed at migration time")
	}

	var appAlertsEnabled bool
	if err := pool.QueryRow(ctx, `SELECT app_alerts_enabled FROM alert_configs WHERE tenant_id = $1`, tenant).Scan(&appAlertsEnabled); err != nil {
		t.Fatalf("query alert_configs.app_alerts_enabled: %v", err)
	}
	if appAlertsEnabled {
		t.Fatal("the pre-existing alert_configs row must be backfilled to app_alerts_enabled = false on an upgrade deployment")
	}

	// A NEW tenant's FIRST-ever alert_configs row, inserted AFTER m108 ran,
	// must ALSO default to false - the deployment-wide decision applies to
	// every tenant on this deployment, present and future, not re-decided
	// per tenant.
	var tenant2 uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO tenants (name, slug) VALUES ($1, $1) RETURNING id`,
		"m108-upgrade-new-"+uuid.NewString()[:8]).Scan(&tenant2); err != nil {
		t.Fatalf("seed second tenant: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO alert_configs (tenant_id, enabled) VALUES ($1, true)`, tenant2); err != nil {
		t.Fatalf("seed second alert_configs: %v", err)
	}
	var appAlertsEnabled2 bool
	if err := pool.QueryRow(ctx, `SELECT app_alerts_enabled FROM alert_configs WHERE tenant_id = $1`, tenant2).Scan(&appAlertsEnabled2); err != nil {
		t.Fatalf("query second alert_configs.app_alerts_enabled: %v", err)
	}
	if appAlertsEnabled2 {
		t.Fatal("a brand-new tenant's alert_configs row on an upgrade deployment must also default app_alerts_enabled = false")
	}
}

// TestM108Migration_FreshInstall_DefaultsAppAlertsOn is the mirror case: NO
// sites exist when m108 runs (a genuinely fresh install) - both the rollout
// singleton and the column's own DEFAULT must land on true.
func TestM108Migration_FreshInstall_DefaultsAppAlertsOn(t *testing.T) {
	pool := startPostgresBeforeM108(t)
	ctx := context.Background()

	// No sites seeded - this is the point of the test.
	if err := pool.Migrate(ctx); err != nil {
		t.Fatalf("m108 migration failed: %v", err)
	}

	var freshInstall bool
	if err := pool.QueryRow(ctx, `SELECT fresh_install FROM app_alert_rollout WHERE singleton = true`).Scan(&freshInstall); err != nil {
		t.Fatalf("query app_alert_rollout: %v", err)
	}
	if !freshInstall {
		t.Fatal("app_alert_rollout.fresh_install must be true: no site existed at migration time")
	}

	var tenant uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO tenants (name, slug) VALUES ($1, $1) RETURNING id`,
		"m108-fresh-"+uuid.NewString()[:8]).Scan(&tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO alert_configs (tenant_id, enabled) VALUES ($1, true)`, tenant); err != nil {
		t.Fatalf("seed alert_configs: %v", err)
	}
	var appAlertsEnabled bool
	if err := pool.QueryRow(ctx, `SELECT app_alerts_enabled FROM alert_configs WHERE tenant_id = $1`, tenant).Scan(&appAlertsEnabled); err != nil {
		t.Fatalf("query alert_configs.app_alerts_enabled: %v", err)
	}
	if !appAlertsEnabled {
		t.Fatal("a fresh install's alert_configs row must default app_alerts_enabled = true")
	}
}

// TestM108Migration_Idempotent proves re-executing m108's own SQL text a
// second time (mirrors every other "run twice" migration regression test in
// this suite) does not error and does not change the already-decided
// rollout default.
func TestM108Migration_Idempotent(t *testing.T) {
	pool := startPostgresBeforeM108(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`INSERT INTO tenants (name, slug) VALUES ($1, $1)`, "m108-idempotent-"+uuid.NewString()[:8]); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO sites (tenant_id, url, name) SELECT id, 'https://m108-idempotent.example.com', 'seed' FROM tenants LIMIT 1`); err != nil {
		t.Fatalf("seed site: %v", err)
	}

	if err := pool.Migrate(ctx); err != nil {
		t.Fatalf("m108 migration failed: %v", err)
	}
	var before bool
	if err := pool.QueryRow(ctx, `SELECT fresh_install FROM app_alert_rollout WHERE singleton = true`).Scan(&before); err != nil {
		t.Fatalf("query app_alert_rollout: %v", err)
	}

	body, err := fs.ReadFile(migrations.FS, m108MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m108 migration body: %v", err)
	}
	// Re-applying with sites still present must remain a no-op (still false)
	// even though a naive re-run would recompute fresh_install from the
	// CURRENT (now non-empty) sites table - ON CONFLICT DO NOTHING on the
	// singleton row is what pins the decision permanently.
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("re-apply m108 migration SQL: %v", err)
	}
	var after bool
	if err := pool.QueryRow(ctx, `SELECT fresh_install FROM app_alert_rollout WHERE singleton = true`).Scan(&after); err != nil {
		t.Fatalf("query app_alert_rollout after re-apply: %v", err)
	}
	if before != after {
		t.Fatalf("fresh_install changed on re-apply (%v -> %v); the rollout decision must be permanent", before, after)
	}

	var rowCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM app_alert_rollout`).Scan(&rowCount); err != nil {
		t.Fatalf("count app_alert_rollout rows: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("app_alert_rollout must stay a true singleton, got %d rows", rowCount)
	}
}
