-- M37 — operator-tunable preload (cache-warm) throttle knobs.
--
-- Adds four columns to site_perf_config so the operator can tune how the agent's
-- preload (cache-warm) queue drains: parallel drain workers, inter-request delay,
-- per-pass batch size, and a load-average-per-core ceiling above which a pass
-- defers. These are CP-authored config (the CP is the source of truth); the agent
-- mirrors them via the perf_config_update command and clamps them to the same
-- bounds locally.
--
--   preload_concurrency  integer  DEFAULT 1   — parallel loopback drain workers (1..4).
--   preload_delay_ms     integer  DEFAULT 500 — inter-request warm delay, ms (0..10000;
--                                               0 = no delay). The agent converts to µs.
--   preload_batch_size   integer  DEFAULT 50  — max URLs a single drain pass handles
--                                               (1..500; informational for the time-boxed
--                                               loopback runner).
--   preload_max_load     real     DEFAULT 0   — 1-min load-average-PER-CORE ceiling above
--                                               which a pass defers (0..64; 0 = disabled).
--
-- `real` (float4) is used for the load ceiling; the agent coerces it to a PHP
-- float. Defaults preserve today's strictly-serial behavior (concurrency 1,
-- 500 ms delay, load gate off).
--
-- Additive + idempotent (ADD COLUMN IF NOT EXISTS); re-running is safe. No RLS,
-- index, or policy change — these ride the existing site_perf_config row.

DO $$
BEGIN
    ALTER TABLE "public"."site_perf_config"
        ADD COLUMN IF NOT EXISTS "preload_concurrency" integer NOT NULL DEFAULT 1,
        ADD COLUMN IF NOT EXISTS "preload_delay_ms"    integer NOT NULL DEFAULT 500,
        ADD COLUMN IF NOT EXISTS "preload_batch_size"  integer NOT NULL DEFAULT 50,
        ADD COLUMN IF NOT EXISTS "preload_max_load"    real    NOT NULL DEFAULT 0;
END;
$$;
