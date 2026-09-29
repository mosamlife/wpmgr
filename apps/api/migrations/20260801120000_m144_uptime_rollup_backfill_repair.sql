-- m144 - rebuild the site_uptime_daily and site_uptime_status rows that m99's
-- one-time backfill should have written, from the raw probes that still exist.
--
-- WHAT IT GUARANTEES. For every (site, UTC day) up to and including the day m99
-- was applied, site_uptime_daily ends holding at least the counts a direct
-- aggregate over site_uptime_probes gives for that day, with m99's math
-- unchanged: up_checks, total_checks, sum_latency_ms and latency_samples are
-- computed exactly as m99 and metrics.pgStore.UpsertRollup compute them. A
-- missing day is inserted. An existing day is replaced only when the raw
-- aggregate has strictly more checks than the row, so the file only ever
-- raises a count and never lowers one: where the retention GC has already
-- pruned some of a day's raw probes, the smaller raw count is refused and the
-- row stays as it was. Every site with no site_uptime_status row gets m99's
-- seed, its latest probe from before m99 was applied; an existing status row
-- is never touched. Days after the one m99 was applied are never touched.
-- The app_up_checks and app_total_checks columns are never read or written.
--
-- The aggregate groups by (site_id, day), not by tenant as well, so it can
-- never propose the same row twice, which ON CONFLICT DO UPDATE refuses. A
-- new row takes the tenant of that day's earliest probe, which is the tenant
-- UpsertRollup stamps on the row it creates for that day.
--
-- WHY. site_uptime_probes, site_uptime_daily and site_uptime_status are FORCE
-- ROW LEVEL SECURITY, and the production migrator is a NOSUPERUSER
-- NOBYPASSRLS table owner with no app.* setting in scope. For that role m99's
-- backfill read no probes and inserted nothing, so the rollup had no row for
-- any day before m99 was applied, and the day it was applied held only the
-- checks the probe worker counted after it. This file lifts FORCE on the
-- three tables for the length of the repair, so the owner reads and writes
-- every row, and restores it before it returns. row_security is off while
-- FORCE is lifted: a statement a policy would still filter raises an error
-- instead of touching fewer rows.
--
-- ORDINAL. 20260801120000 sorts after m99 (20260801000000) and before m100
-- (20260802000000), so on a database that has not yet run m100 it applies
-- straight after m99. The label m144 is a name; the ordinal is the order.
--
-- LATE RUN. The runner applies every unapplied version, including one that
-- sorts before versions already applied, so on a database already past m100
-- this file runs at the next boot. It reads m99's applied_at from
-- schema_migrations, so the window it repairs is the same whenever it runs.
--
-- OUTSIDE THE SERVER'S RUNNER. `atlas migrate apply` records versions in its
-- own table, not in schema_migrations. When schema_migrations does not exist,
-- or holds no row for m99, m99 was not applied by the server's migration
-- runner, so m99's apply time is unknown and the file has no bound for the
-- window. The repair is then skipped: the file raises a NOTICE saying so and
-- returns before it lifts FORCE or locks any of the three tables. It changes
-- nothing, and every later migration applies after it. The consequence is
-- that days up to and including the day m99 was applied are not rebuilt on
-- that database. Where m99's backfill found no probes, as it does for a
-- NOSUPERUSER NOBYPASSRLS owner, the days before it stay missing from
-- site_uptime_daily and the day itself keeps only the checks counted after
-- m99 was applied.
--
-- For operators who apply migrations with atlas, on a database atlas has
-- already taken past m100: the default `--exec-order linear` fails with
-- "added out of order" and applies nothing; `linear-skip` applies nothing,
-- leaves m144 pending out of order, and exits 0; `non-linear` applies m144,
-- and the repair is then skipped as above. atlas does not print the NOTICE.
--
-- RE-RUN. Running the file again changes nothing: every repaired row already
-- equals its raw aggregate, so no count is strictly larger, and every site it
-- seeded already has a status row.
--
-- LOCKS. Lifting FORCE takes an ACCESS EXCLUSIVE lock on each of the three
-- tables until the migration commits, so no other session observes FORCE
-- lifted, and the probe worker and every uptime read wait for the length of
-- the repair. The file waits at most five seconds for each lock, and the
-- repair runs for at most 120 seconds. Either limit rolls the file back
-- whole, with FORCE and every row as they were, and fails the boot with the
-- previous revision left serving. A lock timeout is transient, and the next
-- boot tries again. A statement timeout that comes from the volume of raw
-- checks the repair reads is not: the same rows meet the same cap, so it
-- fails the same way on every boot. The 120-second cap is sized far above
-- this repair's expected work.
--
-- END STATE. No table, column, index or policy changes. The three tables keep
-- ENABLE and FORCE ROW LEVEL SECURITY and every policy; the check at the end
-- refuses to commit otherwise. db/schema.sql is not changed.

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '120s';

DO $$
DECLARE
    v_cutoff timestamptz;
    v_day    date;
    v_bound  timestamptz;
BEGIN
    IF to_regclass('schema_migrations') IS NULL THEN
        RAISE NOTICE 'm144: repair skipped: no schema_migrations table, so this database was not migrated by the server''s migration runner and m99''s apply time is unknown; days up to and including the day m99 was applied are not rebuilt';
        RETURN;
    END IF;

    SELECT applied_at INTO v_cutoff
    FROM schema_migrations
    WHERE version = '20260801000000_m99_uptime_rollup';

    IF v_cutoff IS NULL THEN
        RAISE NOTICE 'm144: repair skipped: schema_migrations has no row for 20260801000000_m99_uptime_rollup, so m99 was not applied by the server''s migration runner and its apply time is unknown; days up to and including the day m99 was applied are not rebuilt';
        RETURN;
    END IF;

    v_day   := (v_cutoff AT TIME ZONE 'UTC')::date;
    v_bound := (v_day + 1)::timestamp AT TIME ZONE 'UTC';

    ALTER TABLE "public"."site_uptime_probes" NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_uptime_daily"  NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_uptime_status" NO FORCE ROW LEVEL SECURITY;

    PERFORM set_config('row_security', 'off', true);

    INSERT INTO "public"."site_uptime_daily"
        ("tenant_id", "site_id", "day", "up_checks", "total_checks", "sum_latency_ms", "latency_samples")
    SELECT
        (array_agg("tenant_id" ORDER BY "probed_at", "id"))[1]           AS "tenant_id",
        "site_id",
        ("probed_at" AT TIME ZONE 'UTC')::date AS "day",
        count(*) FILTER (WHERE "up")                                   AS "up_checks",
        count(*)                                                       AS "total_checks",
        coalesce(sum("total_ms") FILTER (WHERE "up" AND "total_ms" <> 0), 0) AS "sum_latency_ms",
        count(*) FILTER (WHERE "up" AND "total_ms" <> 0)                AS "latency_samples"
    FROM "public"."site_uptime_probes"
    WHERE "probed_at" < v_bound
    GROUP BY "site_id", (("probed_at" AT TIME ZONE 'UTC')::date)
    ON CONFLICT ("site_id", "day") DO UPDATE SET
        "up_checks"       = EXCLUDED."up_checks",
        "total_checks"    = EXCLUDED."total_checks",
        "sum_latency_ms"  = EXCLUDED."sum_latency_ms",
        "latency_samples" = EXCLUDED."latency_samples",
        "updated_at"      = now()
    WHERE EXCLUDED."total_checks" > "site_uptime_daily"."total_checks";

    INSERT INTO "public"."site_uptime_status"
        ("site_id", "tenant_id", "latest_up", "last_probed_at", "tls_expiry")
    SELECT DISTINCT ON ("site_id")
        "site_id", "tenant_id", "up", "probed_at", "tls_expiry"
    FROM "public"."site_uptime_probes"
    WHERE "probed_at" < v_cutoff
    ORDER BY "site_id", "probed_at" DESC
    ON CONFLICT ("site_id") DO NOTHING;

    PERFORM set_config('row_security', 'on', true);

    ALTER TABLE "public"."site_uptime_probes" FORCE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_uptime_daily"  FORCE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_uptime_status" FORCE ROW LEVEL SECURITY;

    IF (
        SELECT count(*)
        FROM pg_class
        WHERE oid IN (
                'public.site_uptime_probes'::regclass,
                'public.site_uptime_daily'::regclass,
                'public.site_uptime_status'::regclass
              )
          AND relrowsecurity
          AND relforcerowsecurity
    ) <> 3 THEN
        RAISE EXCEPTION 'm144: site_uptime_probes, site_uptime_daily and site_uptime_status must end with ENABLE and FORCE ROW LEVEL SECURITY';
    END IF;
END;
$$;
