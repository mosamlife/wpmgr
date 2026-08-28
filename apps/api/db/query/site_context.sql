-- name: GetLatestSiteContextVersion :one
-- ADR-064 Decision 3: "the current context" is the latest version row, scoped
-- to the CURRENT tenant. After a site transfer only post-transfer rows carry
-- the current tenant_id, so this naturally returns the destination org's
-- active context per Decision 12 with no extra transfer-aware logic.
SELECT * FROM site_context_versions
WHERE tenant_id = $1 AND site_id = $2
ORDER BY version DESC
LIMIT 1;

-- name: GetSiteContextVersionByID :one
-- tenant_id is explicit (defense in depth per house convention) AND is the
-- mechanism that makes a restore-pointer's stamp check work: a version id
-- belonging to a DIFFERENT tenant stamp (a pre-transfer row, ADR-064 Decision
-- 12) returns no rows here even though the row physically exists, because RLS
-- tenant_isolation also filters on tenant_id and this WHERE clause matches
-- it. Callers rely on this: "not found" here means "does not exist OR belongs
-- to a stamp this caller may not read", which is exactly Decision 12's rule.
SELECT * FROM site_context_versions
WHERE tenant_id = $1 AND site_id = $2 AND id = $3;

-- name: GetSiteContextVersionByVersion :one
-- Used to fetch the immediately-prior version for a diff (ADR-064 Decision 5).
-- Same tenant-stamp mechanism as GetSiteContextVersionByID: if the immediately
-- prior version (version-1) is stamped to a different (pre-transfer) tenant,
-- this returns no rows under the current tenant, which the caller must read
-- as "no eligible predecessor, render a baseline" per Decision 5 — not as an
-- error.
SELECT * FROM site_context_versions
WHERE tenant_id = $1 AND site_id = $2 AND version = $3;

-- name: ListSiteContextVersions :many
-- Keyset-paginated newest-first history, scoped to the CURRENT tenant stamp
-- only — this is what makes list/item history "additionally scoped to
-- versions stamped with the site's current organisation" (ADR-064 Decision 13)
-- true with no extra filtering: a pre-transfer row simply never matches
-- tenant_id = $1 once the site belongs to a new org. Cursor is the version
-- number (unique, monotonic, gap-free per site via site_context_versions_
-- version_key); 0 means first page.
SELECT * FROM site_context_versions
WHERE tenant_id = $1 AND site_id = $2
  AND ($3::bigint = 0 OR version < $3::bigint)
ORDER BY version DESC
LIMIT $4;

-- name: CreateSiteContextVersion :one
INSERT INTO site_context_versions (
    tenant_id, site_id, version, restrictions, guidance,
    author_type, author_id, provenance, restored_from_version_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
)
RETURNING *;
