package uptime

import (
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

	// Read the existing config so an omitted webhook_secret preserves the stored
	// one (the secret is write-only and never echoed back, so the client cannot
	// resubmit it).
	existing, err := h.svc.GetAlertConfig(c.Request.Context(), tenantID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	recipients := req.EmailRecipients
	if recipients == nil {
		recipients = []string{}
	}
	cfg := AlertConfig{
		TenantID:        tenantID,
		EmailRecipients: recipients,
		WebhookURL:      req.WebhookURL.Or(""),
		WebhookSecret:   existing.WebhookSecret,
		Enabled:         req.Enabled.Or(true),
	}
	if req.WebhookSecret.Set {
		cfg.WebhookSecret = req.WebhookSecret.Value
	}

	saved, err := h.svc.SaveAlertConfig(c.Request.Context(), cfg)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, tenantID, map[string]any{
		"email_recipients":   len(saved.EmailRecipients),
		"webhook_configured": saved.WebhookURL != "",
		"enabled":            saved.Enabled,
	})
	c.JSON(http.StatusOK, alertConfigToAPI(saved))
}

func (h *Handler) record(c *gin.Context, tenantID uuid.UUID, meta map[string]any) {
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
		Action:     ActionAlertConfigChanged,
		TargetType: "alert_config",
		TargetID:   tenantID.String(),
		Metadata:   meta,
	})
}

// alertConfigToAPI maps an AlertConfig to its OpenAPI representation. The webhook
// secret is NEVER serialized; webhook_configured surfaces only its presence.
func alertConfigToAPI(cfg AlertConfig) *gen.AlertConfig {
	recipients := cfg.EmailRecipients
	if recipients == nil {
		recipients = []string{}
	}
	out := &gen.AlertConfig{
		EmailRecipients:   recipients,
		WebhookConfigured: cfg.WebhookURL != "",
		Enabled:           cfg.Enabled,
	}
	if cfg.WebhookURL != "" {
		out.WebhookURL = gen.NewOptString(cfg.WebhookURL)
	}
	return out
}
