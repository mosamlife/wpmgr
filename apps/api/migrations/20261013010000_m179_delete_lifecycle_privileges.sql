-- The owner keeps DELETE so a site's or tenant's rows go with it. The app
-- role cannot delete these records directly, including when it is the owner.
-- UPDATE and TRUNCATE privileges and existing tenant/site policies stay intact.
-- All tables below already ENABLE and FORCE row security; the migration
-- requires that contract rather than changing the owner's row visibility.

DO $$
DECLARE
    v_table text;
    v_owner text;
BEGIN
    FOREACH v_table IN ARRAY ARRAY[
        'audit_log',
        'org_context_versions',
        'site_context_versions',
        'assistant_update_proposals',
        'assistant_cache_purge_requests',
        'site_content_inventory_runs',
        'site_ability_inventory_runs',
        'assistant_ability_requests',
        'ability_tenant_disables'
    ] LOOP
        SELECT pg_get_userbyid(c.relowner) INTO STRICT v_owner
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'public' AND c.relname = v_table
          AND c.relrowsecurity AND c.relforcerowsecurity;

        -- A restrictive policy also constrains permissive FOR ALL policies.
        -- Referential actions run as the owner with row security bypassed.
        EXECUTE format('DROP POLICY IF EXISTS %I ON public.%I',
                       v_table || '_no_direct_delete', v_table);
        EXECUTE format('CREATE POLICY %I ON public.%I AS RESTRICTIVE FOR DELETE TO wpmgr_app USING (false)',
                       v_table || '_no_direct_delete', v_table);
        EXECUTE format('GRANT DELETE ON TABLE public.%I TO %I', v_table, v_owner);
    END LOOP;
END;
$$;

-- Tenant deletion removes its audit rows through the foreign-key cascade.
-- Empty-tenant cleanup keeps the emptiness guard and reclamation record.
CREATE OR REPLACE FUNCTION public.admin_delete_empty_tenant(p_tenant_id uuid)
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
        DELETE FROM tenants t WHERE t.id = p_tenant_id;
        GET DIAGNOSTICS v_count = ROW_COUNT;
        v_result := v_count > 0;

        -- Reclamation is recorded in the same transaction only after deletion.
        IF v_result THEN
            INSERT INTO tenant_object_reclaim (tenant_id)
            VALUES (p_tenant_id)
            ON CONFLICT (tenant_id, kind) DO UPDATE
            SET completed_at    = NULL,
                attempts        = 0,
                next_attempt_at = now() + interval '24 hours',
                last_error      = NULL,
                updated_at      = now();
        END IF;
    END IF;
    PERFORM set_config('app.agent', coalesce(v_prev_agent, ''), true);
    RETURN v_result;
END;
$$;
REVOKE ALL ON FUNCTION public.admin_delete_empty_tenant(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.admin_delete_empty_tenant(uuid) TO wpmgr_app;

-- Purge retains the caller's tenant setting after deleting the target tenant.
CREATE OR REPLACE FUNCTION public.admin_purge_tenant(p_tenant_id uuid)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_count integer;
    v_prev_tenant text := current_setting('app.tenant_id', true);
BEGIN
    PERFORM set_config('app.tenant_id', p_tenant_id::text, true);
    DELETE FROM tenants t WHERE t.id = p_tenant_id;
    GET DIAGNOSTICS v_count = ROW_COUNT;
    PERFORM set_config('app.tenant_id', coalesce(v_prev_tenant, ''), true);
    RETURN v_count > 0;
END;
$$;
REVOKE ALL ON FUNCTION public.admin_purge_tenant(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.admin_purge_tenant(uuid) TO wpmgr_app;
