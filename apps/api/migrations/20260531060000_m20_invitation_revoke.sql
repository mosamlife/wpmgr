-- M20 — Invitation soft-revoke + revoker trail (link-history UI).
--
-- Adds two columns to invitations so a pending invite can be:
--   1. Soft-revoked (revoked_at) instead of hard-deleted, so the "Pending
--      invites" tab can show revoked/expired rows as genuine history rather
--      than just outstanding invites.
--   2. Attributed (revoked_by) for the who-revoked-it trail.
--
-- An invitation's lifecycle status is DERIVED, never stored:
--   revoked_at IS NOT NULL  -> revoked
--   accepted_at IS NOT NULL -> accepted
--   expires_at < now()      -> expired
--   else                    -> pending
--
-- Idempotent (IF NOT EXISTS). Runs in ONE transaction (no CONCURRENTLY).

ALTER TABLE invitations
    ADD COLUMN IF NOT EXISTS revoked_at timestamptz;

ALTER TABLE invitations
    ADD COLUMN IF NOT EXISTS revoked_by uuid REFERENCES users (id) ON DELETE SET NULL;

-- Index the common "history for this site" read path (scope='site' + site_id),
-- newest first. Partial on scope to keep it tight.
CREATE INDEX IF NOT EXISTS invitations_site_id_idx
    ON invitations (site_id, created_at DESC)
    WHERE scope = 'site';
