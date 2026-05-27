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

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/tenant"
)

// Deps are the server's wired dependencies.
type Deps struct {
	Config      config.Config
	Logger      *slog.Logger
	Pool        *db.Pool
	TenantH     *tenant.Handler
	SiteH       *site.Handler
	ServiceName string
	Version     string
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
		middleware.Tenant(),
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

	v1 := engine.Group("/api/v1")
	deps.TenantH.Register(v1)
	deps.SiteH.Register(v1)

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
