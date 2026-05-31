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

-- ---------------------------------------------------------------------------
-- M21 — site-bound pairing codes (live enrollment + re-enroll, ADR-041).
-- ---------------------------------------------------------------------------

-- name: CreateSiteBoundPairingCode :one
-- Tenant-scoped (app.tenant_id): mint a code bound to an existing site_id (the
-- site already exists in pending_enrollment). Consuming this code transitions
-- THAT site to connected rather than creating a new row.
INSERT INTO pairing_codes (tenant_id, code_hash, created_by, site_name, tags, expires_at, site_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: PeekPairingCodeSiteID :one
-- Enroll path (app.enroll GUC): resolve whether a presented code is site-bound
-- (returns its site_id) WITHOUT consuming it, so /enroll can route between the
-- site-first consume and the legacy create-at-enroll flow. Does not leak: only
-- the site_id (nullable) is returned, and the caller already holds the code.
SELECT site_id FROM pairing_codes
WHERE code_hash = $1;

-- name: ConsumeSiteBoundPairingCode :one
-- Enroll path (app.enroll GUC): the ATOMIC single-use consume. Marks the code
-- consumed only if it is still unconsumed AND unexpired, recording the source
-- IP. Exactly one concurrent caller wins (the conditional UPDATE is the lock);
-- a loser gets pgx.ErrNoRows. Returns the resolved tenant_id + site_id so the
-- caller can transition the bound site. NULL site_id ⇒ legacy create-at-enroll.
UPDATE pairing_codes
SET consumed_at      = now(),
    consumed_from_ip = $2
WHERE code_hash = $1
  AND consumed_at IS NULL
  AND expires_at > now()
RETURNING id, tenant_id, site_id, site_name, tags;
