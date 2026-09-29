-- m142 - apply m91's grandfather backfill under the production migrator role.
--
-- WHAT IT GUARANTEES. Every tenant with plan 'free', plan_status 'none' and no
-- max_sites key in plan_overrides, that had more than 3 non-archived sites
-- among the sites that already existed when m91 applied, gets
-- plan_overrides.max_sites set to that count. No other tenant and no other
-- key is written.
--
-- WHY. m91's contract is that no tenant loses capability at the cutover: a
-- tenant already running more than the free cap keeps its count as an
-- override. sites is FORCE ROW LEVEL SECURITY, and the production migrator is
-- a NOSUPERUSER NOBYPASSRLS table owner with no app.* setting in scope, so
-- m91's UPDATE read no sites and wrote no override. This file lifts FORCE on
-- sites for the length of the backfill, so the owner reads every row, and
-- restores it before it returns. row_security is off while FORCE is lifted:
-- a statement a policy would still filter raises an error instead of touching
-- fewer rows. tenants has no row level security.
--
-- WHO IS NEVER TOUCHED. A max_sites override replaces the plan's cap outright,
-- so a tenant that already has one keeps it. A tenant on any plan other than
-- free, or with any billing status other than none, is left as it is.
--
-- ORDINAL. 20260724120000 sorts after m91 (20260724000000) and before m92
-- (20260725000000), so on a database that has not yet run m91 it applies in
-- the same boot, straight after it. The label m142 is a name; the ordinal is
-- the order.
--
-- LATE RUN. The runner applies every unapplied version, including one that
-- sorts before versions already applied, so this file also runs on every
-- database already past m91. The cutoff is m91's own applied_at from
-- schema_migrations, so only sites that existed when m91 ran are counted, and
-- a site added since then never raises an override. A database whose m91
-- already wrote its overrides has the max_sites key set on those tenants, and
-- this file changes nothing for them.
--
-- SECOND RUN. Every tenant this file writes then carries max_sites, so the
-- same statement matches nothing.
--
-- LOCK WAIT. This file waits at most five seconds for its lock on sites, and
-- a timeout rolls the file back and fails the boot with the previous revision
-- left serving.
--
-- END STATE IS m91's. sites keeps ENABLE and FORCE ROW LEVEL SECURITY and
-- every policy; the check at the end refuses to commit otherwise. No column,
-- default, table or policy changes, so db/schema.sql is not changed.

DO $$
DECLARE
    v_cutoff timestamptz;
BEGIN
    SELECT applied_at INTO v_cutoff
    FROM schema_migrations
    WHERE version = '20260724000000_m91_hosted_billing_substrate';

    IF v_cutoff IS NULL THEN
        RAISE EXCEPTION 'm142: m91 has no applied_at in schema_migrations';
    END IF;

    PERFORM set_config('lock_timeout', '5s', true);

    ALTER TABLE "public"."sites" NO FORCE ROW LEVEL SECURITY;

    PERFORM set_config('row_security', 'off', true);

    UPDATE "public"."tenants" t
    SET "plan_overrides" = jsonb_set(t.plan_overrides, '{max_sites}', to_jsonb(cnt.active_count), true)
    FROM (
        SELECT tenant_id, count(*) AS active_count
        FROM "public"."sites"
        WHERE connection_state <> 'archived'
          AND created_at <= v_cutoff
        GROUP BY tenant_id
    ) cnt
    WHERE t.id = cnt.tenant_id
      AND cnt.active_count > 3
      AND t.plan = 'free'
      AND t.plan_status = 'none'
      AND NOT (t.plan_overrides ? 'max_sites');

    PERFORM set_config('row_security', 'on', true);

    ALTER TABLE "public"."sites" FORCE ROW LEVEL SECURITY;

    IF NOT EXISTS (
        SELECT 1 FROM pg_class
        WHERE oid = 'public.sites'::regclass
          AND relrowsecurity
          AND relforcerowsecurity
    ) THEN
        RAISE EXCEPTION 'm142: sites must end with ENABLE and FORCE ROW LEVEL SECURITY';
    END IF;
END;
$$;
