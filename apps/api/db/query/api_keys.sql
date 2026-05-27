-- name: CreateAPIKey :one
INSERT INTO api_keys (tenant_id, name, prefix, key_hash, role)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetAPIKey :one
SELECT * FROM api_keys
WHERE id = $1 AND tenant_id = $2;

-- name: ListAPIKeys :many
SELECT * FROM api_keys
WHERE tenant_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: RevokeAPIKey :execrows
UPDATE api_keys
SET revoked_at = now()
WHERE id = $1 AND tenant_id = $2 AND revoked_at IS NULL;

-- GetAPIKeyByPrefix resolves a presented key by its unique prefix. This runs
-- WITHOUT a tenant GUC (the auth layer does not yet know the tenant), so it must
-- be executed via InAdminTx which sets app.tenant_id to the row's own tenant is
-- impossible chicken/egg — instead this query is run with RLS disabled scope by
-- using the prefix-unique lookup helper that sets the GUC after. See repo.
-- name: GetAPIKeyByPrefix :one
SELECT * FROM api_keys
WHERE prefix = $1;

-- name: TouchAPIKey :exec
UPDATE api_keys SET last_used_at = now() WHERE id = $1 AND tenant_id = $2;
