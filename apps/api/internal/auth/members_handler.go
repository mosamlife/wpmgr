package auth

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// memberDTO is the members-list row enriched with the user's email + name so the
// dashboard shows a human identity, not a bare UUID. (The ogen gen.Membership
// carries only ids; this hand-rolled route is free to return more.)
type memberDTO struct {
	UserID    string `json:"user_id"`
	TenantID  string `json:"tenant_id"`
	Role      string `json:"role"`
	Email     string `json:"email,omitempty"`
	Name      string `json:"name,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

type memberListDTO struct {
	Items []memberDTO `json:"items"`
}

// MembersHandler serves tenant member management under /api/v1/members.
// Reads require viewer+; mutations require admin+ (enforced via middleware).
type MembersHandler struct {
	svc *Service
}

// NewMembersHandler builds a MembersHandler.
func NewMembersHandler(svc *Service) *MembersHandler {
	return &MembersHandler{svc: svc}
}

// Register mounts member routes with per-route RBAC.
func (h *MembersHandler) Register(r *gin.RouterGroup) {
	r.GET("/members", authz.RequirePermission(authz.PermMemberRead), h.list)
	r.POST("/members", authz.RequirePermission(authz.PermMemberManage), h.invite)
	r.PATCH("/members/:userId", authz.RequirePermission(authz.PermMemberManage), h.patchRole)
	r.DELETE("/members/:userId", authz.RequirePermission(authz.PermMemberManage), h.removeMember)
}

func (h *MembersHandler) list(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	ms, err := h.svc.repo.ListMembershipsForTenant(c.Request.Context(), p.TenantID,
		parseLimit(c.Query("limit")), parseOffset(c.Query("offset")))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, memberListDTO{Items: h.enrich(c.Request.Context(), ms)})
}

// enrich resolves each membership's user_id to email + name via a single batch
// query, so the members list shows a human identity instead of a UUID. The
// users table has no RLS, so the lookup spans the (small) member set directly.
func (h *MembersHandler) enrich(ctx context.Context, ms []Membership) []memberDTO {
	ids := make([]uuid.UUID, 0, len(ms))
	for _, m := range ms {
		ids = append(ids, m.UserID)
	}
	byID := make(map[uuid.UUID]UserBrief)
	if briefs, err := h.svc.repo.GetUsersByIDs(ctx, ids); err == nil {
		for _, b := range briefs {
			byID[b.ID] = b
		}
	}
	out := make([]memberDTO, 0, len(ms))
	for _, m := range ms {
		d := memberDTO{
			UserID:    m.UserID.String(),
			TenantID:  m.TenantID.String(),
			Role:      string(m.Role),
			CreatedAt: m.CreatedAt.UTC().Format(time.RFC3339),
		}
		if b, ok := byID[m.UserID]; ok {
			d.Email = b.Email
			d.Name = b.Name
		}
		out = append(out, d)
	}
	return out
}

type inviteBody struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
	Role     string `json:"role"`
}

func (h *MembersHandler) invite(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	var body inviteBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	_, m, err := h.svc.Invite(c.Request.Context(), p.TenantID, p.UserID, authz.Role(p.Role), InviteInput{
		Email:    body.Email,
		Password: body.Password,
		Name:     body.Name,
		Role:     roleOrDefault(body.Role),
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	out := toAPIMembership(m)
	c.JSON(http.StatusCreated, &out)
}

type patchRoleBody struct {
	Role string `json:"role"`
}

func (h *MembersHandler) patchRole(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	targetUserID, err := parseUUID(c.Param("userId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_user_id", "userId is not a valid UUID"))
		return
	}
	var body patchRoleBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	newRole := authz.Role(body.Role)
	if !newRole.Valid() {
		httpx.Error(c, domain.Validation("role_invalid", "invalid role"))
		return
	}
	actorRole := authz.Role(p.Role)
	// Privilege ceiling: actor cannot grant a role higher than their own.
	if !actorRole.AtLeast(newRole) {
		httpx.Error(c, domain.Forbidden("role_grant_exceeds_actor", "you cannot grant a role higher than your own"))
		return
	}
	// Last-owner protection: cannot demote the last owner.
	if newRole != authz.RoleOwner {
		// Check whether the target is currently an owner.
		existing, getErr := h.svc.repo.GetMembership(c.Request.Context(), targetUserID, p.TenantID)
		if getErr != nil {
			httpx.Error(c, getErr)
			return
		}
		if existing.Role == authz.RoleOwner {
			ownerCount, countErr := h.svc.CountOwners(c.Request.Context(), p.TenantID)
			if countErr != nil {
				httpx.Error(c, countErr)
				return
			}
			if ownerCount <= 1 {
				httpx.Error(c, domain.Forbidden("last_owner", "cannot demote the last owner"))
				return
			}
		}
	}
	m, err := h.svc.repo.UpdateMembershipRole(c.Request.Context(), targetUserID, p.TenantID, newRole)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.svc.RecordAudit(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  audit.ActorUser,
		ActorID:    p.UserID.String(),
		Action:     "member.role_changed",
		TargetType: "user",
		TargetID:   targetUserID.String(),
		Metadata:   map[string]any{"role": string(newRole)},
	})
	out := toAPIMembership(m)
	c.JSON(http.StatusOK, &out)
}

func (h *MembersHandler) removeMember(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	targetUserID, err := parseUUID(c.Param("userId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_user_id", "userId is not a valid UUID"))
		return
	}
	// Last-owner protection: cannot remove the last owner.
	existing, getErr := h.svc.repo.GetMembership(c.Request.Context(), targetUserID, p.TenantID)
	if getErr != nil {
		httpx.Error(c, getErr)
		return
	}
	if existing.Role == authz.RoleOwner {
		ownerCount, countErr := h.svc.CountOwners(c.Request.Context(), p.TenantID)
		if countErr != nil {
			httpx.Error(c, countErr)
			return
		}
		if ownerCount <= 1 {
			httpx.Error(c, domain.Forbidden("last_owner", "cannot remove the last owner"))
			return
		}
	}
	if err := h.svc.repo.DeleteMembership(c.Request.Context(), targetUserID, p.TenantID); err != nil {
		httpx.Error(c, err)
		return
	}
	h.svc.RecordAudit(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  audit.ActorUser,
		ActorID:    p.UserID.String(),
		Action:     "member.removed",
		TargetType: "user",
		TargetID:   targetUserID.String(),
	})
	c.Status(http.StatusNoContent)
}

func parseUUID(s string) (uuid.UUID, error) {
	return uuid.Parse(s)
}

func parseLimit(s string) int32 {
	n := parseInt32(s, 50)
	if n <= 0 {
		n = 50
	}
	if n > 200 {
		n = 200
	}
	return n
}

func parseOffset(s string) int32 {
	n := parseInt32(s, 0)
	if n < 0 {
		n = 0
	}
	return n
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
