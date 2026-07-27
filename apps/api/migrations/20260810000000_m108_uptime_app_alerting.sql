-- m108 - GH #291 Phase 3: application-health ALERTING (conservative, opt-in,
-- circuit-broken). Phase 2 (m107) shipped ONLY collection/display; nothing
-- alerted on app_up yet. This migration adds the schema this phase needs.
--
-- New columns:
--   sites.app_alerts_disabled        per-site opt-out. The probe keeps
--                                     running (the dashboard stays accurate);
--                                     only ALERTING is disabled for the site.
--   alert_configs.app_alerts_enabled per-tenant gate for the NEW app-health
--                                     alert kind, independent of `enabled`
--                                     (the existing reachability channel) so
--                                     an operator who already has downtime
--                                     alerts on does not silently start
--                                     receiving app-health alerts too. Its
--                                     DEFAULT is computed ONCE below (see the
--                                     rollout note).
--
-- New tables:
--   site_app_alert_state    per-site transition memory for the app-health
--                            alert state machine (mirrors site_alert_state,
--                            M5), plus ever_app_up: a STICKY flag, set once
--                            and never reset, recording whether this site has
--                            EVER been conclusively observed healthy. A site
--                            that has never cleared that bar may never fire
--                            an app alert (almost always a blocked/disabled
--                            REST route, not a broken site) - see
--                            internal/uptime/app_alerts.go EvaluateApp.
--   tenant_app_alert_breaker the fleet circuit-breaker's own transition
--                            memory: when more than a configurable ratio of
--                            a tenant's alert-eligible sites are
--                            simultaneously app-down, individual per-site
--                            alerts collapse into ONE aggregate notification
--                            instead of a page-per-site storm (far more
--                            likely to be our own monitoring, or a shared
--                            host/network, having a bad day than N unrelated
--                            clients breaking at once).
--   app_alert_rollout        a single global row recording the SAME
--                            migration-time "was this deployment already
--                            managing sites" decision described below, so
--                            application code that has not yet persisted a
--                            given tenant's alert_configs row (the
--                            synthesized zero-value default returned by
--                            uptime.Service.GetAlertConfig when none exists
--                            yet) reads the IDENTICAL default instead of
--                            hardcoding a second, possibly-inconsistent copy
--                            of this decision. Deliberately NOT RLS-scoped:
--                            it carries one global, non-tenant, non-sensitive
--                            fact - mirrors the `tenants` registry table's
--                            own no-RLS rationale (schema.sql: "tenants are
--                            not RLS-scoped").
--
-- Rollout default (design doc docs/security/uptime-app-health-design-
-- 2026-07-27.md section 5, "measure first, alert later"): app alerting must
-- default OFF on any deployment that ALREADY has sites at upgrade time (an
-- operator who has not asked for a wave of pages), and ON for a genuinely
-- fresh install (nothing to discover yet). Decided ONCE, deterministically,
-- HERE - never re-evaluated at runtime - from whether ANY site existed the
-- moment this migration ran:
--     SELECT (count(*) = 0) FROM sites
-- That single boolean becomes BOTH the literal DEFAULT baked onto the new
-- alert_configs.app_alerts_enabled column (so it applies to every existing
-- tenant row AND every future INSERT on this deployment identically - a
-- deployment-wide decision, not re-decided per tenant) AND the one row
-- seeded into app_alert_rollout. Computed via dynamic SQL (EXECUTE format(...,
-- %L)) because a column DEFAULT clause cannot itself be a subquery.
--
-- This migration runs under the migration bootstrap role (cmd/wpmgr/main.go:
-- "Migrations run with the owner/superuser DSN"), which is NOT subject to
-- RLS - mirrors m103's own cross-tenant backfill UPDATE, which needed no
-- app.agent/app.tenant_id GUC for the identical reason - so the
-- `SELECT count(*) FROM sites` below sees every tenant's rows, not just
-- whatever a request-scoped GUC would otherwise expose.
--
-- Idempotent throughout (ADD COLUMN IF NOT EXISTS / CREATE TABLE IF NOT
-- EXISTS / guarded CREATE POLICY, mirroring m36/m94/m103). The DO block that
-- computes fresh_install re-runs safely: ADD COLUMN IF NOT EXISTS is a no-op
-- on a second run (the column's default does not change once it exists), and
-- the app_alert_rollout insert is ON CONFLICT DO NOTHING.

-- ---------------------------------------------------------------------------
-- app_alert_rollout (no RLS - see the doc comment above)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."app_alert_rollout" (
        "singleton"     boolean     PRIMARY KEY DEFAULT true,
        "fresh_install" boolean     NOT NULL,
        "decided_at"    timestamptz NOT NULL DEFAULT now(),
        CONSTRAINT "app_alert_rollout_singleton_chk" CHECK ("singleton")
    );
END;
$$;

-- ---------------------------------------------------------------------------
-- site_app_alert_state (mirrors site_alert_state, M5, plus ever_app_up)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."site_app_alert_state" (
        "site_id"          uuid        NOT NULL,
        "tenant_id"        uuid        NOT NULL,
        "last_status"      text        NOT NULL DEFAULT 'unknown',
        "consecutive_down" integer     NOT NULL DEFAULT 0,
        "in_incident"      boolean     NOT NULL DEFAULT false,
        -- Sticky: set true the first time a CONCLUSIVE app_up=true verdict is
        -- observed, and never reset false again (a site cannot "un-prove"
        -- that it was once conclusively healthy). See EvaluateApp.
        "ever_app_up"      boolean     NOT NULL DEFAULT false,
        "last_alert_at"    timestamptz,
        "updated_at"       timestamptz NOT NULL DEFAULT now(),
        PRIMARY KEY ("site_id"),
        CONSTRAINT "site_app_alert_state_site_id_fkey" FOREIGN KEY ("site_id")
            REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "site_app_alert_state_tenant_id_fkey" FOREIGN KEY ("tenant_id")
            REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
    );
END;
$$;

CREATE INDEX IF NOT EXISTS "site_app_alert_state_tenant_id_idx"
    ON "public"."site_app_alert_state" ("tenant_id");

DO $$
BEGIN
    ALTER TABLE "public"."site_app_alert_state" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_app_alert_state" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_app_alert_state'
          AND policyname = 'site_app_alert_state_tenant_isolation'
    ) THEN
        CREATE POLICY "site_app_alert_state_tenant_isolation" ON "public"."site_app_alert_state"
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

-- The probe worker reads/writes this state cross-tenant inside the SAME
-- TransitionAlertState transaction as site_alert_state (app.agent GUC),
-- mirroring site_alert_state_agent exactly.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_app_alert_state'
          AND policyname = 'site_app_alert_state_agent'
    ) THEN
        CREATE POLICY "site_app_alert_state_agent" ON "public"."site_app_alert_state"
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- tenant_app_alert_breaker (one row per tenant; the fleet circuit-breaker's
-- own transition memory)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."tenant_app_alert_breaker" (
        "tenant_id"        uuid        NOT NULL,
        "tripped"          boolean     NOT NULL DEFAULT false,
        "tripped_at"       timestamptz,
        "last_alert_at"    timestamptz,
        -- last_down_count (GH #291 Phase 3 Fix 3): the down count AT THE
        -- TIME OF THE LAST notification (trip, update, or recovery), so a
        -- later tick can detect "materially worse since we last said
        -- anything" - see EvaluateAppBreaker's FireUpdate.
        "last_down_count"  integer     NOT NULL DEFAULT 0,
        "updated_at"       timestamptz NOT NULL DEFAULT now(),
        PRIMARY KEY ("tenant_id"),
        CONSTRAINT "tenant_app_alert_breaker_tenant_id_fkey" FOREIGN KEY ("tenant_id")
            REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
    );
END;
$$;

-- Idempotent add for a container that already ran this migration before
-- last_down_count existed (safe to re-run: ADD COLUMN IF NOT EXISTS).
ALTER TABLE "public"."tenant_app_alert_breaker"
    ADD COLUMN IF NOT EXISTS "last_down_count" integer NOT NULL DEFAULT 0;

DO $$
BEGIN
    ALTER TABLE "public"."tenant_app_alert_breaker" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."tenant_app_alert_breaker" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'tenant_app_alert_breaker'
          AND policyname = 'tenant_app_alert_breaker_tenant_isolation'
    ) THEN
        CREATE POLICY "tenant_app_alert_breaker_tenant_isolation" ON "public"."tenant_app_alert_breaker"
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
          AND tablename  = 'tenant_app_alert_breaker'
          AND policyname = 'tenant_app_alert_breaker_agent'
    ) THEN
        CREATE POLICY "tenant_app_alert_breaker_agent" ON "public"."tenant_app_alert_breaker"
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- sites.app_alerts_disabled - per-site opt-out (probe unaffected)
-- ---------------------------------------------------------------------------
ALTER TABLE "public"."sites"
    ADD COLUMN IF NOT EXISTS "app_alerts_disabled" boolean NOT NULL DEFAULT false;

-- ---------------------------------------------------------------------------
-- alert_configs.app_alerts_enabled - deployment-fresh-decided default (see
-- the rollout note above)
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    fresh_install boolean;
BEGIN
    SELECT (count(*) = 0) INTO fresh_install FROM "public"."sites";

    EXECUTE format(
        'ALTER TABLE "public"."alert_configs" ADD COLUMN IF NOT EXISTS "app_alerts_enabled" boolean NOT NULL DEFAULT %L',
        fresh_install
    );

    INSERT INTO "public"."app_alert_rollout" ("singleton", "fresh_install")
    VALUES (true, fresh_install)
    ON CONFLICT ("singleton") DO NOTHING;
END;
$$;
