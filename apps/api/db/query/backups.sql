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

-- name: MarkBackupSnapshotRunning :one
UPDATE backup_snapshots
SET status = 'running', started_at = now(), updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: CompleteBackupSnapshot :one
UPDATE backup_snapshots
SET status = 'completed',
    total_size = $3,
    chunk_count = $4,
    finished_at = now(),
    updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: FailBackupSnapshot :one
UPDATE backup_snapshots
SET status = 'failed',
    error = $3,
    finished_at = now(),
    updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

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
-- Watchdog feeder: a running snapshot whose latest progress is older than the
-- stall threshold (or whose runner never reported any progress despite being
-- running for longer than the threshold). The new index
-- backup_snapshots_running_progress_idx makes the predicate selective.
-- Cross-tenant select via the GC RLS policy (app.agent='on').
SELECT id, tenant_id, site_id, created_at, started_at, progress_updated_at
FROM backup_snapshots
WHERE status = 'running'
  AND (
    (progress_updated_at IS NOT NULL AND progress_updated_at < now() - ($1::interval))
    OR (progress_updated_at IS NULL AND started_at IS NOT NULL AND started_at < now() - ($1::interval))
  );

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

-- name: ListTenantsWithCompletedSnapshots :many
-- Distinct tenant IDs that have at least one completed snapshot, for the
-- periodic retention GC. Runs cross-tenant under the app.agent GUC (the
-- backup_snapshots_gc SELECT policy); the prune then runs per tenant.
SELECT DISTINCT tenant_id FROM backup_snapshots
WHERE status = 'completed';

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
