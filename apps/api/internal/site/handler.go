package site

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the site HTTP endpoints under /api/v1/sites. The active tenant
// is taken from request context (tenant middleware); a missing tenant yields
// 403 via the service layer.
type Handler struct {
	svc *Service
}

// NewHandler builds a site Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Register mounts the site routes on the given /api/v1 router group.
func (h *Handler) Register(r *gin.RouterGroup) {
	r.POST("/sites", h.create)
	r.GET("/sites", h.list)
	r.GET("/sites/:siteId", h.get)
	r.DELETE("/sites/:siteId", h.delete)
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
	c.JSON(http.StatusCreated, toAPI(s))
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
	c.JSON(http.StatusOK, toAPI(s))
}

func (h *Handler) list(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	ss, err := h.svc.List(c.Request.Context(), ListInput{
		TenantID: tenantID,
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
	c.Status(http.StatusNoContent)
}

func toAPI(s Site) gen.Site {
	u, _ := url.Parse(s.URL)
	if u == nil {
		u = &url.URL{}
	}
	return gen.Site{
		ID:         s.ID,
		TenantID:   s.TenantID,
		URL:        *u,
		Name:       s.Name,
		Status:     gen.SiteStatus(s.Status),
		WpVersion:  s.WPVersion,
		PhpVersion: s.PHPVersion,
		CreatedAt:  s.CreatedAt,
		UpdatedAt:  s.UpdatedAt,
	}
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
