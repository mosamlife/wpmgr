package capture_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/screenshot"
	"github.com/mosamlife/wpmgr/apps/api/internal/screenshot/capture"
)

// ---------------------------------------------------------------------------
// m117 (GH #414) phase 3: screenshot_weekly_fanout pause.
//
// Selection is filtered in listConnectedForScheduledScreenshotSQL (proved
// against the database in apps/api/tests); this file proves the point-of-action
// half — that a capture job queued BEFORE the pause takes no action after it,
// and that a person clicking Refresh still gets a screenshot.
// ---------------------------------------------------------------------------

type pauseChecker struct {
	paused map[uuid.UUID]bool
	err    error
	asked  int
}

func (c *pauseChecker) IsMonitoringPaused(_ context.Context, siteID uuid.UUID) (bool, error) {
	c.asked++
	if c.err != nil {
		return false, c.err
	}
	return c.paused[siteID], nil
}

// captureRecorder counts how many times the browser capture actually ran.
type captureRecorder struct{ ran int }

// pauseNoopRepo absorbs the MarkFailed that follows the deliberate capture
// error. It must never be reached on a skipped capture — a skipped capture
// writes no row at all, which markedFailed asserts.
type pauseNoopRepo struct{ markedFailed int }

func (r *pauseNoopRepo) MarkReady(_ context.Context, _ screenshot.MarkReadyInput) (screenshot.Screenshot, error) {
	return screenshot.Screenshot{}, nil
}

func (r *pauseNoopRepo) MarkFailed(_ context.Context, _, _ uuid.UUID, _ string) (screenshot.Screenshot, error) {
	r.markedFailed++
	return screenshot.Screenshot{}, nil
}

func newPausedCaptureWorker(t *testing.T, checker *pauseChecker, rec *captureRecorder, repo *pauseNoopRepo) *capture.Worker {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// The recorder returns a site-level error, so Work stops right after the
	// capture step: it records the failure and returns nil without touching
	// the store or the event publisher (both nil here on purpose).
	w := capture.NewWorker(repo, nil, nil, 1, logger)
	w.SetCaptureFuncForTest(func(_ context.Context, _ string) ([]byte, []byte, error) {
		rec.ran++
		return nil, nil, errors.New("capture stopped here on purpose")
	})
	w.SetMonitoringPauseChecker(checker)
	return w
}

func captureJob(a screenshot.CaptureArgs) *river.Job[screenshot.CaptureArgs] {
	return &river.Job[screenshot.CaptureArgs]{Args: a}
}

// A scheduled capture queued before the pause takes no action after it.
func TestQueuedScheduledCaptureTakesNoActionAfterAPause(t *testing.T) {
	site := uuid.New()
	checker := &pauseChecker{paused: map[uuid.UUID]bool{site: true}}
	rec := &captureRecorder{}
	repo := &pauseNoopRepo{}
	w := newPausedCaptureWorker(t, checker, rec, repo)

	err := w.Work(context.Background(), captureJob(screenshot.CaptureArgs{
		TenantID: uuid.New(), SiteID: site, SiteURL: "https://paused.example.com",
		Reason: screenshot.ReasonScheduled,
	}))
	if err != nil {
		t.Fatalf("a skipped capture must succeed, not fail the job: %v", err)
	}
	if rec.ran != 0 {
		t.Fatalf("a paused site's queued scheduled capture must take no action, but Chromium ran %d time(s)", rec.ran)
	}
	if checker.asked != 1 {
		t.Fatalf("a scheduled capture must re-check the pause exactly once, asked %d", checker.asked)
	}
	if repo.markedFailed != 0 {
		t.Fatalf("a skipped capture must write no row at all; the dashboard must not learn a pause as a failure (markedFailed=%d)", repo.markedFailed)
	}
}

// The active sibling in the same weekly wave still captures.
func TestActiveSiblingStillCapturesInTheSameWave(t *testing.T) {
	pausedID, activeID := uuid.New(), uuid.New()
	checker := &pauseChecker{paused: map[uuid.UUID]bool{pausedID: true}}
	rec := &captureRecorder{}
	repo := &pauseNoopRepo{}
	w := newPausedCaptureWorker(t, checker, rec, repo)
	tenant := uuid.New()

	_ = w.Work(context.Background(), captureJob(screenshot.CaptureArgs{
		TenantID: tenant, SiteID: pausedID, SiteURL: "https://paused.example.com",
		Reason: screenshot.ReasonScheduled,
	}))
	_ = w.Work(context.Background(), captureJob(screenshot.CaptureArgs{
		TenantID: tenant, SiteID: activeID, SiteURL: "https://active.example.com",
		Reason: screenshot.ReasonScheduled,
	}))

	if rec.ran != 1 {
		t.Fatalf("exactly the active sibling must capture, got %d captures", rec.ran)
	}
}

// A resumed site resumes.
func TestResumedSiteCapturesAgain(t *testing.T) {
	site := uuid.New()
	checker := &pauseChecker{paused: map[uuid.UUID]bool{site: true}}
	rec := &captureRecorder{}
	repo := &pauseNoopRepo{}
	w := newPausedCaptureWorker(t, checker, rec, repo)
	args := screenshot.CaptureArgs{
		TenantID: uuid.New(), SiteID: site, SiteURL: "https://s.example.com",
		Reason: screenshot.ReasonScheduled,
	}

	_ = w.Work(context.Background(), captureJob(args))
	if rec.ran != 0 {
		t.Fatalf("precondition: the paused site must not capture, got %d", rec.ran)
	}

	checker.paused[site] = false // resume
	_ = w.Work(context.Background(), captureJob(args))
	if rec.ran != 1 {
		t.Fatalf("a resumed site must capture again, got %d captures after resume", rec.ran)
	}
}

// A hand-triggered screenshot still works on a paused site — and enroll too.
func TestOperatorCaptureStillRunsOnAPausedSite(t *testing.T) {
	for _, reason := range []screenshot.CaptureReason{screenshot.ReasonManual, screenshot.ReasonEnroll} {
		t.Run(string(reason), func(t *testing.T) {
			site := uuid.New()
			checker := &pauseChecker{paused: map[uuid.UUID]bool{site: true}}
			rec := &captureRecorder{}
			repo := &pauseNoopRepo{}
			w := newPausedCaptureWorker(t, checker, rec, repo)

			_ = w.Work(context.Background(), captureJob(screenshot.CaptureArgs{
				TenantID: uuid.New(), SiteID: site, SiteURL: "https://s.example.com",
				Reason: reason,
			}))

			if rec.ran != 1 {
				t.Fatalf("a %s capture must run on a paused site, got %d", reason, rec.ran)
			}
			if checker.asked != 0 {
				t.Fatalf("a %s capture must not consult the pause at all, asked %d", reason, checker.asked)
			}
		})
	}
}

// A pause-check failure captures anyway rather than silently dropping.
func TestCapturePauseCheckFailureStillCaptures(t *testing.T) {
	checker := &pauseChecker{err: errors.New("db down")}
	rec := &captureRecorder{}
	repo := &pauseNoopRepo{}
	w := newPausedCaptureWorker(t, checker, rec, repo)

	_ = w.Work(context.Background(), captureJob(screenshot.CaptureArgs{
		TenantID: uuid.New(), SiteID: uuid.New(), SiteURL: "https://s.example.com",
		Reason: screenshot.ReasonScheduled,
	}))

	if rec.ran != 1 {
		t.Fatalf("a pause-check failure must not silently drop the capture, got %d", rec.ran)
	}
}

// An unwired checker (nil) leaves behaviour exactly as it was pre-m117.
func TestNilPauseCheckerCapturesAsBefore(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := &captureRecorder{}
	w := capture.NewWorker(&pauseNoopRepo{}, nil, nil, 1, logger)
	w.SetCaptureFuncForTest(func(_ context.Context, _ string) ([]byte, []byte, error) {
		rec.ran++
		return nil, nil, errors.New("capture stopped here on purpose")
	})

	_ = w.Work(context.Background(), captureJob(screenshot.CaptureArgs{
		TenantID: uuid.New(), SiteID: uuid.New(), SiteURL: "https://s.example.com",
		Reason: screenshot.ReasonScheduled,
	}))

	if rec.ran != 1 {
		t.Fatalf("with no checker wired the capture must run, got %d", rec.ran)
	}
}
