package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// EVERY GRANTABLE SCOPE IS ADVERTISED, AND ADVERTISING CONFERS NOTHING.
//
// An MCP client picks its scope from the 401 challenge, then from the
// protected resource document's scopes_supported, and uses that one list for
// both dynamic registration and /authorize. These tests walk that path the way
// a client does: they READ the advertised list off the wire, register with it,
// authorize with it, and approve the consent screen with and without ticks.
// Nothing below restates the list from a literal except the one expectation it
// is compared against.
// ---------------------------------------------------------------------------

// wantAdvertised is the grantable set, in the order discovery lists it.
var wantAdvertised = []string{string(ScopeRead), string(ScopeSite), string(ScopeCache)}

func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// TestAdvertisedScopes_AreEveryGrantableScope pins the advertised list to the
// registry in both directions. A scope the registry holds and discovery omits
// is one no standard client ever asks for; a scope discovery names and the
// registry does not hold is one /authorize refuses.
func TestAdvertisedScopes_AreEveryGrantableScope(t *testing.T) {
	got := AdvertisedScopes()
	if !slices.Equal(got, wantAdvertised) {
		t.Fatalf("AdvertisedScopes() = %v, want %v", got, wantAdvertised)
	}
	if registry := SupportedScopes(); !slices.Equal(sortedCopy(got), registry) {
		t.Fatalf("advertised %v but the registry holds %v; every scope this "+
			"server can grant must be advertised, and nothing else", sortedCopy(got), registry)
	}
	for s := range scopeCapabilities {
		if !slices.Contains(got, string(s)) {
			t.Errorf("scope %q confers capabilities but is not advertised", s)
		}
	}
	for _, s := range got {
		if len(scopeCapabilities[Scope(s)]) == 0 {
			t.Errorf("advertised scope %q confers nothing", s)
		}
	}

	// A fresh slice per call: a caller that appends to one cannot widen the
	// next document.
	got[0] = "mcp:planted"
	if again := AdvertisedScopes(); !slices.Equal(again, wantAdvertised) {
		t.Fatalf("mutating one result changed the next: %v", again)
	}
}

// TestDiscoveryDocuments_ServeEveryGrantableScope reads scopes_supported off
// all three mounted well-known paths, through the gin route table.
func TestDiscoveryDocuments_ServeEveryGrantableScope(t *testing.T) {
	engine := mountedEngine(t, false)
	for _, path := range []string{
		WellKnownAuthorizationServerPath,
		WellKnownProtectedResourcePath,
		WellKnownProtectedResourceMCPPath,
	} {
		res, body := getDoc(t, engine, path)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, body %s", path, res.StatusCode, body)
		}
		var doc struct {
			ScopesSupported []string `json:"scopes_supported"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if !slices.Equal(doc.ScopesSupported, wantAdvertised) {
			t.Errorf("GET %s scopes_supported = %v, want %v", path, doc.ScopesSupported, wantAdvertised)
		}
	}
}

var challengeScopeRE = regexp.MustCompile(`scope="([^"]*)"`)

// challengeScope makes an unauthenticated MCP request through the mounted
// transport and returns the scope attribute of the 401 challenge, which is
// the first source an MCP client reads its scope from.
func challengeScope(t *testing.T) string {
	t.Helper()
	r := newTransportRouter(t, &fakeStore{})
	req := httptest.NewRequest(http.MethodPost, TransportPath,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request = %d, want 401; body %s", w.Code, w.Body.String())
	}
	h := w.Header().Get("WWW-Authenticate")
	m := challengeScopeRE.FindStringSubmatch(h)
	if m == nil {
		t.Fatalf("WWW-Authenticate %q carries no scope attribute", h)
	}
	return m[1]
}

func TestUnauthorizedChallenge_NamesEveryGrantableScope(t *testing.T) {
	got := challengeScope(t)
	if want := strings.Join(wantAdvertised, " "); got != want {
		t.Fatalf("401 challenge scope = %q, want %q", got, want)
	}
}

// TestRegisteringTheAdvertisedList_RegistersEveryScope is the client's second
// step: it registers with the scope it read from the challenge.
func TestRegisteringTheAdvertisedList_RegistersEveryScope(t *testing.T) {
	store := &fakeStore{registerRows: 1}
	got, err := NewService(store).Register(context.Background(), RegistrationRequest{
		RedirectURIs:            []string{registeredRedirect},
		TokenEndpointAuthMethod: "none",
		Scope:                   challengeScope(t),
	})
	if err != nil {
		t.Fatalf("registering the advertised list was refused: %v", err)
	}
	if want := sortedCopy(wantAdvertised); !slices.Equal(got.Scopes, want) {
		t.Fatalf("registered %v, want %v", got.Scopes, want)
	}
	if len(got.DroppedScopes) != 0 {
		t.Fatalf("an advertised scope was dropped at registration: %v", got.DroppedScopes)
	}

	// Control: a registration that names no scope still registers the read
	// scope alone. Advertising widened nothing a client did not ask for.
	plain := &fakeStore{registerRows: 1}
	got, err = NewService(plain).Register(context.Background(), RegistrationRequest{
		RedirectURIs: []string{registeredRedirect}, TokenEndpointAuthMethod: "none",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !slices.Equal(got.Scopes, []string{string(ScopeRead)}) {
		t.Fatalf("a registration naming no scope registered %v, want [mcp:read]", got.Scopes)
	}
}

// advertisedClientStore is a store holding one client registered the way a
// standard client now registers: for the whole advertised list.
func advertisedClientStore(t *testing.T) *fakeStore {
	t.Helper()
	c := liveClient(registeredRedirect)
	c.RegisteredScopes = sortedCopy(strings.Split(challengeScope(t), " "))
	return &fakeStore{clientOK: true, client: c}
}

// TestAuthorizeHandler_OffersTheSiteAndCacheCapabilities drives GET
// /authorize with the advertised list and reads the consent payload the
// dashboard renders its boxes from.
func TestAuthorizeHandler_OffersTheSiteAndCacheCapabilities(t *testing.T) {
	store := advertisedClientStore(t)
	router := newAuthorizeRouter(t, store)

	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {registeredClientID},
		"redirect_uri":          {registeredRedirect},
		"scope":                 {challengeScope(t)},
		"state":                 {"s"},
		"code_challenge":        {"a-real-challenge-value"},
		"code_challenge_method": {CodeChallengeMethodS256},
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, AuthorizePath+"?"+q.Encode(), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /authorize = %d, body %s", w.Code, w.Body.String())
	}
	var resp consentResponseDTO
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(resp.Scopes, wantAdvertised) {
		t.Fatalf("consent scopes = %v, want %v", resp.Scopes, wantAdvertised)
	}
	effects := map[string]string{}
	for _, c := range resp.ConferrableCapabilities {
		effects[c.Name] = c.Effect
	}
	for name, effect := range map[Capability]Effect{
		CapAbilityRead:    EffectRead,
		CapAbilityRequest: EffectRequest,
		CapCachePurge:     EffectRequest,
		CapSitesRead:      EffectRead,
	} {
		if got, ok := effects[string(name)]; !ok || got != string(effect) {
			t.Errorf("conferrable %q = %q (present %v), want effect %q", name, got, ok, effect)
		}
	}

	// Control: a client asking for the read scope alone is offered no
	// site or cache capability, exactly as before.
	q.Set("scope", string(ScopeRead))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, AuthorizePath+"?"+q.Encode(), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /authorize (read only) = %d, body %s", w.Code, w.Body.String())
	}
	resp = consentResponseDTO{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.ConferrableCapabilities {
		switch Capability(c.Name) {
		case CapAbilityRead, CapAbilityRequest, CapCachePurge:
			t.Errorf("a read-only request was offered %q", c.Name)
		}
	}
}

// operatorPrincipal holds site.content.edit and site.cache.purge, so the
// request capabilities are its to confer and a refusal below cannot be the
// creator-permission rule standing in for the rule under test.
func operatorPrincipal() domain.Principal {
	return domain.Principal{
		Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: uuid.New(),
		Role: "operator", Scope: domain.ScopeOrg, AuthModel: domain.AuthModelRole,
	}
}

// TestApproveTheAdvertisedList_ConfersExactlyWhatIsTicked is the consent half:
// asking for every scope offers every box, and the grant holds what the
// operator ticked and nothing more.
func TestApproveTheAdvertisedList_ConfersExactlyWhatIsTicked(t *testing.T) {
	reads := []Capability{CapActivityRead, CapBackupsRead, CapDiagnosticsRead,
		CapPerformanceRead, CapSecurityRead, CapSitesRead, CapUptimeRead}

	cases := []struct {
		name   string
		ticked *[]Capability
		want   []Capability
	}{
		{"no list: the preset, never a site or cache capability", nil,
			[]Capability{CapSitesRead}},
		{"the reads only: both boxes left clear", &reads, reads},
		{"site tools ticked", ptrCaps(append(slices.Clone(reads), CapAbilityRead, CapAbilityRequest)),
			append(slices.Clone(reads), CapAbilityRead, CapAbilityRequest)},
		{"cache ticked", ptrCaps(append(slices.Clone(reads), CapCachePurge)),
			append(slices.Clone(reads), CapCachePurge)},
		{"one site box only", ptrCaps([]Capability{CapSitesRead, CapAbilityRead}),
			[]Capability{CapSitesRead, CapAbilityRead}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := advertisedClientStore(t)
			svc := approvalService(store)
			consent := authorizeForTest(t, svc, challengeScope(t))

			req := validApproval()
			req.Principal = operatorPrincipal()
			req.Consent = consent
			req.Capabilities = tc.ticked
			if _, err := svc.Approve(context.Background(), req); err != nil {
				t.Fatalf("Approve: %v", err)
			}
			got := storedCapabilities(t, store)
			want := capabilityNames(NewCapabilitySet(tc.want).Sorted())
			if !slices.Equal(sortedCopy(got), sortedCopy(want)) {
				t.Fatalf("stored %v, want exactly %v", got, want)
			}
		})
	}
}

func ptrCaps(c []Capability) *[]Capability { return &c }

// TestAuthorizeTheAdvertisedList_NarrowedForAReadOnlyRegistration is what an
// existing client meets at its next sign-in. It registered for mcp:read alone
// (m140 filled every older row with exactly that), it keeps that client_id, and
// it asks for the advertised list it read off the challenge. The request is
// narrowed to the registration rather than refused: the client signs in with
// the read scope, the withheld scopes are named for the consent screen, and
// nothing outside the registration reaches the ticket or the grant.
func TestAuthorizeTheAdvertisedList_NarrowedForAReadOnlyRegistration(t *testing.T) {
	store := approvalStore() // registered for mcp:read alone
	if !slices.Equal(store.client.RegisteredScopes, []string{string(ScopeRead)}) {
		t.Fatalf("fixture registered for %v; this test needs [mcp:read] alone", store.client.RegisteredScopes)
	}
	svc := approvalService(store)
	consent := authorizeForTest(t, svc, challengeScope(t))

	if !slices.Equal(consent.Scopes, []Scope{ScopeRead}) {
		t.Fatalf("consent scopes = %v, want [mcp:read], the overlap with the registration", consent.Scopes)
	}
	if want := []Scope{ScopeSite, ScopeCache}; !slices.Equal(consent.UnregisteredScopes, want) {
		t.Fatalf("unregistered scopes = %v, want %v in the order asked", consent.UnregisteredScopes, want)
	}
	claims, ok := testTicketCodec.open(consent.ConsentTicket, time.Now())
	if !ok {
		t.Fatal("the consent ticket does not open under the test key")
	}
	if !slices.Equal(claims.Scopes, []string{string(ScopeRead)}) {
		t.Fatalf("the ticket seals %v; a scope outside the registration must never be sealed", claims.Scopes)
	}
	for _, c := range consent.ConferrableCapabilities {
		switch c.Name {
		case CapAbilityRead, CapAbilityRequest, CapCachePurge:
			t.Errorf("the narrowed consent offers %q, which only a withheld scope confers", c.Name)
		}
	}

	// The withheld scopes cannot be spent with this ticket: an approval body
	// naming the request as the client sent it is refused by the binding.
	widened := approvalFor(consent)
	widened.Principal = operatorPrincipal()
	widened.Consent.Scopes = []Scope{ScopeRead, ScopeSite, ScopeCache}
	_, err := svc.Approve(context.Background(), widened)
	if de, ok := domain.AsDomain(err); !ok || de.Code != ErrCodeScopeNotAuthorized {
		t.Fatalf("Approve with the withheld scopes = %v, want %s", err, ErrCodeScopeNotAuthorized)
	}
	if len(store.approved) != 0 {
		t.Fatalf("a refused approval wrote %d grants", len(store.approved))
	}

	// The narrowed consent approves, and the grant holds the read scope alone.
	req := approvalFor(consent)
	req.Principal = operatorPrincipal()
	if _, err := svc.Approve(context.Background(), req); err != nil {
		t.Fatalf("Approve of the narrowed consent: %v", err)
	}
	if len(store.approved) != 1 {
		t.Fatalf("want 1 grant written, got %d", len(store.approved))
	}
	if got := store.approved[0].OauthScopes; !slices.Equal(got, []string{string(ScopeRead)}) {
		t.Fatalf("the grant stores scopes %v, want [mcp:read]", got)
	}
}

// TestAuthorizeHandler_NamesTheWithheldScopes reads unregistered_scopes off
// the consent payload the dashboard renders, for a read-only registration
// asking for the advertised list, and checks the field is [] rather than null
// when nothing was withheld.
func TestAuthorizeHandler_NamesTheWithheldScopes(t *testing.T) {
	get := func(store *fakeStore) map[string]json.RawMessage {
		t.Helper()
		q := url.Values{
			"response_type":         {"code"},
			"client_id":             {registeredClientID},
			"redirect_uri":          {registeredRedirect},
			"scope":                 {challengeScope(t)},
			"code_challenge":        {"a-real-challenge-value"},
			"code_challenge_method": {CodeChallengeMethodS256},
		}
		w := httptest.NewRecorder()
		newAuthorizeRouter(t, store).ServeHTTP(w,
			httptest.NewRequest(http.MethodGet, AuthorizePath+"?"+q.Encode(), nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET /authorize = %d, body %s", w.Code, w.Body.String())
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}

	raw := get(approvalStore()) // registered for mcp:read alone
	if got := string(raw["scopes"]); got != `["mcp:read"]` {
		t.Fatalf("scopes = %s, want [\"mcp:read\"]", got)
	}
	if got := string(raw["unregistered_scopes"]); got != `["mcp:site","mcp:cache"]` {
		t.Fatalf("unregistered_scopes = %s, want [\"mcp:site\",\"mcp:cache\"]", got)
	}

	raw = get(advertisedClientStore(t)) // registered for the whole list
	if got := string(raw["unregistered_scopes"]); got != `[]` {
		t.Fatalf("unregistered_scopes = %s with nothing withheld, want []", got)
	}
}

// TestAuthorizeTheAdvertisedList_RefusedWhenNothingOverlaps keeps the refusal
// for the request narrowing cannot answer: a client registered for mcp:read
// alone asking only for scopes it never registered for is refused by name, and
// no ticket is sealed.
func TestAuthorizeTheAdvertisedList_RefusedWhenNothingOverlaps(t *testing.T) {
	store := approvalStore() // registered for mcp:read alone
	got, err := consentSvc(store).Authorize(context.Background(),
		authorizeReqWithScope(string(ScopeSite)+" "+string(ScopeCache)))
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != ErrCodeInvalidScope {
		t.Fatalf("Authorize = %v, want %s when nothing overlaps the registration", err, ErrCodeInvalidScope)
	}
	if de.Details["unregistered_scope"] != string(ScopeSite) {
		t.Fatalf("refusal details = %v, want unregistered_scope %q", de.Details, ScopeSite)
	}
	if got.ConsentTicket != "" {
		t.Fatal("a refused authorize call still sealed a consent ticket")
	}
}
