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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
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

	grant, code, err := repo.CreateGrantWithCode(ctx, sqlc.CreateMCPGrantParams{
		TenantID:      tenantID,
		Name:          "s7 grant",
		Status:        "active",
		SiteScopeMode: mode,
		ScopeTagIds:   []uuid.UUID{},
		ScopeSiteIds:  siteIDs,
		ClientID:      &clientID,
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
	})
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
	svc := mcp.NewService(mcpRepo)

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
	if len(visible) != 1 || visible[0].Name != mcp.ToolListSites {
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
	if _, _, err := mcp.AuthorizeTool(mcp.ToolListSites, auth); err != nil {
		t.Fatalf("the registered tool was refused for a fully-scoped connection: %v", err)
	}

	for _, guess := range []string{
		"restart_site", "update_plugin", "run_backup", "delete_site",
		"list_sites_all", "listSites", "LIST_SITES", "list_sites ",
	} {
		if _, _, err := mcp.AuthorizeTool(guess, auth); err == nil {
			t.Fatalf("GUESSED NAME %q WAS GRANTED to a real connection", guess)
		}
	}
	t.Logf("all guessed names refused for a real, fully-capable connection")
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
	if len(visible) != 1 || visible[0].Name != mcp.ToolListSites {
		t.Fatalf("an empty site scope hid the tool from tools/list: %+v", visible)
	}

	// And calling it refuses BY NAME, with the code that says "site scope",
	// not the uniform not-available one.
	_, _, err = mcp.AuthorizeTool(mcp.ToolListSites, auth)
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
