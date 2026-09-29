-- m148: record why a running backup is being retried, apart from why a backup
-- failed.
--
-- THE CONTRACT
--
-- Adds one column to each of backup_snapshots and backup_schedule_runs:
--
--   attempt_error text NOT NULL DEFAULT ''
--
-- attempt_error is the control plane's description of the most recent failed
-- attempt to start the backup on the site, written while the control plane is
-- still retrying. It is written only while the row's status is 'running' (the
-- write queries in db/query/backups.sql and db/query/schedule_runs.sql carry
-- that guard), and it is cleared by the next proof of life from the site and
-- by completion. '' means no attempt error is outstanding.
--
-- error keeps its existing meaning, unchanged: the final reason a backup
-- failed. A reader decides whether a row failed from status alone, and a
-- running row never carries a failure reason in error. The two are kept in
-- separate columns so that no reader of error, the API, the MCP read tools or
-- the dashboard, can take a row that is still being retried for a failed one.
--
-- When the progress watchdog ends a run it has watched go quiet, the failure
-- reason it writes to error carries the last attempt error after its own
-- message, so the reason the site gave is not lost when the run ends.
--
-- On a failed row attempt_error is left as it was when the row failed. It is
-- a record of the last retry, not a second failure reason; status and error
-- are the outcome.
--
-- WHAT THIS DOES NOT DO
--
-- No backfill. Every existing row takes the default '', which is the correct
-- value for it: no row was ever recorded as retrying before this column
-- existed.
--
-- RLS is unchanged. Both tables already carry ENABLE and FORCE ROW LEVEL
-- SECURITY, the permissive tenant-isolation and agent policies, and the
-- restrictive site-scope policy. A policy applies to a row, not to a column,
-- so a new column is covered by every policy the table already has.
--
-- THE LOCKS
--
-- ADD COLUMN takes ACCESS EXCLUSIVE on the table it alters. With a constant
-- default on PostgreSQL 11 and later the change is recorded in the catalog
-- only: the table is not rewritten and no row is read, so the lock is held
-- only for the catalog update once it is granted.
--
-- The tables are altered parent first (backup_snapshots, then
-- backup_schedule_runs, which references it), the order a cascade from a
-- snapshot delete takes them in.
--
-- THE TIMEOUTS
--
-- The file opens by setting lock_timeout = '2s' and statement_timeout = '10s'
-- with set_config(..., true), which is SET LOCAL: both end with this file's
-- transaction and reach no other migration and no other connection. The
-- settings are in a DO block of their own so the file also runs statement by
-- statement outside a transaction block, as schema tooling does when it
-- replays migrations into a throwaway database.
--
-- lock_timeout bounds the wait for each ACCESS EXCLUSIVE lock. While ALTER
-- TABLE waits, every later reader and writer of that table queues behind it,
-- so an unbounded wait behind one long transaction would stall backups on the
-- running control plane for as long as that transaction stayed open. Two
-- seconds lets a boot wait out ordinary short transactions and refuses to
-- wait on anything longer.
--
-- WHEN A TIMEOUT FIRES. The statement fails (SQLSTATE 55P03 for the lock wait,
-- 57014 for the statement), the transaction rolls back, nothing in this file
-- persists, and schema_migrations gets no row for it. cmd/wpmgr applies
-- migrations before it listens, so the process exits without serving and the
-- instances already serving keep serving. The next boot runs this file again
-- from the start.
--
-- CONVERGENCE AND IDEMPOTENCY
--
-- No earlier version of this change was ever applied anywhere, so there is no
-- converge path to carry. Both statements are ADD COLUMN IF NOT EXISTS, so a
-- second run changes nothing, and a database built from db/schema.sql reaches
-- the same end state.

DO $$
BEGIN
    PERFORM set_config('lock_timeout', '2s', true);
    PERFORM set_config('statement_timeout', '10s', true);
END;
$$;

ALTER TABLE "public"."backup_snapshots"
    ADD COLUMN IF NOT EXISTS "attempt_error" text NOT NULL DEFAULT '';

ALTER TABLE "public"."backup_schedule_runs"
    ADD COLUMN IF NOT EXISTS "attempt_error" text NOT NULL DEFAULT '';
