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

// OrgInviter creates a tokenized org invitation (the invitee sets their own
// password via the emailed /accept link) and returns that accept link. Satisfied
// by *invitation.Service; declared here so this package does not import
// invitation (which already imports auth — avoids a cycle). ADR-045 Phase 3.
type OrgInviter interface {
	CreateOrgInvitation(ctx context.Context, tenantID, actorID uuid.UUID, actorRole authz.Role, email, role string) (acceptLink string, err error)
}

// MembersHandler serves tenant member management under /api/v1/members.
// Reads require viewer+; mutations require admin+ (enforced via middleware).
type MembersHandler struct {
	svc     *Service
	inviter OrgInviter
}

// NewMembersHandler builds a MembersHandler. inviter wires the tokenized
// email-invite flow (nil falls back to the legacy password-in-body invite).
func NewMembersHandler(svc *Service, inviter OrgInviter) *MembersHandler {
	return &MembersHandler{svc: svc, inviter: inviter}
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

	// ADR-045 Phase 3 — tokenized email invite: the teammate gets an emailed
	// /accept link and sets their OWN password. The membership is created when
	// they accept, so we return the invitation details + the accept link (which
	// the admin can also hand-deliver). The legacy password-in-body path is kept
	// only as a fallback when no inviter is wired.
	if h.inviter != nil {
		role := body.Role
		if role == "" {
			role = string(authz.RoleViewer)
		}
		// RoleClient is portal-only; org membership invitations must never carry it.
		if authz.Role(role) == authz.RoleClient {
			httpx.Error(c, domain.Validation("role_invalid", "client role cannot be assigned via org invitation"))
			return
		}
		link, err := h.inviter.CreateOrgInvitation(c.Request.Context(), p.TenantID, p.UserID, authz.Role(p.Role), body.Email, role)
		if err != nil {
			httpx.Error(c, err)
			return
		}
		c.JSON(http.StatusCreated, gin.H{"email": body.Email, "role": role, "accept_link": link})
		return
	}

	// RoleClient is portal-only; the legacy path must reject it too (the DB
	// memberships_role_check would refuse it anyway, but as a 500, not a 400).
	if roleOrDefault(body.Role) == authz.RoleClient {
		httpx.Error(c, domain.Validation("role_invalid", "client role cannot be assigned via org invitation"))
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
	// RoleClient is portal-only and must never be set as an org membership role.
	if newRole == authz.RoleClient {
		httpx.Error(c, domain.Validation("role_invalid", "client role cannot be assigned as an org membership role"))
		return
	}
	actorRole := authz.Role(p.Role)
	// Privilege ceiling: actor cannot grant a role higher than their own.
	if !actorRole.AtLeast(newRole) {
		httpx.Error(c, domain.Forbidden("role_grant_exceeds_actor", "you cannot grant a role higher than your own"))
		return
	}
	// GH #406 — the TARGET's current role is read UNCONDITIONALLY, before any
	// branch. Until this moved out of the `newRole != RoleOwner` arm below, an
	// admin could demote or re-role an OWNER: the grant ceiling above only
	// compares the actor to the role being GRANTED, and the last-owner COUNT
	// guard only fires in a single-owner org. In an org with two owners there
	// was no refusal at all.
	existing, getErr := h.svc.repo.GetMembership(c.Request.Context(), targetUserID, p.TenantID)
	if getErr != nil {
		httpx.Error(c, getErr)
		return
	}
	// Target ceiling: an actor may never act on a member who outranks them.
	// owner-on-owner stays ALLOWED (ownership transfer is a supported
	// capability); admin-on-admin and admin-on-viewer are unchanged.
	if !actorRole.AtLeast(existing.Role) {
		httpx.Error(c, domain.Forbidden("target_role_exceeds_actor", "you cannot change the role of a member who outranks you"))
		return
	}
	// Last-owner protection: cannot demote the last owner.
	if newRole != authz.RoleOwner && existing.Role == authz.RoleOwner {
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
	m, err := h.svc.repo.UpdateMembershipRole(c.Request.Context(), targetUserID, p.TenantID, newRole)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.svc.RecordAudit(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorTypeOf(p),
		ActorID:    p.ActorID(),
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
	existing, getErr := h.svc.repo.GetMembership(c.Request.Context(), targetUserID, p.TenantID)
	if getErr != nil {
		httpx.Error(c, getErr)
		return
	}
	// GH #406 — target ceiling. Before this, removeMember never read p.Role at
	// all: its only gate was the last-owner COUNT, so an admin could delete an
	// owner outright from any org holding two or more owners.
	if !authz.Role(p.Role).AtLeast(existing.Role) {
		httpx.Error(c, domain.Forbidden("target_role_exceeds_actor", "you cannot remove a member who outranks you"))
		return
	}
	// Last-owner protection: cannot remove the last owner.
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
		ActorType:  actorTypeOf(p),
		ActorID:    p.ActorID(),
		Action:     "member.removed",
		TargetType: "user",
		TargetID:   targetUserID.String(),
	})
	c.Status(http.StatusNoContent)
}

// actorTypeOf maps a principal to its audit actor type. A member mutation can
// be driven by an API key as easily as by a session, and hard-coding ActorUser
// logged those as the null UUID (p.UserID is uuid.Nil for a key). Mirrors
// apikey.actorType.
func actorTypeOf(p domain.Principal) string {
	if p.Type == domain.PrincipalAPIKey {
		return audit.ActorAPIKey
	}
	return audit.ActorUser
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
