// Package server wires the Gin engine, middleware stack, route groups, system
// endpoints (/healthz, /readyz), and graceful shutdown.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	"github.com/mosamlife/wpmgr/apps/api/internal/agent"
	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/tenant"
	"github.com/mosamlife/wpmgr/apps/api/internal/update"
	"github.com/mosamlife/wpmgr/apps/api/internal/uptime"
)

// Deps are the server's wired dependencies.
type Deps struct {
	Config       config.Config
	Logger       *slog.Logger
	Pool         *db.Pool
	Sessions     *auth.SessionManager
	Auth         *middleware.Authenticator
	AuthH        *auth.Handler
	MembersH     *auth.MembersHandler
	APIKeyH      *apikey.Handler
	AuditH       *audit.Handler
	TenantH      *tenant.Handler
	SiteH        *site.Handler
	UpdateH      *update.Handler
	BackupH      *backup.Handler
	BackupAgentH *backup.AgentHandler
	UptimeH      *uptime.Handler
	AgentAuth    *agent.Authenticator
	AgentH       *agent.Handler
	ServiceName  string
	Version      string
}

// Server bundles the HTTP server and its dependencies.
type Server struct {
	http *http.Server
	deps Deps
	log  *slog.Logger
}

// New builds the Gin engine and HTTP server.
func New(deps Deps) *Server {
	if deps.Config.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	engine := gin.New()
	engine.Use(
		middleware.RequestID(),
		otelgin.Middleware(deps.ServiceName),
		middleware.Logger(deps.Logger),
		middleware.Recovery(deps.Logger),
		deps.Sessions.LoadAndSave(),
		deps.Auth.Authenticate(),
	)

	s := &Server{
		deps: deps,
		log:  deps.Logger,
		http: &http.Server{
			Addr:              deps.Config.HTTPAddr,
			Handler:           engine,
			ReadHeaderTimeout: 10 * time.Second,
		},
	}

	s.registerSystem(engine)

	// Public auth endpoints (login/register/logout/me + OIDC).
	deps.AuthH.Register(engine)

	// Public agent enrollment (no session/tenant; the pairing code authorizes).
	deps.SiteH.RegisterPublic(engine)

	// Agent-authenticated endpoints: the agent authenticator verifies an Ed25519
	// signed request and resolves the site/tenant from the verified key — this
	// group does NOT use the session/API-key principal chain.
	if deps.AgentAuth != nil && deps.AgentH != nil {
		agentGroup := engine.Group("/agent/v1")
		agentGroup.Use(deps.AgentAuth.Authenticate())
		deps.AgentH.Register(agentGroup)
		// M4 backup callbacks (presigned-URL requests + manifest submission) live
		// under the same agent-authenticated group.
		if deps.BackupAgentH != nil {
			deps.BackupAgentH.Register(agentGroup)
		}
	}

	// Everything under /api/v1 requires an authenticated principal with an
	// active tenant; finer per-route RBAC is applied by each handler.
	v1 := engine.Group("/api/v1")
	v1.Use(authz.RequireAuth(), authz.RequireTenant())
	deps.TenantH.Register(v1)
	deps.SiteH.Register(v1)
	deps.MembersH.Register(v1)
	deps.APIKeyH.Register(v1)
	deps.AuditH.Register(v1)
	if deps.UpdateH != nil {
		deps.UpdateH.Register(v1)
	}
	if deps.BackupH != nil {
		deps.BackupH.Register(v1)
	}
	if deps.UptimeH != nil {
		deps.UptimeH.Register(v1)
	}

	return s
}

func (s *Server) registerSystem(engine *gin.Engine) {
	engine.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gen.Health{
			Status:  gen.HealthStatusOk,
			Version: gen.NewOptString(s.deps.Version),
		})
	})

	engine.GET("/readyz", func(c *gin.Context) {
		checks := map[string]string{}
		status := gen.ReadinessStatusOk
		code := http.StatusOK

		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		defer cancel()
		if err := s.deps.Pool.Ping(ctx); err != nil {
			checks["database"] = "unreachable: " + err.Error()
			status = gen.ReadinessStatusDegraded
			code = http.StatusServiceUnavailable
		} else {
			checks["database"] = "ok"
		}

		c.JSON(code, gen.Readiness{
			Status: status,
			Checks: gen.ReadinessChecks(checks),
		})
	})
}

// Run starts the HTTP server and blocks until ctx is cancelled, then performs a
// graceful shutdown bounded by the configured timeout.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("http server listening", slog.String("addr", s.http.Addr))
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		s.log.Info("shutdown signal received, draining connections")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.deps.Config.Shutdown.Timeout)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	}
}
