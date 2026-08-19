package update

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// GH #463 Phase 2 — the safety net, and the alarms.
//
// DispatchWorker fires a run from its own job. This file covers the case that
// worker cannot: A JOB THAT WAS LOST BEFORE IT EVER RAN. An enqueue that
// committed but never materialised, a queue wiped by an operator, a job
// dead-lettered and purged. In every one of those the run row is untouched —
// still 'scheduled', still due, still in update_runs_due_idx — and absolutely
// nothing would ever look at it again, because the only reader of that index
// was the job that no longer exists.
//
// It is deliberately NOT the primary path. River's per-run scheduled insert is,
// and this sweeper's normal outcome is to find nothing to do.

// sweepLockKey is the advisory-lock namespace for the dispatch sweeper. One
// pass at a time across every replica.
const sweepLockKey = "update_dispatch_sweeper"

// SweepInterval is how often the safety net runs, exported so cmd/wpmgr can
// register the periodic job and its UniqueOpts window with the SAME value. They
// must match: ByPeriod is what stops a rolling deploy's RunOnStart from firing
// one pass per replica, and a window narrower than the interval would let them
// through.
const SweepInterval = time.Minute

// SweepDueRunsArgs is the periodic River job that re-enqueues dispatch jobs for
// due runs that have none. Cross-tenant; carries no payload.
type SweepDueRunsArgs struct{}

// Kind implements river.JobArgs.
func (SweepDueRunsArgs) Kind() string { return "update_dispatch_sweeper" }

// SweepWorker is the safety-net sweeper.
type SweepWorker struct {
	river.WorkerDefaults[SweepDueRunsArgs]
	repo   Repo
	enq    SweepEnqueuer
	pool   *pgxpool.Pool
	logger *slog.Logger
	clock  func() time.Time
}

// NewSweepWorker builds the dispatch sweeper. pool is used only for the
// single-flight advisory lock and may be nil (the pass then runs unlocked,
// which is safe but wasteful — see Work).
func NewSweepWorker(repo Repo, enq SweepEnqueuer, pool *pgxpool.Pool, logger *slog.Logger) *SweepWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &SweepWorker{repo: repo, enq: enq, pool: pool, logger: logger}
}

// SetEnqueuer wires the dispatch-job inserter. A setter for the same cycle
// reason as DispatchWorker.SetTxEnqueuer: the enqueuer needs the River client,
// and the client needs this worker. A nil enqueuer makes every pass a no-op
// that still logs, which sweep() reports rather than hiding.
func (w *SweepWorker) SetEnqueuer(enq SweepEnqueuer) { w.enq = enq }

// SetClock overrides the sweeper's notion of now. Tests only.
func (w *SweepWorker) SetClock(fn func() time.Time) { w.clock = fn }

func (w *SweepWorker) now() time.Time {
	if w.clock != nil {
		return w.clock()
	}
	return time.Now()
}

// SweepStats is one pass's outcome, returned so a test can assert on it
// directly rather than parsing log lines.
type SweepStats struct {
	// Due is how many due runs the scan returned, capped at dueRunScanLimit.
	Due int
	// Enqueued is how many dispatch jobs this pass actually inserted.
	Enqueued int
	// AlreadyLive is how many runs already had a dispatch job, so River's
	// unique guard skipped the insert. On a healthy install this is where
	// nearly every due run lands.
	AlreadyLive int
	// Overdue is how many of the scanned runs are already past the grace
	// window. See the gauge discussion in Work: this is a LOWER BOUND.
	Overdue int
	// Capped reports that the scan filled dueRunScanLimit, so Due is a floor
	// and not a total.
	Capped bool
	// Skipped reports that another replica held the lock and this pass did
	// nothing at all. Distinct from a pass that ran and found nothing.
	Skipped bool
}

// Work runs one sweeper pass.
//
// Errors are logged and swallowed rather than returned, matching ReaperWorker:
// failing the periodic job would only retry the identical scan sooner, and a
// dead-lettered sweeper is a silently absent safety net.
func (w *SweepWorker) Work(ctx context.Context, _ *river.Job[SweepDueRunsArgs]) error {
	stats, err := w.sweep(ctx)
	if err != nil {
		w.logger.Warn("update dispatch sweeper: pass failed", slog.Any("error", err))
		return nil
	}
	if stats.Skipped {
		return nil
	}

	// SIGNAL (b), the heartbeat. Emitted on EVERY completed pass, including the
	// overwhelmingly common one that finds nothing, because THE ABSENCE OF THIS
	// LINE IS THE POINT. A dispatcher that has stopped entirely produces no
	// error and no alert; it produces silence, and silence is only detectable
	// against a line you expected to see. Mirrors backup_scheduler's
	// "due candidates found".
	w.logger.Info("update dispatch sweeper: pass complete",
		slog.Int("due", stats.Due),
		slog.Int("enqueued", stats.Enqueued),
		slog.Int("already_live", stats.AlreadyLive),
		slog.Int("overdue", stats.Overdue),
		slog.Bool("capped", stats.Capped))

	// SIGNAL (a), and it is the one worth the most. Due work found, and not one
	// job enqueued and not one already live — meaning the sweeper saw runs that
	// should have had a trigger and neither found nor created one.
	//
	// THIS EXACT SIGNATURE IS HOW BOTH OF THIS CODEBASE'S PREVIOUS SILENT
	// SCHEDULER DEFECTS PRESENTED (#96's backup_schedules policy, and the
	// missing update_tasks_agent policy — both post-mortems are written into
	// schema.sql). In both, a scheduler ran happily forever, found rows or
	// found none, and did nothing, for every install, with no error anywhere.
	//
	// The condition is deliberately "nothing happened at all" rather than
	// "enqueued == 0". A pass where every due run already has a live job is the
	// HEALTHY steady state and must stay quiet: a warning that fires on correct
	// work gets filtered, and then it warns about nothing.
	if stats.Due > 0 && stats.Enqueued == 0 && stats.AlreadyLive == 0 {
		w.logger.Warn("update dispatch sweeper: due runs found but none dispatched or already live; the dispatcher may not be running",
			slog.Int("due", stats.Due),
			slog.Int("overdue", stats.Overdue))
	}

	// SIGNAL (c), the overdue gauge. After a correct pass this is structurally
	// zero: anything past the grace window should have been expired by its own
	// dispatch job. A non-zero value means jobs are not running at all, which is
	// precisely the failure the grace-window logic cannot see from inside — that
	// logic only runs when something fires it.
	//
	// It is a LOWER BOUND, not a total, because the scan is capped. Named
	// "at_least" in the log so nobody reads a bounded number as a complete one.
	if stats.Overdue > 0 {
		w.logger.Warn("update dispatch sweeper: runs are past their grace window and still scheduled; dispatch jobs are not running",
			slog.Int("overdue_at_least", stats.Overdue),
			slog.String("grace_window", dispatchGraceWindow.String()))
	}

	// SIGNAL (d)'s other half. A silent cap reads as "covered everything" when
	// it did not, so say so and say what to conclude.
	if stats.Capped {
		w.logger.Warn("update dispatch sweeper: pass hit its per-pass bound; more due runs remain",
			slog.Int("limit", dueRunScanLimit),
			slog.String("note", "the remainder is picked up by the next pass, oldest first"))
	}
	return nil
}

// sweep is Work without the logging, so tests can assert on the outcome
// directly instead of on log lines.
func (w *SweepWorker) sweep(ctx context.Context) (SweepStats, error) {
	var stats SweepStats

	if w.enq == nil {
		// A boot that never called SetEnqueuer. Loud, and specifically NOT a
		// swallowed nil: a safety net that silently does nothing is worse than
		// no safety net, because the heartbeat below would still print a
		// reassuring "pass complete" line every minute while nothing was ever
		// re-enqueued.
		return stats, fmt.Errorf("update dispatch sweeper: no enqueuer is wired; the safety net is inert")
	}

	// Single-flight across replicas. The house pattern (backup ScheduleWorker):
	// a session-level advisory lock on a pinned connection, released explicitly.
	//
	// Losing is NORMAL and returns quietly. Note the lock is an efficiency
	// guard, not the correctness one — two replicas sweeping at once would
	// still produce one dispatch each, because River's unique constraint on the
	// insert is what actually prevents the double. The lock keeps them from
	// doing the same scan twice.
	if w.pool != nil {
		conn, err := w.pool.Acquire(ctx)
		if err != nil {
			w.logger.Warn("update dispatch sweeper: could not acquire a connection for the advisory lock, skipping pass",
				slog.Any("error", err))
			stats.Skipped = true
			return stats, nil
		}
		defer conn.Release()

		var got bool
		if serr := conn.QueryRow(ctx,
			`SELECT pg_try_advisory_lock(hashtext($1))`, sweepLockKey).Scan(&got); serr != nil {
			// Proceed unlocked: the unique constraint on the insert is the real
			// guard, so the worst case is duplicated scan work.
			w.logger.Warn("update dispatch sweeper: advisory lock query failed, proceeding without it",
				slog.Any("error", serr))
		} else if !got {
			w.logger.Info("update dispatch sweeper: another replica holds the lock, skipping pass")
			stats.Skipped = true
			return stats, nil
		} else {
			defer func() {
				_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext($1))`, sweepLockKey)
			}()
		}
	}

	// THE CONSUMER ListDueRuns WAS BUILT FOR. Bounded per pass, so one tick can
	// never be unbounded; ListDueUpdateRuns orders by scheduled_at ASC, so a
	// backlog drains oldest-first and the remainder is next pass's.
	due, err := w.repo.ListDueRuns(ctx, dueRunScanLimit)
	if err != nil {
		return stats, err
	}
	stats.Due = len(due)
	stats.Capped = len(due) >= dueRunScanLimit

	graceCutoff := w.now().Add(-dispatchGraceWindow)
	for _, run := range due {
		if run.ScheduledAt != nil && run.ScheduledAt.Before(graceCutoff) {
			stats.Overdue++
		}

		// Enqueue a dispatch job for NOW. Idempotency is River's:
		// EnqueueDispatch inserts with UniqueOpts{ByArgs}, and DispatchRunArgs
		// is (tenant_id, run_id), so a run that already has a pending,
		// scheduled, available, running or retryable dispatch job gets no
		// second one. Re-enqueueing a live job would double-dispatch — which
		// the run CAS would survive, but a safety net should not be leaning on
		// the thing it exists to back up.
		inserted, eerr := w.enq.EnqueueDispatchIfAbsent(ctx, run.TenantID, run.ID, w.now())
		if eerr != nil {
			// One run's failure must not abandon the rest of the pass.
			w.logger.Warn("update dispatch sweeper: failed to enqueue a dispatch job",
				slog.String("run_id", run.ID.String()),
				slog.String("tenant_id", run.TenantID.String()),
				slog.Any("error", eerr))
			continue
		}
		if inserted {
			stats.Enqueued++
			w.logger.Warn("update dispatch sweeper: re-enqueued a due run that had no dispatch job",
				slog.String("run_id", run.ID.String()),
				slog.String("tenant_id", run.TenantID.String()))
		} else {
			stats.AlreadyLive++
		}
	}
	return stats, nil
}

// SweepEnqueuer is the sweeper's half of the enqueuer contract, kept separate
// from DispatchEnqueuer because the two want opposite things from a duplicate.
//
// Create-time enqueue wants the job inserted, full stop. The sweeper wants it
// inserted ONLY IF ABSENT, and needs to be told which happened — "already live"
// is the healthy steady state it must stay quiet about, and "inserted" is a
// safety net catching a real miss, which is worth a warning every time.
type SweepEnqueuer interface {
	// EnqueueDispatchIfAbsent inserts a dispatch job for a run that has none,
	// reporting whether it actually inserted one. False means a live job
	// already exists and nothing was done.
	EnqueueDispatchIfAbsent(ctx context.Context, tenantID, runID uuid.UUID, at time.Time) (bool, error)
}
