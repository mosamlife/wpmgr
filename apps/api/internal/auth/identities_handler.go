package auth

// identities_handler.go -- the connected accounts surface at settings/security,
// beside 2FA, passkeys and trusted devices.
//
//	GET    /auth/me/identities            what this account can sign in with
//	DELETE /auth/me/identities/:provider  disconnect one provider
//	POST   /auth/me/password/set          add a password to an account with none
//
// All three act on the CALLER's own account only: there is no user id anywhere
// in these routes, so no request can name somebody else's account.
//
// All three also require a human session specifically (PrincipalUser), not
// merely an authenticated principal. An API key is a machine credential that
// carries the same role matrix as a user, and it must not be able to change
// which humans can sign in to the account it belongs to, nor mint itself a
// password. Same gate as /auth/me and /auth/me/password.
//
// Hand-written Gin, matching handler.go.

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// RegisterIdentities mounts the connected accounts routes on the /auth group.
// Called from handler.go Register.
func (h *Handler) RegisterIdentities(g gin.IRouter) {
	g.GET("/me/identities", h.listIdentities)
	g.DELETE("/me/identities/:provider", h.unlinkIdentity)
	// Deliberately NOT a second shape of POST /auth/me/password. Adding a first
	// password and changing an existing one have different authorisation (a
	// session versus the current password) and different failure modes, and
	// folding them into one endpoint that branches on a missing field is how
	// the weaker of the two ends up accepting the stronger one's requests.
	g.POST("/me/password/set", h.setPassword)
}

// connectedIdentity is the wire shape of one linked provider. Subject and
// issuer are the provider's internal identifiers and are deliberately absent:
// they are of no use to the person reading the page and they are exactly what
// an attacker would want to know to forge a matching identity.
type connectedIdentity struct {
	Provider      string  `json:"provider"`
	Email         string  `json:"email"`
	EmailVerified bool    `json:"email_verified"`
	CreatedAt     string  `json:"created_at"`
	LastLoginAt   *string `json:"last_login_at"`
}

type connectedAccountsResponse struct {
	HasPassword bool                `json:"has_password"`
	CanUnlink   bool                `json:"can_unlink"`
	Items       []connectedIdentity `json:"items"`
}

// listIdentities handles GET /auth/me/identities.
func (h *Handler) listIdentities(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	methods, err := h.svc.SignInMethods(c.Request.Context(), p.UserID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	items := make([]connectedIdentity, 0, len(methods.Identities))
	for _, id := range methods.Identities {
		item := connectedIdentity{
			Provider:      id.Provider,
			Email:         id.Email,
			EmailVerified: id.EmailVerified,
			CreatedAt:     id.CreatedAt.UTC().Format(time.RFC3339),
		}
		if id.LastLoginAt != nil {
			s := id.LastLoginAt.UTC().Format(time.RFC3339)
			item.LastLoginAt = &s
		}
		items = append(items, item)
	}
	c.JSON(http.StatusOK, connectedAccountsResponse{
		HasPassword: methods.HasPassword,
		// The same answer the server will give when a Disconnect is actually
		// attempted, so the page can hide a button that would only be refused.
		// It is a display hint, never the enforcement: decideUnlink re-decides
		// on every request.
		CanUnlink: methods.CanUnlink(),
		Items:     items,
	})
}

// unlinkIdentity handles DELETE /auth/me/identities/:provider. Refuses with 409
// when it would leave the account with no way to sign in.
func (h *Handler) unlinkIdentity(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	if err := h.svc.UnlinkIdentity(c.Request.Context(), p.UserID, c.Param("provider")); err != nil {
		httpx.Error(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// setPasswordBody is the request body for POST /auth/me/password/set.
type setPasswordBody struct {
	Password string `json:"password"`
}

// setPassword handles POST /auth/me/password/set -- give a social-only account
// a password so it no longer depends on the provider.
func (h *Handler) setPassword(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	var body setPasswordBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	if err := h.svc.SetInitialPassword(c.Request.Context(), p.UserID, body.Password, clientAddr(c)); err != nil {
		httpx.Error(c, err)
		return
	}
	// Same reason as changePassword: the write stamps password_changed_at, and
	// without this the session that just made the request would be the first
	// one the Authenticator throws out.
	h.sessions.RefreshAuthAt(c.Request.Context())
	c.Status(http.StatusNoContent)
}
