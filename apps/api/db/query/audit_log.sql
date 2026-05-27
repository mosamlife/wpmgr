-- name: InsertAuditEntry :one
INSERT INTO audit_log (
    tenant_id, actor_type, actor_id, action, target_type, target_id, metadata, prev_hash, hash, created_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetLastAuditHash :one
SELECT hash FROM audit_log
WHERE tenant_id = $1
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: ListAuditEntries :many
SELECT * FROM audit_log
WHERE tenant_id = $1
ORDER BY created_at ASC, id ASC
LIMIT $2 OFFSET $3;

-- name: ListAuditEntriesForVerify :many
SELECT * FROM audit_log
WHERE tenant_id = $1
ORDER BY created_at ASC, id ASC;
