-- m105: GH #286, per-site default "Login As User". Adds the column the mint
-- path reads to inject a default target_wp_user_login when the operator's
-- request omits one; the audit trail (autologin.requested / .consumed) then
-- carries the real username instead of an opaque "first administrator" gap.
--
-- No RLS change: the column lands on the existing tenant-RLS'd
-- autologin_policies table (mirrors m101/m103/m104's column-only cadence).
--
-- Idempotent: ADD COLUMN IF NOT EXISTS mirrors m104/m103/m101/m92/m93.

ALTER TABLE "public"."autologin_policies"
    ADD COLUMN IF NOT EXISTS "default_wp_user_login" text NOT NULL DEFAULT '';
