package autologin

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agent"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// MintHandler serves the operator-facing mint endpoint mounted under /api/v1.
type MintHandler struct {
	svc *Service
}

// NewMintHandler builds a MintHandler.
func NewMintHandler(svc *Service) *MintHandler { return &MintHandler{svc: svc} }

// Register mounts POST /sites/:siteId/autologin under the /api/v1 group. The
// group is expected to already carry RequireAuth + RequireTenant; this handler
// adds RequirePermission(PermSiteAutologin) and RequireSiteAccess (site allowlist
// guard for site-scoped principals).
func (h *MintHandler) Register(r *gin.RouterGroup) {
	r.POST("/sites/:siteId/autologin", authz.RequirePermission(authz.PermSiteAutologin), authz.RequireSiteAccess("siteId"), h.mint)
}

// mintRequest is the API body for the mint endpoint.
//
//	target_wp_user_login  optional; "" means "agent picks the first admin".
//	redirect_to           optional; appended to the redirect URL as a query
//	                      parameter the agent forwards after session establish.
type mintRequest struct {
	TargetWPUserLogin string `json:"target_wp_user_login"`
	RedirectTo        string `json:"redirect_to"`
}

// mintResponse mirrors the OpenAPI AutologinResponse schema. The minted JWT is
// inside the redirect_url query string and is the only credential — the
// response intentionally does NOT echo the bare JWT (and no logging should
// either; httpx.Error never dumps bodies).
type mintResponse struct {
	RedirectURL string    `json:"redirect_url"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func (h *MintHandler) mint(c *gin.Context) {
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
	var req mintRequest
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
			return
		}
	}
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	if p.Type != domain.PrincipalUser || p.UserID == uuid.Nil {
		// Autologin is intentionally a human-only operation: an API-key principal
		// has no operator user_id to attribute or rate-limit by.
		httpx.Error(c, domain.Forbidden("user_required", "autologin requires an authenticated human user"))
		return
	}

	tok, err := h.svc.Mint(c.Request.Context(), MintRequest{
		TenantID:     tenantID,
		SiteID:       siteID,
		InitiatorID:  p.UserID,
		TargetWPUser: req.TargetWPUserLogin,
		RedirectTo:   req.RedirectTo,
		IP:           c.ClientIP(),
		UserAgent:    c.Request.UserAgent(),
	})
	if err != nil {
		// Rate-limit responses carry a Retry-After header AND a structured field.
		if de, ok := domain.AsDomain(err); ok && de.Kind == domain.KindRateLimited {
			sec := RetryAfterFromError(err)
			c.Header("Retry-After", itoa(sec))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, map[string]any{
				"code":                de.Code,
				"message":             de.Message,
				"retry_after_seconds": sec,
			})
			return
		}
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, mintResponse{
		RedirectURL: tok.RedirectURL,
		ExpiresAt:   tok.ExpiresAt.UTC(),
	})
}

// AgentHandler serves the agent-facing consume callback under /agent/v1. The
// group is already wrapped by the M2 agent Authenticator middleware; the agent
// identity (verified site_id + tenant_id) is read from the context.
type AgentHandler struct {
	svc *Service
}

// NewAgentHandler builds an AgentHandler.
func NewAgentHandler(svc *Service) *AgentHandler { return &AgentHandler{svc: svc} }

// Register mounts POST /autologin/consume on the agent-authenticated group.
func (h *AgentHandler) Register(r *gin.RouterGroup) {
	r.POST("/autologin/consume", h.consume)
}

type consumeRequest struct {
	Nonce          string `json:"nonce"`
	SiteID         string `json:"site_id"`
	ConsumedFromIP string `json:"consumed_from_ip"`
}

type consumeResponse struct {
	OK                bool      `json:"ok"`
	TargetWPUserLogin string    `json:"target_wp_user_login"`
	AllowedWPRoles    []string  `json:"allowed_wp_roles"`
	AuditID           uuid.UUID `json:"audit_id"`
}

func (h *AgentHandler) consume(c *gin.Context) {
	id, ok := agent.IdentityFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("agent_unauthenticated", "agent identity required"))
		return
	}
	var req consumeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	// Parse + validate the body site_id. An empty / malformed value is treated
	// as "no claim" so the service uses the verified identity; a present value
	// MUST equal the agent's verified site_id (the service enforces this).
	var claimed uuid.UUID
	if req.SiteID != "" {
		parsed, err := uuid.Parse(req.SiteID)
		if err != nil {
			httpx.Error(c, domain.Validation("invalid_site_id", "site_id is not a valid UUID"))
			return
		}
		claimed = parsed
	}
	ip := req.ConsumedFromIP
	if ip == "" {
		ip = c.ClientIP()
	}

	res, err := h.svc.Consume(c.Request.Context(), id.SiteID, claimed, req.Nonce, ip)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, consumeResponse{
		OK:                true,
		TargetWPUserLogin: res.TargetWPUser,
		AllowedWPRoles:    res.AllowedRoles,
		AuditID:           res.AuditID,
	})
}

// itoa avoids strconv import noise for a single tiny conversion.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
