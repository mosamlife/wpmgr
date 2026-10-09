package aireadiness

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the AI readiness routes.
type Handler struct{ svc *Service }

// NewHandler builds the handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Register mounts, on the authenticated /api/v1 group:
//
//	GET  /fleet/ai-readiness
//	GET  /sites/:siteId/ai/readiness
//	POST /sites/:siteId/ai/readiness/refresh
//
// The two reads need site:read: the facts are versions and plugin states a
// viewer already sees in the plugin list. The refresh queues work on the
// site's agent, so it needs site.content.refresh, the operator tier of the
// content inventory refresh; a viewer reads the result but cannot ask for it.
// The per-site routes carry RequireSiteAccess. The fleet route has no site id
// to bind, so it is narrowed by the row policies and by the per-site check in
// the service. The refresh takes JSON only (the CSRF guard for a
// cookie-authenticated POST).
func (h *Handler) Register(r *gin.RouterGroup) {
	r.GET("/fleet/ai-readiness", authz.RequirePermission(authz.PermSiteRead), h.fleet)
	g := r.Group("/sites/:siteId", authz.RequireSiteAccess("siteId"))
	g.GET("/ai/readiness", authz.RequirePermission(authz.PermSiteRead), h.get)
	g.POST("/ai/readiness/refresh",
		authz.RequirePermission(authz.PermSiteContentRefresh), httpx.RequireJSONBody(), h.refresh)
}

func principal(c *gin.Context) (domain.Principal, bool) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
	}
	return p, ok
}

func parseSiteID(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return uuid.Nil, false
	}
	return id, true
}

func (h *Handler) get(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	res, err := h.svc.Get(c.Request.Context(), p, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toSiteDTO(res))
}

func (h *Handler) fleet(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	rs, err := h.svc.Fleet(c.Request.Context(), p)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toFleetDTO(rs))
}

func (h *Handler) refresh(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	res, err := h.svc.RequestRefresh(c.Request.Context(), p, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusAccepted, refreshResultDTO{Metadata: res.Metadata, Abilities: res.Abilities})
}
