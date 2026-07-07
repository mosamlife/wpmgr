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
--
-- GH #152 LOW fast-follow: joins tenants and excludes a soft-deleted tenant's
-- shares. This query backs auth.Repo.FirstActiveShareTenant, the login-time
-- "which tenant should this session activate" pick for a site-scoped
-- collaborator with no org membership — without this filter, a user whose
-- ONLY access was a share into a now-soft-deleted tenant would have their
-- session pinned to that (now-invisible) tenant on every login, landing in a
-- permanent 403 loop instead of the no-access screen.
SELECT site_shares.* FROM site_shares
JOIN tenants t ON t.id = site_shares.tenant_id
WHERE site_shares.user_id = $1
  AND (site_shares.expires_at IS NULL OR site_shares.expires_at > now())
  AND t.deleted_at IS NULL
ORDER BY site_shares.created_at ASC;

-- name: ListSharedSitesForUser :many
-- Shared-with-me, ENRICHED with each site's url + name and the owning org's name.
-- Runs under InUserTx: site_shares_self_read exposes the share rows, the M22
-- sites_shared_read policy exposes the (cross-tenant) site rows, and tenants has
-- no RLS. So the operator sees url/name/org_name for sites shared with them
-- without any membership in the owning org.
SELECT ss.id, ss.tenant_id, ss.site_id, ss.user_id, ss.role,
       ss.granted_by, ss.expires_at, ss.created_at,
       st.url   AS site_url,
       st.name  AS site_name,
       t.name   AS org_name
FROM site_shares ss
JOIN sites   st ON st.id = ss.site_id
JOIN tenants t  ON t.id  = ss.tenant_id
WHERE ss.user_id = $1
  AND (ss.expires_at IS NULL OR ss.expires_at > now())
ORDER BY ss.created_at ASC;

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
