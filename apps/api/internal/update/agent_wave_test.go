package update

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Shared in-memory doubles for the agent self-update channel
// ---------------------------------------------------------------------------

// agentTaskID builds a UUID whose STRING form sorts by i. waveOrder breaks
// ties on the id string, and every task in a run shares one created_at (they
// are inserted by a single transaction, and Postgres gives every now() in a
// transaction the same value), so the id is what actually orders a run. Making
// that order match the array index is what lets these tests talk about "task
// 0" and mean the canary.
func agentTaskID(i int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-%012d", i))
}

// agentStore is a faithful in-memory stand-in for the two tables the wave
// machine writes: update_runs and update_tasks.
//
// It mirrors pgRepo's logic step for step and drives the SAME pure derivation
// (DeriveAgentWaveState), with a mutex where the repo takes
// pg_advisory_xact_lock(hashtext('update_agent_wave'), hashtext(run_id)) and a
// map where the repo has rows. So what these tests exercise is the state
// machine and the exactly-once discipline built on top of it. What they cannot
// exercise is Postgres itself holding that lock across two connections; that
// is the repo's own contract and is pinned by reading agent_repo.go, where
// every wave-machine transaction takes the lock as its first statement.
type agentStore struct {
	mu    sync.Mutex
	run   Run
	tasks []*Task

	// proceeds counts how many callers were ever handed ClaimProceed. Exactly
	// one per task is the whole point.
	proceeds map[uuid.UUID]int
	// haltTransitions counts calls that actually moved the run into halted,
	// as opposed to finding it already halted.
	haltTransitions int
	// firstHaltReason is the reason recorded by the call that performed the
	// halt transition.
	firstHaltReason string
}

// The plan-time record every seeded agent task carries, matching what
// planAgentTasks writes for a fleet on agentPlanFrom while agentPlanTarget is
// the published build:
//
//   - desired_version is the RESOLVED target, never the literal "latest". It is
//     the run's premise, and it is what an "up_to_date" answer is scored
//     against, so a fake that stored "latest" here could not exercise the rule
//     at all.
//   - from_version is what the site itself reported at plan time, which is what
//     makes "the site moved" a checkable statement later.
const (
	agentPlanFrom   = "0.61.80"
	agentPlanTarget = "0.62.0"
)

func newAgentStore(tenantID uuid.UUID, n int) *agentStore {
	runID := uuid.New()
	s := &agentStore{
		run:      Run{ID: runID, TenantID: tenantID, Status: RunPending},
		proceeds: map[uuid.UUID]int{},
	}
	created := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		s.tasks = append(s.tasks, &Task{
			ID:             agentTaskID(i),
			RunID:          runID,
			TenantID:       tenantID,
			SiteID:         uuid.New(),
			TargetType:     TargetAgent,
			TargetSlug:     AgentTargetSlug,
			DesiredVersion: agentPlanTarget,
			FromVersion:    agentPlanFrom,
			Status:         TaskPending,
			// Every task shares one created_at, exactly as a single insert
			// transaction produces.
			CreatedAt: created,
			UpdatedAt: created,
		})
	}
	return s
}

// snapshotLocked copies the task rows, the way a SELECT inside the locked
// transaction would.
func (s *agentStore) snapshotLocked() []Task {
	out := make([]Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		out = append(out, *t)
	}
	return out
}

func (s *agentStore) find(id uuid.UUID) *Task {
	for _, t := range s.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// Task returns a copy of one task row (test assertions).
func (s *agentStore) Task(i int) Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.tasks[i]
}

// RunStatus returns the run's current status (test assertions).
func (s *agentStore) RunStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run.Status
}

// setStatus is a test helper that writes a task's status directly, standing in
// for an outcome some other part of the system already recorded.
func (s *agentStore) setStatus(i int, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[i].Status = status
}

// setStatusByID is setStatus for a driver that only holds task ids.
func (s *agentStore) setStatusByID(id uuid.UUID, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.find(id); t != nil {
		t.Status = status
	}
}

// haltLocked mirrors the repo's haltLocked (agent_repo.go), including the rule
// that matters most: ONLY PENDING tasks are cancelled. A running task has
// already had its command delivered and a cron event spawned on the site, so
// cancelling it would record "nothing was ever sent to this site" about a site
// that was in fact touched, and would stop the confirm poll from ever
// establishing what happened there.
func (s *agentStore) haltLocked(reason string) AgentRunEvaluation {
	ev := AgentRunEvaluation{Halted: true, Reason: reason, Changed: s.run.Status != RunHalted}
	for _, t := range s.tasks {
		if t.Status != TaskPending {
			continue
		}
		t.Status = TaskCancelled
		t.Detail = "cancelled: " + reason
		ev.Cancelled++
	}
	s.run.Status = RunHalted
	if ev.Changed {
		s.haltTransitions++
		s.firstHaltReason = reason
	}
	return ev
}

// lastHaltReason returns the reason recorded by the call that actually halted
// the run (later calls only re-assert an existing halt). Tests assert on it
// because WHY a rollout stopped is what an operator acts on: "the canary failed"
// sends them to look at the build, "the wave attempted nothing" sends them to
// look at the sites.
func (f *fakeWaveRepo) lastHaltReason() string {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	return f.s.firstHaltReason
}

// --- AgentWaveRepo ---------------------------------------------------------

type fakeWaveRepo struct{ s *agentStore }

func (f *fakeWaveRepo) ClaimAgentWaveTask(_ context.Context, _, _, taskID uuid.UUID) (AgentWaveClaim, Task, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()

	me := f.s.find(taskID)
	if me == nil {
		return ClaimWait, Task{}, fmt.Errorf("task not found")
	}
	if terminal(me.Status) {
		return ClaimAlreadyClaimed, *me, nil
	}

	state := DeriveAgentWaveState(f.s.snapshotLocked())
	if f.s.run.Status == RunHalted || state.Halt {
		reason := state.HaltReason
		if reason == "" {
			reason = "the run was halted"
		}
		f.s.haltLocked(reason)
		return ClaimHalted, *me, nil
	}
	if me.Status != TaskPending {
		return ClaimAlreadyClaimed, *me, nil
	}
	if !state.GateOpenFor(taskID) {
		return ClaimWait, *me, nil
	}
	me.Status = TaskRunning
	f.s.proceeds[taskID]++
	return ClaimProceed, *me, nil
}

func (f *fakeWaveRepo) EvaluateAgentRun(_ context.Context, _, _ uuid.UUID) (AgentRunEvaluation, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()

	state := DeriveAgentWaveState(f.s.snapshotLocked())
	if !state.Halt && f.s.run.Status != RunHalted {
		return AgentRunEvaluation{Dispatchable: state.DispatchableTasks()}, nil
	}
	reason := state.HaltReason
	if reason == "" {
		reason = "the run was halted"
	}
	return f.s.haltLocked(reason), nil
}

func (f *fakeWaveRepo) HaltAgentRun(_ context.Context, _, _ uuid.UUID, reason string) (AgentRunEvaluation, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	return f.s.haltLocked(reason), nil
}

// --- Repo (only the methods the agent branch reaches) ----------------------

type fakeAgentRepo struct {
	probeFakeRepo
	s *agentStore
}

func (f *fakeAgentRepo) GetTask(_ context.Context, _, taskID uuid.UUID) (Task, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	t := f.s.find(taskID)
	if t == nil {
		return Task{}, fmt.Errorf("task not found")
	}
	return *t, nil
}

// FinishTask mirrors pgRepo.FinishTask, including the status precondition the
// SQL carries (db/query/updates.sql: FinishUpdateTask matches only
// status IN ('pending','running')). A task that already reached a terminal
// state is returned unchanged with ErrTaskNotOpen, so a worker coming back
// after its run was halted cannot overwrite 'cancelled' with 'succeeded'.
func (f *fakeAgentRepo) FinishTask(_ context.Context, in FinishTaskInput) (Task, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	t := f.s.find(in.TaskID)
	if t == nil {
		return Task{}, fmt.Errorf("task not found")
	}
	if terminal(t.Status) {
		return *t, ErrTaskNotOpen
	}
	f.finished = append(f.finished, in)
	t.Status = in.Status
	t.FromVersion = in.FromVersion
	t.ToVersion = in.ToVersion
	t.Detail = in.Detail
	t.Error = in.Error
	return *t, nil
}

func (f *fakeAgentRepo) CountUnfinishedTasks(context.Context, uuid.UUID, uuid.UUID) (int64, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	var n int64
	for _, t := range f.s.tasks {
		if !terminal(t.Status) {
			n++
		}
	}
	return n, nil
}

func (f *fakeAgentRepo) SetRunStatus(_ context.Context, _, _ uuid.UUID, status string) (Run, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	f.s.run.Status = status
	return f.s.run, nil
}

func (f *fakeAgentRepo) GetRun(context.Context, uuid.UUID, uuid.UUID) (Run, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	return f.s.run, nil
}

// ---------------------------------------------------------------------------
// Wave plan
// ---------------------------------------------------------------------------

// TestPlanWavesShape pins the rollout shape the whole safety property rests
// on: one canary, then a small pilot (5% of the run, never fewer than 3), then
// the remainder. If this ever silently changed, a fleet-wide agent rollout
// would go out in one step and nothing else in this file could catch it.
func TestPlanWavesShape(t *testing.T) {
	cases := []struct {
		n    int
		want []WaveRange
	}{
		{0, nil},
		{1, []WaveRange{{0, 1}}},
		{2, []WaveRange{{0, 1}, {1, 2}}},
		{4, []WaveRange{{0, 1}, {1, 4}}},
		// 5% of 10 is 0.5, floored by the 3-site pilot minimum.
		{10, []WaveRange{{0, 1}, {1, 4}, {4, 10}}},
		// 5% of 200 is 10, comfortably above the minimum.
		{200, []WaveRange{{0, 1}, {1, 11}, {11, 200}}},
		// 5% of 61 is 3.05, which must round UP to 4 rather than down to 3.
		{61, []WaveRange{{0, 1}, {1, 5}, {5, 61}}},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("n=%d", tc.n), func(t *testing.T) {
			got := PlanWaves(tc.n)
			if len(got) != len(tc.want) {
				t.Fatalf("PlanWaves(%d) = %+v, want %+v", tc.n, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("PlanWaves(%d) = %+v, want %+v", tc.n, got, tc.want)
				}
			}
			// Every wave must be non-empty and the plan must cover exactly
			// [0, n) with no gap and no overlap: a task that fell in no wave
			// would never be gated, and one in two waves would be double-sent.
			covered := 0
			for i, w := range got {
				if w.Len() <= 0 {
					t.Fatalf("wave %d is empty: %+v", i, got)
				}
				if i > 0 && w.Start != got[i-1].End {
					t.Fatalf("wave plan has a gap or overlap: %+v", got)
				}
				covered += w.Len()
			}
			if covered != tc.n {
				t.Fatalf("wave plan covers %d of %d tasks: %+v", covered, tc.n, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Gate + halt derivation
// ---------------------------------------------------------------------------

// TestWaveDoesNotOpenUntilThePriorWaveConfirmed is the ordering property: a
// wave is not merely "later", it is BLOCKED until every site in the wave
// before it has confirmed. It walks a 10-site run through wave 0 in the three
// states that must all keep wave 1 shut (pending, running, and running with
// the canary armed but unconfirmed), then opens it only on confirmation.
func TestWaveDoesNotOpenUntilThePriorWaveConfirmed(t *testing.T) {
	s := newAgentStore(uuid.New(), 10)
	wave1Task := s.tasks[1].ID

	for _, tc := range []struct {
		canaryStatus string
		// wantDispatchable is the canary itself while it is still pending
		// (wave 0 is open on sight), and nothing at all once it is running.
		// Either way NO wave-1 site is ever in the set.
		wantDispatchable int
	}{
		{TaskPending, 1},
		{TaskRunning, 0},
	} {
		s.setStatus(0, tc.canaryStatus)
		st := DeriveAgentWaveState(s.snapshotLocked())
		if st.GateOpenFor(wave1Task) {
			t.Fatalf("wave 1 must stay shut while the canary is %q", tc.canaryStatus)
		}
		if st.OpenThrough != 1 {
			t.Fatalf("only wave 0 may be open while the canary is %q, got OpenThrough=%d", tc.canaryStatus, st.OpenThrough)
		}
		dispatchable := st.DispatchableTasks()
		if len(dispatchable) != tc.wantDispatchable {
			t.Fatalf("canary %q: %d dispatchable task(s), want %d", tc.canaryStatus, len(dispatchable), tc.wantDispatchable)
		}
		for _, task := range dispatchable {
			if task.ID != s.tasks[0].ID {
				t.Fatalf("canary %q: a site outside wave 0 became dispatchable: %s", tc.canaryStatus, task.ID)
			}
		}
	}

	// Only a CONFIRMED canary opens wave 1.
	s.setStatus(0, TaskSucceeded)
	st := DeriveAgentWaveState(s.snapshotLocked())
	if !st.GateOpenFor(wave1Task) {
		t.Fatal("wave 1 must open once the canary confirmed")
	}
	if st.OpenThrough != 2 {
		t.Fatalf("OpenThrough = %d, want 2 (waves 0 and 1)", st.OpenThrough)
	}
	// ...and only wave 1. Wave 2 must still be shut.
	if st.GateOpenFor(s.tasks[4].ID) {
		t.Fatal("wave 2 must stay shut until wave 1 confirms")
	}
	dispatchable := st.DispatchableTasks()
	if len(dispatchable) != 3 {
		t.Fatalf("exactly wave 1's three sites become dispatchable, got %d", len(dispatchable))
	}
	for _, task := range dispatchable {
		if task.ID == s.tasks[0].ID || task.ID == s.tasks[4].ID {
			t.Fatalf("dispatchable set leaked outside wave 1: %s", task.ID)
		}
	}
}

// TestWaveZeroFailureHalts proves the canary is a real gate: a single failure
// in wave 0 stops the run, whatever the size of the fleet behind it.
func TestWaveZeroFailureHalts(t *testing.T) {
	for _, status := range []string{TaskFailed, TaskSkipped} {
		t.Run(status, func(t *testing.T) {
			s := newAgentStore(uuid.New(), 100)
			s.setStatus(0, status)

			st := DeriveAgentWaveState(s.snapshotLocked())
			if !st.Halt {
				t.Fatalf("a wave-0 %q must halt the run", status)
			}
			if st.HaltReason == "" {
				t.Fatal("a halt must carry an operator-readable reason")
			}
			if st.OpenThrough != 0 {
				t.Fatalf("a halted run opens no wave, got OpenThrough=%d", st.OpenThrough)
			}
			if got := len(st.DispatchableTasks()); got != 0 {
				t.Fatalf("a halted run dispatches nothing, got %d task(s)", got)
			}
			if st.GateOpenFor(s.tasks[1].ID) {
				t.Fatal("no task may pass the gate once the run has halted")
			}
		})
	}
}

// TestLaterWaveHaltsOnFailureRate covers the threshold gate: a later wave is
// allowed a thin tail of stragglers but not a real failure rate. With a pilot
// of three, one failure is already 33% and stops the run; a large final wave
// tolerates a couple of unconfirmed sites out of a hundred.
func TestLaterWaveHaltsOnFailureRate(t *testing.T) {
	t.Run("one failure in a three-site pilot halts", func(t *testing.T) {
		s := newAgentStore(uuid.New(), 10)
		s.setStatus(0, TaskSucceeded)
		s.setStatus(1, TaskSucceeded)
		s.setStatus(2, TaskSucceeded)
		s.setStatus(3, TaskFailed)

		st := DeriveAgentWaveState(s.snapshotLocked())
		if !st.Halt {
			t.Fatal("a 33% pilot failure rate must halt the run")
		}
		if st.GateOpenFor(s.tasks[4].ID) {
			t.Fatal("wave 2 must never open behind a failed pilot")
		}
	})

	t.Run("a thin tail in a large wave does not halt", func(t *testing.T) {
		// 100 sites: wave 0 = 1, wave 1 = 5, wave 2 = 94. Five failures in
		// wave 2 is 5.3%, under the threshold.
		s := newAgentStore(uuid.New(), 100)
		for i := 0; i < 100; i++ {
			s.setStatus(i, TaskSucceeded)
		}
		for i := 95; i < 100; i++ {
			s.setStatus(i, TaskFailed)
		}
		st := DeriveAgentWaveState(s.snapshotLocked())
		if st.Halt {
			t.Fatalf("5%% failures in the final wave must not halt: %s", st.HaltReason)
		}
	})

	t.Run("a wave that confirmed nothing halts even with no failures", func(t *testing.T) {
		// Every pilot site was skipped: no failures, but no evidence either.
		// Opening the next wave on that would be opening it on nothing.
		s := newAgentStore(uuid.New(), 10)
		s.setStatus(0, TaskSucceeded)
		for i := 1; i < 4; i++ {
			s.setStatus(i, TaskSkipped)
		}
		st := DeriveAgentWaveState(s.snapshotLocked())
		if !st.Halt {
			t.Fatal("a wave that confirmed no site proves nothing and must halt")
		}
	})
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

// TestConcurrentClaimsDispatchOnce proves the exactly-once claim: however many
// duplicate jobs race for the same task, only one is ever told to contact the
// site. This is what makes re-enqueueing an open wave safe, and therefore what
// lets the wave machine be stateless.
func TestConcurrentClaimsDispatchOnce(t *testing.T) {
	tenant := uuid.New()
	s := newAgentStore(tenant, 10)
	repo := &fakeWaveRepo{s: s}
	taskID := s.tasks[0].ID

	const racers = 16
	var wg sync.WaitGroup
	claims := make([]AgentWaveClaim, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, _, err := repo.ClaimAgentWaveTask(context.Background(), tenant, s.run.ID, taskID)
			if err != nil {
				t.Errorf("claim: %v", err)
			}
			claims[i] = c
		}(i)
	}
	wg.Wait()

	proceeds := 0
	for _, c := range claims {
		if c == ClaimProceed {
			proceeds++
		} else if c != ClaimAlreadyClaimed {
			t.Fatalf("a loser must see ClaimAlreadyClaimed, got %v", c)
		}
	}
	if proceeds != 1 {
		t.Fatalf("exactly one racer may dispatch, got %d", proceeds)
	}
	if s.proceeds[taskID] != 1 {
		t.Fatalf("the store recorded %d claims, want 1", s.proceeds[taskID])
	}
}

// TestConcurrentCompletionsAdvanceOnce proves two workers finishing at the
// same moment cannot both advance the wave: the dispatchable set they compute
// is identical, and re-dispatching it is a no-op because each task can still
// only be claimed once. The assertion is on the OUTCOME that matters (how many
// sites get contacted), not on how many times the evaluation ran.
func TestConcurrentCompletionsAdvanceOnce(t *testing.T) {
	tenant := uuid.New()
	s := newAgentStore(tenant, 10)
	repo := &fakeWaveRepo{s: s}
	s.setStatus(0, TaskSucceeded) // canary confirmed: wave 1 is now open.

	// Two finishers evaluate concurrently, each enqueueing whatever it is
	// told is dispatchable, and every enqueued task is then claimed.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var enqueued []Task
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev, err := repo.EvaluateAgentRun(context.Background(), tenant, s.run.ID)
			if err != nil {
				t.Errorf("evaluate: %v", err)
				return
			}
			mu.Lock()
			enqueued = append(enqueued, ev.Dispatchable...)
			mu.Unlock()
		}()
	}
	wg.Wait()

	// Both evaluations may well have returned the same three tasks, that is
	// expected and harmless. What must hold is that claiming them all still
	// dispatches each site exactly once.
	for _, task := range enqueued {
		if _, _, err := repo.ClaimAgentWaveTask(context.Background(), tenant, s.run.ID, task.ID); err != nil {
			t.Fatalf("claim: %v", err)
		}
	}
	for i := 1; i < 4; i++ {
		if got := s.proceeds[s.tasks[i].ID]; got != 1 {
			t.Fatalf("wave 1 task %d was dispatched %d times, want exactly 1", i, got)
		}
	}
	for i := 4; i < 10; i++ {
		if got := s.proceeds[s.tasks[i].ID]; got != 0 {
			t.Fatalf("wave 2 task %d was dispatched %d times before its wave opened", i, got)
		}
	}
}

// TestClaimNeverDispatchesIntoAHalt is the second half of the race the design
// calls out: a wave must not advance into a halt that just cancelled it. A
// claim that arrives after the halt commits must be refused and the task
// cancelled, never dispatched.
func TestClaimNeverDispatchesIntoAHalt(t *testing.T) {
	tenant := uuid.New()
	s := newAgentStore(tenant, 10)
	repo := &fakeWaveRepo{s: s}
	s.setStatus(0, TaskSucceeded) // wave 1 open

	// The halt lands (for a reason outside the gate, e.g. the kill switch).
	if _, err := repo.HaltAgentRun(context.Background(), tenant, s.run.ID, "stopped by the operator"); err != nil {
		t.Fatalf("halt: %v", err)
	}

	// Every still-enqueued job for this run now arrives.
	for i := 1; i < 10; i++ {
		claim, task, err := repo.ClaimAgentWaveTask(context.Background(), tenant, s.run.ID, s.tasks[i].ID)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if claim == ClaimProceed {
			t.Fatalf("task %d was dispatched after the run halted", i)
		}
		if task.Status != TaskCancelled {
			t.Fatalf("task %d after a halt has status %q, want %q", i, task.Status, TaskCancelled)
		}
	}
	if s.RunStatus() != RunHalted {
		t.Fatalf("run status = %q, want %q", s.RunStatus(), RunHalted)
	}
	for id, n := range s.proceeds {
		t.Fatalf("no site may be contacted after a halt, but %s was claimed %d time(s)", id, n)
	}
}

// TestConcurrentHaltsRecordOneIncident proves the halt itself is
// exactly-once: several workers can decide to halt the same run at the same
// moment, but only one reports Changed, so the operator sees one incident
// rather than a storm of duplicates.
func TestConcurrentHaltsRecordOneIncident(t *testing.T) {
	tenant := uuid.New()
	s := newAgentStore(tenant, 10)
	repo := &fakeWaveRepo{s: s}
	s.setStatus(0, TaskFailed) // the canary failed: every finisher will halt.

	const racers = 8
	var wg sync.WaitGroup
	changed := make([]bool, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ev, err := repo.EvaluateAgentRun(context.Background(), tenant, s.run.ID)
			if err != nil {
				t.Errorf("evaluate: %v", err)
				return
			}
			if !ev.Halted {
				t.Errorf("every evaluation must see the halt")
			}
			changed[i] = ev.Changed
		}(i)
	}
	wg.Wait()

	n := 0
	for _, c := range changed {
		if c {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("exactly one caller may perform the halt transition, got %d", n)
	}
	if s.haltTransitions != 1 {
		t.Fatalf("the store recorded %d halt transitions, want 1", s.haltTransitions)
	}
	// And the halt actually cancelled the rest of the fleet.
	for i := 1; i < 10; i++ {
		if got := s.Task(i).Status; got != TaskCancelled {
			t.Fatalf("task %d status = %q, want %q", i, got, TaskCancelled)
		}
	}
}

// ---------------------------------------------------------------------------
// Confirmation predicate
// ---------------------------------------------------------------------------

// TestAgentSelfUpdateConfirmed pins what counts as proof the upgrade landed.
// The channel's whole safety argument is that only the NEW code phoning home
// is success, so every way of NOT knowing must read as "not confirmed".
func TestAgentSelfUpdateConfirmed(t *testing.T) {
	cases := []struct {
		name                   string
		reported, from, expect string
		want                   bool
	}{
		{"reported matches the expected build", "0.62.0", "0.61.80", "0.62.0", true},
		{"reported is newer than expected", "0.62.1", "0.61.80", "0.62.0", true},
		{"still reporting the old version", "0.61.80", "0.61.80", "0.62.0", false},
		{"never reported anything", "", "0.61.80", "0.62.0", false},
		{"reported garbage", "not-a-version", "0.61.80", "0.62.0", false},
		{"reported older than expected", "0.61.90", "0.61.80", "0.62.0", false},
		// With no expected version, only a strictly newer well-formed version
		// confirms: re-reporting the same build is not evidence of anything.
		{"no expectation, newer version", "0.62.0", "0.61.80", "", true},
		{"no expectation, same version", "0.61.80", "0.61.80", "", false},
		{"no expectation, older version", "0.61.70", "0.61.80", "", false},
		{"no expectation and nothing known", "0.62.0", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentSelfUpdateConfirmed(tc.reported, tc.from, tc.expect); got != tc.want {
				t.Fatalf("agentSelfUpdateConfirmed(%q, %q, %q) = %v, want %v",
					tc.reported, tc.from, tc.expect, got, tc.want)
			}
		})
	}
}
