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
-- name: ListMembershipsForUser :many
SELECT * FROM memberships
WHERE user_id = $1
ORDER BY created_at ASC;

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
