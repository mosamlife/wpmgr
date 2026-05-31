-- M21 follow-up — RLS policies for the connection-lifecycle pre-tenant paths.
--
-- The base M21 migration (20260531070000) gave site_connection_history and
-- site_events ONLY a tenant-isolation policy (app.tenant_id). Two lifecycle
-- code paths legitimately run OUTSIDE a tenant scope and would otherwise be
-- denied by RLS:
--
--   1. The site-first enroll CONSUME (InEnrollTx, app.enroll='on') transitions
--      a bound site pending_enrollment→connected AND must append the matching
--      site_connection_history row in the same tx — before app.tenant_id is set.
--   2. The site_events ring-buffer PRUNE (InAgentTx, app.agent='on') is a
--      cross-tenant maintenance DELETE.
--
-- These additive policies mirror sites_enroll / sites_agent exactly. Idempotent.

-- 1. Append connection history during the public enroll consume.
DROP POLICY IF EXISTS conn_history_enroll ON site_connection_history;
CREATE POLICY conn_history_enroll ON site_connection_history
    USING (current_setting('app.enroll', true) = 'on')
    WITH CHECK (current_setting('app.enroll', true) = 'on');

-- 2. Cross-tenant prune of the SSE journal.
DROP POLICY IF EXISTS site_events_agent ON site_events;
CREATE POLICY site_events_agent ON site_events
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');
