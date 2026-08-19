package update

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// CancelScheduledRun is the operator's call-back of a run that has NOT yet
// fired: the run reaches its terminal status and every still-scheduled task
// becomes 'cancelled', IN ONE TRANSACTION.
//
// FROM 'scheduled' ONLY, and that is the safety property rather than a
// convenience. It guarantees the cancel can never race a dispatch into a
// half-cancelled state: once the dispatcher has claimed the run
// ('dispatching') or the work is out ('running'), the CAS matches nothing and
// the operator is told to use the halt path instead — a different operation
// with different consequences, because halting a running run leaves commands
// already in flight on real sites. Cancelling a scheduled run promises that
// NOTHING WAS EVER SENT TO ANY SITE, and this precondition is what makes that
// promise true rather than merely likely.
//
// Tasks become TaskCancelled and NOT TaskExpired. Both record that the task was
// never attempted; they differ in what the operator should be told, and here
// somebody made a decision. Expiry is the other caller of the same statement
// and passes the other status, which is exactly why it takes one.
//
// The run's own status is RunHalted, because the run vocabulary has no
// 'cancelled' and minting a seventh run status by literal here would create one
// no existing reader knows how to render. What distinguishes this from an
// agent-rollout halt is the TASK statuses underneath: uniformly 'cancelled'
// with nothing ever dispatched, versus a halt's mixture of real outcomes.
//
// Returns cancelled=false when the run was not cancellable — it left
// 'scheduled' in the gap since the caller read it. That is "TOO LATE", not an
// error: the caller reports a conflict rather than claiming a cancellation that
// did not happen, and must NEVER fall back to SetUpdateRunStatus to force it.
func (r *pgRepo) CancelScheduledRun(ctx context.Context, tenantID, runID uuid.UUID, detail string) (Run, int, bool, error) {
	var out Run
	var tasks int
	var cancelled bool

	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)

		runRow, err := q.CancelScheduledUpdateRun(ctx, sqlc.CancelScheduledUpdateRunParams{
			Status:   RunHalted,
			ID:       runID,
			TenantID: tenantID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Too late, or already gone. Benign; cancelled stays false and
				// the service turns it into a conflict for the operator.
				return nil
			}
			return domain.Internal("update_run_cancel_failed", "failed to cancel the scheduled update run").WithCause(err)
		}
		cancelled = true
		out = toRun(runRow)

		// SAME TRANSACTION, non-negotiable. A run that reaches a terminal state
		// while its tasks are still 'scheduled' strands them permanently:
		// 'scheduled' is not terminal so no outcome is ever recorded, it is
		// outside ListStaleUpdateTasks so the reaper will never un-stick them,
		// and the run has left update_runs_due_idx so no future tick looks at
		// it again. Nothing in this system would find them.
		rows, err := q.TerminalizeScheduledTasksForRun(ctx, sqlc.TerminalizeScheduledTasksForRunParams{
			Status:   TaskCancelled,
			Detail:   detail,
			RunID:    runID,
			TenantID: tenantID,
		})
		if err != nil {
			return domain.Internal("update_tasks_cancel_failed", "failed to cancel the scheduled update tasks").WithCause(err)
		}
		tasks = len(rows)
		return nil
	})
	return out, tasks, cancelled, err
}
