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
-- HOW THE SITE-SCOPE GATE IS LIFTED. An in-body set_config, saved and
-- restored around the one SELECT. A function-level `SET app.site_scope = ''`
-- clause would restore itself, but PostgreSQL refuses it at CREATE time for a
-- NOSUPERUSER migrator (42501, "permission denied to set parameter"), so it
-- would fail at boot on every real install. set_config(..., true) is NOT
-- undone at function exit (m91 Security review Finding A), so:
--   * the normal path writes the caller's saved value back before RETURN;
--   * the lift happens inside a BEGIN ... EXCEPTION block, which runs as a
--     subtransaction; if the SELECT raises, the subtransaction aborts and
--     PostgreSQL reverts the GUC with it, and the error is re-raised.
-- Either way the caller's transaction is site-scoped again the instant this
-- returns, and the lift covers only this function's single SELECT.
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
VOLATILE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_caller uuid := nullif(current_setting('app.tenant_id', true), '')::uuid;
    v_prev   text := coalesce(current_setting('app.site_scope', true), '');
    v_active boolean;
BEGIN
    IF p_tenant IS NULL OR p_grant IS NULL OR v_caller IS NULL
       OR p_tenant <> v_caller THEN
        RETURN false;
    END IF;

    BEGIN
        PERFORM set_config('app.site_scope', '', true);

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

        PERFORM set_config('app.site_scope', v_prev, true);
    EXCEPTION WHEN OTHERS THEN
        -- The subtransaction has aborted and PostgreSQL has already reverted
        -- the set_config above; re-raise unchanged.
        RAISE;
    END;

    RETURN COALESCE(v_active, false);
END;
$$;

REVOKE ALL ON FUNCTION mcp_grant_is_active(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mcp_grant_is_active(uuid, uuid) TO wpmgr_app;
