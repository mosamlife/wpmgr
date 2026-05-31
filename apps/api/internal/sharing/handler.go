package sharing

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the site-sharing endpoints under /api/v1.
// Hand-rolled Gin style (no ogen churn) mirroring restore_run_handler.go.
type Handler struct {
	svc *Service
}

// NewHandler builds a sharing Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Register mounts site-share routes on the /api/v1 group.
//
//	GET    /sites/:siteId/shares         — list collaborators (PermMemberManage, org-scoped only)
//	POST   /sites/:siteId/shares         — grant/invite (PermMemberManage, org-scoped only)
//	DELETE /sites/:siteId/shares/:userId — revoke (PermMemberManage, org-scoped only)
//	GET    /shared-with-me               — self-service list (any authenticated user)
func (h *Handler) Register(r *gin.RouterGroup) {
	// FIX 4: GET /shares must also be org-scoped only — a site-scoped
	// collaborator must not enumerate who else has access to the site.
	// PermMemberManage is already an org-level permission (blocked by
	// RequirePermission's org_scope_required guard), but requireOrgScope()
	// is added explicitly here for defense-in-depth and clarity.
	r.GET("/sites/:siteId/shares", authz.RequirePermission(authz.PermMemberManage), requireOrgScope(), h.list)
	// POST and DELETE require org membership — a site-scoped principal cannot manage shares.
	r.POST("/sites/:siteId/shares", authz.RequirePermission(authz.PermMemberManage), requireOrgScope(), h.grant)
	r.DELETE("/sites/:siteId/shares/:userId", authz.RequirePermission(authz.PermMemberManage), requireOrgScope(), h.revoke)
	r.GET("/shared-with-me", h.sharedWithMe)
}

// requireOrgScope is a middleware that rejects site-scoped principals with 403:
// only org members (full membership) may create or revoke site shares.
func requireOrgScope() gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := domain.PrincipalFromContext(c.Request.Context())
		if !ok {
			httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
			c.Abort()
			return
		}
		if p.Scope == domain.ScopeSite {
			httpx.Error(c, domain.Forbidden("org_scope_required", "site-scoped collaborators cannot manage shares"))
			c.Abort()
			return
		}
		c.Next()
	}
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

type shareDTO struct {
	ID        string  `json:"id"`
	SiteID    string  `json:"site_id"`
	UserID    string  `json:"user_id"`
	Role      string  `json:"role"`
	GrantedBy *string `json:"granted_by,omitempty"`
	ExpiresAt *string `json:"expires_at,omitempty"`
	CreatedAt string  `json:"created_at"`
}

type shareListDTO struct {
	Items []shareDTO `json:"items"`
}

type grantBody struct {
	Email     string  `json:"email"`
	Role      string  `json:"role"`
	ExpiresAt *string `json:"expires_at,omitempty"` // RFC3339 or null
}

// parseExpiry accepts an RFC3339 timestamp (the web sends this), a browser
// "datetime-local" value ("2006-01-02T15:04", treated as UTC), or a plain date
// ("2006-01-02", treated as the end of that day UTC so a date-only expiry stays
// valid through that whole day). Returns ok=false on an unrecognised format.
func parseExpiry(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano, "2006-01-02T15:04"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.Add(24*time.Hour - time.Second).UTC(), true
	}
	return time.Time{}, false
}

type grantResponseDTO struct {
	Share      *shareDTO `json:"share,omitempty"`
	Invited    bool      `json:"invited"`
	AcceptLink string    `json:"accept_link,omitempty"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (h *Handler) list(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	siteID, ok := uuidParam(c, "siteId", "invalid_site_id")
	if !ok {
		return
	}
	shares, err := h.svc.ListForSite(c.Request.Context(), tenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]shareDTO, 0, len(shares))
	for _, s := range shares {
		items = append(items, toShareDTO(s))
	}
	c.JSON(http.StatusOK, shareListDTO{Items: items})
}

func (h *Handler) grant(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	siteID, ok := uuidParam(c, "siteId", "invalid_site_id")
	if !ok {
		return
	}
	var body grantBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	if body.Email == "" {
		httpx.Error(c, domain.Validation("email_required", "email is required"))
		return
	}
	if body.Role == "" {
		body.Role = "viewer"
	}

	var expiresAt *time.Time
	if body.ExpiresAt != nil && *body.ExpiresAt != "" {
		t, ok := parseExpiry(*body.ExpiresAt)
		if !ok {
			httpx.Error(c, domain.Validation("invalid_expires_at",
				"expires_at must be an RFC3339 timestamp, a datetime-local value, or a YYYY-MM-DD date"))
			return
		}
		if t.Before(time.Now()) {
			httpx.Error(c, domain.Validation("expires_in_past", "expires_at must be in the future"))
			return
		}
		expiresAt = &t
	}

	result, err := h.svc.Grant(c.Request.Context(), tenantID, siteID, GrantInput{
		Email:     body.Email,
		Role:      body.Role,
		ExpiresAt: expiresAt,
		ActorID:   p.UserID,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}

	resp := grantResponseDTO{Invited: result.Invited, AcceptLink: result.AcceptLink}
	if result.Share != nil {
		d := toShareDTO(*result.Share)
		resp.Share = &d
	}
	status := http.StatusCreated
	if result.Invited {
		status = http.StatusAccepted
	}
	c.JSON(status, resp)
}

func (h *Handler) revoke(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	siteID, ok := uuidParam(c, "siteId", "invalid_site_id")
	if !ok {
		return
	}
	userID, ok := uuidParam(c, "userId", "invalid_user_id")
	if !ok {
		return
	}
	if err := h.svc.Revoke(c.Request.Context(), tenantID, siteID, userID, p.UserID); err != nil {
		httpx.Error(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) sharedWithMe(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	shares, err := h.svc.SharedWithMe(c.Request.Context(), p.UserID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]shareDTO, 0, len(shares))
	for _, s := range shares {
		items = append(items, toShareDTO(s))
	}
	c.JSON(http.StatusOK, shareListDTO{Items: items})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func toShareDTO(s Share) shareDTO {
	d := shareDTO{
		ID:        s.ID.String(),
		SiteID:    s.SiteID.String(),
		UserID:    s.UserID.String(),
		Role:      s.Role,
		CreatedAt: s.CreatedAt.UTC().Format(time.RFC3339),
	}
	if s.GrantedBy != nil {
		v := s.GrantedBy.String()
		d.GrantedBy = &v
	}
	if s.ExpiresAt != nil {
		v := s.ExpiresAt.UTC().Format(time.RFC3339)
		d.ExpiresAt = &v
	}
	return d
}

func tenantOf(c *gin.Context) (uuid.UUID, bool) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.TenantID == uuid.Nil {
		httpx.Error(c, domain.Forbidden("tenant_required", "no active tenant"))
		return uuid.Nil, false
	}
	return p.TenantID, true
}

func uuidParam(c *gin.Context, name, code string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		httpx.Error(c, domain.Validation(code, name+" is not a valid UUID"))
		return uuid.Nil, false
	}
	return id, true
}
