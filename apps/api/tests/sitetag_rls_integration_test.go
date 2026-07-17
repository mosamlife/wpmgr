// sitetag_rls_integration_test.go — GH #230 "rich tags" (m100) row-level-
// security: proves site_tags enforces tenant isolation (a second tenant sees
// zero rows) and allows the cross-tenant agent path, against a real Postgres
// with the production non-superuser role. Mirrors
// tests/perf_rls_integration_test.go's pattern exactly (m36 precedent this
// table's RLS is modeled on, via the m63 clients template). Requires Docker;
// skips when unavailable (via startPostgres).
package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/sitetag"
)

func TestSiteTagsRLS(t *testing.T) {
	app := startPostgres(t) // connected as the non-superuser wpmgr_app role
	admin := connectAdmin(t, app)
	defer admin.Close()
	ctx := context.Background()

	tenantA := seedTenant(t, app, "sitetag-rls-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, app, "sitetag-rls-b-"+uuid.NewString()[:8])

	siteSvc := site.NewService(site.NewRepo(app), domain.NewValidator(), domain.SystemClock{})
	sA, err := siteSvc.Create(ctx, site.CreateInput{TenantID: tenantA, URL: "https://rls-a.example.com", Name: "a"})
	if err != nil {
		t.Fatalf("create site for tenant A: %v", err)
	}

	tagSvc := sitetag.NewService(sitetag.NewRepo(app))
	if _, err := tagSvc.Create(ctx, sitetag.CreateInput{TenantID: tenantA, Name: "secret-a"}); err != nil {
		t.Fatalf("create tag for tenant A: %v", err)
	}
	if _, err := siteSvc.SetTags(ctx, site.SetTagsInput{TenantID: tenantA, SiteID: sA.ID, Tags: []string{"secret-a"}}); err != nil {
		t.Fatalf("SetTags for tenant A: %v", err)
	}

	countUnder := func(run func(fn func(pgx.Tx) error) error) int {
		var n int
		if err := run(func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM site_tags WHERE tenant_id = $1`, tenantA).Scan(&n)
		}); err != nil {
			t.Fatalf("count query: %v", err)
		}
		return n
	}

	// 1. Tenant A sees its own tag.
	if got := countUnder(func(fn func(pgx.Tx) error) error { return app.InTenantTx(ctx, tenantA, fn) }); got != 1 {
		t.Fatalf("tenant A must see its own tag, got %d", got)
	}
	// 2. Tenant B does NOT see tenant A's tag (tenant_isolation) — even when
	// the WHERE clause explicitly targets tenantA's id, proving RLS (not just
	// the app-level WHERE) is what's enforcing isolation.
	if got := countUnder(func(fn func(pgx.Tx) error) error { return app.InTenantTx(ctx, tenantB, fn) }); got != 0 {
		t.Fatalf("tenant B must NOT see tenant A's tag (tenant_isolation), got %d", got)
	}
	// 3. Service-level equivalent: tenant B's tag list is empty.
	listB, err := tagSvc.List(ctx, tenantB)
	if err != nil {
		t.Fatalf("List for tenant B: %v", err)
	}
	if len(listB) != 0 {
		t.Fatalf("tenant B's tag list must be empty, got %d items", len(listB))
	}
	// 4. The agent/worker scope sees it cross-tenant (agent policy).
	if got := countUnder(func(fn func(pgx.Tx) error) error { return app.InAgentTx(ctx, fn) }); got != 1 {
		t.Fatalf("agent scope must see the tag (agent policy), got %d", got)
	}

	// 5. WITH CHECK: tenant B cannot INSERT a tag row carrying tenant A's id.
	errWrite := app.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO site_tags (tenant_id, name) VALUES ($1, 'cross-tenant')`, tenantA)
		return e
	})
	if errWrite == nil {
		t.Fatal("tenant B must NOT be able to write a site_tags row for tenant A (WITH CHECK)")
	}

	// 6. Sanity: exactly one row overall (only tenant A's).
	var total int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM site_tags`).Scan(&total); err != nil {
		t.Fatalf("total count: %v", err)
	}
	if total != 1 {
		t.Fatalf("expected exactly 1 site_tags row overall, got %d", total)
	}
}
