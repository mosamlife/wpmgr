-- S3 Malware / File-Integrity Scan (stream-hashes-to-CP).
--
-- Adds four tables:
--   scan_runs             — per-site scan job (queued → scanning → diffing → done/failed).
--                          RLS: tenant isolation + agent policy.
--   scan_run_hashes       — staging table for streamed file hashes; purged on done/failed.
--                          RLS mirrors scan_runs.
--   scan_findings         — deduplicated finding rows; UNIQUE(tenant,site,dedup_key);
--                          ON CONFLICT bumps last_seen_run + actual_md5, PRESERVES ignored.
--                          RLS mirrors scan_runs.
--   wporg_core_checksums  — public reference: WordPress.org known-good checksums.
--                          NO RLS (public data, not tenant-scoped).
--   wporg_core_checksums_meta — negative-cache / freshness metadata for checksums.
--                          NO RLS (public data).
--
-- Idempotency: every statement is guarded with IF NOT EXISTS checks so running
-- this migration twice is safe.

-- ---------------------------------------------------------------------------
-- scan_runs
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."scan_runs" (
        "id"            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
        "tenant_id"     uuid NOT NULL,
        "site_id"       uuid NOT NULL,
        "kind"          text NOT NULL DEFAULT 'core',  -- 'core'|'files'|'full'
        "status"        text NOT NULL DEFAULT 'queued', -- 'queued'|'scanning'|'diffing'|'done'|'failed'
        -- cursor is the resume_cursor JSON the agent returned in the last batch.
        -- NULL means we have not started the first scan command yet.
        "cursor"        jsonb,
        "files_scanned" bigint NOT NULL DEFAULT 0,
        "wp_version"    text,
        "locale"        text,
        "error"         text,
        -- finding_counts is a JSONB map: {core_modified:N, core_missing:N, core_unknown_injected:N}
        "finding_counts" jsonb,
        "created_at"    timestamptz NOT NULL DEFAULT now(),
        "started_at"    timestamptz,
        "finished_at"   timestamptz,
        CONSTRAINT "scan_runs_tenant_id_fkey" FOREIGN KEY ("tenant_id")
            REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "scan_runs_site_id_fkey" FOREIGN KEY ("site_id")
            REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
    );
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'scan_runs'
          AND indexname  = 'scan_runs_tenant_site_created_idx'
    ) THEN
        CREATE INDEX "scan_runs_tenant_site_created_idx"
            ON "public"."scan_runs" ("tenant_id", "site_id", "created_at" DESC);
    END IF;
END;
$$;

DO $$
BEGIN
    ALTER TABLE "public"."scan_runs" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."scan_runs" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'scan_runs'
          AND policyname = 'scan_runs_tenant_isolation'
    ) THEN
        CREATE POLICY "scan_runs_tenant_isolation" ON "public"."scan_runs"
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
          AND tablename  = 'scan_runs'
          AND policyname = 'scan_runs_agent'
    ) THEN
        CREATE POLICY "scan_runs_agent" ON "public"."scan_runs"
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- scan_run_hashes  (staging; purged on completion / failed)
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."scan_run_hashes" (
        "id"        bigserial PRIMARY KEY,
        "tenant_id" uuid NOT NULL,
        "run_id"    uuid NOT NULL,
        "path"      text NOT NULL,
        "size"      bigint,
        "md5"       text,      -- 32 hex chars or '' for unreadable
        "mtime"     bigint,    -- Unix seconds from agent
        "is_link"   boolean NOT NULL DEFAULT false,
        UNIQUE ("run_id", "path"),
        CONSTRAINT "scan_run_hashes_tenant_id_fkey" FOREIGN KEY ("tenant_id")
            REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "scan_run_hashes_run_id_fkey" FOREIGN KEY ("run_id")
            REFERENCES "public"."scan_runs" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
    );
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'scan_run_hashes'
          AND indexname  = 'scan_run_hashes_run_idx'
    ) THEN
        CREATE INDEX "scan_run_hashes_run_idx"
            ON "public"."scan_run_hashes" ("run_id");
    END IF;
END;
$$;

DO $$
BEGIN
    ALTER TABLE "public"."scan_run_hashes" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."scan_run_hashes" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'scan_run_hashes'
          AND policyname = 'scan_run_hashes_tenant_isolation'
    ) THEN
        CREATE POLICY "scan_run_hashes_tenant_isolation" ON "public"."scan_run_hashes"
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
          AND tablename  = 'scan_run_hashes'
          AND policyname = 'scan_run_hashes_agent'
    ) THEN
        CREATE POLICY "scan_run_hashes_agent" ON "public"."scan_run_hashes"
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- scan_findings
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."scan_findings" (
        "id"            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
        "tenant_id"     uuid NOT NULL,
        "site_id"       uuid NOT NULL,
        "run_id"        uuid NOT NULL,
        -- finding_type: 'core_modified'|'core_missing'|'core_unknown_injected'
        "finding_type"  text NOT NULL,
        "path"          text NOT NULL,
        -- severity: 'high'|'medium'
        "severity"      text NOT NULL,
        "expected_md5"  text,
        "actual_md5"    text,
        -- dedup_key = md5(site_id || ':' || finding_type || ':' || path)
        "dedup_key"     text NOT NULL,
        "ignored"       boolean NOT NULL DEFAULT false,
        "ignored_by"    text,
        "created_at"    timestamptz NOT NULL DEFAULT now(),
        "last_seen_run" uuid NOT NULL,
        UNIQUE ("tenant_id", "site_id", "dedup_key"),
        CONSTRAINT "scan_findings_tenant_id_fkey" FOREIGN KEY ("tenant_id")
            REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "scan_findings_site_id_fkey" FOREIGN KEY ("site_id")
            REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
    );
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'scan_findings'
          AND indexname  = 'scan_findings_tenant_site_idx'
    ) THEN
        CREATE INDEX "scan_findings_tenant_site_idx"
            ON "public"."scan_findings" ("tenant_id", "site_id", "ignored", "created_at" DESC);
    END IF;
END;
$$;

DO $$
BEGIN
    ALTER TABLE "public"."scan_findings" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."scan_findings" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'scan_findings'
          AND policyname = 'scan_findings_tenant_isolation'
    ) THEN
        CREATE POLICY "scan_findings_tenant_isolation" ON "public"."scan_findings"
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
          AND tablename  = 'scan_findings'
          AND policyname = 'scan_findings_agent'
    ) THEN
        CREATE POLICY "scan_findings_agent" ON "public"."scan_findings"
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- wporg_core_checksums  (public reference; NO RLS)
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."wporg_core_checksums" (
        "version"    text NOT NULL,
        "locale"     text NOT NULL,
        "path"       text NOT NULL,
        "md5"        text NOT NULL,
        "fetched_at" timestamptz NOT NULL DEFAULT now(),
        PRIMARY KEY ("version", "locale", "path")
    );
END;
$$;

-- ---------------------------------------------------------------------------
-- wporg_core_checksums_meta  (negative-cache / freshness; NO RLS)
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."wporg_core_checksums_meta" (
        "version"    text NOT NULL,
        "locale"     text NOT NULL,
        "fetched_at" timestamptz NOT NULL DEFAULT now(),
        -- ok=false means the fetch failed (404/empty); used for negative-cache TTL.
        "ok"         boolean NOT NULL DEFAULT true,
        PRIMARY KEY ("version", "locale")
    );
END;
$$;
