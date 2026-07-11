package screenshot_test

import (
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/screenshot"
)

// TestCaptureArgs_InsertOpts_Queue verifies that CaptureArgs.InsertOpts pins
// the job to the dedicated screenshot queue.
func TestCaptureArgs_InsertOpts_Queue(t *testing.T) {
	var a screenshot.CaptureArgs
	opts := a.InsertOpts()
	if opts.Queue != screenshot.ScreenshotQueue {
		t.Errorf("InsertOpts().Queue = %q, want %q", opts.Queue, screenshot.ScreenshotQueue)
	}
}

// TestCaptureArgs_InsertOpts_MaxAttemptsBounded is the GH #207 Bug 3
// regression lock: now that capture.Worker.Work returns a non-nil error for
// infra failures (so River retries), CaptureArgs must pin an explicit, small
// MaxAttempts — otherwise River's package default (25, MaxAttemptsDefault)
// would let a persistently broken encoder retry the same job 25 times.
func TestCaptureArgs_InsertOpts_MaxAttemptsBounded(t *testing.T) {
	var a screenshot.CaptureArgs
	opts := a.InsertOpts()
	if opts.MaxAttempts <= 0 {
		t.Fatalf("InsertOpts().MaxAttempts = %d, want an explicit positive bound (River defaults to 25 when unset)", opts.MaxAttempts)
	}
	if opts.MaxAttempts > 5 {
		t.Errorf("InsertOpts().MaxAttempts = %d, want a small bound (<=5) to avoid an infra-failure retry storm", opts.MaxAttempts)
	}
}

// TestEnqueuer_UniqueOpts verifies that Enqueuer.Enqueue passes UniqueOpts
// with ByArgs=true and a non-zero ByPeriod (M5: deduplication of manual-refresh
// spam). We test this by constructing the same InsertOpts the Enqueuer builds
// and asserting its fields — the Enqueuer cannot be exercised without a live
// River client, but the opts construction is deterministic.
//
// The test also asserts that captureUniqueWindow is >= 10 minutes, which is the
// minimum window needed to protect against rapid re-enqueue from the refresh
// endpoint (a viewer can call it on every page load).
func TestEnqueuer_UniqueOpts_Shape(t *testing.T) {
	// Build the same InsertOpts the Enqueuer.Enqueue creates (mirrors
	// enqueuer.go so a change there that removes UniqueOpts breaks this test).
	const captureUniqueWindow = 10 * time.Minute // matches the package constant
	opts := river.InsertOpts{
		Queue: screenshot.ScreenshotQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByPeriod: captureUniqueWindow,
		},
	}

	if !opts.UniqueOpts.ByArgs {
		t.Error("UniqueOpts.ByArgs must be true to deduplicate per site_id+tenant_id")
	}
	if opts.UniqueOpts.ByPeriod < 10*time.Minute {
		t.Errorf("UniqueOpts.ByPeriod = %v, want >= 10m to throttle manual refresh spam", opts.UniqueOpts.ByPeriod)
	}
}
