-- m143 - deduplicate completed backup_snapshots per (chain_id, generation)
-- and create backup_snapshots_chain_gen_completed_uidx, ahead of m96.
--
-- WHAT IT GUARANTEES. When backup_snapshots_chain_gen_completed_uidx does not
-- exist, this file marks every completed snapshot except the lowest id per
-- (chain_id, generation) as 'failed', with m96's own UPDATE unchanged, then
-- creates m96's partial unique index in the same transaction, so no duplicate
-- can be inserted between the two. m96 then finds nothing to deduplicate and
-- the index already present; its GIN index on backup_manifest_entries is left
-- to m96.
--
-- WHY. backup_snapshots is FORCE ROW LEVEL SECURITY, and the production
-- migrator is a NOSUPERUSER NOBYPASSRLS table owner with no app.* setting in
-- scope. For that role m96's UPDATE reads no rows, so its CREATE UNIQUE INDEX
-- fails on any duplicate already present and the boot aborts. This file lifts
-- FORCE for the length of the deduplication, so the owner reads every row, and
-- restores it before it returns. row_security is off while FORCE is lifted: a
-- statement a policy would still filter raises an error instead of touching
-- fewer rows.
--
-- ORDINAL. 20260728120000 sorts after m95 (20260728000000) and before m96
-- (20260729000000), so it applies first. The label m143 is a name; the ordinal
-- is the order.
--
-- LATE RUN. The runner applies every unapplied version, including one that
-- sorts before versions already applied, so this file also runs on every
-- database already past m96. There the index exists, the probe returns before
-- any statement takes a lock, and nothing changes. An existing unique index is
-- itself proof that no duplicate completed snapshot exists, so nothing is left
-- to converge.
--
-- END STATE IS m96's. backup_snapshots keeps ENABLE and FORCE ROW LEVEL
-- SECURITY and every policy; the check at the end refuses to commit otherwise.
-- db/schema.sql already describes this end state and is not changed.

DO $$
BEGIN
    IF to_regclass('public.backup_snapshots_chain_gen_completed_uidx') IS NOT NULL THEN
        RETURN;
    END IF;

    PERFORM set_config('lock_timeout', '5s', true);

    ALTER TABLE "public"."backup_snapshots" NO FORCE ROW LEVEL SECURITY;

    PERFORM set_config('row_security', 'off', true);

    UPDATE backup_snapshots losers
    SET status      = 'failed',
        error       = 'superseded duplicate completed snapshot at the same chain generation (m96 dedup)',
        finished_at = COALESCE(finished_at, now()),
        updated_at  = now()
    WHERE losers.status = 'completed'
      AND losers.chain_id IS NOT NULL
      AND EXISTS (
          SELECT 1 FROM backup_snapshots winners
           WHERE winners.chain_id   = losers.chain_id
             AND winners.generation = losers.generation
             AND winners.status     = 'completed'
             AND winners.id         < losers.id
      );

    CREATE UNIQUE INDEX IF NOT EXISTS backup_snapshots_chain_gen_completed_uidx
        ON backup_snapshots (chain_id, generation)
        WHERE status = 'completed';

    PERFORM set_config('row_security', 'on', true);

    ALTER TABLE "public"."backup_snapshots" FORCE ROW LEVEL SECURITY;

    IF NOT EXISTS (
        SELECT 1 FROM pg_class
        WHERE oid = 'public.backup_snapshots'::regclass
          AND relrowsecurity
          AND relforcerowsecurity
    ) THEN
        RAISE EXCEPTION 'm143: backup_snapshots must end with ENABLE and FORCE ROW LEVEL SECURITY';
    END IF;
END;
$$;
