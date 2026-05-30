package backup

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

// ScheduleRunStore is the interface the service uses for backup_schedule_runs
// persistence. The concrete ScheduleRunRepo satisfies it; tests substitute a
// fake.
type ScheduleRunStore interface {
	// UpsertScheduleRun pre-inserts (or idempotently updates) a schedule run
	// row. ON CONFLICT (schedule_id, scheduled_for) only advances status when
	// the existing row is still 'scheduled', so a queued/running row is
	// untouched. Used both for the pre-inserted 'scheduled' sentinel and for
	// advancing to 'queued' at fire time.
	UpsertScheduleRun(ctx context.Context, in UpsertScheduleRunInput) (ScheduleRun, error)

	// SetScheduleRunSnapshot links a snapshot_id to the run and marks it
	// 'queued'. Tenant-scoped.
	SetScheduleRunSnapshot(ctx context.Context, tenantID, runID, snapshotID uuid.UUID) (ScheduleRun, error)

	// SetScheduleRunStatusByID advances a run to the given status by its
	// primary key. setStartedAt / setFinishedAt write the respective timestamp
	// once (only when true). Tenant-scoped. Guards against double-transition:
	// the DB query does not restrict terminal rows (the scheduler calls this
	// after reconciling the linked snapshot, not after the run itself).
	SetScheduleRunStatusByID(ctx context.Context, in SetScheduleRunStatusInput) (ScheduleRun, error)

	// SetScheduleRunStatusBySnapshot advances a run via its linked snapshot_id.
	// Called by the snapshot-finalize / reconciliation path which does not carry
	// the schedule run id. Tenant-scoped.
	SetScheduleRunStatusBySnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID, in SetScheduleRunStatusInput) (ScheduleRun, error)

	// GetScheduleRun fetches a single schedule_run row by id, tenant-scoped.
	GetScheduleRun(ctx context.Context, tenantID, runID uuid.UUID) (ScheduleRun, error)

	// ListScheduleRunsBySite returns all runs (upcoming + past) for a site,
	// newest scheduled_for first. Tenant-scoped.
	ListScheduleRunsBySite(ctx context.Context, tenantID, siteID uuid.UUID, limit, offset int32) ([]ScheduleRun, error)

	// ListUpcomingScheduleRuns returns non-terminal (scheduled/queued) runs
	// with scheduled_for in the future, ordered ASC. Tenant-scoped.
	ListUpcomingScheduleRuns(ctx context.Context, tenantID, siteID uuid.UUID, limit int32) ([]ScheduleRun, error)

	// ListPastScheduleRuns returns terminal (completed/failed/skipped/canceled)
	// runs, newest first. Tenant-scoped.
	ListPastScheduleRuns(ctx context.Context, tenantID, siteID uuid.UUID, limit, offset int32) ([]ScheduleRun, error)

	// AgentUpsertScheduleRun is the cross-tenant variant used by the scheduler
	// periodic job. It runs under app.agent='on' (same as ListDueSchedules)
	// because the scheduler writes across tenants before narrowing to a single
	// tenant for the snapshot creation step.
	AgentUpsertScheduleRun(ctx context.Context, in UpsertScheduleRunInput) (ScheduleRun, error)
}

// UpsertScheduleRunInput carries the parameters for inserting / updating a
// schedule run row.
type UpsertScheduleRunInput struct {
	TenantID     uuid.UUID
	SiteID       uuid.UUID
	ScheduleID   uuid.UUID
	ScheduledFor time.Time
	Status       string
	Kind         string
	TriggeredBy  *string
}

// SetScheduleRunStatusInput carries parameters for a status transition on a
// schedule run.
type SetScheduleRunStatusInput struct {
	TenantID    uuid.UUID
	RunID       uuid.UUID // required for ByID; ignored by BySnapshot
	Status      string
	Error       *string
	SetStarted  bool // set started_at = now()
	SetFinished bool // set finished_at = now()
}

// ScheduleRunRepo is the concrete pgx-backed implementation of ScheduleRunStore.
type ScheduleRunRepo struct {
	pool *db.Pool
}

// NewScheduleRunRepo wires a ScheduleRunRepo with the shared pgx pool.
func NewScheduleRunRepo(pool *db.Pool) *ScheduleRunRepo {
	return &ScheduleRunRepo{pool: pool}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// toScheduleRun converts a sqlc.BackupScheduleRun into the domain ScheduleRun.
func toScheduleRun(r sqlc.BackupScheduleRun) ScheduleRun {
	out := ScheduleRun{
		ID:           r.ID,
		TenantID:     r.TenantID,
		SiteID:       r.SiteID,
		ScheduleID:   r.ScheduleID,
		ScheduledFor: r.ScheduledFor,
		Status:       r.Status,
		Kind:         r.Kind,
		Error:        r.Error,
		TriggeredBy:  r.TriggeredBy,
		CreatedAt:    r.CreatedAt,
		UpdatedAt:    r.UpdatedAt,
	}
	if r.SnapshotID.Valid {
		id := uuid.UUID(r.SnapshotID.Bytes)
		out.SnapshotID = &id
	}
	if r.StartedAt.Valid {
		t := r.StartedAt.Time
		out.StartedAt = &t
	}
	if r.FinishedAt.Valid {
		t := r.FinishedAt.Time
		out.FinishedAt = &t
	}
	return out
}

// notFoundOrInternal wraps a pgx.ErrNoRows as a domain NotFound error; any
// other error becomes an Internal error with the given code.
func notFoundOrInternal(err error, notFoundCode, internalCode, msg string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NotFound(notFoundCode, msg)
	}
	return domain.Internal(internalCode, msg).WithCause(err)
}

// ---------------------------------------------------------------------------
// UpsertScheduleRun (tenant-scoped)
// ---------------------------------------------------------------------------

func (r *ScheduleRunRepo) UpsertScheduleRun(ctx context.Context, in UpsertScheduleRunInput) (ScheduleRun, error) {
	var out ScheduleRun
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).UpsertScheduleRun(ctx, sqlc.UpsertScheduleRunParams{
			TenantID:     in.TenantID,
			SiteID:       in.SiteID,
			ScheduleID:   in.ScheduleID,
			ScheduledFor: in.ScheduledFor,
			Status:       in.Status,
			Kind:         in.Kind,
			TriggeredBy:  in.TriggeredBy,
		})
		if err != nil {
			return domain.Internal("schedule_run_upsert_failed", "failed to upsert schedule run").WithCause(err)
		}
		out = toScheduleRun(row)
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// AgentUpsertScheduleRun (cross-tenant, agent context)
// ---------------------------------------------------------------------------

// AgentUpsertScheduleRun writes a schedule run under the agent RLS context
// (app.agent='on'), matching how ListDueSchedules reads across tenants in the
// scheduler periodic job. The FOR ALL agent policy on backup_schedule_runs
// permits both INSERT and UPDATE from the scheduler.
func (r *ScheduleRunRepo) AgentUpsertScheduleRun(ctx context.Context, in UpsertScheduleRunInput) (ScheduleRun, error) {
	var out ScheduleRun
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).UpsertScheduleRun(ctx, sqlc.UpsertScheduleRunParams{
			TenantID:     in.TenantID,
			SiteID:       in.SiteID,
			ScheduleID:   in.ScheduleID,
			ScheduledFor: in.ScheduledFor,
			Status:       in.Status,
			Kind:         in.Kind,
			TriggeredBy:  in.TriggeredBy,
		})
		if err != nil {
			return domain.Internal("schedule_run_agent_upsert_failed", "failed to upsert schedule run (agent)").WithCause(err)
		}
		out = toScheduleRun(row)
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// SetScheduleRunSnapshot
// ---------------------------------------------------------------------------

func (r *ScheduleRunRepo) SetScheduleRunSnapshot(ctx context.Context, tenantID, runID, snapshotID uuid.UUID) (ScheduleRun, error) {
	var out ScheduleRun
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).SetScheduleRunSnapshot(ctx, sqlc.SetScheduleRunSnapshotParams{
			ID:         runID,
			TenantID:   tenantID,
			SnapshotID: pgtype.UUID{Bytes: snapshotID, Valid: true},
		})
		if err != nil {
			return notFoundOrInternal(err,
				"schedule_run_not_found", "schedule_run_snapshot_failed",
				"failed to link snapshot to schedule run")
		}
		out = toScheduleRun(row)
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// SetScheduleRunStatusByID
// ---------------------------------------------------------------------------

func (r *ScheduleRunRepo) SetScheduleRunStatusByID(ctx context.Context, in SetScheduleRunStatusInput) (ScheduleRun, error) {
	var out ScheduleRun
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).SetScheduleRunStatusByID(ctx, sqlc.SetScheduleRunStatusByIDParams{
			ID:       in.RunID,
			TenantID: in.TenantID,
			Status:   in.Status,
			Error:    in.Error,
			Column5:  in.SetStarted,
			Column6:  in.SetFinished,
		})
		if err != nil {
			return notFoundOrInternal(err,
				"schedule_run_not_found", "schedule_run_status_failed",
				"failed to update schedule run status")
		}
		out = toScheduleRun(row)
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// SetScheduleRunStatusBySnapshot
// ---------------------------------------------------------------------------

func (r *ScheduleRunRepo) SetScheduleRunStatusBySnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID, in SetScheduleRunStatusInput) (ScheduleRun, error) {
	var out ScheduleRun
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).SetScheduleRunStatusBySnapshot(ctx, sqlc.SetScheduleRunStatusBySnapshotParams{
			SnapshotID: pgtype.UUID{Bytes: snapshotID, Valid: true},
			TenantID:   tenantID,
			Status:     in.Status,
			Error:      in.Error,
			Column5:    in.SetStarted,
			Column6:    in.SetFinished,
		})
		if err != nil {
			return notFoundOrInternal(err,
				"schedule_run_not_found", "schedule_run_status_by_snapshot_failed",
				"failed to update schedule run status by snapshot")
		}
		out = toScheduleRun(row)
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// GetScheduleRun
// ---------------------------------------------------------------------------

func (r *ScheduleRunRepo) GetScheduleRun(ctx context.Context, tenantID, runID uuid.UUID) (ScheduleRun, error) {
	var out ScheduleRun
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetScheduleRun(ctx, sqlc.GetScheduleRunParams{
			ID:       runID,
			TenantID: tenantID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("schedule_run_not_found", "schedule run not found")
			}
			return domain.Internal("schedule_run_get_failed", "failed to fetch schedule run").WithCause(err)
		}
		out = toScheduleRun(row)
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// ListScheduleRunsBySite
// ---------------------------------------------------------------------------

func (r *ScheduleRunRepo) ListScheduleRunsBySite(ctx context.Context, tenantID, siteID uuid.UUID, limit, offset int32) ([]ScheduleRun, error) {
	if limit <= 0 {
		limit = 50
	}
	var out []ScheduleRun
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListScheduleRunsBySite(ctx, sqlc.ListScheduleRunsBySiteParams{
			TenantID: tenantID,
			SiteID:   siteID,
			Limit:    limit,
			Offset:   offset,
		})
		if err != nil {
			return domain.Internal("schedule_runs_list_failed", "failed to list schedule runs").WithCause(err)
		}
		out = make([]ScheduleRun, 0, len(rows))
		for _, row := range rows {
			out = append(out, toScheduleRun(row))
		}
		return nil
	})
	if out == nil {
		out = []ScheduleRun{}
	}
	return out, err
}

// ---------------------------------------------------------------------------
// ListUpcomingScheduleRuns
// ---------------------------------------------------------------------------

func (r *ScheduleRunRepo) ListUpcomingScheduleRuns(ctx context.Context, tenantID, siteID uuid.UUID, limit int32) ([]ScheduleRun, error) {
	if limit <= 0 {
		limit = 5
	}
	var out []ScheduleRun
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListUpcomingScheduleRuns(ctx, sqlc.ListUpcomingScheduleRunsParams{
			TenantID: tenantID,
			SiteID:   siteID,
			Limit:    limit,
		})
		if err != nil {
			return domain.Internal("schedule_runs_upcoming_failed", "failed to list upcoming schedule runs").WithCause(err)
		}
		out = make([]ScheduleRun, 0, len(rows))
		for _, row := range rows {
			out = append(out, toScheduleRun(row))
		}
		return nil
	})
	if out == nil {
		out = []ScheduleRun{}
	}
	return out, err
}

// ---------------------------------------------------------------------------
// ListPastScheduleRuns
// ---------------------------------------------------------------------------

func (r *ScheduleRunRepo) ListPastScheduleRuns(ctx context.Context, tenantID, siteID uuid.UUID, limit, offset int32) ([]ScheduleRun, error) {
	if limit <= 0 {
		limit = 50
	}
	var out []ScheduleRun
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListPastScheduleRuns(ctx, sqlc.ListPastScheduleRunsParams{
			TenantID: tenantID,
			SiteID:   siteID,
			Limit:    limit,
			Offset:   offset,
		})
		if err != nil {
			return domain.Internal("schedule_runs_past_failed", "failed to list past schedule runs").WithCause(err)
		}
		out = make([]ScheduleRun, 0, len(rows))
		for _, row := range rows {
			out = append(out, toScheduleRun(row))
		}
		return nil
	})
	if out == nil {
		out = []ScheduleRun{}
	}
	return out, err
}
