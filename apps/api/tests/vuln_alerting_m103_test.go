// vuln_alerting_m103_test.go — m103 (GH #247) migration backfill regression
// test. Mirrors sitetag_migration_test.go's pattern exactly: boot a
// container, apply every embedded migration UP TO (not including) m103, seed
// alert_configs and site_vulnerabilities rows the way a live pre-m103
// deployment would already have them (across open/dismissed/resolved
// statuses), finish the boot (applying m103, including its backfill UPDATE),
// and assert EVERY pre-existing site_vulnerabilities row is stamped
// notified_at (so the first-ever dispatch never emails the tenant's entire
// historical backlog) and the new alert_configs columns land with their
// documented defaults. Also proves the backfill is idempotent by re-executing
// the migration's own SQL text a second time.
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

// m103MigrationVersion is the embedded migration filename (sans .sql) that
// adds vulnerability-alerting columns and backfills notified_at.
const m103MigrationVersion = "20260805000000_m103_vuln_alerting"

// startPostgresBeforeM103 mirrors startPostgresBeforeM100's container
// bootstrap but stops short of m103.
func startPostgresBeforeM103(t *testing.T) *db.Pool {
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

	applyMigrationsBeforeM103(t, pool, m103MigrationVersion)
	return pool
}

// applyMigrationsBeforeM103 is a package-local copy of
// applyMigrationsBeforeM100's embedded-FS walk (kept local per that file's
// own stated convention so this file has no compile-order dependency on it).
func applyMigrationsBeforeM103(t *testing.T, pool *db.Pool, stopAt string) {
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

// TestM103Migration_BackfillNotifiedAt_AndDefaults is the ship-level
// regression test: every pre-existing site_vulnerabilities row (regardless of
// status) gets notified_at stamped, and alert_configs gains the three new
// columns with their documented defaults.
func TestM103Migration_BackfillNotifiedAt_AndDefaults(t *testing.T) {
	pool := startPostgresBeforeM103(t)
	ctx := context.Background()

	var tenant uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO tenants (name, slug) VALUES ($1, $1) RETURNING id`,
		"m103-"+uuid.NewString()[:8]).Scan(&tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	var siteID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, 'seed') RETURNING id`,
		tenant, "https://m103-site.example.com").Scan(&siteID); err != nil {
		t.Fatalf("seed site: %v", err)
	}

	// A pre-existing alert_configs row (pre-m103 shape: no vuln columns yet).
	if _, err := pool.Exec(ctx,
		`INSERT INTO alert_configs (tenant_id, enabled, notify_security) VALUES ($1, true, true)`,
		tenant); err != nil {
		t.Fatalf("seed alert_configs: %v", err)
	}

	// Three pre-existing findings across the three statuses — the backfill
	// must stamp ALL of them, not just open ones (dismissed/resolved rows
	// never alert anyway per the dispatch claim's WHERE status='open', but
	// the migration's backfill is a blanket UPDATE by design).
	seedFinding := func(vulnID, status string) uuid.UUID {
		var id uuid.UUID
		err := pool.QueryRow(ctx, `
			INSERT INTO site_vulnerabilities
				(tenant_id, site_id, vuln_id, kind, slug, name, installed_version,
				 severity, title, status)
			VALUES ($1, $2, $3, 'plugin', $3, 'seed', '1.0.0', 'high', 'seed finding', $4)
			RETURNING id`,
			tenant, siteID, vulnID, status,
		).Scan(&id)
		if err != nil {
			t.Fatalf("seed finding (%s): %v", status, err)
		}
		return id
	}
	openID := seedFinding("vuln-open", "open")
	dismissedID := seedFinding("vuln-dismissed", "dismissed")
	resolvedID := seedFinding("vuln-resolved", "resolved")

	// Finish the boot: applies m103 (columns + CHECK + backfill + index).
	if err := pool.Migrate(ctx); err != nil {
		t.Fatalf("m103 migration failed: %v", err)
	}

	assertBackfilled := func(t *testing.T) map[uuid.UUID]time.Time {
		t.Helper()
		stamps := map[uuid.UUID]time.Time{}
		for _, id := range []uuid.UUID{openID, dismissedID, resolvedID} {
			var notifiedAt *time.Time
			if err := pool.QueryRow(ctx, `SELECT notified_at FROM site_vulnerabilities WHERE id = $1`, id).Scan(&notifiedAt); err != nil {
				t.Fatalf("query notified_at for %s: %v", id, err)
			}
			if notifiedAt == nil {
				t.Fatalf("finding %s: notified_at must be backfilled (non-NULL), got NULL", id)
			}
			stamps[id] = *notifiedAt
		}
		return stamps
	}
	firstStamps := assertBackfilled(t)

	var notifyVulns, vulnIncludeInDigest bool
	var vulnMinSeverity string
	if err := pool.QueryRow(ctx,
		`SELECT notify_vulns, vuln_min_severity, vuln_include_in_digest FROM alert_configs WHERE tenant_id = $1`,
		tenant).Scan(&notifyVulns, &vulnMinSeverity, &vulnIncludeInDigest); err != nil {
		t.Fatalf("query alert_configs new columns: %v", err)
	}
	if notifyVulns {
		t.Error("notify_vulns must default to false")
	}
	if vulnMinSeverity != "high" {
		t.Errorf("vuln_min_severity must default to 'high', got %q", vulnMinSeverity)
	}
	if !vulnIncludeInDigest {
		t.Error("vuln_include_in_digest must default to true")
	}

	// Idempotency: re-execute m103's own SQL text a second time directly
	// (bypassing schema_migrations tracking) and assert the notified_at
	// stamps are UNCHANGED (not re-stamped to a later time).
	body, err := fs.ReadFile(migrations.FS, m103MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m103 migration body: %v", err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("re-apply m103 migration SQL: %v", err)
	}
	secondStamps := assertBackfilled(t)
	for id, first := range firstStamps {
		if !secondStamps[id].Equal(first) {
			t.Fatalf("finding %s: notified_at changed on re-apply (%v -> %v); backfill must be idempotent", id, first, secondStamps[id])
		}
	}
}

// TestM103_NewFindingAfterMigration_NotBackfilled proves new-enrollment first
// scans are NOT suppressed: a finding INSERTed after m103 has applied gets
// notified_at = NULL from the column default (no explicit value), so it
// alerts normally on the next dispatch — the migration's backfill only
// touches rows that existed AT MIGRATION TIME.
func TestM103_NewFindingAfterMigration_NotBackfilled(t *testing.T) {
	pool := startPostgres(t) // full boot (all migrations applied), non-superuser role
	ctx := context.Background()

	tenant := seedTenant(t, pool, "m103-new-"+uuid.NewString()[:8])
	siteID := seedSite(t, pool, tenant, "")

	err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO site_vulnerabilities
				(tenant_id, site_id, vuln_id, kind, slug, name, installed_version, severity, title, status)
			VALUES ($1, $2, 'vuln-new', 'plugin', 'vuln-new', 'seed', '1.0.0', 'high', 'seed', 'open')`,
			tenant, siteID)
		return err
	})
	if err != nil {
		t.Fatalf("insert post-migration finding: %v", err)
	}

	var notifiedAt *time.Time
	err = pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT notified_at FROM site_vulnerabilities WHERE tenant_id = $1 AND vuln_id = 'vuln-new'`, tenant).Scan(&notifiedAt)
	})
	if err != nil {
		t.Fatalf("query notified_at: %v", err)
	}
	if notifiedAt != nil {
		t.Fatalf("a NEW finding must have notified_at = NULL (alerts normally), got %v", *notifiedAt)
	}
}
