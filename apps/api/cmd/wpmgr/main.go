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

	"github.com/mosamlife/wpmgr/apps/api/internal/activity"
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
	"github.com/mosamlife/wpmgr/apps/api/internal/diagnostics"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
	"github.com/mosamlife/wpmgr/apps/api/internal/invitation"
	"github.com/mosamlife/wpmgr/apps/api/internal/loginbrand"
	"github.com/mosamlife/wpmgr/apps/api/internal/media"
	mediahandler "github.com/mosamlife/wpmgr/apps/api/internal/media/handler"
	mediamodel "github.com/mosamlife/wpmgr/apps/api/internal/media/model"
	mediarepo "github.com/mosamlife/wpmgr/apps/api/internal/media/repo"
	mediaservice "github.com/mosamlife/wpmgr/apps/api/internal/media/service"
	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
	"github.com/mosamlife/wpmgr/apps/api/internal/org"
	"github.com/mosamlife/wpmgr/apps/api/internal/scan"
	"github.com/mosamlife/wpmgr/apps/api/internal/security"
	"github.com/mosamlife/wpmgr/apps/api/internal/server"
	"github.com/mosamlife/wpmgr/apps/api/internal/sharing"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	siteevents "github.com/mosamlife/wpmgr/apps/api/internal/site/events"
	"github.com/mosamlife/wpmgr/apps/api/internal/sitedestination"
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

	authn := middleware.NewAuthenticator(sessions, authSvc, apiKeySvc, pool)

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
	// Updates feature (Track B): the refresh-inventory worker dispatches signed
	// CP->agent commands to re-pull a site's inventory. It satisfies River's
	// JobArgs interface so the per-tenant queue shard bounds its concurrency
	// alongside the update tasks. A nil commander cleanly cancels the job (no
	// unsigned commands ever sent).
	var refreshCmd update.RefreshCommander
	if rc, ok := commander.(update.RefreshCommander); ok {
		refreshCmd = rc
	}
	refreshWorker := update.NewRefreshInventoryWorker(refreshCmd, auditRec, logger)
	refreshDebouncer := update.NewRefreshDebouncer(30 * time.Second)

	// M4 backups: an S3-compatible blobstore (ADR-010) for presigned chunk
	// upload/download (only ciphertext is ever stored), the backup command client
	// (mints signed `backup`/`restore` JWTs; reuses the SSRF client), and the
	// tenant-scoped backup repo+service. When no bucket is configured the backup
	// feature is disabled cleanly (the endpoints 501 and no workers/periodics
	// run). The CP base URL is where the agent calls back for presign/manifest.
	var backupSvc *backup.Service
	var backupH *backup.Handler
	var backupAgentH *backup.AgentHandler
	var restoreRunH *backup.RestoreRunHandler
	var scheduleRunH *backup.ScheduleRunHandler
	var backupWorker *backup.BackupWorker
	var restoreWorker *backup.RestoreWorker
	var gcWorker *backup.GCWorker
	var scheduleWorker *backup.ScheduleWorker
	var progressWatchdog *backup.ProgressWatchdogWorker
	// M6 / Track 4: SQL inspection legacy worker. V1 has no plaintext source
	// or CP-side cache writer wired yet (the agent ships its own inspection
	// artifact in the manifest; the CP-legacy parser is a future fallback for
	// snapshots that pre-date that). The worker is still added to the River
	// pool + queue so any spurious enqueue surfaces a clear River failure
	// metric ("plaintext source or cache unwired") rather than a stuck job.
	var sqlInspectLegacyWorker *backup.SqlInspectLegacyWorker
	// M6 / Track 4: inspection-handler deps, populated below alongside the
	// backup feature gate. The handler.RegisterInspection mount in server.go
	// uses these to fetch agent-supplied sql-inspection artifacts from the
	// chunk store on demand. PlaintextSource + CacheWriter (the legacy-parser
	// tier) stay nil in V1 — those wires light up in a future track.
	var inspectionDeps backup.InspectionDeps
	// M5.6 backup-progress SSE hub: in-process pub/sub keyed by snapshot ID.
	// Service.Publish fans transitions out; Handler.events subscribes per stream.
	backupHub := backup.NewHub()
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
			// Backup/restore commands run synchronously on the agent today (the
			// PHP backup walks the site, chunk-encrypts, and PUTs to S3 inline
			// before responding). On real sites that easily exceeds the snappy
			// 30s update timeout — so we build a SEPARATE SSRF-hardened client
			// with a much longer per-attempt cap just for the backup commander.
			// MaxRetries is 0: the agent's JWT jti is single-use (DoOnce already
			// enforces no auto-retry), and the River job mints a fresh JWT on
			// the next attempt.
			backupSSRFClient := httpclient.New(httpclient.Config{
				Timeout:    cfg.Backup.HTTPTimeout,
				MaxRetries: 0,
			})
			backupCmd = agentcmd.NewClient(backupSSRFClient, signer)
		} else {
			backupCmd = disabledBackupCommander{}
		}
		backupRepo := backup.NewRepo(pool)
		backupSvc = backup.NewService(backupRepo, newBackupSiteLookup(siteSvc), nil, store, clock, backup.Config{
			PresignTTL:         cfg.Backup.PresignTTL,
			RetentionDays:      cfg.Backup.RetentionDays,
			MonthlyArchiveKeep: cfg.Backup.MonthlyArchiveKeep,
		})
		backupSvc.SetHub(backupHub)
		// m16 — Restore Runs + Logs: wire the restore-run repo into the backup
		// service so CreateRestore + RecordProgress persist durable run entities.
		backupSvc.SetRestoreRunStore(backup.NewRestoreRunRepo(pool))
		cpBaseURL := os.Getenv("WPMGR_PUBLIC_BASE_URL")
		// River's default per-job context deadline is 60s — far too short for a
		// real-site backup. Override with the configured backup HTTPTimeout plus
		// a 2-minute buffer so the http.Client's per-attempt timeout (which has
		// a clearer "awaiting headers" diagnostic) fires first when the agent
		// genuinely stalls.
		backupJobTimeout := cfg.Backup.HTTPTimeout + 2*time.Minute
		backupWorker = backup.NewBackupWorker(backupSvc, backupCmd, auditRec, logger, cpBaseURL, backupJobTimeout)
		restoreWorker = backup.NewRestoreWorker(backupSvc, backupCmd, auditRec, logger, cpBaseURL, backupJobTimeout)
		gcWorker = backup.NewGCWorker(backupSvc, logger)
		scheduleWorker = backup.NewScheduleWorker(backupSvc, logger)
		// M5.6 progress watchdog: 120s stall threshold (longest natural silent
		// gap in the phpbu pipeline is age-encrypt for a multi-GB site).
		progressWatchdog = backup.NewProgressWatchdogWorker(backupSvc, 120*time.Second, logger)
		backupH = backup.NewHandler(backupSvc, backupHub, auditRec)
		backupAgentH = backup.NewAgentHandler(backupSvc, auditRec)
		restoreRunH = backup.NewRestoreRunHandler(backupSvc)
		// Wire the auth service as the UserDirectory so restore run DTOs resolve
		// triggered_by UUIDs to human-readable email + name.
		restoreRunH.SetUserDirectory(authSvc)
		// M17 — Schedule run queue (upcoming + past history).
		scheduleRunH = backup.NewScheduleRunHandler(backupSvc)
		scheduleRunH.SetUserDirectory(authSvc)
		// Wire the schedule-run store into the backup service so the scheduler
		// materializes run rows and the reconciliation hooks update them.
		backupSvc.SetScheduleRunStore(backup.NewScheduleRunRepo(pool))
		// M6 / Track 4: agent-supplied inspection artifact fetcher. Streams the
		// ordered chunks of the manifest's `sql-inspection.json` entry from the
		// blobstore and validates the result is JSON. V0 agents ship the report
		// as plaintext chunks (ENCRYPT_CHUNKS=false), so no age decryption is
		// performed here. Cache + Enqueuer stay nil in V1 — legacy snapshots
		// (no inspection entry) return 503 `inspection_unwired` until the
		// CP-side cache backend and the SqlInspectLegacy plaintext source land.
		manifestInspectionFetcher := backup.NewManifestInspectionFetcher(store, backupRepo)
		inspectionDeps = backup.InspectionDeps{
			ManifestFetch: manifestInspectionFetcher,
			Logger:        logger,
		}
		// ADR-037 Sprint 1, 1D — environment fingerprint. Reuses the SQL-
		// inspection fetcher adapter (it's artifact-agnostic — concatenates
		// chunk ciphertext and probes JSON) for the agent-shipped
		// environment.json manifest entry.
		backupH.SetEnvironmentFetcher(manifestInspectionFetcher)
		// M6 / Track 4: SQL inspection legacy parser worker. V1 wires nil for
		// both InspectionPlaintextSource (no agent-side decrypted-dump endpoint
		// yet) and InspectionCacheWriter (no CP-side cache backend yet). The
		// worker.Work method short-circuits with a stable error in that case so
		// any enqueue surfaces a clear River failure metric rather than silently
		// looping. The handler's GET path remains operational: snapshots whose
		// manifest carries an agent-supplied inspection artifact resolve via
		// the ManifestInspectionFetcher path; legacy snapshots return 503
		// "inspection_unwired" until the source/cache deps are filled in.
		sqlInspectLegacyWorker = backup.NewSqlInspectLegacyWorker(nil, nil, logger)
		logger.Info("backups enabled", slog.String("s3_bucket", cfg.S3.Bucket))
	} else {
		logger.Warn("WPMGR_S3_BUCKET is empty: backup/restore endpoints are disabled")
	}

	// ADR-036 P1: per-site destination service + presign registry. Always wired
	// even when backups are disabled so the destinations CRUD is reachable for
	// configuration ahead of enabling backups. The registry is bound to the
	// backup service via SetRegistry below.
	siteDestRepo := sitedestination.NewRepo(pool)
	siteDestAgeID, err := sitedestination.NewAgeIdentity(os.Getenv("WPMGR_SITE_DEST_AGE_SECRET"))
	if err != nil {
		return fmt.Errorf("site destination age identity: %w", err)
	}
	siteDestSvc := sitedestination.NewService(siteDestRepo, siteDestAgeID, logger)
	siteDestH := sitedestination.NewHandler(siteDestSvc, auditRec)
	if backupSvc != nil {
		registry := blobstore.NewRegistry(nil, siteDestSvc) // defaultStore wired below
		// Bind the legacy CP-global store as the registry's default. Built
		// fresh from cfg.S3 because the original `store` variable is only in
		// scope inside the `if cfg.S3.Enabled()` block above. When backups
		// are enabled, S3 IS configured, so this rebuild always succeeds.
		defStore, derr := blobstore.New(blobstore.Config{
			Endpoint:       cfg.S3.Endpoint,
			Region:         cfg.S3.Region,
			Bucket:         cfg.S3.Bucket,
			AccessKey:      cfg.S3.AccessKey,
			SecretKey:      cfg.S3.SecretKey,
			ForcePathStyle: cfg.S3.ForcePathStyle,
		})
		if derr != nil {
			return fmt.Errorf("registry default store: %w", derr)
		}
		registry = blobstore.NewRegistry(defStore, siteDestSvc)
		backupSvc.SetRegistry(&registryAdapter{r: registry})
		// M5.7 P4: wire the manifest index writer so SubmitManifest writes
		// tenant/<tenantID>/site/<siteID>/backup/<snapshotID>/manifest.json
		// via the same CP-global store used for presigning. Best-effort:
		// failures are logged and never fail the backup. Uses defStore (the
		// rebuilt CP-global *blobstore.Store) which satisfies IndexPutter.
		backupSvc.SetIndexPutter(defStore)
	}

	// M5/M6 uptime monitoring: the uptime metrics store, the SSRF-hardened
	// probe, the alert dispatcher (email via go-mail/ADR-029 + signed webhook
	// over the SSRF client), and the tenant-scoped uptime repo/service/handler.
	// The probe worker runs on a periodic River job; it writes time-series to
	// the metrics store, refreshes each site's Postgres health_status, and
	// fires downtime/recovery alerts on transition (de-duped).
	//
	// Backend selection (M6, GCP cutover): when WPMGR_CLICKHOUSE_ADDR is set we
	// use the original ClickHouse store (ADR-028). When it is empty we fall
	// back to the Postgres-backed store added in the M6 migration. Postgres is
	// the M6 default because the GCP managed deployment does not run a
	// ClickHouse cluster — before this fix the empty addr produced a disabled
	// store whose writes/queries no-op'd, so the dashboard had no status, no
	// graph, and no cert data.
	var metricsStore metrics.Store
	if cfg.ClickHouse.Enabled() {
		s, err := metrics.New(ctx, metrics.Config{
			Addr:     cfg.ClickHouse.Addr,
			Database: cfg.ClickHouse.Database,
			Username: cfg.ClickHouse.Username,
			Password: cfg.ClickHouse.Password,
		}, logger)
		if err != nil {
			return err
		}
		metricsStore = s
	} else {
		metricsStore = metrics.NewPostgres(pool, logger)
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

	// ADR-037 Sprint 2 — diagnostics + php-error monitor repo. Built here
	// (before River) so the phpErrorsGCWorker can be registered at River start.
	// The service, handler, and enqueuer wiring continues after River starts.
	diagnosticsRepo := diagnostics.NewRepo(pool)

	// River: connection-health worker pool plus the M3 update-task workers and the
	// M4 backup/restore/GC/scheduler workers. The health job marks a site
	// unreachable when its agent heartbeat goes stale (freshness-based). The M5
	// probe job actively probes every enrolled site (~60s). Update tasks run on
	// per-tenant queue shards so one tenant cannot starve another. Started below,
	// stopped on shutdown.
	siteRepo := site.NewRepo(pool)
	healthChecker := site.NewHealthChecker(siteRepo, cfg.Agent.StaleAfter, cfg.Agent.SignatureSkew)

	// M21 — Live enrollment + connection lifecycle (ADR-038/039/040/041).
	// Event bus: tenant-keyed SSE Hub + durable site_events journal + LISTEN
	// fan-out. The connection service is the single owner of every state
	// transition; the sweeper is the only caller of the degraded/disconnected
	// transitions. The Listener goroutine is started below (after the pool is up).
	siteEventsHub := siteevents.NewHub()
	siteEventsPub := siteevents.NewPublisher(pool, clock)
	// Revoke-token minter (Phase 6 finding B): reuse the agentcmd Ed25519 signer
	// to sign the "revoke" instruction. Keep it a true nil interface when the CP
	// has no signing key, so connService falls back to an unsigned instruction
	// rather than calling Mint on a typed-nil *Signer.
	var revokeMinter site.RevokeTokenMinter
	if cmdSigner, serr := agentcmd.NewSigner(cfg.Agent.SigningPrivateKey); serr == nil {
		revokeMinter = cmdSigner
	}
	connSvc := site.NewConnectionService(siteRepo, validator, auditRec, siteEventsPub, clock, revokeMinter)
	// Inject the lifecycle service into the enroll branch (site-bound consume)
	// and the agent heartbeat/disconnect handler.
	siteSvc.SetConnectionService(connSvc)
	agentH.SetLifecycleSink(site.NewAgentLifecycleAdapter(connSvc))
	// Timeout sweeper (every 15s) + site_events prune (every minute).
	siteSweeper := site.NewSweeper(siteRepo, connSvc.(site.SweeperTransitioner), siteEventsPub)
	siteSweepWorker := site.NewSweepWorker(siteSweeper)
	siteEventPruneWorker := site.NewEventPruneWorker(siteSweeper)
	// SSE endpoint + the dedicated LISTEN listener.
	siteEventsH := siteevents.NewHandler(pool, siteEventsHub)
	siteEventsListener := siteevents.NewListener(pool, siteEventsHub, logger)
	go siteEventsListener.Run(ctx)

	// S1.1 (D) — PHP-error retention GC. Always wired (the table always exists);
	// runs once per hour sweeping rows older than 30 days.
	phpErrorsGCWorker := diagnostics.NewErrorsGCWorker(diagnosticsRepo, 30*24*time.Hour, logger)

	// S3 — Malware / File-Integrity Scan. Workers are built here (before River)
	// with a nil enqueuer; the enqueuer is wired post-River-start via SetEnqueuer
	// so the worker can re-enqueue partial iterations using the started River client.
	scanRepo := scan.NewRepo(pool)
	scanSvc := scan.NewService(scanRepo, auditRec)
	scanH := scan.NewHandler(scanSvc)
	scanChecksums := scan.NewChecksumProvider(scanRepo, ssrfClient)
	scanSiteAdapter := newScanSiteAdapter(siteSvc)
	var scanWorker *scan.ScanRunWorker
	var scanHashGCWorker *scan.HashGCWorker
	if scanCmd, ok := commander.(scan.AgentScanClient); ok {
		scanWorker = scan.NewScanRunWorker(scanRepo, scanChecksums, scanCmd, scanSiteAdapter, nil, auditRec, logger)
		scanHashGCWorker = scan.NewHashGCWorker(scanRepo, 24*time.Hour, logger)
		scanSvc.SetAgentClient(scanCmd, scanSiteAdapter)
		logger.Info("scan agent client wired")
	} else {
		logger.Warn("scan agent client not wired: CP->agent commander unavailable (signing key empty?)")
	}

	// M23 — Media Optimizer (ADR-043). The service + handlers are built here
	// (before River) so the dashboard GETs work as soon as the agent syncs; the
	// EncodeArgs enqueuer is wired post-River-start (the media_encode queue is
	// registered with MaxWorkers=0 in the API — the separate media-encoder
	// process runs the actual encoders). NO encoder import reaches this binary:
	// the API only client.Inserts model.EncodeArgs (a pure-Go River job type).
	mediaRepo := mediarepo.NewRepo(pool)
	var mediaStore *blobstore.Store
	if cfg.S3.Enabled() {
		ms, merr := blobstore.New(blobstore.Config{
			Endpoint:       cfg.S3.Endpoint,
			Region:         cfg.S3.Region,
			Bucket:         cfg.S3.Bucket,
			AccessKey:      cfg.S3.AccessKey,
			SecretKey:      cfg.S3.SecretKey,
			ForcePathStyle: cfg.S3.ForcePathStyle,
		})
		if merr != nil {
			return fmt.Errorf("media blobstore init: %w", merr)
		}
		mediaStore = ms
	}
	mediaCPBaseURL := os.Getenv("WPMGR_PUBLIC_BASE_URL")
	mediaSvc := mediaservice.NewService(mediaRepo, mediaStore, siteEventsPub, auditRec, clock, mediaservice.Config{
		PresignTTL:    cfg.Backup.PresignTTL,
		CPBaseURL:     mediaCPBaseURL,
		RatePerSite:   200,
		RatePerTenant: 1000,
		RateWindow:    time.Minute,
	}, logger)
	mediaSiteAdapterImpl := newMediaSiteAdapter(siteSvc)
	if mediaCmd, ok := commander.(mediaservice.AgentMediaClient); ok {
		mediaSvc.SetAgentClient(mediaCmd, mediaSiteAdapterImpl)
		logger.Info("media optimizer agent client wired")
	} else {
		logger.Warn("media optimizer agent client not wired: CP->agent commander unavailable (signing key empty?)")
	}
	mediaH := mediahandler.NewHandler(mediaSvc)
	mediaAgentH := mediahandler.NewAgentHandler(mediaSvc)

	riverClient, err := startRiver(ctx, pool.Pool, logger, riverDeps{
		healthChecker:          healthChecker,
		healthInterval:         cfg.Agent.HealthInterval,
		siteSweepWorker:        siteSweepWorker,
		siteEventPruneWorker:   siteEventPruneWorker,
		updateWorker:           updateWorker,
		refreshWorker:          refreshWorker,
		perTenantParallelism:   cfg.Update.PerTenantParallelism,
		backupWorker:           backupWorker,
		restoreWorker:          restoreWorker,
		gcWorker:               gcWorker,
		scheduleWorker:         scheduleWorker,
		progressWatchdog:       progressWatchdog,
		sqlInspectLegacyWorker: sqlInspectLegacyWorker,
		scheduleInterval:       cfg.Backup.ScheduleInterval,
		gcInterval:             cfg.Backup.GCInterval,
		uptimeWorker:           uptimeWorker,
		probeInterval:          cfg.Uptime.ProbeInterval,
		phpErrorsGCWorker:      phpErrorsGCWorker,
		// S3 scan workers (nil when signing key is not configured).
		scanRunWorker:    scanWorker,
		scanHashGCWorker: scanHashGCWorker,
	})
	if err != nil {
		return err
	}

	// The enqueuer needs the started River client; the update service needs the
	// enqueuer. Wire them after the client is up. The same enqueuer also serves
	// the post-update inventory-refresh path (via the update Worker) and the
	// operator-facing refresh route on the site handler (via siteRefreshAdapter).
	updateEnqueuer := update.NewRiverEnqueuer(riverClient)
	updateSvc := update.NewService(updateRepo, sitesLookup, updateEnqueuer, validator, clock)
	updateH := update.NewHandler(updateSvc, updateHub, auditRec)
	updateWorker.SetRefreshEnqueuer(updateEnqueuer, refreshDebouncer)
	siteH := site.NewHandler(siteSvc, auditRec, cpPublicKey)
	siteH.SetRefreshEnqueuer(newSiteRefreshAdapter(updateEnqueuer), cfg.Agent.StaleAfter)
	// M21: enable the site-first create + revoke/archive/restore/re-enroll routes.
	siteH.SetConnectionService(connSvc)
	if backupSvc != nil {
		backupSvc.SetEnqueuer(backup.NewRiverEnqueuer(riverClient))
	}

	// S3 scan: wire the River enqueuer into the service + worker now that River
	// has started. The scan service needs it for StartRun; the worker needs it
	// to re-enqueue partial iterations.
	scanEnqueuer := scan.NewRiverEnqueuer(riverClient)
	scanSvc.SetEnqueuer(scanEnqueuer)
	if scanWorker != nil {
		scanWorker.SetEnqueuer(scanEnqueuer)
	}

	// M23 Media Optimizer: wire the EncodeArgs enqueuer now that River has
	// started. The enqueuer lives in the PURE media package (no encoder import),
	// so this binary still has no CGO dependency.
	mediaSvc.SetEnqueuer(media.NewRiverEnqueuer(riverClient))

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

	// ADR-037 Sprint 2 — diagnostics + php-error monitor wiring. The service
	// is always built (the operator GET endpoints work as soon as the agent
	// ships its first payload).
	//
	// v0.9.13 (CP-side fix B): wire the on-demand RefreshEnqueuer when the
	// commander supports the `diagnostics` agentcmd verb (every real-mode
	// build does; the disabledCommander does NOT, in which case /refresh
	// keeps returning the legacy 503 unwired sentinel). The enqueuer issues
	// one signed POST to the agent's /wp-json/wpmgr/v1/command/diagnostics
	// route, reads the agent's synchronous 14-category response body, and
	// feeds it into the same IngestDiagnostics splitter the daily cron-push
	// path uses — so the operator's "Re-run check" click renders fresh data
	// on the next GET /diagnostics.
	// diagnosticsRepo is already built above (before River start) so we reuse it.
	diagnosticsSvc := diagnostics.NewService(diagnosticsRepo)
	diagnosticsH := diagnostics.NewHandler(diagnosticsSvc, auditRec)
	diagnosticsAgentH := agent.NewDiagnosticsHandler(diagnosticsSvc)
	errorsAgentH := agent.NewErrorsHandler(diagnosticsSvc)
	diagSiteAdapter := newDiagnosticsSiteAdapter(siteSvc)
	if diagCmd, ok := commander.(diagnostics.AgentDiagnosticsClient); ok {
		diagEnq := diagnostics.NewRefreshEnqueuer(
			diagCmd,
			diagSiteAdapter,
			diagnosticsSvc,
		)
		if diagEnq != nil {
			diagnosticsSvc.SetRefreshEnqueuer(diagEnq)
			logger.Info("diagnostics refresh enqueuer wired")
		}
	} else {
		logger.Warn("diagnostics refresh enqueuer not wired: CP->agent commander unavailable (signing key empty?)")
	}
	// S1.2 — error config push: wire the agentcmd client when the commander
	// supports the sync_error_config verb. The same SSRF client and signer
	// used for the diagnostics refresh are reused here (the update commander
	// is the full agentcmd.Client in real-mode builds; it now also satisfies
	// AgentErrorConfigClient via SyncErrorConfig).
	if errCfgCmd, ok := commander.(diagnostics.AgentErrorConfigClient); ok {
		diagnosticsSvc.SetErrorConfigClient(errCfgCmd, diagSiteAdapter)
		logger.Info("error config sync client wired")
	} else {
		logger.Warn("error config sync client not wired: CP->agent commander unavailable (signing key empty?)")
	}

	// ADR-037 Sprint 3 — WordPress activity log. The CP re-verifies the agent's
	// hash chain at ingest (tamper-evidence) and routes high-severity events into
	// the EXISTING uptime alert Dispatcher (no parallel notification system). The
	// security alerter loads the tenant's AlertConfig and gates on its
	// notify_security flag before dispatching email + webhook.
	activityRepo := activity.NewRepo(pool)
	activitySecAlerter := newActivitySecurityAlerter(uptimeRepo, uptimeDispatcher, clock, logger)
	activitySvc := activity.NewService(activityRepo, activitySecAlerter, newActivitySiteAdapter(siteSvc))
	activityH := activity.NewHandler(activitySvc)
	activityAgentH := agent.NewActivityHandler(activitySvc)

	// S2 — Login Protection + IP store. The security service stores per-site
	// login-protection config, pushes it to the agent via the signed
	// `sync_security_config` command, ingests login events, and exposes an
	// unblock-IP action. The agent client is wired when the commander supports
	// the security command verbs (every real-mode build does).
	securityRepo := security.NewRepo(pool)
	securitySvc := security.NewService(securityRepo)
	securityH := security.NewHandler(securitySvc, auditRec)
	securityAgentH := agent.NewSecurityLoginEventsHandler(securitySvc)
	secSiteAdapter := newSecuritySiteAdapter(siteSvc)
	if secCmd, ok := commander.(security.AgentSecurityClient); ok {
		securitySvc.SetAgentClient(secCmd, secSiteAdapter)
		logger.Info("security agent client wired")
	} else {
		logger.Warn("security agent client not wired: CP->agent commander unavailable (signing key empty?)")
	}

	// M14 — Login Whitelabel. The loginbrand service stores per-site login brand
	// config (logo URL, logo link, message) and pushes it to the agent via the
	// signed `sync_login_brand` command. The agent client is wired when the
	// commander supports the SyncLoginBrand method (every real-mode build does).
	loginBrandRepo := loginbrand.NewRepo(pool)
	loginBrandSvc := loginbrand.NewService(loginBrandRepo)
	loginBrandH := loginbrand.NewHandler(loginBrandSvc, auditRec)
	loginBrandSiteAdapter := newLoginBrandSiteAdapter(siteSvc)
	if lbCmd, ok := commander.(loginbrand.AgentLoginBrandClient); ok {
		loginBrandSvc.SetAgentClient(lbCmd, loginBrandSiteAdapter)
		logger.Info("login brand agent client wired")
	} else {
		logger.Warn("login brand agent client not wired: CP->agent commander unavailable (signing key empty?)")
	}

	// M5.7 — Orgs + Sharing + Invitations.
	publicBaseURL := os.Getenv("WPMGR_PUBLIC_BASE_URL")

	// Build the sharing mailer (reuse SMTP config; may be nil/noop).
	var sharingMailer sharing.Mailer
	if cfg.SMTP.Enabled() {
		sharingMailer = uptime.NewSMTPMailer(cfg.SMTP, logger)
	}
	sharingSvc := sharing.NewService(pool, authRepo, auditRec, sharingMailer, publicBaseURL)
	sharingH := sharing.NewHandler(sharingSvc)

	// Org handler: create org + activate.
	orgTenantCreator := &orgTenantAdapter{svc: tenantSvc}
	orgH := org.NewHandler(pool, orgTenantCreator, sessions, authSvc, auditRec)

	// Invitation service + handler.
	var invitationMailer invitation.Mailer
	if cfg.SMTP.Enabled() {
		invitationMailer = uptime.NewSMTPMailer(cfg.SMTP, logger)
	}
	invitationSvc := invitation.NewService(pool, authRepo, auditRec, sessions, invitationMailer, publicBaseURL)
	invitationH := invitation.NewHandler(invitationSvc)

	// ADR-042 — CP-driven agent self-update manifest handler. Needs object
	// storage (to read agent-releases/latest.json + presign the package) AND the
	// CP signing key (to sign the manifest). When either is absent the handler
	// stays nil and the /agent/v1/update/manifest route is simply not mounted.
	// The store is built fresh from cfg.S3 because the earlier defStore is scoped
	// inside the backup block.
	var updateAgentH *agent.UpdateHandler
	if cfg.S3.Enabled() && cfg.Agent.SigningPrivateKey != "" {
		manifestStore, merr := blobstore.New(blobstore.Config{
			Endpoint:       cfg.S3.Endpoint,
			Region:         cfg.S3.Region,
			Bucket:         cfg.S3.Bucket,
			AccessKey:      cfg.S3.AccessKey,
			SecretKey:      cfg.S3.SecretKey,
			ForcePathStyle: cfg.S3.ForcePathStyle,
		})
		if merr != nil {
			return fmt.Errorf("update manifest store: %w", merr)
		}
		manifestSigner, serr := agentcmd.NewSigner(cfg.Agent.SigningPrivateKey)
		if serr != nil {
			return fmt.Errorf("update manifest signer: %w", serr)
		}
		// Clamp the package presign + manifest exp window to <=5min (ADR-042 §1).
		manifestTTL := cfg.Backup.PresignTTL
		if manifestTTL <= 0 || manifestTTL > 5*time.Minute {
			manifestTTL = 5 * time.Minute
		}
		updateAgentH = agent.NewUpdateHandler(manifestStore, manifestSigner, manifestTTL)

		// Boot probe: exercise the exact storage ops the manifest handler relies
		// on (read agent-releases/latest.json + mint a presigned GET) so a
		// misconfiguration surfaces in the startup log instead of as an opaque
		// 500 on the agent's first poll. Runs once, off the hot path.
		ms := manifestStore
		go func() {
			pctx := context.Background()
			if rc, gerr := ms.GetViaPresign(pctx, "agent-releases/latest.json"); gerr != nil {
				logger.Error("ADR-042 self-update boot probe: fetch latest.json failed", "err", gerr.Error())
			} else {
				_ = rc.Close()
				logger.Info("ADR-042 self-update boot probe: fetch latest.json OK")
			}
		}()
	} else {
		logger.Warn("ADR-042 self-update disabled: object storage or WPMGR_AGENT_SIGNING_PRIVATE_KEY not configured")
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
		SiteH:           siteH,
		SiteEventsH:     siteEventsH,
		UpdateH:         updateH,
		BackupH:         backupH,
		BackupAgentH:    backupAgentH,
		InspectionDeps:  inspectionDeps,
		UptimeH:         uptimeH,
		AutologinH:      autologinH,
		AutologinAgentH: autologinAgentH,
		AgentAuth:       agentAuthn,
		AgentH:          agentH,
		UpdateAgentH:    updateAgentH,
		SiteDestH:       siteDestH,
		// ADR-037 Sprint 2 wiring.
		DiagnosticsH:      diagnosticsH,
		DiagnosticsAgentH: diagnosticsAgentH,
		ErrorsAgentH:      errorsAgentH,
		// ADR-037 Sprint 3 wiring — activity log + agent ingest.
		ActivityH:      activityH,
		ActivityAgentH: activityAgentH,
		// S2 — Login Protection + IP store.
		SecurityH:      securityH,
		SecurityAgentH: securityAgentH,
		// M14 — Login Whitelabel.
		LoginBrandH: loginBrandH,
		// S3 — Malware / File-Integrity Scan.
		ScanH: scanH,
		// m16 — Restore Runs + Logs.
		RestoreRunH: restoreRunH,
		// M17 — Schedule Run queue.
		ScheduleRunH: scheduleRunH,
		// M5.7 — Orgs + Sharing + Invitations.
		OrgH:        orgH,
		SharingH:    sharingH,
		InvitationH: invitationH,
		// M23 — Media Optimizer.
		MediaH:      mediaH,
		MediaAgentH: mediaAgentH,
		ServiceName: cfg.OTel.ServiceName,
		Version:     version,
	})

	return srv.Run(ctx)
}

// orgTenantAdapter adapts tenant.Service to the org.TenantCreator interface
// (which takes (name, slug) directly instead of tenant.CreateInput).
type orgTenantAdapter struct {
	svc *tenant.Service
}

func (a *orgTenantAdapter) Create(ctx context.Context, name, slug string) (uuid.UUID, error) {
	t, err := a.svc.Create(ctx, tenant.CreateInput{Name: name, Slug: slug})
	if err != nil {
		return uuid.Nil, err
	}
	return t.ID, nil
}

// registryAdapter bridges the blobstore.Registry (which knows about Stores in
// blobstore terms) into the backup.PresignerForSnapshot interface (which works
// in backup terms, so the backup package needs no import cycle on blobstore).
// ADR-036 P1 storage adapter routing.
type registryAdapter struct {
	r *blobstore.Registry
}

func (a *registryAdapter) PresignerForSnapshot(ctx context.Context, snap backup.Snapshot) (backup.Presigner, error) {
	store, err := a.r.StoreForSnapshot(ctx, blobstore.SnapshotLike{
		TenantID:      snap.TenantID,
		SiteID:        snap.SiteID,
		DestinationID: snap.DestinationID,
	})
	if err != nil {
		return nil, err
	}
	if store == nil {
		return nil, nil
	}
	return store, nil
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
	healthChecker          *site.HealthChecker
	healthInterval         time.Duration
	// M21 connection lifecycle: the timeout sweeper (15s) + site_events prune (1m).
	siteSweepWorker      *site.SweepWorker
	siteEventPruneWorker *site.EventPruneWorker
	updateWorker           *update.Worker
	refreshWorker          *update.RefreshInventoryWorker
	perTenantParallelism   int
	backupWorker           *backup.BackupWorker
	restoreWorker          *backup.RestoreWorker
	gcWorker               *backup.GCWorker
	scheduleWorker         *backup.ScheduleWorker
	progressWatchdog       *backup.ProgressWatchdogWorker
	sqlInspectLegacyWorker *backup.SqlInspectLegacyWorker
	scheduleInterval       time.Duration
	gcInterval             time.Duration
	uptimeWorker           *uptime.ProbeWorker
	probeInterval          time.Duration
	// S1.1 (D) — PHP-error retention GC. Always non-nil (wired unconditionally).
	phpErrorsGCWorker *diagnostics.ErrorsGCWorker
	// S3 — Malware / File-Integrity Scan workers (nil when signing key empty).
	scanRunWorker    *scan.ScanRunWorker
	scanHashGCWorker *scan.HashGCWorker
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
	if d.refreshWorker != nil {
		river.AddWorker(workers, d.refreshWorker)
	}

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
		// M23 Media Optimizer (ADR-043): the API registers the media_encode queue
		// with ZERO workers — it only client.Inserts model.EncodeArgs. The actual
		// EncodeWorker (which imports the CGO lilliput encoder) runs ONLY in the
		// separate cmd/media-encoder process. This keeps the API CGO_ENABLED=0.
		mediamodel.MediaEncodeQueue: {MaxWorkers: 0},
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

	// M21 connection-lifecycle timeout sweeper (every 15s, ADR-039) + the
	// site_events ring-buffer prune (every minute, ADR-038). The sweeper is the
	// ONLY caller of the degraded/disconnected transitions.
	if d.siteSweepWorker != nil {
		river.AddWorker(workers, d.siteSweepWorker)
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(15*time.Second),
			func() (river.JobArgs, *river.InsertOpts) { return site.SweepArgs{}, nil },
			&river.PeriodicJobOpts{RunOnStart: true},
		))
	}
	if d.siteEventPruneWorker != nil {
		river.AddWorker(workers, d.siteEventPruneWorker)
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(time.Minute),
			func() (river.JobArgs, *river.InsertOpts) { return site.EventPruneArgs{}, nil },
			nil,
		))
	}

	backupsEnabled := d.backupWorker != nil
	if backupsEnabled {
		river.AddWorker(workers, d.backupWorker)
		river.AddWorker(workers, d.restoreWorker)
		river.AddWorker(workers, d.gcWorker)
		river.AddWorker(workers, d.scheduleWorker)
		// M6 / Track 4: SQL inspection legacy parser. Pinned to its own queue
		// (sql_inspect_legacy) with MaxWorkers=1 per CP instance — a streaming
		// SQL parse is CPU-heavy and the operator-poll cadence is generous, so
		// queue depth >1 doesn't help any one user and would risk OOM on a
		// multi-GB dump if two ran in parallel.
		if d.sqlInspectLegacyWorker != nil {
			river.AddWorker(workers, d.sqlInspectLegacyWorker)
			queues[backup.SqlInspectLegacyQueue] = river.QueueConfig{MaxWorkers: 1}
		}

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
		if d.progressWatchdog != nil {
			river.AddWorker(workers, d.progressWatchdog)
			// 30s tick is half the stall threshold — guarantees we detect a stall
			// within at most threshold+30s. Cheap (a single indexed SELECT).
			periodics = append(periodics, river.NewPeriodicJob(
				river.PeriodicInterval(30*time.Second),
				func() (river.JobArgs, *river.InsertOpts) { return backup.ProgressWatchdogArgs{}, nil },
				nil,
			))
		}
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

	// S1.1 (D) — PHP-error retention GC: always wired, runs once per hour.
	// Deletes agent_php_errors rows with last_seen_at older than 30 days
	// (configured on the worker). Cross-tenant under app.agent GUC.
	if d.phpErrorsGCWorker != nil {
		river.AddWorker(workers, d.phpErrorsGCWorker)
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(time.Hour),
			func() (river.JobArgs, *river.InsertOpts) { return diagnostics.ErrorsGCArgs{}, nil },
			nil,
		))
	}

	// S3 — Malware / File-Integrity Scan. The scan_run worker drives the
	// multi-step hash-streaming loop; the hash GC worker sweeps orphan
	// staging rows every hour.
	if d.scanRunWorker != nil {
		river.AddWorker(workers, d.scanRunWorker)
		queues[scan.ScanRunQueue] = river.QueueConfig{MaxWorkers: 4}
	}
	if d.scanHashGCWorker != nil {
		river.AddWorker(workers, d.scanHashGCWorker)
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(time.Hour),
			func() (river.JobArgs, *river.InsertOpts) { return scan.HashGCArgs{}, nil },
			nil,
		))
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

func (disabledCommander) SyncErrorConfig(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.ErrorConfigRequest) (agentcmd.ErrorConfigResult, error) {
	return agentcmd.ErrorConfigResult{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) SyncSecurityConfig(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.SecurityConfigRequest) (agentcmd.SecurityConfigResult, error) {
	return agentcmd.SecurityConfigResult{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) UnblockIP(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.UnblockIPRequest) (agentcmd.UnblockIPResult, error) {
	return agentcmd.UnblockIPResult{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) SyncLoginBrand(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.LoginBrandRequest) (agentcmd.LoginBrandResult, error) {
	return agentcmd.LoginBrandResult{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) Scan(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.ScanRequest) (agentcmd.ScanResponse, error) {
	return agentcmd.ScanResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) GetFile(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.GetFileRequest) (agentcmd.GetFileResponse, error) {
	return agentcmd.GetFileResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

// Media Optimizer (ADR-043) — the disabledCommander refuses every media command
// so the build still satisfies media.AgentMediaClient when no signing key is set.
func (disabledCommander) MediaOptimize(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaOptimizeRequest) (agentcmd.MediaOptimizeResponse, error) {
	return agentcmd.MediaOptimizeResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) MediaApply(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaApplyRequest) (agentcmd.MediaApplyResponse, error) {
	return agentcmd.MediaApplyResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) MediaSync(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaSyncRequest) (agentcmd.MediaSyncResponse, error) {
	return agentcmd.MediaSyncResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) MediaRestore(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaRestoreRequest) (agentcmd.MediaRestoreResponse, error) {
	return agentcmd.MediaRestoreResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) MediaDeleteOriginals(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaDeleteOriginalsRequest) (agentcmd.MediaDeleteOriginalsResponse, error) {
	return agentcmd.MediaDeleteOriginalsResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
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
