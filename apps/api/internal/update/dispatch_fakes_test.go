package update

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The GH #463 deferred-dispatch half of the Repo interface, implemented for the
// package's three existing fakes.
//
// They live in one file rather than being scattered into the three test files
// that declare their receivers so that widening Repo again touches one place,
// and so a reader looking for "what does the scheduled path expect of a repo"
// finds it all together. Only fakeCreateRepo implements anything for real: it
// is the one the service-level scheduling tests drive.

// --- fakeCreateRepo (service_test.go): the scheduled create is REAL ---------

// CreateScheduledRunWithTasks mirrors pgRepo's: run and tasks born
// 'scheduled', and — unlike the immediate path — dropOnInsert is NOT consulted,
// because a scheduled task does not enter update_tasks_inflight_target_idx and
// therefore cannot lose an insert to it. That asymmetry is the behaviour under
// test, so the fake must reproduce it rather than share one body with the
// immediate path.
func (f *fakeCreateRepo) CreateScheduledRunWithTasks(_ context.Context, in CreateRunInput, tasks []NewTask) (Run, []Task, error) {
	run := Run{ID: uuid.New(), TenantID: in.TenantID, Status: RunScheduled, DryRun: in.DryRun, ScheduledAt: in.ScheduledAt}
	out := make([]Task, 0, len(tasks))
	for _, nt := range tasks {
		t := Task{
			ID:             uuid.New(),
			RunID:          run.ID,
			TenantID:       in.TenantID,
			SiteID:         nt.SiteID,
			TargetType:     nt.TargetType,
			TargetSlug:     nt.TargetSlug,
			DesiredVersion: nt.DesiredVersion,
			FromVersion:    nt.FromVersion,
			Status:         TaskScheduled,
		}
		out = append(out, t)
		f.tasks = append(f.tasks, t)
	}
	return run, out, nil
}

func (f *fakeCreateRepo) ListDueRuns(context.Context, int32) ([]Run, error) {
	panic("not implemented")
}

func (f *fakeCreateRepo) DispatchDueRun(context.Context, TxEnqueuer, Run) (DispatchOutcome, error) {
	panic("not implemented")
}

func (f *fakeCreateRepo) ExpireDueRun(context.Context, uuid.UUID, uuid.UUID, time.Time, string) (bool, int, error) {
	panic("not implemented")
}

// --- probeFakeRepo (worker_test.go) ---------------------------------------

func (f *probeFakeRepo) CreateScheduledRunWithTasks(context.Context, CreateRunInput, []NewTask) (Run, []Task, error) {
	panic("not implemented")
}

func (f *probeFakeRepo) ListDueRuns(context.Context, int32) ([]Run, error) {
	panic("not implemented")
}

func (f *probeFakeRepo) DispatchDueRun(context.Context, TxEnqueuer, Run) (DispatchOutcome, error) {
	panic("not implemented")
}

func (f *probeFakeRepo) ExpireDueRun(context.Context, uuid.UUID, uuid.UUID, time.Time, string) (bool, int, error) {
	panic("not implemented")
}

// --- fakeRepo (handler_test.go) -------------------------------------------

func (f *fakeRepo) CreateScheduledRunWithTasks(context.Context, CreateRunInput, []NewTask) (Run, []Task, error) {
	panic("not used")
}

func (f *fakeRepo) ListDueRuns(context.Context, int32) ([]Run, error) {
	panic("not used")
}

func (f *fakeRepo) DispatchDueRun(context.Context, TxEnqueuer, Run) (DispatchOutcome, error) {
	panic("not used")
}

func (f *fakeRepo) ExpireDueRun(context.Context, uuid.UUID, uuid.UUID, time.Time, string) (bool, int, error) {
	panic("not used")
}

// --- a recording DispatchEnqueuer ------------------------------------------

// fakeDispatchEnqueuer records the dispatch jobs a Service asked for, so a test
// can assert that a deferred run enqueued EXACTLY ONE dispatch job at its
// scheduled_at and NO per-task jobs — the property that makes "contacts no site
// until its time" true.
type fakeDispatchEnqueuer struct {
	calls []dispatchCall
	err   error
}

type dispatchCall struct {
	tenantID uuid.UUID
	runID    uuid.UUID
	at       time.Time
}

func (f *fakeDispatchEnqueuer) EnqueueDispatch(_ context.Context, tenantID, runID uuid.UUID, at time.Time) error {
	f.calls = append(f.calls, dispatchCall{tenantID: tenantID, runID: runID, at: at})
	return f.err
}

// fakeTxEnqueuer satisfies TxEnqueuer without a database. Never invoked by the
// service-level tests; present so a DispatchOutcome can be constructed in a
// worker test without a River client.
type fakeTxEnqueuer struct{ n int }

func (f *fakeTxEnqueuer) EnqueueTaskTx(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, uuid.UUID, bool) error {
	f.n++
	return nil
}

// fixedClock is a domain.Clock returning a pinned instant, so schedule
// validation is tested against a time that does not move under it.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }
