-- m98: "sign up into a plan" Phase 0 — carry a chosen plan through self-serve
-- signup + email verification so the web app can auto-start checkout right
-- after the account is verified.
--
-- desired_plan is a nullable hint captured on the email-verification token
-- row at registration time (validated against internal/billing's plan
-- ladder in Go — this column itself is intentionally vendor/vocabulary
-- neutral, just text). It rides the same single-use/TTL lifecycle as the
-- token: VerifyEmail reads it off the row it just consumed and surfaces it in
-- the response, at which point it is gone with the token. Storing it here
-- (not on users) keeps it naturally scoped to one registration attempt.
--
-- Mirrors m97's exact idempotent add-column style (information_schema.columns
-- existence check inside a DO $$ block) so this migration is safe to re-run.

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public'
          AND table_name   = 'email_verification_tokens'
          AND column_name  = 'desired_plan'
    ) THEN
        ALTER TABLE "public"."email_verification_tokens"
            ADD COLUMN "desired_plan" text;
    END IF;
END;
$$;
