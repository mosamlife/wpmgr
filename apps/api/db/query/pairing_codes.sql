-- name: CreatePairingCode :one
-- Tenant-scoped (app.tenant_id) — operator generates a code for the tenant.
INSERT INTO pairing_codes (tenant_id, code_hash, created_by, site_name, tags, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetPairingCodeByHash :one
-- Enroll path (app.enroll GUC): resolve a presented code by its hash before the
-- tenant is known.
SELECT * FROM pairing_codes
WHERE code_hash = $1;

-- name: ConsumePairingCode :execrows
-- Enroll path (app.enroll GUC): mark consumed only if still unconsumed.
UPDATE pairing_codes
SET consumed_at = now()
WHERE id = $1 AND consumed_at IS NULL;

-- name: IncrementPairingCodeAttempts :execrows
-- Enroll path (app.enroll GUC): record a failed validation attempt.
UPDATE pairing_codes
SET attempts = attempts + 1
WHERE id = $1;
