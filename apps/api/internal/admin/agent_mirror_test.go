package admin

// agent_mirror_test.go: unit tests for AgentMirrorCheckService (GH #322).
// All in-memory fakes; no DB or River required.

import (
	"context"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentmirror"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentupstream"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// fakeMirrorState is an AgentMirrorStateLoader test double.
type fakeMirrorState struct {
	state agentmirror.State
	err   error
}

func (f *fakeMirrorState) Load(context.Context) (agentmirror.State, error) {
	return f.state, f.err
}

// fakeMirrorEnqueuer is an AgentMirrorCheckEnqueuer test double.
type fakeMirrorEnqueuer struct {
	queued bool
	err    error
	calls  int
}

func (f *fakeMirrorEnqueuer) EnqueueManualMirrorCheck(context.Context) (bool, error) {
	f.calls++
	return f.queued, f.err
}

// TestTriggerCheck_Disabled: mirroring off entirely refuses cleanly with a
// 503-mapped code that names the env var, and never touches the enqueuer.
func TestTriggerCheck_Disabled(t *testing.T) {
	enq := &fakeMirrorEnqueuer{queued: true}
	svc := NewAgentMirrorCheckService(false, true, nil, enq)
	_, err := svc.TriggerCheck(context.Background())
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindServiceUnavailable || de.Code != "agent_mirror_disabled" {
		t.Fatalf("err = %v, want a KindServiceUnavailable agent_mirror_disabled error", err)
	}
	if enq.calls != 0 {
		t.Fatalf("enqueuer called %d times; want 0 while disabled", enq.calls)
	}
}

// TestTriggerCheck_NotConfigured: enabled but object storage never wired
// (wired=false) must refuse, not attempt to enqueue a job that will only
// record OutcomeNotConfigured.
func TestTriggerCheck_NotConfigured(t *testing.T) {
	enq := &fakeMirrorEnqueuer{queued: true}
	svc := NewAgentMirrorCheckService(true, false, nil, enq)
	_, err := svc.TriggerCheck(context.Background())
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindServiceUnavailable || de.Code != "agent_mirror_not_configured" {
		t.Fatalf("err = %v, want a KindServiceUnavailable agent_mirror_not_configured error", err)
	}
	if enq.calls != 0 {
		t.Fatalf("enqueuer called %d times; want 0 when not configured", enq.calls)
	}
}

// TestTriggerCheck_RateLimited pins C3: inside minRequestSpacing of the
// PERSISTED last_request_at, TriggerCheck must refuse with 429 and details,
// and must NEVER enqueue a job: a fake success would lie about having
// checked anything.
func TestTriggerCheck_RateLimited(t *testing.T) {
	enq := &fakeMirrorEnqueuer{queued: true}
	last := time.Now().Add(-5 * time.Minute) // well inside the 30-minute window
	state := &fakeMirrorState{state: agentmirror.State{LastRequestAt: &last}}
	svc := NewAgentMirrorCheckService(true, true, state, enq)

	_, err := svc.TriggerCheck(context.Background())
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindRateLimited || de.Code != "agent_mirror_rate_limited" {
		t.Fatalf("err = %v, want a KindRateLimited agent_mirror_rate_limited error", err)
	}
	retryAfter, ok := de.Details["retry_after_seconds"].(int)
	if !ok || retryAfter <= 0 {
		t.Fatalf("details.retry_after_seconds = %v, want a positive int", de.Details["retry_after_seconds"])
	}
	if _, ok := de.Details["next_check_after"].(string); !ok {
		t.Fatal("details.next_check_after missing")
	}
	if enq.calls != 0 {
		t.Fatalf("enqueuer called %d times; want 0 while rate limited (must not fake success)", enq.calls)
	}
}

// TestTriggerCheck_AllowsAfterSpacingWindow: outside minRequestSpacing, the
// check proceeds to enqueue.
func TestTriggerCheck_AllowsAfterSpacingWindow(t *testing.T) {
	enq := &fakeMirrorEnqueuer{queued: true}
	last := time.Now().Add(-agentupstream.MinRequestSpacing - time.Minute)
	state := &fakeMirrorState{state: agentmirror.State{LastRequestAt: &last}}
	svc := NewAgentMirrorCheckService(true, true, state, enq)

	res, err := svc.TriggerCheck(context.Background())
	if err != nil {
		t.Fatalf("TriggerCheck: %v", err)
	}
	if res.QueuedAt.IsZero() {
		t.Fatal("QueuedAt is zero")
	}
	if enq.calls != 1 {
		t.Fatalf("enqueuer called %d times; want 1", enq.calls)
	}
}

// TestTriggerCheck_NeverRequested: no last_request_at recorded yet (a fresh
// install) must not be treated as rate limited.
func TestTriggerCheck_NeverRequested(t *testing.T) {
	enq := &fakeMirrorEnqueuer{queued: true}
	state := &fakeMirrorState{state: agentmirror.State{}}
	svc := NewAgentMirrorCheckService(true, true, state, enq)

	if _, err := svc.TriggerCheck(context.Background()); err != nil {
		t.Fatalf("TriggerCheck: %v", err)
	}
	if enq.calls != 1 {
		t.Fatalf("enqueuer called %d times; want 1", enq.calls)
	}
}

// TestTriggerCheck_StateReadFailureFailsOpen: a state-load error is a
// courtesy pre-flight failing, not a lock: it must not block the request.
func TestTriggerCheck_StateReadFailureFailsOpen(t *testing.T) {
	enq := &fakeMirrorEnqueuer{queued: true}
	state := &fakeMirrorState{err: context.DeadlineExceeded}
	svc := NewAgentMirrorCheckService(true, true, state, enq)

	if _, err := svc.TriggerCheck(context.Background()); err != nil {
		t.Fatalf("TriggerCheck: %v (a state read failure must fail OPEN)", err)
	}
	if enq.calls != 1 {
		t.Fatalf("enqueuer called %d times; want 1", enq.calls)
	}
}

// TestTriggerCheck_InFlight: the enqueuer reporting a duplicate (River's
// UniqueSkippedAsDuplicate) must map to 409, never to a fresh 202 success.
func TestTriggerCheck_InFlight(t *testing.T) {
	enq := &fakeMirrorEnqueuer{queued: false}
	svc := NewAgentMirrorCheckService(true, true, nil, enq)

	_, err := svc.TriggerCheck(context.Background())
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindConflict || de.Code != "agent_mirror_check_in_flight" {
		t.Fatalf("err = %v, want a KindConflict agent_mirror_check_in_flight error", err)
	}
}

// TestTriggerCheck_EnqueueErrorMapsToInternal: a hard River insert failure
// (not a duplicate) maps to a 500-class internal error, not a false 202/409.
func TestTriggerCheck_EnqueueErrorMapsToInternal(t *testing.T) {
	enq := &fakeMirrorEnqueuer{err: context.Canceled}
	svc := NewAgentMirrorCheckService(true, true, nil, enq)

	_, err := svc.TriggerCheck(context.Background())
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindInternal || de.Code != "agent_mirror_check_failed" {
		t.Fatalf("err = %v, want a KindInternal agent_mirror_check_failed error", err)
	}
}
