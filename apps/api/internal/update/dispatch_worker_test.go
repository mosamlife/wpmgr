package update

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// GH #463 — the DispatchWorker layer.
//
// Nothing but main constructed a DispatchWorker before this file, so the
// fire-time ROUTING was unexecuted: the grace-window decision itself, the
// not-yet-due case, the missing-enqueuer refusal and the nil-scheduled_at
// guard. The integration suite proves what repo.ExpireDueRun does once called
// with a cutoff; these prove which of the two arms actually gets called, which
// is the half an operator's "no site was contacted" guarantee rests on.

// dispatchFakeRepo is a Repo that serves one run and records what the worker
// asked of it. Only the four methods the dispatch path touches do anything.
type dispatchFakeRepo struct {
	run      Run
	getErr   error
	getCalls int

	dispatchCalls int
	dispatchOut   DispatchOutcome
	dispatchErr   error

	expireCalls  int
	expireCutoff time.Time
	expired      bool
	expireTasks  int

	// listDueCalls counts uses of the cross-tenant scan. The dispatch path must
	// never touch it: a per-run job that reaches for a bounded scan is the
	// stranding defect this file's headline test exists to prevent.
	listDueCalls int
}

func (f *dispatchFakeRepo) GetRun(_ context.Context, _, _ uuid.UUID) (Run, error) {
	f.getCalls++
	if f.getErr != nil {
		return Run{}, f.getErr
	}
	return f.run, nil
}

func (f *dispatchFakeRepo) ListDueRuns(context.Context, int32) ([]Run, error) {
	f.listDueCalls++
	return nil, nil
}

func (f *dispatchFakeRepo) DispatchDueRun(context.Context, TxEnqueuer, Run) (DispatchOutcome, error) {
	f.dispatchCalls++
	return f.dispatchOut, f.dispatchErr
}

func (f *dispatchFakeRepo) ExpireDueRun(_ context.Context, _, _ uuid.UUID, cutoff time.Time, _ string) (bool, int, error) {
	f.expireCalls++
	f.expireCutoff = cutoff
	return f.expired, f.expireTasks, nil
}

// The rest of Repo, unused by the dispatch path.
func (f *dispatchFakeRepo) CreateRunWithTasks(context.Context, CreateRunInput, []NewTask) (Run, []Task, error) {
	panic("not used")
}
func (f *dispatchFakeRepo) CreateScheduledRunWithTasks(context.Context, CreateRunInput, []NewTask) (Run, []Task, error) {
	panic("not used")
}
func (f *dispatchFakeRepo) ListRuns(context.Context, uuid.UUID, int32, int32) ([]Run, error) {
	panic("not used")
}
func (f *dispatchFakeRepo) ListRunSummaries(context.Context, uuid.UUID, int32, int32) ([]RunSummary, error) {
	panic("not used")
}
func (f *dispatchFakeRepo) ListTasks(context.Context, uuid.UUID, uuid.UUID) ([]Task, error) {
	panic("not used")
}
func (f *dispatchFakeRepo) GetTask(context.Context, uuid.UUID, uuid.UUID) (Task, error) {
	panic("not used")
}
func (f *dispatchFakeRepo) MarkTaskRunning(context.Context, uuid.UUID, uuid.UUID, time.Duration) (Task, error) {
	panic("not used")
}
func (f *dispatchFakeRepo) FinishTask(context.Context, FinishTaskInput) (Task, error) {
	panic("not used")
}
func (f *dispatchFakeRepo) SetRunStatus(context.Context, uuid.UUID, uuid.UUID, string) (Run, error) {
	panic("not used")
}
func (f *dispatchFakeRepo) CountUnfinishedTasks(context.Context, uuid.UUID, uuid.UUID) (int64, error) {
	panic("not used")
}
func (f *dispatchFakeRepo) CountRunningTasksForTenant(context.Context, uuid.UUID) (int64, error) {
	panic("not used")
}
func (f *dispatchFakeRepo) ListInFlightTargets(context.Context, uuid.UUID, []uuid.UUID) (map[InFlightKey]struct{}, error) {
	panic("not used")
}
func (f *dispatchFakeRepo) ListStaleUpdateTasks(context.Context, time.Duration, int32) ([]Task, error) {
	panic("not used")
}
func (f *dispatchFakeRepo) SiteHasRunningTask(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, time.Duration) (bool, error) {
	panic("not used")
}
func (f *dispatchFakeRepo) DeferTaskToPending(context.Context, DeferTaskInput) (Task, error) {
	panic("not used")
}

func newDispatchWorkerForTest(repo Repo, now time.Time) (*DispatchWorker, *fakeTxEnqueuer) {
	enq := &fakeTxEnqueuer{}
	w := NewDispatchWorker(repo, enq, nil, slog.New(slog.DiscardHandler))
	w.SetClock(func() time.Time { return now })
	return w, enq
}

func dispatchJob(tenant, runID uuid.UUID) *river.Job[DispatchRunArgs] {
	return &river.Job[DispatchRunArgs]{Args: DispatchRunArgs{TenantID: tenant, RunID: runID}}
}

func scheduledRun(tenant uuid.UUID, at time.Time) Run {
	return Run{ID: uuid.New(), TenantID: tenant, Status: RunScheduled, ScheduledAt: &at}
}

// TestDispatchWorkerReadsItsOwnRunAndNeverScans is the regression for the
// stranding defect, and it is the reason this file exists.
//
// The worker used to locate its run by filtering repo.ListDueRuns, which is
// bounded at dueRunScanLimit. A run outside the oldest N due runs never
// appeared in its OWN job's scan; the worker returned nil, River completed the
// job, and nothing ever found the run again — still 'scheduled', still in the
// due index, but the only reader of that index is a dispatch job and this run's
// job had been consumed. It could not even expire. Recovery was manual DB
// surgery.
//
// Asserting listDueCalls == 0 is what pins the fix: any future edit that
// reaches for the scan from the per-run path reintroduces a limit the run can
// fall out of.
func TestDispatchWorkerReadsItsOwnRunAndNeverScans(t *testing.T) {
	now := time.Now()
	tenant := uuid.New()
	run := scheduledRun(tenant, now.Add(-time.Minute))
	repo := &dispatchFakeRepo{run: run, dispatchOut: DispatchOutcome{Claimed: true, Dispatched: 2, Status: RunRunning}}

	w, _ := newDispatchWorkerForTest(repo, now)
	if err := w.Work(context.Background(), dispatchJob(tenant, run.ID)); err != nil {
		t.Fatalf("Work: %v", err)
	}

	if repo.getCalls != 1 {
		t.Errorf("GetRun called %d times, want 1", repo.getCalls)
	}
	if repo.listDueCalls != 0 {
		t.Errorf("the per-run dispatch path called the bounded due scan %d times, want 0: a run outside the scan's page would be stranded forever with no way back", repo.listDueCalls)
	}
	if repo.dispatchCalls != 1 {
		t.Errorf("DispatchDueRun called %d times, want 1", repo.dispatchCalls)
	}
	if repo.expireCalls != 0 {
		t.Errorf("a run one minute past its start was expired; want dispatched")
	}
}

// TestDispatchWorkerGraceWindowRouting is the fire-time decision itself: which
// of the two arms gets called, for a run at each interesting age.
//
// The integration suite proves what ExpireDueRun DOES once called with a
// cutoff. It calls it directly, with a cutoff the test computes, so the routing
// that decides to call it at all was proven nowhere. This is that half — and it
// is the half the operator's "no site was contacted" guarantee rests on, since
// a run wrongly routed to dispatch contacts every site in it.
func TestDispatchWorkerGraceWindowRouting(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name         string
		scheduledAt  time.Time
		wantExpire   bool
		wantDispatch bool
		wantSnooze   bool
	}{
		{"just due", now.Add(-time.Second), false, true, false},
		{"late but inside the window", now.Add(-dispatchGraceWindow + time.Minute), false, true, false},
		{"exactly at the window edge", now.Add(-dispatchGraceWindow), false, true, false},
		{"past the window", now.Add(-dispatchGraceWindow - time.Minute), true, false, false},
		{"hours past the window", now.Add(-12 * time.Hour), true, false, false},
		{"not yet due", now.Add(10 * time.Minute), false, false, true},
		{"within the early-fire tolerance", now.Add(dispatchEarlyFireTolerance / 2), false, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenant := uuid.New()
			run := scheduledRun(tenant, tc.scheduledAt)
			repo := &dispatchFakeRepo{
				run:         run,
				expired:     true,
				expireTasks: 1,
				dispatchOut: DispatchOutcome{Claimed: true, Dispatched: 1, Status: RunRunning},
			}
			w, _ := newDispatchWorkerForTest(repo, now)

			err := w.Work(context.Background(), dispatchJob(tenant, run.ID))

			var snoozed *river.JobSnoozeError
			gotSnooze := errors.As(err, &snoozed)
			if !gotSnooze && err != nil {
				t.Fatalf("Work: %v", err)
			}
			if gotSnooze != tc.wantSnooze {
				t.Errorf("snoozed = %v, want %v", gotSnooze, tc.wantSnooze)
			}
			if (repo.expireCalls > 0) != tc.wantExpire {
				t.Errorf("expired = %v, want %v", repo.expireCalls > 0, tc.wantExpire)
			}
			if (repo.dispatchCalls > 0) != tc.wantDispatch {
				t.Errorf("dispatched = %v, want %v", repo.dispatchCalls > 0, tc.wantDispatch)
			}
			// The clause an operator cares about: an expired run contacts
			// nothing. Routing to expire and to dispatch must be exclusive.
			if repo.expireCalls > 0 && repo.dispatchCalls > 0 {
				t.Error("the same run was BOTH expired and dispatched; an expired run must contact no site")
			}
			if tc.wantExpire && !repo.expireCutoff.Equal(now.Add(-dispatchGraceWindow)) {
				t.Errorf("expiry cutoff = %v, want now-%v", repo.expireCutoff, dispatchGraceWindow)
			}
		})
	}
}

// TestDispatchWorkerNotYetDueSnoozesRatherThanCompleting pins the specific
// failure mode the snooze exists to avoid.
//
// Returning nil for an early fire would COMPLETE the job — consuming the run's
// only trigger before its time, leaving it 'scheduled' with nothing that would
// ever fire it again. That is the same stranding as the scan-page miss, reached
// through a different door, so it gets its own assertion rather than riding on
// the routing table above.
func TestDispatchWorkerNotYetDueSnoozesRatherThanCompleting(t *testing.T) {
	now := time.Now()
	tenant := uuid.New()
	run := scheduledRun(tenant, now.Add(30*time.Minute))
	repo := &dispatchFakeRepo{run: run}
	w, _ := newDispatchWorkerForTest(repo, now)

	err := w.Work(context.Background(), dispatchJob(tenant, run.ID))

	var snoozed *river.JobSnoozeError
	if !errors.As(err, &snoozed) {
		t.Fatalf("Work returned %v for a not-yet-due run; it must SNOOZE. Returning nil completes the job and consumes the run's only trigger before its time", err)
	}
	if repo.dispatchCalls != 0 || repo.expireCalls != 0 {
		t.Errorf("a not-yet-due run was acted on: dispatch=%d expire=%d", repo.dispatchCalls, repo.expireCalls)
	}
}

// TestDispatchWorkerRefusesWithoutAnEnqueuer proves the boot-order guard is
// loud. A nil enqueuer means SetTxEnqueuer was never called; returning nil there
// would mark every dispatch job complete while the runs never fired, and the
// feature would look like it had simply never been triggered.
func TestDispatchWorkerRefusesWithoutAnEnqueuer(t *testing.T) {
	now := time.Now()
	tenant := uuid.New()
	run := scheduledRun(tenant, now.Add(-time.Minute))
	repo := &dispatchFakeRepo{run: run}

	w := NewDispatchWorker(repo, nil, nil, slog.New(slog.DiscardHandler))
	w.SetClock(func() time.Time { return now })

	err := w.Work(context.Background(), dispatchJob(tenant, run.ID))
	if err == nil {
		t.Fatal("Work returned nil with no enqueuer wired; the job would be marked complete and the run would never fire")
	}
	if repo.getCalls != 0 || repo.dispatchCalls != 0 {
		t.Error("the worker touched the repo before noticing it could not enqueue")
	}
}

// TestDispatchWorkerLeavesNonScheduledRunsAlone covers the statuses a duplicate
// or late job can meet. Each is benign and none may be retried or acted on —
// most importantly 'expired', which must never be resurrected into a run that
// contacts sites hours after its window closed.
func TestDispatchWorkerLeavesNonScheduledRunsAlone(t *testing.T) {
	now := time.Now()
	for _, status := range []string{RunPending, RunRunning, RunCompleted, RunHalted, RunDispatching, RunExpired} {
		t.Run(status, func(t *testing.T) {
			tenant := uuid.New()
			at := now.Add(-time.Minute)
			run := Run{ID: uuid.New(), TenantID: tenant, Status: status, ScheduledAt: &at}
			repo := &dispatchFakeRepo{run: run}
			w, enq := newDispatchWorkerForTest(repo, now)

			if err := w.Work(context.Background(), dispatchJob(tenant, run.ID)); err != nil {
				t.Fatalf("Work on a %s run returned %v, want nil (benign, not retryable)", status, err)
			}
			if repo.dispatchCalls != 0 || repo.expireCalls != 0 {
				t.Errorf("a %s run was acted on: dispatch=%d expire=%d", status, repo.dispatchCalls, repo.expireCalls)
			}
			if enq.n != 0 {
				t.Errorf("a %s run enqueued %d task jobs, want 0: no site may be contacted", status, enq.n)
			}
		})
	}
}

// TestDispatchWorkerHandlesAMissingOrUnreadableRun separates the two error
// shapes, because they need opposite handling and confusing them is how work
// gets lost.
//
// A deleted run is done: nothing to dispatch, complete the job. An
// infrastructure error is NOT done: completing the job there consumes the run's
// only trigger over a transient database blip, which is the stranding shape
// again.
func TestDispatchWorkerHandlesAMissingOrUnreadableRun(t *testing.T) {
	now := time.Now()
	tenant := uuid.New()

	t.Run("deleted run completes the job", func(t *testing.T) {
		repo := &dispatchFakeRepo{getErr: domain.NotFound("run_not_found", "no such run")}
		w, _ := newDispatchWorkerForTest(repo, now)
		if err := w.Work(context.Background(), dispatchJob(tenant, uuid.New())); err != nil {
			t.Errorf("Work on a deleted run = %v, want nil", err)
		}
	})

	t.Run("infrastructure error retries", func(t *testing.T) {
		repo := &dispatchFakeRepo{getErr: errors.New("connection refused")}
		w, _ := newDispatchWorkerForTest(repo, now)
		if err := w.Work(context.Background(), dispatchJob(tenant, uuid.New())); err == nil {
			t.Error("Work swallowed an infrastructure error; River would complete the job and the run would never fire again")
		}
	})
}

// TestDispatchWorkerRefusesARunWithNoScheduledAt covers the should-not-exist
// row: a 'scheduled' run with a NULL scheduled_at, which
// CreateScheduledRunWithTasks refuses to create precisely because it could
// never become due.
//
// It asserts an ERROR, not nil, and that is the point of the test rather than
// an incidental detail. Reading the run by id made this branch reachable — while
// Work filtered the due scan, such a row could never arrive here at all. The row
// is stranded either way, since nothing can invent a start time the operator
// never gave, so the only thing left to choose is whether anyone finds out.
// Returning nil completes the job silently and leaves no trace; an error retries
// harmlessly and then dead-letters, which puts it in front of someone.
//
// Firing it is of course still forbidden: any action would guess at an intent
// that was never recorded.
func TestDispatchWorkerRefusesARunWithNoScheduledAt(t *testing.T) {
	now := time.Now()
	tenant := uuid.New()
	run := Run{ID: uuid.New(), TenantID: tenant, Status: RunScheduled, ScheduledAt: nil}
	repo := &dispatchFakeRepo{run: run}
	w, enq := newDispatchWorkerForTest(repo, now)

	err := w.Work(context.Background(), dispatchJob(tenant, run.ID))
	if err == nil {
		t.Error("Work returned nil for a run that can never become due; the job completes and the row is stranded with no trace at all")
	}
	if repo.dispatchCalls != 0 || repo.expireCalls != 0 || enq.n != 0 {
		t.Error("a run with no scheduled_at was fired; its start time is unknown, so any action guesses at the operator's intent")
	}
}

// TestIsUniqueViolation closes the 23505 gap named in the integration suite.
//
// The savepoint recovery in dispatchOneTask turns on this classification: a
// 23505 must be read as "the target is busy, skip this task", while any other
// error must abort. Getting it wrong in one direction fails a whole run over a
// contained collision; in the other, it silently swallows a real database
// error as a skip.
func TestIsUniqueViolation(t *testing.T) {
	if !isUniqueViolation(&pgconn.PgError{Code: "23505"}) {
		t.Error("a real 23505 was not recognised; a contained collision would fail the whole scheduled run")
	}
	if !isUniqueViolation(fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "23505"})) {
		t.Error("a WRAPPED 23505 was not recognised; the repo layer wraps its errors")
	}
	for _, code := range []string{"23503", "23514", "40001", "42P01"} {
		if isUniqueViolation(&pgconn.PgError{Code: code}) {
			t.Errorf("SQLSTATE %s was misread as a unique violation; a real database error would be silently recorded as a skipped task", code)
		}
	}
	if isUniqueViolation(errors.New("not a pg error")) {
		t.Error("a non-pg error was misread as a unique violation")
	}
	if isUniqueViolation(nil) {
		t.Error("nil was misread as a unique violation")
	}
}
