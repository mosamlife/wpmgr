-- m106: stop discarding the sha256 the wp.org plugin-checksums API returns
-- alongside md5 for each file. The CP previously decoded only the md5 field
-- (checksums.go pluginChecksumAPIResponse) and threw the sha256 away. This
-- migration only adds storage for the value; no comparison logic changes in
-- this release (see the ingest-site comment in internal/scan/checksums.go for
-- the program-wide rule this sets up: md5 stays a negative filter only, a
-- stronger hash is required before any positive/auto-clear trust decision).
--
-- Nullable: existing rows were fetched before this change and have no sha256;
-- they backfill on the next positive-cache refresh (30-day TTL) rather than
-- needing a backfill job.
--
-- No RLS change: wporg_plugin_checksums carries no RLS (public wp.org
-- reference data, not tenant-scoped) per its m77 header; this column-only
-- change does not alter that.
--
-- Idempotent: ADD COLUMN IF NOT EXISTS mirrors m105/m104/m103/m101/m92/m93.
--
-- SET LOCAL lock_timeout scopes the guard to this migration's own
-- transaction (each migration file runs in its own tx; see
-- internal/db/migrate.go), so the brief ACCESS EXCLUSIVE lock this ALTER
-- TABLE needs cannot queue indefinitely behind a long-running reader on
-- wporg_plugin_checksums and, transitively, block every other writer that
-- queues behind it. A nullable column-only add with no default takes the
-- lock only briefly in the common case; a few seconds is enough headroom
-- while still failing fast instead of stalling the boot-time migration run.

SET LOCAL lock_timeout = '5s';

ALTER TABLE "public"."wporg_plugin_checksums"
    ADD COLUMN IF NOT EXISTS "sha256" text;
