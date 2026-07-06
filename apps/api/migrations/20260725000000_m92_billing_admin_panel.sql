-- M92 — superadmin billing-admin panel (M16 Phase C1).
--
-- Adds four manual-override columns to tenants, all superadmin-only
-- (internal/admin) and layered on top of Phase A/B without touching their
-- existing vocabulary — see the matching comment in db/schema.sql for the
-- full field-by-field rationale:
--   comp_reason           — free-text reason recorded alongside a manual
--                           plan_status='comped' grant.
--   suspended_at          — a SEPARATE hard-lockout flag (NOT a plan_status
--                           value): the underlying billing state is left
--                           untouched, so "restore" is a clean, lossless
--                           un-suspend. NULL = not suspended.
--   suspended_reason      — free-text reason recorded alongside suspend.
--   cancel_at_period_end  — display mirror of the provider's own flag.
--
-- RLS (this migration ADDS two policies; it never drops or narrows an
-- existing one): the superadmin accounts-list aggregate query
-- (internal/admin.Repo.ListAccounts) computes, in ONE query across ALL
-- tenants, each tenant's managed-storage bytes (SUM backup_chunks.size) and
-- last_activity (GREATEST of audit_log/sites/memberships timestamps). sites
-- and memberships already carry a cross-tenant SELECT policy gated on
-- app.agent='on' (sites_agent, memberships_agent — see schema.sql), but
-- backup_chunks and audit_log do not: without a matching agent-scope SELECT
-- policy on those two tables, the accounts-list query would silently see
-- zero rows for storage/audit-derived columns under FORCE ROW LEVEL
-- SECURITY when run via InAgentTx. backup_chunks_agent and audit_log_agent
-- below close that gap, mirroring memberships_agent exactly (SELECT-only,
-- no cross-tenant writes; audit_log's append-only privilege revocation is
-- unaffected — this is a read policy, not a grant).
--
-- Idempotent throughout: ADD COLUMN IF NOT EXISTS + guarded CREATE POLICY,
-- safe to re-run (mirrors the project's established style, e.g. m91).

ALTER TABLE "public"."tenants" ADD COLUMN IF NOT EXISTS "comp_reason" text;
ALTER TABLE "public"."tenants" ADD COLUMN IF NOT EXISTS "suspended_at" timestamptz;
ALTER TABLE "public"."tenants" ADD COLUMN IF NOT EXISTS "suspended_reason" text;
ALTER TABLE "public"."tenants" ADD COLUMN IF NOT EXISTS "cancel_at_period_end" boolean NOT NULL DEFAULT false;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'backup_chunks' AND policyname = 'backup_chunks_agent'
    ) THEN
        CREATE POLICY "backup_chunks_agent" ON "public"."backup_chunks"
            FOR SELECT
            USING (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'audit_log' AND policyname = 'audit_log_agent'
    ) THEN
        CREATE POLICY "audit_log_agent" ON "public"."audit_log"
            FOR SELECT
            USING (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;
