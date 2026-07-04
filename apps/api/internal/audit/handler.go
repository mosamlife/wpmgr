package audit

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/go-faster/jx"
	"github.com/google/uuid"

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
	r.POST("/audit/integrity/rebaseline", authz.RequirePermission(authz.PermAuditManage), h.rebaseline)
}

func (h *Handler) list(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	limit := parseInt32(c.Query("limit"), 50)
	offset := parseInt32(c.Query("offset"), 0)

	// Optional filters: action prefix and/or site_id. When both are absent we
	// fall back to the simpler unfiltered query (no behavioural difference; the
	// filtered path is correct but carries slightly more query overhead).
	actionPrefix := c.Query("action")
	rawSiteID := c.Query("site_id")

	var entries []Entry
	var err error

	if actionPrefix != "" || rawSiteID != "" {
		f := Filter{ActionPrefix: actionPrefix}
		if rawSiteID != "" {
			if id, perr := uuid.Parse(rawSiteID); perr == nil {
				f.SiteID = &id
			}
			// A non-UUID site_id silently ignores the filter (same as an invalid
			// limit falling back to the default) rather than returning a 400, so
			// callers with a bad UUID get an empty-site-filtered unfiltered list
			// rather than an error that might break existing integrations.
		}
		entries, err = h.rec.ListFiltered(c.Request.Context(), p.TenantID, f, limit, offset)
	} else {
		entries, err = h.rec.List(c.Request.Context(), p.TenantID, limit, offset)
	}

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

// rebaselineRequest is the optional body for POST /audit/integrity/rebaseline.
// BrokenAt, when present, is the previously-reported broken-link id this
// re-baseline acknowledges; it is recorded in the acknowledgment event's
// metadata for forensic context only — it is never used to locate or touch
// any row (audit_log stays append-only; nothing is altered or deleted).
type rebaselineRequest struct {
	BrokenAt *uuid.UUID `json:"broken_at"`
}

// rebaseline moves the tenant's audit-integrity anchor to the current chain
// head (authz.PermAuditManage, owner-only). The endpoint is itself audited —
// Rebaseline records an `audit.integrity.rebaselined` entry — and never
// alters or deletes any existing audit_log row. Returns the fresh Verify
// result, which should report ok=true immediately afterward (barring any
// tampering that happened concurrently with this call).
func (h *Handler) rebaseline(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())

	var body rebaselineRequest
	// Body is optional; tolerate an empty/absent body (no broken_at to record).
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&body); err != nil {
			httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
			return
		}
	}

	if _, err := h.rec.Rebaseline(c.Request.Context(), p.TenantID, actorType(p), p.ActorID(), p.UserID, body.BrokenAt); err != nil {
		httpx.Error(c, err)
		return
	}

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
	if e.ActorName != nil {
		out.ActorName = gen.NewOptNilString(*e.ActorName)
	} else {
		out.ActorName.SetToNull()
	}
	if e.ActorEmail != nil {
		out.ActorEmail = gen.NewOptNilString(*e.ActorEmail)
	} else {
		out.ActorEmail.SetToNull()
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

// actorType returns the audit ActorType string for a principal, mirroring the
// same convention used by every other handler's audit.Record call.
func actorType(p domain.Principal) string {
	if p.Type == domain.PrincipalAPIKey {
		return ActorAPIKey
	}
	return ActorUser
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
