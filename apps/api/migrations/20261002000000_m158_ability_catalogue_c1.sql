-- m158: ability_catalogue C1 constraints, pinned output fields, and the three
-- core abilities seeded as denied.
--
-- Engine slice E3, part D1. Five parts.
--
-- ===========================================================================
-- 1. wpmgr_version_cmp(text, text): plugin version ordering in SQL
-- ===========================================================================
--
-- IMMUTABLE, STRICT, PARALLEL SAFE. Returns -1, 0 or 1 under the same rules
-- as Go's wpversion.Compare (PHP version_compare semantics): '_', '-' and '+'
-- are separators like '.', a token splits on digit/non-digit boundaries,
-- numeric tokens compare as integers (leading zeros ignored, any width), a
-- missing token is '0', an empty token is the '#' sentinel, and the
-- pre-release order is dev < alpha|a < beta|b < rc < '#' < release < pl|p.
-- '*' compares as the empty string. Digits are ASCII; every version column
-- that reaches it is held to ^[0-9A-Za-z.+-]{1,32}$ by its own CHECK.
--
-- Parity with Go is proved by one fixture, apps/api/db/testdata/
-- wpmgr_version_cmp_cases.json, read by a Go unit test of wpversion.Compare
-- and by an integration test of this function.
--
-- ===========================================================================
-- 2. CHECKs (amendment C1), each counted before it is added
-- ===========================================================================
--
--   * ability_catalogue_schema_pinned_check: a non-wpmgr row carries
--     schema_struct_sha256, unless its class is 'denied'. A denied entry never
--     runs, so there is no shape to pin; the three core denials in part 5 are
--     exactly that case.
--   * ability_catalogue_version_order_check: version_min <= version_max_tested
--     when both are set.
--   * ability_catalogue_output_fields_check (part 3).
--
-- Each is preceded by a count of the rows it would refuse. 0: it is added
-- NOT VALID and then VALIDATEd. Anything else: the migration raises with the
-- count and the boot fails. It is never skipped silently; a superadmin fixes
-- or retires the offending rows and the boot is retried.
--
-- ===========================================================================
-- 3. output_fields jsonb: the pinned output shape of a read
-- ===========================================================================
--
-- NULL or a shape in the grammar the engine uses for its own outputs:
--   {"fields": {key: shape, ...}} | {"items": shape} | "string" | "int" | "bool"
-- validated by ability_output_shape_valid() (IMMUTABLE, depth <= 8), capped
-- at 16 KiB. A read that is not WPMgr's own must pin one:
-- class <> 'read' OR source = 'wpmgr' OR output_fields IS NOT NULL.
--
-- ability_catalogue_row_sha256() now includes it. That changes every row
-- hash; audit rows written before m158 keep the hashes the old function
-- computed, and nothing chains them.
--
-- The Go wire entry gains output_fields as its LAST member, so the canonical
-- entry bytes of every row change. A stored entry_sha256 on a source = 'wpmgr'
-- row is therefore cleared here, and the boot stamp (m157's
-- stamp_wpmgr_ability_entry_hash, driven by abilities.StampOwnEntryHashes)
-- fills it from the new bytes when the new revision starts. The clear is not
-- audited: m157's NULL-actor CHECK admits only the stamp shape, and this
-- migration is the record of the clear. A request approved against the old
-- hash and not yet dispatched closes as entry_changed (fail closed). A vendor
-- or core row's stored hash is NOT cleared: that is the superadmin path's to
-- re-stamp, and before E3 no vendor row is admitted.
--
-- ===========================================================================
-- 4. Non-overlapping admitted version ranges per name
-- ===========================================================================
--
-- admin_upsert_ability_catalogue_entry (the only write path) is replaced by a
-- version taking p_output_fields as its last parameter; the old 32-argument
-- signature is dropped. Under m155's per-name advisory lock (the same key the
-- Go admin repo and m157's stamp take), before it writes an admitted row it
-- refuses with SQLSTATE 23P01 and message ability_catalogue_range_overlap
-- when another admitted row of the same name has an overlapping
-- [version_min, version_max_tested] range. A NULL bound is unbounded. Not an
-- exclusion constraint: text versions have no range type.
--
-- Existing data is counted the same way as part 2: any overlapping admitted
-- pair fails the migration with the count.
--
-- ===========================================================================
-- 5. Seed: core/get-site-info, core/get-environment-info, core/get-user-info
-- ===========================================================================
--
-- source core, class denied, status detect_only, approval none. OUR copy. The
-- reason code is in admission.denied_reason. NOT EXISTS-guarded on the name,
-- so a re-run never overwrites a row a superadmin has edited.
--
-- ===========================================================================
-- CONVERGE PATH
-- ===========================================================================
--
-- None needed: m158 is new. Every step is guarded (ADD COLUMN IF NOT EXISTS,
-- constraint-existence checks, CREATE OR REPLACE, DROP FUNCTION IF EXISTS on
-- the old signature, NOT EXISTS seeds), and the hash clear touches only rows
-- still carrying a hash, which the boot stamp re-fills.

-- ---------------------------------------------------------------------------
-- 1. wpmgr_version_cmp
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION "public"."wpmgr_version_tokens"(v text)
RETURNS text[]
LANGUAGE plpgsql
IMMUTABLE STRICT PARALLEL SAFE
SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    part text;
    out  text[] := '{}';
BEGIN
    IF v = '' THEN
        RETURN ARRAY['#'];
    END IF;
    v := translate(v, '_-+', '...');
    FOREACH part IN ARRAY string_to_array(v, '.') LOOP
        IF part = '' THEN
            out := out || '#'::text;
        ELSE
            out := out || ARRAY(
                SELECT m[1] FROM regexp_matches(part, '([0-9]+|[^0-9]+)', 'g') AS m
            );
        END IF;
    END LOOP;
    RETURN out;
END;
$$;

CREATE OR REPLACE FUNCTION "public"."wpmgr_version_rank"(tok text)
RETURNS int
LANGUAGE sql
IMMUTABLE STRICT PARALLEL SAFE
SET search_path = pg_catalog, pg_temp
AS $$
    SELECT CASE lower(tok)
        WHEN 'dev' THEN 0
        WHEN 'alpha' THEN 1 WHEN 'a' THEN 1
        WHEN 'beta' THEN 2 WHEN 'b' THEN 2
        WHEN 'rc' THEN 3
        WHEN '#' THEN 4
        WHEN 'pl' THEN 6 WHEN 'p' THEN 6
        ELSE 5
    END;
$$;

CREATE OR REPLACE FUNCTION "public"."wpmgr_version_cmp"(a text, b text)
RETURNS int
LANGUAGE plpgsql
IMMUTABLE STRICT PARALLEL SAFE
SET search_path = public, pg_temp
AS $$
DECLARE
    ta text[];
    tb text[];
    x  text;
    y  text;
    xn boolean;
    yn boolean;
    rx int;
    ry int;
    i  int;
    n  int;
BEGIN
    IF a = '*' THEN a := ''; END IF;
    IF b = '*' THEN b := ''; END IF;
    ta := wpmgr_version_tokens(a);
    tb := wpmgr_version_tokens(b);
    n := greatest(coalesce(array_length(ta, 1), 0), coalesce(array_length(tb, 1), 0));
    FOR i IN 1..n LOOP
        x := coalesce(ta[i], '0');
        y := coalesce(tb[i], '0');
        xn := x ~ '^[0-9]+$';
        yn := y ~ '^[0-9]+$';
        IF xn AND yn THEN
            x := ltrim(x, '0');
            y := ltrim(y, '0');
            IF length(x) <> length(y) THEN
                RETURN sign(length(x) - length(y))::int;
            END IF;
            IF x COLLATE "C" < y COLLATE "C" THEN RETURN -1; END IF;
            IF x COLLATE "C" > y COLLATE "C" THEN RETURN 1; END IF;
        ELSE
            rx := CASE WHEN xn THEN 5 ELSE wpmgr_version_rank(x) END;
            ry := CASE WHEN yn THEN 5 ELSE wpmgr_version_rank(y) END;
            IF rx <> ry THEN
                RETURN sign(rx - ry)::int;
            END IF;
        END IF;
    END LOOP;
    RETURN 0;
END;
$$;

REVOKE ALL ON FUNCTION "public"."wpmgr_version_tokens"(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION "public"."wpmgr_version_rank"(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION "public"."wpmgr_version_cmp"(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION "public"."wpmgr_version_tokens"(text) TO "wpmgr_app";
GRANT EXECUTE ON FUNCTION "public"."wpmgr_version_rank"(text) TO "wpmgr_app";
GRANT EXECUTE ON FUNCTION "public"."wpmgr_version_cmp"(text, text) TO "wpmgr_app";

-- ---------------------------------------------------------------------------
-- 3a. output_fields and its shape validator (before the CHECKs that use it)
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION "public"."ability_output_shape_valid"(s jsonb, depth int)
RETURNS boolean
LANGUAGE plpgsql
IMMUTABLE STRICT PARALLEL SAFE
SET search_path = public, pg_temp
AS $$
DECLARE
    k text;
    v jsonb;
BEGIN
    IF depth > 8 THEN
        RETURN false;
    END IF;
    IF jsonb_typeof(s) = 'string' THEN
        RETURN s #>> '{}' IN ('string', 'int', 'bool');
    END IF;
    IF jsonb_typeof(s) <> 'object' THEN
        RETURN false;
    END IF;
    IF s ?& ARRAY['fields'] AND (SELECT count(*) FROM jsonb_object_keys(s)) = 1 THEN
        IF jsonb_typeof(s -> 'fields') <> 'object' THEN
            RETURN false;
        END IF;
        FOR k, v IN SELECT * FROM jsonb_each(s -> 'fields') LOOP
            IF k !~ '^[A-Za-z0-9_-]{1,64}$' OR NOT ability_output_shape_valid(v, depth + 1) THEN
                RETURN false;
            END IF;
        END LOOP;
        RETURN true;
    END IF;
    IF s ?& ARRAY['items'] AND (SELECT count(*) FROM jsonb_object_keys(s)) = 1 THEN
        RETURN ability_output_shape_valid(s -> 'items', depth + 1);
    END IF;
    RETURN false;
END;
$$;

REVOKE ALL ON FUNCTION "public"."ability_output_shape_valid"(jsonb, int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION "public"."ability_output_shape_valid"(jsonb, int) TO "wpmgr_app";

ALTER TABLE "public"."ability_catalogue"
    ADD COLUMN IF NOT EXISTS "output_fields" jsonb NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.ability_catalogue'::regclass
          AND conname  = 'ability_catalogue_output_fields_shape_check'
    ) THEN
        ALTER TABLE "public"."ability_catalogue"
            ADD CONSTRAINT "ability_catalogue_output_fields_shape_check"
            CHECK ("output_fields" IS NULL OR (
                octet_length("output_fields"::text) <= 16384
                AND ability_output_shape_valid("output_fields", 0)));
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 2. The counted CHECKs
-- ---------------------------------------------------------------------------

DO $$
DECLARE
    v_bad bigint;
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.ability_catalogue'::regclass
          AND conname  = 'ability_catalogue_schema_pinned_check'
    ) THEN
        SELECT count(*) INTO v_bad FROM "public"."ability_catalogue"
        WHERE NOT ("source" = 'wpmgr' OR "class" = 'denied' OR "schema_struct_sha256" IS NOT NULL);
        IF v_bad <> 0 THEN
            RAISE EXCEPTION 'm158: % ability_catalogue row(s) are not source wpmgr, not denied, and carry no schema_struct_sha256; pin or retire them before this migration can apply', v_bad
                USING ERRCODE = '23514';
        END IF;
        ALTER TABLE "public"."ability_catalogue"
            ADD CONSTRAINT "ability_catalogue_schema_pinned_check"
            CHECK ("source" = 'wpmgr' OR "class" = 'denied' OR "schema_struct_sha256" IS NOT NULL)
            NOT VALID;
        ALTER TABLE "public"."ability_catalogue"
            VALIDATE CONSTRAINT "ability_catalogue_schema_pinned_check";
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.ability_catalogue'::regclass
          AND conname  = 'ability_catalogue_output_fields_check'
    ) THEN
        SELECT count(*) INTO v_bad FROM "public"."ability_catalogue"
        WHERE NOT ("class" <> 'read' OR "source" = 'wpmgr' OR "output_fields" IS NOT NULL);
        IF v_bad <> 0 THEN
            RAISE EXCEPTION 'm158: % non-wpmgr read row(s) in ability_catalogue have no output_fields; pin or retire them before this migration can apply', v_bad
                USING ERRCODE = '23514';
        END IF;
        ALTER TABLE "public"."ability_catalogue"
            ADD CONSTRAINT "ability_catalogue_output_fields_check"
            CHECK ("class" <> 'read' OR "source" = 'wpmgr' OR "output_fields" IS NOT NULL)
            NOT VALID;
        ALTER TABLE "public"."ability_catalogue"
            VALIDATE CONSTRAINT "ability_catalogue_output_fields_check";
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.ability_catalogue'::regclass
          AND conname  = 'ability_catalogue_version_order_check'
    ) THEN
        SELECT count(*) INTO v_bad FROM "public"."ability_catalogue"
        WHERE "version_min" IS NOT NULL AND "version_max_tested" IS NOT NULL
          AND wpmgr_version_cmp("version_min", "version_max_tested") > 0;
        IF v_bad <> 0 THEN
            RAISE EXCEPTION 'm158: % ability_catalogue row(s) have version_min above version_max_tested; fix them before this migration can apply', v_bad
                USING ERRCODE = '23514';
        END IF;
        ALTER TABLE "public"."ability_catalogue"
            ADD CONSTRAINT "ability_catalogue_version_order_check"
            CHECK ("version_min" IS NULL OR "version_max_tested" IS NULL
                   OR wpmgr_version_cmp("version_min", "version_max_tested") <= 0)
            NOT VALID;
        ALTER TABLE "public"."ability_catalogue"
            VALIDATE CONSTRAINT "ability_catalogue_version_order_check";
    END IF;

    -- Part 4's invariant over the data already stored.
    SELECT count(*) INTO v_bad
    FROM "public"."ability_catalogue" x
    JOIN "public"."ability_catalogue" y
      ON x.name = y.name AND x.entry_id < y.entry_id
    WHERE x.status = 'admitted' AND y.status = 'admitted'
      AND (x.version_min IS NULL OR y.version_max_tested IS NULL
           OR wpmgr_version_cmp(x.version_min, y.version_max_tested) <= 0)
      AND (y.version_min IS NULL OR x.version_max_tested IS NULL
           OR wpmgr_version_cmp(y.version_min, x.version_max_tested) <= 0);
    IF v_bad <> 0 THEN
        RAISE EXCEPTION 'm158: % pair(s) of admitted ability_catalogue rows have overlapping version ranges; fix them before this migration can apply', v_bad
            USING ERRCODE = '23P01';
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 3b. The row hash includes output_fields
-- ---------------------------------------------------------------------------

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
        'entry_sha256', r.entry_sha256,
        'output_fields', r.output_fields
    )::text, 'UTF8')), 'hex');
$$;

-- The wire entry gained output_fields; the boot stamp re-fills these.
UPDATE "public"."ability_catalogue"
SET "entry_sha256" = NULL, "updated_at" = now()
WHERE "source" = 'wpmgr' AND "entry_sha256" IS NOT NULL;

-- ---------------------------------------------------------------------------
-- 4. The write path: output_fields, and no overlapping admitted ranges
-- ---------------------------------------------------------------------------

DROP FUNCTION IF EXISTS "public"."admin_upsert_ability_catalogue_entry"(
    uuid, uuid, text, text, text, text, boolean, text, text, text, text, text,
    text, text, text, text, text[], text, text, text, text, jsonb, text, text,
    jsonb, text, jsonb, text[], text[], jsonb, jsonb, text
);

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
    p_entry_sha256 text,
    p_output_fields jsonb
)
RETURNS "public"."ability_catalogue"
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_before ability_catalogue;
    v_after  ability_catalogue;
    v_clash  uuid;
BEGIN
    IF p_actor_user_id IS NULL OR NOT EXISTS (
        SELECT 1 FROM users u
        WHERE u.id = p_actor_user_id AND u.is_superadmin
    ) THEN
        RAISE EXCEPTION 'ability_catalogue: actor is not a superadmin'
            USING ERRCODE = '42501';
    END IF;

    -- Serialise writers of one ability name (m155's key, which the Go admin
    -- repo and m157's stamp also take): the overlap check below and the write
    -- that follows it see no concurrent writer of this name.
    PERFORM pg_advisory_xact_lock(hashtext('ability_catalogue'), hashtext(p_name));

    IF p_entry_id IS NOT NULL THEN
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
    END IF;

    -- One admitted version range per name at any version.
    IF p_status = 'admitted' THEN
        SELECT c.entry_id INTO v_clash
        FROM ability_catalogue c
        WHERE c.name = p_name
          AND c.status = 'admitted'
          AND c.entry_id IS DISTINCT FROM p_entry_id
          AND (c.version_min IS NULL OR p_version_max_tested IS NULL
               OR wpmgr_version_cmp(c.version_min, p_version_max_tested) <= 0)
          AND (p_version_min IS NULL OR c.version_max_tested IS NULL
               OR wpmgr_version_cmp(p_version_min, c.version_max_tested) <= 0)
        LIMIT 1;
        IF v_clash IS NOT NULL THEN
            RAISE EXCEPTION 'ability_catalogue_range_overlap'
                USING ERRCODE = '23P01',
                      DETAIL = format('admitted entry %s of %s overlaps this version range', v_clash, p_name);
        END IF;
    END IF;

    IF p_entry_id IS NULL THEN
        INSERT INTO ability_catalogue AS ac (
            name, source, class, status, enabled, approval_mode, permission_mode,
            integration_id, owner_dir, version_min, version_max_tested,
            min_wp_version, min_agent_version, schema_struct_sha256,
            dynamic_enum_paths, title, description, usage, operator_permission,
            target, snapshot, preview, arg_render, effect_copy, limits,
            nested_allow, global_option_keys, integration_block, admission,
            entry_sha256, output_fields, updated_at, updated_by_user_id
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
            p_entry_sha256, p_output_fields, now(), p_actor_user_id
        )
        RETURNING ac.* INTO v_after;
    ELSE
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
            output_fields = p_output_fields,
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
    jsonb, text, jsonb, text[], text[], jsonb, jsonb, text, jsonb
) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION "public"."admin_upsert_ability_catalogue_entry"(
    uuid, uuid, text, text, text, text, boolean, text, text, text, text, text,
    text, text, text, text, text[], text, text, text, text, jsonb, text, text,
    jsonb, text, jsonb, text[], text[], jsonb, jsonb, text, jsonb
) TO "wpmgr_app";

-- ---------------------------------------------------------------------------
-- 5. Seed: the three core abilities, denied
-- ---------------------------------------------------------------------------

INSERT INTO "public"."ability_catalogue"
    ("name", "source", "class", "status", "enabled", "approval_mode",
     "title", "description", "admission")
SELECT v.name, 'core', 'denied', 'detect_only', true, 'none',
       v.title, v.description, jsonb_build_object('denied_reason', v.reason)
FROM (VALUES
    ('core/get-site-info', 'Site information (not available)',
     'WordPress offers this only to administrators. WPMgr''s site connection deliberately is not one, so it cannot use it. WPMgr reads the same facts its own way.',
     'principal_lacks_permission'),
    ('core/get-environment-info', 'Server environment (not available)',
     'WordPress offers this only to administrators. WPMgr''s site connection deliberately is not one, so it cannot use it. WPMgr reads the same facts its own way.',
     'principal_lacks_permission'),
    ('core/get-user-info', 'Current user profile (not available)',
     'This would only describe WPMgr''s own connection account on the site, not any person, so WPMgr never runs it.',
     'discloses_service_account')
) AS v(name, title, description, reason)
WHERE NOT EXISTS (
    SELECT 1 FROM "public"."ability_catalogue" c WHERE c.name = v.name
);
