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
SELECT * FROM sites
WHERE tenant_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: DeleteSite :execrows
DELETE FROM sites
WHERE id = $1 AND tenant_id = $2;
