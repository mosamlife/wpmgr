package admin

// gate.go: the authorisation gates for the /api/v1/admin route group.
//
// Two gates live here and they are deliberately kept apart:
//
//	requireSuperadmin
//	    The gate on the /admin group as a whole. Behaviour unchanged: the
//	    principal must be a logged-in user whose users.is_superadmin is true.
//
//	requireSuperadminOrSoleTenantOwner
//	    GH #322. Mounted on exactly ONE route, POST /admin/agent-mirror/check,
//	    and on nothing else. See its doc comment for what it admits, why, and
//	    for the property that must not be lost.
//
// Both read through adminGateStore rather than reaching for *db.Pool directly,
// so both are drivable from a unit test without a database. That indirection
// is the ONLY change to requireSuperadmin: the SQL it runs, the connection it
// runs it on, and its fail-closed condition are identical to what they were
// when it read the pool inline.

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// adminGateStore is the narrow read surface the admin gates need. Both reads
// happen at request time on every request; neither is cached and neither may
// be served from a boot snapshot. A tenant count that is stale in the
// permissive direction is a cross-tenant authorisation hole, so the gate pays
// one small indexed read rather than trusting a remembered answer.
type adminGateStore interface {
	// IsSuperadmin reports users.is_superadmin for the given user.
	IsSuperadmin(ctx context.Context, userID uuid.UUID) (bool, error)
	// IsSoleLiveTenantOwner reports whether this install has exactly one live
	// organisation AND the given user is an owner of a live organisation.
	// Both facts come from one statement, so they cannot be read a moment
	// apart and cannot disagree.
	IsSoleLiveTenantOwner(ctx context.Context, userID uuid.UUID) (bool, error)
}

// isSuperadminSQL is a targeted single-column read against users, which has no
// RLS (see db/schema.sql), so it runs on the bare pool with no tenant context.
const isSuperadminSQL = `SELECT is_superadmin FROM users WHERE id = $1`

// soleLiveTenantOwnerSQL answers the whole of the GH #322 question in ONE
// statement against ONE snapshot: is there exactly one organisation on this
// install, and does this caller own one? Splitting it into two round trips
// would let an organisation be created between them.
//
// If the count is 1 and the caller owns some live organisation, then the
// organisation they own IS that one; no third read is needed to tie them
// together.
//
// role = 'owner' is exact, not a ladder: memberships.role is constrained to
// ('owner', 'admin', 'operator', 'viewer') by memberships_role_check, and
// admin, operator and viewer are all refused here. Being a member of the only
// organisation is not enough.
//
// "live" means deleted_at IS NULL, matching every other tenant-resolving read
// in this repo (db/query/tenants.sql ListOrgsForUser and GetTenantForUser,
// db/query/memberships.sql ListMembershipsForUser, db/query/api_keys.sql
// GetAPIKeyByPrefix, db/query/site_shares.sql, db/query/client_members.sql,
// all GH #152). The reasoning for counting it this way rather than counting
// every row:
//
//   - A soft-deleted organisation cannot spend anything. Its members cannot
//     switch into it, its API keys stop resolving, and every read path already
//     hides it, so there is no principal able to act as that tenant and
//     therefore no budget of its own to protect. The gate exists to stop one
//     tenant spending another tenant's share; a tenant nobody can act as has
//     no share.
//   - Counting soft-deleted rows would refuse a legitimate single-tenant
//     install for the whole grace window, and then indefinitely on any install
//     where the purge worker is not running, on the strength of an
//     organisation that is invisible everywhere else in the product.
//   - Restoring it closes the path again immediately. RestoreTenant clears
//     deleted_at, the count is read fresh on the next request, and this gate
//     returns to refusing. There is nothing cached to invalidate.
const soleLiveTenantOwnerSQL = `
SELECT (SELECT count(*) FROM tenants WHERE deleted_at IS NULL) = 1
   AND EXISTS (
           SELECT 1
           FROM memberships m
           JOIN tenants t ON t.id = m.tenant_id
           WHERE m.user_id = $1
             AND m.role = 'owner'
             AND t.deleted_at IS NULL
       )`

// poolGateStore is the production adminGateStore, reading Postgres directly.
type poolGateStore struct{ pool *db.Pool }

func newPoolGateStore(pool *db.Pool) poolGateStore { return poolGateStore{pool: pool} }

func (s poolGateStore) IsSuperadmin(ctx context.Context, userID uuid.UUID) (bool, error) {
	var isSA bool
	if err := s.pool.QueryRow(ctx, isSuperadminSQL, userID).Scan(&isSA); err != nil {
		return false, err
	}
	return isSA, nil
}

// IsSoleLiveTenantOwner runs soleLiveTenantOwnerSQL under InUserTx. That is
// required, not incidental: memberships is under FORCE RLS and the only policy
// that lets a principal read its OWN membership rows across tenants is
// memberships_self_read, which keys on the app.user_id GUC that InUserTx sets.
// On the bare pool the EXISTS would silently see zero rows and the gate would
// refuse everyone. tenants carries no RLS, so the count sees every row.
func (s poolGateStore) IsSoleLiveTenantOwner(ctx context.Context, userID uuid.UUID) (bool, error) {
	var allowed bool
	err := s.pool.InUserTx(ctx, userID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, soleLiveTenantOwnerSQL, userID).Scan(&allowed)
	})
	if err != nil {
		return false, err
	}
	return allowed, nil
}

// denyAdminGate is the ONE refusal both gates use. The code and the message are
// byte-identical on every path, including the widened one, so a caller cannot
// learn from a refusal whether this install has one organisation or many.
func denyAdminGate(c *gin.Context) {
	httpx.Error(c, domain.Forbidden("superadmin_required", "superadmin access required"))
	c.Abort()
}

// requireSuperadmin is a Gin middleware that returns 403 unless the
// authenticated principal has is_superadmin=true. It gates the whole /admin
// group.
func requireSuperadmin(store adminGateStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := domain.PrincipalFromContext(c.Request.Context())
		if !ok || p.Type != domain.PrincipalUser {
			denyAdminGate(c)
			return
		}
		isSA, err := store.IsSuperadmin(c.Request.Context(), p.UserID)
		if err != nil || !isSA {
			denyAdminGate(c)
			return
		}
		c.Next()
	}
}

// requireSuperadminOrSoleTenantOwner gates POST /admin/agent-mirror/check, and
// nothing else on this install. It admits:
//
//	users.is_superadmin = true
//	OR the caller is an owner of the only live organisation on this install.
//
// Why the second arm exists (GH #322). The superadmin gate on this route is
// there so that one tenant cannot spend another tenant's share of the install's
// shared, unauthenticated upstream request budget. On an install with exactly
// one organisation there is no other tenant for that to protect, and what is
// left is the mechanics without the reason: set WPMGR_SUPERADMIN_EMAILS,
// restart, then discover that the seeder is additive only and never demotes, so
// getting back out means a manual UPDATE against users and another restart.
// That is a lot of platform-operator ceremony for someone checking whether
// their own fleet's agent reference is current.
//
// THE PROPERTY THAT MUST NOT BE LOST: the owner NEVER becomes a superadmin
// under this. No env var, no restart, no is_superadmin flag written anywhere,
// and no Sites-page redirect (the web app's isSuperadminAllowedPath guard in
// routes/_authed.tsx redirects superadmins AWAY from tenant pages, which is
// exactly why this action could not live beside the Agent column before). This
// admits one request on one route and confers nothing else. A second
// organisation appearing on the install closes the path again on the very next
// request, with no migration and nothing to clean up, because the count is read
// at request time and is never cached.
//
// Fail closed: an error reading either fact is a refusal, never an allow.
func requireSuperadminOrSoleTenantOwner(store adminGateStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := domain.PrincipalFromContext(c.Request.Context())
		if !ok || p.Type != domain.PrincipalUser {
			// An API-key principal is refused even when the key belongs to the
			// owner of the only organisation. This is an install-level action
			// against a shared upstream budget and the audit record wants a
			// human, which is the same reason requireSuperadmin refuses one.
			denyAdminGate(c)
			return
		}
		isSA, err := store.IsSuperadmin(c.Request.Context(), p.UserID)
		if err != nil {
			denyAdminGate(c)
			return
		}
		if isSA {
			c.Next()
			return
		}
		allowed, err := store.IsSoleLiveTenantOwner(c.Request.Context(), p.UserID)
		if err != nil || !allowed {
			denyAdminGate(c)
			return
		}
		c.Next()
	}
}
