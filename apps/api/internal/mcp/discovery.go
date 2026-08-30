package mcp

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// Mounted paths — the single source of truth
// ---------------------------------------------------------------------------

// APIV1Prefix is the group the OAuth endpoints are mounted under. server.New
// passes exactly engine.Group(APIV1Prefix) to Handler.RegisterPublic and
// derives the operator group from the same prefix, so the constants below are
// the paths that actually exist rather than a second description of them.
//
// THE POINT OF THESE CONSTANTS IS THAT THE DISCOVERY DOCUMENT CANNOT DRIFT
// FROM THE ROUTER. A discovery document is a promise a client acts on without
// ever seeing this repository: it opens a browser at authorization_endpoint
// and posts to token_endpoint. A hand-written list here that fell one rename
// behind the router would send every GUI client to a 404 at handshake time,
// which is a failure the server never observes and the user cannot diagnose.
// Both halves now read the same constants, and TestAdvertisedEndpointsAreMounted
// walks the real gin route table to prove it.
const APIV1Prefix = "/api/v1"

// oauthGroupPath is the group Handler.RegisterPublic and Handler.Register open
// under APIV1Prefix. Kept unexported: callers mount by calling those methods,
// never by rebuilding the path.
const oauthGroupPath = "/oauth/mcp"

// connectionsGroupPath is the group Handler.RegisterConnections opens under
// APIV1Prefix, and connectionIDParam is the path parameter it binds.
//
// They live here beside the OAuth paths, and as CONSTANTS rather than literals,
// for the reason the block above gives: a path that is written twice is a path
// that drifts. infra/urlmap.yaml routes these under its `/api/*` rule, so no
// new url-map path rule is required -- unlike the root-mounted /mcp and
// /.well-known documents, which each needed one and were unreachable until they
// got it.
const (
	connectionsGroupPath = "/mcp/connections"
	connectionIDParam    = "connectionId"
)

// ConnectionsPath and ConnectionRevokePathFor are the absolute forms, exported
// so a test asserts the mounted route rather than restating the string.
const ConnectionsPath = APIV1Prefix + connectionsGroupPath

// ConnectionRevokePathFor builds the revoke path for one connection id.
func ConnectionRevokePathFor(id string) string {
	return ConnectionsPath + "/" + id + "/revoke"
}

// The four OAuth paths, absolute, exactly as mounted.
const (
	RegisterPath  = APIV1Prefix + oauthGroupPath + "/register"
	AuthorizePath = APIV1Prefix + oauthGroupPath + "/authorize"
	TokenPath     = APIV1Prefix + oauthGroupPath + "/token"
	ConsentPath   = APIV1Prefix + oauthGroupPath + "/consent"
)

// The two discovery paths, mounted on the ROOT engine.
//
// WellKnownProtectedResourceMCPPath is the RFC 9728 section 3.1 path-insertion
// form for a resource whose identifier carries a path: an MCP endpoint at
// https://host/mcp advertises its metadata at
// https://host/.well-known/oauth-protected-resource/mcp. The MCP specification
// (revision 2025-11-25, "Protected Resource Metadata Discovery Requirements")
// has clients try that form FIRST and fall back to the root form, so BOTH are
// served and both return the same document. Serving only the root form works
// but costs every client an extra 404 on its first connection.
const (
	WellKnownAuthorizationServerPath  = "/.well-known/oauth-authorization-server"
	WellKnownProtectedResourcePath    = "/.well-known/oauth-protected-resource"
	WellKnownProtectedResourceMCPPath = WellKnownProtectedResourcePath + TransportPath
)

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// DiscoveryHandler serves the two unauthenticated OAuth discovery documents a
// GUI client fetches to find this server's authorization endpoints:
//
//   - RFC 8414 authorization server metadata, which the MCP revision floor
//     (2025-03-26) requires a client to fetch at the ROOT of the origin hosting
//     the MCP endpoint, with any path component of the MCP URL discarded;
//   - RFC 9728 protected resource metadata, which revision 2025-11-25 made
//     mandatory for MCP servers ("MCP servers MUST implement OAuth 2.0
//     Protected Resource Metadata") and which points at the authorization
//     server via authorization_servers.
//
// Both revisions are served at once because the two documents answer different
// questions and a client of either revision finds what it looks for.
type DiscoveryHandler struct {
	// issuer is the ORIGIN of the configured public base URL: scheme, host and
	// port, with no path. RFC 8414 section 3.3 requires the advertised issuer
	// to be the exact prefix the well-known URI was built from, and this
	// handler is mounted at the root of the origin.
	issuer string
	// base is the configured public base URL with any trailing slash removed.
	// Endpoint URLs are built from it rather than from issuer so that a
	// sub-path install still names reachable endpoints.
	base string
	// unusableReason is non-empty when the configured base URL was absent or
	// could not be parsed into an origin. Both documents then answer 503 and
	// name the variable to set.
	//
	// THE ABSENT CASE IS THE WHOLE REASON THIS FIELD EXISTS. Falling back to a
	// constant issuer, to the request's own Host header, or to a relative URL
	// would all produce a document that parses, validates and sends the client
	// to somewhere real and wrong: a self-hosted install would hand out
	// https://app.wpmgr.app as its own issuer, and the operator would see a
	// client fail against someone else's server. A missing origin is an
	// absence, and the answer to an absence is a refusal that names it.
	unusableReason string
}

// NewDiscoveryHandler derives the issuer from configuration.
//
// publicBaseURL is cfg.PublicBaseURL (WPMGR_PUBLIC_BASE_URL), the one typed
// value every outward-facing URL in this product is built from. It is NOT a
// constant and NOT the request Host header: the Host header is attacker
// controlled behind a proxy that does not pin it, and a constant is wrong for
// every self-hosted install.
func NewDiscoveryHandler(publicBaseURL string) *DiscoveryHandler {
	raw := strings.TrimSpace(publicBaseURL)
	if raw == "" {
		return &DiscoveryHandler{
			unusableReason: "WPMGR_PUBLIC_BASE_URL is not configured, so this server cannot state its own issuer",
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return &DiscoveryHandler{
			unusableReason: "WPMGR_PUBLIC_BASE_URL is not an absolute http(s) URL, so this server cannot state its own issuer",
		}
	}
	return &DiscoveryHandler{
		issuer: u.Scheme + "://" + u.Host,
		base:   u.Scheme + "://" + u.Host + strings.TrimSuffix(u.Path, "/"),
	}
}

// Register mounts the discovery documents on the ROOT engine, unauthenticated.
//
// Unauthenticated is not a relaxation: a client fetches these BEFORE it holds
// any credential — discovering how to get one is the entire purpose — and both
// documents contain only this server's own public endpoint URLs and the
// closed scope vocabulary. There is nothing here a caller could not learn by
// reading the connect screen.
func (h *DiscoveryHandler) Register(r gin.IRouter) {
	wk := r.Group("/.well-known")

	for _, p := range []string{
		strings.TrimPrefix(WellKnownAuthorizationServerPath, "/.well-known"),
	} {
		wk.GET(p, h.authorizationServerMetadata)
		wk.HEAD(p, h.authorizationServerMetadata)
		wk.OPTIONS(p, h.preflight)
		methodNotAllowedExcept(wk, p, http.MethodGet, http.MethodHead, http.MethodOptions)
	}

	for _, p := range []string{
		strings.TrimPrefix(WellKnownProtectedResourcePath, "/.well-known"),
		strings.TrimPrefix(WellKnownProtectedResourceMCPPath, "/.well-known"),
	} {
		wk.GET(p, h.protectedResourceMetadata)
		wk.HEAD(p, h.protectedResourceMetadata)
		wk.OPTIONS(p, h.preflight)
		methodNotAllowedExcept(wk, p, http.MethodGet, http.MethodHead, http.MethodOptions)
	}
}

// ---------------------------------------------------------------------------
// Documents
// ---------------------------------------------------------------------------

// authorizationServerMetadataDTO is RFC 8414 section 2. Every field is one this
// server actually implements; there is deliberately no jwks_uri (the connection
// token is opaque and nothing here signs a JWT), no revocation_endpoint and no
// introspection_endpoint, because neither is mounted and advertising an
// unmounted endpoint is the drift this file exists to prevent.
type authorizationServerMetadataDTO struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	ScopesSupported                   []string `json:"scopes_supported"`
}

// protectedResourceMetadataDTO is RFC 9728 section 2.
type protectedResourceMetadataDTO struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
	ResourceName           string   `json:"resource_name"`
}

// AuthorizationServerMetadata builds the RFC 8414 document. Exported so the
// drift test can compare it against the live route table without going through
// HTTP first.
func (h *DiscoveryHandler) AuthorizationServerMetadata() authorizationServerMetadataDTO {
	return authorizationServerMetadataDTO{
		Issuer:                h.issuer,
		AuthorizationEndpoint: h.base + AuthorizePath,
		TokenEndpoint:         h.base + TokenPath,
		RegistrationEndpoint:  h.base + RegisterPath,
		// Read from the same values Service.Authorize and Service.Exchange
		// compare against. A second literal here is how a server comes to
		// advertise a grant it refuses.
		ResponseTypesSupported: SupportedResponseTypes(),
		GrantTypesSupported:    SupportedGrantTypes(),
		// S256 ONLY, and "plain" is deliberately absent — it is absent from the
		// schema's closed set and Service.Authorize refuses it. Advertising it
		// would be a lie a client acts on: it would send a plain challenge, get
		// refused at /authorize, and have no way to learn why.
		CodeChallengeMethodsSupported:     SupportedCodeChallengeMethods(),
		TokenEndpointAuthMethodsSupported: SupportedTokenEndpointAuthMethods(),
		// From the closed registry in scope.go, never a literal. S7's exit gate
		// is that discovery never names authority the registry does not hold.
		ScopesSupported: SupportedScopes(),
	}
}

// ProtectedResourceMetadata builds the RFC 9728 document.
func (h *DiscoveryHandler) ProtectedResourceMetadata() protectedResourceMetadataDTO {
	return protectedResourceMetadataDTO{
		// The canonical URI of this MCP server, per the MCP specification's
		// "Canonical Server URI": the endpoint clients actually POST to.
		Resource: h.base + TransportPath,
		// This control plane is its own authorization server.
		AuthorizationServers: []string{h.issuer},
		ScopesSupported:      SupportedScopes(),
		// TransportHandler.bearerToken reads the Authorization header and
		// nothing else — no form field, no query parameter. Access tokens in a
		// query string are forbidden by the MCP specification and this server
		// would not read one anyway.
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "WPMgr MCP",
	}
}

func (h *DiscoveryHandler) authorizationServerMetadata(c *gin.Context) {
	if h.refuseWhenUnconfigured(c) {
		return
	}
	h.writeJSON(c, h.AuthorizationServerMetadata())
}

func (h *DiscoveryHandler) protectedResourceMetadata(c *gin.Context) {
	if h.refuseWhenUnconfigured(c) {
		return
	}
	h.writeJSON(c, h.ProtectedResourceMetadata())
}

// refuseWhenUnconfigured answers 503 when the origin is unknown, and reports
// whether it answered.
//
// 503 rather than 404: 404 reads as "this server does not do OAuth", which
// would send a client down the 2025-03-26 fallback path of guessing /authorize,
// /token and /register at the origin root — endpoints that do not exist here.
// 503 with a named variable tells the operator what to set.
func (h *DiscoveryHandler) refuseWhenUnconfigured(c *gin.Context) bool {
	if h.unusableReason == "" {
		return false
	}
	c.Header("Access-Control-Allow-Origin", "*")
	c.JSON(http.StatusServiceUnavailable, oauthErrorDTO{
		Err:     "server_error",
		ErrDesc: h.unusableReason,
	})
	return true
}

// writeJSON emits the document with the content type RFC 8414 section 3.2 and
// RFC 9728 section 3.2 both require.
//
// Access-Control-Allow-Origin is "*" because a browser-hosted MCP client
// fetches these cross-origin before it holds any credential. It is safe here
// and only here: the response carries no credential, no cookie is read on this
// route (it is mounted on the root engine, outside the session group), and
// Allow-Credentials is deliberately NOT set, so a browser will not attach one.
func (h *DiscoveryHandler) writeJSON(c *gin.Context, doc any) {
	c.Header("Access-Control-Allow-Origin", "*")
	c.JSON(http.StatusOK, doc)
}

func (h *DiscoveryHandler) preflight(c *gin.Context) {
	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	c.Header("Access-Control-Allow-Headers", "Authorization, "+ProtocolHeader)
	c.Status(http.StatusNoContent)
}
