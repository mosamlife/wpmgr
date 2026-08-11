// gh402_m114_convergence_integration_test.go: the fix has to reach the
// databases that already have the defect, and the first draft of it could not.
//
// internal/db/migrate.go skips any version already present in schema_migrations.
// A database that applied the PRE-REVIEW m113 therefore never reads m113 again,
// so the DROP CONSTRAINT and the site-scope policy originally added to that file
// would have run only on databases that never had the problem. That is the same
// shape as the defect being fixed: a corrective statement placed where the thing
// it corrects can never reach it.
//
// This test builds the affected database (fully migrated, then regressed to the
// pre-review m113 shape, with m114 marked unapplied), boots the migrator the way
// the server does, and asserts that both defects are actually gone AND that the
// reclaim record then survives the tenant delete that used to destroy it.
package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

const gh402M114Version = "20260816000000_m114_site_object_reclaim_converge"

// gh402RegressToPreReviewM113 puts the schema back into the exact shape the
// first version of m113 produced: a tenant foreign key that cascades, and no
// site-scope policy. m113 itself stays recorded as applied, which is the whole
// point: the runner will not revisit it.
func gh402RegressToPreReviewM113(t *testing.T, admin *db.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`ALTER TABLE site_object_reclaim
		   ADD CONSTRAINT site_object_reclaim_tenant_id_fkey
		   FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE`,
		`DROP POLICY IF EXISTS site_object_reclaim_site_scope ON site_object_reclaim`,
		`DELETE FROM schema_migrations WHERE version = '` + gh402M114Version + `'`,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("regress to pre-review m113 (%q): %v", stmt, err)
		}
	}
}

func gh402TenantFKExists(t *testing.T, admin *db.Pool) bool {
	t.Helper()
	var exists bool
	if err := admin.QueryRow(context.Background(),
		`SELECT EXISTS (
		     SELECT 1 FROM pg_constraint
		     WHERE conrelid = 'public.site_object_reclaim'::regclass
		       AND contype = 'f'
		       AND confrelid = 'public.tenants'::regclass
		 )`).Scan(&exists); err != nil {
		t.Fatalf("check tenant fk: %v", err)
	}
	return exists
}

func gh402SiteScopePolicyExists(t *testing.T, admin *db.Pool) bool {
	t.Helper()
	var exists bool
	if err := admin.QueryRow(context.Background(),
		`SELECT EXISTS (
		     SELECT 1 FROM pg_policies
		     WHERE schemaname = 'public' AND tablename = 'site_object_reclaim'
		       AND policyname = 'site_object_reclaim_site_scope'
		 )`).Scan(&exists); err != nil {
		t.Fatalf("check site-scope policy: %v", err)
	}
	return exists
}

func TestGH402_M114_ConvergesADatabaseThatAppliedThePreReviewM113(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	gh402RegressToPreReviewM113(t, admin)
	if !gh402TenantFKExists(t, admin) {
		t.Fatal("the regression did not reinstate the tenant foreign key; this test's premise is wrong")
	}
	if gh402SiteScopePolicyExists(t, admin) {
		t.Fatal("the regression did not remove the site-scope policy; this test's premise is wrong")
	}

	// Boot the server's migrator, exactly as cmd/wpmgr does on startup. m113 is
	// still recorded as applied and is skipped; m114 is not, and runs.
	if err := admin.Migrate(ctx); err != nil {
		t.Fatalf("boot migration: %v", err)
	}

	if gh402TenantFKExists(t, admin) {
		t.Error("the tenant foreign key survived the boot migration. Convergence that lives in " +
			"m113 never runs on a database that already applied m113, which is every database " +
			"that has this defect")
	}
	if !gh402SiteScopePolicyExists(t, admin) {
		t.Error("the site-scope policy is still missing after the boot migration; a collaborator " +
			"invited to one site can reach every other site's reclaim rows on exactly the " +
			"databases that upgraded rather than started fresh")
	}

	// The behaviour, not just the catalogue: the record now survives Lane A.
	tenant := seedTenant(t, pool, "gh402-m114-"+uuid.NewString()[:8])
	siteID := gh402SeedSite(t, admin, tenant)
	if err := site.NewRepo(pool).Delete(ctx, tenant, siteID); err != nil {
		t.Fatalf("delete site: %v", err)
	}
	if got := gh402CountTasks(t, admin, tenant, siteID); got != 1 {
		t.Fatalf("expected 1 reclaim task, got %d", got)
	}
	if deleted := callDeleteEmptyTenant(t, pool, tenant); !deleted {
		t.Fatal("admin_delete_empty_tenant refused an empty tenant; this test's premise is wrong")
	}
	if got := gh402CountTasks(t, admin, tenant, siteID); got != 1 {
		t.Errorf("after convergence the reclaim record still did not survive the tenant delete "+
			"(count %d, want 1)", got)
	}
}

// m114 must be safe to apply to a database that never had the defect, and safe
// to apply twice. It runs on every install, and almost all of them are the
// no-op case.
func TestGH402_M114_IsANoOpOnAHealthyDatabase(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	// startPostgres already applied everything, m114 included. Re-running it
	// against the healthy end state must change nothing and raise nothing.
	for i := 0; i < 2; i++ {
		if _, err := admin.Exec(ctx,
			`DELETE FROM schema_migrations WHERE version = $1`, gh402M114Version); err != nil {
			t.Fatalf("unmark m114: %v", err)
		}
		if err := admin.Migrate(ctx); err != nil {
			t.Fatalf("re-apply m114 (round %d): %v", i+1, err)
		}
		if gh402TenantFKExists(t, admin) {
			t.Error("m114 created a tenant foreign key")
		}
		if !gh402SiteScopePolicyExists(t, admin) {
			t.Error("m114 removed the site-scope policy")
		}
	}

	// Exactly one site-scope policy, not two that merely look alike: m113
	// creates it on a fresh database and m114 must not add a duplicate.
	var n int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM pg_policies
		 WHERE schemaname = 'public' AND tablename = 'site_object_reclaim'
		   AND policyname = 'site_object_reclaim_site_scope'`).Scan(&n); err != nil {
		t.Fatalf("count policies: %v", err)
	}
	if n != 1 {
		t.Errorf("site_object_reclaim has %d site-scope policies, want exactly 1", n)
	}
}
