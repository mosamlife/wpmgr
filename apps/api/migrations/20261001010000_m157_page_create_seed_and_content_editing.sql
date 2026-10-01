-- m157: seed wpmgr/page-create, a narrow stamp path for WPMgr's own catalogue
-- hashes, and the per-site content-editing state on sites.
--
-- Engine slice E2. Three parts, all additive.
--
-- ===========================================================================
-- 1. Seed: wpmgr/page-create
-- ===========================================================================
--
-- The first admitted write. source wpmgr, class write, approval per_call,
-- snapshot created_post_trash (undo trashes the draft it created), effect_copy
-- draft (nothing is published), operator_permission site.content.edit. It
-- satisfies m155's ability_catalogue_write_rules_check. entry_sha256 stays
-- NULL until Go stamps it through part 2. NOT EXISTS-guarded on the name, as
-- m155's seed is, so a re-run never overwrites a row a superadmin has edited.
--
-- ===========================================================================
-- 2. stamp_wpmgr_ability_entry_hash(): fill a NULL hash on a wpmgr row, once
-- ===========================================================================
--
-- Every dispatch compares the catalogue's entry_sha256 with the request's, so
-- a NULL hash refuses every request for that entry. The hash is over the
-- canonical entry bytes Go sends, which SQL cannot reproduce (m155 header), so
-- Go computes it and this function stores it. It is SECURITY DEFINER because
-- wpmgr_app holds no write on ability_catalogue (m155).
--
-- It is deliberately narrower than admin_upsert_ability_catalogue_entry and
-- needs no superadmin actor, because the only change it can make is
-- NULL -> a well-formed hash on a source = 'wpmgr' row:
--
--   * no entry                      -> P0002
--   * hash not ^[0-9a-f]{64}$       -> 22023
--   * source is not 'wpmgr'         -> 42501 (vendor and core rows untouched)
--   * entry_sha256 already set      -> 55000 (never changes a set hash; a
--                                      concurrent stamper that lost the race
--                                      gets this and should re-read the row)
--
-- It takes m155's per-name advisory lock and a row lock, so it serialises
-- with the superadmin writer. It writes one ability_catalogue_audit row with
-- actor_user_id NULL, which is why that column becomes nullable below, and a
-- CHECK holds a NULL actor to exactly this shape: an update from a NULL entry
-- hash to a set one with enabled unchanged. A superadmin write still always
-- names its actor (the definer refuses otherwise).
--
-- Changing a hash that is already set remains the superadmin path's job.
--
-- EXECUTE: revoked from PUBLIC, granted to wpmgr_app only. search_path pinned.
--
-- ===========================================================================
-- 3. sites: content-editing state
-- ===========================================================================
--
-- Three nullable columns on sites, written only by the enable action:
-- content_editing_enabled_at (NULL = not enabled), content_editing_principal_
-- user_id (the WordPress user id the agent returned; a WordPress id, not a
-- WPMgr one, hence bigint and no FK), content_editing_enabled_by (the WPMgr
-- user who enabled it, FK ON DELETE SET NULL, the m117 monitoring_paused_by
-- pattern). A CHECK keeps enabled_at and the principal together, and the
-- principal positive.
--
-- Columns, not a table, because that is how sites carries its other per-site
-- state (m117's monitoring_paused_*). Grants: sites' grants are table-level
-- (no column-level GRANT on sites in any migration), and a table-level grant
-- covers columns added later, so there is nothing to GRANT and nothing
-- withheld for the integration harness to re-revoke. RLS is row-level: sites
-- is ENABLE + FORCE with sites_tenant_isolation and the RESTRICTIVE
-- sites_site_scope (m19), and the new columns inherit both.
--
-- ===========================================================================
-- CONVERGE PATH
-- ===========================================================================
--
-- None needed: every object is new or additive. ADD COLUMN IF NOT EXISTS,
-- guarded constraints, DROP NOT NULL (a no-op when already dropped), CREATE OR
-- REPLACE FUNCTION and a NOT EXISTS seed make a re-run a no-op.

-- ---------------------------------------------------------------------------
-- 1. Seed
-- ---------------------------------------------------------------------------

INSERT INTO "public"."ability_catalogue" (
    "name", "source", "class", "status", "enabled", "approval_mode",
    "snapshot", "effect_copy", "operator_permission", "min_agent_version",
    "title", "description"
)
SELECT 'wpmgr/page-create', 'wpmgr', 'write', 'admitted', true, 'per_call',
       'created_post_trash', 'draft', 'site.content.edit', '0.61.156',
       'Create a draft page',
       'Creates a new draft page or post from a text outline. Nothing is published. Undo moves the draft to the trash.'
WHERE NOT EXISTS (
    SELECT 1 FROM "public"."ability_catalogue" c WHERE c.name = 'wpmgr/page-create'
);

-- ---------------------------------------------------------------------------
-- 2. The audit admits a system stamp, and only a system stamp, with no actor
-- ---------------------------------------------------------------------------

ALTER TABLE "public"."ability_catalogue_audit"
    ALTER COLUMN "actor_user_id" DROP NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.ability_catalogue_audit'::regclass
          AND conname  = 'ability_catalogue_audit_null_actor_is_stamp_check'
    ) THEN
        ALTER TABLE "public"."ability_catalogue_audit"
            ADD CONSTRAINT "ability_catalogue_audit_null_actor_is_stamp_check"
            CHECK ("actor_user_id" IS NOT NULL OR (
                "action" = 'update'
                AND "before_entry_sha256" IS NULL
                AND "after_entry_sha256" IS NOT NULL
                AND "before_enabled" IS NOT DISTINCT FROM "after_enabled"));
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION "public"."stamp_wpmgr_ability_entry_hash"(
    p_entry_id uuid,
    p_sha text
)
RETURNS "public"."ability_catalogue"
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_name   text;
    v_before ability_catalogue;
    v_after  ability_catalogue;
BEGIN
    IF p_sha IS NULL OR p_sha !~ '^[0-9a-f]{64}$' THEN
        RAISE EXCEPTION 'ability_catalogue: entry hash is not 64 lowercase hex'
            USING ERRCODE = '22023';
    END IF;

    SELECT name INTO v_name FROM ability_catalogue WHERE entry_id = p_entry_id;
    IF v_name IS NULL THEN
        RAISE EXCEPTION 'ability_catalogue: no entry %', p_entry_id
            USING ERRCODE = 'P0002';
    END IF;

    -- m155's writer lock, then the row: the before-state cannot move under us.
    PERFORM pg_advisory_xact_lock(hashtext('ability_catalogue'), hashtext(v_name));
    SELECT * INTO v_before FROM ability_catalogue
        WHERE entry_id = p_entry_id
        FOR UPDATE;

    IF v_before.source IS DISTINCT FROM 'wpmgr' THEN
        RAISE EXCEPTION 'ability_catalogue: only a wpmgr entry is stamped here'
            USING ERRCODE = '42501';
    END IF;
    IF v_before.entry_sha256 IS NOT NULL THEN
        RAISE EXCEPTION 'ability_catalogue: entry % is already stamped', p_entry_id
            USING ERRCODE = '55000';
    END IF;

    UPDATE ability_catalogue AS ac SET
        entry_sha256 = p_sha,
        updated_at = now()
    WHERE ac.entry_id = p_entry_id
      AND ac.source = 'wpmgr'
      AND ac.entry_sha256 IS NULL
    RETURNING ac.* INTO v_after;

    INSERT INTO ability_catalogue_audit (
        entry_id, name, action, actor_user_id,
        before_row_sha256, after_row_sha256,
        before_entry_sha256, after_entry_sha256,
        before_enabled, after_enabled
    ) VALUES (
        v_after.entry_id,
        v_after.name,
        'update',
        NULL,
        ability_catalogue_row_sha256(v_before),
        ability_catalogue_row_sha256(v_after),
        v_before.entry_sha256,
        v_after.entry_sha256,
        v_before.enabled,
        v_after.enabled
    );

    RETURN v_after;
END;
$$;

REVOKE ALL ON FUNCTION "public"."stamp_wpmgr_ability_entry_hash"(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION "public"."stamp_wpmgr_ability_entry_hash"(uuid, text) TO "wpmgr_app";

-- ---------------------------------------------------------------------------
-- 3. sites: content-editing state
-- ---------------------------------------------------------------------------

ALTER TABLE "public"."sites"
    ADD COLUMN IF NOT EXISTS "content_editing_enabled_at"        timestamptz NULL,
    ADD COLUMN IF NOT EXISTS "content_editing_principal_user_id" bigint      NULL,
    ADD COLUMN IF NOT EXISTS "content_editing_enabled_by"        uuid        NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.sites'::regclass
          AND conname  = 'sites_content_editing_enabled_by_fkey'
    ) THEN
        ALTER TABLE "public"."sites"
            ADD CONSTRAINT "sites_content_editing_enabled_by_fkey"
            FOREIGN KEY ("content_editing_enabled_by") REFERENCES "public"."users" ("id")
            ON UPDATE NO ACTION ON DELETE SET NULL;
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.sites'::regclass
          AND conname  = 'sites_content_editing_state_check'
    ) THEN
        ALTER TABLE "public"."sites"
            ADD CONSTRAINT "sites_content_editing_state_check"
            CHECK (
                ("content_editing_enabled_at" IS NULL) = ("content_editing_principal_user_id" IS NULL)
                AND ("content_editing_principal_user_id" IS NULL OR "content_editing_principal_user_id" > 0)
            );
    END IF;
END;
$$;
