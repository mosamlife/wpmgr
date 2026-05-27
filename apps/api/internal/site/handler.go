package site

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the site HTTP endpoints under /api/v1/sites plus the public
// /enroll endpoint. The active tenant for authed routes is taken from the
// authenticated principal; /enroll derives the tenant from the pairing code.
type Handler struct {
	svc   *Service
	audit *audit.Recorder
	// cpPublicKey is the control-plane's base64 Ed25519 PUBLIC signing key,
	// returned to agents at enrollment so they can verify CP->agent commands.
	cpPublicKey string
}

// NewHandler builds a site Handler. cpPublicKey is the control plane's base64
// public signing key.
func NewHandler(svc *Service, rec *audit.Recorder, cpPublicKey string) *Handler {
	return &Handler{svc: svc, audit: rec, cpPublicKey: cpPublicKey}
}

func (h *Handler) record(c *gin.Context, tenantID uuid.UUID, action, siteID string, meta map[string]any) {
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
		TargetType: "site",
		TargetID:   siteID,
		Metadata:   meta,
	})
}

// Register mounts the authed site routes on the /api/v1 router group.
func (h *Handler) Register(r *gin.RouterGroup) {
	r.POST("/sites", authz.RequirePermission(authz.PermSiteWrite), h.create)
	r.GET("/sites", authz.RequirePermission(authz.PermSiteRead), h.list)
	r.POST("/sites/pairing-codes", authz.RequirePermission(authz.PermSiteWrite), h.createPairingCode)
	r.GET("/sites/:siteId", authz.RequirePermission(authz.PermSiteRead), h.get)
	r.DELETE("/sites/:siteId", authz.RequirePermission(authz.PermSiteWrite), h.delete)
	r.PUT("/sites/:siteId/tags", authz.RequirePermission(authz.PermSiteWrite), h.setTags)
}

// RegisterPublic mounts the public, unauthenticated /enroll endpoint on the
// root engine. The agent has no session/tenant; the pairing code is the
// authorization.
func (h *Handler) RegisterPublic(r gin.IRouter) {
	r.POST("/enroll", h.enroll)
}

type createSiteRequest struct {
	URL        string `json:"url"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	WPVersion  string `json:"wp_version"`
	PHPVersion string `json:"php_version"`
}

func (h *Handler) create(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	var req createSiteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	s, err := h.svc.Create(c.Request.Context(), CreateInput{
		TenantID:   tenantID,
		URL:        req.URL,
		Name:       req.Name,
		Status:     req.Status,
		WPVersion:  req.WPVersion,
		PHPVersion: req.PHPVersion,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, tenantID, audit.ActionSiteCreate, s.ID.String(), nil)
	out := toAPI(s)
	c.JSON(http.StatusCreated, &out)
}

func (h *Handler) get(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	id, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	s, err := h.svc.Get(c.Request.Context(), tenantID, id)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	out := toAPI(s)
	c.JSON(http.StatusOK, &out)
}

func (h *Handler) list(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	ss, err := h.svc.List(c.Request.Context(), ListInput{
		TenantID: tenantID,
		Tag:      c.Query("tag"),
		Limit:    parseInt32(c.Query("limit"), 50),
		Offset:   parseInt32(c.Query("offset"), 0),
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]gen.Site, 0, len(ss))
	for _, s := range ss {
		items = append(items, toAPI(s))
	}
	c.JSON(http.StatusOK, gen.SiteList{Items: items})
}

func (h *Handler) delete(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	id, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	if err := h.svc.Delete(c.Request.Context(), tenantID, id); err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, tenantID, audit.ActionSiteDelete, id.String(), nil)
	c.Status(http.StatusNoContent)
}

type pairingCodeRequest struct {
	SiteName string   `json:"site_name"`
	Tags     []string `json:"tags"`
}

func (h *Handler) createPairingCode(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	var req pairingCodeRequest
	// Body is optional; tolerate an empty/absent body.
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
			return
		}
	}
	var createdBy uuid.UUID
	if p, ok := domain.PrincipalFromContext(c.Request.Context()); ok {
		createdBy = p.UserID
	}
	created, err := h.svc.CreatePairingCode(c.Request.Context(), CreatePairingCodeInput{
		TenantID:  tenantID,
		CreatedBy: createdBy,
		SiteName:  req.SiteName,
		Tags:      req.Tags,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, tenantID, audit.ActionPairingCodeCreated, "", map[string]any{
		"pairing_code_id": created.Code.ID.String(),
		"expires_at":      created.Code.ExpiresAt,
	})
	out := gen.PairingCode{
		ID:        created.Code.ID,
		TenantID:  created.Code.TenantID,
		Code:      created.Plaintext,
		Tags:      created.Code.Tags,
		ExpiresAt: created.Code.ExpiresAt,
		CreatedAt: created.Code.CreatedAt,
	}
	if created.Code.SiteName != "" {
		out.SiteName = gen.NewOptString(created.Code.SiteName)
	}
	c.JSON(http.StatusCreated, &out)
}

type setTagsRequest struct {
	Tags []string `json:"tags"`
}

func (h *Handler) setTags(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	id, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	var req setTagsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	s, err := h.svc.SetTags(c.Request.Context(), SetTagsInput{TenantID: tenantID, SiteID: id, Tags: req.Tags})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, tenantID, audit.ActionSiteTagsSet, s.ID.String(), map[string]any{"tags": s.Tags})
	out := toAPI(s)
	c.JSON(http.StatusOK, &out)
}

type enrollRequest struct {
	PairingCode    string   `json:"pairing_code"`
	SiteURL        string   `json:"site_url"`
	AgentPublicKey string   `json:"agent_public_key"`
	Name           string   `json:"name"`
	WPVersion      string   `json:"wp_version"`
	PHPVersion     string   `json:"php_version"`
	Tags           []string `json:"tags"`
}

func (h *Handler) enroll(c *gin.Context) {
	var req enrollRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	s, err := h.svc.Enroll(c.Request.Context(), EnrollRequest{
		PairingCode:    req.PairingCode,
		SiteURL:        req.SiteURL,
		AgentPublicKey: req.AgentPublicKey,
		Name:           req.Name,
		WPVersion:      req.WPVersion,
		PHPVersion:     req.PHPVersion,
		Tags:           req.Tags,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, s.TenantID, audit.ActionSiteEnrolled, s.ID.String(), map[string]any{"url": s.URL})
	c.JSON(http.StatusOK, &gen.EnrollResponse{
		SiteID:                s.ID,
		TenantID:              s.TenantID,
		ControlPlanePublicKey: h.cpPublicKey,
	})
}

// toAPI maps a Site to its OpenAPI representation, including the M2 enrollment,
// health, and metadata fields.
func toAPI(s Site) gen.Site {
	u, _ := url.Parse(s.URL)
	if u == nil {
		u = &url.URL{}
	}
	out := gen.Site{
		ID:           s.ID,
		TenantID:     s.TenantID,
		URL:          *u,
		Name:         s.Name,
		Status:       gen.SiteStatus(s.Status),
		WpVersion:    s.WPVersion,
		PhpVersion:   s.PHPVersion,
		HealthStatus: gen.SiteHealthStatus(s.HealthStatus),
		Multisite:    s.Multisite,
		Tags:         s.Tags,
		Enrolled:     gen.NewOptBool(s.EnrolledAt != nil),
		CreatedAt:    s.CreatedAt,
		UpdatedAt:    s.UpdatedAt,
	}
	if s.Tags == nil {
		out.Tags = []string{}
	}
	if s.ServerInfo != "" {
		out.ServerInfo = gen.NewOptString(s.ServerInfo)
	}
	if s.ActiveTheme != "" {
		out.ActiveTheme = gen.NewOptString(s.ActiveTheme)
	}
	if s.EnrolledAt != nil {
		out.EnrolledAt = gen.NewOptDateTime(*s.EnrolledAt)
	}
	if s.LastSeenAt != nil {
		out.LastSeenAt = gen.NewOptDateTime(*s.LastSeenAt)
	}
	if len(s.Components) > 0 {
		var comp struct {
			Plugins []Component `json:"plugins"`
			Themes  []Component `json:"themes"`
		}
		if json.Unmarshal(s.Components, &comp) == nil && (len(comp.Plugins) > 0 || len(comp.Themes) > 0) {
			out.Components = gen.NewOptSiteComponents(gen.SiteComponents{
				Plugins: toAPIComponents(comp.Plugins),
				Themes:  toAPIComponents(comp.Themes),
			})
		}
	}
	return out
}

func toAPIComponents(cs []Component) []gen.SiteComponent {
	out := make([]gen.SiteComponent, 0, len(cs))
	for _, c := range cs {
		gc := gen.SiteComponent{Slug: c.Slug, Active: gen.NewOptBool(c.Active)}
		if c.Name != "" {
			gc.Name = gen.NewOptString(c.Name)
		}
		if c.Version != "" {
			gc.Version = gen.NewOptString(c.Version)
		}
		out = append(out, gc)
	}
	return out
}

func parseInt32(s string, def int32) int32 {
	if s == "" {
		return def
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return def
	}
	return int32(n)
}
