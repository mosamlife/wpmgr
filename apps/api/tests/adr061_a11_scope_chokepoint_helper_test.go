// adr061_a11_scope_chokepoint_helper_test.go: ADR-061 A11 item 2, executed.
//
// THE CLAIM UNDER TEST. mcp.Repo.ResolveScopeSites is the one audited
// chokepoint for "which sites does this scope name". It used to take a bare
// tenant uuid and run db.Pool.InTenantTx, which meant it could not route on
// scope EVEN IN PRINCIPLE: app.site_scope was never set, so every RESTRICTIVE
// `_site_scope` policy -- sites_site_scope (m19) above all, the one this
// query's join through `sites` passes through -- was switched OFF at the
// chokepoint that exists to enforce scope. It now takes a db.ScopedPrincipal
// and runs RunTenantTx, so a site-constrained principal reaches
// InScopedTenantTx and the policy engages.
//
// WHAT MAKES THIS A REAL PROOF AND NOT A VACUOUS ONE. Every assertion below
// goes through mcp.Repo -- the same method, the same generated query, the same
// tx helper the request path uses. No connection is opened here and no GUC is
// hand-set. The role is asserted from INSIDE a transaction taken by the very
// helper under test, because wpmgr_app being SUPERUSER or BYPASSRLS would make
// all of this pass while proving nothing; that is m112's failure and it is the
// reason this file prints the role rather than trusting it.
package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// orgPrincipal, the tenant-wide operator, is NOT redeclared here: the shape
// this file needs already exists in sitetag_routes_contract_test.go and is
// exactly right (Scope: domain.ScopeOrg, so domain.IsSiteConstrained is false
// and the dispatch sends it to InTenantTxAsUser). A second copy would be a
// second place for the two to drift apart.

// scopedPrincipal is the site-constrained collaborator. domain.IsSiteConstrained
// is true for it, so db.Pool.RunTenantTx MUST route it to InScopedTenantTx,
// which is the only helper that sets app.site_scope and app.allowed_site_ids.
func scopedPrincipal(tenantID uuid.UUID, allowed ...uuid.UUID) domain.Principal {
	return domain.Principal{
		TenantID:       tenantID,
		UserID:         uuid.New(),
		Scope:          domain.ScopeSite,
		AllowedSiteIDs: allowed,
	}
}

// TestA11ScopeChokepointRoutesOnPrincipalAsAppRole is the before/after. The
// SAME mode-'list' resolution, over the SAME two real sites in ONE tenant, is
// run twice and differs ONLY in the principal handed to the chokepoint:
//
//	org-scoped    -> InTenantTx        -> app.site_scope unset -> both sites
//	site-scoped   -> InScopedTenantTx  -> app.site_scope on    -> only the
//	                                                             allowlisted one
//
// The second result is the policy engaging. Before the signature change the
// site-scoped call was NOT EXPRESSIBLE -- the function took a uuid -- and the
// tenant-wide answer was the only answer this chokepoint could give.
//
// Both sites belong to the SAME tenant on purpose. Tenant isolation already
// drops a foreign site (TestMCPScopeResolutionDropsForeignSiteIDsAsAppRole
// proves that), so a cross-tenant fixture here would pass identically with the
// site-scope policy switched off and would prove nothing about it.
func TestA11ScopeChokepointRoutesOnPrincipalAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	mcpRepo := mcp.NewRepo(pool)
	siteRepo := site.NewRepo(pool)

	tenant := seedTenant(t, pool, "a11-chokepoint-"+uuid.NewString()[:8])

	// THE ROLE, READ FROM INSIDE A TRANSACTION THIS HELPER OPENED. Asserted
	// before anything else, because every assertion after it is worthless if
	// the connection can see through RLS.
	var role string
	var super, bypass bool
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT current_user, rolsuper, rolbypassrls
			   FROM pg_roles WHERE rolname = current_user`).Scan(&role, &super, &bypass)
	}); err != nil {
		t.Fatalf("read current role inside the tx under test: %v", err)
	}
	t.Logf("connected as %q rolsuper=%v rolbypassrls=%v", role, super, bypass)
	if super || bypass {
		t.Fatalf("this proof runs as %q with rolsuper=%v rolbypassrls=%v; either "+
			"privilege makes every RLS policy inert and every assertion below "+
			"vacuous", role, super, bypass)
	}

	inside, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenant, URL: "https://a11-inside.example.com", Name: "inside"})
	if err != nil {
		t.Fatalf("create the allowlisted site: %v", err)
	}
	outside, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenant, URL: "https://a11-outside.example.com", Name: "outside"})
	if err != nil {
		t.Fatalf("create the non-allowlisted site: %v", err)
	}
	named := []uuid.UUID{inside.ID, outside.ID}

	// BEFORE: the tenant-wide read. NOT "byte-for-byte what the old signature
	// did" -- that claim was wrong. orgPrincipal carries a UserID, so
	// dispatchTenantTx routes it to InTenantTxAsUser rather than the InTenantTx
	// the bare-uuid version always took, which sets app.user_id as well as
	// app.tenant_id. The RESOLVED SET is what must be unchanged, and it is: the
	// only app.user_id-keyed policy on `sites` is sites_shared_read (m22),
	// PERMISSIVE FOR SELECT over other tenants' shares, which this query's
	// `WHERE s.tenant_id = $1` excludes. An org-scoped operator must still
	// reach every site in its own tenant.
	org, err := mcpRepo.ResolveScopeSites(ctx, orgPrincipal(tenant), "list", nil, named)
	if err != nil {
		t.Fatalf("resolve as an org-scoped principal: %v", err)
	}
	t.Logf("org-scoped principal, mode 'list' naming %d sites, resolved to %d",
		len(named), len(org))
	if len(org) != len(named) {
		t.Fatalf("an org-scoped operator resolved %d of the %d sites it named; "+
			"the tenant-wide path has regressed, which is the OVER-FIRE "+
			"direction: got %v", len(org), len(named), org)
	}

	// AFTER: the same query, same tenant, same two ids, site-constrained
	// principal whose allowlist holds only one of them.
	scoped, err := mcpRepo.ResolveScopeSites(ctx,
		scopedPrincipal(tenant, inside.ID), "list", nil, named)
	if err != nil {
		t.Fatalf("resolve as a site-constrained principal: %v", err)
	}
	t.Logf("site-scoped principal (allowlist of 1), same %d ids, resolved to %d",
		len(named), len(scoped))
	if len(scoped) != 1 || scoped[0] != inside.ID {
		t.Fatalf("a site-constrained principal naming site %v OUTSIDE its "+
			"allowlist resolved %v, want just %v. app.site_scope is not "+
			"reaching sites_site_scope: the chokepoint is running under the "+
			"plain tenant helper", outside.ID, scoped, inside.ID)
	}
}

// TestA11ScopeChokepointDoesNotOverFireAsAppRole is the other half of the guard
// rule: a check that reddens correct work gets switched off, and then it guards
// nothing. Three honest cases that MUST still resolve exactly as they did
// before the signature moved.
func TestA11ScopeChokepointDoesNotOverFireAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	mcpRepo := mcp.NewRepo(pool)
	siteRepo := site.NewRepo(pool)

	tenant := seedTenant(t, pool, "a11-overfire-"+uuid.NewString()[:8])
	one, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenant, URL: "https://a11-of1.example.com", Name: "of1"})
	if err != nil {
		t.Fatalf("create site one: %v", err)
	}
	two, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenant, URL: "https://a11-of2.example.com", Name: "of2"})
	if err != nil {
		t.Fatalf("create site two: %v", err)
	}
	tenantSites := 2

	// 1. AN ORG-SCOPED OPERATOR, MODE 'all'. Org-wide is the stored grant's
	//    intended meaning and nothing may narrow it.
	all, err := mcpRepo.ResolveScopeSites(ctx, orgPrincipal(tenant), "all", nil, nil)
	if err != nil {
		t.Fatalf("mode 'all' as an org principal: %v", err)
	}
	t.Logf("mode 'all', org-scoped: resolved to %d of the tenant's %d sites",
		len(all), tenantSites)
	if len(all) != tenantSites {
		t.Fatalf("mode 'all' resolved %d sites in a tenant holding %d: %v",
			len(all), tenantSites, all)
	}

	// 2. THE UNAUTHENTICATED BOOTSTRAP. Service.Authenticate has no principal
	//    -- it is authenticating a bearer token -- and hands the chokepoint a
	//    principal built from the token's tenant alone. That shape is
	//    org-scoped by construction (Scope and UserID unset), so it must reach
	//    every site the tenant-wide path reached. If this ever narrows, EVERY
	//    MCP connection in the fleet resolves to fewer sites than its grant
	//    names, which is a silent fleet-wide outage rather than an error.
	//
	//    The literal below is the shape mcp.bootstrapTenantPrincipal returns;
	//    it is unexported, so this restates it rather than calling it.
	bootstrap, err := mcpRepo.ResolveScopeSites(ctx,
		domain.Principal{TenantID: tenant}, "all", nil, nil)
	if err != nil {
		t.Fatalf("bootstrap principal, mode 'all': %v", err)
	}
	t.Logf("bootstrap principal (no scope, no user), mode 'all': resolved to %d",
		len(bootstrap))
	if len(bootstrap) != len(all) {
		t.Fatalf("the bootstrap principal resolved %d sites where the org "+
			"principal resolved %d; Authenticate is being narrowed and every "+
			"live connection loses sites", len(bootstrap), len(all))
	}

	// 3. MODE 'list' NAMING EVERY SITE, ORG-SCOPED. The ordinary operator case,
	//    and the exact call mint.go's verifyScopeReferents makes.
	listed, err := mcpRepo.ResolveScopeSites(ctx, orgPrincipal(tenant), "list", nil,
		[]uuid.UUID{one.ID, two.ID})
	if err != nil {
		t.Fatalf("mode 'list' as an org principal: %v", err)
	}
	t.Logf("mode 'list' naming %d sites, org-scoped: resolved to %d",
		tenantSites, len(listed))
	if len(listed) != tenantSites {
		t.Fatalf("mode 'list' naming %d of its own sites resolved %d: %v",
			tenantSites, len(listed), listed)
	}
}

// TestA11ScopedPrincipalWithEmptyAllowlistResolvesToZeroSites is A11 item 3 at
// this chokepoint: a scope of "site" with an empty allowlist means NO SITES and
// never ALL SITES. It is the single most likely place for a fail-open default
// to survive review, and with the signature change it is now decided by the
// DATABASE -- sites_site_scope against an empty app.allowed_site_ids matches
// nothing -- rather than by Go remembering to check.
//
// It is ALSO why Service.Authenticate must NOT scope its bootstrap resolution:
// under mode 'tags' the allowlist is not known until this query runs, so
// passing the empty one would resolve every tag-scoped connection to nothing.
// This test is what makes that consequence concrete instead of asserted in a
// comment.
func TestA11ScopedPrincipalWithEmptyAllowlistResolvesToZeroSites(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	mcpRepo := mcp.NewRepo(pool)
	siteRepo := site.NewRepo(pool)

	tenant := seedTenant(t, pool, "a11-empty-"+uuid.NewString()[:8])
	only, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenant, URL: "https://a11-empty.example.com", Name: "empty"})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}

	// Scope "site", allowlist empty. domain.IsSiteConstrained is true on the
	// scope label alone, so this reaches InScopedTenantTx.
	empty := domain.Principal{TenantID: tenant, UserID: uuid.New(), Scope: domain.ScopeSite}
	got, err := mcpRepo.ResolveScopeSites(ctx, empty, "all", nil, nil)
	if err != nil {
		t.Fatalf("empty-allowlist principal, mode 'all': %v", err)
	}
	t.Logf("scope=site with an EMPTY allowlist, mode 'all', in a tenant holding "+
		"site %v: resolved to %d", only.ID, len(got))
	if len(got) != 0 {
		t.Fatalf("scope 'site' with an empty allowlist resolved %d sites (%v) "+
			"in a tenant that holds %v; an empty allowlist must mean NO SITES, "+
			"never every site", len(got), got, only.ID)
	}
}
