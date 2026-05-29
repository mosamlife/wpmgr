-- ADR-037 Sprint 3 — WordPress activity log (tamper-evident, hash-chained).
--
-- The agent ships a hash-chained event stream to POST /agent/v1/activity. The
-- CP re-verifies the chain at ingest and flags any tamper as chain_valid=false
-- on the affected row (leapfrog: neither WPvivid nor WPRemote ships chain
-- integrity). High-severity events route into the EXISTING uptime alert
-- Dispatcher when the tenant's alert_configs.notify_security flag is on.
--
-- RLS mirrors the M4 backup_snapshots / M8 diagnostics pattern: tenant
-- isolation + an agent escape under app.agent='on'. The tenant-scoped agent
-- ingest path passes the tenant_isolation WITH CHECK (it sets app.tenant_id);
-- the agent policy is the additional permissive escape for maintenance.

ALTER TABLE "public"."alert_configs"
    ADD COLUMN "notify_security" boolean NOT NULL DEFAULT false;

CREATE TABLE "public"."agent_activity_log" (
    "id"            bigserial    NOT NULL,
    "tenant_id"     uuid         NOT NULL,
    "site_id"       uuid         NOT NULL,
    -- Agent-assigned monotonic sequence within the (tenant, site) chain.
    "seq"           bigint       NOT NULL,
    "event_type"    text         NOT NULL,
    "object_type"   text         NOT NULL,
    "object_id"     text         NOT NULL DEFAULT '',
    "object_label"  text         NOT NULL DEFAULT '',
    "actor_user_id" bigint       NOT NULL DEFAULT 0,
    "actor_login"   text         NOT NULL DEFAULT '',
    "actor_ip"      text         NOT NULL DEFAULT '',
    "summary"       text         NOT NULL DEFAULT '',
    "meta"          jsonb,
    -- Verbatim agent-serialized meta bytes — the EXACT preimage the agent
    -- hashed (wp_json_encode key order + slash/unicode escaping). Chain
    -- re-verification hashes THIS, not the jsonb "meta" column: Postgres JSONB
    -- normalizes key order + whitespace and Go's json.Marshal sorts keys +
    -- HTML-escapes, either of which diverges from PHP's encoding and would
    -- false-flag every multi-key event (e.g. {version,severity}) as a break.
    "meta_raw"      text         NOT NULL DEFAULT '{}',
    -- Extracted from meta.severity (high|medium|low); drives the alert decision.
    "severity"      text         NOT NULL DEFAULT 'low',
    -- Hash chain (see SHARED WIRE CONTRACT). prev_hash of the first event is 64
    -- zero chars; this_hash = sha256(canonical preimage). chain_valid is set by
    -- the CP's server-side re-verification at ingest.
    "prev_hash"     text         NOT NULL,
    "this_hash"     text         NOT NULL,
    "chain_valid"   boolean      NOT NULL DEFAULT true,
    "occurred_at"   timestamptz  NOT NULL,
    "received_at"   timestamptz  NOT NULL DEFAULT now(),
    PRIMARY KEY ("id"),
    CONSTRAINT "agent_activity_log_tenant_id_fkey" FOREIGN KEY ("tenant_id")
        REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
    CONSTRAINT "agent_activity_log_site_id_fkey" FOREIGN KEY ("site_id")
        REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
    -- Idempotent agent retry: one row per (tenant, site, seq).
    CONSTRAINT "agent_activity_log_tenant_site_seq_key" UNIQUE ("tenant_id", "site_id", "seq")
);

-- Newest-first listing per site (the operator's default view).
CREATE INDEX "activity_site_occurred_idx"
    ON "public"."agent_activity_log" ("site_id", "occurred_at" DESC);

-- Partial index for the high-severity security feed (the alert-worthy slice).
CREATE INDEX "activity_site_severity_idx"
    ON "public"."agent_activity_log" ("site_id", "severity")
    WHERE severity = 'high';

-- ---------------------------------------------------------------------------
-- Row-Level Security (ADR-002: hand-appended; Atlas CE cannot diff policies).
-- ---------------------------------------------------------------------------
ALTER TABLE "public"."agent_activity_log" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."agent_activity_log" FORCE ROW LEVEL SECURITY;

CREATE POLICY "agent_activity_log_tenant_isolation" ON "public"."agent_activity_log"
    USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- Agent ingestion runs under app.agent (the agent path resolves the tenant from
-- the verified Ed25519 identity before INSERT). SELECT-only escape for
-- cross-tenant maintenance, mirroring the M8 diagnostics policy.
CREATE POLICY "agent_activity_log_agent" ON "public"."agent_activity_log"
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');
