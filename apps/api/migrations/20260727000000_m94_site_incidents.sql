-- M94 — GH #148 (part 1): persisted incident history for the fleet incident-
-- detail feature.
--
-- Problem: site_alert_state (M5) stores only the CURRENT transition memory
-- (last_status/consecutive_down/in_incident/last_alert_at) — a single row per
-- site with no history. GetFleetIncidents therefore could only report the
-- ONE open incident (if any) plus a same-shape row for "recently alerted"
-- sites, and closed-incident ended_at/duration_seconds were ESTIMATES derived
-- from site_alert_state.updated_at, not a true incident-close record. There
-- was also no way to open a per-incident detail view (no stable incident id,
-- no persisted probe/flapping history per incident).
--
-- Fix: site_incidents is a new, append-style table — one row PER INCIDENT
-- (open or closed), keyed by its own uuid id, so the fleet incidents list and
-- the new GET /api/v1/fleet/incidents/:incidentId detail endpoint can both
-- read real history instead of estimating it from a single mutable row.
-- site_alert_state is UNCHANGED and continues to own the transition
-- de-dupe/evaluator logic (see internal/uptime/alerts.go Evaluate) — this
-- table is written ALONGSIDE it, inside the same TransitionAlertState
-- transaction (internal/uptime/repo.go), never instead of it.
--
-- Columns:
--   ended_at IS NULL      == the incident is open/ongoing (mirrors
--                            backup_snapshots' pending/running convention).
--   peak_status            reserved for a future "degraded vs down" incident
--                            severity distinction; the alert state machine
--                            only ever opens a 'down' incident today (see
--                            FleetIncidentItem.Kind's doc comment), so this is
--                            always 'down' for now.
--   opened_by               'probe' for every incident the ProbeWorker opens
--                            (the overwhelming common case) vs 'seed' for the
--                            one-time day-1 backfill below — lets a future
--                            audit distinguish real detections from the
--                            migration's own bootstrap rows.
--   probe_count/down_count  reserved counters for a future rollup (NOT
--                            populated by this change — see M94 rollout
--                            notes); default 0 keeps them well-defined.
--
-- tenant_id is denormalized onto this table (rather than joined through
-- sites) for the SAME reason site_alert_state carries it: a join-free RLS
-- policy and join-free tenant-scoped queries, mirroring that table exactly.
--
-- site_incidents_one_open_per_site is the race-safety guard: at most one OPEN
-- incident (ended_at IS NULL) per site, modeled on
-- backup_snapshots_one_inflight_per_site (m75) — the state machine only ever
-- transitions one site's alert state under a row lock (GetSiteAlertStateForUpdate
-- FOR UPDATE, see repo.go), so this is belt-and-suspenders rather than the
-- primary defense, but it makes a double-open a hard DB-level impossibility
-- instead of merely an application-logic invariant.
--
-- Idempotent throughout (DO $$ ... IF NOT EXISTS ... $$), mirrors m39/m93.

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."site_incidents" (
        "id"               uuid        NOT NULL DEFAULT gen_random_uuid(),
        "tenant_id"        uuid        NOT NULL,
        "site_id"          uuid        NOT NULL,
        "started_at"       timestamptz NOT NULL DEFAULT now(),
        "ended_at"         timestamptz,
        "peak_status"      text        NOT NULL DEFAULT 'down',
        "last_http_status" integer     NOT NULL DEFAULT 0,
        "probe_count"      integer     NOT NULL DEFAULT 0,
        "down_count"       integer     NOT NULL DEFAULT 0,
        "opened_by"        text        NOT NULL DEFAULT 'probe',
        "reason"           text        NOT NULL DEFAULT '',
        "created_at"       timestamptz NOT NULL DEFAULT now(),
        "updated_at"       timestamptz NOT NULL DEFAULT now(),
        PRIMARY KEY ("id"),
        CONSTRAINT "site_incidents_tenant_id_fkey" FOREIGN KEY ("tenant_id")
            REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "site_incidents_site_id_fkey" FOREIGN KEY ("site_id")
            REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
    );
END;
$$;

-- Per-site incident history, newest first (the detail/list read paths).
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'site_incidents'
          AND indexname  = 'site_incidents_site_started_idx'
    ) THEN
        CREATE INDEX "site_incidents_site_started_idx"
            ON "public"."site_incidents" ("site_id", "started_at" DESC);
    END IF;
END;
$$;

-- Tenant-wide incident history (fleet incidents list).
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'site_incidents'
          AND indexname  = 'site_incidents_tenant_started_idx'
    ) THEN
        CREATE INDEX "site_incidents_tenant_started_idx"
            ON "public"."site_incidents" ("tenant_id", "started_at" DESC);
    END IF;
END;
$$;

-- Race-safety guard: at most one OPEN incident per site (m75
-- backup_snapshots_one_inflight_per_site precedent). CREATE UNIQUE INDEX
-- supports IF NOT EXISTS directly (unlike CREATE POLICY below).
CREATE UNIQUE INDEX IF NOT EXISTS "site_incidents_one_open_per_site"
    ON "public"."site_incidents" ("site_id")
    WHERE "ended_at" IS NULL;

-- ---------------------------------------------------------------------------
-- Row-Level Security (mirrors site_alert_state exactly: tenant isolation +
-- an app.agent policy for the probe worker's cross-tenant writes + the m19
-- AS RESTRICTIVE site_scope policy every direct site-keyed table carries).
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    ALTER TABLE "public"."site_incidents" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_incidents" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_incidents'
          AND policyname = 'site_incidents_tenant_isolation'
    ) THEN
        CREATE POLICY "site_incidents_tenant_isolation" ON "public"."site_incidents"
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
          AND tablename  = 'site_incidents'
          AND policyname = 'site_incidents_agent'
    ) THEN
        CREATE POLICY "site_incidents_agent" ON "public"."site_incidents"
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;

-- site_scope (security review Finding 1, GH #148): mirrors m19's
-- site_alert_state_site_scope verbatim (20260531050000_m19_orgs_sharing.sql,
-- section 5g — "PK = site_id; use site_id directly"). site_id is a direct
-- column on site_incidents (same shape as site_alert_state), so the
-- RESTRICTIVE predicate uses it directly, no indirect join needed. This is
-- AND-combined with every permissive policy above: when app.site_scope is NOT
-- 'on' (the overwhelming default — normal members, service/agent paths) the
-- first branch is a tautology and the policy is a no-op; when app.site_scope
-- IS 'on' (the InScopedTenantTx collaborator path), only rows whose site_id
-- is in app.allowed_site_ids pass. Not exploitable today — every current
-- read of this table uses InTenantTx/InAgentTx, neither of which sets
-- app.site_scope, and collaborator scoping is enforced in application code
-- via Principal.CanAccessSite/AllowedSiteIDs — but RLS is the last line of
-- defense, not the only one: the day a future read path uses
-- InScopedTenantTx against this table, this policy is what keeps a
-- site-scoped collaborator from seeing another site's incidents even if the
-- application-code check is ever missed.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_incidents'
          AND policyname = 'site_incidents_site_scope'
    ) THEN
        CREATE POLICY "site_incidents_site_scope" ON "public"."site_incidents"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- Day-1 seed: adopt every CURRENTLY-open incident (as tracked by
-- site_alert_state) as an open site_incidents row, so a site already down at
-- deploy time shows up in the new incidents list/detail immediately instead
-- of waiting for its next down->recovery transition. started_at falls back
-- to the alert state's last_alert_at (the timestamp the ORIGINAL down alert
-- fired) rather than now(), so the incident's displayed duration is accurate
-- from the moment this migration runs, not reset to zero.
--
-- ON CONFLICT targets the partial unique index above; safe to re-run — a
-- second application of this migration (or the repo's own "adopt" path in
-- TransitionAlertState, see internal/uptime/repo.go) is a silent no-op once a
-- row is already open for that site.
INSERT INTO "public"."site_incidents"
    ("tenant_id", "site_id", "started_at", "ended_at", "peak_status", "reason", "opened_by")
SELECT
    "tenant_id",
    "site_id",
    COALESCE("last_alert_at", now()),
    NULL,
    'down',
    '',
    'seed'
FROM "public"."site_alert_state"
WHERE "in_incident" = true
ON CONFLICT ("site_id") WHERE "ended_at" IS NULL DO NOTHING;
