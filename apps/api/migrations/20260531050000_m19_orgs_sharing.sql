-- M19 — Orgs + Per-Site Sharing (M5.7).
--
-- Adds:
--   1. citext extension (for case-insensitive email storage on invitations).
--   2. UNIQUE(id, tenant_id) on sites — backs the composite FK on site_shares.
--   3. Role CHECK on memberships.
--   4. site_shares — the structural per-site grant table.
--   5. invitations — org + site invitation table (tokenized, single-use, 7-day TTL).
--   6. AS RESTRICTIVE site_scope policies on all 21 direct site-keyed tables plus
--      join-based restrictive policies on the 3 indirect children
--      (backup_manifest_entries, restore_run_events, scan_run_hashes).
--      backup_chunks is intentionally excluded (shared/content-addressed, no site_id).
--
-- Idempotency: every statement is guarded with IF NOT EXISTS checks so running
-- this migration twice is safe. Runs in ONE transaction (no CONCURRENTLY).
--
-- NOTE on tables whose site column differs from the generic pattern:
--   site_alert_state   — PK is site_id (no separate id column); USING(site_id=…)
--   autologin_policies — PK is site_id; USING(site_id=…)
--   site_error_config  — PK is site_id; USING(site_id=…)
--   site_login_brand   — PK is site_id; USING(site_id=…)
--   site_security_config — PK is site_id; USING(site_id=…)
--   site_destinations  — site_id is NULLABLE; coalesce(site_id,id) trick is not
--                         applicable; use site_id directly (NULL rows pass ANY).
--   scan_run_hashes    — no site_id column; indirect child via run_id→scan_runs.
--   sites itself       — restrictive key is id (not site_id).

-- ---------------------------------------------------------------------------
-- 0. citext extension
-- ---------------------------------------------------------------------------

CREATE EXTENSION IF NOT EXISTS citext;

-- ---------------------------------------------------------------------------
-- 1. UNIQUE(id, tenant_id) on sites  — backs composite FK on site_shares
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname    = 'sites_id_tenant_key'
          AND conrelid   = 'public.sites'::regclass
    ) THEN
        ALTER TABLE "public"."sites"
            ADD CONSTRAINT "sites_id_tenant_key" UNIQUE ("id", "tenant_id");
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 2. Role CHECK on memberships
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname  = 'memberships_role_check'
          AND conrelid = 'public.memberships'::regclass
    ) THEN
        ALTER TABLE "public"."memberships"
            ADD CONSTRAINT "memberships_role_check"
            CHECK (role IN ('owner', 'admin', 'operator', 'viewer'));
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 3. site_shares
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."site_shares" (
        "id"         uuid        NOT NULL DEFAULT gen_random_uuid(),
        "tenant_id"  uuid        NOT NULL,
        "site_id"    uuid        NOT NULL,
        "user_id"    uuid        NOT NULL,
        "role"       text        NOT NULL DEFAULT 'viewer'
                     CHECK (role IN ('viewer', 'operator', 'admin')),
        "granted_by" uuid        NULL,
        "expires_at" timestamptz NULL,
        "created_at" timestamptz NOT NULL DEFAULT now(),
        PRIMARY KEY ("id"),
        CONSTRAINT "site_shares_user_id_fkey"
            FOREIGN KEY ("user_id") REFERENCES "public"."users" ("id")
            ON DELETE CASCADE,
        CONSTRAINT "site_shares_granted_by_fkey"
            FOREIGN KEY ("granted_by") REFERENCES "public"."users" ("id")
            ON DELETE SET NULL,
        CONSTRAINT "site_shares_site_tenant_fkey"
            FOREIGN KEY ("site_id", "tenant_id")
            REFERENCES "public"."sites" ("id", "tenant_id")
            ON DELETE CASCADE,
        CONSTRAINT "site_shares_site_user_key"
            UNIQUE ("site_id", "user_id")
    );
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'site_shares'
          AND indexname  = 'site_shares_user_id_idx'
    ) THEN
        CREATE INDEX "site_shares_user_id_idx"
            ON "public"."site_shares" ("user_id");
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'site_shares'
          AND indexname  = 'site_shares_tenant_id_idx'
    ) THEN
        CREATE INDEX "site_shares_tenant_id_idx"
            ON "public"."site_shares" ("tenant_id");
    END IF;
END;
$$;

DO $$
BEGIN
    ALTER TABLE "public"."site_shares" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_shares" FORCE ROW LEVEL SECURITY;
END;
$$;

-- Org admins: tenant-scoped read/write.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_shares'
          AND policyname = 'site_shares_tenant_isolation'
    ) THEN
        CREATE POLICY "site_shares_tenant_isolation" ON "public"."site_shares"
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

-- Grantee cross-org discovery: a site-scoped collaborator reads their own shares
-- (no tenant scope yet at auth time) to build the AllowedSiteIDs allowlist.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_shares'
          AND policyname = 'site_shares_self_read'
    ) THEN
        CREATE POLICY "site_shares_self_read" ON "public"."site_shares"
            FOR SELECT
            USING ("user_id" = nullif(current_setting('app.user_id', true), '')::uuid);
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 4. invitations
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."invitations" (
        "id"               uuid        NOT NULL DEFAULT gen_random_uuid(),
        "tenant_id"        uuid        NOT NULL,
        "email"            citext      NOT NULL,
        "scope"            text        NOT NULL CHECK (scope IN ('org', 'site')),
        "site_id"          uuid        NULL,
        "role"             text        NOT NULL,
        "token_hash"       text        NOT NULL,
        "invited_by"       uuid        NULL,
        "expires_at"       timestamptz NOT NULL,
        "attempts"         integer     NOT NULL DEFAULT 0,
        "accepted_at"      timestamptz NULL,
        "accepted_user_id" uuid        NULL,
        "created_at"       timestamptz NOT NULL DEFAULT now(),
        PRIMARY KEY ("id"),
        CONSTRAINT "invitations_token_hash_key"
            UNIQUE ("token_hash"),
        CONSTRAINT "invitations_tenant_id_fkey"
            FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id")
            ON DELETE CASCADE,
        CONSTRAINT "invitations_site_id_fkey"
            FOREIGN KEY ("site_id") REFERENCES "public"."sites" ("id")
            ON DELETE CASCADE,
        CONSTRAINT "invitations_invited_by_fkey"
            FOREIGN KEY ("invited_by") REFERENCES "public"."users" ("id")
            ON DELETE SET NULL,
        CONSTRAINT "invitations_accepted_user_id_fkey"
            FOREIGN KEY ("accepted_user_id") REFERENCES "public"."users" ("id")
            ON DELETE SET NULL
    );
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'invitations'
          AND indexname  = 'invitations_tenant_id_idx'
    ) THEN
        CREATE INDEX "invitations_tenant_id_idx"
            ON "public"."invitations" ("tenant_id");
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'invitations'
          AND indexname  = 'invitations_email_idx'
    ) THEN
        CREATE INDEX "invitations_email_idx"
            ON "public"."invitations" ("email");
    END IF;
END;
$$;

DO $$
BEGIN
    ALTER TABLE "public"."invitations" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."invitations" FORCE ROW LEVEL SECURITY;
END;
$$;

-- Org admins: tenant-scoped read/write.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'invitations'
          AND policyname = 'invitations_tenant_isolation'
    ) THEN
        CREATE POLICY "invitations_tenant_isolation" ON "public"."invitations"
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

-- Public accept endpoint: look up an invitation by its token hash before any
-- session/tenant scope exists. Mirrors api_keys_prefix_lookup.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'invitations'
          AND policyname = 'invitations_token_lookup'
    ) THEN
        CREATE POLICY "invitations_token_lookup" ON "public"."invitations"
            FOR SELECT
            USING (current_setting('app.invite_lookup', true) = 'on');
    END IF;
END;
$$;

-- ===========================================================================
-- 5. AS RESTRICTIVE site_scope policies
-- ===========================================================================
--
-- These policies are AND-combined with every permissive policy on the table.
-- When app.site_scope is NOT 'on' (normal members, service paths), the first
-- branch of the coalesce is true → the policy is a tautology (no-op).
-- When app.site_scope IS 'on' (collaborator path), only rows whose site_id is
-- in the app.allowed_site_ids GUC (comma-separated uuid list) pass.
--
-- The macro pattern (repeated for each of the 21 direct tables):
--
--   CREATE POLICY "<t>_site_scope" ON "public"."<t>"
--     AS RESTRICTIVE FOR ALL
--     USING (
--       coalesce(current_setting('app.site_scope', true), '') <> 'on'
--       OR <site_key> = ANY (
--           string_to_array(
--               nullif(current_setting('app.allowed_site_ids', true), ''), ','
--           )::uuid[]
--         )
--     )
--     WITH CHECK (
--       coalesce(current_setting('app.site_scope', true), '') <> 'on'
--       OR <site_key> = ANY (
--           string_to_array(
--               nullif(current_setting('app.allowed_site_ids', true), ''), ','
--           )::uuid[]
--         )
--     );
--
-- <site_key>:
--   sites              → id
--   all other direct   → site_id
--   site_destinations  → site_id (nullable; NULL passes ANY check safely)
--
-- Indirect children (no direct site_id):
--   backup_manifest_entries → snapshot_id IN (SELECT id FROM backup_snapshots WHERE site_id=ANY…)
--   restore_run_events      → restore_run_id IN (SELECT id FROM restore_runs WHERE site_id=ANY…)
--   scan_run_hashes         → run_id IN (SELECT id FROM scan_runs WHERE site_id=ANY…)
-- ===========================================================================

-- ---------------------------------------------------------------------------
-- 5a. sites  (key = id)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'sites'
          AND policyname = 'sites_site_scope'
    ) THEN
        CREATE POLICY "sites_site_scope" ON "public"."sites"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5b. update_tasks  (key = site_id)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'update_tasks'
          AND policyname = 'update_tasks_site_scope'
    ) THEN
        CREATE POLICY "update_tasks_site_scope" ON "public"."update_tasks"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5c. backup_snapshots  (key = site_id)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'backup_snapshots'
          AND policyname = 'backup_snapshots_site_scope'
    ) THEN
        CREATE POLICY "backup_snapshots_site_scope" ON "public"."backup_snapshots"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5d. backup_schedules  (key = site_id)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'backup_schedules'
          AND policyname = 'backup_schedules_site_scope'
    ) THEN
        CREATE POLICY "backup_schedules_site_scope" ON "public"."backup_schedules"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5e. backup_schedule_runs  (key = site_id)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'backup_schedule_runs'
          AND policyname = 'backup_schedule_runs_site_scope'
    ) THEN
        CREATE POLICY "backup_schedule_runs_site_scope" ON "public"."backup_schedule_runs"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5f. restore_runs  (key = site_id)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'restore_runs'
          AND policyname = 'restore_runs_site_scope'
    ) THEN
        CREATE POLICY "restore_runs_site_scope" ON "public"."restore_runs"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5g. site_alert_state  (PK = site_id; use site_id directly)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_alert_state'
          AND policyname = 'site_alert_state_site_scope'
    ) THEN
        CREATE POLICY "site_alert_state_site_scope" ON "public"."site_alert_state"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5h. site_uptime_probes  (key = site_id)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_uptime_probes'
          AND policyname = 'site_uptime_probes_site_scope'
    ) THEN
        CREATE POLICY "site_uptime_probes_site_scope" ON "public"."site_uptime_probes"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5i. autologin_tokens  (key = site_id)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'autologin_tokens'
          AND policyname = 'autologin_tokens_site_scope'
    ) THEN
        CREATE POLICY "autologin_tokens_site_scope" ON "public"."autologin_tokens"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5j. autologin_policies  (PK = site_id; use site_id directly)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'autologin_policies'
          AND policyname = 'autologin_policies_site_scope'
    ) THEN
        CREATE POLICY "autologin_policies_site_scope" ON "public"."autologin_policies"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5k. agent_activity_log  (key = site_id)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'agent_activity_log'
          AND policyname = 'agent_activity_log_site_scope'
    ) THEN
        CREATE POLICY "agent_activity_log_site_scope" ON "public"."agent_activity_log"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5l. agent_diagnostics  (key = site_id)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'agent_diagnostics'
          AND policyname = 'agent_diagnostics_site_scope'
    ) THEN
        CREATE POLICY "agent_diagnostics_site_scope" ON "public"."agent_diagnostics"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5m. agent_php_errors  (key = site_id)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'agent_php_errors'
          AND policyname = 'agent_php_errors_site_scope'
    ) THEN
        CREATE POLICY "agent_php_errors_site_scope" ON "public"."agent_php_errors"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5n. agent_login_events  (key = site_id)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'agent_login_events'
          AND policyname = 'agent_login_events_site_scope'
    ) THEN
        CREATE POLICY "agent_login_events_site_scope" ON "public"."agent_login_events"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5o. agent_nonces  (key = site_id)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'agent_nonces'
          AND policyname = 'agent_nonces_site_scope'
    ) THEN
        CREATE POLICY "agent_nonces_site_scope" ON "public"."agent_nonces"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5p. site_destinations  (key = site_id; nullable — NULL rows pass ANY safely)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_destinations'
          AND policyname = 'site_destinations_site_scope'
    ) THEN
        CREATE POLICY "site_destinations_site_scope" ON "public"."site_destinations"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5q. site_error_config  (PK = site_id; use site_id directly)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_error_config'
          AND policyname = 'site_error_config_site_scope'
    ) THEN
        CREATE POLICY "site_error_config_site_scope" ON "public"."site_error_config"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5r. site_login_brand  (PK = site_id; use site_id directly)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_login_brand'
          AND policyname = 'site_login_brand_site_scope'
    ) THEN
        CREATE POLICY "site_login_brand_site_scope" ON "public"."site_login_brand"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5s. site_security_config  (PK = site_id; use site_id directly)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_security_config'
          AND policyname = 'site_security_config_site_scope'
    ) THEN
        CREATE POLICY "site_security_config_site_scope" ON "public"."site_security_config"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5t. scan_runs  (key = site_id)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'scan_runs'
          AND policyname = 'scan_runs_site_scope'
    ) THEN
        CREATE POLICY "scan_runs_site_scope" ON "public"."scan_runs"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5u. scan_findings  (key = site_id)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'scan_findings'
          AND policyname = 'scan_findings_site_scope'
    ) THEN
        CREATE POLICY "scan_findings_site_scope" ON "public"."scan_findings"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5v. scan_run_hashes  (indirect; no site_id — join via run_id→scan_runs)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'scan_run_hashes'
          AND policyname = 'scan_run_hashes_site_scope'
    ) THEN
        CREATE POLICY "scan_run_hashes_site_scope" ON "public"."scan_run_hashes"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "run_id" IN (
                    SELECT "id" FROM "public"."scan_runs"
                    WHERE "site_id" = ANY (
                        string_to_array(
                            nullif(current_setting('app.allowed_site_ids', true), ''), ','
                        )::uuid[]
                    )
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "run_id" IN (
                    SELECT "id" FROM "public"."scan_runs"
                    WHERE "site_id" = ANY (
                        string_to_array(
                            nullif(current_setting('app.allowed_site_ids', true), ''), ','
                        )::uuid[]
                    )
                )
            );
    END IF;
END;
$$;

-- ===========================================================================
-- 6. Indirect children — join-based RESTRICTIVE policies
-- ===========================================================================

-- ---------------------------------------------------------------------------
-- 6a. backup_manifest_entries  (no site_id; join via snapshot_id→backup_snapshots)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'backup_manifest_entries'
          AND policyname = 'backup_manifest_entries_site_scope'
    ) THEN
        CREATE POLICY "backup_manifest_entries_site_scope" ON "public"."backup_manifest_entries"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "snapshot_id" IN (
                    SELECT "id" FROM "public"."backup_snapshots"
                    WHERE "site_id" = ANY (
                        string_to_array(
                            nullif(current_setting('app.allowed_site_ids', true), ''), ','
                        )::uuid[]
                    )
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "snapshot_id" IN (
                    SELECT "id" FROM "public"."backup_snapshots"
                    WHERE "site_id" = ANY (
                        string_to_array(
                            nullif(current_setting('app.allowed_site_ids', true), ''), ','
                        )::uuid[]
                    )
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 6b. restore_run_events  (no site_id; join via restore_run_id→restore_runs)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'restore_run_events'
          AND policyname = 'restore_run_events_site_scope'
    ) THEN
        CREATE POLICY "restore_run_events_site_scope" ON "public"."restore_run_events"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "restore_run_id" IN (
                    SELECT "id" FROM "public"."restore_runs"
                    WHERE "site_id" = ANY (
                        string_to_array(
                            nullif(current_setting('app.allowed_site_ids', true), ''), ','
                        )::uuid[]
                    )
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "restore_run_id" IN (
                    SELECT "id" FROM "public"."restore_runs"
                    WHERE "site_id" = ANY (
                        string_to_array(
                            nullif(current_setting('app.allowed_site_ids', true), ''), ','
                        )::uuid[]
                    )
                )
            );
    END IF;
END;
$$;
