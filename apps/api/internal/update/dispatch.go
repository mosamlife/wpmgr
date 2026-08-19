package update

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
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

	due, err := w.repo.ListDueRuns(ctx, dueRunScanLimit)
	if err != nil {
		return err
	}
	for _, run := range due {
		if run.ID != a.RunID {
			// This job fires ONE run. The scan is how the run's authoritative
			// current state is read (there is no tenant context here to read
			// it any other way), not a licence to dispatch the whole backlog:
			// each run has its own job, and draining them from here would run
			// runs whose own jobs are about to run them too.
			continue
		}
		return w.fire(ctx, run)
	}

	// Not due, not 'scheduled', or gone. Every one of those is benign and
	// none is retryable: cancelled by an operator, already dispatched by a
	// duplicate job, or expired by an earlier pass.
	w.logger.Info("update dispatch: run is no longer due; nothing to do",
		slog.String("run_id", a.RunID.String()),
		slog.String("tenant_id", a.TenantID.String()))
	return nil
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

	cutoff := w.now().Add(-dispatchGraceWindow)
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
		_, _ = w.audit.Record(ctx, audit.Event{
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
		})
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
		_, _ = w.audit.Record(ctx, audit.Event{
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
		})
	}
	return nil
}
