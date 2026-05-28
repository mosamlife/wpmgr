// Command wpmgr is the WPMgr control-plane API server: it loads config,
// initializes telemetry, connects to Postgres, applies migrations, wires the
// domains, and serves the Gin HTTP API with graceful shutdown.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/mosamlife/wpmgr/apps/api/internal/agent"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/autologin"
	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/blobstore"
	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
	"github.com/mosamlife/wpmgr/apps/api/internal/server"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/telemetry"
	"github.com/mosamlife/wpmgr/apps/api/internal/tenant"
	"github.com/mosamlife/wpmgr/apps/api/internal/update"
	"github.com/mosamlife/wpmgr/apps/api/internal/uptime"
)

// version is overridden at build time via -ldflags.
var version = "0.0.0-dev"

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

	tp, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName:  cfg.OTel.ServiceName,
		OTLPEndpoint: cfg.OTel.OTLPEndpoint,
	})
	if err != nil {
		return err
	}
	defer func() { _ = tp.Shutdown(context.Background()) }()

	// Refuse to boot with a weak/placeholder session secret.
	if err := cfg.ValidateSessionSecret(); err != nil {
		return err
	}

	// Refuse to boot in production with a known committed dev control-plane
	// signing key (no-op in development).
	if err := cfg.ValidateAgentSigningKey(); err != nil {
		return err
	}

	// Migrations run with the owner/superuser DSN (creates the app role +
	// privileged DDL); the application connects with the unprivileged app DSN.
	// River's own schema is migrated here too, with the same owner DSN.
	migPool, err := db.Connect(ctx, cfg.DB.MigrateDSN())
	if err != nil {
		return err
	}
	if err := migPool.Migrate(ctx); err != nil {
		migPool.Close()
		return err
	}
	if err := migrateRiver(ctx, migPool.Pool); err != nil {
		migPool.Close()
		return err
	}
	migPool.Close()
	logger.Info("migrations applied")

	pool, err := db.Connect(ctx, cfg.DB.DSN())
	if err != nil {
		return err
	}
	defer pool.Close()

	// Hard-fail if the application role bypasses RLS (overridable for dev via
	// WPMGR_ALLOW_RLS_BYPASS_ROLE=true).
	if err := pool.EnforceRLSRole(ctx, logger, cfg.DB.AllowRLSBypassRole); err != nil {
		return err
	}

	validator := domain.NewValidator()
	clock := domain.SystemClock{}

	tenantSvc := tenant.NewService(tenant.NewRepo(pool), validator, clock)
	siteSvc := site.NewService(site.NewRepo(pool), validator, clock)
	auditRec := audit.NewRecorder(pool, clock)

	// A narrow tenant-creation capability handed to the auth domain (bootstrap +
	// OIDC first-login) without coupling it to the tenant package internals.
	newTenant := func(ctx context.Context, name, slug string) (uuid.UUID, error) {
		t, err := tenantSvc.Create(ctx, tenant.CreateInput{Name: name, Slug: slug})
		if err != nil {
			return uuid.Nil, err
		}
		return t.ID, nil
	}

	authRepo := auth.NewRepo(pool)
	authSvc := auth.NewService(authRepo, auditRec, validator)
	apiKeySvc := apikey.NewService(pool)

	oidcProvider, err := auth.NewOIDCProvider(ctx, cfg.OIDC)
	if err != nil {
		// Discovery failure should not silently disable OIDC; surface it.
		return err
	}
	if oidcProvider.Enabled() {
		logger.Info("OIDC relying party enabled", slog.String("issuer", cfg.OIDC.Issuer))
	} else {
		logger.Info("OIDC disabled (no issuer configured); email+password only")
	}

	redisPool := auth.NewRedisPool(cfg.Redis.Addr, cfg.Redis.Password)
	sessions := auth.NewRedisSessionManager(redisPool, cfg.Auth.IdleTimeout, cfg.Auth.AbsoluteExpiry, cfg.IsProduction())

	authn := middleware.NewAuthenticator(sessions, authSvc, apiKeySvc)

	// Agent protocol: the control plane's PUBLIC signing key is handed to agents
	// at enrollment so they can verify CP->agent commands. Validate the keypair
	// up front so misconfiguration fails fast rather than at first enroll.
	cpPublicKey, err := agentSigningPublicKey(cfg.Agent)
	if err != nil {
		return err
	}
	agentAuthn := agent.NewAuthenticator(siteSvc, clock, cfg.Agent.SignatureSkew)
	agentH := agent.NewHandler(siteSvc)

	// M3 bulk updates: the SSRF-hardened HTTP client (ADR-009) for all outbound
	// calls to agent/site URLs, the CP->agent command client (mints the signed
	// EdDSA JWT), the post-update health prober, the in-process SSE hub, and the
	// tenant-scoped update repo. The command signer is built from the CP signing
	// private key; an empty key disables minting (the worker will then fail
	// update commands loudly rather than send unsigned ones).
	ssrfClient := httpclient.New(httpclient.Config{
		Timeout:    cfg.Update.HTTPTimeout,
		MaxRetries: cfg.Update.HTTPRetries,
	})
	var commander update.Commander
	if cfg.Agent.SigningPrivateKey != "" {
		signer, serr := agentcmd.NewSigner(cfg.Agent.SigningPrivateKey)
		if serr != nil {
			return fmt.Errorf("build command signer: %w", serr)
		}
		commander = agentcmd.NewClient(ssrfClient, signer)
	} else {
		logger.Warn("WPMGR_AGENT_SIGNING_PRIVATE_KEY is empty: CP->agent update commands are disabled")
		commander = disabledCommander{}
	}
	prober := agentcmd.NewProbe(ssrfClient)
	updateHub := update.NewHub()
	updateRepo := update.NewRepo(pool)
	sitesLookup := newSiteLookup(siteSvc)
	updateWorker := update.NewWorker(updateRepo, sitesLookup, commander, prober, updateHub, auditRec, logger, cfg.Update.PerTenantParallelism)

	// M4 backups: an S3-compatible blobstore (ADR-010) for presigned chunk
	// upload/download (only ciphertext is ever stored), the backup command client
	// (mints signed `backup`/`restore` JWTs; reuses the SSRF client), and the
	// tenant-scoped backup repo+service. When no bucket is configured the backup
	// feature is disabled cleanly (the endpoints 501 and no workers/periodics
	// run). The CP base URL is where the agent calls back for presign/manifest.
	var backupSvc *backup.Service
	var backupH *backup.Handler
	var backupAgentH *backup.AgentHandler
	var backupWorker *backup.BackupWorker
	var restoreWorker *backup.RestoreWorker
	var gcWorker *backup.GCWorker
	var scheduleWorker *backup.ScheduleWorker
	if cfg.S3.Enabled() {
		store, serr := blobstore.New(blobstore.Config{
			Endpoint:       cfg.S3.Endpoint,
			Region:         cfg.S3.Region,
			Bucket:         cfg.S3.Bucket,
			AccessKey:      cfg.S3.AccessKey,
			SecretKey:      cfg.S3.SecretKey,
			ForcePathStyle: cfg.S3.ForcePathStyle,
		})
		if serr != nil {
			return fmt.Errorf("blobstore init: %w", serr)
		}
		if berr := store.EnsureBucket(ctx); berr != nil {
			return fmt.Errorf("blobstore ensure bucket: %w", berr)
		}
		var backupCmd backup.Commander
		if cfg.Agent.SigningPrivateKey != "" {
			signer, _ := agentcmd.NewSigner(cfg.Agent.SigningPrivateKey)
			backupCmd = agentcmd.NewClient(ssrfClient, signer)
		} else {
			backupCmd = disabledBackupCommander{}
		}
		backupRepo := backup.NewRepo(pool)
		backupSvc = backup.NewService(backupRepo, newBackupSiteLookup(siteSvc), nil, store, clock, backup.Config{
			PresignTTL:         cfg.Backup.PresignTTL,
			RetentionDays:      cfg.Backup.RetentionDays,
			MonthlyArchiveKeep: cfg.Backup.MonthlyArchiveKeep,
		})
		cpBaseURL := os.Getenv("WPMGR_PUBLIC_BASE_URL")
		backupWorker = backup.NewBackupWorker(backupSvc, backupCmd, auditRec, logger, cpBaseURL)
		restoreWorker = backup.NewRestoreWorker(backupSvc, backupCmd, auditRec, logger)
		gcWorker = backup.NewGCWorker(backupSvc, logger)
		scheduleWorker = backup.NewScheduleWorker(backupSvc, logger)
		backupH = backup.NewHandler(backupSvc, auditRec)
		backupAgentH = backup.NewAgentHandler(backupSvc, auditRec)
		logger.Info("backups enabled", slog.String("s3_bucket", cfg.S3.Bucket))
	} else {
		logger.Warn("WPMGR_S3_BUCKET is empty: backup/restore endpoints are disabled")
	}

	// M5 uptime monitoring: the ClickHouse metrics store (ADR-028; disabled
	// cleanly when WPMGR_CLICKHOUSE_ADDR is empty), the SSRF-hardened probe, the
	// alert dispatcher (email via go-mail/ADR-029 + signed webhook over the SSRF
	// client), and the tenant-scoped uptime repo/service/handler. The probe worker
	// runs on a periodic River job; it writes time-series to ClickHouse, refreshes
	// each site's Postgres health_status, and fires downtime/recovery alerts on
	// transition (de-duped).
	metricsStore, err := metrics.New(ctx, metrics.Config{
		Addr:     cfg.ClickHouse.Addr,
		Database: cfg.ClickHouse.Database,
		Username: cfg.ClickHouse.Username,
		Password: cfg.ClickHouse.Password,
	}, logger)
	if err != nil {
		return err
	}
	defer func() { _ = metricsStore.Close() }()

	uptimeRepo := uptime.NewRepo(pool)
	uptimeSiteAdapter := newUptimeSiteAdapter(siteSvc)
	uptimeProber := uptime.NewProber(ssrfClient, cfg.Uptime.ProbeTimeout)
	var mailer uptime.Mailer
	if cfg.SMTP.Enabled() {
		mailer = uptime.NewSMTPMailer(cfg.SMTP, logger)
		logger.Info("uptime alert email enabled", slog.String("smtp_host", cfg.SMTP.Host))
	} else {
		mailer = uptime.NewNoopMailer(logger)
		logger.Warn("WPMGR_SMTP_HOST is empty: uptime alert emails disabled (webhooks still fire)")
	}
	webhookPoster := uptime.NewSSRFWebhookPoster(ssrfClient)
	uptimeDispatcher := uptime.NewDispatcher(mailer, webhookPoster, auditRec, logger)
	uptimeWorker := uptime.NewProbeWorker(uptimeRepo, uptimeProber, metricsStore, uptimeDispatcher, uptimeSiteAdapter, logger, cfg.Uptime.ProbeConcurrency, cfg.Uptime.DownThreshold)
	uptimeSvc := uptime.NewService(uptimeRepo, metricsStore, uptimeSiteAdapter)
	uptimeH := uptime.NewHandler(uptimeSvc, auditRec)

	// River: connection-health worker pool plus the M3 update-task workers and the
	// M4 backup/restore/GC/scheduler workers. The health job marks a site
	// unreachable when its agent heartbeat goes stale (freshness-based). The M5
	// probe job actively probes every enrolled site (~60s). Update tasks run on
	// per-tenant queue shards so one tenant cannot starve another. Started below,
	// stopped on shutdown.
	siteRepo := site.NewRepo(pool)
	healthChecker := site.NewHealthChecker(siteRepo, cfg.Agent.StaleAfter, cfg.Agent.SignatureSkew)
	riverClient, err := startRiver(ctx, pool.Pool, logger, riverDeps{
		healthChecker:        healthChecker,
		healthInterval:       cfg.Agent.HealthInterval,
		updateWorker:         updateWorker,
		perTenantParallelism: cfg.Update.PerTenantParallelism,
		backupWorker:         backupWorker,
		restoreWorker:        restoreWorker,
		gcWorker:             gcWorker,
		scheduleWorker:       scheduleWorker,
		scheduleInterval:     cfg.Backup.ScheduleInterval,
		gcInterval:           cfg.Backup.GCInterval,
		uptimeWorker:         uptimeWorker,
		probeInterval:        cfg.Uptime.ProbeInterval,
	})
	if err != nil {
		return err
	}

	// The enqueuer needs the started River client; the update service needs the
	// enqueuer. Wire them after the client is up.
	updateSvc := update.NewService(updateRepo, sitesLookup, update.NewRiverEnqueuer(riverClient), validator, clock)
	updateH := update.NewHandler(updateSvc, updateHub, auditRec)
	if backupSvc != nil {
		backupSvc.SetEnqueuer(backup.NewRiverEnqueuer(riverClient))
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), cfg.Shutdown.Timeout)
		defer cancel()
		if err := riverClient.Stop(stopCtx); err != nil {
			logger.Warn("river stop", slog.Any("error", err))
		}
	}()

	// Phase 5.5 One-Click Login (ADR-030/031). Mint+consume require the CP
	// signing key (the JWT is Ed25519-signed by the same control-plane keypair
	// used for M3/M4 commands). When the key is empty in dev, the mint endpoint
	// is wired but every mint will return 500 (the signer interface is satisfied
	// by a small refusing shim so the rest of the boot still completes). Redis
	// is the hot-path consume store; when WPMGR_REDIS_ADDR is empty the Redigo
	// pool still constructs but every Set/GETDEL no-ops -> the service falls
	// back to the durable PG single-shot consume on every callback.
	var autologinH *autologin.MintHandler
	var autologinAgentH *autologin.AgentHandler
	{
		var signer autologin.Signer
		if cfg.Agent.SigningPrivateKey != "" {
			s, serr := agentcmd.NewSigner(cfg.Agent.SigningPrivateKey)
			if serr != nil {
				return fmt.Errorf("build autologin signer: %w", serr)
			}
			signer = s
		} else {
			logger.Warn("WPMGR_AGENT_SIGNING_PRIVATE_KEY is empty: autologin mint is disabled (the endpoint will return 500)")
			signer = disabledAutologinSigner{}
		}
		store := autologin.NonceStore(autologin.NewRedigoStore(redisPool))
		if cfg.Redis.Addr == "" {
			store = autologin.NoopStore{}
		}
		limiter := autologin.NewMemoryLimiter()
		// Janitor stops with the process — no explicit Stop() is required because
		// the process lives until shutdown; the goroutine is bounded and idle.
		autologinSvc := autologin.NewService(
			autologin.NewRepo(pool),
			store,
			signer,
			newAutologinSiteAdapter(siteSvc),
			limiter,
			auditRec,
			clock,
			autologin.Config{Require2FAStepUp: cfg.Autologin.Require2FAStepUp},
		)
		autologinH = autologin.NewMintHandler(autologinSvc)
		autologinAgentH = autologin.NewAgentHandler(autologinSvc)
	}

	srv := server.New(server.Deps{
		Config:          cfg,
		Logger:          logger,
		Pool:            pool,
		Sessions:        sessions,
		Auth:            authn,
		AuthH:           auth.NewHandler(authSvc, sessions, oidcProvider, newTenant),
		MembersH:        auth.NewMembersHandler(authSvc),
		APIKeyH:         apikey.NewHandler(apiKeySvc, auditRec),
		AuditH:          audit.NewHandler(auditRec),
		TenantH:         tenant.NewHandler(tenantSvc, auditRec),
		SiteH:           site.NewHandler(siteSvc, auditRec, cpPublicKey),
		UpdateH:         updateH,
		BackupH:         backupH,
		BackupAgentH:    backupAgentH,
		UptimeH:         uptimeH,
		AutologinH:      autologinH,
		AutologinAgentH: autologinAgentH,
		AgentAuth:       agentAuthn,
		AgentH:          agentH,
		ServiceName:     cfg.OTel.ServiceName,
		Version:         version,
	})

	return srv.Run(ctx)
}

// disabledAutologinSigner refuses to mint when no CP signing key is
// configured, mirroring disabledCommander for M3/M4.
type disabledAutologinSigner struct{}

func (disabledAutologinSigner) MintAutologin(_ time.Time, _, _ string) (string, string, error) {
	return "", "", fmt.Errorf("autologin is disabled: no CP signing key configured")
}

// agentSigningPublicKey validates the control-plane Ed25519 signing keypair from
// config and returns the base64 public half handed to agents at enrollment. An
// empty keypair is permitted in dev (returns ""), but a malformed one fails.
func agentSigningPublicKey(cfg config.AgentConfig) (string, error) {
	if cfg.SigningPublicKey == "" && cfg.SigningPrivateKey == "" {
		return "", nil
	}
	if _, err := agent.DecodePublicKey(cfg.SigningPublicKey); err != nil {
		return "", fmt.Errorf("invalid WPMGR_AGENT_SIGNING_PUBLIC_KEY: %w", err)
	}
	return cfg.SigningPublicKey, nil
}

// migrateRiver applies River's own schema using the migration-owner pool.
func migrateRiver(ctx context.Context, pool *pgxpool.Pool) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("river migrate: %w", err)
	}
	return nil
}

// riverDeps bundles everything startRiver needs: the M2 health checker, the M3
// update worker, and the M4 backup/restore/GC/scheduler workers (any of which
// may be nil when the corresponding feature is disabled).
type riverDeps struct {
	healthChecker        *site.HealthChecker
	healthInterval       time.Duration
	updateWorker         *update.Worker
	perTenantParallelism int
	backupWorker         *backup.BackupWorker
	restoreWorker        *backup.RestoreWorker
	gcWorker             *backup.GCWorker
	scheduleWorker       *backup.ScheduleWorker
	scheduleInterval     time.Duration
	gcInterval           time.Duration
	uptimeWorker         *uptime.ProbeWorker
	probeInterval        time.Duration
}

// startRiver builds and starts the River client with the health-check worker, a
// periodic health job, the M3 update-task worker on per-tenant queue shards, and
// (when backups are enabled) the M4 backup/restore/GC/scheduler workers plus the
// periodic scheduler and retention-GC jobs. The client uses the application pool
// (RLS-bound); cross-tenant jobs (health/scheduler/GC enumeration) run under the
// app.agent GUC, while backup/restore/update work runs tenant-scoped (the worker
// sets app.tenant_id per job via the repo). perTenantParallelism caps each
// update tenant shard's concurrent workers so one tenant cannot starve others.
func startRiver(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, d riverDeps) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, site.NewHealthCheckWorker(d.healthChecker))
	river.AddWorker(workers, d.updateWorker)

	interval := d.healthInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	perTenantParallelism := d.perTenantParallelism
	if perTenantParallelism <= 0 {
		perTenantParallelism = 5
	}

	queues := map[string]river.QueueConfig{
		river.QueueDefault: {MaxWorkers: 5},
	}
	// One bounded queue per tenant shard: MaxWorkers caps a single tenant's
	// concurrency to the per-tenant parallelism limit.
	for _, q := range update.QueueNames() {
		queues[q] = river.QueueConfig{MaxWorkers: perTenantParallelism}
	}

	periodics := []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(interval),
			func() (river.JobArgs, *river.InsertOpts) {
				return site.HealthCheckArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: true},
		),
	}

	backupsEnabled := d.backupWorker != nil
	if backupsEnabled {
		river.AddWorker(workers, d.backupWorker)
		river.AddWorker(workers, d.restoreWorker)
		river.AddWorker(workers, d.gcWorker)
		river.AddWorker(workers, d.scheduleWorker)

		schedInterval := d.scheduleInterval
		if schedInterval <= 0 {
			schedInterval = 5 * time.Minute
		}
		gcInterval := d.gcInterval
		if gcInterval <= 0 {
			gcInterval = time.Hour
		}
		periodics = append(periodics,
			river.NewPeriodicJob(
				river.PeriodicInterval(schedInterval),
				func() (river.JobArgs, *river.InsertOpts) { return backup.ScheduleArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true},
			),
			river.NewPeriodicJob(
				river.PeriodicInterval(gcInterval),
				func() (river.JobArgs, *river.InsertOpts) { return backup.GCArgs{}, nil },
				nil,
			),
		)
	}

	// M5 uptime probe: a periodic job (~60s) that probes every enrolled site,
	// records the time-series, refreshes health_status, and evaluates alerts.
	uptimeEnabled := d.uptimeWorker != nil
	if uptimeEnabled {
		river.AddWorker(workers, d.uptimeWorker)
		probeInterval := d.probeInterval
		if probeInterval <= 0 {
			probeInterval = time.Minute
		}
		periodics = append(periodics,
			river.NewPeriodicJob(
				river.PeriodicInterval(probeInterval),
				func() (river.JobArgs, *river.InsertOpts) { return uptime.ProbeArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true},
			),
		)
	}

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Logger:       logger,
		Queues:       queues,
		Workers:      workers,
		PeriodicJobs: periodics,
	})
	if err != nil {
		return nil, fmt.Errorf("river client: %w", err)
	}
	if err := client.Start(ctx); err != nil {
		return nil, fmt.Errorf("river start: %w", err)
	}
	logger.Info("river worker pool started",
		slog.Duration("health_interval", interval),
		slog.Int("update_per_tenant_parallelism", perTenantParallelism),
		slog.Bool("backups_enabled", backupsEnabled))
	return client, nil
}

// disabledBackupCommander refuses to send backup/restore commands when no CP
// signing key is configured (rather than sending unsigned ones).
type disabledBackupCommander struct{}

func (disabledBackupCommander) Backup(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.BackupRequest) (agentcmd.BackupResponse, error) {
	return agentcmd.BackupResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledBackupCommander) Restore(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.RestoreRequest) (agentcmd.RestoreResponse, error) {
	return agentcmd.RestoreResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

// disabledCommander is the no-op Commander used when no CP signing key is
// configured: it refuses to send commands rather than sending unsigned ones.
type disabledCommander struct{}

func (disabledCommander) Update(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.UpdateRequest) (agentcmd.UpdateResponse, error) {
	return agentcmd.UpdateResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) Rollback(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.RollbackRequest) (agentcmd.RollbackResponse, error) {
	return agentcmd.RollbackResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func newLogger(cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	_ = level.UnmarshalText([]byte(cfg.LogLevel))

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.IsProduction() {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}
