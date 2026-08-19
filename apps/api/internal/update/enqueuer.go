package update

import (
	"context"
	"fmt"
	"time"

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

// EnqueueTaskTx inserts one update-task job INSIDE an existing transaction, so
// the job becomes visible if and only if that transaction commits. Satisfies
// TxEnqueuer.
//
// This is what makes deferred dispatch atomic. pgRepo.DispatchDueRun claims the
// run into the transient 'dispatching' status, moves each task to 'pending' and
// enqueues, all in one transaction: a crash anywhere rolls the claim back and
// the next tick finds the run still 'scheduled'. Using the non-transactional
// Insert here would commit jobs for a claim that then rolled back, and — worse
// in the other direction — commit a claim whose jobs were never inserted, which
// strands the run permanently ('dispatching' is outside update_runs_due_idx, so
// nothing ever scans it again).
func (e *RiverEnqueuer) EnqueueTaskTx(ctx context.Context, tx pgx.Tx, tenantID, runID, taskID uuid.UUID, dryRun bool) error {
	args := TaskArgs{TenantID: tenantID, RunID: runID, TaskID: taskID, DryRun: dryRun}
	if _, err := e.client.InsertTx(ctx, tx, args, nil); err != nil {
		return fmt.Errorf("enqueue update task in tx: %w", err)
	}
	return nil
}

// EnqueueDispatch inserts the one-shot job that fires a deferred run, using
// River's NATIVE scheduled insert (InsertOpts.ScheduledAt) rather than a
// polling loop. Satisfies DispatchEnqueuer.
//
// River holds the job in 'scheduled' and makes it available at `at`, which is
// exactly the semantics wanted and is why no new timer machinery exists here.
// The job is still only a TRIGGER: DispatchWorker re-reads the run, so a job
// that fires twice, late, or for a cancelled run loses harmlessly against the
// CAS statements.
//
// The job's own retention against a 30-day-out schedule is reasoned but not
// measured, which is what the #463 Phase 2 safety-net sweeper exists to cover:
// a run whose queue insert was lost or reaped is still 'scheduled' and still
// due-scannable, so the sweeper finds it. Until that lands, this insert is the
// only thing that fires a run.
func (e *RiverEnqueuer) EnqueueDispatch(ctx context.Context, tenantID, runID uuid.UUID, at time.Time) error {
	args := DispatchRunArgs{TenantID: tenantID, RunID: runID}
	opts := &river.InsertOpts{Queue: QueueForTenant(tenantID), ScheduledAt: at}
	if _, err := e.client.Insert(ctx, args, opts); err != nil {
		return fmt.Errorf("enqueue scheduled update run dispatch: %w", err)
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

// EnqueueAgentConfirm inserts one agent self-update confirmation-poll job
// (beat 2). The job's InsertOpts pin it to the tenant's queue shard. Satisfies
// AgentConfirmEnqueuer.
func (e *RiverEnqueuer) EnqueueAgentConfirm(ctx context.Context, args AgentConfirmArgs) error {
	if _, err := e.client.Insert(ctx, args, nil); err != nil {
		return fmt.Errorf("enqueue agent self-update confirmation: %w", err)
	}
	return nil
}
