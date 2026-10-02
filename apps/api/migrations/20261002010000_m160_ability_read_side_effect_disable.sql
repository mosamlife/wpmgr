-- m160: automatic fleet-wide disable of a vendor read caught with side effects
-- (owner ruling 4).
--
-- Engine slice E3. Three parts, all additive.
--
-- ===========================================================================
-- 1. ability_read_side_effect_sites: GLOBAL, definer-only
-- ===========================================================================
--
-- One row per (catalogue entry, site) on which a vendor read was caught
-- writing or calling out. The primary key makes a repeat report from the same
-- site a no-op, so the row count per entry is the distinct-site count.
--
-- It holds no tenant content: an entry id, a site id, a tenant id and a
-- timestamp. tenant_id is kept so an operator can see whether the sites span
-- tenants; it is not a tenancy boundary here. RLS is decided deliberately:
--
--   * ENABLE ROW LEVEL SECURITY with NO policy at all, NOT FORCE. A role that
--     is not the owner sees no row and can write none, even holding a grant.
--     The owner (the migration role) bypasses RLS because it is not FORCEd,
--     which is what lets the SECURITY DEFINER function below run. FORCE would
--     disable the definer, as it would for m155's ability_catalogue.
--   * When the migration role is NOT wpmgr_app, wpmgr_app is REVOKEd ALL on
--     the table, so a direct SELECT, INSERT, UPDATE or DELETE is refused with
--     42501 before RLS is consulted. m1's default privileges would otherwise
--     hand it the four.
--   * SINGLE-DSN INSTALLS HAVE NO DATABASE-LEVEL FENCE, exactly as m155
--     documents for ability_catalogue: when the migration role is wpmgr_app it
--     owns the table, and the REVOKE would disable the definer. It is skipped
--     there.
--
-- No foreign key to sites or tenants, and no cascade. A site or tenant that is
-- deleted after it was counted stays counted: the threshold is about the
-- vendor tool, and a deletion must not quietly lower a count a disable was
-- decided on. entry_id references ability_catalogue with the default NO
-- ACTION; the catalogue has no delete path.
--
-- ===========================================================================
-- 2. record_ability_read_side_effect(entry, site, tenant) RETURNS int
-- ===========================================================================
--
-- SECURITY DEFINER, search_path pinned, EXECUTE revoked from PUBLIC and
-- granted to wpmgr_app only. The one write path into part 1.
--
--   * any argument NULL                 -> 22023
--   * no entry                          -> P0002
--   * entry is not source vendor, class read -> 42501 (nothing recorded)
--
-- It takes m155's per-name advisory lock (hashtext('ability_catalogue'),
-- hashtext(name)), the key admin_upsert_ability_catalogue_entry, m157's stamp
-- and the Go admin repo take. Every reporter for one entry therefore
-- serialises, and each count below runs in a fresh READ COMMITTED snapshot
-- after the previous reporter committed, so two concurrent "third" sites
-- cannot both read two. The caller must not run it inside a REPEATABLE READ
-- or SERIALIZABLE transaction that has already taken its snapshot.
--
-- It inserts ON CONFLICT DO NOTHING and returns the entry's distinct-site
-- count. When the insert was NEW, the count is 3 or more, and the entry is
-- enabled, it sets enabled = false and writes one ability_catalogue_audit row
-- with actor_user_id NULL. A repeat report from an already-counted site never
-- disables, so a superadmin who re-enables the entry is overridden only by a
-- site not seen before.
--
-- ===========================================================================
-- 3. The audit admits exactly one more NULL-actor shape
-- ===========================================================================
--
-- m157's ability_catalogue_audit_null_actor_is_stamp_check admitted a NULL
-- actor only for the stamp: an update from a NULL entry hash to a set one,
-- enabled unchanged. It is replaced by ..._null_actor_is_system_check, which
-- admits that shape OR the auto-disable shape: an update, entry hash
-- unchanged, enabled exactly true -> false. Nothing else. Existing rows all
-- satisfy the old CHECK, which is a subset of the new one.
--
-- ===========================================================================
-- CONVERGE PATH
-- ===========================================================================
--
-- None needed: m160 is new. CREATE TABLE IF NOT EXISTS, CREATE OR REPLACE
-- FUNCTION, and DROP CONSTRAINT IF EXISTS of both CHECK names before the ADD
-- make a re-run converge on the same end state.

-- ---------------------------------------------------------------------------
-- 1. The table
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS "public"."ability_read_side_effect_sites" (
    "entry_id" uuid NOT NULL
        REFERENCES "public"."ability_catalogue" ("entry_id"),
    "site_id" uuid NOT NULL,
    "tenant_id" uuid NOT NULL,
    "first_seen" timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY ("entry_id", "site_id")
);

CREATE INDEX IF NOT EXISTS "ability_read_side_effect_sites_tenant_idx"
    ON "public"."ability_read_side_effect_sites" ("tenant_id");

ALTER TABLE "public"."ability_read_side_effect_sites" ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON "public"."ability_read_side_effect_sites" FROM PUBLIC;

-- Skipped when the migration role is wpmgr_app itself; see the header.
DO $$
BEGIN
    IF current_user <> 'wpmgr_app' THEN
        REVOKE ALL ON "public"."ability_read_side_effect_sites" FROM "wpmgr_app";
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 3. The audit CHECK (before the function that writes the new shape)
-- ---------------------------------------------------------------------------

ALTER TABLE "public"."ability_catalogue_audit"
    DROP CONSTRAINT IF EXISTS "ability_catalogue_audit_null_actor_is_stamp_check";
ALTER TABLE "public"."ability_catalogue_audit"
    DROP CONSTRAINT IF EXISTS "ability_catalogue_audit_null_actor_is_system_check";
ALTER TABLE "public"."ability_catalogue_audit"
    ADD CONSTRAINT "ability_catalogue_audit_null_actor_is_system_check"
    CHECK ("actor_user_id" IS NOT NULL OR ("action" = 'update' AND (
        -- m157: the stamp of a wpmgr entry's hash.
        ("before_entry_sha256" IS NULL
         AND "after_entry_sha256" IS NOT NULL
         AND "before_enabled" IS NOT DISTINCT FROM "after_enabled")
        OR
        -- m160: the read side-effect auto-disable.
        ("before_entry_sha256" IS NOT DISTINCT FROM "after_entry_sha256"
         AND "before_enabled" IS TRUE
         AND "after_enabled" IS FALSE))));

-- ---------------------------------------------------------------------------
-- 2. The function
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION "public"."record_ability_read_side_effect"(
    p_entry_id uuid,
    p_site_id uuid,
    p_tenant_id uuid
)
RETURNS int
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_name   text;
    v_new    boolean;
    v_count  int;
    v_before ability_catalogue;
    v_after  ability_catalogue;
BEGIN
    IF p_entry_id IS NULL OR p_site_id IS NULL OR p_tenant_id IS NULL THEN
        RAISE EXCEPTION 'ability_read_side_effect: entry, site and tenant are required'
            USING ERRCODE = '22023';
    END IF;

    SELECT name INTO v_name FROM ability_catalogue WHERE entry_id = p_entry_id;
    IF v_name IS NULL THEN
        RAISE EXCEPTION 'ability_catalogue: no entry %', p_entry_id
            USING ERRCODE = 'P0002';
    END IF;

    -- m155's writer lock, then the row: serialises every reporter and every
    -- admin write of this name.
    PERFORM pg_advisory_xact_lock(hashtext('ability_catalogue'), hashtext(v_name));
    SELECT * INTO v_before FROM ability_catalogue
        WHERE entry_id = p_entry_id
        FOR UPDATE;

    IF v_before.source IS DISTINCT FROM 'vendor' OR v_before.class IS DISTINCT FROM 'read' THEN
        RAISE EXCEPTION 'ability_read_side_effect: only a vendor read is recorded'
            USING ERRCODE = '42501';
    END IF;

    INSERT INTO ability_read_side_effect_sites (entry_id, site_id, tenant_id)
    VALUES (p_entry_id, p_site_id, p_tenant_id)
    ON CONFLICT (entry_id, site_id) DO NOTHING;
    v_new := FOUND;

    SELECT count(*) INTO v_count
    FROM ability_read_side_effect_sites
    WHERE entry_id = p_entry_id;

    IF v_new AND v_count >= 3 AND v_before.enabled THEN
        UPDATE ability_catalogue AS ac SET
            enabled = false,
            updated_at = now(),
            updated_by_user_id = NULL
        WHERE ac.entry_id = p_entry_id
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
    END IF;

    RETURN v_count;
END;
$$;

REVOKE ALL ON FUNCTION "public"."record_ability_read_side_effect"(uuid, uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION "public"."record_ability_read_side_effect"(uuid, uuid, uuid) TO "wpmgr_app";
