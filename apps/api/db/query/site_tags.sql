-- GH #230 "rich tags" — tenant-level tag registry (m100). site_tags owns
-- existence/color/canonical name; sites.tags (text[]) remains the assignment
-- store. See internal/sitetag for the orchestration that keeps them in sync.

-- name: ListTagsWithUsage :many
-- No pagination — a tenant's tag registry is small. usage_count is the
-- number of sites (in this tenant) currently carrying the tag, computed live
-- rather than stored so it can never drift from sites.tags.
SELECT
    t.id,
    t.name,
    t.color,
    t.created_at,
    count(s.id) FILTER (WHERE s.id IS NOT NULL) AS usage_count
FROM site_tags t
LEFT JOIN sites s ON s.tenant_id = t.tenant_id AND t.name = ANY (s.tags)
WHERE t.tenant_id = $1
GROUP BY t.id
ORDER BY lower(t.name), t.name;

-- name: UpsertTagNames :exec
-- Registers a batch of tag names into the tenant's registry, ignoring names
-- already present (color untouched — the registry owns color, not the
-- assignment path). Called from every write path that lands tag names onto
-- sites.tags: site.Service.SetTags, pairing-code minting, and bulk-apply's
-- `add` list — always in the SAME transaction as that write (binding
-- invariant, m100).
INSERT INTO site_tags (tenant_id, name)
SELECT @tenant_id::uuid, unnest(@names::text[])
ON CONFLICT (tenant_id, name) DO NOTHING;

-- name: CreateTag :one
-- unique_violation (23505) on site_tags_tenant_name_key maps to 409
-- tag_name_exists in the repo (exact-case unique; case-insensitive near-dupes
-- like "Prod"/"prod" are intentionally NOT blocked here — the client steers
-- around them, and rename+merge is the remedy after the fact).
INSERT INTO site_tags (tenant_id, name, color)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetTag :one
SELECT * FROM site_tags
WHERE id = $1 AND tenant_id = $2;

-- name: GetTagByName :one
-- Used by the rename-merge path to resolve the survivor tag when a rename
-- collides with an existing name.
SELECT * FROM site_tags
WHERE tenant_id = $1 AND name = $2;

-- name: RenameTagRow :one
-- May fail with unique_violation (23505) when @name collides with another
-- tag in the same tenant — the repo maps that to either 409 tag_name_exists
-- or drives the merge path, per the caller's `merge` flag.
UPDATE site_tags
SET name = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: RecolorTag :one
UPDATE site_tags
SET color = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: DeleteTagRow :execrows
DELETE FROM site_tags
WHERE id = $1 AND tenant_id = $2;

-- name: CountSitesWithTag :one
-- Recomputes usage_count after a mutation (create/rename/merge/recolor) so
-- the returned SiteTag never reports a stale count.
SELECT count(*) FROM sites
WHERE tenant_id = @tenant_id::uuid AND @name::text = ANY (tags);

-- name: RewriteSiteTagName :exec
-- Propagates a tag rename (or a merge's rewrite onto the survivor's name)
-- across every site in the tenant currently carrying @old_name. The
-- array_agg(DISTINCT ...) also dedups the array in the same statement, which
-- is exactly what a merge needs when a site already carries both the source
-- and the survivor name.
UPDATE sites
SET tags = (
        SELECT coalesce(array_agg(DISTINCT CASE WHEN x = @old_name::text THEN @new_name::text ELSE x END), '{}')
        FROM unnest(tags) AS x
    ),
    updated_at = now()
WHERE tenant_id = @tenant_id::uuid
  AND @old_name::text = ANY (tags);

-- name: RemoveSiteTagName :exec
UPDATE sites
SET tags = array_remove(tags, @name::text),
    updated_at = now()
WHERE tenant_id = @tenant_id::uuid
  AND @name::text = ANY (tags);

-- name: ApplyTagDeltaToSite :execrows
-- Bulk-apply's per-site write: computes dedup(tags ∪ @add) − @remove
-- entirely in SQL from the CURRENT row (never from a stale client-side read).
-- 0 affected rows means the site does not exist in this tenant (the handler
-- has already excluded sites the caller cannot access via CanAccessSite
-- before this runs).
UPDATE sites
SET tags = (
        SELECT coalesce(array_agg(DISTINCT x), '{}')
        FROM unnest(array_cat(tags, @add::text[])) AS x
        WHERE NOT (x = ANY (@remove::text[]))
    ),
    updated_at = now()
WHERE tenant_id = @tenant_id::uuid AND id = @site_id::uuid;
