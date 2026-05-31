package repo

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/model"
)

const jobCols = `id, tenant_id, site_id, asset_id, wp_attachment_id, kind,
	target_format, target_quality, state, bytes_before, bytes_after,
	variants_total, variants_succeeded, variants_failed, error_reason,
	initiator_user_id, created_at, started_at, completed_at`

func jobFromRow(row pgx.Row) (model.Job, error) {
	var j model.Job
	var assetID, initiator *uuid.UUID
	var targetFormat, targetQuality, errReason *string
	var bytesBefore, bytesAfter *int64
	var startedAt, completedAt *time.Time
	if err := row.Scan(
		&j.ID, &j.TenantID, &j.SiteID, &assetID, &j.WPAttachmentID, &j.Kind,
		&targetFormat, &targetQuality, &j.State, &bytesBefore, &bytesAfter,
		&j.VariantsTotal, &j.VariantsSucceeded, &j.VariantsFailed, &errReason,
		&initiator, &j.CreatedAt, &startedAt, &completedAt,
	); err != nil {
		return model.Job{}, err
	}
	j.AssetID = assetID
	j.InitiatorUserID = initiator
	if targetFormat != nil {
		j.TargetFormat = *targetFormat
	}
	if targetQuality != nil {
		j.TargetQuality = *targetQuality
	}
	if errReason != nil {
		j.ErrorReason = *errReason
	}
	j.BytesBefore = bytesBefore
	j.BytesAfter = bytesAfter
	j.StartedAt = startedAt
	j.CompletedAt = completedAt
	return j, nil
}

// InsertJobInput creates a queued media job row.
type InsertJobInput struct {
	ID              string // ULID
	SiteID          uuid.UUID
	AssetID         *uuid.UUID
	WPAttachmentID  int64
	Kind            model.JobKind
	TargetFormat    string
	TargetQuality   string
	InitiatorUserID *uuid.UUID
}

// InsertJob creates a queued media_optimization_jobs row (tenant-scoped).
func (r *Repo) InsertJob(ctx context.Context, tenantID uuid.UUID, in InsertJobInput) (model.Job, error) {
	var out model.Job
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`INSERT INTO media_optimization_jobs
				(id, tenant_id, site_id, asset_id, wp_attachment_id, kind,
				 target_format, target_quality, state, initiator_user_id, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'queued', $9, now())
			 RETURNING `+jobCols,
			in.ID, tenantID, in.SiteID, in.AssetID, in.WPAttachmentID, in.Kind,
			nilIfEmpty(in.TargetFormat), nilIfEmpty(in.TargetQuality), in.InitiatorUserID)
		j, err := jobFromRow(row)
		if err != nil {
			return domain.Internal("media_job_insert_failed", "failed to insert media job").WithCause(err)
		}
		out = j
		return nil
	})
	return out, err
}

// GetJob returns a single job by id (tenant-scoped, operator path).
func (r *Repo) GetJob(ctx context.Context, tenantID uuid.UUID, jobID string) (model.Job, error) {
	var out model.Job
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT `+jobCols+` FROM media_optimization_jobs WHERE tenant_id = $1 AND id = $2`,
			tenantID, jobID)
		j, err := jobFromRow(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("media_job_not_found", "media job not found")
		}
		if err != nil {
			return domain.Internal("media_job_get_failed", "failed to get media job").WithCause(err)
		}
		out = j
		return nil
	})
	return out, err
}

// GetJobAgent returns a single job by id under the agent GUC (callback path).
// The tenant/site are re-asserted against the verified identity by the caller.
func (r *Repo) GetJobAgent(ctx context.Context, jobID string) (model.Job, error) {
	var out model.Job
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT `+jobCols+` FROM media_optimization_jobs WHERE id = $1`, jobID)
		j, err := jobFromRow(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("media_job_not_found", "media job not found")
		}
		if err != nil {
			return domain.Internal("media_job_get_failed", "failed to get media job").WithCause(err)
		}
		out = j
		return nil
	})
	return out, err
}

// ListJobsInput is the cursor-paginated job list query.
type ListJobsInput struct {
	TenantID uuid.UUID
	SiteID   uuid.UUID
	Limit    int
	Cursor   string // last seen job id (ULID; lexically sortable)
	State    string // optional filter
}

// ListJobs returns a page of jobs ordered by created_at DESC, id DESC.
func (r *Repo) ListJobs(ctx context.Context, in ListJobsInput) ([]model.Job, string, error) {
	limit := in.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var out []model.Job
	var nextCursor string
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		args := []any{in.TenantID, in.SiteID}
		q := `SELECT ` + jobCols + `
			  FROM media_optimization_jobs
			  WHERE tenant_id = $1 AND site_id = $2`
		if in.State != "" {
			args = append(args, in.State)
			q += ` AND state = $` + strconv.Itoa(len(args))
		}
		if in.Cursor != "" {
			args = append(args, in.Cursor)
			q += ` AND created_at < (SELECT created_at FROM media_optimization_jobs WHERE id = $` +
				strconv.Itoa(len(args)) + `)`
		}
		args = append(args, limit+1)
		q += ` ORDER BY created_at DESC, id DESC LIMIT $` + strconv.Itoa(len(args))

		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return domain.Internal("media_jobs_list_failed", "failed to list media jobs").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			j, err := jobFromRow(rows)
			if err != nil {
				return domain.Internal("media_jobs_list_failed", "failed to read media job").WithCause(err)
			}
			out = append(out, j)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(out) > limit {
			nextCursor = out[limit-1].ID
			out = out[:limit]
		}
		return nil
	})
	return out, nextCursor, err
}

// MarkJobInProgressAgent transitions a queued job → in_progress and records the
// variant total. Runs under the agent GUC (encode-ready callback). Idempotent:
// re-running on an already-in-progress job is a no-op (only queued rows match).
func (r *Repo) MarkJobInProgressAgent(ctx context.Context, jobID string, variantsTotal int) error {
	return r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE media_optimization_jobs
			 SET state = 'in_progress', started_at = COALESCE(started_at, now()),
			     variants_total = $2
			 WHERE id = $1 AND state = 'queued'`,
			jobID, variantsTotal)
		if err != nil {
			return domain.Internal("media_job_inprogress_failed", "failed to mark job in progress").WithCause(err)
		}
		return nil
	})
}

// FinalizeJobInput is the terminal job outcome (set by the agent job-status
// callback or the encode worker's last-variant finalize).
type FinalizeJobInput struct {
	State             model.JobState
	BytesBefore       *int64
	BytesAfter        *int64
	VariantsSucceeded int
	VariantsFailed    int
	ErrorReason       string
}

// FinalizeJobAgent writes a terminal job state under the agent GUC. Idempotent:
// only non-terminal rows are updated, so a duplicate callback is a no-op.
func (r *Repo) FinalizeJobAgent(ctx context.Context, jobID string, in FinalizeJobInput) (model.Job, error) {
	var out model.Job
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`UPDATE media_optimization_jobs
			 SET state               = $2,
			     bytes_before        = COALESCE($3, bytes_before),
			     bytes_after         = COALESCE($4, bytes_after),
			     variants_succeeded  = $5,
			     variants_failed     = $6,
			     error_reason        = $7,
			     completed_at        = now()
			 WHERE id = $1
			   AND state NOT IN ('succeeded','partially_succeeded','failed','cancelled')
			 RETURNING `+jobCols,
			jobID, in.State, in.BytesBefore, in.BytesAfter,
			in.VariantsSucceeded, in.VariantsFailed, nilIfEmpty(in.ErrorReason))
		j, err := jobFromRow(row)
		if errors.Is(err, pgx.ErrNoRows) {
			// Already terminal — load the existing row so the caller can proceed.
			r2 := tx.QueryRow(ctx, `SELECT `+jobCols+` FROM media_optimization_jobs WHERE id = $1`, jobID)
			j2, e2 := jobFromRow(r2)
			if e2 != nil {
				return domain.NotFound("media_job_not_found", "media job not found")
			}
			out = j2
			return nil
		}
		if err != nil {
			return domain.Internal("media_job_finalize_failed", "failed to finalize media job").WithCause(err)
		}
		out = j
		return nil
	})
	return out, err
}

// CancelJobs cancels all non-terminal jobs for a site (tenant-scoped, operator
// path). Returns the cancelled count.
func (r *Repo) CancelJobs(ctx context.Context, tenantID, siteID uuid.UUID) (int64, error) {
	var cancelled int64
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE media_optimization_jobs
			 SET state = 'cancelled', completed_at = now()
			 WHERE tenant_id = $1 AND site_id = $2
			   AND state IN ('queued','in_progress')`,
			tenantID, siteID)
		if err != nil {
			return domain.Internal("media_jobs_cancel_failed", "failed to cancel media jobs").WithCause(err)
		}
		cancelled = tag.RowsAffected()
		return nil
	})
	return cancelled, err
}
