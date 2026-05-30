-- S2 Login Protection + IP store.
--
-- Adds two tables:
--   site_security_config — per-site login-protection mode, thresholds, IP
--                          header selection, and CIDR allow/deny lists. Holds
--                          at most one row per site (site_id is PRIMARY KEY).
--                          The CP pushes this config to the agent via the
--                          signed `sync_security_config` command on every save.
--   agent_login_events   — time-series of login attempts shipped by the agent.
--                          Deduped by (tenant_id, site_id, agent_event_id);
--                          ordered by occurred_at DESC for the operator UI.
--
-- RLS mirrors the M8/M12 pattern: tenant isolation + an agent-write policy so
-- both InTenantTx (operator) and InTenantTxAsAgent (ingest) work correctly.
-- Idempotency: every statement is guarded with IF NOT EXISTS / IF NOT EXISTS
-- checks so running this migration twice is safe.

-- ---------------------------------------------------------------------------
-- site_security_config
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."site_security_config" (
        "tenant_id"    uuid NOT NULL,
        "site_id"      uuid NOT NULL,
        -- mode controls what the agent does with login attempts:
        --   "disabled" — no login protection active.
        --   "audit"    — record events but do not block.
        --   "protect"  — record events AND block based on thresholds.
        "mode"         text NOT NULL DEFAULT 'protect',
        -- thresholds is a JSONB map that the agent uses to decide when to
        -- challenge, temporarily block, or permanently block an IP.
        -- Default matches the agent's built-in defaults.
        "thresholds"   jsonb NOT NULL DEFAULT '{"captcha_limit":3,"temp_block_limit":10,"block_all_limit":100,"failed_login_gap":1800,"success_login_gap":1800,"all_blocked_gap":1800}',
        -- ip_header is the HTTP header the agent reads to extract the real
        -- client IP (e.g. "REMOTE_ADDR", "HTTP_X_FORWARDED_FOR").
        "ip_header"    text NOT NULL DEFAULT 'REMOTE_ADDR',
        -- allow_cidrs / deny_cidrs are CIDR strings the agent applies before
        -- threshold evaluation. Stored as text[] for simple equality queries.
        "allow_cidrs"  text[] NOT NULL DEFAULT '{}',
        "deny_cidrs"   text[] NOT NULL DEFAULT '{}',
        "updated_at"   timestamptz NOT NULL DEFAULT now(),
        PRIMARY KEY ("site_id"),
        CONSTRAINT "site_security_config_tenant_id_fkey" FOREIGN KEY ("tenant_id")
            REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "site_security_config_site_id_fkey" FOREIGN KEY ("site_id")
            REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
    );
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'site_security_config'
          AND indexname  = 'site_security_config_tenant_idx'
    ) THEN
        CREATE INDEX "site_security_config_tenant_idx"
            ON "public"."site_security_config" ("tenant_id");
    END IF;
END;
$$;

-- Row-Level Security for site_security_config.
DO $$
BEGIN
    ALTER TABLE "public"."site_security_config" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_security_config" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_security_config'
          AND policyname = 'site_security_config_tenant_isolation'
    ) THEN
        CREATE POLICY "site_security_config_tenant_isolation" ON "public"."site_security_config"
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
          AND tablename  = 'site_security_config'
          AND policyname = 'site_security_config_agent'
    ) THEN
        CREATE POLICY "site_security_config_agent" ON "public"."site_security_config"
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- agent_login_events
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."agent_login_events" (
        "id"             bigserial PRIMARY KEY,
        "tenant_id"      uuid NOT NULL,
        "site_id"        uuid NOT NULL,
        -- agent_event_id is the agent's local row id for dedup cursor tracking.
        "agent_event_id" bigint NOT NULL,
        "ip"             text,
        -- status: 1=failure, 2=success, 3=blocked.
        "status"         smallint,
        "category"       text,
        "username"       text,
        "request_id"     text,
        "occurred_at"    timestamptz,
        "ingested_at"    timestamptz NOT NULL DEFAULT now(),
        CONSTRAINT "agent_login_events_tenant_id_fkey" FOREIGN KEY ("tenant_id")
            REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "agent_login_events_site_id_fkey" FOREIGN KEY ("site_id")
            REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
    );
END;
$$;

-- Time-series index: serves the operator login-events list ordered by time.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'agent_login_events'
          AND indexname  = 'agent_login_events_tenant_site_time_idx'
    ) THEN
        CREATE INDEX "agent_login_events_tenant_site_time_idx"
            ON "public"."agent_login_events" ("tenant_id", "site_id", "occurred_at" DESC);
    END IF;
END;
$$;

-- Dedup unique: one row per (tenant, site, agent_event_id).
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'agent_login_events'
          AND indexname  = 'agent_login_events_dedup_idx'
    ) THEN
        CREATE UNIQUE INDEX "agent_login_events_dedup_idx"
            ON "public"."agent_login_events" ("tenant_id", "site_id", "agent_event_id");
    END IF;
END;
$$;

-- Row-Level Security for agent_login_events.
DO $$
BEGIN
    ALTER TABLE "public"."agent_login_events" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."agent_login_events" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'agent_login_events'
          AND policyname = 'agent_login_events_tenant_isolation'
    ) THEN
        CREATE POLICY "agent_login_events_tenant_isolation" ON "public"."agent_login_events"
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
          AND tablename  = 'agent_login_events'
          AND policyname = 'agent_login_events_agent'
    ) THEN
        CREATE POLICY "agent_login_events_agent" ON "public"."agent_login_events"
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;
