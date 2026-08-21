-- CreateAPIKey is the ONLY INSERT path for api_keys, deliberately. m120 (#510)
-- extended it in place rather than adding a second capability-aware INSERT
-- alongside it: two INSERT statements over a table whose CHECK constraints
-- encode an authorization contract is one statement that can forget a column,
-- and the column it forgets defaults to the permissive legacy value.
--
-- Callers pass auth_model='role' + capabilities=NULL for a legacy role key, or
-- auth_model='capability' + a non-NULL (possibly empty) set for a least-
-- privilege key. api_keys_auth_model_capabilities_check refuses every other
-- combination, so an inconsistent pair fails 23514 here rather than
-- authenticating with surprise authority later.
-- name: CreateAPIKey :one
INSERT INTO api_keys (
    tenant_id, name, prefix, key_hash, role,
    kind, auth_model, capabilities, site_scope, allowed_site_ids
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
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
--
-- GH #152: joins tenants and excludes t.deleted_at IS NOT NULL rows, so a
-- bearer-key request bound to a soft-deleted org's tenant fails exactly like
-- an unknown prefix (ErrNoRows -> apikey.Service.Authenticate's existing
-- domain.Unauthorized("apikey_invalid", ...) path) rather than continuing to
-- authenticate into an org every session/UI path has already hidden.
-- name: GetAPIKeyByPrefix :one
SELECT api_keys.* FROM api_keys
JOIN tenants t ON t.id = api_keys.tenant_id
WHERE api_keys.prefix = $1 AND t.deleted_at IS NULL;

-- name: TouchAPIKey :exec
UPDATE api_keys SET last_used_at = now() WHERE id = $1 AND tenant_id = $2;
