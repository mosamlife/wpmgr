package update

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// RiverEnqueuer enqueues update-task jobs onto River. It satisfies the
// service's Enqueuer interface.
type RiverEnqueuer struct {
	client *river.Client[pgx.Tx]
}

// NewRiverEnqueuer builds an enqueuer around the River client.
func NewRiverEnqueuer(client *river.Client[pgx.Tx]) *RiverEnqueuer {
	return &RiverEnqueuer{client: client}
}

// EnqueueTask inserts one update-task job. The job's InsertOpts pin it to the
// tenant's queue shard (per-tenant parallelism). dryRun is carried in the args.
func (e *RiverEnqueuer) EnqueueTask(ctx context.Context, tenantID, runID, taskID uuid.UUID, dryRun bool) error {
	args := TaskArgs{TenantID: tenantID, RunID: runID, TaskID: taskID, DryRun: dryRun}
	if _, err := e.client.Insert(ctx, args, nil); err != nil {
		return fmt.Errorf("enqueue update task: %w", err)
	}
	return nil
}

// EnqueueRefresh inserts one refresh-inventory job. The job's InsertOpts pin it
// to the tenant's queue shard. Satisfies RefreshEnqueuer.
func (e *RiverEnqueuer) EnqueueRefresh(ctx context.Context, args RefreshInventoryArgs) error {
	if _, err := e.client.Insert(ctx, args, nil); err != nil {
		return fmt.Errorf("enqueue refresh inventory: %w", err)
	}
	return nil
}
