package update

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// GH #463 Phase 1 — the deferred-dispatch job and its worker.

// Audit actions for the deferred-dispatch lifecycle.
const (
	// ActionRunDispatched records that a scheduled run fired. It carries the
	// dispatched and skipped counts, so "the run updated fewer sites than it
	// named" has an answer that does not depend on reading every task row.
	ActionRunDispatched = "update.run.dispatched"
	// ActionRunExpired records that a scheduled run passed its grace window
	// undispatched and no site was contacted. This is the record the #463
	// design asks for by name: an expired run is a bulk update that silently
	// never happened, and without an audit entry the only trace is a status
	// nobody was watching.
	ActionRunExpired = "update.run.expired"
)

// dispatchGraceWindow is how late a scheduled run may fire and still be run at
// all. Past it, the run is expired instead.
//
// THIS VALUE IS PER-DOMAIN AND NOT GLOBAL. Updates are deliberately timed to
// miss traffic, so firing hours late lands the fleet's updates at business
// open — a harm the operator specifically did not choose. Backups are the
// opposite: a late backup is still a backup, and should always catch up. The
// two must not be unified into one constant.
//
// Two hours is short enough that no plausible business-hours window is crossed
// and long enough that an ordinary deploy or a brief restart does not silently
// destroy an operator's scheduled work.
const dispatchGraceWindow = 2 * time.Hour

// dueRunScanLimit bounds how many due runs one dispatcher pass handles, so an
// unbounded backlog cannot make a single tick unbounded. The remainder is
// picked up by the next pass; ListDueUpdateRuns orders by scheduled_at ASC, so
// a backlog drains in the order the operators asked for. Mirrors
// staleTaskReapLimit's role for the reaper.
const dueRunScanLimit = 200

// dispatchEarlyFireTolerance is how far ahead of scheduled_at a job may fire and
// still count as due. It absorbs ordinary skew between River's timer and this
// process's wall clock; a job arriving earlier than this snoozes until the real
// instant rather than dispatching early.
const dispatchEarlyFireTolerance = 5 * time.Second

// TxEnqueuer enqueues a task job INSIDE an existing transaction (River's
// InsertTx). The deferred-dispatch path needs this and cannot use the ordinary
// Enqueuer: the claim, the per-task transitions and the enqueue must commit
// together or not at all. See pgRepo.DispatchDueRun for why splitting them
// strands a run permanently.
type TxEnqueuer interface {
	EnqueueTaskTx(ctx context.Context, tx pgx.Tx, tenantID, runID, taskID uuid.UUID, dryRun bool) error
}

// DispatchRunArgs is the River job payload for firing ONE deferred run.
//
// It carries no task ids and no scheduled_at, on purpose. What is dispatchable
// at fire time is not knowable at create time — a target may have gone in
// flight, the run may have been cancelled — so the worker re-reads everything.
// Carrying the schedule would let a stale job argue with the row about when the
// run was due.
type DispatchRunArgs struct {
	TenantID uuid.UUID `json:"tenant_id"`
	RunID    uuid.UUID `json:"run_id"`
}

// Kind implements river.JobArgs.
func (DispatchRunArgs) Kind() string { return "update_run_dispatch" }

// InsertOpts pins the dispatch job to the tenant's queue shard, the same shard
// its tasks will land on, so a tenant's dispatch cannot starve another
// tenant's.
func (a DispatchRunArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueForTenant(a.TenantID)}
}

// DispatchWorker fires deferred update runs: River's timer wakes it at the
// run's scheduled_at, and it re-reads, checks the grace window, claims, and
// enqueues.
type DispatchWorker struct {
	river.WorkerDefaults[DispatchRunArgs]
	repo   Repo
	enq    TxEnqueuer
	audit  *audit.Recorder
	logger *slog.Logger
	clock  func() time.Time
}

// NewDispatchWorker builds the deferred-run dispatcher.
func NewDispatchWorker(repo Repo, enq TxEnqueuer, rec *audit.Recorder, logger *slog.Logger) *DispatchWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &DispatchWorker{repo: repo, enq: enq, audit: rec, logger: logger}
}

// SetTxEnqueuer wires the transactional job inserter. It is a setter rather
// than a constructor argument because of a genuine cycle in main: the enqueuer
// needs the River client, and the River client needs the worker set this worker
// belongs to. Same shape as Worker.SetRefreshEnqueuer, and it must be called at
// boot — Work refuses loudly without it (see below) rather than dispatching a
// run whose tasks would then have no jobs.
func (w *DispatchWorker) SetTxEnqueuer(enq TxEnqueuer) { w.enq = enq }

// SetClock overrides the worker's notion of now. Tests only; production leaves
// it nil and reads the wall clock. Due-ness itself is never decided here —
// ListDueUpdateRuns compares against the DATABASE clock — so this only moves
// the grace-window boundary.
func (w *DispatchWorker) SetClock(fn func() time.Time) { w.clock = fn }

func (w *DispatchWorker) now() time.Time {
	if w.clock != nil {
		return w.clock()
	}
	return time.Now()
}

// Work fires one deferred run.
//
// The job is a TRIGGER, not an authority. Everything that decides what happens
// is re-read from the row: whether the run is still 'scheduled', whether it is
// inside its grace window, and which of its tasks are still dispatchable. A
// River job that fires twice, fires late, or fires for a run an operator
// cancelled ten minutes ago all reach the same CAS statements and lose
// harmlessly.
//
// THE GRACE WINDOW IS CHECKED HERE, AT FIRE TIME, and not at the point that
// scheduled the job. That placement is the reason it holds for every trigger
// path — River's timer, the Phase 2 safety-net sweeper, an operator's manual
// re-drive — rather than only for the one that happens to have been written
// first.
func (w *DispatchWorker) Work(ctx context.Context, job *river.Job[DispatchRunArgs]) error {
	a := job.Args

	if w.enq == nil {
		// A boot that never called SetTxEnqueuer. Returning an error is the
		// point: River retries and eventually dead-letters, which is loud.
		// Returning nil would mark every scheduled run's dispatch job complete
		// while the runs sat at 'scheduled' forever, and the feature would look
		// like it was simply never triggered.
		return fmt.Errorf("update dispatch: no task enqueuer is wired; refusing to dispatch run %s", a.RunID)
	}

	// READ THIS RUN BY ID. The job carries both ids, so it needs nothing else,
	// and that self-sufficiency is a correctness property rather than a tidy-up.
	//
	// This used to find its run by filtering the cross-tenant due scan, which is
	// bounded at dueRunScanLimit. A run outside the oldest N due runs — an
	// outage spanning many scheduled times, or a fleet with many runs on one
	// slot — never appeared in ITS OWN job's scan. The worker returned nil,
	// River marked the job completed, and nothing found the run again: still
	// 'scheduled' so still in the due index, but the only reader of that index
	// is a dispatch job and this run's job was consumed. Its tasks stayed
	// 'scheduled', outside the dedup index and outside the reaper, and the run
	// could not even expire, because expiry is reached through the same consumed
	// job. Recovery was manual DB surgery. A per-run read has no page to fall
	// out of.
	//
	// It is also LESS privileged: GetRun is tenant-scoped (InTenantTx), so the
	// dispatch path no longer reads cross-tenant at all. ListDueRuns stays on
	// the Repo for the Phase 2 sweeper, which genuinely needs to enumerate.
	run, err := w.repo.GetRun(ctx, a.TenantID, a.RunID)
	if err != nil {
		if de, ok := domain.AsDomain(err); ok && de.Kind == domain.KindNotFound {
			// The run was deleted. Nothing to dispatch and nothing to record.
			w.logger.Info("update dispatch: run no longer exists",
				slog.String("run_id", a.RunID.String()),
				slog.String("tenant_id", a.TenantID.String()))
			return nil
		}
		// An infrastructure error must NOT complete the job: that is the
		// stranding shape above reached by a different door. Return it so River
		// retries.
		return err
	}

	if run.Status != RunScheduled {
		// Cancelled by an operator, already dispatched by a duplicate job, or
		// expired by an earlier pass. All benign, none retryable — and all
		// concluded from the row itself, which is exactly what the scan could
		// not distinguish from "not in my page of results".
		w.logger.Info("update dispatch: run is no longer scheduled; nothing to do",
			slog.String("run_id", run.ID.String()),
			slog.String("tenant_id", run.TenantID.String()),
			slog.String("status", run.Status))
		return nil
	}
	return w.fire(ctx, run)
}

// fire applies the grace window to one due run and then either dispatches it or
// expires it. The two arms are mutually exclusive at the database, not here:
// both CAS statements require status = 'scheduled', so only one of them can
// ever match a given row.
func (w *DispatchWorker) fire(ctx context.Context, run Run) error {
	if run.ScheduledAt == nil {
		// A 'scheduled' run with no scheduled_at cannot have been returned by
		// the due scan (NULL <= now() is NULL), so this is unreachable. Log
		// rather than dispatch: firing a run whose start time is unknown is
		// guessing at what the operator asked for.
		w.logger.Warn("update dispatch: due run has no scheduled_at; refusing to fire",
			slog.String("run_id", run.ID.String()))
		return nil
	}

	now := w.now()

	// NOT YET DUE => SNOOZE, NEVER COMPLETE. River fires the job at
	// scheduled_at, so arriving early means a clock disagreement rather than a
	// run that should go now. Snoozing reschedules without consuming an attempt
	// and, critically, without completing the job — completing it here would
	// consume the run's only trigger before its time and strand it exactly the
	// way the scan-page miss did.
	if now.Before(run.ScheduledAt.Add(-dispatchEarlyFireTolerance)) {
		wait := run.ScheduledAt.Sub(now)
		w.logger.Info("update dispatch: job fired before its run was due; snoozing until the scheduled time",
			slog.String("run_id", run.ID.String()),
			slog.Duration("wait", wait))
		return river.JobSnooze(wait)
	}

	cutoff := now.Add(-dispatchGraceWindow)
	if run.ScheduledAt.Before(cutoff) {
		return w.expire(ctx, run, cutoff)
	}

	out, err := w.repo.DispatchDueRun(ctx, w.enq, run)
	if err != nil {
		return err
	}
	if !out.Claimed {
		// Another writer owns it. Normal on a multi-replica install; not an
		// error, not retried.
		w.logger.Info("update dispatch: run was claimed by another writer",
			slog.String("run_id", run.ID.String()))
		return nil
	}

	// A claimed pass that dispatched nothing is worth saying out loud: it is
	// legitimate (every target was busy) but it is also the signature both of
	// this repo's previous silent-scheduler defects, and it is indistinguishable
	// from them without the counts.
	level := slog.LevelInfo
	if out.Dispatched == 0 {
		level = slog.LevelWarn
	}
	w.logger.Log(ctx, level, "update dispatch: scheduled run fired",
		slog.String("run_id", run.ID.String()),
		slog.String("tenant_id", run.TenantID.String()),
		slog.Int("dispatched", out.Dispatched),
		slog.Int("skipped", out.Skipped),
		slog.String("run_status", out.Status))

	if w.audit != nil {
		if _, aerr := w.audit.Record(ctx, audit.Event{
			TenantID:   run.TenantID,
			ActorType:  audit.ActorSystem,
			Action:     ActionRunDispatched,
			TargetType: "update_run",
			TargetID:   run.ID.String(),
			Metadata: map[string]any{
				"scheduled_at": run.ScheduledAt.UTC().Format(time.RFC3339),
				"dispatched":   out.Dispatched,
				"skipped":      out.Skipped,
				"run_status":   out.Status,
			},
		}); aerr != nil {
			// Not fatal: the work IS dispatched, and failing the job here would
			// re-run a dispatch that already happened. But it must not be
			// silent — this record is the only durable trace of how much of the
			// run actually went out versus was skipped.
			w.logger.Error("update dispatch: failed to record the dispatch audit entry",
				slog.String("run_id", run.ID.String()),
				slog.Any("error", aerr))
		}
	}
	return nil
}

// expire terminalizes a run that came due more than dispatchGraceWindow ago.
//
// The audit record is not optional bookkeeping. An expired run is a bulk update
// that silently never happened; without a record, the only trace is a status on
// a row nobody was watching, and the operator's first evidence is an unpatched
// fleet.
func (w *DispatchWorker) expire(ctx context.Context, run Run, cutoff time.Time) error {
	detail := "not attempted: the run passed its dispatch window before the control plane could start it"
	expired, tasks, err := w.repo.ExpireDueRun(ctx, run.TenantID, run.ID, cutoff, detail)
	if err != nil {
		return err
	}
	if !expired {
		// It left 'scheduled' in the gap since the scan, or is not actually
		// past the cutoff. Benign; never retried.
		w.logger.Info("update dispatch: run was not expirable",
			slog.String("run_id", run.ID.String()))
		return nil
	}

	lateBy := w.now().Sub(*run.ScheduledAt)
	w.logger.Warn("update dispatch: scheduled run expired without contacting any site",
		slog.String("run_id", run.ID.String()),
		slog.String("tenant_id", run.TenantID.String()),
		slog.String("scheduled_at", run.ScheduledAt.UTC().Format(time.RFC3339)),
		slog.Duration("late_by", lateBy),
		slog.Duration("grace_window", dispatchGraceWindow),
		slog.Int("tasks_expired", tasks))

	if w.audit != nil {
		if _, aerr := w.audit.Record(ctx, audit.Event{
			TenantID:   run.TenantID,
			ActorType:  audit.ActorSystem,
			Action:     ActionRunExpired,
			TargetType: "update_run",
			TargetID:   run.ID.String(),
			Metadata: map[string]any{
				"scheduled_at":    run.ScheduledAt.UTC().Format(time.RFC3339),
				"grace_window":    dispatchGraceWindow.String(),
				"late_by":         lateBy.String(),
				"tasks_expired":   tasks,
				"sites_contacted": 0,
			},
		}); aerr != nil {
			// Louder than its dispatch counterpart, deliberately. An expired run
			// is a bulk update that silently never happened and this record is
			// its ONLY durable trace; losing it leaves exactly the state the
			// comment above warns about, where the operator's first evidence is
			// an unpatched fleet.
			w.logger.Error("update dispatch: failed to record the EXPIRY audit entry; an expired run may now have no durable trace",
				slog.String("run_id", run.ID.String()),
				slog.Any("error", aerr))
		}
	}
	return nil
}
