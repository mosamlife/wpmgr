// Command media-encoder is the OPTIONAL, separately-deployed image-encode worker
// for the WPMgr Media Optimizer (ADR-043). It runs a River worker on the bounded
// `media_encode` queue, presigned-GETs source variants from object storage,
// encodes them with the CGO lilliput encoder, presigned-PUTs the outputs, writes
// media_variant_results, and dispatches the signed `media_apply` command so the
// agent applies on disk.
//
// THIS BINARY IS THE ONLY PLACE THAT IMPORTS internal/media/encoder (CGO +
// lilliput). The main API (cmd/wpmgr) builds CGO_ENABLED=0 on distroless/static
// and NEVER imports the encoder — it only client.Inserts model.EncodeArgs (a
// pure-Go River job type). This binary builds CGO_ENABLED=1 on a glibc base
// (infra/Dockerfile.media-encoder).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/blobstore"
	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/encoder"
	mediafont "github.com/mosamlife/wpmgr/apps/api/internal/media/font"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/model"
	mediarepo "github.com/mosamlife/wpmgr/apps/api/internal/media/repo"
	mediaworker "github.com/mosamlife/wpmgr/apps/api/internal/media/worker"
	"github.com/mosamlife/wpmgr/apps/api/internal/perf"
	"github.com/mosamlife/wpmgr/apps/api/internal/riverutil"
	"github.com/mosamlife/wpmgr/apps/api/internal/screenshot"
	"github.com/mosamlife/wpmgr/apps/api/internal/screenshot/capture"
	siteevents "github.com/mosamlife/wpmgr/apps/api/internal/site/events"
)

// defaultEncodeWorkers bounds the media_encode queue concurrency. libaom AVIF is
// CPU-bound and a single still image can't saturate many cores, so throughput
// comes from encoding several images in PARALLEL (workers) rather than more
// threads per image. The lilliput ImageOps pool is sized to match. Sized for the
// recommended multi-vCPU encoder instance; override with WPMGR_MEDIA_ENCODE_WORKERS.
const defaultEncodeWorkers = 3

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", slog.Any("error", err))
		os.Exit(1)
	}
}

// run boots the media-encoder: loads config, ensures the isolated River schema
// when configured, wires workers, and starts the health/drain server.
func run() error {
	cfg, err := config.Load(os.Getenv("WPMGR_CONFIG_FILE"))
	if err != nil {
		return err
	}
	logger := newLogger(cfg)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mediaSchema, err := riverutil.NormalizeSchema(cfg.River.MediaSchema)
	if err != nil {
		return err
	}
	// Log the resolved media River schema. The encoder polls this schema while
	// the API inserts into it; a mismatch silently strands jobs, so emitting it
	// on each process makes an operator eyeball-diff trivial.
	mediaSchemaLog := mediaSchema
	if riverutil.IsDefaultSchema(mediaSchema) {
		mediaSchemaLog = "public"
	}
	logger.Info("media River schema resolved", slog.String("media_river_schema", mediaSchemaLog))

	// GH #205 (CRITICAL): this binary runs WORKERS, which makes it a River
	// leadership candidate on whatever schema it connects to. River leader
	// election and the periodic-job scheduler are per-schema, and only the
	// elected leader's own client runs its PeriodicJobs. If this process
	// shares the API's default/public schema, it can win leadership and
	// silently stop the API's ENTIRE fleet cron (uptime_probe,
	// backup_scheduler, site_connection_sweep, health-check, reapers, every
	// GC/rollup) with no error anywhere. Refuse to boot rather than risk it —
	// schema isolation is mandatory, not advisory.
	if riverutil.IsDefaultSchema(mediaSchema) {
		return errEnv("WPMGR_RIVER_MEDIA_SCHEMA must be set to a dedicated schema (e.g. \"media_encoder\"); " +
			"running on the API's default/public River schema silently steals leader election and stops " +
			"ALL fleet periodic jobs (GH #205)")
	}

	if !cfg.S3.Enabled() {
		return errEnv("WPMGR_S3_BUCKET is required: the media-encoder transfers bytes via presigned object storage")
	}

	// Connect with the unprivileged app DSN (RLS-enforced; the worker runs under
	// the app.agent GUC, exactly like the main API's cross-tenant jobs).
	pool, err := db.Connect(ctx, cfg.DB.DSN())
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.EnforceRLSRole(ctx, logger, cfg.DB.AllowRLSBypassRole); err != nil {
		return err
	}
	if !riverutil.IsDefaultSchema(mediaSchema) {
		migPool, err := db.Connect(ctx, cfg.DB.MigrateDSN())
		if err != nil {
			return err
		}
		if err := riverutil.EnsureSchema(ctx, migPool.Pool, mediaSchema, cfg.DB.User); err != nil {
			migPool.Close()
			return err
		}
		migPool.Close()
	}

	// Object storage (presigned URLs only — never a live GetObject).
	store, err := blobstore.New(blobstore.Config{
		Endpoint:       cfg.S3.Endpoint,
		Region:         cfg.S3.Region,
		Bucket:         cfg.S3.Bucket,
		AccessKey:      cfg.S3.AccessKey,
		SecretKey:      cfg.S3.SecretKey,
		ForcePathStyle: cfg.S3.ForcePathStyle,
	})
	if err != nil {
		return err
	}

	// CP->agent signed media_apply client. When the signing key is empty the
	// disabled client refuses (the job will retry/fail), mirroring the API.
	var applyClient mediaworker.AgentApplyClient = disabledApply{}
	if cfg.Agent.SigningPrivateKey != "" {
		signer, serr := agentcmd.NewSigner(cfg.Agent.SigningPrivateKey)
		if serr != nil {
			return serr
		}
		// Generous per-attempt timeout: media_apply makes the agent download +
		// apply ≤10 variants. Reuse the backup HTTP timeout budget.
		applyHTTP := httpclient.New(httpclient.Config{
			Timeout:    cfg.Backup.HTTPTimeout,
			MaxRetries: 0,
		})
		applyClient = agentcmd.NewClient(applyHTTP, signer)
	} else {
		logger.Warn("WPMGR_AGENT_SIGNING_PRIVATE_KEY empty: media_apply dispatch disabled (encode jobs will not finalize)")
	}

	workers := encodeWorkerCount()
	clock := domain.SystemClock{}

	// Build the CGO encoder with a pool sized to the worker concurrency.
	enc := encoder.NewLilliputEncoder(workers)
	defer enc.Close()

	repo := mediarepo.NewRepo(pool)
	eventsPub := siteevents.NewPublisher(pool, clock)
	siteLookup := mediaworker.NewDBSiteLookup(pool)

	// The agent's media_apply status callback (job-status) MUST be an absolute
	// CP URL — the agent posts it via wp_remote_post(), which rejects a relative
	// path. Same env the API uses to build its agent-facing callback URLs.
	cpBaseURL := os.Getenv("WPMGR_PUBLIC_BASE_URL")
	if cpBaseURL == "" {
		return errEnv("WPMGR_PUBLIC_BASE_URL is required: the media_apply job-status callback must be an absolute CP URL")
	}

	encodeWorker := mediaworker.NewEncodeWorker(
		enc, repo, store, eventsPub, siteLookup, applyClient, cpBaseURL, cfg.Backup.PresignTTL, logger,
	)

	// Font transcode worker (pure-Go, no CGO). Uses the same blobstore and the
	// perf repo for recording results. The perf.Repo is wired here as the
	// FontTranscodeRepo interface.
	perfRepo := perf.NewRepo(pool)
	fontTranscodeWorker := mediafont.NewTranscodeWorker(perfRepo, store, cfg.Backup.PresignTTL, logger)

	// Site screenshot capture worker. Headless Chromium is available in this binary
	// only (cmd/media-encoder ships with /usr/bin/chromium-browser); the main API
	// binary (distroless/static, CGO_ENABLED=0) only client.Inserts screenshot.CaptureArgs.
	//
	// SSRF protection: every Chromium TCP connection is routed through the in-process
	// ssrfproxy.New() which calls ssrf.Safe at dial time (RFC1918 + link-local blocked).
	screenshotRepo := screenshot.NewRepo(pool)
	screenshotCaptureWorker := capture.NewWorker(
		screenshotRepo,
		store,
		eventsPub,
		0, // defaultConcurrency (2 concurrent Chromium captures)
		logger,
	)
	// Weekly fanout: lists all connected sites cross-tenant and enqueues captures.
	// The fanout enqueuer is wired after River starts (post-client start) via
	// screenshotFanoutWorker.SetEnqueuer; a nil enqueuer logs and skips enqueue.
	screenshotSiteLister := capture.NewDBSiteIDLister(pool)
	screenshotFanoutWorker := capture.NewWeeklyFanoutWorker(
		screenshotSiteLister,
		nil, // wired below after River starts
		0,   // fanoutCap default (500)
		logger,
	)

	riverWorkers := river.NewWorkers()
	river.AddWorker(riverWorkers, encodeWorker)
	river.AddWorker(riverWorkers, fontTranscodeWorker)
	river.AddWorker(riverWorkers, screenshotCaptureWorker)
	river.AddWorker(riverWorkers, screenshotFanoutWorker)

	// encodeJobTimeout must match EncodeWorker.Timeout(). SoftStopTimeout gives
	// in-flight jobs this long to finish after a SIGTERM before their contexts are
	// hard-cancelled. Without it the work context inherits from the start context
	// and is cancelled immediately on signal, which cuts Work() mid-variant-loop
	// (root cause of the "stuck at 25%" bug: 1 variant recorded, others lost).
	const encodeJobTimeout = 5 * time.Minute
	const fontTranscodeJobTimeout = 3 * time.Minute

	// screenshotFanoutInterval runs the weekly screenshot sweep. 7 days (168h) is
	// the default; the fanout itself fans out to per-site capture jobs.
	const screenshotFanoutInterval = 7 * 24 * time.Hour

	client, err := river.NewClient(riverpgxv5.New(pool.Pool), &river.Config{
		Logger: logger,
		Schema: mediaSchema,
		Queues: map[string]river.QueueConfig{
			model.MediaEncodeQueue:       {MaxWorkers: workers},
			mediafont.FontTranscodeQueue: {MaxWorkers: workers * 2}, // pure-Go, more concurrency is fine
			// Screenshot capture queue: Chromium captures are memory-heavy (~150–300 MiB each).
			// MaxWorkers is bounded by the capture worker's own semaphore; we match that here.
			screenshot.ScreenshotQueue: {MaxWorkers: 2},
		},
		Workers: riverWorkers,
		PeriodicJobs: []*river.PeriodicJob{
			// Weekly screenshot fanout: lists all connected sites and enqueues captures.
			// RunOnStart: false — avoids a burst on every encoder restart/scale-event.
			river.NewPeriodicJob(
				river.PeriodicInterval(screenshotFanoutInterval),
				func() (river.JobArgs, *river.InsertOpts) { return capture.WeeklyFanoutArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: false},
			),
		},
		// SoftStopTimeout must be >= the longest in-flight job. encodeJobTimeout
		// (5 min) > fontTranscodeJobTimeout (3 min) and captureTimeout (30s), so it covers all.
		SoftStopTimeout: encodeJobTimeout,
	})
	_ = fontTranscodeJobTimeout // documented above
	if err != nil {
		return err
	}
	if err := client.Start(ctx); err != nil {
		return err
	}

	// Wire the screenshot fanout enqueuer now that the River client is started.
	// The fanout worker's Job table INSERT requires an active client.
	screenshotFanoutWorker.SetEnqueuer(screenshot.NewEnqueuer(client))

	logger.Info("media-encoder started",
		slog.Int("encode_workers", workers),
		slog.String("queue", model.MediaEncodeQueue),
		slog.String("s3_bucket", cfg.S3.Bucket))

	// Cloud Run (a Service) requires the container to listen on $PORT or the
	// startup probe fails the revision. The health server also hosts the
	// /internal/drain wake endpoint: at min-instances=0 the CP holds a request
	// open there to keep this cold-started instance alive until the media_encode
	// queue drains. Self-hosters running this via the base docker-compose stack
	// run it always-on and never call /internal/drain.
	// Pass encoder-owned queues to the drain handler. The encoder must stay warm
	// while any queue it processes has pending work.
	//
	// Bind $PORT here, BEFORE the reconcile sweep below, so the startup probe
	// passes immediately regardless of how large the public backlog is.
	healthSrv := startHealthServer(logger, pool, mediaSchema, model.MediaEncodeQueue, screenshot.ScreenshotQueue, mediafont.FontTranscodeQueue)

	// GH #205 follow-up: move any encoder-owned jobs stranded in the public
	// schema (from before this schema was dedicated) onto this client. The
	// hard-fail above guarantees mediaSchema is non-default here, so this
	// always runs once the client is up; it is a no-op once public.river_job
	// holds no matching rows. Best-effort, batched, and only after $PORT is
	// bound — so it never delays startup.
	reconcileStrandedPublicJobs(ctx, pool, client, logger)

	<-ctx.Done()
	logger.Info("shutdown signal received, draining encode queue")

	// The stop context must outlive SoftStopTimeout (encodeJobTimeout = 5 min) so
	// River has the full window to let in-flight jobs finish before the context
	// cancels and Stop() returns early. cfg.Shutdown.Timeout (default 15s) is
	// sized for the main API; the encoder needs the full job timeout plus a margin
	// for River's own teardown. We add 30s of margin for River bookkeeping.
	stopTimeout := encodeJobTimeout + 30*time.Second
	stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	if herr := healthSrv.Shutdown(stopCtx); herr != nil {
		logger.Warn("health server shutdown", slog.Any("error", herr))
	}
	if err := client.Stop(stopCtx); err != nil {
		logger.Warn("river stop", slog.Any("error", err))
	}
	return nil
}

// drain hold tuning. The CP holds a POST /internal/drain request open to keep
// this scale-to-zero instance alive while it works; the handler returns once the
// media_encode queue has been continuously empty for drainQuietPeriod (so a job
// enqueued moments after the last one still keeps the instance up), or after
// drainMaxHold as a hard ceiling (kept under the Cloud Run request timeout).
const (
	drainPollInterval = 2 * time.Second
	drainQuietPeriod  = 20 * time.Second
	drainMaxHold      = 50 * time.Minute
	// maxConcurrentDrains caps simultaneous /internal/drain holds. The CP is
	// singleflighted to a single hold, so this never affects the legitimate path;
	// it is defense-in-depth that bounds the blast radius (pinned goroutines +
	// COUNT load) should the encoder ever be misconfigured allow-unauthenticated
	// or ingress=all. Excess holds get 429 instead of pinning instances.
	maxConcurrentDrains = 3
)

// drainConfig parameterizes the /internal/drain hold so it is unit-testable with
// a fake count func and short durations.
type drainConfig struct {
	poll    time.Duration
	quiet   time.Duration
	maxHold time.Duration
	count   func(ctx context.Context) (int, error)
	logger  *slog.Logger
	// sem caps concurrent holds (nil = uncapped, used by the pure-loop tests).
	sem chan struct{}
}

// startHealthServer binds a minimal HTTP server on $PORT (default 8080). It
// serves the Cloud Run startup/liveness probe (200 on every other path) and the
// /internal/drain wake endpoint. Runs in a goroutine so it never blocks the
// queue. pool/queues drive the drain handler's live-job count.
func startHealthServer(logger *slog.Logger, pool *db.Pool, mediaSchema string, queues ...string) *http.Server {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/drain", drainHandler(drainConfig{
		poll:    drainPollInterval,
		quiet:   drainQuietPeriod,
		maxHold: drainMaxHold,
		count:   func(ctx context.Context) (int, error) { return liveEncodeJobs(ctx, pool, mediaSchema, queues...) },
		logger:  logger,
		sem:     make(chan struct{}, maxConcurrentDrains),
	}))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
		// No WriteTimeout: /internal/drain intentionally holds the response open
		// for the duration of the drain. ReadHeaderTimeout still bounds slowloris.
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("health server", slog.Any("error", err))
		}
	}()
	logger.Info("media-encoder health server listening", slog.String("port", port))
	return srv
}

// drainHandler keeps the instance alive while the media_encode queue has live
// work. It is gated by Cloud Run IAM (the service is not allow-unauthenticated)
// and internal ingress, so the container needs no additional auth — only the CP
// (granted run.invoker, presenting an ID token for this service's audience) can
// reach it. The River workers do the actual encoding in the background; this
// handler merely holds the request — and thus the Cloud Run instance — open until
// the queue is drained.
func drainHandler(cfg drainConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Defense-in-depth: cap concurrent holds (non-blocking acquire). The
		// legitimate CP caller is singleflighted to one hold, so this only bites a
		// misconfigured-ingress flood.
		if cfg.sem != nil {
			select {
			case cfg.sem <- struct{}{}:
				defer func() { <-cfg.sem }()
			default:
				http.Error(w, "too many concurrent drains", http.StatusTooManyRequests)
				return
			}
		}
		drained, reason := holdUntilDrained(r.Context(), cfg)
		// If the client (CP) already disconnected the write is a harmless no-op.
		writeDrainResult(w, drained, reason)
	}
}

// holdUntilDrained blocks until the queue has been continuously empty for
// cfg.quiet (returns drained=true), the cfg.maxHold ceiling elapses
// (drained=false, "max-hold"), or ctx is canceled because the client went away
// (drained=false, "client-gone"). A count error is treated as "not known-empty"
// so the hold continues rather than releasing the instance prematurely. Pure
// loop, extracted for unit testing.
func holdUntilDrained(ctx context.Context, cfg drainConfig) (bool, string) {
	deadline := time.Now().Add(cfg.maxHold)
	var quietSince time.Time
	cfg.logger.Info("drain hold started")
	for {
		if ctx.Err() != nil {
			cfg.logger.Info("drain hold ended: client gone")
			return false, "client-gone"
		}
		n, err := cfg.count(ctx)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return false, "client-gone"
			}
			cfg.logger.Warn("drain: live-count failed", slog.Any("error", err))
			quietSince = time.Time{} // an error is not "known empty"
		case n == 0:
			if quietSince.IsZero() {
				quietSince = time.Now()
			}
			if time.Since(quietSince) >= cfg.quiet {
				cfg.logger.Info("drain hold complete: queue drained")
				return true, "empty"
			}
		default:
			quietSince = time.Time{} // live work — reset the quiet timer
		}
		if time.Now().After(deadline) {
			cfg.logger.Info("drain hold ended: max-hold ceiling")
			return false, "max-hold"
		}
		select {
		case <-ctx.Done():
			return false, "client-gone"
		case <-time.After(cfg.poll):
		}
	}
}

// liveEncodeJobs counts jobs in any of the given queues that need an awake encoder:
// available, running, or retryable. Mirrors the CP waker's query so both sides agree.
// Accepts multiple queues so every encoder-owned queue can keep the process alive.
func liveEncodeJobs(ctx context.Context, pool *db.Pool, mediaSchema string, queues ...string) (int, error) {
	q, err := liveEncodeJobsQuery(mediaSchema)
	if err != nil {
		return 0, err
	}
	var n int
	if err := pool.QueryRow(ctx, q, queues).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// liveEncodeJobsQuery builds the schema-aware drain query used by the encoder
// health endpoint. It counts available, running, and retryable jobs across the
// encoder-owned queues so /internal/drain keeps the instance alive until the
// work actually finishes.
func liveEncodeJobsQuery(mediaSchema string) (string, error) {
	table, err := riverutil.QualifiedTable(mediaSchema, "river_job")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`SELECT count(*) FROM %s WHERE queue = ANY($1) AND state IN ('available','running','retryable')`, table), nil
}

// writeDrainResult writes the small JSON drain summary (best-effort).
func writeDrainResult(w http.ResponseWriter, drained bool, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"drained":%t,"reason":%q}`, drained, reason)
}

// encodeWorkerCount returns the configured media_encode concurrency, bounded
// between 1 and twice the CPU count. The default is tuned for multi-vCPU
// encoder instances where throughput comes from parallel workers.
func encodeWorkerCount() int {
	if s := os.Getenv("WPMGR_MEDIA_ENCODE_WORKERS"); s != "" {
		if v := atoiClamp(s, 1, runtime.NumCPU()*2); v > 0 {
			return v
		}
	}
	return defaultEncodeWorkers
}

// atoiClamp parses a non-negative integer and clamps it to [lo, hi]. It
// returns 0 for non-numeric input so callers can fall back to a default.
func atoiClamp(s string, lo, hi int) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// disabledApply refuses to dispatch media_apply when no signing key is set.
type disabledApply struct{}

func (disabledApply) MediaApply(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaApplyRequest) (agentcmd.MediaApplyResponse, error) {
	return agentcmd.MediaApplyResponse{}, errEnv("media_apply disabled: no CP signing key configured")
}

// newLogger builds a JSON logger at the configured level for production encoder
// output.
func newLogger(cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

// envError is a typed configuration error so missing/invalid environment values
// are distinguishable from runtime failures.
type envError string

func (e envError) Error() string { return string(e) }

// errEnv wraps a message in envError so boot can report a clear fatal config
// problem.
func errEnv(msg string) error { return envError(msg) }
