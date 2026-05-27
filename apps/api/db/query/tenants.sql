-- name: CreateTenant :one
INSERT INTO tenants (name, slug)
VALUES ($1, $2)
RETURNING *;

-- name: GetTenant :one
SELECT * FROM tenants
WHERE id = $1;

-- name: ListTenants :many
SELECT * FROM tenants
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- ListTenantsForUser returns only the tenants the given user is a member of.
-- It joins memberships under the memberships_self_read policy (app.user_id GUC),
-- so it MUST be run via InUserTx; the join itself restricts the result to the
-- caller's own memberships, preventing cross-tenant enumeration.
-- name: ListTenantsForUser :many
SELECT t.id, t.name, t.slug, t.created_at, t.updated_at
FROM tenants t
JOIN memberships m ON m.tenant_id = t.id
WHERE m.user_id = $1
ORDER BY t.created_at DESC
LIMIT $2 OFFSET $3;

-- GetTenantForUser returns a tenant by id only when the given user is a member.
-- Like ListTenantsForUser it relies on the memberships_self_read policy and must
-- be run via InUserTx; a non-member (or unknown tenant) yields no rows.
-- name: GetTenantForUser :one
SELECT t.id, t.name, t.slug, t.created_at, t.updated_at
FROM tenants t
JOIN memberships m ON m.tenant_id = t.id
WHERE t.id = $1 AND m.user_id = $2;
