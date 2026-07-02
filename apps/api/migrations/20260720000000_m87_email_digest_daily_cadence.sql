-- m87 — allow 'daily' as an email_notify_settings.digest_cadence value.
--
-- The Notifications settings UI only ever offered a "Daily digest" toggle
-- with a single "Send at (UTC)" time — it never collected a cadence or a
-- digest_day. The backend CHECK constraint (and validateNotifySettings)
-- only accepted weekly/monthly, so every save was rejected with
-- "digest_cadence must be 'weekly' or 'monthly'" (issue #123). This widens
-- the constraint to match the UI's actual shape; 'daily' does not require
-- digest_day (enforced in application validation, not a DB constraint).

ALTER TABLE "public"."email_notify_settings" DROP CONSTRAINT IF EXISTS "email_notify_settings_digest_cadence";
ALTER TABLE "public"."email_notify_settings"
    ADD CONSTRAINT "email_notify_settings_digest_cadence" CHECK (digest_cadence IN ('daily', 'weekly', 'monthly'));
