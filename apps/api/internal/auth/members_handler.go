package auth

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

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
}

func (h *MembersHandler) list(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	ms, err := h.svc.repo.ListMembershipsForTenant(c.Request.Context(), p.TenantID,
		parseLimit(c.Query("limit")), parseOffset(c.Query("offset")))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gen.MembershipList{Items: toAPIMemberships(ms)})
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
	_, m, err := h.svc.Invite(c.Request.Context(), p.TenantID, p.UserID, InviteInput{
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
