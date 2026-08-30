// adr064_s6b3_mcp_oauth_end_to_end_test.go: the whole MCP connect flow, driven
// through the MOUNTED HTTP routes against the real schema as wpmgr_app.
//
// WHY THIS FILE EXISTS. Handler.Register and Handler.RegisterPublic were called
// from nowhere. DCR, authorize, consent and token exchange were built, reviewed
// and unreachable, so POST /mcp was mounted, correct, and refused every request
// forever because no token could be minted. Mounting them is what this slice
// did; this is the proof that the four endpoints compose into a working flow
// rather than four endpoints that each pass their own unit tests.
//
// WHAT MAKES IT A PROOF RATHER THAN A REHEARSAL.
//
//   - It goes through gin, through the same Handler.RegisterPublic and
//     Handler.Register calls server.New makes, to the same Service and Repo,
//     which use the same tx helpers. No GUC is hand-set anywhere in this file.
//   - It opens NO connection of its own for the flow. Every read and write
//     lands through db.Pool as wpmgr_app -- NOSUPERUSER, NOBYPASSRLS -- which is
//     asserted and printed below. A test that opened its own connection would
//     leave every policy inert and pass regardless; that has happened here.
//   - The last step is the one the owner is waiting on: the minted bearer token
//     is presented to POST /mcp and used to READ SITES. Anything short of that
//     proves the OAuth dance and not the thing the dance exists for.
//
// The authenticated half is mounted behind a middleware that injects a
// principal, which is what authz.RequireAuth + RequireTenant hand the handler in
// production. The session machinery itself is not under test here.
package tests

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// TestMCPOAuthEndToEndThroughMountedRoutesAsAppRole is the QA gate: register,
// authorize with PKCE, consent, exchange, then read sites over MCP.
func TestMCPOAuthEndToEndThroughMountedRoutesAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	// The role is load-bearing. Either privilege would make all of this pass
	// vacuously, so it is asserted inside a transaction the flow actually uses.
	if err := pool.InMCPClientLookupTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InMCPClientLookupTx")
		return nil
	}); err != nil {
		t.Fatalf("open lookup tx: %v", err)
	}

	tenantID := seedTenant(t, pool, "mcp-s6b3-"+uuid.NewString()[:8])
	userID := seedUserRow(t, pool, "mcp-s6b3-"+uuid.NewString()[:8]+"@example.test")
	// No membership row is seeded: memberships is RLS-protected and this pool is
	// wpmgr_app, which is the point of the test. The principal below stands in
	// for what authz.RequireAuth + RequireTenant have already established by the
	// time the consent handler runs, so membership is upstream of what is under
	// test here.
	siteURL := "https://" + uuid.NewString()[:8] + ".example.test"
	siteID := seedSite(t, pool, tenantID, siteURL)

	svc := mcp.NewService(mcp.NewRepo(pool))
	// An ORG-scoped operator: the only scope /consent admits.
	eng := mountLikeProduction(t, svc, domain.Principal{
		UserID: userID, TenantID: tenantID, Scope: domain.ScopeOrg,
	})

	const redirectURI = "https://claude.ai/api/mcp/auth_callback"

	// -----------------------------------------------------------------------
	// STEP 1: RFC 7591 dynamic client registration, UNAUTHENTICATED.
	// -----------------------------------------------------------------------
	regBody := map[string]any{
		"redirect_uris":              []string{redirectURI},
		"client_name":                "Claude Desktop",
		"client_uri":                 "https://claude.ai",
		"token_endpoint_auth_method": "none", // public client: PKCE is the proof
	}
	var reg struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	code := mcpDoJSON(t, eng, http.MethodPost, "/api/v1/oauth/mcp/register", regBody, nil, &reg)
	if code != http.StatusCreated {
		t.Fatalf("STEP 1 registration answered %d, want 201", code)
	}
	if reg.ClientID == "" {
		t.Fatal("STEP 1 returned no client_id")
	}
	if reg.ClientSecret != "" {
		t.Errorf("STEP 1 minted a client_secret for a token_endpoint_auth_method " +
			"of 'none'; a public client must get no secret")
	}
	t.Logf("STEP 1 ok: registered client_id=%s (unauthenticated, no secret)", reg.ClientID)

	// -----------------------------------------------------------------------
	// STEP 2: authorize. Returns the consent screen's contents and mints
	// nothing. PKCE S256 throughout -- the verifier never leaves this test until
	// the exchange.
	// -----------------------------------------------------------------------
	verifier := "s6b3-verifier-" + uuid.NewString() + uuid.NewString()
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", reg.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", string(mcp.ScopeRead))
	q.Set("state", "s6b3-state")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")

	var consentScreen struct {
		ClientID             string   `json:"client_id"`
		ClientNameUnverified string   `json:"client_name_unverified"`
		IdentityVerified     bool     `json:"identity_verified"`
		RedirectURI          string   `json:"redirect_uri"`
		RedirectHost         string   `json:"redirect_host"`
		Scopes               []string `json:"scopes"`
	}
	code = mcpDoJSON(t, eng, http.MethodGet,
		"/api/v1/oauth/mcp/authorize?"+q.Encode(), nil, nil, &consentScreen)
	if code != http.StatusOK {
		t.Fatalf("STEP 2 authorize answered %d, want 200", code)
	}

	// m124 obligation 7, on the path that is actually mounted. Registration is
	// unauthenticated, so client_name is an attacker-controlled string and two
	// clients may both call themselves "Claude Desktop". The server must mark it
	// unverified; a UI that renders it as an identity is then the UI's defect,
	// not a missing signal.
	if consentScreen.IdentityVerified {
		t.Error("STEP 2 consent screen reports identity_verified=true; registration " +
			"is unauthenticated and nothing about this identity is verified, ever")
	}
	if consentScreen.ClientNameUnverified != "Claude Desktop" {
		t.Errorf("STEP 2 client_name_unverified = %q, want the registered value",
			consentScreen.ClientNameUnverified)
	}
	if consentScreen.RedirectHost == "" {
		t.Error("STEP 2 supplied no redirect_host; the host is the part of the " +
			"request a human can actually judge")
	}
	t.Logf("STEP 2 ok: consent screen for %q, identity_verified=%t, redirect_host=%s",
		consentScreen.ClientNameUnverified, consentScreen.IdentityVerified,
		consentScreen.RedirectHost)

	// -----------------------------------------------------------------------
	// STEP 3: consent. The human's approval, and the only thing that binds this
	// client to an organisation.
	// -----------------------------------------------------------------------
	approveBody := map[string]any{
		"client_id":             reg.ClientID,
		"redirect_uri":          redirectURI,
		"scopes":                []string{string(mcp.ScopeRead)},
		"state":                 "s6b3-state",
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"name":                  "s6b3 end-to-end connection",
		"site_scope_mode":       string(mcp.SiteScopeModeAll),
	}
	var approval struct {
		GrantID string `json:"grant_id"`
		Code    string `json:"code"`
		State   string `json:"state"`
	}
	code = mcpDoJSON(t, eng, http.MethodPost, "/api/v1/oauth/mcp/consent", approveBody, nil, &approval)
	if code != http.StatusOK {
		t.Fatalf("STEP 3 consent answered %d, want 200", code)
	}
	if approval.Code == "" {
		t.Fatal("STEP 3 returned no authorization code")
	}
	if approval.State != "s6b3-state" {
		t.Errorf("STEP 3 state = %q, want the value the client sent; a dropped "+
			"state breaks the client's own CSRF check", approval.State)
	}
	t.Logf("STEP 3 ok: grant %s approved under tenant %s", approval.GrantID, tenantID)

	// -----------------------------------------------------------------------
	// STEP 4: token exchange, UNAUTHENTICATED, form-encoded as every client
	// library sends it. The code plus the PKCE verifier is the entire credential.
	// -----------------------------------------------------------------------
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", approval.Code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", reg.ClientID)
	form.Set("code_verifier", verifier)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/mcp/token",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "203.0.113.7:5555"
	w := httptest.NewRecorder()
	eng.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("STEP 4 token exchange answered %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("STEP 4 Cache-Control = %q, want no-store (RFC 6749 5.1)", got)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &tok); err != nil {
		t.Fatalf("STEP 4 token response is not JSON: %v", err)
	}
	if tok.AccessToken == "" {
		t.Fatal("STEP 4 returned no access_token")
	}
	if tok.ExpiresIn <= 0 {
		t.Errorf("STEP 4 expires_in = %d; a non-positive lifetime reads as "+
			"'already expired' or 'never expires', and neither is intended", tok.ExpiresIn)
	}
	t.Logf("STEP 4 ok: minted a %s token, scope=%q, expires_in=%d",
		tok.TokenType, tok.Scope, tok.ExpiresIn)

	// The code is single-use. Replaying it must not mint a second token.
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/mcp/token",
		strings.NewReader(form.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.RemoteAddr = "203.0.113.7:5555"
	eng.ServeHTTP(w2, req2)
	if w2.Code == http.StatusOK {
		t.Errorf("STEP 4 replay of a consumed authorization code minted a SECOND "+
			"token (%d); the code is single-use", w2.Code)
	}

	// -----------------------------------------------------------------------
	// STEP 5: THE POINT OF ALL OF IT. Present the token to POST /mcp and read
	// the fleet. This is the owner's QA gate.
	// -----------------------------------------------------------------------

	// First, the negative control. An unauthenticated call must be refused, so
	// that a success below is the token's doing and not an open endpoint.
	anon := mcpRPC(t, eng, "", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": mcp.ToolListSites, "arguments": map[string]any{}},
	})
	if anon.status == http.StatusOK {
		t.Fatalf("POST /mcp with NO bearer token answered 200; every result below "+
			"would be meaningless. body: %s", anon.body)
	}
	if anon.status == http.StatusNotFound {
		t.Fatal("POST /mcp answered 404, which reads as 'not deployed' rather " +
			"than 'refused'; the transport is not mounted in this test")
	}
	t.Logf("STEP 5 control ok: unauthenticated POST /mcp answered %d", anon.status)

	// Now with the minted token.
	got := mcpRPC(t, eng, tok.AccessToken, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": mcp.ToolListSites, "arguments": map[string]any{}},
	})
	if got.status != http.StatusOK {
		t.Fatalf("STEP 5 tools/call %s with the minted token answered %d; the whole "+
			"OAuth flow completed and the token it produced does not work. body: %s",
			mcp.ToolListSites, got.status, got.body)
	}
	if strings.Contains(got.body, `"error"`) {
		t.Fatalf("STEP 5 tools/call returned a JSON-RPC error: %s", got.body)
	}

	// The site seeded into this tenant must actually appear. "200 with an empty
	// list" is the shape a broken scope resolution produces, and it is the one
	// outcome most easily mistaken for success -- swapped arguments made every
	// scoped grant resolve to zero sites on this very stack.
	if !strings.Contains(got.body, siteURL) {
		t.Fatalf("STEP 5 tools/call %s returned 200 but the tenant's only site "+
			"(%s, id=%s) is absent. An empty read is not a successful read.\nbody: %s",
			mcp.ToolListSites, siteURL, siteID, got.body)
	}
	t.Logf("STEP 5 ok: read site %s over MCP with the OAuth-minted bearer token", siteURL)
	t.Log("END TO END: register -> authorize -> consent -> exchange -> read, " +
		"through the mounted routes, as wpmgr_app")
}

// mountLikeProduction builds the engine the way server.New does: RegisterPublic
// on a bare /api/v1 group with NO session middleware, Register on a group that
// carries a principal the way RequireAuth + RequireTenant guarantee, and the
// transport on the root engine.
//
// The two Register calls and their groups are the part under test. If
// server.New's mounting diverges from this, that is a defect in one of the two
// and this comment is where to start.
// It takes the WHOLE principal rather than (tenantID, userID) because scope is
// now load-bearing on these routes: Handler.Register applies
// authz.RequireOrgScope, and a harness that could only express a scopeless
// principal could never exercise it. Flattening this back to two ids would
// make the org-scope gate untestable from here, which is how it came to be
// absent.
func mountLikeProduction(t *testing.T, svc *mcp.Service, principal domain.Principal) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	eng := gin.New()
	h := mcp.NewHandler(svc)

	// Unauthenticated half, on the root engine.
	h.RegisterPublic(eng.Group("/api/v1"))

	// Operator-authenticated half. The middleware stands in for session auth +
	// authz.RequireAuth + authz.RequireTenant, which is exactly a principal
	// carrying a non-nil tenant. It deliberately does NOT apply
	// authz.RequireOrgScope: that gate belongs to Handler.Register itself, and
	// applying it here too would prove only that this file can add middleware.
	authed := eng.Group("/api/v1")
	authed.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), principal))
		c.Next()
	})
	h.Register(authed)

	// The transport, on the root engine, exactly as server.New mounts it.
	mcp.NewTransportHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)), "test").Register(eng)

	return eng
}

type rpcResult struct {
	status int
	body   string
}

// mcpRPC posts a JSON-RPC envelope to the mounted /mcp endpoint.
func mcpRPC(t *testing.T, eng *gin.Engine, bearer string, payload map[string]any) rpcResult {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal rpc payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, mcp.TransportPath, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.RemoteAddr = "203.0.113.7:5555"
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	eng.ServeHTTP(w, req)
	return rpcResult{status: w.Code, body: w.Body.String()}
}

// doJSON sends a JSON request (or none) and decodes the response into out,
// returning the status. It fails the test on a transport-level problem only --
// a non-2xx is data for the caller to assert on, never something swallowed here.
func mcpDoJSON(t *testing.T, eng *gin.Engine, method, path string, body any,
	_ http.Header, out any) int {
	t.Helper()

	var reader *strings.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal %s %s: %v", method, path, err)
		}
		reader = strings.NewReader(string(raw))
	} else {
		reader = strings.NewReader("")
	}

	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = "203.0.113.7:5555"

	w := httptest.NewRecorder()
	eng.ServeHTTP(w, req)

	if out != nil && w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s returned %d with a body that is not JSON: %v\nbody: %s",
				method, path, w.Code, err, w.Body.String())
		}
	}
	return w.Code
}
