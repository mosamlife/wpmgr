package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// STEP 10'S TOOL LIST.
//
// The screen exists to stop one failure: an operator reading a tool name off a
// "you're connected" page, asking for it, and being refused. So the ONLY
// acceptable answer is registry.VisibleTools for this grant -- the same
// membership and the same descriptions the model receives from tools/list.
//
// EVERY TEST BELOW USES A GRANT THAT DOES NOT HOLD EVERYTHING. A grant holding
// the capability both registry tools require would make an endpoint returning
// the raw registry indistinguishable from one that resolves the grant, and the
// proof would be vacuous.
// ---------------------------------------------------------------------------

// toolsPath is the route under test, built from the same constants the router
// mounts so a rename moves both.
func toolsPath(id uuid.UUID) string {
	return ConnectionsPath + "/" + id.String() + "/tools"
}

// grantHolding is a stored grant carrying exactly the capabilities named.
func grantHolding(id uuid.UUID, caps ...Capability) sqlc.McpGrant {
	return sqlc.McpGrant{
		ID:            id,
		Name:          "Priya's laptop",
		Status:        string(GrantStatusActive),
		SiteScopeMode: string(SiteScopeModeAll),
		Capabilities:  capabilityNames(caps),
	}
}

// TestConnectionToolsAnswersWithTheGrantsOwnList is the central proof: the
// endpoint's answer is VisibleTools for THIS grant, not the registry.
//
// The grant holds mcp.uptime.read and NOT mcp.sites.read, which every registry
// tool requires. Both tools are inside the organisation ceiling, so both are
// LISTED -- capability narrowing explains, it does not hide (ruling 9) -- and
// each description carries the permanent refusal naming the capability the
// grant lacks. An endpoint returning the plain registry passes none of this.
func TestConnectionToolsAnswersWithTheGrantsOwnList(t *testing.T) {
	tenantID := uuid.New()
	grantID := uuid.New()
	store := &fakeStore{grants: []sqlc.McpGrant{grantHolding(grantID, CapUptimeRead)}}

	got, err := NewService(store).ConnectionTools(context.Background(),
		orgPrincipal(tenantID), grantID)
	if err != nil {
		t.Fatalf("ConnectionTools: %v", err)
	}

	ceiling, err := OrgDefaultCapabilities(grantScopes())
	if err != nil {
		t.Fatalf("OrgDefaultCapabilities: %v", err)
	}
	want := VisibleTools(AuthorizedRequest{
		TenantID:     tenantID,
		GrantID:      grantID,
		Capabilities: NewCapabilitySet([]Capability{CapUptimeRead}),
		OrgCeiling:   ceiling,
	})
	if len(got) != len(want) {
		t.Fatalf("the endpoint returned %d tools, VisibleTools returns %d for this grant",
			len(got), len(want))
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].Description != want[i].Description {
			t.Fatalf("tool %d is not what this grant can see.\n got: %q / %q\nwant: %q / %q",
				i, got[i].Name, got[i].Description, want[i].Name, want[i].Description)
		}
	}

	// AND IT IS NOT THE REGISTRY'S OWN ANSWER. Tools() carries the unannotated
	// descriptions; if the two agreed, the assertion above would be satisfied
	// by an endpoint that never looked at the grant at all.
	registry := Tools()
	same := len(registry) == len(got)
	if same {
		for i := range registry {
			if registry[i].Description != got[i].Description {
				same = false
				break
			}
		}
	}
	if same {
		t.Fatal("the endpoint's answer is byte-identical to the unnarrowed registry, " +
			"so this test cannot tell a resolved list from a synthesised one")
	}
	for _, tool := range got {
		if !strings.Contains(tool.Description, string(CapSitesRead)) {
			t.Fatalf("tool %q is listed without the capability it needs named in its "+
				"description; the operator is being shown a tool that would refuse, "+
				"with no reason on the screen", tool.Name)
		}
	}
}

// TestConnectionToolsDoesNotAnnotateAToolTheGrantHolds is the over-fire half.
// A guard that annotated everything would pass the test above and be useless:
// a grant that DOES hold the capability must get the registry's own
// description, unmarked.
func TestConnectionToolsDoesNotAnnotateAToolTheGrantHolds(t *testing.T) {
	tenantID := uuid.New()
	grantID := uuid.New()
	store := &fakeStore{grants: []sqlc.McpGrant{grantHolding(grantID, CapSitesRead)}}

	got, err := NewService(store).ConnectionTools(context.Background(),
		orgPrincipal(tenantID), grantID)
	if err != nil {
		t.Fatalf("ConnectionTools: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("a grant holding mcp.sites.read was shown no tools at all")
	}
	byName := map[string]string{}
	for _, tool := range Tools() {
		byName[tool.Name] = tool.Description
	}
	for _, tool := range got {
		want, ok := byName[tool.Name]
		if !ok {
			t.Fatalf("the endpoint returned %q, which is not in the registry", tool.Name)
		}
		if tool.Description != want {
			t.Fatalf("tool %q carries an annotation for a capability the grant holds:\n%s",
				tool.Name, tool.Description)
		}
	}
}

// TestConnectionToolsRefusesAGrantHoldingNoCapability keeps an empty stored set
// out of the "this connection has no tools" rendering.
//
// The column's shape CHECK admits '{}' and Authenticate refuses it by name on
// every request, so a connection in that state is REFUSED rather than empty.
// Answering 200 with [] would put a fact about the tool surface on the screen
// for a credential that cannot make a request at all.
func TestConnectionToolsRefusesAGrantHoldingNoCapability(t *testing.T) {
	tenantID := uuid.New()
	grantID := uuid.New()
	empty := grantHolding(grantID)
	empty.Capabilities = []string{}
	store := &fakeStore{grants: []sqlc.McpGrant{empty}}

	got, err := NewService(store).ConnectionTools(context.Background(),
		orgPrincipal(tenantID), grantID)
	if err == nil {
		t.Fatalf("a grant holding no capability answered with %d tools instead of refusing",
			len(got))
	}
	if got != nil {
		t.Fatal("a refusal returned a tool list beside its error")
	}
}

// TestConnectionToolsRefusesASiteScopedPrincipal is the service-layer half of
// the org-scope gate. A grant is an organisation-wide credential with no
// :siteId for RequireSiteAccess to key on, so a site-constrained principal is
// refused outright rather than filtered -- the same refusal ConnectionStatus
// and ListConnections make, restated here so a caller arriving without the
// route middleware is still refused.
func TestConnectionToolsRefusesASiteScopedPrincipal(t *testing.T) {
	tenantID := uuid.New()
	grantID := uuid.New()
	store := &fakeStore{grants: []sqlc.McpGrant{grantHolding(grantID, CapSitesRead)}}

	siteScoped := domain.Principal{
		Type:           domain.PrincipalUser,
		UserID:         uuid.New(),
		TenantID:       tenantID,
		Role:           "admin",
		Scope:          domain.ScopeSite,
		AllowedSiteIDs: []uuid.UUID{uuid.New()},
	}
	if _, err := NewService(store).ConnectionTools(context.Background(), siteScoped, grantID); err == nil {
		t.Fatal("a site-scoped principal read what an organisation-wide connection can do")
	}
	for _, c := range store.calls {
		if c == "GetGrant" {
			t.Fatal("the refusal happened after the read rather than before it")
		}
	}
}

// TestConnectionToolsIsA404ForAnUnknownConnection: absent, foreign and
// RLS-refused all answer the same, or the difference is an oracle for which
// ids exist.
func TestConnectionToolsIsA404ForAnUnknownConnection(t *testing.T) {
	store := &fakeStore{}
	eng := newConnectionsRouter(t, store, orgPrincipal(uuid.New()))

	w := httptest.NewRecorder()
	eng.ServeHTTP(w, httptest.NewRequest(http.MethodGet, toolsPath(uuid.New()), nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("an unknown connection answered %d, want 404. body: %s", w.Code, w.Body.String())
	}
}

// TestConnectionToolsRouteAnswersTheGrantsListOnTheWire runs the whole thing
// through the MOUNTED route, with the real permission middleware, so the wire
// shape the frontend binds to is pinned as well as the resolution.
func TestConnectionToolsRouteAnswersTheGrantsListOnTheWire(t *testing.T) {
	tenantID := uuid.New()
	grantID := uuid.New()
	store := &fakeStore{grants: []sqlc.McpGrant{grantHolding(grantID, CapUptimeRead)}}
	eng := newConnectionsRouter(t, store, orgPrincipal(tenantID))

	w := httptest.NewRecorder()
	eng.ServeHTTP(w, httptest.NewRequest(http.MethodGet, toolsPath(grantID), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET tools answered %d, want 200. body: %s", w.Code, w.Body.String())
	}

	var got connectionToolListDTO
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("the tools body did not decode: %v. body: %s", err, w.Body.String())
	}
	want, err := NewService(store).ConnectionTools(context.Background(),
		orgPrincipal(tenantID), grantID)
	if err != nil {
		t.Fatalf("ConnectionTools: %v", err)
	}
	if len(got.Tools) != len(want) {
		t.Fatalf("the route returned %d tools, the service resolves %d", len(got.Tools), len(want))
	}
	for i := range want {
		if got.Tools[i].Name != want[i].Name || got.Tools[i].Description != want[i].Description {
			t.Fatalf("wire tool %d = %q / %q, want %q / %q",
				i, got.Tools[i].Name, got.Tools[i].Description, want[i].Name, want[i].Description)
		}
	}
	if !strings.Contains(w.Body.String(), `"tools":[`) {
		t.Fatalf("the response is not an object wrapping `tools`: %s", w.Body.String())
	}
}

// TestConnectionToolsRouteRefusesAWrongVerb: 405 with an Allow header, never
// gin's bare 404, because a 404 reads as "not deployed".
func TestConnectionToolsRouteRefusesAWrongVerb(t *testing.T) {
	store := &fakeStore{}
	eng := newConnectionsRouter(t, store, orgPrincipal(uuid.New()))

	w := httptest.NewRecorder()
	eng.ServeHTTP(w, httptest.NewRequest(http.MethodPost, toolsPath(uuid.New()), nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST to the tools route answered %d, want 405. body: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Allow") == "" {
		t.Error("a 405 carried no Allow header, so it does not say which verb to use")
	}
}

// TestConnectionToolsReadsUnderTheCallersTenantNotTheRequests is the proof the
// containment allowlist entry for GetGrant.grantID rests on.
//
// THE REQUEST SUPPLIES THE ID. IT DOES NOT SUPPLY THE TENANT. That sentence is
// the whole reason `:connectionId` is not a bypass of ADR-061 A11: the id binds
// to GetMCPGrant's `id` and nothing else, while the `tenant_id` beside it in
// the same WHERE clause comes from the AUTHENTICATED PRINCIPAL. Another
// organisation's grant id therefore matches no row and answers the same 404 an
// absent id answers -- which TestConnectionToolsIsA404ForAnUnknownConnection
// pins the other half of.
//
// This pins the GO half: that ConnectionTools hands the store the caller's own
// principal, unaltered, rather than one it assembled. The SQL half (the
// tenant_id predicate) and the RLS half (the RESTRICTIVE _site_scope policy,
// live only because Repo.GetGrant dispatches through RunTenantTx) are proved
// against a real database as the app role in apps/api/tests. A refactor that
// synthesised a principal here -- from the grant row, from a header, from a
// request field -- would leave both of those halves bounding the read by a
// tenant the caller never proved, and every one of those database proofs would
// still pass, because each would be asked the wrong question. This test would
// not.
//
// It asserts on the WHOLE principal and not only the tenant id, because the
// scope and the allowed-site list on it are what db.Pool.RunTenantTx dispatches
// on: a principal flattened to org scope on the way down would set the wrong
// GUCs and switch the site-scope policy off with no error.
func TestConnectionToolsReadsUnderTheCallersTenantNotTheRequests(t *testing.T) {
	caller := orgPrincipal(uuid.New())
	grantID := uuid.New()

	// The stored grant deliberately carries a DIFFERENT tenant id from the
	// caller's. Nothing in the service may prefer it: if the tenant that bounds
	// the read could come from the row, the read would be bounded by the very
	// thing it exists to check.
	stored := grantHolding(grantID, CapUptimeRead)
	stored.TenantID = uuid.New()
	store := &fakeStore{grants: []sqlc.McpGrant{stored}}

	if _, err := NewService(store).ConnectionTools(context.Background(), caller, grantID); err != nil {
		t.Fatalf("ConnectionTools: %v", err)
	}

	if len(store.getPrincipals) != 1 {
		t.Fatalf("GetGrant was handed %d principals, want exactly 1", len(store.getPrincipals))
	}
	got := store.getPrincipals[0]
	if got.TenantID != caller.TenantID {
		t.Fatalf("GetGrant ran under tenant %s, want the caller's %s -- the read is bounded by something other than the authenticated principal",
			got.TenantID, caller.TenantID)
	}
	if got.TenantID == stored.TenantID {
		t.Fatal("GetGrant ran under the STORED grant's tenant, so the row bounds its own lookup")
	}
	if got.Scope != caller.Scope {
		t.Fatalf("GetGrant ran with scope %q, want the caller's %q -- RunTenantTx dispatches on this, so a rewritten scope sets the wrong GUCs",
			got.Scope, caller.Scope)
	}
	if len(got.AllowedSiteIDs) != len(caller.AllowedSiteIDs) {
		t.Fatalf("GetGrant ran with %d allowed site ids, want the caller's %d",
			len(got.AllowedSiteIDs), len(caller.AllowedSiteIDs))
	}
	if got.UserID != caller.UserID {
		t.Fatalf("GetGrant ran as user %s, want the caller's %s", got.UserID, caller.UserID)
	}
}
