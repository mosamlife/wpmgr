package uptime

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// seriesBuckets is the target number of downsampled points returned in an uptime
// series (the store rounds the bucket width to whole minutes).
const seriesBuckets = 100

// Handler serves the M5 uptime + alert-config endpoints under /api/v1.
type Handler struct {
	svc   *Service
	audit *audit.Recorder
}

// NewHandler builds an uptime Handler.
func NewHandler(svc *Service, rec *audit.Recorder) *Handler {
	return &Handler{svc: svc, audit: rec}
}

// Register mounts the uptime routes. Uptime reads require viewer+; alert-config
// management requires admin+ (it sets delivery channels + a signing secret).
func (h *Handler) Register(r *gin.RouterGroup) {
	// Per-siteId route: RequireSiteAccess enforces the site allowlist for
	// site-scoped principals (belt-and-braces in front of the RLS policy on
	// site_uptime_probes / site_alert_state).
	r.GET("/sites/:siteId/uptime", authz.RequirePermission(authz.PermSiteRead), authz.RequireSiteAccess("siteId"), h.getUptime)
	// Tenant-wide collection route: site-scoped filtering done by RLS.
	r.GET("/uptime/summary", authz.RequirePermission(authz.PermSiteRead), h.summary)
	// Tenant-level alert-config routes: PermAuditRead is an org-level permission
	// so RequirePermission will already block site-scoped principals.
	r.GET("/alert-config", authz.RequirePermission(authz.PermAuditRead), h.getAlertConfig)
	r.PUT("/alert-config", authz.RequirePermission(authz.PermAuditRead), h.putAlertConfig)
	// Fleet uptime endpoints (no :siteId). Site-scoped principals see only
	// their AllowedSiteIDs (filtered inside the handler). No RequireOrgScope()
	// because site-scoped collaborators get a filtered view, not an error.
	r.GET("/fleet/status", authz.RequirePermission(authz.PermSiteRead), h.fleetStatus)
	// Fleet daily availability strip (GH #460). Same gating as /fleet/status:
	// no :siteId to hand RequireSiteAccess, so the site set is resolved by
	// FleetSiteIDs, which applies the site-scoped principal's allowlist before
	// any site id reaches the metrics store.
	r.GET("/fleet/uptime-history", authz.RequirePermission(authz.PermSiteRead), h.fleetUptimeHistory)
	r.GET("/fleet/incidents", authz.RequirePermission(authz.PermSiteRead), h.fleetIncidents)
	// Incident detail: no :siteId param to gate via RequireSiteAccess, so a
	// site-scoped principal's access is checked explicitly inside the handler
	// (see incidentDetail) once the incident's site is known.
	r.GET("/fleet/incidents/:incidentId", authz.RequirePermission(authz.PermSiteRead), h.incidentDetail)
	// Per-site app-health settings (GH #291 Phase 3 section 3): PermSiteWrite
	// mirrors the floor used for other per-site settings writes (e.g.
	// PUT /sites/:siteId/tags) - this is an operational setting, not a
	// credential/security-sensitive one like autologin.
	r.GET("/sites/:siteId/app-health-settings", authz.RequirePermission(authz.PermSiteWrite), authz.RequireSiteAccess("siteId"), h.getAppHealthSettings)
	r.PUT("/sites/:siteId/app-health-settings", authz.RequirePermission(authz.PermSiteWrite), authz.RequireSiteAccess("siteId"), h.putAppHealthSettings)
}

func windowDuration(w string) (time.Duration, gen.UptimeStatusWindow) {
	switch w {
	case "30d":
		return 30 * 24 * time.Hour, gen.UptimeStatusWindow30d
	case "90d":
		return 90 * 24 * time.Hour, gen.UptimeStatusWindow90d
	default:
		return 7 * 24 * time.Hour, gen.UptimeStatusWindow7d
	}
}

func (h *Handler) getUptime(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	dur, winEnum := windowDuration(c.Query("window"))

	rep, err := h.svc.Uptime(c.Request.Context(), tenantID, siteID, dur, seriesBuckets)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	out := gen.UptimeStatus{
		SiteID:       rep.SiteID,
		Window:       winEnum,
		UptimePct:    rep.UptimePct,
		AvgLatencyMs: rep.AvgLatencyMs,
		Checks:       int64(rep.Checks),
		Up:           rep.Up,
		Series:       make([]gen.UptimePoint, 0, len(rep.Series)),
	}
	if rep.LastCheck != nil {
		out.LastCheck = gen.NewOptDateTime(*rep.LastCheck)
	}
	if rep.TLSExpiry != nil {
		out.TLSExpiry = gen.NewOptDateTime(*rep.TLSExpiry)
	}
	for _, p := range rep.Series {
		out.Series = append(out.Series, gen.UptimePoint{
			Bucket:       p.Bucket,
			Checks:       int64(p.Checks),
			UpChecks:     int64(p.UpChecks),
			AvgLatencyMs: p.AvgLatencyMs,
		})
	}
	c.JSON(http.StatusOK, &out)
}

func (h *Handler) summary(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	items, err := h.svc.Summary(c.Request.Context(), tenantID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	// Summary enumerates ALL sites in the tenant; a site-scoped collaborator
	// must only see their granted sites, so filter to the allowlist here (the
	// per-site /sites/:siteId/uptime route is already RequireSiteAccess-gated).
	if p, ok := domain.PrincipalFromContext(c.Request.Context()); ok && p.Scope == domain.ScopeSite {
		allowed := make([]SummaryItem, 0, len(items))
		for _, it := range items {
			if p.CanAccessSite(it.SiteID) {
				allowed = append(allowed, it)
			}
		}
		items = allowed
	}
	out := gen.UptimeSummary{Items: make([]gen.UptimeSummaryItem, 0, len(items))}
	for _, it := range items {
		gi := gen.UptimeSummaryItem{SiteID: it.SiteID, Up: it.Up}
		if it.Found && it.HTTPStatus > 0 {
			gi.HTTPStatus = gen.NewOptInt32(int32(it.HTTPStatus))
		}
		if it.LastCheck != nil {
			gi.LastCheck = gen.NewOptDateTime(*it.LastCheck)
		}
		if it.TLSExpiry != nil {
			gi.TLSExpiry = gen.NewOptDateTime(*it.TLSExpiry)
		}
		out.Items = append(out.Items, gi)
	}
	c.JSON(http.StatusOK, &out)
}

func (h *Handler) getAlertConfig(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	cfg, err := h.svc.GetAlertConfig(c.Request.Context(), tenantID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, alertConfigToAPI(cfg))
}

func (h *Handler) putAlertConfig(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	var req gen.AlertConfigUpdate
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}

	// Read the existing config so every OMITTED optional field preserves its
	// stored value instead of resetting to a hardcoded default. Before m103
	// this handler only did this for webhook_secret — every other optional
	// field (including notify_security) was unconditionally overwritten with
	// its Go zero value/hardcoded default on every PUT, so e.g. saving a
	// webhook_secret rotation with notify_security omitted silently turned
	// notify_security back off. See mergeAlertConfigUpdate.
	existing, err := h.svc.GetAlertConfig(c.Request.Context(), tenantID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	cfg := mergeAlertConfigUpdate(tenantID, existing, req)

	saved, err := h.svc.SaveAlertConfig(c.Request.Context(), cfg)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, tenantID, map[string]any{
		"email_recipients":       len(saved.EmailRecipients),
		"webhook_configured":     saved.WebhookURL != "",
		"enabled":                saved.Enabled,
		"notify_security":        saved.NotifySecurity,
		"notify_vulns":           saved.NotifyVulns,
		"vuln_min_severity":      saved.VulnMinSeverity,
		"vuln_include_in_digest": saved.VulnIncludeInDigest,
		"app_alerts_enabled":     saved.AppAlertsEnabled,
	})
	c.JSON(http.StatusOK, alertConfigToAPI(saved))
}

// mergeAlertConfigUpdate applies a partial AlertConfigUpdate onto the
// tenant's existing config. Every optional field follows the same
// nil-sentinel "Or(existing.X)" pattern: an omitted field in the request body
// preserves whatever is already stored rather than resetting it to a zero
// value/hardcoded default. Declared as a pure function (no I/O) so the m103
// regression — notify_security being unconditionally dropped on every PUT —
// is directly unit-testable without a database or HTTP round-trip.
func mergeAlertConfigUpdate(tenantID uuid.UUID, existing AlertConfig, req gen.AlertConfigUpdate) AlertConfig {
	recipients := req.EmailRecipients
	if recipients == nil {
		recipients = []string{}
	}
	cfg := AlertConfig{
		TenantID:            tenantID,
		EmailRecipients:     recipients,
		WebhookURL:          req.WebhookURL.Or(existing.WebhookURL),
		WebhookSecret:       existing.WebhookSecret,
		Enabled:             req.Enabled.Or(existing.Enabled),
		NotifySecurity:      req.NotifySecurity.Or(existing.NotifySecurity),
		NotifyVulns:         req.NotifyVulns.Or(existing.NotifyVulns),
		VulnMinSeverity:     string(req.VulnMinSeverity.Or(gen.AlertConfigUpdateVulnMinSeverity(existing.VulnMinSeverity))),
		VulnIncludeInDigest: req.VulnIncludeInDigest.Or(existing.VulnIncludeInDigest),
		AppAlertsEnabled:    req.AppAlertsEnabled.Or(existing.AppAlertsEnabled),
	}
	if req.WebhookSecret.Set {
		cfg.WebhookSecret = req.WebhookSecret.Value
	}
	return cfg
}

// record audits an alert-config change (ActionAlertConfigChanged, targeting
// the tenant's alert_config row).
func (h *Handler) record(c *gin.Context, tenantID uuid.UUID, meta map[string]any) {
	h.recordAction(c, tenantID, ActionAlertConfigChanged, "alert_config", tenantID.String(), meta)
}

// recordAction is the general audit-record helper shared by every uptime
// handler mutation - record (above) is the alert-config-specific caller;
// putAppHealthSettings (GH #291 Phase 3) is the other.
func (h *Handler) recordAction(c *gin.Context, tenantID uuid.UUID, action, targetType, targetID string, meta map[string]any) {
	if h.audit == nil {
		return
	}
	actorType := audit.ActorSystem
	actorID := ""
	if p, ok := domain.PrincipalFromContext(c.Request.Context()); ok {
		actorType = audit.ActorUser
		if p.Type == domain.PrincipalAPIKey {
			actorType = audit.ActorAPIKey
		}
		actorID = p.ActorID()
	}
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   tenantID,
		ActorType:  actorType,
		ActorID:    actorID,
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Metadata:   meta,
	})
}

// ---------------------------------------------------------------------------
// Fleet uptime endpoints
// ---------------------------------------------------------------------------

// fleetStatus handles GET /api/v1/fleet/status.
// Returns summary counts {up, degraded, down, unknown} and per-site status
// items derived from the latest probe result and 7-day aggregates.
// Site-scoped principals see only their AllowedSiteIDs.
func (h *Handler) fleetStatus(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteIDs, err := h.svc.FleetSiteIDs(c.Request.Context(), tenantID, p)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	if len(siteIDs) == 0 {
		c.JSON(http.StatusOK, FleetStatusResponse{
			Summary: FleetStatusCounts{},
			Items:   []FleetStatusItem{},
		})
		return
	}
	resp, err := h.svc.GetFleetStatus(c.Request.Context(), tenantID, siteIDs)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// fleetUptimeHistory handles GET /api/v1/fleet/uptime-history?window=7d|30d|90d
// (GH #460): the per-site daily availability strip, one entry per UTC day,
// with uptime_pct null on any day that has no stored measurement.
//
// It replaces a strip the browser used to derive from a single 7-day scalar.
// The contract that matters is that every non-null cell corresponds to stored
// counters, so an operator exporting this into a client report is forwarding
// measurements rather than an inference.
//
// Site-scoped principals see only their AllowedSiteIDs: FleetSiteIDs applies
// the allowlist, exactly as /fleet/status does, so no site id outside the
// principal's grant is ever passed to the metrics store.
func (h *Handler) fleetUptimeHistory(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	window := c.Query("window")
	if window == "" {
		window = "90d"
	}
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteIDs, err := h.svc.FleetSiteIDs(c.Request.Context(), tenantID, p)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	resp, err := h.svc.GetFleetUptimeHistory(c.Request.Context(), tenantID, siteIDs, window)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// fleetIncidents handles GET /api/v1/fleet/incidents.
// Returns open incidents and incidents that started within the requested
// window, read from the persisted site_incidents table (M94, GH #148).
//
// Query params:
//
//	since — RFC 3339 timestamp; defaults to 7 days ago. Controls the
//	         "recently started" window for closed incidents.
//	limit — max 100, default 100.
func (h *Handler) fleetIncidents(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteIDs, err := h.svc.FleetSiteIDs(c.Request.Context(), tenantID, p)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	since := time.Now().UTC().AddDate(0, 0, -7)
	if s := c.Query("since"); s != "" {
		if t, terr := time.Parse(time.RFC3339, s); terr == nil {
			since = t
		}
	}
	limit := 100
	if s := c.Query("limit"); s != "" {
		if n, nerr := parseInt(s); nerr == nil && n > 0 && n <= 100 {
			limit = n
		}
	}

	if len(siteIDs) == 0 {
		c.JSON(http.StatusOK, gin.H{"items": []FleetIncidentItem{}})
		return
	}
	items, err := h.svc.GetFleetIncidents(c.Request.Context(), tenantID, siteIDs, since, limit)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// incidentDetail handles GET /api/v1/fleet/incidents/:incidentId (M94, GH
// #148 part 1): the incident summary, a probe timeline over its window, and
// its 30-day flapping count. The incident summary is fetched FIRST so a
// site-scoped principal can be denied (404, to avoid leaking existence —
// mirrors RequireSiteAccess's own not-found response) before any metrics-
// store round-trip is spent building the rest of the response.
func (h *Handler) incidentDetail(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	incidentID, err := uuid.Parse(c.Param("incidentId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_incident_id", "incidentId is not a valid UUID"))
		return
	}

	summary, err := h.svc.GetIncidentSummary(c.Request.Context(), tenantID, incidentID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	// Site-scoped collaborators only see incidents for their granted sites
	// (mirrors the summary handler's allowlist filter above); a denied lookup
	// reads as not-found, never a different-error signal that would confirm
	// the incident's existence.
	if p, ok := domain.PrincipalFromContext(c.Request.Context()); ok && p.Scope == domain.ScopeSite {
		if !p.CanAccessSite(summary.SiteID) {
			httpx.Error(c, domain.NotFound("incident_not_found", "incident not found"))
			return
		}
	}

	detail, err := h.svc.GetIncidentDetail(c.Request.Context(), tenantID, summary)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, detail)
}

// ---------------------------------------------------------------------------
// Per-site app-health settings (GH #291 Phase 3 section 3)
// ---------------------------------------------------------------------------

func (h *Handler) getAppHealthSettings(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	settings, err := h.svc.GetAppHealthSettings(c.Request.Context(), tenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, appHealthSettingsToAPI(settings))
}

func (h *Handler) putAppHealthSettings(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	var req gen.AppHealthSettingsUpdate
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	saved, err := h.svc.UpdateAppHealthSettings(c.Request.Context(), tenantID, siteID, req.AppProbePath, req.AppAlertsDisabled)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.recordAction(c, tenantID, ActionAppHealthSettingsChanged, "site", siteID.String(), map[string]any{
		"app_probe_path_set":  saved.AppProbePath != "",
		"app_alerts_disabled": saved.AppAlertsDisabled,
	})
	c.JSON(http.StatusOK, appHealthSettingsToAPI(saved))
}

// appHealthSettingsToAPI maps an AppHealthSettings to its OpenAPI
// representation.
func appHealthSettingsToAPI(s AppHealthSettings) *gen.AppHealthSettings {
	return &gen.AppHealthSettings{
		AppProbePath:      s.AppProbePath,
		AppAlertsDisabled: s.AppAlertsDisabled,
	}
}

// parseInt is a minimal helper for query-param int parsing in handler methods
// that don't have access to the backup package's parseInt32.
func parseInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscan(s, &n)
	return n, err
}

// alertConfigToAPI maps an AlertConfig to its OpenAPI representation. The webhook
// secret is NEVER serialized; webhook_configured surfaces only its presence.
func alertConfigToAPI(cfg AlertConfig) *gen.AlertConfig {
	recipients := cfg.EmailRecipients
	if recipients == nil {
		recipients = []string{}
	}
	out := &gen.AlertConfig{
		EmailRecipients:     recipients,
		WebhookConfigured:   cfg.WebhookURL != "",
		Enabled:             cfg.Enabled,
		NotifySecurity:      cfg.NotifySecurity,
		NotifyVulns:         cfg.NotifyVulns,
		VulnMinSeverity:     gen.AlertConfigVulnMinSeverity(cfg.VulnMinSeverity),
		VulnIncludeInDigest: cfg.VulnIncludeInDigest,
		AppAlertsEnabled:    cfg.AppAlertsEnabled,
	}
	if cfg.WebhookURL != "" {
		out.WebhookURL = gen.NewOptString(cfg.WebhookURL)
	}
	return out
}
