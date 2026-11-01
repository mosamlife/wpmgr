// remove_config_integration_test.go: GH #968's delete, Repo.DeleteSiteConfig,
// driven through the repo's own transaction dispatch (scopedTenantTx) against
// the real migrations and policies, as the non-superuser wpmgr_app role. The
// harness and fixture are repo_site_scope_integration_test.go's; ground truth
// is read on the superuser pool, which RLS does not filter, so a row a policy
// merely hides cannot pass for a row that is still there. Skips without Docker.
package email

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRepoDeleteSiteConfigThroughTheScopedTx(t *testing.T) {
	app, admin := startScopeTestPostgres(t)
	f := seedScopeFixture(t, admin)
	repo := NewRepo(app)

	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := admin.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
			t.Fatalf("count (%s): %v", q, err)
		}
		return n
	}
	siteARows := func() int {
		return count(`SELECT count(*) FROM site_email_config WHERE tenant_id = $1 AND site_id = $2`, f.tenant, f.siteA)
	}

	// The service reads the row through the repo and hands its updated_at back
	// exactly as read.
	read, err := repo.GetSiteConfig(collaboratorCtx(f.tenant, f.siteA), f.tenant, f.siteA)
	if err != nil {
		t.Fatalf("read site A's config through the repo: %v", err)
	}

	// A collaborator granted only site B removes nothing of site A's, even
	// with site A's true updated_at.
	n, err := repo.DeleteSiteConfig(collaboratorCtx(f.tenant, f.siteB), f.tenant, f.siteA, read.UpdatedAt)
	if err != nil {
		t.Fatalf("site-B collaborator delete: %v", err)
	}
	if n != 0 || siteARows() != 1 {
		t.Fatalf("a collaborator granted only site B removed site A's config: %d row(s) deleted, %d left", n, siteARows())
	}

	// An org member naming an updated_at that is not the row's removes nothing:
	// that is the edited-since-read case.
	n, err = repo.DeleteSiteConfig(orgMemberCtx(f.tenant), f.tenant, f.siteA, read.UpdatedAt.Add(-time.Microsecond))
	if err != nil {
		t.Fatalf("stale delete: %v", err)
	}
	if n != 0 || siteARows() != 1 {
		t.Fatalf("a delete naming a stale updated_at removed the row: %d deleted, %d left", n, siteARows())
	}

	// The collaborator granted site A, with the value exactly as read, removes
	// it, and only it.
	n, err = repo.DeleteSiteConfig(collaboratorCtx(f.tenant, f.siteA), f.tenant, f.siteA, read.UpdatedAt)
	if err != nil {
		t.Fatalf("site-A collaborator delete: %v", err)
	}
	if n != 1 || siteARows() != 0 {
		t.Fatalf("the site's own delete did not remove its row: %d deleted, %d left", n, siteARows())
	}
	if got := count(`SELECT count(*) FROM site_email_config WHERE id = $1`, f.orgConfig); got != 1 {
		t.Fatalf("the organisation default is gone (%d rows)", got)
	}
	if got := count(`SELECT count(*) FROM site_email_connection WHERE config_id = $1`, f.orgConfig); got != 1 {
		t.Fatalf("the organisation default's connection is gone (%d rows)", got)
	}
	if got := count(`SELECT count(*) FROM site_email_connection WHERE config_id = $1`, f.siteAConfig); got != 0 {
		t.Fatalf("site A's connections survived its config (%d rows)", got)
	}

	// Afterwards the site has no row of its own, which is what makes the
	// service fall back to the organisation default.
	if _, err := repo.GetSiteConfig(orgMemberCtx(f.tenant), f.tenant, f.siteA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("site A still has a config of its own after the delete: %v", err)
	}
	if _, err := repo.GetOrgConfig(collaboratorCtx(f.tenant, f.siteA), f.tenant); err != nil {
		t.Fatalf("the organisation default is not served after the delete: %v", err)
	}
}
