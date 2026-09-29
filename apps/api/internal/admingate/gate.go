// Package admingate holds the ONE definition of instance-level authority (who
// may act on install-wide state rather than on one organisation's), plus the
// narrow database reads that decision needs.
//
// Why this is its own package (GH #322).
//
// The decision is asked in several places that live in different packages and
// cannot import each other cleanly:
//
//	internal/admin        the ROUTE GATE on POST /api/v1/admin/agent-mirror/check.
//	                      Answers "may this request proceed" and returns 403 if not.
//	internal/agentrelease the CAPABILITY FLAG agent_mirror.can_check_now on
//	                      GET /api/v1/fleet/agents. Answers "should the dashboard
//	                      show this viewer a Check now button".
//	internal/settings     the ROUTE GATE on /api/v1/settings/smtp (GET, PUT and
//	                      POST /test). The instance SMTP relay is one row for the
//	                      whole install, so it is instance configuration, not an
//	                      organisation setting, and a tenant role does not reach it.
//	internal/auth         the CAPABILITY FLAG can_manage_instance_email on the Me
//	                      response. Both it and the settings gate call
//	                      CanManageInstanceEmail.
//
// Those answers MUST be the same answer. If they are computed separately
// they can drift, and every way they can drift is a bug an operator sees: a
// button that always 403s, or a permission nobody is ever offered (which is
// precisely the state GH #322 was left in after 0.61.123 widened the gate with
// no button behind it). So the decision is written once, here, and both callers
// call it. Neither copies the SQL and neither re-derives the rule.
//
// Two decisions, not one. The agent-mirror pair asks ResolveInstanceAuthority
// (superadmin, or owner of the only live organisation). The instance-email pair
// asks InstanceEmailAuthority, which is that decision plus one more arm: on a
// self-hosted install, the account that set the install up (ArmInstallOwner).
// The extra arm is reachable only through InstanceEmailStore, a wider interface
// the agent-mirror callers never hold, so it cannot leak into their answer.
//
// Nothing in this package is cached. See Store's doc for why that matters.
package admingate

import (
	"context"
	"errors"
	"fmt"

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
	// SoleLiveTenantOwnedBy returns the id of the only live organisation on
	// this install when the given user is an owner of it, and uuid.Nil
	// otherwise (more than one live organisation, or the user owns none).
	// Both facts come from one statement, so they cannot be read a moment
	// apart and cannot disagree, and the id returned is the organisation that
	// statement found, never one named by the request.
	SoleLiveTenantOwnedBy(ctx context.Context, userID uuid.UUID) (uuid.UUID, error)
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
// together. The statement returns that organisation's id, and no row when
// either fact is false.
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
SELECT t.id
FROM memberships m
JOIN tenants t ON t.id = m.tenant_id
WHERE m.user_id = $1
  AND m.role = 'owner'
  AND t.deleted_at IS NULL
  AND (SELECT count(*) FROM tenants WHERE deleted_at IS NULL) = 1
LIMIT 1`

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

// SoleLiveTenantOwnedBy runs soleLiveTenantOwnerSQL under InUserTx. That is
// required, not incidental: memberships is under FORCE RLS and the only policy
// that lets a principal read its OWN membership rows across tenants is
// memberships_self_read, which keys on the app.user_id GUC that InUserTx sets.
// On the bare pool the membership read would silently see zero rows and the
// answer would be wrong in the REFUSING direction, which on the capability-flag
// caller means a button that never appears for the one person GH #322 built it
// for. tenants carries no RLS, so the count sees every row.
func (s PoolStore) SoleLiveTenantOwnedBy(ctx context.Context, userID uuid.UUID) (uuid.UUID, error) {
	var tenantID uuid.UUID
	err := s.pool.InUserTx(ctx, userID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, soleLiveTenantOwnerSQL, userID).Scan(&tenantID)
		if errors.Is(err, pgx.ErrNoRows) {
			tenantID = uuid.Nil
			return nil
		}
		return err
	})
	if err != nil {
		return uuid.Nil, err
	}
	return tenantID, nil
}

// Arm names the fact that admitted a caller to instance-level authority.
type Arm int

const (
	// ArmNone means the caller was not admitted.
	ArmNone Arm = iota
	// ArmSuperadmin means users.is_superadmin admitted the caller.
	ArmSuperadmin
	// ArmSoleLiveTenantOwner means the caller owns the only live organisation
	// on this install.
	ArmSoleLiveTenantOwner
	// ArmInstallOwner means the caller is the account recorded in
	// install_owner as having set up this install, on an install that is not
	// hosted. Only InstanceEmailAuthority returns it.
	ArmInstallOwner
)

// String names the arm for logs and audit metadata. The names are stable:
// they are written into audit rows as admitted_as.
func (a Arm) String() string {
	switch a {
	case ArmNone:
		return "none"
	case ArmSuperadmin:
		return "superadmin"
	case ArmSoleLiveTenantOwner:
		return "sole_live_tenant_owner"
	case ArmInstallOwner:
		return "install_owner"
	default:
		return fmt.Sprintf("arm(%d)", int(a))
	}
}

// Authority is the outcome of the instance-authority decision together with
// the fact that decided it. A caller that needs to know which arm admitted
// (the SMTP settings audit trail does) reads it from the decision itself
// rather than deriving it a second time.
type Authority struct {
	Arm Arm
	// TenantID names the organisation whose audit_log also records what the
	// caller does under this authority. It is found by the statement that
	// admitted them and is never taken from the request's active organisation.
	//
	//   ArmSoleLiveTenantOwner: the only live organisation, which the caller
	//                           owns.
	//   ArmInstallOwner:        the organisation created with the install
	//                           owner at bootstrap, only while it is live and
	//                           the caller still owns it; uuid.Nil otherwise.
	//
	// It is uuid.Nil for every other arm.
	TenantID uuid.UUID
}

// Admitted reports whether the decision granted instance-level authority.
func (a Authority) Admitted() bool { return a.Arm != ArmNone }

// HasInstanceAuthority is THE decision: does the principal carried on ctx hold
// instance-level authority on this install? It admits:
//
//	users.is_superadmin = true
//	OR the caller is an owner of the only live organisation on this install.
//
// Why the second arm exists (GH #322). Instance-level authority separates the
// operator of an install from its tenants: on an install with several
// organisations, no tenant role reaches install-wide state. On an install with
// exactly one organisation there is no other tenant to separate from, and what
// is left is the mechanics without the reason: set WPMGR_SUPERADMIN_EMAILS,
// restart, then discover that the seeder is additive only and never demotes, so
// getting back out means a manual UPDATE against users and another restart.
// That is a lot of platform-operator ceremony for a self-hosted owner managing
// their own install.
//
// THE PROPERTY THAT MUST NOT BE LOST: the owner NEVER becomes a superadmin
// under this. No env var, no restart, no is_superadmin flag written anywhere,
// and no Sites-page redirect (the web app's isSuperadminAllowedPath guard in
// routes/_authed.tsx redirects superadmins AWAY from tenant pages, which is why
// the admin console remains the superadmin's route to the agent-mirror check
// and why the Sites-page button is only ever seen by the owner arm). Each caller
// admits the specific routes it gates and nothing else. A second organisation
// appearing on the install closes the owner arm again on the very next request,
// with no migration and nothing to clean up, because the count is read at
// request time and is never cached. (The instance email settings have one
// further arm that survives a second organisation, ArmInstallOwner. It is
// InstanceEmailAuthority's, not this decision's, and the property above holds
// for it too: nothing is written to users.)
//
// An API-key principal is refused even when the key belongs to the owner of the
// only organisation. Install-level actions want a human in the audit record.
// Neither store read is performed for a non-user principal.
//
// Fail closed: an error reading either fact is a refusal, never an allow, and
// a failed is_superadmin read does NOT fall through to the widened arm (that
// would mean a broken users table silently downgraded this decision).
//
// store may be nil (the decision is not wired on this install), which is also
// a refusal.
func HasInstanceAuthority(ctx context.Context, store Store) bool {
	return ResolveInstanceAuthority(ctx, store).Admitted()
}

// ResolveInstanceAuthority is the decision HasInstanceAuthority reports,
// returned with the arm that admitted. The superadmin arm is consulted first,
// so a superadmin who also owns the only live organisation is reported as
// ArmSuperadmin.
func ResolveInstanceAuthority(ctx context.Context, store Store) Authority {
	a, err := resolveInstanceAuthority(ctx, store)
	if err != nil {
		return Authority{}
	}
	return a
}

// errStoreRead marks a refusal that happened because a fact could not be
// read, as opposed to a fact that was read and said no.
var errStoreRead = errors.New("admingate: instance authority read failed")

// resolveInstanceAuthority is ResolveInstanceAuthority with the two kinds of
// ArmNone kept apart. A nil error with ArmNone means every fact was read and
// none admitted. A non-nil error means a read failed, and the caller must not
// consult any further arm on the strength of that ArmNone: a broken read would
// otherwise silently become the next arm's answer.
func resolveInstanceAuthority(ctx context.Context, store Store) (Authority, error) {
	if store == nil {
		return Authority{}, nil
	}
	p, ok := domain.PrincipalFromContext(ctx)
	if !ok || p.Type != domain.PrincipalUser {
		return Authority{}, nil
	}
	isSA, err := store.IsSuperadmin(ctx, p.UserID)
	if err != nil {
		return Authority{}, fmt.Errorf("%w: superadmin: %w", errStoreRead, err)
	}
	if isSA {
		return Authority{Arm: ArmSuperadmin}, nil
	}
	tenantID, err := store.SoleLiveTenantOwnedBy(ctx, p.UserID)
	if err != nil {
		return Authority{}, fmt.Errorf("%w: sole live tenant owner: %w", errStoreRead, err)
	}
	if tenantID == uuid.Nil {
		return Authority{}, nil
	}
	return Authority{Arm: ArmSoleLiveTenantOwner, TenantID: tenantID}, nil
}

// CanRunAgentMirrorCheck answers whether the principal carried on ctx may
// trigger an immediate upstream agent-release mirror check on this install. It
// is exactly HasInstanceAuthority; the name is kept so the admin route gate and
// the fleet capability flag read as asking the question they ask.
func CanRunAgentMirrorCheck(ctx context.Context, store Store) bool {
	return HasInstanceAuthority(ctx, store)
}

// CanManageInstanceEmail is THE decision behind the instance SMTP settings: may
// the principal carried on ctx read, change and test the install-wide relay?
// It is InstanceEmailAuthority(...).Admitted().
//
// Two callers, one answer:
//
//	internal/settings the ROUTE GATE on /api/v1/settings/smtp (GET, PUT and
//	                  POST /test).
//	internal/auth     the CAPABILITY FLAG can_manage_instance_email on the Me
//	                  response, which tells the dashboard whether to offer the
//	                  page at all.
//
// The site-constraint arm is here, and not only in authz.RequireOrgScope in
// front of the route, so that the flag reflects the whole gate. A superadmin
// whose active session is a site-scoped collaboration is refused by the route,
// so the flag must be false for them too, or the dashboard would offer a page
// that always answers 403.
//
// No active organisation is NOT a refusal. Instance authority is a property of
// the person, not of the organisation they are looking at, so an operator with
// no membership anywhere is admitted exactly like one who has one.
func CanManageInstanceEmail(ctx context.Context, store InstanceEmailStore) bool {
	return InstanceEmailAuthority(ctx, store).Admitted()
}

// InstanceEmailAuthority is the decision CanManageInstanceEmail reports,
// returned with the arm that admitted. The settings route gate calls it so the
// handler behind the gate knows which arm admitted without asking again.
//
// It admits, in this order:
//
//	users.is_superadmin = true                        ArmSuperadmin
//	OR the caller owns the only live organisation     ArmSoleLiveTenantOwner
//	OR, on a self-hosted install only, the caller is
//	   the account recorded as having set it up       ArmInstallOwner
//
// The first two are ResolveInstanceAuthority, unchanged. The third is last so
// that a caller admitted before it existed keeps the arm it was admitted
// under, and with it the audit routing it had.
//
// ArmInstallOwner (owner ruling, 2026-09-29). On a self-hosted install the
// person who set the install up keeps the instance relay when a second
// organisation appears. On hosted, where organisations belong to different
// customers, nobody does. The authority belongs to the account recorded in
// install_owner, not to a role: another owner of the same organisation does
// not hold it, nobody inherits it when that account is deleted, and a
// disabled account is refused. Where no install owner was ever recorded the
// arm admits nobody, and WPMGR_SUPERADMIN_EMAILS is the remedy.
//
// Refused before any read: a nil store, no principal, a site-constrained
// principal, and any principal that is not a signed-in user. The principal
// type is checked here and not only inside ResolveInstanceAuthority, because
// that decision answers ArmNone with no error for an API key, which would
// otherwise fall through to ArmInstallOwner for a key minted by the install
// owner. Install-level changes want a human in the audit record.
//
// Fail closed: a read error in either earlier arm refuses and does NOT fall
// through to ArmInstallOwner, and a read error in ArmInstallOwner refuses.
func InstanceEmailAuthority(ctx context.Context, store InstanceEmailStore) Authority {
	if store == nil {
		return Authority{}
	}
	p, ok := domain.PrincipalFromContext(ctx)
	if !ok || p.IsSiteConstrained() || p.Type != domain.PrincipalUser {
		return Authority{}
	}
	a, err := resolveInstanceAuthority(ctx, store)
	if err != nil {
		return Authority{}
	}
	if a.Admitted() {
		return a
	}
	if !store.SelfHosted() {
		return Authority{}
	}
	isOwner, home, err := store.InstallOwnerHomeTenant(ctx, p.UserID)
	if err != nil || !isOwner {
		return Authority{}
	}
	return Authority{Arm: ArmInstallOwner, TenantID: home}
}

// InstanceEmailStore is the read surface InstanceEmailAuthority needs: Store,
// plus the install-owner facts. Only the instance-email callers hold one. The
// agent-mirror callers hold a plain Store, so ArmInstallOwner is unreachable
// from CanRunAgentMirrorCheck at the type level.
type InstanceEmailStore interface {
	Store
	// SelfHosted reports whether this install is self-hosted (WPMGR_HOSTED is
	// not true). It is fixed when the store is built.
	SelfHosted() bool
	// InstallOwnerHomeTenant reports whether userID is the install owner and,
	// if so, the organisation to record their changes in: the organisation
	// created with them at bootstrap, only while it is live and they still
	// own it, and uuid.Nil otherwise. isOwner is false when no install owner
	// is recorded, when another account is, and when the recorded account is
	// not active.
	InstallOwnerHomeTenant(ctx context.Context, userID uuid.UUID) (isOwner bool, home uuid.UUID, err error)
}

// installOwnerHomeTenantSQL answers both InstallOwnerHomeTenant facts in one
// statement.
//
// No row: the caller is not the install owner. That covers an empty
// install_owner (no owner was ever proven), a row naming someone else, a row
// naming a user that has been deleted (the row has no foreign key, so it keeps
// naming the missing id and nobody inherits it), and a user whose status is
// not 'active'.
//
// A row: the caller is the install owner, and the value is the home
// organisation, or NULL when that organisation is gone, soft-deleted, or no
// longer owned by the caller.
//
// memberships is FORCE RLS, so the EXISTS runs under InUserTx and reads the
// caller's own rows through memberships_self_read. install_owner, users and
// tenants carry no RLS.
const installOwnerHomeTenantSQL = `
SELECT CASE
         WHEN t.id IS NOT NULL
          AND t.deleted_at IS NULL
          AND EXISTS (
                SELECT 1 FROM memberships m
                WHERE m.user_id = io.user_id
                  AND m.tenant_id = io.tenant_id
                  AND m.role = 'owner')
         THEN io.tenant_id
       END
FROM install_owner io
JOIN users u ON u.id = io.user_id AND u.status = 'active'
LEFT JOIN tenants t ON t.id = io.tenant_id
WHERE io.user_id = $1`

// InstanceEmailPoolStore is the production InstanceEmailStore: PoolStore plus
// the install-owner read and the hosted flag.
//
// The zero value's selfHosted is false, which is the hosted answer: the
// install-owner arm is off unless NewInstanceEmailPoolStore was told the
// install is not hosted.
type InstanceEmailPoolStore struct {
	PoolStore
	selfHosted bool
}

// NewInstanceEmailPoolStore builds the production InstanceEmailStore over
// pool. hosted is cfg.Hosted.Enabled; the install-owner arm is live only when
// it is false.
func NewInstanceEmailPoolStore(pool *db.Pool, hosted bool) InstanceEmailPoolStore {
	return InstanceEmailPoolStore{PoolStore: NewPoolStore(pool), selfHosted: !hosted}
}

// SelfHosted reports the flag fixed at construction.
func (s InstanceEmailPoolStore) SelfHosted() bool { return s.selfHosted }

// InstallOwnerHomeTenant runs installOwnerHomeTenantSQL under InUserTx; see
// its doc for what each answer means.
func (s InstanceEmailPoolStore) InstallOwnerHomeTenant(ctx context.Context, userID uuid.UUID) (bool, uuid.UUID, error) {
	var (
		isOwner bool
		home    *uuid.UUID
	)
	err := s.pool.InUserTx(ctx, userID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, installOwnerHomeTenantSQL, userID).Scan(&home)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		isOwner = true
		return nil
	})
	if err != nil {
		return false, uuid.Nil, err
	}
	if !isOwner || home == nil {
		return isOwner, uuid.Nil, nil
	}
	return true, *home, nil
}
