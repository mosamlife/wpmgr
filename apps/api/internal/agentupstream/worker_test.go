package agentupstream

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
)

// TestWorkerDisabledDoesNothing is the off-by-default lock. With the flag false
// (its default, see config.UpdateConfig.AgentMirrorEnabled), the job must make no
// outbound request and write nothing: merging this feature changes nothing until
// an operator opts in.
func TestWorkerDisabledDoesNothing(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	doer := wire(f)
	w := NewMirrorWorker(false, newTestMirror(store, doer), nil)

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
	w := NewMirrorWorker(true, newTestMirror(store, wire(f)), nil)

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
	w := NewMirrorWorker(true, nil, nil)
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
			w := NewMirrorWorker(true, tc.build(t), nil)
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
