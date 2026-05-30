-- M14 Login Whitelabel — per-site login brand config.
--
-- Adds one table:
--   site_login_brand — per-site login brand config (logo_url, logo_link,
--                      message). Holds at most one row per site (site_id is
--                      PRIMARY KEY). The CP pushes this config to the agent via
--                      the signed `sync_login_brand` command on every save.
--
-- RLS mirrors the M12/M13 pattern: tenant isolation + an agent-write policy so
-- both InTenantTx (operator) and InTenantTxAsAgent (ingest) work correctly.
-- Idempotency: every statement is guarded with IF NOT EXISTS / IF NOT EXISTS
-- checks so running this migration twice is safe.

-- ---------------------------------------------------------------------------
-- site_login_brand
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."site_login_brand" (
        "tenant_id"  uuid NOT NULL,
        "site_id"    uuid NOT NULL,
        -- logo_url is the full URL of the image shown on the WP login page.
        -- Empty string = no override (WordPress default logo).
        "logo_url"   text NOT NULL DEFAULT '',
        -- logo_link is the URL the logo links to. Empty = no override.
        "logo_link"  text NOT NULL DEFAULT '',
        -- message is the text shown below the logo on the login page.
        -- Empty = no override. Max 2000 characters enforced at the CP layer.
        "message"    text NOT NULL DEFAULT '',
        "updated_at" timestamptz NOT NULL DEFAULT now(),
        PRIMARY KEY ("site_id"),
        CONSTRAINT "site_login_brand_tenant_id_fkey" FOREIGN KEY ("tenant_id")
            REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "site_login_brand_site_id_fkey" FOREIGN KEY ("site_id")
            REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
    );
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'site_login_brand'
          AND indexname  = 'site_login_brand_tenant_idx'
    ) THEN
        CREATE INDEX "site_login_brand_tenant_idx"
            ON "public"."site_login_brand" ("tenant_id");
    END IF;
END;
$$;

-- Row-Level Security for site_login_brand.
DO $$
BEGIN
    ALTER TABLE "public"."site_login_brand" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_login_brand" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_login_brand'
          AND policyname = 'site_login_brand_tenant_isolation'
    ) THEN
        CREATE POLICY "site_login_brand_tenant_isolation" ON "public"."site_login_brand"
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
          AND tablename  = 'site_login_brand'
          AND policyname = 'site_login_brand_agent'
    ) THEN
        CREATE POLICY "site_login_brand_agent" ON "public"."site_login_brand"
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;
