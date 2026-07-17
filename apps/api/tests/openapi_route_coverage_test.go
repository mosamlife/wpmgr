// openapi_route_coverage_test.go — the P2-B docs-drift structural fix: a
// full-engine contract test that builds the REAL production Gin engine
// (server.New, the exact wiring internal/server/server.go's New performs)
// against a real Postgres, enumerates engine.Routes(), normalises Gin's
// :param syntax to OpenAPI's {param} syntax, and diffs it in BOTH directions
// against packages/openapi/openapi.yaml:
//
//   - a live route-method with no matching spec path+method fails the test
//     (an undocumented route — the class this file exists to catch).
//   - a spec path+method with no matching live route fails the test (a STALE
//     spec entry — this is how a handler that exists but is never mounted,
//     e.g. GH #240's fonts/transcode, gets caught automatically).
//
// A small, explicitly-commented allowlist covers routes that are genuinely
// not documented API surface (static system probes) or that this harness
// cannot mount without infrastructure this repo's own test suite does not
// stand up anywhere (a live payment-provider SDK, a live SMTP relay, a real
// object-storage endpoint, a WebAuthn relying-party origin, a configured OIDC
// issuer). Every allowlist entry carries a reason; the goal is an
// EMPTY-ish list — grep the reasons below before adding to it.
package tests

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/mosamlife/wpmgr/apps/api/internal/activity"
	"github.com/mosamlife/wpmgr/apps/api/internal/admin"
	"github.com/mosamlife/wpmgr/apps/api/internal/agent"
	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/autologin"
	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	clientpkg "github.com/mosamlife/wpmgr/apps/api/internal/client"
	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/diagnostics"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/email"
	"github.com/mosamlife/wpmgr/apps/api/internal/files"
	"github.com/mosamlife/wpmgr/apps/api/internal/invitation"
	"github.com/mosamlife/wpmgr/apps/api/internal/loginbrand"
	mediahandler "github.com/mosamlife/wpmgr/apps/api/internal/media/handler"
	mediarepo "github.com/mosamlife/wpmgr/apps/api/internal/media/repo"
	mediaservice "github.com/mosamlife/wpmgr/apps/api/internal/media/service"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
	"github.com/mosamlife/wpmgr/apps/api/internal/objectcache"
	"github.com/mosamlife/wpmgr/apps/api/internal/org"
	"github.com/mosamlife/wpmgr/apps/api/internal/perf"
	"github.com/mosamlife/wpmgr/apps/api/internal/portal"
	"github.com/mosamlife/wpmgr/apps/api/internal/pricing"
	reportpkg "github.com/mosamlife/wpmgr/apps/api/internal/report"
	"github.com/mosamlife/wpmgr/apps/api/internal/rum"
	"github.com/mosamlife/wpmgr/apps/api/internal/scan"
	"github.com/mosamlife/wpmgr/apps/api/internal/screenshot"
	"github.com/mosamlife/wpmgr/apps/api/internal/security"
	"github.com/mosamlife/wpmgr/apps/api/internal/server"
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

// ---------------------------------------------------------------------------
// Allowlist — routes NOT expected to appear in the OpenAPI spec, or that this
// harness cannot mount, each with a mandatory reason.
// ---------------------------------------------------------------------------

type routeKey struct {
	method string
	path   string // OpenAPI-style, {param} not :param
}

// allowlistLiveNotInSpec: live Gin routes that are intentionally undocumented.
// Empty: every route this harness mounts has a matching spec entry. Add an
// entry here (with a reason) only for genuinely non-API surface — e.g. a
// future debug-only route never meant to be part of the public contract.
var allowlistLiveNotInSpec = map[routeKey]string{}

// allowlistSpecNotMountedHere: spec paths this harness does not mount because
// doing so would require live third-party infrastructure this repository's
// own test suite never stands up (a real Stripe/Razorpay account, a live SMTP
// relay, a WebAuthn browser origin, a configured OIDC issuer, a real
// object-storage bucket for presigned URLs). Each of these routes IS live in
// production; a self-host or hosted deploy mounts them via cmd/wpmgr/main.go
// exactly like every other route. Nothing here is a GH #240-class bug — see
// TestBillingRoutes_404WhenUnhosted for the precedent that hosted-only routes
// legitimately 404 on an unhosted build, which is what happens when this
// harness leaves BillingH/PricingH nil too.
var allowlistSpecNotMountedHere = map[routeKey]string{
	{"GET", "/auth/oidc/login"}:    "requires a configured OIDC issuer (OIDCProvider.Enabled()); this harness runs with OIDC disabled, matching a self-host default install",
	{"GET", "/auth/oidc/callback"}: "same as /auth/oidc/login — no OIDC issuer configured in this harness",
}

// ---------------------------------------------------------------------------
// Live-route enumeration
// ---------------------------------------------------------------------------

// ginParamRe matches a Gin path parameter segment (:name) so it can be
// rewritten to OpenAPI's {name} form for comparison against the spec.
var ginParamRe = regexp.MustCompile(`:([A-Za-z0-9_]+)`)

func normalizeGinPath(p string) string {
	return ginParamRe.ReplaceAllString(p, "{$1}")
}

func liveRoutes(t *testing.T, engine *gin.Engine) map[routeKey]bool {
	t.Helper()
	out := map[routeKey]bool{}
	for _, r := range engine.Routes() {
		if r.Method == http.MethodHead || r.Method == http.MethodOptions {
			// OPTIONS/HEAD are transport-level conveniences, not distinct API
			// surface — the RUM preflight OPTIONS route is the one documented
			// exception and is explicitly modeled in the spec, so it still
			// participates in the diff via its own path+OPTIONS entry below.
			if r.Method == http.MethodOptions && strings.HasSuffix(r.Path, "/ingest") {
				out[routeKey{"OPTIONS", normalizeGinPath(r.Path)}] = true
			}
			continue
		}
		out[routeKey{r.Method, normalizeGinPath(r.Path)}] = true
	}
	return out
}

// ---------------------------------------------------------------------------
// Spec-route enumeration (minimal YAML walk — no external OpenAPI library
// dependency; we only need path+method keys, not full schema validation).
// ---------------------------------------------------------------------------

func specRoutes(t *testing.T) map[routeKey]bool {
	t.Helper()
	raw, err := os.ReadFile(specPath(t))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	methods := map[string]bool{
		"get": true, "post": true, "put": true, "patch": true, "delete": true, "options": true,
	}
	out := map[routeKey]bool{}
	for path, ops := range doc.Paths {
		for verb := range ops {
			lower := strings.ToLower(verb)
			if !methods[lower] {
				continue // parameters:, description:, etc. — not an HTTP method
			}
			out[routeKey{strings.ToUpper(lower), path}] = true
		}
	}
	return out
}

func specPath(t *testing.T) string {
	t.Helper()
	// tests/ -> apps/api/ -> apps/ -> repo root -> packages/openapi/openapi.yaml
	return "../../../packages/openapi/openapi.yaml"
}

// ---------------------------------------------------------------------------
// TestOpenAPIRouteCoverage — the structural anti-drift gate.
// ---------------------------------------------------------------------------

func TestOpenAPIRouteCoverage(t *testing.T) {
	pool := startPostgres(t)
	engine := buildFullEngine(t, pool)

	live := liveRoutes(t, engine)
	spec := specRoutes(t)

	var undocumented []string
	for k := range live {
		if spec[k] {
			continue
		}
		if _, ok := allowlistLiveNotInSpec[k]; ok {
			continue
		}
		undocumented = append(undocumented, k.method+" "+k.path)
	}
	sort.Strings(undocumented)

	var stale []string
	for k := range spec {
		if live[k] {
			continue
		}
		if _, ok := allowlistSpecNotMountedHere[k]; ok {
			continue
		}
		stale = append(stale, k.method+" "+k.path)
	}
	sort.Strings(stale)

	if len(undocumented) > 0 {
		t.Errorf("%d live route(s) are NOT documented in packages/openapi/openapi.yaml (add them, or add a justified allowlistLiveNotInSpec entry):\n  %s",
			len(undocumented), strings.Join(undocumented, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("%d documented path+method(s) have NO matching live route (stale spec entry — remove it, mount the handler, or add a justified allowlistSpecNotMountedHere entry):\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// buildFullEngine — wires server.Deps exactly like cmd/wpmgr/main.go's run(),
// against a real Postgres pool, with every genuinely-external integration
// (object storage, SMTP, payment providers, agent signing keys) either
// nil (routes degrade gracefully — Register() only inspects handler-nilness,
// never service-nilness, to decide whether to mount) or a minimal in-process
// fake (a generated Ed25519 keypair for agent/autologin signing, scs.New()'s
// in-memory session store, the tests package's existing billing fakeProvider).
// No HTTP request is ever issued through this engine in this file — only
// engine.Routes() is read — so a handler whose SERVICE is nil is safe to
// mount: nothing invokes its methods.
func buildFullEngine(t *testing.T, pool *db.Pool) *gin.Engine {
	t.Helper()
	logger := slog.Default()
	clock := domain.SystemClock{}
	validator := domain.NewValidator()
	auditRec := audit.NewRecorder(pool, clock)

	// --- auth / sessions / tenants -------------------------------------------
	sessions := auth.NewSessionManagerWithStore(scs.New(), false)
	tenantSvc := tenant.NewService(tenant.NewRepo(pool), validator, clock)
	newTenant := func(ctx context.Context, name, slug string) (uuid.UUID, error) {
		tt, err := tenantSvc.Create(ctx, tenant.CreateInput{Name: name, Slug: slug})
		if err != nil {
			return uuid.Nil, err
		}
		return tt.ID, nil
	}
	authRepo := auth.NewRepo(pool)
	authSvc := auth.NewService(authRepo, auditRec, validator)
	apiKeySvc := apikey.NewService(pool)
	authn := middleware.NewAuthenticator(sessions, authSvc, apiKeySvc, pool)
	oidcProvider, err := auth.NewOIDCProvider(context.Background(), config.OIDCConfig{})
	if err != nil {
		t.Fatalf("NewOIDCProvider (disabled): %v", err)
	}
	authH := auth.NewHandler(authSvc, sessions, oidcProvider, newTenant)

	siteSvc := site.NewService(site.NewRepo(pool), validator, clock)
	siteH := site.NewHandler(siteSvc, auditRec, "")
	siteEventsH := siteevents.NewHandler(pool, siteevents.NewHub())

	// --- agent authentication. resolver/signer are narrow interfaces — nil is
	// safe because this file only ever calls engine.Routes(), never issues a
	// real signed request through the agent group, so no method is invoked. ---
	agentAuthn := agent.NewAuthenticator(nil, clock, 0)
	agentH := agent.NewHandler(siteSvc)

	// --- org / sharing / invitations -----------------------------------------
	orgH := org.NewHandler(pool, tenantCreatorAdapter{newTenant}, sessions, authSvc, auditRec)
	sharingH := sharing.NewHandler(sharing.NewService(pool, authRepo, auditRec, nil, ""))
	invitationSvc := invitation.NewService(pool, authRepo, auditRec, sessions, nil, "")
	invitationH := invitation.NewHandler(invitationSvc)

	// --- destinations / diagnostics / activity / security / login-brand ------
	siteDestH := sitedestination.NewHandler(sitedestination.NewService(sitedestination.NewRepo(pool), nil, logger), auditRec)
	diagSvc := diagnostics.NewService(diagnostics.NewRepo(pool))
	diagnosticsH := diagnostics.NewHandler(diagSvc, auditRec)
	diagnosticsAgentH := agent.NewDiagnosticsHandler(diagSvc)
	errorsAgentH := agent.NewErrorsHandler(diagSvc)
	activitySvc := activity.NewService(activity.NewRepo(pool), nil, nil)
	activityH := activity.NewHandler(activitySvc)
	activityAgentH := agent.NewActivityHandler(activitySvc)
	securitySvc := security.NewService(security.NewRepo(pool))
	securityH := security.NewHandler(securitySvc, auditRec)
	securityAgentH := agent.NewSecurityLoginEventsHandler(securitySvc)
	hibpAgentH := agent.NewHIBPHandler(securitySvc)
	loginBrandH := loginbrand.NewHandler(loginbrand.NewService(loginbrand.NewRepo(pool)), auditRec)

	// --- scans / vulnerabilities ----------------------------------------------
	scanH := scan.NewHandler(scan.NewService(scan.NewRepo(pool), auditRec))
	vulnH := vuln.NewHandler(vuln.NewService(vuln.NewRepo(pool), pool, nil, nil, nil, logger), nil, auditRec)

	// --- updates ---------------------------------------------------------------
	updateHub := update.NewHub()
	updateSvc := update.NewService(update.NewRepo(pool), nil, nil, validator, clock)
	updateH := update.NewHandler(updateSvc, updateHub, auditRec)
	updateAgentH := agent.NewUpdateHandler(nil, nil, 0)

	// --- backups -----------------------------------------------------------
	backupHub := backup.NewHub()
	backupSvc := backup.NewService(backup.NewRepo(pool), nil, nil, nil, clock, backup.Config{})
	backupH := backup.NewHandler(backupSvc, backupHub, auditRec)
	backupAgentH := backup.NewAgentHandler(backupSvc, auditRec)
	restoreRunH := backup.NewRestoreRunHandler(backupSvc)
	scheduleRunH := backup.NewScheduleRunHandler(backupSvc)
	inspectionDeps := backup.InspectionDeps{Logger: logger}

	// --- uptime / autologin ------------------------------------------------
	uptimeSvc := uptime.NewService(uptime.NewRepo(pool), nil, nil)
	uptimeH := uptime.NewHandler(uptimeSvc, auditRec)
	autologinSvc := autologin.NewService(autologin.NewRepo(pool), nil, nil, nil, nil, nil, clock, autologin.Config{})
	autologinH := autologin.NewMintHandler(autologinSvc)
	autologinAgentH := autologin.NewAgentHandler(autologinSvc)

	// --- media optimizer -----------------------------------------------------
	mediaSvc := mediaservice.NewService(mediarepo.NewRepo(pool), nil, nil, auditRec, clock, mediaservice.Config{}, logger)
	mediaH := mediahandler.NewHandler(mediaSvc)
	mediaAgentH := mediahandler.NewAgentHandler(mediaSvc)

	// --- performance suite + object cache --------------------------------------
	perfSvc := perf.NewService(perf.NewRepo(pool), nil, nil, logger)
	perfH := perf.NewHandler(perfSvc, nil, auditRec)
	perfAgentH := perf.NewAgentHandler(perfSvc, nil, nil)
	fontResultsAgentH := perf.NewFontResultsAgentHandler(perf.NewRepo(pool))
	ocH := objectcache.NewHandler(objectcache.NewService(objectcache.NewRepo(pool), nil, nil, nil, nil), auditRec)

	// --- settings / files / screenshots -----------------------------------
	settingsH := settings.NewHandler(settings.NewService(settings.NewRepo(pool), nil, nil, logger), auditRec)
	filesH := files.NewHandler(files.NewService(pool), auditRec)
	screenshotH := screenshot.NewHandler(screenshot.NewService(screenshot.NewRepo(pool), nil, nil, nil), siteGetterAdapter{siteSvc})

	// --- email -----------------------------------------------------------------
	emailSvc := email.NewService(email.NewRepo(pool), nil, logger)
	emailH := email.NewHandler(emailSvc, auditRec)
	emailAgentH := email.NewAgentHandler(emailSvc)
	emailWebhookH := email.NewWebhookHandler(emailSvc, "", logger)
	emailAgentSuppressionH := email.NewAgentSuppressionHandler(emailSvc)

	// --- clients / reports / portal / tags -----------------------------------
	clientSvc := clientpkg.NewService(clientpkg.NewRepo(pool))
	clientH := clientpkg.NewHandler(clientSvc, auditRec)
	// Member/invitation sub-routes live on a separate MemberHandler that
	// clientH.Register only mounts once wired via SetMemberHandler — mirrors
	// cmd/wpmgr/main.go's clientH.SetMemberHandler(clientMemberH) exactly.
	clientMemberH := clientpkg.NewMemberHandler(pool, authRepo, invitationSvc, auditRec, "")
	clientH.SetMemberHandler(clientMemberH)
	reportH := reportpkg.NewHandler(reportpkg.NewService(reportpkg.NewRepo(pool), nil), auditRec)
	portalH := portal.NewHandler(pool, nil, nil, nil, nil, nil)
	siteTagSvc := sitetag.NewService(sitetag.NewRepo(pool))
	siteTagH := sitetag.NewHandler(siteTagSvc, auditRec)

	// --- admin (m33 + vuln-feed) -----------------------------------------------
	adminH := admin.NewHandler(admin.NewService(admin.NewRepo(pool), nil), pool)
	adminH.SetAuditRecorder(auditRec)
	// vuln-feed key-management sub-routes are only mounted once wired via
	// SetVulnFeed — mirrors cmd/wpmgr/main.go's adminH.SetVulnFeed(...) call.
	adminH.SetVulnFeed(nil, admin.NewVulnFeedKeyService(admin.NewInstanceSettingsRepo(pool), nil, "", nil, logger))

	// --- RUM (public) ------------------------------------------------------
	rumH := rum.NewHandlerWithPublisher(rum.NewStorePostgres(pool), rum.NewBeaconKeyRepo(pool), nil, logger)

	// --- billing / pricing (WPMGR_HOSTED path). Mounted with the tests
	// package's existing fake payment provider (billing_webhook_integration_
	// test.go's newTestBillingService/newFakeProvider) so every /billing and
	// /api/v1/pricing route is real, exactly mirroring the hosted-enabled
	// branch of cmd/wpmgr/main.go. ------------------------------------------
	fp := newFakeProvider("fake")
	billingSvc := newTestBillingService(pool, fp)
	billingH := billing.NewHandler(billingSvc, nil, "https://cp.example.test")
	billingWebhookH := billing.NewWebhookHandler(billingSvc, logger)
	pricingSvc := pricing.NewService(billing.NewRegistry(fp), nil, logger)
	pricingH := pricing.NewHandler(pricingSvc)

	deps := server.Deps{
		Config:                 config.Config{},
		Logger:                 logger,
		Pool:                   pool,
		Sessions:               sessions,
		Auth:                   authn,
		AuthH:                  authH,
		MembersH:               auth.NewMembersHandler(authSvc, nil),
		APIKeyH:                apikey.NewHandler(apiKeySvc, auditRec),
		AuditH:                 audit.NewHandler(auditRec),
		TenantH:                tenant.NewHandler(tenantSvc, auditRec),
		SiteH:                  siteH,
		SiteEventsH:            siteEventsH,
		UpdateH:                updateH,
		BackupH:                backupH,
		BackupAgentH:           backupAgentH,
		InspectionDeps:         inspectionDeps,
		UptimeH:                uptimeH,
		AutologinH:             autologinH,
		AutologinAgentH:        autologinAgentH,
		AgentAuth:              agentAuthn,
		AgentH:                 agentH,
		UpdateAgentH:           updateAgentH,
		SiteDestH:              siteDestH,
		SettingsH:              settingsH,
		DiagnosticsH:           diagnosticsH,
		DiagnosticsAgentH:      diagnosticsAgentH,
		ErrorsAgentH:           errorsAgentH,
		ActivityH:              activityH,
		ActivityAgentH:         activityAgentH,
		SecurityH:              securityH,
		SecurityAgentH:         securityAgentH,
		LoginBrandH:            loginBrandH,
		ScanH:                  scanH,
		VulnH:                  vulnH,
		RestoreRunH:            restoreRunH,
		ScheduleRunH:           scheduleRunH,
		OrgH:                   orgH,
		SharingH:               sharingH,
		InvitationH:            invitationH,
		MediaH:                 mediaH,
		MediaAgentH:            mediaAgentH,
		PerfH:                  perfH,
		PerfAgentH:             perfAgentH,
		FontResultsAgentH:      fontResultsAgentH,
		ObjectCacheH:           ocH,
		ScreenshotH:            screenshotH,
		AdminH:                 adminH,
		RumH:                   rumH,
		EmailH:                 emailH,
		FilesH:                 filesH,
		EmailAgentH:            emailAgentH,
		EmailWebhookH:          emailWebhookH,
		EmailAgentSuppressionH: emailAgentSuppressionH,
		HIBPAgentH:             hibpAgentH,
		ClientH:                clientH,
		SiteTagH:               siteTagH,
		ReportH:                reportH,
		PortalH:                portalH,
		BillingH:               billingH,
		BillingWebhookH:        billingWebhookH,
		PricingH:               pricingH,
		ServiceName:            "wpmgr-api-test",
		Version:                "test",
	}

	srv := server.New(deps)
	engine, ok := srv.Handler().(*gin.Engine)
	if !ok {
		t.Fatalf("server.New's Handler() is not a *gin.Engine (got %T)", srv.Handler())
	}
	return engine
}

// tenantCreatorAdapter adapts a (ctx, name, slug) func to org.TenantCreator's
// Create method — mirrors cmd/wpmgr/main.go's orgTenantAdapter.
type tenantCreatorAdapter struct {
	create func(ctx context.Context, name, slug string) (uuid.UUID, error)
}

func (a tenantCreatorAdapter) Create(ctx context.Context, name, slug string) (uuid.UUID, error) {
	return a.create(ctx, name, slug)
}

// siteGetterAdapter adapts *site.Service to screenshot.SiteGetter.
type siteGetterAdapter struct {
	svc *site.Service
}

func (a siteGetterAdapter) GetSiteURL(ctx context.Context, tenantID, siteID uuid.UUID) (string, bool, error) {
	s, err := a.svc.Get(ctx, tenantID, siteID)
	if err != nil {
		return "", false, err
	}
	return s.URL, true, nil
}
