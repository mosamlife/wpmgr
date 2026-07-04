-- name: GetAuditIntegrityBaseline :one
-- Returns the tenant's current re-baseline anchor, if one has ever been set.
-- Callers must handle pgx.ErrNoRows as "no baseline" (Verify walks the full
-- chain from genesis in that case).
SELECT * FROM audit_integrity_baseline
WHERE tenant_id = $1;

-- name: UpsertAuditIntegrityBaseline :one
-- Moves the tenant's integrity anchor to the given chain-head snapshot. At
-- most one row per tenant (PRIMARY KEY tenant_id) — a second re-baseline
-- replaces the previous anchor rather than stacking history; the audit_log
-- entry recorded alongside each call is the durable history of when/why.
INSERT INTO audit_integrity_baseline (
    tenant_id, baseline_created_at, baseline_id, baseline_hash, set_by, set_at
)
VALUES (@tenant_id, @baseline_created_at, @baseline_id, @baseline_hash, @set_by, @set_at)
ON CONFLICT (tenant_id) DO UPDATE SET
    baseline_created_at = EXCLUDED.baseline_created_at,
    baseline_id          = EXCLUDED.baseline_id,
    baseline_hash        = EXCLUDED.baseline_hash,
    set_by               = EXCLUDED.set_by,
    set_at                = EXCLUDED.set_at
RETURNING *;
