-- m107 - GH #291 Phase 2: application-health probe (the actual new
-- capability; Phase 0 hardened ClickHouse, Phase 1 fixed deriveFleetStatus
-- to stop rendering a sweeper-detected outage as green Up).
--
-- Problem: the FROZEN reachability probe requests the cacheable homepage, so
-- a site whose PHP is completely dead but is still served a cached 200
-- reports "up" forever. Phase 1 already fixed the case where the AGENT can
-- tell us (a disconnected agent now derives Degraded). This migration adds
-- the columns for a DIRECT measurement so the control plane does not depend
-- on the agent being enrolled-and-disconnected to notice.
--
-- Two-signal model (locked, see docs/security/uptime-app-health-design-
-- 2026-07-27.md section 1): "up" (reachability) keeps its exact current
-- meaning forever - every column it already feeds (site_uptime_probes.up,
-- sites.health_status, site_incidents, uptime percentages, SLA, the client
-- portal, white-label PDFs) is UNTOUCHED by this migration. "app_up"
-- (application health) is a new, orthogonal, three-valued signal (true /
-- false / unknown=NULL) living entirely in new, additive, nullable columns.
-- No new sites.connection_state / FleetSiteStatus enum value.
--
-- Columns, all additive and nullable (no backfill, no default, no table
-- rewrite):
--
--   sites.app_probe_path            B3 per-site override path (text).
--   site_uptime_probes.app_up            per-probe verdict (boolean).
--   site_uptime_probes.app_probe_reason  per-probe machine-readable reason.
--   site_uptime_daily.app_up_checks      additive day counter (integer).
--   site_uptime_daily.app_total_checks   additive day counter (integer).
--   site_uptime_status.latest_app_up        latest verdict (boolean).
--   site_uptime_status.app_probe_reason     latest reason (text).
--   site_uptime_status.last_app_probed_at   latest app-probe timestamp.
--
-- TRAP #1 (this migration does NOT fall into it): site_uptime_daily is keyed
-- (site_id, day) with no "kind" dimension, so recording app health as a
-- SECOND ROW per sweep would double total_checks and blend two different
-- meanings, silently corrupting every uptime percentage in the product. App
-- health rides the SAME row as new nullable columns instead.
--
-- TRAP #2 (handled in application code, not SQL, but the reason these
-- columns are nullable-with-no-default rather than NOT NULL DEFAULT 0/false):
-- the app probe runs at a slower cadence than the reachability probe
-- (default 300s vs 60s), so most rollup upserts carry no app-health opinion
-- at all. metrics.pgStore.UpsertRollup COALESCE/CASE-guards every write to
-- these columns so an absent opinion never overwrites a known value - see
-- that function's doc comment.
--
-- Idempotent (ADD COLUMN IF NOT EXISTS mirrors m104/m103/m101/m92/m93).

ALTER TABLE "public"."sites"
    ADD COLUMN IF NOT EXISTS "app_probe_path" text NULL;

ALTER TABLE "public"."site_uptime_probes"
    ADD COLUMN IF NOT EXISTS "app_up" boolean NULL,
    ADD COLUMN IF NOT EXISTS "app_probe_reason" text NULL;

ALTER TABLE "public"."site_uptime_daily"
    ADD COLUMN IF NOT EXISTS "app_up_checks" integer NULL,
    ADD COLUMN IF NOT EXISTS "app_total_checks" integer NULL;

ALTER TABLE "public"."site_uptime_status"
    ADD COLUMN IF NOT EXISTS "latest_app_up" boolean NULL,
    ADD COLUMN IF NOT EXISTS "app_probe_reason" text NULL,
    ADD COLUMN IF NOT EXISTS "last_app_probed_at" timestamptz NULL;
