package tests

// font_transcode_results_cascade_integration_test.go: regression coverage for
// PR #768 / migration m138 (20260909000000_m138_font_transcode_results_cascade.sql)
// -- "font_transcode_results rows are removed with their site and with their
// organisation." See that migration's header for the full contract; this file
// proves the three cases it draws:
//
//   * DELETE /sites/{id} (site.Repo.Delete) removes every font_transcode_results
//     row naming the deleted site, and only that site's rows.
//   * admin_purge_tenant removes every font_transcode_results row the purged
//     organisation owns, and only that organisation's rows.
//   * a SOFT-deleted organisation (tenants.deleted_at set, tenants row still
//     live) keeps its font_transcode_results rows -- they go with the eventual
//     purge, not the soft-delete.
//
// Each case also seeds a font_results (m55) row alongside the
// font_transcode_results row under test, as a positive control: font_results
// has carried the identical ON DELETE CASCADE pair since m55, well before
// m138. If the control row ever survived a case that also expects the
// font_transcode_results row gone, that would mean the delete/purge path
// itself silently did nothing -- a false green this file must not produce --
// rather than m138's own FKs being what is under test.
//
// Reached through the real dispatch: site.NewRepo(pool).Delete for the site
// case (the same path DELETE /sites/{id} runs), and sqlc.New(pool.Pool).
// AdminPurgeTenant for the purge case (the same call org.PurgeWorker makes),
// both against the pool startPostgres returns -- wpmgr_app, NOSUPERUSER,
// NOBYPASSRLS, the role every real install connects as. A proof that opened
// its own connection, or ran as the bootstrap superuser, would leave the FK
// cascade the only thing doing real work but could never distinguish that from
// RLS quietly hiding rows instead.
//
// NOT RUN BY CI (apps/api/tests is excluded from the fast lane by owner
// decision). Run with `make test-integration` from the repository root.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// ---------------------------------------------------------------------------
// seed / read helpers
// ---------------------------------------------------------------------------

// ftrSeedSite inserts a site row directly. Site creation itself is not what
// this file is about; internal/site's own tests cover Create.
func ftrSeedSite(t *testing.T, admin *db.Pool, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := admin.QueryRow(context.Background(),
		`INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, 'ftr') RETURNING id`,
		tenant, "https://"+uuid.NewString()+".example.com").Scan(&id); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	return id
}

// ftrSeedTranscodeResult inserts a font_transcode_results row naming
// (sourceHash, tenant, site), as if a transcode had already completed.
func ftrSeedTranscodeResult(t *testing.T, admin *db.Pool, tenant, siteID uuid.UUID, sourceHash string) {
	t.Helper()
	if _, err := admin.Exec(context.Background(),
		`INSERT INTO font_transcode_results (source_hash, tenant_id, site_id, woff2_key)
		 VALUES ($1, $2, $3, $4)`,
		sourceHash, tenant, siteID, "woff2/"+sourceHash); err != nil {
		t.Fatalf("seed font_transcode_results: %v", err)
	}
}

// ftrTranscodeResultExists reports whether the (tenant, sourceHash) row still
// exists. tenant_id is part of the primary key, so this is exact.
func ftrTranscodeResultExists(t *testing.T, admin *db.Pool, tenant uuid.UUID, sourceHash string) bool {
	t.Helper()
	var n int
	if err := admin.QueryRow(context.Background(),
		`SELECT count(*) FROM font_transcode_results WHERE tenant_id = $1 AND source_hash = $2`,
		tenant, sourceHash).Scan(&n); err != nil {
		t.Fatalf("count font_transcode_results: %v", err)
	}
	return n > 0
}

// ftrSeedFontResult inserts a font_results (m55) row naming (tenant, site,
// sourceHash) -- the control described at the top of this file.
func ftrSeedFontResult(t *testing.T, admin *db.Pool, tenant, siteID uuid.UUID, sourceHash string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := admin.QueryRow(context.Background(),
		`INSERT INTO font_results (tenant_id, site_id, source_hash) VALUES ($1, $2, $3) RETURNING id`,
		tenant, siteID, sourceHash).Scan(&id); err != nil {
		t.Fatalf("seed font_results: %v", err)
	}
	return id
}

func ftrFontResultExists(t *testing.T, admin *db.Pool, id uuid.UUID) bool {
	t.Helper()
	var n int
	if err := admin.QueryRow(context.Background(),
		`SELECT count(*) FROM font_results WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count font_results: %v", err)
	}
	return n > 0
}

// ---------------------------------------------------------------------------
// TestFontTranscodeResultsCascade
// ---------------------------------------------------------------------------

func TestFontTranscodeResultsCascade(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	ctx := context.Background()

	// Confirm, inside a transaction opened the SAME way the dispatch under
	// test opens one, that this pool is wpmgr_app with rolsuper=false and
	// rolbypassrls=false -- otherwise every case below would pass whether or
	// not m138's FKs exist at all.
	if err := pool.InTenantTx(ctx, uuid.New(), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "font_transcode_results cascade proof")
		return nil
	}); err != nil {
		t.Fatalf("role-check tx: %v", err)
	}

	t.Run("site_delete_removes_its_rows_only", func(t *testing.T) {
		tenantA := seedTenant(t, pool, "ftr-sitea-"+uuid.NewString()[:8])
		tenantB := seedTenant(t, pool, "ftr-siteb-"+uuid.NewString()[:8])
		siteA := ftrSeedSite(t, admin, tenantA)
		siteB := ftrSeedSite(t, admin, tenantB)

		hashA := "hash-site-a-" + uuid.NewString()
		hashB := "hash-site-b-" + uuid.NewString()
		ftrSeedTranscodeResult(t, admin, tenantA, siteA, hashA)
		ftrSeedTranscodeResult(t, admin, tenantB, siteB, hashB)
		frA := ftrSeedFontResult(t, admin, tenantA, siteA, hashA)
		frB := ftrSeedFontResult(t, admin, tenantB, siteB, hashB)

		// The product's own delete path: DELETE /sites/{id} runs through
		// exactly this repository method (internal/site/repo.go).
		if err := site.NewRepo(pool).Delete(ctx, tenantA, siteA); err != nil {
			t.Fatalf("delete site: %v", err)
		}

		if ftrTranscodeResultExists(t, admin, tenantA, hashA) {
			t.Fatal("font_transcode_results row for the deleted site must be gone (m138 site_id FK, ON DELETE CASCADE)")
		}
		if ftrFontResultExists(t, admin, frA) {
			t.Fatal("font_results control row for the deleted site should also be gone (m55); if it survived, the delete path itself is not exercising a real cascade and this test proves nothing")
		}
		if !ftrTranscodeResultExists(t, admin, tenantB, hashB) {
			t.Fatal("the OTHER organisation's font_transcode_results row must survive a sibling site's delete")
		}
		if !ftrFontResultExists(t, admin, frB) {
			t.Fatal("the other organisation's font_results control row must survive a sibling site's delete")
		}
	})

	t.Run("org_purge_removes_its_rows_only", func(t *testing.T) {
		tenantA := seedTenant(t, pool, "ftr-purgea-"+uuid.NewString()[:8])
		tenantB := seedTenant(t, pool, "ftr-purgeb-"+uuid.NewString()[:8])
		siteA := ftrSeedSite(t, admin, tenantA)
		siteB := ftrSeedSite(t, admin, tenantB)

		hashA := "hash-purge-a-" + uuid.NewString()
		hashB := "hash-purge-b-" + uuid.NewString()
		ftrSeedTranscodeResult(t, admin, tenantA, siteA, hashA)
		ftrSeedTranscodeResult(t, admin, tenantB, siteB, hashB)
		frA := ftrSeedFontResult(t, admin, tenantA, siteA, hashA)
		frB := ftrSeedFontResult(t, admin, tenantB, siteB, hashB)

		// admin_purge_tenant is SECURITY DEFINER; org.PurgeWorker calls it
		// exactly this way, through the app pool's underlying *pgxpool.Pool
		// (internal/org/purge_worker.go, step c).
		purged, err := sqlc.New(pool.Pool).AdminPurgeTenant(ctx, tenantA)
		if err != nil {
			t.Fatalf("admin_purge_tenant: %v", err)
		}
		if !purged {
			t.Fatal("admin_purge_tenant should have purged tenantA")
		}

		if ftrTranscodeResultExists(t, admin, tenantA, hashA) {
			t.Fatal("font_transcode_results row must be gone once its organisation is purged (m138 tenant_id FK, ON DELETE CASCADE)")
		}
		if ftrFontResultExists(t, admin, frA) {
			t.Fatal("font_results control row must be gone once its organisation is purged (m55); if it survived, the purge itself did not really cascade and this test proves nothing")
		}
		if !ftrTranscodeResultExists(t, admin, tenantB, hashB) {
			t.Fatal("the OTHER organisation's font_transcode_results row must survive a sibling org's purge")
		}
		if !ftrFontResultExists(t, admin, frB) {
			t.Fatal("the other organisation's font_results control row must survive a sibling org's purge")
		}
	})

	t.Run("org_purge_removes_rows_via_tenant_fk_alone", func(t *testing.T) {
		// The two subtests above cannot tell font_transcode_results_tenant_fk
		// apart from font_transcode_results_site_fk: in each, the row's site
		// belongs to the SAME organisation being deleted/purged, so purging
		// tenantA also deletes siteA (tenants -> sites cascade), which by
		// itself removes the row via site_fk -- tenant_fk is never the thing
		// doing the work, and dropping it from a scratch copy leaves every
		// other subtest here green.
		//
		// This case seeds a row naming organisation A (tenant_id) but a SITE
		// THAT BELONGS TO ORGANISATION B (site_id). Nothing in the schema
		// requires the two to agree -- font_transcode_results carries two
		// independent foreign keys, not a composite one -- and this insert
		// goes through the admin (superuser) connection, which bypasses RLS
		// entirely, exactly like every other seed in this file. Purging
		// tenantA never touches siteB or tenantB, so the row can be removed
		// ONLY by font_transcode_results_tenant_fk's own CASCADE. If that FK
		// is missing, the row survives the purge.
		tenantA := seedTenant(t, pool, "ftr-purgetfk-a-"+uuid.NewString()[:8])
		tenantB := seedTenant(t, pool, "ftr-purgetfk-b-"+uuid.NewString()[:8])
		siteB := ftrSeedSite(t, admin, tenantB)

		hash := "hash-purgetfk-" + uuid.NewString()
		ftrSeedTranscodeResult(t, admin, tenantA, siteB, hash)

		purged, err := sqlc.New(pool.Pool).AdminPurgeTenant(ctx, tenantA)
		if err != nil {
			t.Fatalf("admin_purge_tenant: %v", err)
		}
		if !purged {
			t.Fatal("admin_purge_tenant should have purged tenantA")
		}

		if ftrTranscodeResultExists(t, admin, tenantA, hash) {
			t.Fatal("a font_transcode_results row naming the purged organisation must be gone even when its site_id names a DIFFERENT, still-live organisation's site (m138 tenant_id FK, ON DELETE CASCADE) -- the site_fk cascade cannot be what did this, because siteB was never deleted")
		}

		var siteBExists bool
		if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sites WHERE id = $1)`, siteB).Scan(&siteBExists); err != nil {
			t.Fatalf("check siteB survives: %v", err)
		}
		if !siteBExists {
			t.Fatal("tenantB's site must survive tenantA's purge -- if it did not, this case is not isolating tenant_fk either")
		}
	})

	t.Run("org_soft_delete_keeps_rows_until_purge", func(t *testing.T) {
		tenant := seedTenant(t, pool, "ftr-soft-"+uuid.NewString()[:8])
		siteID := ftrSeedSite(t, admin, tenant)

		hash := "hash-soft-" + uuid.NewString()
		ftrSeedTranscodeResult(t, admin, tenant, siteID, hash)
		fr := ftrSeedFontResult(t, admin, tenant, siteID, hash)

		// Soft delete only: tenants.deleted_at set, the tenants row itself
		// stays. m138's header is explicit that this must NOT touch
		// font_transcode_results -- the row goes only when the eventual purge
		// removes the tenants row.
		if _, err := admin.Exec(ctx, `UPDATE tenants SET deleted_at = now() WHERE id = $1`, tenant); err != nil {
			t.Fatalf("soft-delete tenant: %v", err)
		}

		if !ftrTranscodeResultExists(t, admin, tenant, hash) {
			t.Fatal("a soft-deleted organisation's font_transcode_results row must survive until the purge (the tenants row itself is still live)")
		}
		if !ftrFontResultExists(t, admin, fr) {
			t.Fatal("a soft-deleted organisation's font_results row must survive until the purge")
		}
	})
}
