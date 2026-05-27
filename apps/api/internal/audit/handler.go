package audit

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/go-faster/jx"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the audit-log endpoints under /api/v1/audit. All routes
// require admin+ (audit:read).
type Handler struct {
	rec *Recorder
}

// NewHandler builds an audit Handler.
func NewHandler(rec *Recorder) *Handler {
	return &Handler{rec: rec}
}

// Register mounts the audit routes with per-route RBAC.
func (h *Handler) Register(r *gin.RouterGroup) {
	r.GET("/audit", authz.RequirePermission(authz.PermAuditRead), h.list)
	r.GET("/audit/verify", authz.RequirePermission(authz.PermAuditRead), h.verify)
}

func (h *Handler) list(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	entries, err := h.rec.List(c.Request.Context(), p.TenantID, parseInt32(c.Query("limit"), 50), parseInt32(c.Query("offset"), 0))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]gen.AuditEntry, 0, len(entries))
	for _, e := range entries {
		items = append(items, toAPI(e))
	}
	c.JSON(http.StatusOK, gen.AuditList{Items: items})
}

func (h *Handler) verify(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	ok, brokenAt, err := h.rec.Verify(c.Request.Context(), p.TenantID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	out := gen.AuditVerify{Ok: ok}
	if !ok {
		out.BrokenAt = gen.NewOptUUID(brokenAt)
	}
	c.JSON(http.StatusOK, &out)
}

func toAPI(e Entry) gen.AuditEntry {
	out := gen.AuditEntry{
		ID:         e.ID,
		TenantID:   e.TenantID,
		ActorType:  e.ActorType,
		ActorID:    e.ActorID,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		PrevHash:   e.PrevHash,
		Hash:       e.Hash,
		CreatedAt:  e.CreatedAt,
	}
	if len(e.Metadata) > 0 {
		md := make(gen.AuditEntryMetadata, len(e.Metadata))
		for k, v := range e.Metadata {
			b, err := json.Marshal(v)
			if err != nil {
				continue
			}
			md[k] = jx.Raw(b)
		}
		out.Metadata = gen.NewOptAuditEntryMetadata(md)
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
	v := int32(n)
	if v < 0 {
		return def
	}
	return v
}
