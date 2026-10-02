-- m160: disable a vendor read caught with side effects, per tenant at once and
-- fleet-wide on three distinct paying or established tenants (owner ruling 4,
-- as amended 2026-10-02).
--
-- Engine slice E3. Five parts, all additive.
--
-- ===========================================================================
-- RULES
-- ===========================================================================
--
--   R1 Count distinct TENANTS, not sites: one contribution per tenant, however
--      many of its sites report.
--   R2 Only a QUALIFIED tenant counts: paid (an active paid subscription) or
--      aged (created at least ability_side_effect_tenant_min_age() ago).
--   R3 EPOCH RESET: when a superadmin re-enables the entry, only reports made
--      after that re-enable count toward the next fleet-wide disable.
--   R4 The reporting tenant has the entry disabled for ITSELF at once,
--      whatever the fleet count. A tenant admin or superadmin re-enables it
--      for that tenant.
--   R5 The definer derives the tenant from app.tenant_id and refuses a site
--      that is not that tenant's.
--
-- ===========================================================================
-- 1. Qualification (R2)
-- ===========================================================================
--
-- ability_side_effect_tenant_min_age() is the ONE place the 30 days lives.
-- ability_side_effect_tenant_qualifies(tenant) reads the billing source of
-- truth, the tenants row (plan, plan_status, grace_until; the same columns
-- internal/billing resolves Entitlements from), and answers true when the
-- tenant is neither soft-deleted nor suspended AND either:
--
--   * paid: plan <> 'free' AND (plan_status = 'active'
--           OR (plan_status = 'past_due' AND grace_until > now())).
--     That is a subscription money is being collected on. 'trialing' and
--     'comped' are NOT paid: a trial costs nothing to start, and a comp is a
--     grant, not a payment. Either still qualifies through age.
--   * aged: created_at <= now() - ability_side_effect_tenant_min_age().
--
-- tenants carries no RLS (m130 DECISION 1), so the definer reads it whole.
--
-- ===========================================================================
-- 2. ability_read_side_effect_reports: GLOBAL, definer-only (R1, R2, R3)
-- ===========================================================================
--
-- One row per (entry, epoch, tenant). The primary key is the one-contribution
-- rule: a second site of the same tenant in the same epoch lands on the same
-- row. first_site_id is the site that first reported, for an operator.
--
-- epoch is the id of the entry's latest ability_catalogue_audit row that moved
-- enabled from false to true (a superadmin re-enable through
-- admin_upsert_ability_catalogue_entry), or 0 when there is none. It is
-- DERIVED at report time from the append-only audit, so no new column on
-- ability_catalogue and no change to its row hash. A re-enable writes a new
-- audit id, so every row from before it sits in an older epoch and stops
-- counting, and a tenant counted before is counted again only if it reports
-- again.
--
-- qualified is evaluated at report time and only ever moves false -> true: a
-- tenant that reported while young and unpaid becomes counted when it reports
-- again after qualifying, and a counted tenant that later downgrades stays
-- counted, so a billing change never quietly lowers a count a disable was
-- decided on.
--
-- RLS, as m160 decided before: ENABLE with NO policy, NOT FORCE, ALL revoked
-- from wpmgr_app when the migration role is not wpmgr_app. A direct statement
-- from the app role is refused with 42501. SINGLE-DSN INSTALLS HAVE NO
-- DATABASE-LEVEL FENCE on this table, exactly as m155 documents for
-- ability_catalogue. No FK to tenants and no cascade, so a deleted tenant stays
-- counted.
--
-- ===========================================================================
-- 3. ability_tenant_disables: TENANT-SCOPED (R4)
-- ===========================================================================
--
-- One row per (tenant, entry). The entry is disabled for the tenant while
-- reenabled_at IS NULL. A re-enable sets reenabled_at and reenabled_by_user_id
-- and keeps the row as the record; a later report from that tenant clears them
-- and disables again.
--
-- RLS is the m19 tenant pattern: ENABLE and FORCE, a permissive
-- ability_tenant_disables_tenant_isolation with USING and WITH CHECK on
-- app.tenant_id. FORCE also binds the definer that writes it, so the definer
-- can only write the tenant in app.tenant_id.
--
-- No _site_scope policy, deliberately: the table carries no site_id and the
-- disable governs the whole tenant. A restrictive site-scope policy keyed on a
-- reporting site would hide the disable from a site-scoped session outside that
-- site, which would make the disable inert for exactly those sessions. No
-- _agent policy: the agent never reads or writes it.
--
-- Privileges for wpmgr_app: SELECT, and UPDATE of (reenabled_at,
-- reenabled_by_user_id) only. No INSERT (the definer is the one way in), no
-- DELETE or TRUNCATE (the record of a disable is kept). Skipped when the
-- migration role is wpmgr_app, for the same single-DSN reason as part 2; RLS
-- still holds there because the table is FORCEd.
--
-- tenant_id cascades from tenants: the row is the tenant's own setting and
-- names nothing that must be reclaimed. entry_id is NO ACTION.
--
-- ===========================================================================
-- 4. record_ability_read_side_effect(entry, site, tenant) RETURNS int
-- ===========================================================================
--
-- SECURITY DEFINER, search_path pinned, EXECUTE revoked from PUBLIC and
-- granted to wpmgr_app only. The signature is unchanged; the RETURN is now the
-- number of qualified tenants counted in the current epoch.
--
--   * any argument NULL                                  -> 22023
--   * app.tenant_id unset, or not p_tenant_id            -> 42501
--   * p_site_id is not a site of that tenant             -> 42501
--   * no entry                                           -> P0002
--   * entry is not source vendor, class read             -> 42501
--
-- Nothing is recorded on a refusal. The site lookup runs under sites' FORCEd
-- policies with the caller's GUCs, so a site-scoped session can only report a
-- site in its scope.
--
-- It takes m155's per-name advisory lock (hashtext('ability_catalogue'),
-- hashtext(name)), the key admin_upsert_ability_catalogue_entry takes. Every
-- reporter for one entry and every superadmin re-enable of it therefore
-- serialise, so the epoch read and the count each see the previous writer's
-- commit. The caller must run it in its own READ COMMITTED transaction.
--
-- Then: upsert the tenant disable (R4), upsert the report row, and when this
-- call made the tenant newly qualified-and-counted in this epoch, the count is
-- 3 or more, and the entry is enabled, set enabled = false and write one
-- ability_catalogue_audit row with actor_user_id NULL.
--
-- ===========================================================================
-- 5. The audit admits exactly one more NULL-actor shape
-- ===========================================================================
--
-- m157's ability_catalogue_audit_null_actor_is_stamp_check is replaced by
-- ..._null_actor_is_system_check, which admits that shape OR the auto-disable
-- shape: an update, entry hash unchanged, enabled exactly true -> false.
--
-- ===========================================================================
-- CONVERGE PATH
-- ===========================================================================
--
-- None needed: m160 is unmerged and has been applied only by test databases,
-- which are created fresh. This file replaces the pre-review m160 in place by
-- a router ruling. IF NOT EXISTS, CREATE OR REPLACE, DROP ... IF EXISTS and a
-- guarded CREATE POLICY make a re-run converge on the same end state.

-- ---------------------------------------------------------------------------
-- 1. Qualification
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION "public"."ability_side_effect_tenant_min_age"()
RETURNS interval
LANGUAGE sql
IMMUTABLE
AS $$ SELECT interval '30 days' $$;

CREATE OR REPLACE FUNCTION "public"."ability_side_effect_tenant_qualifies"(p_tenant_id uuid)
RETURNS boolean
LANGUAGE sql
STABLE
SET search_path = public, pg_temp
AS $$
    SELECT COALESCE((
        SELECT t.deleted_at IS NULL
           AND t.suspended_at IS NULL
           AND (
                (t.plan <> 'free'
                 AND (t.plan_status = 'active'
                      OR (t.plan_status = 'past_due' AND t.grace_until > now())))
             OR t.created_at <= now() - ability_side_effect_tenant_min_age()
           )
        FROM tenants AS t
        WHERE t.id = p_tenant_id
    ), false)
$$;

REVOKE ALL ON FUNCTION "public"."ability_side_effect_tenant_qualifies"(uuid) FROM PUBLIC;

-- ---------------------------------------------------------------------------
-- 2. The fleet counting table
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS "public"."ability_read_side_effect_reports" (
    "entry_id" uuid NOT NULL
        REFERENCES "public"."ability_catalogue" ("entry_id"),
    "epoch" bigint NOT NULL
        CONSTRAINT "ability_read_side_effect_reports_epoch_check" CHECK ("epoch" >= 0),
    "tenant_id" uuid NOT NULL,
    "first_site_id" uuid NOT NULL,
    "qualified" boolean NOT NULL,
    "first_seen" timestamptz NOT NULL DEFAULT now(),
    "qualified_at" timestamptz NULL,
    CONSTRAINT "ability_read_side_effect_reports_qualified_at_check"
        CHECK ("qualified" = ("qualified_at" IS NOT NULL)),
    PRIMARY KEY ("entry_id", "epoch", "tenant_id")
);

CREATE INDEX IF NOT EXISTS "ability_read_side_effect_reports_tenant_idx"
    ON "public"."ability_read_side_effect_reports" ("tenant_id");

ALTER TABLE "public"."ability_read_side_effect_reports" ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON "public"."ability_read_side_effect_reports" FROM PUBLIC;

-- ---------------------------------------------------------------------------
-- 3. The per-tenant disable
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS "public"."ability_tenant_disables" (
    "tenant_id" uuid NOT NULL
        REFERENCES "public"."tenants" ("id") ON DELETE CASCADE,
    "entry_id" uuid NOT NULL
        REFERENCES "public"."ability_catalogue" ("entry_id"),
    "disabled_at" timestamptz NOT NULL DEFAULT now(),
    "reenabled_at" timestamptz NULL,
    "reenabled_by_user_id" uuid NULL,
    CONSTRAINT "ability_tenant_disables_reenabled_check"
        CHECK (("reenabled_at" IS NULL) = ("reenabled_by_user_id" IS NULL)),
    PRIMARY KEY ("tenant_id", "entry_id")
);

CREATE INDEX IF NOT EXISTS "ability_tenant_disables_tenant_id_idx"
    ON "public"."ability_tenant_disables" ("tenant_id");

ALTER TABLE "public"."ability_tenant_disables" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."ability_tenant_disables" FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'ability_tenant_disables'
          AND policyname = 'ability_tenant_disables_tenant_isolation'
    ) THEN
        CREATE POLICY "ability_tenant_disables_tenant_isolation"
            ON "public"."ability_tenant_disables"
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

REVOKE ALL ON "public"."ability_tenant_disables" FROM PUBLIC;

-- Skipped when the migration role is wpmgr_app itself; see the header.
DO $$
BEGIN
    IF current_user <> 'wpmgr_app' THEN
        REVOKE ALL ON "public"."ability_read_side_effect_reports" FROM "wpmgr_app";
        REVOKE ALL ON "public"."ability_tenant_disables" FROM "wpmgr_app";
        GRANT SELECT ON "public"."ability_tenant_disables" TO "wpmgr_app";
        GRANT UPDATE ("reenabled_at", "reenabled_by_user_id")
            ON "public"."ability_tenant_disables" TO "wpmgr_app";
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- 5. The audit CHECK (before the function that writes the new shape)
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
-- 4. The function
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
    v_tenant uuid;
    v_name   text;
    v_epoch  bigint;
    v_qual   boolean;
    v_newly  boolean;
    v_count  int;
    v_before ability_catalogue;
    v_after  ability_catalogue;
BEGIN
    IF p_entry_id IS NULL OR p_site_id IS NULL OR p_tenant_id IS NULL THEN
        RAISE EXCEPTION 'ability_read_side_effect: entry, site and tenant are required'
            USING ERRCODE = '22023';
    END IF;

    -- R5: the tenant is the transaction's, never the parameter's.
    v_tenant := nullif(current_setting('app.tenant_id', true), '')::uuid;
    IF v_tenant IS NULL OR v_tenant <> p_tenant_id THEN
        RAISE EXCEPTION 'ability_read_side_effect: not the tenant of this transaction'
            USING ERRCODE = '42501';
    END IF;
    PERFORM 1 FROM sites WHERE id = p_site_id AND tenant_id = v_tenant;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ability_read_side_effect: not a site of this tenant'
            USING ERRCODE = '42501';
    END IF;

    SELECT name INTO v_name FROM ability_catalogue WHERE entry_id = p_entry_id;
    IF v_name IS NULL THEN
        RAISE EXCEPTION 'ability_catalogue: no entry %', p_entry_id
            USING ERRCODE = 'P0002';
    END IF;

    -- m155's writer lock, then the row: serialises every reporter and every
    -- admin write of this name, including a superadmin re-enable.
    PERFORM pg_advisory_xact_lock(hashtext('ability_catalogue'), hashtext(v_name));
    SELECT * INTO v_before FROM ability_catalogue
        WHERE entry_id = p_entry_id
        FOR UPDATE;

    IF v_before.source IS DISTINCT FROM 'vendor' OR v_before.class IS DISTINCT FROM 'read' THEN
        RAISE EXCEPTION 'ability_read_side_effect: only a vendor read is recorded'
            USING ERRCODE = '42501';
    END IF;

    -- R4: off for this tenant at once. A re-enabled row is disabled again; a
    -- row already disabled keeps its first disabled_at.
    INSERT INTO ability_tenant_disables (tenant_id, entry_id)
    VALUES (v_tenant, p_entry_id)
    ON CONFLICT (tenant_id, entry_id) DO UPDATE SET
        disabled_at = now(),
        reenabled_at = NULL,
        reenabled_by_user_id = NULL
    WHERE ability_tenant_disables.reenabled_at IS NOT NULL;

    -- R3: the epoch is the latest superadmin re-enable.
    SELECT COALESCE(max(a.id), 0) INTO v_epoch
    FROM ability_catalogue_audit AS a
    WHERE a.entry_id = p_entry_id
      AND a.before_enabled IS FALSE
      AND a.after_enabled IS TRUE;

    -- R1 + R2: one row per tenant per epoch; qualified only moves to true.
    v_qual := ability_side_effect_tenant_qualifies(v_tenant);
    INSERT INTO ability_read_side_effect_reports AS r
        (entry_id, epoch, tenant_id, first_site_id, qualified, qualified_at)
    VALUES (p_entry_id, v_epoch, v_tenant, p_site_id, v_qual,
            CASE WHEN v_qual THEN now() END)
    ON CONFLICT (entry_id, epoch, tenant_id) DO UPDATE SET
        qualified = true,
        qualified_at = now()
    WHERE NOT r.qualified AND EXCLUDED.qualified;
    v_newly := FOUND AND v_qual;

    SELECT count(*) INTO v_count
    FROM ability_read_side_effect_reports
    WHERE entry_id = p_entry_id AND epoch = v_epoch AND qualified;

    IF v_newly AND v_count >= 3 AND v_before.enabled THEN
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
