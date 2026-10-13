package abilityrequest

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

const contentRetentionBatchSize int32 = 500
const contentRetentionMaxBatches = 100

type ContentRetentionArgs struct{}

func (ContentRetentionArgs) Kind() string { return "ability_request_content_retention" }
func (ContentRetentionArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByState: []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning,
		rivertype.JobStateRetryable, rivertype.JobStateScheduled,
	}}}
}

type contentRetentionQueries interface {
	ClearAbilityRequestContent(context.Context, int32) (sqlc.ClearAbilityRequestContentRow, error)
}

// ContentRetentionWorker clears bounded batches and resumes when its work
// budget is reached. Each database call commits independently.
type ContentRetentionWorker struct {
	river.WorkerDefaults[ContentRetentionArgs]
	queries contentRetentionQueries
	logger  *slog.Logger
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
	var total, failed int64
	defer func() {
		if w.logger != nil {
			w.logger.InfoContext(ctx, "ability request content retention", "cleared", total, "failed", failed)
		}
	}()
	for batch := 0; batch < contentRetentionMaxBatches; batch++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		counts, err := w.queries.ClearAbilityRequestContent(ctx, contentRetentionBatchSize)
		// Database diagnostics can contain row values; the job records no content.
		if err != nil {
			return errors.New("ability request content retention batch failed")
		}
		if counts.Cleared < 0 || counts.Failed < 0 || int64(counts.Cleared)+int64(counts.Failed) > int64(contentRetentionBatchSize) {
			return errors.New("invalid ability request content retention count")
		}
		total += int64(counts.Cleared)
		failed += int64(counts.Failed)
		if counts.Failed > 0 {
			return errors.New("ability request content retention rows failed")
		}
		if counts.Cleared < contentRetentionBatchSize {
			return nil
		}
	}
	return river.JobSnooze(time.Minute)
}
