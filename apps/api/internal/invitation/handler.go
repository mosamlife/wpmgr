package invitation

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

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

// acceptIntentHeader carries the affirmative act that a session cookie cannot.
//
// It is not a secret and does not need to be. Its whole value is that a browser
// will not send it across origins: markup cannot set request headers at all, so
// no form, image or iframe on another site can produce it, and a cross-origin
// fetch that sets it becomes a preflighted request, which this route answers
// for no origin (the only CORS-enabled route on this server is the RUM beacon,
// and it is mounted outside this group). What remains is a request issued by a
// script running on this install's own page, which is precisely the "the person
// meant to do this" that a password used to supply on this route.
//
// SameSite=Lax on the session cookie is the other half, and deliberately not
// the only half. Lax already withholds the cookie from a cross-site POST, and
// Chrome's two-minute Lax-allowing-unsafe window does not apply here because it
// covers only cookies that DEFAULT to Lax by omitting the attribute, while this
// one declares SameSite=Lax explicitly (see auth.SessionManager). Resting a
// membership grant on that distinction alone would be resting it on one browser
// vendor's read of one edge of one spec, so this header holds it up on a second
// mechanism that fails independently.
const acceptIntentHeader = "X-WPMgr-Invite-Accept"

// authorizingSessionUser returns the user whose session may stand in for the
// invitation password, or uuid.Nil when none may.
//
// The route is mounted on the session-carrying group, so a signed-in caller
// arrives with a principal. It is read from the request context and NEVER from
// the body: an id a caller could type would make this a way to accept as
// somebody else.
//
// Two things must both hold. There must be a user principal, and the request
// must carry the intent header, because a session cookie on its own is ambient
// authority that travels on requests the person never initiated. Any non-empty
// header value counts: its presence is the whole signal, and checking a magic
// value would only invite the belief that the value is a credential.
//
// When either is missing the caller is treated as anonymous and the unchanged
// password path applies, so a forged request can do no more than an
// unauthenticated one, which is nothing.
func authorizingSessionUser(c *gin.Context) uuid.UUID {
	if c.GetHeader(acceptIntentHeader) == "" {
		return uuid.Nil
	}
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser {
		return uuid.Nil
	}
	return p.UserID
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
	ClientID *string `json:"client_id,omitempty"`
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
		Token:         body.Token,
		Email:         body.Email,
		Name:          body.Name,
		Password:      body.Password,
		SessionUserID: authorizingSessionUser(c),
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
	if result.ClientID != nil {
		v := result.ClientID.String()
		resp.ClientID = &v
	}
	c.JSON(http.StatusOK, resp)
}
