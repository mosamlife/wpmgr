-- name: CreateSite :one
-- tenant_id is supplied explicitly for defense-in-depth; RLS additionally
-- enforces that it matches the current app.tenant_id setting.
INSERT INTO sites (tenant_id, url, name, status, wp_version, php_version)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetSite :one
-- GH #243: page_cache_enabled / object_cache_enabled surface the REAL
-- drop-in config state (site_perf_config.cache_enabled / site_object_cache_
-- config.enabled) via a PK-keyed LEFT JOIN (both tables are one-row-per-site,
-- site_id PRIMARY KEY) instead of the old plugin-slug inference that could
-- never match (both features ship as drop-ins, not plugins). Both joined
-- tables carry their own tenant_isolation RLS, so this is RLS-safe; the
-- explicit tenant_id match in the ON clause is defense-in-depth + keeps the
-- planner on the PK index (project convention — see clients.sql).
SELECT s.*,
       COALESCE(pc.cache_enabled, false) AS page_cache_enabled,
       COALESCE(oc.enabled, false) AS object_cache_enabled
FROM sites s
LEFT JOIN site_perf_config pc
    ON pc.site_id = s.id AND pc.tenant_id = s.tenant_id
LEFT JOIN site_object_cache_config oc
    ON oc.site_id = s.id AND oc.tenant_id = s.tenant_id
WHERE s.id = $1 AND s.tenant_id = $2;

-- name: ListSites :many
-- Defaults to hiding archived sites (ADR-041). When sqlc.narg('state') is set
-- the list is filtered to exactly that connection_state (e.g. 'archived' for
-- the archived chip); when it is NULL every non-archived site is returned.
-- When sqlc.narg('client_id') is set only sites belonging to that client are returned (m63).
-- M100 (GH #230 "rich tags"): any_tags (tags && ...) and all_tags (tags @> ...)
-- replace the single-tag filter; both are served by the sites_tags_idx GIN
-- index. The service maps exactly ONE of them per request (legacy ?tag=
-- becomes any_tags=[tag]).
-- GH #243: page_cache_enabled / object_cache_enabled — see GetSite's comment
-- above. Both LEFT JOINs are PK lookups (site_id), so this stays O(sites) per
-- page (an index-only nested-loop per row) and does not regress the
-- optimized /sites path.
--
-- GH #349 free-text search + ordering. Both moved SERVER side on purpose: the
-- web used to filter an already-fetched page client side, which silently
-- searched only the first page (50 newest sites by default) and told an agency
-- with more sites than that "no results" for a site it owns. A filter applied
-- after the server truncated the list is wrong at any page size.
--
--   sqlc.narg('q') substring-matches, case-insensitively, the site NAME, the
--   site URL, or ANY of the site's tags. The tag arm reads s.tags, the very
--   array this query returns, so any tag the operator can see on a site is a
--   tag they can find that site by. strpos(lower(..), lower(..)) is used
--   rather than ILIKE because the operator typed a search string, not a
--   pattern: with ILIKE a literal % or _ in the query would silently become a
--   wildcard. NULL (absent) disables the whole predicate.
--
--   sqlc.arg('sort') selects the order. It is BOUND AS A PARAMETER and
--   compared against fixed literals; no SQL text is ever concatenated, and the
--   set of accepted values is closed in Go (site.ParseListSort, which 422s an
--   unrecognised value rather than silently falling back). Every branch that
--   is not the selected sort evaluates to NULL for every row, which makes that
--   ORDER BY term a constant and therefore a no-op, so one query serves all
--   six orders.
--     - lower(s.name) sorts names case-insensitively, so "acme" and "Acme"
--       sit together regardless of the server's collation.
--     - NULLS LAST on BOTH last_seen directions: last_seen_at is NULL for a
--       site that has never reported in. Never-seen sites therefore land at
--       the END of the list in either direction, never crowding the top of a
--       descending "most recently seen" sort, and never vanishing.
--     - s.id DESC is the TOTAL-ORDER tiebreak. Two sites can share a name and
--       two can share a created_at; without a final unique key, LIMIT/OFFSET
--       paging is free to drop one row and repeat another across pages.
SELECT s.*,
       COALESCE(pc.cache_enabled, false) AS page_cache_enabled,
       COALESCE(oc.enabled, false) AS object_cache_enabled
FROM sites s
LEFT JOIN site_perf_config pc
    ON pc.site_id = s.id AND pc.tenant_id = s.tenant_id
LEFT JOIN site_object_cache_config oc
    ON oc.site_id = s.id AND oc.tenant_id = s.tenant_id
WHERE s.tenant_id = $1
  AND (sqlc.narg('any_tags')::text[] IS NULL OR s.tags && sqlc.narg('any_tags')::text[])
  AND (sqlc.narg('all_tags')::text[] IS NULL OR s.tags @> sqlc.narg('all_tags')::text[])
  AND (
        (sqlc.narg('state')::text IS NULL AND s.connection_state <> 'archived')
        OR sqlc.narg('state')::text = s.connection_state
      )
  AND (sqlc.narg('client_id')::uuid IS NULL OR s.client_id = sqlc.narg('client_id')::uuid)
  AND (
        sqlc.narg('q')::text IS NULL
        OR strpos(lower(s.name), lower(sqlc.narg('q')::text)) > 0
        OR strpos(lower(s.url), lower(sqlc.narg('q')::text)) > 0
        OR EXISTS (
             SELECT 1 FROM unnest(s.tags) AS tg(tag)
             WHERE strpos(lower(tg.tag), lower(sqlc.narg('q')::text)) > 0
           )
      )
ORDER BY
    CASE WHEN sqlc.arg('sort')::text = 'name'        THEN lower(s.name)  END ASC,
    CASE WHEN sqlc.arg('sort')::text = '-name'       THEN lower(s.name)  END DESC,
    CASE WHEN sqlc.arg('sort')::text = 'created_at'  THEN s.created_at   END ASC,
    CASE WHEN sqlc.arg('sort')::text = '-created_at' THEN s.created_at   END DESC,
    CASE WHEN sqlc.arg('sort')::text = 'last_seen'   THEN s.last_seen_at END ASC NULLS LAST,
    CASE WHEN sqlc.arg('sort')::text = '-last_seen'  THEN s.last_seen_at END DESC NULLS LAST,
    s.id DESC
LIMIT $2 OFFSET $3;

-- name: ListSitesAgentVersions :many
-- Tenant-scoped site_id/name/agent_version rollup for the read-only agent
-- fleet-version dashboard (internal/agentrelease): "how many of my sites are
-- running an outdated agent?". Excludes archived sites, matching the
-- ListSites/ListAllSiteIDs default (ADR-041). Classification against the
-- currently published version happens in Go (internal/agentrelease.Classify);
-- this query only returns the raw per-site facts.
--
-- plugin_identities is a narrow {slug,name} projection of the site's plugin
-- inventory, carrying just enough for Go to recognize which build of the agent
-- the site runs (internal/agentplugin.DistributionOf): the plugin-directory
-- build cannot self-update, so its sites are classified ineligible instead of
-- being reported outdated forever against a channel they cannot consume.
-- The projection is deliberate: shipping the whole components document would
-- move megabytes per dashboard load on a large fleet, and matching the agent's
-- identity in SQL would duplicate literals that must live in exactly one place.
-- The CASE guards a components document whose "plugins" key is absent or is not
-- an array, which jsonb_array_elements would otherwise error on.
SELECT
    s.id,
    s.name,
    s.agent_version,
    COALESCE((
        SELECT jsonb_agg(jsonb_build_object('slug', p ->> 'slug', 'name', p ->> 'name'))
        FROM jsonb_array_elements(
            CASE WHEN jsonb_typeof(s.components -> 'plugins') = 'array'
                 THEN s.components -> 'plugins'
                 ELSE '[]'::jsonb
            END
        ) AS p
    ), '[]'::jsonb)::jsonb AS plugin_identities
FROM sites s
WHERE s.tenant_id = $1
  AND s.connection_state <> 'archived'
ORDER BY s.name;

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
-- last_seen_at and app_probe_path (m107, GH #291 Phase 2) are carried for the
-- application-health prober: B0 (agent ground truth) reads last_seen_at, B3
-- (per-site override) reads app_probe_path. app_alerts_disabled (m108, GH
-- #291 Phase 3) is carried so the app-alert transition step can skip a site
-- whose operator disabled app ALERTING (the probe itself still runs).
-- connection_state (GH #291 Phase 3 Fix 2) is carried for the SAME app-alert
-- transition step, so it can exclude a revoked/archived site using the
-- IDENTICAL predicate GetTenantAppAlertRatio's WHERE clause enforces
-- server-side (see uptime.appAlertEligible) - the fire path and the fleet
-- circuit breaker's ratio must never disagree about which sites count. The
-- reachability probe and the cron-kicker ignore all four.
SELECT id, tenant_id, url, health_status, last_seen_at, app_probe_path, app_alerts_disabled, connection_state FROM sites
WHERE enrolled_at IS NOT NULL;

-- name: GetSiteAppHealthSettings :one
-- Tenant-scoped read of a site's app-health settings (GH #291 Phase 3):
-- the B3 override path and the per-site alerting opt-out.
SELECT app_probe_path, app_alerts_disabled FROM sites
WHERE id = @id AND tenant_id = @tenant_id;

-- name: UpdateSiteAppHealthSettings :one
-- Tenant-scoped write of a site's app-health settings (GH #291 Phase 3).
-- app_probe_path is sqlc.narg so an empty override clears back to
-- auto-detect (NULL); the service layer is responsible for path validation
-- (site-relative, no scheme/host/traversal - see uptime.ValidateAppProbePath).
UPDATE sites
SET app_probe_path = sqlc.narg('app_probe_path'),
    app_alerts_disabled = @app_alerts_disabled,
    updated_at = now()
WHERE id = @id AND tenant_id = @tenant_id
RETURNING app_probe_path, app_alerts_disabled;

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
