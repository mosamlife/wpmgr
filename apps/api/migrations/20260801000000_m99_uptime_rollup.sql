-- m99 — durable fix for the /api/v1/sites cold-uptime-scan latency
--
-- Problem (see 20260730... m97 area and the interim 0.61.67 keep-warm):
-- metrics.pgStore.QueryFleetUptime enriches the /sites list with per-site
-- uptime via a LEFT JOIN LATERAL aggregate (COUNT/COUNT FILTER/AVG) over the
-- raw site_uptime_probes table for a 30-day window — ~43,200 rows/site at
-- the ~60s probe cadence. The m85 covering index made this index-only but
-- still O(probes-in-window) per site: on a cold Postgres buffer cache
-- (Cloud SQL db-g1-small) re-reading ~43k index leaf pages/site costs 6-8s.
-- 0.61.67 shipped an interim keep-warm refresher (internal/site/
-- uptime_keepwarm.go, WPMGR_UPTIME_KEEPWARM) that periodically re-ran the
-- query to keep the buffer cache resident — a stopgap, not a fix.
--
-- Fix: two small per-site rollup tables, maintained incrementally by the
-- probe worker (one UPSERT batch per sweep, see metrics.pgStore.UpsertRollup
-- / uptime.ProbeWorker.Sweep) instead of computed at read time:
--
--   site_uptime_daily   one row per (site, UTC calendar day), additive
--                       counters (up_checks/total_checks/sum_latency_ms/
--                       latency_samples) incremented once per probe.
--   site_uptime_status  one row per site, the latest probe snapshot
--                       (latest_up/last_probed_at/tls_expiry), upserted with
--                       a last_probed_at >= freshness guard so an
--                       overlapping/delayed sweep can never regress it.
--
-- QueryFleetUptime is rewritten (internal/metrics/postgres.go) to read ONLY
-- these two small tables — O(days-in-window) per site instead of
-- O(probes-in-window) — so it no longer scans site_uptime_probes at all.
-- The interim keep-warm refresher and its WPMGR_UPTIME_KEEPWARM flag are
-- removed in the same change (no longer needed).
--
-- Raw-probe retention (90 days, m85/m86) is UNCHANGED — site_uptime_probes
-- stays the system of record for incident drill-down (QueryProbeWindow/
-- QuerySeries), which this migration does not touch.
--
-- RLS mirrors site_incidents (m94) exactly: tenant isolation + the app.agent
-- cross-tenant probe-worker/read path + the m19 AS RESTRICTIVE site_scope
-- policy every direct site-keyed table carries.
--
-- Idempotent throughout (DO $$ ... IF NOT EXISTS ... $$), mirrors m93/m94.

-- ---------------------------------------------------------------------------
-- site_uptime_daily
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."site_uptime_daily" (
        "tenant_id"       uuid             NOT NULL,
        "site_id"         uuid             NOT NULL,
        "day"             date             NOT NULL,
        "up_checks"       integer          NOT NULL DEFAULT 0,
        "total_checks"    integer          NOT NULL DEFAULT 0,
        "sum_latency_ms"  double precision NOT NULL DEFAULT 0,
        "latency_samples" integer          NOT NULL DEFAULT 0,
        "updated_at"      timestamptz      NOT NULL DEFAULT now(),
        PRIMARY KEY ("site_id", "day"),
        CONSTRAINT "site_uptime_daily_tenant_id_fkey" FOREIGN KEY ("tenant_id")
            REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "site_uptime_daily_site_id_fkey" FOREIGN KEY ("site_id")
            REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
    );
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'site_uptime_daily'
          AND indexname  = 'site_uptime_daily_tenant_idx'
    ) THEN
        CREATE INDEX "site_uptime_daily_tenant_idx"
            ON "public"."site_uptime_daily" ("tenant_id");
    END IF;
END;
$$;

DO $$
BEGIN
    ALTER TABLE "public"."site_uptime_daily" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_uptime_daily" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_uptime_daily'
          AND policyname = 'site_uptime_daily_tenant_isolation'
    ) THEN
        CREATE POLICY "site_uptime_daily_tenant_isolation" ON "public"."site_uptime_daily"
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_uptime_daily'
          AND policyname = 'site_uptime_daily_agent'
    ) THEN
        CREATE POLICY "site_uptime_daily_agent" ON "public"."site_uptime_daily"
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_uptime_daily'
          AND policyname = 'site_uptime_daily_site_scope'
    ) THEN
        CREATE POLICY "site_uptime_daily_site_scope" ON "public"."site_uptime_daily"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- site_uptime_status
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."site_uptime_status" (
        "site_id"        uuid        NOT NULL,
        "tenant_id"      uuid        NOT NULL,
        "latest_up"      boolean     NOT NULL,
        "last_probed_at" timestamptz NOT NULL,
        "tls_expiry"     timestamptz,
        "updated_at"     timestamptz NOT NULL DEFAULT now(),
        PRIMARY KEY ("site_id"),
        CONSTRAINT "site_uptime_status_tenant_id_fkey" FOREIGN KEY ("tenant_id")
            REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "site_uptime_status_site_id_fkey" FOREIGN KEY ("site_id")
            REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
    );
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'site_uptime_status'
          AND indexname  = 'site_uptime_status_tenant_idx'
    ) THEN
        CREATE INDEX "site_uptime_status_tenant_idx"
            ON "public"."site_uptime_status" ("tenant_id");
    END IF;
END;
$$;

DO $$
BEGIN
    ALTER TABLE "public"."site_uptime_status" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_uptime_status" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_uptime_status'
          AND policyname = 'site_uptime_status_tenant_isolation'
    ) THEN
        CREATE POLICY "site_uptime_status_tenant_isolation" ON "public"."site_uptime_status"
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_uptime_status'
          AND policyname = 'site_uptime_status_agent'
    ) THEN
        CREATE POLICY "site_uptime_status_agent" ON "public"."site_uptime_status"
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_uptime_status'
          AND policyname = 'site_uptime_status_site_scope'
    ) THEN
        CREATE POLICY "site_uptime_status_site_scope" ON "public"."site_uptime_status"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- Backfill (one-time, day-1 seed from existing raw probes)
-- ---------------------------------------------------------------------------
-- Scale assumption: at WPMgr's expected fleet size (≤100 sites × ~1 probe/
-- 60s, 90-day retention, m86) site_uptime_probes holds at most a few million
-- rows, so a single GROUP BY pass is inexpensive and runs comfortably inside
-- this migration's own transaction (internal/db/migrate.go wraps each
-- migration file in one tx). If the raw table ever grows beyond that (a much
-- larger fleet), this backfill would need batching by tenant/site — not done
-- here since it is out of scope for WPMgr's current/near-term scale.
--
-- ON CONFLICT DO NOTHING makes both backfills idempotent: a second
-- application (or a migration re-run) is a no-op once rows exist, and it
-- never overwrites a bucket the probe worker has already started
-- incrementing (the worker only runs after this migration has committed, on
-- the same boot sequence, so in practice there is no overlap — but DO
-- NOTHING is the safe default regardless).
--
-- The aggregate math mirrors the OLD QueryFleetUptime exactly:
--   up_checks       = count(*) FILTER (WHERE up)
--   total_checks    = count(*)
--   sum_latency_ms  = sum(total_ms) FILTER (WHERE up AND total_ms <> 0)
--   latency_samples = count(*) FILTER (WHERE up AND total_ms <> 0)
-- reproducing the old AVG(NULLIF(total_ms, 0)) FILTER (WHERE up) exactly
-- (sum_latency_ms / latency_samples), so historical uptime % / avg latency
-- read identically before and after this migration.
INSERT INTO "public"."site_uptime_daily"
    ("tenant_id", "site_id", "day", "up_checks", "total_checks", "sum_latency_ms", "latency_samples")
SELECT
    "tenant_id",
    "site_id",
    ("probed_at" AT TIME ZONE 'UTC')::date AS "day",
    count(*) FILTER (WHERE "up")                                   AS "up_checks",
    count(*)                                                       AS "total_checks",
    coalesce(sum("total_ms") FILTER (WHERE "up" AND "total_ms" <> 0), 0) AS "sum_latency_ms",
    count(*) FILTER (WHERE "up" AND "total_ms" <> 0)                AS "latency_samples"
FROM "public"."site_uptime_probes"
GROUP BY "tenant_id", "site_id", (("probed_at" AT TIME ZONE 'UTC')::date)
ON CONFLICT ("site_id", "day") DO NOTHING;

-- Seed the current-status stamp from each site's single most recent probe.
INSERT INTO "public"."site_uptime_status"
    ("site_id", "tenant_id", "latest_up", "last_probed_at", "tls_expiry")
SELECT DISTINCT ON ("site_id")
    "site_id", "tenant_id", "up", "probed_at", "tls_expiry"
FROM "public"."site_uptime_probes"
ORDER BY "site_id", "probed_at" DESC
ON CONFLICT ("site_id") DO NOTHING;
