package main

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"sort"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/activity"
	"github.com/mosamlife/wpmgr/apps/api/internal/admin"
	"github.com/mosamlife/wpmgr/apps/api/internal/agent"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentrelease"
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
	"github.com/mosamlife/wpmgr/apps/api/internal/govcontext"
	"github.com/mosamlife/wpmgr/apps/api/internal/invitation"
	"github.com/mosamlife/wpmgr/apps/api/internal/loginbrand"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
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

// ginParamRe matches a Gin path parameter segment (:name) so it can be
// rewritten to OpenAPI's {name} form — same normalisation
// tests/contract/openapi_route_coverage_test.go applies, kept in sync by
// inspection since this command intentionally shares no code with the test
// package (see doc.go: that package must never import a cmd, and a cmd must
// not import a _test.go file).
var ginParamRe = regexp.MustCompile(`:([A-Za-z0-9_]+)`)

func normalizeGinPath(p string) string {
	return ginParamRe.ReplaceAllString(p, "{$1}")
}

// dumpRoutes returns every route mounted on engine, normalised to
// "METHOD\tPATH" form, deduplicated, and sorted for a stable diff. It applies
// NO business logic about which routes matter to an API contract — that
// judgement belongs to whatever consumes this output, not to the dumper.
func dumpRoutes(engine *gin.Engine) []string {
	seen := map[string]bool{}
	var lines []string
	for _, r := range engine.Routes() {
		line := r.Method + "\t" + normalizeGinPath(r.Path)
		if seen[line] {
			continue
		}
		seen[line] = true
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return lines
}

// nilOptionalFields walks server.Deps by reflection and reports the name of
// every nil-able field (pointer, interface, func, map, slice, chan) left nil.
// server.New guards each optional field with "if deps.X != nil" before
// registering its routes, so a nil field here is a silent gap in the dumped
// route surface. buildEngine populates every field it can without live
// third-party infrastructure; whatever this returns is exactly the set of
// routes buildEngine cannot see, and buildEngine's caller is responsible for
// surfacing it (main.go writes each to stderr).
func nilOptionalFields(deps server.Deps) []string {
	v := reflect.ValueOf(deps)
	t := v.Type()
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Ptr, reflect.Interface, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan:
			if f.IsNil() {
				out = append(out, t.Field(i).Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// buildEngine builds the REAL production Gin engine via server.New, with
// EVERY optional server.Deps field populated by a real or minimal-but-non-nil
// handler, so no "if deps.X != nil" guard in server.New can suppress a route
// from engine.Routes(). It is a deliberate near-duplicate of
// tests/contract/openapi_route_coverage_test.go's buildFullEngine — kept
// separate (rather than shared) because that test package must not be
// imported by a cmd, and this command must not import a _test.go file. Where
// the two diverge, it is because this command additionally wires
// MCPTransportH, MCPOAuthH, MCPDiscoveryH and BillingSuspensionGate, which
// buildFullEngine leaves nil: those four are exactly the fields that hid the
// PR #589 OAuth-discovery routes and POST /mcp from the coverage test.
//
// No Postgres connection and no network call is ever made: every service is
// constructed against an empty, unconnected *db.Pool, and this function only
// ever reads engine.Routes() — it never issues a request through the engine.
func buildEngine() (engine *gin.Engine, omittedDepsFields []string, err error) {
	// Force release mode BEFORE any gin.New()/route registration: debug mode
	// makes gin print a "[GIN-debug] METHOD path --> handler" line per route
	// straight to os.Stdout, which would corrupt the "stdout carries ONLY
	// route lines" contract this command promises its callers. server.New
	// only does this itself when deps.Config.IsProduction() is true, which a
	// zero-value config.Config{} (used below) is not.
	gin.SetMode(gin.ReleaseMode)

	pool := &db.Pool{}
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
	oidcProvider := auth.NewOIDCProvider(config.OIDCConfig{})
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

	// --- agent-freshness dashboard (read-only) ---------------------------------
	agentReleaseH := agentrelease.NewHandler(agentrelease.NewService(agentrelease.NewRepo(pool), agentrelease.NewReader(nil, 0)), false)

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
	autologinPolicyH := autologin.NewPolicyHandler(autologinSvc)
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

	// --- ADR-064 S4 governed org/site context ----------------------------------
	govContextRepo := govcontext.NewRepo(pool)
	govContextH := govcontext.NewHandler(govcontext.NewService(govContextRepo, auditRec, &govcontext.Resolver{Store: govContextRepo}))

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
	clientMemberH := clientpkg.NewMemberHandler(pool, authRepo, invitationSvc, auditRec, "")
	clientH.SetMemberHandler(clientMemberH)
	reportH := reportpkg.NewHandler(reportpkg.NewService(reportpkg.NewRepo(pool), nil), auditRec)
	portalH := portal.NewHandler(pool, nil, nil, nil, nil, nil)
	siteTagSvc := sitetag.NewService(sitetag.NewRepo(pool))
	siteTagH := sitetag.NewHandler(siteTagSvc, auditRec)

	// --- admin (m33 + vuln-feed) -----------------------------------------------
	adminH := admin.NewHandler(admin.NewService(admin.NewRepo(pool), nil), pool)
	adminH.SetAuditRecorder(auditRec)
	adminH.SetVulnFeed(nil, admin.NewVulnFeedKeyService(admin.NewInstanceSettingsRepo(pool), nil, "", nil, logger))
	adminH.SetAgentMirror(admin.NewAgentMirrorCheckService(false, false, nil, nil))

	// --- RUM (public) ------------------------------------------------------
	rumH := rum.NewHandlerWithPublisher(rum.NewStorePostgres(pool), rum.NewBeaconKeyRepo(pool), nil, logger)

	// --- billing / pricing (WPMGR_HOSTED path) -------------------------------
	// enabled=true so BillingH/BillingWebhookH/PricingH/BillingSuspensionGate
	// all mount for real, matching a hosted deploy's route set.
	billingSvc := billing.New(pool, nil, true, clock, logger)
	billingH := billing.NewHandler(billingSvc, nil, "https://cp.example.test")
	billingWebhookH := billing.NewWebhookHandler(billingSvc, logger)
	pricingSvc := pricing.NewService(billing.NewRegistry(), nil, logger)
	pricingH := pricing.NewHandler(pricingSvc)

	// --- MCP (S6b/S7): transport + OAuth + discovery -------------------------
	// This is the piece tests/contract/openapi_route_coverage_test.go's
	// buildFullEngine leaves nil. Constructed exactly as
	// cmd/wpmgr/main.go wires it (mcp.NewService(mcp.NewRepo(pool)) etc.), so
	// POST /mcp, the four OAuth paths and the three well-known discovery
	// documents all mount.
	mcpSvc := mcp.NewService(mcp.NewRepo(pool)).WithClock(clock.Now).WithAudit(auditRec).
		WithContextResolver(&govcontext.Resolver{Store: govContextRepo})
	mcpTransportH := mcp.NewTransportHandler(mcpSvc, logger, "dump-routes")
	mcpOAuthH := mcp.NewHandler(mcpSvc)
	mcpDiscoveryH := mcp.NewDiscoveryHandler("https://cp.example.test")

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
		AutologinPolicyH:       autologinPolicyH,
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
		AgentReleaseH:          agentReleaseH,
		RestoreRunH:            restoreRunH,
		ScheduleRunH:           scheduleRunH,
		OrgH:                   orgH,
		SharingH:               sharingH,
		InvitationH:            invitationH,
		GovContextH:            govContextH,
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
		MCPTransportH:          mcpTransportH,
		MCPOAuthH:              mcpOAuthH,
		MCPDiscoveryH:          mcpDiscoveryH,
		BillingSuspensionGate:  billingSvc.SuspensionGate(),
		ServiceName:            "wpmgr-dump-routes",
		Version:                "dump-routes",
	}

	srv := server.New(deps)
	e, ok := srv.Handler().(*gin.Engine)
	if !ok {
		return nil, nil, fmt.Errorf("server.New's Handler() is not a *gin.Engine (got %T)", srv.Handler())
	}

	return e, nilOptionalFields(deps), nil
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
