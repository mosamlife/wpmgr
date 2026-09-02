package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// THE CONSENT PATH'S CAPABILITY SET.
//
// Approve stored capabilityNames(DefaultGrantCapabilities()) as a LITERAL, so
// every browser-sign-in connection held the same capabilities whatever the
// operator chose. The token path had accepted a list for as long as
// resolveGrantCapabilities has existed. Two paths creating one object, with one
// of them ignoring half the request, is the asymmetry these tests pin shut.
//
// Everything below asserts THE STORED SET -- the `capabilities` column value
// handed to the INSERT -- and not the request that produced it. A grant is what
// its row says it is; asserting that Approve returned no error proves nothing
// about what the connection may reach.
// ---------------------------------------------------------------------------

// approvalService is Approve's honest fixture: the audit recorder is wired,
// because Approve writes ActionMCPGrantCreated inside the grant transaction and
// refuses the whole approval when it cannot. A service without one refuses
// before the capability set is ever stored, which would make every assertion
// below vacuous.
func approvalService(store *fakeStore) *Service {
	return NewService(store).withAuditRecorder(&capturingRecorder{})
}

// storedCapabilities is the capability column of the single grant the consent
// path wrote. It fails rather than returns a zero value when no grant was
// written, because "the connection was created with nothing" and "no connection
// was created" are different outcomes and only one of them is a pass.
func storedCapabilities(t *testing.T, store *fakeStore) []string {
	t.Helper()
	if len(store.approved) != 1 {
		t.Fatalf("want exactly 1 grant written by the consent path, got %d", len(store.approved))
	}
	return store.approved[0].Capabilities
}

// TestApproveStoresTheOperatorsNarrowedChoice is the whole point of the change:
// a capability list on the approval reaches the grant row.
//
// The chosen set is deliberately NOT the default preset and NOT the whole
// ceiling. A test that asked for {mcp.sites.read} would pass against the
// hard-coded literal it replaces, and one that asked for the ceiling would pass
// against any implementation that ignores narrowing entirely.
func TestApproveStoresTheOperatorsNarrowedChoice(t *testing.T) {
	store := approvalStore()
	req := validApproval()
	chosen := []Capability{CapSitesRead, CapUptimeRead}
	req.Capabilities = &chosen

	if _, err := approvalService(store).Approve(context.Background(), req); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	got := storedCapabilities(t, store)
	want := capabilityNames(NewCapabilitySet(chosen).Sorted())
	if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
		t.Fatalf("the consent path stored %v, want the operator's choice %v.\n"+
			"A connection that holds capabilities its operator did not tick is a "+
			"grant nobody approved.", got, want)
	}
	// And it is genuinely narrower than the ceiling, or the assertion above
	// would be satisfied by an implementation that stores everything.
	ceiling, err := OrgDefaultCapabilities(grantScopes())
	if err != nil {
		t.Fatalf("OrgDefaultCapabilities: %v", err)
	}
	if len(got) >= ceiling.Len() {
		t.Fatalf("the chosen set %v is not narrower than the ceiling %v, so this test "+
			"cannot tell narrowing from storing everything",
			got, capabilityNames(ceiling.Sorted()))
	}
}

// TestApproveWithNoCapabilityListStoresThePresetNotTheCeiling pins the ABSENT
// request, which is the case the frontend hits for every client that does not
// render the picker.
//
// The answer to "nobody asked" is the preset, never the widest set the
// organisation ceiling allows. It is the same rule the token path already
// holds, asserted here on the row rather than on the resolver, so a consent
// path that stopped calling the resolver at all goes red.
func TestApproveWithNoCapabilityListStoresThePresetNotTheCeiling(t *testing.T) {
	store := approvalStore()
	req := validApproval()
	req.Capabilities = nil

	if _, err := approvalService(store).Approve(context.Background(), req); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	got := storedCapabilities(t, store)
	want := capabilityNames(DefaultGrantCapabilities())
	if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
		t.Fatalf("an omitted capability list stored %v, want exactly the preset %v", got, want)
	}
	if len(got) == 0 {
		t.Fatal("an EMPTY capability set was stored: this connection would authenticate " +
			"and reach no tool")
	}
}

// TestApproveRefusesACapabilitySetWiderThanTheCeiling is the refusal arm.
// NarrowTo refuses rather than intersects, and the refusal has to survive the
// trip through this path: an approval that quietly dropped the capability would
// tell the operator they approved something they did not.
//
// NOTHING IS WRITTEN when it refuses -- no grant and no authorization code.
func TestApproveRefusesACapabilitySetWiderThanTheCeiling(t *testing.T) {
	cases := map[string][]Capability{
		// Not in the vocabulary at all.
		"unknown name": {CapSitesRead, Capability("mcp.sites.write")},
		// SEATED AND DELIBERATELY UNREACHABLE: mcp.content.read is in the m131
		// vocabulary and is conferred by no scope, so it is never inside the
		// ceiling and can never be granted. Step 4 renders its row disabled;
		// this is the server-side half of that, on the consent path.
		"seated but conferred by no scope": {CapSitesRead, CapContentRead},
	}

	for name, chosen := range cases {
		t.Run(name, func(t *testing.T) {
			store := approvalStore()
			req := validApproval()
			req.Capabilities = &chosen

			got, err := approvalService(store).Approve(context.Background(), req)
			if err == nil {
				t.Fatalf("Approve accepted %v and minted code %q", chosen, got.Code)
			}
			if len(store.approved) != 0 {
				t.Fatalf("a refused approval still wrote %d grant(s) with %v",
					len(store.approved), store.approved[0].Capabilities)
			}
			for _, c := range store.calls {
				if c == "CreateGrantWithCode" {
					t.Fatal("a refused approval still reached the grant insert")
				}
			}
		})
	}
}

// TestApproveRefusesAnExplicitlyEmptyCapabilityList is the empty-array arm, and
// it is a REFUSAL rather than the default on purpose.
//
// An operator who unticks every capability has asked for a connection that can
// reach no tool. Reading that as "you did not narrow" would grant a capability
// they had just removed, and reading it as "narrow to nothing" would mint a
// credential that authenticates and then refuses every request. Neither is an
// answer, so the request is.
func TestApproveRefusesAnExplicitlyEmptyCapabilityList(t *testing.T) {
	store := approvalStore()
	req := validApproval()
	empty := []Capability{}
	req.Capabilities = &empty

	if _, err := approvalService(store).Approve(context.Background(), req); err == nil {
		t.Fatal("an explicitly empty capability list was accepted; " +
			"it must not silently become the default preset")
	}
	if len(store.approved) != 0 {
		t.Fatalf("a refused approval wrote a grant holding %v", store.approved[0].Capabilities)
	}
}

// ---------------------------------------------------------------------------
// THE WIRE SPELLING. The frontend has to match it exactly, so it is pinned
// through the mounted route and not only through the service struct.
// ---------------------------------------------------------------------------

// TestConsentHandlerCarriesTheCapabilityFieldToTheGrant proves the JSON key is
// `capabilities` and that its value reaches the row. A field the handler
// decodes and drops looks identical from the outside to one it never decoded.
func TestConsentHandlerCarriesTheCapabilityFieldToTheGrant(t *testing.T) {
	store := approvalStore()
	router := newAuthorizeRouter(t, store)
	NewHandler(approvalService(store)).Register(router.Group("/api/v2"))

	chosen := []string{string(CapSitesRead), string(CapUptimeRead)}
	body := approvalRequestDTO{
		ClientID:            registeredClientID,
		RedirectURI:         registeredRedirect,
		Scopes:              []string{"mcp:read"},
		State:               "s",
		CodeChallenge:       "challenge",
		CodeChallengeMethod: "S256",
		GrantName:           "Priya's laptop",
		SiteScopeMode:       "all",
		Capabilities:        &chosen,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"capabilities":["mcp.sites.read","mcp.uptime.read"]`) {
		t.Fatalf("the wire spelling changed: %s", raw)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v2/oauth/mcp/consent", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST /consent = %d, body = %s", w.Code, w.Body.String())
	}
	got := storedCapabilities(t, store)
	if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(chosen))) {
		t.Fatalf("the handler stored %v for a body naming %v", got, chosen)
	}
}

// TestConsentHandlerRefusesAnEmptyCapabilityArray is the wire half of the
// empty-array decision: `"capabilities": []` is a 4xx and writes nothing, so
// the frontend learns it must not send one rather than silently receiving a
// connection wider than the screen showed.
func TestConsentHandlerRefusesAnEmptyCapabilityArray(t *testing.T) {
	store := approvalStore()
	router := newAuthorizeRouter(t, store)
	NewHandler(approvalService(store)).Register(router.Group("/api/v2"))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v2/oauth/mcp/consent", strings.NewReader(
		`{"client_id":"`+registeredClientID+`","redirect_uri":"`+registeredRedirect+`",`+
			`"scopes":["mcp:read"],"state":"s","code_challenge":"challenge",`+
			`"code_challenge_method":"S256","name":"Priya's laptop",`+
			`"site_scope_mode":"all","capabilities":[]}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Fatalf("POST /consent accepted an empty capability array: %s", w.Body.String())
	}
	if len(store.approved) != 0 {
		t.Fatalf("a refused consent wrote a grant holding %v", store.approved[0].Capabilities)
	}
}
