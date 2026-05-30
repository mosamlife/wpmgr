-- Backup Schedule (m17).
--
-- Extends backup_schedules with full time/day/frequency columns and keep_last
-- retention; extends sites with wp_timezone/wp_gmt_offset from diagnostics;
-- creates backup_schedule_runs to materialize the pre-inserted upcoming run
-- and the terminal history (mirrors restore_runs / m16).
--
-- Idempotency: every statement is guarded with IF NOT EXISTS checks so running
-- this migration twice is safe. Runs in ONE transaction (no CONCURRENTLY).

-- ---------------------------------------------------------------------------
-- A. Extend backup_schedules
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    ALTER TABLE "public"."backup_schedules"
        ADD COLUMN IF NOT EXISTS "run_hour"       smallint NOT NULL DEFAULT 2,
        ADD COLUMN IF NOT EXISTS "run_minute"     smallint NOT NULL DEFAULT 0,
        ADD COLUMN IF NOT EXISTS "day_of_week"    smallint NULL,
        ADD COLUMN IF NOT EXISTS "day_of_month"   smallint NULL,
        ADD COLUMN IF NOT EXISTS "frequency_hours" smallint NULL,
        ADD COLUMN IF NOT EXISTS "keep_last"      integer  NOT NULL DEFAULT 7;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'backup_schedules_run_hour_check'
          AND conrelid = 'public.backup_schedules'::regclass
    ) THEN
        ALTER TABLE "public"."backup_schedules"
            ADD CONSTRAINT "backup_schedules_run_hour_check"
            CHECK ("run_hour" BETWEEN 0 AND 23);
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'backup_schedules_run_minute_check'
          AND conrelid = 'public.backup_schedules'::regclass
    ) THEN
        ALTER TABLE "public"."backup_schedules"
            ADD CONSTRAINT "backup_schedules_run_minute_check"
            CHECK ("run_minute" BETWEEN 0 AND 59);
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'backup_schedules_day_of_week_check'
          AND conrelid = 'public.backup_schedules'::regclass
    ) THEN
        ALTER TABLE "public"."backup_schedules"
            ADD CONSTRAINT "backup_schedules_day_of_week_check"
            CHECK ("day_of_week" BETWEEN 0 AND 6);
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'backup_schedules_day_of_month_check'
          AND conrelid = 'public.backup_schedules'::regclass
    ) THEN
        ALTER TABLE "public"."backup_schedules"
            ADD CONSTRAINT "backup_schedules_day_of_month_check"
            CHECK ("day_of_month" BETWEEN 1 AND 28);
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'backup_schedules_frequency_hours_check'
          AND conrelid = 'public.backup_schedules'::regclass
    ) THEN
        ALTER TABLE "public"."backup_schedules"
            ADD CONSTRAINT "backup_schedules_frequency_hours_check"
            CHECK ("frequency_hours" BETWEEN 1 AND 24);
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'backup_schedules_keep_last_check'
          AND conrelid = 'public.backup_schedules'::regclass
    ) THEN
        ALTER TABLE "public"."backup_schedules"
            ADD CONSTRAINT "backup_schedules_keep_last_check"
            CHECK ("keep_last" >= 0);
    END IF;
END;
$$;

-- Widen cadence to include the two new values (hourly, every_n_hours).
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'backup_schedules_cadence_check'
          AND conrelid = 'public.backup_schedules'::regclass
    ) THEN
        ALTER TABLE "public"."backup_schedules"
            ADD CONSTRAINT "backup_schedules_cadence_check"
            CHECK ("cadence" IN ('hourly','every_n_hours','daily','weekly','monthly'));
    END IF;
END;
$$;

-- Add kind CHECK (guard so re-run is safe).
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'backup_schedules_kind_check'
          AND conrelid = 'public.backup_schedules'::regclass
    ) THEN
        ALTER TABLE "public"."backup_schedules"
            ADD CONSTRAINT "backup_schedules_kind_check"
            CHECK ("kind" IN ('files','db','full'));
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- B. Extend sites with wp_timezone / wp_gmt_offset
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    ALTER TABLE "public"."sites"
        ADD COLUMN IF NOT EXISTS "wp_timezone"   text NOT NULL DEFAULT '',
        ADD COLUMN IF NOT EXISTS "wp_gmt_offset" real NOT NULL DEFAULT 0;
END;
$$;

-- ---------------------------------------------------------------------------
-- C. New table backup_schedule_runs
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."backup_schedule_runs" (
        "id"            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
        "tenant_id"     uuid        NOT NULL,
        "site_id"       uuid        NOT NULL,
        "schedule_id"   uuid        NOT NULL,
        "snapshot_id"   uuid        NULL,
        "scheduled_for" timestamptz NOT NULL,
        "status"        text        NOT NULL DEFAULT 'scheduled',
        "kind"          text        NOT NULL DEFAULT 'full',
        "error"         text        NULL,
        "triggered_by"  text        NULL,
        "created_at"    timestamptz NOT NULL DEFAULT now(),
        "started_at"    timestamptz NULL,
        "finished_at"   timestamptz NULL,
        "updated_at"    timestamptz NOT NULL DEFAULT now(),
        CONSTRAINT "backup_schedule_runs_status_check"
            CHECK ("status" IN ('scheduled','queued','running','completed','failed','skipped','canceled')),
        CONSTRAINT "backup_schedule_runs_tenant_id_fkey" FOREIGN KEY ("tenant_id")
            REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "backup_schedule_runs_site_id_fkey" FOREIGN KEY ("site_id")
            REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "backup_schedule_runs_schedule_id_fkey" FOREIGN KEY ("schedule_id")
            REFERENCES "public"."backup_schedules" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "backup_schedule_runs_snapshot_id_fkey" FOREIGN KEY ("snapshot_id")
            REFERENCES "public"."backup_snapshots" ("id") ON UPDATE NO ACTION ON DELETE SET NULL
    );
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'backup_schedule_runs'
          AND indexname  = 'backup_schedule_runs_tenant_site_for_idx'
    ) THEN
        CREATE INDEX "backup_schedule_runs_tenant_site_for_idx"
            ON "public"."backup_schedule_runs" ("tenant_id", "site_id", "scheduled_for" DESC);
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'backup_schedule_runs'
          AND indexname  = 'backup_schedule_runs_status_for_idx'
    ) THEN
        CREATE INDEX "backup_schedule_runs_status_for_idx"
            ON "public"."backup_schedule_runs" ("status", "scheduled_for");
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'backup_schedule_runs'
          AND indexname  = 'backup_schedule_runs_schedule_id_idx'
    ) THEN
        CREATE INDEX "backup_schedule_runs_schedule_id_idx"
            ON "public"."backup_schedule_runs" ("schedule_id");
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'backup_schedule_runs'
          AND indexname  = 'backup_schedule_runs_schedule_for_key'
    ) THEN
        CREATE UNIQUE INDEX "backup_schedule_runs_schedule_for_key"
            ON "public"."backup_schedule_runs" ("schedule_id", "scheduled_for");
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- D. RLS for backup_schedule_runs
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    ALTER TABLE "public"."backup_schedule_runs" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."backup_schedule_runs" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'backup_schedule_runs'
          AND policyname = 'backup_schedule_runs_tenant_isolation'
    ) THEN
        CREATE POLICY "backup_schedule_runs_tenant_isolation" ON "public"."backup_schedule_runs"
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

-- FOR ALL: the scheduler INSERTs and UPDATEs run rows cross-tenant under
-- app.agent='on'. Unlike restore_runs (agent reads only), the schedule
-- materializer both writes (pre-insert upcoming) and updates (advance to
-- queued/running/completed/failed/skipped) across tenant boundaries.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'backup_schedule_runs'
          AND policyname = 'backup_schedule_runs_agent'
    ) THEN
        CREATE POLICY "backup_schedule_runs_agent" ON "public"."backup_schedule_runs"
            FOR ALL
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;
