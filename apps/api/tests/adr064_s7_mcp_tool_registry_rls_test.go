// adr064_s7_mcp_tool_registry_rls_test.go: the S7 tool registry and its policy
// filtering, driven through mcp.Service.Authenticate against the REAL schema as
// wpmgr_app.
//
// WHY THIS FILE EXISTS. The S7 unit proofs in internal/mcp build an
// AuthorizedRequest by literal and hand it a chosen CapabilitySet. Everything
// they establish about the gate is real, and none of it touches the two things
// that are only decidable by executing:
//
//  1. THAT Authenticate ACTUALLY POPULATES Capabilities. The unit tests
//     construct the field directly, so a wiring defect -- Authenticate
//     returning an AuthorizedRequest whose CapabilitySet is the zero value --
//     is invisible to every one of them, and its symptom in production is a
//     connection that authenticates and then reaches no tool. The mirror defect
//     is worse and equally invisible: a future refactor that hands out a
//     capability set nobody narrowed.
//
//  2. THAT THE SITE AXIS IS RESOLVED BY THE DATABASE, NOT BY GO. The registry
//     refuses a site-keyed tool when the resolved SiteSet is empty. Whether it
//     IS empty is decided by ResolveScopeSites inside InTenantTx, under the
//     sites RLS policy. Four hours before this file was written, a reviewer read
//     that resolution closely, found no defect, and the first real-database run
//     found two swapped arguments that made every site-scoped and tag-scoped
//     grant resolve to ZERO sites -- which through the S7 gate would now present
//     as "your connection may call nothing", with the error blaming the user.
//
// HOW IT IS TESTED. Through mcp.Service.Authenticate -- the same method the
// transport calls on every request -- over a token minted through
// mcp.Repo.RedeemAuthorizationCode, the same way the token endpoint mints one.
// No connection is opened by this file and no GUC is hand-set.
//
// The role is load-bearing: wpmgr_app is NOSUPERUSER NOBYPASSRLS, and either
// privilege would make the site-axis half pass vacuously. It is asserted, and
// printed.
package tests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/govcontext"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// s7GrantWithBearer mints a grant AND a live connection token for it, returning
// the grant and the plaintext bearer.
//
// Everything goes through mcp.Repo: CreateGrantWithCode is what the consent
// endpoint calls, RedeemAuthorizationCode is what the token endpoint calls. No
// row is inserted by this file and no connection is opened by it.
func s7GrantWithBearer(
	t *testing.T, repo *mcp.Repo, tenantID uuid.UUID, mode string, siteIDs []uuid.UUID,
) (sqlc.McpGrant, string) {
	t.Helper()
	ctx := context.Background()

	clientID := "s7-client-" + uuid.NewString()
	secretHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	affected, err := repo.RegisterClient(ctx, sqlc.RegisterMCPOAuthClientParams{
		ClientID:                clientID,
		ClientSecretHash:        &secretHash,
		TokenEndpointAuthMethod: "client_secret_basic",
		RedirectUris:            []string{"https://claude.ai/api/mcp/auth_callback"},
	})
	if err != nil || affected != 1 {
		t.Fatalf("register client: affected=%d err=%v", affected, err)
	}

	if siteIDs == nil {
		// Both arrays are NOT NULL with a '{}' default, so nil is a 23502.
		siteIDs = []uuid.UUID{}
	}
	codePlain := "s7-code-" + uuid.NewString()
	codeSum := sha256.Sum256([]byte(codePlain))

	// An ORG-scoped principal: seeding a grant is what an operator does, and it
	// is the only scope the consent route now admits.
	approver := domain.Principal{TenantID: tenantID, Scope: domain.ScopeOrg}
	grant, code, err := repo.CreateGrantWithCode(ctx, approver, sqlc.CreateMCPGrantParams{
		TenantID:      tenantID,
		Name:          "s7 grant",
		Status:        "active",
		SiteScopeMode: mode,
		ScopeTagIds:   []uuid.UUID{},
		ScopeSiteIds:  siteIDs,
		ClientID:      &clientID,
		// m127: both NOT NULL with no default. See the s6b2 fixture.
		Capabilities:        []string{"mcp.sites.read"},
		ExpiresAt:           time.Now().UTC().Add(90 * 24 * time.Hour),
		IdleExpireAfterDays: nil,
	}, func(grantID uuid.UUID) sqlc.CreateMCPAuthorizationCodeParams {
		return sqlc.CreateMCPAuthorizationCodeParams{
			TenantID:            tenantID,
			GrantID:             grantID,
			ClientID:            clientID,
			CodeHash:            hex.EncodeToString(codeSum[:]),
			CodeChallenge:       "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM",
			CodeChallengeMethod: "S256",
			RedirectUri:         "https://claude.ai/api/mcp/auth_callback",
			ExpiresAt:           time.Now().UTC().Add(5 * time.Minute),
		}
	}, nil)
	if err != nil {
		t.Fatalf("create grant for tenant %s: %v", tenantID, err)
	}

	// The bearer's hash is what the lookup index is queried on. The
	// construction is the one every credential column's CHECK requires:
	// lower-case hex SHA-256.
	bearer := "s7-bearer-" + uuid.NewString()
	bearerSum := sha256.Sum256([]byte(bearer))

	tok, err := repo.RedeemAuthorizationCode(ctx, tenantID, code.ID,
		sqlc.CreateMCPConnectionTokenParams{
			TenantID:    tenantID,
			GrantID:     grant.ID,
			TokenPrefix: "s7test",
			TokenHash:   hex.EncodeToString(bearerSum[:]),
			Status:      "active",
		})
	if err != nil {
		t.Fatalf("mint connection token: %v", err)
	}
	if tok.Status != "active" {
		t.Fatalf("minted token status = %q, want active", tok.Status)
	}
	return grant, bearer
}

// TestS7CapabilitiesAreResolvedByAuthenticateAsAppRole is question 1: a real
// bearer, resolved by the real Authenticate against the real schema, must come
// back holding a capability -- and the registry must then admit the tool.
//
// THE ZERO-VALUE ASSERTION IS THE POINT. CapabilitySet's zero value allows
// nothing, which is the safe direction and is exactly why a wiring defect here
// is silent: everything still refuses, and it refuses for a reason that reads
// like a scoping problem the operator does not have.
func TestS7CapabilitiesAreResolvedByAuthenticateAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	mcpRepo := mcp.NewRepo(pool)
	siteRepo := site.NewRepo(pool)
	// THE CONTEXT RESOLVER IS WIRED BECAUSE PRODUCTION WIRES IT: both non-test
	// call sites of mcp.NewService (cmd/wpmgr/main.go, cmd/dump-routes/routes.go)
	// chain WithContextResolver, and a Service without one refuses every fleet
	// tool call rather than serving with the operator's governance silently
	// absent. Omitting it here would make the tools/call below refuse and this
	// test would prove nothing about what it is named for. It is the REAL
	// govcontext.Repo over the same pool, so the organisation-context read runs
	// through InTenantTx as wpmgr_app under the same RLS policies as every other
	// read in this file.
	svc := mcp.NewService(mcpRepo).
		WithContextResolver(&govcontext.Resolver{Store: govcontext.NewRepo(pool)})

	tenant := seedTenant(t, pool, "mcp-s7-caps-"+uuid.NewString()[:8])

	// Assert the role inside the transaction the resolution actually uses.
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (ResolveScopeSites path)")
		return nil
	}); err != nil {
		t.Fatalf("open tenant tx: %v", err)
	}

	s1, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenant, URL: "https://s7a.example.com", Name: "s7-alpha"})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}

	_, bearer := s7GrantWithBearer(t, mcpRepo, tenant, "list", []uuid.UUID{s1.ID})

	auth, err := svc.Authenticate(ctx, bearer)
	if err != nil {
		t.Fatalf("Authenticate with a live bearer: %v", err)
	}
	if auth.TenantID != tenant {
		t.Fatalf("Authenticate resolved tenant %s, want %s", auth.TenantID, tenant)
	}

	// (1) The capability set is populated, not the zero value.
	if auth.Capabilities.IsEmpty() {
		t.Fatalf("Authenticate returned a ZERO-VALUE CapabilitySet; every tool would refuse " +
			"with a message that reads like a site-scope problem")
	}
	if !auth.Capabilities.Allows(mcp.CapSitesRead) {
		t.Fatalf("the resolved capability set does not hold %q: %v",
			mcp.CapSitesRead, auth.Capabilities.Sorted())
	}
	t.Logf("Authenticate resolved %d capabilities and %d sites as wpmgr_app",
		auth.Capabilities.Len(), auth.Sites.Len())

	// (2) The site axis was resolved by the database, and resolved to the one
	// site this grant names. A zero here is the swapped-argument defect.
	if auth.Sites.Len() != 1 || !auth.Sites.Allows(s1.ID) {
		t.Fatalf("site scope resolved to %d sites and Allows(%s)=%v; want exactly the one granted site",
			auth.Sites.Len(), s1.ID, auth.Sites.Allows(s1.ID))
	}

	// (3) The registry admits the tool for this real connection, and tools/list
	// shows it.
	visible := mcp.VisibleTools(auth)
	if !listsWholeRegistry(visible, auth) {
		t.Fatalf("tools/list for a fully-scoped connection returned %+v", visible)
	}

	// (4) And the tool actually runs end to end for it, returning this org's
	// site. This is the positive control: without it, every refusal proof below
	// could be passing because nothing works at all.
	text, err := svc.ListSitesForModel(ctx, auth)
	if err != nil {
		t.Fatalf("ListSitesForModel for a fully-scoped connection: %v", err)
	}
	if len(text) == 0 {
		t.Fatal("list_sites returned empty text")
	}
	t.Logf("list_sites returned %d bytes for the granted connection", len(text))
}

// TestS7GuessedToolNameIsRefusedForARealConnectionAsAppRole is the exit gate
// executed against the real database rather than against a literal.
//
// The connection is REAL and FULLY CAPABLE. It is not a contrived
// no-capability request: it holds mcp:read, it is scoped to a site, and
// list_sites works for it (proved above). What it must not be able to do is
// reach a name the registry does not carry -- and it must not be able to tell
// that name apart from one the registry DOES carry.
func TestS7GuessedToolNameIsRefusedForARealConnectionAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	mcpRepo := mcp.NewRepo(pool)
	siteRepo := site.NewRepo(pool)
	svc := mcp.NewService(mcpRepo)

	tenant := seedTenant(t, pool, "mcp-s7-guess-"+uuid.NewString()[:8])
	s1, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenant, URL: "https://s7g.example.com", Name: "s7-guess"})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}

	_, bearer := s7GrantWithBearer(t, mcpRepo, tenant, "list", []uuid.UUID{s1.ID})

	auth, err := svc.Authenticate(ctx, bearer)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	// The registered tool IS reachable for this connection -- so a refusal
	// below is the gate, not a broken fixture.
	if _, _, err := mcp.AuthorizeTool(mcp.ToolFleetSitesList, auth); err != nil {
		t.Fatalf("the registered tool was refused for a fully-scoped connection: %v", err)
	}

	// THE NEAR MISSES MUST BE MISSES OF THE LIVE NAME.
	//
	// This list previously spelled variants of list_sites, the name this tool
	// had before the rename to fleet_sites_list. Those strings stayed valid
	// negatives -- they are unregistered, so refusing them is correct -- but
	// they had quietly stopped testing what the test's name claims: that a
	// near miss of a REAL name is not normalised into it. Every variant below
	// is a near miss of the name the registry actually carries today.
	//
	// The old name is kept in the list on purpose, as its own case: after a
	// rename, the retired name is the single most likely string a client or a
	// model will send, and it must refuse rather than resolve.
	for _, guess := range []string{
		"restart_site", "update_plugin", "run_backup", "delete_site",
		// Near misses of the LIVE name.
		"fleet_sites_list_all", "fleetSitesList", "FLEET_SITES_LIST",
		"fleet_sites_list ", " fleet_sites_list", "fleet-sites-list",
		"fleet_site_list", "fleet_sites_lists", "sites_list",
		// The RETIRED name, which must not be aliased back into service.
		"list_sites", "list_sites_all", "listSites", "LIST_SITES",
	} {
		if _, _, err := mcp.AuthorizeTool(guess, auth); err == nil {
			t.Fatalf("GUESSED NAME %q WAS GRANTED to a real connection", guess)
		}
	}
	t.Logf("all guessed names refused for a real, fully-capable connection")
}

// TestS7UntickedCapabilityRefusesByNameAsAppRole is the D1 ruling, executed
// against the real schema as wpmgr_app: an unticked capability REFUSES, it does
// not hide.
//
// HOW THE CAPABILITY-LESS CONNECTION IS BUILT, AND WHY IT IS NOT A LITERAL.
// Everything about this connection comes from the database: the tenant, the
// site, the grant, the connection token, and the SiteSet, all resolved by
// mcp.Service.Authenticate inside InTenantTx under the sites RLS policy as
// wpmgr_app. Exactly ONE field is then overwritten -- Capabilities -- and it
// has to be, because the capability axis is not stored per-grant yet: m124
// DECISION 1 declines to mint a capabilities column, so grantScopes() is a
// constant and every bearer that can authenticate resolves to mcp.sites.read.
// There is no bearer in this schema that lacks it.
//
// What that costs is stated plainly: this proves the GATE, not the resolution.
// TestS7CapabilitiesAreResolvedByAuthenticateAsAppRole above proves the
// resolution half -- that Authenticate really populates the set rather than
// handing back the zero value -- and the two together cover the path. The
// moment a capabilities column exists, this test replaces the one assignment
// with a narrower grant and proves both halves at once.
func TestS7UntickedCapabilityRefusesByNameAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	mcpRepo := mcp.NewRepo(pool)
	siteRepo := site.NewRepo(pool)
	// THE CONTEXT RESOLVER IS WIRED BECAUSE PRODUCTION WIRES IT: both non-test
	// call sites of mcp.NewService (cmd/wpmgr/main.go, cmd/dump-routes/routes.go)
	// chain WithContextResolver, and a Service without one refuses every fleet
	// tool call rather than serving with the operator's governance silently
	// absent. Omitting it here would make the tools/call below refuse and this
	// test would prove nothing about what it is named for. It is the REAL
	// govcontext.Repo over the same pool, so the organisation-context read runs
	// through InTenantTx as wpmgr_app under the same RLS policies as every other
	// read in this file.
	svc := mcp.NewService(mcpRepo).
		WithContextResolver(&govcontext.Resolver{Store: govcontext.NewRepo(pool)})

	tenant := seedTenant(t, pool, "mcp-s7-cap-"+uuid.NewString()[:8])

	// The role is asserted and printed INSIDE the transaction the resolution
	// uses. Either privilege would make the site half of this pass vacuously.
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (capability-refusal proof)")
		return nil
	}); err != nil {
		t.Fatalf("open tenant tx: %v", err)
	}

	s1, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenant, URL: "https://s7cap.example.com", Name: "s7-cap"})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}

	_, bearer := s7GrantWithBearer(t, mcpRepo, tenant, "list", []uuid.UUID{s1.ID})

	auth, err := svc.Authenticate(ctx, bearer)
	if err != nil {
		t.Fatalf("Authenticate with a live bearer: %v", err)
	}

	// POSITIVE CONTROL FIRST. The real connection reaches the tool, so every
	// refusal below is the capability gate rather than a broken fixture.
	if _, _, err := mcp.AuthorizeTool(mcp.ToolFleetSitesList, auth); err != nil {
		t.Fatalf("the real, fully-granted connection was refused: %v", err)
	}
	if _, err := svc.ListSitesForModel(ctx, auth); err != nil {
		t.Fatalf("list_sites failed for the real connection: %v", err)
	}

	// Now the same connection with the capability withheld. Note what is NOT
	// touched: the site scope stays as the database resolved it, so nothing
	// below can be attributed to the site axis.
	denied := auth
	denied.Capabilities = mcp.NewCapabilitySet(nil)
	if denied.Sites.IsEmpty() || !denied.Sites.Allows(s1.ID) {
		t.Fatalf("the DB-resolved site scope did not survive: %d sites, Allows(%s)=%v",
			denied.Sites.Len(), s1.ID, denied.Sites.Allows(s1.ID))
	}
	t.Logf("capability-less connection carries the DB-resolved scope: %d site(s), tenant %s",
		denied.Sites.Len(), denied.TenantID)

	// (1) IT DOES NOT HIDE. The tool is still listed, and the descriptor says
	// why it cannot be called.
	visible := mcp.VisibleTools(denied)
	// COMPARED AGAINST denied's OWN CEILING, not auth's. They are different
	// connections; measuring one request's listing against another's ceiling
	// would pass or fail for reasons unrelated to the capability axis under
	// test here.
	if !listsWholeRegistry(visible, denied) {
		t.Fatalf("an unticked capability HID the tool from tools/list: %+v", visible)
	}
	// FOUND BY NAME, not by index. visible[0] happened to be the right tool
	// while the registry held one entry, and an index into a growing list is a
	// proof that quietly starts checking a different subject.
	sitesDesc := descriptionOf(t, visible, mcp.ToolFleetSitesList)
	if !strings.Contains(sitesDesc, string(mcp.CapSitesRead)) {
		t.Fatalf("the listed descriptor does not name the missing capability:\n%s",
			sitesDesc)
	}

	// (2) IT REFUSES, BY NAME. Asserted by VALUE on the code and the kind: an
	// "err != nil" here would pass under the site-scope refusal, under the
	// uniform not-available refusal, and under a 401 -- three different bugs,
	// two of which this change exists to rule out.
	_, _, err = mcp.AuthorizeTool(mcp.ToolFleetSitesList, denied)
	if err == nil {
		t.Fatal("a tool was authorized for a connection that does not hold its capability")
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("err = %v, want a domain error", err)
	}
	if de.Code != mcp.ErrCodeCapabilityNotGranted {
		t.Fatalf("code = %q, want %q", de.Code, mcp.ErrCodeCapabilityNotGranted)
	}
	if de.Code == mcp.ErrCodeScopeEmpty {
		t.Fatal("the capability refusal answered on the SITE axis")
	}
	if de.Kind != domain.KindForbidden {
		t.Fatalf("kind = %v, want KindForbidden; a 401 makes clients re-run an OAuth "+
			"handshake that cannot change the answer", de.Kind)
	}
	if !strings.Contains(de.Message, string(mcp.CapSitesRead)) {
		t.Fatalf("the refusal does not name the capability: %s", de.Message)
	}
	if !strings.Contains(de.Message, "permanent") {
		t.Fatalf("the refusal does not tell the model it is permanent: %s", de.Message)
	}
	if got := de.Details["retryable"]; got != false {
		t.Fatalf("details[retryable] = %v, want false", got)
	}
	if got := de.Details["required_capability"]; got != string(mcp.CapSitesRead) {
		t.Fatalf("details[required_capability] = %v, want %q", got, mcp.CapSitesRead)
	}

	// (3) IT DOES NOT OVER-FIRE. The untouched connection still works, after
	// the refusal, against the same live database.
	if _, _, err := mcp.AuthorizeTool(mcp.ToolFleetSitesList, auth); err != nil {
		t.Fatalf("the granted connection was refused after the denied one: %v", err)
	}
	t.Logf("capability refusal: code=%s retryable=%v; granted connection unaffected",
		de.Code, de.Details["retryable"])
}

// TestS7EmptySiteScopeRefusesByNameAsAppRole is the site axis, executed.
//
// The grant names a site belonging to ANOTHER organisation. The column accepts
// it -- scope_site_ids is a uuid[] and PostgreSQL has no foreign key over array
// elements -- so the only thing that drops it is resolving inside InTenantTx
// under the sites policy. The connection therefore authenticates with a
// capability it holds and a site scope that resolves to nothing, and the
// registry must refuse the site-keyed tool BY NAME rather than uniformly, and
// must still list it.
func TestS7EmptySiteScopeRefusesByNameAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	mcpRepo := mcp.NewRepo(pool)
	siteRepo := site.NewRepo(pool)
	svc := mcp.NewService(mcpRepo)

	tenantA := seedTenant(t, pool, "mcp-s7-scope-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "mcp-s7-scope-b-"+uuid.NewString()[:8])

	siteB, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenantB, URL: "https://s7b.example.com", Name: "s7-foreign"})
	if err != nil {
		t.Fatalf("create foreign site: %v", err)
	}

	// Org A's grant names ONLY org B's site.
	_, bearer := s7GrantWithBearer(t, mcpRepo, tenantA, "list", []uuid.UUID{siteB.ID})

	auth, err := svc.Authenticate(ctx, bearer)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	// The foreign id was dropped by RLS, so the scope is empty.
	if !auth.Sites.IsEmpty() {
		t.Fatalf("CROSS-TENANT LEAK: org A's grant resolved to %d sites naming org B's site %s",
			auth.Sites.Len(), siteB.ID)
	}
	// But the capability is held: this is not a capability failure.
	if !auth.Capabilities.Allows(mcp.CapSitesRead) {
		t.Fatalf("the capability was lost along with the site scope: %v", auth.Capabilities.Sorted())
	}
	t.Logf("org A's grant naming org B's site resolved to %d sites, capability still held",
		auth.Sites.Len())

	// The tool is STILL LISTED -- the site axis does not hide it, because this
	// connection's org and capability set already entitle it to know the tool
	// exists. Hiding it here would send the operator hunting a capability
	// problem they do not have.
	visible := mcp.VisibleTools(auth)
	if !listsWholeRegistry(visible, auth) {
		t.Fatalf("an empty site scope hid the tool from tools/list: %+v", visible)
	}

	// And calling it refuses BY NAME, with the code that says "site scope",
	// not the uniform not-available one.
	_, _, err = mcp.AuthorizeTool(mcp.ToolFleetSitesList, auth)
	if err == nil {
		t.Fatal("a site-keyed tool was authorized for a connection whose site scope is empty")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != mcp.ErrCodeScopeEmpty {
		t.Fatalf("err = %v, want the named %s refusal", err, mcp.ErrCodeScopeEmpty)
	}

	// The service refuses too, so the gate is not the only thing standing
	// between an empty scope and a result that reads as a healthy empty org.
	if _, err := svc.ListSitesForModel(ctx, auth); err == nil {
		t.Fatal("ListSitesForModel returned a result for an empty site scope")
	}
}

// TestS7CapabilityOutsideTheOrgCeilingIsNotListedAsAppRole is the THIRD case of
// the ruling, and the only one that hides.
//
// The other two are above: a held capability is listed and callable, and a
// capability the ORG allows but this GRANT lacks is still listed and refuses by
// name. This one is the organisation boundary: a capability the org's ceiling
// does not contain is absent from tools/list entirely, and calling it answers
// exactly as an unregistered name does -- because a distinguishable refusal
// would hand back the enumeration the listing withheld.
//
// HOW THE NARROW CEILING IS CONSTRUCTED, AND WHETHER IT IS REACHABLE.
//
// It is set directly on the AuthorizedRequest that the real Authenticate
// returned, and the value is the EMPTY set. That is not a shortcut, it is the
// only narrower ceiling that exists today: capabilityVocabulary holds exactly
// one capability, so the sole proper subset of it is the empty set.
//
// THIS STATE IS NOT REACHABLE THROUGH Authenticate TODAY, and saying so is part
// of the proof rather than an apology for it. OrgDefaultCapabilities derives
// the ceiling from the closed scope registry and REFUSES to return an empty
// set -- all three of its error paths raise mcp_capability_unmapped rather than
// yield one. So no live connection can currently carry a ceiling narrower than
// the vocabulary, every tool is inside every real ceiling, and the hiding arm
// cannot fire in production. It becomes reachable when the vocabulary grows a
// second capability and org policy can select within it; the structure is
// proven now so that it is already correct then.
//
// The connection is otherwise REAL: a live bearer, resolved by the real
// Authenticate against the real schema as wpmgr_app, carrying the site scope
// the database resolved. Only the ceiling is substituted.
func TestS7CapabilityOutsideTheOrgCeilingIsNotListedAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	mcpRepo := mcp.NewRepo(pool)
	siteRepo := site.NewRepo(pool)
	// THE CONTEXT RESOLVER IS WIRED BECAUSE PRODUCTION WIRES IT: both non-test
	// call sites of mcp.NewService (cmd/wpmgr/main.go, cmd/dump-routes/routes.go)
	// chain WithContextResolver, and a Service without one refuses every fleet
	// tool call rather than serving with the operator's governance silently
	// absent. Omitting it here would make the tools/call below refuse and this
	// test would prove nothing about what it is named for. It is the REAL
	// govcontext.Repo over the same pool, so the organisation-context read runs
	// through InTenantTx as wpmgr_app under the same RLS policies as every other
	// read in this file.
	svc := mcp.NewService(mcpRepo).
		WithContextResolver(&govcontext.Resolver{Store: govcontext.NewRepo(pool)})

	tenant := seedTenant(t, pool, "mcp-s7-ceil-"+uuid.NewString()[:8])

	// The role is asserted and printed INSIDE the transaction the resolution
	// uses. Either privilege would make the site half of this pass vacuously.
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (org-ceiling proof)")
		return nil
	}); err != nil {
		t.Fatalf("open tenant tx: %v", err)
	}

	s1, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenant, URL: "https://s7ceil.example.com", Name: "s7-ceil"})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}

	_, bearer := s7GrantWithBearer(t, mcpRepo, tenant, "list", []uuid.UUID{s1.ID})

	auth, err := svc.Authenticate(ctx, bearer)
	if err != nil {
		t.Fatalf("Authenticate with a live bearer: %v", err)
	}

	// POSITIVE CONTROL. The real connection -- real ceiling, real grant -- sees
	// the tool and can call it, so an absence below is the ceiling and not a
	// broken fixture.
	if got := mcp.VisibleTools(auth); !listsWholeRegistry(got, auth) {
		t.Fatalf("the real connection did not see the tool: %+v", got)
	}
	if _, _, err := mcp.AuthorizeTool(mcp.ToolFleetSitesList, auth); err != nil {
		t.Fatalf("the real, fully-granted connection was refused: %v", err)
	}

	// THE CEILING RESOLVED BY Authenticate CONTAINS THE CAPABILITY. Asserted
	// rather than assumed: if it did not, the omission below would prove
	// nothing, because the tool would be missing for the reason the test is
	// trying to create rather than the one it is trying to observe.
	if !auth.OrgCeiling.Allows(mcp.CapSitesRead) {
		t.Fatalf("Authenticate resolved a ceiling WITHOUT %s, so this test cannot "+
			"distinguish the ceiling arm from a broken fixture: %v",
			mcp.CapSitesRead, auth.OrgCeiling.Sorted())
	}

	// Now the same connection with a ceiling that excludes the capability.
	// Capabilities and Sites are untouched -- the grant still HOLDS the
	// capability -- so nothing below can be attributed to the grant axis or the
	// site axis. This is the org axis alone.
	outside := auth
	outside.OrgCeiling = mcp.NewCapabilitySet(nil)
	if !outside.Capabilities.Allows(mcp.CapSitesRead) {
		t.Fatalf("the grant lost the capability, so this would prove the GRANT arm, "+
			"not the ceiling arm: %v", outside.Capabilities.Sorted())
	}
	if outside.Sites.IsEmpty() || !outside.Sites.Allows(s1.ID) {
		t.Fatalf("the DB-resolved site scope did not survive: %d sites, Allows(%s)=%v",
			outside.Sites.Len(), s1.ID, outside.Sites.Allows(s1.ID))
	}

	// (1) IT IS NOT LISTED. By value: the list is empty, and specifically does
	// not contain the tool under any description.
	visible := mcp.VisibleTools(outside)
	if len(visible) != 0 {
		t.Fatalf("a capability outside the org ceiling was LISTED: %+v", visible)
	}
	for _, d := range visible {
		if d.Name == mcp.ToolFleetSitesList {
			t.Fatalf("the tool outside the ceiling appeared in tools/list: %+v", d)
		}
	}

	// (2) INVOKING IT ANSWERS AS AN UNREGISTERED NAME. Asserted by VALUE on the
	// code, and explicitly against the code it must NOT be: answering
	// mcp_capability_not_granted here would name a capability the organisation
	// switched off, which is the enumeration the omission above exists to
	// prevent.
	_, _, err = mcp.AuthorizeTool(mcp.ToolFleetSitesList, outside)
	if err == nil {
		t.Fatal("a tool outside the org ceiling was AUTHORIZED")
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("err = %v, want a domain error", err)
	}
	if de.Code != mcp.ErrCodeToolNotAvailable {
		t.Fatalf("code = %q, want %q", de.Code, mcp.ErrCodeToolNotAvailable)
	}
	if de.Code == mcp.ErrCodeCapabilityNotGranted {
		t.Fatal("the ceiling refusal disclosed the capability the organisation disabled")
	}
	if de.Kind != domain.KindForbidden {
		t.Fatalf("kind = %v, want KindForbidden", de.Kind)
	}
	if strings.Contains(de.Message, string(mcp.CapSitesRead)) {
		t.Fatalf("the ceiling refusal NAMES the disabled capability: %s", de.Message)
	}

	// (3) IT IS INDISTINGUISHABLE FROM A NAME THAT WAS NEVER REGISTERED. Same
	// code, same message. If these ever diverge, the refusal becomes an oracle
	// for what the organisation switched off.
	//
	// The comparison is on the TEMPLATE, not the literal string: both messages
	// echo the name the caller asked for, and that difference is the caller's
	// own input rather than a disclosure. Substituting it back is what isolates
	// the property -- everything OTHER than the echoed name must match.
	const guessed = "sites_restart_everything"
	_, _, guessErr := mcp.AuthorizeTool(guessed, outside)
	gde, ok := domain.AsDomain(guessErr)
	if !ok {
		t.Fatalf("guessed name err = %v, want a domain error", guessErr)
	}
	if gde.Code != de.Code {
		t.Fatalf("a disabled capability answers %q and a guessed name answers %q; "+
			"the difference tells a caller which capabilities exist", de.Code, gde.Code)
	}
	if want := strings.ReplaceAll(gde.Message, guessed, mcp.ToolFleetSitesList); de.Message != want {
		t.Fatalf("the two refusals differ beyond the echoed name, which is the same oracle:\n"+
			"disabled: %s\nexpected: %s", de.Message, want)
	}

	// (4) IT DOES NOT OVER-FIRE. The untouched connection still lists and still
	// calls the tool, after the refusals, against the same live database.
	if got := mcp.VisibleTools(auth); !listsWholeRegistry(got, auth) {
		t.Fatalf("the real connection lost the tool after the ceiling refusal: %+v", got)
	}
	if _, _, err := mcp.AuthorizeTool(mcp.ToolFleetSitesList, auth); err != nil {
		t.Fatalf("the granted connection was refused after the ceiling one: %v", err)
	}
	if _, err := svc.ListSitesForModel(ctx, auth); err != nil {
		t.Fatalf("list_sites failed for the real connection: %v", err)
	}

	t.Logf("org-ceiling omission: listed=%d refusal=%s (identical to an unregistered "+
		"name); the real connection is unaffected", len(visible), de.Code)
}

// listsWholeRegistry reports whether a tools/list answer holds every tool the
// server has, order-insensitively.
//
// IT REPLACES `len(visible) != 1 && visible[0].Name != ToolFleetSitesList` IN
// FIVE PROOFS ABOVE, and the replacement is a correction rather than a tidy-up.
// Each of those proofs is about a BOUNDARY -- the capability axis, the site
// axis, the org ceiling -- and none of them is about how many tools had been
// built on the day it was written. All five went red on a correct second read
// tool that crossed no boundary at all. A proof that reddens on correct work is
// one someone eventually deletes, and then the boundary is unguarded.
//
// It compares against mcp.Tools(), the server's whole surface, so it still
// fails if a tool is genuinely hidden -- which is the thing each proof is
// actually asserting.
func listsWholeRegistry(got []mcp.ToolDescriptor, auth mcp.AuthorizedRequest) bool {
	// FILTERED BY THE ORG CEILING. VisibleTools HIDES a tool outside the
	// ceiling and only ANNOTATES one the grant merely lacks, so the set it
	// returns is the ceiling-admitted subset -- not the whole registry.
	// Comparing against the whole registry is correct only while every tool
	// requires the same capability, and stops being correct the moment one does
	// not: a propose tool outside the read ceiling would turn all five proofs
	// below red on a listing that was right. That is precisely the over-firing
	// this helper replaced a hard-coded len() == 1 to avoid, so it has to know
	// about the ceiling too.
	//
	// The predicate is re-derived here rather than borrowed, which does mean a
	// bug in withinOrgCeiling would be invisible to these five. That is
	// deliberate: they are positive controls for OTHER axes, and the ceiling
	// itself has its own dedicated proof
	// (TestS7CapabilityOutsideTheOrgCeilingIsNotListedAsAppRole) which asserts
	// the omission directly.
	want := []string{}
	for _, p := range mcp.RegistryPolicies() {
		if auth.OrgCeiling.Allows(p.Capability) {
			want = append(want, p.Name)
		}
	}
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, d := range got {
		seen[d.Name]++
	}
	for _, n := range want {
		seen[n]--
		if seen[n] < 0 {
			return false
		}
	}
	return true
}

// descriptionOf finds one tool's description by NAME, failing the test when it
// is absent rather than returning "" and letting a Contains check pass or fail
// for the wrong reason.
func descriptionOf(t *testing.T, got []mcp.ToolDescriptor, name string) string {
	t.Helper()
	for _, d := range got {
		if d.Name == name {
			return d.Description
		}
	}
	t.Fatalf("tool %q is absent from the listing %+v", name, got)
	return ""
}
