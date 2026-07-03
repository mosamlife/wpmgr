-- M3 bulk-update queries. Every statement is tenant-scoped both explicitly
-- (tenant_id in the WHERE/VALUES) and by RLS (the app.tenant_id policy).

-- name: CreateUpdateRun :one
-- tenant_id is supplied explicitly for defense-in-depth; RLS additionally
-- enforces it matches the current app.tenant_id setting.
INSERT INTO update_runs (tenant_id, created_by, status, dry_run, scheduled_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: CreateUpdateTask :one
-- ON CONFLICT targets the update_tasks_inflight_target_idx partial unique
-- index (tenant_id, site_id, target_type, target_slug) WHERE status IN
-- ('pending','running') — the authoritative cross-run dedup guard (#131
-- hardening). If the same (site, target) already has an in-flight task from
-- ANY OTHER run, the insert is a no-op and this returns zero rows (pgx
-- surfaces that as ErrNoRows); the repo treats that as "already in flight",
-- not an error, and skips creating the duplicate task.
INSERT INTO update_tasks (
    run_id, tenant_id, site_id, target_type, target_slug, desired_version,
    from_version, status
)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending')
ON CONFLICT (tenant_id, site_id, target_type, target_slug)
    WHERE status IN ('pending', 'running')
    DO NOTHING
RETURNING *;

-- name: GetUpdateRun :one
SELECT * FROM update_runs
WHERE id = $1 AND tenant_id = $2;

-- name: ListUpdateRuns :many
SELECT * FROM update_runs
WHERE tenant_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: ListUpdateRunsWithCounts :many
-- List runs with per-run task aggregate counts in a single query.
-- task_count: all tasks for the run.
-- succeeded_count: tasks with status='succeeded'.
-- failed_count: tasks with status IN ('failed','rolled_back').
-- site_count: distinct site_id values across all tasks.
-- `, id` tiebreaker follows the project ORDER BY convention.
SELECT
    r.*,
    coalesce(agg.task_count, 0)      AS task_count,
    coalesce(agg.succeeded_count, 0) AS succeeded_count,
    coalesce(agg.failed_count, 0)    AS failed_count,
    coalesce(agg.site_count, 0)      AS site_count
FROM update_runs r
LEFT JOIN LATERAL (
    SELECT
        count(*)                                          AS task_count,
        count(*) FILTER (WHERE status = 'succeeded')     AS succeeded_count,
        count(*) FILTER (WHERE status IN ('failed', 'rolled_back'))
                                                          AS failed_count,
        count(DISTINCT site_id)                           AS site_count
    FROM update_tasks t
    WHERE t.run_id = r.id AND t.tenant_id = r.tenant_id
) agg ON true
WHERE r.tenant_id = @tenant_id
ORDER BY r.created_at DESC, r.id DESC
LIMIT @row_limit OFFSET @row_offset;

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

-- name: ListInFlightUpdateTargets :many
-- Returns the (site_id, target_type, target_slug) of every task currently
-- pending or running for the tenant, restricted to the given site_ids, across
-- ANY run. Used by Service.planTasks to skip creating a duplicate task for a
-- target that already has an update in flight from another run (#131
-- hardening — see the update_tasks_inflight_target_idx partial unique index,
-- which is the authoritative guard; this query is the service-level
-- pre-check that avoids planning doomed tasks).
SELECT DISTINCT site_id, target_type, target_slug
FROM update_tasks
WHERE tenant_id = @tenant_id
  AND site_id = ANY(@site_ids::uuid[])
  AND status IN ('pending', 'running');

-- name: ListStaleUpdateTasks :many
-- Returns update_tasks stuck in pending/running for longer than @threshold,
-- across ALL tenants. Used by the periodic stale-task reaper (see
-- update.ReaperWorker) to un-stick a target that would otherwise permanently
-- occupy the update_tasks_inflight_target_idx partial unique index slot (m88)
-- forever — e.g. a worker crash between MarkUpdateTaskRunning and
-- FinishUpdateTask, or a failed EnqueueTask that leaves a task pending
-- (service.go's best-effort enqueue after CreateRunWithTasks). updated_at is
-- the staleness watermark: it is set at row creation and again by
-- MarkUpdateTaskRunning/FinishUpdateTask, so it reflects the last real
-- progress on the row. Capped at @row_limit per sweep so one periodic tick
-- cannot be unbounded; any remainder is caught by the next sweep. Runs under
-- app.agent (cross-tenant).
SELECT * FROM update_tasks
WHERE status IN ('pending', 'running')
  AND updated_at < now() - @threshold::interval
ORDER BY created_at ASC, id ASC
LIMIT @row_limit;

-- name: ListAppliedTasksForSite :many
-- Returns successfully applied update tasks for one site, ordered newest first.
-- Used by the client portal /portal/sites/:siteId/updates endpoint. Site-scope
-- RLS AND the explicit (site_id, tenant_id) filter together prevent cross-site
-- leakage.
SELECT target_type, target_slug, from_version, to_version, status, finished_at
FROM update_tasks
WHERE site_id   = @site_id
  AND tenant_id = @tenant_id
  AND status    = 'succeeded'
ORDER BY finished_at DESC, id DESC
LIMIT @row_limit;

-- name: ListAppliedTasksForSites :many
-- Returns recently succeeded update tasks across a set of sites, ordered newest
-- first. Used by the portal /summary recent_work feed. The site_ids param is
-- always p.AllowedSiteIDs (RLS double-gate via app.site_scope on update_tasks).
-- `, id` tiebreaker follows the project ORDER BY convention.
SELECT site_id, target_type, target_slug, from_version, to_version, finished_at
FROM update_tasks
WHERE tenant_id  = @tenant_id
  AND site_id    = ANY(@site_ids::uuid[])
  AND status     = 'succeeded'
  AND finished_at >= @since
ORDER BY finished_at DESC, id DESC
LIMIT @row_limit;
