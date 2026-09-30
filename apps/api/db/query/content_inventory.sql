-- m153: content_integrations (global allowlist) and site_content_inventory
-- (per-site probe results). See the migration header for the grant model.

-- name: ListContentIntegrations :many
-- Every allowlist row, for the probe dispatch and the admin screen. Readable
-- in any transaction: the table is global and its one policy is FOR SELECT
-- USING (true).
SELECT * FROM content_integrations
ORDER BY integration_id;

-- name: ListEnabledContentIntegrations :many
-- The rows the probe is sent. A disabled row is the kill switch.
SELECT * FROM content_integrations
WHERE enabled
ORDER BY integration_id;

-- name: AdminUpsertContentIntegration :one
-- The ONLY write path. Call it only behind requireSuperadmin. The function
-- refuses (SQLSTATE 42501) unless actor_user_id names a superadmin, and it
-- writes a content_integrations_audit row in the same statement.
SELECT (admin_upsert_content_integration(
    sqlc.arg(actor_user_id)::uuid,
    sqlc.arg(integration_id)::text,
    sqlc.arg(display_name)::text,
    sqlc.arg(enabled)::boolean,
    sqlc.arg(status)::text,
    sqlc.arg(descriptor)::jsonb,
    sqlc.narg(abilities)::jsonb,
    sqlc.narg(min_version)::text,
    sqlc.narg(max_tested_version)::text,
    sqlc.narg(min_wp_version)::text,
    sqlc.narg(integration_entry_sha256)::text
)).*;

-- name: ListContentIntegrationAudit :many
-- Newest first, for the admin screen.
SELECT * FROM content_integrations_audit
WHERE integration_id = sqlc.arg(integration_id)::text
ORDER BY id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: UpsertSiteContentInventory :execrows
-- One refresh's rows for ONE site, in one statement. Run in the site's tenant
-- transaction, then DeleteStaleSiteContentInventory with the same checked_at
-- in the same transaction: together they replace the site's inventory.
-- The arrays are parallel, one element per post. Nullable text columns take
-- '' for NULL (pgx cannot carry a NULL element in []string).
INSERT INTO site_content_inventory (
    tenant_id, site_id, post_id, post_type, post_status, verdict,
    route_number, route_reason, owner_integration_id, owner_display_name,
    owner_version, fingerprint, title, checked_at
)
SELECT
    sqlc.arg(tenant_id)::uuid,
    sqlc.arg(site_id)::uuid,
    r.post_id, r.post_type, r.post_status, r.verdict,
    r.route_number, r.route_reason,
    nullif(r.owner_integration_id, ''),
    nullif(r.owner_display_name, ''),
    nullif(r.owner_version, ''),
    nullif(r.fingerprint, ''),
    nullif(r.title, ''),
    sqlc.arg(checked_at)::timestamptz
FROM (
    -- Parallel unnests in one select list zip element-wise.
    SELECT
        unnest(sqlc.arg(post_ids)::bigint[]) AS post_id,
        unnest(sqlc.arg(post_types)::text[]) AS post_type,
        unnest(sqlc.arg(post_statuses)::text[]) AS post_status,
        unnest(sqlc.arg(verdicts)::text[]) AS verdict,
        unnest(sqlc.arg(route_numbers)::smallint[]) AS route_number,
        unnest(sqlc.arg(route_reasons)::text[]) AS route_reason,
        unnest(sqlc.arg(owner_integration_ids)::text[]) AS owner_integration_id,
        unnest(sqlc.arg(owner_display_names)::text[]) AS owner_display_name,
        unnest(sqlc.arg(owner_versions)::text[]) AS owner_version,
        unnest(sqlc.arg(fingerprints)::text[]) AS fingerprint,
        unnest(sqlc.arg(titles)::text[]) AS title
) AS r
ON CONFLICT (site_id, post_id) DO UPDATE SET
    post_type = EXCLUDED.post_type,
    post_status = EXCLUDED.post_status,
    verdict = EXCLUDED.verdict,
    route_number = EXCLUDED.route_number,
    route_reason = EXCLUDED.route_reason,
    owner_integration_id = EXCLUDED.owner_integration_id,
    owner_display_name = EXCLUDED.owner_display_name,
    owner_version = EXCLUDED.owner_version,
    fingerprint = EXCLUDED.fingerprint,
    title = EXCLUDED.title,
    checked_at = EXCLUDED.checked_at;

-- name: DeleteStaleSiteContentInventory :execrows
-- The second half of a replace: drops the site's rows the latest refresh did
-- not report. Pass the checked_at the upsert used.
DELETE FROM site_content_inventory
WHERE tenant_id = sqlc.arg(tenant_id)::uuid
  AND site_id = sqlc.arg(site_id)::uuid
  AND checked_at < sqlc.arg(checked_at)::timestamptz;

-- name: ListSiteContentInventory :many
-- One page of a site's inventory, keyset-paged on post_id ascending. Pass
-- after_post_id = 0 for the first page. owner_integration_id filters by
-- editor: NULL for every row, 'classic' for rows no builder owns, or an
-- integration id.
SELECT * FROM site_content_inventory
WHERE tenant_id = sqlc.arg(tenant_id)::uuid
  AND site_id = sqlc.arg(site_id)::uuid
  AND post_id > sqlc.arg(after_post_id)::bigint
  AND (
    sqlc.narg(owner_integration_id)::text IS NULL
    OR (sqlc.narg(owner_integration_id)::text = 'classic'
        AND owner_integration_id IS NULL)
    OR owner_integration_id = sqlc.narg(owner_integration_id)::text
  )
ORDER BY post_id
LIMIT sqlc.arg(row_limit)::int;

-- name: FleetContentShareByVerdict :many
-- Owner fleet report: pages and sites per verdict and route, across every
-- tenant. Run under pool.InAgentTx (site_content_inventory_agent, FOR SELECT).
SELECT verdict, route_number,
       count(*)::bigint AS pages,
       count(DISTINCT site_id)::bigint AS sites
FROM site_content_inventory
GROUP BY verdict, route_number
ORDER BY verdict, route_number;

-- name: FleetContentShareByBuilder :many
-- Owner fleet report: builder pages per builder and version, across every
-- tenant. Run under pool.InAgentTx.
SELECT owner_integration_id::text AS owner_integration_id,
       owner_version,
       count(*)::bigint AS pages,
       count(DISTINCT site_id)::bigint AS sites
FROM site_content_inventory
WHERE owner_integration_id IS NOT NULL
GROUP BY owner_integration_id, owner_version
ORDER BY owner_integration_id, owner_version NULLS FIRST;
