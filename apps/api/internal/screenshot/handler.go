package screenshot

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// SiteGetter resolves a site's URL and enrolled status for the refresh gate.
// A local interface keeps the screenshot package free of the site import cycle.
type SiteGetter interface {
	GetSiteURL(ctx context.Context, tenantID, siteID uuid.UUID) (string, bool, error)
}

// Handler serves the screenshot refresh endpoint.
type Handler struct {
	svc    *Service
	sites  SiteGetter
	now    func() time.Time
}

// NewHandler builds a screenshot Handler.
func NewHandler(svc *Service, sites SiteGetter) *Handler {
	return &Handler{svc: svc, sites: sites, now: time.Now}
}

// Register mounts the screenshot refresh endpoint on the /api/v1 group.
// Called from server.go alongside the other per-site route groups.
func (h *Handler) Register(r *gin.RouterGroup) {
	r.POST("/sites/:siteId/screenshot/refresh",
		authz.RequirePermission(authz.PermSiteRead),
		authz.RequireSiteAccess("siteId"),
		h.refresh,
	)
}

// refresh enqueues a manual screenshot capture for the resolved site.
// POST /api/v1/sites/{siteId}/screenshot/refresh
//
// Returns 202 Accepted on success with the pending screenshot row.
// Returns 404 if the site does not exist in the tenant.
// Returns 409 if the site is not enrolled.
// Returns 501 if the enqueuer is not wired.
func (h *Handler) refresh(c *gin.Context) {
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

	if h.svc.enqueuer == nil {
		httpx.Error(c, domain.Unavailable("screenshot_disabled", "screenshot capture is not configured on this server"))
		return
	}

	siteURL, enrolled, err := h.sites.GetSiteURL(c.Request.Context(), tenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	if !enrolled {
		httpx.Error(c, domain.Conflict("site_not_enrolled", "site is not enrolled with an agent; enroll first to trigger a screenshot"))
		return
	}

	row, err := h.svc.EnqueueCapture(c.Request.Context(), tenantID, siteID, siteURL, ReasonManual)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"status":     row.Status,
		"updated_at": row.UpdatedAt,
	})
}
