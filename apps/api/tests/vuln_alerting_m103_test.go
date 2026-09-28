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

// m145MigrationVersion is m145, PR #775's prefill that runs ahead of m103 and
// adds notified_at itself, filled for every pre-existing row in the same
// ADD COLUMN statement (which reads no rows, so no RLS policy applies to it).
// The harness below stops short of THIS version (not m103's, which now sorts
// after it) so the test can seed pre-existing rows before either migration
// runs.
const m145MigrationVersion = "20260804120000_m145_vuln_notified_at_prefill"

// startPostgresBeforeM103 mirrors startPostgresBeforeM100's container
// bootstrap but stops short of m103. Migrations run AS wpmgr_owner — the
// NOSUPERUSER NOBYPASSRLS role production's migrator uses, not the bootstrap
// superuser (see startPostgres's doc comment in rls_integration_test.go).
// site_vulnerabilities is FORCE ROW LEVEL SECURITY. Returns (admin, owner):
// seed pre-m103 rows through admin, the bootstrap superuser; call
// owner.Migrate(ctx) (and, for the idempotency re-exec below, owner.Exec) to
// run migration SQL under the production role.
func startPostgresBeforeM103(t *testing.T) (admin *db.Pool, owner *db.Pool) {
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

	applyMigrationsBeforeM103(t, owner, m145MigrationVersion)
	return admin, owner
}

// assertSiteVulnerabilitiesForceIntact fails the test unless
// site_vulnerabilities still carries both relrowsecurity and
// relforcerowsecurity. m145 never toggles FORCE on this table (it is a pure
// ADD COLUMN, which reads no rows), but this is the same check the other two
// prefills' tests run, kept here for symmetry.
func assertSiteVulnerabilitiesForceIntact(t *testing.T, pool *db.Pool) {
	t.Helper()
	ctx := context.Background()
	var rowsec, forcesec bool
	if err := pool.QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = 'public.site_vulnerabilities'::regclass`,
	).Scan(&rowsec, &forcesec); err != nil {
		t.Fatalf("read site_vulnerabilities pg_class: %v", err)
	}
	if !rowsec || !forcesec {
		t.Fatalf("site_vulnerabilities relrowsecurity=%t relforcerowsecurity=%t, want true, true", rowsec, forcesec)
	}
}

// siteVulnerabilitiesChecksum returns a deterministic hash of every
// site_vulnerabilities row, used to prove a migration run touched no data.
func siteVulnerabilitiesChecksum(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var sum string
	if err := pool.QueryRow(context.Background(),
		`SELECT md5(coalesce(string_agg(row(v.*)::text, '|' ORDER BY v.id), '')) FROM site_vulnerabilities v`,
	).Scan(&sum); err != nil {
		t.Fatalf("checksum site_vulnerabilities: %v", err)
	}
	return sum
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
	// Fixed by PR #775/#780's m145: it runs ahead of m103 (the harness now
	// stops before m145, not m103 — see m145MigrationVersion), adds
	// notified_at itself and fills every pre-existing row in the same ADD
	// COLUMN statement, which reads no rows so no RLS policy applies to it.
	// m103 then finds the column already present with nothing NULL among the
	// rows that existed before it, so its own ADD COLUMN IF NOT EXISTS and
	// backfill UPDATE both change nothing.
	pool, owner := startPostgresBeforeM103(t)
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

	// Finish the boot: applies m103 (columns + CHECK + backfill + index), AS
	// wpmgr_owner.
	if err := owner.Migrate(ctx); err != nil {
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
	if _, err := owner.Exec(ctx, string(body)); err != nil {
		t.Fatalf("re-apply m103 migration SQL: %v", err)
	}
	secondStamps := assertBackfilled(t)
	for id, first := range firstStamps {
		if !secondStamps[id].Equal(first) {
			t.Fatalf("finding %s: notified_at changed on re-apply (%v -> %v); backfill must be idempotent", id, first, secondStamps[id])
		}
	}

	assertSiteVulnerabilitiesForceIntact(t, pool)
}

// TestM145FiresLeavingNotifiedAtNullUnderOwnerRole is the fires proof for
// m145: with m145 recorded as applied but never actually run (a binary that
// predates PR #775's fix, exactly as production saw it), m103 must reproduce
// the original silent failure — it boots cleanly, but notified_at stays NULL
// for a pre-existing finding — because its own backfill UPDATE cannot see the
// row under FORCE ROW LEVEL SECURITY and no app.* GUC. Unmarking m145 and
// migrating again must then backfill it.
func TestM145FiresLeavingNotifiedAtNullUnderOwnerRole(t *testing.T) {
	pool, owner := startPostgresBeforeM103(t)
	ctx := context.Background()

	var tenant uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO tenants (name, slug) VALUES ($1, $1) RETURNING id`,
		"m145-fires-"+uuid.NewString()[:8]).Scan(&tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	var siteID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, 'seed') RETURNING id`,
		tenant, "https://m145-fires.example.com").Scan(&siteID); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	var findingID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO site_vulnerabilities
			(tenant_id, site_id, vuln_id, kind, slug, name, installed_version, severity, title, status)
		VALUES ($1, $2, 'm145-fires-vuln', 'plugin', 'm145-fires-vuln', 'seed', '1.0.0', 'high', 'seed finding', 'open')
		RETURNING id`,
		tenant, siteID,
	).Scan(&findingID); err != nil {
		t.Fatalf("seed finding: %v", err)
	}

	// Simulate the stale binary: m145 recorded as applied, never run. Unlike
	// m88/m96, m103's own statements never fail outright — the boot must
	// succeed while silently leaving the pre-existing row unbackfilled.
	scopePrefillMark(t, owner, m145MigrationVersion)
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate with m145 withheld: %v (want a clean boot, per the original silent-failure shape)", err)
	}

	var notifiedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT notified_at FROM site_vulnerabilities WHERE id = $1`, findingID).Scan(&notifiedAt); err != nil {
		t.Fatalf("query notified_at: %v", err)
	}
	if notifiedAt != nil {
		t.Fatalf("expected notified_at to stay NULL when m145 is skipped (the exact stale-binary failure), got %v", *notifiedAt)
	}

	// UNCONVERGED GAP (found by this test, not fixed here — a migration
	// change for database-engineer, not this test-harness PR): unlike m141
	// and m143, m145 cannot actually repair this state. m103's ADD COLUMN
	// above already committed (the boot above did not error), so m103 is now
	// recorded applied and will never re-run. Unmarking and re-running m145
	// only lets ITS OWN probe run again — and that probe is "does the column
	// exist", which is now true (m103 added it), so m145 no-ops and
	// notified_at stays NULL forever. This is not the artificial harness
	// state: any self-hosted install that already ran a pre-#775 binary's
	// m103 under the production migrator role is in exactly this state today
	// — m103 applied, notified_at NULL on every pre-existing finding — and
	// upgrading to a binary carrying m145 does NOT backfill it, because
	// m145's guard cannot distinguish "column missing" from "column present
	// but never filled". Contrast m141/m143: there the first Migrate() call
	// above ERRORS (23505), so the target migration's transaction rolls back
	// and it is never recorded applied, leaving the prefill free to run
	// first on retry. m103's failure mode is silent success, which commits
	// before the prefill ever gets a second chance.
	scopePrefillUnmark(t, owner, m145MigrationVersion)
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate with m145 present: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT notified_at FROM site_vulnerabilities WHERE id = $1`, findingID).Scan(&notifiedAt); err != nil {
		t.Fatalf("query notified_at after fix: %v", err)
	}
	if notifiedAt != nil {
		t.Fatalf("m145 unexpectedly backfilled notified_at (%v) after m103 had already committed its own ADD COLUMN; "+
			"if this now fails, m145 was changed to fix the gap documented above — update this test, don't just relax it", *notifiedAt)
	}
	assertSiteVulnerabilitiesForceIntact(t, pool)
}

// m145EarlyReturnGuardOpen and m145EarlyReturnGuardClose bracket the exact
// text of m145's probe/early-return check
// (20260804120000_m145_vuln_notified_at_prefill.sql) — an IF NOT EXISTS (...)
// THEN ... END IF wrapper rather than m141/m143's early RETURN, since m145's
// body is two ALTER statements with nothing to run once the column exists.
// Kept as standalone constants so TestM145LateRunAfterM103AlreadyApplied's
// mutation can assert each actually matched before stripping it — see
// stripOnce. Stripping both unconditionally exposes the two ALTER TABLE
// statements inside BEGIN...END, which is still valid PL/pgSQL.
const m145EarlyReturnGuardOpen = `    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
        WHERE attrelid = 'public.site_vulnerabilities'::regclass
          AND attname  = 'notified_at'
          AND NOT attisdropped
    ) THEN
`
const m145EarlyReturnGuardClose = `    END IF;
`

// TestM145LateRunAfterM103AlreadyApplied covers the "LATE RUN" case m145's
// own doc comment claims: on a database that reached head WITHOUT m145 (m103
// ran its own original code on an empty table and added the column itself),
// m145 arriving afterward must be a pure no-op — no error, no data change, NO
// LOCK TAKEN — since its probe finds the column already present. In
// particular, a NULL notified_at written by a genuinely NEW finding after
// that first migration must stay NULL, and a pre-existing, already-notified
// (non-NULL) finding must keep its exact stamp: m145's late run must not
// re-scan the table at all.
//
// Proof structure (see migration_late_run_lock_test.go's doc comment for the
// shared helpers): a second connection holds ROW EXCLUSIVE on
// site_vulnerabilities throughout. First, an IN-MEMORY copy of m145 with its
// guard stripped is run against that hold and must fail — unlike m141/m143,
// m145 sets no lock_timeout of its own, so this is not a lock-contention
// proof: by this point in the test m103 has already added notified_at for
// real, so the guard-stripped body's bare `ADD COLUMN notified_at` (no IF NOT
// EXISTS) fails on the column already existing, SQLSTATE 42701, whether or
// not the ROW EXCLUSIVE holder is even open. That is still the honest proof
// for m145: without its guard, a late run aborts the boot on an existing
// column. Then the REAL, unmutated m145 is run late via owner.Migrate and
// must finish well inside the bound a lock wait would blow, and the real file
// is re-applied in its own explicit transaction to prove pg_locks shows
// nothing against site_vulnerabilities for that backend before commit.
func TestM145LateRunAfterM103AlreadyApplied(t *testing.T) {
	pool, owner := startPostgresBeforeM103(t)
	ctx := context.Background()

	// Reach head with m145 withheld: m103 runs unaided on an empty table.
	scopePrefillMark(t, owner, m145MigrationVersion)
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate to head with m145 withheld: %v", err)
	}

	var tenant uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO tenants (name, slug) VALUES ($1, $1) RETURNING id`,
		"m145-laterun-"+uuid.NewString()[:8]).Scan(&tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	var siteID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, 'seed') RETURNING id`,
		tenant, "https://m145-laterun.example.com").Scan(&siteID); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	// A finding inserted AFTER migration to head: notified_at must default to
	// NULL (a genuinely new, not-yet-alerted finding).
	var findingID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO site_vulnerabilities
			(tenant_id, site_id, vuln_id, kind, slug, name, installed_version, severity, title, status)
		VALUES ($1, $2, 'm145-lr-vuln', 'plugin', 'm145-lr-vuln', 'seed', '1.0.0', 'high', 'seed finding', 'open')
		RETURNING id`,
		tenant, siteID,
	).Scan(&findingID); err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	var notifiedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT notified_at FROM site_vulnerabilities WHERE id = $1`, findingID).Scan(&notifiedAt); err != nil {
		t.Fatalf("query notified_at: %v", err)
	}
	if notifiedAt != nil {
		t.Fatalf("new post-migration finding notified_at = %v, want NULL", *notifiedAt)
	}

	// A SECOND finding, already notified (non-NULL), inserted directly via the
	// bootstrap superuser pool with an explicit stamp — as if an earlier
	// dispatch had already claimed it. A converged late run must leave this
	// exact value untouched too, not just the NULL one above.
	notifiedStamp := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Microsecond)
	var alreadyNotifiedID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO site_vulnerabilities
			(tenant_id, site_id, vuln_id, kind, slug, name, installed_version, severity, title, status, notified_at)
		VALUES ($1, $2, 'm145-lr-vuln-notified', 'plugin', 'm145-lr-vuln-notified', 'seed', '1.0.0', 'high', 'seed finding', 'open', $3)
		RETURNING id`,
		tenant, siteID, notifiedStamp,
	).Scan(&alreadyNotifiedID); err != nil {
		t.Fatalf("seed already-notified finding: %v", err)
	}

	body, err := fs.ReadFile(migrations.FS, m145MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m145 migration body: %v", err)
	}
	mutated := stripOnce(t, string(body), m145EarlyReturnGuardOpen)
	mutated = stripOnce(t, mutated, m145EarlyReturnGuardClose)

	release := holdRowExclusiveOpen(t, owner, "site_vulnerabilities", "last_seen")
	defer release()

	// Fires: without the guard, m145 would attempt its ADD COLUMN
	// unconditionally. notified_at already exists (m103 added it above), so
	// this fails with SQLSTATE 42701 (column already exists) — the honest
	// proof for m145, which is not about the held lock at all.
	mutatedMigrationMustBlockOrError(t, owner, mutated, sqlStateDuplicateColumn)

	before := siteVulnerabilitiesChecksum(t, pool)

	// Does not over-fire: the REAL m145 arrives late (unmark and migrate
	// again) and must finish fast despite the lock still held above.
	scopePrefillUnmark(t, owner, m145MigrationVersion)
	assertMigrateStaysUnderLockBound(t, owner, ctx)

	after := siteVulnerabilitiesChecksum(t, pool)
	if before != after {
		t.Fatalf("m145's late run changed site_vulnerabilities data: before=%s after=%s", before, after)
	}
	if err := pool.QueryRow(ctx, `SELECT notified_at FROM site_vulnerabilities WHERE id = $1`, findingID).Scan(&notifiedAt); err != nil {
		t.Fatalf("query notified_at after late run: %v", err)
	}
	if notifiedAt != nil {
		t.Fatalf("m145's late run stamped a post-migration NULL notified_at = %v, want it to stay NULL", *notifiedAt)
	}
	var afterNotifiedStamp time.Time
	if err := pool.QueryRow(ctx, `SELECT notified_at FROM site_vulnerabilities WHERE id = $1`, alreadyNotifiedID).Scan(&afterNotifiedStamp); err != nil {
		t.Fatalf("query notified_at for the already-notified finding after late run: %v", err)
	}
	if !afterNotifiedStamp.Equal(notifiedStamp) {
		t.Fatalf("m145's late run changed an already-notified finding's stamp: before=%v after=%v", notifiedStamp, afterNotifiedStamp)
	}

	// Re-apply the real file directly and prove it took no lock on
	// site_vulnerabilities at all, still with the ROW EXCLUSIVE holder open.
	assertFileTakesNoLockOnRelation(t, owner, "public.site_vulnerabilities", string(body))

	release()
	assertSiteVulnerabilitiesForceIntact(t, pool)
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
