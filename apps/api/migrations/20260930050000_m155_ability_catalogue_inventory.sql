-- m155: ability_catalogue (the GLOBAL allowlist of reviewed abilities) and
-- site_ability_inventory (which abilities each site has registered), plus the
-- per-site refresh record site_ability_inventory_runs.
--
-- Engine slice E1. Three tables, one audit table, one write function.
--
-- ===========================================================================
-- 1. ability_catalogue: GLOBAL, superadmin-written, app role reads only
-- ===========================================================================
--
-- One row per reviewed ability and version range. No tenant_id: a platform
-- allowlist, not tenant data. The write model is m153's content_integrations
-- model, unchanged:
--
--   * ENABLE ROW LEVEL SECURITY, NOT FORCE, with one FOR SELECT policy
--     USING (true). The owner (the migration role) bypasses RLS, which lets
--     the seed and the SECURITY DEFINER writer run. wpmgr_app has no write
--     policy, so a write that somehow held a grant is still refused.
--   * When the migration role is NOT wpmgr_app (separate owner DSN, the
--     production model), wpmgr_app is REVOKEd INSERT, UPDATE, DELETE and
--     TRUNCATE and keeps SELECT.
--   * SINGLE-DSN INSTALLS HAVE NO DATABASE-LEVEL WRITE FENCE. When the
--     migration role is wpmgr_app, it owns the table and the function, and the
--     REVOKE would only disable the definer (and with it the enabled = false
--     kill switch). So it is skipped there, and the application's superadmin
--     gate is the only control over writes on such an install.
--   * The ONE write path is admin_upsert_ability_catalogue_entry(), SECURITY
--     DEFINER, EXECUTE granted only to wpmgr_app. It refuses unless the actor
--     is a users row with is_superadmin, serialises writers of one ability
--     name on an advisory lock, and writes an ability_catalogue_audit row with
--     the before and after row hashes AND the before and after entry_sha256 in
--     the same statement.
--   * No delete function. Retiring an entry is enabled = false (kill switch
--     1), which the audit records like any other change.
--
-- entry_id is a stable surrogate key. A request row (E2) references the entry
-- it was approved against by entry_id and by entry_sha256; the id never
-- changes across an update, the hash does.
--
-- entry_sha256 is NULL until the Go admin path stamps it: it is the hash of
-- the exact entry JSON bytes the control plane sends (amendment R2), which SQL
-- cannot reproduce, so the seed does not invent one.
--
-- CHECKs enforce what is decidable per row: closed sets for source, class,
-- status, approval_mode, permission_mode, snapshot, preview and effect_copy;
-- a write entry needs approval per_call, a snapshot other than 'none' and an
-- operator_permission; vendor fields are present only for source = 'vendor'.
-- Deliberately NOT here: the schema-hash-required-for-non-wpmgr rule and
-- non-overlapping version ranges (amendment C1, before E3), which land with
-- the vendor admission path.
--
-- ===========================================================================
-- 2. site_ability_inventory: tenant- and site-scoped, FORCE RLS
-- ===========================================================================
--
-- The wpmgr/abilities-inventory read for one site, one row per registered
-- ability. A refresh REPLACES a site's rows: upsert every row seen with one
-- checked_at, then delete the site's rows with an older checked_at, both in
-- the site's tenant transaction.
--
-- Amendment R4, met in full: ENABLE and FORCE RLS; the permissive
-- site_ability_inventory_tenant_isolation with WITH CHECK identical to USING;
-- the RESTRICTIVE site_ability_inventory_site_scope on app.site_scope and
-- app.allowed_site_ids with the literal 'on' sentinel (m19's predicate, as
-- m151 and m153 apply it).
--
-- NO CROSS-TENANT POLICY, deliberately: no <table>_agent policy, so no
-- session reads another tenant's rows. Site-supplied labels and descriptions
-- live here; a fleet-wide view, when one is built, reads counts through a
-- definer function as m153's fleet report does.
--
-- Grants: SELECT, INSERT, UPDATE, DELETE for wpmgr_app (the replace deletes
-- rows the site no longer reports); TRUNCATE REVOKEd, because TRUNCATE
-- bypasses row-level security.
--
-- CASCADE from tenants and sites: a re-derivable cache of what the site last
-- said, not an audit record, and it names nothing to reclaim.
--
-- Every site-supplied string carries a length or shape CHECK. The name must
-- match the Abilities API shape; Go drops names that do not before insert.
-- site_label and site_description are site text, cleaned and capped by Go,
-- and capped again here.
--
-- ===========================================================================
-- 3. site_ability_inventory_runs: the last refresh per site
-- ===========================================================================
--
-- One row per site, written in the same tenant transaction as the refresh, so
-- discover can report as_of, stale and truncated truthfully. snapshot_id is
-- new on every refresh; the discover cursor binds to it, so a cursor from an
-- earlier refresh is recognisably stale. Same tenancy as the inventory: FORCE
-- RLS, tenant_isolation, RESTRICTIVE site_scope, no cross-tenant policy. No
-- DELETE for wpmgr_app (the row is replaced by upsert and goes with its
-- site); TRUNCATE revoked.
--
-- ===========================================================================
-- CONVERGE PATH
-- ===========================================================================
--
-- None needed: every object is new. CREATE ... IF NOT EXISTS, the policy
-- guards and a NOT EXISTS seed make a re-run a no-op, and the seed never
-- overwrites a row a superadmin has since edited.

-- ---------------------------------------------------------------------------
-- ability_catalogue
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS "public"."ability_catalogue" (
    "entry_id" uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    "name" text NOT NULL
        CONSTRAINT "ability_catalogue_name_check"
        CHECK ("name" ~ '^[a-z0-9-]{1,64}/[a-z0-9-]{1,64}$'),
    "source" text NOT NULL
        CONSTRAINT "ability_catalogue_source_check"
        CHECK ("source" IN ('wpmgr', 'core', 'vendor')),
    "class" text NOT NULL
        CONSTRAINT "ability_catalogue_class_check"
        CHECK ("class" IN ('read', 'write', 'denied')),
    "status" text NOT NULL
        CONSTRAINT "ability_catalogue_status_check"
        CHECK ("status" IN ('admitted', 'detect_only', 'awaiting_vendor_tools')),
    "enabled" boolean NOT NULL DEFAULT true,
    "approval_mode" text NOT NULL
        CONSTRAINT "ability_catalogue_approval_mode_check"
        CHECK ("approval_mode" IN ('none', 'per_call')),
    "permission_mode" text NOT NULL DEFAULT 'principal'
        CONSTRAINT "ability_catalogue_permission_mode_check"
        CHECK ("permission_mode" IN ('principal', 'asserted')),

    -- Vendor: the builder descriptor this entry belongs to, the owning plugin
    -- or theme directory, and the tested version range. Present only for
    -- source = 'vendor'.
    "integration_id" text NULL
        REFERENCES "public"."content_integrations" ("integration_id"),
    "owner_dir" text NULL
        CONSTRAINT "ability_catalogue_owner_dir_check"
        CHECK ("owner_dir" ~ '^[A-Za-z0-9._-]{1,100}$'),
    "version_min" text NULL
        CONSTRAINT "ability_catalogue_version_min_check"
        CHECK ("version_min" ~ '^[0-9A-Za-z.+-]{1,32}$'),
    "version_max_tested" text NULL
        CONSTRAINT "ability_catalogue_version_max_tested_check"
        CHECK ("version_max_tested" ~ '^[0-9A-Za-z.+-]{1,32}$'),
    CONSTRAINT "ability_catalogue_vendor_fields_check"
        CHECK ("source" = 'vendor' OR (
            "integration_id" IS NULL AND "owner_dir" IS NULL
            AND "version_min" IS NULL AND "version_max_tested" IS NULL)),
    CONSTRAINT "ability_catalogue_vendor_owner_check"
        CHECK ("source" <> 'vendor' OR "owner_dir" IS NOT NULL),

    "min_wp_version" text NULL
        CONSTRAINT "ability_catalogue_min_wp_version_check"
        CHECK ("min_wp_version" ~ '^[0-9]+(\.[0-9]+){0,2}$'),
    "min_agent_version" text NULL
        CONSTRAINT "ability_catalogue_min_agent_version_check"
        CHECK ("min_agent_version" ~ '^[0-9]+(\.[0-9]+){0,2}$'),

    "schema_struct_sha256" text NULL
        CONSTRAINT "ability_catalogue_schema_struct_sha256_check"
        CHECK ("schema_struct_sha256" ~ '^[0-9a-f]{64}$'),
    "dynamic_enum_paths" text[] NOT NULL DEFAULT '{}'::text[]
        CONSTRAINT "ability_catalogue_dynamic_enum_paths_check"
        CHECK (cardinality("dynamic_enum_paths") <= 64),

    -- OUR plain-language copy. Never site text.
    "title" text NOT NULL
        CONSTRAINT "ability_catalogue_title_check"
        CHECK (length(btrim("title")) > 0 AND char_length("title") <= 80),
    "description" text NOT NULL
        CONSTRAINT "ability_catalogue_description_check"
        CHECK (length(btrim("description")) > 0 AND char_length("description") <= 1000),
    "usage" text NULL
        CONSTRAINT "ability_catalogue_usage_check"
        CHECK (char_length("usage") <= 2000),

    "operator_permission" text NULL
        CONSTRAINT "ability_catalogue_operator_permission_check"
        CHECK ("operator_permission" ~ '^[a-z]+(\.[a-z_]+){1,3}$'),
    "target" jsonb NULL
        CONSTRAINT "ability_catalogue_target_check"
        CHECK ("target" IS NULL OR jsonb_typeof("target") = 'object'),
    "snapshot" text NOT NULL DEFAULT 'none'
        CONSTRAINT "ability_catalogue_snapshot_check"
        CHECK ("snapshot" IN (
            'none', 'created_post_trash', 'wp_revision', 'vendor_draft_discard',
            'vendor_tree_rewrite', 'post_fields', 'own_attachment_delete',
            'menu_items', 'option_values'
        )),
    "preview" text NULL
        CONSTRAINT "ability_catalogue_preview_check"
        CHECK ("preview" IN ('rich_create', 'rich_edit', 'structured')),
    "arg_render" jsonb NOT NULL DEFAULT '{}'::jsonb
        CONSTRAINT "ability_catalogue_arg_render_check"
        CHECK (jsonb_typeof("arg_render") = 'object'),
    "effect_copy" text NOT NULL DEFAULT 'none'
        CONSTRAINT "ability_catalogue_effect_copy_check"
        CHECK ("effect_copy" IN ('draft', 'live', 'none')),
    "limits" jsonb NOT NULL DEFAULT '{}'::jsonb
        CONSTRAINT "ability_catalogue_limits_check"
        CHECK (jsonb_typeof("limits") = 'object'),
    "nested_allow" text[] NOT NULL DEFAULT '{}'::text[],
    "global_option_keys" text[] NOT NULL DEFAULT '{}'::text[],
    "integration_block" jsonb NULL
        CONSTRAINT "ability_catalogue_integration_block_check"
        CHECK ("integration_block" IS NULL OR jsonb_typeof("integration_block") = 'object'),
    "admission" jsonb NOT NULL DEFAULT '{}'::jsonb
        CONSTRAINT "ability_catalogue_admission_check"
        CHECK (jsonb_typeof("admission") = 'object'),

    "entry_sha256" text NULL
        CONSTRAINT "ability_catalogue_entry_sha256_check"
        CHECK ("entry_sha256" ~ '^[0-9a-f]{64}$'),

    -- A write is approved per call, has an undo, and names the permission
    -- its approver must hold.
    CONSTRAINT "ability_catalogue_write_rules_check"
        CHECK ("class" <> 'write' OR (
            "approval_mode" = 'per_call'
            AND "snapshot" <> 'none'
            AND "operator_permission" IS NOT NULL)),

    "created_at" timestamptz NOT NULL DEFAULT now(),
    "updated_at" timestamptz NOT NULL DEFAULT now(),
    -- NULL only for a row the migration seeded.
    "updated_by_user_id" uuid NULL
);

-- One entry per name and range start. NULLS NOT DISTINCT so two wpmgr or core
-- entries (version_min NULL) for one name collide.
CREATE UNIQUE INDEX IF NOT EXISTS "ability_catalogue_name_version_key"
    ON "public"."ability_catalogue" ("name", "version_min") NULLS NOT DISTINCT;

ALTER TABLE "public"."ability_catalogue" ENABLE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'ability_catalogue'
          AND policyname = 'ability_catalogue_read'
    ) THEN
        CREATE POLICY "ability_catalogue_read"
            ON "public"."ability_catalogue"
            FOR SELECT
            USING (true);
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- ability_catalogue_audit: append-only, written only by the definer
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS "public"."ability_catalogue_audit" (
    "id" bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    "entry_id" uuid NOT NULL,
    "name" text NOT NULL,
    "action" text NOT NULL
        CONSTRAINT "ability_catalogue_audit_action_check"
        CHECK ("action" IN ('insert', 'update')),
    "actor_user_id" uuid NOT NULL,
    "before_row_sha256" text NULL
        CONSTRAINT "ability_catalogue_audit_before_row_check"
        CHECK ("before_row_sha256" ~ '^[0-9a-f]{64}$'),
    "after_row_sha256" text NOT NULL
        CONSTRAINT "ability_catalogue_audit_after_row_check"
        CHECK ("after_row_sha256" ~ '^[0-9a-f]{64}$'),
    CONSTRAINT "ability_catalogue_audit_before_iff_update_check"
        CHECK (("action" = 'update') = ("before_row_sha256" IS NOT NULL)),
    "before_entry_sha256" text NULL,
    "after_entry_sha256" text NULL,
    "before_enabled" boolean NULL,
    "after_enabled" boolean NOT NULL,
    "at" timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS "ability_catalogue_audit_entry_idx"
    ON "public"."ability_catalogue_audit" ("entry_id", "id" DESC);

ALTER TABLE "public"."ability_catalogue_audit" ENABLE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'ability_catalogue_audit'
          AND policyname = 'ability_catalogue_audit_read'
    ) THEN
        CREATE POLICY "ability_catalogue_audit_read"
            ON "public"."ability_catalogue_audit"
            FOR SELECT
            USING (true);
    END IF;
END;
$$;

-- The row hash the audit records, computed in SQL from the stored row so the
-- audit does not trust a caller-supplied hash. Timestamps and the actor are
-- excluded: the hash describes the entry, not who touched it when.
CREATE OR REPLACE FUNCTION "public"."ability_catalogue_row_sha256"(
    r "public"."ability_catalogue"
)
RETURNS text
LANGUAGE sql
IMMUTABLE
SET search_path = public, pg_temp
AS $$
    SELECT encode(sha256(convert_to(jsonb_build_object(
        'entry_id', r.entry_id,
        'name', r.name,
        'source', r.source,
        'class', r.class,
        'status', r.status,
        'enabled', r.enabled,
        'approval_mode', r.approval_mode,
        'permission_mode', r.permission_mode,
        'integration_id', r.integration_id,
        'owner_dir', r.owner_dir,
        'version_min', r.version_min,
        'version_max_tested', r.version_max_tested,
        'min_wp_version', r.min_wp_version,
        'min_agent_version', r.min_agent_version,
        'schema_struct_sha256', r.schema_struct_sha256,
        'dynamic_enum_paths', to_jsonb(r.dynamic_enum_paths),
        'title', r.title,
        'description', r.description,
        'usage', r.usage,
        'operator_permission', r.operator_permission,
        'target', r.target,
        'snapshot', r.snapshot,
        'preview', r.preview,
        'arg_render', r.arg_render,
        'effect_copy', r.effect_copy,
        'limits', r.limits,
        'nested_allow', to_jsonb(r.nested_allow),
        'global_option_keys', to_jsonb(r.global_option_keys),
        'integration_block', r.integration_block,
        'admission', r.admission,
        'entry_sha256', r.entry_sha256
    )::text, 'UTF8')), 'hex');
$$;

-- The one write path. p_entry_id NULL inserts a new entry; a non-NULL
-- p_entry_id updates that entry and refuses (P0002) if it does not exist, so
-- an update can never silently become an insert.
CREATE OR REPLACE FUNCTION "public"."admin_upsert_ability_catalogue_entry"(
    p_actor_user_id uuid,
    p_entry_id uuid,
    p_name text,
    p_source text,
    p_class text,
    p_status text,
    p_enabled boolean,
    p_approval_mode text,
    p_permission_mode text,
    p_integration_id text,
    p_owner_dir text,
    p_version_min text,
    p_version_max_tested text,
    p_min_wp_version text,
    p_min_agent_version text,
    p_schema_struct_sha256 text,
    p_dynamic_enum_paths text[],
    p_title text,
    p_description text,
    p_usage text,
    p_operator_permission text,
    p_target jsonb,
    p_snapshot text,
    p_preview text,
    p_arg_render jsonb,
    p_effect_copy text,
    p_limits jsonb,
    p_nested_allow text[],
    p_global_option_keys text[],
    p_integration_block jsonb,
    p_admission jsonb,
    p_entry_sha256 text
)
RETURNS "public"."ability_catalogue"
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_before ability_catalogue;
    v_after  ability_catalogue;
BEGIN
    IF p_actor_user_id IS NULL OR NOT EXISTS (
        SELECT 1 FROM users u
        WHERE u.id = p_actor_user_id AND u.is_superadmin
    ) THEN
        RAISE EXCEPTION 'ability_catalogue: actor is not a superadmin'
            USING ERRCODE = '42501';
    END IF;

    -- Serialise writers of one ability name, so two concurrent writers cannot
    -- both read the same before-state and audit it twice.
    PERFORM pg_advisory_xact_lock(hashtext('ability_catalogue'), hashtext(p_name));

    IF p_entry_id IS NULL THEN
        INSERT INTO ability_catalogue AS ac (
            name, source, class, status, enabled, approval_mode, permission_mode,
            integration_id, owner_dir, version_min, version_max_tested,
            min_wp_version, min_agent_version, schema_struct_sha256,
            dynamic_enum_paths, title, description, usage, operator_permission,
            target, snapshot, preview, arg_render, effect_copy, limits,
            nested_allow, global_option_keys, integration_block, admission,
            entry_sha256, updated_at, updated_by_user_id
        ) VALUES (
            p_name, p_source, p_class, p_status, p_enabled, p_approval_mode,
            coalesce(p_permission_mode, 'principal'),
            p_integration_id, p_owner_dir, p_version_min, p_version_max_tested,
            p_min_wp_version, p_min_agent_version, p_schema_struct_sha256,
            coalesce(p_dynamic_enum_paths, '{}'::text[]), p_title, p_description,
            p_usage, p_operator_permission, p_target,
            coalesce(p_snapshot, 'none'), p_preview,
            coalesce(p_arg_render, '{}'::jsonb), coalesce(p_effect_copy, 'none'),
            coalesce(p_limits, '{}'::jsonb),
            coalesce(p_nested_allow, '{}'::text[]),
            coalesce(p_global_option_keys, '{}'::text[]),
            p_integration_block, coalesce(p_admission, '{}'::jsonb),
            p_entry_sha256, now(), p_actor_user_id
        )
        RETURNING ac.* INTO v_after;
    ELSE
        SELECT * INTO v_before FROM ability_catalogue
            WHERE entry_id = p_entry_id
            FOR UPDATE;
        IF v_before.entry_id IS NULL THEN
            RAISE EXCEPTION 'ability_catalogue: no entry %', p_entry_id
                USING ERRCODE = 'P0002';
        END IF;
        IF v_before.name <> p_name THEN
            -- The lock above is keyed on p_name; renaming would escape it.
            RAISE EXCEPTION 'ability_catalogue: an entry''s name cannot change'
                USING ERRCODE = '22023';
        END IF;

        UPDATE ability_catalogue AS ac SET
            source = p_source,
            class = p_class,
            status = p_status,
            enabled = p_enabled,
            approval_mode = p_approval_mode,
            permission_mode = coalesce(p_permission_mode, 'principal'),
            integration_id = p_integration_id,
            owner_dir = p_owner_dir,
            version_min = p_version_min,
            version_max_tested = p_version_max_tested,
            min_wp_version = p_min_wp_version,
            min_agent_version = p_min_agent_version,
            schema_struct_sha256 = p_schema_struct_sha256,
            dynamic_enum_paths = coalesce(p_dynamic_enum_paths, '{}'::text[]),
            title = p_title,
            description = p_description,
            usage = p_usage,
            operator_permission = p_operator_permission,
            target = p_target,
            snapshot = coalesce(p_snapshot, 'none'),
            preview = p_preview,
            arg_render = coalesce(p_arg_render, '{}'::jsonb),
            effect_copy = coalesce(p_effect_copy, 'none'),
            limits = coalesce(p_limits, '{}'::jsonb),
            nested_allow = coalesce(p_nested_allow, '{}'::text[]),
            global_option_keys = coalesce(p_global_option_keys, '{}'::text[]),
            integration_block = p_integration_block,
            admission = coalesce(p_admission, '{}'::jsonb),
            entry_sha256 = p_entry_sha256,
            updated_at = now(),
            updated_by_user_id = p_actor_user_id
        WHERE ac.entry_id = p_entry_id
        RETURNING ac.* INTO v_after;
    END IF;

    INSERT INTO ability_catalogue_audit (
        entry_id, name, action, actor_user_id,
        before_row_sha256, after_row_sha256,
        before_entry_sha256, after_entry_sha256,
        before_enabled, after_enabled
    ) VALUES (
        v_after.entry_id,
        v_after.name,
        CASE WHEN v_before.entry_id IS NULL THEN 'insert' ELSE 'update' END,
        p_actor_user_id,
        CASE WHEN v_before.entry_id IS NULL THEN NULL
             ELSE ability_catalogue_row_sha256(v_before) END,
        ability_catalogue_row_sha256(v_after),
        v_before.entry_sha256,
        v_after.entry_sha256,
        v_before.enabled,
        v_after.enabled
    );

    RETURN v_after;
END;
$$;

REVOKE ALL ON FUNCTION "public"."admin_upsert_ability_catalogue_entry"(
    uuid, uuid, text, text, text, text, boolean, text, text, text, text, text,
    text, text, text, text, text[], text, text, text, text, jsonb, text, text,
    jsonb, text, jsonb, text[], text[], jsonb, jsonb, text
) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION "public"."admin_upsert_ability_catalogue_entry"(
    uuid, uuid, text, text, text, text, boolean, text, text, text, text, text,
    text, text, text, text, text[], text, text, text, text, jsonb, text, text,
    jsonb, text, jsonb, text[], text[], jsonb, jsonb, text
) TO "wpmgr_app";

-- ---------------------------------------------------------------------------
-- Seed: the three E1 own read abilities, admitted.
-- ---------------------------------------------------------------------------
--
-- source wpmgr, class read, approval none, snapshot none. Their code ships in
-- the agent, so no vendor fields and no schema hash. entry_sha256 stays NULL
-- until the Go admin path stamps it (see the header). Inserted before the
-- REVOKE (the m40.1 order) so the seed also lands where the migration runner
-- is wpmgr_app.

INSERT INTO "public"."ability_catalogue"
    ("name", "source", "class", "status", "enabled", "approval_mode", "title", "description")
SELECT v.name, 'wpmgr', 'read', 'admitted', true, 'none', v.title, v.description
FROM (VALUES
    ('wpmgr/abilities-inventory', 'List the site''s abilities',
     'Lists every ability registered on the site: its name, which plugin, theme or WordPress itself registered it, its version and the shape of its input. Changes nothing.'),
    ('wpmgr/site-facts', 'Read site facts',
     'Reads the site''s WordPress and agent versions, the page builders it has, and which editors it can create pages with. Changes nothing.'),
    ('wpmgr/content-read', 'Read a page''s text',
     'Reads the text of one page or post on the site, as the content inventory reports it. Changes nothing.')
) AS v(name, title, description)
WHERE NOT EXISTS (
    SELECT 1 FROM "public"."ability_catalogue" c WHERE c.name = v.name
);

-- Skipped when the migration role is wpmgr_app itself; see the header.
DO $$
BEGIN
    IF current_user <> 'wpmgr_app' THEN
        REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON "public"."ability_catalogue" FROM "wpmgr_app";
        REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON "public"."ability_catalogue_audit" FROM "wpmgr_app";
    END IF;
END;
$$;
GRANT SELECT ON "public"."ability_catalogue" TO "wpmgr_app";
GRANT SELECT ON "public"."ability_catalogue_audit" TO "wpmgr_app";

-- ---------------------------------------------------------------------------
-- site_ability_inventory
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS "public"."site_ability_inventory" (
    "tenant_id" uuid NOT NULL
        REFERENCES "public"."tenants" ("id") ON DELETE CASCADE,
    "site_id" uuid NOT NULL,
    CONSTRAINT "site_ability_inventory_site_within_tenant_fkey"
        FOREIGN KEY ("tenant_id", "site_id")
        REFERENCES "public"."sites" ("tenant_id", "id") ON DELETE CASCADE,

    "name" text NOT NULL
        CONSTRAINT "site_ability_inventory_name_check"
        CHECK ("name" ~ '^[a-z0-9-]{1,64}/[a-z0-9-]{1,64}$'),
    CONSTRAINT "site_ability_inventory_pkey" PRIMARY KEY ("site_id", "name"),
    "namespace" text GENERATED ALWAYS AS (split_part("name", '/', 1)) STORED,

    -- Who registered it, as the agent resolved it.
    "owner_kind" text NOT NULL
        CONSTRAINT "site_ability_inventory_owner_kind_check"
        CHECK ("owner_kind" IN ('core', 'plugin', 'theme', 'mu-plugin', 'unknown')),
    "owner_dir" text NULL
        CONSTRAINT "site_ability_inventory_owner_dir_check"
        CHECK ("owner_dir" ~ '^[A-Za-z0-9._-]{1,100}$'),
    "owner_ok" boolean NULL,
    "owner_version" text NULL
        CONSTRAINT "site_ability_inventory_owner_version_check"
        CHECK ("owner_version" ~ '^[0-9A-Za-z.+-]{1,32}$'),

    "schema_struct_sha256" text NULL
        CONSTRAINT "site_ability_inventory_schema_struct_sha256_check"
        CHECK ("schema_struct_sha256" ~ '^[0-9a-f]{64}$'),
    -- Structural projections, capped. Site-origin; never returned raw.
    "input_schema" jsonb NULL
        CONSTRAINT "site_ability_inventory_input_schema_check"
        CHECK ("input_schema" IS NULL OR (
            jsonb_typeof("input_schema") = 'object'
            AND octet_length("input_schema"::text) <= 32768)),
    "output_schema" jsonb NULL
        CONSTRAINT "site_ability_inventory_output_schema_check"
        CHECK ("output_schema" IS NULL OR (
            jsonb_typeof("output_schema") = 'object'
            AND octet_length("output_schema"::text) <= 32768)),
    "annotations" jsonb NULL
        CONSTRAINT "site_ability_inventory_annotations_check"
        CHECK ("annotations" IS NULL OR (
            jsonb_typeof("annotations") = 'object'
            AND octet_length("annotations"::text) <= 4096)),

    -- Site text: the label and description the registrant supplied. Cleaned
    -- and capped by Go; capped again here.
    "site_label" text NULL
        CONSTRAINT "site_ability_inventory_site_label_check"
        CHECK (char_length("site_label") <= 200),
    "site_description" text NULL
        CONSTRAINT "site_ability_inventory_site_description_check"
        CHECK (char_length("site_description") <= 1000),

    "checked_at" timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS "site_ability_inventory_tenant_idx"
    ON "public"."site_ability_inventory" ("tenant_id");
CREATE INDEX IF NOT EXISTS "site_ability_inventory_site_namespace_idx"
    ON "public"."site_ability_inventory" ("site_id", "namespace", "name");

ALTER TABLE "public"."site_ability_inventory" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."site_ability_inventory" FORCE ROW LEVEL SECURITY;

GRANT SELECT, INSERT, UPDATE, DELETE ON "public"."site_ability_inventory" TO "wpmgr_app";
REVOKE TRUNCATE ON "public"."site_ability_inventory" FROM "wpmgr_app";

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'site_ability_inventory'
          AND policyname = 'site_ability_inventory_tenant_isolation'
    ) THEN
        CREATE POLICY "site_ability_inventory_tenant_isolation"
            ON "public"."site_ability_inventory"
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'site_ability_inventory'
          AND policyname = 'site_ability_inventory_site_scope'
    ) THEN
        CREATE POLICY "site_ability_inventory_site_scope"
            ON "public"."site_ability_inventory"
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
-- site_ability_inventory_runs
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS "public"."site_ability_inventory_runs" (
    "tenant_id" uuid NOT NULL
        REFERENCES "public"."tenants" ("id") ON DELETE CASCADE,
    "site_id" uuid PRIMARY KEY,
    CONSTRAINT "site_ability_inventory_runs_site_within_tenant_fkey"
        FOREIGN KEY ("tenant_id", "site_id")
        REFERENCES "public"."sites" ("tenant_id", "id") ON DELETE CASCADE,
    "checked_at" timestamptz NOT NULL,
    "snapshot_id" uuid NOT NULL,
    -- False when the site has no Abilities API (WordPress before it shipped).
    "api_present" boolean NOT NULL,
    "abilities_stored" integer NOT NULL
        CONSTRAINT "site_ability_inventory_runs_abilities_stored_check"
        CHECK ("abilities_stored" >= 0),
    "truncated" boolean NOT NULL
);

CREATE INDEX IF NOT EXISTS "site_ability_inventory_runs_tenant_idx"
    ON "public"."site_ability_inventory_runs" ("tenant_id");

ALTER TABLE "public"."site_ability_inventory_runs" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."site_ability_inventory_runs" FORCE ROW LEVEL SECURITY;

GRANT SELECT, INSERT, UPDATE ON "public"."site_ability_inventory_runs" TO "wpmgr_app";
REVOKE DELETE, TRUNCATE ON "public"."site_ability_inventory_runs" FROM "wpmgr_app";

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'site_ability_inventory_runs'
          AND policyname = 'site_ability_inventory_runs_tenant_isolation'
    ) THEN
        CREATE POLICY "site_ability_inventory_runs_tenant_isolation"
            ON "public"."site_ability_inventory_runs"
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'site_ability_inventory_runs'
          AND policyname = 'site_ability_inventory_runs_site_scope'
    ) THEN
        CREATE POLICY "site_ability_inventory_runs_site_scope"
            ON "public"."site_ability_inventory_runs"
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
