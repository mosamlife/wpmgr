-- name: GetLatestOrgContextVersion :one
-- ADR-064 Decision 3: "the current context" is the latest version row.
-- tenant_id is the leading column of org_context_versions_version_key, so this
-- read uses that index as a backward scan — no separate ordering index needed.
SELECT * FROM org_context_versions
WHERE tenant_id = $1
ORDER BY version DESC
LIMIT 1;

-- name: GetOrgContextVersionByID :one
SELECT * FROM org_context_versions
WHERE tenant_id = $1 AND id = $2;

-- name: GetOrgContextVersionByVersion :one
-- Used to fetch the immediately-prior version for a diff (ADR-064 Decision 5).
-- Org context has no organisation-transfer analogue (the tenant_id subject
-- never changes), so this always resolves when version > 1.
SELECT * FROM org_context_versions
WHERE tenant_id = $1 AND version = $2;

-- name: ListOrgContextVersions :many
-- Keyset-paginated newest-first history (ADR-064 Decision 5). Cursor is the
-- version number itself, not created_at/id: version is unique, monotonic and
-- gap-free per tenant (org_context_versions_version_key), which is strictly
-- stronger than a (created_at, id) tiebreak and is the index m122's header
-- says this read path is meant to use. cursor = 0 means "first page" (version
-- is CHECKed >= 1, so 0 never collides with a real row).
SELECT * FROM org_context_versions
WHERE tenant_id = $1
  AND ($2::bigint = 0 OR version < $2::bigint)
ORDER BY version DESC
LIMIT $3;

-- name: CreateOrgContextVersion :one
INSERT INTO org_context_versions (
    tenant_id, version, restrictions, guidance,
    author_type, author_id, provenance, restored_from_version_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8
)
RETURNING *;
