-- Restore Runs + Logs (m16).
--
-- Adds two tables:
--   restore_runs        — first-class per-site restore entity (queued→running→completed/failed/rolled_back).
--                         RLS: tenant-isolation + agent policy.
--   restore_run_events  — durable append-only phase log; one row per agent progress POST
--                         that is a restore phase. ON DELETE CASCADE from restore_runs.
--                         RLS: tenant-isolation + agent policy.
--
-- Idempotency: every statement is guarded with IF NOT EXISTS checks so running
-- this migration twice is safe.

-- ---------------------------------------------------------------------------
-- restore_runs
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."restore_runs" (
        "id"            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
        "tenant_id"     uuid NOT NULL,
        "site_id"       uuid NOT NULL,
        "snapshot_id"   uuid NOT NULL,
        "mode"          text NOT NULL DEFAULT '',
        "components"    text[] NOT NULL DEFAULT '{}',
        "selection"     jsonb NOT NULL DEFAULT '{}',
        "status"        text NOT NULL DEFAULT 'queued',
        "current_phase" text,
        "error"         text,
        "triggered_by"  text,
        "created_at"    timestamptz NOT NULL DEFAULT now(),
        "started_at"    timestamptz,
        "finished_at"   timestamptz,
        "updated_at"    timestamptz NOT NULL DEFAULT now(),
        CONSTRAINT "restore_runs_tenant_id_fkey" FOREIGN KEY ("tenant_id")
            REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "restore_runs_site_id_fkey" FOREIGN KEY ("site_id")
            REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
    );
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'restore_runs'
          AND indexname  = 'restore_runs_tenant_site_created_idx'
    ) THEN
        CREATE INDEX "restore_runs_tenant_site_created_idx"
            ON "public"."restore_runs" ("tenant_id", "site_id", "created_at" DESC);
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'restore_runs'
          AND indexname  = 'restore_runs_snapshot_status_idx'
    ) THEN
        CREATE INDEX "restore_runs_snapshot_status_idx"
            ON "public"."restore_runs" ("snapshot_id", "status");
    END IF;
END;
$$;

DO $$
BEGIN
    ALTER TABLE "public"."restore_runs" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."restore_runs" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'restore_runs'
          AND policyname = 'restore_runs_tenant_isolation'
    ) THEN
        CREATE POLICY "restore_runs_tenant_isolation" ON "public"."restore_runs"
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
          AND tablename  = 'restore_runs'
          AND policyname = 'restore_runs_agent'
    ) THEN
        CREATE POLICY "restore_runs_agent" ON "public"."restore_runs"
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- restore_run_events
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."restore_run_events" (
        "id"             bigserial PRIMARY KEY,
        "tenant_id"      uuid NOT NULL,
        "restore_run_id" uuid NOT NULL,
        "phase"          text NOT NULL,
        "status"         text NOT NULL DEFAULT '',
        "message"        text NOT NULL DEFAULT '',
        "detail"         jsonb,
        "occurred_at"    timestamptz NOT NULL DEFAULT now(),
        CONSTRAINT "restore_run_events_tenant_id_fkey" FOREIGN KEY ("tenant_id")
            REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "restore_run_events_restore_run_id_fkey" FOREIGN KEY ("restore_run_id")
            REFERENCES "public"."restore_runs" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
    );
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'restore_run_events'
          AND indexname  = 'restore_run_events_run_id_idx'
    ) THEN
        CREATE INDEX "restore_run_events_run_id_idx"
            ON "public"."restore_run_events" ("restore_run_id", "id");
    END IF;
END;
$$;

DO $$
BEGIN
    ALTER TABLE "public"."restore_run_events" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."restore_run_events" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'restore_run_events'
          AND policyname = 'restore_run_events_tenant_isolation'
    ) THEN
        CREATE POLICY "restore_run_events_tenant_isolation" ON "public"."restore_run_events"
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
          AND tablename  = 'restore_run_events'
          AND policyname = 'restore_run_events_agent'
    ) THEN
        CREATE POLICY "restore_run_events_agent" ON "public"."restore_run_events"
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;
