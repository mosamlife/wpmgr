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
-- Records a terminal task state (succeeded|failed|rolled_back|skipped|cancelled)
-- with the resolved versions and any detail/error. Tenant-scoped by
-- id+tenant_id.
--
-- The status precondition is what makes a terminal state FINAL. Without it, a
-- worker that was already in flight when its run was halted comes back later
-- and overwrites 'cancelled' with 'succeeded', so the kill switch appears to
-- have stopped a rollout that in fact reported itself as a success. Only a task
-- still open (pending|running) may be finished; a caller that matches no row
-- must read the row back and leave the recorded outcome alone (see
-- pgRepo.FinishTask / ErrTaskNotOpen).
UPDATE update_tasks
SET status = $3,
    from_version = $4,
    to_version = $5,
    detail = $6,
    error = $7,
    finished_at = now(),
    updated_at = now()
WHERE id = $1 AND tenant_id = $2
  AND status IN ('pending', 'running')
RETURNING *;

-- name: SiteHasRunningUpdateTask :one
-- GH #328 per-site serialisation pre-check (INVARIANT R). Is a command
-- CURRENTLY IN FLIGHT to this site? Deliberately best-effort and read-only:
-- the AGENT's own site-update lock is the authoritative bound (WP_Upgrader::
-- create_lock, see the agent lane). A missed collision here costs one HTTP
-- round trip the agent itself refuses, which the worker then defers; it is
-- not a correctness gap. Do NOT "fix" this with an advisory lock or a
-- conditional claim across the round trip — the guarantee is not here.
--
-- status = 'running' means exactly "dispatched and not yet resolved"
-- (INVARIANT R). DeferUpdateTaskToPending is what keeps that true: a task
-- WAITING for this site is 'pending' and therefore can never itself match
-- this predicate. That is why a waiter can never block a sibling waiter:
-- waiters depend only on runners, and runners depend on nothing (every River
-- job for this worker exits within its own job timeout, whether the site
-- answers or not), so the wait-for graph has no cycle.
--
-- target_type <> 'agent' is NOT an optimisation. An agent self-update task
-- stays 'running' for its whole confirmation window (20m, or 90m on external
-- cron; see agentConfirmDeadline/agentConfirmDeadlineExternalCron), during
-- which the site's writer is NOT held: the apply happens after the ARM's
-- HTTP response has already been released, and from then on the agent polls
-- its OWN site lock directly. Counting a running agent-task row here would
-- block every plugin/theme/core update on that site for up to 90 minutes for
-- no reason; the agent's own lock is what serialises the two channels in
-- both directions.
--
-- The staleness clause bounds a crashed worker: a row whose command went out
-- longer ago than @hold_max cannot have a live worker behind it (River
-- cancels the job's context at its own job timeout regardless of whether the
-- site answers). Ignoring such a row only makes the gate MORE permissive
-- (lets a sibling dispatch it would otherwise have deferred), never less,
-- which is safe — the agent's own lock is still the backstop either way.
-- coalesce(started_at, updated_at) covers a row observed between
-- MarkUpdateTaskRunning (which sets both) and any later touch.
--
-- Rides the existing update_tasks_site_id_idx (m3). No new column and no new
-- index: every predicate here reads columns update_tasks already has, and
-- the row is bounded to one site at a time by that existing index, so no
-- schema migration is needed for this gate.
SELECT EXISTS (
    SELECT 1 FROM update_tasks
    WHERE site_id = @site_id
      AND tenant_id = @tenant_id
      AND id <> @exclude_task_id
      AND target_type <> 'agent'
      AND status = 'running'
      AND coalesce(started_at, updated_at) > now() - @hold_max::interval
);

-- name: DeferUpdateTaskToPending :one
-- GH #328. Records that this task is WAITING for its site (busy with
-- another update) and is NOT talking to it right now. The status write is
-- the whole point: a waiting task must not look 'running' to
-- SiteHasRunningUpdateTask above, to CountRunningTasksForTenant (so one busy
-- site consumes ONE of the tenant's parallelism slots rather than one per
-- deferred sibling), or to an operator watching the run.
--
-- started_at is cleared because it would otherwise be a lie (the task is not
-- currently running); MarkUpdateTaskRunning sets it again on the next real
-- dispatch. updated_at = now() is what keeps ListStaleUpdateTasks off a row a
-- live worker is actively minding: this statement is only ever issued from
-- inside Worker.Work, so a fresh updated_at is EVIDENCE a River job is still
-- behind the row, not merely an assertion. When the job is lost (a crashed
-- worker, a dropped queue) the watermark freezes and the periodic reaper
-- (ListStaleUpdateTasks) claims the row after its own threshold, exactly as
-- it would for any other stuck task — a busy task never becomes immortal.
--
-- The status precondition mirrors FinishUpdateTask's: a task a halt already
-- terminalized matches no row, and the caller (Worker.deferForBusySite) must
-- stop rather than snooze — the recorded terminal outcome wins. IN
-- ('pending','running') rather than = 'running' so a task that is ALREADY
-- pending (the pre-dispatch gate case: nothing was ever sent) can still
-- record the wait detail idempotently.
UPDATE update_tasks
SET status     = 'pending',
    started_at = NULL,
    detail     = @detail,
    updated_at = now()
WHERE id = @id AND tenant_id = @tenant_id
  AND status IN ('pending', 'running')
RETURNING *;

-- name: CancelPendingUpdateTask :one
-- Cancels ONE not-yet-dispatched task as part of halting its run.
--
-- The 'pending' precondition is the whole point, and it is enforced here rather
-- than in Go so it is atomic against a worker claiming the same row. A halt may
-- only cancel tasks nothing was ever sent for: update_tasks.status='cancelled'
-- means exactly "nothing was ever sent to this site". A RUNNING task has
-- already had its command delivered and (for an agent self-update) a cron event
-- spawned on the site, so cancelling it would both record a falsehood and make
-- the control plane stop listening for the outcome, at the exact moment an
-- operator hit the kill switch and most needs to know. Running tasks are left
-- to be resolved by their own confirmation job; the run is halted, so no
-- further wave can open behind them.
--
-- GH #328: a task DeferUpdateTaskToPending moved back to 'pending' MAY have
-- had one earlier command sent that the site refused before touching
-- anything (or one that never reached the agent at all), so 'pending' here
-- means "nothing was ever APPLIED to this site", not literally "never
-- contacted". That is still exactly what 'cancelled' is allowed to mean:
-- nothing on the site changed as a result, whichever of those two it was.
UPDATE update_tasks
SET status = 'cancelled',
    detail = $3,
    finished_at = now(),
    updated_at = now()
WHERE id = $1 AND tenant_id = $2
  AND status = 'pending'
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
