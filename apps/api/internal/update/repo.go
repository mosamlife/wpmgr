package update

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Repo is the tenant-scoped persistence interface for update runs/tasks. Every
// method runs inside a tenant-scoped transaction so RLS enforces isolation even
// if a query omitted its tenant filter.
type Repo interface {
	// CreateRunWithTasks atomically creates a run and its tasks in one tx.
	CreateRunWithTasks(ctx context.Context, in CreateRunInput, tasks []NewTask) (Run, []Task, error)
	GetRun(ctx context.Context, tenantID, runID uuid.UUID) (Run, error)
	ListRuns(ctx context.Context, tenantID uuid.UUID, limit, offset int32) ([]Run, error)
	ListTasks(ctx context.Context, tenantID, runID uuid.UUID) ([]Task, error)
	GetTask(ctx context.Context, tenantID, taskID uuid.UUID) (Task, error)

	MarkTaskRunning(ctx context.Context, tenantID, taskID uuid.UUID) (Task, error)
	FinishTask(ctx context.Context, in FinishTaskInput) (Task, error)
	SetRunStatus(ctx context.Context, tenantID, runID uuid.UUID, status string) (Run, error)
	CountUnfinishedTasks(ctx context.Context, tenantID, runID uuid.UUID) (int64, error)
	CountRunningTasksForTenant(ctx context.Context, tenantID uuid.UUID) (int64, error)
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
				return domain.Internal("update_task_create_failed", "failed to create update task").WithCause(err)
			}
			outTasks = append(outTasks, toTask(taskRow))
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
		row, err := sqlc.New(tx).FinishUpdateTask(ctx, sqlc.FinishUpdateTaskParams{
			ID:          in.TaskID,
			TenantID:    in.TenantID,
			Status:      in.Status,
			FromVersion: in.FromVersion,
			ToVersion:   in.ToVersion,
			Detail:      in.Detail,
			Error:       in.Error,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("update_task_not_found", "update task not found")
			}
			return domain.Internal("update_task_finish_failed", "failed to finish task").WithCause(err)
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
