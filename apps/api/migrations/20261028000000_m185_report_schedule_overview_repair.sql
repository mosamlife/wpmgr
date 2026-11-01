-- m185 - turn the client report Overview section back on for every saved
-- report schedule that stores it as off.
--
-- WHAT IT GUARANTEES. Every report_schedules row whose sections object holds
-- "overview": false ends holding "overview": true. Nothing else in the row
-- changes: every other key of sections keeps its value, and every other
-- column keeps its value, updated_at included. A row whose sections holds
-- "overview": true, holds no "overview" key, holds any value for it other
-- than the JSON boolean false, or is not a JSON object, is not written.
--
-- WHY. Until GH #972 the dashboard's schedule form never sent
-- sections.overview, and the server read the missing flag as false, so every
-- schedule saved from the dashboard stored "overview": false. The dashboard
-- had no Overview control, so no false stored that way was chosen. From
-- GH #972 the server keeps a stored flag that a save leaves out, and an
-- on-demand report reads the stored flags, so without this file those rows
-- would keep Overview off on every report, scheduled and on demand.
--
-- A false that a caller of the schedule API sent on purpose is turned on as
-- well: the stored value does not say how it got there. Saving the schedule
-- with overview off turns it off again.
--
-- RLS. report_schedules is FORCE ROW LEVEL SECURITY, and the production
-- migrator is a NOSUPERUSER NOBYPASSRLS table owner with no app.* setting in
-- scope, so a plain UPDATE here reads no rows and changes nothing. This file
-- lifts FORCE on report_schedules for its length, so the owner reads every
-- row, and restores it before it returns. row_security is off while FORCE is
-- lifted: a statement a policy would still filter raises an error instead of
-- touching fewer rows. A migrator that is a superuser reads every row anyway.
-- A migrator that does not own report_schedules cannot lift FORCE, and the
-- file then fails the boot rather than repair some of the rows.
--
-- ORDINAL. 20261028000000 sorts after every migration before it. The label
-- m185 is the next free number (m183 and m184 are taken by an open branch);
-- the label is a name, the ordinal is the order. The file reads no other
-- migration's state, so it applies the same wherever it sorts.
--
-- RE-RUN. Running the file again changes nothing: no row stores
-- "overview": false any more, so the UPDATE matches no row.
--
-- LOCKS. Lifting FORCE takes an ACCESS EXCLUSIVE lock on report_schedules
-- until the migration commits, so no other session observes FORCE lifted, and
-- the report scheduler and every schedule read wait for the length of the
-- repair. The file waits at most five seconds for that lock. A timeout rolls
-- the file back whole, with FORCE and every row as they were, and fails the
-- boot; the next boot tries again.
--
-- END STATE. No table, column, index or policy changes. report_schedules
-- keeps ENABLE and FORCE ROW LEVEL SECURITY and both of its policies; the
-- check at the end refuses to commit otherwise. db/schema.sql is not changed.

DO $$
DECLARE
    v_repaired bigint;
BEGIN
    PERFORM set_config('lock_timeout', '5s', true);

    ALTER TABLE "public"."report_schedules" NO FORCE ROW LEVEL SECURITY;

    PERFORM set_config('row_security', 'off', true);

    UPDATE "public"."report_schedules"
    SET "sections" = jsonb_set("sections", '{overview}', 'true'::jsonb, false)
    WHERE "sections" -> 'overview' = 'false'::jsonb;

    GET DIAGNOSTICS v_repaired = ROW_COUNT;

    PERFORM set_config('row_security', 'on', true);

    ALTER TABLE "public"."report_schedules" FORCE ROW LEVEL SECURITY;

    IF NOT EXISTS (
        SELECT 1 FROM pg_class
        WHERE oid = 'public.report_schedules'::regclass
          AND relrowsecurity
          AND relforcerowsecurity
    ) THEN
        RAISE EXCEPTION 'm185: report_schedules must end with ENABLE and FORCE ROW LEVEL SECURITY';
    END IF;

    RAISE NOTICE 'm185: turned the Overview section back on for % report schedule(s)', v_repaired;
END;
$$;
