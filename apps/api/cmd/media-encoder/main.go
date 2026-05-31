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
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"

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
	"github.com/mosamlife/wpmgr/apps/api/internal/media/model"
	mediarepo "github.com/mosamlife/wpmgr/apps/api/internal/media/repo"
	mediaworker "github.com/mosamlife/wpmgr/apps/api/internal/media/worker"
	siteevents "github.com/mosamlife/wpmgr/apps/api/internal/site/events"
)

// maxEncodeWorkers bounds the media_encode queue concurrency so a burst of large
// AVIF encodes cannot OOM this instance (ADR-043 §5). The lilliput ImageOps pool
// is sized to match. Override with WPMGR_MEDIA_ENCODE_WORKERS.
const defaultEncodeWorkers = 2

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv("WPMGR_CONFIG_FILE"))
	if err != nil {
		return err
	}
	logger := newLogger(cfg)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	encodeWorker := mediaworker.NewEncodeWorker(
		enc, repo, store, eventsPub, siteLookup, applyClient, cfg.Backup.PresignTTL, logger,
	)

	riverWorkers := river.NewWorkers()
	river.AddWorker(riverWorkers, encodeWorker)

	client, err := river.NewClient(riverpgxv5.New(pool.Pool), &river.Config{
		Logger: logger,
		Queues: map[string]river.QueueConfig{
			model.MediaEncodeQueue: {MaxWorkers: workers},
		},
		Workers: riverWorkers,
	})
	if err != nil {
		return err
	}
	if err := client.Start(ctx); err != nil {
		return err
	}
	logger.Info("media-encoder started",
		slog.Int("encode_workers", workers),
		slog.String("queue", model.MediaEncodeQueue),
		slog.String("s3_bucket", cfg.S3.Bucket))

	<-ctx.Done()
	logger.Info("shutdown signal received, draining encode queue")
	stopCtx, cancel := context.WithTimeout(context.Background(), cfg.Shutdown.Timeout)
	defer cancel()
	if err := client.Stop(stopCtx); err != nil {
		logger.Warn("river stop", slog.Any("error", err))
	}
	return nil
}

func encodeWorkerCount() int {
	if s := os.Getenv("WPMGR_MEDIA_ENCODE_WORKERS"); s != "" {
		if v := atoiClamp(s, 1, runtime.NumCPU()*2); v > 0 {
			return v
		}
	}
	return defaultEncodeWorkers
}

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

func newLogger(cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

type envError string

func (e envError) Error() string { return string(e) }

func errEnv(msg string) error { return envError(msg) }
