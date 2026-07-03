-- m89: add the missing app.agent RLS policy on update_tasks (#131 follow-up)
--
-- Root cause: update_tasks was created (M3) with only
-- update_tasks_tenant_isolation (the app.tenant_id operator-path policy) — no
-- app.agent policy, unlike the m36-template tables that ship with both from
-- day one. Every existing update_tasks query ran under InTenantTx, so this
-- went unnoticed. The #131 stale-task reaper (ReaperWorker.Work ->
-- Repo.ListStaleUpdateTasks) is the first CROSS-TENANT read against this
-- table: it runs under InAgentTx (app.agent = 'on', app.tenant_id unset) to
-- find stuck tasks belonging to ANY tenant in one sweep. Under FORCE ROW
-- LEVEL SECURITY with only the tenant_isolation policy present, that context
-- satisfies neither policy, so the SELECT silently returned zero rows on
-- every sweep — the exact "FOR UPDATE / cross-tenant query returns nothing
-- under app.agent" class of bug already fixed once for backup_schedules in
-- m84.
--
-- Fix: add update_tasks_agent, mirroring the m36 template's paired policy
-- convention. Idempotent: guarded CREATE POLICY inside a DO block, safe to
-- re-run.

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'update_tasks'
          AND policyname = 'update_tasks_agent'
    ) THEN
        CREATE POLICY update_tasks_agent ON update_tasks
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;
