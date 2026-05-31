-- name: CreateShare :one
-- Upsert on (site_id, user_id): if a share already exists for this (site, user)
-- pair, update the role, granted_by and expires_at in place.
INSERT INTO site_shares (tenant_id, site_id, user_id, role, granted_by, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (site_id, user_id)
DO UPDATE SET
    role       = EXCLUDED.role,
    granted_by = EXCLUDED.granted_by,
    expires_at = EXCLUDED.expires_at
RETURNING *;

-- name: ListSharesForSite :many
SELECT * FROM site_shares
WHERE site_id = $1
ORDER BY created_at ASC;

-- name: ListSharesForUser :many
-- Self-read: returns the caller's own non-expired shares across all tenants.
-- Must be run under a tx that sets app.user_id (site_shares_self_read policy).
SELECT * FROM site_shares
WHERE user_id = $1
  AND (expires_at IS NULL OR expires_at > now())
ORDER BY created_at ASC;

-- name: DeleteShare :execrows
DELETE FROM site_shares
WHERE site_id = $1 AND user_id = $2;

-- name: GetActiveSharesForUserTenant :many
-- Auth-time allowlist resolver: returns all non-expired site shares for a given
-- (user, tenant) pair. The result is used to build the AllowedSiteIDs list for a
-- site-scoped principal. Run under InUserTx (app.user_id set) or directly with
-- explicit params.
SELECT * FROM site_shares
WHERE user_id   = $1
  AND tenant_id = $2
  AND (expires_at IS NULL OR expires_at > now());
