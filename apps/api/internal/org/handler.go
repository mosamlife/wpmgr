// Package org serves the organisation management endpoints:
//   - POST   /api/v1/orgs               — create a new org (tenant) + creator owner membership
//   - POST   /api/v1/orgs/{orgId}/activate — switch the session's active tenant
//
// Hand-rolled Gin style (no ogen churn) mirroring restore_run_handler.go.
package org

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// TenantCreator creates a new tenant and returns its ID.
type TenantCreator interface {
	Create(ctx context.Context, name, slug string) (uuid.UUID, error)
}

// SessionManager sets the active tenant on the current session.
type SessionManager interface {
	SetActiveTenant(ctx context.Context, tenantID uuid.UUID)
}

// AuthService provides membership queries.
type AuthService interface {
	RoleInTenant(ctx context.Context, userID, tenantID uuid.UUID) (authz.Role, bool)
}

// Handler serves /api/v1/orgs and /api/v1/orgs/:orgId/activate.
type Handler struct {
	pool     *db.Pool
	tenants  TenantCreator
	sessions SessionManager
	authSvc  AuthService
	audit    *audit.Recorder
}

// NewHandler builds an org Handler.
func NewHandler(pool *db.Pool, tenants TenantCreator, sessions SessionManager, authSvc AuthService, rec *audit.Recorder) *Handler {
	return &Handler{pool: pool, tenants: tenants, sessions: sessions, authSvc: authSvc, audit: rec}
}

// Register mounts the org routes on the /api/v1 group.
// POST /orgs requires only authentication (any logged-in user can create a new org).
// POST /orgs/:orgId/activate requires the caller to be a member of that org.
func (h *Handler) Register(r *gin.RouterGroup) {
	r.GET("/orgs", h.list)
	r.POST("/orgs", h.create)
	r.POST("/orgs/:orgId/activate", h.activate)
	r.PATCH("/orgs/:orgId", h.rename)
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

type createOrgBody struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type orgDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// orgListItemDTO carries the caller's role so the switcher/settings can gate
// the rename action (admin/owner only) without a second round-trip.
type orgListItemDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
	Role string `json:"role"`
}

type orgListDTO struct {
	Items []orgListItemDTO `json:"items"`
}

type renameOrgBody struct {
	Name string `json:"name"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (h *Handler) create(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	var body createOrgBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	if body.Name == "" {
		httpx.Error(c, domain.Validation("name_required", "name is required"))
		return
	}
	slug := body.Slug
	if slug == "" {
		slug = slugify(body.Name)
	}

	// Step 1: create the tenant (no RLS scope yet — tenant service handles this).
	tenantID, err := h.tenants.Create(c.Request.Context(), body.Name, slug)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	// Step 2: grant creator an owner membership inside the new tenant's scope.
	err = h.pool.InTenantTx(c.Request.Context(), tenantID, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).UpsertOwnerMembership(c.Request.Context(), sqlc.UpsertOwnerMembershipParams{
			UserID:   p.UserID,
			TenantID: tenantID,
		})
		return err
	})
	if err != nil {
		httpx.Error(c, domain.Internal("membership_create_failed", "failed to create owner membership").WithCause(err))
		return
	}

	// Audit org.created.
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   tenantID,
		ActorType:  audit.ActorUser,
		ActorID:    p.UserID.String(),
		Action:     "org.created",
		TargetType: "tenant",
		TargetID:   tenantID.String(),
		Metadata:   map[string]any{"name": body.Name, "slug": slug},
	})

	c.JSON(http.StatusCreated, orgDTO{ID: tenantID.String(), Name: body.Name, Slug: slug})
}

func (h *Handler) activate(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	rawID := c.Param("orgId")
	orgID, err := uuid.Parse(rawID)
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_org_id", "orgId is not a valid UUID"))
		return
	}

	// The caller may activate an org they're a member of OR one where a site has
	// been shared with them (a site-scoped collaborator). The auth middleware then
	// scopes a non-member to exactly their allowed sites in that org, so this
	// cannot grant org-wide access — it only lets them reach their shared sites.
	_, isMember := h.authSvc.RoleInTenant(c.Request.Context(), p.UserID, orgID)
	if !isMember && !h.hasActiveShare(c.Request.Context(), p.UserID, orgID) {
		httpx.Error(c, domain.Forbidden("not_a_member", "you do not have access to this organisation"))
		return
	}

	// Persist the new active tenant on the session.
	h.sessions.SetActiveTenant(c.Request.Context(), orgID)

	c.JSON(http.StatusOK, gin.H{"active_tenant_id": orgID.String()})
}

// list returns the caller's organisations with names + their role in each. Runs
// under InUserTx so the memberships_self_read policy scopes the join to the
// caller (no cross-tenant enumeration).
func (h *Handler) list(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	var rows []sqlc.ListOrgsForUserRow
	err := h.pool.InUserTx(c.Request.Context(), p.UserID, func(tx pgx.Tx) error {
		var qErr error
		rows, qErr = sqlc.New(tx).ListOrgsForUser(c.Request.Context(), p.UserID)
		return qErr
	})
	if err != nil {
		httpx.Error(c, domain.Internal("org_list_failed", "failed to list organisations").WithCause(err))
		return
	}
	items := make([]orgListItemDTO, 0, len(rows))
	for _, r := range rows {
		items = append(items, orgListItemDTO{
			ID:   r.ID.String(),
			Name: r.Name,
			Slug: r.Slug,
			Role: r.Role,
		})
	}
	c.JSON(http.StatusOK, orgListDTO{Items: items})
}

// rename updates an organisation's display name. Requires the caller to be an
// admin or owner of that org (tenants has no RLS, so the role check is the gate).
func (h *Handler) rename(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	orgID, err := uuid.Parse(c.Param("orgId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_org_id", "orgId is not a valid UUID"))
		return
	}
	var body renameOrgBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		httpx.Error(c, domain.Validation("name_required", "name is required"))
		return
	}
	if len(name) > 200 {
		httpx.Error(c, domain.Validation("name_too_long", "name must be 200 characters or fewer"))
		return
	}

	// Authorize: caller must be an admin/owner of this org.
	role, isMember := h.authSvc.RoleInTenant(c.Request.Context(), p.UserID, orgID)
	if !isMember {
		httpx.Error(c, domain.Forbidden("not_a_member", "you are not a member of this organisation"))
		return
	}
	if !role.AtLeast(authz.RoleAdmin) {
		httpx.Error(c, domain.Forbidden("insufficient_role", "only an admin or owner can rename the organisation"))
		return
	}

	var updated sqlc.Tenant
	err = h.pool.InTenantTx(c.Request.Context(), orgID, func(tx pgx.Tx) error {
		var uErr error
		updated, uErr = sqlc.New(tx).UpdateTenantName(c.Request.Context(), sqlc.UpdateTenantNameParams{
			ID:   orgID,
			Name: name,
		})
		return uErr
	})
	if err != nil {
		httpx.Error(c, domain.Internal("org_rename_failed", "failed to rename organisation").WithCause(err))
		return
	}

	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   orgID,
		ActorType:  audit.ActorUser,
		ActorID:    p.UserID.String(),
		Action:     "org.renamed",
		TargetType: "tenant",
		TargetID:   orgID.String(),
		Metadata:   map[string]any{"name": name},
	})

	c.JSON(http.StatusOK, orgDTO{ID: updated.ID.String(), Name: updated.Name, Slug: updated.Slug})
}

// hasActiveShare reports whether the user holds a non-expired site_share in the
// given tenant (a site-scoped collaborator). Runs under InUserTx so the
// site_shares_self_read policy exposes the caller's own share rows.
func (h *Handler) hasActiveShare(ctx context.Context, userID, tenantID uuid.UUID) bool {
	var has bool
	err := h.pool.InUserTx(ctx, userID, func(tx pgx.Tx) error {
		shares, qErr := sqlc.New(tx).GetActiveSharesForUserTenant(ctx, sqlc.GetActiveSharesForUserTenantParams{
			UserID:   userID,
			TenantID: tenantID,
		})
		if qErr != nil {
			return qErr
		}
		has = len(shares) > 0
		return nil
	})
	return err == nil && has
}

// slugify converts a name to a URL-safe slug (lowercase, spaces→hyphens, strip others).
func slugify(name string) string {
	out := make([]byte, 0, len(name))
	for _, ch := range name {
		switch {
		case ch >= 'A' && ch <= 'Z':
			out = append(out, byte(ch-'A'+'a'))
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch == '-':
			out = append(out, byte(ch))
		case ch == ' ' || ch == '_':
			out = append(out, '-')
		}
	}
	if len(out) == 0 {
		return "org"
	}
	return string(out)
}
