package server

// mcp_authorize_navigation_test.go: a browser that opens the advertised
// authorization_endpoint is sent on to the consent screen, through the engine
// New builds. The address is read from the served discovery document, never
// from a constant, so the proof covers what a client actually opens. The
// consent screen's own fetch of the same address still answers JSON, and no
// other route redirects. Runs with no database, like settings_mount_test.go.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/tenant"
)

// consentScreen is the dashboard route a navigation must land on
// (apps/web/src/routes/_authed/connect.ai.tsx). A literal, not
// mcp.ConsentScreenPath: this is the contract with the web app, and a change
// to the constant must fail here.
const consentScreen = "/connect/ai"

const (
	navClientID    = "c1"
	navRedirectURI = "http://localhost:61695/callback"
)

// navQuery is an authorize query in the order and encoding a client sends it.
// The redirect must carry it byte for byte: reordered, re-encoded ("+" for
// "%20") or decoded, it is a different request.
const navQuery = "response_type=code&client_id=c1&code_challenge=abc&code_challenge_method=S256" +
	"&state=a~b-_c&redirect_uri=http%3A%2F%2Flocalhost%3A61695%2Fcallback" +
	"&scope=mcp%3Aread%20mcp%3Asite%20mcp%3Acache" +
	"&resource=https%3A%2F%2Fmanage.example.test%2Fmcp"

// browserNavigation is what a browser sends when a client opens the address.
var browserNavigation = map[string]string{
	"Accept":         "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
	"Sec-Fetch-Mode": "navigate",
	"Sec-Fetch-Dest": "document",
	"Sec-Fetch-Site": "none",
}

// screenFetch is what the consent screen sends when it fetches the JSON.
var screenFetch = map[string]string{
	"Accept":         "application/json",
	"Sec-Fetch-Mode": "cors",
	"Sec-Fetch-Dest": "empty",
	"Sec-Fetch-Site": "same-origin",
}

// consentClientStore answers the one read Service.Authorize makes, with a
// client registered for the fixture's redirect and scopes. Any other Store
// method is the nil embedded interface; a call to one panics, Recovery turns
// it into a 500, and the assertions below report it.
type consentClientStore struct{ mcp.Store }

func (consentClientStore) LookupClient(_ context.Context, clientID string) (sqlc.McpOauthClient, error) {
	if clientID != navClientID {
		return sqlc.McpOauthClient{}, pgx.ErrNoRows
	}
	return sqlc.McpOauthClient{
		ID:                      uuid.New(),
		ClientID:                navClientID,
		TokenEndpointAuthMethod: "none",
		RedirectUris:            []string{navRedirectURI},
		RegisteredScopes:        []string{"mcp:read", "mcp:site", "mcp:cache"},
	}, nil
}

func mcpMountEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	sessions := auth.NewSessionManagerWithStore(scs.New(), false)
	srv := New(Deps{
		Config:        config.Config{},
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sessions:      sessions,
		Auth:          middleware.NewAuthenticator(sessions, nil, nil, nil),
		AuthH:         auth.NewHandler(nil, sessions, nil, nil),
		MembersH:      auth.NewMembersHandler(nil, nil),
		APIKeyH:       apikey.NewHandler(nil, nil),
		AuditH:        audit.NewHandler(nil),
		TenantH:       tenant.NewHandler(nil, nil),
		SiteH:         site.NewHandler(nil, nil, ""),
		MCPOAuthH:     mcp.NewHandler(mcp.NewService(consentClientStore{})),
		MCPDiscoveryH: mcp.NewDiscoveryHandler("https://manage.example.test"),
	})
	e, ok := srv.Handler().(*gin.Engine)
	if !ok {
		t.Fatalf("Handler() is %T, want *gin.Engine", srv.Handler())
	}
	return e
}

// advertisedAuthorizePath is the path of the authorization_endpoint the engine
// serves in its own discovery document.
func advertisedAuthorizePath(t *testing.T, e *gin.Engine) string {
	t.Helper()
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("discovery document: status %d, body %s", w.Code, w.Body.String())
	}
	var doc struct {
		AuthorizationEndpoint string `json:"authorization_endpoint"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("discovery document is not JSON: %v (%s)", err, w.Body.String())
	}
	u, err := url.Parse(doc.AuthorizationEndpoint)
	if err != nil || u.Path == "" {
		t.Fatalf("authorization_endpoint %q is not a URL with a path (%v)", doc.AuthorizationEndpoint, err)
	}
	return u.Path
}

// signedInOwner can authorize a connection: an organisation member with
// PermAPIKeyManage and no site constraint.
func signedInOwner() *domain.Principal {
	return &domain.Principal{
		Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: uuid.New(),
		Role: "owner", Scope: domain.ScopeOrg,
	}
}

// serveMCP sends one request through e. A non-nil principal is placed on the
// request context, which Authenticate leaves in place when the request
// carries no credential.
func serveMCP(e *gin.Engine, method, target string, headers map[string]string, p *domain.Principal) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if p != nil {
		req = req.WithContext(domain.WithPrincipal(req.Context(), *p))
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

func TestNew_AuthorizeNavigationOpensTheConsentScreen(t *testing.T) {
	e := mcpMountEngine(t)
	target := advertisedAuthorizePath(t, e) + "?" + navQuery

	for _, tc := range []struct {
		name string
		p    *domain.Principal
	}{
		{"signed_out", nil},
		{"signed_in", signedInOwner()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := serveMCP(e, http.MethodGet, target, browserNavigation, tc.p)
			if w.Code != http.StatusSeeOther {
				t.Fatalf("a browser opening the advertised authorization_endpoint got %d %s (body %s); want 303 See Other to the consent screen",
					w.Code, w.Header().Get("Content-Type"), w.Body.String())
			}
			if got, want := w.Header().Get("Location"), consentScreen+"?"+navQuery; got != want {
				t.Fatalf("Location = %q\nwant       %q (the query byte for byte)", got, want)
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			if strings.Contains(w.Body.String(), "consent_ticket") {
				t.Errorf("the redirect carried a consent ticket: %s", w.Body.String())
			}
		})
	}
}

// The consent screen fetches the same address for its JSON, and that fetch,
// and anything that is not a browser navigation, must still reach the route
// and answer JSON, never a redirect.
func TestNew_AuthorizeFetchStillAnswersJSON(t *testing.T) {
	e := mcpMountEngine(t)
	target := advertisedAuthorizePath(t, e) + "?" + navQuery

	for _, tc := range []struct {
		name    string
		headers map[string]string
		p       *domain.Principal
		status  int
		// field is a JSON key the answer must carry: "code" for the house
		// unauthenticated envelope, "consent_ticket" for the handler's answer.
		field string
	}{
		{"the screen's fetch, signed out", screenFetch, nil, http.StatusUnauthorized, "code"},
		{"the screen's fetch, signed in", screenFetch, signedInOwner(), http.StatusOK, "consent_ticket"},
		{"no headers", map[string]string{}, nil, http.StatusUnauthorized, "code"},
		{"wildcard accept", map[string]string{"Accept": "*/*"}, nil, http.StatusUnauthorized, "code"},
		{"html accept on a cors fetch", map[string]string{
			"Accept": "text/html", "Sec-Fetch-Mode": "cors"}, nil, http.StatusUnauthorized, "code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := serveMCP(e, http.MethodGet, target, tc.headers, tc.p)
			if w.Code >= 300 && w.Code < 400 {
				t.Fatalf("got %d to %q; a fetch of the authorize JSON must never be redirected",
					w.Code, w.Header().Get("Location"))
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json (status %d, body %s)", ct, w.Code, w.Body.String())
			}
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d; body %s", w.Code, tc.status, w.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %v (%s)", err, w.Body.String())
			}
			if v, _ := body[tc.field].(string); v == "" {
				t.Fatalf("body has no %q: %s", tc.field, w.Body.String())
			}
		})
	}
}

// The redirect target is fixed. Nothing in the request, whether the Host, a
// forwarding header or a query parameter that looks like a destination, can
// move it off the consent screen or onto another origin, and an encoded CR LF
// in the query stays encoded.
func TestNew_AuthorizeNavigationCannotBeSteered(t *testing.T) {
	e := mcpMountEngine(t)
	query := "response_type=code&client_id=c1&code_challenge=abc&code_challenge_method=S256" +
		"&state=%0d%0aSet-Cookie:x&redirect_uri=https://evil.example/cb&scope=mcp%3Aread" +
		"&next=//evil.example&return_to=https://evil.example"
	headers := map[string]string{
		"X-Forwarded-Host":  "evil.example",
		"X-Forwarded-Proto": "http",
	}
	for k, v := range browserNavigation {
		headers[k] = v
	}
	req := httptest.NewRequest(http.MethodGet, advertisedAuthorizePath(t, e)+"?"+query, nil)
	req.Host = "evil.example"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	// First that there is a redirect at all, so nothing below passes vacuously.
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if strings.ContainsAny(loc, "\r\n") {
		t.Fatalf("Location carries a raw CR or LF: %q", loc)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Location %q does not parse: %v", loc, err)
	}
	if u.Scheme != "" || u.Host != "" {
		t.Errorf("Location %q names scheme %q host %q; want a path on the origin the browser opened", loc, u.Scheme, u.Host)
	}
	if u.Path != consentScreen {
		t.Errorf("Location path = %q, want exactly %q", u.Path, consentScreen)
	}
	if u.RawQuery != query {
		t.Errorf("Location query = %q\nwant             %q (the request's, byte for byte)", u.RawQuery, query)
	}
}

// Only a GET on exactly the authorize path is redirected. A browser
// navigation to anything else, including the authorize path under another
// method, answers exactly as it did before.
func TestNew_OnlyTheAuthorizePathRedirects(t *testing.T) {
	e := mcpMountEngine(t)
	authorize := advertisedAuthorizePath(t, e)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, authorize + "X"},
		{http.MethodGet, authorize + "/x"},
		{http.MethodPost, authorize},
		{http.MethodHead, authorize},
		{http.MethodGet, "/api/v1/mcp/connections"},
		{http.MethodGet, "/api/v1/oauth/mcp/consent"},
		{http.MethodPost, "/api/v1/oauth/mcp/token"},
		{http.MethodGet, "/.well-known/oauth-authorization-server"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := serveMCP(e, tc.method, tc.path+"?"+navQuery, browserNavigation, nil)
			if w.Code >= 300 && w.Code < 400 {
				t.Fatalf("got %d to %q; only a GET on the authorize path may redirect", w.Code, w.Header().Get("Location"))
			}
			if loc := w.Header().Get("Location"); loc != "" {
				t.Fatalf("Location = %q on a %d", loc, w.Code)
			}
		})
	}
}
