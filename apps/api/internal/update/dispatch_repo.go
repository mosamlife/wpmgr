package update

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// GH #463 Phase 1 — the persistence half of deferred dispatch.
//
// Everything here is a compare-and-swap with a status precondition, because
// two control-plane replicas tick at the same time and the transition has to
// be decided by the row under its own lock, not by what this process read a
// moment earlier. db/query/updates.sql documents what a zero-row result means
// for each statement; this file is where those contracts are obeyed.

// uniqueViolation is Postgres' SQLSTATE for a unique-index violation. It is
// the one error MarkScheduledUpdateTaskPending can raise that is NOT a bug:
// its NOT EXISTS arm is a guard and not a lock, so a concurrent transaction
// can insert the conflicting in-flight row between that predicate's evaluation
// and this statement's commit. See dispatchOneTask for why it must be
// survivable rather than fatal.
const uniqueViolation = "23505"

// DispatchOutcome reports what one pass over one due run actually did. Every
// field is a count the caller can log, and the caller is expected to log them:
// a pass that finds due work and dispatches none is the signature of both
// previous silent-scheduler defects in this repo.
type DispatchOutcome struct {
	// Claimed is false when ClaimUpdateRunForDispatch matched no row, which
	// means someone else owns this run. NOT an error, and on a two-replica
	// install it is the normal outcome for about half of every contested tick.
	Claimed bool
	// Dispatched is how many tasks moved 'scheduled' -> 'pending' and had a
	// job enqueued.
	Dispatched int
	// Skipped is how many tasks this pass did not dispatch: their (site,
	// target) was in flight from another run, or their site's monitoring was
	// paused, and they were terminalized 'skipped'. This does NOT fail the run.
	Skipped int
	// PausedSkipped is how many of Skipped were declined specifically because
	// the site's monitoring was paused at fire time.
	//
	// It is a SUBSET of Skipped and not a sibling of it, so the two must never
	// be summed. Skipped stays the answer to "how many tasks did this pass not
	// dispatch"; this one exists because the two causes are different stories
	// to put in front of an operator — "another run was already updating this
	// target" is transient and self-correcting, while "you paused this site" is
	// a state the operator themselves created and is the only one of the two
	// they can act on. A run reporting "8 of 10 applied" with no separation
	// between them reads as broken.
	PausedSkipped int
	// Status is the run status this pass left behind.
	Status string
}

// pausedSiteSkipDetail is recorded on a task the dispatcher declined because
// its site's monitoring was paused when the schedule fired.
//
// It names the pause explicitly. This string is the ONLY per-task trace of why
// the task did not run — it reaches the operator through update_tasks.detail,
// which the task DTO carries to the dashboard — so "skipped" alone would leave
// them with a run that updated fewer sites than it named and no reason why.
const pausedSiteSkipDetail = "not attempted: monitoring is paused for this site"

// CreateScheduledRunWithTasks creates a DEFERRED run: the run row is born
// 'scheduled' and every task is born 'scheduled' too, in one transaction.
//
// It is separate from CreateRunWithTasks rather than a flag on it because the
// per-task insert is a DIFFERENT STATEMENT with an opposite reading of the same
// zero-row result. CreateUpdateTask's ON CONFLICT ... DO NOTHING is the
// authoritative in-flight guard, so zero rows there means "another run holds
// this target, skip it". CreateScheduledUpdateTask's identical-looking clause
// is UNREACHABLE — a row inserted 'scheduled' does not satisfy
// update_tasks_inflight_target_idx's partial predicate, so it never enters the
// index and cannot conflict — so zero rows there would mean something has gone
// wrong that nobody has thought about. One function serving both would be one
// body reading a zero row two ways depending on a bool.
func (r *pgRepo) CreateScheduledRunWithTasks(ctx context.Context, in CreateRunInput, tasks []NewTask) (Run, []Task, error) {
	if in.ScheduledAt == nil {
		// Unreachable from Service.CreateRun, which only takes this branch for
		// a non-nil future time. Refused here anyway: a run committed
		// 'scheduled' with a NULL scheduled_at can never match
		// ListDueUpdateRuns (NULL <= now() is NULL), so it would wait forever,
		// invisible to the reaper and to the due scan alike.
		return Run{}, nil, domain.Internal("scheduled_run_without_time",
			"a scheduled run requires a scheduled_at")
	}

	var run Run
	var outTasks []Task
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)

		var createdBy pgtype.UUID
		if in.CreatedBy != uuid.Nil {
			createdBy = pgtype.UUID{Bytes: in.CreatedBy, Valid: true}
		}

		runRow, err := q.CreateUpdateRun(ctx, sqlc.CreateUpdateRunParams{
			TenantID:    in.TenantID,
			CreatedBy:   createdBy,
			Status:      RunScheduled,
			DryRun:      in.DryRun,
			ScheduledAt: pgtype.Timestamptz{Time: *in.ScheduledAt, Valid: true},
		})
		if err != nil {
			return domain.Internal("update_run_create_failed", "failed to create update run").WithCause(err)
		}
		run = toRun(runRow)

		outTasks = make([]Task, 0, len(tasks))
		for _, t := range tasks {
			taskRow, err := q.CreateScheduledUpdateTask(ctx, sqlc.CreateScheduledUpdateTaskParams{
				RunID:          run.ID,
				TenantID:       in.TenantID,
				SiteID:         t.SiteID,
				TargetType:     t.TargetType,
				TargetSlug:     t.TargetSlug,
				DesiredVersion: t.DesiredVersion,
				FromVersion:    t.FromVersion,
			})
			if err != nil {
				// pgx.ErrNoRows here is NOT the in-flight skip it is on the
				// immediate path. The ON CONFLICT arm is unreachable for a
				// 'scheduled' row, so reaching it means an assumption this
				// feature rests on has stopped holding — most plausibly that
				// update_tasks_inflight_target_idx was widened to cover
				// 'scheduled', which would silently reintroduce both defects
				// #463 exists to avoid. Fail loudly rather than dropping the
				// task.
				if errors.Is(err, pgx.ErrNoRows) {
					return domain.Internal("scheduled_task_conflicted",
						"a scheduled update task was rejected by the in-flight index, which should be unreachable").
						WithCause(fmt.Errorf("site %s target %s/%s", t.SiteID, t.TargetType, t.TargetSlug))
				}
				return domain.Internal("update_task_create_failed", "failed to create update task").WithCause(err)
			}
			outTasks = append(outTasks, toTask(taskRow))
		}
		if len(outTasks) == 0 {
			return domain.Internal("scheduled_run_empty",
				"the scheduled run produced no tasks")
		}
		return nil
	})
	return run, outTasks, err
}

// ListDueRuns returns runs whose scheduled_at has arrived, across ALL tenants,
// capped at limit rows.
//
// CROSS-TENANT, under InAgentTx, admitted by the m118 update_runs_agent policy.
// This is the one statement in the deferred-dispatch path that cannot name a
// tenant, because finding the tenant is what it is for; every statement after
// it names the tenant the scan returned.
//
// ZERO ROWS IS AMBIGUOUS BY NATURE and must not be disambiguated by guessing.
// "Nothing is due" is the overwhelmingly common case, and it is also exactly
// what a missing RLS policy looks like — the failure mode that produced m84,
// m89 and m118 in turn, each time as a cheerful "0 rows" and a scheduler that
// never fired for any install. The proof that this returns rows at all belongs
// to an integration test running as wpmgr_app through this method, not to a
// log line.
//
// The window is deliberately NOT applied here: every due run comes back, and
// the caller splits them into dispatch and expiry. A scan that filtered by the
// window would leave a run that fell past it unreachable by any query, at
// 'scheduled' forever.
func (r *pgRepo) ListDueRuns(ctx context.Context, limit int32) ([]Run, error) {
	var out []Run
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListDueUpdateRuns(ctx, limit)
		if err != nil {
			return domain.Internal("due_runs_list_failed", "failed to list due update runs").WithCause(err)
		}
		out = make([]Run, 0, len(rows))
		for _, row := range rows {
			out = append(out, toRun(row))
		}
		return nil
	})
	return out, err
}

// ExpireDueRun terminalizes a run that came due more than the grace window ago,
// together with every task it never dispatched, IN ONE TRANSACTION.
//
// The single transaction is the whole safety property. A run that reaches a
// terminal state while its tasks are still 'scheduled' strands them
// permanently: 'scheduled' is not terminal so no outcome is recorded, it is
// outside ListStaleUpdateTasks so the reaper never un-sticks them, and the run
// has left update_runs_due_idx so no future tick looks at it again. There is no
// janitor anywhere in this system that would find them.
//
// Tasks become TaskExpired, not TaskCancelled. Nobody cancelled this run; the
// control plane was not up in time to start it, and those are different facts
// to put in front of an operator.
//
// Returns claimed=false when the run was not expirable — it left 'scheduled'
// in the gap since the scan, or it is not actually past the cutoff. Both are
// benign; the caller skips it and never retries expiry.
func (r *pgRepo) ExpireDueRun(ctx context.Context, tenantID, runID uuid.UUID, expireBefore time.Time, detail string) (bool, int, error) {
	if expireBefore.IsZero() {
		// A zero cutoff would arrive as a NULL timestamptz and match nothing,
		// so the statement itself already fails closed. Refused here anyway so
		// the caller learns it computed a cutoff wrong, instead of watching
		// expiry silently never happen.
		return false, 0, domain.Internal("expire_cutoff_unset",
			"the expiry cutoff was not computed")
	}
	var expired bool
	var tasks int
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := q.ExpireDueUpdateRun(ctx, sqlc.ExpireDueUpdateRunParams{
			ID:           runID,
			TenantID:     tenantID,
			ExpireBefore: pgtype.Timestamptz{Time: expireBefore, Valid: true},
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // not expirable; leave it alone.
			}
			return domain.Internal("update_run_expire_failed", "failed to expire update run").WithCause(err)
		}
		expired = true

		rows, err := q.TerminalizeScheduledTasksForRun(ctx, sqlc.TerminalizeScheduledTasksForRunParams{
			Status:   TaskExpired,
			Detail:   detail,
			RunID:    runID,
			TenantID: tenantID,
		})
		if err != nil {
			return domain.Internal("update_tasks_expire_failed", "failed to expire update tasks").WithCause(err)
		}
		tasks = len(rows)
		return nil
	})
	return expired, tasks, err
}

// DispatchDueRun claims one due run and turns its scheduled tasks into real,
// enqueued work — ALL OF IT IN ONE TRANSACTION, the River inserts included.
//
// The atomicity is not tidiness. 'dispatching' is absent from
// update_runs_due_idx, so a run left there is never scanned again and is
// stranded forever with no reaper that would find it. Committing the claim
// separately from the enqueue is therefore the one way to lose a run
// permanently: crash in the gap and the run is claimed, un-enqueued and
// invisible. Inside one transaction, the same crash rolls the claim back and
// the next tick finds the run still 'scheduled'.
//
// enq inserts each task's job INSIDE this transaction (River's InsertTx), which
// is what makes that guarantee reachable at all.
func (r *pgRepo) DispatchDueRun(ctx context.Context, enq TxEnqueuer, run Run) (DispatchOutcome, error) {
	var out DispatchOutcome
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		q := sqlc.New(tx)

		if _, err := q.ClaimUpdateRunForDispatch(ctx, sqlc.ClaimUpdateRunForDispatchParams{
			ID:       run.ID,
			TenantID: run.TenantID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Someone else owns this run: another replica claimed it, an
				// operator cancelled it in the gap since the scan, or this
				// pass's own expiry arm took it. Every losing interpretation
				// has the same correct action, so the reason is not read.
				return nil
			}
			return domain.Internal("update_run_claim_failed", "failed to claim update run for dispatch").WithCause(err)
		}
		out.Claimed = true

		tasks, err := q.ListUpdateTasksForRun(ctx, sqlc.ListUpdateTasksForRunParams{
			RunID:    run.ID,
			TenantID: run.TenantID,
		})
		if err != nil {
			return domain.Internal("update_tasks_list_failed", "failed to list tasks for dispatch").WithCause(err)
		}

		// READ THE PAUSE STATE HERE, AT FIRE TIME, AND NOT WHEN THE RUN WAS
		// CREATED. That placement is the entire point of the check.
		//
		// The scenario this exists for is a site paused AFTER its update was
		// scheduled: an operator defers a fleet update to 02:00, something goes
		// wrong on one site at 23:00, they pause it, and at 02:00 the run fires.
		// Reading pause at create time would miss exactly that case and let the
		// update land on the site somebody deliberately froze mid-incident,
		// which is the outcome nobody can undo.
		//
		// One set-based read rather than a per-task one: a run can span the
		// whole fleet, and an N+1 inside the dispatch transaction would hold it
		// open across hundreds of round trips.
		siteIDs := make([]uuid.UUID, 0, len(tasks))
		for _, row := range tasks {
			if row.Status == TaskScheduled {
				siteIDs = append(siteIDs, row.SiteID)
			}
		}
		paused, err := pausedSiteIDs(ctx, tx, run.TenantID, siteIDs)
		if err != nil {
			// FAIL CLOSED. Returning the error rolls the claim back with it, so
			// the run returns to 'scheduled' and a later tick retries. The
			// alternative — treating an unreadable pause state as "not paused"
			// — dispatches into sites whose pause we simply failed to read,
			// which is the one outcome this whole check exists to prevent.
			return err
		}

		for _, row := range tasks {
			if row.Status != TaskScheduled {
				// Already dispatched by an earlier partial pass, or
				// terminalized. Not this pass's to move.
				continue
			}

			// PER TASK, NOT PER RUN. A run spanning ten sites with two paused
			// dispatches to eight and skips two; one frozen site must not take
			// the other nine down with it. Hence `continue` on this one row
			// rather than any early return out of the loop.
			if _, isPaused := paused[row.SiteID]; isPaused {
				// Declined BEFORE MarkScheduledUpdateTaskPending and before any
				// enqueue, which is what makes "no command reaches this site"
				// true rather than merely likely: the task never enters
				// update_tasks_inflight_target_idx and no TaskArgs job is
				// inserted, and update.Worker.Work — the only thing that can
				// sign a command for a site — is reachable only through such a
				// job.
				recorded, serr := skipPausedTask(ctx, tx, toTask(row))
				if serr != nil {
					return serr
				}
				if recorded {
					out.PausedSkipped++
				}
				out.Skipped++
				continue
			}

			dispatched, derr := dispatchOneTask(ctx, tx, enq, run, toTask(row))
			if derr != nil {
				return derr
			}
			if dispatched {
				out.Dispatched++
			} else {
				out.Skipped++
			}
		}

		// ASK THE DATABASE WHETHER ANYTHING IS LEFT. Do NOT infer it from this
		// pass's counters.
		//
		// out.Dispatched and out.Skipped describe what THIS PASS did, which is
		// a strictly narrower thing than what the RUN still owes, and the two
		// diverge in ways that all point the same dangerous direction:
		//
		//   - The loop skips every task that is not 'scheduled' WITHOUT
		//     counting it. A run holding 'pending' or 'running' tasks from an
		//     earlier partial pass therefore contributes nothing to either
		//     counter, so "Dispatched == 0" could be reached with live commands
		//     already out on the operator's sites.
		//   - out.Skipped also absorbs the two dispatchOneTask outcomes that
		//     record no terminal state at all: the row stayed 'scheduled' (a
		//     concurrent writer moved it, or is about to), and the row was gone.
		//     Neither means "nothing left to wait for".
		//
		// Marking such a run 'completed' tells the operator their fleet update
		// finished while it is still going out. That is the stranded-run defect
		// inverted — there, a run nobody would ever finish; here, a run declared
		// finished while it is still working — and both come from inferring run
		// state from a partial view instead of asking.
		//
		// CountUnfinishedTasksForRun is exactly this question and is already the
		// authority for it elsewhere. It counts 'pending', 'running' AND
		// 'scheduled' — the last one added for this very feature — so it sees
		// the earlier pass's live work, this pass's fresh dispatches, and any
		// task still awaiting a future pass, while terminal rows (including the
		// tasks this pass recorded 'skipped') correctly fall out.
		unfinished, err := q.CountUnfinishedTasksForRun(ctx, sqlc.CountUnfinishedTasksForRunParams{
			RunID:    run.ID,
			TenantID: run.TenantID,
		})
		if err != nil {
			return domain.Internal("update_run_unfinished_count_failed",
				"failed to count unfinished tasks before closing dispatch").WithCause(err)
		}
		// Only a run that genuinely owes nothing is 'completed' — the case where
		// every target was busy and each task was terminalized 'skipped'.
		// Anything still outstanding stays 'running' and is finished by the
		// worker that owns it.
		//
		// A RUN WHOSE SITES WERE ALL PAUSED THEREFORE LANDS 'completed', and
		// that is a deliberate choice rather than a fallthrough.
		//
		// 'completed' is defined in schema.sql as "All tasks reached a terminal
		// state" — it has never meant "all work succeeded", and every task here
		// did reach one. It is not 'failed': nothing was attempted and nothing
		// is broken. It is not 'expired', which means the control plane missed
		// the window and is about the control plane's own health. It is not
		// 'halted', which is reserved for an agent self-update wave whose gate
		// refused mid-rollout, with commands already out on real sites.
		//
		// The decisive argument is that an all-paused run is the SAME SHAPE as
		// the all-targets-busy run directly above, which has always landed
		// 'completed'. Giving one of two identical situations a different
		// status would make the run list report the same fact two ways.
		//
		// What stops 'completed' from overstating the outcome is not the run
		// status but the per-task record: every task carries
		// pausedSiteSkipDetail, and out.PausedSkipped reaches the dispatch
		// audit entry and the dispatch log line. Introducing a new run status
		// instead would be a wire-contract change across schema.sql, the
		// OpenAPI enum and the dashboard, to say something the task rows
		// already say precisely.
		out.Status = RunRunning
		if unfinished == 0 {
			out.Status = RunCompleted
		}
		if _, err := q.FinishUpdateRunDispatch(ctx, sqlc.FinishUpdateRunDispatchParams{
			Status:   out.Status,
			ID:       run.ID,
			TenantID: run.TenantID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Unlike the claim, losing HERE is not a normal race: the only
				// writer that can move a row out of 'dispatching' is the one
				// that put it there, and that is this transaction. Surface it.
				return domain.Internal("update_run_dispatch_unfinished",
					"the claimed run was no longer 'dispatching' when its dispatch closed")
			}
			return domain.Internal("update_run_dispatch_finish_failed", "failed to close update run dispatch").WithCause(err)
		}
		return nil
	})
	return out, err
}

// dispatchOneTask moves ONE task 'scheduled' -> 'pending' and enqueues its job,
// or records it as skipped. Reports whether it was dispatched.
//
// EVERY ATTEMPT RUNS UNDER ITS OWN SAVEPOINT, and that is what stops one
// operator's manual update from killing a whole scheduled run.
// MarkScheduledUpdateTaskPending's NOT EXISTS arm is a guard, not a lock: a
// concurrent transaction can insert the conflicting in-flight row between the
// predicate's evaluation and this statement's commit, and then
// update_tasks_inflight_target_idx raises 23505. Inside the dispatcher's single
// transaction an unguarded 23505 aborts everything and takes every OTHER task
// of the run down with it. The savepoint contains it to the one task that lost.
//
// The zero-row result is ambiguous by construction and is disambiguated by
// RE-READING THE ROW, which is part of the statement's contract and not an
// optimisation to skip:
//
//	status <> 'scheduled'  Someone else already moved it. Leave the recorded
//	                       state alone; do not re-dispatch, do not terminalize.
//	status  = 'scheduled'  Its target is in flight from another run. Record
//	                       'skipped' so the row reaches a terminal state — a
//	                       skip left unrecorded is a task stuck at 'scheduled'
//	                       forever, outside the reaper and outside the due
//	                       scan.
//
// A 23505 is treated as the SAME outcome as the second case, because it is: the
// row is still 'scheduled' and its target is taken.
func dispatchOneTask(ctx context.Context, tx pgx.Tx, enq TxEnqueuer, run Run, task Task) (bool, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return false, domain.Internal("dispatch_savepoint_failed", "failed to open a savepoint for task dispatch").WithCause(err)
	}
	spq := sqlc.New(sp)

	_, merr := spq.MarkScheduledUpdateTaskPending(ctx, sqlc.MarkScheduledUpdateTaskPendingParams{
		ID:       task.ID,
		TenantID: task.TenantID,
	})
	switch {
	case merr == nil:
		// Enqueue INSIDE the savepoint, so a failed insert rolls the task back
		// to 'scheduled' rather than leaving a 'pending' row with no job — a
		// row that would hold its in-flight slot until the 45-minute reaper
		// failed it.
		if eerr := enq.EnqueueTaskTx(ctx, sp, run.TenantID, run.ID, task.ID, run.DryRun); eerr != nil {
			_ = sp.Rollback(ctx)
			return false, domain.Internal("dispatch_enqueue_failed", "failed to enqueue a dispatched update task").WithCause(eerr)
		}
		if cerr := sp.Commit(ctx); cerr != nil {
			return false, domain.Internal("dispatch_savepoint_commit_failed", "failed to commit a task dispatch").WithCause(cerr)
		}
		return true, nil

	case errors.Is(merr, pgx.ErrNoRows), isUniqueViolation(merr):
		// Both land in the same place, and the row itself says which case it
		// is. The rollback is mandatory before the re-read on the 23505 arm:
		// the savepoint is aborted and no further statement can run in it.
		_ = sp.Rollback(ctx)

	default:
		_ = sp.Rollback(ctx)
		return false, domain.Internal("dispatch_task_claim_failed", "failed to move a scheduled task to pending").WithCause(merr)
	}

	// Re-read on the OUTER transaction: the savepoint that would have carried
	// the read has been rolled back.
	q := sqlc.New(tx)
	current, gerr := q.GetUpdateTask(ctx, sqlc.GetUpdateTaskParams{ID: task.ID, TenantID: task.TenantID})
	if gerr != nil {
		if errors.Is(gerr, pgx.ErrNoRows) {
			// The row is gone. Nothing to record and nothing to dispatch.
			return false, nil
		}
		return false, domain.Internal("dispatch_task_reread_failed", "failed to re-read a task after a refused dispatch").WithCause(gerr)
	}
	if current.Status != TaskScheduled {
		// Someone else moved it. Not ours to record an outcome for.
		return false, nil
	}

	// Still 'scheduled' => the target is in flight from another run. This is a
	// SKIP, not a failure: the run is not failed and its remaining tasks
	// proceed normally.
	if _, ferr := q.FinishScheduledUpdateTask(ctx, sqlc.FinishScheduledUpdateTaskParams{
		Status:   TaskSkipped,
		Detail:   "not attempted: another run was already updating this target when the schedule fired",
		ID:       task.ID,
		TenantID: task.TenantID,
	}); ferr != nil {
		if errors.Is(ferr, pgx.ErrNoRows) {
			// It left 'scheduled' between the re-read and this write. The
			// recorded outcome wins.
			return false, nil
		}
		return false, domain.Internal("dispatch_task_skip_failed", "failed to record a skipped scheduled task").WithCause(ferr)
	}
	return false, nil
}

// pausedSiteIDs returns the subset of siteIDs whose monitoring is currently
// paused, read on the CALLER'S transaction so it is a fire-time fact about the
// same snapshot the dispatch is deciding from.
//
// This is m117's monitoring pause (GH #414) applied to the deferred dispatcher.
// The predicate is `monitoring_paused_at IS NOT NULL`, the same one
// vuln.Repo.IsMonitoringPaused and screenshot's DBSiteIDLister.IsMonitoringPaused
// already ask per site; this is the set-based form of that question, because a
// dispatcher asking it one row at a time inside its own transaction would hold
// that transaction open across the whole fleet. It reuses the existing column
// contract rather than introducing a second notion of "paused".
//
// A SITE WITH NO ROW IS REPORTED NOT PAUSED, by construction — it simply does
// not appear in the result. That matches both existing implementations and is
// the right reading: a missing row means the site was deleted between
// scheduling and firing, and its dispatch then finds no site and does nothing.
// Inventing a pause for a deleted site would be the worse failure.
//
// tenant_id IS IN THE PREDICATE AND IS LOAD-BEARING HERE, not merely the usual
// defence in depth. This runs under InAgentTx, whose whole purpose is to let
// the dispatcher work across tenants, so RLS is not the boundary it is on a
// tenant-scoped path. The explicit tenant_id is what stops a task row somehow
// naming another tenant's site from reading that site's pause state, and it
// keeps the (tenant_id, ...) index in play.
func pausedSiteIDs(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, siteIDs []uuid.UUID) (map[uuid.UUID]struct{}, error) {
	out := make(map[uuid.UUID]struct{})
	if len(siteIDs) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT id
		  FROM sites
		 WHERE tenant_id = $1
		   AND id = ANY($2::uuid[])
		   AND monitoring_paused_at IS NOT NULL`, tenantID, siteIDs)
	if err != nil {
		return nil, domain.Internal("dispatch_pause_read_failed",
			"failed to read monitoring pause state before dispatch").WithCause(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, domain.Internal("dispatch_pause_scan_failed",
				"failed to scan monitoring pause state before dispatch").WithCause(err)
		}
		out[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, domain.Internal("dispatch_pause_read_failed",
			"failed to read monitoring pause state before dispatch").WithCause(err)
	}
	return out, nil
}

// skipPausedTask terminalizes ONE task 'scheduled' -> 'skipped' because its
// site's monitoring is paused. Reports whether this call is the one that
// recorded it.
//
// No savepoint, unlike dispatchOneTask, and the asymmetry is the point rather
// than an omission. dispatchOneTask needs one because
// MarkScheduledUpdateTaskPending can raise 23505 off
// update_tasks_inflight_target_idx and abort the whole dispatch transaction.
// This path never inserts into that index — declining is precisely NOT taking
// the in-flight slot — so the only statement it runs is an UPDATE guarded by a
// status precondition, which cannot raise it.
//
// A zero-row result means the row left 'scheduled' between the task list and
// this write: a concurrent cancel, or a competing dispatcher. The recorded
// outcome wins and false comes back, exactly as on the busy-target skip path.
func skipPausedTask(ctx context.Context, tx pgx.Tx, task Task) (bool, error) {
	q := sqlc.New(tx)
	if _, err := q.FinishScheduledUpdateTask(ctx, sqlc.FinishScheduledUpdateTaskParams{
		Status:   TaskSkipped,
		Detail:   pausedSiteSkipDetail,
		ID:       task.ID,
		TenantID: task.TenantID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, domain.Internal("dispatch_paused_skip_failed",
			"failed to record a task skipped for a paused site").WithCause(err)
	}
	return true, nil
}

// isUniqueViolation reports whether err is Postgres' 23505.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}
