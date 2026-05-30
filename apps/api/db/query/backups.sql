-- M4 backup queries. Every statement is tenant-scoped both explicitly
-- (tenant_id in the WHERE/VALUES) and by RLS (the app.tenant_id policy).

-- ---------------------------------------------------------------------------
-- backup_snapshots
-- ---------------------------------------------------------------------------

-- name: CreateBackupSnapshot :one
INSERT INTO backup_snapshots (tenant_id, site_id, created_by, kind, status, age_recipient)
VALUES ($1, $2, $3, $4, 'pending', $5)
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
-- archive set (newest per calendar month).
SELECT id, created_at, archived FROM backup_snapshots
WHERE tenant_id = $1 AND site_id = $2 AND status = 'completed'
ORDER BY created_at DESC;

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
-- Decrements but never below zero. Returns the new refcount + s3_key so the GC
-- job can delete the object from storage when the count reaches zero.
UPDATE backup_chunks
SET refcount = GREATEST(refcount - 1, 0), updated_at = now()
WHERE tenant_id = $1 AND blake3 = $2
RETURNING refcount, s3_key, blake3;

-- name: DeleteOrphanChunk :execrows
-- Deletes a chunk row only if its refcount is zero (the object was already
-- removed from storage by the GC job). Tenant-scoped.
DELETE FROM backup_chunks
WHERE tenant_id = $1 AND blake3 = $2 AND refcount = 0;

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
    run_hour, run_minute, day_of_week, day_of_month, frequency_hours, keep_last
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
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
