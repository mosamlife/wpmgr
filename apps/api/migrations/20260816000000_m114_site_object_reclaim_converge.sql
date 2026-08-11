-- m114: converge any database that applied the PRE-REVIEW m113.
--
-- WHY THIS IS A SEPARATE FILE AND NOT AN EDIT TO m113.
--
-- internal/db/migrate.go sorts the embedded versions lexically and skips every
-- one already present in schema_migrations:
--
--     for _, version := range versions {
--         if applied[version] { continue }
--
-- So a database that has already run m113 will never read m113 again, however
-- the file is edited. Fixing the two defects below by editing m113 in place
-- would have changed exactly the databases that did not need changing and left
-- untouched the only one that did. The first draft of this fix did that, which
-- is the same shape of mistake as the defect it was fixing: a statement placed
-- where the thing it targets can never reach it.
--
-- Which databases have the pre-review m113? Any that ran the GH #402 branch
-- before its second review round: developer machines and preview environments.
-- No released version carries m113 at all, so this migration is a no-op on
-- every production database, and it must be safe there rather than merely
-- unnecessary. Both statements are IF EXISTS / IF NOT EXISTS guarded, so it is
-- re-runnable forever and does nothing at all on a database whose m113 is the
-- reviewed one.
--
-- WHAT THE PRE-REVIEW m113 GOT WRONG
--
-- 1. site_object_reclaim carried a tenant foreign key with ON DELETE CASCADE.
--    That destroys the reclaim record in the operation that should have
--    triggered it. admin_delete_empty_tenant (DELETE /orgs/{orgId} for an org
--    with no sites and no other members, and the superadmin orphan cleanup)
--    hard-deletes a tenant row and frees NO object storage, because its guard,
--    "no memberships and no sites", was read as "owns no objects". An org whose
--    sites were all deleted first satisfies that guard and owns objects. Only
--    Lane B, the grace-window purge worker, sweeps the tenant object roots
--    before its hard delete. So the cascade reinstated GH #402 one level up: the
--    site's manifests stayed in the bucket with nothing left naming them.
--
--    A record of what to clean up must outlive the deletion it describes. That
--    is the whole point of the table, and a cascade onto it is a contradiction
--    in terms. m113 as reviewed has no foreign key to either parent.
--
-- 2. It had no site-scope policy. site_object_reclaim is site-keyed, so tenant
--    isolation alone left a collaborator invited to exactly one site able to
--    read, close or cancel the reclaim rows naming every other site in the
--    organisation. Closing another site's row leaves that site's manifests
--    orphaned, which is GH #402 reached through a different door. m112 shipped
--    one migration earlier for this exact class in the email domain.

-- ---------------------------------------------------------------------------
-- 1. Drop the cascade.
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'site_object_reclaim'
    ) THEN
        ALTER TABLE "public"."site_object_reclaim"
            DROP CONSTRAINT IF EXISTS "site_object_reclaim_tenant_id_fkey";
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 2. Add the m19 RESTRICTIVE site-scope policy if it is missing.
--
-- Identical in every character to the one m113 creates, so a fresh database and
-- a converged one end up with the same policy rather than two that merely look
-- alike. RESTRICTIVE policies are AND-combined with the permissive ones, so
-- this can only ever subtract: both real writers (the enqueue riding DELETE
-- /sites/{id}'s InTenantTx, and the worker's InAgentTx) leave app.site_scope
-- unset, which makes the first branch a tautology for them.
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'site_object_reclaim'
    ) AND NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'site_object_reclaim'
          AND policyname = 'site_object_reclaim_site_scope'
    ) THEN
        CREATE POLICY "site_object_reclaim_site_scope" ON "public"."site_object_reclaim"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
                )
            );
    END IF;
END;
$$;
