package update

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ErrTaskNotOpen reports that a task could not be finished because it was no
// longer open (pending|running): something else already recorded its terminal
// state. The overwhelmingly common case is a run that was halted while this
// worker was in flight, the halt cancelled the task, and the worker returning
// afterwards must not overwrite 'cancelled' with 'succeeded', which would make
// the kill switch look like it stopped a rollout that then reported itself a
// success. It is not an error condition for the caller: the recorded outcome
// stands and the job is done. See Worker.finish.
var ErrTaskNotOpen = errors.New("update task is not open")

// ErrTaskNotClaimed reports that MarkTaskRunning's compare-and-swap matched no
// row: the caller did NOT get the claim and must not dispatch. It is
// deliberately distinct from domain.NotFound, which this path used to return
// for the same condition — the row is virtually always still there, held by
// another worker, and reporting "update task not found" sends an operator
// hunting for a deleted row that exists. It is also distinct from
// ErrTaskNotOpen: that one means a TERMINAL outcome is already recorded and
// the job is done, whereas this one carries no verdict at all. Zero rows does
// not say why (see MarkUpdateTaskRunning in db/query/updates.sql), so the
// caller re-reads the row and decides: terminal => stop; otherwise another
// worker holds it => give the row up WITHOUT consuming a retry attempt
// (river.JobSnooze), because that holder may still die and the row must stay
// reclaimable. See Worker.yieldContendedClaim.
var ErrTaskNotClaimed = errors.New("update task was not claimed")

// Repo is the tenant-scoped persistence interface for update runs/tasks. Every
// method runs inside a tenant-scoped transaction so RLS enforces isolation even
// if a query omitted its tenant filter.
type Repo interface {
	// CreateRunWithTasks atomically creates a run and its tasks in one tx.
	CreateRunWithTasks(ctx context.Context, in CreateRunInput, tasks []NewTask) (Run, []Task, error)

	// CreateScheduledRunWithTasks atomically creates a DEFERRED run (GH #463):
	// the run and every task are born 'scheduled', nothing enters the
	// in-flight index and nothing is enqueued. Requires in.ScheduledAt.
	CreateScheduledRunWithTasks(ctx context.Context, in CreateRunInput, tasks []NewTask) (Run, []Task, error)
	// ListDueRuns returns every run whose scheduled_at has arrived, across ALL
	// tenants, capped at limit. Cross-tenant, under InAgentTx. It does NOT
	// apply the grace window: the caller splits the result into dispatch and
	// expiry, because a scan that filtered by the window would leave a run
	// that fell past it unreachable by any query at all.
	ListDueRuns(ctx context.Context, limit int32) ([]Run, error)
	// DispatchDueRun claims one due run and, in the SAME transaction, moves
	// each of its scheduled tasks to 'pending' and enqueues its job. A task
	// whose target went in flight meanwhile is recorded 'skipped' and does not
	// fail the run. Claimed=false means another writer owns the run: skip it,
	// it is not an error.
	DispatchDueRun(ctx context.Context, enq TxEnqueuer, run Run) (DispatchOutcome, error)
	// ExpireDueRun terminalizes a run past the grace window along with its
	// undispatched tasks (as 'expired'), in ONE transaction. Reports whether
	// the run was expirable and how many tasks it terminalized.
	ExpireDueRun(ctx context.Context, tenantID, runID uuid.UUID, expireBefore time.Time, detail string) (bool, int, error)
	// CancelScheduledRun is the operator's call-back of a run that has not yet
	// fired: run and every still-scheduled task go terminal in ONE
	// transaction, tasks as 'cancelled'. Valid from 'scheduled' ONLY, so it can
	// never race a dispatch into a half-cancelled state. The bool reports
	// whether the run was cancellable; false means "too late", not an error.
	CancelScheduledRun(ctx context.Context, tenantID, runID uuid.UUID, detail string) (Run, int, bool, error)
	GetRun(ctx context.Context, tenantID, runID uuid.UUID) (Run, error)
	ListRuns(ctx context.Context, tenantID uuid.UUID, limit, offset int32) ([]Run, error)
	// ListRunSummaries returns runs with pre-computed task aggregate counts
	// (task_count, succeeded_count, failed_count, site_count) in a single query.
	// Used by the list endpoint to avoid N+1 per-run task fetches.
	ListRunSummaries(ctx context.Context, tenantID uuid.UUID, limit, offset int32) ([]RunSummary, error)
	ListTasks(ctx context.Context, tenantID, runID uuid.UUID) ([]Task, error)
	GetTask(ctx context.Context, tenantID, taskID uuid.UUID) (Task, error)

	// MarkTaskRunning is the claim: a compare-and-swap that transitions a task
	// to 'running' only from 'pending', or from an ABANDONED 'running' (a
	// non-agent row whose command went out longer ago than staleAfter). It
	// returns ErrTaskNotClaimed, never domain.NotFound, when it matches no row.
	//
	// staleAfter must be POSITIVE and is refused otherwise. It is NOT the
	// per-site gate's siteWriterHoldMax: the gate and the claim hold
	// deliberately separate values with opposite safe directions (see
	// ClaimStaleAfter). Production callers pass the worker's own derived
	// Worker.claimStaleAfter.
	//
	// A zero duration is NOT a NULL interval and does NOT fail closed. It is
	// interval '0', which makes every non-agent 'running' row instantly
	// reclaimable — the fail-OPEN direction. Hence the refusal.
	MarkTaskRunning(ctx context.Context, tenantID, taskID uuid.UUID, staleAfter time.Duration) (Task, error)
	// FinishTask records a terminal state for a task that is still OPEN
	// (pending|running). A task that already reached a terminal state is
	// returned unchanged alongside ErrTaskNotOpen: its recorded outcome wins.
	FinishTask(ctx context.Context, in FinishTaskInput) (Task, error)
	SetRunStatus(ctx context.Context, tenantID, runID uuid.UUID, status string) (Run, error)
	CountUnfinishedTasks(ctx context.Context, tenantID, runID uuid.UUID) (int64, error)
	CountRunningTasksForTenant(ctx context.Context, tenantID uuid.UUID) (int64, error)
	// ListInFlightTargets returns the set of (site_id, target_type, target_slug)
	// keys that currently have a pending or running task for the tenant, across
	// ANY run, restricted to siteIDs. Used by Service.planTasks to skip creating
	// a duplicate task for a target that already has an update in flight from
	// another run (#131 hardening): without this, a scheduled auto-update, an
	// operator bulk update, and a portal trigger can all create tasks for the
	// SAME (site, plugin) within the same window, and several run concurrently, // racing the agent's own rollback-snapshot pruning and running concurrent
	// WordPress Plugin_Upgrader instances against the same plugin directory. The
	// authoritative guard is the update_tasks_inflight_target_idx partial unique
	// index (see CreateUpdateTask's ON CONFLICT); this pre-check just avoids
	// planning doomed tasks and lets CreateRun report a clean "already in
	// progress" error instead of a partially-empty run.
	ListInFlightTargets(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID) (map[InFlightKey]struct{}, error)
	// ListStaleUpdateTasks returns tasks stuck in pending/running for longer
	// than threshold, across ALL tenants, capped at limit rows. Used by the
	// periodic reaper (see ReaperWorker) to terminalize a task a worker crash
	// or a failed enqueue left permanently occupying its (tenant, site,
	// target_type, target_slug) slot in the update_tasks_inflight_target_idx
	// partial unique index (m88) — without this, every future update attempt
	// for that target would 409 targets_in_flight forever.
	ListStaleUpdateTasks(ctx context.Context, threshold time.Duration, limit int32) ([]Task, error)

	// SiteHasRunningTask is the GH #328 per-site serialisation pre-check
	// (INVARIANT R): does SOME OTHER task for this site currently have a
	// command dispatched and unresolved? Best-effort — the agent's own
	// site-update lock is the authoritative bound; a missed collision here
	// just costs one HTTP round trip the agent itself refuses, which the
	// worker then defers. excludeTaskID excludes the caller's own row (a task
	// re-checking the gate against itself would always see itself as busy).
	// holdMax is the staleness bound (see SiteHasRunningUpdateTask's own
	// comment in db/query/updates.sql for why an over-age 'running' row is
	// ignored rather than trusted).
	SiteHasRunningTask(ctx context.Context, tenantID, siteID, excludeTaskID uuid.UUID, holdMax time.Duration) (bool, error)

	// DeferTaskToPending records that a task is WAITING for its site (busy
	// with another update) rather than talking to it: moves it back to
	// 'pending' (see DeferUpdateTaskToPending). Returns ErrTaskNotOpen when
	// the task already reached a terminal state (e.g. a halt raced the
	// defer); the caller must stop rather than snooze in that case, exactly
	// like FinishTask.
	DeferTaskToPending(ctx context.Context, in DeferTaskInput) (Task, error)
}

// DeferTaskInput is the input to DeferTaskToPending.
type DeferTaskInput struct {
	TenantID uuid.UUID
	TaskID   uuid.UUID
	// Detail is the operator-facing "waiting: ..." sentence recorded on the
	// task while it waits for its site.
	Detail string
}

// InFlightKey identifies one (site, target) pair that currently has a
// pending/running update task, keyed the same way tasks are planned
// ("site + type + slug"). Used by ListInFlightTargets/planTasks for the
// cross-run dedup guard (#131 hardening).
type InFlightKey struct {
	SiteID     uuid.UUID
	TargetType string
	TargetSlug string
}

// NewTask is the slim per-(site,item) row to insert when creating a run.
type NewTask struct {
	SiteID         uuid.UUID
	TargetType     string
	TargetSlug     string
	DesiredVersion string
	FromVersion    string
}

// FinishTaskInput records a terminal task state.
type FinishTaskInput struct {
	TenantID    uuid.UUID
	TaskID      uuid.UUID
	Status      string
	FromVersion string
	ToVersion   string
	Detail      string
	Error       string
}

type pgRepo struct {
	pool *db.Pool
}

// NewRepo builds a Repo backed by the pgx pool with RLS enforcement.
func NewRepo(pool *db.Pool) Repo { return &pgRepo{pool: pool} }

func (r *pgRepo) CreateRunWithTasks(ctx context.Context, in CreateRunInput, tasks []NewTask) (Run, []Task, error) {
	var run Run
	var outTasks []Task
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)

		var createdBy pgtype.UUID
		if in.CreatedBy != uuid.Nil {
			createdBy = pgtype.UUID{Bytes: in.CreatedBy, Valid: true}
		}
		var scheduledAt pgtype.Timestamptz
		if in.ScheduledAt != nil {
			scheduledAt = pgtype.Timestamptz{Time: *in.ScheduledAt, Valid: true}
		}

		runRow, err := q.CreateUpdateRun(ctx, sqlc.CreateUpdateRunParams{
			TenantID:    in.TenantID,
			CreatedBy:   createdBy,
			Status:      RunPending,
			DryRun:      in.DryRun,
			ScheduledAt: scheduledAt,
		})
		if err != nil {
			return domain.Internal("update_run_create_failed", "failed to create update run").WithCause(err)
		}
		run = toRun(runRow)

		outTasks = make([]Task, 0, len(tasks))
		for _, t := range tasks {
			taskRow, err := q.CreateUpdateTask(ctx, sqlc.CreateUpdateTaskParams{
				RunID:          run.ID,
				TenantID:       in.TenantID,
				SiteID:         t.SiteID,
				TargetType:     t.TargetType,
				TargetSlug:     t.TargetSlug,
				DesiredVersion: t.DesiredVersion,
				FromVersion:    t.FromVersion,
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					// The update_tasks_inflight_target_idx partial unique index
					// rejected this insert as a no-op (ON CONFLICT ... DO NOTHING):
					// a sibling run's task for the same (site, target) became
					// in-flight in the narrow window between the service's
					// ListInFlightTargets pre-check and this insert (#131
					// hardening). Skip it — a tight race is not a hard error.
					continue
				}
				return domain.Internal("update_task_create_failed", "failed to create update task").WithCause(err)
			}
			outTasks = append(outTasks, toTask(taskRow))
		}
		if len(outTasks) == 0 {
			// Every planned task lost the in-flight race to a concurrent run
			// between the pre-check and this transaction: do not commit a run
			// with zero tasks (it would otherwise sit "pending" forever, since
			// nothing ever marks it completed).
			return domain.Conflict("targets_in_flight", "the selected updates already have an update in progress in another run")
		}
		return nil
	})
	return run, outTasks, err
}

func (r *pgRepo) GetRun(ctx context.Context, tenantID, runID uuid.UUID) (Run, error) {
	var out Run
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetUpdateRun(ctx, sqlc.GetUpdateRunParams{ID: runID, TenantID: tenantID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("update_run_not_found", "update run not found")
			}
			return domain.Internal("update_run_get_failed", "failed to load update run").WithCause(err)
		}
		out = toRun(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) ListRuns(ctx context.Context, tenantID uuid.UUID, limit, offset int32) ([]Run, error) {
	var out []Run
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListUpdateRuns(ctx, sqlc.ListUpdateRunsParams{TenantID: tenantID, Limit: limit, Offset: offset})
		if err != nil {
			return domain.Internal("update_run_list_failed", "failed to list update runs").WithCause(err)
		}
		out = make([]Run, 0, len(rows))
		for _, row := range rows {
			out = append(out, toRun(row))
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) ListRunSummaries(ctx context.Context, tenantID uuid.UUID, limit, offset int32) ([]RunSummary, error) {
	var out []RunSummary
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListUpdateRunsWithCounts(ctx, sqlc.ListUpdateRunsWithCountsParams{
			TenantID:  tenantID,
			RowLimit:  limit,
			RowOffset: offset,
		})
		if err != nil {
			return domain.Internal("update_run_list_failed", "failed to list update runs with counts").WithCause(err)
		}
		out = make([]RunSummary, 0, len(rows))
		for _, row := range rows {
			s := RunSummary{
				Run:            toRunFromCounts(row),
				TaskCount:      row.TaskCount,
				SucceededCount: row.SucceededCount,
				FailedCount:    row.FailedCount,
				SiteCount:      row.SiteCount,
			}
			out = append(out, s)
		}
		return nil
	})
	return out, err
}

// toRunFromCounts converts a ListUpdateRunsWithCountsRow (flat struct with run
// fields + aggregate counts) to a Run. The count fields are handled separately
// by the caller.
func toRunFromCounts(r sqlc.ListUpdateRunsWithCountsRow) Run {
	out := Run{
		ID:        r.ID,
		TenantID:  r.TenantID,
		Status:    r.Status,
		DryRun:    r.DryRun,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
	if r.CreatedBy.Valid {
		id := uuid.UUID(r.CreatedBy.Bytes)
		out.CreatedBy = &id
	}
	if r.ScheduledAt.Valid {
		t := r.ScheduledAt.Time
		out.ScheduledAt = &t
	}
	return out
}

// ListTasks is the DETAIL read: every task of a run, in a stable order, each
// carrying its site's display name (see ListUpdateTasksForRunWithSiteName).
// Deliberately unpaginated, as it has always been: a run's task set is the run,
// and a caller that saw only part of it could not count, select over, or retry
// the run it is looking at.
func (r *pgRepo) ListTasks(ctx context.Context, tenantID, runID uuid.UUID) ([]Task, error) {
	var out []Task
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListUpdateTasksForRunWithSiteName(ctx, sqlc.ListUpdateTasksForRunWithSiteNameParams{RunID: runID, TenantID: tenantID})
		if err != nil {
			return domain.Internal("update_task_list_failed", "failed to list update tasks").WithCause(err)
		}
		out = make([]Task, 0, len(rows))
		for _, row := range rows {
			out = append(out, toTaskWithSiteName(row))
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) GetTask(ctx context.Context, tenantID, taskID uuid.UUID) (Task, error) {
	var out Task
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetUpdateTask(ctx, sqlc.GetUpdateTaskParams{ID: taskID, TenantID: tenantID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("update_task_not_found", "update task not found")
			}
			return domain.Internal("update_task_get_failed", "failed to load update task").WithCause(err)
		}
		out = toTask(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) MarkTaskRunning(ctx context.Context, tenantID, taskID uuid.UUID, staleAfter time.Duration) (Task, error) {
	// A non-positive bound is refused HERE, before the statement runs, which
	// is the only place that makes the failure unreachable by construction
	// rather than by convention.
	//
	// durationToInterval always sets Valid: true, so a zero Duration is not a
	// SQL NULL — it is interval '0', and
	// `coalesce(started_at, updated_at) < now() - '0'::interval` is true for
	// every non-agent 'running' row the instant it is written. That reopens
	// the double-dispatch race this whole path exists to close: it fails OPEN,
	// not closed. Refusing beats documenting, because the next caller reads
	// the signature, not this comment.
	if staleAfter <= 0 {
		return Task{}, domain.Internal("update_task_claim_bound_invalid",
			"failed to mark task running").WithCause(
			fmt.Errorf("claim staleness bound must be positive, got %v: a non-positive bound "+
				"makes every running row instantly reclaimable", staleAfter))
	}
	var out Task
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).MarkUpdateTaskRunning(ctx, sqlc.MarkUpdateTaskRunningParams{
			ID:         taskID,
			TenantID:   tenantID,
			StaleAfter: durationToInterval(staleAfter),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// The CAS matched nothing. NOT NotFound: the row is almost
				// certainly present and held by someone else.
				return ErrTaskNotClaimed
			}
			return domain.Internal("update_task_run_failed", "failed to mark task running").WithCause(err)
		}
		out = toTask(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) FinishTask(ctx context.Context, in FinishTaskInput) (Task, error) {
	var out Task
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		row, err := q.FinishUpdateTask(ctx, sqlc.FinishUpdateTaskParams{
			ID:          in.TaskID,
			TenantID:    in.TenantID,
			Status:      in.Status,
			FromVersion: in.FromVersion,
			ToVersion:   in.ToVersion,
			Detail:      in.Detail,
			Error:       in.Error,
		})
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return domain.Internal("update_task_finish_failed", "failed to finish task").WithCause(err)
			}
			// FinishUpdateTask only matches an OPEN task (pending|running), so
			// no row means either the task does not exist or it already
			// reached a terminal state, most importantly 'cancelled', written
			// by a halt while this worker was still in flight. Read the row
			// back to tell the two apart, and return the recorded outcome
			// rather than overwriting it.
			existing, gerr := q.GetUpdateTask(ctx, sqlc.GetUpdateTaskParams{ID: in.TaskID, TenantID: in.TenantID})
			if gerr != nil {
				if errors.Is(gerr, pgx.ErrNoRows) {
					return domain.NotFound("update_task_not_found", "update task not found")
				}
				return domain.Internal("update_task_get_failed", "failed to load update task").WithCause(gerr)
			}
			out = toTask(existing)
			return ErrTaskNotOpen
		}
		out = toTask(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) SetRunStatus(ctx context.Context, tenantID, runID uuid.UUID, status string) (Run, error) {
	var out Run
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).SetUpdateRunStatus(ctx, sqlc.SetUpdateRunStatusParams{ID: runID, TenantID: tenantID, Status: status})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("update_run_not_found", "update run not found")
			}
			return domain.Internal("update_run_status_failed", "failed to set run status").WithCause(err)
		}
		out = toRun(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) CountUnfinishedTasks(ctx context.Context, tenantID, runID uuid.UUID) (int64, error) {
	var n int64
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		c, err := sqlc.New(tx).CountUnfinishedTasksForRun(ctx, sqlc.CountUnfinishedTasksForRunParams{RunID: runID, TenantID: tenantID})
		if err != nil {
			return domain.Internal("update_count_failed", "failed to count unfinished tasks").WithCause(err)
		}
		n = c
		return nil
	})
	return n, err
}

func (r *pgRepo) CountRunningTasksForTenant(ctx context.Context, tenantID uuid.UUID) (int64, error) {
	var n int64
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		c, err := sqlc.New(tx).CountRunningTasksForTenant(ctx, tenantID)
		if err != nil {
			return domain.Internal("update_count_running_failed", "failed to count running tasks").WithCause(err)
		}
		n = c
		return nil
	})
	return n, err
}

func (r *pgRepo) ListInFlightTargets(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID) (map[InFlightKey]struct{}, error) {
	out := map[InFlightKey]struct{}{}
	if len(siteIDs) == 0 {
		return out, nil
	}
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListInFlightUpdateTargets(ctx, sqlc.ListInFlightUpdateTargetsParams{
			TenantID: tenantID,
			SiteIds:  siteIDs,
		})
		if err != nil {
			return domain.Internal("update_inflight_list_failed", "failed to list in-flight update targets").WithCause(err)
		}
		for _, row := range rows {
			out[InFlightKey{SiteID: row.SiteID, TargetType: row.TargetType, TargetSlug: row.TargetSlug}] = struct{}{}
		}
		return nil
	})
	return out, err
}

// ListStaleUpdateTasks runs cross-tenant (InAgentTx) since a stuck task can
// belong to any tenant and the reaper sweep must find all of them in one pass.
func (r *pgRepo) ListStaleUpdateTasks(ctx context.Context, threshold time.Duration, limit int32) ([]Task, error) {
	var out []Task
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListStaleUpdateTasks(ctx, sqlc.ListStaleUpdateTasksParams{
			Threshold: durationToInterval(threshold),
			RowLimit:  limit,
		})
		if err != nil {
			return domain.Internal("update_task_stale_list_failed", "failed to list stale update tasks").WithCause(err)
		}
		out = make([]Task, 0, len(rows))
		for _, row := range rows {
			out = append(out, toTask(row))
		}
		return nil
	})
	return out, err
}

// SiteHasRunningTask runs in the tenant's RLS scope (the caller already holds
// a tenant-scoped task, and a cross-tenant collision on the same site is not
// possible: site_id belongs to exactly one tenant).
func (r *pgRepo) SiteHasRunningTask(ctx context.Context, tenantID, siteID, excludeTaskID uuid.UUID, holdMax time.Duration) (bool, error) {
	var busy bool
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		b, err := sqlc.New(tx).SiteHasRunningUpdateTask(ctx, sqlc.SiteHasRunningUpdateTaskParams{
			SiteID:        siteID,
			TenantID:      tenantID,
			ExcludeTaskID: excludeTaskID,
			HoldMax:       durationToInterval(holdMax),
		})
		if err != nil {
			return domain.Internal("update_site_busy_check_failed", "failed to check for a running update task on this site").WithCause(err)
		}
		busy = b
		return nil
	})
	return busy, err
}

func (r *pgRepo) DeferTaskToPending(ctx context.Context, in DeferTaskInput) (Task, error) {
	var out Task
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).DeferUpdateTaskToPending(ctx, sqlc.DeferUpdateTaskToPendingParams{
			ID:       in.TaskID,
			TenantID: in.TenantID,
			Detail:   in.Detail,
		})
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return domain.Internal("update_task_defer_failed", "failed to defer update task to pending").WithCause(err)
			}
			// No open (pending|running) row matched: the task already reached
			// a terminal state, most likely 'cancelled' from a halt racing
			// this defer. Read it back and report ErrTaskNotOpen, exactly
			// like FinishTask, so the caller leaves the recorded outcome
			// alone rather than treating this as an infrastructure error.
			existing, gerr := sqlc.New(tx).GetUpdateTask(ctx, sqlc.GetUpdateTaskParams{ID: in.TaskID, TenantID: in.TenantID})
			if gerr != nil {
				if errors.Is(gerr, pgx.ErrNoRows) {
					return domain.NotFound("update_task_not_found", "update task not found")
				}
				return domain.Internal("update_task_get_failed", "failed to load update task").WithCause(gerr)
			}
			out = toTask(existing)
			return ErrTaskNotOpen
		}
		out = toTask(row)
		return nil
	})
	return out, err
}

// durationToInterval converts a time.Duration to a pgtype.Interval suitable
// for an @threshold::interval parameter (pgtype.Interval stores microseconds
// in the Microseconds field).
//
// Valid is ALWAYS true, so this never produces SQL NULL. A zero Duration
// becomes interval '0', not NULL — which for a `now() - $bound` staleness
// comparison means "everything is stale" rather than "nothing matches".
// Callers whose bound must be positive check it themselves before calling;
// see pgRepo.MarkTaskRunning.
func durationToInterval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}

func toRun(r sqlc.UpdateRun) Run {
	out := Run{
		ID:        r.ID,
		TenantID:  r.TenantID,
		Status:    r.Status,
		DryRun:    r.DryRun,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
	if r.CreatedBy.Valid {
		id := uuid.UUID(r.CreatedBy.Bytes)
		out.CreatedBy = &id
	}
	if r.ScheduledAt.Valid {
		t := r.ScheduledAt.Time
		out.ScheduledAt = &t
	}
	return out
}

func toTask(t sqlc.UpdateTask) Task {
	out := Task{
		ID:             t.ID,
		RunID:          t.RunID,
		TenantID:       t.TenantID,
		SiteID:         t.SiteID,
		TargetType:     t.TargetType,
		TargetSlug:     t.TargetSlug,
		DesiredVersion: t.DesiredVersion,
		FromVersion:    t.FromVersion,
		ToVersion:      t.ToVersion,
		Status:         t.Status,
		Detail:         t.Detail,
		Error:          t.Error,
		CreatedAt:      t.CreatedAt,
		UpdatedAt:      t.UpdatedAt,
	}
	if t.StartedAt.Valid {
		s := t.StartedAt.Time
		out.StartedAt = &s
	}
	if t.FinishedAt.Valid {
		f := t.FinishedAt.Time
		out.FinishedAt = &f
	}
	return out
}

// toTaskWithSiteName converts the detail-read row (task columns + the joined
// site name). Same mapping as toTask plus SiteName; kept as its own function
// because sqlc gives a joined query its own row type.
func toTaskWithSiteName(t sqlc.ListUpdateTasksForRunWithSiteNameRow) Task {
	out := Task{
		ID:             t.ID,
		RunID:          t.RunID,
		TenantID:       t.TenantID,
		SiteID:         t.SiteID,
		SiteName:       t.SiteName,
		TargetType:     t.TargetType,
		TargetSlug:     t.TargetSlug,
		DesiredVersion: t.DesiredVersion,
		FromVersion:    t.FromVersion,
		ToVersion:      t.ToVersion,
		Status:         t.Status,
		Detail:         t.Detail,
		Error:          t.Error,
		CreatedAt:      t.CreatedAt,
		UpdatedAt:      t.UpdatedAt,
	}
	if t.StartedAt.Valid {
		s := t.StartedAt.Time
		out.StartedAt = &s
	}
	if t.FinishedAt.Valid {
		f := t.FinishedAt.Time
		out.FinishedAt = &f
	}
	return out
}
