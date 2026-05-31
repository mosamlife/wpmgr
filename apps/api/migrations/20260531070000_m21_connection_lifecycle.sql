-- M21 — Connection lifecycle (Phase 5.7).
--
-- Adds the connection-state machine + supporting tables for live enrollment and
-- the enroll → connect → degraded → disconnected → revoked → archived → re-enroll
-- lifecycle. See ADR-038 (SSE bus), ADR-039 (heartbeat/timeouts), ADR-040
-- (last-will), ADR-041 (state model + re-enrollment identity).
--
-- Everything is ADDITIVE + idempotent (IF NOT EXISTS). The legacy free-text
-- `sites.status` / `sites.health_status` columns are intentionally KEPT and still
-- written (mapped from connection_state) for backward-compat; the UI reads only
-- connection_state. Runs in ONE transaction.

-- ---------------------------------------------------------------------------
-- 1. sites — connection-state columns (single source of truth)
-- ---------------------------------------------------------------------------
ALTER TABLE sites
    ADD COLUMN IF NOT EXISTS connection_state text NOT NULL DEFAULT 'pending_enrollment'
        CHECK (connection_state IN
            ('pending_enrollment','connected','degraded','disconnected','revoked','archived'));
ALTER TABLE sites ADD COLUMN IF NOT EXISTS connection_generation integer NOT NULL DEFAULT 0;
ALTER TABLE sites ADD COLUMN IF NOT EXISTS disconnected_at      timestamptz;
ALTER TABLE sites ADD COLUMN IF NOT EXISTS disconnected_reason  text;
ALTER TABLE sites ADD COLUMN IF NOT EXISTS archived_at          timestamptz;

-- One-time backfill from the legacy status: an already-enrolled (status='active')
-- site is 'connected'; everything else stays 'pending_enrollment'. Idempotent:
-- after the first run those rows are 'connected', so the WHERE no longer matches.
UPDATE sites SET connection_state = 'connected'
    WHERE connection_state = 'pending_enrollment' AND status = 'active';

CREATE INDEX IF NOT EXISTS idx_sites_connection_state
    ON sites (tenant_id, connection_state);
CREATE INDEX IF NOT EXISTS idx_sites_last_seen
    ON sites (last_seen_at) WHERE connection_state IN ('connected','degraded');

-- ---------------------------------------------------------------------------
-- 2. pairing_codes — bind a code to a specific site (site-first + re-enroll)
-- ---------------------------------------------------------------------------
-- NULL site_id  = legacy tenant-scoped "create the site at enroll time" flow.
-- Set  site_id  = the row already exists in pending_enrollment; consuming the
--                 code transitions THAT site to connected (the live-enroll flow).
ALTER TABLE pairing_codes
    ADD COLUMN IF NOT EXISTS site_id uuid REFERENCES sites (id) ON DELETE CASCADE;
ALTER TABLE pairing_codes
    ADD COLUMN IF NOT EXISTS consumed_from_ip inet;
CREATE INDEX IF NOT EXISTS idx_pairing_codes_site
    ON pairing_codes (site_id) WHERE site_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- 3. site_connection_history — append-only transition log
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS site_connection_history (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    site_id       uuid        NOT NULL REFERENCES sites (id) ON DELETE CASCADE,
    from_state    text        NOT NULL,
    to_state      text        NOT NULL,
    reason        text,
    actor_user_id uuid        REFERENCES users (id) ON DELETE SET NULL,
    generation    integer     NOT NULL DEFAULT 0,
    occurred_at   timestamptz NOT NULL DEFAULT now(),
    metadata      jsonb       NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX IF NOT EXISTS idx_conn_history_site
    ON site_connection_history (site_id, occurred_at DESC);

ALTER TABLE site_connection_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_connection_history FORCE ROW LEVEL SECURITY;
CREATE POLICY conn_history_tenant_isolation ON site_connection_history
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- 4. site_events — durable SSE journal (LISTEN/NOTIFY fan-out + ~5min replay)
-- ---------------------------------------------------------------------------
-- event_id is a ULID (text, lexicographically sortable, monotonic per tenant)
-- minted by the app. NOTIFY carries only '<tenant_id>:<event_id>'; instances
-- read the body here (under the notify tenant's scope) and fan out + replay.
CREATE TABLE IF NOT EXISTS site_events (
    event_id   text        PRIMARY KEY,
    tenant_id  uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    site_id    uuid,        -- nullable: some events are tenant-level (e.g. created)
    type       text        NOT NULL,
    data       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_site_events_tenant ON site_events (tenant_id, event_id);
CREATE INDEX IF NOT EXISTS idx_site_events_created ON site_events (created_at);

ALTER TABLE site_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_events FORCE ROW LEVEL SECURITY;
CREATE POLICY site_events_tenant_isolation ON site_events
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
