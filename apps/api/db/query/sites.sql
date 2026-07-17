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
-- When sqlc.narg('client_id') is set only sites belonging to that client are returned (m63).
-- M100 (GH #230 "rich tags"): any_tags (tags && ...) and all_tags (tags @> ...)
-- replace the single-tag filter; both are served by the sites_tags_idx GIN
-- index. The service maps exactly ONE of them per request (legacy ?tag=
-- becomes any_tags=[tag]).
SELECT * FROM sites
WHERE tenant_id = $1
  AND (sqlc.narg('any_tags')::text[] IS NULL OR tags && sqlc.narg('any_tags')::text[])
  AND (sqlc.narg('all_tags')::text[] IS NULL OR tags @> sqlc.narg('all_tags')::text[])
  AND (
        (sqlc.narg('state')::text IS NULL AND connection_state <> 'archived')
        OR sqlc.narg('state')::text = connection_state
      )
  AND (sqlc.narg('client_id')::uuid IS NULL OR client_id = sqlc.narg('client_id')::uuid)
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: ListClientNamesForSites :many
-- Returns the client id + name for sites that have a client_id set (m63).
-- Used to enrich the sites-list DTO with client_name in a single batched join.
SELECT s.id AS site_id, c.id AS client_id, c.name AS client_name
FROM sites s
JOIN clients c ON c.id = s.client_id AND c.tenant_id = s.tenant_id
WHERE s.tenant_id = $1
  AND s.id = ANY($2::uuid[])
  AND s.client_id IS NOT NULL;

-- name: ListLatestBackupsForSites :many
-- The most-recent backup snapshot per site, for the sites-table "Backup" column.
-- DISTINCT ON + ORDER BY (site_id, created_at DESC) is served by
-- backup_snapshots_tenant_site_idx (tenant_id, site_id, created_at DESC) — one
-- index-only seek per site, fetched in a single batched call for the listed ids.
SELECT DISTINCT ON (site_id)
       site_id, status, finished_at, created_at
FROM backup_snapshots
WHERE tenant_id = $1 AND site_id = ANY($2::uuid[])
ORDER BY site_id, created_at DESC;

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
    agent_version = $8,
    components   = $9,
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

-- name: ListAllSiteIDs :many
-- Returns all non-archived site IDs for a tenant. Lightweight alternative to
-- ListSites used by fleet adapters that need the full ID set without a 500-row
-- cap. Excludes archived sites (connection_state = 'archived') to match the
-- default ListSites behaviour.
SELECT id FROM sites
WHERE tenant_id = @tenant_id
  AND connection_state <> 'archived'
ORDER BY created_at DESC;

-- name: ListConnectedSiteIDsForScreenshot :many
-- Cross-tenant enumeration of connected sites for the weekly screenshot fanout.
-- Returns only sites in the 'connected' state (not degraded/pending/archived).
-- Runs under the app.agent GUC (sites_agent policy) since it spans tenants.
SELECT id, tenant_id, url FROM sites
WHERE connection_state = 'connected'
  AND enrolled_at IS NOT NULL
ORDER BY created_at DESC, id DESC;
