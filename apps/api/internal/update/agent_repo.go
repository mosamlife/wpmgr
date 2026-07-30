package update

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// agentWaveLockKey is the advisory-lock key namespace serializing every write
// the agent self-update wave machine makes to ONE run. Both ClaimAgentWaveTask
// (which turns a pending task into a dispatched one) and EvaluateAgentRun
// (which halts the run and cancels what is left) take
// pg_advisory_xact_lock(hashtext('update_agent_wave'), hashtext(run_id)) as
// their first statement, so they mutually exclude each other per run and
// release on commit/rollback. This is the same discipline org lifecycle uses
// (see internal/org/delete_handler.go's orgLifecycleLockKey); no new locking
// mechanism is introduced here.
//
// The lock is what makes the derived wave state (agent_wave.go) safe to act
// on. Two workers finishing concurrently both compute the same verdict, but
// only one of them gets to perform the transition: the loser re-reads the rows
// the winner just wrote, inside its own lock, and sees that there is nothing
// left to do. That is why neither "both advance the wave" nor "advance a wave
// a halt just cancelled" is representable.
const agentWaveLockKey = "update_agent_wave"

// AgentWaveClaim is the outcome of asking to dispatch one agent self-update
// task.
type AgentWaveClaim int

const (
	// ClaimWait: an earlier wave has not finished proving itself. Nothing was
	// written; the caller must snooze and ask again.
	ClaimWait AgentWaveClaim = iota
	// ClaimProceed: THIS caller moved the task pending -> running and is the
	// only caller that will. It, and only it, may contact the site.
	ClaimProceed
	// ClaimHalted: the run is halted (already, or as of this call). The caller
	// must not contact the site. A task that was still pending has been
	// cancelled by the halt; one that was already running is left alone for its
	// own confirmation job to resolve (see haltLocked), and this claim is
	// simply a duplicate job arriving late.
	ClaimHalted
	// ClaimAlreadyClaimed: another job already took this task past pending, or
	// it is already terminal. Nothing was written and the caller must not
	// dispatch, this is the duplicate-enqueue no-op.
	ClaimAlreadyClaimed
)

// AgentRunEvaluation is the result of re-judging a run's wave gate after one
// of its tasks reached a terminal state.
type AgentRunEvaluation struct {
	// Halted is true when the run is halted, whether this call halted it or
	// found it already halted.
	Halted bool
	// Reason explains the halt in operator-readable terms; empty when !Halted.
	Reason string
	// Cancelled counts the tasks THIS call cancelled.
	Cancelled int
	// Changed is true only for the call that performed the halt transition, so
	// exactly one caller records the audit event and logs the incident.
	Changed bool
	// Dispatchable is the tasks of a wave that JUST opened: the set the caller
	// should enqueue now, and nothing it has already enqueued. Empty when
	// Halted, and empty on the transitions that open no new wave, which is
	// what keeps a fleet-wide rollout's enqueue work linear rather than
	// quadratic (see AgentWaveState.DispatchableTasks). Safe to enqueue
	// unconditionally: the claim path only ever hands one caller the
	// pending -> running transition.
	Dispatchable []Task
}

// AgentWaveRepo is the extra persistence the agent self-update wave machine
// needs, kept separate from Repo so the ordinary update path (and every fake
// that implements Repo) is untouched by it.
type AgentWaveRepo interface {
	// ClaimAgentWaveTask atomically decides whether one agent task may be
	// dispatched and, if so, claims it. Everything happens inside one
	// transaction under the per-run advisory lock.
	ClaimAgentWaveTask(ctx context.Context, tenantID, runID, taskID uuid.UUID) (AgentWaveClaim, Task, error)
	// EvaluateAgentRun re-judges the wave gate after a terminal transition,
	// halting the run (and cancelling every task that was never dispatched)
	// when a completed wave failed its gate. Idempotent: only the first caller
	// to halt a given run reports Changed.
	EvaluateAgentRun(ctx context.Context, tenantID, runID uuid.UUID) (AgentRunEvaluation, error)
	// HaltAgentRun halts a run for a reason the wave gate itself cannot see
	// (today: the fleet-wide kill switch), cancelling every task that was never
	// dispatched and leaving running tasks to their own confirmation job (see
	// haltLocked). Idempotent in the same way as EvaluateAgentRun.
	HaltAgentRun(ctx context.Context, tenantID, runID uuid.UUID, reason string) (AgentRunEvaluation, error)
}

// NewAgentWaveRepo builds the agent self-update wave repo, backed by the same
// pgx pool and the same tenant-scoped RLS transactions as NewRepo.
func NewAgentWaveRepo(pool *db.Pool) AgentWaveRepo { return &pgRepo{pool: pool} }

// lockAgentRun takes the transaction-scoped advisory lock for one run. It must
// be the first statement of any wave-machine transaction.
func lockAgentRun(ctx context.Context, tx pgx.Tx, runID uuid.UUID) error {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`,
		agentWaveLockKey, runID.String(),
	); err != nil {
		return domain.Internal("update_agent_wave_lock_failed", "failed to lock the update run's wave gate").WithCause(err)
	}
	return nil
}

// loadAgentRunLocked reads a run and its tasks inside an already-locked
// transaction.
func loadAgentRunLocked(ctx context.Context, q *sqlc.Queries, tenantID, runID uuid.UUID) (Run, []Task, error) {
	runRow, err := q.GetUpdateRun(ctx, sqlc.GetUpdateRunParams{ID: runID, TenantID: tenantID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Run{}, nil, domain.NotFound("update_run_not_found", "update run not found")
		}
		return Run{}, nil, domain.Internal("update_run_get_failed", "failed to load update run").WithCause(err)
	}
	taskRows, err := q.ListUpdateTasksForRun(ctx, sqlc.ListUpdateTasksForRunParams{RunID: runID, TenantID: tenantID})
	if err != nil {
		return Run{}, nil, domain.Internal("update_task_list_failed", "failed to list update tasks").WithCause(err)
	}
	tasks := make([]Task, 0, len(taskRows))
	for _, row := range taskRows {
		tasks = append(tasks, toTask(row))
	}
	return toRun(runRow), tasks, nil
}

func (r *pgRepo) ClaimAgentWaveTask(ctx context.Context, tenantID, runID, taskID uuid.UUID) (AgentWaveClaim, Task, error) {
	claim := ClaimWait
	var out Task
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if err := lockAgentRun(ctx, tx, runID); err != nil {
			return err
		}
		q := sqlc.New(tx)
		run, tasks, err := loadAgentRunLocked(ctx, q, tenantID, runID)
		if err != nil {
			return err
		}

		me, found := findTask(tasks, taskID)
		if !found {
			return domain.NotFound("update_task_not_found", "update task not found")
		}
		out = me

		// Already terminal (a duplicate job, or a halt that cancelled us
		// between enqueue and now): never dispatch.
		if terminal(me.Status) {
			claim = ClaimAlreadyClaimed
			return nil
		}

		state := DeriveAgentWaveState(tasks)

		// The gate is re-judged here, not just after terminal transitions, so
		// a halt is self-healing: even if the post-finish evaluation was lost
		// to a crash, the next task that asks to dispatch halts the run
		// instead of proceeding on a failed wave.
		if run.Status == RunHalted || state.Halt {
			reason := state.HaltReason
			if run.Status == RunHalted && reason == "" {
				reason = "the run was halted"
			}
			if _, herr := haltLocked(ctx, q, tenantID, run, tasks, reason); herr != nil {
				return herr
			}
			// Re-read our own row: haltLocked cancelled it if it was still
			// pending, and left it running if it had already been dispatched.
			cancelled, cerr := q.GetUpdateTask(ctx, sqlc.GetUpdateTaskParams{ID: taskID, TenantID: tenantID})
			if cerr != nil {
				return domain.Internal("update_task_get_failed", "failed to load update task").WithCause(cerr)
			}
			out = toTask(cancelled)
			claim = ClaimHalted
			return nil
		}

		// Someone else already took this task out of pending.
		if me.Status != TaskPending {
			claim = ClaimAlreadyClaimed
			return nil
		}

		if !state.GateOpenFor(taskID) {
			claim = ClaimWait
			return nil
		}

		row, err := q.MarkUpdateTaskRunning(ctx, sqlc.MarkUpdateTaskRunningParams{ID: taskID, TenantID: tenantID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("update_task_not_found", "update task not found")
			}
			return domain.Internal("update_task_run_failed", "failed to mark task running").WithCause(err)
		}
		out = toTask(row)
		claim = ClaimProceed
		return nil
	})
	if err != nil {
		return ClaimWait, Task{}, err
	}
	return claim, out, nil
}

func (r *pgRepo) EvaluateAgentRun(ctx context.Context, tenantID, runID uuid.UUID) (AgentRunEvaluation, error) {
	var out AgentRunEvaluation
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if err := lockAgentRun(ctx, tx, runID); err != nil {
			return err
		}
		q := sqlc.New(tx)
		run, tasks, err := loadAgentRunLocked(ctx, q, tenantID, runID)
		if err != nil {
			return err
		}

		state := DeriveAgentWaveState(tasks)
		if !state.Halt && run.Status != RunHalted {
			out.Dispatchable = state.DispatchableTasks()
			return nil
		}
		reason := state.HaltReason
		if reason == "" {
			reason = "the run was halted"
		}
		ev, herr := haltLocked(ctx, q, tenantID, run, tasks, reason)
		if herr != nil {
			return herr
		}
		out = ev
		return nil
	})
	return out, err
}

func (r *pgRepo) HaltAgentRun(ctx context.Context, tenantID, runID uuid.UUID, reason string) (AgentRunEvaluation, error) {
	var out AgentRunEvaluation
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if err := lockAgentRun(ctx, tx, runID); err != nil {
			return err
		}
		q := sqlc.New(tx)
		run, tasks, err := loadAgentRunLocked(ctx, q, tenantID, runID)
		if err != nil {
			return err
		}
		ev, herr := haltLocked(ctx, q, tenantID, run, tasks, reason)
		if herr != nil {
			return herr
		}
		out = ev
		return nil
	})
	return out, err
}

// haltLocked performs the halt inside an already-locked transaction: every
// task that was never dispatched is cancelled with the reason, and the run is
// marked halted.
//
// ONLY PENDING TASKS ARE CANCELLED. A running task has already had its command
// delivered and, for an agent self-update, the upgrade applied inline on the site.
// Cancelling it would record a falsehood, model.go defines TaskCancelled as
// "nothing was ever sent to this site", and, worse, blind the control plane:
// AgentConfirmWorker.Work short-circuits on a terminal status, so the confirm
// poll would return, see 'cancelled', and stop. The CP would never learn
// whether those sites upgraded or bricked, at the exact moment an operator hit
// the kill switch and most needs to know. So a running task is left to be
// resolved by its own confirmation job; the run is halted either way, so no
// further wave can open behind it.
//
// The cancel is atomic against a concurrent claim: CancelPendingUpdateTask
// carries the status='pending' precondition in SQL rather than trusting the
// snapshot read at the top of this transaction.
//
// The run status is (re-)written even when it already reads halted. That is
// deliberate: a task that was already dispatched when the halt landed will
// finish later, and Worker.finish's ordinary run-completion check can flip a
// halted run to completed. Since every agent task's terminal transition is
// followed by EvaluateAgentRun, which lands here, that drift is always
// corrected and the run's final recorded state is the honest one.
//
// Changed is true only when this call was the one that moved the run into the
// halted state, so exactly one caller records the audit event.
func haltLocked(ctx context.Context, q *sqlc.Queries, tenantID uuid.UUID, run Run, tasks []Task, reason string) (AgentRunEvaluation, error) {
	ev := AgentRunEvaluation{Halted: true, Reason: reason, Changed: run.Status != RunHalted}

	detail := "cancelled: " + reason
	for _, t := range tasks {
		if t.Status != TaskPending {
			continue
		}
		if _, err := q.CancelPendingUpdateTask(ctx, sqlc.CancelPendingUpdateTaskParams{
			ID:       t.ID,
			TenantID: tenantID,
			Detail:   detail,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Claimed or finished between the snapshot and this statement.
				// Whatever it became, it is not ours to overwrite.
				continue
			}
			return AgentRunEvaluation{}, domain.Internal("update_task_finish_failed", "failed to cancel update task").WithCause(err)
		}
		ev.Cancelled++
	}

	if _, err := q.SetUpdateRunStatus(ctx, sqlc.SetUpdateRunStatusParams{ID: run.ID, TenantID: tenantID, Status: RunHalted}); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return AgentRunEvaluation{}, domain.Internal("update_run_status_failed", "failed to halt update run").WithCause(err)
		}
	}
	return ev, nil
}

func findTask(tasks []Task, id uuid.UUID) (Task, bool) {
	for _, t := range tasks {
		if t.ID == id {
			return t, true
		}
	}
	return Task{}, false
}
