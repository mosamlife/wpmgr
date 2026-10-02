-- m155: ability_catalogue (global allowlist) and site_ability_inventory
-- (per-site registered abilities). See the migration header for the grant
-- model and the tenancy.

-- name: ListAdmittedAbilityCatalogue :many
-- The entries the engine may offer: admitted and enabled. A disabled entry is
-- kill switch 1. Readable in any transaction: the table is global and its one
-- policy is FOR SELECT USING (true).
SELECT * FROM ability_catalogue
WHERE status = 'admitted' AND enabled
ORDER BY name, version_min NULLS FIRST;

-- name: ListAbilityCatalogue :many
-- Every entry, any status, for the admin screen.
SELECT * FROM ability_catalogue
ORDER BY name, version_min NULLS FIRST;

-- name: GetAbilityCatalogueEntry :one
-- One entry by its stable id. pgx.ErrNoRows when it does not exist.
SELECT * FROM ability_catalogue
WHERE entry_id = sqlc.arg(entry_id)::uuid;

-- name: AdminUpsertAbilityCatalogueEntry :one
-- The ONLY write path. Call it only behind requireSuperadmin. entry_id NULL
-- inserts; a non-NULL entry_id updates that entry (SQLSTATE P0002 if absent,
-- 22023 if the name would change). Refuses with 42501 unless actor_user_id
-- names a superadmin, and writes an ability_catalogue_audit row in the same
-- statement. m159: refuses 23P01 (ability_catalogue_range_overlap) when an
-- admitted entry of the same name overlaps this admitted version range.
SELECT * FROM admin_upsert_ability_catalogue_entry(
    sqlc.arg(actor_user_id)::uuid,
    sqlc.narg(entry_id)::uuid,
    sqlc.arg(name)::text,
    sqlc.arg(source)::text,
    sqlc.arg(class)::text,
    sqlc.arg(status)::text,
    sqlc.arg(enabled)::boolean,
    sqlc.arg(approval_mode)::text,
    sqlc.arg(permission_mode)::text,
    sqlc.narg(integration_id)::text,
    sqlc.narg(owner_dir)::text,
    sqlc.narg(version_min)::text,
    sqlc.narg(version_max_tested)::text,
    sqlc.narg(min_wp_version)::text,
    sqlc.narg(min_agent_version)::text,
    sqlc.narg(schema_struct_sha256)::text,
    sqlc.arg(dynamic_enum_paths)::text[],
    sqlc.arg(title)::text,
    sqlc.arg(description)::text,
    sqlc.narg(usage)::text,
    sqlc.narg(operator_permission)::text,
    sqlc.narg(target)::jsonb,
    sqlc.arg(snapshot)::text,
    sqlc.narg(preview)::text,
    sqlc.arg(arg_render)::jsonb,
    sqlc.arg(effect_copy)::text,
    sqlc.arg(limits)::jsonb,
    sqlc.arg(nested_allow)::text[],
    sqlc.arg(global_option_keys)::text[],
    sqlc.narg(integration_block)::jsonb,
    sqlc.arg(admission)::jsonb,
    sqlc.narg(entry_sha256)::text,
    sqlc.narg(output_fields)::jsonb
);

-- name: StampWpmgrAbilityEntryHash :one
-- m157. Stores the Go-computed canonical entry hash on a WPMgr-owned entry
-- whose hash is still NULL, and audits it with a NULL actor. Needs no
-- superadmin: it can only move NULL to a hash, on a source = 'wpmgr' row.
-- Refusals: P0002 no entry, 22023 not 64 lowercase hex, 42501 not a wpmgr
-- row, 55000 already stamped (a concurrent stamper lost the race; re-read).
SELECT * FROM stamp_wpmgr_ability_entry_hash(
    sqlc.arg(entry_id)::uuid,
    sqlc.arg(entry_sha256)::text
);

-- name: RecordAbilityReadSideEffect :one
-- m160 (owner ruling 4). Records that a vendor read was caught writing or
-- calling out on this site and returns the entry's distinct-site count. The
-- report from a NEW site that brings the count to 3 or more disables an
-- enabled entry fleet-wide and audits it with a NULL actor. A repeat report
-- from a counted site changes nothing. Run it in its own READ COMMITTED
-- transaction. Refusals: 22023 a NULL argument, P0002 no entry, 42501 the
-- entry is not a vendor read (nothing recorded).
SELECT record_ability_read_side_effect(
    sqlc.arg(entry_id)::uuid,
    sqlc.arg(site_id)::uuid,
    sqlc.arg(tenant_id)::uuid
)::int AS distinct_sites;

-- name: ListAbilityCatalogueAudit :many
-- Newest first, for the admin screen.
SELECT * FROM ability_catalogue_audit
WHERE entry_id = sqlc.arg(entry_id)::uuid
ORDER BY id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: UpsertSiteAbilityInventory :execrows
-- One refresh's rows for ONE site, in one statement. Run in the site's tenant
-- transaction, then DeleteStaleSiteAbilityInventory with the same checked_at
-- in the same transaction, then UpsertSiteAbilityInventoryRun: together they
-- replace the site's inventory. The arrays are parallel, one element per
-- ability. Nullable text columns take '' for NULL (pgx cannot carry a NULL
-- element in []string); the schema and annotation arrays carry JSON text.
-- owner_oks is text: 'true', 'false' or '' for unknown.
INSERT INTO site_ability_inventory (
    tenant_id, site_id, name, owner_kind, owner_dir, owner_ok, owner_version,
    schema_struct_sha256, input_schema, output_schema, annotations,
    site_label, site_description, checked_at
)
SELECT
    sqlc.arg(tenant_id)::uuid,
    sqlc.arg(site_id)::uuid,
    r.name, r.owner_kind,
    nullif(r.owner_dir, ''),
    nullif(r.owner_ok, '')::boolean,
    nullif(r.owner_version, ''),
    nullif(r.schema_struct_sha256, ''),
    nullif(r.input_schema, '')::jsonb,
    nullif(r.output_schema, '')::jsonb,
    nullif(r.annotations, '')::jsonb,
    nullif(r.site_label, ''),
    nullif(r.site_description, ''),
    sqlc.arg(checked_at)::timestamptz
FROM (
    -- Parallel unnests in one select list zip element-wise.
    SELECT
        unnest(sqlc.arg(names)::text[]) AS name,
        unnest(sqlc.arg(owner_kinds)::text[]) AS owner_kind,
        unnest(sqlc.arg(owner_dirs)::text[]) AS owner_dir,
        unnest(sqlc.arg(owner_oks)::text[]) AS owner_ok,
        unnest(sqlc.arg(owner_versions)::text[]) AS owner_version,
        unnest(sqlc.arg(schema_struct_sha256s)::text[]) AS schema_struct_sha256,
        unnest(sqlc.arg(input_schemas)::text[]) AS input_schema,
        unnest(sqlc.arg(output_schemas)::text[]) AS output_schema,
        unnest(sqlc.arg(annotations)::text[]) AS annotations,
        unnest(sqlc.arg(site_labels)::text[]) AS site_label,
        unnest(sqlc.arg(site_descriptions)::text[]) AS site_description
) AS r
ON CONFLICT (site_id, name) DO UPDATE SET
    owner_kind = EXCLUDED.owner_kind,
    owner_dir = EXCLUDED.owner_dir,
    owner_ok = EXCLUDED.owner_ok,
    owner_version = EXCLUDED.owner_version,
    schema_struct_sha256 = EXCLUDED.schema_struct_sha256,
    input_schema = EXCLUDED.input_schema,
    output_schema = EXCLUDED.output_schema,
    annotations = EXCLUDED.annotations,
    site_label = EXCLUDED.site_label,
    site_description = EXCLUDED.site_description,
    checked_at = EXCLUDED.checked_at;

-- name: DeleteStaleSiteAbilityInventory :execrows
-- The second half of a replace: drops the site's rows the latest refresh did
-- not report. Pass the checked_at the upsert used.
DELETE FROM site_ability_inventory
WHERE tenant_id = sqlc.arg(tenant_id)::uuid
  AND site_id = sqlc.arg(site_id)::uuid
  AND checked_at < sqlc.arg(checked_at)::timestamptz;

-- name: ListSiteAbilityInventory :many
-- One page of a site's inventory, keyset-paged on name ascending. Pass
-- after_name = '' for the first page. namespace NULL lists every namespace.
SELECT * FROM site_ability_inventory
WHERE tenant_id = sqlc.arg(tenant_id)::uuid
  AND site_id = sqlc.arg(site_id)::uuid
  AND name > sqlc.arg(after_name)::text
  AND (sqlc.narg(namespace)::text IS NULL OR namespace = sqlc.narg(namespace)::text)
ORDER BY name
LIMIT sqlc.arg(row_limit)::int;

-- name: GetSiteAbilityInventoryEntry :one
-- One ability on one site. pgx.ErrNoRows when the site did not report it.
SELECT * FROM site_ability_inventory
WHERE tenant_id = sqlc.arg(tenant_id)::uuid
  AND site_id = sqlc.arg(site_id)::uuid
  AND name = sqlc.arg(name)::text;

-- name: UpsertSiteAbilityInventoryRun :exec
-- Records one refresh of one site. Call it in the SAME tenant transaction as
-- UpsertSiteAbilityInventory and DeleteStaleSiteAbilityInventory, with the
-- same checked_at and a fresh snapshot_id, so the record and the rows commit
-- or roll back together.
INSERT INTO site_ability_inventory_runs (
    tenant_id, site_id, checked_at, snapshot_id, api_present, abilities_stored, truncated
)
VALUES (
    sqlc.arg(tenant_id)::uuid, sqlc.arg(site_id)::uuid, sqlc.arg(checked_at)::timestamptz,
    sqlc.arg(snapshot_id)::uuid, sqlc.arg(api_present)::boolean,
    sqlc.arg(abilities_stored)::int, sqlc.arg(truncated)::boolean
)
ON CONFLICT (site_id) DO UPDATE SET
    checked_at = EXCLUDED.checked_at,
    snapshot_id = EXCLUDED.snapshot_id,
    api_present = EXCLUDED.api_present,
    abilities_stored = EXCLUDED.abilities_stored,
    truncated = EXCLUDED.truncated;

-- name: GetSiteAbilityInventoryRun :one
-- The site's last refresh. pgx.ErrNoRows means the site has never been
-- refreshed.
SELECT * FROM site_ability_inventory_runs
WHERE tenant_id = sqlc.arg(tenant_id)::uuid
  AND site_id = sqlc.arg(site_id)::uuid;
