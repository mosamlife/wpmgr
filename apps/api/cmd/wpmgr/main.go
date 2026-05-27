// Command wpmgr is the WPMgr control-plane API server: it loads config,
// initializes telemetry, connects to Postgres, applies migrations, wires the
// domains, and serves the Gin HTTP API with graceful shutdown.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
	"github.com/mosamlife/wpmgr/apps/api/internal/server"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/telemetry"
	"github.com/mosamlife/wpmgr/apps/api/internal/tenant"
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

	// Migrations run with the owner/superuser DSN (creates the app role +
	// privileged DDL); the application connects with the unprivileged app DSN.
	migPool, err := db.Connect(ctx, cfg.DB.MigrateDSN())
	if err != nil {
		return err
	}
	if err := migPool.Migrate(ctx); err != nil {
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

	srv := server.New(server.Deps{
		Config:      cfg,
		Logger:      logger,
		Pool:        pool,
		Sessions:    sessions,
		Auth:        authn,
		AuthH:       auth.NewHandler(authSvc, sessions, oidcProvider, newTenant),
		MembersH:    auth.NewMembersHandler(authSvc),
		APIKeyH:     apikey.NewHandler(apiKeySvc, auditRec),
		AuditH:      audit.NewHandler(auditRec),
		TenantH:     tenant.NewHandler(tenantSvc, auditRec),
		SiteH:       site.NewHandler(siteSvc, auditRec),
		ServiceName: cfg.OTel.ServiceName,
		Version:     version,
	})

	return srv.Run(ctx)
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
