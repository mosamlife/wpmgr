package update

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// fakeRepo is a minimal Repo implementation for handler-level tests. Only the
// methods the events handler reaches through (GetRun + ListTasks) need real
// behaviour; the rest panic so a future test that drifts into them fails
// loudly.
type fakeRepo struct {
	run   Run
	tasks []Task
}

func (f *fakeRepo) CreateRunWithTasks(context.Context, CreateRunInput, []NewTask) (Run, []Task, error) {
	panic("not used")
}

func (f *fakeRepo) GetRun(_ context.Context, _ uuid.UUID, runID uuid.UUID) (Run, error) {
	if runID != f.run.ID {
		return Run{}, domain.NotFound("run_not_found", "no such run")
	}
	return f.run, nil
}

func (f *fakeRepo) ListRuns(context.Context, uuid.UUID, int32, int32) ([]Run, error) {
	panic("not used")
}

func (f *fakeRepo) ListTasks(_ context.Context, _ uuid.UUID, _ uuid.UUID) ([]Task, error) {
	return f.tasks, nil
}

func (f *fakeRepo) GetTask(context.Context, uuid.UUID, uuid.UUID) (Task, error) {
	panic("not used")
}

func (f *fakeRepo) MarkTaskRunning(context.Context, uuid.UUID, uuid.UUID) (Task, error) {
	panic("not used")
}

func (f *fakeRepo) FinishTask(context.Context, FinishTaskInput) (Task, error) {
	panic("not used")
}

func (f *fakeRepo) SetRunStatus(context.Context, uuid.UUID, uuid.UUID, string) (Run, error) {
	panic("not used")
}

func (f *fakeRepo) CountUnfinishedTasks(context.Context, uuid.UUID, uuid.UUID) (int64, error) {
	panic("not used")
}

func (f *fakeRepo) CountRunningTasksForTenant(context.Context, uuid.UUID) (int64, error) {
	panic("not used")
}

// flushableRecorder wraps httptest.ResponseRecorder so gin's writer implements
// http.Flusher (the events handler checks for it before opening the stream).
type flushableRecorder struct {
	*httptest.ResponseRecorder
}

func (f *flushableRecorder) Flush() {}

// newEventsContext builds a gin.Context that targets GET /events on a stand-in
// run id, wired with the tenant context the events handler expects.
func newEventsContext(t *testing.T, tenantID, runID uuid.UUID) (*gin.Context, *flushableRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := &flushableRecorder{ResponseRecorder: httptest.NewRecorder()}
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/updates/"+runID.String()+"/events", nil)
	req = req.WithContext(domain.WithTenantID(req.Context(), tenantID))
	ctx.Request = req
	ctx.Params = gin.Params{{Key: "runId", Value: runID.String()}}
	return ctx, rec
}

// TestEventsHandlerLateSubscriberGetsCurrentSnapshot is the regression test for
// the v0.9.0 stuck-at-Queued bug: when a real run completes BEFORE the browser
// subscribes (the 12 s window observed in production), the events handler MUST
// still flush a current-state event for every task so the client cache
// transitions from the pending POST response to the terminal state. Previously
// the client never patched these frames because they were emitted under the
// `event: task` name and the browser used onmessage; this test pins down the
// server-side contract so future refactors don't accidentally regress it.
func TestEventsHandlerLateSubscriberGetsCurrentSnapshot(t *testing.T) {
	tenantID := uuid.New()
	runID := uuid.New()
	taskID := uuid.New()
	siteID := uuid.New()
	finished := time.Now().UTC()

	repo := &fakeRepo{
		run: Run{
			ID:        runID,
			TenantID:  tenantID,
			Status:    RunCompleted,
			CreatedAt: finished.Add(-30 * time.Second),
			UpdatedAt: finished,
		},
		tasks: []Task{{
			ID:          taskID,
			RunID:       runID,
			TenantID:    tenantID,
			SiteID:      siteID,
			TargetType:  TargetPlugin,
			TargetSlug:  "woocommerce/woocommerce.php",
			Status:      TaskSucceeded,
			FromVersion: "10.8.0",
			ToVersion:   "10.8.1",
			Detail:      "updated and healthy",
			FinishedAt:  &finished,
			CreatedAt:   finished.Add(-30 * time.Second),
			UpdatedAt:   finished,
		}},
	}

	h := NewHandler(NewService(repo, nil, nil, nil, nil), NewHub(), nil)

	ctx, rec := newEventsContext(t, tenantID, runID)
	h.events(ctx)

	body := rec.Body.String()
	if !strings.Contains(body, "event: task") {
		t.Fatalf("expected initial frame to use the named `task` event, body=\n%s", body)
	}
	if !strings.Contains(body, `"status":"succeeded"`) {
		t.Fatalf("expected the terminal task status to be in the initial frame, body=\n%s", body)
	}
	if !strings.Contains(body, `"run_status":"completed"`) {
		t.Fatalf("expected the terminal run status to be in the initial frame, body=\n%s", body)
	}
}

// TestEventsHandlerEmitsNamedEventForLiveTransitions exercises the live
// (in-flight) path: a fresh run with a pending task, the worker publishes a
// running transition, the handler MUST forward it as an `event: task` frame.
// Pairs with the late-subscriber test above; together they cover both the
// snapshot-on-subscribe and the live-fanout paths.
func TestEventsHandlerEmitsNamedEventForLiveTransitions(t *testing.T) {
	tenantID := uuid.New()
	runID := uuid.New()
	taskID := uuid.New()
	siteID := uuid.New()
	created := time.Now().UTC()

	repo := &fakeRepo{
		run: Run{
			ID:        runID,
			TenantID:  tenantID,
			Status:    RunRunning,
			CreatedAt: created,
			UpdatedAt: created,
		},
		tasks: []Task{{
			ID:         taskID,
			RunID:      runID,
			TenantID:   tenantID,
			SiteID:     siteID,
			TargetType: TargetPlugin,
			TargetSlug: "akismet/akismet.php",
			Status:     TaskPending,
			CreatedAt:  created,
			UpdatedAt:  created,
		}},
	}

	hub := NewHub()
	h := NewHandler(NewService(repo, nil, nil, nil, nil), hub, nil)

	ctx, rec := newEventsContext(t, tenantID, runID)
	// The events handler blocks until ctx cancellation or terminal run_status.
	// Drive completion via the hub from a goroutine, then wait briefly for the
	// handler to flush + return.
	done := make(chan struct{})
	go func() {
		h.events(ctx)
		close(done)
	}()
	// Give the handler a moment to subscribe and flush the initial pending
	// snapshot, then publish a terminal transition.
	time.Sleep(20 * time.Millisecond)
	hub.Publish(Event{
		RunID:      runID,
		TaskID:     taskID,
		SiteID:     siteID,
		TargetType: TargetPlugin,
		TargetSlug: "akismet/akismet.php",
		Status:     TaskSucceeded,
		RunStatus:  RunCompleted,
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("events handler did not return after terminal hub event")
	}

	body := rec.Body.String()
	// Initial snapshot (pending) must appear.
	if !strings.Contains(body, `"status":"pending"`) {
		t.Fatalf("expected initial pending snapshot, body=\n%s", body)
	}
	// Live succeeded transition must appear.
	if !strings.Contains(body, `"status":"succeeded"`) {
		t.Fatalf("expected live succeeded frame, body=\n%s", body)
	}
	// All frames are named `task` (the contract the browser EventSource
	// listens on).
	for _, line := range bytes.Split([]byte(body), []byte("\n\n")) {
		if len(line) == 0 {
			continue
		}
		if bytes.HasPrefix(line, []byte(":")) {
			continue // heartbeat comment
		}
		if !bytes.HasPrefix(line, []byte("event: task\n")) {
			t.Fatalf("unexpected frame missing `event: task` prefix:\n%s", line)
		}
	}
}
