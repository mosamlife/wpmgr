package aitrust

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/aipolicy"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the AI trust routes.
type Handler struct{ svc *Service }

// NewHandler builds the handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// maxBody bounds a settings body; both are a few dozen bytes.
const maxBody = 4 << 10

// Register mounts, on the authenticated /api/v1 group:
//
//	GET /sites/:siteId/ai/mode                site.content.read, site access
//	PUT /sites/:siteId/ai/mode                site.content.edit, site access, JSON
//	GET /ai/connections/:grantId/usage        apikey:read (organisation-wide)
//	PUT /ai/connections/:grantId/auto         apikey:manage (organisation-wide), JSON
//	GET /ai/activity                          site.content.edit; row security narrows
//
// The service decides who may loosen: a signed-in person only.
func (h *Handler) Register(r *gin.RouterGroup) {
	g := r.Group("/sites/:siteId", authz.RequireSiteAccess("siteId"))
	g.GET("/ai/mode", authz.RequirePermission(authz.PermSiteContentRead), h.getMode)
	g.PUT("/ai/mode", authz.RequirePermission(authz.PermSiteContentEdit), httpx.RequireJSONBody(), h.putMode)
	r.GET("/ai/connections/:grantId/usage", authz.RequirePermission(authz.PermAPIKeyRead), h.getUsage)
	r.PUT("/ai/connections/:grantId/auto",
		authz.RequirePermission(authz.PermAPIKeyManage), httpx.RequireJSONBody(), h.putAuto)
	r.GET("/ai/activity", authz.RequirePermission(authz.PermSiteContentEdit), h.listActivity)
}

func principal(c *gin.Context) (domain.Principal, bool) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
	}
	return p, ok
}

func parseID(c *gin.Context, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_"+name, name+" is not a valid UUID"))
		return uuid.Nil, false
	}
	return id, true
}

// bindBody decodes a small JSON object, refusing unknown fields and trailing
// data.
func bindBody(c *gin.Context, dst any) bool {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBody+1))
	if err != nil || len(raw) > maxBody {
		httpx.Error(c, domain.Validation("invalid_body", "the body is not valid"))
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil || dec.More() {
		httpx.Error(c, domain.Validation("invalid_body", "the body is not valid"))
		return false
	}
	return true
}

func (h *Handler) getMode(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	siteID, ok := parseID(c, "siteId")
	if !ok {
		return
	}
	m, err := h.svc.GetMode(c.Request.Context(), p, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toSiteAIModeDTO(m))
}

func (h *Handler) putMode(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	siteID, ok := parseID(c, "siteId")
	if !ok {
		return
	}
	var body PutSiteAIModeBody
	if !bindBody(c, &body) {
		return
	}
	if body.Version == nil {
		httpx.Error(c, domain.Validation("invalid_version", "version is required"))
		return
	}
	m, err := h.svc.SetMode(c.Request.Context(), p, siteID, aipolicy.Mode(body.Mode), *body.Version)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toSiteAIModeDTO(m))
}

func (h *Handler) getUsage(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	grantID, ok := parseID(c, "grantId")
	if !ok {
		return
	}
	u, err := h.svc.ConnectionUsage(c.Request.Context(), p, grantID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toConnectionUsageDTO(u))
}

func (h *Handler) putAuto(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	grantID, ok := parseID(c, "grantId")
	if !ok {
		return
	}
	var body PutAIConnectionAutoBody
	if !bindBody(c, &body) {
		return
	}
	a, err := h.svc.SetConnectionAuto(c.Request.Context(), p, grantID, aipolicy.ConnectionAuto(body.AIAuto))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toConnectionAutoDTO(a))
}

func (h *Handler) listActivity(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	q := ActivityQuery{Filter: FilterAll, Limit: ActivityDefaultLimit}
	if v, set := c.GetQuery("filter"); set {
		q.Filter = v
	}
	if v, set := c.GetQuery("site_id"); set {
		id, err := uuid.Parse(v)
		if err != nil {
			httpx.Error(c, domain.Validation("invalid_site_id", "site_id is not a valid UUID"))
			return
		}
		q.SiteID = &id
	}
	if v, set := c.GetQuery("grant_id"); set {
		id, err := uuid.Parse(v)
		if err != nil {
			httpx.Error(c, domain.Validation("invalid_grant_id", "grant_id is not a valid UUID"))
			return
		}
		q.GrantID = &id
	}
	if v, set := c.GetQuery("limit"); set {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > ActivityMaxLimit {
			httpx.Error(c, domain.Validation("invalid_limit", "limit must be between 1 and 100"))
			return
		}
		q.Limit = int32(n)
	}
	if v, set := c.GetQuery("cursor"); set {
		cur, ok := DecodeCursor(v)
		if !ok {
			httpx.Error(c, domain.Validation("invalid_cursor", "cursor is not valid"))
			return
		}
		q.After = &cur
	}
	page, err := h.svc.Activity(c.Request.Context(), p, q)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toActivityPageDTO(page))
}
