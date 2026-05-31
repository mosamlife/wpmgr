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

	"github.com/mosamlife/wpmgr/apps/api/internal/activity"
	"github.com/mosamlife/wpmgr/apps/api/internal/agent"
	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/autologin"
	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/diagnostics"
	"github.com/mosamlife/wpmgr/apps/api/internal/invitation"
	"github.com/mosamlife/wpmgr/apps/api/internal/loginbrand"
	mediahandler "github.com/mosamlife/wpmgr/apps/api/internal/media/handler"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
	"github.com/mosamlife/wpmgr/apps/api/internal/org"
	"github.com/mosamlife/wpmgr/apps/api/internal/scan"
	"github.com/mosamlife/wpmgr/apps/api/internal/security"
	"github.com/mosamlife/wpmgr/apps/api/internal/sharing"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	siteevents "github.com/mosamlife/wpmgr/apps/api/internal/site/events"
	"github.com/mosamlife/wpmgr/apps/api/internal/sitedestination"
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
	// SiteEventsH serves the M21 tenant-scoped connection-lifecycle SSE stream at
	// GET /api/v1/sites/events (ADR-038). nil ⇒ the route is not mounted.
	SiteEventsH  *siteevents.Handler
	UpdateH      *update.Handler
	BackupH      *backup.Handler
	BackupAgentH *backup.AgentHandler
	// InspectionDeps wires the optional collaborators for the M6 SQL inspection
	// endpoint (manifest fetcher / CP-side legacy cache / River enqueuer). Any
	// field may be nil — the handler degrades to a 503 pointing at the missing
	// tier, so a partial rollout is observable rather than a 404 mystery.
	InspectionDeps  backup.InspectionDeps
	UptimeH         *uptime.Handler
	AutologinH      *autologin.MintHandler
	AutologinAgentH *autologin.AgentHandler
	AgentAuth       *agent.Authenticator
	AgentH          *agent.Handler
	// UpdateAgentH serves the ADR-042 CP-driven self-update manifest at
	// GET /agent/v1/update/manifest. nil ⇒ the route is not mounted (object
	// storage or the signing key is unconfigured). Distinct from UpdateH, the
	// unrelated operator-facing /api/v1 plugin-update handler.
	UpdateAgentH *agent.UpdateHandler
	// SiteDestH serves the ADR-036 P1 per-site destinations CRUD under
	// /api/v1/sites/{siteId}/destinations.
	SiteDestH *sitedestination.Handler
	// ADR-037 Sprint 2 — diagnostics + php-error monitor.
	// DiagnosticsH serves the operator-facing GETs + silence/refresh under
	// /api/v1/sites/{siteId}/(diagnostics|errors).
	DiagnosticsH *diagnostics.Handler
	// DiagnosticsAgentH ingests the agent's daily 14-category push at
	// POST /agent/v1/diagnostics.
	DiagnosticsAgentH *agent.DiagnosticsHandler
	// ErrorsAgentH ingests the heartbeat-driven php-error batches at
	// POST /agent/v1/errors.
	ErrorsAgentH *agent.ErrorsHandler
	// ADR-037 Sprint 3 — WordPress activity log.
	// ActivityH serves the operator-facing list + chain-verify under
	// /api/v1/sites/{siteId}/activity[/verify].
	ActivityH *activity.Handler
	// ActivityAgentH ingests the agent's hash-chained activity batch at
	// POST /agent/v1/activity.
	ActivityAgentH *agent.ActivityHandler
	// S2 — Login Protection + IP store.
	// SecurityH serves the operator-facing security routes under
	// /api/v1/sites/{siteId}/security/...
	SecurityH *security.Handler
	// SecurityAgentH ingests the agent's login-event batch at
	// POST /agent/v1/security/login-events.
	SecurityAgentH *agent.SecurityLoginEventsHandler
	// M14 — Login Whitelabel.
	// LoginBrandH serves the operator-facing login brand routes under
	// /api/v1/sites/{siteId}/login-brand.
	LoginBrandH *loginbrand.Handler
	// S3 — Malware / File-Integrity Scan. ScanH serves operator-facing scan
	// run management + findings under /api/v1/sites/{siteId}/scans and
	// /api/v1/findings/{id}/ignore.
	ScanH *scan.Handler
	// m16 — Restore Runs + Logs. RestoreRunH serves the per-site restore
	// history and the by-id detail + phase-log endpoints.
	RestoreRunH *backup.RestoreRunHandler
	// M17 — Schedule Runs. ScheduleRunH serves the per-site schedule run
	// queue (upcoming + past) and the by-id detail endpoint.
	ScheduleRunH *backup.ScheduleRunHandler
	// M5.7 — Orgs + Sharing + Invitations.
	OrgH        *org.Handler        // POST /orgs, POST /orgs/:orgId/activate
	SharingH    *sharing.Handler    // site shares CRUD + shared-with-me
	InvitationH *invitation.Handler // public POST /invitations/accept
	// M23 — Media Optimizer (ADR-043). MediaH serves the operator-facing
	// /api/v1/sites/{siteId}/media/... dashboard routes; MediaAgentH serves the
	// agent-authenticated /agent/v1/media/... callbacks. Either may be nil.
	MediaH      *mediahandler.Handler
	MediaAgentH *mediahandler.AgentHandler
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

	// Public invitation-accept endpoint (creates the session itself; no RequireAuth).
	if deps.InvitationH != nil {
		deps.InvitationH.RegisterPublic(engine)
	}

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
		if deps.AutologinAgentH != nil {
			deps.AutologinAgentH.Register(agentGroup)
		}
		// ADR-037 Sprint 2 — agent ingestion routes for diagnostics + errors.
		// Authenticated via the same Ed25519 signed-request middleware as the
		// metadata/heartbeat routes; the site + tenant are resolved from the
		// verified identity.
		if deps.DiagnosticsAgentH != nil {
			deps.DiagnosticsAgentH.Register(agentGroup)
		}
		if deps.ErrorsAgentH != nil {
			deps.ErrorsAgentH.Register(agentGroup)
		}
		// ADR-037 Sprint 3 — agent ingestion route for the hash-chained
		// WordPress activity log. Same Ed25519 signed-request auth as above.
		if deps.ActivityAgentH != nil {
			deps.ActivityAgentH.Register(agentGroup)
		}
		// S2 — agent ingest route for login events. Authenticated via the same
		// Ed25519 signed-request middleware as all other agent routes.
		if deps.SecurityAgentH != nil {
			deps.SecurityAgentH.Register(agentGroup)
		}
		// ADR-042 — CP-driven self-update manifest. Same Ed25519 signed-request
		// auth; the agent's site is resolved from the verified identity and
		// pinned into the manifest's aud claim.
		if deps.UpdateAgentH != nil {
			deps.UpdateAgentH.Register(agentGroup)
		}
		// M23 — Media Optimizer agent callbacks (sync-batch / presign /
		// encode-ready / job-status / restore-status). Same Ed25519 signed-request
		// auth; site + tenant resolved from the verified identity.
		if deps.MediaAgentH != nil {
			deps.MediaAgentH.Register(agentGroup)
		}
	}

	// Everything under /api/v1 requires an authenticated principal with an
	// active tenant; finer per-route RBAC is applied by each handler.
	v1 := engine.Group("/api/v1")
	v1.Use(authz.RequireAuth(), authz.RequireTenant())
	deps.TenantH.Register(v1)
	deps.SiteH.Register(v1)
	// M21 — tenant-scoped connection-lifecycle SSE stream (GET /sites/events).
	if deps.SiteEventsH != nil {
		deps.SiteEventsH.Register(v1)
	}
	deps.MembersH.Register(v1)
	deps.APIKeyH.Register(v1)
	deps.AuditH.Register(v1)
	if deps.SiteDestH != nil {
		// ADR-036 P1 storage adapter: per-site destination management.
		deps.SiteDestH.Register(v1)
	}
	if deps.UpdateH != nil {
		deps.UpdateH.Register(v1)
	}
	if deps.BackupH != nil {
		deps.BackupH.Register(v1)
		// M6 / Track 4: mount the sql-inspection route. Split from Register so
		// callers without the optional inspection deps (manifest fetcher / cache
		// / River enqueuer) can mount the rest of the backup API without it.
		deps.BackupH.RegisterInspection(v1, deps.InspectionDeps)
	}
	if deps.UptimeH != nil {
		deps.UptimeH.Register(v1)
	}
	if deps.AutologinH != nil {
		deps.AutologinH.Register(v1)
	}
	// ADR-037 Sprint 2 — operator-facing site Health + Errors routes.
	if deps.DiagnosticsH != nil {
		deps.DiagnosticsH.Register(v1)
	}
	// ADR-037 Sprint 3 — operator-facing activity log + chain verify.
	if deps.ActivityH != nil {
		deps.ActivityH.Register(v1)
	}
	// S2 — operator-facing security routes (login protection config + events).
	if deps.SecurityH != nil {
		deps.SecurityH.Register(v1)
	}
	// M14 — operator-facing login brand routes.
	if deps.LoginBrandH != nil {
		deps.LoginBrandH.Register(v1)
	}
	// S3 — operator-facing scan run management + findings routes.
	if deps.ScanH != nil {
		deps.ScanH.Register(v1)
	}
	// m16 — restore run history + phase log.
	if deps.RestoreRunH != nil {
		deps.RestoreRunH.Register(v1)
	}
	// M17 — schedule run queue (upcoming + past history).
	if deps.ScheduleRunH != nil {
		deps.ScheduleRunH.Register(v1)
	}
	// M5.7 — Orgs + Sharing.
	if deps.OrgH != nil {
		deps.OrgH.Register(v1)
	}
	if deps.SharingH != nil {
		deps.SharingH.Register(v1)
	}
	// M23 — operator-facing Media Optimizer dashboard routes.
	if deps.MediaH != nil {
		deps.MediaH.Register(v1)
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
