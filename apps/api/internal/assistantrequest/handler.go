package assistantrequest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the AI request queue and the approve and decline routes.
type Handler struct {
	svc *Service
}

// NewHandler builds the handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Register mounts the routes on the authenticated /api/v1 group:
//
//	GET  /ai/requests
//	GET  /sites/:siteId/ai/requests
//	POST /sites/:siteId/ai/requests/:requestId/approve
//	POST /sites/:siteId/ai/requests/:requestId/decline
//
// Every route needs site.cache.purge. The by-site routes also pass the
// site-access gate, and approve and decline take JSON only, which is the
// CSRF guard for a cookie-authenticated change.
func (h *Handler) Register(r *gin.RouterGroup) {
	r.GET("/ai/requests", authz.RequirePermission(authz.PermSiteCachePurge), h.list)
	g := r.Group("/sites/:siteId", authz.RequireSiteAccess("siteId"))
	g.GET("/ai/requests", authz.RequirePermission(authz.PermSiteCachePurge), h.listForSite)
	g.POST("/ai/requests/:requestId/approve",
		authz.RequirePermission(authz.PermSiteCachePurge), httpx.RequireJSONBody(), h.approve)
	g.POST("/ai/requests/:requestId/decline",
		authz.RequirePermission(authz.PermSiteCachePurge), httpx.RequireJSONBody(), h.decline)
}

func (h *Handler) list(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	limit, offset := pageParams(c)
	q, err := h.svc.List(c.Request.Context(), p, nil, limit, offset)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, listResponse(q, limit, offset))
}

func (h *Handler) listForSite(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	siteID, ok := parseID(c, "siteId")
	if !ok {
		return
	}
	limit, offset := pageParams(c)
	q, err := h.svc.List(c.Request.Context(), p, &siteID, limit, offset)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, listResponse(q, limit, offset))
}

func (h *Handler) approve(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	if err := requireSession(p); err != nil {
		httpx.Error(c, err)
		return
	}
	siteID, ok := parseID(c, "siteId")
	if !ok {
		return
	}
	requestID, ok := parseID(c, "requestId")
	if !ok {
		return
	}
	var body ApproveBody
	if err := json.NewDecoder(io.LimitReader(c.Request.Body, 4<<10)).Decode(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	req, err := h.svc.Approve(c.Request.Context(), p, siteID, requestID, body.PresentedDigest)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toDTO(req))
}

func (h *Handler) decline(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	if err := requireSession(p); err != nil {
		httpx.Error(c, err)
		return
	}
	siteID, ok := parseID(c, "siteId")
	if !ok {
		return
	}
	requestID, ok := parseID(c, "requestId")
	if !ok {
		return
	}
	req, err := h.svc.Decline(c.Request.Context(), p, siteID, requestID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toDTO(req))
}

// RenderCachePurgeRequests renders rows exactly as the request queue returns
// them, for the AI activity feed.
func (h *Handler) RenderCachePurgeRequests(ctx context.Context, p domain.Principal, rows []sqlc.AssistantCachePurgeRequest) ([]any, error) {
	reqs, err := h.svc.Render(ctx, p, rows)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, toDTO(r))
	}
	return out, nil
}

func listResponse(q Queue, limit, offset int32) ListResponse {
	out := ListResponse{Requests: make([]RequestDTO, 0, len(q.Requests)), PendingCount: q.PendingCount, Limit: limit, Offset: offset}
	for _, r := range q.Requests {
		out.Requests = append(out.Requests, toDTO(r))
	}
	return out
}

func parseID(c *gin.Context, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_"+name, name+" is not a valid UUID"))
		return uuid.Nil, false
	}
	return id, true
}

func pageParams(c *gin.Context) (limit, offset int32) {
	limit = 50
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 100 {
			limit = int32(n)
		}
	}
	if s := c.Query("offset"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 && n <= 1_000_000 {
			offset = int32(n)
		}
	}
	return limit, offset
}
