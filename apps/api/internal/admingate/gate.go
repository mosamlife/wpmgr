// Package admingate holds the ONE definition of who may run an install-level
// agent-mirror check, plus the narrow database reads that decision needs.
//
// Why this is its own package (GH #322).
//
// The decision is asked in two places that live in different packages and
// cannot import each other cleanly:
//
//	internal/admin        the ROUTE GATE on POST /api/v1/admin/agent-mirror/check.
//	                      Answers "may this request proceed" and returns 403 if not.
//	internal/agentrelease the CAPABILITY FLAG agent_mirror.can_check_now on
//	                      GET /api/v1/fleet/agents. Answers "should the dashboard
//	                      show this viewer a Check now button".
//
// Those two answers MUST be the same answer. If they are computed separately
// they can drift, and every way they can drift is a bug an operator sees: a
// button that always 403s, or a permission nobody is ever offered (which is
// precisely the state GH #322 was left in after 0.61.123 widened the gate with
// no button behind it). So the decision is written once, here, and both callers
// call it. Neither copies the SQL and neither re-derives the rule.
//
// Nothing in this package is cached. See Store's doc for why that matters.
package admingate

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Store is the narrow read surface the decision needs. Both reads happen at
// request time on every request; neither is cached and neither may be served
// from a boot snapshot. A tenant count that is stale in the permissive
// direction is a cross-tenant authorisation hole, so the decision pays one
// small indexed read rather than trusting a remembered answer.
type Store interface {
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
//     deleted_at, the count is read fresh on the next request, and this
//     decision returns to refusing. There is nothing cached to invalidate.
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

// PoolStore is the production Store, reading Postgres directly.
type PoolStore struct{ pool *db.Pool }

// NewPoolStore builds the production Store over the given pool.
func NewPoolStore(pool *db.Pool) PoolStore { return PoolStore{pool: pool} }

func (s PoolStore) IsSuperadmin(ctx context.Context, userID uuid.UUID) (bool, error) {
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
// On the bare pool the EXISTS would silently see zero rows and the answer would
// be wrong in the REFUSING direction, which on the capability-flag caller means
// a button that never appears for the one person GH #322 built it for. tenants
// carries no RLS, so the count sees every row.
func (s PoolStore) IsSoleLiveTenantOwner(ctx context.Context, userID uuid.UUID) (bool, error) {
	var allowed bool
	err := s.pool.InUserTx(ctx, userID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, soleLiveTenantOwnerSQL, userID).Scan(&allowed)
	})
	if err != nil {
		return false, err
	}
	return allowed, nil
}

// CanRunAgentMirrorCheck is THE decision: may the principal carried on ctx
// trigger an immediate upstream agent-release mirror check on this install?
// It admits:
//
//	users.is_superadmin = true
//	OR the caller is an owner of the only live organisation on this install.
//
// Why the second arm exists (GH #322). The superadmin gate on this action is
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
// routes/_authed.tsx redirects superadmins AWAY from tenant pages, which is why
// the admin console remains the superadmin's route to this action and why the
// Sites-page button is only ever seen by the owner arm). This admits one
// request on one route, and reveals one boolean on one dashboard field. A
// second organisation appearing on the install closes the path again on the
// very next request, with no migration and nothing to clean up, because the
// count is read at request time and is never cached.
//
// An API-key principal is refused even when the key belongs to the owner of the
// only organisation. This is an install-level action against a shared upstream
// budget and the audit record wants a human. Neither store read is performed
// for a non-user principal.
//
// Fail closed: an error reading either fact is a refusal, never an allow, and
// a failed is_superadmin read does NOT fall through to the widened arm (that
// would mean a broken users table silently downgraded this decision).
//
// store may be nil (the decision is not wired on this install), which is also
// a refusal.
func CanRunAgentMirrorCheck(ctx context.Context, store Store) bool {
	if store == nil {
		return false
	}
	p, ok := domain.PrincipalFromContext(ctx)
	if !ok || p.Type != domain.PrincipalUser {
		return false
	}
	isSA, err := store.IsSuperadmin(ctx, p.UserID)
	if err != nil {
		return false
	}
	if isSA {
		return true
	}
	allowed, err := store.IsSoleLiveTenantOwner(ctx, p.UserID)
	if err != nil {
		return false
	}
	return allowed
}
