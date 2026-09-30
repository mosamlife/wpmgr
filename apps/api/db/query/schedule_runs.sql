-- M17 backup_schedule_runs queries. Mirrors the restore_runs query style.
-- Every tenant-scoped method is called with app.tenant_id already set.
-- The scheduler's cross-tenant materializer writes run under the agent context
-- (app.agent='on'), like ListDueBackupSchedules.

-- ---------------------------------------------------------------------------
-- backup_schedule_runs
-- ---------------------------------------------------------------------------

-- name: UpsertScheduleRun :one
-- Pre-inserts the next scheduled run row idempotently. On conflict
-- (schedule_id, scheduled_for) — e.g. CP restart — updates status only when
-- the existing row is still 'scheduled' so a queued/running row is untouched.
INSERT INTO backup_schedule_runs (
    tenant_id, site_id, schedule_id, scheduled_for, status, kind, triggered_by
)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (schedule_id, scheduled_for)
DO UPDATE SET
    status       = CASE
                       WHEN backup_schedule_runs.status = 'scheduled'
                       THEN EXCLUDED.status
                       ELSE backup_schedule_runs.status
                   END,
    updated_at   = now()
RETURNING *;

-- name: SetScheduleRunSnapshot :one
-- Links the pending snapshot_id to a run and advances its status to 'queued'.
UPDATE backup_schedule_runs
SET snapshot_id = $3,
    status      = 'queued',
    updated_at  = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: SetScheduleRunStatusByID :one
-- Advances a run to a terminal or intermediate status by its primary key.
-- started_at and finished_at are set conditionally so they are only written
-- once (the scheduler calls this for running→completed/failed transitions).
-- attempt_error (GH #791) is cleared when the run completes; a completed run
-- never carries an outstanding attempt error. Any other status leaves it.
UPDATE backup_schedule_runs
SET status        = $3,
    error         = $4,
    attempt_error = CASE WHEN $3 = 'completed' THEN '' ELSE attempt_error END,
    started_at  = CASE WHEN $5::boolean THEN now() ELSE started_at  END,
    finished_at = CASE WHEN $6::boolean THEN now() ELSE finished_at END,
    updated_at  = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: SetScheduleRunStatusBySnapshot :one
-- Reconciliation path: when the linked snapshot reaches a terminal status,
-- update the run row to match. Keyed on snapshot_id so the snapshot finalize
-- path does not need to carry the run id. Runs tenant-scoped.
-- attempt_error (GH #791) is cleared when the run completes; a completed run
-- never carries an outstanding attempt error. Any other status leaves it.
UPDATE backup_schedule_runs
SET status        = $3,
    error         = $4,
    attempt_error = CASE WHEN $3 = 'completed' THEN '' ELSE attempt_error END,
    started_at  = CASE WHEN $5::boolean THEN now() ELSE started_at  END,
    finished_at = CASE WHEN $6::boolean THEN now() ELSE finished_at END,
    updated_at  = now()
WHERE snapshot_id = $1 AND tenant_id = $2
RETURNING *;

-- name: SetScheduleRunAttemptErrorBySnapshot :execrows
-- GH #791: mirrors SetBackupSnapshotAttemptError onto the run linked to the
-- snapshot. The status='running' guard is the contract: it never touches a
-- queued, completed, failed, skipped or canceled run, and unlike
-- SetScheduleRunStatusBySnapshot it never changes status, so it cannot drag
-- a finished run back to running. Rows-affected: 1 = recorded; 0 = no running
-- run is linked to the snapshot (a manual backup has none). The value is
-- capped at 1024 characters here as well as by the caller.
UPDATE backup_schedule_runs
SET attempt_error = left(sqlc.arg(attempt_error)::text, 1024),
    updated_at    = now()
WHERE snapshot_id = sqlc.arg(snapshot_id) AND tenant_id = sqlc.arg(tenant_id)
  AND status = 'running';

-- name: ClearScheduleRunAttemptErrorBySnapshot :execrows
-- GH #791: the run-side half of the proof-of-life clear
-- (ClearBackupSnapshotStalled clears the snapshot side). Matches only a
-- running run with an outstanding attempt error. Rows-affected: 1 = cleared;
-- 0 = nothing to clear.
UPDATE backup_schedule_runs
SET attempt_error = '',
    updated_at    = now()
WHERE snapshot_id = sqlc.arg(snapshot_id) AND tenant_id = sqlc.arg(tenant_id)
  AND status = 'running' AND attempt_error <> '';

-- name: GetScheduleRun :one
SELECT * FROM backup_schedule_runs
WHERE id = $1 AND tenant_id = $2;

-- name: ListScheduleRunsBySite :many
-- All runs for a site (upcoming + past), newest scheduled_for first.
SELECT * FROM backup_schedule_runs
WHERE tenant_id = $1 AND site_id = $2
ORDER BY scheduled_for DESC
LIMIT $3 OFFSET $4;

-- name: ListUpcomingScheduleRuns :many
-- Non-terminal runs for the UI upcoming panel. 'running' rows are always
-- included (the backup is executing now, regardless of scheduled_for);
-- 'scheduled'/'queued' rows are included only when scheduled_for is in the
-- future (they represent fires that have not yet dispatched).
SELECT * FROM backup_schedule_runs
WHERE tenant_id = $1 AND site_id = $2
  AND (
      (status IN ('scheduled', 'queued') AND scheduled_for > now())
      OR status = 'running'
  )
ORDER BY scheduled_for ASC
LIMIT $3;

-- name: ListPastScheduleRuns :many
-- Terminal runs (completed/failed/skipped/canceled) for a site, newest first.
SELECT * FROM backup_schedule_runs
WHERE tenant_id = $1 AND site_id = $2
  AND status IN ('completed', 'failed', 'skipped', 'canceled')
ORDER BY scheduled_for DESC
LIMIT $3 OFFSET $4;
