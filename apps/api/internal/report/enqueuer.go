package report

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// RiverEnqueuer enqueues GenerateArgs onto River. It satisfies Enqueuer.
type RiverEnqueuer struct {
	client *river.Client[pgx.Tx]
}

// NewRiverEnqueuer builds a RiverEnqueuer around the started River client.
func NewRiverEnqueuer(client *river.Client[pgx.Tx]) *RiverEnqueuer {
	return &RiverEnqueuer{client: client}
}

// EnqueueGenerate inserts one report_generate job.
func (e *RiverEnqueuer) EnqueueGenerate(ctx context.Context, args GenerateArgs) error {
	if _, err := e.client.Insert(ctx, args, nil); err != nil {
		return fmt.Errorf("enqueue report_generate: %w", err)
	}
	return nil
}

// activeGenerateJobStates are the River states of a job that has not finished:
// waiting to be fetched (available, or scheduled for a later time), parked
// (pending), waiting for a retry (retryable) or running. In any of them the job
// can still complete or fail its report. A completed, cancelled or discarded job
// never touches the report again.
var activeGenerateJobStates = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRetryable,
	rivertype.JobStateRunning,
	rivertype.JobStateScheduled,
}

// generateJobFilter is the part of a stored GenerateArgs that names one
// report's job: the same two keys GenerateArgs encodes them under (a test holds
// the two together), matched by JSON containment, so the rest of the job's args
// do not have to be known.
type generateJobFilter struct {
	TenantID uuid.UUID `json:"tenant_id"`
	ReportID uuid.UUID `json:"report_id"`
}

// HasActiveGenerateJob asks River whether a report_generate job for the report
// has not finished. River does the listing, so its table layout and schema stay
// its own. The job's tenant is part of the match, not only the report's id.
func (e *RiverEnqueuer) HasActiveGenerateJob(ctx context.Context, tenantID, reportID uuid.UUID) (bool, error) {
	filter, err := json.Marshal(generateJobFilter{TenantID: tenantID, ReportID: reportID})
	if err != nil {
		return false, fmt.Errorf("encode report_generate job filter: %w", err)
	}
	res, err := e.client.JobList(ctx, river.NewJobListParams().
		Kinds(GenerateArgs{}.Kind()).
		States(activeGenerateJobStates...).
		Where("args @> @args_filter::jsonb", river.NamedArgs{"args_filter": string(filter)}).
		First(1))
	if err != nil {
		return false, fmt.Errorf("list report_generate jobs: %w", err)
	}
	return len(res.Jobs) > 0, nil
}
