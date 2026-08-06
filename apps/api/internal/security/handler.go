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
//	GET  /security/login-protection          — get login-protection config
//	PUT  /security/login-protection          — save config + push to agent
//	POST /security/unblock-ip               — unblock an IP address
//	GET  /security/login-events             — list login events
//	GET  /security/hardening                — get hardening config (ADR-057)
//	PUT  /security/hardening                — save hardening config + push
//	GET  /security/bans                     — list ban entries
//	POST /security/bans                     — create ban entry
//	DELETE /security/bans/:banId            — delete ban entry
//	GET  /security/policy                   — get site-user auth policy (ADR-059)
//	PUT  /security/policy                   — save policy + push to agent
//	GET  /security/policy/groups            — list per-role group overrides
//	PUT  /security/policy/groups/:role      — upsert a per-role group override
//	DELETE /security/policy/groups/:role    — delete a per-role group override
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
	// agent_login_events / site_security_hardening_config / site_security_bans).
	g := r.Group("/sites/:siteId/security", authz.RequireSiteAccess("siteId"))

	// Login-protection (S2).
	g.GET("/login-protection", authz.RequirePermission(authz.PermSiteRead), h.getConfig)
	g.PUT("/login-protection", authz.RequirePermission(authz.PermSiteWrite), h.putConfig)
	g.POST("/unblock-ip", authz.RequirePermission(authz.PermSiteWrite), h.unblockIP)
	g.GET("/login-events", authz.RequirePermission(authz.PermSiteRead), h.listLoginEvents)

	// Hardening config + ban list (ADR-057 Phase 1).
	g.GET("/hardening", authz.RequirePermission(authz.PermSiteRead), h.getHardeningConfig)
	g.PUT("/hardening", authz.RequirePermission(authz.PermSecurityManage), h.putHardeningConfig)
	g.GET("/bans", authz.RequirePermission(authz.PermSiteRead), h.listBans)
	g.POST("/bans", authz.RequirePermission(authz.PermSecurityManage), h.createBan)
	g.DELETE("/bans/:banId", authz.RequirePermission(authz.PermSecurityManage), h.deleteBan)

	// Site-user auth policy (ADR-059 Phase 3).
	g.GET("/policy", authz.RequirePermission(authz.PermSiteRead), h.getPolicy)
	g.PUT("/policy", authz.RequirePermission(authz.PermSecurityManage), h.putPolicy)
	g.GET("/policy/groups", authz.RequirePermission(authz.PermSiteRead), h.listPolicyGroups)
	g.PUT("/policy/groups/:role", authz.RequirePermission(authz.PermSecurityManage), h.putPolicyGroup)
	g.DELETE("/policy/groups/:role", authz.RequirePermission(authz.PermSecurityManage), h.deletePolicyGroup)
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

// ---------------------------------------------------------------------------
// ADR-057 Phase 1 — hardening config DTOs + handlers
// ---------------------------------------------------------------------------

// hardeningConfigDTO is the JSON shape for GET/PUT /security/hardening.
type hardeningConfigDTO struct {
	DisableFileEditor        bool   `json:"disable_file_editor"`
	XMLRPCMode               string `json:"xmlrpc_mode"`
	RestrictRESTAPI          string `json:"restrict_rest_api"`
	RestrictLoginIdentifier  string `json:"restrict_login_identifier"`
	ForceUniqueNickname      bool   `json:"force_unique_nickname"`
	DisableAuthorArchiveEnum bool   `json:"disable_author_archive_enum"`
	ForceSSL                 bool   `json:"force_ssl"`
	DisableDirectoryBrowsing bool   `json:"disable_directory_browsing"`
	DisablePHPInUploads      bool   `json:"disable_php_in_uploads"`
	ProtectSystemFiles       bool   `json:"protect_system_files"`
	UpdatedAt                string `json:"updated_at,omitempty"`
}

func toHardeningDTO(cfg HardeningConfig) hardeningConfigDTO {
	dto := hardeningConfigDTO{
		DisableFileEditor:        cfg.DisableFileEditor,
		XMLRPCMode:               string(cfg.XMLRPCMode),
		RestrictRESTAPI:          string(cfg.RestrictRESTAPI),
		RestrictLoginIdentifier:  string(cfg.RestrictLoginIdentifier),
		ForceUniqueNickname:      cfg.ForceUniqueNickname,
		DisableAuthorArchiveEnum: cfg.DisableAuthorArchiveEnum,
		ForceSSL:                 cfg.ForceSSL,
		DisableDirectoryBrowsing: cfg.DisableDirectoryBrowsing,
		DisablePHPInUploads:      cfg.DisablePHPInUploads,
		ProtectSystemFiles:       cfg.ProtectSystemFiles,
	}
	if !cfg.UpdatedAt.IsZero() {
		dto.UpdatedAt = cfg.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return dto
}

func fromHardeningDTO(dto hardeningConfigDTO, tenantID, siteID uuid.UUID) HardeningConfig {
	return HardeningConfig{
		TenantID:                tenantID,
		SiteID:                  siteID,
		DisableFileEditor:       dto.DisableFileEditor,
		XMLRPCMode:              XMLRPCMode(dto.XMLRPCMode),
		RestrictRESTAPI:         RESTAPIMode(dto.RestrictRESTAPI),
		RestrictLoginIdentifier: LoginIdentifierMode(dto.RestrictLoginIdentifier),
		ForceUniqueNickname:     dto.ForceUniqueNickname,
		DisableAuthorArchiveEnum: dto.DisableAuthorArchiveEnum,
		ForceSSL:                dto.ForceSSL,
		DisableDirectoryBrowsing: dto.DisableDirectoryBrowsing,
		DisablePHPInUploads:     dto.DisablePHPInUploads,
		ProtectSystemFiles:      dto.ProtectSystemFiles,
	}
}

func (h *Handler) getHardeningConfig(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	cfg, err := h.svc.GetHardeningConfig(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toHardeningDTO(cfg))
}

func (h *Handler) putHardeningConfig(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	var body hardeningConfigDTO
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	cfg := fromHardeningDTO(body, p.TenantID, siteID)

	saved, saveErr := h.svc.SaveHardeningConfig(
		c.Request.Context(), p.TenantID, siteID, cfg,
		actorType(p), p.ActorID(),
	)
	if saveErr != nil {
		if _, ok := domain.AsDomain(saveErr); ok {
			httpx.Error(c, saveErr)
			return
		}
		// Non-domain = agent push failed after successful store. Return 200 with
		// stored config; surface the push warning in a header.
		c.Header("X-Agent-Push-Warning", saveErr.Error())
		c.JSON(http.StatusOK, toHardeningDTO(saved))
		return
	}

	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorType(p),
		ActorID:    p.ActorID(),
		Action:     "site_security_hardening.update",
		TargetType: "site",
		TargetID:   siteID.String(),
		Metadata: map[string]any{
			"disable_file_editor":  saved.DisableFileEditor,
			"xmlrpc_mode":          string(saved.XMLRPCMode),
			"restrict_rest_api":    string(saved.RestrictRESTAPI),
			"force_ssl":            saved.ForceSSL,
		},
	})

	c.JSON(http.StatusOK, toHardeningDTO(saved))
}

// ---------------------------------------------------------------------------
// ADR-057 Phase 1 — ban list DTOs + handlers
// ---------------------------------------------------------------------------

// banDTO is the JSON shape for one ban entry (list + create response).
type banDTO struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Value     string `json:"value"`
	Comment   string `json:"comment"`
	ActorType string `json:"actor_type"`
	ActorID   string `json:"actor_id"`
	CreatedAt string `json:"created_at"`
}

// createBanBody is the POST /security/bans request body.
type createBanBody struct {
	Type    string `json:"type"`
	Value   string `json:"value"`
	Comment string `json:"comment"`
}

type banListDTO struct {
	Items []banDTO `json:"items"`
}

func toBanDTO(b Ban) banDTO {
	return banDTO{
		ID:        b.ID.String(),
		Type:      string(b.Type),
		Value:     b.Value,
		Comment:   b.Comment,
		ActorType: b.ActorType,
		ActorID:   b.ActorID,
		CreatedAt: b.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func (h *Handler) listBans(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	bans, err := h.svc.ListBans(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]banDTO, 0, len(bans))
	for _, b := range bans {
		items = append(items, toBanDTO(b))
	}
	c.JSON(http.StatusOK, banListDTO{Items: items})
}

func (h *Handler) createBan(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	var body createBanBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}

	ban := Ban{
		TenantID:  p.TenantID,
		SiteID:    siteID,
		Type:      BanType(strings.TrimSpace(body.Type)),
		Value:     strings.TrimSpace(body.Value),
		Comment:   body.Comment,
		ActorType: actorType(p),
		ActorID:   p.ActorID(),
	}

	saved, err := h.svc.CreateBan(c.Request.Context(), p.TenantID, siteID, ban)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorType(p),
		ActorID:    p.ActorID(),
		Action:     "site_security_ban.create",
		TargetType: "site",
		TargetID:   siteID.String(),
		Metadata:   map[string]any{"ban_id": saved.ID.String(), "type": string(saved.Type), "value": saved.Value},
	})

	c.JSON(http.StatusCreated, toBanDTO(saved))
}

func (h *Handler) deleteBan(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	banID, err := uuid.Parse(c.Param("banId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_ban_id", "banId is not a valid UUID"))
		return
	}

	if err := h.svc.DeleteBan(c.Request.Context(), p.TenantID, siteID, banID); err != nil {
		httpx.Error(c, err)
		return
	}

	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorType(p),
		ActorID:    p.ActorID(),
		Action:     "site_security_ban.delete",
		TargetType: "site",
		TargetID:   siteID.String(),
		Metadata:   map[string]any{"ban_id": banID.String()},
	})

	c.Status(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// ADR-059 Phase 3 — site-user auth policy DTOs + handlers
// ---------------------------------------------------------------------------

// policyDTO is the JSON shape for GET/PUT /security/policy.
//
// GET response example:
//
//	{
//	  "two_factor_enabled": false,
//	  "two_factor_methods": ["totp","email","backup"],
//	  "two_factor_required_roles": [],
//	  "two_factor_grace_logins": 3,
//	  "two_factor_remember_device_days": 30,
//	  "block_xmlrpc_for_2fa_users": true,
//	  "password_min_zxcvbn_score": 0,
//	  "password_min_zxcvbn_roles": [],
//	  "password_block_compromised": false,
//	  "password_reuse_block_count": 0,
//	  "password_max_age_days": 0,
//	  "password_expiry_roles": [],
//	  "hide_backend_enabled": false,
//	  "hide_backend_slug": "",
//	  "hide_backend_redirect": "",
//	  "updated_at": "2026-06-20T00:00:00Z"
//	}
type policyDTO struct {
	TwoFactorEnabled            bool     `json:"two_factor_enabled"`
	TwoFactorMethods            []string `json:"two_factor_methods"`
	TwoFactorRequiredRoles      []string `json:"two_factor_required_roles"`
	TwoFactorGraceLogins        int      `json:"two_factor_grace_logins"`
	TwoFactorRememberDeviceDays int      `json:"two_factor_remember_device_days"`
	BlockXMLRPCFor2FAUsers      bool     `json:"block_xmlrpc_for_2fa_users"`
	PasswordMinZxcvbnScore      int      `json:"password_min_zxcvbn_score"`
	PasswordMinZxcvbnRoles      []string `json:"password_min_zxcvbn_roles"`
	PasswordBlockCompromised    bool     `json:"password_block_compromised"`
	PasswordReuseBlockCount     int      `json:"password_reuse_block_count"`
	PasswordMaxAgeDays          int      `json:"password_max_age_days"`
	PasswordExpiryRoles         []string `json:"password_expiry_roles"`
	HideBackendEnabled          bool     `json:"hide_backend_enabled"`
	HideBackendSlug             string   `json:"hide_backend_slug"`
	HideBackendRedirect         string   `json:"hide_backend_redirect"`
	UpdatedAt                   string   `json:"updated_at,omitempty"`
}

// siteRoleDTO is one WordPress role that exists on the site.
//
//	slug — what the policy stores and what the agent enforces against.
//	name — what the site itself displays for that role, in the site's own
//	       locale (an Italian site reports "Amministratore" for administrator).
//
// Both are always present; name falls back to slug when the site reported none.
type siteRoleDTO struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// policyWithRolesDTO is the GET /security/policy response: the stored policy
// plus the site's real WordPress role registry (GH #350).
//
// site_roles is READ-ONLY discovery data and is deliberately NOT part of the
// PUT request body: the write path must stay a pure policy write, and the agent
// is the only writer of the role registry.
//
// An EMPTY site_roles array means the site has not reported its roles (an agent
// older than 0.61.127, or one that has not pushed metadata yet). The dashboard
// renders the default WordPress roles in that case WITH a visible notice, since
// silently offering only those five is the defect this field exists to fix.
type policyWithRolesDTO struct {
	policyDTO
	SiteRoles []siteRoleDTO `json:"site_roles"`
}

func toPolicyWithRolesDTO(p SiteSecurityPolicy, roles []SiteRole) policyWithRolesDTO {
	items := make([]siteRoleDTO, 0, len(roles))
	for _, r := range roles {
		if r.Slug == "" {
			continue
		}
		name := r.Name
		if name == "" {
			name = r.Slug
		}
		items = append(items, siteRoleDTO{Slug: r.Slug, Name: name})
	}
	return policyWithRolesDTO{policyDTO: toPolicyDTO(p), SiteRoles: items}
}

func toPolicyDTO(p SiteSecurityPolicy) policyDTO {
	dto := policyDTO{
		TwoFactorEnabled:            p.TwoFactorEnabled,
		TwoFactorMethods:            coalesceStringSliceDTO(p.TwoFactorMethods),
		TwoFactorRequiredRoles:      coalesceStringSliceDTO(p.TwoFactorRequiredRoles),
		TwoFactorGraceLogins:        p.TwoFactorGraceLogins,
		TwoFactorRememberDeviceDays: p.TwoFactorRememberDeviceDays,
		BlockXMLRPCFor2FAUsers:      p.BlockXMLRPCFor2FAUsers,
		PasswordMinZxcvbnScore:      p.PasswordMinZxcvbnScore,
		PasswordMinZxcvbnRoles:      coalesceStringSliceDTO(p.PasswordMinZxcvbnRoles),
		PasswordBlockCompromised:    p.PasswordBlockCompromised,
		PasswordReuseBlockCount:     p.PasswordReuseBlockCount,
		PasswordMaxAgeDays:          p.PasswordMaxAgeDays,
		PasswordExpiryRoles:         coalesceStringSliceDTO(p.PasswordExpiryRoles),
		HideBackendEnabled:          p.HideBackendEnabled,
		HideBackendSlug:             p.HideBackendSlug,
		HideBackendRedirect:         p.HideBackendRedirect,
	}
	if !p.UpdatedAt.IsZero() {
		dto.UpdatedAt = p.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return dto
}

func fromPolicyDTO(dto policyDTO, tenantID, siteID uuid.UUID) SiteSecurityPolicy {
	return SiteSecurityPolicy{
		TenantID:                    tenantID,
		SiteID:                      siteID,
		TwoFactorEnabled:            dto.TwoFactorEnabled,
		TwoFactorMethods:            coalesceStringSliceDTO(dto.TwoFactorMethods),
		TwoFactorRequiredRoles:      coalesceStringSliceDTO(dto.TwoFactorRequiredRoles),
		TwoFactorGraceLogins:        dto.TwoFactorGraceLogins,
		TwoFactorRememberDeviceDays: dto.TwoFactorRememberDeviceDays,
		BlockXMLRPCFor2FAUsers:      dto.BlockXMLRPCFor2FAUsers,
		PasswordMinZxcvbnScore:      dto.PasswordMinZxcvbnScore,
		PasswordMinZxcvbnRoles:      coalesceStringSliceDTO(dto.PasswordMinZxcvbnRoles),
		PasswordBlockCompromised:    dto.PasswordBlockCompromised,
		PasswordReuseBlockCount:     dto.PasswordReuseBlockCount,
		PasswordMaxAgeDays:          dto.PasswordMaxAgeDays,
		PasswordExpiryRoles:         coalesceStringSliceDTO(dto.PasswordExpiryRoles),
		HideBackendEnabled:          dto.HideBackendEnabled,
		HideBackendSlug:             dto.HideBackendSlug,
		HideBackendRedirect:         dto.HideBackendRedirect,
	}
}

// coalesceStringSliceDTO returns an empty (non-nil) slice for nil JSON arrays
// so the response always serialises as [] not null.
func coalesceStringSliceDTO(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// policyGroupDTO is the JSON shape for one per-role group override.
//
// GET /security/policy/groups response shape:
//
//	{
//	  "items": [
//	    {
//	      "role": "administrator",
//	      "require_2fa": true,
//	      "allowed_methods": ["totp","backup"],
//	      "min_zxcvbn_score": 3,
//	      "block_compromised": true,
//	      "max_age_days": 90,
//	      "created_at": "2026-06-20T00:00:00Z"
//	    }
//	  ]
//	}
type policyGroupDTO struct {
	Role             string   `json:"role"`
	Require2FA       *bool    `json:"require_2fa,omitempty"`
	AllowedMethods   []string `json:"allowed_methods,omitempty"`
	MinZxcvbnScore   *int     `json:"min_zxcvbn_score,omitempty"`
	BlockCompromised *bool    `json:"block_compromised,omitempty"`
	MaxAgeDays       *int     `json:"max_age_days,omitempty"`
	CreatedAt        string   `json:"created_at,omitempty"`
}

type policyGroupListDTO struct {
	Items []policyGroupDTO `json:"items"`
}

func toPolicyGroupDTO(g PolicyGroup) policyGroupDTO {
	dto := policyGroupDTO{
		Role:             g.Role,
		Require2FA:       g.Require2FA,
		AllowedMethods:   g.AllowedMethods,
		MinZxcvbnScore:   g.MinZxcvbnScore,
		BlockCompromised: g.BlockCompromised,
		MaxAgeDays:       g.MaxAgeDays,
	}
	if !g.CreatedAt.IsZero() {
		dto.CreatedAt = g.CreatedAt.UTC().Format(time.RFC3339)
	}
	return dto
}

func (h *Handler) getPolicy(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	pol, err := h.svc.GetSiteSecurityPolicy(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	// The site's real role registry rides along on the policy read so the
	// dashboard knows which roles it may offer BEFORE the operator has saved
	// anything. Never fails the read: an unavailable registry yields an empty
	// list, which the dashboard labels rather than papers over.
	roles := h.svc.GetSiteRoles(c.Request.Context(), p.TenantID, siteID)
	c.JSON(http.StatusOK, toPolicyWithRolesDTO(pol, roles))
}

func (h *Handler) putPolicy(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	var body policyDTO
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}

	pol := fromPolicyDTO(body, p.TenantID, siteID)
	saved, saveErr := h.svc.SaveSiteSecurityPolicy(
		c.Request.Context(), p.TenantID, siteID, pol,
		actorType(p), p.ActorID(),
	)
	if saveErr != nil {
		if _, ok := domain.AsDomain(saveErr); ok {
			// A rejected policy returns no body, so there is nothing for the
			// role registry to ride on. Bail before looking it up.
			httpx.Error(c, saveErr)
			return
		}
		// Non-domain = agent push failed after successful store.
		c.Header("X-Agent-Push-Warning", saveErr.Error())
		c.JSON(http.StatusOK, toPolicyWithRolesDTO(saved, h.svc.GetSiteRoles(c.Request.Context(), p.TenantID, siteID)))
		return
	}

	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorType(p),
		ActorID:    p.ActorID(),
		Action:     "site_security_policy.update",
		TargetType: "site",
		TargetID:   siteID.String(),
		Metadata: map[string]any{
			"two_factor_enabled":       saved.TwoFactorEnabled,
			"password_block_compromised": saved.PasswordBlockCompromised,
			"hide_backend_enabled":     saved.HideBackendEnabled,
		},
	})

	// The save response carries site_roles too. The dashboard writes this body
	// straight into its policy cache, so omitting the registry here would make
	// the role chips collapse back to the five defaults the moment an operator
	// pressed Save.
	c.JSON(http.StatusOK, toPolicyWithRolesDTO(saved, h.svc.GetSiteRoles(c.Request.Context(), p.TenantID, siteID)))
}

func (h *Handler) listPolicyGroups(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	groups, err := h.svc.GetPolicyGroups(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]policyGroupDTO, 0, len(groups))
	for _, g := range groups {
		items = append(items, toPolicyGroupDTO(g))
	}
	c.JSON(http.StatusOK, policyGroupListDTO{Items: items})
}

func (h *Handler) putPolicyGroup(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	role := strings.TrimSpace(c.Param("role"))
	if role == "" {
		httpx.Error(c, domain.Validation("invalid_role", "role path parameter is required"))
		return
	}

	var body policyGroupDTO
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	// The role in the body must match the path parameter (or be empty — the
	// path param wins).
	body.Role = role

	g := PolicyGroup{
		TenantID:         p.TenantID,
		SiteID:           siteID,
		Role:             role,
		Require2FA:       body.Require2FA,
		AllowedMethods:   body.AllowedMethods,
		MinZxcvbnScore:   body.MinZxcvbnScore,
		BlockCompromised: body.BlockCompromised,
		MaxAgeDays:       body.MaxAgeDays,
	}

	saved, saveErr := h.svc.UpsertPolicyGroup(c.Request.Context(), p.TenantID, siteID, g)
	if saveErr != nil {
		if _, ok := domain.AsDomain(saveErr); ok {
			httpx.Error(c, saveErr)
			return
		}
		c.Header("X-Agent-Push-Warning", saveErr.Error())
		c.JSON(http.StatusOK, toPolicyGroupDTO(saved))
		return
	}

	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorType(p),
		ActorID:    p.ActorID(),
		Action:     "site_security_policy_group.upsert",
		TargetType: "site",
		TargetID:   siteID.String(),
		Metadata:   map[string]any{"role": role},
	})

	c.JSON(http.StatusOK, toPolicyGroupDTO(saved))
}

func (h *Handler) deletePolicyGroup(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	role := strings.TrimSpace(c.Param("role"))
	if role == "" {
		httpx.Error(c, domain.Validation("invalid_role", "role path parameter is required"))
		return
	}

	if err := h.svc.DeletePolicyGroup(c.Request.Context(), p.TenantID, siteID, role); err != nil {
		httpx.Error(c, err)
		return
	}

	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorType(p),
		ActorID:    p.ActorID(),
		Action:     "site_security_policy_group.delete",
		TargetType: "site",
		TargetID:   siteID.String(),
		Metadata:   map[string]any{"role": role},
	})

	c.Status(http.StatusNoContent)
}
