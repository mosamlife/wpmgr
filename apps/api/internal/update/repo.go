package update

import (
	"context"
	"errors"
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

// Repo is the tenant-scoped persistence interface for update runs/tasks. Every
// method runs inside a tenant-scoped transaction so RLS enforces isolation even
// if a query omitted its tenant filter.
type Repo interface {
	// CreateRunWithTasks atomically creates a run and its tasks in one tx.
	CreateRunWithTasks(ctx context.Context, in CreateRunInput, tasks []NewTask) (Run, []Task, error)
	GetRun(ctx context.Context, tenantID, runID uuid.UUID) (Run, error)
	ListRuns(ctx context.Context, tenantID uuid.UUID, limit, offset int32) ([]Run, error)
	// ListRunSummaries returns runs with pre-computed task aggregate counts
	// (task_count, succeeded_count, failed_count, site_count) in a single query.
	// Used by the list endpoint to avoid N+1 per-run task fetches.
	ListRunSummaries(ctx context.Context, tenantID uuid.UUID, limit, offset int32) ([]RunSummary, error)
	ListTasks(ctx context.Context, tenantID, runID uuid.UUID) ([]Task, error)
	GetTask(ctx context.Context, tenantID, taskID uuid.UUID) (Task, error)

	MarkTaskRunning(ctx context.Context, tenantID, taskID uuid.UUID) (Task, error)
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

func (r *pgRepo) ListTasks(ctx context.Context, tenantID, runID uuid.UUID) ([]Task, error) {
	var out []Task
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListUpdateTasksForRun(ctx, sqlc.ListUpdateTasksForRunParams{RunID: runID, TenantID: tenantID})
		if err != nil {
			return domain.Internal("update_task_list_failed", "failed to list update tasks").WithCause(err)
		}
		out = make([]Task, 0, len(rows))
		for _, row := range rows {
			out = append(out, toTask(row))
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

func (r *pgRepo) MarkTaskRunning(ctx context.Context, tenantID, taskID uuid.UUID) (Task, error) {
	var out Task
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).MarkUpdateTaskRunning(ctx, sqlc.MarkUpdateTaskRunningParams{ID: taskID, TenantID: tenantID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("update_task_not_found", "update task not found")
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

// durationToInterval converts a time.Duration to a pgtype.Interval suitable
// for an @threshold::interval parameter (pgtype.Interval stores microseconds
// in the Microseconds field).
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
