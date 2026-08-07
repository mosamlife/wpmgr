-- name: CreateUser :one
INSERT INTO users (email, password_hash, oidc_subject, oidc_issuer, name)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: GetUserByOIDC :one
SELECT * FROM users WHERE oidc_issuer = $1 AND oidc_subject = $2;

-- name: ListUsersByLegacyOIDCSubject :many
-- The pre-m110 identity, looked up by SUBJECT ALONE, so a sign-in can still
-- find someone whose user_identities row was never written (see
-- AdoptLegacyIdentity) even after the operator repointed WPMGR_OIDC_ISSUER.
--
-- :many, not :one, and LIMIT 2 is the point rather than an optimisation. The old
-- users_oidc_identity_key was unique on (oidc_issuer, oidc_subject), so two rows
-- CAN share a subject under different issuers. Subject alone does not identify
-- anyone in that case, and adopting either one would be a coin flip between two
-- humans. The caller sees both and refuses to guess; two rows is all it needs to
-- know that.
SELECT * FROM users WHERE oidc_subject = $1 ORDER BY created_at, id LIMIT 2;

-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: SetUserPasswordHash :exec
-- Stamps password_changed_at so the Authenticator invalidates the user's other
-- sessions (ADR-045 Phase 2).
UPDATE users SET password_hash = $2, password_changed_at = now(), updated_at = now() WHERE id = $1;

-- name: GetUserPasswordChangedAt :one
-- Lightweight per-request lookup for the session reject-stale check.
SELECT password_changed_at FROM users WHERE id = $1;

-- name: LinkUserOIDC :one
UPDATE users
SET oidc_issuer = $2, oidc_subject = $3, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: TouchUserLogin :exec
UPDATE users SET last_login_at = now() WHERE id = $1;

-- name: AdminListUsers :many
-- List all users across the instance with org_count (number of memberships).
-- search filters email or name case-insensitively; pass NULL to list all.
-- Ordered by created_at DESC, id DESC (stable keyset). Used only by the
-- superadmin area; it bypasses the tenant-scoped InTenantTx path.
SELECT
    u.id,
    u.email,
    u.name,
    u.status,
    u.email_verified_at IS NOT NULL   AS email_verified,
    u.created_at,
    u.last_login_at,
    u.is_superadmin,
    COUNT(m.tenant_id)::bigint        AS org_count
FROM users u
LEFT JOIN memberships m ON m.user_id = u.id
WHERE ($1::text IS NULL
    OR u.email ILIKE '%' || $1 || '%'
    OR u.name  ILIKE '%' || $1 || '%')
GROUP BY u.id
ORDER BY u.created_at DESC, u.id DESC
LIMIT  $2
OFFSET $3;

-- name: AdminGetUserByID :one
-- Lightweight superadmin fetch that includes is_superadmin.
SELECT id, email, name, status, email_verified_at IS NOT NULL AS email_verified,
       created_at, last_login_at, is_superadmin
FROM users WHERE id = $1;

-- name: AdminDeleteUser :execrows
DELETE FROM users WHERE id = $1;

-- name: AdminSetUserStatus :one
UPDATE users SET status = $2, updated_at = now()
WHERE id = $1
RETURNING id, email, name, status, email_verified_at IS NOT NULL AS email_verified,
          created_at, last_login_at, is_superadmin;

-- name: AdminSetSuperadminByEmail :exec
-- Boot seeder only. Sets is_superadmin=true for an email if the user exists.
-- No-op for unknown emails.
UPDATE users SET is_superadmin = true WHERE email = $1;

-- name: AdminInstanceStats :one
SELECT
    (SELECT COUNT(*) FROM users)       AS user_count,
    (SELECT COUNT(*) FROM tenants)     AS org_count,
    (SELECT COUNT(*) FROM sites)       AS site_count;

-- name: AdminUserSoleTenants :many
-- Tenants where @user_id is the ONLY member (so deleting them orphans the org),
-- with each tenant's name + site count. Run under Pool.InAgentTx
-- (memberships_agent + sites_agent) so the cross-tenant read is allowed.
SELECT m.tenant_id,
       t.name AS tenant_name,
       (SELECT COUNT(*) FROM sites s WHERE s.tenant_id = m.tenant_id)::bigint AS site_count
FROM memberships m
JOIN tenants t ON t.id = m.tenant_id
WHERE m.tenant_id IN (SELECT m2.tenant_id FROM memberships m2 WHERE m2.user_id = @user_id)
GROUP BY m.tenant_id, t.name
HAVING COUNT(DISTINCT m.user_id) = 1;

-- name: AdminDeleteEmptyTenant :one
-- Deletes a tenant ONLY if it has no memberships and no sites, returning whether
-- a row was removed. Delegates to the SECURITY DEFINER admin_delete_empty_tenant
-- function: the tenant's ON DELETE CASCADE reaches audit_log, which wpmgr_app may
-- NOT delete (insert-only), so a direct DELETE fails 42501. The function runs as
-- its owner (which keeps DELETE on audit_log) and pins app.agent='on' so its
-- emptiness checks see rows under FORCE RLS.
SELECT admin_delete_empty_tenant(@tenant_id) AS deleted;
