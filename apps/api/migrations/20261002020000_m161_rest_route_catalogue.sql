-- m161: rest_route_catalogue (the GLOBAL allowlist of WordPress REST routes an
-- AI may call), its audit and write paths, the two catalogue entries that run
-- it, and the request facts a REST write carries.
--
-- Engine slice E3, part D2. Five parts, all additive.
--
-- ===========================================================================
-- 1. rest_route_catalogue: GLOBAL, superadmin-written, app role reads only
-- ===========================================================================
--
-- One row per reviewed route. No tenant_id: a platform allowlist, not tenant
-- data. The protection model is m155's ability_catalogue, unchanged:
--
--   * ENABLE ROW LEVEL SECURITY, NOT FORCE, with one FOR SELECT policy
--     USING (true). The owner (the migration role) bypasses RLS, which lets
--     the seed and the SECURITY DEFINER writers run. wpmgr_app has no write
--     policy, so a write that somehow held a grant is still refused.
--   * When the migration role is NOT wpmgr_app, wpmgr_app is REVOKEd INSERT,
--     UPDATE, DELETE and TRUNCATE on the table and its audit, and keeps
--     SELECT.
--   * SINGLE-DSN INSTALLS HAVE NO DATABASE-LEVEL WRITE FENCE, exactly as m155
--     documents: when the migration role is wpmgr_app it owns the table and
--     the functions, and the REVOKE would only disable the definers. It is
--     skipped there, and the application's superadmin gate is the only control
--     over writes on such an install.
--   * The superadmin write path is admin_upsert_rest_route(), SECURITY
--     DEFINER, search_path pinned, EXECUTE granted only to wpmgr_app. It
--     refuses unless the actor is a users row with is_superadmin, serialises
--     writers of one route_id on an advisory lock, and writes a
--     rest_route_catalogue_audit row with the before and after row hashes and
--     the before and after route_sha256 in the same statement. p_create true
--     inserts only (23505 when the route exists); false updates only (P0002
--     when it does not), so an update can never silently become an insert.
--   * No delete function. Retiring a route is enabled = false.
--
-- route_sha256 is NULL until Go stamps it: it is the hash of the exact route
-- JSON bytes the control plane sends the agent, which SQL cannot reproduce.
-- Part 2's stamp definer stores it, as m157's does for entry_sha256.
--
-- CHECKs enforce what is decidable per row:
--   * closed sets for method, class, snapshot and effect_copy;
--   * a read is GET and a GET is a read; a GET has no body keys;
--   * a write carries snapshot post_fields, a target naming one of its path
--     params, an operator_permission, and an effect_copy other than 'none';
--     a read carries snapshot 'none' and effect_copy 'none';
--   * output_fields is NOT NULL and in m159's shape grammar
--     (ability_output_shape_valid), capped at 16 KiB;
--   * path_params, query_keys and body_keys are typed key specs
--     (rest_route_params_valid); pinned_query maps keys to short strings;
--   * no key is both pinned and caller-supplied, and no path param is also a
--     query or body key;
--   * the keys the agent pins or never forwards (context, status, author,
--     password, meta, slug, template) are never caller-supplied;
--   * owner ruling 5 (v1 reads are published content only): a pinned context
--     is 'view', and a read's pinned status, when present, is publish or
--     inherit. Widening either needs a migration, which is the intent.
--
-- snapshot is NOT NULL with 'none' for a read, as ability_catalogue's is. A
-- nullable snapshot would let "class = 'read' OR snapshot <> 'none'" pass for
-- a write with a NULL snapshot, because a NULL CHECK passes.
--
-- ===========================================================================
-- 2. stamp_wpmgr_rest_route_hash(): fill a NULL route_sha256, once
-- ===========================================================================
--
-- m157's stamp, for routes. Every route is WPMgr's own reviewed row, so there
-- is no source to check; the only change it can make is NULL -> a well-formed
-- hash:
--
--   * hash not ^[0-9a-f]{64}$      -> 22023
--   * no route                     -> P0002
--   * route_sha256 already set     -> 55000 (a concurrent stamper that lost
--                                     the race; re-read)
--
-- It takes the per-route advisory lock and a row lock, so it serialises with
-- the superadmin writer, and writes one audit row with actor_user_id NULL.
-- rest_route_catalogue_audit_null_actor_is_stamp_check admits a NULL actor
-- for exactly that shape: an update from a NULL route hash to a set one,
-- enabled unchanged. Nothing else; a superadmin write always names its actor.
--
-- ===========================================================================
-- 3. Seed: the v1 routes (engine v4 §6), and the two catalogue entries
-- ===========================================================================
--
-- Reads: pages and posts list and get (pinned status=publish, context=view),
-- categories and tags list, media list (pinned status=inherit), types and
-- taxonomies. Writes: pages and posts update-fields (title and excerpt only).
-- No context=edit row and no own-drafts row (owner ruling 5). route_sha256
-- NULL until Go stamps it. ON CONFLICT DO NOTHING on route_id, so a re-run
-- never overwrites a row a superadmin has since edited.
--
-- ability_catalogue gains wpmgr/rest-read (class read, approval none) and
-- wpmgr/rest-write (class write, per_call, snapshot post_fields,
-- operator_permission site.content.edit, effect_copy live, preview
-- structured), owner ruling 1. Both are source wpmgr, so m159's
-- schema_pinned and output_fields CHECKs exempt them: the per-route
-- output_fields is the pinned shape. entry_sha256 NULL until the boot stamp.
-- NOT EXISTS-guarded on the name, as m155's and m157's seeds are.
--
-- ===========================================================================
-- 4. assistant_ability_requests: route_id, route_sha256, card_facts
-- ===========================================================================
--
-- Recorded facts of a REST write request, immutable after insert: the
-- table-level INSERT grant covers them and m156's column-level UPDATE grant
-- does not name them, so wpmgr_app cannot change them. CHECKs:
--   * route_id shape, route_sha256 64 lowercase hex, and the two together;
--   * (ability_name = 'wpmgr/rest-write') = (route_id IS NOT NULL), both
--     sides NOT NULL-safe;
--   * card_facts an object of at most 64 KiB, present on every rest-write.
-- The not_sent_reason closed set gains route_changed and route_disabled, the
-- W1 closes for a route edited or disabled after approval.
--
-- Existing rows satisfy every new CHECK (route_id NULL, no rest-write row can
-- predate this migration's catalogue entry). ADD CONSTRAINT validates the
-- whole table without row security, so a violation fails the boot loudly
-- instead of passing on a count FORCE RLS hid from the owner.
--
-- RLS is unchanged and row-level: ENABLE + FORCE, tenant_isolation, the
-- RESTRICTIVE site_scope and the FOR SELECT agent policy (m156) cover the new
-- columns.
--
-- ===========================================================================
-- CONVERGE PATH
-- ===========================================================================
--
-- None needed: m161 is new. CREATE ... IF NOT EXISTS, CREATE OR REPLACE,
-- policy and constraint existence guards, DROP CONSTRAINT IF EXISTS before
-- the not_sent_reason re-ADD, and conflict-guarded seeds make a re-run
-- converge on the same end state.

-- ---------------------------------------------------------------------------
-- 1a. Validators (before the CHECKs that use them)
-- ---------------------------------------------------------------------------

-- A typed key spec: {key: {"type": int|string|enum|int_list, ...}}.
--   int:      min, max (integers, min <= max)
--   string:   max_len (1..10000)
--   enum:     values (1..32 strings, ^[a-z0-9_-]{1,32}$)
--   int_list: max_items (1..100)
-- Optional on every type: required (boolean). No other member.
CREATE OR REPLACE FUNCTION "public"."rest_route_params_valid"(s jsonb)
RETURNS boolean
LANGUAGE plpgsql
IMMUTABLE STRICT PARALLEL SAFE
SET search_path = public, pg_temp
AS $$
DECLARE
    k text;
    v jsonb;
    m text;
    t text;
    e jsonb;
BEGIN
    IF jsonb_typeof(s) <> 'object' THEN
        RETURN false;
    END IF;
    IF (SELECT count(*) FROM jsonb_object_keys(s)) > 32 THEN
        RETURN false;
    END IF;
    FOR k, v IN SELECT * FROM jsonb_each(s) LOOP
        IF k !~ '^[a-z][a-z0-9_]{0,31}$' OR jsonb_typeof(v) <> 'object' THEN
            RETURN false;
        END IF;
        FOR m IN SELECT jsonb_object_keys(v) LOOP
            IF m NOT IN ('type', 'min', 'max', 'max_len', 'values', 'max_items', 'required') THEN
                RETURN false;
            END IF;
        END LOOP;
        IF v ? 'required' AND jsonb_typeof(v -> 'required') <> 'boolean' THEN
            RETURN false;
        END IF;
        t := v ->> 'type';
        IF t = 'int' THEN
            IF NOT (v ?& ARRAY['min', 'max'])
               OR (v ?| ARRAY['max_len', 'values', 'max_items'])
               OR jsonb_typeof(v -> 'min') <> 'number'
               OR jsonb_typeof(v -> 'max') <> 'number'
               OR (v ->> 'min') !~ '^-?[0-9]{1,10}$'
               OR (v ->> 'max') !~ '^-?[0-9]{1,10}$'
               OR (v ->> 'min')::bigint > (v ->> 'max')::bigint THEN
                RETURN false;
            END IF;
        ELSIF t = 'string' THEN
            IF NOT (v ? 'max_len')
               OR (v ?| ARRAY['min', 'max', 'values', 'max_items'])
               OR jsonb_typeof(v -> 'max_len') <> 'number'
               OR (v ->> 'max_len') !~ '^[0-9]{1,5}$'
               OR (v ->> 'max_len')::int NOT BETWEEN 1 AND 10000 THEN
                RETURN false;
            END IF;
        ELSIF t = 'enum' THEN
            IF NOT (v ? 'values')
               OR (v ?| ARRAY['min', 'max', 'max_len', 'max_items'])
               OR jsonb_typeof(v -> 'values') <> 'array'
               OR jsonb_array_length(v -> 'values') NOT BETWEEN 1 AND 32 THEN
                RETURN false;
            END IF;
            FOR e IN SELECT * FROM jsonb_array_elements(v -> 'values') LOOP
                IF jsonb_typeof(e) <> 'string' OR (e #>> '{}') !~ '^[a-z0-9_-]{1,32}$' THEN
                    RETURN false;
                END IF;
            END LOOP;
        ELSIF t = 'int_list' THEN
            IF NOT (v ? 'max_items')
               OR (v ?| ARRAY['min', 'max', 'max_len', 'values'])
               OR jsonb_typeof(v -> 'max_items') <> 'number'
               OR (v ->> 'max_items') !~ '^[0-9]{1,3}$'
               OR (v ->> 'max_items')::int NOT BETWEEN 1 AND 100 THEN
                RETURN false;
            END IF;
        ELSE
            RETURN false;
        END IF;
    END LOOP;
    RETURN true;
END;
$$;

-- pinned_query: {key: "short printable string"}, at most 16 keys.
CREATE OR REPLACE FUNCTION "public"."rest_route_pinned_valid"(s jsonb)
RETURNS boolean
LANGUAGE sql
IMMUTABLE STRICT PARALLEL SAFE
SET search_path = public, pg_temp
AS $$
    SELECT jsonb_typeof(s) = 'object'
       AND (SELECT count(*) FROM jsonb_object_keys(s)) <= 16
       AND NOT EXISTS (
           SELECT 1 FROM jsonb_each(s) AS e
           WHERE e.key !~ '^[a-z][a-z0-9_]{0,31}$'
              OR jsonb_typeof(e.value) <> 'string'
              OR (e.value #>> '{}') !~ '^[!-~]{1,64}$'
       );
$$;

-- True when no key of a is a key of b.
CREATE OR REPLACE FUNCTION "public"."rest_route_keys_disjoint"(a jsonb, b jsonb)
RETURNS boolean
LANGUAGE sql
IMMUTABLE STRICT PARALLEL SAFE
SET search_path = public, pg_temp
AS $$
    SELECT NOT EXISTS (SELECT 1 FROM jsonb_object_keys(a) AS k WHERE b ? k);
$$;

REVOKE ALL ON FUNCTION "public"."rest_route_params_valid"(jsonb) FROM PUBLIC;
REVOKE ALL ON FUNCTION "public"."rest_route_pinned_valid"(jsonb) FROM PUBLIC;
REVOKE ALL ON FUNCTION "public"."rest_route_keys_disjoint"(jsonb, jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION "public"."rest_route_params_valid"(jsonb) TO "wpmgr_app";
GRANT EXECUTE ON FUNCTION "public"."rest_route_pinned_valid"(jsonb) TO "wpmgr_app";
GRANT EXECUTE ON FUNCTION "public"."rest_route_keys_disjoint"(jsonb, jsonb) TO "wpmgr_app";

-- ---------------------------------------------------------------------------
-- 1b. rest_route_catalogue
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS "public"."rest_route_catalogue" (
    "route_id" text PRIMARY KEY
        CONSTRAINT "rest_route_catalogue_route_id_check"
        CHECK ("route_id" ~ '^[a-z0-9-]{1,64}$'),
    "method" text NOT NULL
        CONSTRAINT "rest_route_catalogue_method_check"
        CHECK ("method" IN ('GET', 'POST', 'PUT', 'PATCH', 'DELETE')),
    "namespace" text NOT NULL
        CONSTRAINT "rest_route_catalogue_namespace_check"
        CHECK ("namespace" ~ '^[a-z0-9_-]{1,64}(/[a-z0-9_.-]{1,32}){0,2}$'),
    -- Our path, with {param} placeholders the agent fills from typed path
    -- params only.
    "template" text NOT NULL
        CONSTRAINT "rest_route_catalogue_template_check"
        CHECK ("template" ~ '^/[a-z0-9_./{}-]{1,200}$'
               AND ("template" = '/' || "namespace"
                    OR starts_with("template", '/' || "namespace" || '/'))),
    -- The exact registered route string the matched handler must report.
    "core_pattern" text NOT NULL
        CONSTRAINT "rest_route_catalogue_core_pattern_check"
        CHECK ("core_pattern" ~ '^/[!-~]{1,255}$'
               AND starts_with("core_pattern", '/' || "namespace")),
    "path_params" jsonb NOT NULL DEFAULT '{}'::jsonb
        CONSTRAINT "rest_route_catalogue_path_params_check"
        CHECK (rest_route_params_valid("path_params")),
    "query_keys" jsonb NOT NULL DEFAULT '{}'::jsonb
        CONSTRAINT "rest_route_catalogue_query_keys_check"
        CHECK (rest_route_params_valid("query_keys")),
    "pinned_query" jsonb NOT NULL DEFAULT '{}'::jsonb
        CONSTRAINT "rest_route_catalogue_pinned_query_check"
        CHECK (rest_route_pinned_valid("pinned_query")),
    "body_keys" jsonb NOT NULL DEFAULT '{}'::jsonb
        CONSTRAINT "rest_route_catalogue_body_keys_check"
        CHECK (rest_route_params_valid("body_keys")),
    "class" text NOT NULL
        CONSTRAINT "rest_route_catalogue_class_check"
        CHECK ("class" IN ('read', 'write')),
    "output_fields" jsonb NOT NULL
        CONSTRAINT "rest_route_catalogue_output_fields_check"
        CHECK (octet_length("output_fields"::text) <= 16384
               AND ability_output_shape_valid("output_fields", 0)),
    "snapshot" text NOT NULL DEFAULT 'none'
        CONSTRAINT "rest_route_catalogue_snapshot_check"
        CHECK ("snapshot" IN ('none', 'post_fields')),
    "target" jsonb NULL
        CONSTRAINT "rest_route_catalogue_target_check"
        -- coalesce: a missing kind or param makes ->> NULL, and a NULL CHECK
        -- passes.
        CHECK ("target" IS NULL OR coalesce(
            jsonb_typeof("target") = 'object'
            AND "target" ->> 'kind' = 'post'
            AND "path_params" ? ("target" ->> 'param'), false)),
    "arg_render" jsonb NOT NULL DEFAULT '{}'::jsonb
        CONSTRAINT "rest_route_catalogue_arg_render_check"
        CHECK (jsonb_typeof("arg_render") = 'object'
               AND octet_length("arg_render"::text) <= 16384),
    "operator_permission" text NULL
        CONSTRAINT "rest_route_catalogue_operator_permission_check"
        CHECK ("operator_permission" ~ '^[a-z]+(\.[a-z_]+){1,3}$'),
    "effect_copy" text NOT NULL DEFAULT 'none'
        CONSTRAINT "rest_route_catalogue_effect_copy_check"
        CHECK ("effect_copy" IN ('draft', 'live', 'none')),
    "enabled" boolean NOT NULL DEFAULT true,
    "min_wp_version" text NULL
        CONSTRAINT "rest_route_catalogue_min_wp_version_check"
        CHECK ("min_wp_version" ~ '^[0-9]+(\.[0-9]+){0,2}$'),

    -- OUR plain-language copy. Never site text.
    "title" text NOT NULL
        CONSTRAINT "rest_route_catalogue_title_check"
        CHECK (length(btrim("title")) > 0 AND char_length("title") <= 80),
    "description" text NOT NULL
        CONSTRAINT "rest_route_catalogue_description_check"
        CHECK (length(btrim("description")) > 0 AND char_length("description") <= 1000),

    "route_sha256" text NULL
        CONSTRAINT "rest_route_catalogue_route_sha256_check"
        CHECK ("route_sha256" ~ '^[0-9a-f]{64}$'),

    -- A read is a GET and a GET is a read. Every column below is NOT NULL.
    CONSTRAINT "rest_route_catalogue_read_is_get_check"
        CHECK (("class" = 'read') = ("method" = 'GET')),
    CONSTRAINT "rest_route_catalogue_get_has_no_body_check"
        CHECK ("method" <> 'GET' OR "body_keys" = '{}'::jsonb),
    -- A write has an undo, a target, an approver permission and an effect; a
    -- read has none of the first and no effect.
    CONSTRAINT "rest_route_catalogue_write_rules_check"
        CHECK (("class" = 'write') = ("snapshot" <> 'none')
               AND ("class" = 'write') = ("effect_copy" <> 'none')
               AND ("class" <> 'write' OR (
                   "target" IS NOT NULL AND "operator_permission" IS NOT NULL))),
    CONSTRAINT "rest_route_catalogue_keys_disjoint_check"
        CHECK (rest_route_keys_disjoint("pinned_query", "query_keys")
               AND rest_route_keys_disjoint("pinned_query", "body_keys")
               AND rest_route_keys_disjoint("path_params", "query_keys")
               AND rest_route_keys_disjoint("path_params", "body_keys")),
    CONSTRAINT "rest_route_catalogue_forbidden_keys_check"
        CHECK (NOT ("query_keys" ?| ARRAY['context', 'status', 'author', 'password', 'meta', 'slug', 'template'])
               AND NOT ("body_keys" ?| ARRAY['context', 'status', 'author', 'password', 'meta', 'slug', 'template'])),
    -- Owner ruling 5: v1 reads are published content only.
    CONSTRAINT "rest_route_catalogue_published_only_check"
        CHECK (coalesce("pinned_query" ->> 'context', 'view') = 'view'
               AND ("class" <> 'read'
                    OR coalesce("pinned_query" ->> 'status', 'publish') IN ('publish', 'inherit'))),

    "created_at" timestamptz NOT NULL DEFAULT now(),
    "updated_at" timestamptz NOT NULL DEFAULT now(),
    -- NULL only for a row the migration seeded or the stamp touched last.
    "updated_by_user_id" uuid NULL
);

ALTER TABLE "public"."rest_route_catalogue" ENABLE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'rest_route_catalogue'
          AND policyname = 'rest_route_catalogue_read'
    ) THEN
        CREATE POLICY "rest_route_catalogue_read"
            ON "public"."rest_route_catalogue"
            FOR SELECT
            USING (true);
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 1c. rest_route_catalogue_audit: append-only, written only by the definers
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS "public"."rest_route_catalogue_audit" (
    "id" bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    "route_id" text NOT NULL,
    "action" text NOT NULL
        CONSTRAINT "rest_route_catalogue_audit_action_check"
        CHECK ("action" IN ('insert', 'update')),
    -- NULL only for the stamp (part 2).
    "actor_user_id" uuid NULL,
    "before_row_sha256" text NULL
        CONSTRAINT "rest_route_catalogue_audit_before_row_check"
        CHECK ("before_row_sha256" ~ '^[0-9a-f]{64}$'),
    "after_row_sha256" text NOT NULL
        CONSTRAINT "rest_route_catalogue_audit_after_row_check"
        CHECK ("after_row_sha256" ~ '^[0-9a-f]{64}$'),
    CONSTRAINT "rest_route_catalogue_audit_before_iff_update_check"
        CHECK (("action" = 'update') = ("before_row_sha256" IS NOT NULL)),
    "before_route_sha256" text NULL,
    "after_route_sha256" text NULL,
    "before_enabled" boolean NULL,
    "after_enabled" boolean NOT NULL,
    CONSTRAINT "rest_route_catalogue_audit_null_actor_is_stamp_check"
        CHECK ("actor_user_id" IS NOT NULL OR (
            "action" = 'update'
            AND "before_route_sha256" IS NULL
            AND "after_route_sha256" IS NOT NULL
            AND "before_enabled" IS NOT DISTINCT FROM "after_enabled")),
    "at" timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS "rest_route_catalogue_audit_route_idx"
    ON "public"."rest_route_catalogue_audit" ("route_id", "id" DESC);

ALTER TABLE "public"."rest_route_catalogue_audit" ENABLE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'rest_route_catalogue_audit'
          AND policyname = 'rest_route_catalogue_audit_read'
    ) THEN
        CREATE POLICY "rest_route_catalogue_audit_read"
            ON "public"."rest_route_catalogue_audit"
            FOR SELECT
            USING (true);
    END IF;
END;
$$;

-- The row hash the audit records, computed in SQL from the stored row so the
-- audit does not trust a caller-supplied hash. Timestamps and the actor are
-- excluded.
CREATE OR REPLACE FUNCTION "public"."rest_route_catalogue_row_sha256"(
    r "public"."rest_route_catalogue"
)
RETURNS text
LANGUAGE sql
IMMUTABLE
SET search_path = public, pg_temp
AS $$
    SELECT encode(sha256(convert_to(jsonb_build_object(
        'route_id', r.route_id,
        'method', r.method,
        'namespace', r.namespace,
        'template', r.template,
        'core_pattern', r.core_pattern,
        'path_params', r.path_params,
        'query_keys', r.query_keys,
        'pinned_query', r.pinned_query,
        'body_keys', r.body_keys,
        'class', r.class,
        'output_fields', r.output_fields,
        'snapshot', r.snapshot,
        'target', r.target,
        'arg_render', r.arg_render,
        'operator_permission', r.operator_permission,
        'effect_copy', r.effect_copy,
        'enabled', r.enabled,
        'min_wp_version', r.min_wp_version,
        'title', r.title,
        'description', r.description,
        'route_sha256', r.route_sha256
    )::text, 'UTF8')), 'hex');
$$;

-- ---------------------------------------------------------------------------
-- 1d. The superadmin write path
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION "public"."admin_upsert_rest_route"(
    p_actor_user_id uuid,
    p_create boolean,
    p_route_id text,
    p_method text,
    p_namespace text,
    p_template text,
    p_core_pattern text,
    p_path_params jsonb,
    p_query_keys jsonb,
    p_pinned_query jsonb,
    p_body_keys jsonb,
    p_class text,
    p_output_fields jsonb,
    p_snapshot text,
    p_target jsonb,
    p_arg_render jsonb,
    p_operator_permission text,
    p_effect_copy text,
    p_enabled boolean,
    p_min_wp_version text,
    p_title text,
    p_description text,
    p_route_sha256 text
)
RETURNS "public"."rest_route_catalogue"
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_before rest_route_catalogue;
    v_after  rest_route_catalogue;
BEGIN
    IF p_actor_user_id IS NULL OR NOT EXISTS (
        SELECT 1 FROM users u
        WHERE u.id = p_actor_user_id AND u.is_superadmin
    ) THEN
        RAISE EXCEPTION 'rest_route_catalogue: actor is not a superadmin'
            USING ERRCODE = '42501';
    END IF;
    IF p_create IS NULL OR p_route_id IS NULL THEN
        RAISE EXCEPTION 'rest_route_catalogue: create flag and route_id are required'
            USING ERRCODE = '22023';
    END IF;

    -- Serialise writers of one route (the stamp takes the same key).
    PERFORM pg_advisory_xact_lock(hashtext('rest_route_catalogue'), hashtext(p_route_id));

    SELECT * INTO v_before FROM rest_route_catalogue
        WHERE route_id = p_route_id
        FOR UPDATE;

    IF p_create THEN
        IF v_before.route_id IS NOT NULL THEN
            RAISE EXCEPTION 'rest_route_catalogue: route % already exists', p_route_id
                USING ERRCODE = '23505';
        END IF;
        INSERT INTO rest_route_catalogue AS rc (
            route_id, method, namespace, template, core_pattern,
            path_params, query_keys, pinned_query, body_keys, class,
            output_fields, snapshot, target, arg_render, operator_permission,
            effect_copy, enabled, min_wp_version, title, description,
            route_sha256, updated_at, updated_by_user_id
        ) VALUES (
            p_route_id, p_method, p_namespace, p_template, p_core_pattern,
            coalesce(p_path_params, '{}'::jsonb), coalesce(p_query_keys, '{}'::jsonb),
            coalesce(p_pinned_query, '{}'::jsonb), coalesce(p_body_keys, '{}'::jsonb),
            p_class, p_output_fields, coalesce(p_snapshot, 'none'), p_target,
            coalesce(p_arg_render, '{}'::jsonb), p_operator_permission,
            coalesce(p_effect_copy, 'none'), p_enabled, p_min_wp_version,
            p_title, p_description, p_route_sha256, now(), p_actor_user_id
        )
        RETURNING rc.* INTO v_after;
    ELSE
        IF v_before.route_id IS NULL THEN
            RAISE EXCEPTION 'rest_route_catalogue: no route %', p_route_id
                USING ERRCODE = 'P0002';
        END IF;
        UPDATE rest_route_catalogue AS rc SET
            method = p_method,
            namespace = p_namespace,
            template = p_template,
            core_pattern = p_core_pattern,
            path_params = coalesce(p_path_params, '{}'::jsonb),
            query_keys = coalesce(p_query_keys, '{}'::jsonb),
            pinned_query = coalesce(p_pinned_query, '{}'::jsonb),
            body_keys = coalesce(p_body_keys, '{}'::jsonb),
            class = p_class,
            output_fields = p_output_fields,
            snapshot = coalesce(p_snapshot, 'none'),
            target = p_target,
            arg_render = coalesce(p_arg_render, '{}'::jsonb),
            operator_permission = p_operator_permission,
            effect_copy = coalesce(p_effect_copy, 'none'),
            enabled = p_enabled,
            min_wp_version = p_min_wp_version,
            title = p_title,
            description = p_description,
            route_sha256 = p_route_sha256,
            updated_at = now(),
            updated_by_user_id = p_actor_user_id
        WHERE rc.route_id = p_route_id
        RETURNING rc.* INTO v_after;
    END IF;

    INSERT INTO rest_route_catalogue_audit (
        route_id, action, actor_user_id,
        before_row_sha256, after_row_sha256,
        before_route_sha256, after_route_sha256,
        before_enabled, after_enabled
    ) VALUES (
        v_after.route_id,
        CASE WHEN v_before.route_id IS NULL THEN 'insert' ELSE 'update' END,
        p_actor_user_id,
        CASE WHEN v_before.route_id IS NULL THEN NULL
             ELSE rest_route_catalogue_row_sha256(v_before) END,
        rest_route_catalogue_row_sha256(v_after),
        v_before.route_sha256,
        v_after.route_sha256,
        v_before.enabled,
        v_after.enabled
    );

    RETURN v_after;
END;
$$;

REVOKE ALL ON FUNCTION "public"."admin_upsert_rest_route"(
    uuid, boolean, text, text, text, text, text, jsonb, jsonb, jsonb, jsonb,
    text, jsonb, text, jsonb, jsonb, text, text, boolean, text, text, text, text
) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION "public"."admin_upsert_rest_route"(
    uuid, boolean, text, text, text, text, text, jsonb, jsonb, jsonb, jsonb,
    text, jsonb, text, jsonb, jsonb, text, text, boolean, text, text, text, text
) TO "wpmgr_app";

-- ---------------------------------------------------------------------------
-- 2. The stamp
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION "public"."stamp_wpmgr_rest_route_hash"(
    p_route_id text,
    p_sha text
)
RETURNS "public"."rest_route_catalogue"
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_before rest_route_catalogue;
    v_after  rest_route_catalogue;
BEGIN
    IF p_sha IS NULL OR p_sha !~ '^[0-9a-f]{64}$' THEN
        RAISE EXCEPTION 'rest_route_catalogue: route hash is not 64 lowercase hex'
            USING ERRCODE = '22023';
    END IF;
    IF p_route_id IS NULL THEN
        RAISE EXCEPTION 'rest_route_catalogue: no route (NULL)'
            USING ERRCODE = 'P0002';
    END IF;

    PERFORM pg_advisory_xact_lock(hashtext('rest_route_catalogue'), hashtext(p_route_id));
    SELECT * INTO v_before FROM rest_route_catalogue
        WHERE route_id = p_route_id
        FOR UPDATE;

    IF v_before.route_id IS NULL THEN
        RAISE EXCEPTION 'rest_route_catalogue: no route %', p_route_id
            USING ERRCODE = 'P0002';
    END IF;
    IF v_before.route_sha256 IS NOT NULL THEN
        RAISE EXCEPTION 'rest_route_catalogue: route % is already stamped', p_route_id
            USING ERRCODE = '55000';
    END IF;

    UPDATE rest_route_catalogue AS rc SET
        route_sha256 = p_sha,
        updated_at = now()
    WHERE rc.route_id = p_route_id
      AND rc.route_sha256 IS NULL
    RETURNING rc.* INTO v_after;

    INSERT INTO rest_route_catalogue_audit (
        route_id, action, actor_user_id,
        before_row_sha256, after_row_sha256,
        before_route_sha256, after_route_sha256,
        before_enabled, after_enabled
    ) VALUES (
        v_after.route_id,
        'update',
        NULL,
        rest_route_catalogue_row_sha256(v_before),
        rest_route_catalogue_row_sha256(v_after),
        v_before.route_sha256,
        v_after.route_sha256,
        v_before.enabled,
        v_after.enabled
    );

    RETURN v_after;
END;
$$;

REVOKE ALL ON FUNCTION "public"."stamp_wpmgr_rest_route_hash"(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION "public"."stamp_wpmgr_rest_route_hash"(text, text) TO "wpmgr_app";

-- ---------------------------------------------------------------------------
-- 3a. Seed: the v1 routes. Before the REVOKE (the m40.1 order), so the seed
-- also lands where the migration runner is wpmgr_app.
-- ---------------------------------------------------------------------------

INSERT INTO "public"."rest_route_catalogue" (
    "route_id", "method", "namespace", "template", "core_pattern",
    "path_params", "query_keys", "pinned_query", "body_keys", "class",
    "output_fields", "snapshot", "target", "arg_render", "operator_permission",
    "effect_copy", "title", "description"
)
SELECT v.route_id, v.method, 'wp/v2', v.template, v.core_pattern,
       v.path_params::jsonb, v.query_keys::jsonb, v.pinned_query::jsonb,
       v.body_keys::jsonb, v.class, v.output_fields::jsonb, v.snapshot,
       v.target::jsonb, v.arg_render::jsonb, v.operator_permission,
       v.effect_copy, v.title, v.description
FROM (VALUES
    ('wp-v2-pages-list', 'GET', '/wp/v2/pages', '/wp/v2/pages',
     '{}',
     '{"per_page":{"type":"int","min":1,"max":50},"page":{"type":"int","min":1,"max":1000},"search":{"type":"string","max_len":64},"orderby":{"type":"enum","values":["date","modified","title","id"]},"include":{"type":"int_list","max_items":50}}',
     '{"status":"publish","context":"view"}',
     '{}', 'read',
     '{"items":{"fields":{"id":"int","date_gmt":"string","modified_gmt":"string","slug":"string","status":"string","type":"string","link":"string","parent":"int","menu_order":"int","author":"int","title":{"fields":{"rendered":"string"}},"excerpt":{"fields":{"rendered":"string"}}}}}',
     'none', NULL, '{}', NULL, 'none',
     'List published pages',
     'Lists the site''s published pages: title, excerpt, address and dates. Up to 50 per call. Changes nothing.'),
    ('wp-v2-posts-list', 'GET', '/wp/v2/posts', '/wp/v2/posts',
     '{}',
     '{"per_page":{"type":"int","min":1,"max":50},"page":{"type":"int","min":1,"max":1000},"search":{"type":"string","max_len":64},"orderby":{"type":"enum","values":["date","modified","title","id"]},"include":{"type":"int_list","max_items":50}}',
     '{"status":"publish","context":"view"}',
     '{}', 'read',
     '{"items":{"fields":{"id":"int","date_gmt":"string","modified_gmt":"string","slug":"string","status":"string","type":"string","link":"string","author":"int","categories":{"items":"int"},"tags":{"items":"int"},"title":{"fields":{"rendered":"string"}},"excerpt":{"fields":{"rendered":"string"}}}}}',
     'none', NULL, '{}', NULL, 'none',
     'List published posts',
     'Lists the site''s published posts: title, excerpt, address, dates, categories and tags. Up to 50 per call. Changes nothing.'),
    ('wp-v2-pages-get', 'GET', '/wp/v2/pages/{id}', '/wp/v2/pages/(?P<id>[\d]+)',
     '{"id":{"type":"int","min":1,"max":9999999999,"required":true}}',
     '{}',
     '{"status":"publish","context":"view"}',
     '{}', 'read',
     '{"fields":{"id":"int","date_gmt":"string","modified_gmt":"string","slug":"string","status":"string","type":"string","link":"string","parent":"int","menu_order":"int","author":"int","title":{"fields":{"rendered":"string"}},"excerpt":{"fields":{"rendered":"string"}},"content":{"fields":{"rendered":"string"}}}}',
     'none', NULL, '{}', NULL, 'none',
     'Read a published page',
     'Reads one published page: its title, excerpt, text, address and dates. Changes nothing.'),
    ('wp-v2-posts-get', 'GET', '/wp/v2/posts/{id}', '/wp/v2/posts/(?P<id>[\d]+)',
     '{"id":{"type":"int","min":1,"max":9999999999,"required":true}}',
     '{}',
     '{"status":"publish","context":"view"}',
     '{}', 'read',
     '{"fields":{"id":"int","date_gmt":"string","modified_gmt":"string","slug":"string","status":"string","type":"string","link":"string","author":"int","categories":{"items":"int"},"tags":{"items":"int"},"title":{"fields":{"rendered":"string"}},"excerpt":{"fields":{"rendered":"string"}},"content":{"fields":{"rendered":"string"}}}}',
     'none', NULL, '{}', NULL, 'none',
     'Read a published post',
     'Reads one published post: its title, excerpt, text, address, dates, categories and tags. Changes nothing.'),
    ('wp-v2-categories-list', 'GET', '/wp/v2/categories', '/wp/v2/categories',
     '{}',
     '{"per_page":{"type":"int","min":1,"max":50},"page":{"type":"int","min":1,"max":1000},"search":{"type":"string","max_len":64}}',
     '{"context":"view"}',
     '{}', 'read',
     '{"items":{"fields":{"id":"int","count":"int","name":"string","slug":"string","parent":"int","description":"string","link":"string"}}}',
     'none', NULL, '{}', NULL, 'none',
     'List categories',
     'Lists the site''s post categories with their names and how many posts each has. Changes nothing.'),
    ('wp-v2-tags-list', 'GET', '/wp/v2/tags', '/wp/v2/tags',
     '{}',
     '{"per_page":{"type":"int","min":1,"max":50},"page":{"type":"int","min":1,"max":1000},"search":{"type":"string","max_len":64}}',
     '{"context":"view"}',
     '{}', 'read',
     '{"items":{"fields":{"id":"int","count":"int","name":"string","slug":"string","description":"string","link":"string"}}}',
     'none', NULL, '{}', NULL, 'none',
     'List tags',
     'Lists the site''s post tags with their names and how many posts each has. Changes nothing.'),
    ('wp-v2-media-list', 'GET', '/wp/v2/media', '/wp/v2/media',
     '{}',
     '{"per_page":{"type":"int","min":1,"max":50},"page":{"type":"int","min":1,"max":1000},"search":{"type":"string","max_len":64},"media_type":{"type":"enum","values":["image","video","text","application","audio"]}}',
     '{"status":"inherit","context":"view"}',
     '{}', 'read',
     '{"items":{"fields":{"id":"int","date_gmt":"string","slug":"string","link":"string","media_type":"string","mime_type":"string","source_url":"string","alt_text":"string","title":{"fields":{"rendered":"string"}}}}}',
     'none', NULL, '{}', NULL, 'none',
     'List media',
     'Lists files in the site''s media library: name, type, address and alt text. Up to 50 per call. Changes nothing.'),
    ('wp-v2-types', 'GET', '/wp/v2/types', '/wp/v2/types',
     '{}', '{}',
     '{"context":"view"}',
     '{}', 'read',
     '{"fields":{"post":{"fields":{"name":"string","slug":"string","description":"string","hierarchical":"bool","rest_base":"string","rest_namespace":"string","taxonomies":{"items":"string"}}},"page":{"fields":{"name":"string","slug":"string","description":"string","hierarchical":"bool","rest_base":"string","rest_namespace":"string","taxonomies":{"items":"string"}}},"attachment":{"fields":{"name":"string","slug":"string","description":"string","hierarchical":"bool","rest_base":"string","rest_namespace":"string","taxonomies":{"items":"string"}}}}}',
     'none', NULL, '{}', NULL, 'none',
     'List content types',
     'Lists the kinds of content the site has, such as posts, pages and media. Changes nothing.'),
    ('wp-v2-taxonomies', 'GET', '/wp/v2/taxonomies', '/wp/v2/taxonomies',
     '{}', '{}',
     '{"context":"view"}',
     '{}', 'read',
     '{"fields":{"category":{"fields":{"name":"string","slug":"string","description":"string","hierarchical":"bool","rest_base":"string","rest_namespace":"string","types":{"items":"string"}}},"post_tag":{"fields":{"name":"string","slug":"string","description":"string","hierarchical":"bool","rest_base":"string","rest_namespace":"string","types":{"items":"string"}}}}}',
     'none', NULL, '{}', NULL, 'none',
     'List groupings',
     'Lists the ways the site groups its content, such as categories and tags. Changes nothing.'),
    ('wp-v2-pages-update-fields', 'POST', '/wp/v2/pages/{id}', '/wp/v2/pages/(?P<id>[\d]+)',
     '{"id":{"type":"int","min":1,"max":9999999999,"required":true}}',
     '{}', '{}',
     '{"title":{"type":"string","max_len":200},"excerpt":{"type":"string","max_len":1000}}',
     'write',
     '{"fields":{"id":"int","modified_gmt":"string","status":"string","link":"string","title":{"fields":{"rendered":"string"}},"excerpt":{"fields":{"rendered":"string"}}}}',
     'post_fields', '{"kind":"post","param":"id","post_type":"page"}',
     '{"title":{"label":"Title","kind":"text"},"excerpt":{"label":"Excerpt","kind":"text"}}',
     'site.content.edit', 'live',
     'Change a page''s title or excerpt',
     'Changes the title or excerpt of one page. A published page changes immediately. Undo puts back the previous title and excerpt.'),
    ('wp-v2-posts-update-fields', 'POST', '/wp/v2/posts/{id}', '/wp/v2/posts/(?P<id>[\d]+)',
     '{"id":{"type":"int","min":1,"max":9999999999,"required":true}}',
     '{}', '{}',
     '{"title":{"type":"string","max_len":200},"excerpt":{"type":"string","max_len":1000}}',
     'write',
     '{"fields":{"id":"int","modified_gmt":"string","status":"string","link":"string","title":{"fields":{"rendered":"string"}},"excerpt":{"fields":{"rendered":"string"}}}}',
     'post_fields', '{"kind":"post","param":"id","post_type":"post"}',
     '{"title":{"label":"Title","kind":"text"},"excerpt":{"label":"Excerpt","kind":"text"}}',
     'site.content.edit', 'live',
     'Change a post''s title or excerpt',
     'Changes the title or excerpt of one post. A published post changes immediately. Undo puts back the previous title and excerpt.')
) AS v(route_id, method, template, core_pattern, path_params, query_keys,
       pinned_query, body_keys, class, output_fields, snapshot, target,
       arg_render, operator_permission, effect_copy, title, description)
ON CONFLICT ("route_id") DO NOTHING;

-- ---------------------------------------------------------------------------
-- 3b. Seed: the two catalogue entries (owner ruling 1)
-- ---------------------------------------------------------------------------

INSERT INTO "public"."ability_catalogue"
    ("name", "source", "class", "status", "enabled", "approval_mode",
     "title", "description")
SELECT 'wpmgr/rest-read', 'wpmgr', 'read', 'admitted', true, 'none',
       'Read the site''s published content',
       'Reads published pages, posts, categories, tags, media and content types through the site''s own WordPress API, using only routes WPMgr has reviewed. Changes nothing.'
WHERE NOT EXISTS (
    SELECT 1 FROM "public"."ability_catalogue" c WHERE c.name = 'wpmgr/rest-read'
);

INSERT INTO "public"."ability_catalogue"
    ("name", "source", "class", "status", "enabled", "approval_mode",
     "snapshot", "preview", "effect_copy", "operator_permission",
     "title", "description")
SELECT 'wpmgr/rest-write', 'wpmgr', 'write', 'admitted', true, 'per_call',
       'post_fields', 'structured', 'live', 'site.content.edit',
       'Change a page''s or post''s title or excerpt',
       'Changes the title or excerpt of one page or post through the site''s own WordPress API, after a person approves it. A published page changes immediately. Undo puts back the previous values.'
WHERE NOT EXISTS (
    SELECT 1 FROM "public"."ability_catalogue" c WHERE c.name = 'wpmgr/rest-write'
);

-- Skipped when the migration role is wpmgr_app itself; see the header.
DO $$
BEGIN
    IF current_user <> 'wpmgr_app' THEN
        REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON "public"."rest_route_catalogue" FROM "wpmgr_app";
        REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON "public"."rest_route_catalogue_audit" FROM "wpmgr_app";
    END IF;
END;
$$;
GRANT SELECT ON "public"."rest_route_catalogue" TO "wpmgr_app";
GRANT SELECT ON "public"."rest_route_catalogue_audit" TO "wpmgr_app";

-- ---------------------------------------------------------------------------
-- 4. assistant_ability_requests: the REST write facts
-- ---------------------------------------------------------------------------

ALTER TABLE "public"."assistant_ability_requests"
    ADD COLUMN IF NOT EXISTS "route_id"     text  NULL,
    ADD COLUMN IF NOT EXISTS "route_sha256" text  NULL,
    ADD COLUMN IF NOT EXISTS "card_facts"   jsonb NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.assistant_ability_requests'::regclass
          AND conname  = 'assistant_ability_requests_route_id_shape_check'
    ) THEN
        ALTER TABLE "public"."assistant_ability_requests"
            ADD CONSTRAINT "assistant_ability_requests_route_id_shape_check"
            CHECK ("route_id" ~ '^[a-z0-9-]{1,64}$');
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.assistant_ability_requests'::regclass
          AND conname  = 'assistant_ability_requests_route_sha256_shape_check'
    ) THEN
        ALTER TABLE "public"."assistant_ability_requests"
            ADD CONSTRAINT "assistant_ability_requests_route_sha256_shape_check"
            CHECK ("route_sha256" ~ '^[0-9a-f]{64}$');
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.assistant_ability_requests'::regclass
          AND conname  = 'assistant_ability_requests_route_pair_check'
    ) THEN
        ALTER TABLE "public"."assistant_ability_requests"
            ADD CONSTRAINT "assistant_ability_requests_route_pair_check"
            CHECK (("route_id" IS NULL) = ("route_sha256" IS NULL));
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.assistant_ability_requests'::regclass
          AND conname  = 'assistant_ability_requests_rest_write_route_check'
    ) THEN
        ALTER TABLE "public"."assistant_ability_requests"
            ADD CONSTRAINT "assistant_ability_requests_rest_write_route_check"
            CHECK (("ability_name" = 'wpmgr/rest-write') = ("route_id" IS NOT NULL));
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.assistant_ability_requests'::regclass
          AND conname  = 'assistant_ability_requests_card_facts_check'
    ) THEN
        ALTER TABLE "public"."assistant_ability_requests"
            ADD CONSTRAINT "assistant_ability_requests_card_facts_check"
            CHECK ("card_facts" IS NULL OR (
                jsonb_typeof("card_facts") = 'object'
                AND octet_length("card_facts"::text) <= 65536));
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.assistant_ability_requests'::regclass
          AND conname  = 'assistant_ability_requests_rest_write_card_check'
    ) THEN
        ALTER TABLE "public"."assistant_ability_requests"
            ADD CONSTRAINT "assistant_ability_requests_rest_write_card_check"
            CHECK ("ability_name" <> 'wpmgr/rest-write' OR "card_facts" IS NOT NULL);
    END IF;
END;
$$;

-- The W1 closes for a route edited or disabled after approval. A strict
-- widening of m156's set, so every stored row still satisfies it.
ALTER TABLE "public"."assistant_ability_requests"
    DROP CONSTRAINT IF EXISTS "assistant_ability_requests_not_sent_reason_check";
ALTER TABLE "public"."assistant_ability_requests"
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
        'route_disabled'
    ));
