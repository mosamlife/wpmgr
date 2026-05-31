package repo

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/model"
)

const variantCols = `id, job_id, tenant_id, variant_name, source_size_bytes,
	optimized_size_bytes, source_mime, optimized_mime, encode_ms, state, reason, created_at`

// UpsertVariantInput is one variant result written by the encode worker.
type UpsertVariantInput struct {
	JobID              string
	VariantName        string
	SourceSizeBytes    int64
	OptimizedSizeBytes *int64
	SourceMime         string
	OptimizedMime      string
	EncodeMS           *int
	State              model.VariantState
	Reason             string
}

// UpsertVariantAgent records (or replaces) a variant result under the agent GUC.
// The encode worker (media-encoder) runs cross-tenant under app.agent; ON
// CONFLICT keeps the latest attempt so a River retry re-records cleanly.
func (r *Repo) UpsertVariantAgent(ctx context.Context, tenantID uuid.UUID, in UpsertVariantInput) error {
	return r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO media_variant_results
				(id, job_id, tenant_id, variant_name, source_size_bytes,
				 optimized_size_bytes, source_mime, optimized_mime, encode_ms,
				 state, reason, created_at)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())`,
			in.JobID, tenantID, in.VariantName, in.SourceSizeBytes,
			in.OptimizedSizeBytes, in.SourceMime, nilIfEmpty(in.OptimizedMime),
			in.EncodeMS, in.State, nilIfEmpty(in.Reason))
		if err != nil {
			return domain.Internal("media_variant_insert_failed", "failed to insert variant result").WithCause(err)
		}
		return nil
	})
}

// ListVariantsForJob returns all variant results for a job (tenant-scoped,
// operator path).
func (r *Repo) ListVariantsForJob(ctx context.Context, tenantID uuid.UUID, jobID string) ([]model.VariantResult, error) {
	var out []model.VariantResult
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+variantCols+`
			 FROM media_variant_results
			 WHERE tenant_id = $1 AND job_id = $2
			 ORDER BY created_at ASC`,
			tenantID, jobID)
		if err != nil {
			return domain.Internal("media_variants_list_failed", "failed to list variant results").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			v, err := scanVariant(rows)
			if err != nil {
				return domain.Internal("media_variants_list_failed", "failed to read variant result").WithCause(err)
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, err
}

// CountVariantStatesAgent returns (succeeded, failed) counts for a job under the
// agent GUC. The encode worker uses it to decide the terminal job state.
func (r *Repo) CountVariantStatesAgent(ctx context.Context, jobID string) (succeeded, failed int, err error) {
	err = r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT
			    count(*) FILTER (WHERE state = 'succeeded'),
			    count(*) FILTER (WHERE state = 'failed')
			 FROM media_variant_results WHERE job_id = $1`,
			jobID)
		return row.Scan(&succeeded, &failed)
	})
	return succeeded, failed, err
}

func scanVariant(row pgx.Row) (model.VariantResult, error) {
	var v model.VariantResult
	var optSize *int64
	var optMime, reason *string
	var encodeMS *int
	if err := row.Scan(
		&v.ID, &v.JobID, &v.TenantID, &v.VariantName, &v.SourceSizeBytes,
		&optSize, &v.SourceMime, &optMime, &encodeMS, &v.State, &reason, &v.CreatedAt,
	); err != nil {
		return model.VariantResult{}, err
	}
	v.OptimizedSizeBytes = optSize
	v.EncodeMS = encodeMS
	if optMime != nil {
		v.OptimizedMime = *optMime
	}
	if reason != nil {
		v.Reason = *reason
	}
	return v, nil
}
