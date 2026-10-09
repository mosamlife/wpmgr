// GH #802, end to end as wpmgr_app: the authority mcp.Service.AuthorizeGrant
// derives from a verdict comes only from the stored grant row that
// mcp.Repo.ReCheckGrantAuthorization read, in the tenant it read it in. A
// caller can hand the verdict back, but cannot use it in another tenant or
// widen it by editing its exported fields.
//
// Every read and every derivation goes through the shipped mcp.Repo and
// mcp.Service on the pool startPostgres returns (wpmgr_app: NOSUPERUSER,
// NOBYPASSRLS, asserted from inside a transaction on that pool). The superuser
// connection only arranges rows: a revoked grant, a narrowed capability set,
// an 'all' scope.
package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// newAGVStack is the assistant-request stack, with the pool's role asserted
// from inside a transaction before anything is read through it.
func newAGVStack(t *testing.T) *arStack {
	t.Helper()
	st := newARStack(t)
	if err := st.pool.InTenantTx(context.Background(), st.tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "authorize-grant proof")
		return nil
	}); err != nil {
		t.Fatalf("assert the pool's role: %v", err)
	}
	return st
}

// TestAuthorizeGrantAcceptsAnUneditedVerdictInItsOwnTenantAsAppRole is the
// positive control for the two refusals below: the same read and the same
// call, unedited and in the grant's own tenant, derive the stored grant.
func TestAuthorizeGrantAcceptsAnUneditedVerdictInItsOwnTenantAsAppRole(t *testing.T) {
	ctx := context.Background()
	st := newAGVStack(t)
	site := st.site(t)
	outside := st.site(t)
	g := st.grant(t, site)

	v, err := st.mcpRepo.ReCheckGrantAuthorization(ctx, st.tenant, g.ID)
	if err != nil || !v.Found || !v.Authorized {
		t.Fatalf("read the verdict: found=%v authorized=%v err=%v", v.Found, v.Authorized, err)
	}
	auth, err := st.mcpSvc.AuthorizeGrant(ctx, st.tenant, v)
	if err != nil {
		t.Fatalf("AuthorizeGrant(unedited verdict, its own tenant) err = %v, want nil", err)
	}
	t.Logf("own tenant: TenantID=%s GrantID=%s Allows(site)=%v Allows(outside)=%v cache purge=%v",
		auth.TenantID, auth.GrantID, auth.Sites.Allows(site), auth.Sites.Allows(outside),
		mcp.ToolPermitted(mcp.ToolSiteCachePurgeRequest, auth))
	if auth.TenantID != st.tenant || auth.GrantID != g.ID || auth.GrantName != g.Name {
		t.Fatalf("derived tenant %s grant %s %q, want the stored grant's: tenant %s grant %s %q",
			auth.TenantID, auth.GrantID, auth.GrantName, st.tenant, g.ID, g.Name)
	}
	if !auth.Sites.Allows(site) || auth.Sites.Allows(outside) {
		t.Fatalf("derived scope is not the stored {site}: Allows(site)=%v Allows(outside)=%v",
			auth.Sites.Allows(site), auth.Sites.Allows(outside))
	}
	if !mcp.ToolPermitted(mcp.ToolSiteCachePurgeRequest, auth) {
		t.Fatal("the stored grant holds mcp.cache.purge, and the derivation does not")
	}
	if _, err := mcp.SingleSitePrincipal(auth, site); err != nil {
		t.Fatalf("SingleSitePrincipal(site in scope) err = %v", err)
	}
}

// TestAuthorizeGrantRefusesAVerdictReadInAnotherTenantAsAppRole: a verdict read
// for a grant in tenant A, handed to AuthorizeGrant with tenant B's id.
func TestAuthorizeGrantRefusesAVerdictReadInAnotherTenantAsAppRole(t *testing.T) {
	ctx := context.Background()
	st := newAGVStack(t)
	siteA := st.site(t)
	g := st.grant(t, siteA)
	// An 'all' scope (the payload CHECK admits it), so a verdict honoured in
	// another tenant would resolve every site there, not an empty set.
	st.exec(t, `UPDATE mcp_grants SET site_scope_mode = 'all', scope_site_ids = '{}' WHERE id = $1`, g.ID)

	tenantB := seedTenant(t, st.pool, "agv-b-"+uuid.NewString()[:8])
	siteB := seedSite(t, st.pool, tenantB, "")

	v, err := st.mcpRepo.ReCheckGrantAuthorization(ctx, st.tenant, g.ID)
	if err != nil || !v.Found || !v.Authorized {
		t.Fatalf("control: verdict in tenant A: found=%v authorized=%v err=%v", v.Found, v.Authorized, err)
	}
	own, err := st.mcpSvc.AuthorizeGrant(ctx, st.tenant, v)
	if err != nil || !own.Sites.Allows(siteA) || own.Sites.Allows(siteB) {
		t.Fatalf("control: tenant A's own derivation err=%v Allows(siteA)=%v Allows(siteB)=%v, want nil/true/false",
			err, own.Sites.Allows(siteA), own.Sites.Allows(siteB))
	}

	cross, err := st.mcpSvc.AuthorizeGrant(ctx, tenantB, v)
	t.Logf("verdict read in tenant A, used in tenant B: err=%v TenantID=%s GrantID=%s Allows(siteB)=%v",
		err, cross.TenantID, cross.GrantID, cross.Sites.Allows(siteB))
	if !errors.Is(err, mcp.ErrGrantNotAuthorized) {
		t.Fatalf("AuthorizeGrant(tenant B, verdict read in tenant A) err = %v, want ErrGrantNotAuthorized", err)
	}
	if cross.Sites.Allows(siteB) || cross.Sites.Allows(siteA) || cross.TenantID != uuid.Nil || cross.GrantID != uuid.Nil {
		t.Fatalf("the refusal still carried an authorisation: %+v", cross)
	}

	// Tenant B's own read of the grant id finds nothing, and confers nothing.
	vb, err := st.mcpRepo.ReCheckGrantAuthorization(ctx, tenantB, g.ID)
	if err != nil || vb.Found || vb.Authorized {
		t.Fatalf("tenant B read tenant A's grant: found=%v authorized=%v err=%v", vb.Found, vb.Authorized, err)
	}
	if _, err := st.mcpSvc.AuthorizeGrant(ctx, tenantB, vb); !errors.Is(err, mcp.ErrGrantNotAuthorized) {
		t.Fatalf("AuthorizeGrant(tenant B, its own empty verdict) err = %v, want ErrGrantNotAuthorized", err)
	}
}

// TestAuthorizeGrantRefusesAnEditedVerdictAsAppRole: a row-derived verdict
// whose exported fields the caller edits before handing it back. The exported
// fields are views, so an edit is not honoured: a revoked grant stays refused,
// and an authorized one derives the stored grant and nothing more.
func TestAuthorizeGrantRefusesAnEditedVerdictAsAppRole(t *testing.T) {
	ctx := context.Background()
	st := newAGVStack(t)
	site := st.site(t)

	// (a) A revoked grant, Authorized set by the caller.
	revoked := st.grant(t, site)
	st.exec(t, `UPDATE mcp_grants SET status = 'revoked', revoked_at = now() WHERE id = $1`, revoked.ID)
	v, err := st.mcpRepo.ReCheckGrantAuthorization(ctx, st.tenant, revoked.ID)
	if err != nil || !v.Found || v.Authorized {
		t.Fatalf("control: revoked verdict found=%v authorized=%v err=%v", v.Found, v.Authorized, err)
	}
	if _, err := st.mcpSvc.AuthorizeGrant(ctx, st.tenant, v); !errors.Is(err, mcp.ErrGrantNotAuthorized) {
		t.Fatalf("control: unedited revoked verdict err = %v, want ErrGrantNotAuthorized", err)
	}
	v.Authorized, v.GrantStatus = true, "active"
	auth, err := st.mcpSvc.AuthorizeGrant(ctx, st.tenant, v)
	t.Logf("(a) revoked grant %s, Authorized edited to true: err=%v Allows(site)=%v cache purge=%v",
		revoked.ID, err, auth.Sites.Allows(site), mcp.ToolPermitted(mcp.ToolSiteCachePurgeRequest, auth))
	if !errors.Is(err, mcp.ErrGrantNotAuthorized) || auth.Sites.Allows(site) {
		t.Errorf("(a) a revoked grant with Authorized edited to true: err = %v Allows(site)=%v, want ErrGrantNotAuthorized and no site",
			err, auth.Sites.Allows(site))
	}

	// (b) A grant holding only mcp.sites.read, with mcp.cache.purge appended to
	// its verdict's Capabilities and its GrantID and GrantName pointed at
	// another grant.
	ro := st.grant(t, site)
	st.exec(t, `UPDATE mcp_grants SET capabilities = ARRAY['mcp.sites.read'] WHERE id = $1`, ro.ID)
	other := st.grant(t, site)
	v2, err := st.mcpRepo.ReCheckGrantAuthorization(ctx, st.tenant, ro.ID)
	if err != nil || !v2.Found || !v2.Authorized {
		t.Fatalf("control: read-only verdict found=%v authorized=%v err=%v", v2.Found, v2.Authorized, err)
	}
	base, err := st.mcpSvc.AuthorizeGrant(ctx, st.tenant, v2)
	if err != nil || mcp.ToolPermitted(mcp.ToolSiteCachePurgeRequest, base) || base.GrantID != ro.ID {
		t.Fatalf("control: read-only grant err=%v cache purge=%v GrantID=%s, want nil/false/%s",
			err, mcp.ToolPermitted(mcp.ToolSiteCachePurgeRequest, base), base.GrantID, ro.ID)
	}
	v2.Capabilities = append(v2.Capabilities, string(mcp.CapCachePurge))
	v2.GrantID, v2.GrantName = other.ID, "edited name"
	edited, err := st.mcpSvc.AuthorizeGrant(ctx, st.tenant, v2)
	t.Logf("(b) read-only grant %s, %s appended and GrantID set to %s: err=%v cache purge=%v GrantID=%s GrantName=%q",
		ro.ID, mcp.CapCachePurge, other.ID, err, mcp.ToolPermitted(mcp.ToolSiteCachePurgeRequest, edited),
		edited.GrantID, edited.GrantName)
	if err != nil || mcp.ToolPermitted(mcp.ToolSiteCachePurgeRequest, edited) ||
		edited.GrantID != ro.ID || edited.GrantName != ro.Name || !edited.Sites.Allows(site) {
		t.Errorf("(b) the edited verdict derived err=%v cache purge=%v GrantID=%s GrantName=%q Allows(site)=%v; want the stored grant: nil, false, %s, %q, true",
			err, mcp.ToolPermitted(mcp.ToolSiteCachePurgeRequest, edited), edited.GrantID, edited.GrantName,
			edited.Sites.Allows(site), ro.ID, ro.Name)
	}
}
