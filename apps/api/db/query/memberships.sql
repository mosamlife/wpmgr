-- name: CreateMembership :one
INSERT INTO memberships (user_id, tenant_id, role)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetMembership :one
SELECT * FROM memberships
WHERE user_id = $1 AND tenant_id = $2;

-- name: ListMembershipsForTenant :many
SELECT * FROM memberships
WHERE tenant_id = $1
ORDER BY created_at ASC
LIMIT $2 OFFSET $3;

-- ListMembershipsForUser reads the caller's own memberships across all tenants.
-- It relies on the memberships_self_read policy (app.user_id GUC), so it must be
-- run via InUserTx, not InTenantTx.
--
-- GH #152: joins tenants and excludes t.deleted_at IS NOT NULL rows. This is
-- the SINGLE highest-leverage soft-delete read-path filter in the whole
-- feature — every caller of authz-critical auth.Service.RoleInTenant (the
-- session auth middleware's membership check, org activate/rename) and
-- auth.Service.Me (/auth/me) is backed by this query, as is every login-time
-- "which tenant should this session activate" resolution (password login,
-- OIDC, 2FA challenge). A soft-deleted org's membership row therefore becomes
-- invisible to ALL of those call sites in one place, the instant DELETE
-- /orgs/{orgId} commits, without a per-call-site patch.
-- name: ListMembershipsForUser :many
SELECT m.* FROM memberships m
JOIN tenants t ON t.id = m.tenant_id
WHERE m.user_id = $1 AND t.deleted_at IS NULL
ORDER BY m.created_at ASC;

-- name: UpdateMembershipRole :one
UPDATE memberships
SET role = $3, updated_at = now()
WHERE user_id = $1 AND tenant_id = $2
RETURNING *;

-- name: DeleteMembership :execrows
DELETE FROM memberships
WHERE user_id = $1 AND tenant_id = $2;

-- name: UpsertOwnerMembership :one
-- Tenant-create helper: insert an owner membership for the creator; on conflict
-- (e.g. migration replay or second create attempt) update role to 'owner'.
INSERT INTO memberships (user_id, tenant_id, role)
VALUES ($1, $2, 'owner')
ON CONFLICT (user_id, tenant_id)
DO UPDATE SET role = 'owner', updated_at = now()
RETURNING *;
