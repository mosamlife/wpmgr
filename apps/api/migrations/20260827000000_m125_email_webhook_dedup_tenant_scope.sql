-- M125 — Bind email_webhook_events_tenant_isolation to app.tenant_id.
--
-- m60 (20260623000000) created email_webhook_events_tenant_isolation with a
-- two-branch predicate in both USING and WITH CHECK:
--
--     tenant_id IS NULL OR tenant_id = <app.tenant_id>
--
-- Only the second branch binds the row's tenant_id to the caller's tenant. This
-- migration drops the first branch, leaving the single bound branch that every
-- sibling *_tenant_isolation policy carries (rum_rollup_daily_tenant_isolation
-- is the shape being matched).
--
-- WHY THE DROPPED BRANCH IS NOT LOAD-BEARING
--
-- m60's header argues the branch is required because a dedup row is written
-- before the tenant is known, so a row nobody could see would break the dedup
-- key. That reasoning does not depend on THIS policy. Every code path that
-- touches the table runs under Pool.InAgentTx (internal/db/db.go:429), which
-- sets app.agent='on' and sets no tenant GUC at all:
--
--   * InsertWebhookEventDedup  (internal/email/repo.go:1633) — the dedup write
--   * PruneWebhookDedup        (internal/email/repo.go:1699) — the 7-day sweep
--
-- Both are admitted by the separate email_webhook_events_agent policy, which is
-- FOR ALL over every row regardless of tenant_id and is left untouched here.
-- PostgreSQL ORs permissive policies, so removing a branch from a policy that
-- never admitted the agent path cannot narrow it. The table has no SELECT query
-- in db/query/*.sql and no handler reads it, so no operator-facing read regresses
-- either. The policy retains the audit-read affordance m60 intended for it, now
-- scoped to the reading tenant's own attributed rows.
--
-- CONVERGE PATH
--
-- Unconditional DROP ... IF EXISTS + CREATE, so a database that applied m60 and
-- a database created fresh from db/schema.sql reach the same end state. m60 is
-- applied in production and is not edited; this is the forward correction.
--
-- Deliberately NOT added here: a RESTRICTIVE email_webhook_events_site_scope
-- policy. The table carries site_id, but it has no tenant- or site-scoped read
-- path to narrow, and a RESTRICTIVE policy under FORCE ROW LEVEL SECURITY would
-- also apply to the InAgentTx writers — the shape that matches zero rows with no
-- error. That belongs in its own reviewed change, not folded into this one.
--
-- All DDL is idempotent.

DROP POLICY IF EXISTS "email_webhook_events_tenant_isolation" ON "public"."email_webhook_events";

CREATE POLICY "email_webhook_events_tenant_isolation" ON "public"."email_webhook_events"
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
