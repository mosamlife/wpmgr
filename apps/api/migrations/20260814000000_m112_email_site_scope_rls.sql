-- m112 - GH #380: give the email domain the RESTRICTIVE app.site_scope RLS
-- policies that 25 other site-keyed tables already carry, and split them into
-- a READ policy and WRITE policies because this domain has an inheriting row
-- that the exemplar tables do not.
--
-- WHY THIS EXISTS AT ALL
--
-- Three review rounds on this issue found seven privilege-escalation doors in
-- the email domain and closed each one in a handler. The third round worked
-- out why they kept appearing, and it is not that the handlers were careless.
-- It is that the database had no opinion. site_email_config,
-- site_email_connection, site_email_log and email_suppression carried only
-- tenant_isolation plus the app.agent escape, so the only thing the DB
-- refused was another TENANT. Nothing in the database stopped a site-scoped
-- collaborator from touching their OWN tenant's organisation row.
--
-- Every one of those seven doors had the same shape. A per-site route
-- (PermEmailManage is a per-site permission, so an outside collaborator
-- invited to exactly one site holds it) resolves a config; GetConfig's
-- inheritance fallback silently returns the ORGANISATION row, site_id IS
-- NULL, along with its ID; and the write then lands on the organisation. The
-- payload varies - repoint the org's SMTP host, rewrite an org connection,
-- rotate the org's webhook route token, delete a fleet-wide suppression entry
-- - but the mechanism is one mechanism. A handler guard closes one instance of
-- it. A policy closes the class, including the instances nobody has thought of
-- yet, and including the ones a future refactor reintroduces.
--
-- WHY THIS IS NOT A COPY OF site_destinations_site_scope
--
-- The exemplar (m19, db/schema.sql) is a single AS RESTRICTIVE FOR ALL policy
-- with one predicate used as both USING and WITH CHECK. That works there
-- because those tables have no meaningful site_id IS NULL row. Here the org
-- row is not an edge case, it is a shipped feature: a site with no config of
-- its own INHERITS the organisation's, and that inherited row is the config
-- the site actually sends mail with. GET /sites/:siteId/email/config and the
-- connections list both legitimately surface it to a site-scoped collaborator,
-- because it is a true and necessary answer to "what will this site send
-- with". Copying the exemplar verbatim would take one of two wrong turns:
--
--   * predicate requires site_id = ANY(allowed) -> the org row fails it, the
--     inheriting read returns nothing, and the shipped feature breaks. The
--     collaborator's own site page would show "no configuration" for a site
--     that is in fact configured and sending.
--   * predicate also permits site_id IS NULL -> the org row passes it for
--     WRITES too, and we have re-opened every door this migration exists to
--     close.
--
-- So read is split from write. Stated exactly, per operation:
--
--   _site_scope_read   FOR SELECT, USING only.
--       Permits the org row (site_id IS NULL) plus the principal's own
--       granted sites. This is the policy that keeps inheritance working.
--
--   _site_scope_insert FOR INSERT, WITH CHECK only.
--       Permits ONLY the principal's own granted sites. A site-scoped
--       principal may not bring an org row into existence. INSERT has no
--       USING clause in Postgres, so WITH CHECK is the whole policy.
--
--   _site_scope_update FOR UPDATE, USING and WITH CHECK.
--       USING decides which existing rows are visible to update (this is what
--       refuses the org row); WITH CHECK decides what they may become (this is
--       what refuses moving one's own row to site_id NULL, which would
--       otherwise be a two-step route to the same place).
--
--   _site_scope_delete FOR DELETE, USING only.
--       Permits ONLY the principal's own granted sites.
--
-- The split matters mechanically, not just descriptively: RESTRICTIVE policies
-- are AND-combined with each other. A FOR ALL write policy would therefore
-- also AND itself onto SELECT and block the inheriting read, which is the
-- precise trap described above. Four operation-specific policies are the only
-- shape that lets SELECT be permissive about the org row while INSERT, UPDATE
-- and DELETE are strict about it.
--
-- WHAT DOES NOT CHANGE, AND WHY THAT IS DELIBERATE
--
--   * ORG-SCOPED MEMBERS. Every predicate opens with
--     coalesce(current_setting('app.site_scope', true), '') <> 'on'. For any
--     transaction that does not set app.site_scope (which is every org-member
--     path, every worker, every agent path) that branch is a tautology and all
--     four policies are no-ops. An org member's behaviour after this migration
--     is byte-for-byte what it was before it.
--
--   * THE AGENT ESCAPE. The agent pushes email logs and reads config under
--     InAgentTx, which sets app.agent='on' and never sets app.site_scope. The
--     tautology branch above covers it. The existing permissive _agent policies
--     are untouched. This was checked explicitly because breaking it would
--     stop every site in the fleet from reporting mail.
--
--   * TENANT ISOLATION. The permissive _tenant_isolation policies are
--     untouched. RESTRICTIVE policies only ever narrow; they cannot widen
--     anything, so nothing here can grant access that did not already exist.
--
-- THE FOUR TABLES, AND WHY EACH GETS THE SHAPE IT GETS
--
--   site_email_config      site_id is a direct nullable column. Org row =
--                          site_id IS NULL. Full read/write split.
--
--   site_email_connection  has NO site_id column; it hangs off config_id.
--                          The predicate therefore reaches through to
--                          site_email_config, the same indirect-join approach
--                          m19 uses for its own child tables. This table
--                          stores provider_secret_encrypted per connection, so
--                          leaving it ungated while gating its parent would
--                          have left the credential reachable by the child.
--
--   site_email_log         site_id is NOT NULL, so there is no org row and
--                          nothing to inherit. It still gets the split rather
--                          than a FOR ALL policy, purely so all four tables
--                          read identically and a later reviewer does not have
--                          to work out why one is different. The read
--                          predicate has no IS NULL branch because a NULL
--                          site_id cannot occur here.
--
--   email_suppression      site_id is nullable and fleet-wide (NULL) entries
--                          are read by every site (see IsSuppressed, which
--                          matches site_id IS NULL OR site_id = @site_id).
--                          Exactly the config table's shape, so exactly the
--                          config table's split. Without it a site-scoped
--                          collaborator could DELETE a fleet-wide suppression
--                          row, which is what stops the whole organisation
--                          mailing an address that complained.
--
-- IDEMPOTENCE AND BOOT SAFETY
--
-- internal/db/migrate.go applies these on boot, in lexical order, each in its
-- own transaction, and a failure here takes the control plane down. Adversarial
-- review has caught a boot-blocking migration in this repo before, so:
--
--   * CREATE POLICY has no IF NOT EXISTS in Postgres 16, so every policy is
--     wrapped in a pg_policies existence check (the m94 pattern).
--   * ALTER TABLE ... ENABLE/FORCE ROW LEVEL SECURITY is already true for all
--     four tables (m59/m60/m62) and is idempotent regardless; it is restated
--     so this file is self-contained if a future squash reorders history.
--   * Nothing is dropped, no data is touched, no column changes. The worst a
--     re-run can do is nothing.
--   * This applies cleanly to a database that already carries every migration
--     through m111.

-- ---------------------------------------------------------------------------
-- 1. site_email_config
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    ALTER TABLE "public"."site_email_config" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_email_config" FORCE ROW LEVEL SECURITY;
END;
$$;

-- READ. The org row (site_id IS NULL) is deliberately visible: it is the
-- config an inheriting site actually sends with, and GET /email/config plus
-- listConnections legitimately surface it.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_email_config'
          AND policyname = 'site_email_config_site_scope_read'
    ) THEN
        CREATE POLICY "site_email_config_site_scope_read" ON "public"."site_email_config"
            AS RESTRICTIVE FOR SELECT
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" IS NULL
                OR "site_id" = ANY (
                    string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
                )
            );
    END IF;
END;
$$;

-- INSERT. No org row may be created by a site-scoped principal. INSERT takes
-- no USING clause, so WITH CHECK is the entire policy.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_email_config'
          AND policyname = 'site_email_config_site_scope_insert'
    ) THEN
        CREATE POLICY "site_email_config_site_scope_insert" ON "public"."site_email_config"
            AS RESTRICTIVE FOR INSERT
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
                )
            );
    END IF;
END;
$$;

-- UPDATE. USING refuses the org row as a target; WITH CHECK refuses turning an
-- own-site row into an org row.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_email_config'
          AND policyname = 'site_email_config_site_scope_update'
    ) THEN
        CREATE POLICY "site_email_config_site_scope_update" ON "public"."site_email_config"
            AS RESTRICTIVE FOR UPDATE
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

-- DELETE.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_email_config'
          AND policyname = 'site_email_config_site_scope_delete'
    ) THEN
        CREATE POLICY "site_email_config_site_scope_delete" ON "public"."site_email_config"
            AS RESTRICTIVE FOR DELETE
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 2. site_email_connection
--
-- No site_id column: the row hangs off config_id. The predicate reaches
-- through to site_email_config, which is itself RLS-protected, so the subquery
-- sees only rows the principal may already see. That is intentional layering,
-- not an accident: the read predicate's reach-through resolves the org row
-- (permitted by _site_scope_read above) while the write predicates require a
-- non-NULL site_id that is in the allowlist, so an org connection is readable
-- and not writable, matching its parent exactly.
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    ALTER TABLE "public"."site_email_connection" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_email_connection" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_email_connection'
          AND policyname = 'site_email_connection_site_scope_read'
    ) THEN
        CREATE POLICY "site_email_connection_site_scope_read" ON "public"."site_email_connection"
            AS RESTRICTIVE FOR SELECT
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR EXISTS (
                    SELECT 1 FROM "public"."site_email_config" c
                    WHERE c."id" = "site_email_connection"."config_id"
                      AND (
                          c."site_id" IS NULL
                          OR c."site_id" = ANY (
                              string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
                          )
                      )
                )
            );
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_email_connection'
          AND policyname = 'site_email_connection_site_scope_insert'
    ) THEN
        CREATE POLICY "site_email_connection_site_scope_insert" ON "public"."site_email_connection"
            AS RESTRICTIVE FOR INSERT
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR EXISTS (
                    SELECT 1 FROM "public"."site_email_config" c
                    WHERE c."id" = "site_email_connection"."config_id"
                      AND c."site_id" = ANY (
                          string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
                      )
                )
            );
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_email_connection'
          AND policyname = 'site_email_connection_site_scope_update'
    ) THEN
        CREATE POLICY "site_email_connection_site_scope_update" ON "public"."site_email_connection"
            AS RESTRICTIVE FOR UPDATE
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR EXISTS (
                    SELECT 1 FROM "public"."site_email_config" c
                    WHERE c."id" = "site_email_connection"."config_id"
                      AND c."site_id" = ANY (
                          string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
                      )
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR EXISTS (
                    SELECT 1 FROM "public"."site_email_config" c
                    WHERE c."id" = "site_email_connection"."config_id"
                      AND c."site_id" = ANY (
                          string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
                      )
                )
            );
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_email_connection'
          AND policyname = 'site_email_connection_site_scope_delete'
    ) THEN
        CREATE POLICY "site_email_connection_site_scope_delete" ON "public"."site_email_connection"
            AS RESTRICTIVE FOR DELETE
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR EXISTS (
                    SELECT 1 FROM "public"."site_email_config" c
                    WHERE c."id" = "site_email_connection"."config_id"
                      AND c."site_id" = ANY (
                          string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
                      )
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 3. site_email_log
--
-- site_id is NOT NULL here, so there is no org row and no inheritance. The
-- split is kept anyway for uniformity across the domain; the read predicate
-- simply has no IS NULL branch because a NULL cannot occur.
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    ALTER TABLE "public"."site_email_log" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_email_log" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_email_log'
          AND policyname = 'site_email_log_site_scope_read'
    ) THEN
        CREATE POLICY "site_email_log_site_scope_read" ON "public"."site_email_log"
            AS RESTRICTIVE FOR SELECT
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid
                OR "site_id" = ANY (
                    string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
                )
            );
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_email_log'
          AND policyname = 'site_email_log_site_scope_insert'
    ) THEN
        CREATE POLICY "site_email_log_site_scope_insert" ON "public"."site_email_log"
            AS RESTRICTIVE FOR INSERT
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
                )
            );
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_email_log'
          AND policyname = 'site_email_log_site_scope_update'
    ) THEN
        CREATE POLICY "site_email_log_site_scope_update" ON "public"."site_email_log"
            AS RESTRICTIVE FOR UPDATE
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

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_email_log'
          AND policyname = 'site_email_log_site_scope_delete'
    ) THEN
        CREATE POLICY "site_email_log_site_scope_delete" ON "public"."site_email_log"
            AS RESTRICTIVE FOR DELETE
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 4. email_suppression
--
-- Fleet-wide entries carry site_id IS NULL and are read by every site, so this
-- is the config table's shape and gets the config table's split. The DELETE
-- policy is the load-bearing one: a fleet-wide suppression row is what stops
-- the whole organisation mailing an address that complained, and deleting it
-- is an organisation-level act.
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    ALTER TABLE "public"."email_suppression" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."email_suppression" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'email_suppression'
          AND policyname = 'email_suppression_site_scope_read'
    ) THEN
        CREATE POLICY "email_suppression_site_scope_read" ON "public"."email_suppression"
            AS RESTRICTIVE FOR SELECT
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" IS NULL
                OR "site_id" = ANY (
                    string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
                )
            );
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'email_suppression'
          AND policyname = 'email_suppression_site_scope_insert'
    ) THEN
        CREATE POLICY "email_suppression_site_scope_insert" ON "public"."email_suppression"
            AS RESTRICTIVE FOR INSERT
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
                )
            );
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'email_suppression'
          AND policyname = 'email_suppression_site_scope_update'
    ) THEN
        CREATE POLICY "email_suppression_site_scope_update" ON "public"."email_suppression"
            AS RESTRICTIVE FOR UPDATE
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

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'email_suppression'
          AND policyname = 'email_suppression_site_scope_delete'
    ) THEN
        CREATE POLICY "email_suppression_site_scope_delete" ON "public"."email_suppression"
            AS RESTRICTIVE FOR DELETE
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
                )
            );
    END IF;
END;
$$;
