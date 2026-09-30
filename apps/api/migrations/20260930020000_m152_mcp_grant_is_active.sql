-- m152: mcp_grant_is_active(tenant, grant) -- one boolean a site-scoped
-- session may ask about its own connection (GH #800, database half).
--
-- THE PROBLEM. Creating an AI cache-clear request runs under the connection's
-- resolved site scope (app.site_scope = 'on'). In that transaction the
-- RESTRICTIVE mcp_grants_site_scope_select policy refuses every mcp_grants row
-- (it must: scope_site_ids is a column naming every other site the org has
-- granted, and RLS filters rows, not columns), and mcp_connection_tokens
-- behaves the same. So the creation path cannot re-check, under its grant
-- lock, that the grant it authenticated with is still active, and a call
-- authenticated before a revoke that reaches the lock after the revoke commits
-- would still insert a pending request.
--
-- THE CHOICE: a function, not a guarded INSERT ... SELECT. A guarded insert
-- would need the same exemption from mcp_grants_site_scope_select inside the
-- INSERT's own SELECT, which only a function can grant, and it would bury the
-- verdict inside one statement where the caller cannot tell "grant inactive"
-- from "some other predicate matched nothing". A function answers exactly one
-- question and returns exactly one bit, which the caller checks before the
-- insert in the same transaction, after the lock.
--
-- WHAT IT REVEALS: one boolean. No column of the grant row leaves the
-- function, and "no such grant", "another tenant's grant" and "inactive grant"
-- are all the same false, so it cannot be used to probe for existence.
--
-- "ACTIVE" is exactly the `authorized` verdict of
-- ReCheckMCPGrantAuthorizationInTenantTx (db/query/mcp_connections.sql):
-- status = 'active', not past expires_at, not idle-expired, and the tenant's
-- assistant not paused. If that definition changes, this function changes with
-- it in a new migration, or the two verdicts disagree.
--
-- HOW THE SITE-SCOPE GATE IS LIFTED. By the function-level
-- `SET app.site_scope = ''` clause, NOT by an in-body set_config. PostgreSQL
-- saves the GUC on entry and restores it on exit, including on error, so the
-- caller's transaction is still site-scoped the instant this returns. An
-- in-body set_config(..., true) would persist for the rest of the caller's
-- transaction (m91 Security review Finding A); the SET clause cannot leak.
-- The lift covers only this function's single SELECT.
--
-- TENANT BOUNDARY, twice. The function refuses (false) unless p_tenant equals
-- the caller's app.tenant_id, and it reads with mcp_grants_tenant_isolation
-- still in force under the caller's app.tenant_id. The explicit check is not
-- redundant: SECURITY DEFINER runs as the migrating role, and on an install
-- whose migrator is a superuser or BYPASSRLS row security does not apply to
-- the body at all, so the explicit check is then the only boundary.
--
-- SECURITY DEFINER, search_path pinned, EXECUTE revoked from PUBLIC and
-- granted to wpmgr_app only. Idempotent: CREATE OR REPLACE, and REVOKE/GRANT
-- are repeatable.
--
-- Converge path: none needed. This is a new object; no earlier version of it
-- was ever applied.

CREATE OR REPLACE FUNCTION mcp_grant_is_active(p_tenant uuid, p_grant uuid)
RETURNS boolean
LANGUAGE plpgsql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
SET app.site_scope = ''
AS $$
DECLARE
    v_caller uuid := nullif(current_setting('app.tenant_id', true), '')::uuid;
    v_active boolean;
BEGIN
    IF p_tenant IS NULL OR p_grant IS NULL OR v_caller IS NULL
       OR p_tenant <> v_caller THEN
        RETURN false;
    END IF;

    SELECT COALESCE(g.status = 'active'
            AND g.expires_at > now()
            AND (g.idle_expire_after_days IS NULL
                 OR COALESCE(g.last_used_at, g.created_at)
                    + make_interval(days => g.idle_expire_after_days) > now())
            AND tn.assistant_paused_at IS NULL,
            false)
      INTO v_active
      FROM mcp_grants g
      JOIN tenants tn ON tn.id = g.tenant_id
     WHERE g.tenant_id = p_tenant
       AND g.id = p_grant;

    RETURN COALESCE(v_active, false);
END;
$$;

REVOKE ALL ON FUNCTION mcp_grant_is_active(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mcp_grant_is_active(uuid, uuid) TO wpmgr_app;
