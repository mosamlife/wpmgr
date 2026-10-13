package abilityrequest

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

const contentRetentionBatchSize int32 = 500

type ContentRetentionArgs struct{}

func (ContentRetentionArgs) Kind() string { return "ability_request_content_retention" }
func (ContentRetentionArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByState: []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning,
		rivertype.JobStateRetryable, rivertype.JobStateScheduled,
	}}}
}

type contentRetentionQueries interface {
	ClearAbilityRequestContent(context.Context, sqlc.ClearAbilityRequestContentParams) (sqlc.ClearAbilityRequestContentRow, error)
}

// ContentRetentionWorker visits the backlog in bounded batches within its
// timeout. Each call commits independently; every run starts at the beginning.
type ContentRetentionWorker struct {
	river.WorkerDefaults[ContentRetentionArgs]
	// BatchSize defaults to contentRetentionBatchSize when zero.
	BatchSize int32
	queries   contentRetentionQueries
	logger    *slog.Logger
}

func NewContentRetentionWorker(pool *db.Pool, logger *slog.Logger) *ContentRetentionWorker {
	return &ContentRetentionWorker{queries: sqlc.New(pool.Pool), logger: logger}
}

func NewContentRetentionPeriodicJob() *river.PeriodicJob {
	return river.NewPeriodicJob(river.PeriodicInterval(time.Hour), func() (river.JobArgs, *river.InsertOpts) {
		return ContentRetentionArgs{}, nil
	}, &river.PeriodicJobOpts{RunOnStart: true})
}

func (w *ContentRetentionWorker) Timeout(*river.Job[ContentRetentionArgs]) time.Duration {
	return 5 * time.Minute
}

func (w *ContentRetentionWorker) Work(ctx context.Context, _ *river.Job[ContentRetentionArgs]) error {
	limit := w.BatchSize
	if limit == 0 {
		limit = contentRetentionBatchSize
	}
	if limit < 1 || limit > 1000 {
		return errors.New("invalid ability request content retention batch size")
	}
	params := sqlc.ClearAbilityRequestContentParams{BatchSize: limit}
	var total, failed int64
	defer func() {
		if w.logger != nil {
			w.logger.InfoContext(ctx, "ability request content retention", "cleared", total, "failed", failed)
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		counts, err := w.queries.ClearAbilityRequestContent(ctx, params)
		// Database diagnostics can contain row values; the job records no content.
		if err != nil {
			return errors.New("ability request content retention batch failed")
		}
		processed := int64(counts.Cleared) + int64(counts.Failed)
		if counts.Cleared < 0 || counts.Failed < 0 || counts.Examined < 0 || processed > int64(limit) || processed > int64(counts.Examined) {
			return errors.New("invalid ability request content retention count")
		}
		if counts.Examined > 0 && params.AfterFinished.Valid &&
			(counts.AfterFinished.Before(params.AfterFinished.Time) ||
				(counts.AfterFinished.Equal(params.AfterFinished.Time) && bytes.Compare(counts.AfterID[:], params.AfterID.Bytes[:]) <= 0)) {
			return errors.New("invalid ability request content retention cursor")
		}
		total += int64(counts.Cleared)
		failed += int64(counts.Failed)
		if counts.Examined < limit {
			break
		}
		params.AfterFinished = pgtype.Timestamptz{Time: counts.AfterFinished, Valid: true}
		params.AfterID = pgtype.UUID{Bytes: counts.AfterID, Valid: true}
	}
	if failed > 0 {
		return errors.New("ability request content retention rows failed")
	}
	return nil
}
