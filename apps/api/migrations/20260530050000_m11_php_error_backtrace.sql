-- S1.1 PHP-error tightening — backtrace column.
--
-- Adds a jsonb backtrace column to agent_php_errors so the CP stores the
-- 10-frame call stack the agent now ships with every error batch row. The
-- column is NOT NULL with a safe default so existing rows are valid without
-- a backfill and future inserts that omit the field still succeed.
--
-- Style note: we use DO $$ ... END $$ idempotency guards because Atlas CE
-- cannot diff column additions; running this migration twice must be a no-op.

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'public'
          AND table_name   = 'agent_php_errors'
          AND column_name  = 'backtrace'
    ) THEN
        ALTER TABLE "public"."agent_php_errors"
            ADD COLUMN "backtrace" jsonb NOT NULL DEFAULT '[]'::jsonb;
    END IF;
END;
$$;
