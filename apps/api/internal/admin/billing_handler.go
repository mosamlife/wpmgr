package admin

// billing_handler.go — M16 Phase C1 superadmin billing-admin panel HTTP
// handlers. Routes are mounted in handler.go's Register (already gated by
// requireSuperadmin). Hand-rolled Gin JSON, matching every other admin
// handler in this package (no ogen/OpenAPI involvement) — see billing_dto.go
// for the documented response shapes.

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// splitCSV splits a comma-separated query param into a trimmed, non-empty
// slice. Returns nil for an empty input.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func queryBool(c *gin.Context, name string) bool {
	v := c.Query(name)
	return v == "1" || strings.EqualFold(v, "true")
}

func (h *Handler) accountsList(c *gin.Context) {
	opts := AccountsListOptions{
		Search:       c.Query("search"),
		Statuses:     splitCSV(c.Query("status")),
		Plans:        splitCSV(c.Query("plan")),
		PastDue:      queryBool(c, "past_due"),
		NearLimit:    queryBool(c, "near_limit"),
		HasOverrides: queryBool(c, "has_overrides"),
		Comped:       queryBool(c, "comped"),
		Idle90d:      queryBool(c, "idle_90d"),
		Limit:        int(parseInt32(c.Query("limit"), 50)),
		Offset:       int(parseInt32(c.Query("offset"), 0)),
	}
	resp, err := h.svc.ListAccounts(c.Request.Context(), opts)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

func parseTenantIDParam(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_tenant_id", "id is not a valid UUID"))
		return uuid.Nil, false
	}
	return id, true
}

func (h *Handler) accountDetail(c *gin.Context) {
	tenantID, ok := parseTenantIDParam(c)
	if !ok {
		return
	}
	detail, err := h.svc.GetAccountDetail(c.Request.Context(), tenantID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, detail)
}

func (h *Handler) revenue(c *gin.Context) {
	resp, err := h.svc.GetRevenue(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// superadminActorID returns the calling superadmin's REAL user id — the
// mutation handlers below always audit under this, never a synthetic actor
// string (see the audit_log actor_id ::uuid-cast incident this rule guards
// against, referenced throughout billing_service.go).
func superadminActorID(c *gin.Context) (uuid.UUID, bool) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser {
		httpx.Error(c, domain.Forbidden("superadmin_required", "superadmin access required"))
		return uuid.Nil, false
	}
	return p.UserID, true
}

type compRequestBody struct {
	Tier   string `json:"tier"`
	Reason string `json:"reason"`
}

func (h *Handler) compAccount(c *gin.Context) {
	tenantID, ok := parseTenantIDParam(c)
	if !ok {
		return
	}
	actorID, ok := superadminActorID(c)
	if !ok {
		return
	}
	var body compRequestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	if err := h.svc.CompAccount(c.Request.Context(), actorID, tenantID, billing.Tier(body.Tier), body.Reason); err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

type reasonOnlyBody struct {
	Reason string `json:"reason"`
}

func (h *Handler) revokeComp(c *gin.Context) {
	tenantID, ok := parseTenantIDParam(c)
	if !ok {
		return
	}
	actorID, ok := superadminActorID(c)
	if !ok {
		return
	}
	var body reasonOnlyBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	if err := h.svc.RevokeComp(c.Request.Context(), actorID, tenantID, body.Reason); err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

type overridesRequestBody struct {
	Sites     *int   `json:"sites"`
	StorageGB *int   `json:"storage_gb"`
	Seats     *int   `json:"seats"`
	Reason    string `json:"reason"`
}

func (h *Handler) setOverrides(c *gin.Context) {
	tenantID, ok := parseTenantIDParam(c)
	if !ok {
		return
	}
	actorID, ok := superadminActorID(c)
	if !ok {
		return
	}
	var body overridesRequestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	deltas := OverrideDeltas{SitesDelta: body.Sites, StorageGBDelta: body.StorageGB, SeatsDelta: body.Seats}
	if err := h.svc.SetAccountOverrides(c.Request.Context(), actorID, tenantID, deltas, body.Reason); err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

type graceRequestBody struct {
	Until  string `json:"until"` // RFC3339
	Reason string `json:"reason"`
}

func (h *Handler) extendGrace(c *gin.Context) {
	tenantID, ok := parseTenantIDParam(c)
	if !ok {
		return
	}
	actorID, ok := superadminActorID(c)
	if !ok {
		return
	}
	var body graceRequestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	until, perr := time.Parse(time.RFC3339, body.Until)
	if perr != nil {
		httpx.Error(c, domain.Validation("invalid_until", "until must be an RFC3339 timestamp"))
		return
	}
	if err := h.svc.ExtendGrace(c.Request.Context(), actorID, tenantID, until, body.Reason); err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) suspendAccount(c *gin.Context) {
	tenantID, ok := parseTenantIDParam(c)
	if !ok {
		return
	}
	actorID, ok := superadminActorID(c)
	if !ok {
		return
	}
	var body reasonOnlyBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	if err := h.svc.SuspendAccount(c.Request.Context(), actorID, tenantID, body.Reason); err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) restoreAccount(c *gin.Context) {
	tenantID, ok := parseTenantIDParam(c)
	if !ok {
		return
	}
	actorID, ok := superadminActorID(c)
	if !ok {
		return
	}
	var body reasonOnlyBody
	// A body is often omitted on a restore action in practice; treat a
	// missing/empty body as an empty reason (rejected by requireReason below)
	// rather than a hard 400, so the frontend's error message is consistent
	// with every other mutation's "reason is required" validation.
	_ = c.ShouldBindJSON(&body)
	if err := h.svc.RestoreAccount(c.Request.Context(), actorID, tenantID, body.Reason); err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

type forceStateRequestBody struct {
	Plan       string `json:"plan"`
	PlanStatus string `json:"plan_status"`
	Reason     string `json:"reason"`
}

func (h *Handler) forceState(c *gin.Context) {
	tenantID, ok := parseTenantIDParam(c)
	if !ok {
		return
	}
	actorID, ok := superadminActorID(c)
	if !ok {
		return
	}
	var body forceStateRequestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	err := h.svc.ForceAccountState(c.Request.Context(), actorID, tenantID, billing.Tier(body.Plan), billing.Status(body.PlanStatus), body.Reason)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
