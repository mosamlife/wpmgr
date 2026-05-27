package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// HeaderTenantOverride lets a multi-tenant session caller pick which of their
// tenants the request operates in (must be one they're a member of). It is NOT
// trusted on its own: membership is always re-verified. API-key callers ignore
// it (a key is bound to exactly one tenant).
const HeaderTenantOverride = "X-Tenant-ID"

// Authenticator derives the request Principal from EITHER a session cookie OR
// an `Authorization: Bearer <key>` API key. It replaces the old X-Tenant-ID
// stub: the active tenant comes from the authenticated principal, and for
// session callers the membership in that tenant is always verified.
type Authenticator struct {
	sessions *auth.SessionManager
	authSvc  *auth.Service
	keys     *apikey.Service
}

// NewAuthenticator builds an Authenticator.
func NewAuthenticator(sessions *auth.SessionManager, authSvc *auth.Service, keys *apikey.Service) *Authenticator {
	return &Authenticator{sessions: sessions, authSvc: authSvc, keys: keys}
}

// Authenticate is middleware that attaches a Principal to the request context
// when valid credentials are present. It does NOT itself reject anonymous
// requests — RequireAuth/RequireRole/RequirePermission enforce that — so the
// same chain can host both public (login/register) and protected routes.
func (a *Authenticator) Authenticate() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		// 1. Bearer API key takes precedence when present.
		if authzHeader := c.GetHeader("Authorization"); strings.HasPrefix(authzHeader, "Bearer ") {
			token := strings.TrimSpace(strings.TrimPrefix(authzHeader, "Bearer "))
			key, err := a.keys.Authenticate(ctx, token)
			if err != nil {
				httpx.Error(c, err)
				c.Abort()
				return
			}
			p := domain.Principal{
				Type:     domain.PrincipalAPIKey,
				APIKeyID: key.ID,
				TenantID: key.TenantID,
				Role:     string(key.Role),
			}
			c.Request = c.Request.WithContext(domain.WithPrincipal(ctx, p))
			c.Next()
			return
		}

		// 2. Session cookie.
		userID, activeTenant, ok := a.sessions.Current(ctx)
		if !ok {
			c.Next()
			return
		}

		// Allow a session caller to select an alternate tenant they belong to.
		if override := c.GetHeader(HeaderTenantOverride); override != "" {
			if tid, err := uuid.Parse(override); err == nil {
				activeTenant = tid
			}
		}

		p := domain.Principal{Type: domain.PrincipalUser, UserID: userID, TenantID: activeTenant}
		// Verify membership + resolve role in the active tenant (if one is set).
		if activeTenant != uuid.Nil {
			role, member := a.authSvc.RoleInTenant(ctx, userID, activeTenant)
			if !member {
				// The session's active tenant is not one the user belongs to:
				// proceed as an authenticated user WITHOUT a tenant so /auth/me
				// still works, but tenant-scoped routes will 403.
				p.TenantID = uuid.Nil
			} else {
				p.Role = string(role)
			}
		}
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	}
}
