package tests

// font_transcode_results_cascade_upgrade_migration_test.go: m138
// (20260909000000_m138_font_transcode_results_cascade.sql) applied to a
// database that ALREADY HAS font_transcode_results rows -- the UPGRADE path,
// as opposed to font_transcode_results_cascade_integration_test.go, whose
// startPostgres(t) applies every embedded migration, m138 included, before any
// row is inserted. Against that harness the two FKs m138 adds are already in
// place by the time a row can be written at all, so nothing there can ever
// produce a row that violates them, and m138's own orphan clean-up step (its
// header's "step 2") and its behaviour under FORCE ROW LEVEL SECURITY are
// exercised by neither file until this one. That gap is what Greptile flagged
// on PR #768 (thread PRRT_kwDOSpQhPc6mvOWK, on
// font_transcode_results_cascade_integration_test.go:119).
//
// The day this migration was proposed, production's deploy failed for the
// same class of gap one migration earlier: m136's backfill ran as a
// non-superuser table owner under FORCE ROW LEVEL SECURITY and silently
// touched zero rows, because every CI test up to then migrated an empty
// database. The fix (#770) added
// apps/api/tests/mcp_scope_columns_prefill_migration_test.go, which migrates
// to the version BEFORE the one under test as a NOSUPERUSER NOBYPASSRLS owner
// role, seeds rows through the product's own transaction helpers, then runs
// the real db.Pool.Migrate. This file follows that pattern for m138, reusing
// its startPostgresAsOwner/scopePrefillCount helpers directly (same package).
//
// m138's own header explains why the assertion below that counts violating
// rows independently of convalidated matters: step 4's ADD CONSTRAINT ...
// VALIDATED runs as the owner, under FORCE ROW LEVEL SECURITY, OUTSIDE
// app.agent='on' (step 2 restores the previous GUC values before step 4
// runs). A non-superuser owner's validation therefore only sees rows the
// tenant_isolation policy admits under whatever app.tenant_id happens to be
// set (typically none, during a migration), and PostgreSQL will still mark
// the constraint convalidated=true even though the validation query saw zero
// rows. convalidated=true is consequently NOT proof that no violating row
// exists; only counting the rows directly, as the superuser admin connection
// (which bypasses RLS and sees everything), is.
//
// NOT RUN BY CI (apps/api/tests is excluded from the fast lane by owner
// decision). Run with `make test-integration` from the repository root.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/perf"
)

// ftrUpgradeM138 is m138's version string, exactly as it sorts among the
// embedded migrations.
const ftrUpgradeM138 = "20260909000000_m138_font_transcode_results_cascade"

// ftrUpgradeUpToM137 applies every migration that sorts lexically before
// m138: the schema a database has the moment before m138 lands, whichever
// migration happens to be lexically last among them (20260908000000_m137, at
// the time this file was written).
func ftrUpgradeUpToM137(version string) bool { return version < ftrUpgradeM138 }

// ftrUpgradeRequireValidatedFK asserts a FOREIGN KEY constraint on
// font_transcode_results exists and is convalidated, read as the superuser
// admin connection.
func ftrUpgradeRequireValidatedFK(t *testing.T, admin *db.Pool, name, refTable string) {
	t.Helper()
	var validated bool
	var confrelid string
	err := admin.QueryRow(context.Background(),
		`SELECT convalidated, confrelid::regclass::text
		   FROM pg_constraint
		  WHERE conrelid = 'public.font_transcode_results'::regclass
		    AND conname  = $1
		    AND contype  = 'f'`,
		name).Scan(&validated, &confrelid)
	if err != nil {
		t.Fatalf("constraint %s: %v (does m138 add a FOREIGN KEY of this name?)", name, err)
	}
	if !validated {
		t.Errorf("constraint %s exists but convalidated=false", name)
	}
	if confrelid != refTable && confrelid != "public."+refTable {
		t.Errorf("constraint %s references %s, want %s", name, confrelid, refTable)
	}
}

// TestFontTranscodeResultsCascade_UpgradeWithExistingRows is the upgrade
// proof: seed rows before m138 has ever run, then run it, then prove its
// clean-up step and its two FKs did exactly what the header promises.
func TestFontTranscodeResultsCascade_UpgradeWithExistingRows(t *testing.T) {
	d := startPostgresAsOwner(t, ftrUpgradeUpToM137)
	ctx := context.Background()

	// The premise this whole file rests on: the migrating role is subject to
	// row security. startPostgresAsOwner already asserts this before applying
	// a single migration; reconfirmed here, inside this test, because that is
	// what #770's test does and what the task asks this one to do too.
	var ownerSuper, ownerBypass bool
	if err := d.owner.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&ownerSuper, &ownerBypass); err != nil {
		t.Fatalf("read wpmgr_owner role attributes: %v", err)
	}
	t.Logf("migrating role: current_user=wpmgr_owner rolsuper=%t rolbypassrls=%t", ownerSuper, ownerBypass)
	if ownerSuper || ownerBypass {
		t.Fatalf("wpmgr_owner has rolsuper=%t rolbypassrls=%t; every assertion below "+
			"would pass whether or not m138's clean-up step and FKs do anything at all",
			ownerSuper, ownerBypass)
	}

	if n := scopePrefillCount(t, d.admin, `SELECT count(*) FROM schema_migrations WHERE version = $1`, ftrUpgradeM138); n != 0 {
		t.Fatalf("m138 already recorded (%d rows) before Migrate; this test's premise is wrong", n)
	}

	// Seed through the product's own write path for this table:
	// perf.Repo.UpsertFontTranscodeJob, which runs under InAgentTx -- the
	// same dispatch the font_transcode River worker uses in production. As
	// wpmgr_app, non-superuser, non-BYPASSRLS.
	repo := perf.NewRepo(d.app)
	if err := d.app.InAgentTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "seeding via InAgentTx (UpsertFontTranscodeJob's real write path)")
		return nil
	}); err != nil {
		t.Fatalf("role-check tx: %v", err)
	}

	// Two organisations still exist once m138 runs: tenantAlive (untouched)
	// and tenantSoft (soft-deleted, tenants row still live). A third
	// identity, tenantDoomed, is hard-deleted before Migrate runs and so is
	// not an organisation any more by the time m138 sees it -- it is exactly
	// the dangling tenant_id m138's clean-up step exists to remove.

	// tenantAlive carries two rows on two different sites, so the clean-up
	// step's precision is provable at the row level: deleting one of a
	// tenant's sites must remove only that site's row, not every row the
	// tenant owns.
	tenantAlive := seedTenant(t, d.app, "ftr-up-alive-"+uuid.NewString()[:8])
	siteLive := ftrSeedSite(t, d.admin, tenantAlive)
	siteDoomed := ftrSeedSite(t, d.admin, tenantAlive)

	validHash := "ftr-upgrade-valid-" + uuid.NewString()
	if _, err := repo.UpsertFontTranscodeJob(ctx, tenantAlive, siteLive, validHash, 9001); err != nil {
		t.Fatalf("seed valid row: %v", err)
	}
	orphanSiteHash := "ftr-upgrade-orphan-site-" + uuid.NewString()
	if _, err := repo.UpsertFontTranscodeJob(ctx, tenantAlive, siteDoomed, orphanSiteHash, 9002); err != nil {
		t.Fatalf("seed orphan-by-site row: %v", err)
	}
	// Hard-delete just the site. Pre-m138 there is no FK from
	// font_transcode_results to sites, so this does not cascade the row away
	// -- it becomes exactly the orphan m138's clean-up step must remove.
	if _, err := d.admin.Exec(ctx, `DELETE FROM sites WHERE id = $1`, siteDoomed); err != nil {
		t.Fatalf("hard-delete siteDoomed: %v", err)
	}

	// tenantDoomed is hard-deleted entirely. sites.tenant_id has carried ON
	// DELETE CASCADE since the initial migration, so deleting the tenants row
	// also removes its site; font_transcode_results has no FK yet, so the row
	// naming both survives, doubly orphaned, until m138 runs.
	tenantDoomed := seedTenant(t, d.app, "ftr-up-doomed-"+uuid.NewString()[:8])
	siteOfDoomedTenant := ftrSeedSite(t, d.admin, tenantDoomed)
	orphanTenantHash := "ftr-upgrade-orphan-tenant-" + uuid.NewString()
	if _, err := repo.UpsertFontTranscodeJob(ctx, tenantDoomed, siteOfDoomedTenant, orphanTenantHash, 9003); err != nil {
		t.Fatalf("seed orphan-by-tenant row: %v", err)
	}
	if _, err := d.admin.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenantDoomed); err != nil {
		t.Fatalf("hard-delete tenantDoomed: %v", err)
	}

	// tenantSoft is only soft-deleted: tenants.deleted_at is set, but the
	// tenants row itself stays live. m138's header is explicit this row must
	// survive until the eventual purge, not this migration.
	tenantSoft := seedTenant(t, d.app, "ftr-up-soft-"+uuid.NewString()[:8])
	siteSoft := ftrSeedSite(t, d.admin, tenantSoft)
	softHash := "ftr-upgrade-soft-" + uuid.NewString()
	if _, err := repo.UpsertFontTranscodeJob(ctx, tenantSoft, siteSoft, softHash, 9004); err != nil {
		t.Fatalf("seed soft-deleted-org row: %v", err)
	}
	if _, err := d.admin.Exec(ctx, `UPDATE tenants SET deleted_at = now() WHERE id = $1`, tenantSoft); err != nil {
		t.Fatalf("soft-delete tenantSoft: %v", err)
	}

	// Premise check, counted as the superuser admin connection, which
	// bypasses RLS and sees every row: exactly the four seeded above.
	if got := scopePrefillCount(t, d.admin, `SELECT count(*) FROM font_transcode_results`); got != 4 {
		t.Fatalf("seeded %d font_transcode_results rows, want 4; this test's premise is wrong", got)
	}

	// The real migrator, as the owner, exactly as cmd/wpmgr does with
	// WPMGR_DB_MIGRATION_DSN.
	if err := d.owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate (upgrade path, m138 against a database with existing rows): %v", err)
	}

	if ftrTranscodeResultExists(t, d.admin, tenantAlive, orphanSiteHash) {
		t.Error("the orphan-by-site row must be removed by m138's clean-up step")
	}
	if ftrTranscodeResultExists(t, d.admin, tenantDoomed, orphanTenantHash) {
		t.Error("the orphan-by-tenant row must be removed by m138's clean-up step")
	}
	if !ftrTranscodeResultExists(t, d.admin, tenantAlive, validHash) {
		t.Error("the valid row must survive m138")
	}
	if !ftrTranscodeResultExists(t, d.admin, tenantSoft, softHash) {
		t.Error("a soft-deleted organisation's row must survive m138 -- it goes with the eventual purge, not this migration")
	}
	if got := scopePrefillCount(t, d.admin, `SELECT count(*) FROM font_transcode_results`); got != 2 {
		t.Errorf("font_transcode_results has %d row(s) after Migrate, want 2 (the valid row and the soft-deleted organisation's row)", got)
	}

	ftrUpgradeRequireValidatedFK(t, d.admin, "font_transcode_results_tenant_fk", "tenants")
	ftrUpgradeRequireValidatedFK(t, d.admin, "font_transcode_results_site_fk", "sites")

	// convalidated=true is not, by itself, proof that no row violates the
	// constraint -- see this file's header. Count directly, as the
	// superuser, which sees every row regardless of RLS.
	violating := scopePrefillCount(t, d.admin, `
		SELECT count(*) FROM font_transcode_results r
		 WHERE NOT EXISTS (SELECT 1 FROM tenants t WHERE t.id = r.tenant_id)
		    OR NOT EXISTS (SELECT 1 FROM sites   s WHERE s.id = r.site_id)`)
	if violating != 0 {
		t.Errorf("%d font_transcode_results row(s) violate the FKs m138 just added; "+
			"convalidated=true does not by itself prove this is zero under a non-superuser "+
			"owner's FORCE ROW LEVEL SECURITY-scoped validation", violating)
	}

	// A second boot applies nothing and changes nothing.
	beforeMigrations := scopePrefillCount(t, d.admin, `SELECT count(*) FROM schema_migrations`)
	if err := d.owner.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if after := scopePrefillCount(t, d.admin, `SELECT count(*) FROM schema_migrations`); after != beforeMigrations {
		t.Errorf("second Migrate recorded %d new migration(s), want 0", after-beforeMigrations)
	}
	if got := scopePrefillCount(t, d.admin, `SELECT count(*) FROM font_transcode_results`); got != 2 {
		t.Errorf("second Migrate changed font_transcode_results to %d row(s), want 2 (unchanged)", got)
	}
}
