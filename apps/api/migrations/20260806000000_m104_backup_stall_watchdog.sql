-- m104 — GH #279: two-tier backup progress watchdog. A "soft" stall (the
-- agent has gone quiet past the soft threshold) no longer immediately fails
-- the snapshot -- it stamps stalled_at and keeps status='running' so a
-- slow-but-alive run (e.g. a large ZipArchive finalize with no intermediate
-- progress event) can still complete. Only a "hard" stall, past a much
-- longer deadline, fails the run, with a distinct stall-timeout error
-- message. Proof of life (a presign, manifest submit, or progress POST)
-- clears stalled_at and the run resumes.
--
-- stalled_at is nullable with no default and no backfill: every existing
-- running snapshot simply reads as healthy (NULL) until the next watchdog
-- tick evaluates it. Non-blocking (no default, no table rewrite) and
-- idempotent (ADD COLUMN IF NOT EXISTS mirrors m103/m101/m92/m93).

ALTER TABLE "public"."backup_snapshots"
    ADD COLUMN IF NOT EXISTS "stalled_at" timestamptz NULL;
