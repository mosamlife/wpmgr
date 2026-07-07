package middleware

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// clientAccess is the result of resolveClientAccess.
type clientAccess struct {
	clientIDs []uuid.UUID
	siteIDs   []uuid.UUID
}

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
	pool     *db.Pool
}

// NewAuthenticator builds an Authenticator. pool is used for the share-lookup
// query executed when a session user has no membership in the active tenant
// (site-scoped collaborator path).
func NewAuthenticator(sessions *auth.SessionManager, authSvc *auth.Service, keys *apikey.Service, pool *db.Pool) *Authenticator {
	return &Authenticator{sessions: sessions, authSvc: authSvc, keys: keys, pool: pool}
}

// Authenticate is middleware that attaches a Principal to the request context
// when valid credentials are present. It does NOT itself reject anonymous
// requests — RequireAuth/RequireRole/RequirePermission enforce that — so the
// same chain can host both public (login/register) and protected routes.
//
// Scope resolution for session principals:
//  1. API-key principals always get Scope="org".
//  2. Session principals with a full membership in the active tenant get
//     Scope="org" (unchanged from previous behaviour).
//  3. Session principals WITHOUT a membership check site_shares (via
//     GetActiveSharesForUserTenant under InUserTx / app.user_id):
//     - >=1 non-expired share  → Scope="site", AllowedSiteIDs=[site_ids],
//     Role=highest per-site role clamped to "admin" (never "owner").
//     - 0 shares               → TenantID=Nil (403 on tenant-scoped routes).
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
				Scope:    domain.ScopeOrg,
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

		// ADR-045 Phase 2 — reject sessions older than the user's last password
		// change. This is how a reset/change invalidates the user's OTHER
		// sessions (the SCS/Redis store cannot enumerate per-user sessions). A
		// session predating password_changed_at is treated as logged out.
		if changedAt, hasChanged := a.authSvc.PasswordChangedAt(ctx, userID); hasChanged {
			if a.sessions.AuthAt(ctx).Before(changedAt) {
				c.Next()
				return
			}
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
			if member {
				// Full org member: Scope="org", unchanged behaviour.
				p.Role = string(role)
				p.Scope = domain.ScopeOrg
			} else if a.tenantSoftDeleted(ctx, activeTenant) {
				// GH #152: the active tenant has been soft-deleted (or no longer
				// exists at all). RoleInTenant's backing query (ListMembershipsForUser)
				// already excludes a soft-deleted tenant's membership rows, so a
				// FORMER org member lands here too, not just a genuine non-member —
				// without this explicit check they could still fall through to the
				// site_shares/client_members branches below and regain ScopeSite
				// access via an unrelated collaborator/portal grant into the SAME
				// now-deleted tenant. Fail closed exactly like the "no shares, no
				// client memberships" case: clear TenantID (RequireTenant -> 403)
				// but keep UserID so /auth/me still works.
				p.TenantID = uuid.Nil
			} else {
				// No membership row. Check site_shares for collaborator access.
				// Run under InUserTx so the site_shares_self_read RLS policy
				// (USING user_id = app.user_id) allows the SELECT.
				shares, shareErr := a.resolveActiveShares(ctx, userID, activeTenant)
				if shareErr == nil && len(shares) > 0 {
					// Site-scoped collaborator: collect site IDs + highest role.
					// site_shares win and are EXCLUSIVE: when the user has one or
					// more active shares, client_members is NOT consulted. Merging
					// would let a share role (up to operator) escalate to cover
					// the client's sites, which the share never granted.
					p.Scope = domain.ScopeSite
					p.TenantID = activeTenant
					siteIDs := make([]uuid.UUID, 0, len(shares))
					highestRole := authz.RoleViewer
					for _, s := range shares {
						siteIDs = append(siteIDs, s.SiteID)
						r := authz.Role(s.Role)
						// Clamp per-site role to operator maximum (belt-and-braces).
						// A site-scoped collaborator must NEVER receive an effective
						// role of admin or owner regardless of what the share row
						// holds: admin would pass org-level permission checks before
						// the RequirePermission org_scope_required guard fires.
						// Clamping to operator here means the stored share role can
						// never escalate to org-level actions.
						if r.AtLeast(authz.RoleAdmin) {
							r = authz.RoleOperator
						}
						if r.AtLeast(highestRole) {
							highestRole = r
						}
					}
					p.AllowedSiteIDs = siteIDs
					p.Role = string(highestRole)
				} else {
					// No shares (or lookup error). Check client_members for portal
					// access. This branch is only reached when zero site_shares
					// exist so there is no risk of role mixing.
					ca, caErr := a.resolveClientAccess(ctx, userID, activeTenant)
					if caErr != nil || len(ca.clientIDs) == 0 {
						// No client memberships either: user has no access to this
						// tenant. Clear TenantID so RequireTenant returns 403, but
						// keep UserID so /auth/me still works.
						p.TenantID = uuid.Nil
					} else {
						// Portal principal: site allowlist is the union across ALL
						// client memberships in this tenant.
						p.Scope = domain.ScopeSite
						p.TenantID = activeTenant
						p.AllowedSiteIDs = ca.siteIDs
						p.ClientIDs = ca.clientIDs
						// Hard-clamp to RoleClient; client_members has no role
						// column so nothing stored can ever raise this above client.
						p.Role = string(authz.RoleClient)
					}
				}
			}
		}

		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	}
}

// tenantSoftDeleted reports whether tenantID has been soft-deleted (GH #152)
// or no longer exists at all. tenants carries no RLS (see db/schema.sql's
// file header), so this is a plain, unscoped read directly on the pool — no
// tx/GUC wrapper needed. Fail-closed on a lookup error: treat as deleted so a
// transient DB error can never leave a torn-down tenant's collaborator/portal
// grants reachable.
func (a *Authenticator) tenantSoftDeleted(ctx context.Context, tenantID uuid.UUID) bool {
	t, err := sqlc.New(a.pool.Pool).GetTenant(ctx, tenantID)
	if err != nil {
		return true
	}
	return t.DeletedAt.Valid
}

// resolveActiveShares loads non-expired site_shares for (userID, tenantID).
// It runs under InUserTx so that app.user_id is set and the
// site_shares_self_read RLS policy (USING user_id = app.user_id) allows the
// SELECT. A share lookup failure is treated as no-access (fail-closed).
func (a *Authenticator) resolveActiveShares(ctx context.Context, userID, tenantID uuid.UUID) ([]sqlc.SiteShare, error) {
	var shares []sqlc.SiteShare
	err := a.pool.InUserTx(ctx, userID, func(tx pgx.Tx) error {
		var txErr error
		shares, txErr = sqlc.New(tx).GetActiveSharesForUserTenant(ctx, sqlc.GetActiveSharesForUserTenantParams{
			UserID:   userID,
			TenantID: tenantID,
		})
		return txErr
	})
	return shares, err
}

// resolveClientAccess loads client_members rows for (userID, tenantID) and
// returns the union of client IDs and site IDs across all memberships. Runs
// under InUserTx so client_members_self_read (user_id = app.user_id) and
// sites_client_read policies allow the necessary SELECTs. Fail-closed: any
// error or empty result returns (clientAccess{}, nil) or (clientAccess{}, err).
func (a *Authenticator) resolveClientAccess(ctx context.Context, userID, tenantID uuid.UUID) (clientAccess, error) {
	var ca clientAccess
	err := a.pool.InUserTx(ctx, userID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).GetClientAccessForUserTenant(ctx, sqlc.GetClientAccessForUserTenantParams{
			UserID:   userID,
			TenantID: tenantID,
		})
		if qerr != nil {
			return qerr
		}
		clientSeen := make(map[uuid.UUID]struct{})
		siteSeen := make(map[uuid.UUID]struct{})
		for _, row := range rows {
			if _, ok := clientSeen[row.ClientID]; !ok {
				clientSeen[row.ClientID] = struct{}{}
				ca.clientIDs = append(ca.clientIDs, row.ClientID)
			}
			// LEFT JOIN: SiteID may be NULL for a zero-site client.
			if row.SiteID.Valid {
				sid := uuid.UUID(row.SiteID.Bytes)
				if _, ok := siteSeen[sid]; !ok {
					siteSeen[sid] = struct{}{}
					ca.siteIDs = append(ca.siteIDs, sid)
				}
			}
		}
		return nil
	})
	return ca, err
}
