package screenshot

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// RiverArgs implements river.JobArgs for site_screenshot_capture.
// Kind() must be stable — changing it orphans in-flight jobs.
func (CaptureArgs) Kind() string { return "site_screenshot_capture" }

// captureMaxAttempts bounds River's retry storm for site_screenshot_capture
// jobs. GH #207 Bug 3: capture.Worker.Work now returns a non-nil error (which
// River retries) for INFRASTRUCTURE capture failures (Chromium missing/launch
// crash, SSRF proxy start failure) so a transient encoder hiccup self-heals,
// instead of the job being recorded "completed" with no further attempt ever
// made. Without an explicit cap, River's package default (MaxAttemptsDefault
// = 25, with exponential backoff) would let a PERSISTENTLY broken encoder
// environment retry the same job 25 times. 3 is enough to ride out a
// transient hiccup (or a redeploy of a broken encoder image) without turning
// a real outage into a retry storm; site-level failures never retry at all
// (Work returns nil for those), so this cap only ever bounds the infra case.
const captureMaxAttempts = 3

// InsertOpts pins every capture job to the screenshot queue with MaxWorkers=2.
// The main API registers this queue with MaxWorkers=0 (insert-only); the
// media-encoder process runs the workers.
func (CaptureArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       ScreenshotQueue,
		MaxAttempts: captureMaxAttempts,
	}
}

// captureUniqueWindow is the deduplication window for manual screenshot refresh
// requests (M5). A viewer with PermSiteRead can call POST /screenshot/refresh
// freely; UniqueOpts on the River insert prevents unbounded Chromium job
// accumulation by deduplicating per site_id+tenant_id within this window.
// 10 minutes is generous — a fresh capture takes at most ~15 s.
const captureUniqueWindow = 10 * time.Minute

// Enqueuer inserts site_screenshot_capture River jobs. Pure-Go; the main API
// (CGO_ENABLED=0) can import and use it.
type Enqueuer struct {
	client *river.Client[pgx.Tx]
}

// NewEnqueuer builds an Enqueuer around the River client.
func NewEnqueuer(client *river.Client[pgx.Tx]) *Enqueuer {
	return &Enqueuer{client: client}
}

// Enqueue inserts a capture job. River de-dupes by (args, period) so rapid
// re-enqueues for the same site within captureUniqueWindow are no-ops (M5).
// Returns the River job ID (which may be an existing job's ID on de-dupe).
func (e *Enqueuer) Enqueue(ctx context.Context, args CaptureArgs) (int64, error) {
	opts := &river.InsertOpts{
		Queue: ScreenshotQueue,
		// M5: deduplicate manual-refresh spam. ByArgs keys on the full
		// CaptureArgs JSON, which includes site_id + tenant_id. ByPeriod
		// ensures at most one pending/running job per site per 10-minute window.
		// ByState is omitted to use the River default (Available + Pending +
		// Running + Scheduled), which is the required set for v3 uniqueness.
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByPeriod: captureUniqueWindow,
		},
	}
	res, err := e.client.Insert(ctx, args, opts)
	if err != nil {
		return 0, fmt.Errorf("enqueue screenshot capture: %w", err)
	}
	return res.Job.ID, nil
}
