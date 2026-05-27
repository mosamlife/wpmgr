-- M3 bulk-update queries. Every statement is tenant-scoped both explicitly
-- (tenant_id in the WHERE/VALUES) and by RLS (the app.tenant_id policy).

-- name: CreateUpdateRun :one
-- tenant_id is supplied explicitly for defense-in-depth; RLS additionally
-- enforces it matches the current app.tenant_id setting.
INSERT INTO update_runs (tenant_id, created_by, status, dry_run, scheduled_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: CreateUpdateTask :one
INSERT INTO update_tasks (
    run_id, tenant_id, site_id, target_type, target_slug, desired_version,
    from_version, status
)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending')
RETURNING *;

-- name: GetUpdateRun :one
SELECT * FROM update_runs
WHERE id = $1 AND tenant_id = $2;

-- name: ListUpdateRuns :many
SELECT * FROM update_runs
WHERE tenant_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: ListUpdateTasksForRun :many
SELECT * FROM update_tasks
WHERE run_id = $1 AND tenant_id = $2
ORDER BY created_at ASC;

-- name: GetUpdateTask :one
SELECT * FROM update_tasks
WHERE id = $1 AND tenant_id = $2;

-- name: MarkUpdateTaskRunning :one
UPDATE update_tasks
SET status = 'running', started_at = now(), updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: FinishUpdateTask :one
-- Records a terminal task state (succeeded|failed|rolled_back|skipped) with the
-- resolved versions and any detail/error. Tenant-scoped by id+tenant_id.
UPDATE update_tasks
SET status = $3,
    from_version = $4,
    to_version = $5,
    detail = $6,
    error = $7,
    finished_at = now(),
    updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: SetUpdateRunStatus :one
UPDATE update_runs
SET status = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: CountUnfinishedTasksForRun :one
-- Counts tasks not yet in a terminal state, used to decide when a run completes.
SELECT count(*) FROM update_tasks
WHERE run_id = $1 AND tenant_id = $2
  AND status IN ('pending', 'running');

-- name: CountRunningTasksForTenant :one
-- Best-effort per-tenant in-flight task count, used by the parallelism guard so
-- one tenant cannot saturate the worker pool. Runs in the tenant's RLS scope.
SELECT count(*) FROM update_tasks
WHERE tenant_id = $1 AND status = 'running';
