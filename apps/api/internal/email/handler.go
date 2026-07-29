package email

import (
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the operator-facing per-site email routes under
// /api/v1/sites/{siteId}/email/... and the tenant-level routes at
// /api/v1/email/...
type Handler struct {
	svc        *Service
	audit      *audit.Recorder
	pub        EventPublisher // may be nil; guarded before use
	publicBase string         // e.g. "https://manage.wpmgr.app" for webhook_url
}

// NewHandler constructs the email handler.
func NewHandler(svc *Service, rec *audit.Recorder) *Handler {
	return &Handler{svc: svc, audit: rec}
}

// NewHandlerWithPublisher constructs the email handler wired to the SSE event
// publisher so manual suppression add/delete emits email.suppression_updated.
func NewHandlerWithPublisher(svc *Service, rec *audit.Recorder, pub EventPublisher) *Handler {
	return &Handler{svc: svc, audit: rec, pub: pub}
}

// SetPublisher wires the SSE event publisher after construction. Mirrors
// SetAgentClient — called from main.go after the publisher is available.
func (h *Handler) SetPublisher(pub EventPublisher) {
	h.pub = pub
}

// SetPublicBase sets the public base URL used to construct webhook_url in GET
// responses (e.g. "https://manage.wpmgr.app"). Called from main.go after the
// public base URL is known. If not set, webhook_url will be omitted from responses.
func (h *Handler) SetPublicBase(publicBase string) {
	h.publicBase = publicBase
}

// Register mounts all email routes onto the authenticated /api/v1 group.
//
// Per-site routes (under /sites/:siteId, gated by RequireSiteAccess):
//
//	GET    /sites/:siteId/email/config                — masked config
//	PUT    /sites/:siteId/email/config                — upsert per-site config
//	POST   /sites/:siteId/email/test                  — dispatch send_test_email command
//	POST   /sites/:siteId/email/sync                  — push stored config to agent (explicit sync)
//	GET    /sites/:siteId/email/log                   — keyset-paginated log list
//	GET    /sites/:siteId/email/log/export            — CSV export
//	GET    /sites/:siteId/email/log/:logId            — single log entry detail
//	GET    /sites/:siteId/email/stats                 — per-site email stats
//	POST   /sites/:siteId/email/log/:logId/resend     — single resend (body_stored gate)
//	POST   /sites/:siteId/email/log/resend            — bulk resend (body log_ids[])
//	DELETE /sites/:siteId/email/log                   — bulk delete (log_ids[])
//	GET    /sites/:siteId/email/suppression           — per-site suppression list
//	POST   /sites/:siteId/email/suppression           — manual add
//	DELETE /sites/:siteId/email/suppression/:id       — un-suppress
//
// Org-level routes (no :siteId — RequireAuth+RequireTenant+RequireOrgScope):
//
//	GET  /email/providers              — static provider catalog
//	GET  /email/org-config             — org-wide default config
//	PUT  /email/org-config             — upsert org-wide default config
//	GET  /email/log                    — fleet cross-site log list
//	GET  /email/stats                  — fleet cross-site stats
//	GET  /email/suppression            — fleet suppression list
//	POST /email/suppression            — fleet manual add
//	DELETE /email/suppression/:id      — fleet un-suppress
func (h *Handler) Register(r *gin.RouterGroup) {
	// Per-site group: inherits RequireAuth + RequireTenant from v1, adds site gate.
	g := r.Group("/sites/:siteId", authz.RequireSiteAccess("siteId"))
	g.GET("/email/config", authz.RequirePermission(authz.PermEmailManage), h.getConfig)
	g.PUT("/email/config", authz.RequirePermission(authz.PermEmailManage), h.putConfig)
	// m61: per-site webhook security config (route token rotation, signing key, SES ARNs).
	g.PUT("/email/webhook-config", authz.RequirePermission(authz.PermEmailManage), h.putWebhookConfig)
	// m62 Area 2: named connections CRUD.
	g.GET("/email/connections", authz.RequirePermission(authz.PermEmailManage), h.listConnections)
	g.PUT("/email/connections/:connKey", authz.RequirePermission(authz.PermEmailManage), h.putConnection)
	g.DELETE("/email/connections/:connKey", authz.RequirePermission(authz.PermEmailManage), h.deleteConnection)
	g.POST("/email/test", authz.RequirePermission(authz.PermEmailManage), h.testSend)
	g.POST("/email/sync", authz.RequirePermission(authz.PermEmailManage), h.syncToAgent)
	// Phase 3 — log viewer + stats.
	// NOTE: /email/log/export and /email/log/resend must be registered BEFORE
	// /email/log/:logId so Gin does not parse "export"/"resend" as a :logId UUID.
	g.GET("/email/log", authz.RequirePermission(authz.PermEmailManage), h.listLog)
	g.GET("/email/log/export", authz.RequirePermission(authz.PermEmailManage), h.exportLog)
	// Phase 4a — bulk resend (before :logId to avoid Gin ambiguity).
	g.POST("/email/log/resend", authz.RequirePermission(authz.PermEmailManage), h.bulkResendLog)
	// DELETE /email/log — bulk delete (no :logId; body carries ids).
	g.DELETE("/email/log", authz.RequirePermission(authz.PermEmailManage), h.bulkDeleteLog)
	g.GET("/email/log/:logId", authz.RequirePermission(authz.PermEmailManage), h.getLog)
	// Phase 4a — single resend.
	g.POST("/email/log/:logId/resend", authz.RequirePermission(authz.PermEmailManage), h.resendLog)
	g.GET("/email/stats", authz.RequirePermission(authz.PermEmailManage), h.getSiteStats)
	// Phase 4a — per-site suppression.
	g.GET("/email/suppression", authz.RequirePermission(authz.PermEmailManage), h.listSiteSuppression)
	g.POST("/email/suppression", authz.RequirePermission(authz.PermEmailManage), h.addSuppression)
	g.DELETE("/email/suppression/:suppressionId", authz.RequirePermission(authz.PermEmailManage), h.deleteSuppression)

	// Org-level routes (no :siteId; RequireOrgScope blocks site-collaborators).
	org := r.Group("/email")
	org.Use(authz.RequireOrgScope())
	org.GET("/providers", authz.RequirePermission(authz.PermEmailManage), h.getProviders)
	org.GET("/org-config", authz.RequirePermission(authz.PermEmailManage), h.getOrgConfig)
	org.PUT("/org-config", authz.RequirePermission(authz.PermEmailManage), h.putOrgConfig)
	// m61: org-wide webhook security config.
	org.PUT("/org-config/webhook-config", authz.RequirePermission(authz.PermEmailManage), h.putOrgWebhookConfig)
	// Fleet cross-site log + stats.
	org.GET("/log", authz.RequirePermission(authz.PermEmailManage), h.listFleetLog)
	org.GET("/stats", authz.RequirePermission(authz.PermEmailManage), h.getFleetStats)
	// Phase 4a — fleet suppression.
	org.GET("/suppression", authz.RequirePermission(authz.PermEmailManage), h.listFleetSuppression)
	org.POST("/suppression", authz.RequirePermission(authz.PermEmailManage), h.addFleetSuppression)
	org.DELETE("/suppression/:suppressionId", authz.RequirePermission(authz.PermEmailManage), h.deleteFleetSuppression)
	// m62 Area 4: notify settings.
	org.GET("/notify-settings", authz.RequirePermission(authz.PermEmailManage), h.getNotifySettings)
	org.PUT("/notify-settings", authz.RequirePermission(authz.PermEmailManage), h.putNotifySettings)
	// Deliverability dashboard.
	org.GET("/deliverability", authz.RequirePermission(authz.PermEmailManage), h.getFleetDeliverability)
}

// ---------------------------------------------------------------------------
// per-site handlers
// ---------------------------------------------------------------------------

// getConfig handles GET /sites/:siteId/email/config.
// Returns the per-site config masked read. Falls back to the org-wide default
// when no per-site row exists. Includes secret_set: bool (never the ciphertext).
func (h *Handler) getConfig(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	cfg, err := h.svc.GetConfig(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toConfigDTO(cfg, h.publicBase))
}

// putConfig handles PUT /sites/:siteId/email/config.
// Creates or updates the per-site config. If `secret` is present in the body
// the service age-encrypts it; absent = preserve existing stored ciphertext.
func (h *Handler) putConfig(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	var body putConfigBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	sitePtr := &siteID
	in := fromPutBody(body, p.TenantID, sitePtr)
	saved, err := h.svc.UpsertSiteConfig(c.Request.Context(), in)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, p, audit.ActionEmailConfigUpdated, siteID, map[string]any{
		"provider":   saved.Provider,
		"secret_set": saved.SecretSet,
	})
	c.JSON(http.StatusOK, toConfigDTO(saved, h.publicBase))
}

// testSend handles POST /sites/:siteId/email/test.
// Dispatches the signed send_test_email command to the site's agent.
// Phase 1: the agent does not implement this command yet (Phase 2). The route
// dispatches regardless and surfaces the agent's "unknown command" gracefully.
//
// TODO(phase2-agent): implement the send_test_email command on the agent side.
// Command name: send_test_email
// Request fields: to (string), subject (string), body (string)
// Response: {ok: bool, detail: string}
func (h *Handler) testSend(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	var body testSendBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	if body.To == "" {
		httpx.Error(c, domain.Validation("email_test_missing_to", "to is required"))
		return
	}
	result, err := h.svc.SendTest(c.Request.Context(), p.TenantID, siteID, TestSendInput{
		To:      body.To,
		Subject: body.Subject,
		Body:    body.Body,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": result.OK, "detail": result.Detail})
}

// syncToAgent handles POST /sites/:siteId/email/sync.
// Pushes the stored email config to the site agent without sending a test
// email. Useful after saving a config when the agent was offline at save
// time, or after rotating a secret, so the agent picks up the latest config
// immediately rather than waiting for the next implicit sync.
//
// The response is always 200; ok=false carries the failure detail so the
// frontend can display it without treating it as an error.
func (h *Handler) syncToAgent(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	result, err := h.svc.SyncConfigToAgent(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": result.OK, "detail": result.Detail})
}

// ---------------------------------------------------------------------------
// org-level handlers
// ---------------------------------------------------------------------------

// getProviders handles GET /email/providers.
// Returns the static v1 provider catalog (no DB access needed).
func (h *Handler) getProviders(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"providers": Catalog})
}

// getOrgConfig handles GET /email/org-config.
func (h *Handler) getOrgConfig(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	cfg, err := h.svc.GetOrgConfig(c.Request.Context(), p.TenantID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toConfigDTO(cfg, h.publicBase))
}

// putOrgConfig handles PUT /email/org-config.
func (h *Handler) putOrgConfig(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	var body putConfigBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	in := fromPutBody(body, p.TenantID, nil) // nil siteID = org-wide
	saved, err := h.svc.UpsertOrgConfig(c.Request.Context(), in)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, p, audit.ActionEmailConfigUpdated, uuid.Nil, map[string]any{
		"scope":      "org",
		"provider":   saved.Provider,
		"secret_set": saved.SecretSet,
	})
	c.JSON(http.StatusOK, toConfigDTO(saved, h.publicBase))
}

// ---------------------------------------------------------------------------
// m61 — webhook config handlers
// ---------------------------------------------------------------------------

// putWebhookConfig handles PUT /sites/:siteId/email/webhook-config.
// Rotates the route token, sets the signing key, and/or updates the SES TopicArn
// allowlist for a per-site config row.
func (h *Handler) putWebhookConfig(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	var body webhookConfigBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	// Resolve the config row ID first — SetWebhookFields needs the surrogate PK.
	cfg, err := h.svc.GetConfig(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	result, serr := h.svc.UpsertWebhookConfig(c.Request.Context(), UpsertWebhookInput{
		TenantID:      p.TenantID,
		ConfigID:      cfg.ID,
		RotateToken:   body.RotateToken,
		SigningKeyRaw: body.WebhookSigningKey,
		SesTopicArns:  body.SesTopicArns,
	})
	if serr != nil {
		httpx.Error(c, serr)
		return
	}
	h.record(c, p, audit.ActionEmailConfigUpdated, siteID, map[string]any{
		"webhook_config":  true,
		"rotated_token":   body.RotateToken,
		"signing_key_set": body.WebhookSigningKey != nil,
	})
	c.JSON(http.StatusOK, toConfigDTO(result.Config, h.publicBase))
}

// putOrgWebhookConfig handles PUT /email/org-config/webhook-config.
func (h *Handler) putOrgWebhookConfig(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	var body webhookConfigBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	cfg, err := h.svc.GetOrgConfig(c.Request.Context(), p.TenantID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	result, serr := h.svc.UpsertWebhookConfig(c.Request.Context(), UpsertWebhookInput{
		TenantID:      p.TenantID,
		ConfigID:      cfg.ID,
		RotateToken:   body.RotateToken,
		SigningKeyRaw: body.WebhookSigningKey,
		SesTopicArns:  body.SesTopicArns,
	})
	if serr != nil {
		httpx.Error(c, serr)
		return
	}
	h.record(c, p, audit.ActionEmailConfigUpdated, uuid.Nil, map[string]any{
		"scope":           "org",
		"webhook_config":  true,
		"rotated_token":   body.RotateToken,
		"signing_key_set": body.WebhookSigningKey != nil,
	})
	c.JSON(http.StatusOK, toConfigDTO(result.Config, h.publicBase))
}

// ---------------------------------------------------------------------------
// Phase 3 — per-site log viewer handlers
// ---------------------------------------------------------------------------

// listLog handles GET /sites/:siteId/email/log.
// Returns a keyset-paginated list. Body is never included.
func (h *Handler) listLog(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	f := parseLogListFilter(c)
	page, err := h.svc.ListSiteLog(c.Request.Context(), p.TenantID, siteID, f)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]logEntryDTO, 0, len(page.Entries))
	for _, e := range page.Entries {
		items = append(items, toLogEntryDTO(e, false))
	}
	c.JSON(http.StatusOK, gin.H{
		"entries":     items,
		"next_cursor": page.NextCursor,
	})
}

// getLog handles GET /sites/:siteId/email/log/:logId.
// Returns a single entry with body (when stored) + prev/next navigation.
func (h *Handler) getLog(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	logID, err := uuid.Parse(c.Param("logId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_log_id", "logId is not a valid UUID"))
		return
	}
	detail, serr := h.svc.GetLogEntry(c.Request.Context(), p.TenantID, siteID, logID)
	if serr != nil {
		httpx.Error(c, serr)
		return
	}
	dto := toLogDetailDTO(detail)
	c.JSON(http.StatusOK, dto)
}

// exportLog handles GET /sites/:siteId/email/log/export.
// Streams a CSV (or JSON) of the filtered result set. Body is excluded unless
// the caller explicitly adds ?include_body=1 AND the row has body_stored=true.
func (h *Handler) exportLog(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	f := parseLogListFilter(c)
	// Force a large limit for the export — stream up to 10,000 rows.
	f.Limit = 10000

	page, err := h.svc.ListSiteLog(c.Request.Context(), p.TenantID, siteID, f)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	format := c.DefaultQuery("format", "csv")
	if format == "json" {
		items := make([]logEntryDTO, 0, len(page.Entries))
		for _, e := range page.Entries {
			items = append(items, toLogEntryDTO(e, false))
		}
		c.Header("Content-Disposition", "attachment; filename=email-log.json")
		c.JSON(http.StatusOK, gin.H{"entries": items})
		return
	}

	// Default: CSV.
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", "attachment; filename=email-log.csv")
	c.Status(http.StatusOK)
	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{
		"id", "created_at", "status", "provider",
		"from_address", "to_addresses", "subject",
		"retries", "resent_count", "error",
	})
	for _, e := range page.Entries {
		toStr := ""
		for i, t := range e.ToAddresses {
			if i > 0 {
				toStr += "; "
			}
			toStr += t
		}
		_ = w.Write([]string{
			e.ID.String(),
			e.CreatedAt.UTC().Format(time.RFC3339),
			e.Status,
			e.Provider,
			e.FromAddress,
			toStr,
			e.Subject,
			strconv.Itoa(e.Retries),
			strconv.Itoa(e.ResentCount),
			e.Error,
		})
	}
	w.Flush()
}

// getSiteStats handles GET /sites/:siteId/email/stats?from=&to=.
func (h *Handler) getSiteStats(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	from, to := parseStatRange(c)
	stats, err := h.svc.GetSiteStats(c.Request.Context(), p.TenantID, siteID, from, to)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toEmailStatsDTO(stats))
}

// ---------------------------------------------------------------------------
// Phase 3 — fleet (org-level) log + stats handlers
// ---------------------------------------------------------------------------

// listFleetLog handles GET /email/log (org-scope, no :siteId).
func (h *Handler) listFleetLog(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	f := parseLogListFilter(c)
	page, err := h.svc.ListFleetLog(c.Request.Context(), p.TenantID, f)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]logEntryDTO, 0, len(page.Entries))
	for _, e := range page.Entries {
		items = append(items, toLogEntryDTO(e, false))
	}
	c.JSON(http.StatusOK, gin.H{
		"entries":     items,
		"next_cursor": page.NextCursor,
	})
}

// getFleetStats handles GET /email/stats?from=&to= (org-scope).
func (h *Handler) getFleetStats(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	from, to := parseStatRange(c)
	stats, err := h.svc.GetFleetStats(c.Request.Context(), p.TenantID, from, to)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toEmailStatsDTO(stats))
}

// getFleetDeliverability handles GET /email/deliverability?window=<days> (org-scope).
// Returns per-site deliverability aggregates (bounce/complaint rates + sparklines).
// window defaults to 30, clamped to [1, 365].
func (h *Handler) getFleetDeliverability(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	windowDays := 30
	if s := c.Query("window"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			windowDays = n
		}
	}
	// Clamp: [1, 365].
	if windowDays < 1 {
		windowDays = 1
	}
	if windowDays > 365 {
		windowDays = 365
	}
	report, err := h.svc.GetFleetDelivery(c.Request.Context(), p.TenantID, windowDays)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toDeliverabilityReportDTO(report))
}

// ---------------------------------------------------------------------------
// Phase 4a — resend + bulk delete log handlers
// ---------------------------------------------------------------------------

// resendLog handles POST /sites/:siteId/email/log/:logId/resend.
// Gates on body_stored=true; returns 409 otherwise.
func (h *Handler) resendLog(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	logID, err := uuid.Parse(c.Param("logId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_log_id", "logId is not a valid UUID"))
		return
	}
	result, serr := h.svc.ResendEmail(c.Request.Context(), p.TenantID, siteID, logID)
	if serr != nil {
		httpx.Error(c, serr)
		return
	}
	h.record(c, p, audit.ActionEmailResent, siteID, map[string]any{"log_id": logID.String()})
	c.JSON(http.StatusOK, gin.H{
		"ok":         result.OK,
		"detail":     result.Detail,
		"message_id": result.MessageID,
	})
}

// bulkLogIDsBody is the request body shared by bulk resend and bulk delete.
//
// GH #307: both handlers used to bind only "ids", but the OpenAPI schemas
// (BulkResendRequest, BulkDeleteLogsRequest) declare the field as "log_ids",
// which is what the generated @wpmgr/api client and the web app have always
// sent. The field therefore never bound, the id list was empty, and both
// endpoints returned 200 with a zero count while doing nothing.
//
// "log_ids" is the contract and wins. "ids" stays accepted because it is the
// name the shipped handler read, so a hand-rolled caller written against the
// handler source rather than the spec has been working all along and must not
// break. log_ids takes precedence when both are present.
type bulkLogIDsBody struct {
	LogIDs []string `json:"log_ids"`
	IDs    []string `json:"ids"` // legacy alias, see above
}

func (b bulkLogIDsBody) ids() []string {
	if len(b.LogIDs) > 0 {
		return b.LogIDs
	}
	return b.IDs
}

// errEmptyLogIDs is the rejection for a body that carries no ids at all: `{}`,
// `{"log_ids": []}`, or a legacy `{"ids": []}`.
//
// GH #307 follow-up. log_ids is `required` in both schemas, but nothing
// enforced that server-side: an empty list sailed through to the service,
// deleted nothing, still wrote an audit row, and still answered 200
// {"deleted": 0}. That is the exact "0 log entries deleted" success toast the
// original bug produced, so the symptom stayed reachable for any caller that
// is not the TypeScript client (which blocks it at compile time). A request
// that asks for nothing is a caller mistake, not a successful no-op, and it
// must not leave an audit trail claiming otherwise.
func errEmptyLogIDs() error {
	return domain.Validation("log_ids_required", "log_ids must contain at least one log entry id")
}

// bulkResendLog handles POST /sites/:siteId/email/log/resend.
// Body: {"log_ids": ["uuid", ...]}, the OpenAPI BulkResendRequest schema.
func (h *Handler) bulkResendLog(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	var body bulkLogIDsBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	raw := body.ids()
	if len(raw) == 0 {
		httpx.Error(c, errEmptyLogIDs())
		return
	}
	if len(raw) > MaxBulkResend {
		httpx.Error(c, domain.Validation("resend_bulk_too_large", "bulk resend maximum is 100 entries per request"))
		return
	}
	ids := make([]uuid.UUID, 0, len(raw))
	for _, s := range raw {
		id, err := uuid.Parse(s)
		if err != nil {
			httpx.Error(c, domain.Validation("invalid_log_id", "log_ids contains an invalid UUID: "+s))
			return
		}
		ids = append(ids, id)
	}
	results, serr := h.svc.BulkResendEmail(c.Request.Context(), p.TenantID, siteID, ids)
	if serr != nil {
		httpx.Error(c, serr)
		return
	}
	h.record(c, p, audit.ActionEmailResent, siteID, map[string]any{"count": len(ids)})
	dtos := make([]gin.H, 0, len(results))
	for _, r := range results {
		dtos = append(dtos, gin.H{
			"log_id": r.LogID.String(),
			"ok":     r.OK,
			"detail": r.Detail,
		})
	}
	c.JSON(http.StatusOK, gin.H{"results": dtos})
}

// bulkDeleteLog handles DELETE /sites/:siteId/email/log.
// Body: {"log_ids": ["uuid", ...]}, the OpenAPI BulkDeleteLogsRequest schema.
func (h *Handler) bulkDeleteLog(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	var body bulkLogIDsBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	raw := body.ids()
	if len(raw) == 0 {
		httpx.Error(c, errEmptyLogIDs())
		return
	}
	if len(raw) > MaxBulkDelete {
		httpx.Error(c, domain.Validation("bulk_delete_too_large", "bulk delete maximum is 500 entries per request"))
		return
	}
	ids := make([]uuid.UUID, 0, len(raw))
	for _, s := range raw {
		id, err := uuid.Parse(s)
		if err != nil {
			httpx.Error(c, domain.Validation("invalid_log_id", "log_ids contains an invalid UUID: "+s))
			return
		}
		ids = append(ids, id)
	}
	deleted, serr := h.svc.BulkDeleteLogs(c.Request.Context(), p.TenantID, siteID, ids)
	if serr != nil {
		httpx.Error(c, serr)
		return
	}
	h.record(c, p, audit.ActionEmailLogDeleted, siteID, map[string]any{"deleted": deleted})
	c.JSON(http.StatusOK, gin.H{"deleted": deleted})
}

// ---------------------------------------------------------------------------
// Phase 4a — per-site suppression handlers
// ---------------------------------------------------------------------------

// listSiteSuppression handles GET /sites/:siteId/email/suppression.
func (h *Handler) listSiteSuppression(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	f := parseSuppressionFilter(c)
	page, err := h.svc.ListSiteSuppression(c.Request.Context(), p.TenantID, siteID, f)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]suppressionDTO, 0, len(page.Entries))
	for _, s := range page.Entries {
		items = append(items, toSuppressionDTO(s))
	}
	c.JSON(http.StatusOK, gin.H{"entries": items, "next_cursor": page.NextCursor})
}

// addSuppression handles POST /sites/:siteId/email/suppression.
func (h *Handler) addSuppression(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	var body addSuppressionBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	sitePtr := &siteID
	sup, err := h.svc.AddSuppression(c.Request.Context(), UpsertSuppressionInput{
		TenantID: p.TenantID,
		SiteID:   sitePtr,
		Email:    body.Email,
		Reason:   body.Reason,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, p, audit.ActionEmailSuppressionAdded, siteID, map[string]any{
		"reason": body.Reason,
		"scope":  "site",
	})
	// SSE: notify the email dashboard that a suppression row was manually added.
	publishSuppressionUpdated(c.Request.Context(), h.pub, p.TenantID, sitePtr, maskEmail(body.Email), body.Reason)
	c.JSON(http.StatusCreated, toSuppressionDTO(sup))
}

// deleteSuppression handles DELETE /sites/:siteId/email/suppression/:suppressionId.
func (h *Handler) deleteSuppression(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	supID, err := uuid.Parse(c.Param("suppressionId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_suppression_id", "suppressionId is not a valid UUID"))
		return
	}
	if serr := h.svc.DeleteSuppression(c.Request.Context(), p.TenantID, supID); serr != nil {
		httpx.Error(c, serr)
		return
	}
	h.record(c, p, audit.ActionEmailSuppressionDeleted, siteID, map[string]any{"suppression_id": supID.String()})
	// SSE: notify the email dashboard that a suppression row was manually removed.
	// The email address is not available at delete time (only the ID); emit with
	// empty email — the event signals "list changed, refetch" to the dashboard.
	sitePtr := &siteID
	publishSuppressionUpdated(c.Request.Context(), h.pub, p.TenantID, sitePtr, "", "manual")
	c.Status(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Phase 4a — fleet suppression handlers
// ---------------------------------------------------------------------------

// listFleetSuppression handles GET /email/suppression (org-scope).
func (h *Handler) listFleetSuppression(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	f := parseSuppressionFilter(c)
	page, err := h.svc.ListFleetSuppression(c.Request.Context(), p.TenantID, f)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]suppressionDTO, 0, len(page.Entries))
	for _, s := range page.Entries {
		items = append(items, toSuppressionDTO(s))
	}
	c.JSON(http.StatusOK, gin.H{"entries": items, "next_cursor": page.NextCursor})
}

// addFleetSuppression handles POST /email/suppression (fleet-wide, site_id=nil).
func (h *Handler) addFleetSuppression(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	var body addSuppressionBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	sup, err := h.svc.AddSuppression(c.Request.Context(), UpsertSuppressionInput{
		TenantID: p.TenantID,
		SiteID:   nil, // fleet-wide
		Email:    body.Email,
		Reason:   body.Reason,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, p, audit.ActionEmailSuppressionAdded, uuid.Nil, map[string]any{
		"reason": body.Reason,
		"scope":  "fleet",
	})
	// SSE: fleet-wide suppression row — siteID=nil → SSE SiteID=uuid.Nil →
	// the publisher stores NULL site_id, fanning the event to all active tenant
	// streams regardless of the site currently in view.
	publishSuppressionUpdated(c.Request.Context(), h.pub, p.TenantID, nil, maskEmail(body.Email), body.Reason)
	c.JSON(http.StatusCreated, toSuppressionDTO(sup))
}

// deleteFleetSuppression handles DELETE /email/suppression/:suppressionId (fleet).
func (h *Handler) deleteFleetSuppression(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	supID, err := uuid.Parse(c.Param("suppressionId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_suppression_id", "suppressionId is not a valid UUID"))
		return
	}
	if serr := h.svc.DeleteSuppression(c.Request.Context(), p.TenantID, supID); serr != nil {
		httpx.Error(c, serr)
		return
	}
	h.record(c, p, audit.ActionEmailSuppressionDeleted, uuid.Nil, map[string]any{
		"suppression_id": supID.String(),
		"scope":          "fleet",
	})
	// SSE: fleet-wide suppression row deleted — siteID=nil fans out to all active
	// tenant streams. Empty email: not available at delete time.
	publishSuppressionUpdated(c.Request.Context(), h.pub, p.TenantID, nil, "", "manual")
	c.Status(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// m62 Area 2 — connection CRUD handlers
// ---------------------------------------------------------------------------

// listConnections handles GET /sites/:siteId/email/connections.
// Returns all named connections for the site's email config row.
func (h *Handler) listConnections(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	// Resolve the config row ID.
	cfg, err := h.svc.GetConfig(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	conns, serr := h.svc.ListConnections(c.Request.Context(), p.TenantID, cfg.ID)
	if serr != nil {
		httpx.Error(c, serr)
		return
	}
	items := make([]connectionDTO, 0, len(conns))
	for _, conn := range conns {
		items = append(items, toConnectionDTO(conn))
	}
	c.JSON(http.StatusOK, gin.H{"connections": items})
}

// putConnection handles PUT /sites/:siteId/email/connections/:connKey.
// Creates or updates a named connection.
func (h *Handler) putConnection(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	connKey := c.Param("connKey")

	cfg, err := h.svc.GetConfig(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	var body putConnectionBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}

	conn, serr := h.svc.UpsertConnection(c.Request.Context(), ConnectionUpsertInput{
		TenantID:      p.TenantID,
		ConfigID:      cfg.ID,
		ConnectionKey: connKey,
		Provider:      body.Provider,
		Config:        body.Config,
		SecretRaw:     body.Secret,
		FromAddress:   body.FromAddress,
		FromName:      body.FromName,
	})
	if serr != nil {
		httpx.Error(c, serr)
		return
	}
	h.record(c, p, audit.ActionEmailConfigUpdated, siteID, map[string]any{
		"connection_key": connKey,
		"provider":       body.Provider,
	})
	c.JSON(http.StatusOK, toConnectionDTO(conn))
}

// deleteConnection handles DELETE /sites/:siteId/email/connections/:connKey.
func (h *Handler) deleteConnection(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	connKey := c.Param("connKey")

	cfg, err := h.svc.GetConfig(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	if serr := h.svc.DeleteConnection(c.Request.Context(), p.TenantID, cfg.ID, connKey); serr != nil {
		httpx.Error(c, serr)
		return
	}
	h.record(c, p, audit.ActionEmailConfigUpdated, siteID, map[string]any{
		"connection_key": connKey,
		"deleted":        true,
	})
	c.Status(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// m62 Area 4 — notify settings handlers
// ---------------------------------------------------------------------------

// getNotifySettings handles GET /email/notify-settings.
// Always 200; returns defaults when no row exists.
func (h *Handler) getNotifySettings(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	settings, err := h.svc.GetNotifySettings(c.Request.Context(), p.TenantID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toNotifySettingsDTO(settings))
}

// putNotifySettings handles PUT /email/notify-settings.
func (h *Handler) putNotifySettings(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	var body notifySettingsUpdateBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	settings, err := h.svc.PutNotifySettings(c.Request.Context(), NotifySettingsUpsertInput{
		TenantID:             p.TenantID,
		Enabled:              body.Enabled,
		Recipients:           body.Recipients,
		AlertOnFailure:       body.AlertOnFailure,
		AlertThrottleMinutes: body.AlertThrottleMinutes,
		DigestEnabled:        body.DigestEnabled,
		DigestCadence:        body.DigestCadence,
		DigestDay:            body.DigestDay,
		DigestHour:           body.DigestHour,
		Timezone:             body.Timezone,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, p, audit.ActionEmailConfigUpdated, uuid.Nil, map[string]any{
		"scope":          "notify_settings",
		"enabled":        body.Enabled,
		"digest_enabled": body.DigestEnabled,
	})
	c.JSON(http.StatusOK, toNotifySettingsDTO(settings))
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func parseSiteID(c *gin.Context) (uuid.UUID, bool) {
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return uuid.Nil, false
	}
	return siteID, true
}

func bindJSON(c *gin.Context, dst any) error {
	// Mirror the perf handler: 1 MiB body cap, strict JSON decoder.
	dec := json.NewDecoder(io.LimitReader(c.Request.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		return domain.Validation("invalid_body", "request body is not valid JSON: "+err.Error())
	}
	return nil
}

func (h *Handler) record(c *gin.Context, p domain.Principal, action string, siteID uuid.UUID, meta map[string]any) {
	if h.audit == nil {
		return
	}
	actorType := audit.ActorUser
	if p.Type == domain.PrincipalAPIKey {
		actorType = audit.ActorAPIKey
	}
	targetID := siteID.String()
	targetType := "site"
	if siteID == uuid.Nil {
		targetType = "tenant"
		targetID = p.TenantID.String()
	}
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorType,
		ActorID:    p.ActorID(),
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Metadata:   meta,
	})
}
