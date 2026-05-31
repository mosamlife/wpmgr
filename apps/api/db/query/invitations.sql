-- name: CreateInvitation :one
INSERT INTO invitations (
    tenant_id, email, scope, site_id, role,
    token_hash, invited_by, expires_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetInvitationByTokenHash :one
-- Pre-auth lookup: must be run under InInviteLookupTx (app.invite_lookup='on').
SELECT * FROM invitations
WHERE token_hash = $1;

-- name: MarkInvitationAccepted :one
UPDATE invitations
SET accepted_at      = now(),
    accepted_user_id = $2
WHERE id = $1
  AND accepted_at IS NULL
RETURNING *;

-- name: IncrementInviteAttempts :one
UPDATE invitations
SET attempts = attempts + 1
WHERE id = $1
RETURNING *;

-- name: ListPendingInvitations :many
-- List pending (not yet accepted, not expired) invitations for the current tenant.
SELECT * FROM invitations
WHERE tenant_id  = $1
  AND accepted_at IS NULL
  AND expires_at > now()
ORDER BY created_at DESC;
