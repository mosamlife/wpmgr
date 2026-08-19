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
	if _, err := e.client.Insert(ctx, dispatchArgs(tenantID, runID), dispatchInsertOpts(tenantID, at)); err != nil {
		return fmt.Errorf("enqueue scheduled update run dispatch: %w", err)
	}
	return nil
}

// EnqueueDispatchIfAbsent inserts a dispatch job only if the run does not
// already have a live one, reporting whether it inserted. Satisfies
// SweepEnqueuer.
//
// THE IDEMPOTENCY IS RIVER'S, NOT A PRE-CHECK OF OURS. dispatchInsertOpts sets
// UniqueOpts{ByArgs: true} and DispatchRunArgs is exactly (tenant_id, run_id),
// so River refuses a second job for a run that already has one pending,
// scheduled, available, running or retryable — atomically, against every
// replica at once. A read-then-insert here would have a window between the two
// wide enough for a second sweeper to drive through.
//
// The false return is the HEALTHY steady state (the run's original job is still
// sitting there waiting for its time) and the caller must stay quiet about it.
// A true return means the safety net actually caught something.
func (e *RiverEnqueuer) EnqueueDispatchIfAbsent(ctx context.Context, tenantID, runID uuid.UUID, at time.Time) (bool, error) {
	res, err := e.client.Insert(ctx, dispatchArgs(tenantID, runID), dispatchInsertOpts(tenantID, at))
	if err != nil {
		return false, fmt.Errorf("sweep-enqueue update run dispatch: %w", err)
	}
	return !res.UniqueSkippedAsDuplicate, nil
}

func dispatchArgs(tenantID, runID uuid.UUID) DispatchRunArgs {
	return DispatchRunArgs{TenantID: tenantID, RunID: runID}
}

// dispatchInsertOpts is shared by the create-time enqueue and the sweeper so
// the two cannot disagree about the unique key. They MUST agree: the whole
// reason a sweeper insert is safe is that it collides with the create-time job
// for the same run, and a difference in these options anywhere would let both
// exist and double-dispatch the run.
func dispatchInsertOpts(tenantID uuid.UUID, at time.Time) *river.InsertOpts {
	return &river.InsertOpts{
		Queue:       QueueForTenant(tenantID),
		ScheduledAt: at,
		// ByArgs alone, deliberately — NOT ByPeriod. The unique key is the run
		// itself, and a run's identity does not expire on a schedule: a job for
		// this run is either live or it is not. Adding a period would let a
		// second job for the same run exist once the window rolled over, which
		// is the double-dispatch this is here to prevent.
		UniqueOpts: river.UniqueOpts{ByArgs: true},
	}
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
