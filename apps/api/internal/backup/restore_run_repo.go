package backup

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// RestoreRunRepo is the persistence layer for restore_runs and
// restore_run_events. It satisfies the RestoreRunStore interface used by the
// service so tests can substitute a fake without a live DB.
type RestoreRunRepo struct {
	pool *db.Pool
}

// NewRestoreRunRepo wires a RestoreRunRepo with the shared pgx pool.
func NewRestoreRunRepo(pool *db.Pool) *RestoreRunRepo {
	return &RestoreRunRepo{pool: pool}
}

// RestoreRunStore is the interface the service uses; the concrete
// RestoreRunRepo satisfies it.
type RestoreRunStore interface {
	// CreateRestoreRun inserts a new restore_runs row in the queued state.
	CreateRestoreRun(ctx context.Context, in CreateRestoreRunInput) (RestoreRun, error)
	// GetRestoreRun fetches a single restore_run row by id, tenant-scoped (RLS).
	GetRestoreRun(ctx context.Context, tenantID, runID uuid.UUID) (RestoreRun, error)
	// ListRestoreRunsBySite returns restore runs for the site newest first.
	ListRestoreRunsBySite(ctx context.Context, tenantID, siteID uuid.UUID, limit int) ([]RestoreRun, error)
	// ActiveRestoreRunForSnapshot returns the most-recent queued or running
	// restore run for the given snapshot (status IN (queued,running) ORDER BY
	// created_at DESC LIMIT 1). Returns domain.ErrNotFound when none exists.
	ActiveRestoreRunForSnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID) (RestoreRun, error)
	// AppendRestoreEvent inserts one restore_run_events row.
	AppendRestoreEvent(ctx context.Context, in AppendRestoreEventInput) (RestoreRunEvent, error)
	// UpdateRestoreRunPhase sets current_phase + updated_at on a restore run.
	UpdateRestoreRunPhase(ctx context.Context, tenantID, runID uuid.UUID, phase string) error
	// MarkRestoreRunStatus transitions the run to the given status; when
	// terminal it also sets finished_at. When started=true it sets started_at.
	MarkRestoreRunStatus(ctx context.Context, in MarkRestoreRunStatusInput) error
	// ListRestoreEvents returns restore_run_events for a run ordered by id ASC.
	// If afterID > 0 only rows with id > afterID are returned (incremental).
	ListRestoreEvents(ctx context.Context, tenantID, runID uuid.UUID, afterID int64, limit int) ([]RestoreRunEvent, error)
}

// CreateRestoreRunInput carries the parameters for inserting a new restore run.
type CreateRestoreRunInput struct {
	TenantID    uuid.UUID
	SiteID      uuid.UUID
	SnapshotID  uuid.UUID
	Mode        string
	Components  []string
	Selection   []byte // JSON-encoded RestoreSelection
	TriggeredBy string
}

// AppendRestoreEventInput carries the parameters for one restore phase event.
type AppendRestoreEventInput struct {
	TenantID     uuid.UUID
	RestoreRunID uuid.UUID
	Phase        string
	Status       string
	Message      string
	Detail       []byte // JSON-encoded phase_detail
}

// MarkRestoreRunStatusInput carries the parameters for a status transition.
type MarkRestoreRunStatusInput struct {
	TenantID    uuid.UUID
	RunID       uuid.UUID
	Status      string
	Error       string // non-empty only on failed
	SetStarted  bool   // set started_at = now()
	SetFinished bool   // set finished_at = now()
}

// ---------------------------------------------------------------------------
// SQL helpers
// ---------------------------------------------------------------------------

const restoreRunCols = `id, tenant_id, site_id, snapshot_id, mode, components, selection,
	status, current_phase, error, triggered_by, created_at, started_at, finished_at, updated_at`

func scanRestoreRun(row pgx.Row) (RestoreRun, error) {
	var r RestoreRun
	var components []string
	var selectionRaw []byte
	var currentPhase, errStr, triggeredBy *string
	var startedAt, finishedAt *time.Time

	if err := row.Scan(
		&r.ID, &r.TenantID, &r.SiteID, &r.SnapshotID,
		&r.Mode, &components, &selectionRaw,
		&r.Status, &currentPhase, &errStr, &triggeredBy,
		&r.CreatedAt, &startedAt, &finishedAt, &r.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RestoreRun{}, domain.NotFound("restore_run_not_found", "restore run not found")
		}
		return RestoreRun{}, err
	}
	r.Components = components
	r.Selection = selectionRaw
	if currentPhase != nil {
		r.CurrentPhase = *currentPhase
	}
	if errStr != nil {
		r.Error = *errStr
	}
	if triggeredBy != nil {
		r.TriggeredBy = *triggeredBy
	}
	r.StartedAt = startedAt
	r.FinishedAt = finishedAt
	return r, nil
}

// ---------------------------------------------------------------------------
// CreateRestoreRun
// ---------------------------------------------------------------------------

// CreateRestoreRun inserts a new restore_runs row in the queued state under
// the given tenant transaction (RLS ensures isolation).
func (r *RestoreRunRepo) CreateRestoreRun(ctx context.Context, in CreateRestoreRunInput) (RestoreRun, error) {
	components := in.Components
	if components == nil {
		components = []string{}
	}
	selection := in.Selection
	if len(selection) == 0 {
		selection = []byte("{}")
	}

	var run RestoreRun
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO restore_runs
				(id, tenant_id, site_id, snapshot_id, mode, components, selection,
				 status, triggered_by, created_at, updated_at)
			VALUES
				(gen_random_uuid(), $1, $2, $3, $4, $5, $6,
				 'queued', $7, now(), now())
			RETURNING `+restoreRunCols,
			in.TenantID, in.SiteID, in.SnapshotID,
			in.Mode, components, selection,
			in.TriggeredBy,
		)
		var err error
		run, err = scanRestoreRun(row)
		return err
	})
	return run, err
}

// ---------------------------------------------------------------------------
// GetRestoreRun
// ---------------------------------------------------------------------------

// GetRestoreRun fetches a single restore_run by id under the tenant RLS.
func (r *RestoreRunRepo) GetRestoreRun(ctx context.Context, tenantID, runID uuid.UUID) (RestoreRun, error) {
	var run RestoreRun
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT `+restoreRunCols+` FROM restore_runs WHERE id = $1`,
			runID,
		)
		var err error
		run, err = scanRestoreRun(row)
		return err
	})
	return run, err
}

// ---------------------------------------------------------------------------
// ListRestoreRunsBySite
// ---------------------------------------------------------------------------

// ListRestoreRunsBySite returns restore runs for a site, newest first.
func (r *RestoreRunRepo) ListRestoreRunsBySite(ctx context.Context, tenantID, siteID uuid.UUID, limit int) ([]RestoreRun, error) {
	if limit <= 0 {
		limit = 50
	}
	var runs []RestoreRun
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+restoreRunCols+`
			 FROM restore_runs
			 WHERE site_id = $1
			 ORDER BY created_at DESC
			 LIMIT $2`,
			siteID, limit,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			run, err := scanRestoreRun(rows)
			if err != nil {
				return err
			}
			runs = append(runs, run)
		}
		return rows.Err()
	})
	if runs == nil {
		runs = []RestoreRun{}
	}
	return runs, err
}

// ---------------------------------------------------------------------------
// ActiveRestoreRunForSnapshot
// ---------------------------------------------------------------------------

// ActiveRestoreRunForSnapshot returns the most-recent queued or running
// restore run for the snapshot. Returns domain.NotFound when none exists.
// This runs under the agent transaction (cross-tenant progress ingest) because
// RecordProgress is called by the agent handler which has already resolved the
// tenant. We use InTenantTx since we have the tenant from the agent identity.
func (r *RestoreRunRepo) ActiveRestoreRunForSnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID) (RestoreRun, error) {
	var run RestoreRun
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT `+restoreRunCols+`
			 FROM restore_runs
			 WHERE snapshot_id = $1
			   AND status IN ('queued', 'running')
			 ORDER BY created_at DESC
			 LIMIT 1`,
			snapshotID,
		)
		var err error
		run, err = scanRestoreRun(row)
		return err
	})
	return run, err
}

// ---------------------------------------------------------------------------
// AppendRestoreEvent
// ---------------------------------------------------------------------------

// AppendRestoreEvent inserts one restore_run_events row.
func (r *RestoreRunRepo) AppendRestoreEvent(ctx context.Context, in AppendRestoreEventInput) (RestoreRunEvent, error) {
	detail := in.Detail
	if len(detail) == 0 {
		detail = nil
	}

	var ev RestoreRunEvent
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO restore_run_events
				(tenant_id, restore_run_id, phase, status, message, detail, occurred_at)
			VALUES ($1, $2, $3, $4, $5, $6, now())
			RETURNING id, tenant_id, restore_run_id, phase, status, message, detail, occurred_at`,
			in.TenantID, in.RestoreRunID, in.Phase, in.Status, in.Message, detail,
		)
		var detailRaw []byte
		return row.Scan(
			&ev.ID, &ev.TenantID, &ev.RestoreRunID,
			&ev.Phase, &ev.Status, &ev.Message, &detailRaw, &ev.OccurredAt,
		)
	})
	if err != nil {
		return RestoreRunEvent{}, err
	}
	ev.Detail = detail
	return ev, nil
}

// ---------------------------------------------------------------------------
// UpdateRestoreRunPhase
// ---------------------------------------------------------------------------

// UpdateRestoreRunPhase sets current_phase and updated_at on the run.
func (r *RestoreRunRepo) UpdateRestoreRunPhase(ctx context.Context, tenantID, runID uuid.UUID, phase string) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE restore_runs
			 SET current_phase = $1, updated_at = now()
			 WHERE id = $2`,
			phase, runID,
		)
		return err
	})
}

// ---------------------------------------------------------------------------
// MarkRestoreRunStatus
// ---------------------------------------------------------------------------

// MarkRestoreRunStatus transitions the run status. Guards against double-
// transition: if the row is already in a terminal state this is a no-op
// (UPDATE WHERE status NOT IN terminal).
func (r *RestoreRunRepo) MarkRestoreRunStatus(ctx context.Context, in MarkRestoreRunStatusInput) error {
	return r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		var errVal *string
		if in.Error != "" {
			v := in.Error
			errVal = &v
		}

		// Build the SET clause dynamically.
		if in.SetStarted && in.SetFinished {
			_, err := tx.Exec(ctx, `
				UPDATE restore_runs
				SET status = $1, error = $2, started_at = now(), finished_at = now(), updated_at = now()
				WHERE id = $3
				  AND status NOT IN ('completed', 'failed', 'rolled_back')`,
				in.Status, errVal, in.RunID,
			)
			return err
		} else if in.SetStarted {
			_, err := tx.Exec(ctx, `
				UPDATE restore_runs
				SET status = $1, error = $2, started_at = now(), updated_at = now()
				WHERE id = $3
				  AND status NOT IN ('completed', 'failed', 'rolled_back')`,
				in.Status, errVal, in.RunID,
			)
			return err
		} else if in.SetFinished {
			_, err := tx.Exec(ctx, `
				UPDATE restore_runs
				SET status = $1, error = $2, finished_at = now(), updated_at = now()
				WHERE id = $3
				  AND status NOT IN ('completed', 'failed', 'rolled_back')`,
				in.Status, errVal, in.RunID,
			)
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE restore_runs
			SET status = $1, error = $2, updated_at = now()
			WHERE id = $3
			  AND status NOT IN ('completed', 'failed', 'rolled_back')`,
			in.Status, errVal, in.RunID,
		)
		return err
	})
}

// ---------------------------------------------------------------------------
// ListRestoreEvents
// ---------------------------------------------------------------------------

// ListRestoreEvents returns restore_run_events for a run ordered by id ASC.
// If afterID > 0 only rows with id > afterID are returned.
func (r *RestoreRunRepo) ListRestoreEvents(ctx context.Context, tenantID, runID uuid.UUID, afterID int64, limit int) ([]RestoreRunEvent, error) {
	if limit <= 0 {
		limit = 200
	}
	var events []RestoreRunEvent
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var rows pgx.Rows
		var err error
		if afterID > 0 {
			rows, err = tx.Query(ctx, `
				SELECT id, tenant_id, restore_run_id, phase, status, message, detail, occurred_at
				FROM restore_run_events
				WHERE restore_run_id = $1 AND id > $2
				ORDER BY id ASC
				LIMIT $3`,
				runID, afterID, limit,
			)
		} else {
			rows, err = tx.Query(ctx, `
				SELECT id, tenant_id, restore_run_id, phase, status, message, detail, occurred_at
				FROM restore_run_events
				WHERE restore_run_id = $1
				ORDER BY id ASC
				LIMIT $2`,
				runID, limit,
			)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ev RestoreRunEvent
			var detailRaw []byte
			if err := rows.Scan(
				&ev.ID, &ev.TenantID, &ev.RestoreRunID,
				&ev.Phase, &ev.Status, &ev.Message, &detailRaw, &ev.OccurredAt,
			); err != nil {
				return err
			}
			ev.Detail = detailRaw
			events = append(events, ev)
		}
		return rows.Err()
	})
	if events == nil {
		events = []RestoreRunEvent{}
	}
	return events, err
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// marshalSelection JSON-encodes a RestoreSelection for storage in the
// restore_runs.selection JSONB column. Returns "{}" on marshal failure.
func marshalSelection(sel RestoreSelection) []byte {
	type selectionJSON struct {
		Full         bool     `json:"full"`
		Paths        []string `json:"paths,omitempty"`
		DBTables     []string `json:"db_tables,omitempty"`
		Components   []string `json:"components,omitempty"`
		KeepOldFiles bool     `json:"keep_old_files"`
	}
	raw, err := json.Marshal(selectionJSON{
		Full:         sel.Full,
		Paths:        sel.Paths,
		DBTables:     sel.DBTables,
		Components:   sel.Components,
		KeepOldFiles: sel.KeepOldFiles,
	})
	if err != nil {
		return []byte("{}")
	}
	return raw
}
