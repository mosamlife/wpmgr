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
//
// BOTH ROUTES CARRY IT, not just the one that writes. /consent does not require
// a prior /authorize: Approve re-resolves the client and re-matches the redirect
// from the POST body, and /register is unauthenticated, so the write is
// reachable with a self-registered client and a single POST -- no consent
// screen, no operator interaction, no /authorize call. Gating only the writing
// route would therefore gate nothing, and gating only /authorize would leave the
// write open. The pair is the boundary.
func (h *Handler) Register(r *gin.RouterGroup) {
	g := r.Group(oauthGroupPath, h.requireOrgScope())
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

	// POST "" mints a connection token: the DOCUMENTED HEADLESS PATH, because
	// no client documents a device-code flow and Claude Code cannot run browser
	// sign-in non-interactively. It is not a fallback for a failed OAuth dance.
	//
	// PermAPIKeyManage, THE SAME PERMISSION AS REVOKE, and that pairing is the
	// org-scope gate rather than a convenience. Both PermAPIKeyRead and
	// PermAPIKeyManage are members of authz.orgLevelPerms, so RequirePermission
	// refuses ANY site-constrained principal outright -- which is what closes
	// the hole here, since a grant is an organisation-wide credential with no
	// :siteId for RequireSiteAccess to key on. Reusing the API-key permissions
	// rather than minting a new one is what guarantees that membership is not a
	// step somebody has to remember: a fresh permission left out of orgLevelPerms
	// would look identical on this line and gate nothing.
	//
	// Minting is at least as sensitive as revoking -- revoke removes authority,
	// mint creates a long-lived bearer credential -- so it takes the manage
	// tier, never the read one.
	g.POST("", authz.RequirePermission(authz.PermAPIKeyManage), h.mintConnection)

	g.POST("/:"+connectionIDParam+"/revoke",
		authz.RequirePermission(authz.PermAPIKeyManage), h.revokeConnection)

	// GET /:connectionId/status -- the add-connection wizard's Step 8 and
	// Step 9 poll (S29). PermAPIKeyRead, the SAME permission as the list
	// above, because it reads the same object: a caller who may list the
	// organisation's connections may read the handshake state of one of them,
	// and a caller who may not must not learn it a row at a time. Both are
	// org-level perms, so a site-constrained principal is refused here for the
	// same reason it is refused on the list.
	g.GET("/:"+connectionIDParam+"/status",
		authz.RequirePermission(authz.PermAPIKeyRead), h.connectionStatus)

	// 405 rather than gin's bare 404 on a wrong verb, for the reason
	// RegisterPublic gives: a 404 reads as "not deployed", which is exactly how
	// the S6b-2 blocker presented and cost a debugging session. These carry NO
	// permission middleware on purpose -- the verb is wrong regardless of who is
	// asking, and answering 403 to a DELETE sends an operator to check a role
	// when the fix is to send a POST.
	houseMethodNotAllowedExcept(g, "", http.MethodGet, http.MethodPost)
	houseMethodNotAllowedExcept(g, "/:"+connectionIDParam+"/revoke", http.MethodPost)
	houseMethodNotAllowedExcept(g, "/:"+connectionIDParam+"/status", http.MethodGet)
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

// mintConnection answers POST /api/v1/mcp/connections.
//
// It returns 201 with the plaintext token in the body, ONCE. The plaintext is
// never put in a URL, a query string, a header or a log line -- a URL is
// written to browser history, proxy logs and Referer headers, and this
// credential outlives the session that asked for it by up to ninety days.
//
// THE HANDLER DOES NO AUTHORIZATION OF ITS OWN beyond re-reading the principal.
// The org-scope decision belongs to the service (and to the route's
// RequirePermission, and to the RLS beneath), so there is no fourth opinion
// here that could drift from the other three.
func (h *Handler) mintConnection(c *gin.Context) {
	principal, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		// Unreachable behind RequireAuth; refusing anyway, because a handler
		// that trusts its mount point is one refactor away from anonymous.
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}

	// Capped like the OAuth bodies. This one IS authenticated, so the cap is
	// not admission control -- it bounds a decoder fed a streamed body by a
	// caller who is already inside, which is cheap insurance rather than a
	// security boundary.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxOAuthBodyBytes)

	var body mintConnectionRequestDTO
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation(ErrCodeInvalidRequest,
			"the request body is not valid JSON"))
		return
	}

	// parseUUIDs REFUSES a malformed id rather than dropping it, and that is
	// the point on this path: uuid.Parse failure decays to uuid.Nil, and a
	// dropped tag id silently NARROWS the scope the operator thought they were
	// minting -- or empties it entirely, which then looks like a deliberate
	// choice. The error names which field so the wizard can point at it.
	tagIDs, err := parseUUIDs(body.ScopeTagIDs)
	if err != nil {
		httpx.Error(c, domain.Validation(ErrCodeInvalidSiteScope,
			"scope_tag_ids contains a value that is not a UUID"))
		return
	}
	siteIDs, err := parseUUIDs(body.ScopeSiteIDs)
	if err != nil {
		httpx.Error(c, domain.Validation(ErrCodeInvalidSiteScope,
			"scope_site_ids contains a value that is not a UUID"))
		return
	}

	caps := make([]Capability, 0, len(body.Capabilities))
	for _, name := range body.Capabilities {
		caps = append(caps, Capability(name))
	}

	out, err := h.svc.MintConnection(c.Request.Context(), MintConnectionRequest{
		// The WHOLE principal. Its Scope and AllowedSiteIDs are what route the
		// grant insert to a site-scoped transaction, so narrowing this to
		// (TenantID, UserID) would disarm the RLS layer beneath while still
		// compiling -- the same trap ApprovalRequest.Principal documents.
		Principal:    principal,
		Name:         body.Name,
		SiteScope:    SiteScopeRequest{Mode: SiteScopeMode(body.SiteScopeMode), TagIDs: tagIDs, SiteIDs: siteIDs},
		Capabilities: caps,
		// Forwarded as the pointer it arrived as, so "omitted" survives the
		// handler as nil rather than being flattened to "". The service
		// validates its shape and refuses a malformed one; the handler does not
		// second-guess it, for the same reason it does no authorization of its
		// own -- a fourth opinion is a fourth thing that can drift.
		SetupClient: body.SetupClient,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}

	// Never cached, and never stored by an intermediary: the body holds a
	// bearer credential. Same stance RFC 6749 5.1 takes on a token response,
	// applied here because the payload is the same kind of thing.
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.JSON(http.StatusCreated, toMintConnectionResponse(out))
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

// requireOrgScope is authz.RequireOrgScope's refusal in THIS package's error
// envelope. It is deliberately not authz.RequireOrgScope itself.
//
// The predicate is identical -- domain.Principal.IsSiteConstrained, the single
// definition every other site gate calls -- so there is no second opinion about
// who is constrained. Only the response shape differs, and it has to: the two
// routes below answer in the RFC 6749 section 5.2 envelope
// {error, error_description}, and the dashboard's consent screen parses exactly
// that (apps/web/src/features/mcp-consent/use-consent.ts reads `error` and
// `error_description`, falling back to "server_error" when neither is present).
// Aborting through the generic {code, message} responder would make a
// deliberate, correct refusal arrive at the screen as an unexplained server
// fault, which tells a collaborator the system is broken when the truth is that
// they may not approve this.
//
// The status is NOT softened to make the body prettier: oauthError maps
// ErrCodeAccessDenied to 403. 401 would be wrong here -- the credential is
// valid and re-presenting it cannot help, so inviting a retry is inviting a
// pointless one.
//
// No OAuth CLIENT ever reads this body. authorization_endpoint is opened in the
// user's browser and /consent is posted by our own screen; a client learns of a
// refusal from the redirect the screen builds (buildDenialTarget), carrying
// error=access_denied and the original state. The envelope matters for the
// screen, not for the protocol.
func (h *Handler) requireOrgScope() gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := domain.PrincipalFromContext(c.Request.Context())
		if !ok {
			// Mirrors the handlers' own unauthenticated answer rather than the
			// generic 401, for the same envelope reason.
			c.AbortWithStatusJSON(http.StatusUnauthorized, oauthErrorDTO{
				Err: "access_denied", ErrDesc: "sign in to authorize a connection"})
			return
		}
		if p.IsSiteConstrained() {
			status, dto := oauthError(domain.Forbidden(ErrCodeAccessDenied,
				"site-scoped access cannot authorize an organisation-level connection"))
			c.AbortWithStatusJSON(status, dto)
			return
		}
		c.Next()
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
