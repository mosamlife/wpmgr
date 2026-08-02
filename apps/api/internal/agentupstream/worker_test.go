package agentupstream

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentmirror"
)

// fakeRecorder is an AttemptRecorder test double capturing every recorded
// attempt, so tests can assert on Trigger/Outcome/Detail without a real
// Postgres connection.
type fakeRecorder struct {
	mu    sync.Mutex
	calls []agentmirror.AttemptInput
}

func (f *fakeRecorder) RecordAttempt(_ context.Context, in agentmirror.AttemptInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	return nil
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
