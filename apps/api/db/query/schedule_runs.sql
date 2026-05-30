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
UPDATE backup_schedule_runs
SET status      = $3,
    error       = $4,
    started_at  = CASE WHEN $5::boolean THEN now() ELSE started_at  END,
    finished_at = CASE WHEN $6::boolean THEN now() ELSE finished_at END,
    updated_at  = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: SetScheduleRunStatusBySnapshot :one
-- Reconciliation path: when the linked snapshot reaches a terminal status,
-- update the run row to match. Keyed on snapshot_id so the snapshot finalize
-- path does not need to carry the run id. Runs tenant-scoped.
UPDATE backup_schedule_runs
SET status      = $3,
    error       = $4,
    started_at  = CASE WHEN $5::boolean THEN now() ELSE started_at  END,
    finished_at = CASE WHEN $6::boolean THEN now() ELSE finished_at END,
    updated_at  = now()
WHERE snapshot_id = $1 AND tenant_id = $2
RETURNING *;

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
-- Runs that have not yet fired: status 'scheduled' or 'queued', scheduled_for
-- in the future. Used for the UI upcoming preview (typically 1–3 rows).
SELECT * FROM backup_schedule_runs
WHERE tenant_id = $1 AND site_id = $2
  AND status IN ('scheduled', 'queued')
  AND scheduled_for > now()
ORDER BY scheduled_for ASC
LIMIT $3;

-- name: ListPastScheduleRuns :many
-- Terminal runs (completed/failed/skipped/canceled) for a site, newest first.
SELECT * FROM backup_schedule_runs
WHERE tenant_id = $1 AND site_id = $2
  AND status IN ('completed', 'failed', 'skipped', 'canceled')
ORDER BY scheduled_for DESC
LIMIT $3 OFFSET $4;
