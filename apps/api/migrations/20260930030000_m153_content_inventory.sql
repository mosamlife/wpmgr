-- m153: content_integrations (the platform allowlist of page builders) and
-- site_content_inventory (which editor owns each page on a site).
--
-- Track B slice S1. Three objects, one migration, because the inventory's
-- rows name integrations and the report reads both.
--
-- ===========================================================================
-- 1. content_integrations: GLOBAL, superadmin-written, app role reads only
-- ===========================================================================
--
-- A platform allowlist, not tenant data: no tenant_id, one row per builder.
--
-- WRITE MODEL. The plugin_signatures (m40) and admin_delete_empty_tenant
-- (m35) patterns combined:
--   * ENABLE ROW LEVEL SECURITY, NOT FORCE, with one FOR SELECT policy
--     USING (true). The table owner (the migration role) bypasses RLS, which
--     is what lets the seed below and the SECURITY DEFINER writer run.
--     wpmgr_app has no write policy, so a write that somehow held a grant
--     would still be refused.
--   * wpmgr_app is REVOKEd INSERT, UPDATE, DELETE and TRUNCATE (m1's default
--     privileges gave it all four). SELECT stays.
--   * The ONE write path is admin_upsert_content_integration(), SECURITY
--     DEFINER, owned by the migration role, EXECUTE granted only to
--     wpmgr_app, reached only through the requireSuperadmin-gated admin
--     routes. It refuses unless the named actor is a users row with
--     is_superadmin, and it writes a content_integrations_audit row with the
--     actor and the before and after row hashes in the same statement.
--   * There is no delete function. Retiring a builder is enabled = false,
--     which the audit records like any other change.
--
-- status is closed to 'detect_only' in this migration. 'admitted' needs the
-- six admission proofs and the S5 ability fields; the migration that builds
-- that admission path widens this CHECK. Until then no path, superadmin
-- included, can record a builder as admitted.
--
-- integration_entry_sha256 is NULL until the Go admin path stamps it. The
-- canonical-JSON hash is defined in Go and recomputed by the agent (Sec-F1);
-- SQL cannot reproduce that canonical form, so the seed does not invent one.
--
-- ===========================================================================
-- 2. site_content_inventory: tenant- and site-scoped, FORCE RLS
-- ===========================================================================
--
-- The content probe's list-mode result for one site, one row per post. The
-- daily job REPLACES a site's rows on each refresh: it upserts every row it
-- saw with a single checked_at, then deletes the site's rows with an older
-- checked_at. Both run in the site's tenant transaction, so tenant isolation
-- admits them. The owner fleet report reads across tenants under app.agent,
-- which is why the agent policy exists and is FOR SELECT only.
--
-- Policies, the m19/m151 set: tenant_isolation (permissive), the RESTRICTIVE
-- site_scope (the m19 predicate as m151 applies it), and a FOR SELECT agent
-- policy.
--
-- Grants: SELECT, INSERT, UPDATE, DELETE for wpmgr_app, because the
-- replacement deletes rows the probe no longer reports. TRUNCATE is REVOKEd:
-- TRUNCATE bypasses row-level security entirely, and this table holds every
-- tenant's rows.
--
-- CASCADE. Both foreign keys cascade from tenants and sites. The table is a
-- re-derivable cache of what the probe last said; it is not an audit record
-- and names no object to reclaim, so nothing is lost with it.
--
-- title is stored only for published posts, capped at 120 bytes (the probe
-- returns titles only for published posts in list mode). Every site-supplied
-- string has a length or shape CHECK; verdict and route_reason are closed
-- sets (Sec-F7).
--
-- ===========================================================================
-- CONVERGE PATH
-- ===========================================================================
--
-- None needed. Every object is new here. CREATE ... IF NOT EXISTS and the
-- policy guards make a re-run a no-op, and the seed is ON CONFLICT DO NOTHING
-- so it never overwrites a row a superadmin has since edited.

-- ---------------------------------------------------------------------------
-- content_integrations
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS "public"."content_integrations" (
    "integration_id" text PRIMARY KEY
        CONSTRAINT "content_integrations_integration_id_shape_check"
        CHECK ("integration_id" ~ '^[a-z0-9]+(-[a-z0-9]+)*$'
               AND length("integration_id") <= 64),
    "display_name" text NOT NULL
        CONSTRAINT "content_integrations_display_name_check"
        CHECK (length(btrim("display_name")) > 0
               AND char_length("display_name") <= 80),
    "enabled" boolean NOT NULL DEFAULT true,
    "status" text NOT NULL
        CONSTRAINT "content_integrations_status_check"
        CHECK ("status" IN ('detect_only')),
    -- Detection: namespace, version_constant, mode_flag, payload_keys,
    -- draft_keys, shortcode_prefixes, special_page_options,
    -- singular_override, override_check, plugin_dir (design 3.3).
    "descriptor" jsonb NOT NULL DEFAULT '{}'::jsonb
        CONSTRAINT "content_integrations_descriptor_object_check"
        CHECK (jsonb_typeof("descriptor") = 'object'),
    -- Ability names and structural schema hashes. NULL until S5.
    "abilities" jsonb NULL
        CONSTRAINT "content_integrations_abilities_object_check"
        CHECK ("abilities" IS NULL OR jsonb_typeof("abilities") = 'object'),
    "min_version" text NULL
        CONSTRAINT "content_integrations_min_version_check"
        CHECK ("min_version" ~ '^[0-9A-Za-z.+-]{1,32}$'),
    "max_tested_version" text NULL
        CONSTRAINT "content_integrations_max_tested_version_check"
        CHECK ("max_tested_version" ~ '^[0-9A-Za-z.+-]{1,32}$'),
    "min_wp_version" text NULL
        CONSTRAINT "content_integrations_min_wp_version_check"
        CHECK ("min_wp_version" ~ '^[0-9]+(\.[0-9]+){0,2}$'),
    "integration_entry_sha256" text NULL
        CONSTRAINT "content_integrations_entry_sha256_check"
        CHECK ("integration_entry_sha256" ~ '^[0-9a-f]{64}$'),
    "created_at" timestamptz NOT NULL DEFAULT now(),
    "updated_at" timestamptz NOT NULL DEFAULT now(),
    -- NULL only for a row the migration seeded.
    "updated_by_user_id" uuid NULL
);

ALTER TABLE "public"."content_integrations" ENABLE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'content_integrations'
          AND policyname = 'content_integrations_read'
    ) THEN
        CREATE POLICY "content_integrations_read"
            ON "public"."content_integrations"
            FOR SELECT
            USING (true);
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- content_integrations_audit: append-only, written only by the definer
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS "public"."content_integrations_audit" (
    "id" bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    "integration_id" text NOT NULL,
    "action" text NOT NULL
        CONSTRAINT "content_integrations_audit_action_check"
        CHECK ("action" IN ('insert', 'update')),
    "actor_user_id" uuid NOT NULL,
    "before_sha256" text NULL
        CONSTRAINT "content_integrations_audit_before_check"
        CHECK ("before_sha256" ~ '^[0-9a-f]{64}$'),
    "after_sha256" text NOT NULL
        CONSTRAINT "content_integrations_audit_after_check"
        CHECK ("after_sha256" ~ '^[0-9a-f]{64}$'),
    CONSTRAINT "content_integrations_audit_before_iff_update_check"
        CHECK (("action" = 'update') = ("before_sha256" IS NOT NULL)),
    "before_enabled" boolean NULL,
    "after_enabled" boolean NOT NULL,
    "at" timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS "content_integrations_audit_integration_idx"
    ON "public"."content_integrations_audit" ("integration_id", "id" DESC);

ALTER TABLE "public"."content_integrations_audit" ENABLE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'content_integrations_audit'
          AND policyname = 'content_integrations_audit_read'
    ) THEN
        CREATE POLICY "content_integrations_audit_read"
            ON "public"."content_integrations_audit"
            FOR SELECT
            USING (true);
    END IF;
END;
$$;

-- The row hash the audit records. Computed in SQL from the stored row, so the
-- audit does not trust a hash the caller supplied. jsonb's text form is
-- deterministic for equal values.
CREATE OR REPLACE FUNCTION "public"."content_integration_row_sha256"(
    r "public"."content_integrations"
)
RETURNS text
LANGUAGE sql
IMMUTABLE
SET search_path = public, pg_temp
AS $$
    SELECT encode(sha256(convert_to(jsonb_build_object(
        'integration_id', r.integration_id,
        'display_name', r.display_name,
        'enabled', r.enabled,
        'status', r.status,
        'descriptor', r.descriptor,
        'abilities', r.abilities,
        'min_version', r.min_version,
        'max_tested_version', r.max_tested_version,
        'min_wp_version', r.min_wp_version,
        'integration_entry_sha256', r.integration_entry_sha256
    )::text, 'UTF8')), 'hex');
$$;

-- The one write path. See section 1 of the header.
CREATE OR REPLACE FUNCTION "public"."admin_upsert_content_integration"(
    p_actor_user_id uuid,
    p_integration_id text,
    p_display_name text,
    p_enabled boolean,
    p_status text,
    p_descriptor jsonb,
    p_abilities jsonb,
    p_min_version text,
    p_max_tested_version text,
    p_min_wp_version text,
    p_integration_entry_sha256 text
)
RETURNS "public"."content_integrations"
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_before content_integrations;
    v_after  content_integrations;
BEGIN
    IF p_actor_user_id IS NULL OR NOT EXISTS (
        SELECT 1 FROM users u
        WHERE u.id = p_actor_user_id AND u.is_superadmin
    ) THEN
        RAISE EXCEPTION 'content_integrations: actor is not a superadmin'
            USING ERRCODE = '42501';
    END IF;

    SELECT * INTO v_before FROM content_integrations
        WHERE integration_id = p_integration_id
        FOR UPDATE;

    INSERT INTO content_integrations AS ci (
        integration_id, display_name, enabled, status, descriptor, abilities,
        min_version, max_tested_version, min_wp_version,
        integration_entry_sha256, updated_at, updated_by_user_id
    ) VALUES (
        p_integration_id, p_display_name, p_enabled, p_status,
        coalesce(p_descriptor, '{}'::jsonb), p_abilities,
        p_min_version, p_max_tested_version, p_min_wp_version,
        p_integration_entry_sha256, now(), p_actor_user_id
    )
    ON CONFLICT (integration_id) DO UPDATE SET
        display_name = EXCLUDED.display_name,
        enabled = EXCLUDED.enabled,
        status = EXCLUDED.status,
        descriptor = EXCLUDED.descriptor,
        abilities = EXCLUDED.abilities,
        min_version = EXCLUDED.min_version,
        max_tested_version = EXCLUDED.max_tested_version,
        min_wp_version = EXCLUDED.min_wp_version,
        integration_entry_sha256 = EXCLUDED.integration_entry_sha256,
        updated_at = now(),
        updated_by_user_id = EXCLUDED.updated_by_user_id
    RETURNING ci.* INTO v_after;

    INSERT INTO content_integrations_audit (
        integration_id, action, actor_user_id,
        before_sha256, after_sha256, before_enabled, after_enabled
    ) VALUES (
        p_integration_id,
        CASE WHEN v_before.integration_id IS NULL THEN 'insert' ELSE 'update' END,
        p_actor_user_id,
        CASE WHEN v_before.integration_id IS NULL THEN NULL
             ELSE content_integration_row_sha256(v_before) END,
        content_integration_row_sha256(v_after),
        v_before.enabled,
        v_after.enabled
    );

    RETURN v_after;
END;
$$;

REVOKE ALL ON FUNCTION "public"."admin_upsert_content_integration"(
    uuid, text, text, boolean, text, jsonb, jsonb, text, text, text, text
) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION "public"."admin_upsert_content_integration"(
    uuid, text, text, boolean, text, jsonb, jsonb, text, text, text, text
) TO "wpmgr_app";

-- ---------------------------------------------------------------------------
-- Seed: the builders ADR-062 names, detect_only, detection unverified.
-- ---------------------------------------------------------------------------
--
-- Display names only. descriptor is '{}' until the S1b scratch day verifies
-- each builder's detection constants and keys; an empty descriptor matches
-- nothing, so an unverified builder shows as "a page builder".
--
-- The table is ENABLE (not FORCE) RLS, so the owner running this migration
-- bypasses RLS and the INSERT lands. The REVOKE comes after the INSERT, the
-- m40.1 order, so the seed also succeeds in the single-DSN model where the
-- migration runner is wpmgr_app and still holds m1's default INSERT grant.

INSERT INTO "public"."content_integrations"
    ("integration_id", "display_name", "enabled", "status", "descriptor")
VALUES
    ('beaver-builder', 'Beaver Builder', true, 'detect_only', '{}'::jsonb),
    ('bricks',         'Bricks',         true, 'detect_only', '{}'::jsonb),
    ('divi',           'Divi',           true, 'detect_only', '{}'::jsonb),
    ('elementor',      'Elementor',      true, 'detect_only', '{}'::jsonb),
    ('wpbakery',       'WPBakery',       true, 'detect_only', '{}'::jsonb)
ON CONFLICT ("integration_id") DO NOTHING;

REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON "public"."content_integrations" FROM "wpmgr_app";
GRANT SELECT ON "public"."content_integrations" TO "wpmgr_app";
REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON "public"."content_integrations_audit" FROM "wpmgr_app";
GRANT SELECT ON "public"."content_integrations_audit" TO "wpmgr_app";

-- ---------------------------------------------------------------------------
-- site_content_inventory
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS "public"."site_content_inventory" (
    "tenant_id" uuid NOT NULL
        REFERENCES "public"."tenants" ("id") ON DELETE CASCADE,
    "site_id" uuid NOT NULL,
    CONSTRAINT "site_content_inventory_site_within_tenant_fkey"
        FOREIGN KEY ("tenant_id", "site_id")
        REFERENCES "public"."sites" ("tenant_id", "id") ON DELETE CASCADE,

    "post_id" bigint NOT NULL
        CONSTRAINT "site_content_inventory_post_id_check"
        CHECK ("post_id" > 0),
    CONSTRAINT "site_content_inventory_pkey" PRIMARY KEY ("site_id", "post_id"),

    "post_type" text NOT NULL
        CONSTRAINT "site_content_inventory_post_type_check"
        CHECK ("post_type" ~ '^[a-z0-9_-]{1,20}$'),
    "post_status" text NOT NULL
        CONSTRAINT "site_content_inventory_post_status_check"
        CHECK ("post_status" ~ '^[a-z0-9_-]{1,20}$'),

    "verdict" text NOT NULL
        CONSTRAINT "site_content_inventory_verdict_check"
        CHECK ("verdict" IN (
            'classic', 'empty', 'block_document', 'builder', 'ambiguous',
            'unrecognised_builder', 'special_page', 'template_may_override'
        )),
    "route_number" smallint NOT NULL
        CONSTRAINT "site_content_inventory_route_number_check"
        CHECK ("route_number" IN (1, 2, 3)),
    "route_reason" text NOT NULL
        CONSTRAINT "site_content_inventory_route_reason_check"
        CHECK ("route_reason" IN (
            'content_column', 'unrecognised_builder', 'template_may_override',
            'special_page', 'empty_page', 'block_editor_unsupported',
            'vendor_ability', 'unpublished_draft_exists', 'ai_draft_pending',
            'draft_unreadable', 'builder_version_below_floor',
            'builder_version_unverified', 'builder_ability_missing',
            'builder_ability_changed', 'ability_owner_mismatch',
            'vendor_requires_admin', 'builder_access_not_granted',
            'builder_not_supported', 'ambiguous_owner'
        )),

    -- The owning editor as the probe named it. No foreign key: the probe may
    -- report an entry that was later disabled, and this row is a record of
    -- what the site said.
    "owner_integration_id" text NULL
        CONSTRAINT "site_content_inventory_owner_integration_id_check"
        CHECK ("owner_integration_id" ~ '^[a-z0-9]+(-[a-z0-9]+)*$'
               AND length("owner_integration_id") <= 64),
    "owner_display_name" text NULL
        CONSTRAINT "site_content_inventory_owner_display_name_check"
        CHECK (char_length("owner_display_name") <= 80),
    "owner_version" text NULL
        CONSTRAINT "site_content_inventory_owner_version_check"
        CHECK ("owner_version" ~ '^[0-9A-Za-z.+-]{1,32}$'),

    "fingerprint" text NULL
        CONSTRAINT "site_content_inventory_fingerprint_check"
        CHECK ("fingerprint" ~ '^sha256:[0-9a-f]{64}$'),

    "title" text NULL
        CONSTRAINT "site_content_inventory_title_length_check"
        CHECK (octet_length("title") <= 120),
    CONSTRAINT "site_content_inventory_title_only_when_published_check"
        CHECK ("title" IS NULL OR "post_status" = 'publish'),

    "checked_at" timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS "site_content_inventory_tenant_idx"
    ON "public"."site_content_inventory" ("tenant_id");
CREATE INDEX IF NOT EXISTS "site_content_inventory_site_owner_idx"
    ON "public"."site_content_inventory" ("site_id", "owner_integration_id", "post_id");

ALTER TABLE "public"."site_content_inventory" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."site_content_inventory" FORCE ROW LEVEL SECURITY;

GRANT SELECT, INSERT, UPDATE, DELETE ON "public"."site_content_inventory" TO "wpmgr_app";
REVOKE TRUNCATE ON "public"."site_content_inventory" FROM "wpmgr_app";

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'site_content_inventory'
          AND policyname = 'site_content_inventory_tenant_isolation'
    ) THEN
        CREATE POLICY "site_content_inventory_tenant_isolation"
            ON "public"."site_content_inventory"
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'site_content_inventory'
          AND policyname = 'site_content_inventory_site_scope'
    ) THEN
        CREATE POLICY "site_content_inventory_site_scope"
            ON "public"."site_content_inventory"
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

-- The owner fleet report aggregates across tenants with a plain SELECT under
-- pool.InAgentTx. Every write runs in the site's tenant transaction, so FOR
-- SELECT is the whole need; FOR ALL would let app.agent (which also serves
-- agent->CP requests) rewrite another tenant's inventory.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'site_content_inventory'
          AND policyname = 'site_content_inventory_agent'
    ) THEN
        CREATE POLICY "site_content_inventory_agent"
            ON "public"."site_content_inventory"
            FOR SELECT
            USING (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;
