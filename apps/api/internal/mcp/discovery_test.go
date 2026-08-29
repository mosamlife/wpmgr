package mcp

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

const testBaseURL = "https://manage.example.com"

// mountedEngine builds a gin engine with the MCP surface mounted EXACTLY as
// internal/server/server.go mounts it.
//
// The three call sites it mirrors, so a reader can check the correspondence by
// hand: server.go's `deps.MCPOAuthH.RegisterPublic(engine.Group(mcp.APIV1Prefix))`,
// `deps.MCPOAuthH.Register(v1)` where v1 is `sessionAuthGroup.Group(mcp.APIV1Prefix)`,
// and `deps.MCPTransportH.Register(engine)`. The session middleware on v1 is
// irrelevant here: this test asks whether a ROUTE EXISTS, not whether it lets
// an anonymous caller through.
func mountedEngine(t *testing.T, withOAuth bool) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()

	svc := NewService(nil)
	if withOAuth {
		oauthH := NewHandler(svc)
		oauthH.RegisterPublic(engine.Group(APIV1Prefix))
		oauthH.Register(engine.Group(APIV1Prefix))
		NewTransportHandler(svc, slog.New(slog.DiscardHandler), "test").Register(engine)
	}
	NewDiscoveryHandler(testBaseURL).Register(engine)
	return engine
}

func getDoc(t *testing.T, engine *gin.Engine, path string) (*http.Response, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	res := rec.Result()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	_ = res.Body.Close()
	return res, body
}

// advertisedEndpoint is one promise the discovery documents make.
type advertisedEndpoint struct {
	field  string
	rawURL string
	method string
}

// unmountedAdvertisements returns one message per advertised endpoint that is
// NOT actually served by engine. Empty means every promise is kept.
//
// It is a helper rather than a body of t.Errorf calls so the negative control
// below can assert that it FINDS something when the routes are missing. A check
// that has never been seen to fail is not known to check anything.
func unmountedAdvertisements(engine *gin.Engine, endpoints []advertisedEndpoint) []string {
	routes := engine.Routes()
	var problems []string
	for _, ep := range endpoints {
		u, err := url.Parse(ep.rawURL)
		if err != nil {
			problems = append(problems, ep.field+" is not a URL: "+ep.rawURL)
			continue
		}
		mounted := slices.ContainsFunc(routes, func(r gin.RouteInfo) bool {
			return r.Path == u.Path && r.Method == ep.method
		})
		if !mounted {
			problems = append(problems,
				ep.field+" advertises "+ep.method+" "+u.Path+" but no such route is mounted")
		}
	}
	return problems
}

// documentedEndpoints reads the two live documents and returns every endpoint
// they promise, with the HTTP method a client will use against it.
func documentedEndpoints(t *testing.T, engine *gin.Engine) []advertisedEndpoint {
	t.Helper()

	_, asBody := getDoc(t, engine, WellKnownAuthorizationServerPath)
	var as authorizationServerMetadataDTO
	if err := json.Unmarshal(asBody, &as); err != nil {
		t.Fatalf("authorization server metadata is not JSON: %v (%s)", err, asBody)
	}
	_, prBody := getDoc(t, engine, WellKnownProtectedResourcePath)
	var pr protectedResourceMetadataDTO
	if err := json.Unmarshal(prBody, &pr); err != nil {
		t.Fatalf("protected resource metadata is not JSON: %v (%s)", err, prBody)
	}

	return []advertisedEndpoint{
		// A client opens a browser here. RFC 6749 section 3.1: GET.
		{"authorization_endpoint", as.AuthorizationEndpoint, http.MethodGet},
		// RFC 6749 section 3.2: the token endpoint is POST.
		{"token_endpoint", as.TokenEndpoint, http.MethodPost},
		// RFC 7591 section 3.1: registration is POST.
		{"registration_endpoint", as.RegistrationEndpoint, http.MethodPost},
		// RFC 9728's `resource` is the MCP endpoint itself, which this
		// server serves over POST (Streamable HTTP, no SSE in phase 1).
		{"resource", pr.Resource, http.MethodPost},
	}
}

// TestAdvertisedEndpointsAreMounted is THE pin.
//
// Drift between a discovery document and the router is silent: nothing in this
// process ever fetches its own metadata, so a renamed route leaves a document
// that still parses, still validates against its RFC, and sends every GUI
// client to a 404 at handshake time — the one moment nobody is watching a log.
// This walks the real gin route table and fails on the rename, in the same test
// run that made it.
func TestAdvertisedEndpointsAreMounted(t *testing.T) {
	engine := mountedEngine(t, true)
	endpoints := documentedEndpoints(t, engine)
	if len(endpoints) == 0 {
		t.Fatal("no endpoints were advertised; the documents are empty and this test would pass vacuously")
	}
	for _, problem := range unmountedAdvertisements(engine, endpoints) {
		t.Error(problem)
	}
}

// TestAdvertisedEndpointsCheckFiresWhenRoutesAreMissing is the other half:
// proof the check above can fail. The engine here serves the discovery
// documents and mounts NONE of the endpoints they name, which is exactly the
// production shape the load balancer hides — a document served, its endpoints
// unreachable.
func TestAdvertisedEndpointsCheckFiresWhenRoutesAreMissing(t *testing.T) {
	engine := mountedEngine(t, false)
	endpoints := documentedEndpoints(t, engine)
	problems := unmountedAdvertisements(engine, endpoints)
	if len(problems) != len(endpoints) {
		t.Fatalf("expected all %d advertised endpoints to be reported unmounted, got %d: %v",
			len(endpoints), len(problems), problems)
	}
}

// TestAdvertisedURLsAreAbsoluteAndOnTheConfiguredOrigin catches the other way a
// document sends a client somewhere wrong: a relative URL, or an origin that is
// not the one configured. Both are "an absence coerced into a plausible value".
func TestAdvertisedURLsAreAbsoluteAndOnTheConfiguredOrigin(t *testing.T) {
	engine := mountedEngine(t, true)
	for _, ep := range documentedEndpoints(t, engine) {
		if !strings.HasPrefix(ep.rawURL, testBaseURL+"/") {
			t.Errorf("%s = %q; want an absolute URL on the configured origin %s",
				ep.field, ep.rawURL, testBaseURL)
		}
	}
}

// TestIssuerIsDerivedFromConfiguration pins that the issuer follows the
// configured origin rather than a constant. A self-hosted install must never
// see this project's own hostname in its own metadata.
func TestIssuerIsDerivedFromConfiguration(t *testing.T) {
	for _, base := range []string{
		"https://manage.example.com",
		"https://selfhost.internal:8443",
		"http://localhost:8080",
	} {
		doc := NewDiscoveryHandler(base).AuthorizationServerMetadata()
		if doc.Issuer != base {
			t.Errorf("NewDiscoveryHandler(%q).Issuer = %q; want %q", base, doc.Issuer, base)
		}
		prDoc := NewDiscoveryHandler(base).ProtectedResourceMetadata()
		if len(prDoc.AuthorizationServers) != 1 || prDoc.AuthorizationServers[0] != base {
			t.Errorf("NewDiscoveryHandler(%q).AuthorizationServers = %v; want [%q]",
				base, prDoc.AuthorizationServers, base)
		}
		if want := base + TransportPath; prDoc.Resource != want {
			t.Errorf("NewDiscoveryHandler(%q).Resource = %q; want %q", base, prDoc.Resource, want)
		}
	}
}

// TestDiscoveryRefusesWhenOriginIsUnconfigured is the absence case.
//
// An unset WPMGR_PUBLIC_BASE_URL must NOT produce a document naming a fallback
// host, a relative path or an empty issuer. A client cannot tell a wrong issuer
// from a right one, so the only safe answer is a refusal that names the
// variable.
func TestDiscoveryRefusesWhenOriginIsUnconfigured(t *testing.T) {
	for _, base := range []string{"", "   ", "manage.example.com", "/wpmgr", "ftp://manage.example.com"} {
		gin.SetMode(gin.TestMode)
		engine := gin.New()
		NewDiscoveryHandler(base).Register(engine)

		for _, path := range []string{
			WellKnownAuthorizationServerPath,
			WellKnownProtectedResourcePath,
			WellKnownProtectedResourceMCPPath,
		} {
			res, body := getDoc(t, engine, path)
			if res.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("base %q, GET %s = %d; want 503", base, path, res.StatusCode)
			}
			if !strings.Contains(string(body), "WPMGR_PUBLIC_BASE_URL") {
				t.Errorf("base %q, GET %s body %s; want it to name WPMGR_PUBLIC_BASE_URL", base, path, body)
			}
			if strings.Contains(string(body), "wpmgr.app") {
				t.Errorf("base %q, GET %s leaked a fallback host: %s", base, path, body)
			}
		}
	}
}

// TestDiscoveryVocabularyMatchesTheValidators is the anti-copy test.
//
// For each OAuth vocabulary the documents advertise, it drives the validator
// the request path actually runs with every neighbouring value and asserts:
// advertised ⟺ accepted. A hand-copied list that gained "plain", or a validator
// that quietly started accepting one, fails here.
func TestDiscoveryVocabularyMatchesTheValidators(t *testing.T) {
	doc := NewDiscoveryHandler(testBaseURL).AuthorizationServerMetadata()

	cases := []struct {
		name       string
		advertised []string
		candidates []string
		accepts    func(string) error
	}{
		{
			name:       "code_challenge_methods_supported",
			advertised: doc.CodeChallengeMethodsSupported,
			// "plain" is the one that matters: RFC 7636 defines it, this
			// server refuses it, and advertising it would have clients send
			// a challenge that is rejected at /authorize with no way to
			// learn why.
			candidates: []string{"S256", "plain", "s256", "S512", ""},
			accepts:    validateCodeChallengeMethod,
		},
		{
			name:       "response_types_supported",
			advertised: doc.ResponseTypesSupported,
			candidates: []string{"code", "token", "id_token", "code id_token", ""},
			accepts:    validateResponseType,
		},
		{
			name:       "grant_types_supported",
			advertised: doc.GrantTypesSupported,
			// refresh_token is the notable absence: nothing here mints one.
			candidates: []string{"authorization_code", "refresh_token", "client_credentials", "implicit", ""},
			accepts:    validateGrantType,
		},
		{
			name:       "token_endpoint_auth_methods_supported",
			advertised: doc.TokenEndpointAuthMethodsSupported,
			candidates: []string{"none", "client_secret_basic", "client_secret_post", "client_secret_jwt", "private_key_jwt", ""},
			accepts:    validateTokenEndpointAuthMethod,
		},
	}

	for _, tc := range cases {
		if len(tc.advertised) == 0 {
			t.Errorf("%s is empty; a client reading this document learns nothing and, for PKCE, MUST refuse to proceed", tc.name)
		}
		for _, candidate := range tc.candidates {
			advertised := slices.Contains(tc.advertised, candidate)
			accepted := tc.accepts(candidate) == nil
			if advertised != accepted {
				t.Errorf("%s: %q advertised=%v but the validator accepts=%v; the document and the code disagree",
					tc.name, candidate, advertised, accepted)
			}
		}
	}
}

// TestScopesComeFromTheRegistryNotALiteral pins S7's exit gate one layer out:
// discovery must never name authority the closed registry in scope.go does not
// hold.
func TestScopesComeFromTheRegistryNotALiteral(t *testing.T) {
	registry := SupportedScopes()
	if len(registry) == 0 {
		t.Fatal("SupportedScopes() is empty; this test would pass vacuously")
	}

	asDoc := NewDiscoveryHandler(testBaseURL).AuthorizationServerMetadata()
	prDoc := NewDiscoveryHandler(testBaseURL).ProtectedResourceMetadata()

	if !slices.Equal(asDoc.ScopesSupported, registry) {
		t.Errorf("authorization server scopes_supported = %v; registry = %v", asDoc.ScopesSupported, registry)
	}
	if !slices.Equal(prDoc.ScopesSupported, registry) {
		t.Errorf("protected resource scopes_supported = %v; registry = %v", prDoc.ScopesSupported, registry)
	}

	// Every advertised scope must survive the parser the /authorize endpoint
	// runs. Advertising a scope that endpoint refuses is the same lie as
	// advertising an unmounted endpoint.
	for _, s := range asDoc.ScopesSupported {
		if _, err := ParseRequestedScopes(s); err != nil {
			t.Errorf("advertised scope %q is refused by ParseRequestedScopes: %v", s, err)
		}
	}
	// And the whole advertised set requested at once must be accepted, which is
	// what a client following the MCP scope-selection strategy sends.
	if _, err := ParseRequestedScopes(strings.Join(asDoc.ScopesSupported, " ")); err != nil {
		t.Errorf("the full advertised scope set is refused by ParseRequestedScopes: %v", err)
	}
	// Negative control: a scope the document does not name is refused.
	if _, err := ParseRequestedScopes("mcp:write"); err == nil {
		t.Error("ParseRequestedScopes accepted an unadvertised scope; the registry is not closed")
	}
}

// TestProtectedResourceMetadataServedAtBothWellKnownPaths covers the MCP
// 2025-11-25 discovery order: a client tries the RFC 9728 path-insertion form
// for the MCP endpoint's path FIRST, then falls back to the root form. Serving
// only one costs every client a 404 on its first connection.
func TestProtectedResourceMetadataServedAtBothWellKnownPaths(t *testing.T) {
	engine := mountedEngine(t, true)

	if want := "/.well-known/oauth-protected-resource/mcp"; WellKnownProtectedResourceMCPPath != want {
		t.Fatalf("path-insertion form = %q; want %q (RFC 9728 section 3.1 over the MCP endpoint path %q)",
			WellKnownProtectedResourceMCPPath, want, TransportPath)
	}

	_, rootBody := getDoc(t, engine, WellKnownProtectedResourcePath)
	res, insertedBody := getDoc(t, engine, WellKnownProtectedResourceMCPPath)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d; want 200", WellKnownProtectedResourceMCPPath, res.StatusCode)
	}
	if string(rootBody) != string(insertedBody) {
		t.Errorf("the two protected-resource paths return different documents:\nroot: %s\ninserted: %s",
			rootBody, insertedBody)
	}
}

// TestDiscoveryDocumentsAreUnauthenticatedJSON pins the two properties the
// production trap hides. The load balancer hands unrouted paths to the SPA,
// which answers 200 with text/html for everything, so a 200 proves nothing:
// the content type and a parseable body are the only evidence the route exists.
func TestDiscoveryDocumentsAreUnauthenticatedJSON(t *testing.T) {
	engine := mountedEngine(t, true)

	for _, path := range []string{
		WellKnownAuthorizationServerPath,
		WellKnownProtectedResourcePath,
		WellKnownProtectedResourceMCPPath,
	} {
		// No Authorization header, no cookie: exactly what a client sends
		// before it holds any credential.
		res, body := getDoc(t, engine, path)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d; want 200 with no credential presented", path, res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("GET %s Content-Type = %q; want application/json", path, ct)
		}
		if res.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("GET %s has no permissive CORS header; a browser-hosted client cannot read it", path)
		}
		if res.Header.Get("Access-Control-Allow-Credentials") != "" {
			t.Errorf("GET %s sets Access-Control-Allow-Credentials; these documents must never be fetched with a cookie", path)
		}
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Errorf("GET %s did not return a JSON object: %v (%s)", path, err, body)
		}
	}
}

// TestRequiredDiscoveryFieldsArePresent checks the fields each RFC makes
// REQUIRED, and that none of them is an empty string wearing the shape of a
// value.
func TestRequiredDiscoveryFieldsArePresent(t *testing.T) {
	engine := mountedEngine(t, true)

	_, asBody := getDoc(t, engine, WellKnownAuthorizationServerPath)
	var as map[string]any
	if err := json.Unmarshal(asBody, &as); err != nil {
		t.Fatalf("authorization server metadata: %v", err)
	}
	// RFC 8414 section 2: issuer, authorization_endpoint, token_endpoint and
	// response_types_supported are REQUIRED. jwks_uri is deliberately absent —
	// the connection token is opaque and nothing here signs a JWT.
	for _, field := range []string{"issuer", "authorization_endpoint", "token_endpoint", "response_types_supported"} {
		if v, ok := as[field]; !ok || v == "" {
			t.Errorf("authorization server metadata is missing required field %q", field)
		}
	}
	if _, ok := as["jwks_uri"]; ok {
		t.Error("authorization server metadata advertises jwks_uri, but nothing here serves one")
	}

	_, prBody := getDoc(t, engine, WellKnownProtectedResourcePath)
	var pr map[string]any
	if err := json.Unmarshal(prBody, &pr); err != nil {
		t.Fatalf("protected resource metadata: %v", err)
	}
	// RFC 9728 section 2 makes `resource` REQUIRED; the MCP specification
	// additionally requires authorization_servers to name at least one.
	if v, ok := pr["resource"]; !ok || v == "" {
		t.Error("protected resource metadata is missing required field \"resource\"")
	}
	servers, ok := pr["authorization_servers"].([]any)
	if !ok || len(servers) == 0 {
		t.Error("protected resource metadata must name at least one authorization server")
	}
}

// TestDiscoveryPathsAnswer405NotFoundForOtherVerbs keeps the house rule: a
// wrong verb must never read as "not deployed".
func TestDiscoveryPathsAnswer405NotFoundForOtherVerbs(t *testing.T) {
	engine := mountedEngine(t, true)

	for _, path := range []string{
		WellKnownAuthorizationServerPath,
		WellKnownProtectedResourcePath,
		WellKnownProtectedResourceMCPPath,
	} {
		for _, verb := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			req := httptest.NewRequest(verb, path, nil)
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d; want 405", verb, path, rec.Code)
			}
		}
		req := httptest.NewRequest(http.MethodOptions, path, nil)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Errorf("OPTIONS %s = %d; want 204 preflight", path, rec.Code)
		}
	}
}
