package mcp

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Handler serves the OAuth surface for the read-only MCP endpoint.
//
// The routes split by who authenticates, and the split is the security
// boundary rather than a routing convenience:
//
//   - RegisterPublic mounts /register and /token. Both are UNAUTHENTICATED by
//     specification -- RFC 7591 registration precedes any human, and the token
//     endpoint authenticates by presenting a code plus a PKCE verifier, not by
//     a session. Neither may ever read a tenant from a session.
//   - Register mounts /authorize and /consent. Both require an authenticated
//     operator, because consent is the entire authorization: the grant belongs
//     to the organisation of the human who approved it.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// RegisterPublic mounts the two unauthenticated OAuth endpoints.
func (h *Handler) RegisterPublic(r *gin.RouterGroup) {
	g := r.Group("/oauth/mcp")
	g.POST("/register", h.register)
	g.POST("/token", h.token)
}

// Register mounts the two operator-authenticated endpoints. The caller's group
// is already behind session auth.
func (h *Handler) Register(r *gin.RouterGroup) {
	g := r.Group("/oauth/mcp")
	g.GET("/authorize", h.authorize)
	g.POST("/consent", h.consent)
}

// register is RFC 7591 dynamic client registration.
func (h *Handler) register(c *gin.Context) {
	var body registrationRequestDTO
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, oauthErrorDTO{
			Err: "invalid_client_metadata", ErrDesc: "the request body is not valid JSON"})
		return
	}

	out, err := h.svc.Register(c.Request.Context(), RegistrationRequest{
		RedirectURIs:            body.RedirectURIs,
		ClientName:              body.ClientName,
		ClientURI:               body.ClientURI,
		TokenEndpointAuthMethod: body.TokenEndpointAuthMethod,
	})
	if err != nil {
		status, dto := oauthError(err)
		c.JSON(status, dto)
		return
	}
	c.JSON(http.StatusCreated, toRegistrationResponse(out))
}

// authorize validates the request and returns the consent screen's contents.
// It mints nothing.
func (h *Handler) authorize(c *gin.Context) {
	if _, ok := domain.PrincipalFromContext(c.Request.Context()); !ok {
		c.JSON(http.StatusUnauthorized, oauthErrorDTO{
			Err: "access_denied", ErrDesc: "sign in to authorize a connection"})
		return
	}

	out, err := h.svc.Authorize(c.Request.Context(), AuthorizeRequest{
		ResponseType:        c.Query("response_type"),
		ClientID:            c.Query("client_id"),
		RedirectURI:         c.Query("redirect_uri"),
		Scope:               c.Query("scope"),
		State:               c.Query("state"),
		CodeChallenge:       c.Query("code_challenge"),
		CodeChallengeMethod: c.Query("code_challenge_method"),
	})
	if err != nil {
		status, dto := oauthError(err)
		c.JSON(status, dto)
		return
	}
	c.JSON(http.StatusOK, toConsentResponse(out))
}

// consent records a human's approval and returns the code to hand back.
func (h *Handler) consent(c *gin.Context) {
	principal, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		c.JSON(http.StatusUnauthorized, oauthErrorDTO{
			Err: "access_denied", ErrDesc: "sign in to authorize a connection"})
		return
	}

	var body approvalRequestDTO
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, oauthErrorDTO{
			Err: "invalid_request", ErrDesc: "the request body is not valid JSON"})
		return
	}

	tagIDs, err := parseUUIDs(body.ScopeTagIDs)
	if err != nil {
		c.JSON(http.StatusBadRequest, oauthErrorDTO{
			Err: "invalid_request", ErrDesc: "scope_tag_ids contains a value that is not a UUID"})
		return
	}
	siteIDs, err := parseUUIDs(body.ScopeSiteIDs)
	if err != nil {
		c.JSON(http.StatusBadRequest, oauthErrorDTO{
			Err: "invalid_request", ErrDesc: "scope_site_ids contains a value that is not a UUID"})
		return
	}

	scopes := make([]Scope, 0, len(body.Scopes))
	for _, s := range body.Scopes {
		scopes = append(scopes, Scope(s))
	}

	out, err := h.svc.Approve(c.Request.Context(), ApprovalRequest{
		TenantID: principal.TenantID,
		UserID:   principal.UserID,
		Consent: ConsentContext{
			ClientID:            body.ClientID,
			RedirectURI:         body.RedirectURI,
			Scopes:              scopes,
			State:               body.State,
			CodeChallenge:       body.CodeChallenge,
			CodeChallengeMethod: body.CodeChallengeMethod,
		},
		GrantName: body.GrantName,
		SiteScope: SiteScopeRequest{
			Mode:    SiteScopeMode(body.SiteScopeMode),
			TagIDs:  tagIDs,
			SiteIDs: siteIDs,
		},
	})
	if err != nil {
		status, dto := oauthError(err)
		c.JSON(status, dto)
		return
	}

	c.JSON(http.StatusOK, approvalResponseDTO{
		GrantID:     out.GrantID.String(),
		Code:        out.Code,
		RedirectURI: out.RedirectURI,
		State:       out.State,
	})
}

// token is the RFC 6749 4.1.3 access token request. It accepts form encoding,
// which is what the RFC specifies and what every client library sends, and
// JSON for convenience.
func (h *Handler) token(c *gin.Context) {
	var body tokenRequestDTO
	if err := c.ShouldBind(&body); err != nil {
		c.JSON(http.StatusBadRequest, oauthErrorDTO{
			Err: "invalid_request", ErrDesc: "the request could not be parsed"})
		return
	}

	// RFC 6749 2.3.1 prefers HTTP Basic for client_secret_basic and permits the
	// body for client_secret_post. Basic wins when both are present, so a body
	// parameter cannot override a header the client actually authenticated with.
	clientID, clientSecret := body.ClientID, body.ClientSecret
	if id, secret, ok := c.Request.BasicAuth(); ok {
		clientID, clientSecret = id, secret
	}

	out, err := h.svc.Exchange(c.Request.Context(), TokenRequest{
		GrantType:    body.GrantType,
		Code:         body.Code,
		RedirectURI:  body.RedirectURI,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		CodeVerifier: body.CodeVerifier,
	})
	if err != nil {
		status, dto := oauthError(err)
		c.JSON(status, dto)
		return
	}

	// RFC 6749 5.1: token responses must not be cached.
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.JSON(http.StatusOK, tokenResponseDTO{
		AccessToken: out.AccessToken,
		TokenType:   out.TokenType,
		ExpiresIn:   out.ExpiresIn,
		Scope:       out.Scope,
	})
}

// parseUUIDs refuses a malformed id rather than dropping it. A dropped id
// silently narrows or widens the scope the operator thought they approved,
// depending on the mode, and uuid.Nil is what a failed parse decays to.
func parseUUIDs(in []string) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(in))
	for _, raw := range in {
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}
