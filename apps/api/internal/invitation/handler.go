package invitation

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the public invitation-accept endpoint.
// Mounted under /api/v1/invitations WITHOUT RequireAuth (the endpoint creates
// the session itself).
type Handler struct {
	svc *Service
}

// NewHandler builds an invitation Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterPublic mounts the public accept route on the engine (not under /api/v1
// auth group).
func (h *Handler) RegisterPublic(r gin.IRouter) {
	r.POST("/api/v1/invitations/accept", h.accept)
}

type acceptBody struct {
	Token    string `json:"token"`
	Email    string `json:"email"`
	Name     string `json:"name,omitempty"`
	Password string `json:"password,omitempty"`
}

type acceptResponseDTO struct {
	TenantID string  `json:"tenant_id"`
	Scope    string  `json:"scope"`
	SiteID   *string `json:"site_id,omitempty"`
}

func (h *Handler) accept(c *gin.Context) {
	var body acceptBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	if body.Token == "" {
		httpx.Error(c, domain.Validation("token_required", "token is required"))
		return
	}
	if body.Email == "" {
		httpx.Error(c, domain.Validation("email_required", "email is required"))
		return
	}

	result, err := h.svc.Accept(c.Request.Context(), AcceptInput{
		Token:    body.Token,
		Email:    body.Email,
		Name:     body.Name,
		Password: body.Password,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}

	resp := acceptResponseDTO{
		TenantID: result.TenantID.String(),
		Scope:    result.Scope,
	}
	if result.SiteID != nil {
		v := result.SiteID.String()
		resp.SiteID = &v
	}
	c.JSON(http.StatusOK, resp)
}
