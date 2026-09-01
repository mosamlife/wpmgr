package tenant

import (
	"net/http"
	"strconv"
	"time"

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

	// m130 / ADR-061: the per-tenant assistant kill switch, reachable in one
	// click.
	//
	// WHY authz.PermTenantManage AND NOT A NEW PERMISSION. It is already
	// declared as "manages tenant settings", it is already pinned to RoleOwner
	// (authz/role.go), and it is already in authz.orgLevelPerms — which is the
	// half that matters here. orgLevelPerms membership makes RequirePermission
	// refuse ANY site-constrained principal outright, regardless of the role it
	// holds on a site, so a collaborator shared into one site can never stop
	// the whole organisation's assistant. The kill switch is per-ORGANISATION
	// state on a table with no RLS, so an org-level, owner-only permission is
	// the honest match, and m130 DECISION 1a already argues these columns are
	// the same fact-shape as suspended_at — operator state, not site state. No
	// new permission is invented: the model already said this.
	//
	// TWO ROUTES, NOT ONE TOGGLE. Pause and resume are separate verbs on
	// separate paths. There is deliberately no PUT /assistant taking
	// {"paused": bool}: that shape lets the same click that stopped the surface
	// restart it, which is exactly what an incident control must not permit.
	//
	// NOT REGISTERED: any route writing assistant_enabled_at. See h.pause's
	// doc comment — enabling is a separate gap with a migration in front of it.
	r.GET("/tenants/:tenantId/assistant",
		authz.RequirePermission(authz.PermTenantManage), h.assistantState)
	r.POST("/tenants/:tenantId/assistant/pause",
		authz.RequirePermission(authz.PermTenantManage), h.pauseAssistant)
	r.POST("/tenants/:tenantId/assistant/resume",
		authz.RequirePermission(authz.PermTenantManage), h.resumeAssistant)
}

// assistantResponse is the wire shape for all three assistant routes.
//
// HAND-WRITTEN, NOT ogen. These three routes are not in
// packages/openapi/openapi.yaml yet; adding them there is a contract change
// that must regenerate both consumers in the same commit. This struct is the
// honest interim shape and the field names are the ones the contract should
// take.
type assistantResponse struct {
	TenantID     uuid.UUID  `json:"tenant_id"`
	Enabled      bool       `json:"enabled"`
	EnabledAt    *time.Time `json:"enabled_at"`
	Paused       bool       `json:"paused"`
	PausedAt     *time.Time `json:"paused_at"`
	PausedReason *string    `json:"paused_reason"`
}

func toAssistantAPI(id uuid.UUID, s AssistantState) assistantResponse {
	return assistantResponse{
		TenantID: id,
		// Enabled and Paused are computed from DIFFERENT columns with
		// DIFFERENT null meanings (m130 DECISION 2) and are deliberately not
		// derived from one another. A tenant can be enabled and paused, or
		// disabled and paused, and the console must be able to show that.
		Enabled:      s.EnabledAt != nil,
		EnabledAt:    s.EnabledAt,
		Paused:       s.Paused(),
		PausedAt:     s.PausedAt,
		PausedReason: s.PausedReason,
	}
}

type pauseAssistantRequest struct {
	Reason string `json:"reason"`
}

func (h *Handler) assistantState(c *gin.Context) {
	p, id, ok := h.assistantTarget(c)
	if !ok {
		return
	}
	st, err := h.svc.GetAssistantState(c.Request.Context(), p, id)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toAssistantAPI(id, st))
}

// pauseAssistant is the emergency stop.
//
// ONE CLICK, ONE UPDATE, ONE ROW. There is no row to create first, so this
// cannot fail with a unique violation during an incident (m130 DECISION 1c).
//
// SCOPE NOTE, STATED RATHER THAN LEFT TO BE DISCOVERED: this route writes
// assistant_paused_at only. It does NOT write assistant_enabled_at, and no
// route registered here does. m130 DECISION 5 holds enablement out of the
// `authorized` verdict on purpose and states that wiring it is a two-part
// change whose second half is a NEW migration carrying DECISION 6's backfill.
// Writing enablement from Go without that migration would produce a column
// that records intent and gates nothing — and adding the predicate without the
// backfill refuses every existing connection in the fleet at boot. That is a
// separate gap, it belongs to database-engineer first, and it is deliberately
// not widened into here.
func (h *Handler) pauseAssistant(c *gin.Context) {
	p, id, ok := h.assistantTarget(c)
	if !ok {
		return
	}
	// An absent body is legitimate: "stop it now, I will explain later" is the
	// 3am case, and a control that demands a justification field before it will
	// fire is a control that costs seconds it does not have.
	var req pauseAssistantRequest
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
			return
		}
	}
	st, err := h.svc.PauseAssistant(c.Request.Context(), p, id, PauseInput{Reason: req.Reason}, h.audit)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toAssistantAPI(id, st))
}

func (h *Handler) resumeAssistant(c *gin.Context) {
	p, id, ok := h.assistantTarget(c)
	if !ok {
		return
	}
	st, err := h.svc.ResumeAssistant(c.Request.Context(), p, id, h.audit)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toAssistantAPI(id, st))
}

// assistantTarget pulls the principal and the path tenant id, or writes the
// error response and reports false. The cross-tenant check itself is NOT here
// — it is in the service (assertOwnTenant), so it cannot be skipped by a
// future caller that reaches the service some other way.
func (h *Handler) assistantTarget(c *gin.Context) (domain.Principal, uuid.UUID, bool) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return domain.Principal{}, uuid.Nil, false
	}
	id, err := uuid.Parse(c.Param("tenantId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_tenant_id", "tenantId is not a valid UUID"))
		return domain.Principal{}, uuid.Nil, false
	}
	return p, id, true
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
