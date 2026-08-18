-- M3 bulk-update queries. Every statement here is tenant-scoped both
-- explicitly (tenant_id in the WHERE/VALUES) and by RLS
-- (update_runs_tenant_isolation / update_tasks_tenant_isolation on
-- app.tenant_id); update_tasks additionally carries the RESTRICTIVE
-- update_tasks_site_scope policy the two portal reads depend on.
--
-- ONE statement is deliberately outside that, and it is the only one:
-- ListStaleUpdateTasks sweeps every tenant for the periodic reaper, carries
-- no tenant_id at all, and is admitted by the update_tasks_agent policy
-- instead. It repeats that at its own definition. Any OTHER statement in
-- db/query/updates.sql without a tenant_id is a bug, not a second exception.

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
-- The wave-machine read (loadAgentRunLocked) and any other path that needs the
-- raw task rows and nothing else. Deliberately join-free: it runs inside the
-- per-run advisory lock on every claim, so it stays as narrow as possible.
SELECT * FROM update_tasks
WHERE run_id = $1 AND tenant_id = $2
ORDER BY created_at ASC;

-- name: ListUpdateTasksForRunWithSiteName :many
-- The DETAIL read (GET /api/v1/updates/{runId}): the same tenant-scoped task
-- rows plus each site's display name.
--
-- The name is resolved HERE, by the database, and not by the caller joining a
-- task list against a separately-fetched site list. A client-side join has to
-- fetch that site list from somewhere, and every list endpoint in this control
-- plane is paginated (site.Service.List defaults to 50 rows), so a run wider
-- than one page silently renders raw UUIDs for the overflow and, worse, gives
-- a selection UI a site identity it cannot resolve. A task row already knows
-- exactly one site; this is the query that says which.
--
-- LEFT JOIN + coalesce rather than an inner join: sites.id is a FK with ON
-- DELETE CASCADE, so a task whose site is gone is gone too and the join
-- ALWAYS matches today. The left join keeps that an assumption the query does
-- not depend on -- a task row must never vanish from a run's history because
-- its site row could not be read.
--
-- The `, t.id` tiebreaker is the project ORDER BY convention and is
-- load-bearing here: every task in a run is inserted by ONE transaction and
-- Postgres gives every now() in a transaction the same timestamp, so
-- created_at alone is not a total order and rows could shuffle between two
-- reads of the same run. It also makes this order identical to the wave order
-- (waveOrder sorts by the same pair), so an agent rollout's canary is the
-- first row of the table.
SELECT t.*, coalesce(s.name, '')::text AS site_name
FROM update_tasks t
LEFT JOIN sites s ON s.id = t.site_id AND s.tenant_id = t.tenant_id
WHERE t.run_id = @run_id AND t.tenant_id = @tenant_id
ORDER BY t.created_at ASC, t.id ASC;

-- name: GetUpdateTask :one
SELECT * FROM update_tasks
WHERE id = $1 AND tenant_id = $2;

-- name: MarkUpdateTaskRunning :one
-- Claims a task for dispatch: the compare-and-swap that makes "I am the one
-- worker talking to this site about this item" true, rather than merely
-- assumed. Tenant-scoped by id+tenant_id.
--
-- WITHOUT the status precondition this was a bare write, and the read that
-- guards it is not in the same statement: Worker.Work loads the task, returns
-- early only for TERMINAL statuses, and claims some way further down
-- (GetTask -> ... -> MarkTaskRunning). Two River jobs for the same task both
-- observe a non-terminal row in that gap and both proceed, so one item is
-- applied to one site twice over. The precondition closes the gap the same way
-- FinishUpdateTask's does: the transition is decided by the row itself, under
-- its own row lock, not by what the caller read a moment earlier.
--
-- TRANSITIONS FROM, deliberately, exactly two states:
--
--   'pending'  — the ordinary claim. Also the ONLY state the agent
--                self-update wave path can present (ClaimAgentWaveTask holds
--                the run's advisory lock and returns ClaimAlreadyClaimed for
--                anything not pending before it ever reaches this statement),
--                and the state deferForBusySite leaves a waiter in
--                (DeferUpdateTaskToPending writes 'pending' and NULLs
--                started_at). So a deferred task re-claims normally.
--
--   'running', but only if ABANDONED — its command went out longer ago than
--                @stale_after. 'running' is not terminal, so a River retry of
--                a job that already claimed re-enters Work and reaches here
--                with its own row 'running'; a strict = 'pending' would match
--                zero rows and drop that work on the floor, which is a worse
--                bug than the one being fixed. Bounding it by age is what
--                separates "my previous attempt died" from "another worker is
--                mid-dispatch right now", but read the next two paragraphs for
--                what that separation is and is not worth.
--
--                WHAT THE BOUND GUARANTEES: past @stale_after, no CONTROL-PLANE
--                worker is still inside its apply call for this row. Callers do
--                not pass a constant; they derive the bound from THIS install's
--                apply budget — ClaimStaleAfter(applyJobTimeout) =
--                max(applyJobTimeout + claimStaleMargin, siteWriterHoldMax) in
--                update/worker.go — and main() refuses to boot on a
--                configuration where the budget could reach it
--                (ValidateClaimTimings, asserting applyJobTimeout <
--                ClaimStaleAfter < staleTaskThreshold). So the reclaim window
--                opens strictly after River has cancelled the previous job's
--                context and strictly before the periodic reaper terminalizes
--                the row.
--
--                WHAT IT DOES NOT GUARANTEE: that nothing is still applying ON
--                THE SITE. Cancelling a River job's context ends the control
--                plane's WAIT for the HTTP response; it does not reach into
--                WordPress and stop an apply the agent has already started. The
--                AGENT's own site-update lock is the authoritative bound on
--                that, exactly as it is for SiteHasRunningUpdateTask's
--                @hold_max below. If the bound is ever exceeded in practice the
--                residual is therefore a duplicate DISPATCH — a second command
--                for the same item and a second set of audit/event records for
--                it, which the agent's lock is what serialises — and not a
--                guaranteed double-apply. Still a real defect, and still worth
--                bounding; simply not a proof that no work is live.
--
--                coalesce(started_at, updated_at) mirrors the gate below for
--                the same reason.
--
-- target_type <> 'agent' on the reclaim branch is NOT an optimisation, and it
-- is the same exclusion SiteHasRunningUpdateTask carries: an agent
-- self-update task stays 'running' for its whole confirmation window (20m, or
-- 90m on external cron) with NO live worker behind it by design, because the
-- apply happens after the ARM response is released. Age is therefore not
-- evidence of abandonment for an agent row, and without this clause a wave
-- task would become re-claimable mid-confirmation. Agent rows may be claimed
-- from 'pending' only.
--
-- A NULL @stale_after makes the reclaim branch match nothing (NULL comparison),
-- which fails CLOSED: strict pending-only claiming. Safe but conservative —
-- an abandoned row then waits for the reaper instead of a retry. Pass the
-- constant.
--
-- ZERO ROWS MEANS: you did not get the claim, and you must NOT dispatch.
-- It does not distinguish why, so the caller re-reads the row and decides:
-- terminal => the outcome is recorded, stop (return nil, as Work already does
-- for a terminal read); still 'pending' or a FRESH 'running' => another worker
-- holds it, so give the row up WITHOUT consuming a retry (river.JobSnooze),
-- because that holder may still die and the row must stay reclaimable. Never
-- treat zero rows as an error to retry into, and never as permission to
-- proceed.
UPDATE update_tasks
SET status = 'running', started_at = now(), updated_at = now()
WHERE id = @id AND tenant_id = @tenant_id
  AND (
    status = 'pending'
    OR (
      status = 'running'
      AND target_type <> 'agent'
      AND coalesce(started_at, updated_at) < now() - @stale_after::interval
    )
  )
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
-- The staleness clause bounds a crashed WORKER, and only that: past @hold_max
-- no River job for this row can still be waiting on the site, because River
-- cancels the job's context at its own job timeout whether the site answers or
-- not. It says nothing about the SITE — cancelling that context ends the
-- control plane's wait for the HTTP response, it does not stop an apply the
-- agent has already begun (the same correction MarkUpdateTaskRunning's reclaim
-- arm carries above). Here that gap is harmless, and for a reason of its own:
-- ignoring an over-age row only makes the gate MORE permissive (lets a sibling
-- dispatch it would otherwise have deferred), never less, which is safe — the
-- agent's own lock is still the backstop either way.
--
-- @hold_max is therefore the flat siteWriterHoldMax constant, passed straight
-- through by worker.go, and NOT the config-derived ClaimStaleAfter the claim
-- above is given. Neither call site is wrong: see ClaimStaleAfter's doc
-- comment in update/worker.go, which sets out why over-permissive is safe for
-- this gate and is precisely the defect for the claim.
--
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
