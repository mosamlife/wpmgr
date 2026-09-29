-- m146 - apply m108's app-alert rollout decision under the production
-- migrator role.
--
-- WHAT IT GUARANTEES. When app_alert_rollout says fresh_install but sites
-- already existed when m108 applied, this file records the deployment as not
-- fresh: app_alert_rollout.fresh_install becomes false, the column default of
-- alert_configs.app_alerts_enabled becomes false, and app alerting is turned
-- off on the alert_configs rows that received m108's wrong default and were
-- not saved since. When the deployment really was fresh, or m108 already
-- recorded it as not fresh, nothing changes.
--
-- WHY. m108's contract is that app alerting starts off on a deployment that
-- already had sites and on for a fresh install, decided once from whether any
-- site existed. sites is FORCE ROW LEVEL SECURITY, and the production migrator
-- is a NOSUPERUSER NOBYPASSRLS table owner with no app.* setting in scope, so
-- m108 counted no sites and recorded every such deployment as fresh. This
-- file lifts FORCE on sites, alert_configs and site_app_alert_state for its
-- length, so the owner reads every row, and restores it before it returns.
-- row_security is off while FORCE is lifted: a statement a policy would still
-- filter raises an error instead of touching fewer rows. app_alert_rollout
-- has no row level security.
--
-- WHICH ROWS ARE TURNED OFF. This file treats the run as the same boot as
-- m108 only when no app alert state exists, no version after this one is
-- recorded, and no alert_configs row has an updated_at later than m108's
-- applied_at. Then every alert_configs row with app alerting on carries m108's
-- default, so every such row is turned off. When any of the three holds, only
-- rows whose updated_at is no later than m108's applied_at are turned off.
-- When none of the three holds, no row is later than that applied_at, so the
-- two branches turn off the same rows. The third signal covers a deployment
-- that stopped at a release whose newest version was m108 and never evaluated
-- an app alert: it records neither of the other two. updated_at changes only
-- when a user saves the alert settings, so a row saved after m108 is kept as
-- saved. The one exception is a save the previous release makes during a
-- same-boot upgrade, between the start of m108 and this file: that row keeps
-- m108's "on" default, and the tenant can turn it off in the alert settings.
--
-- ORDINAL. 20260810120000 sorts after m108 (20260810000000) and before m109
-- (20260811000000), so on a database that has not yet run m108 it applies in
-- the same boot, straight after it. The label m146 is a name; the ordinal is
-- the order.
--
-- LATE RUN. The runner applies every unapplied version, including one that
-- sorts before versions already applied, so this file also runs on every
-- database already past m108. It reads app_alert_rollout first and returns
-- before any lock unless fresh_install is true. The cutoff is m108's own
-- applied_at from schema_migrations, so a site added since m108 never makes a
-- fresh install look like an upgrade.
--
-- SECOND RUN. On a deployment this file corrected, fresh_install is then
-- false and the first statement returns. On a fresh install it again finds no
-- site older than m108 and changes nothing.
--
-- LOCK WAIT. This file waits at most five seconds for each lock it takes, and
-- a timeout rolls the file back and fails the boot with the previous revision
-- left serving.
--
-- END STATE. On a deployment that had sites at m108: fresh_install false and
-- the app_alerts_enabled default false. On a fresh install: m108's end state,
-- which is what db/schema.sql describes. sites, alert_configs and
-- site_app_alert_state keep ENABLE and FORCE ROW LEVEL SECURITY and every
-- policy; the check at the end refuses to commit otherwise.

DO $$
DECLARE
    v_fresh     boolean;
    v_cutoff    timestamptz;
    v_had_sites boolean;
    v_live      boolean;
BEGIN
    SELECT fresh_install INTO v_fresh FROM "public"."app_alert_rollout";

    IF v_fresh IS DISTINCT FROM true THEN
        RETURN;
    END IF;

    SELECT applied_at INTO v_cutoff
    FROM schema_migrations
    WHERE version = '20260810000000_m108_uptime_app_alerting';

    IF v_cutoff IS NULL THEN
        RAISE EXCEPTION 'm146: m108 has no applied_at in schema_migrations';
    END IF;

    PERFORM set_config('lock_timeout', '5s', true);

    ALTER TABLE "public"."sites" NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE "public"."alert_configs" NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_app_alert_state" NO FORCE ROW LEVEL SECURITY;

    PERFORM set_config('row_security', 'off', true);

    v_had_sites := EXISTS (
        SELECT 1 FROM "public"."sites" WHERE created_at < v_cutoff
    );

    IF v_had_sites THEN
        UPDATE "public"."app_alert_rollout" SET fresh_install = false;

        ALTER TABLE "public"."alert_configs"
            ALTER COLUMN "app_alerts_enabled" SET DEFAULT false;

        v_live := EXISTS (SELECT 1 FROM "public"."site_app_alert_state")
            OR EXISTS (
                SELECT 1 FROM schema_migrations
                WHERE version > '20260810120000_m146_app_alert_rollout_repair'
            )
            OR EXISTS (
                SELECT 1 FROM "public"."alert_configs"
                WHERE updated_at > v_cutoff
            );

        IF NOT v_live THEN
            UPDATE "public"."alert_configs"
            SET app_alerts_enabled = false
            WHERE app_alerts_enabled;
        ELSE
            UPDATE "public"."alert_configs"
            SET app_alerts_enabled = false
            WHERE app_alerts_enabled
              AND updated_at <= v_cutoff;
        END IF;
    END IF;

    PERFORM set_config('row_security', 'on', true);

    ALTER TABLE "public"."sites" FORCE ROW LEVEL SECURITY;
    ALTER TABLE "public"."alert_configs" FORCE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_app_alert_state" FORCE ROW LEVEL SECURITY;

    IF (
        SELECT count(*) FROM pg_class
        WHERE oid IN (
                'public.sites'::regclass,
                'public.alert_configs'::regclass,
                'public.site_app_alert_state'::regclass
            )
          AND relrowsecurity
          AND relforcerowsecurity
    ) <> 3 THEN
        RAISE EXCEPTION 'm146: sites, alert_configs and site_app_alert_state must end with ENABLE and FORCE ROW LEVEL SECURITY';
    END IF;
END;
$$;
