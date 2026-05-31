package site

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// rfc3339 is the timestamp format for once-shown enrollment-code expiries.
const rfc3339 = time.RFC3339

// SetConnectionService wires the connection-lifecycle service onto the handler.
// Call once at boot, after the service is constructed. When nil, the lifecycle
// routes return 501 (the feature is disabled in this build).
func (h *Handler) SetConnectionService(cs ConnectionService) { h.conn = cs }

// RegisterConnection mounts the M21 connection-lifecycle routes on the /api/v1
// group. These are DESTRUCTIVE / severing actions (rotate the agent identity,
// soft-delete, re-enroll), so they require ORG scope (RequireOrgScope) on top of
// site:write — a site-scoped *collaborator* (an outside operator shared exactly
// one site) may OPERATE the site but must NOT revoke/archive/re-enroll it out
// from under the owner. (Phase 6 security review, finding #5.) RequireSiteAccess
// stays as defense-in-depth for the :siteId binding.
func (h *Handler) RegisterConnection(r *gin.RouterGroup) {
	r.POST("/sites/:siteId/enrollment-codes",
		authz.RequirePermission(authz.PermSiteWrite), authz.RequireOrgScope(), authz.RequireSiteAccess("siteId"), h.beginReEnrollment)
	r.POST("/sites/:siteId/revoke",
		authz.RequirePermission(authz.PermSiteWrite), authz.RequireOrgScope(), authz.RequireSiteAccess("siteId"), h.revoke)
	r.POST("/sites/:siteId/archive",
		authz.RequirePermission(authz.PermSiteWrite), authz.RequireOrgScope(), authz.RequireSiteAccess("siteId"), h.archive)
	r.POST("/sites/:siteId/restore",
		authz.RequirePermission(authz.PermSiteWrite), authz.RequireOrgScope(), authz.RequireSiteAccess("siteId"), h.restore)
}

// enrollmentCodeResponse is the once-shown enrollment code returned by the
// site-first create and the re-enroll endpoints.
type enrollmentCodeResponse struct {
	SiteID         uuid.UUID `json:"site_id"`
	EnrollmentCode string    `json:"enrollment_code"`
	ExpiresAt      string    `json:"expires_at"`
}

// createSiteV2Request is the site-first "Add site" body (M21). It replaces the
// legacy create body: instead of registering a bare row, it provisions a
// pending_enrollment site AND a site-bound enrollment code in one call.
type createSiteV2Request struct {
	URL  string   `json:"url"`
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// createWithEnrollment is the M21 site-first create. It supersedes the legacy
// POST /sites create-bare-row behaviour: the response now carries an
// enrollment_code + expires_at so the dashboard can immediately show the
// install modal and subscribe to the SSE stream for this site_id.
//
// BREAKING CHANGE vs the legacy create: the response shape changes from a bare
// Site to {site_id, enrollment_code, expires_at}. See the PR notes.
func (h *Handler) createWithEnrollment(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	if h.conn == nil {
		httpx.Error(c, domain.Unavailable("lifecycle_disabled", "connection lifecycle is not enabled on this control plane"))
		return
	}
	var req createSiteV2Request
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	var createdBy uuid.UUID
	if p, ok := domain.PrincipalFromContext(c.Request.Context()); ok {
		createdBy = p.UserID
	}
	code, err := h.conn.MintEnrollmentCode(c.Request.Context(), MintEnrollmentInput{
		TenantID:  tenantID,
		CreatedBy: createdBy,
		URL:       req.URL,
		Name:      req.Name,
		Tags:      req.Tags,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusCreated, enrollmentCodeResponse{
		SiteID:         code.SiteID,
		EnrollmentCode: code.Plaintext,
		ExpiresAt:      code.ExpiresAt.UTC().Format(rfc3339),
	})
}

func (h *Handler) beginReEnrollment(c *gin.Context) {
	tenantID, siteID, actorID, ok := h.lifecycleCtx(c)
	if !ok {
		return
	}
	code, err := h.conn.BeginReEnrollment(c.Request.Context(), ActorSiteInput{
		TenantID: tenantID, SiteID: siteID, ActorID: actorID,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusCreated, enrollmentCodeResponse{
		SiteID:         code.SiteID,
		EnrollmentCode: code.Plaintext,
		ExpiresAt:      code.ExpiresAt.UTC().Format(rfc3339),
	})
}

type reasonRequest struct {
	Reason string `json:"reason"`
}

func (h *Handler) revoke(c *gin.Context) {
	tenantID, siteID, actorID, ok := h.lifecycleCtx(c)
	if !ok {
		return
	}
	reason := optionalReason(c)
	s, err := h.conn.Revoke(c.Request.Context(), ActorSiteInput{
		TenantID: tenantID, SiteID: siteID, ActorID: actorID, Reason: reason,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	out := toAPI(s)
	c.JSON(http.StatusOK, &out)
}

func (h *Handler) archive(c *gin.Context) {
	tenantID, siteID, actorID, ok := h.lifecycleCtx(c)
	if !ok {
		return
	}
	reason := optionalReason(c)
	if err := h.conn.Archive(c.Request.Context(), ActorSiteInput{
		TenantID: tenantID, SiteID: siteID, ActorID: actorID, Reason: reason,
	}); err != nil {
		httpx.Error(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) restore(c *gin.Context) {
	tenantID, siteID, actorID, ok := h.lifecycleCtx(c)
	if !ok {
		return
	}
	s, err := h.conn.Restore(c.Request.Context(), ActorSiteInput{
		TenantID: tenantID, SiteID: siteID, ActorID: actorID,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	out := toAPI(s)
	c.JSON(http.StatusOK, &out)
}

// lifecycleCtx resolves the (tenant, site, actor) tuple common to every
// lifecycle mutation and validates the lifecycle service is wired. It writes
// the error response itself and returns ok=false on any failure.
func (h *Handler) lifecycleCtx(c *gin.Context) (tenantID, siteID, actorID uuid.UUID, ok bool) {
	tenantID, hasTenant := domain.TenantIDFromContext(c.Request.Context())
	if !hasTenant {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	if h.conn == nil {
		httpx.Error(c, domain.Unavailable("lifecycle_disabled", "connection lifecycle is not enabled on this control plane"))
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	id, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	if p, hasP := domain.PrincipalFromContext(c.Request.Context()); hasP {
		actorID = p.UserID
	}
	return tenantID, id, actorID, true
}

// optionalReason parses an optional {reason} body. A missing/empty body is fine.
func optionalReason(c *gin.Context) string {
	var req reasonRequest
	if c.Request.ContentLength != 0 {
		_ = c.ShouldBindJSON(&req)
	}
	return req.Reason
}
