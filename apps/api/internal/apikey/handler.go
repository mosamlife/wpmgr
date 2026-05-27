package apikey

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

// Handler serves API-key management under /api/v1/api-keys. All routes require
// admin+ (apikey:manage / apikey:read).
type Handler struct {
	svc   *Service
	audit *audit.Recorder
}

// NewHandler builds an API-key Handler.
func NewHandler(svc *Service, rec *audit.Recorder) *Handler {
	return &Handler{svc: svc, audit: rec}
}

// Register mounts the API-key routes with per-route RBAC.
func (h *Handler) Register(r *gin.RouterGroup) {
	r.GET("/api-keys", authz.RequirePermission(authz.PermAPIKeyRead), h.list)
	r.POST("/api-keys", authz.RequirePermission(authz.PermAPIKeyManage), h.create)
	r.DELETE("/api-keys/:apiKeyId", authz.RequirePermission(authz.PermAPIKeyManage), h.revoke)
}

type createBody struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

func (h *Handler) create(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	var body createBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	role := authz.Role(body.Role)
	if body.Role == "" {
		role = authz.RoleOperator
	}
	created, err := h.svc.Create(c.Request.Context(), p.TenantID, body.Name, role)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorType(p),
		ActorID:    p.ActorID(),
		Action:     audit.ActionAPIKeyCreate,
		TargetType: "api_key",
		TargetID:   created.Key.ID.String(),
		Metadata:   map[string]any{"name": created.Key.Name, "role": string(created.Key.Role)},
	})
	out := gen.ApiKeyCreated{APIKey: toAPI(created.Key), Token: created.Token}
	c.JSON(http.StatusCreated, &out)
}

func (h *Handler) list(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	keys, err := h.svc.List(c.Request.Context(), p.TenantID, parseInt32(c.Query("limit"), 50), parseInt32(c.Query("offset"), 0))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]gen.ApiKey, 0, len(keys))
	for _, k := range keys {
		items = append(items, toAPI(k))
	}
	c.JSON(http.StatusOK, gen.ApiKeyList{Items: items})
}

func (h *Handler) revoke(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	id, err := uuid.Parse(c.Param("apiKeyId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_api_key_id", "apiKeyId is not a valid UUID"))
		return
	}
	if err := h.svc.Revoke(c.Request.Context(), p.TenantID, id); err != nil {
		httpx.Error(c, err)
		return
	}
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorType(p),
		ActorID:    p.ActorID(),
		Action:     audit.ActionAPIKeyRevoke,
		TargetType: "api_key",
		TargetID:   id.String(),
	})
	c.Status(http.StatusNoContent)
}

func toAPI(k APIKey) gen.ApiKey {
	out := gen.ApiKey{
		ID:        k.ID,
		TenantID:  k.TenantID,
		Name:      k.Name,
		Prefix:    k.Prefix,
		Role:      gen.Role(k.Role),
		CreatedAt: k.CreatedAt,
	}
	if k.LastUsedAt != nil {
		out.LastUsedAt = gen.NewOptDateTime(*k.LastUsedAt)
	}
	if k.RevokedAt != nil {
		out.RevokedAt = gen.NewOptDateTime(*k.RevokedAt)
	}
	return out
}

func actorType(p domain.Principal) string {
	if p.Type == domain.PrincipalAPIKey {
		return audit.ActorAPIKey
	}
	return audit.ActorUser
}

func parseInt32(s string, def int32) int32 {
	if s == "" {
		return def
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return def
	}
	v := int32(n)
	if v < 0 {
		return def
	}
	return v
}
