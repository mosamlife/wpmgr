-- m145 - add site_vulnerabilities.notified_at WITH every existing row already
-- marked notified, ahead of m103.
--
-- WHAT IT GUARANTEES. When site_vulnerabilities has no notified_at column yet,
-- this file adds it as timestamptz and fills every existing row with now() in
-- the same statement, then drops the default so the column ends with none.
-- m103 then finds the column present with no NULL in any row that existed
-- before it, so its ADD COLUMN IF NOT EXISTS and its backfill UPDATE both
-- change nothing, and its index and alert_configs columns apply as written.
--
-- WHY. m103's contract is that every finding that existed before it counts as
-- already notified, so the first alert dispatch never emails the backlog.
-- site_vulnerabilities is FORCE ROW LEVEL SECURITY, and the production migrator
-- is a NOSUPERUSER NOBYPASSRLS table owner with no app.* setting in scope, so
-- m103's backfill UPDATE reads no rows and leaves every existing finding NULL,
-- which the dispatch reads as "not yet alerted". Filling the rows in the
-- statement that adds the column meets the contract whatever role applies the
-- migrations: ADD COLUMN reads no rows, so no policy applies to it.
--
-- NO DEFAULT SURVIVES THIS FILE. m103 adds the column with no default, so that
-- a finding inserted after it starts NULL and alerts. The default exists only
-- for the length of the ADD COLUMN that uses it.
--
-- ORDINAL. 20260804120000 sorts after m102 (20260804000000) and before m103
-- (20260805000000), so it applies first. The label m145 is a name; the ordinal
-- is the order.
--
-- LATE RUN. The runner applies every unapplied version, including one that
-- sorts before versions already applied, so this file also runs on every
-- database already past m103. There the column exists, the probe finds it, and
-- the file changes nothing and takes no lock. It never writes to a column it
-- did not add: once m103 has applied, a NULL means a finding not yet alerted,
-- and filling it would suppress that alert.
--
-- END STATE IS m103's. notified_at timestamptz, nullable, no default.
-- db/schema.sql already describes this end state and is not changed.
--
-- RLS. No table, no policy. site_vulnerabilities keeps m79's ENABLE and FORCE
-- ROW LEVEL SECURITY and its policies; a column is covered by them without a
-- statement.

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
        WHERE attrelid = 'public.site_vulnerabilities'::regclass
          AND attname  = 'notified_at'
          AND NOT attisdropped
    ) THEN
        ALTER TABLE "public"."site_vulnerabilities"
            ADD COLUMN "notified_at" timestamptz DEFAULT now();

        ALTER TABLE "public"."site_vulnerabilities"
            ALTER COLUMN "notified_at" DROP DEFAULT;
    END IF;
END;
$$;
