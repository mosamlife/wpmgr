-- m123 - close the UPDATE/DELETE gap m122 left on org_context_versions.
-- Found by security review on PR #562, before either table shipped anywhere.
--
-- WHY THIS IS A NEW ORDINAL AND NOT AN EDIT TO m122
--
-- m122 has already been applied to real databases: the security review rebuilt
-- a full install from it (131 migrations, wpmgr_app provisioned the way
-- infra/postgres/init/01-app-role.sh does), and every `make test-integration`
-- run applies it to a fresh container. internal/db/migrate.go tracks applied
-- versions in schema_migrations and skips anything already present, so editing
-- m122 in place would be a silent no-op on exactly the databases that need
-- changing, while looking like a fix. A correction is a new ordinal plus a
-- converge path -- that is what m114 and m115 are, and this is the same shape
-- caught earlier.
--
-- CONVERGE PATH: this file IS it, and it needs no separate branch. Every
-- statement is guarded on pg_policies, so a database that applied m122 gains
-- exactly the two missing policies, and a database that has never seen either
-- migration applies m122 then m123 and lands in the same place. There is no
-- third state, because m122 has never existed in more than one version.
--
-- ---------------------------------------------------------------------------
-- WHAT WAS WRONG
-- ---------------------------------------------------------------------------
--
-- m122 gave org_context_versions a RESTRICTIVE gate on INSERT alone, reasoning
-- that the table is append-only so INSERT is the entire write surface.
--
-- That reasoning is wrong, and the way it is wrong is worth writing down
-- because it is a shape that recurs. It is the REVOKE of UPDATE/DELETE that
-- makes INSERT the whole write surface -- so the argument silently promoted the
-- REVOKE from "second layer" to "only layer". Two independent protections were
-- collapsed into one without anyone deciding to. site_context_versions never
-- had this problem: its policy is FOR ALL, so the two layers there are actually
-- two.
--
-- The review granted the privileges back -- simulating a future blanket
-- GRANT ... ON ALL TABLES IN SCHEMA public TO wpmgr_app -- and found:
--
--   ORG  UPDATE by a site-scoped principal -> SUCCEEDED, 1 row
--   ORG  DELETE by a site-scoped principal -> SUCCEEDED, 1 row
--   SITE UPDATE of a sibling site          -> refused, 0 rows
--
-- That blanket GRANT is not an exotic hypothesis. m1 already contains the exact
-- statement (20260527130000_auth_multitenancy.sql:120), and the integration
-- harness runs it on every startPostgres
-- (apps/api/tests/rls_integration_test.go). Any future migration, restore
-- script or operator runbook repeating it re-arms the gap with no error and no
-- log line.
--
-- The consequence is not a read leak. It is that a collaborator invited to ONE
-- site could rewrite or destroy the ORGANISATION's governing context -- layer 2
-- of ADR-064 Decision 1, which applies to every site in the organisation. A
-- principal that can edit layer 2 does not need to widen anything; it becomes
-- the higher layer. That is m112's defect class one more time.
--
-- ---------------------------------------------------------------------------
-- WHAT THIS ADDS, AND WHAT IT DELIBERATELY DOES NOT
-- ---------------------------------------------------------------------------
--
-- Two RESTRICTIVE policies on org_context_versions, FOR UPDATE and FOR DELETE,
-- with the identical predicate m122's INSERT gate already uses:
--
--   coalesce(current_setting('app.site_scope', true), '') <> 'on'
--
-- USING rather than WITH CHECK, because for UPDATE and DELETE the USING clause
-- is what decides which existing rows the statement may touch. A RESTRICTIVE
-- USING that is false for a site-scoped principal removes every row from that
-- statement's reach, so the write matches nothing.
--
-- NO POLICY IS ADDED FOR SELECT, and that is the load-bearing omission rather
-- than an oversight. ADR-064 Decision 6 gives read access "at the organisation
-- AND the site scope that cover that site", and Decision 8's effective-context
-- preview renders layer 2's surviving contribution to a site. A site-scoped
-- collaborator is entitled to READ the organisation context governing their own
-- site; they cannot understand the rules they work under otherwise. A
-- RESTRICTIVE SELECT gate here would break both decisions at once. m122's
-- header says this and it remains true -- the fix is to cover the two write
-- commands it missed, not to gate the read it correctly left open.
--
-- The result is that org_context_versions is now protected the way
-- site_context_versions already was: a policy layer AND a privilege layer, with
-- neither one load-bearing alone.
--
-- WHY NOT SIMPLY REPLACE THE THREE WITH ONE `FOR ALL` POLICY. Because FOR ALL
-- is AND-combined onto SELECT as well, which is exactly the read this table
-- must not gate -- the m112 trap in reverse. Three command-specific policies
-- are the only shape that gates INSERT, UPDATE and DELETE while leaving SELECT
-- alone. Dropping and recreating m122's INSERT policy to "tidy" the set would
-- also be a needless irreversible statement over the tenant boundary taken at
-- boot inside main(), which db/rls-cross-tenant-policies.txt already records a
-- decision against.
--
-- ---------------------------------------------------------------------------
-- A NOTE FOR S4 THAT m122 GOT HALF RIGHT
-- ---------------------------------------------------------------------------
--
-- m122's header assigns the restore-pointer check to S4 as "refused across an
-- organisation-stamp boundary" (ADR-064 Decision 12). That is one of TWO
-- checks, and only that one was written down. The other:
--
--   restored_from_version_id CURRENTLY ACCEPTS A UUID NAMING NO ROW THAT EVER
--   EXISTED. There is no foreign key -- deliberately, because the check that
--   matters is a cross-row stamp comparison no single-column reference can
--   express -- so nothing in the database refuses a dangling pointer.
--
-- "Refused across a stamp boundary" and "refused when it names nothing" are
-- different checks with different failure modes. A dangling pointer produces a
-- version row claiming to be a restore of something unfindable, which
-- ADR-064 Decision 5's version list renders as an entry whose provenance
-- cannot be substantiated -- the precise opposite of the attributability
-- Decision 7 exists to guarantee. S4 must validate BOTH: that the referenced
-- version exists, and that its stamp matches. Neither is enforced here.
--
-- ---------------------------------------------------------------------------
-- IDEMPOTENCE AND BOOT SAFETY
-- ---------------------------------------------------------------------------
--
-- Both statements are wrapped in pg_policies existence checks, because
-- PostgreSQL 16 has no CREATE POLICY IF NOT EXISTS. Nothing is dropped, no
-- existing policy is altered, no row is read or written. Adding a RESTRICTIVE
-- policy can only ever subtract from what a principal may reach, and for any
-- transaction that does not set app.site_scope -- every organisation-member
-- path, every service path, every worker -- the predicate is a tautology and
-- behaviour is unchanged. migrate.go applies this on boot inside main(), in one
-- transaction; a second application is a no-op.

-- ---------------------------------------------------------------------------
-- org_context_versions: the UPDATE gate
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'org_context_versions'
          AND policyname = 'org_context_versions_site_scope_update'
    ) THEN
        CREATE POLICY "org_context_versions_site_scope_update" ON "public"."org_context_versions"
            AS RESTRICTIVE FOR UPDATE
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- org_context_versions: the DELETE gate
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'org_context_versions'
          AND policyname = 'org_context_versions_site_scope_delete'
    ) THEN
        CREATE POLICY "org_context_versions_site_scope_delete" ON "public"."org_context_versions"
            AS RESTRICTIVE FOR DELETE
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
            );
    END IF;
END;
$$;
