package authz

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// RequireSiteAccess returns a Gin middleware that enforces the per-site access
// allowlist for site-scoped principals. It must be applied AFTER RequireAuth
// and RequireTenant (and typically after RequirePermission) on per-site routes.
//
// Behaviour:
//   - Scope == "org" (or ""): no-op; full org members pass unconditionally.
//   - Scope == "site": the :siteId path parameter is parsed and checked against
//     p.AllowedSiteIDs. If siteId is absent from the allowlist, the request is
//     aborted with 404 (not 403, to avoid confirming site existence to a caller
//     who has no access to this tenant at all).
//
// This is a belt-and-braces guard in front of the Postgres RESTRICTIVE RLS
// policy (the policy itself will also deny the rows). Returning 404 here
// provides a clean UX and avoids leaking tenant structure.
func RequireSiteAccess(siteIDParam string) gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := domain.PrincipalFromContext(c.Request.Context())
		if !ok {
			// No principal — RequireAuth should have caught this; be safe.
			abort(c, domain.Unauthorized("unauthenticated", "authentication required"))
			return
		}

		// Org-scoped (or legacy zero-value) principals: no site allowlist check.
		if p.Scope != domain.ScopeSite {
			c.Next()
			return
		}

		// Parse the :siteId (or caller-supplied param name) from the path.
		rawID := c.Param(siteIDParam)
		siteID, err := uuid.Parse(rawID)
		if err != nil {
			// Malformed UUID: the route handler would fail anyway; 404 is fine.
			abort(c, domain.NotFound("site_not_found", "site not found"))
			return
		}

		// Check the allowlist.
		for _, allowed := range p.AllowedSiteIDs {
			if allowed == siteID {
				c.Next()
				return
			}
		}

		// siteId not in AllowedSiteIDs: return 404 (not 403) so the caller
		// cannot distinguish "site exists but you can't see it" from "no such
		// site". This mirrors how RLS silently hides rows.
		abort(c, domain.NotFound("site_not_found", "site not found"))
	}
}

// RequireOrgScope blocks site-scoped principals (outside collaborators) from a
// route entirely. Use it on org-level or cross-site resources that have no
// single :siteId to bind to — e.g. the bulk update-run orchestrator (a run can
// span multiple sites, so a per-site allowlist check is insufficient). Org
// members and API-key principals pass through unchanged.
func RequireOrgScope() gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := domain.PrincipalFromContext(c.Request.Context())
		if !ok {
			abort(c, domain.Unauthorized("unauthenticated", "authentication required"))
			return
		}
		if p.Scope == domain.ScopeSite {
			abort(c, domain.Forbidden("org_scope_required", "this resource is not available to site-scoped access"))
			return
		}
		c.Next()
	}
}

// These Gin middlewares enforce authentication and authorization against the
// Principal placed on the request context by the auth Authenticator. They live
// in authz (not middleware) so domain handlers can apply per-route RBAC without
// importing the middleware package, which itself depends on auth/apikey (that
// would form an import cycle).

// RequireAuth aborts with 401 unless an authenticated principal is present.
func RequireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, ok := domain.PrincipalFromContext(c.Request.Context()); !ok {
			abort(c, domain.Unauthorized("unauthenticated", "authentication required"))
			return
		}
		c.Next()
	}
}

// RequireTenant aborts unless the principal has an active tenant.
func RequireTenant() gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := domain.PrincipalFromContext(c.Request.Context())
		if !ok {
			abort(c, domain.Unauthorized("unauthenticated", "authentication required"))
			return
		}
		if p.TenantID == uuid.Nil {
			abort(c, domain.Forbidden("tenant_required", "no active tenant for this principal"))
			return
		}
		c.Next()
	}
}

// RequireRole aborts unless the principal's role meets the minimum.
func RequireRole(min Role) gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := principalWithTenant(c)
		if !ok {
			return
		}
		if !Role(p.Role).AtLeast(min) {
			abort(c, domain.Forbidden("insufficient_role", "your role does not permit this action"))
			return
		}
		c.Next()
	}
}

// orgLevelPerms is the set of permissions that require Scope=="org". A
// site-scoped collaborator (Scope=="site") must never be able to exercise any
// of these, regardless of the role they were granted on a specific site. This
// is the belt-and-braces guard at the permission layer; the RLS restrictive
// policies on the underlying tables are the database-level guard.
var orgLevelPerms = map[Permission]struct{}{
	PermMemberManage: {},
	PermMemberRead:   {},
	PermAPIKeyRead:   {},
	PermAPIKeyManage: {},
	PermAuditRead:    {},
	PermTenantManage: {},
}

// RequirePermission aborts unless the principal's role holds the permission.
// FIX 1 (CRITICAL): if the requested permission is org-level AND the principal's
// Scope != "org", the request is rejected with 403 (code 'org_scope_required')
// REGARDLESS of role. This prevents a site-scoped collaborator from ever
// reaching member management, API keys, audit log, or tenant management.
func RequirePermission(perm Permission) gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := principalWithTenant(c)
		if !ok {
			return
		}
		// Org-level permission guard: reject site-scoped principals unconditionally.
		if _, isOrgLevel := orgLevelPerms[perm]; isOrgLevel {
			if p.Scope == domain.ScopeSite {
				abort(c, domain.Forbidden("org_scope_required", "this action requires full organisation membership"))
				return
			}
		}
		if !Allows(Role(p.Role), perm) {
			abort(c, domain.Forbidden("insufficient_permission", "your role does not permit this action"))
			return
		}
		c.Next()
	}
}

// principalWithTenant fetches the principal and enforces authn + active tenant,
// aborting the request (and returning ok=false) when either is missing.
func principalWithTenant(c *gin.Context) (domain.Principal, bool) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		abort(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return domain.Principal{}, false
	}
	if p.TenantID == uuid.Nil {
		abort(c, domain.Forbidden("tenant_required", "no active tenant for this principal"))
		return domain.Principal{}, false
	}
	return p, true
}

func abort(c *gin.Context, err error) {
	httpx.Error(c, err)
	c.Abort()
}
