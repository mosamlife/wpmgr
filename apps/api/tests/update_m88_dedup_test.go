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
	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

// m88MigrationVersion is the embedded migration filename (sans .sql) that adds
// update_tasks_inflight_target_idx.
const m88MigrationVersion = "20260721000000_m88_update_tasks_inflight_dedup"

// m141MigrationVersion is m141, PR #775's prefill that runs ahead of m88 and
// does its dedup UPDATE under NO FORCE / row_security=off so it actually sees
// pre-existing rows under the production migrator role. The harness below
// stops short of THIS version (not m88's, which now sorts after it) so the
// test can seed the exact pre-m88 duplicate-row scenario before either
// migration runs.
const m141MigrationVersion = "20260720120000_m141_update_tasks_inflight_dedup_prefill"

// startPostgresBeforeM88 mirrors startPostgres's container bootstrap but
// applies every embedded migration UP TO (not including) m88 AS wpmgr_owner —
// the NOSUPERUSER NOBYPASSRLS role production's WPMGR_DB_MIGRATION_DSN
// migrates as, not the container's bootstrap superuser (see startPostgres's
// doc comment in rls_integration_test.go for why that distinction is the
// whole point of this harness). The caller seeds pre-m88 data — through the
// returned admin pool, since update_tasks is FORCE ROW LEVEL SECURITY and the
// seed step needs to plant rows without an app.tenant_id/app.agent GUC in
// scope — then finishes the boot with owner.Migrate(ctx), which applies
// exactly m88 (and any later migration) under the same role and RLS exposure
// production's migrator has.
//
// Returns (admin, owner): admin is the bootstrap superuser, kept for seeding
// and for the test's own read-back assertions (neither is under test here);
// owner is the NOSUPERUSER NOBYPASSRLS role and is the one the test must call
// Migrate(ctx) on.
func startPostgresBeforeM88(t *testing.T) (admin *db.Pool, owner *db.Pool) {
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

	applyMigrationsBefore(t, owner, m141MigrationVersion)
	return admin, owner
}

// assertUpdateTasksForceIntact fails the test unless update_tasks still
// carries both relrowsecurity and relforcerowsecurity: m141 lifts FORCE for
// the length of its dedup UPDATE and must restore it before it returns, and
// it raises on its own if it doesn't — this is the test-side check on top of
// that.
func assertUpdateTasksForceIntact(t *testing.T, pool *db.Pool) {
	t.Helper()
	ctx := context.Background()
	var rowsec, forcesec bool
	if err := pool.QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = 'public.update_tasks'::regclass`,
	).Scan(&rowsec, &forcesec); err != nil {
		t.Fatalf("read update_tasks pg_class: %v", err)
	}
	if !rowsec || !forcesec {
		t.Fatalf("update_tasks relrowsecurity=%t relforcerowsecurity=%t, want true, true", rowsec, forcesec)
	}
}

// updateTasksChecksum returns a deterministic hash of every update_tasks row,
// used to prove a migration run touched no data.
func updateTasksChecksum(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var sum string
	if err := pool.QueryRow(context.Background(),
		`SELECT md5(coalesce(string_agg(row(u.*)::text, '|' ORDER BY u.id), '')) FROM update_tasks u`,
	).Scan(&sum); err != nil {
		t.Fatalf("checksum update_tasks: %v", err)
	}
	return sum
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
	// Fixed by PR #775/#780's m141: it runs ahead of m88 (the harness now
	// stops before m141, not m88 — see m141MigrationVersion), does m88's own
	// dedup UPDATE under NO FORCE / row_security=off so the production
	// migrator role actually sees the pre-existing duplicates, then creates
	// m88's unique index itself. m88 then finds nothing left to dedup and the
	// index already present, and changes nothing.
	pool, owner := startPostgresBeforeM88(t)
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

	// Finish the boot: this applies m88, AS wpmgr_owner — exactly the role
	// and RLS exposure cmd/wpmgr's real migrator has. It must succeed even
	// though the table already carries the duplicate in-flight rows above.
	if err := owner.Migrate(ctx); err != nil {
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

	assertUpdateTasksForceIntact(t, pool)
}

// TestM141FiresOnPreexistingDuplicatesUnderOwnerRole is the fires proof for
// m141: with m141 recorded as applied but never actually run (a binary that
// predates PR #775's fix, exactly as production saw it), m88 must reproduce
// the original ship-blocker failure — 23505 on update_tasks_inflight_target_idx
// — because its own dedup UPDATE cannot see the duplicates under FORCE ROW
// LEVEL SECURITY and no app.* GUC. Unmarking m141 and migrating again must
// then succeed.
func TestM141FiresOnPreexistingDuplicatesUnderOwnerRole(t *testing.T) {
	pool, owner := startPostgresBeforeM88(t)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "m141-fires")
	var siteID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, $3) RETURNING id`,
		tenant, "https://m141-fires.example.com", "m141 fires site",
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
	base := time.Now().Add(-time.Hour)
	for i, status := range []string{"pending", "running"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO update_tasks
			   (run_id, tenant_id, site_id, target_type, target_slug, status, created_at, updated_at)
			 VALUES ($1, $2, $3, 'plugin', 'akismet', $4, $5, $5)`,
			runID, tenant, siteID, status, base.Add(time.Duration(i)*time.Minute),
		); err != nil {
			t.Fatalf("seed duplicate task %d: %v", i, err)
		}
	}

	// Simulate the stale binary: m141 recorded as applied, never run.
	scopePrefillMark(t, owner, m141MigrationVersion)
	err := owner.Migrate(ctx)
	if err == nil {
		t.Fatal("expected m88 to fail creating its unique index on pre-existing duplicates when m141 is skipped")
	}
	t.Logf("Migrate error (expected): %v", err)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("got %v, want a 23505 unique_violation", err)
	}
	if !strings.Contains(err.Error(), "update_tasks_inflight_target_idx") {
		t.Fatalf("error does not name update_tasks_inflight_target_idx: %v", err)
	}

	// The fix: unmark m141 and migrate again.
	scopePrefillUnmark(t, owner, m141MigrationVersion)
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate with m141 present: %v", err)
	}
	assertUpdateTasksForceIntact(t, pool)
}

// m141EarlyReturnGuard is the exact text of m141's probe/early-return check
// (20260720120000_m141_update_tasks_inflight_dedup_prefill.sql). Kept as a
// standalone constant so TestM141LateRunAfterM88AlreadyApplied's mutation can
// assert it actually matched before stripping it — see stripOnce.
const m141EarlyReturnGuard = `    IF to_regclass('public.update_tasks_inflight_target_idx') IS NOT NULL THEN
        RETURN;
    END IF;
`

// TestM141LateRunAfterM88AlreadyApplied covers the "LATE RUN" case m141's own
// doc comment claims: on a database that reached head WITHOUT m141 (m88 ran
// its own original code on a clean table and created the index itself), m141
// arriving afterward must be a pure no-op — no error, no data change, NO LOCK
// TAKEN — since its probe finds the index already present.
//
// Proof structure (see migration_late_run_lock_test.go's doc comment for the
// shared helpers): a second connection holds ROW EXCLUSIVE on update_tasks
// throughout. First, an IN-MEMORY copy of m141 with its early-return check
// stripped is run against that hold and must fail (the guard is load-bearing:
// this is what a security reviewer measured taking AccessExclusiveLock and
// timing out against a live writer). Then the REAL, unmutated m141 is run
// late via owner.Migrate and must finish well inside the bound a lock wait
// would blow, and the real file is re-applied in its own explicit transaction
// to prove pg_locks shows nothing against update_tasks for that backend
// before commit.
func TestM141LateRunAfterM88AlreadyApplied(t *testing.T) {
	pool, owner := startPostgresBeforeM88(t)
	ctx := context.Background()

	// Reach head with m141 withheld: m88 runs unaided on an empty table.
	scopePrefillMark(t, owner, m141MigrationVersion)
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate to head with m141 withheld: %v", err)
	}
	var idxCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'update_tasks_inflight_target_idx'`,
	).Scan(&idxCount); err != nil {
		t.Fatalf("check index: %v", err)
	}
	if idxCount != 1 {
		t.Fatal("update_tasks_inflight_target_idx should already exist from m88")
	}

	// A REAL row in the target table — the gap this test used to have: an
	// empty-table checksum compares equal no matter what a broken late run
	// does, so it could never fail on data-corruption grounds.
	tenant := seedTenant(t, pool, "m141-laterun")
	var siteID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, $3) RETURNING id`,
		tenant, "https://m141-laterun.example.com", "m141 laterun site",
	).Scan(&siteID); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	var runID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO update_runs (tenant_id, status) VALUES ($1, 'running') RETURNING id`, tenant,
	).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO update_tasks (run_id, tenant_id, site_id, target_type, target_slug, status)
		 VALUES ($1, $2, $3, 'plugin', 'akismet', 'pending')`,
		runID, tenant, siteID,
	); err != nil {
		t.Fatalf("seed a real in-flight task: %v", err)
	}

	body, err := fs.ReadFile(migrations.FS, m141MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m141 migration body: %v", err)
	}
	mutated := stripOnce(t, string(body), m141EarlyReturnGuard)

	release := holdRowExclusiveOpen(t, owner, "update_tasks", "updated_at")
	defer release()

	// Fires: without the guard, m141 would take AccessExclusiveLock (via its
	// NO FORCE ROW LEVEL SECURITY toggle) on update_tasks and block behind the
	// held ROW EXCLUSIVE until its own 5s lock_timeout fires.
	mutatedMigrationMustBlockOrError(t, owner, mutated)

	before := updateTasksChecksum(t, pool)

	// Does not over-fire: the REAL m141 arrives late (unmark and migrate
	// again) and must finish fast despite the lock still held above.
	scopePrefillUnmark(t, owner, m141MigrationVersion)
	assertMigrateStaysUnderLockBound(t, owner, ctx)

	after := updateTasksChecksum(t, pool)
	if before != after {
		t.Fatalf("m141's late run changed update_tasks data: before=%s after=%s", before, after)
	}

	// Re-apply the real file directly and prove it took no lock on
	// update_tasks at all, still with the ROW EXCLUSIVE holder open.
	assertFileTakesNoLockOnRelation(t, owner, "public.update_tasks", string(body))

	release()
	assertUpdateTasksForceIntact(t, pool)
}

// TestM88MigrationIsNoopWhenNoDuplicatesExist proves the common case (a
// database with no pre-existing collisions, e.g. every fresh install and
// every deployment that never hit the pre-m88 race) is unaffected: the
// pre-dedup UPDATE touches nothing and the index is created normally.
func TestM88MigrationIsNoopWhenNoDuplicatesExist(t *testing.T) {
	pool, owner := startPostgresBeforeM88(t)
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

	if err := owner.Migrate(ctx); err != nil {
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
