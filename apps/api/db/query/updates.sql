-- M3 bulk-update queries. Every statement here is tenant-scoped both
-- explicitly (tenant_id in the WHERE/VALUES) and by RLS
-- (update_runs_tenant_isolation / update_tasks_tenant_isolation on
-- app.tenant_id); update_tasks additionally carries the RESTRICTIVE
-- update_tasks_site_scope policy the two portal reads depend on.
--
-- TWO statements are deliberately outside that, and they are the only two.
-- Both are cross-tenant periodic sweeps admitted by an _agent policy rather
-- than by tenant isolation, and each repeats the fact at its own definition:
--
--   ListStaleUpdateTasks  the stale-task reaper (m89 update_tasks_agent).
--   ListDueUpdateRuns     the #463 deferred-dispatch due-scan
--                         (m118 update_runs_agent).
--
-- Any OTHER statement here without a tenant_id is a bug, not a third
-- exception. In particular, every statement the #463 dispatcher issues AFTER
-- the scan — the run claim, the task transitions, the expiry — carries
-- tenant_id explicitly, because the scan already returned the row and
-- therefore already knows it. Running under InAgentTx makes cross-tenant
-- access POSSIBLE; it is not a reason to stop naming the tenant. The scan is
-- the only statement that genuinely cannot, because finding the tenant is
-- what it is for.

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
-- @stale_after HAS TWO DEGENERATE VALUES, AND THEY FAIL IN OPPOSITE
-- DIRECTIONS. A genuine SQL NULL makes the reclaim branch match nothing (NULL
-- comparison) and fails CLOSED: strict pending-only claiming, safe but
-- conservative — an abandoned row then waits for the reaper instead of a
-- retry. A ZERO interval fails OPEN: coalesce(started_at, updated_at) <
-- now() - '0'::interval is true for every non-agent 'running' row the moment
-- it is written, so the reclaim branch matches rows a live worker is
-- mid-dispatch on — the double dispatch this precondition exists to prevent.
--
-- ONLY THE SECOND IS REACHABLE FROM GO. durationToInterval (update/repo.go)
-- builds pgtype.Interval with Valid: true unconditionally, so no caller on
-- this path can produce a SQL NULL: a zero time.Duration arrives as
-- interval '0'. Do NOT read the NULL case as what an unset bound gives you —
-- an unset bound gives you the OPEN one. Pass the derived value
-- (Worker.claimStaleAfter, from ClaimStaleAfter), never the zero value.
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
--
-- THIS PREDICATE IS A DELIBERATE SUPERSET OF THE IN-FLIGHT SET, NOT A COPY OF
-- IT. It answers "is any outcome still owed for this task?", which is a
-- different question from the one the other two predicates in this file ask,
-- and it is only by coincidence that they shared a spelling:
--
--   update_tasks_inflight_target_idx  "does this target hold the dedup slot?"
--   ListStaleUpdateTasks              "could this row be stuck?"
--
-- Both of those are exactly {pending, running}. 'scheduled' is deliberately
-- absent from both (m118: a task waiting for a clock must neither reserve its
-- target nor be reaped as stuck) and is emphatically present here, because a
-- task waiting for a clock is the least finished a task can be. Keeping this
-- predicate in sync with those two by reflex is the mistake; the sets are
-- supposed to differ, and by exactly this one status.
--
-- WITHOUT IT THIS QUERY REPORTS A RUN COMPLETE WHILE IT STILL HAS UNDISPATCHED
-- WORK. Dispatch moves tasks 'scheduled' -> 'pending' ONE AT A TIME
-- (MarkScheduledUpdateTaskPending), so a partially dispatched run legitimately
-- holds both statuses at once. If the tasks moved first finish before the rest
-- are moved, the old predicate counts zero, the caller concludes the run is
-- done and marks it 'completed', and the still-'scheduled' remainder is
-- stranded: not terminal, so no outcome was recorded; not in the reaper's set,
-- so nothing ever un-sticks it; not in the due index, because the RUN is no
-- longer 'scheduled'. The operator sees a completed run that updated a subset
-- of the fleet it named.
--
-- THE SUPERSET RELATIONSHIP IS ENFORCED, NOT MERELY ASSERTED HERE. This query
-- is registered in notFinishedSupersets (update/inflight_status_guard_test.go),
-- which reads this predicate out of this file and pins it from three
-- directions, so neither half of the distinction above can rot:
--
--   * dropping an in-flight status ('pending', 'running') fails — every
--     in-flight task is by definition unfinished, and losing one marks a run
--     complete while dispatched work is outstanding;
--   * adding a TERMINAL status ('skipped', 'cancelled', 'expired') fails —
--     a superset may only add a non-terminal status that holds no dedup slot,
--     and folding in a terminal one leaves the run permanently incomplete;
--   * equalling the in-flight set exactly fails — a predicate that is a plain
--     copy does not belong on that list, and a stale entry there would exempt
--     a genuine copy from the drift check that is the guard's whole point.
--
-- So the invariant to preserve is the RELATIONSHIP, not the literal list: this
-- predicate is InFlightTaskStatuses plus nonTerminalNotInFlight, and changing
-- either side is a decision made in that guard, not silently here.
SELECT count(*) FROM update_tasks
WHERE run_id = $1 AND tenant_id = $2
  AND status IN ('pending', 'running', 'scheduled');

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

-- ===========================================================================
-- GH #463 Phase 1 — deferred dispatch of scheduled update runs.
--
-- Every statement below is a COMPARE-AND-SWAP with a status precondition, and
-- the precondition is never a formality. Two control-plane replicas tick at the
-- same time; the transition is decided by the row, under its own row lock, and
-- not by what the caller read a moment earlier. That is the whole idempotency
-- guarantee of this feature, and it is the lesson of MarkUpdateTaskRunning,
-- which shipped without one.
--
-- ZERO ROWS IS A RESULT, NOT AN ERROR, in every one of them. Each says what it
-- means at its own definition, because that is the contract the Go layer codes
-- against.
--
-- ONE of them (ListDueUpdateRuns) is cross-tenant; see the file header. Every
-- other statement carries tenant_id, taken from the row the scan returned.
-- ===========================================================================

-- name: ListDueUpdateRuns :many
-- The dispatcher's tick: which runs, belonging to ANY tenant, have come due?
-- Cross-tenant, under InAgentTx, admitted by update_runs_agent (m118) — the
-- second and last tenant_id-less statement in this file (see the header).
--
-- "status = 'scheduled'" IS RESTATED VERBATIM AND IS NOT REDUNDANT. It is the
-- exact predicate of update_runs_due_idx, and PostgreSQL will only use a
-- partial index when it can prove the index predicate from the query's own
-- WHERE clauses. Written any other way — status <> 'pending', status IN
-- ('scheduled'), a join against a status table — the proof fails, the index is
-- discarded, and this becomes a sequential scan over every run the install has
-- ever created, on every tick, forever. It would still return the right rows,
-- which is why nothing would ever fail; it would simply get slower for the life
-- of the installation. m118 DECISION 2 chose the predicate to be a clause this
-- query already states for its own reasons, precisely so it can be restated
-- without inventing anything.
--
-- NO INTERVAL PARAMETER, DELIBERATELY, and this is the grace window's doing.
-- The obvious shape takes the grace window here and returns only runs still
-- inside it. That is wrong in the unrecoverable direction: a run that fell PAST
-- the window would then never be returned by any query, so nothing would ever
-- observe it, and it would sit at 'scheduled' with its tasks 'scheduled'
-- forever — invisible to the reaper (which excludes both by construction) and
-- to CountUnfinishedTasksForRun. The scan therefore returns EVERY due run and
-- the caller splits them: inside the window -> ClaimUpdateRunForDispatch,
-- outside it -> ExpireDueUpdateRun. The window is applied at fire time, once,
-- on a row that has already been found.
--
-- Avoiding the parameter also keeps this statement clear of the
-- durationToInterval trap (update/repo.go builds pgtype.Interval with
-- Valid: true unconditionally, so an unset Go duration arrives as interval '0',
-- not NULL). Had the window been an interval here, a zero value would make
-- "now() - '0'" match every scheduled run and the scan would report the whole
-- backlog as expired. See ExpireDueUpdateRun for how the one statement that
-- genuinely needs the window takes it, and why it takes an instant instead.
--
-- now() IS THE DATABASE CLOCK, on purpose: due-ness is compared against the
-- same clock for every replica, so two control planes with drifting wall clocks
-- cannot disagree about whether a run has arrived.
--
-- scheduled_at IS NULL rows can never match (NULL <= now() is NULL). A
-- 'scheduled' run with no scheduled_at is a should-not-exist row that this scan
-- will not surface; m118 DECISION 2 keeps it inside the index so it is at least
-- findable. It is not this statement's job to invent a policy for it.
--
-- @row_limit bounds one pass, mirroring ListStaleUpdateTasks — pass
-- staleTaskReapLimit's sibling constant, never an unbounded value. Any
-- remainder is picked up by the next tick, which is why the ordering is by
-- scheduled_at ASC: the run that has waited longest goes first, so a backlog
-- drains in the order the operators asked for rather than at random.
--
-- ZERO ROWS MEANS: nothing is due. That is the overwhelmingly common case and
-- it is not an error — but it is also EXACTLY what a missing RLS policy looks
-- like (m84/#96, m89/#131, and the reason m118 exists at all). The caller must
-- not distinguish them by guessing; it distinguishes them by the Phase 0 test
-- that proves this statement returns rows under InAgentTx.
SELECT * FROM update_runs
WHERE status = 'scheduled'
  AND scheduled_at <= now()
ORDER BY scheduled_at ASC, id ASC
LIMIT @row_limit;

-- name: ClaimUpdateRunForDispatch :one
-- The claim: 'scheduled' -> 'dispatching', for ONE run the scan returned.
-- Tenant-scoped by id+tenant_id even though the caller runs under InAgentTx,
-- because the scan already handed us the tenant and naming it costs nothing.
--
-- ZERO ROWS MEANS: SOMEONE ELSE OWNS THIS RUN. DO NOT DISPATCH IT. Skip to the
-- next row of the scan and do not treat it as an error, do not retry it, and do
-- not log it as a failure — on a two-replica install this is the normal outcome
-- for roughly half of every contested tick. It is the entire cross-replica
-- idempotency guarantee of this feature: the row's status, under its own lock,
-- decides who dispatches, so exactly one caller can ever observe a returned row
-- for a given transition.
--
-- It does not say WHY, and the caller does not need to know: every losing
-- interpretation has the same correct action (leave it alone). The three that
-- occur are another replica claiming it first ('dispatching'), an operator
-- cancelling it in the gap between the scan and the claim ('halted'), and this
-- pass's own expiry arm having taken it ('expired'). A caller that wants the
-- reason for a log line may re-read with GetUpdateRun; it must not vary its
-- behaviour on the answer.
--
-- ONE ROW MEANS: you own this run for this tick, and you are now responsible for
-- moving it out of 'dispatching'. 'dispatching' is transient by contract
-- (m118), and it is NOT self-healing: it is absent from update_runs_due_idx, so
-- a run left there is never scanned again. Enqueue the work and call
-- FinishUpdateRunDispatch in the SAME transaction, so a crash rolls the claim
-- back and the next tick finds the run still 'scheduled'. Committing the claim
-- separately from the enqueue is what strands a run permanently.
UPDATE update_runs
SET status = 'dispatching', updated_at = now()
WHERE id = @id AND tenant_id = @tenant_id
  AND status = 'scheduled'
RETURNING *;

-- name: FinishUpdateRunDispatch :one
-- Closes the transient 'dispatching' window: 'dispatching' -> @status, where
-- @status is the state the run enters now that its tasks are enqueued
-- ('running' when any task was dispatched, 'completed' when every task was
-- skipped and there is nothing to wait for).
--
-- This exists so the scheduled lifecycle never has to touch SetUpdateRunStatus,
-- which has NO precondition at all: it writes whatever it is given to whatever
-- the row currently holds. That was harmless while runs only ever moved
-- forwards under one worker, and it stops being harmless the moment a second
-- writer (this dispatcher) can be mid-transition on the same row — an
-- unconditioned write lands on top of a claim, or resurrects an 'expired' run
-- into 'running'. See the note handed to backend-architect: SetUpdateRunStatus
-- keeps its existing callers and must not acquire new ones on this path.
--
-- ZERO ROWS MEANS: the run was not in 'dispatching', so this caller did not own
-- the claim it thinks it owns. Treat it as a bug in the calling sequence and
-- surface it — unlike the claim above, losing here is not a normal race. The
-- only writer that can move a row out of 'dispatching' is the one that put it
-- there.
UPDATE update_runs
SET status = @status, updated_at = now()
WHERE id = @id AND tenant_id = @tenant_id
  AND status = 'dispatching'
RETURNING *;

-- name: ExpireDueUpdateRun :one
-- The grace window, applied at fire time: 'scheduled' -> 'expired' for a run
-- that came due so long ago that running it now is no longer what the operator
-- asked for. Terminal, never retried (m118).
--
-- @expire_before IS AN INSTANT, NOT AN INTERVAL, AND THAT IS THE WHOLE POINT OF
-- ITS TYPE. The caller passes now()-grace as a timestamptz; it does NOT pass the
-- grace duration. Written the natural way —
--
--     AND scheduled_at < now() - @grace::interval
--
-- — this statement would be the fail-open inversion this codebase has already
-- paid for four times. durationToInterval (update/repo.go) hardcodes
-- Valid: true, so an unset or zero Go duration cannot arrive as SQL NULL; it
-- arrives as interval '0'. "scheduled_at < now() - '0'" is TRUE for every run
-- that is due, so a zero grace window would EXPIRE EVERY DUE RUN AT THE MOMENT
-- IT BECAME DUE — the fleet's entire scheduled backlog terminalized, silently,
-- by a config value nobody set. The failure would look like the feature working:
-- runs move to a terminal state on schedule, and no site is ever contacted.
--
-- AS A TIMESTAMPTZ, BOTH DEGENERATE VALUES FAIL CLOSED, and unlike the interval
-- case neither of them is reachable by accident into the destructive direction.
-- sqlc generates this parameter as pgtype.Timestamptz (NOT time.Time — the
-- timestamptz override in sqlc.yaml does not apply here, because the comparison
-- is against the nullable scheduled_at, so the parameter is inferred nullable;
-- verified in the generated ExpireDueUpdateRunParams). Its zero value is
-- {Valid: false}, which is a genuine SQL NULL, and "scheduled_at < NULL" is
-- NULL, so an unset cutoff matches NOTHING. A Valid-but-zero Time is year 1 and
-- matches nothing either. An unconfigured grace window therefore expires no run
-- at all: the run stays due and is dispatched or re-scanned — visible,
-- recoverable, and wrong in the direction that does not destroy the operator's
-- work.
--
-- That is the whole argument for the type. Choose the parameter type so the
-- unset value is the safe one, rather than documenting an unsafe one and
-- relying on every future caller to have read the documentation.
--
-- The status precondition still does the mutual exclusion: a run this pass
-- expires cannot also be claimed, because ClaimUpdateRunForDispatch requires the
-- same 'scheduled' and only one of the two CAS statements can match. The time
-- bound is therefore defence-in-depth against a caller that mis-computes the
-- cutoff — and it is defence in the direction that matters, since expiry is the
-- destructive arm and a wrongly expired run is a bulk update that silently never
-- happened.
--
-- ZERO ROWS MEANS: this run was not expirable — either it is no longer
-- 'scheduled' (claimed, or cancelled, in the gap since the scan) or it is not
-- actually past the cutoff. Both are benign; skip it. Never retry expiry, and
-- never fall back to an unconditioned write.
--
-- THE RUN'S TASKS ARE NOT TOUCHED HERE. Call
-- TerminalizeScheduledTasksForRun in the SAME transaction; a run that is
-- 'expired' with tasks still 'scheduled' is exactly the stranded shape
-- CountUnfinishedTasksForRun's comment describes.
UPDATE update_runs
SET status = 'expired', updated_at = now()
WHERE id = @id AND tenant_id = @tenant_id
  AND status = 'scheduled'
  AND scheduled_at < @expire_before
RETURNING *;

-- name: CancelScheduledUpdateRun :one
-- Operator-initiated cancellation of a run that has NOT yet fired.
--
-- FROM 'scheduled' ONLY, and that is the safety property rather than a
-- convenience. It guarantees the cancel can never race a dispatch into a
-- half-cancelled state: once the dispatcher has claimed the run
-- ('dispatching') or the work is out ('running'), this matches zero rows and
-- the operator is told to use the existing HALT path instead — which is a
-- different operation with different consequences, because halting a running
-- run leaves already-dispatched commands in flight on real sites. Cancelling a
-- scheduled run promises that nothing was ever sent, and this precondition is
-- what makes that promise true.
--
-- @status is supplied rather than hardcoded because THE RUN VOCABULARY HAS NO
-- 'cancelled'. The run statuses Go writes are defined in update/model.go — that
-- file is the list, and it is not restated here precisely because a copied list
-- goes stale silently. The intended argument is RunHalted, so an operator's
-- cancellation lands the run on 'halted' while its tasks go to 'cancelled'
-- (TerminalizeScheduledTasksForRun sets out why that pair is asymmetric).
--
-- Parameterised rather than inlined so this file cannot mint a run status by
-- literal: there is no CHECK constraint, so an invented value would store
-- cleanly and then fail to render, gen.UpdateRunStatus being a closed enum.
--
-- STILL OPEN, and verified against this tree rather than remembered: 'halted'
-- appears in no migration and is absent from db/schema.sql's run-status list,
-- yet m118's COMMENT ON COLUMN calls itself the contract. The contract is
-- therefore incomplete for a value the code has written since long before #463.
-- Correcting it is a NEW ordinal, never an edit to m118.
--
-- ZERO ROWS MEANS: too late, or already gone. The run has left 'scheduled', so
-- the caller must re-read it and return a conflict to the operator rather than
-- reporting a cancellation that did not happen. Do NOT fall back to
-- SetUpdateRunStatus to force it.
--
-- Tasks are not touched here; call TerminalizeScheduledTasksForRun with
-- 'cancelled' in the same transaction.
UPDATE update_runs
SET status = @status, updated_at = now()
WHERE id = @id AND tenant_id = @tenant_id
  AND status = 'scheduled'
RETURNING *;

-- name: CreateScheduledUpdateTask :one
-- Plans one task for a DEFERRED run. Identical to CreateUpdateTask except that
-- the row is born 'scheduled', and that difference changes what the ON CONFLICT
-- clause means — read this before assuming the two are interchangeable.
--
-- CreateUpdateTask's zero-row result means "this (site, target) is already in
-- flight from another run, skip it". HERE IT CANNOT MEAN THAT AND MUST NOT BE
-- READ THAT WAY. update_tasks_inflight_target_idx is partial on status IN
-- ('pending','running'); a row inserted as 'scheduled' does not satisfy that
-- predicate, so it never enters the index and can never conflict with anything.
-- The ON CONFLICT clause is retained only so the statement is well-formed
-- against the same arbiter, and it is unreachable: this insert always returns
-- its row.
--
-- THAT IS THE DESIGN, NOT AN OVERSIGHT (#463): a run waiting until 02:00 must
-- not hold the (tenant, site, plugin) slot all day, because doing so would
-- reject the operator's urgent 10:00 update of that same plugin with
-- 409 targets_in_flight. The reservation is deliberately deferred to
-- MarkScheduledUpdateTaskPending, which is where the collision is detected and
-- resolved. DO NOT "fix" this by widening the index: that is a separate
-- migration with a data-dedup step, and it reintroduces the bug this phase
-- exists to avoid.
--
-- The consequence the caller must carry: planning a scheduled run CANNOT
-- pre-verify that its targets will be free when it fires. Some tasks will be
-- skipped at dispatch time. That is expected, and it is why
-- MarkScheduledUpdateTaskPending has a skip path at all.
INSERT INTO update_tasks (
    run_id, tenant_id, site_id, target_type, target_slug, desired_version,
    from_version, status
)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'scheduled')
ON CONFLICT (tenant_id, site_id, target_type, target_slug)
    WHERE status IN ('pending', 'running')
    DO NOTHING
RETURNING *;

-- name: MarkScheduledUpdateTaskPending :one
-- Dispatch, per task: 'scheduled' -> 'pending', which is the moment the task
-- becomes real work — it enters update_tasks_inflight_target_idx, becomes
-- claimable by MarkUpdateTaskRunning, and becomes visible to the stale-task
-- reaper. Nothing before this point is eligible for any of the three.
--
-- THE NOT EXISTS ARM IS THE IN-FLIGHT COLLISION, and it is a legitimate outcome
-- rather than a failure. Because a scheduled task deliberately does not reserve
-- its (tenant, site, target) slot (see CreateScheduledUpdateTask), the slot may
-- have been taken by an urgent immediate update while this run waited. Moving
-- the row to 'pending' would then violate update_tasks_inflight_target_idx and
-- raise 23505 — which, inside the dispatcher's transaction, aborts the whole
-- transaction and takes every OTHER task of the run down with it. One operator
-- updating one plugin by hand would fail the entire scheduled run. The guard
-- turns that into an ordinary zero-row result on the common path.
--
-- IT IS A GUARD, NOT A LOCK, AND THE RESIDUAL RACE REMAINS. A concurrent
-- transaction can insert the conflicting 'pending' row between this predicate's
-- evaluation and this statement's commit, and then the unique index raises
-- 23505 exactly as before. The caller must therefore STILL be able to survive
-- 23505 on this statement — issue it under a SAVEPOINT, or one task per
-- transaction — and treat that error as the same outcome as zero-rows-with-the-
-- row-still-scheduled. The guard makes the common case cheap; it does not make
-- the rare case impossible, and a caller written as though it did will lose a
-- whole run to a race it could have absorbed.
--
-- ZERO ROWS IS AMBIGUOUS BY CONSTRUCTION AND THE CALLER DISAMBIGUATES BY
-- RE-READING THE ROW (GetUpdateTask, same transaction). This mirrors
-- MarkUpdateTaskRunning's documented contract, and the two answers need
-- opposite handling:
--
--   status <> 'scheduled'  Someone else already moved it — a concurrent
--                          dispatcher pass, or an operator cancelling the run
--                          under us. NOT this caller's task. Leave the recorded
--                          state alone and move on; do not re-dispatch.
--
--   status  = 'scheduled'  The target is in flight from another run. This is
--                          the SKIPPED case the design names: the task is not
--                          attempted, the RUN IS NOT FAILED, and the remaining
--                          tasks of the run proceed normally. Record it with
--                          FinishScheduledUpdateTask('skipped', detail) so the
--                          row reaches a terminal state and the operator can
--                          see why that one site was left alone. A skip left
--                          unrecorded is a task stuck at 'scheduled' forever.
--
-- Distinguishing them by the zero-row result alone is impossible, which is why
-- the re-read is part of the contract and not an optimisation the caller may
-- skip. Never treat zero rows here as an error to retry into.
UPDATE update_tasks t
SET status = 'pending', updated_at = now()
WHERE t.id = @id AND t.tenant_id = @tenant_id
  AND t.status = 'scheduled'
  AND NOT EXISTS (
      SELECT 1 FROM update_tasks x
      WHERE x.tenant_id   = t.tenant_id
        AND x.site_id     = t.site_id
        AND x.target_type = t.target_type
        AND x.target_slug = t.target_slug
        AND x.id <> t.id
        AND x.status IN ('pending', 'running')
  )
RETURNING t.*;

-- name: FinishScheduledUpdateTask :one
-- Terminalizes ONE task that never left 'scheduled'. The counterpart to
-- FinishUpdateTask, which cannot be used here: its precondition is
-- status IN ('pending','running'), so a scheduled task is not "open" by its
-- definition and matches zero rows there. That is correct — FinishUpdateTask
-- records the outcome of an ATTEMPT, and nothing was ever attempted here.
--
-- @status is the terminal state, and exactly three values are admissible.
-- All three mean "nothing was ever sent to this site"; they differ in WHY, and
-- that difference is the whole point of recording them separately:
--
--   'skipped'    (update.TaskSkipped) Its (site, target) was in flight from
--                another run when the dispatcher reached it
--                (MarkScheduledUpdateTaskPending's zero-row-still-scheduled
--                case). Consistent with 'skipped' elsewhere in this file: not
--                attempted, by decision of the control plane.
--   'cancelled'  (update.TaskCancelled) A human stopped the run before it
--                fired. Consistent with CancelPendingUpdateTask: nothing was
--                ever sent to this site, and nothing on it changed.
--   'expired'    (update.TaskExpired) The parent run expired without
--                dispatching, so this task was never attempted. Terminal.
--                (m118's COMMENT ON COLUMN and db/schema.sql carry this same
--                sentence; the three are meant to agree word for word.)
--
-- 'cancelled' AND 'expired' ARE NOT SPELLINGS OF EACH OTHER. 'cancelled'
-- records a decision a human made; 'expired' records that the window closed
-- while the control plane was not up in time to start the run. Collapsing them
-- tells an operator that somebody stopped their scheduled update when in fact
-- nobody did — which is the single distinction this feature exists to make, and
-- the erasure m118's column comment was written to prevent.
--
-- There is NO CHECK CONSTRAINT on this column, so a typo stores cleanly and the
-- row leaves every predicate in this file at once — invisible to the reaper,
-- to the dedup index and to CountUnfinishedTasksForRun. Pass a named constant.
--
-- error is deliberately not written: none of these outcomes is a failure, and
-- putting text in error is what makes an operator's run render as failed. The
-- reason goes in detail.
--
-- ZERO ROWS MEANS: the task was not 'scheduled' — it was dispatched, cancelled
-- or terminalized in the gap. The recorded outcome wins; leave it alone (the
-- same rule as FinishUpdateTask's ErrTaskNotOpen path). Never widen the
-- precondition to force the write through.
UPDATE update_tasks
SET status      = @status,
    detail      = @detail,
    finished_at = now(),
    updated_at  = now()
WHERE id = @id AND tenant_id = @tenant_id
  AND status = 'scheduled'
RETURNING *;

-- name: TerminalizeScheduledTasksForRun :many
-- Terminalizes EVERY still-scheduled task of one run, in one statement. Used by
-- both run-level endings — expiry (ExpireDueUpdateRun) and operator
-- cancellation (CancelScheduledUpdateRun) — which differ only in @status and
-- @detail, so they share the statement rather than duplicating a predicate that
-- must not drift between them.
--
-- MUST BE ISSUED IN THE SAME TRANSACTION AS THE RUN TRANSITION. A run that
-- reaches a terminal state while its tasks are still 'scheduled' strands them
-- permanently: 'scheduled' is not terminal, so no outcome is recorded; it is
-- excluded from ListStaleUpdateTasks by construction, so the reaper will never
-- un-stick them; and the run has left update_runs_due_idx, so no future tick
-- will ever look at it again. There is no janitor anywhere in this system that
-- would find them. Splitting these two writes across transactions is the single
-- way to leak rows in this feature.
--
-- @status takes the same admissible values as FinishScheduledUpdateTask, and
-- THE TWO RUN-LEVEL ENDINGS PASS DIFFERENT ONES. They agree that nothing was
-- ever sent to the site; they disagree about why, and the row is the only place
-- that survives to say so:
--
--   expiry  ExpireDueUpdateRun    -> run 'expired', tasks 'expired'
--                                   (dispatch_repo.go, TaskExpired)
--   cancel  CancelScheduledUpdateRun -> run 'halted', tasks 'cancelled'
--                                   (cancel_repo.go, TaskCancelled)
--
-- The run vocabulary has no 'cancelled', so operator cancellation lands the RUN
-- on 'halted' while its TASKS go to 'cancelled'. That asymmetry is deliberate:
-- minting a 'cancelled' run status would create a value no existing reader can
-- render (gen.UpdateRunStatus is a closed enum). Do not "tidy" the pair into
-- matching by inventing one.
--
-- ONLY 'scheduled' ROWS ARE TOUCHED, which is what makes this safe to run
-- against a partially dispatched run: a task already moved to 'pending' or
-- beyond has real work behind it and its outcome belongs to the worker, not to
-- this statement. Cancelling those is CancelPendingUpdateTask's job and carries
-- its own reasoning about what 'cancelled' may claim.
--
-- ZERO ROWS MEANS: the run had no still-scheduled tasks. Benign and common —
-- an already-dispatched run, or a re-run of the same ending. It is NOT
-- confirmation that the run had no tasks, and must not be logged as one.
-- RETURNING the rows so the caller can emit one audit record per affected task
-- without a second read.
UPDATE update_tasks
SET status      = @status,
    detail      = @detail,
    finished_at = now(),
    updated_at  = now()
WHERE run_id = @run_id AND tenant_id = @tenant_id
  AND status = 'scheduled'
RETURNING *;
