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
	"github.com/mosamlife/wpmgr/apps/api/internal/admin"
	"github.com/mosamlife/wpmgr/apps/api/internal/agent"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentrelease"
	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/autologin"
	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	clientpkg "github.com/mosamlife/wpmgr/apps/api/internal/client"
	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/diagnostics"
	"github.com/mosamlife/wpmgr/apps/api/internal/email"
	"github.com/mosamlife/wpmgr/apps/api/internal/files"
	"github.com/mosamlife/wpmgr/apps/api/internal/govcontext"
	"github.com/mosamlife/wpmgr/apps/api/internal/invitation"
	"github.com/mosamlife/wpmgr/apps/api/internal/loginbrand"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	mediahandler "github.com/mosamlife/wpmgr/apps/api/internal/media/handler"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
	"github.com/mosamlife/wpmgr/apps/api/internal/objectcache"
	"github.com/mosamlife/wpmgr/apps/api/internal/org"
	"github.com/mosamlife/wpmgr/apps/api/internal/perf"
	portalpkg "github.com/mosamlife/wpmgr/apps/api/internal/portal"
	"github.com/mosamlife/wpmgr/apps/api/internal/pricing"
	reportpkg "github.com/mosamlife/wpmgr/apps/api/internal/report"
	"github.com/mosamlife/wpmgr/apps/api/internal/rum"
	"github.com/mosamlife/wpmgr/apps/api/internal/scan"
	"github.com/mosamlife/wpmgr/apps/api/internal/screenshot"
	"github.com/mosamlife/wpmgr/apps/api/internal/security"
	"github.com/mosamlife/wpmgr/apps/api/internal/settings"
	"github.com/mosamlife/wpmgr/apps/api/internal/sharing"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	siteevents "github.com/mosamlife/wpmgr/apps/api/internal/site/events"
	"github.com/mosamlife/wpmgr/apps/api/internal/sitedestination"
	"github.com/mosamlife/wpmgr/apps/api/internal/sitetag"
	"github.com/mosamlife/wpmgr/apps/api/internal/tenant"
	"github.com/mosamlife/wpmgr/apps/api/internal/update"
	"github.com/mosamlife/wpmgr/apps/api/internal/uptime"
	"github.com/mosamlife/wpmgr/apps/api/internal/vuln"
)

// Deps are the server's wired dependencies.
type Deps struct {
	Config   config.Config
	Logger   *slog.Logger
	Pool     *db.Pool
	Sessions *auth.SessionManager
	Auth     *middleware.Authenticator
	AuthH    *auth.Handler
	MembersH *auth.MembersHandler
	APIKeyH  *apikey.Handler
	AuditH   *audit.Handler
	TenantH  *tenant.Handler
	SiteH    *site.Handler
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
	InspectionDeps   backup.InspectionDeps
	UptimeH          *uptime.Handler
	AutologinH       *autologin.MintHandler
	AutologinPolicyH *autologin.PolicyHandler
	AutologinAgentH  *autologin.AgentHandler
	AgentAuth        *agent.Authenticator
	AgentH           *agent.Handler
	// UpdateAgentH serves the ADR-042 CP-driven self-update manifest at
	// GET /agent/v1/update/manifest. nil ⇒ the route is not mounted (object
	// storage or the signing key is unconfigured). Distinct from UpdateH, the
	// unrelated operator-facing /api/v1 plugin-update handler.
	UpdateAgentH *agent.UpdateHandler
	// SiteDestH serves the ADR-036 P1 per-site destinations CRUD under
	// /api/v1/sites/{siteId}/destinations.
	SiteDestH *sitedestination.Handler
	// SettingsH serves the ADR-045 instance SMTP settings under
	// /api/v1/settings/smtp (GET/PUT/test). nil ⇒ routes not mounted.
	SettingsH *settings.Handler
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
	// m79 — Vulnerability Scanner. VulnH serves the fleet-rollup endpoint at
	// GET /api/v1/vulnerabilities and per-site finding management under
	// /api/v1/sites/{siteId}/vulnerabilities/... nil ⇒ routes not mounted.
	VulnH *vuln.Handler
	// AgentReleaseH serves the read-only agent-freshness dashboard routes:
	// GET /api/v1/agent/latest (currently published agent version) and
	// GET /api/v1/fleet/agents (tenant-scoped per-site agent-version
	// rollup + counts). nil ⇒ routes not mounted.
	AgentReleaseH *agentrelease.Handler
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
	// ADR-064 slice S4 — governed org/site context: CRUD + version history +
	// diff + restore for layers 2 and 3, plus the effective-context preview
	// (Decision 8) at GET /sites/{siteId}/context/effective.
	GovContextH *govcontext.Handler
	// M23 — Media Optimizer (ADR-043). MediaH serves the operator-facing
	// /api/v1/sites/{siteId}/media/... dashboard routes; MediaAgentH serves the
	// agent-authenticated /agent/v1/media/... callbacks. Either may be nil.
	MediaH      *mediahandler.Handler
	MediaAgentH *mediahandler.AgentHandler
	// m36 / ADR-046 — Performance Suite. PerfH serves the operator-facing
	// /api/v1/sites/{siteId}/perf|cache|db|rucss/... routes + the portfolio
	// /api/v1/cache/* bulk routes; PerfAgentH serves the agent-authenticated
	// /agent/v1/cache/* + /agent/v1/perf/* + /agent/v1/rucss callbacks. Either
	// may be nil.
	PerfH      *perf.Handler
	PerfAgentH *perf.AgentHandler
	// m68 — Object Cache (P0+P1). ObjectCacheH serves the operator-facing
	// /api/v1/sites/{siteId}/perf/object-cache/... routes.
	// nil => routes not mounted.
	ObjectCacheH *objectcache.Handler
	// FontResultsAgentH serves POST /agent/v1/fonts/results (M55 — font results
	// catalog push from the media-encoder). nil ⇒ route not mounted.
	FontResultsAgentH *perf.FontResultsAgentHandler
	// ScreenshotH serves the M72 manual screenshot refresh at
	// POST /api/v1/sites/{siteId}/screenshot/refresh. nil ⇒ route not mounted.
	ScreenshotH *screenshot.Handler
	// AdminH serves the superadmin instance-management area under
	// /api/v1/admin. nil ⇒ routes not mounted.
	AdminH *admin.Handler
	// RumH serves the public POST /rum/ingest endpoint (M56 — Real User
	// Monitoring). Mounted on the root engine (no session, no tenant gate);
	// the beacon key is the sole access credential. nil ⇒ route not mounted.
	RumH *rum.Handler
	// EmailH serves the m59 per-site email management routes under
	// /api/v1/sites/{siteId}/email/... and the org-level routes under
	// /api/v1/email/... nil ⇒ routes not mounted.
	EmailH *email.Handler
	// FilesH serves the P1 read-only File Manager routes under
	// /api/v1/sites/{siteId}/files (list / content / download).
	// nil ⇒ routes not mounted. Off-by-default per site; the per-site opt-in
	// flag gates every route inside the handler.
	FilesH *files.Handler

	// EmailAgentH serves the Phase-3 agent email log ingest at
	// POST /agent/v1/email/log. nil ⇒ route not mounted.
	EmailAgentH *email.AgentHandler
	// EmailWebhookH serves the Phase-4a public webhook endpoints at
	// POST /webhooks/email/{provider}. nil ⇒ routes not mounted.
	// These routes carry NO session or tenant gate — provider-signature
	// verification IS the auth.
	EmailWebhookH *email.WebhookHandler
	// EmailAgentSuppressionH serves the Phase-4a agent suppression-fetch
	// endpoint at GET /agent/v1/email/suppression. nil ⇒ route not mounted.
	EmailAgentSuppressionH *email.AgentSuppressionHandler
	// HIBPAgentH serves the Phase-3 HIBP breach-password range proxy at
	// GET /agent/v1/security/hibp/range/:prefix (ADR-059). Agents send only the
	// 5-char SHA-1 prefix; the CP returns the cached SUFFIX:COUNT body.
	// nil ⇒ route not mounted (safe default; the agent degrades to fail-open).
	HIBPAgentH *agent.HIBPHandler
	// ClientH serves the m63 agency-client management routes under
	// /api/v1/clients. nil ⇒ routes not mounted.
	ClientH *clientpkg.Handler
	// SiteTagH serves the m100 (GH #230 "rich tags") tag-registry routes
	// under /api/v1/tags. nil ⇒ routes not mounted.
	SiteTagH *sitetag.Handler
	// ReportH serves the m64 white-label report routes under
	// /api/v1/clients/:clientId/report-schedule and /api/v1/clients/:clientId/reports.
	// nil ⇒ routes not mounted.
	ReportH *reportpkg.Handler
	// PortalH serves the m66 read-only client portal routes under /api/v1/portal.
	// nil ⇒ routes not mounted. All portal routes are gated by RequireClientPortal.
	PortalH *portalpkg.Handler
	// BillingH serves the M16 Phase B operator-facing billing routes under
	// /api/v1/billing (summary, checkout, portal). nil ⇒ routes not mounted —
	// wired only when WPMGR_HOSTED is enabled (see cmd/wpmgr/main.go); this is
	// the routes-contract 404-when-unhosted guarantee.
	BillingH *billing.Handler
	// BillingWebhookH serves the M16 Phase B public payment-provider webhook
	// endpoint at POST /webhooks/billing/:provider. nil ⇒ route not mounted.
	BillingWebhookH *billing.WebhookHandler
	// PricingH serves the PUBLIC, unauthenticated live-pricing endpoint at
	// GET /api/v1/pricing (internal/pricing) — the marketing site's price
	// source. nil ⇒ route not mounted; wired only when WPMGR_HOSTED is
	// enabled (see cmd/wpmgr/main.go), mirroring BillingH/BillingWebhookH's
	// nil-when-unhosted convention exactly. Mounted directly on the root
	// engine (not under the tenant-gated /api/v1 group) despite sharing its
	// path prefix — see RegisterPublic.
	PricingH *pricing.Handler
	// MCPTransportH serves the SINGLE Streamable HTTP MCP endpoint at
	// POST /mcp (internal/mcp). It authenticates a bearer connection token on
	// every request and carries no session, so it is mounted on the root
	// engine rather than on the session-auth group.
	//
	// UNLIKE the nil-when-unhosted handlers above, this one is REQUIRED and is
	// mounted unconditionally: the endpoint is published to users on the
	// connect screen, and an unmounted route answers 404 — which would hide a
	// wiring failure behind something that looks like a deliberate refusal.
	// An unauthenticated request here must be 401, and 401 is only reachable
	// if the route exists.
	MCPTransportH *mcp.TransportHandler
	// BillingSuspensionGate is the M16 Phase C1 superadmin hard-lockout
	// middleware (billing.Service.SuspensionGate) mounted on the tenant-scoped
	// v1 group. nil ⇒ WPMGR_HOSTED is off; self-host never sees this check
	// (mirrors BillingH/BillingWebhookH's nil-when-unhosted convention).
	BillingSuspensionGate gin.HandlerFunc
	ServiceName           string
	Version               string
}

// Server bundles the HTTP server and its dependencies.
type Server struct {
	http *http.Server
	deps Deps
	log  *slog.Logger
}

// Handler returns the underlying http.Handler (the *gin.Engine built by New).
// Exists so tests can drive the full production route tree directly through
// httptest / engine.Routes() without duplicating New's mounting logic —
// notably the OpenAPI route-coverage contract test (tests/openapi_route_
// coverage_test.go). Not used by production code (Run calls ListenAndServe
// directly on the wrapped http.Server).
func (s *Server) Handler() http.Handler {
	return s.http.Handler
}

// newHTTPServer builds the process's one http.Server, and is where the
// SERVER-LEVEL timeout posture is decided. Split out from New so the decision is
// pinned by a test (server_timeouts_test.go) rather than living as a comment
// somebody re-opens later.
//
// ReadHeaderTimeout IS SET (10s). It bounds a client that opens a connection and
// dribbles request headers, which no legitimate caller does and which costs a
// goroutine each. It never touches a response.
//
// WriteTimeout IS DELIBERATELY UNSET. It is a whole-response deadline measured
// from the end of the request headers, applied to EVERY handler in the process.
// The routes that would break first are the ones that are supposed to stay open:
// the update-run and backup-run SSE streams and the connection-lifecycle event
// bus, all of which hold a single response open for minutes by design; a
// WriteTimeout would cut them at a fixed wall clock and the dashboard would lose
// live progress. The agent package download has the same problem from the other
// end: a legitimately slow site needs minutes for a multi-megabyte zip, and a
// duration cap on that response is exactly the defect this work exists to fix.
// The bound those responses actually need is a PROGRESS bound, which is per
// response and therefore belongs on the response: the package route takes over
// its own connection and runs a stall watchdog restarted by every partial write
// the socket accepts (see internal/agent/update_package_limits.go,
// streamPackage), which bounds a consumer that has stopped accepting bytes,
// imposes no rate on one that is merely slow, and touches no other handler.
//
// IdleTimeout IS DELIBERATELY UNSET. It would only bound keep-alive connections
// BETWEEN requests, so it is harmless to SSE, but it is not free: this API sits
// behind a Google Cloud load balancer whose own backend idle timeout is 600s,
// and a backend that closes an idle connection sooner than the balancer expects
// races the balancer into reusing a socket the server is closing, which surfaces
// to users as sporadic 502s. Any IdleTimeout added here must therefore exceed
// the balancer's, at which point it bounds almost nothing that matters. If one
// is ever wanted, set it above 620s and say so here.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// New builds the Gin engine and HTTP server.
func New(deps Deps) *Server {
	if deps.Config.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	engine := gin.New()
	// Recovery MUST remain on the root engine so that ALL route groups — including
	// the public /rum/ingest endpoint — are covered by panic-safety. (L3: this
	// middleware must not be moved off the root engine when reorganising groups.)
	engine.Use(
		middleware.RequestID(),
		otelgin.Middleware(deps.ServiceName),
		middleware.Logger(deps.Logger),
		middleware.Recovery(deps.Logger),
	)
	// sessionAuthGroup is a zero-prefix group that carries Sessions.LoadAndSave()
	// and Auth.Authenticate(). All routes that need a session or principal
	// (auth endpoints, /api/v1, /agent/v1) register through this group or through
	// groups derived from it. The public /rum/ingest endpoint must NOT share these
	// middlewares: it carries no session and no tenant GUC, so loading a session
	// on each beacon costs a Redis round-trip for nothing and creates a standing
	// trap where future code might accidentally touch the session or principal on
	// the ingest path. (H2 fix: Sessions.LoadAndSave + Auth.Authenticate are
	// absent from the root engine and absent from the /rum group.)
	sessionAuthGroup := engine.Group("",
		deps.Sessions.LoadAndSave(),
		deps.Auth.Authenticate(),
	)

	s := &Server{
		deps: deps,
		log:  deps.Logger,
		http: newHTTPServer(deps.Config.HTTPAddr, engine),
	}

	s.registerSystem(engine)

	// Public auth endpoints (login/register/logout/me + OIDC) need session + auth.
	deps.AuthH.Register(sessionAuthGroup)

	// Public invitation-accept endpoint (creates the session itself; no RequireAuth).
	if deps.InvitationH != nil {
		deps.InvitationH.RegisterPublic(sessionAuthGroup)
	}

	// Public agent enrollment (no session/tenant; the pairing code authorizes).
	deps.SiteH.RegisterPublic(engine)

	// GH #302: public agent package download:
	// GET /agent/v1/update/package/:siteId?token=<signed>. Mounted on the ROOT
	// engine, deliberately NOT on the agent-authenticated group below: the agent
	// downloads the package with a bare request carrying no credentials and no
	// redirect following, so a signed, short-lived, site- and version-bound token
	// in the query string is the entire authorisation (see
	// internal/agent/update_package_handler.go). Must NOT use sessionAuthGroup
	// (H2 note above). Until EnablePackageServing arms the handler this route
	// refuses everything, which is the self-host-off default.
	if deps.UpdateAgentH != nil {
		deps.UpdateAgentH.RegisterPublic(engine)
	}

	// Public RUM ingest (M56): POST /rum/ingest. No session, no tenant gate.
	// The beacon key is the only credential. Isolated from /api/v1 and /agent/v1.
	// Intentionally does NOT use sessionAuthGroup — see H2 note above.
	if deps.RumH != nil {
		deps.RumH.RegisterPublic(engine)
	}

	// Phase-4a public email webhook endpoints: POST /webhooks/email/{provider}.
	// No session, no tenant gate — provider HMAC/RSA/ECDSA signature is the auth.
	// Mounted on the root engine so SNS SubscriptionConfirmation GETs also work
	// without a session cookie. Must NOT use sessionAuthGroup (H2 note above).
	if deps.EmailWebhookH != nil {
		deps.EmailWebhookH.RegisterPublic(engine)
	}

	// M16 Phase B — public payment-provider webhook: POST
	// /webhooks/billing/:provider. No session, no tenant gate — the
	// provider's own request signature (verified inside
	// billing.Service.ProcessWebhook) is the entire auth boundary. Mirrors the
	// email webhook mounting immediately above. Intentionally does NOT use
	// sessionAuthGroup (H2 note above).
	if deps.BillingWebhookH != nil {
		deps.BillingWebhookH.RegisterPublic(engine)
	}

	// M16 live-pricing Phase 1 — public GET /api/v1/pricing. No session, no
	// tenant gate; mounted directly on the root engine like the webhook
	// immediately above. nil ⇒ WPMGR_HOSTED is off; self-host 404s here.
	if deps.PricingH != nil {
		deps.PricingH.RegisterPublic(engine)
	}

	// S6b — the single Streamable HTTP MCP endpoint: POST /mcp. One endpoint,
	// one origin, one TLS boundary; SSE is not required of a client in
	// Phase 1, so this never upgrades.
	//
	// Mounted on the ROOT engine and NOT on sessionAuthGroup: an MCP client
	// presents a bearer connection token and no cookie, so a session load here
	// would be a Redis round trip per request for nothing and would create a
	// standing trap where this path might come to read a session principal.
	// The handler re-checks the grant against current state on every request,
	// so revocation lands on the next call.
	if deps.MCPTransportH != nil {
		deps.MCPTransportH.Register(engine)
	}

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
		// m36 / ADR-046 — Performance Suite agent callbacks: cache stats report,
		// perf config-ack, and the RUCSS multipart ingest. Same Ed25519
		// signed-request auth; the RUCSS endpoint additionally asserts the body's
		// site_id matches the JWT-bound site.
		if deps.PerfAgentH != nil {
			deps.PerfAgentH.Register(agentGroup)
		}
		// M55 — Font results catalog push from the media-encoder/agent.
		if deps.FontResultsAgentH != nil {
			deps.FontResultsAgentH.Register(agentGroup)
		}
		// m59 Phase 3 — email log ingest from the agent.
		if deps.EmailAgentH != nil {
			deps.EmailAgentH.Register(agentGroup)
		}
		// m59 Phase 4a — agent suppression-fetch delta endpoint.
		// GET /agent/v1/email/suppression?since=<cursor>
		if deps.EmailAgentSuppressionH != nil {
			deps.EmailAgentSuppressionH.Register(agentGroup)
		}
		// ADR-059 Phase 3 — HIBP breach-password range proxy.
		// GET /agent/v1/security/hibp/range/:prefix
		if deps.HIBPAgentH != nil {
			deps.HIBPAgentH.Register(agentGroup)
		}
	}

	// Everything under /api/v1 requires session load + authentication + an active
	// tenant; finer per-route RBAC is applied by each handler.
	// Derive from sessionAuthGroup so session + auth middlewares are inherited.
	v1 := sessionAuthGroup.Group("/api/v1")
	v1.Use(authz.RequireAuth(), authz.RequireTenant())
	// M16 Phase C1 — superadmin hard-lockout: 402 {code:"account_suspended"}
	// for a suspended tenant's requests, EXCEPT its own /billing/* routes (so
	// it can pay/fix — see billing.Service.SuspensionGate's exemption). Added
	// before any handler below registers on v1 so it wraps the whole group,
	// including sub-groups (e.g. BillingH's own "/billing" child group)
	// registered later — the gate's own path check is what actually exempts
	// them. nil on an unhosted boot: self-host never sees this middleware at
	// all.
	if deps.BillingSuspensionGate != nil {
		v1.Use(deps.BillingSuspensionGate)
	}

	// Org routes require session + auth but NOT an active tenant: a user creates
	// (or lists, or activates) their FIRST organisation precisely when they have
	// none yet (e.g. a former site-collaborator whose access was revoked).
	// RequireTenant would 403 them out of the create-org onboarding. Each org
	// handler does its own per-org membership/role authorization, so dropping the
	// tenant gate here opens no hole. (ADR-045 Phase 3 onboarding.)
	v1Auth := sessionAuthGroup.Group("/api/v1")
	v1Auth.Use(authz.RequireAuth())
	deps.TenantH.Register(v1)
	deps.SiteH.Register(v1)
	// m100 (GH #230 "rich tags") — tenant-level tag registry.
	if deps.SiteTagH != nil {
		deps.SiteTagH.Register(v1)
	}
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
	if deps.SettingsH != nil {
		// ADR-045 — instance SMTP settings + send-test.
		deps.SettingsH.Register(v1)
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
	if deps.AutologinPolicyH != nil {
		deps.AutologinPolicyH.Register(v1)
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
	// m79 — vulnerability scanner: fleet rollup + per-site finding management.
	if deps.VulnH != nil {
		deps.VulnH.Register(v1)
	}
	// Read-only agent-freshness dashboard: GET /agent/latest (published
	// version) + GET /fleet/agents (tenant-scoped per-site rollup).
	if deps.AgentReleaseH != nil {
		deps.AgentReleaseH.Register(v1)
	}

	// m82 — P1 read-only File Manager (list dir / read content / presigned download).
	// Off-by-default per site; the per-site opt-in flag gates every route.
	// Admin+ for browse/read/download; owner-only for sensitive paths (T4/T6).
	if deps.FilesH != nil {
		deps.FilesH.Register(v1)
	}
	// M72 — site screenshot refresh endpoint. Gated on RequireSiteAccess inside
	// the handler's Register (same as every per-site endpoint).
	if deps.ScreenshotH != nil {
		deps.ScreenshotH.Register(v1)
	}
	// m16 — restore run history + phase log.
	if deps.RestoreRunH != nil {
		deps.RestoreRunH.Register(v1)
	}
	// M17 — schedule run queue (upcoming + past history).
	if deps.ScheduleRunH != nil {
		deps.ScheduleRunH.Register(v1)
	}
	// M5.7 — Orgs + Sharing. Org routes mount on the auth-only group so a
	// tenant-less user can create/list/activate their first org (see v1Auth).
	if deps.OrgH != nil {
		deps.OrgH.Register(v1Auth)
	}
	if deps.SharingH != nil {
		deps.SharingH.Register(v1)
	}
	// ADR-064 slice S4 — governed org/site context (Decision 13). Org routes
	// gate on RequirePermission alone (orgId must equal the caller's active
	// tenant — see govcontext.Handler.orgPrincipal); site routes additionally
	// gate on RequireSiteAccess inside the handler's own Register, same as
	// every other per-site resource.
	if deps.GovContextH != nil {
		deps.GovContextH.Register(v1)
	}
	// M23 — operator-facing Media Optimizer dashboard routes.
	if deps.MediaH != nil {
		deps.MediaH.Register(v1)
	}
	// m36 / ADR-046 — operator-facing Performance Suite dashboard routes +
	// portfolio bulk cache routes.
	if deps.PerfH != nil {
		deps.PerfH.Register(v1)
	}

	// m68 — Object Cache operator routes: GET/PUT config, POST test/enable/
	// disable/flush, GET stats-history. All under /sites/:siteId/perf/object-cache.
	if deps.ObjectCacheH != nil {
		deps.ObjectCacheH.Register(v1)
	}

	// m59 — per-site email management (config + secrets + provider catalog +
	// test-send). Per-site routes under /sites/{siteId}/email/...; org-level
	// routes under /email/...
	if deps.EmailH != nil {
		deps.EmailH.Register(v1)
	}

	// m63 — agency client management: list/create/update/delete clients +
	// bulk site assignment. All routes are org-scoped (no site collaborators).
	if deps.ClientH != nil {
		deps.ClientH.Register(v1)
	}

	// m64 — white-label client reports: schedule management + on-demand generation.
	// Routes are nested under /clients/:clientId/ and share RequireOrgScope().
	if deps.ReportH != nil {
		deps.ReportH.Register(v1)
	}

	// m66 — read-only client portal. Routes under /api/v1/portal/*; gated by
	// RequireClientPortal (session user resolved via client_members). Per-site
	// sub-routes additionally carry RequireSiteAccess. GET-only.
	if deps.PortalH != nil {
		deps.PortalH.Register(v1)
	}

	// m33 — superadmin instance-management area (auth-only, not tenant-gated).
	if deps.AdminH != nil {
		deps.AdminH.Register(v1Auth)
	}

	// M16 Phase B — tenant-facing billing routes (summary/checkout/portal).
	// nil ⇒ WPMGR_HOSTED is off; these three paths simply 404.
	if deps.BillingH != nil {
		deps.BillingH.Register(v1)
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
		err := s.http.Shutdown(shutdownCtx)
		// Shutdown does not cover everything this process is serving, so the
		// return is deliberately held until the rest has drained on the SAME
		// budget. See drainPackageStreams.
		s.drainPackageStreams(shutdownCtx)
		if err != nil {
			return err
		}
		return nil
	}
}

// drainPackageStreams waits out the in-flight agent package downloads that
// http.Server.Shutdown does NOT wait for, inside whatever is left of the
// shutdown budget.
//
// WHY THIS IS NOT COVERED BY Shutdown. Shutdown's contract excludes HIJACKED
// connections, and the agent package route hijacks its connection so the write
// half can observe partial writes and run a real progress bound (see
// internal/agent/update_package_limits.go, streamPackage). Measured against that
// handler with a download mid-body, same handler and same state in both rows:
//
//	HIJACKED  Shutdown returned after 0s      (err=nil)                     in-flight=1
//	FALLBACK  Shutdown returned after 5.002s  (err=context deadline exceeded) in-flight=1
//
// So taking the connection over silently stopped this route draining on
// SIGTERM. On a revision rollout or a scale-down the process would exit
// immediately with up to maxConcurrentPackageStreams zips mid-transfer; each of
// those sites fails its size and sha256 check and retries on its next check.
// Nothing is lost, but "agent updates fail whenever we deploy" is a symptom that
// gets attributed to almost anything else, so the streams are drained rather
// than documented.
//
// BOUNDED BY THE SAME BUDGET, DELIBERATELY. ctx here is the shutdown context, so
// a slow stream can only ever consume time Shutdown itself did not, and can
// never hold a deploy past Shutdown.Timeout. When the budget runs out this logs
// what was still in flight and returns; the process exits and those connections
// die exactly as they did before.
func (s *Server) drainPackageStreams(ctx context.Context) {
	if s.deps.UpdateAgentH == nil {
		return
	}
	inFlight := s.deps.UpdateAgentH.PackageStreamsInFlight()
	if inFlight == 0 {
		return
	}

	s.log.Info("waiting for in-flight agent package downloads",
		slog.Int("in_flight", inFlight))
	start := time.Now()
	if left := s.deps.UpdateAgentH.WaitForPackageStreams(ctx); left > 0 {
		s.log.Warn("shutdown budget expired with agent package downloads still in flight; those sites receive a truncated zip, fail their integrity check and retry on their next update check",
			slog.Int("in_flight", left),
			slog.Duration("waited", time.Since(start)))
		return
	}
	s.log.Info("in-flight agent package downloads drained",
		slog.Duration("waited", time.Since(start)))
}
