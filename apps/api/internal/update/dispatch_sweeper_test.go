package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// GH #463 Phase 2 — the sweeper and its four signals.
//
// The signals get as much attention here as the sweeping does, on purpose. The
// sweeper working is worth little if nobody finds out when it stops, and a
// warning that fires on healthy work is worse than no warning at all, because
// it gets filtered and then it signals nothing. Every signal test therefore has
// a matching over-fire test.

// sweepFakeRepo serves a fixed set of due runs.
type sweepFakeRepo struct {
	dispatchFakeRepo
	due     []Run
	dueErr  error
	dueCall int
}

func (f *sweepFakeRepo) ListDueRuns(context.Context, int32) ([]Run, error) {
	f.dueCall++
	return f.due, f.dueErr
}

// sweepFakeEnqueuer records dispatch inserts and can report a run as already
// having a live job, which is what River's unique constraint does in
// production.
type sweepFakeEnqueuer struct {
	mu       sync.Mutex
	inserted []uuid.UUID
	// live is the set of runs that already have a job; an insert for one of
	// these reports false, exactly as UniqueSkippedAsDuplicate does.
	live map[uuid.UUID]bool
	err  error
}

func (e *sweepFakeEnqueuer) EnqueueDispatchIfAbsent(_ context.Context, _, runID uuid.UUID, _ time.Time) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return false, e.err
	}
	if e.live[runID] {
		return false, nil
	}
	if e.live == nil {
		e.live = map[uuid.UUID]bool{}
	}
	// First insert wins; a concurrent second sees it as live. This is the
	// property River's unique constraint provides, modelled here so the
	// two-replica test means something.
	e.live[runID] = true
	e.inserted = append(e.inserted, runID)
	return true, nil
}

func (e *sweepFakeEnqueuer) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.inserted)
}

// captureLogs returns a logger writing JSON to buf, so a test can assert on
// what was actually emitted rather than on an internal flag. A signal nobody
// can see in the logs is not a signal.
func captureLogs() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// logHasWarn reports whether any emitted record at WARN or above contains sub.
func logHasWarn(t *testing.T, buf *bytes.Buffer, sub string) bool {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		lvl, _ := rec["level"].(string)
		msg, _ := rec["msg"].(string)
		if (lvl == "WARN" || lvl == "ERROR") && strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}

func scheduledRunAt(at time.Time) Run {
	return Run{ID: uuid.New(), TenantID: uuid.New(), Status: RunScheduled, ScheduledAt: &at}
}

func newSweeper(repo Repo, enq SweepEnqueuer, now time.Time) (*SweepWorker, *bytes.Buffer) {
	logger, buf := captureLogs()
	// nil pool: no advisory lock. The lock is an efficiency guard, not the
	// correctness one, so its absence changes no outcome asserted here.
	w := NewSweepWorker(repo, enq, nil, logger)
	w.SetClock(func() time.Time { return now })
	return w, buf
}

// TestSweeperReEnqueuesARunWithNoLiveJob is the safety net doing its job: a run
// committed without a job — the enqueue that never materialised — gets one.
func TestSweeperReEnqueuesARunWithNoLiveJob(t *testing.T) {
	now := time.Now()
	orphan := scheduledRunAt(now.Add(-time.Minute))
	repo := &sweepFakeRepo{due: []Run{orphan}}
	enq := &sweepFakeEnqueuer{}
	w, _ := newSweeper(repo, enq, now)

	stats, err := w.sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if stats.Enqueued != 1 || stats.AlreadyLive != 0 {
		t.Errorf("stats = %+v, want 1 enqueued and 0 already-live", stats)
	}
	if enq.count() != 1 || enq.inserted[0] != orphan.ID {
		t.Errorf("inserted %v, want exactly the orphaned run %s", enq.inserted, orphan.ID)
	}
}

// TestSweeperDoesNotReEnqueueARunThatAlreadyHasAJob is the idempotency
// requirement. Re-enqueueing a live job would double-dispatch — which the run
// CAS would survive, but a safety net must not lean on the thing it exists to
// back up.
func TestSweeperDoesNotReEnqueueARunThatAlreadyHasAJob(t *testing.T) {
	now := time.Now()
	healthy := scheduledRunAt(now.Add(-time.Minute))
	repo := &sweepFakeRepo{due: []Run{healthy}}
	enq := &sweepFakeEnqueuer{live: map[uuid.UUID]bool{healthy.ID: true}}
	w, buf := newSweeper(repo, enq, now)

	stats, err := w.sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if stats.Enqueued != 0 || stats.AlreadyLive != 1 {
		t.Errorf("stats = %+v, want 0 enqueued and 1 already-live", stats)
	}
	if enq.count() != 0 {
		t.Errorf("the sweeper inserted %d duplicate jobs for a run that already had one", enq.count())
	}
	_ = buf
}

// TestSweeperTwoReplicasProduceOneDispatch. The advisory lock is skipped here
// (nil pool) deliberately, so what is under test is the guard that actually
// carries the guarantee: the unique insert. If the lock were the only thing
// preventing a double, this test would fail — which is the point of removing it.
func TestSweeperTwoReplicasProduceOneDispatch(t *testing.T) {
	now := time.Now()
	orphan := scheduledRunAt(now.Add(-time.Minute))
	enq := &sweepFakeEnqueuer{} // shared, as one database would be

	const replicas = 4
	var wg sync.WaitGroup
	start := make(chan struct{})
	stats := make([]SweepStats, replicas)
	errs := make([]error, replicas)
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			repo := &sweepFakeRepo{due: []Run{orphan}}
			w, _ := newSweeper(repo, enq, now)
			<-start
			stats[i], errs[i] = w.sweep(context.Background())
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("replica %d: %v", i, err)
		}
	}
	if enq.count() != 1 {
		t.Errorf("%d replicas produced %d dispatch jobs for one run, want 1", replicas, enq.count())
	}
	enqueued := 0
	for _, s := range stats {
		enqueued += s.Enqueued
	}
	if enqueued != 1 {
		t.Errorf("replicas reported %d enqueues in total, want 1", enqueued)
	}
}

// TestSweeperSilentSchedulerSignalFires is signal (a), the one worth the most.
//
// The condition it detects is: the sweeper found due work and neither dispatched
// anything nor found anything already live. That is the exact signature of both
// previous silent-scheduler defects in this codebase — a scheduler that runs
// forever, finds rows, and does nothing, with no error anywhere.
func TestSweeperSilentSchedulerSignalFires(t *testing.T) {
	now := time.Now()
	repo := &sweepFakeRepo{due: []Run{
		scheduledRunAt(now.Add(-time.Minute)),
		scheduledRunAt(now.Add(-2 * time.Minute)),
	}}
	// Every insert fails, so nothing is enqueued and nothing is already live.
	enq := &sweepFakeEnqueuer{err: errors.New("queue unavailable")}
	w, buf := newSweeper(repo, enq, now)

	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if !logHasWarn(t, buf, "due runs found but none dispatched or already live") {
		t.Errorf("signal (a) did not fire on a pass with due work and no dispatch.\nlogs:\n%s", buf.String())
	}
}

// TestSweeperSilentSchedulerSignalDoesNotOverFire is the half that keeps the
// signal usable. A warning that cries on correct work gets filtered out, and
// then it warns about nothing.
//
// Three healthy shapes, none of which may warn:
//
//	nothing due            the overwhelmingly common pass.
//	all already live       the healthy steady state — every due run has its own
//	                       job, which is what the primary path produces.
//	all newly enqueued     the safety net working exactly as intended.
func TestSweeperSilentSchedulerSignalDoesNotOverFire(t *testing.T) {
	now := time.Now()
	// Just-due runs, well inside the grace window, so signal (c) cannot fire
	// and confuse the assertion.
	a := scheduledRunAt(now.Add(-time.Minute))
	b := scheduledRunAt(now.Add(-2 * time.Minute))

	cases := []struct {
		name string
		due  []Run
		live map[uuid.UUID]bool
	}{
		{"nothing due", nil, nil},
		{"every due run already has a job", []Run{a, b}, map[uuid.UUID]bool{a.ID: true, b.ID: true}},
		{"every due run was newly enqueued", []Run{a, b}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &sweepFakeRepo{due: tc.due}
			enq := &sweepFakeEnqueuer{live: tc.live}
			w, buf := newSweeper(repo, enq, now)

			if err := w.Work(context.Background(), nil); err != nil {
				t.Fatalf("Work: %v", err)
			}
			if logHasWarn(t, buf, "due runs found but none dispatched or already live") {
				t.Errorf("signal (a) fired on a HEALTHY pass (%s); a warning that fires on correct work gets filtered and then signals nothing.\nlogs:\n%s", tc.name, buf.String())
			}
		})
	}
}

// TestSweeperHeartbeatIsEmittedOnEveryPass is signal (b). The absence of this
// line is how a dead dispatcher becomes visible, so it must appear even on the
// empty pass — the case a "only log when there is something to say" instinct
// would remove, taking the detector with it.
// TestSweeperSkippedPassStillHeartbeatsButSignalsNothing pins both halves of
// the F1 hardening.
//
// A pass that yielded to a peer's advisory lock is a LIVING sweeper: it ticked,
// took its turn and stood down. Staying silent would make a healthy
// multi-replica install indistinguishable from a dead one — which is precisely
// the state an un-released session lock produces, where every subsequent pass
// skips forever and the detector goes quiet along with it.
//
// But its counters measured NOTHING. Due == 0 there means "did not look", not
// "looked and found none", so the three measurement signals must stay silent:
// treating an absent observation as a healthy one is the same mistake as
// inferring run completion from a partial view.
func TestSweeperSkippedPassStillHeartbeatsButSignalsNothing(t *testing.T) {
	now := time.Now()
	logger, buf := captureLogs()
	w := NewSweepWorker(&sweepFakeRepo{}, &sweepFakeEnqueuer{}, nil, logger)
	w.SetClock(func() time.Time { return now })

	// A skipped pass, exactly as sweep() reports one after losing the advisory
	// lock: every counter zero, because it never looked.
	w.report(SweepStats{Skipped: true, Due: 3, Overdue: 2, Capped: true})

	if !strings.Contains(buf.String(), "pass complete") {
		t.Errorf("a skipped pass emitted no heartbeat; a healthy multi-replica install would look identical to a dead sweeper.\nlogs:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `"skipped":true`) {
		t.Errorf("the heartbeat does not mark the pass skipped, so a reader cannot tell a yielded pass from a completed one.\nlogs:\n%s", buf.String())
	}
	for _, sig := range []string{
		"due runs found but none dispatched",
		"past their grace window",
		"hit its per-pass bound",
	} {
		if logHasWarn(t, buf, sig) {
			t.Errorf("a SKIPPED pass emitted the %q signal; its zeroes mean 'did not look', not 'looked and found none'.\nlogs:\n%s", sig, buf.String())
		}
	}
}

func TestSweeperHeartbeatIsEmittedOnEveryPass(t *testing.T) {
	now := time.Now()
	repo := &sweepFakeRepo{due: nil}
	w, buf := newSweeper(repo, &sweepFakeEnqueuer{}, now)

	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if !strings.Contains(buf.String(), "pass complete") {
		t.Errorf("no heartbeat on an empty pass; a stopped dispatcher would be indistinguishable from a quiet one.\nlogs:\n%s", buf.String())
	}
	for _, field := range []string{"due", "enqueued", "already_live", "overdue"} {
		if !strings.Contains(buf.String(), `"`+field+`"`) {
			t.Errorf("heartbeat is missing the %q count", field)
		}
	}
}

// TestSweeperOverdueGaugeFiresAndDoesNotOverFire is signal (c). After a correct
// pass it is structurally zero: anything past the grace window should already
// have been expired by its own dispatch job. Non-zero means jobs are not running
// at all, which is the failure the grace-window logic cannot see from inside,
// because that logic only runs when something fires it.
func TestSweeperOverdueGaugeFiresAndDoesNotOverFire(t *testing.T) {
	now := time.Now()

	t.Run("fires past the window", func(t *testing.T) {
		repo := &sweepFakeRepo{due: []Run{scheduledRunAt(now.Add(-dispatchGraceWindow - time.Hour))}}
		w, buf := newSweeper(repo, &sweepFakeEnqueuer{}, now)
		stats, err := w.sweep(context.Background())
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if stats.Overdue != 1 {
			t.Errorf("overdue = %d, want 1", stats.Overdue)
		}
		_ = w.Work(context.Background(), nil)
		if !logHasWarn(t, buf, "past their grace window") {
			t.Errorf("the overdue gauge did not warn.\nlogs:\n%s", buf.String())
		}
	})

	t.Run("silent inside the window", func(t *testing.T) {
		repo := &sweepFakeRepo{due: []Run{scheduledRunAt(now.Add(-time.Minute))}}
		w, buf := newSweeper(repo, &sweepFakeEnqueuer{}, now)
		stats, err := w.sweep(context.Background())
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if stats.Overdue != 0 {
			t.Errorf("overdue = %d for a run one minute past its start, want 0", stats.Overdue)
		}
		if logHasWarn(t, buf, "past their grace window") {
			t.Errorf("the overdue gauge warned about a run well inside the grace window.\nlogs:\n%s", buf.String())
		}
	})
}

// TestSweeperReportsItsCap is signal (d). A silent cap reads as "covered
// everything" when it did not.
func TestSweeperReportsItsCap(t *testing.T) {
	now := time.Now()
	full := make([]Run, dueRunScanLimit)
	for i := range full {
		full[i] = scheduledRunAt(now.Add(-time.Minute))
	}
	repo := &sweepFakeRepo{due: full}
	w, buf := newSweeper(repo, &sweepFakeEnqueuer{}, now)

	stats, err := w.sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !stats.Capped {
		t.Error("a full scan did not report itself capped; the count reads as a total when it is a floor")
	}
	_ = w.Work(context.Background(), nil)
	if !logHasWarn(t, buf, "hit its per-pass bound") {
		t.Errorf("the cap was not logged.\nlogs:\n%s", buf.String())
	}

	// And the control: a short pass must not claim to be capped.
	short := &sweepFakeRepo{due: full[:2]}
	w2, buf2 := newSweeper(short, &sweepFakeEnqueuer{}, now)
	stats2, err := w2.sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if stats2.Capped {
		t.Error("a two-run pass reported itself capped")
	}
	_ = w2.Work(context.Background(), nil)
	if logHasWarn(t, buf2, "hit its per-pass bound") {
		t.Errorf("a short pass logged a cap warning.\nlogs:\n%s", buf2.String())
	}
}

// TestSweeperWithoutAnEnqueuerIsLoud. An inert safety net is worse than none:
// the heartbeat would still print a reassuring "pass complete" every minute
// while nothing was ever re-enqueued.
func TestSweeperWithoutAnEnqueuerIsLoud(t *testing.T) {
	now := time.Now()
	repo := &sweepFakeRepo{due: []Run{scheduledRunAt(now.Add(-time.Minute))}}
	logger, buf := captureLogs()
	w := NewSweepWorker(repo, nil, nil, logger)
	w.SetClock(func() time.Time { return now })

	if _, err := w.sweep(context.Background()); err == nil {
		t.Error("sweep succeeded with no enqueuer wired; the safety net is inert and says nothing")
	}
	_ = w.Work(context.Background(), nil)
	if !logHasWarn(t, buf, "pass failed") {
		t.Errorf("an inert sweeper did not warn.\nlogs:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "pass complete") {
		t.Error("an inert sweeper emitted the healthy heartbeat, which is the reassuring-silence failure this guard exists to prevent")
	}
}

// TestSweeperOneRunFailureDoesNotAbandonThePass. A single run's enqueue failure
// must not cost the rest of the backlog, for the same reason one busy target
// must not fail a whole dispatch.
func TestSweeperOneRunFailureDoesNotAbandonThePass(t *testing.T) {
	now := time.Now()
	runs := []Run{
		scheduledRunAt(now.Add(-3 * time.Minute)),
		scheduledRunAt(now.Add(-2 * time.Minute)),
		scheduledRunAt(now.Add(-time.Minute)),
	}
	repo := &sweepFakeRepo{due: runs}
	enq := &failFirstEnqueuer{failFor: runs[0].ID}
	w, _ := newSweeper(repo, enq, now)

	stats, err := w.sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep returned an error for one failed run: %v", err)
	}
	if stats.Enqueued != 2 {
		t.Errorf("enqueued %d of 3 runs after one failure, want 2: one run's failure abandoned the pass", stats.Enqueued)
	}
}

type failFirstEnqueuer struct {
	failFor uuid.UUID
	n       int
}

func (e *failFirstEnqueuer) EnqueueDispatchIfAbsent(_ context.Context, _, runID uuid.UUID, _ time.Time) (bool, error) {
	if runID == e.failFor {
		return false, errors.New("simulated enqueue failure")
	}
	e.n++
	return true, nil
}
