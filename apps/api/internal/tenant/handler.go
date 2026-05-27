package tenant

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the tenant HTTP endpoints. Responses use the ogen-generated
// types so the wire shape matches the OpenAPI contract exactly.
type Handler struct {
	svc   *Service
	audit *audit.Recorder
}

// NewHandler builds a tenant Handler.
func NewHandler(svc *Service, rec *audit.Recorder) *Handler {
	return &Handler{svc: svc, audit: rec}
}

// Register mounts the tenant routes on the given router group (/api/v1).
// Tenant management requires owner; reads require viewer+ within a tenant.
func (h *Handler) Register(r *gin.RouterGroup) {
	r.POST("/tenants", authz.RequirePermission(authz.PermTenantManage), h.create)
	r.GET("/tenants", authz.RequirePermission(authz.PermSiteRead), h.list)
	r.GET("/tenants/:tenantId", authz.RequirePermission(authz.PermSiteRead), h.get)
}

type createTenantRequest struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

func (h *Handler) create(c *gin.Context) {
	var req createTenantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	t, err := h.svc.Create(c.Request.Context(), CreateInput{Name: req.Name, Slug: req.Slug})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	// Record against the actor's current active tenant (the new tenant has no
	// chain yet and the actor has no membership in it).
	if p, ok := domain.PrincipalFromContext(c.Request.Context()); ok && p.TenantID != uuid.Nil {
		_, _ = h.audit.Record(c.Request.Context(), audit.Event{
			TenantID:   p.TenantID,
			ActorType:  audit.ActorUser,
			ActorID:    p.ActorID(),
			Action:     audit.ActionTenantCreate,
			TargetType: "tenant",
			TargetID:   t.ID.String(),
			Metadata:   map[string]any{"slug": t.Slug},
		})
	}
	// Pointer so ogen's *Tenant MarshalJSON is used (consistent with list).
	out := toAPI(t)
	c.JSON(http.StatusCreated, &out)
}

func (h *Handler) get(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	id, err := uuid.Parse(c.Param("tenantId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_tenant_id", "tenantId is not a valid UUID"))
		return
	}
	t, err := h.svc.GetForPrincipal(c.Request.Context(), p, id)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	out := toAPI(t)
	c.JSON(http.StatusOK, &out)
}

func (h *Handler) list(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	limit := parseInt32(c.Query("limit"), 50)
	offset := parseInt32(c.Query("offset"), 0)
	ts, err := h.svc.ListForPrincipal(c.Request.Context(), p, ListInput{Limit: limit, Offset: offset})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]gen.Tenant, 0, len(ts))
	for _, t := range ts {
		items = append(items, toAPI(t))
	}
	c.JSON(http.StatusOK, gen.TenantList{Items: items})
}

func toAPI(t Tenant) gen.Tenant {
	return gen.Tenant{
		ID:        t.ID,
		Name:      t.Name,
		Slug:      t.Slug,
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
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
