// Package capture implements the site_screenshot_capture River worker.
// It is imported ONLY by cmd/media-encoder (which ships with system Chromium).
// The main API (cmd/wpmgr, CGO_ENABLED=0, no Chromium) MUST NOT import this
// package — it only client.Inserts screenshot.CaptureArgs (a pure-Go type).
package capture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/screenshot"
	"github.com/mosamlife/wpmgr/apps/api/internal/screenshot/ssrfproxy"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	siteevents "github.com/mosamlife/wpmgr/apps/api/internal/site/events"
)

// errInfraCapture is a sentinel wrapped around browser/proxy INFRASTRUCTURE
// failures (Chromium missing, SSRF proxy failing to start, Chromium failing
// to launch) so Work can distinguish "the encoder's own environment can't
// capture right now" (transient — worth a bounded River retry, since a fresh
// attempt or a fresh encoder instance may well succeed) from "this particular
// SITE couldn't be captured" (navigation/render/timeout failures AFTER a
// browser connection was established — not transient; retrying won't make a
// down or blocking site come up). GH #207 Bug 3: previously every capture
// failure — including infra failures like a Chromium launch crash — was
// recorded MarkFailed + Work returned nil, which told River the job
// "completed" and it silently never retried even a self-healing infra hiccup.
var errInfraCapture = errors.New("screenshot: infrastructure capture failure")

// CaptureQueue is the dedicated River queue (alias for screenshot.ScreenshotQueue).
const CaptureQueue = screenshot.ScreenshotQueue

// defaultConcurrency caps simultaneous Chromium captures. Each headless Chrome
// context consumes ~150–300 MiB of RAM; 2 concurrent captures is safe on a
// 1 GiB encoder instance. Override with WPMGR_SCREENSHOT_WORKERS env var.
const defaultConcurrency = 2

// captureTimeout is the hard per-capture deadline (Chromium launch + navigate
// + screenshot + upload). 15 s covers slow sites.
const captureTimeout = 15 * time.Second

// Ready-wait tuning (GH #229). Slow-hosting / uncached pages otherwise
// screenshot blank because the capture fires before the page has painted.
// waitUntilReady waits for the load event + network/DOM stability + a fixed
// settle, all hard-bounded so a genuinely broken or slow page degrades to a
// best-effort partial screenshot rather than hanging the worker.
const (
	// networkIdleWindow is the least quiet period WaitStable requires before it
	// treats the network/DOM as idle. It is the stability WINDOW, not a timeout —
	// the timeout is the ready-wait budget below.
	networkIdleWindow = 500 * time.Millisecond

	// renderSettleWait is the fixed post-stability settle that lets late,
	// lazy-loaded / JS-rendered content paint after the network goes idle.
	renderSettleWait = 1500 * time.Millisecond

	// defaultReadyWaitBudget caps the TOTAL time waitUntilReady may spend (load +
	// stability + settle). Chosen below captureTimeout so navigation, the wait,
	// the screenshot, and the upload all fit inside the job deadline.
	defaultReadyWaitBudget = 8 * time.Second
)

// readyWaitBudget resolves the ready-wait ceiling. Honors the optional
// WPMGR_SCREENSHOT_READY_WAIT override (whole seconds — for self-hosters on
// especially slow hosting), clamped to (0, captureTimeout] so a misconfiguration
// can never let a single capture outlast its own capture/job deadline.
func readyWaitBudget() time.Duration {
	if v := os.Getenv("WPMGR_SCREENSHOT_READY_WAIT"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			d := time.Duration(secs) * time.Second
			if d > captureTimeout {
				d = captureTimeout
			}
			return d
		}
	}
	return defaultReadyWaitBudget
}

// settleClamp returns the fixed render-settle delay, shortened to whatever time
// is left in the ready-wait budget so the settle sleep can never push the total
// wait past the budget. Returns 0 when no budget remains.
func settleClamp(remaining time.Duration) time.Duration {
	if remaining <= 0 {
		return 0
	}
	if remaining < renderSettleWait {
		return remaining
	}
	return renderSettleWait
}

// captureWidth / captureHeight are the 1x viewport dimensions.
const (
	captureWidth  = 1280
	captureHeight = 800
)

// scale2x is the pixel-ratio for the @2x retina thumbnail.
const scale2x = 2.0

// defaultChromiumBin is the system Chromium path baked into the encoder image.
const defaultChromiumBin = "/usr/bin/chromium-browser"

// chromiumBinPath resolves the Chromium binary path at call time. It honors the
// WPMGR_CHROMIUM_BIN override (for self-hosters whose Chromium lives elsewhere,
// and for tests that must force the not-found path deterministically regardless
// of what the host/CI runner happens to have installed).
func chromiumBinPath() string {
	if v := os.Getenv("WPMGR_CHROMIUM_BIN"); v != "" {
		return v
	}
	return defaultChromiumBin
}

// Repo is the subset of the screenshot repo the worker needs.
type Repo interface {
	MarkReady(ctx context.Context, in screenshot.MarkReadyInput) (screenshot.Screenshot, error)
	MarkFailed(ctx context.Context, tenantID, siteID uuid.UUID, reason string) (screenshot.Screenshot, error)
}

// StoreWriter is the blob-write interface the worker needs.
// *blobstore.Store satisfies this via PutViaPresign + Delete.
type StoreWriter interface {
	PutViaPresign(ctx context.Context, key string, body io.Reader, size int64) error
	Delete(ctx context.Context, key string) error
}

// EventPublisher publishes SSE envelopes (optional; nil disables events).
// site.EventPublisher satisfies this.
type EventPublisher interface {
	Publish(ctx context.Context, ev site.ConnectionEvent) error
}

// Worker is the site_screenshot_capture River worker.
type Worker struct {
	river.WorkerDefaults[screenshot.CaptureArgs]
	repo   Repo
	store  StoreWriter
	events EventPublisher
	logger *slog.Logger
	// sem caps concurrent Chromium captures. Sized at construction.
	sem chan struct{}
	// captureFn is the browser-capture step, extracted as a field (defaulting
	// to w.capture) so tests can substitute a fake implementation to exercise
	// the infra-vs-site-level error classification in Work() without
	// launching a real Chromium browser.
	captureFn func(ctx context.Context, siteURL string) (img1x, img2x []byte, err error)
}

// NewWorker builds the capture worker.
func NewWorker(
	repo Repo,
	store StoreWriter,
	events EventPublisher,
	concurrency int,
	logger *slog.Logger,
) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}
	w := &Worker{
		repo:   repo,
		store:  store,
		events: events,
		logger: logger,
		sem:    make(chan struct{}, concurrency),
	}
	w.captureFn = w.capture
	return w
}

// SetCaptureFuncForTest overrides the browser-capture step. Exported so tests
// in other packages (capture_test) can simulate infra vs. site-level capture
// failures deterministically, without a real Chromium binary. Production code
// must never call this — NewWorker already wires captureFn to w.capture.
func (w *Worker) SetCaptureFuncForTest(fn func(ctx context.Context, siteURL string) (img1x, img2x []byte, err error)) {
	w.captureFn = fn
}

// Timeout gives each capture job 30 s.
func (w *Worker) Timeout(*river.Job[screenshot.CaptureArgs]) time.Duration {
	return 30 * time.Second
}

// Work captures a screenshot for one site.
// Idempotent: re-running for the same site_id overwrites the prior row and
// best-effort deletes the prior GCS object.
func (w *Worker) Work(ctx context.Context, job *river.Job[screenshot.CaptureArgs]) error {
	a := job.Args

	// Acquire a concurrency slot (blocks until one is free or ctx expires).
	select {
	case w.sem <- struct{}{}:
		defer func() { <-w.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	capCtx, cancel := context.WithTimeout(ctx, captureTimeout)
	defer cancel()

	img1x, img2x, err := w.captureFn(capCtx, a.SiteURL)
	if err != nil {
		reason := err.Error()
		w.logger.WarnContext(ctx, "screenshot capture failed",
			slog.String("site_id", a.SiteID.String()),
			slog.String("reason", reason),
			slog.Bool("infra", errors.Is(err, errInfraCapture)))
		// MarkFailed unconditionally — the dashboard's "didn't finish" state
		// must reflect every failure, infra or site-level, so the operator
		// always sees the latest outcome regardless of whether River retries.
		_, _ = w.repo.MarkFailed(ctx, a.TenantID, a.SiteID, reason)
		if errors.Is(err, errInfraCapture) {
			// Infrastructure failure (Chromium missing/launch crash, SSRF
			// proxy start failure): return the error so River retries, bounded
			// by CaptureArgs.InsertOpts().MaxAttempts. A transient encoder
			// hiccup — or a self-healed environment on the next attempt —
			// recovers automatically instead of being permanently stuck
			// "completed" with no further attempt ever made.
			return err
		}
		// Site-level failure (navigation/render/timeout after a browser
		// connection was established): return nil — not a transient error
		// worth retrying (if the site is down or blocks headless browsers,
		// retries won't help). The operator can request a manual refresh.
		return nil
	}

	// Mint a new ULID for the object key so every capture is a new unique object
	// (prevents CDN/browser caching of the prior screenshot).
	capULID := siteevents.NewULID(time.Now())
	key1x := fmt.Sprintf("screenshots/%s/%s/%s.webp", a.TenantID, a.SiteID, capULID)
	key2x := fmt.Sprintf("screenshots/%s/%s/%s@2x.webp", a.TenantID, a.SiteID, capULID)

	if err := w.store.PutViaPresign(ctx, key1x, bytes.NewReader(img1x), int64(len(img1x))); err != nil {
		return fmt.Errorf("screenshot: upload 1x: %w", err)
	}

	if err := w.store.PutViaPresign(ctx, key2x, bytes.NewReader(img2x), int64(len(img2x))); err != nil {
		// 2x upload failure is non-fatal — mark ready with the 1x key only.
		w.logger.WarnContext(ctx, "screenshot: upload 2x failed (non-fatal)",
			slog.String("site_id", a.SiteID.String()),
			slog.Any("error", err))
		key2x = ""
	}

	now := time.Now()
	row, err := w.repo.MarkReady(ctx, screenshot.MarkReadyInput{
		SiteID:          a.SiteID,
		TenantID:        a.TenantID,
		ScreenshotKey:   key1x,
		ScreenshotKey2x: key2x,
		Width:           captureWidth,
		Height:          captureHeight,
		CapturedAt:      now,
	})
	if err != nil {
		return fmt.Errorf("screenshot: mark ready: %w", err)
	}

	// Best-effort SSE notification.
	w.publish(ctx, a, map[string]any{
		"screenshot_status":      row.Status,
		"screenshot_captured_at": now,
	})

	w.logger.InfoContext(ctx, "screenshot captured",
		slog.String("site_id", a.SiteID.String()),
		slog.String("key_1x", key1x),
		slog.Int("bytes_1x", len(img1x)),
		slog.Int("bytes_2x", len(img2x)))
	return nil
}

// capture launches a fresh headless Chromium browser context behind the SSRF
// proxy, navigates to siteURL, waits for the page to settle, then takes a
// full-page screenshot at 1x and 2x. Returns raw WebP bytes for each.
//
// SSRF model: the SSRF-guarded proxy (ssrfproxy.New) sits between Chromium and
// the network. Chromium's --proxy-server flag routes ALL connections — the top
// navigation, every redirect hop, and every sub-resource — through the proxy's
// CONNECT tunnel. The proxy's dialer runs code.dny.dev/ssrf.Safe on the
// resolved IP at connect time, rejecting RFC1918 / link-local / loopback.
// This is the authoritative guard: Chromium never touches the network directly.
func (w *Worker) capture(ctx context.Context, siteURL string) (img1x, img2x []byte, err error) {
	// Fast-fail when Chromium is not installed (clear error message).
	chromiumBin := chromiumBinPath()
	if _, lookupErr := exec.LookPath(chromiumBin); lookupErr != nil {
		return nil, nil, fmt.Errorf("chromium not found at %s: install chromium in the media-encoder image: %w", chromiumBin, errInfraCapture)
	}

	// 1. Start the SSRF proxy. Every TCP connection Chromium makes goes through
	//    this proxy, whose dialer calls ssrf.Safe (same guard as httpclient).
	proxy, proxyErr := ssrfproxy.New(w.logger)
	if proxyErr != nil {
		return nil, nil, fmt.Errorf("ssrf proxy start: %w: %w", proxyErr, errInfraCapture)
	}
	defer proxy.Stop()

	// 2. Build the launcher with security-hardening flags.
	//
	// UDP egress note: --proxy-server only covers TCP (CONNECT tunnels). QUIC
	// (HTTP/3) and WebRTC are UDP and bypass the proxy entirely, which lets a
	// hostile page reach private IPs via Alt-Svc upgrade or WebRTC ICE. We
	// therefore disable every out-of-band UDP path at the browser level:
	//   - disable-quic:                  no QUIC / HTTP-3 upgrades
	//   - disable-features=...:          no DNS-over-HTTPS or SVCB out-of-band
	//   - force-webrtc-ip-handling-policy: WebRTC UDP goes through the proxy
	l := launcher.New().
		Bin(chromiumBin).
		Set("headless", "new").
		Set("no-sandbox").
		Set("disable-dev-shm-usage").
		Set("incognito").
		Set("disable-background-networking").
		Set("disable-client-side-phishing-detection").
		Set("disable-default-apps").
		Set("disable-extensions").
		Set("disable-hang-monitor").
		Set("disable-popup-blocking").
		Set("disable-prompt-on-repost").
		Set("disable-sync").
		Set("disable-translate").
		Set("metrics-recording-only").
		Set("no-first-run").
		Set("safebrowsing-disable-auto-update").
		// Route ALL Chromium connections through the SSRF proxy. This is the
		// load-bearing security control: every sub-resource and redirect goes
		// through ssrfproxy which runs ssrf.Safe at dial time.
		Set("proxy-server", "http://"+proxy.Addr()).
		// Close the UDP/QUIC/WebRTC egress bypass (B1).
		// The SSRF proxy is TCP-only (HTTP CONNECT). Without these flags a
		// hostile page can Alt-Svc upgrade to QUIC (HTTP/3, UDP) or open a
		// WebRTC data channel — both bypass the proxy and can reach private IPs.
		Set("disable-quic").
		Set("disable-features", "UseDnsHttpsSvcb,EnableDnsOverHttps").
		Set("force-webrtc-ip-handling-policy", "disable_non_proxied_udp").
		// Renderer memory + GPU hardening (M2).
		// Caps the V8 old-space to 512 MiB so a hostile page cannot OOM the
		// shared encoder instance. Disabling GPU/software-rasterizer removes
		// the GPU process and its additional attack surface.
		Set("js-flags", "--max-old-space-size=512").
		Set("disable-gpu").
		Set("disable-software-rasterizer").
		Headless(true)

	browserURL, launchErr := l.Launch()
	if launchErr != nil {
		return nil, nil, fmt.Errorf("chromium launch: %w: %w", launchErr, errInfraCapture)
	}
	defer func() { l.Cleanup() }()

	browser := rod.New().ControlURL(browserURL).MustConnect()
	defer browser.MustClose()

	// Block downloads at the browser level so malicious pages can't write files.
	_ = proto.BrowserSetDownloadBehavior{
		Behavior: proto.BrowserSetDownloadBehaviorBehaviorDeny,
	}.Call(browser)

	// 3. Open a fresh page (incognito flag ensures no cookie/cache bleed).
	page, pageErr := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if pageErr != nil {
		return nil, nil, fmt.Errorf("chromium page open: %w", pageErr)
	}
	defer func() { _ = page.Close() }()

	// Set viewport (1x).
	if setErr := page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width:             captureWidth,
		Height:            captureHeight,
		DeviceScaleFactor: 1.0,
		Mobile:            false,
	}); setErr != nil {
		return nil, nil, fmt.Errorf("set viewport: %w", setErr)
	}

	// 4. Navigate with a hard timeout.
	navCtx, navCancel := context.WithTimeout(ctx, captureTimeout)
	defer navCancel()

	if navErr := page.Context(navCtx).Navigate(siteURL); navErr != nil {
		return nil, nil, fmt.Errorf("navigate %s: %w", siteURL, navErr)
	}

	// 4a. Wait for the page to actually finish rendering before capturing so
	//     slow-hosting / uncached sites don't screenshot blank (GH #229). This
	//     is best-effort and hard-bounded — see waitUntilReady.
	w.waitUntilReady(ctx, page)

	// 5. Take the 1x screenshot (WebP, 85% quality).
	shot1x, snapErr := page.Screenshot(false, &proto.PageCaptureScreenshot{
		Format:  proto.PageCaptureScreenshotFormatWebp,
		Quality: intPtr(85),
	})
	if snapErr != nil {
		return nil, nil, fmt.Errorf("screenshot 1x: %w", snapErr)
	}

	// 6. Switch to 2x (retina) device scale and re-capture.
	if setErr := page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width:             captureWidth,
		Height:            captureHeight,
		DeviceScaleFactor: scale2x,
		Mobile:            false,
	}); setErr != nil {
		w.logger.Warn("screenshot: 2x viewport failed (using 1x for both)", slog.Any("error", setErr))
		return shot1x, shot1x, nil
	}

	shot2x, snap2xErr := page.Screenshot(false, &proto.PageCaptureScreenshot{
		Format:  proto.PageCaptureScreenshotFormatWebp,
		Quality: intPtr(85),
	})
	if snap2xErr != nil {
		w.logger.Warn("screenshot: 2x snapshot failed (using 1x for both)", slog.Any("error", snap2xErr))
		return shot1x, shot1x, nil
	}

	return shot1x, shot2x, nil
}

// waitUntilReady blocks until the page is visually ready to screenshot, or the
// ready-wait budget elapses — whichever comes first (GH #229). Without it,
// slow-hosting or uncached pages screenshot blank because the capture fires
// before the page has painted. Every step is best-effort and hard-bounded:
//
//   - readyCtx caps the load + network/DOM stability wait at readyWaitBudget()
//     (also transitively bounded by the caller's capture ctx, which carries the
//     captureTimeout deadline).
//   - WaitStable's argument is the least-idle WINDOW, not a timeout; the timeout
//     is readyCtx, so a page that never stabilizes ends the wait at the budget
//     instead of looping forever.
//   - the trailing fixed settle (so late lazy-loaded / JS-rendered paints land)
//     is clamped to the budget's remaining time.
//
// It deliberately NEVER returns an error and NEVER weakens the navigation ctx or
// SSRF proxy: a page that never stabilizes is captured with whatever HAS
// rendered, because a partial screenshot beats both a blank one and a hung
// worker. It cannot hang — both the stability wait and the settle are bounded,
// and the whole function is bounded by the caller's ctx.
func (w *Worker) waitUntilReady(ctx context.Context, page *rod.Page) {
	deadline := time.Now().Add(readyWaitBudget())
	readyCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	// Load event + network-request idle + DOM stability, all bounded by readyCtx.
	// The error (including deadline-exceeded) is intentionally ignored — we go on
	// to capture whatever has rendered.
	_ = page.Context(readyCtx).WaitStable(networkIdleWindow)

	// Fixed settle for late lazy-loaded images / JS-rendered content, clamped so
	// it can never extend the total wait past the budget.
	if settle := settleClamp(time.Until(deadline)); settle > 0 {
		select {
		case <-time.After(settle):
		case <-ctx.Done():
		}
	}
}

func (w *Worker) publish(ctx context.Context, a screenshot.CaptureArgs, data map[string]any) {
	if w.events == nil {
		return
	}
	_ = w.events.Publish(ctx, site.ConnectionEvent{
		Type:     "screenshot.updated",
		TenantID: a.TenantID,
		SiteID:   a.SiteID,
		Data:     data,
	})
}

func intPtr(i int) *int { return &i }

// ---- Weekly fanout worker ----

// WeeklyFanoutArgs is the River job payload for the weekly screenshot sweep.
type WeeklyFanoutArgs struct{}

// Kind implements river.JobArgs.
func (WeeklyFanoutArgs) Kind() string { return "screenshot_weekly_fanout" }

// InsertOpts pins to the screenshot queue.
func (WeeklyFanoutArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: CaptureQueue}
}

// SiteIDLister lists all non-archived, connected site IDs across all tenants
// (runs under InAgentTx).
type SiteIDLister interface {
	ListConnectedSiteIDs(ctx context.Context) ([]SiteIDWithTenantAndURL, error)
}

// SiteIDWithTenantAndURL is the slim projection the fanout worker iterates.
type SiteIDWithTenantAndURL struct {
	SiteID   uuid.UUID
	TenantID uuid.UUID
	URL      string
}

// FanoutEnqueuer inserts individual capture jobs.
type FanoutEnqueuer interface {
	Enqueue(ctx context.Context, args screenshot.CaptureArgs) (int64, error)
}

// WeeklyFanoutWorker fans out screenshot captures across all connected sites.
// Concurrency-capped and schedule-jittered to avoid stampeding the encoder.
type WeeklyFanoutWorker struct {
	river.WorkerDefaults[WeeklyFanoutArgs]
	sites     SiteIDLister
	enqueuer  FanoutEnqueuer
	logger    *slog.Logger
	fanoutCap int
}

// NewWeeklyFanoutWorker builds the fanout worker.
// enqueuer may be nil at construction and wired later via SetEnqueuer
// (mirrors the post-River-start wiring pattern used by other workers).
func NewWeeklyFanoutWorker(
	sites SiteIDLister,
	enqueuer FanoutEnqueuer,
	fanoutCap int,
	logger *slog.Logger,
) *WeeklyFanoutWorker {
	if logger == nil {
		logger = slog.Default()
	}
	if fanoutCap <= 0 {
		fanoutCap = 500
	}
	return &WeeklyFanoutWorker{
		sites:     sites,
		enqueuer:  enqueuer,
		logger:    logger,
		fanoutCap: fanoutCap,
	}
}

// SetEnqueuer wires the River enqueuer post-River-start. Call this before the
// first periodic fanout job fires (i.e. immediately after river.NewClient).
func (w *WeeklyFanoutWorker) SetEnqueuer(e FanoutEnqueuer) {
	w.enqueuer = e
}

// Timeout gives the fanout worker 10 minutes.
func (w *WeeklyFanoutWorker) Timeout(*river.Job[WeeklyFanoutArgs]) time.Duration {
	return 10 * time.Minute
}

// Work lists connected sites and enqueues a capture job for each, up to fanoutCap.
// A small per-enqueue jitter avoids bursting the job-table insert rate on large fleets.
func (w *WeeklyFanoutWorker) Work(ctx context.Context, job *river.Job[WeeklyFanoutArgs]) error {
	if w.enqueuer == nil {
		w.logger.WarnContext(ctx, "screenshot fanout: enqueuer not wired, skipping")
		return nil
	}
	sites, err := w.sites.ListConnectedSiteIDs(ctx)
	if err != nil {
		return fmt.Errorf("screenshot fanout: list sites: %w", err)
	}

	// Shuffle so the cap doesn't always skip the same tail of sites across runs.
	rand.Shuffle(len(sites), func(i, j int) { sites[i], sites[j] = sites[j], sites[i] })

	var (
		enqueued int
		skipped  int
		mu       sync.Mutex
	)

	for _, s := range sites {
		if enqueued >= w.fanoutCap {
			skipped++
			continue
		}
		if _, err := w.enqueuer.Enqueue(ctx, screenshot.CaptureArgs{
			SiteID:   s.SiteID,
			TenantID: s.TenantID,
			SiteURL:  s.URL,
			Reason:   screenshot.ReasonScheduled,
		}); err != nil {
			w.logger.WarnContext(ctx, "screenshot fanout: enqueue failed",
				slog.String("site_id", s.SiteID.String()),
				slog.Any("error", err))
			continue
		}
		mu.Lock()
		enqueued++
		mu.Unlock()

		// Jitter: 0–50ms to avoid a thundering herd on the job table.
		jitter := time.Duration(rand.IntN(50)) * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(jitter):
		}
	}

	w.logger.InfoContext(ctx, "screenshot fanout complete",
		slog.Int("enqueued", enqueued),
		slog.Int("skipped_cap", skipped),
		slog.Int("total_sites", len(sites)))
	return nil
}

// Ensure http package is used (for the proxy CONNECT handler compilation).
var _ = http.StatusOK
