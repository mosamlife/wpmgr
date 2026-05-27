package authz

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

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

// RequirePermission aborts unless the principal's role holds the permission.
func RequirePermission(perm Permission) gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := principalWithTenant(c)
		if !ok {
			return
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
