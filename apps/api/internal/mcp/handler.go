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
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
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
func (h *Handler) Register(r *gin.RouterGroup) {
	g := r.Group(oauthGroupPath)
	g.GET("/authorize", h.authorize)
	g.POST("/consent", h.consent)
}

// RegisterConnections mounts the OPERATOR-FACING connection management surface
// (S16, design Step 10):
//
//	GET  /api/v1/mcp/connections
//	POST /api/v1/mcp/connections/:connectionId/revoke
//
// It is a THIRD mount point, separate from Register and RegisterPublic, because
// these are house API routes rather than OAuth endpoints: they answer in the
// house error envelope, they are read by the dashboard, and they carry per-route
// RBAC that the OAuth endpoints cannot (RFC 7591 registration is anonymous by
// specification).
//
// THE PERMISSIONS ARE THE SITE-SCOPE GATE, AND THE CHOICE OF THESE TWO IS
// DELIBERATE. There is no RequireSiteAccess here and there must not be: a grant
// is an ORGANISATION-wide credential with no :siteId in its path, so a per-site
// gate has nothing to key on. What closes the hole instead is that
// PermAPIKeyRead and PermAPIKeyManage are both members of authz.orgLevelPerms,
// which makes RequirePermission refuse ANY site-constrained principal outright,
// regardless of the role it holds on the one site it can see. That is the
// permission-layer half of what mcp_grants_site_scope_select does in the
// database, and reusing the API-key permissions rather than minting new ones is
// what guarantees the orgLevelPerms membership is not a step somebody has to
// remember -- a fresh permission that was left out of that map would look
// identical here and gate nothing.
//
// An MCP grant is the same class of object as an API key -- a long-lived,
// organisation-scoped bearer credential -- so the trust tier matches too: admin+
// to read, admin+ to revoke (authz.minRoleFor).
//
// POST /revoke AND NOT DELETE. Revocation is not a delete: m124 Decision 2 keeps
// the row precisely so last_used_at and revoked_at stay readable afterwards.
// Naming it DELETE would promise a removal this endpoint does not perform.
func (h *Handler) RegisterConnections(r *gin.RouterGroup) {
	g := r.Group(connectionsGroupPath)

	g.GET("", authz.RequirePermission(authz.PermAPIKeyRead), h.listConnections)
	g.POST("/:"+connectionIDParam+"/revoke",
		authz.RequirePermission(authz.PermAPIKeyManage), h.revokeConnection)

	// 405 rather than gin's bare 404 on a wrong verb, for the reason
	// RegisterPublic gives: a 404 reads as "not deployed", which is exactly how
	// the S6b-2 blocker presented and cost a debugging session. These carry NO
	// permission middleware on purpose -- the verb is wrong regardless of who is
	// asking, and answering 403 to a DELETE sends an operator to check a role
	// when the fix is to send a POST.
	houseMethodNotAllowedExcept(g, "", http.MethodGet)
	houseMethodNotAllowedExcept(g, "/:"+connectionIDParam+"/revoke", http.MethodPost)
}

// listConnections answers GET /api/v1/mcp/connections.
//
// There is no branch here that can turn a failure into an empty list: every
// error path goes to httpx.Error with a non-2xx, and the only 200 is built from
// a slice the service returned alongside a nil error.
func (h *Handler) listConnections(c *gin.Context) {
	principal, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		// Unreachable behind RequireAuth; refusing anyway, because a handler
		// that trusts its mount point is one refactor away from anonymous.
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}

	conns, err := h.svc.ListConnections(c.Request.Context(), principal)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	c.JSON(http.StatusOK, toConnectionListDTO(conns))
}

// revokeConnection answers POST /api/v1/mcp/connections/:connectionId/revoke.
//
// It revokes the grant AND every live token under it. The service refuses to
// express the half; see Service.RevokeConnection.
func (h *Handler) revokeConnection(c *gin.Context) {
	principal, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}

	grantID, err := uuid.Parse(c.Param(connectionIDParam))
	if err != nil {
		// 404 AND NOT 400, matching authz.RequireSiteAccess's stance on a
		// malformed :siteId. A caller who cannot see this organisation's
		// connections must get the same answer for a malformed id, an id from
		// another organisation, and an id that never existed -- otherwise the
		// status code is an existence oracle.
		httpx.Error(c, domain.NotFound(ErrCodeConnectionNotFound, "no such connection"))
		return
	}

	out, err := h.svc.RevokeConnection(c.Request.Context(), principal, grantID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	// 200 for all three success shapes, including the idempotent retry that
	// changed nothing. The counts say what happened; the status says the end
	// state the caller asked for now holds. Answering 404 or 500 on a
	// zero-count success would report a correctly revoked credential as a
	// failure and invite the operator to believe it is still live.
	c.JSON(http.StatusOK, revokeResponseDTO{
		Status:         string(GrantStatusRevoked),
		GrantsRevoked:  out.GrantsRevoked,
		TokensRevoked:  out.TokensRevoked,
		AlreadyRevoked: out.AlreadyRevoked,
	})
}

// houseMethodNotAllowedExcept is methodNotAllowedExcept for house API routes.
//
// Same shape, different envelope, and the duplication is the point: the OAuth
// version answers in oauthErrorDTO because an OAuth client library parses that
// shape, while these routes are read by the dashboard and must answer in the
// house envelope {"code","message"}.
//
// It writes that envelope DIRECTLY rather than going through httpx.Error,
// because domain.Kind has no 405 member -- there is no domain error that maps
// to StatusMethodNotAllowed, and inventing one would put a transport-level
// concept into the domain vocabulary to serve one wrong-verb response. The
// field names are kept identical to httpx's errorEnvelope so a client parses
// one shape for every error on this surface.
func houseMethodNotAllowedExcept(g *gin.RouterGroup, path string, supported ...string) {
	allowed := strings.Join(supported, ", ")
	for _, verb := range allVerbs {
		if slices.Contains(supported, verb) {
			continue
		}
		g.Handle(verb, path, func(c *gin.Context) {
			c.Header("Allow", allowed)
			c.AbortWithStatusJSON(http.StatusMethodNotAllowed, gin.H{
				"code":    "method_not_allowed",
				"message": "this endpoint accepts " + allowed + " only",
			})
		})
	}
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
