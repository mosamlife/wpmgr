-- name: CreateSite :one
-- tenant_id is supplied explicitly for defense-in-depth; RLS additionally
-- enforces that it matches the current app.tenant_id setting.
INSERT INTO sites (tenant_id, url, name, status, wp_version, php_version)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetSite :one
SELECT * FROM sites
WHERE id = $1 AND tenant_id = $2;

-- name: ListSites :many
-- Defaults to hiding archived sites (ADR-041). When sqlc.narg('state') is set
-- the list is filtered to exactly that connection_state (e.g. 'archived' for
-- the archived chip); when it is NULL every non-archived site is returned.
SELECT * FROM sites
WHERE tenant_id = $1
  AND (sqlc.narg('tag')::text IS NULL OR sqlc.narg('tag')::text = ANY (tags))
  AND (
        (sqlc.narg('state')::text IS NULL AND connection_state <> 'archived')
        OR sqlc.narg('state')::text = connection_state
      )
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: DeleteSite :execrows
DELETE FROM sites
WHERE id = $1 AND tenant_id = $2;

-- name: SetSiteTags :one
UPDATE sites
SET tags = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: SetSiteAgeRecipient :one
-- Stores the per-site age PUBLIC recipient backups are encrypted to. The CP
-- never holds the matching identity (private key); it cannot decrypt backups.
UPDATE sites
SET age_recipient = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: UpdateSiteMetadata :one
-- Tenant-scoped metadata update (used by the agent path inside the resolved
-- site's own tenant scope).
UPDATE sites
SET wp_version   = $3,
    php_version  = $4,
    server_info  = $5,
    multisite    = $6,
    active_theme = $7,
    components   = $8,
    last_seen_at = now(),
    health_status = 'healthy',
    updated_at   = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: TouchSiteSeen :one
UPDATE sites
SET last_seen_at = now(),
    health_status = 'healthy',
    updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- ---------------------------------------------------------------------------
-- Enrollment path (app.enroll GUC). These run before any tenant scope exists.
-- ---------------------------------------------------------------------------

-- name: GetSiteByURLForEnroll :one
SELECT * FROM sites
WHERE tenant_id = $1 AND url = $2;

-- name: CreateSiteForEnroll :one
INSERT INTO sites (tenant_id, url, name, status, wp_version, php_version,
                   agent_public_key, enrolled_at, last_seen_at, health_status, tags)
VALUES ($1, $2, $3, 'active', $4, $5, $6, now(), now(), 'healthy', $7)
RETURNING *;

-- name: AttachAgentToSite :one
-- Re-enrollment: rotate the agent key and mark the site active/enrolled again.
UPDATE sites
SET agent_public_key = $3,
    status = 'active',
    enrolled_at = now(),
    last_seen_at = now(),
    health_status = 'healthy',
    wp_version = $4,
    php_version = $5,
    updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- ---------------------------------------------------------------------------
-- Agent-auth path (app.agent GUC). Resolve a site by its agent public key.
-- ---------------------------------------------------------------------------

-- name: GetSiteByAgentKey :one
SELECT * FROM sites
WHERE agent_public_key = $1 AND agent_public_key <> '';

-- ---------------------------------------------------------------------------
-- Health-check job (runs in each enrolled site's tenant scope).
-- ---------------------------------------------------------------------------

-- name: ListEnrolledSitesAllTenants :many
-- Cross-tenant enumeration for the periodic health job. Runs under the
-- app.agent GUC (sites_agent policy) since it spans tenants.
SELECT id, tenant_id, last_seen_at, health_status FROM sites
WHERE enrolled_at IS NOT NULL;

-- name: MarkSiteUnreachable :execrows
-- Marks a site unreachable. Runs under app.agent GUC (cross-tenant job).
UPDATE sites
SET health_status = 'unreachable', updated_at = now()
WHERE id = $1 AND health_status <> 'unreachable';

-- name: ListEnrolledSitesForProbe :many
-- Cross-tenant enumeration of enrolled sites WITH their URL for the M5 uptime
-- probe job. Runs under the app.agent GUC (sites_agent policy) since it spans
-- tenants. Only enrolled sites have an agent URL worth probing.
SELECT id, tenant_id, url, health_status FROM sites
WHERE enrolled_at IS NOT NULL;

-- name: SetSiteHealthStatus :execrows
-- Sets a site's health_status from an M5 probe result (cross-tenant probe job,
-- app.agent GUC). Only writes when the value actually changes to avoid churn.
UPDATE sites
SET health_status = $2, updated_at = now()
WHERE id = $1 AND health_status <> $2;
