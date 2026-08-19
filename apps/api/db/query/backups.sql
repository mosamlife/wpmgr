-- M4 backup queries. Every statement is tenant-scoped both explicitly
-- (tenant_id in the WHERE/VALUES) and by RLS (the app.tenant_id policy).

-- ---------------------------------------------------------------------------
-- backup_snapshots
-- ---------------------------------------------------------------------------

-- name: CreateBackupSnapshot :one
-- destination_id (M7 / ADR-036 P1): NULL routes to the legacy CP-managed
-- bucket; a non-null value is the site_destinations row the caller already
-- resolved (the site's configured default) before creating the snapshot.
INSERT INTO backup_snapshots (tenant_id, site_id, created_by, kind, status, age_recipient, destination_id)
VALUES ($1, $2, $3, $4, 'pending', $5, $6)
RETURNING *;

-- name: GetBackupSnapshot :one
SELECT * FROM backup_snapshots
WHERE id = $1 AND tenant_id = $2;

-- name: ListBackupSnapshotsForSite :many
SELECT * FROM backup_snapshots
WHERE tenant_id = $1 AND site_id = $2
ORDER BY created_at DESC
LIMIT $3 OFFSET $4;

-- name: MarkBackupSnapshotRunning :execrows
-- Claim precondition (GH #458): 'pending' is the only status a claim may
-- transition out of. Without it the UPDATE was blind, so a duplicate/retried
-- worker job, or a job whose snapshot was cancelled or watchdog-failed while
-- it sat in the queue, dragged a terminal row back to 'running' and cleared
-- the run's real outcome. Rows-affected is the contract: 1 = this caller won
-- the claim and owns the run; 0 = the row was not 'pending' (already claimed,
-- already terminal, or gone), and the caller must NOT proceed as the owner and
-- must NOT publish the 'started' SSE event or the schedule-run reconciliation
-- for it. :execrows (not :exec) because a precondition whose failure is
-- invisible is worse than no precondition -- same reasoning as
-- FailStalledBackupSnapshot below.
UPDATE backup_snapshots
SET status = 'running', started_at = now(), updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND status = 'pending';

-- name: CompleteBackupSnapshot :execrows
-- Terminal-transition precondition (GH #458): only a 'pending' or 'running'
-- snapshot may complete. 'pending' stays allowed because the files-only and
-- carry-forward completion paths can land without an intervening MarkRunning.
-- What the guard blocks is resurrection: a late agent manifest submit arriving
-- after the operator cancelled (CancelSnapshot stamps 'failed') or after the
-- watchdog hard-failed the run must not flip the row to 'completed' and must
-- not overwrite the recorded error and finished_at. Rows-affected is the
-- contract: 1 = a real transition; 0 = already terminal (completed/failed) or
-- gone, which the caller must surface as a rejected submit -- the previous
-- blind UPDATE reported success unconditionally.
UPDATE backup_snapshots
SET status = 'completed',
    total_size = $3,
    chunk_count = $4,
    finished_at = now(),
    updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND status IN ('pending', 'running');

-- name: FailBackupSnapshot :execrows
-- Terminal-transition precondition (GH #458). 'pending' is in the guard
-- deliberately: CancelSnapshot ("only a running or pending backup can be
-- cancelled") fails a still-'pending' snapshot through this query, so
-- narrowing to status='running' would silently stop operator cancel of a
-- queued backup. That is the watchdog's guard, not this one -- see
-- FailStalledBackupSnapshot below. What this guard blocks is the overwrite of
-- an ALREADY-terminal row: a worker error landing after the operator cancelled
-- must not replace cancelByOperatorMsg with a generic message, and a fail must
-- never rewrite a completed run's status or finished_at. Rows-affected is the
-- contract: 1 = a real transition; 0 = already terminal, or gone -- do not
-- publish the 'failed' SSE event, the failure email, or the schedule-run
-- reconciliation for a row that had already moved on.
UPDATE backup_snapshots
SET status = 'failed',
    error = $3,
    finished_at = now(),
    updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND status IN ('pending', 'running');

-- name: FailStalledBackupSnapshot :execrows
-- TOCTOU-safe hard-fail for the two-tier progress watchdog (GH #279 must-fix).
-- Identical to FailBackupSnapshot except that the guard is NARROWER:
-- status='running' here, status IN ('pending','running') there (GH #458).
-- ListStalledRunningSnapshots commits, then each hard row is failed in its own
-- separate transaction, so between the two a row may have completed, been
-- operator-cancelled, been agent-failed, or resumed. The watchdog only ever
-- hard-fails a run it observed RUNNING and must never touch a queued 'pending'
-- row -- which FailBackupSnapshot may, because operator cancel of a queued
-- backup goes through it (CancelSnapshot). The two guards therefore stay
-- distinct, and this query remains the watchdog hard-fail branch's ONLY write
-- path. Rows-affected (0 or 1) tells the caller
-- whether a real transition happened, so it can gate the 'failed' SSE publish
-- and the failure notification on an actual state change rather than firing
-- them for a row that already moved on.
UPDATE backup_snapshots
SET status = 'failed',
    error = $3,
    finished_at = now(),
    updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND status = 'running';

-- name: UpdateBackupSnapshotProgress :one
-- M5.6 / ADR-032: agent runner posts a JSONB progress payload at every phpbu
-- stage transition + per-chunk during the custom PresignedS3 Sync. We always
-- replace (no append/history) — the latest phase is what the UI renders, and
-- the watchdog scans by progress_updated_at. Tenant-scoped via RLS; the agent
-- handler injects the tenant from the verified Ed25519 identity, never from
-- the body.
UPDATE backup_snapshots
SET progress = $3,
    progress_updated_at = now(),
    updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: ListStalledRunningSnapshots :many
-- Watchdog feeder (two-tier, GH #279): a running snapshot whose latest
-- progress (or start time, if no progress was ever posted) is older than the
-- SOFT threshold. `hard` is computed the same way against the HARD threshold
-- so the caller can tell a "stamp stalled_at, keep running" row from a
-- "fail it now" row -- both compare against the DB clock so there is no
-- CP/DB clock-drift risk. The index backup_snapshots_running_progress_idx
-- makes the predicate selective. Cross-tenant select via the GC RLS policy
-- (app.agent='on').
SELECT id, tenant_id, site_id, created_at, started_at, progress_updated_at, stalled_at,
    (
      (progress_updated_at IS NOT NULL AND progress_updated_at < now() - (@hard_interval::interval))
      OR (progress_updated_at IS NULL AND started_at IS NOT NULL AND started_at < now() - (@hard_interval::interval))
    ) AS hard
FROM backup_snapshots
WHERE status = 'running'
  AND (
    (progress_updated_at IS NOT NULL AND progress_updated_at < now() - (@soft_interval::interval))
    OR (progress_updated_at IS NULL AND started_at IS NOT NULL AND started_at < now() - (@soft_interval::interval))
  );

-- name: MarkBackupSnapshotStalled :one
-- Soft-stall stamp (GH #279): idempotent via the status='running' AND
-- stalled_at IS NULL guard -- a row already stalled, or no longer running,
-- matches zero rows and the caller treats that as a no-op (not an error).
UPDATE backup_snapshots
SET stalled_at = now(), updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND status = 'running' AND stalled_at IS NULL
RETURNING id;

-- name: ClearBackupSnapshotStalled :execrows
-- Proof-of-life clear (GH #279): a presign, manifest submit, or progress POST
-- that lands after a soft stall clears it so the watchdog does not later
-- hard-fail a run that was merely slow. The status='running' predicate is
-- the whole anti-resurrection guarantee -- a snapshot the watchdog already
-- hard-failed, or the operator cancelled, is never matched here (both moved
-- status away from 'running' first), so this can never revive a genuinely
-- terminal snapshot.
UPDATE backup_snapshots
SET stalled_at = NULL, updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND status = 'running' AND stalled_at IS NOT NULL;

-- name: DeleteBackupSnapshot :execrows
DELETE FROM backup_snapshots
WHERE id = $1 AND tenant_id = $2;

-- name: ListExpiredBackupSnapshots :many
-- Completed snapshots older than the cutoff that are NOT archive-retained, in a
-- single tenant scope. The GC job decrements chunk refcounts for each then
-- deletes the snapshot (manifest entries cascade).
SELECT * FROM backup_snapshots
WHERE tenant_id = $1
  AND status = 'completed'
  AND archived = false
  AND created_at < $2
ORDER BY created_at ASC;

-- name: SetBackupSnapshotArchived :exec
UPDATE backup_snapshots
SET archived = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2;

-- name: ListCompletedSnapshotsForSite :many
-- Completed snapshots for a site, newest first, used to compute the retention
-- archive set (newest per calendar month). ADR-050 widened the projection to
-- carry the chain columns so the mark-and-sweep GC can do chain-aware
-- expansion (pin a carry-forward chunk's old origin generation under a live
-- tip) without a second round-trip.
SELECT id, created_at, archived, chain_id, generation, is_incremental
FROM backup_snapshots
WHERE tenant_id = $1 AND site_id = $2 AND status = 'completed'
ORDER BY created_at DESC;

-- name: ListRecentCompletedSnapshotsForSites :many
-- Returns recently completed snapshots across a set of sites, ordered newest
-- first. Used by the portal /summary recent_work feed. The site_ids param is
-- always p.AllowedSiteIDs (RLS double-gate via app.site_scope on
-- backup_snapshots). `, id` tiebreaker follows the project ORDER BY convention.
SELECT site_id, kind, total_size, finished_at
FROM backup_snapshots
WHERE tenant_id  = @tenant_id
  AND site_id    = ANY(@site_ids::uuid[])
  AND status     = 'completed'
  AND finished_at >= @since
ORDER BY finished_at DESC, id DESC
LIMIT @row_limit;

-- name: ListInFlightSnapshotFloor :one
-- ADR-050 mark-and-sweep grace floor: the oldest created_at among snapshots
-- that are still pending or running for the tenant. A chunk created before this
-- floor cannot be re-referenced by an in-flight backup (its manifest/file_index
-- rows are not yet visible at mark time), so the sweep uses
-- min(markStart, inflightFloor) as the deletion horizon. Returns NULL when no
-- in-flight snapshot exists (the caller then uses markStart alone).
SELECT min(created_at)::timestamptz AS floor
FROM backup_snapshots
WHERE tenant_id = $1 AND status IN ('pending', 'running');

-- name: ListTenantsForBackupGC :many
-- Distinct tenant IDs the periodic retention GC should visit. Runs cross-tenant
-- under the app.agent GUC (the backup_snapshots_gc and backup_chunks_agent
-- SELECT policies); the prune then runs per tenant.
--
-- GH #402: this used to be "tenants with a completed snapshot" alone, which had
-- a second-order leak in it. Deleting the site that held a tenant's LAST
-- completed snapshot dropped that tenant off this roster permanently, so its
-- chunk bytes were never swept again. backup_chunks has no FK to sites and so
-- survives the cascade intact; unioning it in is what lets the sweep reach a
-- tenant whose sites are all gone and reclaim those bytes.
--
-- THE TENANT ROW ITSELF STILL HAS TO EXIST FOR THIS TO HELP, AND THAT GAP IS
-- NOT CLOSED HERE. backup_chunks.tenant_id is ON DELETE CASCADE (m4), so the
-- chunk inventory is destroyed with the tenant row, and admin_delete_empty_tenant
-- (org delete Lane A, and the superadmin orphan cleanup) hard-deletes a tenant
-- while freeing no object storage at all. Delete an organisation's last site and
-- then the now-empty organisation, and this roster loses the tenant along with
-- every row naming its chunk objects: GH #402 at tenant level, for chunks, still
-- open. site_object_reclaim survives that delete because it has no foreign key
-- to either parent, so the deleted site's MANIFESTS are still reclaimed; the
-- chunks are not. Tracked as GH #408, deliberately out of scope for this change.
--
-- This widens ENUMERATION only, never the delete decision. A newly-visited
-- tenant is one with chunk rows and no completed snapshot, and every existing
-- guard still applies to it: the in-flight floor pins effectiveFloor to a
-- running snapshot's created_at so newer chunks are kept, the dedup oracle
-- bumps last_referenced_at on any old chunk a running backup re-references, the
-- ground-truth manifest-entries guard keeps anything a not-yet-completed
-- snapshot already references, and Phase 3 is fail-closed so an empty live set
-- caused by an ERROR can never reach the sweep at all.
SELECT DISTINCT tenant_id FROM backup_snapshots
WHERE status = 'completed'
UNION
SELECT DISTINCT tenant_id FROM backup_chunks;

-- name: ListBackupSiteIDsForTenant :many
-- Distinct site IDs that have at least one snapshot in this tenant (GC iterates
-- per site to apply the per-site monthly-archive rule).
SELECT DISTINCT site_id FROM backup_snapshots
WHERE tenant_id = $1;

-- ---------------------------------------------------------------------------
-- backup_manifest_entries
-- ---------------------------------------------------------------------------

-- name: CreateManifestEntry :one
INSERT INTO backup_manifest_entries (
    snapshot_id, tenant_id, path, entry_kind, table_name, chunk_hashes, size, mode
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ListManifestEntries :many
SELECT * FROM backup_manifest_entries
WHERE snapshot_id = $1 AND tenant_id = $2
ORDER BY path ASC;

-- ---------------------------------------------------------------------------
-- backup_chunks  (content-addressed dedup + refcount GC)
-- ---------------------------------------------------------------------------

-- name: GetBackupChunk :one
SELECT * FROM backup_chunks
WHERE tenant_id = $1 AND blake3 = $2;

-- name: ListBackupChunksByHashes :many
-- Returns the tenant's already-stored chunks among the given hashes (dedup: the
-- agent only uploads hashes NOT returned here).
SELECT * FROM backup_chunks
WHERE tenant_id = $1 AND blake3 = ANY($2::text[]);

-- name: UpsertBackupChunk :one
-- Records a chunk's storage location idempotently. On conflict (the chunk
-- already exists) it leaves size/s3_key as-is (content-addressed: identical
-- hash ⇒ identical bytes) and returns the existing row. refcount is managed
-- separately by IncrementChunkRefcount.
INSERT INTO backup_chunks (tenant_id, blake3, s3_key, size, refcount)
VALUES ($1, $2, $3, $4, 0)
ON CONFLICT (tenant_id, blake3)
DO UPDATE SET updated_at = now()
RETURNING *;

-- name: IncrementChunkRefcount :one
UPDATE backup_chunks
SET refcount = refcount + 1, updated_at = now()
WHERE tenant_id = $1 AND blake3 = $2
RETURNING *;

-- name: DecrementChunkRefcount :one
-- DEPRECATED (ADR-050): refcount is observability-only post-mark-and-sweep and
-- is NEVER consulted for a delete. Retained only so the generated querier keeps
-- compiling; the GC delete path no longer calls it.
UPDATE backup_chunks
SET refcount = GREATEST(refcount - 1, 0), updated_at = now()
WHERE tenant_id = $1 AND blake3 = $2
RETURNING refcount, s3_key, blake3;

-- name: DeleteOrphanChunk :execrows
-- DEPRECATED (ADR-050): the refcount==0 gate is unsound for incremental dedup
-- (refcount counts ORIGIN refs, not live refs). The mark-and-sweep pass deletes
-- chunks by reachability + grace floor instead (see ListChunksForSweep /
-- DeleteSweptChunk below, implemented as raw SQL in repo.go). Retained only so
-- the generated querier keeps compiling.
DELETE FROM backup_chunks
WHERE tenant_id = $1 AND blake3 = $2 AND refcount = 0;

-- ADR-050 MARK-AND-SWEEP retention GC. These are implemented as raw tx.Query /
-- tx.Exec in repo.go (matching the m44/m46 raw-SQL precedent) rather than
-- regenerating sqlc; the canonical statements are documented here.
--
-- m47 data-loss fix: a chunk's deletion boundary is GREATEST(created_at,
-- last_referenced_at), not created_at alone. The dedup oracle (the
-- ExistingChunkHashes path PresignChunks relies on) bumps last_referenced_at =
-- now() for every chunk it reports as already-stored, so an OLD chunk an
-- in-flight backup re-references via tenant-global dedup is protected even
-- though its created_at is ancient and its last completed referrer expired this
-- run.
--
-- TouchExistingChunks (dedup oracle — read + touch in ONE statement):
--   UPDATE backup_chunks
--      SET last_referenced_at = now(), updated_at = now()
--    WHERE tenant_id = $1 AND blake3 = ANY($2::text[])
--   RETURNING id, tenant_id, blake3, s3_key, size, refcount,
--             created_at, updated_at;
--
-- ListChunksForSweep (keyset-paged by (created_at, blake3)):
--   SELECT blake3, s3_key, created_at, last_referenced_at FROM backup_chunks
--    WHERE tenant_id = $1 AND (created_at, blake3) > ($2, $3)
--    ORDER BY created_at ASC, blake3 ASC LIMIT $4;
--
-- DeleteSweptChunk (object deleted first; row only when STILL below the floor by
-- the GREATEST boundary, so a chunk re-referenced after the read survives):
--   DELETE FROM backup_chunks
--    WHERE tenant_id = $1 AND blake3 = $2
--      AND GREATEST(created_at, last_referenced_at) < $3;
--
-- The per-tenant sweep takes a SESSION-level advisory lock (released via
-- pg_advisory_unlock) spanning SHORT per-page transactions, so no pooled
-- connection is pinned across object-store I/O (avoiding Cloud SQL's
-- idle_in_transaction_session_timeout):
--   SELECT pg_try_advisory_lock(hashtext('backup_gc'), hashtext($1));   -- acquire
--   SELECT pg_advisory_unlock(hashtext('backup_gc'), hashtext($1));     -- release
-- so two GC passes never sweep the same tenant concurrently.

-- ---------------------------------------------------------------------------
-- backup_schedules
-- ---------------------------------------------------------------------------

-- name: GetBackupScheduleForSite :one
SELECT * FROM backup_schedules
WHERE tenant_id = $1 AND site_id = $2;

-- name: UpsertBackupSchedule :one
-- Inserts or updates a backup schedule. next_run_at is intentionally NOT
-- included in the ON CONFLICT DO UPDATE set: the service decides when to
-- recompute it (only when timing fields actually change). This prevents a
-- non-timing edit (e.g. retention_days change) from resetting the next run.
INSERT INTO backup_schedules (
    tenant_id, site_id, cadence, kind, enabled, retention_days,
    monthly_archive_keep, next_run_at,
    run_hour, run_minute, day_of_week, day_of_month, frequency_hours, keep_last,
    incremental_enabled, base_window_days
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
ON CONFLICT (site_id)
DO UPDATE SET cadence              = EXCLUDED.cadence,
              kind                 = EXCLUDED.kind,
              enabled              = EXCLUDED.enabled,
              retention_days       = EXCLUDED.retention_days,
              monthly_archive_keep = EXCLUDED.monthly_archive_keep,
              run_hour             = EXCLUDED.run_hour,
              run_minute           = EXCLUDED.run_minute,
              day_of_week          = EXCLUDED.day_of_week,
              day_of_month         = EXCLUDED.day_of_month,
              frequency_hours      = EXCLUDED.frequency_hours,
              keep_last            = EXCLUDED.keep_last,
              incremental_enabled  = EXCLUDED.incremental_enabled,
              base_window_days     = EXCLUDED.base_window_days,
              updated_at           = now()
RETURNING *;

-- name: ListDueBackupSchedules :many
-- Cross-tenant enumeration of enabled schedules whose next_run_at has passed,
-- for the periodic scheduler. Runs under the app.agent GUC (scheduler policy).
SELECT * FROM backup_schedules
WHERE enabled = true AND next_run_at <= $1
ORDER BY next_run_at ASC
LIMIT $2;

-- name: AdvanceBackupScheduleRun :one
-- Records that a scheduled backup was enqueued and advances next_run_at. The
-- scheduler resolves the tenant from the due-row first, then advances within
-- that tenant's scope (the per-tenant isolation policy permits the UPDATE).
UPDATE backup_schedules
SET last_run_at = now(), next_run_at = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: GetBackupSiteInfo :one
-- Returns the site fields the backup scheduler needs: enrollment status,
-- agent URL, age recipient for encryption, and the WP timezone columns
-- added in M17 (wp_timezone IANA name + wp_gmt_offset fallback).
-- Runs tenant-scoped (the caller sets app.tenant_id before this query).
SELECT id, tenant_id, url, enrolled_at, age_recipient,
       wp_timezone, wp_gmt_offset
FROM sites
WHERE id = $1 AND tenant_id = $2;

-- ---------------------------------------------------------------------------
-- Fleet backup endpoints
-- ---------------------------------------------------------------------------

-- name: FleetListSnapshots :many
-- Returns a filtered, paginated list of backup snapshots across a set of
-- sites for the fleet dashboard. site_ids is always filtered to the
-- principal's AllowedSiteIDs (for site-scoped principals) or all tenant
-- sites (for org-scoped principals) so RLS plus the explicit site_ids
-- filter is the double-gate. Offset pagination with ORDER BY created_at
-- DESC, id DESC (project convention).
SELECT bs.id, bs.tenant_id, bs.site_id, bs.kind, bs.status,
       bs.total_size, bs.chunk_count, bs.created_by,
       bs.age_recipient, bs.archived, bs.error,
       bs.started_at, bs.finished_at,
       bs.progress, bs.progress_updated_at,
       bs.is_incremental, bs.generation,
       bs.chain_id, bs.parent_snapshot_id, bs.base_snapshot_id,
       bs.locked, bs.created_at, bs.updated_at
FROM backup_snapshots bs
WHERE bs.tenant_id = @tenant_id
  AND (@site_ids_filter::bool = false OR bs.site_id = ANY(@site_ids::uuid[]))
  AND (@status_filter::bool = false OR bs.status = @status_val)
  AND (@after_filter::bool  = false OR bs.created_at >= @created_after)
  AND (@before_filter::bool = false OR bs.created_at <= @created_before)
ORDER BY bs.created_at DESC, bs.id DESC
LIMIT @row_limit OFFSET @row_offset;

-- name: FleetListSnapshotsCount :one
-- Returns the total row count for the fleet snapshot list (for next_offset
-- calculation). Uses the same predicates as FleetListSnapshots.
SELECT COUNT(*) AS total
FROM backup_snapshots bs
WHERE bs.tenant_id = @tenant_id
  AND (@site_ids_filter::bool = false OR bs.site_id = ANY(@site_ids::uuid[]))
  AND (@status_filter::bool = false OR bs.status = @status_val)
  AND (@after_filter::bool  = false OR bs.created_at >= @created_after)
  AND (@before_filter::bool = false OR bs.created_at <= @created_before);

-- name: FleetBackupHealth :many
-- Returns one row per site with the data needed for the backup health card:
-- latest completed snapshot time, latest failed snapshot time, in-flight
-- count, and the schedule cadence / next_run_at when a schedule exists.
-- Site-scoped principals pass @site_ids with their AllowedSiteIDs;
-- org-scoped principals pass all tenant site IDs so the @site_ids_filter
-- gate applies equally without a separate query path.
SELECT
    s.id            AS site_id,
    s.name          AS site_name,
    s.url           AS site_url,
    -- last completed
    (SELECT MAX(finished_at) FROM backup_snapshots x
     WHERE x.tenant_id = s.tenant_id AND x.site_id = s.id AND x.status = 'completed')
                    AS last_completed_at,
    -- last failed (not cancelled-by-operator)
    (SELECT MAX(finished_at) FROM backup_snapshots x
     WHERE x.tenant_id = s.tenant_id AND x.site_id = s.id AND x.status = 'failed')
                    AS last_failed_at,
    -- latest completed size (COALESCE: a site with zero completed snapshots
    -- must not NULL-scan into the non-nullable int64 LatestSizeBytes column)
    COALESCE(
        (SELECT total_size FROM backup_snapshots x
         WHERE x.tenant_id = s.tenant_id AND x.site_id = s.id AND x.status = 'completed'
         ORDER BY finished_at DESC, x.id DESC LIMIT 1),
        0)
                    AS latest_size_bytes,
    -- in-flight count (pending or running)
    (SELECT COUNT(*) FROM backup_snapshots x
     WHERE x.tenant_id = s.tenant_id AND x.site_id = s.id
       AND x.status IN ('pending','running'))
                    AS in_flight_count,
    -- schedule info (NULL when no schedule exists)
    bs.cadence      AS schedule_cadence,
    bs.next_run_at  AS next_run_at
FROM sites s
LEFT JOIN backup_schedules bs
       ON bs.site_id = s.id AND bs.tenant_id = s.tenant_id AND bs.enabled = true
WHERE s.tenant_id = @tenant_id
  AND s.id = ANY(@site_ids::uuid[])
ORDER BY s.name ASC;
