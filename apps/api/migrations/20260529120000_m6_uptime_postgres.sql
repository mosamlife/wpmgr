-- M6 uptime monitoring on Postgres: one row per probe in site_uptime_probes.
-- ----------------------------------------------------------------------------
-- Background. M5 wrote uptime check time-series to ClickHouse (ADR-028) and
-- left a no-op fallback when WPMGR_CLICKHOUSE_ADDR was empty. The GCP-managed
-- deployment does not run a ClickHouse cluster, so the disabled fallback fired
-- in production and the dashboard had no status, no graph, and no cert data.
--
-- M6 keeps ClickHouse available for high-volume deployments but adds Postgres
-- as the default backend. At WPMgr's expected scale (≤100 sites × ~1 probe/60s
-- → ≤5M rows/year) Postgres comfortably handles the write rate and the
-- aggregate/series queries with a (site_id, probed_at DESC) index for the
-- LIMIT-1 latest read and the windowed bucket scans.
--
-- The metrics store interface is unchanged: the probe worker writes one row
-- per probe via metrics.Store.InsertChecks(). On Postgres each row carries the
-- full timing breakdown PLUS the leaf TLS certificate (issuer + subject +
-- not_after) so the cert section of the dashboard surfaces issuer/expiry from
-- the latest probe row — no separate cert-collection job needed.
--
-- RLS. The probe worker iterates every enrolled site cross-tenant under the
-- app.agent GUC (same pattern as M5 site_alert_state), so the writer path
-- runs under a permissive app.agent policy. Per-tenant reads from the API run
-- through uptime.Service which verifies tenant ownership of the site in
-- Postgres BEFORE issuing any metrics query; the query itself is gated by an
-- explicit tenant_id parameter and the same app.agent policy (the metrics
-- read path also runs under InAgentTx — we filter by tenant_id at the SQL
-- level and tenant verification has already happened in the service).

CREATE TABLE IF NOT EXISTS "public"."site_uptime_probes" (
    "id"           uuid        NOT NULL DEFAULT gen_random_uuid(),
    "tenant_id"    uuid        NOT NULL,
    "site_id"      uuid        NOT NULL,
    "probed_at"    timestamptz NOT NULL DEFAULT now(),
    "up"           boolean     NOT NULL,
    "http_status"  integer     NOT NULL DEFAULT 0,
    "dns_ms"       double precision NOT NULL DEFAULT 0,
    "connect_ms"   double precision NOT NULL DEFAULT 0,
    "tls_ms"       double precision NOT NULL DEFAULT 0,
    "ttfb_ms"      double precision NOT NULL DEFAULT 0,
    "total_ms"     double precision NOT NULL DEFAULT 0,
    "tls_expiry"   timestamptz NULL,
    "tls_issuer"   text        NOT NULL DEFAULT '',
    "tls_subject"  text        NOT NULL DEFAULT '',
    "error_text"   text        NOT NULL DEFAULT '',
    PRIMARY KEY ("id"),
    CONSTRAINT "site_uptime_probes_tenant_fkey" FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id") ON DELETE CASCADE,
    CONSTRAINT "site_uptime_probes_site_fkey"   FOREIGN KEY ("site_id")   REFERENCES "public"."sites"   ("id") ON DELETE CASCADE
);

-- Latest-probe lookup (per site) is a single index seek with this composite.
CREATE INDEX IF NOT EXISTS "site_uptime_probes_site_time_idx"
    ON "public"."site_uptime_probes" ("site_id", "probed_at" DESC);

-- Cross-tenant scans by the summary endpoint (rare; fall back to seq scan if
-- the partial index isn't selective at small data volumes).
CREATE INDEX IF NOT EXISTS "site_uptime_probes_tenant_time_idx"
    ON "public"."site_uptime_probes" ("tenant_id", "probed_at" DESC);

-- ---------------------------------------------------------------------------
-- Row-Level Security.
-- ---------------------------------------------------------------------------
ALTER TABLE "public"."site_uptime_probes" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."site_uptime_probes" FORCE ROW LEVEL SECURITY;

-- Tenant-scoped read for whatever operator-side path inadvertently reads under
-- app.tenant_id (none today — the metrics path runs under app.agent — but the
-- policy guarantees tenant isolation if a future code path does).
CREATE POLICY "site_uptime_probes_tenant_isolation" ON "public"."site_uptime_probes"
    USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- The probe worker writes cross-tenant under app.agent (it sweeps every
-- enrolled site in one job), and the metrics-read path also runs under
-- app.agent with an explicit tenant_id filter at the SQL level. Mirrors the
-- M5 site_alert_state_agent pattern.
CREATE POLICY "site_uptime_probes_agent" ON "public"."site_uptime_probes"
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');
