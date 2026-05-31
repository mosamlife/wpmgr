package loginbrand

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the operator-facing login-brand routes under
// /api/v1/sites/{siteId}/login-brand.
//
//	GET /login-brand — current config (empty strings when no row yet)
//	PUT /login-brand — save + push to agent
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
	// (belt-and-braces in front of the RLS policy on site_login_brand).
	g := r.Group("/sites/:siteId", authz.RequireSiteAccess("siteId"))
	g.GET("/login-brand", authz.RequirePermission(authz.PermSiteRead), h.get)
	g.PUT("/login-brand", authz.RequirePermission(authz.PermSiteWrite), h.put)
}

// loginBrandDTO is the JSON shape for both GET and PUT /login-brand responses.
type loginBrandDTO struct {
	LogoURL   string `json:"logo_url"`
	LogoLink  string `json:"logo_link"`
	Message   string `json:"message"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// loginBrandPutBody is the PUT /login-brand request body.
type loginBrandPutBody struct {
	LogoURL  string `json:"logo_url"`
	LogoLink string `json:"logo_link"`
	Message  string `json:"message"`
}

func (h *Handler) get(c *gin.Context) {
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
	dto := loginBrandDTO{
		LogoURL:  cfg.LogoURL,
		LogoLink: cfg.LogoLink,
		Message:  cfg.Message,
	}
	if !cfg.UpdatedAt.IsZero() {
		dto.UpdatedAt = cfg.UpdatedAt.UTC().Format(time.RFC3339)
	}
	c.JSON(http.StatusOK, dto)
}

func (h *Handler) put(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	var body loginBrandPutBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}

	saved, err := h.svc.SaveConfig(c.Request.Context(), p.TenantID, siteID, LoginBrand{
		LogoURL:  body.LogoURL,
		LogoLink: body.LogoLink,
		Message:  body.Message,
	})
	if err != nil {
		// If the error is "config stored but agent push failed" (non-domain error),
		// return 200 with the stored config and surface the push warning as a header.
		if de, ok := domain.AsDomain(err); ok {
			_ = de
			httpx.Error(c, err)
			return
		}
		// Non-domain error = agent push failure after successful store.
		c.Header("X-Agent-Push-Warning", err.Error())
		dto := loginBrandDTO{
			LogoURL:  saved.LogoURL,
			LogoLink: saved.LogoLink,
			Message:  saved.Message,
		}
		if !saved.UpdatedAt.IsZero() {
			dto.UpdatedAt = saved.UpdatedAt.UTC().Format(time.RFC3339)
		}
		c.JSON(http.StatusOK, dto)
		return
	}

	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorType(p),
		ActorID:    p.ActorID(),
		Action:     "site_login_brand.update",
		TargetType: "site",
		TargetID:   siteID.String(),
		Metadata: map[string]any{
			"has_logo_url":  saved.LogoURL != "",
			"has_logo_link": saved.LogoLink != "",
			"has_message":   saved.Message != "",
		},
	})

	dto := loginBrandDTO{
		LogoURL:  saved.LogoURL,
		LogoLink: saved.LogoLink,
		Message:  saved.Message,
	}
	if !saved.UpdatedAt.IsZero() {
		dto.UpdatedAt = saved.UpdatedAt.UTC().Format(time.RFC3339)
	}
	c.JSON(http.StatusOK, dto)
}

func bindJSON(c *gin.Context, dst any) error {
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return domain.Validation("invalid_body", "request body is not valid JSON")
	}
	return nil
}

func actorType(p domain.Principal) string {
	if p.Type == domain.PrincipalAPIKey {
		return audit.ActorAPIKey
	}
	return audit.ActorUser
}
