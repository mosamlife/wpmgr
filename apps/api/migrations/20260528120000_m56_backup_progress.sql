-- M5.6 / ADR-033 backup progress fields.
--
-- The CP's backup-progress watchdog (apps/api/internal/backup) and the
-- /sites/:siteId/backups list handler both reference these columns. They were
-- added directly to the dev DB during M5.6 implementation but no Atlas
-- migration was emitted, so Cloud SQL boots without them and every progress
-- watchdog tick + every backup list query 500s with
--   ERROR: column "progress_updated_at" does not exist (SQLSTATE 42703)
--
-- Idempotent: IF NOT EXISTS guards let this re-run safely against a dev DB
-- where the columns were added by hand.

ALTER TABLE "public"."backup_snapshots"
  ADD COLUMN IF NOT EXISTS "progress" jsonb NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE "public"."backup_snapshots"
  ADD COLUMN IF NOT EXISTS "progress_updated_at" timestamptz NULL;

-- Partial index — only running snapshots are watchdog candidates. Keeps the
-- index small and the watchdog scan O(running_count).
CREATE INDEX IF NOT EXISTS "backup_snapshots_running_progress_idx"
  ON "public"."backup_snapshots" ("progress_updated_at")
  WHERE status = 'running';
