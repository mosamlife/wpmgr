package agentupstream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentmirror"
)

// fakeRecorder is an AttemptStore test double capturing every recorded
// attempt, so tests can assert on Trigger/Outcome/Detail without a real
// Postgres connection. Load returns state (and loadErr), standing in for the
// persisted agent_mirror_state row.
type fakeRecorder struct {
	mu      sync.Mutex
	calls   []agentmirror.AttemptInput
	state   agentmirror.State
	loadErr error
	// clamps holds every ceiling ClampLastRequestAt was asked to apply, and
	// clampErr is what it fails with when set.
	clamps   []time.Time
	clampErr error
}

// ClampLastRequestAt behaves as agentmirror.Repo.ClampLastRequestAt does: it
// lowers the stored request time to ceiling when, and only when, that time is
// later than ceiling, and says whether it did.
func (f *fakeRecorder) ClampLastRequestAt(_ context.Context, ceiling time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clamps = append(f.clamps, ceiling)
	if f.clampErr != nil {
		return false, f.clampErr
	}
	if f.state.LastRequestAt == nil || !f.state.LastRequestAt.After(ceiling) {
		return false, nil
	}
	lowered := ceiling
	f.state.LastRequestAt = &lowered
	return true, nil
}

func (f *fakeRecorder) clampCalls() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.clamps)
}

func (f *fakeRecorder) storedRequestTime() *time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state.LastRequestAt
}

func (f *fakeRecorder) RecordAttempt(_ context.Context, in agentmirror.AttemptInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	return nil
}

func (f *fakeRecorder) Load(context.Context) (agentmirror.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.loadErr
}

func (f *fakeRecorder) last() (agentmirror.AttemptInput, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return agentmirror.AttemptInput{}, false
	}
	return f.calls[len(f.calls)-1], true
}

func (f *fakeRecorder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// TestWorkerDisabledDoesNothing is the off-by-default lock. With the flag false
// (its default, see config.UpdateConfig.AgentMirrorEnabled), the job must make no
// outbound request and write nothing: merging this feature changes nothing until
// an operator opts in.
func TestWorkerDisabledDoesNothing(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	doer := wire(f)
	w := NewMirrorWorker(false, newTestMirror(store, doer), nil, nil)

	if err := w.Work(context.Background(), &river.Job[MirrorArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if got := doer.urls(); len(got) != 0 {
		t.Fatalf("made outbound requests %v while disabled", got)
	}
	if got := store.writes(); len(got) != 0 {
		t.Fatalf("wrote %v while disabled", got)
	}
}

// TestWorkerEnabledMirrors is the other side of the switch: with the flag on, the
// same wiring actually mirrors.
func TestWorkerEnabledMirrors(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	w := NewMirrorWorker(true, newTestMirror(store, wire(f)), nil, nil)

	if err := w.Work(context.Background(), &river.Job[MirrorArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	writes := store.writes()
	if len(writes) != 2 || writes[0] != packageObjectKey(testVersion) || writes[1] != ManifestKey {
		t.Fatalf("writes = %v, want package then pointer", writes)
	}
}

// TestWorkerNilMirrorDoesNothing: object storage not configured leaves the mirror
// nil. That is a no-op, not a crash.
func TestWorkerNilMirrorDoesNothing(t *testing.T) {
	w := NewMirrorWorker(true, nil, nil, nil)
	if err := w.Work(context.Background(), &river.Job[MirrorArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
}

// TestWorkerNeverReturnsAnError is requirement 10 in test form: a failure of this
// job must degrade to "no new release mirrored" and nothing else. Returning an
// error would buy a River retry storm against an API that allows 60 requests per
// hour; the next scheduled run is the correct retry.
func TestWorkerNeverReturnsAnError(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T) *Mirror
	}{
		{"upstream unreachable", func(t *testing.T) *Mirror {
			f := newFixture(t)
			d := wire(f)
			d.handlers[apiURL(testOwner, testRepo)] = func(*http.Request) (*http.Response, error) {
				return nil, errors.New("no route to host")
			}
			return newTestMirror(newFakeStore(), d)
		}},
		{"rate limited", func(t *testing.T) *Mirror {
			f := newFixture(t)
			d := wire(f)
			d.handlers[apiURL(testOwner, testRepo)] = func(*http.Request) (*http.Response, error) {
				return statusResponse(http.StatusTooManyRequests, nil), nil
			}
			return newTestMirror(newFakeStore(), d)
		}},
		{"refused: digest mismatch", func(t *testing.T) *Mirror {
			f := newFixture(t)
			f.api = f.apiDoc(testTag, "sha256:"+strings.Repeat("b", 64), int64(len(f.pkg)))
			return newTestMirror(newFakeStore(), wire(f))
		}},
		{"refused: invalid manifest", func(t *testing.T) *Mirror {
			f := newFixture(t)
			f.setManifest([]byte(`{"slug":"wrong"}`))
			return newTestMirror(newFakeStore(), wire(f))
		}},
		{"refused: truncated download", func(t *testing.T) *Mirror {
			f := newFixture(t)
			d := wire(f)
			d.handlers[downloadURL(testOwner, testRepo, testTag, packageAssetName)] = func(*http.Request) (*http.Response, error) {
				return okResponse(f.pkg[:10], nil), nil
			}
			return newTestMirror(newFakeStore(), d)
		}},
		{"storage write fails", func(t *testing.T) *Mirror {
			f := newFixture(t)
			s := newFakeStore()
			s.putErr[packageObjectKey(testVersion)] = errors.New("storage unavailable")
			return newTestMirror(s, wire(f))
		}},
		{"unusable owner", func(t *testing.T) *Mirror {
			f := newFixture(t)
			return NewMirror(newFakeStore(), wire(f), "bad/owner", testRepo, nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := NewMirrorWorker(true, tc.build(t), nil, nil)
			if err := w.Work(context.Background(), &river.Job[MirrorArgs]{}); err != nil {
				t.Fatalf("Work returned %v; this job must always return nil", err)
			}
		})
	}
}

// TestMirrorArgsInsertOpts pins the queue, the attempt cap, and the dedupe
// window. The window MUST be shorter than the interval, or two legitimate
// consecutive ticks could land in one unique window and silently swallow a run.
func TestMirrorArgsInsertOpts(t *testing.T) {
	opts := MirrorArgs{}.InsertOpts()
	if opts.Queue != MirrorQueue {
		t.Fatalf("Queue = %q, want %q", opts.Queue, MirrorQueue)
	}
	if opts.MaxAttempts <= 0 {
		t.Fatalf("MaxAttempts = %d, want a bounded positive cap", opts.MaxAttempts)
	}
	if opts.UniqueOpts.ByPeriod >= MirrorInterval {
		t.Fatalf("unique window %v >= interval %v; a legitimate tick could be deduped away", opts.UniqueOpts.ByPeriod, MirrorInterval)
	}
	if kind := (MirrorArgs{}).Kind(); kind != "agent_release_mirror" {
		t.Fatalf("Kind = %q; changing it orphans in-flight jobs", kind)
	}
}

// TestPeriodicInsertOptsIsJittered: each tick gets a fresh delay inside the
// jitter window, so installs that boot together do not hit GitHub in lockstep.
func TestPeriodicInsertOptsIsJittered(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		before := time.Now()
		opts := PeriodicInsertOpts()
		if opts.Queue != MirrorQueue {
			t.Fatalf("Queue = %q, want %q", opts.Queue, MirrorQueue)
		}
		delay := opts.ScheduledAt.Sub(before)
		if delay < 0 || delay > MirrorJitter {
			t.Fatalf("jitter delay %v outside [0, %v]", delay, MirrorJitter)
		}
		seen[delay.Truncate(time.Minute)] = true
	}
	if len(seen) < 2 {
		t.Fatal("PeriodicInsertOpts produced a single fixed delay; it must be jittered")
	}
}

// TestMirrorCadenceIsSaneAgainstTheRateLimit: 6 hours is far inside the
// unauthenticated 60-requests-per-hour-per-IP limit even with the jitter, and the
// in-process spacing guard is shorter than the interval so it can never block a
// scheduled run.
func TestMirrorCadenceIsSaneAgainstTheRateLimit(t *testing.T) {
	if MirrorInterval != 6*time.Hour {
		t.Fatalf("MirrorInterval = %v, want 6h", MirrorInterval)
	}
	if MirrorJitter >= MirrorInterval {
		t.Fatalf("MirrorJitter %v >= MirrorInterval %v", MirrorJitter, MirrorInterval)
	}
	if minRequestSpacing >= MirrorInterval {
		t.Fatalf("minRequestSpacing %v >= MirrorInterval %v; the guard would block scheduled runs", minRequestSpacing, MirrorInterval)
	}
}

// ---------------------------------------------------------------------------
// GH #322: persisting the attempt outcome instead of failing.
// ---------------------------------------------------------------------------

// TestWorkerDisabled_RecordsNothing: mirroring off entirely must NOT stamp an
// attempt. Stamping one would be the same lie in miniature this feature
// exists to remove: "an attempt happened" when none did.
func TestWorkerDisabled_RecordsNothing(t *testing.T) {
	rec := &fakeRecorder{}
	w := NewMirrorWorker(false, newTestMirror(newFakeStore(), wire(newFixture(t))), rec, nil)
	if err := w.Work(context.Background(), &river.Job[MirrorArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if n := rec.count(); n != 0 {
		t.Fatalf("recorded %d attempt(s) while disabled; want 0", n)
	}
}

// TestWorkerNilMirror_RecordsNotConfigured: enabled but object storage never
// wired must record OutcomeNotConfigured: this is an attempt (the operator
// turned the flag on), and misconfiguration never self-heals so it must be
// visible.
func TestWorkerNilMirror_RecordsNotConfigured(t *testing.T) {
	rec := &fakeRecorder{}
	w := NewMirrorWorker(true, nil, rec, nil)
	if err := w.Work(context.Background(), &river.Job[MirrorArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	last, ok := rec.last()
	if !ok {
		t.Fatal("no attempt recorded")
	}
	if last.Outcome != agentmirror.OutcomeNotConfigured {
		t.Fatalf("Outcome = %q, want %q", last.Outcome, agentmirror.OutcomeNotConfigured)
	}
	if last.Detail == "" {
		t.Fatal("Detail is empty; operator gets no explanation")
	}
}

// TestWorkerMirrored_RecordsSuccessWithVersion proves a genuine publish
// records OutcomeMirrored (a success) together with the version examined.
func TestWorkerMirrored_RecordsSuccessWithVersion(t *testing.T) {
	rec := &fakeRecorder{}
	f := newFixture(t)
	w := NewMirrorWorker(true, newTestMirror(newFakeStore(), wire(f)), rec, nil)
	if err := w.Work(context.Background(), &river.Job[MirrorArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	last, ok := rec.last()
	if !ok {
		t.Fatal("no attempt recorded")
	}
	if last.Outcome != agentmirror.OutcomeMirrored {
		t.Fatalf("Outcome = %q, want %q", last.Outcome, agentmirror.OutcomeMirrored)
	}
	if !last.Outcome.IsSuccess() {
		t.Fatal("OutcomeMirrored.IsSuccess() = false, want true")
	}
	if last.Version != testVersion {
		t.Fatalf("Version = %q, want %q", last.Version, testVersion)
	}
	if last.LastRequestAt.IsZero() {
		t.Fatal("LastRequestAt is zero; an actual request was made this run")
	}
}

// TestWorkerRateLimited_RecordsRateLimitedNotFailure pins C5: a rate-limited
// run must record OutcomeRateLimited, which Outcome.IsSuccess() reports as
// NOT a success, but which is never a FAILURE either: nothing in the
// recorded outcome vocabulary conflates the two.
func TestWorkerRateLimited_RecordsRateLimitedNotFailure(t *testing.T) {
	rec := &fakeRecorder{}
	f := newFixture(t)
	d := wire(f)
	d.handlers[apiURL(testOwner, testRepo)] = func(*http.Request) (*http.Response, error) {
		return statusResponse(http.StatusTooManyRequests, nil), nil
	}
	w := NewMirrorWorker(true, newTestMirror(newFakeStore(), d), rec, nil)
	if err := w.Work(context.Background(), &river.Job[MirrorArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	last, ok := rec.last()
	if !ok {
		t.Fatal("no attempt recorded")
	}
	if last.Outcome != agentmirror.OutcomeRateLimited {
		t.Fatalf("Outcome = %q, want %q", last.Outcome, agentmirror.OutcomeRateLimited)
	}
	if last.Outcome.IsSuccess() {
		t.Fatal("OutcomeRateLimited.IsSuccess() = true; rate-limited is not a confirmation")
	}
}

// TestWorkerForeignChannel_RecordsStandingDown pins the "correct, permanent,
// not a fault" outcome: an install publishing its own agent releases must
// record OutcomeForeignChannel every time, not OutcomeRefused.
func TestWorkerForeignChannel_RecordsStandingDown(t *testing.T) {
	rec := &fakeRecorder{}
	f := newFixture(t)
	store := newFakeStore()
	// Stage a pointer this mirror did NOT write (no provenance stamp), naming
	// a DIFFERENT version than upstream so the "already current" short
	// circuit (checked before provenance) does not swallow this case: the
	// operator's own release channel.
	store.objects[ManifestKey] = manifestJSON("0.0.1", strings.Repeat("a", 64), 1234, packageObjectKey("0.0.1"))
	w := NewMirrorWorker(true, newTestMirror(store, wire(f)), rec, nil)
	if err := w.Work(context.Background(), &river.Job[MirrorArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	last, ok := rec.last()
	if !ok {
		t.Fatal("no attempt recorded")
	}
	if last.Outcome != agentmirror.OutcomeForeignChannel {
		t.Fatalf("Outcome = %q, want %q", last.Outcome, agentmirror.OutcomeForeignChannel)
	}
}

// TestWorkerJobTrigger_PropagatesToRecordedAttempt proves the RECORDED
// trigger reflects the actual River job's Args.Trigger, so the manual-check
// path (which enqueues with Trigger: TriggerManual) is distinguishable from a
// scheduled tick in the persisted state, never guessed, always read from the
// job that actually ran.
func TestWorkerJobTrigger_PropagatesToRecordedAttempt(t *testing.T) {
	rec := &fakeRecorder{}
	w := NewMirrorWorker(true, nil, rec, nil)
	if err := w.Work(context.Background(), &river.Job[MirrorArgs]{Args: MirrorArgs{Trigger: TriggerManual}}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	last, ok := rec.last()
	if !ok {
		t.Fatal("no attempt recorded")
	}
	if last.Trigger != agentmirror.TriggerManual {
		t.Fatalf("Trigger = %q, want %q", last.Trigger, agentmirror.TriggerManual)
	}
}

// TestWorkerEmptyTrigger_DefaultsToPeriodic: a job enqueued before the
// Trigger field existed decodes with the Go zero value (""), which must be
// treated as a periodic tick, not an unknown/invalid one.
func TestWorkerEmptyTrigger_DefaultsToPeriodic(t *testing.T) {
	rec := &fakeRecorder{}
	w := NewMirrorWorker(true, nil, rec, nil)
	if err := w.Work(context.Background(), &river.Job[MirrorArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	last, ok := rec.last()
	if !ok {
		t.Fatal("no attempt recorded")
	}
	if last.Trigger != agentmirror.TriggerPeriodic {
		t.Fatalf("Trigger = %q, want %q (empty must default to periodic)", last.Trigger, agentmirror.TriggerPeriodic)
	}
}

// ---------------------------------------------------------------------------
// GH #428: a check shortly after start, without spending the GitHub budget.
// ---------------------------------------------------------------------------

// fakeInserter captures what EnqueueBootCheck asks River to insert.
type fakeInserter struct {
	mu   sync.Mutex
	args []river.JobArgs
	opts []*river.InsertOpts
	dup  bool
	err  error
}

func (f *fakeInserter) Insert(_ context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.args = append(f.args, args)
	f.opts = append(f.opts, opts)
	return &rivertype.JobInsertResult{Job: &rivertype.JobRow{}, UniqueSkippedAsDuplicate: f.dup}, nil
}

// only returns the single insert, failing the test unless exactly one was made.
func (f *fakeInserter) only(t *testing.T) (MirrorArgs, *river.InsertOpts) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.args) != 1 {
		t.Fatalf("inserted %d mirror jobs at start, want exactly 1; with none, the first check is the periodic tick, %v after start",
			len(f.args), MirrorInterval)
	}
	args, ok := f.args[0].(MirrorArgs)
	if !ok {
		t.Fatalf("inserted %T, want MirrorArgs", f.args[0])
	}
	return args, f.opts[0]
}

func requireBootDelay(t *testing.T, from, at time.Time) {
	t.Helper()
	if d := at.Sub(from); d < BootCheckMinDelay || d >= BootCheckMaxDelay {
		t.Fatalf("boot check scheduled %v after %v, want within [%v, %v)", d, from, BootCheckMinDelay, BootCheckMaxDelay)
	}
}

// TestEnqueueBootCheck_FirstCheckFollowsStartWithinMinutes is the GH #428
// regression lock. River runs the periodic job's first tick a full
// MirrorInterval after its scheduler starts, so before this fix every restart
// left the install six hours or more from its next upstream check. The check
// queued at start must run within ten minutes of it.
func TestEnqueueBootCheck_FirstCheckFollowsStartWithinMinutes(t *testing.T) {
	ins := &fakeInserter{}
	start := time.Now()

	bc, err := EnqueueBootCheck(context.Background(), ins, &fakeRecorder{}, start)
	if err != nil {
		t.Fatalf("EnqueueBootCheck: %v", err)
	}
	args, opts := ins.only(t)

	if got := opts.ScheduledAt.Sub(start); got > 10*time.Minute {
		t.Fatalf("first mirror check runs %v after start, want within 10m", got)
	}
	requireBootDelay(t, start, opts.ScheduledAt)
	if args.Trigger != TriggerPeriodic || !args.Boot {
		t.Fatalf("args = %+v, want Trigger %q with Boot set", args, TriggerPeriodic)
	}
	if opts.Queue != MirrorQueue || opts.MaxAttempts != mirrorMaxAttempts {
		t.Fatalf("queue/attempts = %q/%d, want %q/%d", opts.Queue, opts.MaxAttempts, MirrorQueue, mirrorMaxAttempts)
	}
	if !bc.Queued || bc.Deferred || !bc.ScheduledAt.Equal(opts.ScheduledAt) {
		t.Fatalf("BootCheck = %+v, want queued, not deferred, at %v", bc, opts.ScheduledAt)
	}
}

// TestPeriodicScheduleIsAFullIntervalAfterStart pins why the boot check is
// needed: the periodic job alone first runs MirrorInterval after River's
// scheduler starts, and does not run on start.
func TestPeriodicScheduleIsAFullIntervalAfterStart(t *testing.T) {
	start := time.Now()
	if got := mirrorSchedule().Next(start).Sub(start); got != MirrorInterval {
		t.Fatalf("periodic first run %v after start, want %v", got, MirrorInterval)
	}
	if periodicJobOpts().RunOnStart {
		t.Fatal("RunOnStart = true; River re-runs it on every change of leader, the boot check covers start")
	}
	if NewMirrorPeriodicJob() == nil {
		t.Fatal("NewMirrorPeriodicJob returned nil")
	}
	args, opts := periodicTick()
	ma, ok := args.(MirrorArgs)
	if !ok || ma.Trigger != TriggerPeriodic || ma.Boot {
		t.Fatalf("tick args = %#v, want a periodic tick that is not the boot check", args)
	}
	if opts.Queue != MirrorQueue || opts.UniqueOpts.ByPeriod != mirrorUniqueWindow {
		t.Fatalf("tick opts queue/window = %q/%v, want %q/%v", opts.Queue, opts.UniqueOpts.ByPeriod, MirrorQueue, mirrorUniqueWindow)
	}
}

// TestEnqueueBootCheck_WaitsOutTheRequestSpacing: a request recorded five
// minutes ago (by the previous process) defers the boot check to the end of
// the spacing window. It is deferred, never dropped: a release published
// between that request and this restart still gets checked.
func TestEnqueueBootCheck_WaitsOutTheRequestSpacing(t *testing.T) {
	ins := &fakeInserter{}
	start := time.Now()
	last := start.Add(-5 * time.Minute)

	bc, err := EnqueueBootCheck(context.Background(), ins, &fakeRecorder{state: agentmirror.State{LastRequestAt: &last}}, start)
	if err != nil {
		t.Fatalf("EnqueueBootCheck: %v", err)
	}
	_, opts := ins.only(t)
	requireBootDelay(t, last.Add(minRequestSpacing), opts.ScheduledAt)
	if !bc.Queued || !bc.Deferred {
		t.Fatalf("BootCheck = %+v, want queued and deferred", bc)
	}
}

// TestEnqueueBootCheck_OldRequestDoesNotDefer is the over-fire case: a request
// older than the spacing window must not delay the check at all.
func TestEnqueueBootCheck_OldRequestDoesNotDefer(t *testing.T) {
	ins := &fakeInserter{}
	start := time.Now()
	last := start.Add(-minRequestSpacing - time.Minute)

	bc, err := EnqueueBootCheck(context.Background(), ins, &fakeRecorder{state: agentmirror.State{LastRequestAt: &last}}, start)
	if err != nil {
		t.Fatalf("EnqueueBootCheck: %v", err)
	}
	_, opts := ins.only(t)
	requireBootDelay(t, start, opts.ScheduledAt)
	if bc.Deferred {
		t.Fatalf("BootCheck = %+v, want not deferred", bc)
	}
}

// TestEnqueueBootCheck_FutureRequestTimeDelaysOneWindowAtMost: a request time
// in the future can only be a skewed clock. It must not hold the check back by
// however far ahead that clock was.
func TestEnqueueBootCheck_FutureRequestTimeDelaysOneWindowAtMost(t *testing.T) {
	ins := &fakeInserter{}
	start := time.Now()
	future := start.Add(3 * time.Hour)

	if _, err := EnqueueBootCheck(context.Background(), ins, &fakeRecorder{state: agentmirror.State{LastRequestAt: &future}}, start); err != nil {
		t.Fatalf("EnqueueBootCheck: %v", err)
	}
	_, opts := ins.only(t)
	requireBootDelay(t, start.Add(minRequestSpacing), opts.ScheduledAt)
}

// TestEnqueueBootCheck_UnreadableStateStillQueues: failing to read the
// persisted state is no reason to skip the check; the run-time guard still
// applies when it runs.
func TestEnqueueBootCheck_UnreadableStateStillQueues(t *testing.T) {
	ins := &fakeInserter{}
	start := time.Now()

	bc, err := EnqueueBootCheck(context.Background(), ins, &fakeRecorder{loadErr: errors.New("connection refused")}, start)
	if err != nil {
		t.Fatalf("EnqueueBootCheck: %v", err)
	}
	_, opts := ins.only(t)
	requireBootDelay(t, start, opts.ScheduledAt)
	if !bc.Queued {
		t.Fatalf("BootCheck = %+v, want queued", bc)
	}
}

// TestEnqueueBootCheck_OutstandingCheckIsNotQueuedAgain: when River reports
// the insert as a duplicate (an earlier boot check is still outstanding), that
// is reported as not queued, and is not an error.
func TestEnqueueBootCheck_OutstandingCheckIsNotQueuedAgain(t *testing.T) {
	bc, err := EnqueueBootCheck(context.Background(), &fakeInserter{dup: true}, nil, time.Now())
	if err != nil {
		t.Fatalf("EnqueueBootCheck: %v", err)
	}
	if bc.Queued {
		t.Fatalf("BootCheck = %+v, want not queued for a duplicate", bc)
	}
}

// TestEnqueueBootCheck_InsertFailureIsReturned: the caller logs it; it must
// not be swallowed into a reported success.
func TestEnqueueBootCheck_InsertFailureIsReturned(t *testing.T) {
	if _, err := EnqueueBootCheck(context.Background(), &fakeInserter{err: errors.New("db down")}, nil, time.Now()); err == nil {
		t.Fatal("EnqueueBootCheck returned nil error for a failed insert")
	}
	if _, err := EnqueueBootCheck(context.Background(), nil, nil, time.Now()); err == nil {
		t.Fatal("EnqueueBootCheck returned nil error with no inserter")
	}
}

// TestBootInsertOpts_OwnUniqueKeyAndNoTerminalStates pins the two choices
// BootInsertOpts documents. The River-level behaviour they produce (no
// deduplication against a completed tick, at most one outstanding boot check)
// is proven against Postgres in tests/agent_mirror_boot_check_integration_test.go.
func TestBootInsertOpts_OwnUniqueKeyAndNoTerminalStates(t *testing.T) {
	at := time.Now().Add(3 * time.Minute)
	opts := BootInsertOpts(at)
	if opts.Queue != MirrorQueue || opts.MaxAttempts != 1 || !opts.ScheduledAt.Equal(at) {
		t.Fatalf("opts = %+v, want queue %q, 1 attempt, scheduled at %v", opts, MirrorQueue, at)
	}
	if !opts.UniqueOpts.ByArgs || opts.UniqueOpts.ByPeriod != 0 {
		t.Fatalf("unique = %+v, want ByArgs and no ByPeriod", opts.UniqueOpts)
	}
	for _, s := range []rivertype.JobState{rivertype.JobStateCompleted, rivertype.JobStateDiscarded, rivertype.JobStateCancelled} {
		if slices.Contains(opts.UniqueOpts.ByState, s) {
			t.Fatalf("ByState contains %q; a finished boot check would block the next start's", s)
		}
	}
	for _, s := range []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateScheduled} {
		if !slices.Contains(opts.UniqueOpts.ByState, s) {
			t.Fatalf("ByState lacks %q, which River requires", s)
		}
	}

	tick, err := json.Marshal(MirrorArgs{Trigger: TriggerPeriodic})
	if err != nil {
		t.Fatal(err)
	}
	if string(tick) != `{"trigger":"periodic"}` {
		t.Fatalf("periodic tick encodes as %s; its unique key must not move", tick)
	}
	boot, err := json.Marshal(MirrorArgs{Trigger: TriggerPeriodic, Boot: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(boot) == string(tick) {
		t.Fatalf("boot check encodes the same as the tick (%s); they would share a unique key", boot)
	}
}

// TestWorkerBootRun_PersistedRequestClockBlocksTheRequest: a fresh Mirror
// (zero in-memory clock, as after a restart) must still honour a request the
// previous process made five minutes ago. Without the persisted clock, a
// control plane restarting in a loop spends one upstream request per boot.
func TestWorkerBootRun_PersistedRequestClockBlocksTheRequest(t *testing.T) {
	doer := wire(newFixture(t))
	last := time.Now().Add(-5 * time.Minute)
	rec := &fakeRecorder{state: agentmirror.State{LastRequestAt: &last}}
	w := NewMirrorWorker(true, newTestMirror(newFakeStore(), doer), rec, nil)

	job := &river.Job[MirrorArgs]{Args: MirrorArgs{Trigger: TriggerPeriodic, Boot: true}}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if got := doer.urls(); len(got) != 0 {
		t.Fatalf("spent %d upstream request(s) %v five minutes after the last one; want 0", len(got), got)
	}
	got, ok := rec.last()
	if !ok {
		t.Fatal("no attempt recorded")
	}
	if got.Outcome != agentmirror.OutcomeRateLimited || got.Trigger != agentmirror.TriggerPeriodic {
		t.Fatalf("recorded %q/%q, want %q/%q", got.Outcome, got.Trigger, agentmirror.OutcomeRateLimited, agentmirror.TriggerPeriodic)
	}
}

// TestWorker_PersistedRequestClockPastTheWindowStillMirrors is the over-fire
// case: a persisted request older than the spacing window must not block.
func TestWorker_PersistedRequestClockPastTheWindowStillMirrors(t *testing.T) {
	doer := wire(newFixture(t))
	last := time.Now().Add(-minRequestSpacing - time.Minute)
	rec := &fakeRecorder{state: agentmirror.State{LastRequestAt: &last}}
	w := NewMirrorWorker(true, newTestMirror(newFakeStore(), doer), rec, nil)

	if err := w.Work(context.Background(), &river.Job[MirrorArgs]{Args: MirrorArgs{Trigger: TriggerPeriodic, Boot: true}}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(doer.urls()) == 0 {
		t.Fatal("made no upstream request although the last one was outside the spacing window")
	}
	if got, _ := rec.last(); got.Outcome != agentmirror.OutcomeMirrored {
		t.Fatalf("Outcome = %q, want %q", got.Outcome, agentmirror.OutcomeMirrored)
	}
}

// TestWorkerBootRun_FutureRequestTimeStillGetsTheScheduledCheck takes the job
// EnqueueBootCheck queued through Work when the persisted request time is
// AHEAD of this host's clock (a skewed clock on whichever host recorded it, here
// three hours). The check was scheduled for after the spacing window, and when
// it runs it must spend its request: Work completes without rescheduling, so a
// run the spacing guard refuses leaves the install waiting for the six-hour
// tick instead of getting the check it was promised after start.
func TestWorkerBootRun_FutureRequestTimeStillGetsTheScheduledCheck(t *testing.T) {
	ctx := context.Background()
	// The process started 36 minutes ago, so the boot check queued then, at most
	// start + spacing + BootCheckMaxDelay, is due now.
	start := time.Now().Add(-36 * time.Minute)
	future := start.Add(3 * time.Hour)
	rec := &fakeRecorder{state: agentmirror.State{LastRequestAt: &future}}

	ins := &fakeInserter{}
	if _, err := EnqueueBootCheck(ctx, ins, rec, start); err != nil {
		t.Fatalf("EnqueueBootCheck: %v", err)
	}
	args, opts := ins.only(t)
	if !args.Boot {
		t.Fatalf("args = %+v, want the boot check", args)
	}
	if opts.ScheduledAt.After(time.Now()) {
		t.Fatalf("boot check scheduled for %v, which has not arrived; this test models the run at that time", opts.ScheduledAt)
	}
	if opts.ScheduledAt.Before(start.Add(minRequestSpacing)) {
		t.Fatalf("boot check scheduled for %v, before the spacing window after start closes at %v", opts.ScheduledAt, start.Add(minRequestSpacing))
	}

	doer := wire(newFixture(t))
	w := NewMirrorWorker(true, newTestMirror(newFakeStore(), doer), rec, nil)
	if err := w.Work(ctx, &river.Job[MirrorArgs]{Args: args}); err != nil {
		t.Fatalf("Work: %v", err)
	}

	if got := doer.urls(); len(got) == 0 {
		out, _ := rec.last()
		t.Fatalf("the scheduled boot check spent no upstream request (recorded outcome %q); the install now waits for the %v tick", out.Outcome, MirrorInterval)
	}
	if got, ok := rec.last(); !ok || got.Outcome != agentmirror.OutcomeMirrored {
		t.Fatalf("recorded %+v, want outcome %q", got, agentmirror.OutcomeMirrored)
	}
}

// TestWorker_UnreadablePersistedClockDoesNotStopTheMirror: persistence must
// never stop the mirror; on a read failure the run uses its own clock.
func TestWorker_UnreadablePersistedClockDoesNotStopTheMirror(t *testing.T) {
	doer := wire(newFixture(t))
	rec := &fakeRecorder{loadErr: errors.New("connection refused")}
	w := NewMirrorWorker(true, newTestMirror(newFakeStore(), doer), rec, nil)

	if err := w.Work(context.Background(), &river.Job[MirrorArgs]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if got, _ := rec.last(); got.Outcome != agentmirror.OutcomeMirrored {
		t.Fatalf("Outcome = %q, want %q", got.Outcome, agentmirror.OutcomeMirrored)
	}
}

// TestMirrorSeedLastRequestAt_ForwardOnlyAndClamped: a seed never winds the
// clock back, and a future time is clamped to now.
func TestMirrorSeedLastRequestAt_ForwardOnlyAndClamped(t *testing.T) {
	m := newTestMirror(newFakeStore(), newFakeDoer())

	t1 := time.Now().Add(-10 * time.Minute)
	m.SeedLastRequestAt(t1)
	if got := m.LastRequestAt(); !got.Equal(t1) {
		t.Fatalf("after seed: %v, want %v", got, t1)
	}
	m.SeedLastRequestAt(t1.Add(-time.Hour))
	m.SeedLastRequestAt(time.Time{})
	if got := m.LastRequestAt(); !got.Equal(t1) {
		t.Fatalf("an older or zero seed moved the clock to %v, want %v", got, t1)
	}

	before := time.Now()
	m.SeedLastRequestAt(before.Add(24 * time.Hour))
	after := time.Now()
	if got := m.LastRequestAt(); got.Before(before) || got.After(after) {
		t.Fatalf("future seed left the clock at %v, want clamped to now (%v..%v)", got, before, after)
	}
}
