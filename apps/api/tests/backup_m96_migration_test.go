package tests

// backup_m96_migration_test.go — ship-blocker regression test for the m96
// migration's data-mutating dedup UPDATE (GH #168 P1c). Mirrors
// update_m88_dedup_test.go's pattern exactly: boot a container, apply every
// embedded migration UP TO (not including) m96, seed the EXACT pre-m96
// duplicate-row scenario the migration must heal before creating the unique
// index, then finish the boot and assert the outcome. Without this test the
// dedup UPDATE branch is a no-op in CI (every other GH #168 fixture only ever
// creates ONE completed row per generation), so a regression in the dedup
// logic itself would never be caught.

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

	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

// m96MigrationVersion is the embedded migration filename (sans .sql) that
// adds backup_snapshots_chain_gen_completed_uidx. The harness below stops
// short of applying it so the test can seed the exact pre-m96 duplicate-row
// scenario the migration must heal before creating the unique index.
const m96MigrationVersion = "20260729000000_m96_backup_chain_gen_completed_uidx"

// startPostgresBeforeM96 mirrors startPostgresBeforeM88's container bootstrap
// but stops short of m96, using the bootstrap superuser connection (same
// convention as the m88 harness — this test is about migration/dedup
// correctness, not RLS).
func startPostgresBeforeM96(t *testing.T) *db.Pool {
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

	applyMigrationsBeforeM96(t, pool, m96MigrationVersion)
	return pool
}

// applyMigrationsBeforeM96 is a local copy of update_m88_dedup_test.go's
// applyMigrationsBefore (same package, but kept local rather than shared so
// this file has no compile-order dependency on that one — both re-implement
// the same embedded-FS walk internal/db/migrate.go uses).
func applyMigrationsBeforeM96(t *testing.T, pool *db.Pool, stopAt string) {
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

// seedSiteOnAdminPool inserts an enrolled site row directly on the caller's
// pool. Unlike the shared seedSite helper (backup_integration_test.go), this
// does NOT go through connectAdmin — startPostgresBeforeM96 (like
// startPostgresBeforeM88) already returns the bootstrap SUPERUSER connection
// directly, with nothing recorded in the adminDSNs map connectAdmin reads.
func seedSiteOnAdminPool(t *testing.T, pool *db.Pool, tenant uuid.UUID, url string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var siteID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO sites (tenant_id, url, name, age_recipient, enrolled_at)
		 VALUES ($1, $2, $3, $4, now())
		 RETURNING id`,
		tenant, url, "m96 test site", testRecipient,
	).Scan(&siteID); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	return siteID
}

// TestM96MigrationDedupesPreexistingCompletedDuplicates is the ship-blocker
// regression test: on a LIVE database that already ran the pre-#168-fix code,
// TWO status='completed' rows can already exist for the same (chain_id,
// generation) — that is precisely the bug m96's unique index closes, and
// prior to this migration there was nothing to clean them up, so they persist
// indefinitely. CREATE UNIQUE INDEX would fail outright on that duplicate
// data, and since the migration runner wraps each migration in a tx and
// aborts boot on error, that would hard-block the API from booting for every
// affected deployment. This test seeds the exact collision, proves the
// migration heals it deterministically (lowest id survives, mirroring
// chainGenWinner's own tiebreak), and proves the surviving chain still
// resolves end-to-end via PlanRestore.
func TestM96MigrationDedupesPreexistingCompletedDuplicates(t *testing.T) {
	pool := startPostgresBeforeM96(t)
	store := startBlobstore(t)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "m96-dedup")
	siteID := seedSiteOnAdminPool(t, pool, tenant, "https://m96-dedup.example.com")

	now := time.Now()
	gen0ID := uuid.New()
	chainID := gen0ID // base's chain_id is its own id, by project convention.

	// gen0: the chain base (completed, non-incremental), carrying a files-list
	// entry so the chain is archive-delta (ADR-051).
	seedGH168Chunk(t, pool, store, tenant, "fl0", now)
	seedGH168Chunk(t, pool, store, tenant, "part0", now)
	seedGH168Snapshot(t, pool, tenant, siteID, gen0ID, chainID, 0, backup.StatusCompleted, false,
		archiveDeltaEntries(0, "fl0", "part0"), now)

	// TWO completed rows at generation 1 — the exact pre-m96 collision. idA and
	// idB both carry a REAL, resolvable manifest (a genuine race between two
	// submissions completing for the same generation, not a failed/empty
	// retry — that scenario is already covered by the GH #168 regression test
	// and by P1a/P1b; this test is specifically about the MIGRATION's dedup of
	// two live completed rows). idB's manifest is deliberately DIFFERENT
	// (distinct chunk hashes) so the assertions can tell which row's data the
	// post-migration chain actually resolves through.
	idA := uuid.New()
	idB := uuid.New()
	if idA.String() > idB.String() {
		idA, idB = idB, idA
	}
	// idA now has the LOWER id — the survivor chainGenWinner/m96 both pick.
	seedGH168Chunk(t, pool, store, tenant, "fl1-a", now)
	seedGH168Chunk(t, pool, store, tenant, "part1-a", now)
	seedGH168Snapshot(t, pool, tenant, siteID, idA, chainID, 1, backup.StatusCompleted, true,
		archiveDeltaEntries(1, "fl1-a", "part1-a"), now.Add(time.Second))

	seedGH168Chunk(t, pool, store, tenant, "fl1-b", now)
	seedGH168Chunk(t, pool, store, tenant, "part1-b", now)
	seedGH168Snapshot(t, pool, tenant, siteID, idB, chainID, 1, backup.StatusCompleted, true,
		archiveDeltaEntries(1, "fl1-b", "part1-b"), now.Add(2*time.Second))

	// Finish the boot: this applies m96. It must succeed even though the table
	// already carries the duplicate completed rows above.
	if err := pool.Migrate(ctx); err != nil {
		t.Fatalf("m96 migration failed on a table with pre-existing completed duplicates: %v", err)
	}

	// idA (lower id) is untouched — still completed.
	var statusA, statusB, errA, errB string
	if err := pool.QueryRow(ctx, `SELECT status, error FROM backup_snapshots WHERE id = $1`, idA).Scan(&statusA, &errA); err != nil {
		t.Fatalf("get idA: %v", err)
	}
	if statusA != backup.StatusCompleted {
		t.Fatalf("idA (lower id, should survive) status = %q, want completed", statusA)
	}
	if errA != "" {
		t.Fatalf("idA (survivor) error = %q, want untouched empty string", errA)
	}

	// idB (higher id) was demoted to failed, with the m96 dedup marker message.
	if err := pool.QueryRow(ctx, `SELECT status, error FROM backup_snapshots WHERE id = $1`, idB).Scan(&statusB, &errB); err != nil {
		t.Fatalf("get idB: %v", err)
	}
	if statusB != backup.StatusFailed {
		t.Fatalf("idB (higher id, should be demoted) status = %q, want failed", statusB)
	}
	if !strings.Contains(errB, "superseded duplicate completed snapshot at the same chain generation (m96 dedup)") {
		t.Fatalf("idB error = %q, want it to carry the m96 dedup marker message", errB)
	}

	// Exactly one completed row remains at (chainID, generation=1).
	var completedCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM backup_snapshots WHERE chain_id = $1 AND generation = 1 AND status = 'completed'`,
		chainID).Scan(&completedCount); err != nil {
		t.Fatalf("count completed at gen1: %v", err)
	}
	if completedCount != 1 {
		t.Fatalf("completed rows at (chainID, gen1) = %d, want exactly 1 after dedup", completedCount)
	}

	// The partial unique index now exists.
	var idxCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'backup_snapshots_chain_gen_completed_uidx'`,
	).Scan(&idxCount); err != nil {
		t.Fatalf("check index: %v", err)
	}
	if idxCount != 1 {
		t.Fatal("backup_snapshots_chain_gen_completed_uidx was not created")
	}

	// The index is actually enforcing going forward: a fresh completed
	// duplicate insert at the same (chain_id, generation) now fails.
	_, err := pool.Exec(ctx,
		`INSERT INTO backup_snapshots (tenant_id, site_id, kind, status, age_recipient, is_incremental, chain_id, generation)
		 VALUES ($1, $2, 'files', 'completed', $3, true, $4, 1)`,
		tenant, siteID, testRecipient, chainID)
	if err == nil {
		t.Fatal("expected the partial unique index to reject a second completed row at (chain_id, generation) after m96")
	}

	// The surviving chain still resolves end-to-end: PlanRestore for idA (the
	// chain tip) must succeed and resolve idA's OWN chunks — never idB's
	// (now-demoted) manifest data. planRestoreChain's OWN strict chain-
	// integrity CHECK 1 counts EVERY row regardless of status, so the
	// now-failed idB row (still physically present — the migration demotes,
	// never deletes) must be cleaned up first, exactly like an operator would
	// after m96 flags a duplicate: this is the SAME chain_has_dependents-safe
	// delete path GH #168's own regression test exercises, not something
	// special-cased for this test.
	svc := newBackupService(t, pool, store, stubSiteLookup{info: enrolledSiteInfo(siteID, "https://m96-dedup.example.com")}, &stubEnqueuer{})
	if err := svc.DeleteSnapshotForUser(ctx, tenant, idB); err != nil {
		t.Fatalf("DeleteSnapshotForUser(idB, demoted duplicate): %v", err)
	}
	plan, _, _, err := svc.PlanRestore(ctx, tenant, idA, backup.RestoreSelection{Full: true}, "restore-m96", "")
	if err != nil {
		t.Fatalf("PlanRestore(idA) after m96 dedup + duplicate cleanup: unexpected error: %v", err)
	}
	gotHashes := map[string]bool{}
	for _, e := range plan.Manifest.Entries {
		for _, c := range e.Chunks {
			gotHashes[c.Hash] = true
		}
	}
	for _, want := range []string{"part0", "part1-a"} {
		if !gotHashes[want] {
			t.Errorf("restore plan missing %q — the surviving (lower-id) chain member's data", want)
		}
	}
	for _, unwanted := range []string{"part1-b"} {
		if gotHashes[unwanted] {
			t.Errorf("restore plan resolved %q — the DEMOTED (higher-id, non-completed) duplicate's data leaked into the surviving chain", unwanted)
		}
	}
}

// TestM96MigrationIsNoopWhenNoDuplicatesExist proves the common case (a
// database with no pre-existing (chain_id, generation) collisions, i.e. every
// fresh install and every deployment that never hit the pre-#168 race) is
// unaffected: the dedup UPDATE touches nothing and the index is created
// normally.
func TestM96MigrationIsNoopWhenNoDuplicatesExist(t *testing.T) {
	pool := startPostgresBeforeM96(t)
	store := startBlobstore(t)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "m96-noop")
	siteID := seedSiteOnAdminPool(t, pool, tenant, "https://m96-noop.example.com")

	now := time.Now()
	gen0ID := uuid.New()
	chainID := gen0ID
	seedGH168Chunk(t, pool, store, tenant, "noop-fl0", now)
	seedGH168Chunk(t, pool, store, tenant, "noop-part0", now)
	seedGH168Snapshot(t, pool, tenant, siteID, gen0ID, chainID, 0, backup.StatusCompleted, false,
		archiveDeltaEntries(0, "noop-fl0", "noop-part0"), now)

	if err := pool.Migrate(ctx); err != nil {
		t.Fatalf("m96 migration failed on a clean table: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM backup_snapshots WHERE id = $1`, gen0ID).Scan(&status); err != nil {
		t.Fatalf("get gen0: %v", err)
	}
	if status != backup.StatusCompleted {
		t.Fatalf("single completed snapshot status = %q, want untouched completed", status)
	}

	var idxCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'backup_snapshots_chain_gen_completed_uidx'`,
	).Scan(&idxCount); err != nil {
		t.Fatalf("check index: %v", err)
	}
	if idxCount != 1 {
		t.Fatal("backup_snapshots_chain_gen_completed_uidx was not created")
	}
}
