package security

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the operator-facing security routes under
// /api/v1/sites/{siteId}/security/...
//
//	GET  /security/login-protection          — get config
//	PUT  /security/login-protection          — save config + push to agent
//	POST /security/unblock-ip               — unblock an IP address
//	GET  /security/login-events             — list login events
type Handler struct {
	svc   *Service
	audit *audit.Recorder
}

// NewHandler builds the operator handler.
func NewHandler(svc *Service, rec *audit.Recorder) *Handler {
	return &Handler{svc: svc, audit: rec}
}

// Register mounts the routes on the authenticated /api/v1 group.
func (h *Handler) Register(r *gin.RouterGroup) {
	// RequireSiteAccess("siteId") is applied on the group so every sub-route
	// inherits it. This enforces the site allowlist for site-scoped principals
	// (belt-and-braces in front of the RLS policy on site_security_config /
	// agent_login_events).
	g := r.Group("/sites/:siteId/security", authz.RequireSiteAccess("siteId"))
	g.GET("/login-protection", authz.RequirePermission(authz.PermSiteRead), h.getConfig)
	g.PUT("/login-protection", authz.RequirePermission(authz.PermSiteWrite), h.putConfig)
	g.POST("/unblock-ip", authz.RequirePermission(authz.PermSiteWrite), h.unblockIP)
	g.GET("/login-events", authz.RequirePermission(authz.PermSiteRead), h.listLoginEvents)
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

// thresholdsDTO is the JSON shape for the thresholds sub-object.
type thresholdsDTO struct {
	CaptchaLimit    int `json:"captcha_limit"`
	TempBlockLimit  int `json:"temp_block_limit"`
	BlockAllLimit   int `json:"block_all_limit"`
	FailedLoginGap  int `json:"failed_login_gap"`
	SuccessLoginGap int `json:"success_login_gap"`
	AllBlockedGap   int `json:"all_blocked_gap"`
}

// securityConfigDTO is the JSON shape for GET and PUT /security/login-protection.
type securityConfigDTO struct {
	Mode       string        `json:"mode"`
	Thresholds thresholdsDTO `json:"thresholds"`
	IPHeader   string        `json:"ip_header"`
	AllowCIDRs []string      `json:"allow_cidrs"`
	DenyCIDRs  []string      `json:"deny_cidrs"`
	UpdatedAt  string        `json:"updated_at,omitempty"`
}

// unblockIPBody is the PUT /security/unblock-ip request body.
type unblockIPBody struct {
	IP string `json:"ip"`
}

// unblockIPResult is the response to POST /security/unblock-ip.
type unblockIPResultDTO struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// loginEventDTO is the JSON shape for one login event.
type loginEventDTO struct {
	ID           int64  `json:"id"`
	AgentEventID int64  `json:"agent_event_id"`
	IP           string `json:"ip"`
	Status       int16  `json:"status"`
	Category     string `json:"category"`
	Username     string `json:"username"`
	RequestID    string `json:"request_id"`
	OccurredAt   string `json:"occurred_at"`
	IngestedAt   string `json:"ingested_at"`
}

type loginEventListDTO struct {
	Items []loginEventDTO `json:"items"`
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func toConfigDTO(cfg SecurityConfig) securityConfigDTO {
	allowCIDRs := cfg.AllowCIDRs
	if allowCIDRs == nil {
		allowCIDRs = []string{}
	}
	denyCIDRs := cfg.DenyCIDRs
	if denyCIDRs == nil {
		denyCIDRs = []string{}
	}
	dto := securityConfigDTO{
		Mode: cfg.Mode,
		Thresholds: thresholdsDTO{
			CaptchaLimit:    cfg.Thresholds.CaptchaLimit,
			TempBlockLimit:  cfg.Thresholds.TempBlockLimit,
			BlockAllLimit:   cfg.Thresholds.BlockAllLimit,
			FailedLoginGap:  cfg.Thresholds.FailedLoginGap,
			SuccessLoginGap: cfg.Thresholds.SuccessLoginGap,
			AllBlockedGap:   cfg.Thresholds.AllBlockedGap,
		},
		IPHeader:   cfg.IPHeader,
		AllowCIDRs: allowCIDRs,
		DenyCIDRs:  denyCIDRs,
	}
	if !cfg.UpdatedAt.IsZero() {
		dto.UpdatedAt = cfg.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return dto
}

func fromConfigDTO(dto securityConfigDTO, tenantID, siteID uuid.UUID) SecurityConfig {
	return SecurityConfig{
		TenantID: tenantID,
		SiteID:   siteID,
		Mode:     dto.Mode,
		Thresholds: agentcmd.SecurityThresholds{
			CaptchaLimit:    dto.Thresholds.CaptchaLimit,
			TempBlockLimit:  dto.Thresholds.TempBlockLimit,
			BlockAllLimit:   dto.Thresholds.BlockAllLimit,
			FailedLoginGap:  dto.Thresholds.FailedLoginGap,
			SuccessLoginGap: dto.Thresholds.SuccessLoginGap,
			AllBlockedGap:   dto.Thresholds.AllBlockedGap,
		},
		IPHeader:   dto.IPHeader,
		AllowCIDRs: dto.AllowCIDRs,
		DenyCIDRs:  dto.DenyCIDRs,
	}
}

// operatorIP extracts the best-effort client IP from the request for the
// protect+empty-allowlist safety rail. X-Forwarded-For first hop takes
// priority; falls back to RemoteAddr (which may include a port).
func operatorIP(c *gin.Context) string {
	if xff := c.GetHeader("X-Forwarded-For"); xff != "" {
		// X-Forwarded-For is a comma-separated list; the leftmost is the
		// client.
		parts := strings.SplitN(xff, ",", 2)
		ip := strings.TrimSpace(parts[0])
		if ip != "" {
			return ip
		}
	}
	return c.Request.RemoteAddr
}

func bindJSON(c *gin.Context, dst any) error {
	dec := json.NewDecoder(c.Request.Body)
	if err := dec.Decode(dst); err != nil {
		return domain.Validation("invalid_body", "request body is not valid JSON: "+err.Error())
	}
	return nil
}

func actorType(p domain.Principal) string {
	if p.Type == domain.PrincipalAPIKey {
		return audit.ActorAPIKey
	}
	return audit.ActorUser
}

// ---------------------------------------------------------------------------
// route handlers
// ---------------------------------------------------------------------------

func (h *Handler) getConfig(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	cfg, err := h.svc.GetConfig(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toConfigDTO(cfg))
}

func (h *Handler) putConfig(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	var body securityConfigDTO
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	// Nil-safe defaults for omitted array fields.
	if body.AllowCIDRs == nil {
		body.AllowCIDRs = []string{}
	}
	if body.DenyCIDRs == nil {
		body.DenyCIDRs = []string{}
	}

	cfg := fromConfigDTO(body, p.TenantID, siteID)
	opIP := operatorIP(c)

	saved, saveErr := h.svc.SaveConfig(c.Request.Context(), p.TenantID, siteID, cfg, opIP)
	if saveErr != nil {
		if _, ok := domain.AsDomain(saveErr); ok {
			httpx.Error(c, saveErr)
			return
		}
		// Non-domain = agent push failure after successful store. Return 200
		// with stored config; surface the push warning in a header.
		c.Header("X-Agent-Push-Warning", saveErr.Error())
		c.JSON(http.StatusOK, toConfigDTO(saved))
		return
	}

	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorType(p),
		ActorID:    p.ActorID(),
		Action:     "site_security_config.update",
		TargetType: "site",
		TargetID:   siteID.String(),
		Metadata: map[string]any{
			"mode":        saved.Mode,
			"allow_count": len(saved.AllowCIDRs),
			"deny_count":  len(saved.DenyCIDRs),
		},
	})

	c.JSON(http.StatusOK, toConfigDTO(saved))
}

func (h *Handler) unblockIP(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	var body unblockIPBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	if body.IP == "" {
		httpx.Error(c, domain.Validation("invalid_ip", "ip is required"))
		return
	}

	ok, detail, err := h.svc.UnblockIP(c.Request.Context(), p.TenantID, siteID, body.IP)
	if err != nil {
		if _, isDomain := domain.AsDomain(err); isDomain {
			httpx.Error(c, err)
			return
		}
		// Agent semantic rejection (ok=false) is surfaced as a 200 with ok=false
		// so the UI can present the agent's detail without treating it as a CP error.
		c.JSON(http.StatusOK, unblockIPResultDTO{OK: false, Detail: err.Error()})
		return
	}

	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorType(p),
		ActorID:    p.ActorID(),
		Action:     "site_security.unblock_ip",
		TargetType: "site",
		TargetID:   siteID.String(),
		Metadata:   map[string]any{"ip": body.IP, "ok": ok, "detail": detail},
	})

	c.JSON(http.StatusOK, unblockIPResultDTO{OK: ok, Detail: detail})
}

func (h *Handler) listLoginEvents(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	limit := 100
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}
	var statusFilter *LoginEventStatus
	if s := c.Query("status"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 16); err == nil {
			st := LoginEventStatus(int16(n))
			statusFilter = &st
		}
	}
	events, err := h.svc.ListLoginEvents(c.Request.Context(), p.TenantID, siteID, limit, statusFilter)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]loginEventDTO, 0, len(events))
	for _, ev := range events {
		dto := loginEventDTO{
			ID:           ev.ID,
			AgentEventID: ev.AgentEventID,
			IP:           ev.IP,
			Status:       int16(ev.Status),
			Category:     ev.Category,
			Username:     ev.Username,
			RequestID:    ev.RequestID,
			IngestedAt:   ev.IngestedAt.UTC().Format(time.RFC3339),
		}
		if !ev.OccurredAt.IsZero() {
			dto.OccurredAt = ev.OccurredAt.UTC().Format(time.RFC3339)
		}
		items = append(items, dto)
	}
	c.JSON(http.StatusOK, loginEventListDTO{Items: items})
}
