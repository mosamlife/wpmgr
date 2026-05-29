-- ADR-037 Sprint 2 — site Health tab + PHP error monitor.
--
-- Two tables:
--   * agent_diagnostics — one row per (site, category) holding the LATEST
--     14-category-collector blob the agent shipped. The handler upserts on
--     (tenant_id, site_id, category); UI reads "latest" by tenant+site.
--   * agent_php_errors  — one row per (site, md5) for fingerprint-deduped
--     PHP errors. Agents heartbeat batches of up to 50 newest rows above a
--     per-site cursor; the CP upserts by md5 and increments occurrence_count.
--
-- RLS mirrors the M4 backup_snapshots pattern: tenant isolation + an agent
-- SELECT-only escape for cross-tenant maintenance.
--
-- Privacy: agent_diagnostics.payload is a JSONB blob and the agent marks
-- certain fields (admin_email, from_address, individual user_emails) SENSITIVE.
-- The operator-facing GET handler is the place to redact / RLS-gate those at
-- read time; this migration only stores what the agent shipped.

CREATE TABLE "public"."agent_diagnostics" (
    "id"           uuid NOT NULL DEFAULT gen_random_uuid(),
    "tenant_id"    uuid NOT NULL,
    "site_id"      uuid NOT NULL,
    -- One of: identity / php / mysql / filesystem / http / cron / themes /
    -- plugins / users / security / https / mail / performance / hosting
    "category"     text NOT NULL,
    "payload"      jsonb NOT NULL DEFAULT '{}'::jsonb,
    "collected_at" timestamptz NOT NULL DEFAULT now(),
    "received_at"  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY ("id"),
    CONSTRAINT "agent_diagnostics_tenant_id_fkey" FOREIGN KEY ("tenant_id")
        REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
    CONSTRAINT "agent_diagnostics_site_id_fkey" FOREIGN KEY ("site_id")
        REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);

-- One row per (site, category) — the latest payload wins. UPSERT in the
-- agent ingestion handler runs `ON CONFLICT (tenant_id, site_id, category)
-- DO UPDATE SET payload = excluded.payload, ...`.
CREATE UNIQUE INDEX "agent_diagnostics_site_category_idx"
    ON "public"."agent_diagnostics" ("tenant_id", "site_id", "category");

CREATE INDEX "agent_diagnostics_received_idx"
    ON "public"."agent_diagnostics" ("tenant_id", "received_at" DESC);

CREATE TABLE "public"."agent_php_errors" (
    "id"               uuid NOT NULL DEFAULT gen_random_uuid(),
    "tenant_id"        uuid NOT NULL,
    "site_id"          uuid NOT NULL,
    -- md5(code:file:line:message) — the agent-side dedup fingerprint.
    "md5"              text NOT NULL,
    "code"             integer NOT NULL,
    "severity"         text NOT NULL DEFAULT 'warning',
    "message"          text NOT NULL,
    "file"             text NOT NULL DEFAULT '',
    "line"             integer NOT NULL DEFAULT 0,
    "request_path"     text NOT NULL DEFAULT '',
    -- Agent-supplied first_seen / last_seen timestamps (Unix seconds → tz).
    "first_seen_at"    timestamptz NOT NULL DEFAULT now(),
    "last_seen_at"     timestamptz NOT NULL DEFAULT now(),
    "occurrence_count" bigint NOT NULL DEFAULT 1,
    "silenced"         boolean NOT NULL DEFAULT false,
    "created_at"       timestamptz NOT NULL DEFAULT now(),
    "updated_at"       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY ("id"),
    CONSTRAINT "agent_php_errors_tenant_id_fkey" FOREIGN KEY ("tenant_id")
        REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
    CONSTRAINT "agent_php_errors_site_id_fkey" FOREIGN KEY ("site_id")
        REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);

-- One row per (site, md5) — agent-side dedup carries through to the CP.
CREATE UNIQUE INDEX "agent_php_errors_site_md5_idx"
    ON "public"."agent_php_errors" ("tenant_id", "site_id", "md5");

CREATE INDEX "agent_php_errors_site_lastseen_idx"
    ON "public"."agent_php_errors" ("tenant_id", "site_id", "last_seen_at" DESC);

CREATE INDEX "agent_php_errors_silenced_idx"
    ON "public"."agent_php_errors" ("tenant_id", "site_id")
    WHERE silenced = false;

-- ---------------------------------------------------------------------------
-- Row-Level Security (ADR-002: hand-appended; Atlas CE cannot diff policies).
-- ---------------------------------------------------------------------------
ALTER TABLE "public"."agent_diagnostics" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."agent_diagnostics" FORCE ROW LEVEL SECURITY;

CREATE POLICY "agent_diagnostics_tenant_isolation" ON "public"."agent_diagnostics"
    USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- Agent ingestion writes under the agent GUC (the agent path resolves the
-- tenant from the verified Ed25519 identity, so the WITH CHECK clause is
-- safe — the handler sets app.tenant_id before INSERT).
CREATE POLICY "agent_diagnostics_agent" ON "public"."agent_diagnostics"
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

ALTER TABLE "public"."agent_php_errors" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."agent_php_errors" FORCE ROW LEVEL SECURITY;

CREATE POLICY "agent_php_errors_tenant_isolation" ON "public"."agent_php_errors"
    USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY "agent_php_errors_agent" ON "public"."agent_php_errors"
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');
