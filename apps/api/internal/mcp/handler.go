package mcp

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
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
	// regLimit bounds anonymous registration. Never nil after NewHandler; a nil
	// receiver refuses rather than allows (see registrationLimiter.allow).
	regLimit *registrationLimiter
}

// NewHandler builds the OAuth handler with its registration limiter already
// armed. The limiter is constructed HERE rather than injected with a nil
// default, so there is no assembly order in which the endpoint is mounted
// unlimited: an unmounted limiter would be a wiring failure that presents as
// working registration, which is the failure mode this whole slice is most
// exposed to.
func NewHandler(svc *Service) *Handler {
	return &Handler{
		svc:      svc,
		regLimit: newRegistrationLimiter(RegisterGlobalPerMin, RegisterPerPeerPerMin),
	}
}

// maxOAuthBodyBytes caps the two unauthenticated POST bodies.
//
// Registration metadata is a handful of short strings and a redirect_uri array;
// a token request is five short form fields. 16 KiB is orders of magnitude
// above either and still bounds an anonymous caller streaming a body at a
// decoder. Matches the house pattern (rum/handler.go:261,
// site/monitoring_handler.go:212, backup/agent_handler.go:96, transport.go).
const maxOAuthBodyBytes = 16 << 10

// RegisterPublic mounts the two unauthenticated OAuth endpoints, AND the
// method-not-allowed fallbacks for all four OAuth paths including the two
// mounted by Register.
//
// The 405 fallbacks live here, unauthenticated, on purpose, and for the same
// reason TransportHandler.Register gives: the verb is wrong regardless of who
// is asking, and answering 401 to a GET on /consent sends an operator to check
// a credential when the fix is to send a POST. Answering 404 is worse still --
// it reads as "not deployed", which is exactly how the S6b-2 blocker presented.
//
// This couples the two mount points: Register's routes get their 405 siblings
// from here, so a caller that mounts one without the other leaves GET
// /authorize answering 405 for every verb including its own. server.New mounts
// both from one Deps field, which is what keeps that unreachable.
func (h *Handler) RegisterPublic(r *gin.RouterGroup) {
	// oauthGroupPath, not a literal: discovery.go advertises the absolute form
	// of these paths and both halves must read the same constant.
	g := r.Group(oauthGroupPath)
	g.POST("/register", h.register)
	g.POST("/token", h.token)

	// Registered explicitly per verb rather than via HandleMethodNotAllowed,
	// because that flag is engine-global and would change the response of every
	// other route in the API as a side effect of this slice.
	methodNotAllowedExcept(g, "/register", http.MethodPost)
	methodNotAllowedExcept(g, "/token", http.MethodPost)
	methodNotAllowedExcept(g, "/authorize", http.MethodGet)
	methodNotAllowedExcept(g, "/consent", http.MethodPost)
}

// Register mounts the two operator-authenticated endpoints. The caller's group
// is already behind session auth AND authz.RequireAuth/RequireTenant; the
// handlers below re-check the principal anyway, because a handler that trusts
// its mount point is one refactor away from being anonymous.
//
// RequireOrgScope is applied HERE, on the group, rather than left to the
// caller. A grant is an ORG-LEVEL object: it carries a site_scope_mode of
// 'all' | 'tags' | 'sites' chosen by the approver, and 'all' resolves to every
// site in the organisation at read time. There is no :siteId to bind, so
// RequireSiteAccess cannot express the boundary and RequireOrgScope is the
// whole gate -- the same reason the update-run orchestrator and the fleet
// rollups carry it.
//
// It sits on the group and not on the caller's mount so that every mount of
// these two routes gets it, including the integration harness. A gate that
// only exists in server.New is a gate no test exercises.
func (h *Handler) Register(r *gin.RouterGroup) {
	g := r.Group(oauthGroupPath, authz.RequireOrgScope())
	g.GET("/authorize", h.authorize)
	g.POST("/consent", h.consent)
}

// allVerbs is every method the 405 fallback covers. CONNECT and TRACE are
// omitted: gin's router does not serve them and net/http handles CONNECT
// separately.
var allVerbs = []string{
	http.MethodGet,
	http.MethodHead,
	http.MethodPost,
	http.MethodPut,
	http.MethodPatch,
	http.MethodDelete,
	http.MethodOptions,
}

// methodNotAllowedExcept mounts a JSON 405 on every verb except the ones the
// path actually serves, naming the supported verb in Allow.
func methodNotAllowedExcept(g *gin.RouterGroup, path string, supported ...string) {
	allowed := strings.Join(supported, ", ")
	for _, verb := range allVerbs {
		if slices.Contains(supported, verb) {
			continue
		}
		g.Handle(verb, path, func(c *gin.Context) {
			c.Header("Allow", allowed)
			c.JSON(http.StatusMethodNotAllowed, oauthErrorDTO{
				Err:     "invalid_request",
				ErrDesc: "this endpoint accepts " + allowed + " only",
			})
		})
	}
}

// register is RFC 7591 dynamic client registration.
//
// UNAUTHENTICATED BY SPECIFICATION, so the two guards below are the entire
// admission control and both run BEFORE the body is read: the rate limit, then
// the body cap. See register_limit.go for why the limiter key is RemoteIP and
// never ClientIP.
func (h *Handler) register(c *gin.Context) {
	// RemoteIP, not ClientIP. ClientIP returns an attacker-supplied header value
	// under this engine's configuration; keying the limiter on it would enforce
	// nothing while looking correct.
	if ok, retryAfter := h.regLimit.allow(c.RemoteIP()); !ok {
		secs := int(retryAfter.Seconds())
		if secs < 1 {
			secs = 1
		}
		c.Header("Retry-After", strconv.Itoa(secs))
		c.JSON(http.StatusTooManyRequests, oauthErrorDTO{
			Err:     "invalid_request",
			ErrDesc: "too many client registrations; retry later",
		})
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxOAuthBodyBytes)

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
		// The whole principal. Its Scope and AllowedSiteIDs are what route the
		// grant insert to a site-scoped transaction, so narrowing this to
		// (TenantID, UserID) here would disarm the RLS layer beneath.
		Principal: principal,
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
	// Unauthenticated like /register, so the body is capped here too. It is NOT
	// rate limited: a code is single-use, high-entropy and short-lived, so there
	// is nothing here to grind at, and a limiter on this endpoint would refuse
	// legitimate exchanges during the one moment a user is watching.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxOAuthBodyBytes)

	var body tokenRequestDTO
	if err := c.ShouldBind(&body); err != nil {
		c.JSON(http.StatusBadRequest, oauthErrorDTO{
			Err: "invalid_request", ErrDesc: "the request could not be parsed"})
		return
	}

	// DETERMINE HOW THE CREDENTIAL ARRIVED, and pass that to the service so the
	// registered token_endpoint_auth_method can be enforced rather than merely
	// recorded. Only the handler can see the transport.
	//
	// This used to let Basic silently win when both were present, which
	// discarded the credential source entirely: a client_secret_basic
	// registration could authenticate with body parameters and vice versa, and
	// the stored decision governed nothing. RFC 6749 2.3.1 also forbids using
	// more than one mechanism per request, so presenting both is refused rather
	// than resolved by precedence.
	clientID, clientSecret := body.ClientID, body.ClientSecret
	authVia := "none"
	basicID, basicSecret, hasBasic := c.Request.BasicAuth()
	hasPost := strings.TrimSpace(body.ClientSecret) != ""

	switch {
	case hasBasic && hasPost:
		authVia = AuthViaMultiple
	case hasBasic:
		clientID, clientSecret, authVia = basicID, basicSecret, "client_secret_basic"
	case hasPost:
		authVia = "client_secret_post"
	}
	// client_id may still travel in the body for a public client, which sends no
	// secret at all; that is the "none" path and it is unchanged.

	out, err := h.svc.Exchange(c.Request.Context(), TokenRequest{
		GrantType:    body.GrantType,
		Code:         body.Code,
		RedirectURI:  body.RedirectURI,
		ClientID:      clientID,
		ClientSecret:  clientSecret,
		CodeVerifier:  body.CodeVerifier,
		ClientAuthVia: authVia,
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
