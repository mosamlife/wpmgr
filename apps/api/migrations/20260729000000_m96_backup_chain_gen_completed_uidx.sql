-- m96: GH #168 CRITICAL fix — close the (chain_id, generation) duplicate-row
-- gap that let a retention-GC reachability computation lose track of a
-- within-retention parent files-list chunk and break an incremental chain.
--
-- Root cause: the CP derives an incremental backup's generation from
-- GetLatestCompletedSnapshot (completed-only). If an incremental attempt at
-- generation N fails or is aborted partway (e.g. the agent crashes, or the
-- run is superseded by ReconcileDuplicateInflightSnapshots), the NEXT retry
-- still sees the last COMPLETED generation as N-1 and reuses generation N —
-- nothing before this migration prevented more than one row from sharing a
-- (chain_id, generation) pair (the existing backup_snapshots_chain_gen_idx is
-- a plain, non-unique index; the only uniqueness guard,
-- backup_snapshots_one_inflight_per_site (m75), covers pending/running rows
-- only). ListChainSnapshots returns every status with no tie-break beyond
-- "ORDER BY generation ASC", so the reachability walk's byGen[generation]
-- map ended up holding whichever same-generation row happened to be LAST in
-- the (unordered-by-tiebreak) scan. When a FAILED duplicate with an empty or
-- partial manifest won that slot, the COMPLETED row's real files-list/part
-- chunk hashes were silently excluded from the retention GC's live set for
-- every retained snapshot — and Phase 5 (now Phase 4 — see gc.go) swept them
-- as "unreachable", permanently breaking the incremental chain.
--
-- The Go-side fix (chainGenWinner in service.go, and the ListCompletedChain-
-- Snapshots SQL variant reachableChunks/gc.go now call exclusively) makes
-- this deterministic and closes the class of bug going forward. This
-- migration adds the DATABASE-level invariant that makes a duplicate
-- COMPLETED row at the same (chain_id, generation) impossible to create in
-- the first place: a partial unique index scoped to status='completed' (a
-- failed/pending/running row is unconstrained, so a legitimate in-flight
-- retry is never blocked — only two COMPLETED rows at the same slot are
-- rejected).
--
-- Pre-dedup (ship-blocker, mirrors m88's identical precedent exactly): a
-- LIVE database that already ran the pre-#168-fix code may already have more
-- than one COMPLETED row for the same (chain_id, generation) — that is
-- precisely the bug this migration protects against, and there is no reaper,
-- so such duplicates would persist indefinitely. CREATE UNIQUE INDEX below
-- would then fail outright on the duplicate data, and since the migration
-- runner wraps each migration in a transaction and aborts boot on error
-- (internal/db/migrate.go), that would hard-block the API from booting on
-- prod and on every self-hoster who already has one of these collisions.
--
-- Terminalize every completed row EXCEPT the lowest-id one in each
-- (chain_id, generation) group as 'failed' before the index is created, so
-- CREATE UNIQUE INDEX always succeeds. "Lowest id" mirrors chainGenWinner's
-- own stable tiebreak exactly (service.go), so the row this migration keeps
-- completed is the SAME row the application-level fix would have picked
-- regardless. A terminalized duplicate's own chunks are NOT deleted here —
-- they simply stop being counted as reachable by the next retention GC pass,
-- which reclaims any that are not ALSO referenced by the surviving row (the
-- normal, already-idempotent sweep behaviour). The UPDATE is naturally
-- idempotent: once no group has more than one completed row, the EXISTS
-- predicate matches nothing and it is a no-op; wrapped in a DO $$ block to
-- keep this migration's style consistent with the rest of the file (mirrors
-- m36/m88's idempotent-migration convention).
DO $$
BEGIN
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
END $$;

-- Plain CREATE UNIQUE INDEX (the migration runner wraps each migration in a
-- tx, so CONCURRENTLY is unavailable — see m85/m88); IF NOT EXISTS makes it
-- re-run safe. Scoped to status='completed' only: pending/running/failed
-- rows are unconstrained, so a legitimate in-flight retry after a failure is
-- never blocked by this index — only two COMPLETED rows at the same slot are
-- rejected, which is exactly the invariant GC/restore reachability depends on.
CREATE UNIQUE INDEX IF NOT EXISTS backup_snapshots_chain_gen_completed_uidx
    ON backup_snapshots (chain_id, generation)
    WHERE status = 'completed';

-- GH #168 P2: a GIN index on backup_manifest_entries.chunk_hashes so the
-- retention GC's ground-truth guard (ChunkStillReferenced — "is this hash
-- still referenced by ANY manifest row for the tenant?", checked for every
-- sweep candidate the computed live-set marks unreachable) can use an index
-- scan (chunk_hashes @> ARRAY[hash]) instead of a sequential scan across the
-- tenant's manifest rows. Deliberately NOT added to backup_file_index: the
-- P2 guard never queries that table (see ChunkStillReferenced's doc for why
-- checking it would be unsound for the legacy per-file incremental model).
CREATE INDEX IF NOT EXISTS backup_manifest_entries_chunk_hashes_gin
    ON backup_manifest_entries USING gin (chunk_hashes);
