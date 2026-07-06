-- M91 — hosted-billing entitlement substrate (M16 Phase A).
--
-- Ships DARK behind WPMGR_HOSTED (default false): self-host and current prod
-- see ZERO behavior change until the flag is turned on. Provider-agnostic —
-- no payment-SDK naming anywhere in this schema; billing_provider /
-- provider_customer_id / provider_subscription_id are generic so any future
-- provider can be wired in Phase B without another schema change.
--
-- tenants (NOTE: tenants carries NO RLS — it is the billable root, gated by
-- membership/ownership checks in the handler, not a row-level policy) gains:
--   plan                     — the tier: free/starter/agency/scale (default
--                              'free', so every existing tenant becomes free
--                              at cutover — see the grandfather backfill below).
--   plan_status              — none/trialing/active/past_due/canceled/paused/
--                              comped (default 'none' — a tenant that has
--                              never seen a billing event is simply free, per
--                              internal/billing's status gate).
--   plan_overrides           — jsonb per-key delta overlay (e.g.
--                              {"max_sites": 25}), resolved on top of the plan
--                              ladder by internal/billing.Entitlements.
--   grace_until              — a past_due tenant keeps paid limits until this
--                              instant (internal/billing's status gate), then
--                              falls back to free.
--   billing_provider / provider_customer_id / provider_subscription_id /
--   current_period_end       — generic Phase-B webhook-consumer columns.
--
-- billing_events is the Phase-B webhook/event ledger, created now so the
-- ingestion path has a home the moment a provider is wired: UNIQUE(provider,
-- provider_event_id) makes a replayed webhook a no-op insert. tenant_id is
-- nullable because a provider event may arrive before it can be matched to a
-- tenant (e.g. an unrecognized customer id).
--
-- RLS (security review Finding C): billing_events gets the standard
-- tenant/system pairing already used by site_events and sites (m36 pattern):
--   * billing_events_tenant_isolation scopes any future tenant-facing read
--     (e.g. an operator billing-history view) to app.tenant_id, exactly like
--     every other tenant table — a NULL tenant_id row never matches this
--     policy, so an unmatched event is invisible to every tenant.
--   * billing_events_system is the write path for the Phase-B webhook
--     consumer, which processes events across many tenants in one pass (and
--     for events that arrive before a tenant match exists) — it is NOT a
--     single tenant's request scope, so it runs under InAgentTx (app.agent=
--     'on'), the same cross-tenant GUC every other system/worker write uses
--     (see sites_agent, site_events_agent). No new GUC is introduced.
-- Both ENABLE + FORCE so the table owner is also subject to RLS.
--
-- billing_count_active_sites is the SECURITY DEFINER site-cap counter used by
-- internal/billing.CheckSiteCreate — see its own comment below (mirrored in
-- db/schema.sql) for the full RLS-correctness rationale. "Active" mirrors the
-- sites-list default filter exactly: connection_state <> 'archived' (ADR-041).
--
-- Security review Finding A: both SECURITY DEFINER functions below save and
-- restore the app.agent GUC around their in-body set_config, rather than
-- setting it and returning. Postgres does NOT roll back an in-body
-- set_config(..., true) at function exit — the "true" (is_local) flag scopes
-- the change to the CALLER's transaction, not to the function invocation —
-- so an unrestored set_config('app.agent','on',true) leaks 'on' into the rest
-- of the caller's transaction, silently disabling RLS's tenant check for
-- every statement that follows in that same tx. admin_delete_empty_tenant
-- (m35) is also corrected here (CREATE OR REPLACE, same function name) since
-- m35 already ran in prod and editing that historical file would not
-- re-apply.
--
-- Grandfather backfill (non-destructive prime directive): every tenant
-- defaults to plan='free' (cap 3) the instant this migration lands. Any
-- tenant already operating more than 3 non-archived sites would otherwise
-- become silently over-cap; instead we write plan_overrides.max_sites = their
-- current count so nobody loses capability — only NEW growth is capped once
-- WPMGR_HOSTED is actually turned on. The guard
-- (NOT plan_overrides ? 'max_sites') makes this idempotent: a second run (or
-- a fresh apply against a re-migrated dev DB) is a no-op once the override is
-- set. The literal "3" mirrors internal/billing's free-tier MaxSites; if that
-- constant ever changes it does NOT retroactively change an already
-- backfilled override — grandfathering is a point-in-time snapshot, not a
-- moving target.
--
-- Idempotent throughout: ADD COLUMN/CHECK use IF NOT EXISTS / DROP+ADD guards
-- (mirrors the project's established style, e.g. m87), safe to re-run.

ALTER TABLE "public"."tenants" ADD COLUMN IF NOT EXISTS "plan" text NOT NULL DEFAULT 'free';
ALTER TABLE "public"."tenants" ADD COLUMN IF NOT EXISTS "plan_status" text NOT NULL DEFAULT 'none';
ALTER TABLE "public"."tenants" ADD COLUMN IF NOT EXISTS "plan_overrides" jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE "public"."tenants" ADD COLUMN IF NOT EXISTS "grace_until" timestamptz;
ALTER TABLE "public"."tenants" ADD COLUMN IF NOT EXISTS "billing_provider" text;
ALTER TABLE "public"."tenants" ADD COLUMN IF NOT EXISTS "provider_customer_id" text;
ALTER TABLE "public"."tenants" ADD COLUMN IF NOT EXISTS "provider_subscription_id" text;
ALTER TABLE "public"."tenants" ADD COLUMN IF NOT EXISTS "current_period_end" timestamptz;

ALTER TABLE "public"."tenants" DROP CONSTRAINT IF EXISTS "tenants_plan_check";
ALTER TABLE "public"."tenants"
    ADD CONSTRAINT "tenants_plan_check" CHECK (plan IN ('free', 'starter', 'agency', 'scale'));

ALTER TABLE "public"."tenants" DROP CONSTRAINT IF EXISTS "tenants_plan_status_check";
ALTER TABLE "public"."tenants"
    ADD CONSTRAINT "tenants_plan_status_check"
    CHECK (plan_status IN ('none', 'trialing', 'active', 'past_due', 'canceled', 'paused', 'comped'));

CREATE TABLE IF NOT EXISTS "public"."billing_events" (
    "id"                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    "provider"          text        NOT NULL,
    "provider_event_id" text        NOT NULL,
    "kind"              text        NOT NULL,
    "tenant_id"         uuid        REFERENCES "public"."tenants" ("id") ON DELETE CASCADE,
    "payload"           jsonb       NOT NULL DEFAULT '{}'::jsonb,
    "occurred_at"       timestamptz NOT NULL,
    "processed_at"      timestamptz,
    "created_at"        timestamptz NOT NULL DEFAULT now()
);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND indexname = 'billing_events_provider_event_key'
    ) THEN
        CREATE UNIQUE INDEX "billing_events_provider_event_key" ON "public"."billing_events" ("provider", "provider_event_id");
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND indexname = 'billing_events_tenant_id_idx'
    ) THEN
        CREATE INDEX "billing_events_tenant_id_idx" ON "public"."billing_events" ("tenant_id");
    END IF;
END;
$$;

-- Security review Finding C: billing_events RLS. See the top-of-file comment
-- for the tenant/system pairing rationale (mirrors site_events exactly).
ALTER TABLE "public"."billing_events" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."billing_events" FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'billing_events' AND policyname = 'billing_events_tenant_isolation'
    ) THEN
        CREATE POLICY "billing_events_tenant_isolation" ON "public"."billing_events"
            USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'billing_events' AND policyname = 'billing_events_system'
    ) THEN
        CREATE POLICY "billing_events_system" ON "public"."billing_events"
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;

-- billing_count_active_sites: see the matching comment in db/schema.sql for
-- the full SECURITY DEFINER / RLS-correctness rationale.
--
-- Security review Finding A: v_prev captures whatever app.agent was already
-- set to in the CALLER's transaction (typically unset/'' under InTenantTx,
-- but this must be correct under ANY caller context) and restores it before
-- returning, on the ONLY return path. Without this, set_config(...,true)'s
-- in-body write is NOT rolled back at function exit (is_local scopes it to
-- the transaction, not the function call), so 'on' would otherwise persist
-- for the rest of the caller's transaction — e.g. every statement CreatePending
-- and Transition run AFTER this call, in the same site-birth tx, would then
-- see FORCE RLS's app.agent='on' branch instead of the intended app.tenant_id
-- branch, exposing/allowing writes across every tenant for that transaction.
CREATE OR REPLACE FUNCTION billing_count_active_sites(p_tenant uuid)
RETURNS bigint
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_count bigint;
    v_prev text := current_setting('app.agent', true);
BEGIN
    PERFORM set_config('app.agent', 'on', true);
    SELECT count(*) INTO v_count
    FROM sites
    WHERE tenant_id = p_tenant
      AND connection_state <> 'archived';
    PERFORM set_config('app.agent', coalesce(v_prev, ''), true);
    RETURN v_count;
END;
$$;
REVOKE ALL ON FUNCTION billing_count_active_sites(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION billing_count_active_sites(uuid) TO wpmgr_app;

-- Security review Finding A (continued): admin_delete_empty_tenant (m35) has
-- the identical unrestored-app.agent-GUC leak — set_config('app.agent','on',
-- true) at the top of the function body, never restored on either return
-- path (the early "not empty" RETURN false, or the final RETURN after the
-- tenant delete). m35 already ran in prod, so editing that migration file
-- would not re-apply there; this CREATE OR REPLACE (same function name/
-- signature, re-run here as part of m91) is how the fixed body actually
-- reaches prod on next boot. Behavior is otherwise unchanged: still restores
-- app.tenant_id to '' immediately after the scoped audit_log delete (that
-- reset is intentional and unrelated to this fix — the function never nests
-- inside an existing tenant-scoped caller transaction, so blanking rather
-- than restoring app.tenant_id has always been safe here); now additionally
-- saves/restores app.agent around the single RETURN point so FORCE RLS's
-- app.agent='on' branch never survives into the caller's transaction.
CREATE OR REPLACE FUNCTION "public"."admin_delete_empty_tenant"(p_tenant_id uuid)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_count integer;
    v_result boolean := false;
    v_prev_agent text := current_setting('app.agent', true);
BEGIN
    PERFORM set_config('app.agent', 'on', true);
    IF NOT (
        EXISTS (SELECT 1 FROM memberships m WHERE m.tenant_id = p_tenant_id)
        OR EXISTS (SELECT 1 FROM sites s WHERE s.tenant_id = p_tenant_id)
    ) THEN
        PERFORM set_config('app.tenant_id', p_tenant_id::text, true);
        DELETE FROM audit_log WHERE tenant_id = p_tenant_id;
        PERFORM set_config('app.tenant_id', '', true);
        DELETE FROM tenants t WHERE t.id = p_tenant_id;
        GET DIAGNOSTICS v_count = ROW_COUNT;
        v_result := v_count > 0;
    END IF;
    PERFORM set_config('app.agent', coalesce(v_prev_agent, ''), true);
    RETURN v_result;
END;
$$;
REVOKE ALL ON FUNCTION "public"."admin_delete_empty_tenant"(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION "public"."admin_delete_empty_tenant"(uuid) TO "wpmgr_app";

-- Grandfather backfill: see the top-of-file comment for the full rationale.
-- Idempotent via the "NOT plan_overrides ? 'max_sites'" guard.
UPDATE "public"."tenants" t
SET "plan_overrides" = jsonb_set(t.plan_overrides, '{max_sites}', to_jsonb(cnt.active_count), true)
FROM (
    SELECT tenant_id, count(*) AS active_count
    FROM sites
    WHERE connection_state <> 'archived'
    GROUP BY tenant_id
) cnt
WHERE t.id = cnt.tenant_id
  AND cnt.active_count > 3
  AND NOT (t.plan_overrides ? 'max_sites');
