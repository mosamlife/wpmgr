-- M23 — Media Optimizer (ADR-043). Cloud-encode JPEG/PNG → WebP/AVIF.
--
-- Adds three tenant-scoped tables:
--   site_media_assets       — one row per WP attachment we know about (synced
--                             from the site). Carries the optimization status,
--                             the size snapshots, and the per-size optimized /
--                             unoptimized maps. UNIQUE(site_id, wp_attachment_id).
--   media_optimization_jobs — one job per attachment per action (optimize /
--                             restore / delete_originals / sync). id is a ULID
--                             (TEXT) used as the agent's wpmgr_job_id.
--   media_variant_results   — per-variant (full/thumbnail/medium/…) encode result
--                             for a job; failures carry a human reason.
--
-- No image bytes are stored here (ADR-043 §2): bytes move agent↔storage via
-- presigned URLs and are dropped when the job ends; thumbnails load from the
-- site's own URLs. These tables hold metadata + status only.
--
-- RLS: every table is ENABLE + FORCE ROW LEVEL SECURITY with a tenant-isolation
-- policy (operator/API path, app.tenant_id GUC) AND an app.agent policy
-- (cross-tenant worker/agent path). Mirrors the m15 scan migration exactly.
-- updated_at is set by repo code (no trigger — there is no set_updated_at()).
-- Idempotency: every statement is IF-NOT-EXISTS guarded; re-running is safe.

-- ---------------------------------------------------------------------------
-- site_media_assets
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."site_media_assets" (
        "id"                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
        "tenant_id"           uuid NOT NULL,
        "site_id"             uuid NOT NULL,
        "wp_attachment_id"    bigint NOT NULL,
        "title"               text NOT NULL,
        "original_path"       text NOT NULL,
        "original_url"        text NOT NULL,
        "original_mime"       text NOT NULL,
        "original_width"      int,
        "original_height"     int,
        "original_size_bytes" bigint NOT NULL,
        -- 'original'|'webp'|'avif' — the format the optimized variants are in now.
        "current_format"      text NOT NULL DEFAULT 'original',
        "current_size_bytes"  bigint NOT NULL,
        -- pending|optimizing|optimized|failed|restoring|restored|excluded|originals_deleted
        "status"              text NOT NULL DEFAULT 'pending',
        "generation"          int NOT NULL DEFAULT 0,
        "compression_level"   text,  -- 'lossy'|'lossless' at last optimization
        "target_format"       text,  -- requested format at last optimization
        "sizes_optimized"     jsonb NOT NULL DEFAULT '[]'::jsonb,
        "sizes_unoptimized"   jsonb NOT NULL DEFAULT '{}'::jsonb,  -- map<size_name, reason>
        "last_optimized_at"   timestamptz,
        "last_synced_at"      timestamptz NOT NULL DEFAULT now(),
        "created_at"          timestamptz NOT NULL DEFAULT now(),
        "updated_at"          timestamptz NOT NULL DEFAULT now(),  -- set by app code, NOT a trigger
        CONSTRAINT "site_media_assets_tenant_id_fkey" FOREIGN KEY ("tenant_id")
            REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "site_media_assets_site_id_fkey" FOREIGN KEY ("site_id")
            REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "site_media_assets_site_attachment_uniq" UNIQUE ("site_id", "wp_attachment_id")
    );
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'site_media_assets'
          AND indexname = 'site_media_assets_site_status_idx'
    ) THEN
        CREATE INDEX "site_media_assets_site_status_idx"
            ON "public"."site_media_assets" ("site_id", "status");
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'site_media_assets'
          AND indexname = 'site_media_assets_tenant_idx'
    ) THEN
        CREATE INDEX "site_media_assets_tenant_idx"
            ON "public"."site_media_assets" ("tenant_id");
    END IF;
END;
$$;

DO $$
BEGIN
    ALTER TABLE "public"."site_media_assets" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_media_assets" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'site_media_assets'
          AND policyname = 'site_media_assets_tenant_isolation'
    ) THEN
        CREATE POLICY "site_media_assets_tenant_isolation" ON "public"."site_media_assets"
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'site_media_assets'
          AND policyname = 'site_media_assets_agent'
    ) THEN
        CREATE POLICY "site_media_assets_agent" ON "public"."site_media_assets"
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- media_optimization_jobs
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."media_optimization_jobs" (
        "id"                 text PRIMARY KEY,  -- ULID; the agent's wpmgr_job_id
        "tenant_id"          uuid NOT NULL,
        "site_id"            uuid NOT NULL,
        "asset_id"           uuid,
        "wp_attachment_id"   bigint NOT NULL,
        "kind"               text NOT NULL,  -- 'optimize'|'restore'|'delete_originals'|'sync'
        "target_format"      text,           -- 'avif'|'webp'|'original'
        "target_quality"     text,           -- 'lossy'|'lossless'
        -- queued|in_progress|succeeded|partially_succeeded|failed|cancelled
        "state"              text NOT NULL DEFAULT 'queued',
        "bytes_before"       bigint,
        "bytes_after"        bigint,
        "variants_total"     int NOT NULL DEFAULT 0,
        "variants_succeeded" int NOT NULL DEFAULT 0,
        "variants_failed"    int NOT NULL DEFAULT 0,
        "error_reason"       text,
        "initiator_user_id"  uuid,
        "created_at"         timestamptz NOT NULL DEFAULT now(),
        "started_at"         timestamptz,
        "completed_at"       timestamptz,
        CONSTRAINT "media_optimization_jobs_tenant_id_fkey" FOREIGN KEY ("tenant_id")
            REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "media_optimization_jobs_site_id_fkey" FOREIGN KEY ("site_id")
            REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "media_optimization_jobs_asset_id_fkey" FOREIGN KEY ("asset_id")
            REFERENCES "public"."site_media_assets" ("id") ON UPDATE NO ACTION ON DELETE SET NULL,
        CONSTRAINT "media_optimization_jobs_initiator_fkey" FOREIGN KEY ("initiator_user_id")
            REFERENCES "public"."users" ("id") ON UPDATE NO ACTION ON DELETE SET NULL
    );
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'media_optimization_jobs'
          AND indexname = 'media_optimization_jobs_site_state_idx'
    ) THEN
        CREATE INDEX "media_optimization_jobs_site_state_idx"
            ON "public"."media_optimization_jobs" ("site_id", "state");
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'media_optimization_jobs'
          AND indexname = 'media_optimization_jobs_tenant_created_idx'
    ) THEN
        CREATE INDEX "media_optimization_jobs_tenant_created_idx"
            ON "public"."media_optimization_jobs" ("tenant_id", "created_at" DESC);
    END IF;
END;
$$;

DO $$
BEGIN
    ALTER TABLE "public"."media_optimization_jobs" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."media_optimization_jobs" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'media_optimization_jobs'
          AND policyname = 'media_optimization_jobs_tenant_isolation'
    ) THEN
        CREATE POLICY "media_optimization_jobs_tenant_isolation" ON "public"."media_optimization_jobs"
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'media_optimization_jobs'
          AND policyname = 'media_optimization_jobs_agent'
    ) THEN
        CREATE POLICY "media_optimization_jobs_agent" ON "public"."media_optimization_jobs"
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- media_variant_results
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."media_variant_results" (
        "id"                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
        "job_id"               text NOT NULL,
        "tenant_id"            uuid NOT NULL,
        "variant_name"         text NOT NULL,  -- 'full'|'thumbnail'|'medium'|'large'|...
        "source_size_bytes"    bigint NOT NULL,
        "optimized_size_bytes" bigint,
        "source_mime"          text NOT NULL,
        "optimized_mime"       text,
        "encode_ms"            int,
        "state"                text NOT NULL,  -- 'succeeded'|'failed'|'skipped'
        "reason"               text,
        "created_at"           timestamptz NOT NULL DEFAULT now(),
        CONSTRAINT "media_variant_results_job_id_fkey" FOREIGN KEY ("job_id")
            REFERENCES "public"."media_optimization_jobs" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "media_variant_results_tenant_id_fkey" FOREIGN KEY ("tenant_id")
            REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
    );
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'media_variant_results'
          AND indexname = 'media_variant_results_job_idx'
    ) THEN
        CREATE INDEX "media_variant_results_job_idx"
            ON "public"."media_variant_results" ("job_id");
    END IF;
END;
$$;

DO $$
BEGIN
    ALTER TABLE "public"."media_variant_results" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."media_variant_results" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'media_variant_results'
          AND policyname = 'media_variant_results_tenant_isolation'
    ) THEN
        CREATE POLICY "media_variant_results_tenant_isolation" ON "public"."media_variant_results"
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'media_variant_results'
          AND policyname = 'media_variant_results_agent'
    ) THEN
        CREATE POLICY "media_variant_results_agent" ON "public"."media_variant_results"
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;
