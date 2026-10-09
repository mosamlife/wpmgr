-- m174: approval tiers, Merge A. A person chooses, per site, how much an AI
-- connection may change without a click. The database records which setting
-- allowed each automatic approval, and re-checks that setting when the
-- approval is made and again when the change is sent.
--
-- ===========================================================================
-- WHAT THIS REPLACES
-- ===========================================================================
--
-- m156's header says "No automation approves a row.", and m151's says the
-- same of cache clears. Under ADR-065 that sentence is superseded: a request
-- is approved by a person, or by the site's setting that a named person
-- chose ('policy'). A setting never loosens itself, the AI cannot change it,
-- and the person it names must still hold the authority to have chosen it
-- (checked by the control plane on every request). m151 and m156 are not
-- rewritten; this header states what replaced them.
--
-- ===========================================================================
-- THE PARTS
-- ===========================================================================
--
--   1. ai_mode_allows(mode, class): the SQL copy of the mode matrix, the one
--      the approving statements, the reservation and the backstop consult.
--   2. sites: the site's AI mode (ai_mode*), the backfill of sites that
--      already have AI editing on, and the sites_ai_mode_guard trigger.
--   3. mcp_grants: the connection's switch (ai_auto*), its backfill, its
--      guard trigger, and UNIQUE (tenant_id, id).
--   4. mcp_grant_runs_by_setting(tenant, grant): one boolean about one grant.
--   5. Both request tables: who or what approved, the change class, why a
--      request asked, the raw checked target status, their CHECKs, grants and
--      indexes.
--   6. ai_approval_backstop, a trigger on both request tables.
--   7. The catalogues' change classes, their seeds, and the superadmin
--      definer set_ability_change_class().
--
-- ===========================================================================
-- THE MATRIX
-- ===========================================================================
--
-- Modes: 'ask' (every change waits for a person), 'ai_drafts' (Auto for AI
-- drafts), 'full' (Full auto on this site). Classes: ai_draft, operational,
-- unpublished, live, publish, update, always_ask ("the seven"), plus
-- by_target_status, a STORED value only: the control plane derives the
-- effective class from the checked target, and ai_mode_allows() is false for
-- it in every mode.
--
--                 ask   ai_drafts   full
--   ai_draft      no    yes         yes
--   operational   no    yes         yes
--   unpublished   no    no          yes
--   live          no    no          yes
--   publish       no    no          yes
--   update        no    no          yes
--   always_ask    no    no          no
--
-- A Go table holds the same matrix; a test compares every pair.
--
-- ===========================================================================
-- THE SITE MODE (part 2)
-- ===========================================================================
--
-- Columns on sites, not a table: one row per site by construction, and the
-- columns inherit sites' row security (ENABLE + FORCE, tenant isolation, the
-- RESTRICTIVE sites_site_scope). sites' grants are table-level, so the new
-- columns need no GRANT.
--
-- ai_mode_set_by has no foreign key: a deleted account must not fail a
-- delete, and a setting whose person is gone does nothing until a person
-- chooses again (the control plane checks the setter on every request).
--
-- sites_ai_mode_guard fires for every role and every transaction kind,
-- including the agent and enrolment paths:
--
--   * an INSERT carries the defaults (ask, unset, version 0, no setter);
--   * ai_mode_version moves only by exactly +1, and only with a change of
--     ai_mode, ai_mode_set_by or ai_mode_source; ai_mode_set_at and
--     ai_mode_step_up move only with such a change;
--   * a runtime change never writes source 'unset', 'migration' or
--     'launch_default': only this file's backfill, which runs while the
--     trigger is absent, produces those;
--   * a recorded setter is the signed-in person making the change
--     (app.user_id), and a mode above ask always names one;
--   * with no signed-in person the only change is down to ask, recorded as
--     'tightened', with no setter (how an API key lowers a site);
--   * 'enable_default' is written only from 'unset' (turning AI editing on
--     again never resets a choice a person made).
--
-- Every comparison is IS DISTINCT FROM, so NULL on either side is a change.
-- An app.user_id of the all-zero uuid never counts as a person.
--
-- ===========================================================================
-- THE BACKFILL OF SITES WITH AI EDITING ON (part 2)
-- ===========================================================================
--
-- A site has AI editing on when content_editing_enabled_at is set (m157).
--
--   * on, with the enabling person on record: ai_drafts, source
--     'launch_default', setter content_editing_enabled_by, version 1;
--   * on, with that account deleted (the column is ON DELETE SET NULL):
--     ask, source 'migration', no setter, version 1;
--   * every other site keeps the defaults (ask, unset, version 0).
--
-- sites is FORCE ROW LEVEL SECURITY and the production migrator sets no
-- app.* setting, so an UPDATE run as it would touch no row. The backfill uses
-- m142's pattern: FORCE is lifted for the statement, row_security is off (a
-- statement a policy would still filter raises instead of touching fewer
-- rows), FORCE is restored, and the file refuses to commit unless sites ends
-- ENABLE and FORCE. The guard trigger is dropped before the backfill and
-- created after it, in this same transaction.
--
-- Whether the setter still holds the authority the mode needs is checked by
-- the control plane on every request, not here.
--
-- ===========================================================================
-- THE CONNECTION SWITCH (part 3)
-- ===========================================================================
--
-- mcp_grants.ai_auto is 'site_setting' (the connection runs a change when the
-- site's mode allows it) or 'never' (every change waits for a person). A
-- connection can only narrow a site's mode. The DEFAULT 'never' fills every
-- existing row by DDL. The backfill then gives every grant a person created
-- (created_by_user_id set) 'site_setting' with that person as ai_auto_set_by,
-- in m142's pattern; a grant minted with an API key (no creator) stays
-- 'never'. mcp_grants_ai_auto_guard requires the signed-in person to be the
-- recorded setter whenever a row becomes 'site_setting' or changes setter;
-- moving to 'never' needs no person.
--
-- UNIQUE (tenant_id, id) is the target for the composite foreign keys a
-- later migration adds.
--
-- ===========================================================================
-- ONE BOOLEAN ABOUT ONE GRANT (part 4)
-- ===========================================================================
--
-- The dispatch reservation runs in a transaction scoped to one site, where
-- mcp_grants_site_scope_select hides every grant. The reservation must still
-- see whether the request's connection runs by the site's setting at the
-- moment it is sent. mcp_grant_runs_by_setting() answers exactly that for one
-- grant id: it runs with app.site_scope cleared for its own duration, the
-- tenant policy still applies, and it returns a boolean and nothing else.
--
-- ===========================================================================
-- THE REQUEST COLUMNS (part 5)
-- ===========================================================================
--
-- On assistant_ability_requests and assistant_cache_purge_requests:
--
--   approval_source          'person' (default), 'policy' or 'session'
--   approval_site_mode       the site mode a 'policy' approval relied on
--   approval_mode_source     how that mode was chosen: 'launch_default',
--                            'enable_default' or 'person' (a 'policy'
--                            approval only)
--   approval_mode_version    that mode's version
--   approval_setter_user_id  the site's setter (or a session's approver)
--   approval_setter_set_at   when that setter chose the mode
--   approval_session_id      a session approval's session (its foreign key
--                            arrives with the sessions table)
--   base_change_class        the entry's or route's stored class at decision
--   change_class             the effective class (never by_target_status)
--   ask_reason               why a request waits for a person
--   policy_checked_at        when the control plane decided it
--
-- and, on assistant_ability_requests only, checked_target_status: the raw
-- post status the precheck checked, written at creation and never changed.
--
-- The DEFAULT 'person' fills existing rows by DDL; no UPDATE runs on these
-- FORCE ROW LEVEL SECURITY tables. Every existing row satisfies every new
-- CHECK: an existing row is 'person' with every new column NULL.
--
-- approval_names_a_human_check keeps its m151 and m156 name and now reads "a
-- person's approval names a human". Two new CHECKs give the shape of a
-- 'policy' approval and of a 'session' approval. An automatic approval never
-- names a decider: the setter did not decide that request. Only a 'policy'
-- approval records a mode source, and an approved 'policy' row always does.
--
-- The activity feed lists approved requests newest first, keyset on
-- (created_at, id); each table has a partial index on its approved states
-- for it.
--
-- The UPDATE grants gain every new column except checked_target_status.
--
-- ===========================================================================
-- THE BACKSTOP (part 6)
-- ===========================================================================
--
-- ai_approval_backstop, BEFORE INSERT OR UPDATE, on both request tables, for
-- every role:
--
--   1. A request is inserted pending, with approval_source 'person' and every
--      approval, class and check column empty.
--   2. checked_target_status never changes after insert.
--   3. base_change_class, change_class, ask_reason and policy_checked_at are
--      written once, together with policy_checked_at, while the request is
--      pending: by the write that leaves it waiting, or by the statement that
--      approves it. After that none of them changes.
--   4. The approval columns change only when a pending request is approved.
--   5. A request enters the approved states only from pending, and only into
--      'approved' (ability) or 'approved_undispatched' (cache clear).
--   6. 'person': a named decider must be the signed-in person in the same
--      transaction (app.user_id). A missing decider is refused by
--      approval_names_a_human_check.
--   7. 'policy': no user in the transaction; the site's current mode, mode
--      source, version, setter and set time are the ones recorded on the
--      request,
--      ai_mode_allows(mode, change_class) holds, and the connection is active
--      and runs by the site's setting.
--   8. 'session': refused until the migration that adds sessions replaces
--      this function.
--
-- ===========================================================================
-- THE CATALOGUE CLASSES (part 7)
-- ===========================================================================
--
-- ability_catalogue.change_class and rest_route_catalogue.change_class, NOT
-- NULL DEFAULT 'always_ask': an entry nobody has classed waits for a person.
-- ability_catalogue_row_sha256() and the admin upserts are unchanged, so no
-- entry or route hash moves. For wpmgr/rest-write the route's class is the
-- stored class; the entry's own class is not consulted.
--
-- Seeds: wpmgr/page-create is 'ai_draft'; the two REST write routes
-- (wp-v2-pages-update-fields, wp-v2-posts-update-fields) are
-- 'by_target_status'. Both catalogues are ENABLE, not FORCE, row level
-- security and the migration role owns them, so each seed is a plain UPDATE.
-- A missing row raises a NOTICE and stays on the default, which waits for a
-- person.
--
-- set_ability_change_class(actor, kind, id, class) is the only runtime path
-- that re-classes an entry or route: SECURITY DEFINER, search_path pinned,
-- the actor must be a superadmin, the writer's per-name lock is taken, and
-- the audit row naming the old and new class is written in the same
-- statement. The audit tables gain before_change_class and
-- after_change_class, both set on a re-class and both NULL otherwise.
--
-- ===========================================================================
-- FIRST TRIGGERS IN THIS TREE
-- ===========================================================================
--
-- m133 section 6 recorded the decision not to add a trigger then, and that a
-- transition guard would need one. The guards in parts 2, 3 and 6 are
-- transition guards: they compare a row with what it was. That is the
-- decision being overturned, deliberately, here.
--
-- ===========================================================================
-- CONVERGE PATH AND RE-RUN
-- ===========================================================================
--
-- None needed: m174 is new. ADD COLUMN IF NOT EXISTS, DROP CONSTRAINT IF
-- EXISTS before each ADD, CREATE OR REPLACE FUNCTION, DROP TRIGGER IF EXISTS
-- before each CREATE TRIGGER, guarded UNIQUE and seeds make a re-run converge
-- on the same end state. A re-run's backfills touch only rows still on their
-- DDL defaults (sites still 'unset' with AI editing on; person-created grants
-- never switched since), which is the same rule the first run applied.
--
-- LOCK WAIT. Each lock is waited for at most five seconds; a timeout rolls the
-- file back and fails the boot with the previous revision left serving.

SET LOCAL lock_timeout = '5s';

-- ===========================================================================
-- (1) ai_mode_allows
-- ===========================================================================

CREATE OR REPLACE FUNCTION "public"."ai_mode_allows"(p_mode text, p_class text)
RETURNS boolean
LANGUAGE sql
IMMUTABLE PARALLEL SAFE
SET search_path = public, pg_temp
AS $$
    SELECT coalesce(
        CASE p_mode
            WHEN 'ai_drafts' THEN p_class IN ('ai_draft', 'operational')
            WHEN 'full'      THEN p_class IN ('ai_draft', 'operational', 'unpublished',
                                              'live', 'publish', 'update')
            ELSE false
        END,
        false);
$$;

REVOKE ALL ON FUNCTION "public"."ai_mode_allows"(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION "public"."ai_mode_allows"(text, text) TO "wpmgr_app";

-- ===========================================================================
-- (2) sites: the site mode
-- ===========================================================================

ALTER TABLE "public"."sites"
    ADD COLUMN IF NOT EXISTS "ai_mode"                   text        NOT NULL DEFAULT 'ask',
    ADD COLUMN IF NOT EXISTS "ai_mode_source"            text        NOT NULL DEFAULT 'unset',
    ADD COLUMN IF NOT EXISTS "ai_mode_set_by"            uuid        NULL,
    ADD COLUMN IF NOT EXISTS "ai_mode_set_at"            timestamptz NULL,
    ADD COLUMN IF NOT EXISTS "ai_mode_version"           bigint      NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS "ai_mode_step_up"           text        NULL,
    ADD COLUMN IF NOT EXISTS "ai_mode_launch_emailed_at" timestamptz NULL;

ALTER TABLE "public"."sites"
    DROP CONSTRAINT IF EXISTS "sites_ai_mode_check",
    DROP CONSTRAINT IF EXISTS "sites_ai_mode_source_check",
    DROP CONSTRAINT IF EXISTS "sites_ai_mode_step_up_check",
    DROP CONSTRAINT IF EXISTS "sites_ai_mode_version_check",
    DROP CONSTRAINT IF EXISTS "sites_ai_mode_set_by_not_nil_check",
    DROP CONSTRAINT IF EXISTS "sites_ai_mode_unattributed_is_ask_check",
    DROP CONSTRAINT IF EXISTS "sites_ai_mode_above_ask_names_setter_check",
    DROP CONSTRAINT IF EXISTS "sites_ai_mode_default_is_ai_drafts_check",
    DROP CONSTRAINT IF EXISTS "sites_ai_mode_full_has_step_up_check";

ALTER TABLE "public"."sites"
    ADD CONSTRAINT "sites_ai_mode_check"
        CHECK ("ai_mode" IN ('ask', 'ai_drafts', 'full')),
    ADD CONSTRAINT "sites_ai_mode_source_check"
        CHECK ("ai_mode_source" IN ('unset', 'migration', 'launch_default',
                                    'enable_default', 'person', 'tightened')),
    ADD CONSTRAINT "sites_ai_mode_step_up_check"
        CHECK ("ai_mode_step_up" IS NULL
               OR "ai_mode_step_up" IN ('password', 'totp', 'recent_sign_in')),
    ADD CONSTRAINT "sites_ai_mode_version_check"
        CHECK ("ai_mode_version" >= 0),
    ADD CONSTRAINT "sites_ai_mode_set_by_not_nil_check"
        CHECK ("ai_mode_set_by" IS NULL
               OR "ai_mode_set_by" <> '00000000-0000-0000-0000-000000000000'::uuid),
    -- A row no person chose can only be Ask, with no setter.
    ADD CONSTRAINT "sites_ai_mode_unattributed_is_ask_check"
        CHECK ("ai_mode_source" NOT IN ('unset', 'migration', 'tightened')
               OR ("ai_mode" = 'ask' AND "ai_mode_set_by" IS NULL)),
    -- Nothing above Ask without a named person.
    ADD CONSTRAINT "sites_ai_mode_above_ask_names_setter_check"
        CHECK ("ai_mode" = 'ask' OR "ai_mode_set_by" IS NOT NULL),
    -- A default is Auto for AI drafts and nothing else.
    ADD CONSTRAINT "sites_ai_mode_default_is_ai_drafts_check"
        CHECK ("ai_mode_source" NOT IN ('launch_default', 'enable_default')
               OR "ai_mode" = 'ai_drafts'),
    -- Full auto records the second check its person passed.
    ADD CONSTRAINT "sites_ai_mode_full_has_step_up_check"
        CHECK ("ai_mode" <> 'full' OR "ai_mode_step_up" IS NOT NULL);

-- The backfill runs while the guard is absent; the guard is created after it.
DROP TRIGGER IF EXISTS "sites_ai_mode_guard" ON "public"."sites";

DO $$
DECLARE
    v_with_setter    bigint;
    v_without_setter bigint;
BEGIN
    ALTER TABLE "public"."sites" NO FORCE ROW LEVEL SECURITY;
    PERFORM set_config('row_security', 'off', true);

    UPDATE "public"."sites"
    SET "ai_mode"         = 'ai_drafts',
        "ai_mode_source"  = 'launch_default',
        "ai_mode_set_by"  = "content_editing_enabled_by",
        "ai_mode_set_at"  = now(),
        "ai_mode_version" = 1
    WHERE "content_editing_enabled_at" IS NOT NULL
      AND "content_editing_enabled_by" IS NOT NULL
      AND "content_editing_enabled_by" <> '00000000-0000-0000-0000-000000000000'::uuid
      AND "ai_mode_source" = 'unset';
    GET DIAGNOSTICS v_with_setter = ROW_COUNT;

    UPDATE "public"."sites"
    SET "ai_mode"         = 'ask',
        "ai_mode_source"  = 'migration',
        "ai_mode_set_by"  = NULL,
        "ai_mode_set_at"  = now(),
        "ai_mode_version" = 1
    WHERE "content_editing_enabled_at" IS NOT NULL
      AND ("content_editing_enabled_by" IS NULL
           OR "content_editing_enabled_by" = '00000000-0000-0000-0000-000000000000'::uuid)
      AND "ai_mode_source" = 'unset';
    GET DIAGNOSTICS v_without_setter = ROW_COUNT;

    PERFORM set_config('row_security', 'on', true);
    ALTER TABLE "public"."sites" FORCE ROW LEVEL SECURITY;

    RAISE NOTICE 'm174: % sites with AI editing on now run AI drafts automatically; % whose enabling account is gone stay on Ask',
        v_with_setter, v_without_setter;

    IF NOT EXISTS (
        SELECT 1 FROM pg_class
        WHERE oid = 'public.sites'::regclass
          AND relrowsecurity
          AND relforcerowsecurity
    ) THEN
        RAISE EXCEPTION 'm174: sites must end with ENABLE and FORCE ROW LEVEL SECURITY';
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION "public"."sites_ai_mode_guard"()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = public, pg_temp
AS $$
DECLARE
    v_raw     text := nullif(current_setting('app.user_id', true), '');
    v_user    uuid;
    v_changed boolean;
BEGIN
    IF v_raw IS NOT NULL AND v_raw <> '00000000-0000-0000-0000-000000000000' THEN
        v_user := v_raw::uuid;
    END IF;

    IF TG_OP = 'INSERT' THEN
        IF NEW.ai_mode IS DISTINCT FROM 'ask'
           OR NEW.ai_mode_source IS DISTINCT FROM 'unset'
           OR NEW.ai_mode_set_by IS NOT NULL
           OR NEW.ai_mode_set_at IS NOT NULL
           OR NEW.ai_mode_version IS DISTINCT FROM 0
           OR NEW.ai_mode_step_up IS NOT NULL THEN
            RAISE EXCEPTION 'sites_ai_mode_guard: a new site starts on ask, unset, version 0, with no setter'
                USING ERRCODE = '42501';
        END IF;
        RETURN NEW;
    END IF;

    v_changed := NEW.ai_mode IS DISTINCT FROM OLD.ai_mode
              OR NEW.ai_mode_set_by IS DISTINCT FROM OLD.ai_mode_set_by
              OR NEW.ai_mode_source IS DISTINCT FROM OLD.ai_mode_source;

    IF NOT v_changed THEN
        IF NEW.ai_mode_version IS DISTINCT FROM OLD.ai_mode_version
           OR NEW.ai_mode_set_at IS DISTINCT FROM OLD.ai_mode_set_at
           OR NEW.ai_mode_step_up IS DISTINCT FROM OLD.ai_mode_step_up THEN
            RAISE EXCEPTION 'sites_ai_mode_guard: the version, set time and step-up move only with a change of mode, setter or source'
                USING ERRCODE = '55000';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.ai_mode_version IS DISTINCT FROM OLD.ai_mode_version + 1 THEN
        RAISE EXCEPTION 'sites_ai_mode_guard: a change of mode, setter or source moves the version by exactly one'
            USING ERRCODE = '55000';
    END IF;

    IF NEW.ai_mode_source IN ('unset', 'migration', 'launch_default') THEN
        RAISE EXCEPTION 'sites_ai_mode_guard: source % is never written at run time', NEW.ai_mode_source
            USING ERRCODE = '42501';
    END IF;

    IF NEW.ai_mode_source = 'enable_default' AND OLD.ai_mode_source IS DISTINCT FROM 'unset' THEN
        RAISE EXCEPTION 'sites_ai_mode_guard: the default applies only to a site no person has set'
            USING ERRCODE = '55000';
    END IF;

    IF NEW.ai_mode_set_by IS NOT NULL AND NEW.ai_mode_set_by IS DISTINCT FROM v_user THEN
        RAISE EXCEPTION 'sites_ai_mode_guard: the recorded setter is the signed-in person making the change'
            USING ERRCODE = '42501';
    END IF;

    IF NEW.ai_mode <> 'ask' AND NEW.ai_mode_source NOT IN ('person', 'enable_default') THEN
        RAISE EXCEPTION 'sites_ai_mode_guard: a mode above ask is a person''s choice'
            USING ERRCODE = '42501';
    END IF;

    IF v_user IS NULL
       AND (NEW.ai_mode <> 'ask'
            OR NEW.ai_mode_source <> 'tightened'
            OR NEW.ai_mode_set_by IS NOT NULL) THEN
        RAISE EXCEPTION 'sites_ai_mode_guard: with no signed-in person the only change is down to ask, recorded as tightened'
            USING ERRCODE = '42501';
    END IF;

    RETURN NEW;
END;
$$;

REVOKE ALL ON FUNCTION "public"."sites_ai_mode_guard"() FROM PUBLIC;

CREATE TRIGGER "sites_ai_mode_guard"
    BEFORE INSERT OR UPDATE OF "ai_mode", "ai_mode_source", "ai_mode_set_by",
                               "ai_mode_set_at", "ai_mode_version", "ai_mode_step_up"
    ON "public"."sites"
    FOR EACH ROW
    EXECUTE FUNCTION "public"."sites_ai_mode_guard"();

-- ===========================================================================
-- (3) mcp_grants: the connection switch
-- ===========================================================================

ALTER TABLE "public"."mcp_grants"
    ADD COLUMN IF NOT EXISTS "ai_auto"        text        NOT NULL DEFAULT 'never',
    ADD COLUMN IF NOT EXISTS "ai_auto_set_by" uuid        NULL,
    ADD COLUMN IF NOT EXISTS "ai_auto_set_at" timestamptz NULL;

ALTER TABLE "public"."mcp_grants"
    DROP CONSTRAINT IF EXISTS "mcp_grants_ai_auto_check",
    DROP CONSTRAINT IF EXISTS "mcp_grants_ai_auto_names_setter_check",
    DROP CONSTRAINT IF EXISTS "mcp_grants_ai_auto_set_by_not_nil_check";

ALTER TABLE "public"."mcp_grants"
    ADD CONSTRAINT "mcp_grants_ai_auto_check"
        CHECK ("ai_auto" IN ('site_setting', 'never')),
    ADD CONSTRAINT "mcp_grants_ai_auto_names_setter_check"
        CHECK ("ai_auto" = 'never' OR "ai_auto_set_by" IS NOT NULL),
    ADD CONSTRAINT "mcp_grants_ai_auto_set_by_not_nil_check"
        CHECK ("ai_auto_set_by" IS NULL
               OR "ai_auto_set_by" <> '00000000-0000-0000-0000-000000000000'::uuid);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.mcp_grants'::regclass
          AND conname  = 'mcp_grants_tenant_id_id_key'
    ) THEN
        ALTER TABLE "public"."mcp_grants"
            ADD CONSTRAINT "mcp_grants_tenant_id_id_key" UNIQUE ("tenant_id", "id");
    END IF;
END;
$$;

DROP TRIGGER IF EXISTS "mcp_grants_ai_auto_guard" ON "public"."mcp_grants";

DO $$
DECLARE
    v_grants bigint;
BEGIN
    ALTER TABLE "public"."mcp_grants" NO FORCE ROW LEVEL SECURITY;
    PERFORM set_config('row_security', 'off', true);

    UPDATE "public"."mcp_grants"
    SET "ai_auto"        = 'site_setting',
        "ai_auto_set_by" = "created_by_user_id",
        "ai_auto_set_at" = now()
    WHERE "created_by_user_id" IS NOT NULL
      AND "created_by_user_id" <> '00000000-0000-0000-0000-000000000000'::uuid
      AND "ai_auto" = 'never'
      AND "ai_auto_set_by" IS NULL
      AND "ai_auto_set_at" IS NULL;
    GET DIAGNOSTICS v_grants = ROW_COUNT;

    PERFORM set_config('row_security', 'on', true);
    ALTER TABLE "public"."mcp_grants" FORCE ROW LEVEL SECURITY;

    RAISE NOTICE 'm174: % connections a person created now run by each site''s setting; connections minted with an API key stay on never',
        v_grants;

    IF NOT EXISTS (
        SELECT 1 FROM pg_class
        WHERE oid = 'public.mcp_grants'::regclass
          AND relrowsecurity
          AND relforcerowsecurity
    ) THEN
        RAISE EXCEPTION 'm174: mcp_grants must end with ENABLE and FORCE ROW LEVEL SECURITY';
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION "public"."mcp_grants_ai_auto_guard"()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = public, pg_temp
AS $$
DECLARE
    v_raw     text := nullif(current_setting('app.user_id', true), '');
    v_user    uuid;
    v_raising boolean;
BEGIN
    IF v_raw IS NOT NULL AND v_raw <> '00000000-0000-0000-0000-000000000000' THEN
        v_user := v_raw::uuid;
    END IF;

    IF NEW.ai_auto IS DISTINCT FROM 'site_setting' THEN
        RETURN NEW;
    END IF;

    IF TG_OP = 'INSERT' THEN
        v_raising := true;
    ELSE
        v_raising := OLD.ai_auto IS DISTINCT FROM 'site_setting'
                  OR NEW.ai_auto_set_by IS DISTINCT FROM OLD.ai_auto_set_by;
    END IF;

    IF v_raising AND (v_user IS NULL OR NEW.ai_auto_set_by IS DISTINCT FROM v_user) THEN
        RAISE EXCEPTION 'mcp_grants_ai_auto_guard: only the signed-in person recorded as its setter lets a connection run changes by the site''s setting'
            USING ERRCODE = '42501';
    END IF;

    RETURN NEW;
END;
$$;

REVOKE ALL ON FUNCTION "public"."mcp_grants_ai_auto_guard"() FROM PUBLIC;

CREATE TRIGGER "mcp_grants_ai_auto_guard"
    BEFORE INSERT OR UPDATE OF "ai_auto", "ai_auto_set_by"
    ON "public"."mcp_grants"
    FOR EACH ROW
    EXECUTE FUNCTION "public"."mcp_grants_ai_auto_guard"();

-- ===========================================================================
-- (4) mcp_grant_runs_by_setting
-- ===========================================================================

-- app.site_scope is cleared transaction-locally for the one read and put back
-- before the function returns; an error aborts the (sub)transaction, which
-- rolls the setting back with it.
CREATE OR REPLACE FUNCTION "public"."mcp_grant_runs_by_setting"(p_tenant_id uuid, p_grant_id uuid)
RETURNS boolean
LANGUAGE plpgsql
VOLATILE
SET search_path = public, pg_temp
AS $$
DECLARE
    v_scope text := current_setting('app.site_scope', true);
    v_ok    boolean;
BEGIN
    PERFORM set_config('app.site_scope', '', true);
    SELECT EXISTS (
        SELECT 1
        FROM mcp_grants g
        WHERE g.tenant_id = p_tenant_id
          AND g.id = p_grant_id
          AND g.status = 'active'
          AND g.ai_auto = 'site_setting'
    ) INTO v_ok;
    PERFORM set_config('app.site_scope', coalesce(v_scope, ''), true);
    RETURN coalesce(v_ok, false);
END;
$$;

REVOKE ALL ON FUNCTION "public"."mcp_grant_runs_by_setting"(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION "public"."mcp_grant_runs_by_setting"(uuid, uuid) TO "wpmgr_app";

-- ===========================================================================
-- (5) The request columns
-- ===========================================================================

ALTER TABLE "public"."assistant_ability_requests"
    ADD COLUMN IF NOT EXISTS "approval_source"         text        NOT NULL DEFAULT 'person',
    ADD COLUMN IF NOT EXISTS "approval_site_mode"      text        NULL,
    ADD COLUMN IF NOT EXISTS "approval_mode_source"    text        NULL,
    ADD COLUMN IF NOT EXISTS "approval_mode_version"   bigint      NULL,
    ADD COLUMN IF NOT EXISTS "approval_setter_user_id" uuid        NULL,
    ADD COLUMN IF NOT EXISTS "approval_setter_set_at"  timestamptz NULL,
    ADD COLUMN IF NOT EXISTS "approval_session_id"     uuid        NULL,
    ADD COLUMN IF NOT EXISTS "base_change_class"       text        NULL,
    ADD COLUMN IF NOT EXISTS "change_class"            text        NULL,
    ADD COLUMN IF NOT EXISTS "ask_reason"              text        NULL,
    ADD COLUMN IF NOT EXISTS "policy_checked_at"       timestamptz NULL,
    ADD COLUMN IF NOT EXISTS "checked_target_status"   text        NULL;

ALTER TABLE "public"."assistant_cache_purge_requests"
    ADD COLUMN IF NOT EXISTS "approval_source"         text        NOT NULL DEFAULT 'person',
    ADD COLUMN IF NOT EXISTS "approval_site_mode"      text        NULL,
    ADD COLUMN IF NOT EXISTS "approval_mode_source"    text        NULL,
    ADD COLUMN IF NOT EXISTS "approval_mode_version"   bigint      NULL,
    ADD COLUMN IF NOT EXISTS "approval_setter_user_id" uuid        NULL,
    ADD COLUMN IF NOT EXISTS "approval_setter_set_at"  timestamptz NULL,
    ADD COLUMN IF NOT EXISTS "approval_session_id"     uuid        NULL,
    ADD COLUMN IF NOT EXISTS "base_change_class"       text        NULL,
    ADD COLUMN IF NOT EXISTS "change_class"            text        NULL,
    ADD COLUMN IF NOT EXISTS "ask_reason"              text        NULL,
    ADD COLUMN IF NOT EXISTS "policy_checked_at"       timestamptz NULL;

ALTER TABLE "public"."assistant_ability_requests"
    DROP CONSTRAINT IF EXISTS "assistant_ability_requests_approval_names_a_human_check",
    DROP CONSTRAINT IF EXISTS "assistant_ability_requests_not_sent_reason_check",
    DROP CONSTRAINT IF EXISTS "assistant_ability_requests_approval_source_check",
    DROP CONSTRAINT IF EXISTS "assistant_ability_requests_approval_site_mode_check",
    DROP CONSTRAINT IF EXISTS "assistant_ability_requests_approval_mode_source_check",
    DROP CONSTRAINT IF EXISTS "assistant_ability_requests_approval_mode_version_check",
    DROP CONSTRAINT IF EXISTS "assistant_ability_requests_approval_setter_not_nil_check",
    DROP CONSTRAINT IF EXISTS "assistant_ability_requests_base_change_class_check",
    DROP CONSTRAINT IF EXISTS "assistant_ability_requests_change_class_check",
    DROP CONSTRAINT IF EXISTS "assistant_ability_requests_ask_reason_check",
    DROP CONSTRAINT IF EXISTS "assistant_ability_requests_checked_target_status_check",
    DROP CONSTRAINT IF EXISTS "assistant_ability_requests_policy_approval_shape_check",
    DROP CONSTRAINT IF EXISTS "assistant_ability_requests_session_approval_shape_check";

ALTER TABLE "public"."assistant_ability_requests"
    -- A person's approval names a human (m156's name, kept so the code and
    -- tests that name it keep working).
    ADD CONSTRAINT "assistant_ability_requests_approval_names_a_human_check"
        CHECK ("state" NOT IN ('approved', 'dispatched', 'done', 'failed',
                               'not_sent', 'outcome_unknown')
               OR "approval_source" <> 'person'
               OR "decided_by_user_id" IS NOT NULL),
    -- m161's set plus the closes for a setting, session or class that changed
    -- between an automatic approval and the send.
    ADD CONSTRAINT "assistant_ability_requests_not_sent_reason_check"
        CHECK ("not_sent_reason" IN (
            'grant_inactive',
            'assistant_paused',
            'organisation_deleted',
            'capability_not_held',
            'site_absent',
            'forbidden_by_context',
            'agent_outdated',
            'dispatch_deadline_passed',
            'transport_pre_send',
            'entry_changed',
            'entry_disabled',
            'route_changed',
            'route_disabled',
            'setting_changed',
            'session_ended',
            'class_changed'
        )),
    ADD CONSTRAINT "assistant_ability_requests_approval_source_check"
        CHECK ("approval_source" IN ('person', 'policy', 'session')),
    ADD CONSTRAINT "assistant_ability_requests_approval_site_mode_check"
        CHECK ("approval_site_mode" IS NULL OR "approval_site_mode" IN ('ai_drafts', 'full')),
    -- How the mode a 'policy' approval relied on was chosen. Only a
    -- 'policy' approval records it.
    ADD CONSTRAINT "assistant_ability_requests_approval_mode_source_check"
        CHECK ("approval_mode_source" IS NULL
               OR ("approval_source" = 'policy'
                   AND "approval_mode_source" IN ('launch_default', 'enable_default', 'person'))),
    ADD CONSTRAINT "assistant_ability_requests_approval_mode_version_check"
        CHECK ("approval_mode_version" IS NULL OR "approval_mode_version" > 0),
    ADD CONSTRAINT "assistant_ability_requests_approval_setter_not_nil_check"
        CHECK ("approval_setter_user_id" IS NULL
               OR "approval_setter_user_id" <> '00000000-0000-0000-0000-000000000000'::uuid),
    ADD CONSTRAINT "assistant_ability_requests_base_change_class_check"
        CHECK ("base_change_class" IS NULL OR "base_change_class" IN (
            'ai_draft', 'operational', 'unpublished', 'live', 'publish', 'update',
            'always_ask', 'by_target_status')),
    ADD CONSTRAINT "assistant_ability_requests_change_class_check"
        CHECK ("change_class" IS NULL OR "change_class" IN (
            'ai_draft', 'operational', 'unpublished', 'live', 'publish', 'update',
            'always_ask')),
    ADD CONSTRAINT "assistant_ability_requests_ask_reason_check"
        CHECK ("ask_reason" IS NULL OR "ask_reason" IN (
            'kind_always_asks',
            'unknown_target_state',
            'site_mode_ask',
            'kind_not_in_mode',
            'setter_lacks_permission',
            'over_change_budget',
            'over_site_cap',
            'connection_never_auto',
            'connection_setter_invalid',
            'not_checked',
            'visible_auto_held',
            'session_ended')),
    ADD CONSTRAINT "assistant_ability_requests_checked_target_status_check"
        CHECK ("checked_target_status" IS NULL OR char_length("checked_target_status") <= 64),
    ADD CONSTRAINT "assistant_ability_requests_policy_approval_shape_check"
        CHECK ("state" NOT IN ('approved', 'dispatched', 'done', 'failed',
                               'not_sent', 'outcome_unknown')
               OR "approval_source" <> 'policy'
               OR ("decided_by_user_id" IS NULL
                   AND "approval_site_mode" IS NOT NULL
                   AND "approval_mode_source" IS NOT NULL
                   AND "approval_mode_version" IS NOT NULL
                   AND "approval_setter_user_id" IS NOT NULL
                   AND "approval_setter_set_at" IS NOT NULL
                   AND "approval_session_id" IS NULL
                   AND "base_change_class" IS NOT NULL
                   AND "change_class" IS NOT NULL
                   AND "ask_reason" IS NULL
                   AND "policy_checked_at" IS NOT NULL)),
    ADD CONSTRAINT "assistant_ability_requests_session_approval_shape_check"
        CHECK ("state" NOT IN ('approved', 'dispatched', 'done', 'failed',
                               'not_sent', 'outcome_unknown')
               OR "approval_source" <> 'session'
               OR ("decided_by_user_id" IS NULL
                   AND "approval_session_id" IS NOT NULL
                   AND "approval_setter_user_id" IS NOT NULL
                   AND "approval_site_mode" IS NULL
                   AND "approval_mode_source" IS NULL
                   AND "approval_mode_version" IS NULL
                   AND "base_change_class" IS NOT NULL
                   AND "change_class" IS NOT NULL
                   AND "ask_reason" IS NULL
                   AND "policy_checked_at" IS NOT NULL));

ALTER TABLE "public"."assistant_cache_purge_requests"
    DROP CONSTRAINT IF EXISTS "assistant_cache_purge_requests_approval_names_a_human_check",
    DROP CONSTRAINT IF EXISTS "assistant_cache_purge_requests_not_sent_reason_check",
    DROP CONSTRAINT IF EXISTS "assistant_cache_purge_requests_approval_source_check",
    DROP CONSTRAINT IF EXISTS "assistant_cache_purge_requests_approval_site_mode_check",
    DROP CONSTRAINT IF EXISTS "assistant_cache_purge_requests_approval_mode_source_check",
    DROP CONSTRAINT IF EXISTS "assistant_cache_purge_requests_approval_mode_version_check",
    DROP CONSTRAINT IF EXISTS "assistant_cache_purge_requests_approval_setter_not_nil_check",
    DROP CONSTRAINT IF EXISTS "assistant_cache_purge_requests_base_change_class_check",
    DROP CONSTRAINT IF EXISTS "assistant_cache_purge_requests_change_class_check",
    DROP CONSTRAINT IF EXISTS "assistant_cache_purge_requests_ask_reason_check",
    DROP CONSTRAINT IF EXISTS "assistant_cache_purge_requests_policy_approval_shape_check",
    DROP CONSTRAINT IF EXISTS "assistant_cache_purge_requests_session_approval_shape_check";

ALTER TABLE "public"."assistant_cache_purge_requests"
    -- A person's approval names a human (m151's name, kept).
    ADD CONSTRAINT "assistant_cache_purge_requests_approval_names_a_human_check"
        CHECK ("state" NOT IN ('approved_undispatched', 'dispatched')
               OR "approval_source" <> 'person'
               OR "decided_by_user_id" IS NOT NULL),
    ADD CONSTRAINT "assistant_cache_purge_requests_not_sent_reason_check"
        CHECK ("not_sent_reason" IN (
            'grant_inactive',
            'assistant_paused',
            'organisation_deleted',
            'capability_not_held',
            'site_absent',
            'forbidden_by_context',
            'agent_outdated',
            'dispatch_deadline_passed',
            'transport_pre_send',
            'setting_changed',
            'session_ended',
            'class_changed'
        )),
    ADD CONSTRAINT "assistant_cache_purge_requests_approval_source_check"
        CHECK ("approval_source" IN ('person', 'policy', 'session')),
    ADD CONSTRAINT "assistant_cache_purge_requests_approval_site_mode_check"
        CHECK ("approval_site_mode" IS NULL OR "approval_site_mode" IN ('ai_drafts', 'full')),
    -- How the mode a 'policy' approval relied on was chosen. Only a
    -- 'policy' approval records it.
    ADD CONSTRAINT "assistant_cache_purge_requests_approval_mode_source_check"
        CHECK ("approval_mode_source" IS NULL
               OR ("approval_source" = 'policy'
                   AND "approval_mode_source" IN ('launch_default', 'enable_default', 'person'))),
    ADD CONSTRAINT "assistant_cache_purge_requests_approval_mode_version_check"
        CHECK ("approval_mode_version" IS NULL OR "approval_mode_version" > 0),
    ADD CONSTRAINT "assistant_cache_purge_requests_approval_setter_not_nil_check"
        CHECK ("approval_setter_user_id" IS NULL
               OR "approval_setter_user_id" <> '00000000-0000-0000-0000-000000000000'::uuid),
    ADD CONSTRAINT "assistant_cache_purge_requests_base_change_class_check"
        CHECK ("base_change_class" IS NULL OR "base_change_class" IN (
            'ai_draft', 'operational', 'unpublished', 'live', 'publish', 'update',
            'always_ask', 'by_target_status')),
    ADD CONSTRAINT "assistant_cache_purge_requests_change_class_check"
        CHECK ("change_class" IS NULL OR "change_class" IN (
            'ai_draft', 'operational', 'unpublished', 'live', 'publish', 'update',
            'always_ask')),
    ADD CONSTRAINT "assistant_cache_purge_requests_ask_reason_check"
        CHECK ("ask_reason" IS NULL OR "ask_reason" IN (
            'kind_always_asks',
            'unknown_target_state',
            'site_mode_ask',
            'kind_not_in_mode',
            'setter_lacks_permission',
            'over_change_budget',
            'over_site_cap',
            'connection_never_auto',
            'connection_setter_invalid',
            'not_checked',
            'visible_auto_held',
            'session_ended')),
    ADD CONSTRAINT "assistant_cache_purge_requests_policy_approval_shape_check"
        CHECK ("state" NOT IN ('approved_undispatched', 'dispatched')
               OR "approval_source" <> 'policy'
               OR ("decided_by_user_id" IS NULL
                   AND "approval_site_mode" IS NOT NULL
                   AND "approval_mode_source" IS NOT NULL
                   AND "approval_mode_version" IS NOT NULL
                   AND "approval_setter_user_id" IS NOT NULL
                   AND "approval_setter_set_at" IS NOT NULL
                   AND "approval_session_id" IS NULL
                   AND "base_change_class" IS NOT NULL
                   AND "change_class" IS NOT NULL
                   AND "ask_reason" IS NULL
                   AND "policy_checked_at" IS NOT NULL)),
    ADD CONSTRAINT "assistant_cache_purge_requests_session_approval_shape_check"
        CHECK ("state" NOT IN ('approved_undispatched', 'dispatched')
               OR "approval_source" <> 'session'
               OR ("decided_by_user_id" IS NULL
                   AND "approval_session_id" IS NOT NULL
                   AND "approval_setter_user_id" IS NOT NULL
                   AND "approval_site_mode" IS NULL
                   AND "approval_mode_source" IS NULL
                   AND "approval_mode_version" IS NULL
                   AND "base_change_class" IS NOT NULL
                   AND "change_class" IS NOT NULL
                   AND "ask_reason" IS NULL
                   AND "policy_checked_at" IS NOT NULL));

-- Budget counts (approval by setting, per connection and per organisation,
-- in a rolling window), the crash backstop's scan of unchecked waiting rows,
-- the session count, and the activity feed.
CREATE INDEX IF NOT EXISTS "assistant_ability_requests_policy_grant_idx"
    ON "public"."assistant_ability_requests" ("proposed_by_grant_id", "decided_at")
    WHERE "approval_source" = 'policy';
CREATE INDEX IF NOT EXISTS "assistant_ability_requests_policy_tenant_idx"
    ON "public"."assistant_ability_requests" ("tenant_id", "decided_at")
    WHERE "approval_source" = 'policy';
CREATE INDEX IF NOT EXISTS "assistant_ability_requests_unchecked_idx"
    ON "public"."assistant_ability_requests" ("created_at")
    WHERE "state" = 'pending' AND "policy_checked_at" IS NULL;
CREATE INDEX IF NOT EXISTS "assistant_ability_requests_session_idx"
    ON "public"."assistant_ability_requests" ("approval_session_id")
    WHERE "approval_session_id" IS NOT NULL;
DROP INDEX IF EXISTS "public"."assistant_ability_requests_activity_idx";
CREATE INDEX IF NOT EXISTS "assistant_ability_requests_activity_created_idx"
    ON "public"."assistant_ability_requests" ("tenant_id", "created_at" DESC, "id" DESC)
    WHERE "state" IN ('approved', 'dispatched', 'done', 'failed', 'not_sent', 'outcome_unknown');

CREATE INDEX IF NOT EXISTS "assistant_cache_purge_requests_policy_grant_idx"
    ON "public"."assistant_cache_purge_requests" ("proposed_by_grant_id", "decided_at")
    WHERE "approval_source" = 'policy';
CREATE INDEX IF NOT EXISTS "assistant_cache_purge_requests_policy_tenant_idx"
    ON "public"."assistant_cache_purge_requests" ("tenant_id", "decided_at")
    WHERE "approval_source" = 'policy';
CREATE INDEX IF NOT EXISTS "assistant_cache_purge_requests_unchecked_idx"
    ON "public"."assistant_cache_purge_requests" ("created_at")
    WHERE "state" = 'pending' AND "policy_checked_at" IS NULL;
CREATE INDEX IF NOT EXISTS "assistant_cache_purge_requests_session_idx"
    ON "public"."assistant_cache_purge_requests" ("approval_session_id")
    WHERE "approval_session_id" IS NOT NULL;
DROP INDEX IF EXISTS "public"."assistant_cache_purge_requests_activity_idx";
CREATE INDEX IF NOT EXISTS "assistant_cache_purge_requests_activity_created_idx"
    ON "public"."assistant_cache_purge_requests" ("tenant_id", "created_at" DESC, "id" DESC)
    WHERE "state" IN ('approved_undispatched', 'dispatched');

-- The workflow columns the application may write. checked_target_status is
-- not among them: it is written at insert only.
GRANT UPDATE (
    "approval_source", "approval_site_mode", "approval_mode_source", "approval_mode_version",
    "approval_setter_user_id", "approval_setter_set_at", "approval_session_id",
    "base_change_class", "change_class", "ask_reason", "policy_checked_at"
) ON "public"."assistant_ability_requests" TO "wpmgr_app";

GRANT UPDATE (
    "approval_source", "approval_site_mode", "approval_mode_source", "approval_mode_version",
    "approval_setter_user_id", "approval_setter_set_at", "approval_session_id",
    "base_change_class", "change_class", "ask_reason", "policy_checked_at"
) ON "public"."assistant_cache_purge_requests" TO "wpmgr_app";

-- ===========================================================================
-- (6) The backstop
-- ===========================================================================

CREATE OR REPLACE FUNCTION "public"."ai_approval_backstop"()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = public, pg_temp
AS $$
DECLARE
    v_raw    text := nullif(current_setting('app.user_id', true), '');
    v_user   uuid;
    v_entry  text;
    v_family text[];
    v_ok     boolean;
BEGIN
    IF TG_TABLE_NAME = 'assistant_ability_requests' THEN
        v_entry  := 'approved';
        v_family := ARRAY['approved', 'dispatched', 'done', 'failed', 'not_sent', 'outcome_unknown'];
    ELSIF TG_TABLE_NAME = 'assistant_cache_purge_requests' THEN
        v_entry  := 'approved_undispatched';
        v_family := ARRAY['approved_undispatched', 'dispatched'];
    ELSE
        RAISE EXCEPTION 'ai_approval_backstop: not written for table %', TG_TABLE_NAME
            USING ERRCODE = '55000';
    END IF;

    IF v_raw IS NOT NULL AND v_raw <> '00000000-0000-0000-0000-000000000000' THEN
        v_user := v_raw::uuid;
    END IF;

    -- 1. Created waiting, undecided and unchecked.
    IF TG_OP = 'INSERT' THEN
        IF NEW.state IS DISTINCT FROM 'pending' THEN
            RAISE EXCEPTION 'ai_approval_backstop: a request is created pending'
                USING ERRCODE = '55000';
        END IF;
        IF NEW.approval_source IS DISTINCT FROM 'person'
           OR NEW.approval_site_mode IS NOT NULL
           OR NEW.approval_mode_source IS NOT NULL
           OR NEW.approval_mode_version IS NOT NULL
           OR NEW.approval_setter_user_id IS NOT NULL
           OR NEW.approval_setter_set_at IS NOT NULL
           OR NEW.approval_session_id IS NOT NULL
           OR NEW.base_change_class IS NOT NULL
           OR NEW.change_class IS NOT NULL
           OR NEW.ask_reason IS NOT NULL
           OR NEW.policy_checked_at IS NOT NULL THEN
            RAISE EXCEPTION 'ai_approval_backstop: a request is created undecided and unchecked'
                USING ERRCODE = '55000';
        END IF;
        RETURN NEW;
    END IF;

    -- 2. The checked target status is a fact of the creation.
    IF TG_TABLE_NAME = 'assistant_ability_requests' THEN
        IF NEW.checked_target_status IS DISTINCT FROM OLD.checked_target_status THEN
            RAISE EXCEPTION 'ai_approval_backstop: checked_target_status is recorded at creation and never changes'
                USING ERRCODE = '55000';
        END IF;
    END IF;

    -- 3. The decision's class, reason and time: once, together, while waiting.
    IF NEW.base_change_class IS DISTINCT FROM OLD.base_change_class
       OR NEW.change_class IS DISTINCT FROM OLD.change_class
       OR NEW.ask_reason IS DISTINCT FROM OLD.ask_reason
       OR NEW.policy_checked_at IS DISTINCT FROM OLD.policy_checked_at THEN
        IF OLD.policy_checked_at IS NOT NULL
           OR OLD.base_change_class IS NOT NULL
           OR OLD.change_class IS NOT NULL
           OR OLD.ask_reason IS NOT NULL
           OR NEW.policy_checked_at IS NULL
           OR OLD.state IS DISTINCT FROM 'pending'
           OR NEW.state NOT IN ('pending', v_entry) THEN
            RAISE EXCEPTION 'ai_approval_backstop: the class, the reason it asked and the check time are written once, together, while the request waits'
                USING ERRCODE = '55000';
        END IF;
    END IF;

    -- 4. The approval record moves only when a waiting request is approved.
    IF NOT (OLD.state = 'pending' AND NEW.state = v_entry) THEN
        IF NEW.approval_source IS DISTINCT FROM OLD.approval_source
           OR NEW.approval_site_mode IS DISTINCT FROM OLD.approval_site_mode
           OR NEW.approval_mode_source IS DISTINCT FROM OLD.approval_mode_source
           OR NEW.approval_mode_version IS DISTINCT FROM OLD.approval_mode_version
           OR NEW.approval_setter_user_id IS DISTINCT FROM OLD.approval_setter_user_id
           OR NEW.approval_setter_set_at IS DISTINCT FROM OLD.approval_setter_set_at
           OR NEW.approval_session_id IS DISTINCT FROM OLD.approval_session_id THEN
            RAISE EXCEPTION 'ai_approval_backstop: the approval record changes only when a waiting request is approved'
                USING ERRCODE = '55000';
        END IF;
    END IF;

    -- 5. Entering the approved states.
    IF OLD.state <> ALL (v_family) AND NEW.state = ANY (v_family) THEN
        IF OLD.state IS DISTINCT FROM 'pending' OR NEW.state IS DISTINCT FROM v_entry THEN
            RAISE EXCEPTION 'ai_approval_backstop: only a waiting request is approved, and only into %', v_entry
                USING ERRCODE = '55000';
        END IF;

        IF NEW.approval_source = 'person' THEN
            -- 6. A named decider is the signed-in person. A missing one is
            -- refused by approval_names_a_human_check.
            IF NEW.decided_by_user_id IS NOT NULL
               AND (v_user IS NULL OR NEW.decided_by_user_id IS DISTINCT FROM v_user) THEN
                RAISE EXCEPTION 'ai_approval_backstop: a person''s approval is made by that person, signed in'
                    USING ERRCODE = '42501';
            END IF;
        ELSIF NEW.approval_source = 'policy' THEN
            -- 7. The site's setting approves it, with no person in the
            -- transaction.
            IF v_raw IS NOT NULL THEN
                RAISE EXCEPTION 'ai_approval_backstop: an approval by a site''s setting runs with no user in the transaction'
                    USING ERRCODE = '42501';
            END IF;
            SELECT EXISTS (
                       SELECT 1
                       FROM sites s
                       WHERE s.tenant_id = NEW.tenant_id
                         AND s.id = NEW.site_id
                         AND s.ai_mode = NEW.approval_site_mode
                         AND s.ai_mode_source = NEW.approval_mode_source
                         AND s.ai_mode_version = NEW.approval_mode_version
                         AND s.ai_mode_set_by = NEW.approval_setter_user_id
                         AND s.ai_mode_set_at IS NOT DISTINCT FROM NEW.approval_setter_set_at
                         AND ai_mode_allows(s.ai_mode, NEW.change_class)
                   )
               AND mcp_grant_runs_by_setting(NEW.tenant_id, NEW.proposed_by_grant_id)
            INTO v_ok;
            IF NOT coalesce(v_ok, false) THEN
                RAISE EXCEPTION 'ai_approval_backstop: an approval by a site''s setting needs that setting, as recorded, to be current and to allow this change, and the connection to run by it'
                    USING ERRCODE = '42501';
            END IF;
        ELSE
            -- 8. Sessions arrive with their own migration.
            RAISE EXCEPTION 'ai_approval_backstop: an approval by % is not accepted', NEW.approval_source
                USING ERRCODE = '42501';
        END IF;
    END IF;

    RETURN NEW;
END;
$$;

REVOKE ALL ON FUNCTION "public"."ai_approval_backstop"() FROM PUBLIC;

DROP TRIGGER IF EXISTS "assistant_ability_requests_approval_backstop" ON "public"."assistant_ability_requests";
CREATE TRIGGER "assistant_ability_requests_approval_backstop"
    BEFORE INSERT OR UPDATE
    ON "public"."assistant_ability_requests"
    FOR EACH ROW
    EXECUTE FUNCTION "public"."ai_approval_backstop"();

DROP TRIGGER IF EXISTS "assistant_cache_purge_requests_approval_backstop" ON "public"."assistant_cache_purge_requests";
CREATE TRIGGER "assistant_cache_purge_requests_approval_backstop"
    BEFORE INSERT OR UPDATE
    ON "public"."assistant_cache_purge_requests"
    FOR EACH ROW
    EXECUTE FUNCTION "public"."ai_approval_backstop"();

-- ===========================================================================
-- (7) The catalogue classes
-- ===========================================================================

ALTER TABLE "public"."ability_catalogue"
    ADD COLUMN IF NOT EXISTS "change_class" text NOT NULL DEFAULT 'always_ask';

ALTER TABLE "public"."rest_route_catalogue"
    ADD COLUMN IF NOT EXISTS "change_class" text NOT NULL DEFAULT 'always_ask';

ALTER TABLE "public"."ability_catalogue"
    DROP CONSTRAINT IF EXISTS "ability_catalogue_change_class_check",
    DROP CONSTRAINT IF EXISTS "ability_catalogue_change_class_effect_check",
    DROP CONSTRAINT IF EXISTS "ability_catalogue_change_class_snapshot_check";

ALTER TABLE "public"."ability_catalogue"
    ADD CONSTRAINT "ability_catalogue_change_class_check"
        CHECK ("change_class" IN ('ai_draft', 'operational', 'unpublished', 'live',
                                  'publish', 'update', 'always_ask', 'by_target_status')),
    -- A class that runs on Auto for AI drafts publishes nothing.
    ADD CONSTRAINT "ability_catalogue_change_class_effect_check"
        CHECK ("change_class" NOT IN ('ai_draft', 'operational')
               OR "effect_copy" IN ('draft', 'none')),
    -- Every class that can run without a person, except one that changes no
    -- content, has an undo.
    ADD CONSTRAINT "ability_catalogue_change_class_snapshot_check"
        CHECK ("change_class" IN ('always_ask', 'operational') OR "snapshot" <> 'none');

ALTER TABLE "public"."rest_route_catalogue"
    DROP CONSTRAINT IF EXISTS "rest_route_catalogue_change_class_check",
    DROP CONSTRAINT IF EXISTS "rest_route_catalogue_change_class_effect_check",
    DROP CONSTRAINT IF EXISTS "rest_route_catalogue_change_class_snapshot_check";

ALTER TABLE "public"."rest_route_catalogue"
    ADD CONSTRAINT "rest_route_catalogue_change_class_check"
        CHECK ("change_class" IN ('ai_draft', 'operational', 'unpublished', 'live',
                                  'publish', 'update', 'always_ask', 'by_target_status')),
    ADD CONSTRAINT "rest_route_catalogue_change_class_effect_check"
        CHECK ("change_class" NOT IN ('ai_draft', 'operational')
               OR "effect_copy" IN ('draft', 'none')),
    ADD CONSTRAINT "rest_route_catalogue_change_class_snapshot_check"
        CHECK ("change_class" IN ('always_ask', 'operational') OR "snapshot" <> 'none');

ALTER TABLE "public"."ability_catalogue_audit"
    ADD COLUMN IF NOT EXISTS "before_change_class" text NULL,
    ADD COLUMN IF NOT EXISTS "after_change_class"  text NULL;

ALTER TABLE "public"."rest_route_catalogue_audit"
    ADD COLUMN IF NOT EXISTS "before_change_class" text NULL,
    ADD COLUMN IF NOT EXISTS "after_change_class"  text NULL;

ALTER TABLE "public"."ability_catalogue_audit"
    DROP CONSTRAINT IF EXISTS "ability_catalogue_audit_change_class_pair_check";
ALTER TABLE "public"."ability_catalogue_audit"
    ADD CONSTRAINT "ability_catalogue_audit_change_class_pair_check"
        CHECK (("before_change_class" IS NULL) = ("after_change_class" IS NULL)
               AND ("after_change_class" IS NULL
                    OR ("action" = 'update' AND "actor_user_id" IS NOT NULL)));

ALTER TABLE "public"."rest_route_catalogue_audit"
    DROP CONSTRAINT IF EXISTS "rest_route_catalogue_audit_change_class_pair_check";
ALTER TABLE "public"."rest_route_catalogue_audit"
    ADD CONSTRAINT "rest_route_catalogue_audit_change_class_pair_check"
        CHECK (("before_change_class" IS NULL) = ("after_change_class" IS NULL)
               AND ("after_change_class" IS NULL
                    OR ("action" = 'update' AND "actor_user_id" IS NOT NULL)));

-- Seeds. Only a row still on the default is classed, so a re-run never
-- overwrites a class a superadmin set.
DO $$
DECLARE
    v_entries bigint;
    v_routes  bigint;
BEGIN
    UPDATE "public"."ability_catalogue"
    SET "change_class" = 'ai_draft'
    WHERE "name" = 'wpmgr/page-create'
      AND "source" = 'wpmgr'
      AND "change_class" = 'always_ask';
    GET DIAGNOSTICS v_entries = ROW_COUNT;

    UPDATE "public"."rest_route_catalogue"
    SET "change_class" = 'by_target_status'
    WHERE "route_id" IN ('wp-v2-pages-update-fields', 'wp-v2-posts-update-fields')
      AND "class" = 'write'
      AND "change_class" = 'always_ask';
    GET DIAGNOSTICS v_routes = ROW_COUNT;

    IF NOT EXISTS (
        SELECT 1 FROM "public"."ability_catalogue"
        WHERE "name" = 'wpmgr/page-create' AND "source" = 'wpmgr' AND "change_class" = 'ai_draft'
    ) THEN
        RAISE NOTICE 'm174: wpmgr/page-create is not classed ai_draft; every page creation waits for a person until a superadmin classes it';
    END IF;
    IF (SELECT count(*) FROM "public"."rest_route_catalogue"
        WHERE "route_id" IN ('wp-v2-pages-update-fields', 'wp-v2-posts-update-fields')
          AND "change_class" = 'by_target_status') <> 2 THEN
        RAISE NOTICE 'm174: the two REST write routes are not both classed by_target_status; an edit through a route that is not waits for a person until a superadmin classes it';
    END IF;

    RAISE NOTICE 'm174: classed % catalogue entries and % REST routes', v_entries, v_routes;
END;
$$;

CREATE OR REPLACE FUNCTION "public"."set_ability_change_class"(
    p_actor_user_id uuid,
    p_kind text,
    p_id text,
    p_class text
)
RETURNS text
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_name   text;
    v_before ability_catalogue;
    v_after  ability_catalogue;
    v_rb     rest_route_catalogue;
    v_ra     rest_route_catalogue;
    v_entry  uuid;
BEGIN
    IF p_actor_user_id IS NULL OR NOT EXISTS (
        SELECT 1 FROM users u
        WHERE u.id = p_actor_user_id AND u.is_superadmin
    ) THEN
        RAISE EXCEPTION 'set_ability_change_class: actor is not a superadmin'
            USING ERRCODE = '42501';
    END IF;

    IF p_class IS NULL OR p_class NOT IN ('ai_draft', 'operational', 'unpublished', 'live',
                                          'publish', 'update', 'always_ask', 'by_target_status') THEN
        RAISE EXCEPTION 'set_ability_change_class: % is not a change class', p_class
            USING ERRCODE = '22023';
    END IF;

    IF p_kind = 'ability' THEN
        IF p_id IS NULL OR p_id !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN
            RAISE EXCEPTION 'set_ability_change_class: an ability is named by its entry id'
                USING ERRCODE = '22023';
        END IF;
        v_entry := p_id::uuid;

        SELECT name INTO v_name FROM ability_catalogue WHERE entry_id = v_entry;
        IF v_name IS NULL THEN
            RAISE EXCEPTION 'set_ability_change_class: no entry %', p_id
                USING ERRCODE = 'P0002';
        END IF;

        -- The catalogue writers' per-name lock, then the row.
        PERFORM pg_advisory_xact_lock(hashtext('ability_catalogue'), hashtext(v_name));
        SELECT * INTO v_before FROM ability_catalogue WHERE entry_id = v_entry FOR UPDATE;

        UPDATE ability_catalogue AS ac SET
            change_class = p_class,
            updated_at = now(),
            updated_by_user_id = p_actor_user_id
        WHERE ac.entry_id = v_entry
        RETURNING ac.* INTO v_after;

        INSERT INTO ability_catalogue_audit (
            entry_id, name, action, actor_user_id,
            before_row_sha256, after_row_sha256,
            before_entry_sha256, after_entry_sha256,
            before_enabled, after_enabled,
            before_change_class, after_change_class
        ) VALUES (
            v_after.entry_id,
            v_after.name,
            'update',
            p_actor_user_id,
            ability_catalogue_row_sha256(v_before),
            ability_catalogue_row_sha256(v_after),
            v_before.entry_sha256,
            v_after.entry_sha256,
            v_before.enabled,
            v_after.enabled,
            v_before.change_class,
            v_after.change_class
        );

        RETURN v_after.change_class;
    ELSIF p_kind = 'rest_route' THEN
        IF p_id IS NULL OR p_id !~ '^[a-z0-9-]{1,64}$' THEN
            RAISE EXCEPTION 'set_ability_change_class: a route is named by its route id'
                USING ERRCODE = '22023';
        END IF;

        PERFORM pg_advisory_xact_lock(hashtext('rest_route_catalogue'), hashtext(p_id));
        SELECT * INTO v_rb FROM rest_route_catalogue WHERE route_id = p_id FOR UPDATE;
        IF v_rb.route_id IS NULL THEN
            RAISE EXCEPTION 'set_ability_change_class: no route %', p_id
                USING ERRCODE = 'P0002';
        END IF;

        UPDATE rest_route_catalogue AS rr SET
            change_class = p_class,
            updated_at = now(),
            updated_by_user_id = p_actor_user_id
        WHERE rr.route_id = p_id
        RETURNING rr.* INTO v_ra;

        INSERT INTO rest_route_catalogue_audit (
            route_id, action, actor_user_id,
            before_row_sha256, after_row_sha256,
            before_route_sha256, after_route_sha256,
            before_enabled, after_enabled,
            before_change_class, after_change_class
        ) VALUES (
            v_ra.route_id,
            'update',
            p_actor_user_id,
            rest_route_catalogue_row_sha256(v_rb),
            rest_route_catalogue_row_sha256(v_ra),
            v_rb.route_sha256,
            v_ra.route_sha256,
            v_rb.enabled,
            v_ra.enabled,
            v_rb.change_class,
            v_ra.change_class
        );

        RETURN v_ra.change_class;
    END IF;

    RAISE EXCEPTION 'set_ability_change_class: kind % is not ability or rest_route', p_kind
        USING ERRCODE = '22023';
END;
$$;

REVOKE ALL ON FUNCTION "public"."set_ability_change_class"(uuid, text, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION "public"."set_ability_change_class"(uuid, text, text, text) TO "wpmgr_app";
