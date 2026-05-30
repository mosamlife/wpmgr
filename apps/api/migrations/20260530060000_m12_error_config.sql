-- S1.2 PHP-error ignore-list + error-level config.
--
-- Adds a per-site error configuration table that stores the PHP error reporting
-- level mask and an ignore-list of md5 fingerprints. The CP pushes this config
-- to the agent via a signed `sync_error_config` command whenever it changes.
-- The table holds at most one row per site (site_id is the PRIMARY KEY).
--
-- RLS mirrors the M8 agent_php_errors pattern: tenant isolation + a no-op
-- agent policy for symmetry. Operator reads/writes use InTenantTx.
--
-- Idempotency: each statement is guarded so running this migration twice is
-- safe (DO $$ ... IF NOT EXISTS ... $$).

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."site_error_config" (
        "tenant_id"    uuid NOT NULL,
        "site_id"      uuid NOT NULL,
        -- error_level is the PHP E_* bitmask (e.g. 6143 = E_ALL & ~E_STRICT,
        -- the WP default). >0, fits in int32.
        "error_level"  integer NOT NULL DEFAULT 6143,
        -- ignore_md5s is the list of md5 fingerprints the agent must suppress
        -- without counting; empty by default.
        "ignore_md5s"  text[] NOT NULL DEFAULT '{}',
        "updated_at"   timestamptz NOT NULL DEFAULT now(),
        PRIMARY KEY ("site_id"),
        CONSTRAINT "site_error_config_tenant_id_fkey" FOREIGN KEY ("tenant_id")
            REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "site_error_config_site_id_fkey" FOREIGN KEY ("site_id")
            REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
    );
END;
$$;

-- Tenant-scoped lookup index (list all configs for a tenant is a rare admin
-- query but worth supporting without a seq-scan).
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'site_error_config'
          AND indexname  = 'site_error_config_tenant_idx'
    ) THEN
        CREATE INDEX "site_error_config_tenant_idx"
            ON "public"."site_error_config" ("tenant_id");
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- Row-Level Security (mirrors M8 agent_php_errors pattern).
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    -- Enable RLS (idempotent; ALTER TABLE … ENABLE ROW LEVEL SECURITY is safe
    -- to run multiple times).
    ALTER TABLE "public"."site_error_config" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_error_config" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_error_config'
          AND policyname = 'site_error_config_tenant_isolation'
    ) THEN
        CREATE POLICY "site_error_config_tenant_isolation" ON "public"."site_error_config"
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
          AND tablename  = 'site_error_config'
          AND policyname = 'site_error_config_agent'
    ) THEN
        CREATE POLICY "site_error_config_agent" ON "public"."site_error_config"
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;
