// mcp_advertised_scopes_oauth_integration_test.go: a standard MCP client's
// sign-in, driven the way the client drives it. The scope is READ OFF THE 401
// CHALLENGE and the protected resource document, never written here, then used
// for registration and /authorize, and the consent is approved with the site
// and cache boxes left clear and then ticked. Every write and read goes through
// the mounted routes and the service's own tx helpers, as wpmgr_app.
package tests

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/govcontext"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

func TestMCPOAuthAdvertisedScopesReachTheConsentAndOnlyTicksAreGranted(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	if err := pool.InMCPClientLookupTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InMCPClientLookupTx")
		return nil
	}); err != nil {
		t.Fatalf("open lookup tx: %v", err)
	}

	tenantID := seedTenant(t, pool, "mcp-adv-"+uuid.NewString()[:8])
	userID := seedUserRow(t, pool, "mcp-adv-"+uuid.NewString()[:8]+"@example.test")
	seedSite(t, pool, tenantID, "https://"+uuid.NewString()[:8]+".example.test")

	svc := auditedMCPService(pool, mcp.NewRepo(pool)).
		WithContextResolver(&govcontext.Resolver{Store: govcontext.NewRepo(pool)})
	// admin holds api_key.manage (both routes that create a connection),
	// site.content.edit and site.cache.purge (the two request capabilities).
	principal := domain.Principal{UserID: userID, TenantID: tenantID, Role: "admin", Scope: domain.ScopeOrg}
	eng := mountLikeProduction(t, svc, principal)
	mcp.NewDiscoveryHandler("https://manage.example.test").Register(eng)

	const redirectURI = "https://claude.ai/api/mcp/auth_callback"
	want := []string{string(mcp.ScopeRead), string(mcp.ScopeSite), string(mcp.ScopeCache)}

	// STEP 1: what a client reads before it holds anything.
	anon := httptest.NewRecorder()
	anonReq := httptest.NewRequest(http.MethodPost, mcp.TransportPath,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	anonReq.RemoteAddr = "203.0.113.7:5555"
	eng.ServeHTTP(anon, anonReq)
	if anon.Code != http.StatusUnauthorized {
		t.Fatalf("STEP 1 unauthenticated POST /mcp = %d, want 401", anon.Code)
	}
	m := regexp.MustCompile(`scope="([^"]*)"`).FindStringSubmatch(anon.Header().Get("WWW-Authenticate"))
	if m == nil {
		t.Fatalf("STEP 1 challenge %q names no scope", anon.Header().Get("WWW-Authenticate"))
	}
	advertised := m[1]
	if advertised != strings.Join(want, " ") {
		t.Fatalf("STEP 1 challenge scope = %q, want %q", advertised, strings.Join(want, " "))
	}
	var prm struct {
		ScopesSupported []string `json:"scopes_supported"`
	}
	if code := mcpDoJSON(t, eng, http.MethodGet, mcp.WellKnownProtectedResourceMCPPath, nil, nil, &prm); code != http.StatusOK {
		t.Fatalf("STEP 1 protected resource metadata = %d", code)
	}
	if !slices.Equal(prm.ScopesSupported, want) {
		t.Fatalf("STEP 1 scopes_supported = %v, want %v", prm.ScopesSupported, want)
	}
	t.Logf("STEP 1 ok: challenge and scopes_supported both advertise %q", advertised)

	// STEP 2: register with the advertised list, as the client does.
	register := func(scope string) string {
		body := map[string]any{
			"redirect_uris": []string{redirectURI}, "client_name": "Claude Code",
			"token_endpoint_auth_method": "none",
		}
		if scope != "" {
			body["scope"] = scope
		}
		var reg struct {
			ClientID string `json:"client_id"`
			Scope    string `json:"scope"`
		}
		if code := mcpDoJSON(t, eng, http.MethodPost, mcp.RegisterPath, body, nil, &reg); code != http.StatusCreated {
			t.Fatalf("STEP 2 register(%q) = %d", scope, code)
		}
		return reg.ClientID + "|" + reg.Scope
	}
	parts := strings.SplitN(register(advertised), "|", 2)
	clientID, registered := parts[0], strings.Split(parts[1], " ")
	slices.Sort(registered)
	if wantSorted := slices.Sorted(slices.Values(want)); !slices.Equal(registered, wantSorted) {
		t.Fatalf("STEP 2 registered %v, want %v", registered, wantSorted)
	}
	t.Logf("STEP 2 ok: client %s registered for %v", clientID, registered)

	// flow runs authorize -> consent -> token for one client and scope, with
	// the given capability ticks, and returns the stored grant.
	flow := func(step, client, scope string, ticked func([]map[string]string) []string) mcp.Connection {
		t.Helper()
		verifier := "adv-" + uuid.NewString() + uuid.NewString()
		sum := sha256.Sum256([]byte(verifier))
		challenge := base64.RawURLEncoding.EncodeToString(sum[:])
		q := url.Values{
			"response_type": {"code"}, "client_id": {client}, "redirect_uri": {redirectURI},
			"scope": {scope}, "state": {step}, "code_challenge": {challenge},
			"code_challenge_method": {"S256"},
		}
		var screen struct {
			Scopes        []string            `json:"scopes"`
			ConsentTicket string              `json:"consent_ticket"`
			Conferrable   []map[string]string `json:"conferrable_capabilities"`
		}
		if code := mcpDoJSON(t, eng, http.MethodGet, mcp.AuthorizePath+"?"+q.Encode(), nil, nil, &screen); code != http.StatusOK {
			t.Fatalf("%s authorize = %d", step, code)
		}
		caps := ticked(screen.Conferrable)
		var approval struct {
			GrantID string `json:"grant_id"`
			Code    string `json:"code"`
		}
		if code := mcpDoJSON(t, eng, http.MethodPost, mcp.ConsentPath, map[string]any{
			"client_id": client, "redirect_uri": redirectURI, "scopes": screen.Scopes,
			"state": step, "code_challenge": challenge, "code_challenge_method": "S256",
			"name": step, "site_scope_mode": string(mcp.SiteScopeModeAll),
			"consent_ticket": screen.ConsentTicket, "capabilities": caps,
		}, nil, &approval); code != http.StatusOK {
			t.Fatalf("%s consent = %d", step, code)
		}
		form := url.Values{
			"grant_type": {"authorization_code"}, "code": {approval.Code},
			"redirect_uri": {redirectURI}, "client_id": {client}, "code_verifier": {verifier},
		}
		req := httptest.NewRequest(http.MethodPost, mcp.TokenPath, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "203.0.113.7:5555"
		w := httptest.NewRecorder()
		eng.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s token = %d: %s", step, w.Code, w.Body.String())
		}
		var tok struct {
			AccessToken string `json:"access_token"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &tok); err != nil || tok.AccessToken == "" {
			t.Fatalf("%s token response unusable: %v %s", step, err, w.Body.String())
		}
		if res := mcpRPC(t, eng, tok.AccessToken, map[string]any{
			"jsonrpc": "2.0", "id": 2, "method": "tools/list",
		}); res.status != http.StatusOK {
			t.Fatalf("%s tools/list with the minted token = %d: %s", step, res.status, res.body)
		}
		conns, err := svc.ListConnections(ctx, principal)
		if err != nil {
			t.Fatalf("%s ListConnections: %v", step, err)
		}
		for _, c := range conns {
			if c.ID.String() == approval.GrantID {
				return c
			}
		}
		t.Fatalf("%s grant %s not found through ListConnections", step, approval.GrantID)
		return mcp.Connection{}
	}

	// The dashboard's payload with both boxes clear: the conferred reads,
	// never the site-tools read, which is its own box.
	readsOnly := func(conf []map[string]string) []string {
		var out []string
		for _, c := range conf {
			if c["effect"] == string(mcp.EffectRead) && c["name"] != string(mcp.CapAbilityRead) {
				out = append(out, c["name"])
			}
		}
		return out
	}
	capNames := func(c mcp.Connection) []string {
		out := make([]string, 0, len(c.Capabilities))
		for _, x := range c.Capabilities {
			out = append(out, string(x))
		}
		slices.Sort(out)
		return out
	}

	// STEP 3: boxes left clear. The grant holds the reads and nothing else.
	cleared := flow("STEP 3", clientID, advertised, readsOnly)
	for _, held := range cleared.Capabilities {
		switch held {
		case mcp.CapAbilityRead, mcp.CapAbilityRequest, mcp.CapCachePurge:
			t.Fatalf("STEP 3 grant holds %q with every box left clear", held)
		}
	}
	t.Logf("STEP 3 ok: boxes clear, grant holds %v", capNames(cleared))

	// STEP 4: every box ticked. The grant holds exactly the reads plus the three.
	all := func(conf []map[string]string) []string {
		return append(readsOnly(conf), string(mcp.CapAbilityRead), string(mcp.CapAbilityRequest), string(mcp.CapCachePurge))
	}
	ticked := flow("STEP 4", clientID, advertised, all)
	if got := capNames(ticked); !slices.Contains(got, string(mcp.CapAbilityRead)) ||
		!slices.Contains(got, string(mcp.CapAbilityRequest)) || !slices.Contains(got, string(mcp.CapCachePurge)) ||
		len(got) != len(capNames(cleared))+3 {
		t.Fatalf("STEP 4 grant holds %v, want the reads %v plus the three ticked", got, capNames(cleared))
	}
	t.Logf("STEP 4 ok: boxes ticked, grant holds %v", capNames(ticked))

	// STEP 5: a client asking for mcp:read alone, unchanged.
	readOnlyClient := strings.SplitN(register(""), "|", 2)
	plain := flow("STEP 5", readOnlyClient[0], string(mcp.ScopeRead), readsOnly)
	if len(plain.Scopes) != 1 || plain.Scopes[0] != mcp.ScopeRead {
		t.Fatalf("STEP 5 read-only grant scopes = %v, want [mcp:read]", plain.Scopes)
	}
	t.Logf("STEP 5 ok: read-only request granted %v with %v", plain.Scopes, capNames(plain))

	// STEP 6: that read-only registration asking for the advertised list is
	// refused at /authorize with invalid_scope, the case an existing client meets.
	q := url.Values{
		"response_type": {"code"}, "client_id": {readOnlyClient[0]}, "redirect_uri": {redirectURI},
		"scope": {advertised}, "code_challenge": {"x"}, "code_challenge_method": {"S256"},
	}
	var oerr struct {
		Error string `json:"error"`
	}
	if code := mcpDoJSON(t, eng, http.MethodGet, mcp.AuthorizePath+"?"+q.Encode(), nil, nil, &oerr); code != http.StatusBadRequest || oerr.Error != "invalid_scope" {
		t.Fatalf("STEP 6 authorize = %d %q, want 400 invalid_scope", code, oerr.Error)
	}
	t.Log("STEP 6 ok: a read-only registration asking for the advertised list is refused with invalid_scope")
}
