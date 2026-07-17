// sitetag_migration_test.go — m100 migration backfill regression test.
// Mirrors uptime_rollup_backfill_test.go's pattern exactly: boot a
// container, apply every embedded migration UP TO (not including) m100,
// seed sites.tags AND pairing_codes.tags rows the way a live pre-m100
// deployment would already have them (across TWO tenants, including an
// expired code and a consumed code that must NOT be backfilled), finish the
// boot (applying m100, including its backfill INSERTs), and assert the
// site_tags registry matches an independent Go-computed reference. Also
// proves the backfill is idempotent by re-executing the migration's own SQL
// text a second time and asserting nothing changes.
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

// m100MigrationVersion is the embedded migration filename (sans .sql) that
// adds site_tags and backfills it from pre-existing sites.tags /
// pairing_codes.tags rows.
const m100MigrationVersion = "20260802000000_m100_site_tags_registry"

// startPostgresBeforeM100 mirrors startPostgresBeforeM99's container
// bootstrap but stops short of m100, using the bootstrap superuser
// connection directly (this test is about migration/backfill correctness,
// not RLS — RLS on site_tags is covered separately by TestSiteTagsRLS).
func startPostgresBeforeM100(t *testing.T) *db.Pool {
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

	applyMigrationsBeforeM100(t, pool, m100MigrationVersion)
	return pool
}

// applyMigrationsBeforeM100 is a package-local copy of
// applyMigrationsBeforeM99's embedded-FS walk (kept local per that file's own
// stated convention so this file has no compile-order dependency on it).
func applyMigrationsBeforeM100(t *testing.T, pool *db.Pool, stopAt string) {
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

// TestM100Migration_BackfillMatchesExpected_AndIdempotent is the ship-level
// regression test.
func TestM100Migration_BackfillMatchesExpected_AndIdempotent(t *testing.T) {
	pool := startPostgresBeforeM100(t)
	ctx := context.Background()

	var tenantA, tenantB uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO tenants (name, slug) VALUES ($1, $1) RETURNING id`, "m100-a-"+uuid.NewString()[:8]).Scan(&tenantA); err != nil {
		t.Fatalf("seed tenant A: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO tenants (name, slug) VALUES ($1, $1) RETURNING id`, "m100-b-"+uuid.NewString()[:8]).Scan(&tenantB); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}

	// Tenant A: two sites pre-dating m100, carrying overlapping + distinct
	// tags (including a blank entry that must be skipped, matching
	// btrim(name) != '').
	if _, err := pool.Exec(ctx,
		`INSERT INTO sites (tenant_id, url, name, tags) VALUES ($1, $2, 'seed', $3)`,
		tenantA, "https://m100-a1.example.com", []string{"prod", "eu", ""}); err != nil {
		t.Fatalf("seed site A1: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO sites (tenant_id, url, name, tags) VALUES ($1, $2, 'seed', $3)`,
		tenantA, "https://m100-a2.example.com", []string{"prod"}); err != nil {
		t.Fatalf("seed site A2: %v", err)
	}
	// Tenant A: an unexpired, unredeemed pairing code carrying a tag NOT on
	// any site yet — must be backfilled.
	if _, err := pool.Exec(ctx,
		`INSERT INTO pairing_codes (tenant_id, code_hash, site_name, tags, expires_at) VALUES ($1, $2, 'seed', $3, $4)`,
		tenantA, "hash-unexpired-"+uuid.NewString(), []string{"pending-code-tag"}, time.Now().Add(1*time.Hour)); err != nil {
		t.Fatalf("seed unexpired pairing code: %v", err)
	}
	// Tenant A: an EXPIRED pairing code — its tag must NOT be backfilled.
	if _, err := pool.Exec(ctx,
		`INSERT INTO pairing_codes (tenant_id, code_hash, site_name, tags, expires_at) VALUES ($1, $2, 'seed', $3, $4)`,
		tenantA, "hash-expired-"+uuid.NewString(), []string{"expired-code-tag"}, time.Now().Add(-1*time.Hour)); err != nil {
		t.Fatalf("seed expired pairing code: %v", err)
	}
	// Tenant A: a CONSUMED (but unexpired) pairing code — its tag must NOT be
	// backfilled either.
	if _, err := pool.Exec(ctx,
		`INSERT INTO pairing_codes (tenant_id, code_hash, site_name, tags, expires_at, consumed_at) VALUES ($1, $2, 'seed', $3, $4, now())`,
		tenantA, "hash-consumed-"+uuid.NewString(), []string{"consumed-code-tag"}, time.Now().Add(1*time.Hour)); err != nil {
		t.Fatalf("seed consumed pairing code: %v", err)
	}

	// Tenant B: a single distinct tag, proving per-tenant isolation of the
	// backfill (not just per-name global dedup).
	if _, err := pool.Exec(ctx,
		`INSERT INTO sites (tenant_id, url, name, tags) VALUES ($1, $2, 'seed', $3)`,
		tenantB, "https://m100-b1.example.com", []string{"prod"}); err != nil {
		t.Fatalf("seed site B1: %v", err)
	}

	// Finish the boot: applies m100 (table creation + RLS + backfill).
	if err := pool.Migrate(ctx); err != nil {
		t.Fatalf("m100 migration failed: %v", err)
	}

	assertRegistry := func(t *testing.T) {
		t.Helper()
		wantA := map[string]bool{"prod": true, "eu": true, "pending-code-tag": true}
		gotA := registryNamesFor(t, pool, tenantA)
		if !equalNameSets(gotA, wantA) {
			t.Fatalf("tenant A registry = %v, want %v", gotA, wantA)
		}
		if gotA["expired-code-tag"] {
			t.Fatal("an EXPIRED pairing code's tag must NOT be backfilled")
		}
		if gotA["consumed-code-tag"] {
			t.Fatal("a CONSUMED pairing code's tag must NOT be backfilled")
		}
		if gotA[""] {
			t.Fatal("a blank tag entry must NOT be backfilled")
		}

		wantB := map[string]bool{"prod": true}
		gotB := registryNamesFor(t, pool, tenantB)
		if !equalNameSets(gotB, wantB) {
			t.Fatalf("tenant B registry = %v, want %v", gotB, wantB)
		}

		// Colors are left '' (auto) by the backfill.
		var color string
		if err := pool.QueryRow(ctx, `SELECT color FROM site_tags WHERE tenant_id = $1 AND name = 'prod'`, tenantA).Scan(&color); err != nil {
			t.Fatalf("query color: %v", err)
		}
		if color != "" {
			t.Fatalf("backfilled color = %q, want '' (auto)", color)
		}
	}
	assertRegistry(t)

	// Idempotency: re-execute m100's own SQL text a second time directly
	// (bypassing schema_migrations tracking) and assert nothing changed.
	body, err := fs.ReadFile(migrations.FS, m100MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m100 migration body: %v", err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("re-apply m100 migration SQL: %v", err)
	}
	assertRegistry(t)

	var total int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM site_tags`).Scan(&total); err != nil {
		t.Fatalf("total count: %v", err)
	}
	if total != 4 { // A: prod, eu, pending-code-tag; B: prod
		t.Fatalf("total site_tags rows after re-apply = %d, want 4 (idempotent)", total)
	}
}

// TestM100Migration_OverLengthTagSkipped_MigrationSucceeds is the CRITICAL
// regression this fix set was written for (adversarial-verify: empirically
// reproduced SQLSTATE 23514 / migration tx rollback / boot crash-loop before
// the fix). A real pre-m100 deployment could already have an over-length tag
// on sites.tags or pairing_codes.tags (two write paths never enforced the
// 64-char cap until this release — see sitetag_validation_regression_test.go).
// Seeds exactly that pre-existing state (bypassing the now-fixed app-layer
// validation by writing directly to the DB, the way a truly historical row
// would already exist) across BOTH backfill sources, then proves m100 still
// APPLIES SUCCESSFULLY: the over-length tags are simply left off the
// registry (they remain on sites.tags/pairing_codes.tags, harmless), while
// every other, valid-length tag is still backfilled normally.
func TestM100Migration_OverLengthTagSkipped_MigrationSucceeds(t *testing.T) {
	pool := startPostgresBeforeM100(t)
	ctx := context.Background()

	var tenant uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO tenants (name, slug) VALUES ($1, $1) RETURNING id`,
		"m100-overlen-"+uuid.NewString()[:8]).Scan(&tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	longTag := strings.Repeat("a", 65) // one past site_tags' char_length <= 64 CHECK

	// A site pre-dating m100's own length validation, carrying BOTH an
	// over-length tag and a normal one.
	if _, err := pool.Exec(ctx,
		`INSERT INTO sites (tenant_id, url, name, tags) VALUES ($1, $2, 'seed', $3)`,
		tenant, "https://m100-overlen-site.example.com", []string{longTag, "prod"}); err != nil {
		t.Fatalf("seed site with over-length tag: %v", err)
	}
	// An unexpired, unredeemed pairing code carrying ONLY an over-length tag.
	if _, err := pool.Exec(ctx,
		`INSERT INTO pairing_codes (tenant_id, code_hash, site_name, tags, expires_at) VALUES ($1, $2, 'seed', $3, $4)`,
		tenant, "hash-overlen-"+uuid.NewString(), []string{longTag}, time.Now().Add(1*time.Hour)); err != nil {
		t.Fatalf("seed pairing code with over-length tag: %v", err)
	}

	// The migration must APPLY SUCCESSFULLY — this is the CRITICAL assertion.
	if err := pool.Migrate(ctx); err != nil {
		t.Fatalf("m100 migration must succeed even with a pre-existing over-length tag, got: %v", err)
	}

	got := registryNamesFor(t, pool, tenant)
	if got[longTag] {
		t.Fatal("the 65-char tag must NOT be backfilled into site_tags")
	}
	if !got["prod"] {
		t.Fatalf("the normal-length 'prod' tag must still be backfilled; registry = %v", got)
	}
	if len(got) != 1 {
		t.Fatalf("registry = %v, want exactly {prod: true}", got)
	}

	// The over-length tag is untouched on sites.tags (not silently dropped
	// from the site itself — only excluded from the registry).
	var siteTags []string
	if err := pool.QueryRow(ctx, `SELECT tags FROM sites WHERE tenant_id = $1`, tenant).Scan(&siteTags); err != nil {
		t.Fatalf("query site tags: %v", err)
	}
	foundLong := false
	for _, tg := range siteTags {
		if tg == longTag {
			foundLong = true
		}
	}
	if !foundLong {
		t.Fatal("the over-length tag must remain on sites.tags even though it is excluded from the registry")
	}
}

func registryNamesFor(t *testing.T, pool *db.Pool, tenant uuid.UUID) map[string]bool {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT name FROM site_tags WHERE tenant_id = $1`, tenant)
	if err != nil {
		t.Fatalf("query registry names: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan name: %v", err)
		}
		out[name] = true
	}
	return out
}

func equalNameSets(got, want map[string]bool) bool {
	if len(got) != len(want) {
		return false
	}
	for k := range want {
		if !got[k] {
			return false
		}
	}
	return true
}
