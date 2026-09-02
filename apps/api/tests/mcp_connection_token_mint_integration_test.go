package tests

// Integration proofs for the connection-token mint: the DOCUMENTED HEADLESS
// PATH (POST /api/v1/mcp/connections).
//
// EVERYTHING HERE RUNS AS wpmgr_app, THROUGH THE POOL, THROUGH THE MOUNTED
// ROUTE. That is the whole reason this file exists alongside the package's unit
// proofs: a fake store evaluates no policy and a test that opens its own
// connection leaves every RLS policy inert while staying green, which is
// exactly how m112's proofs passed over a cross-site-readable domain. The
// credential minted here is authenticated back through mcp.Service.Authenticate
// -- the same call the live transport makes on every request.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/govcontext"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/internal/sitetag"
)

// mintPath is the route under test, built from the package's own constants so
// this file cannot drift from the mount.
const mintPath = mcp.APIV1Prefix + "/mcp/connections"

// mintResponse is the 201 body. `token` is decoded into a plain string rather
// than skipped, because the point of every test below is what that string can
// then do.
type mintResponse struct {
	GrantID       string   `json:"grant_id"`
	Token         string   `json:"token"`
	TokenPrefix   string   `json:"token_prefix"`
	ExpiresAt     string   `json:"expires_at"`
	SiteScopeMode string   `json:"site_scope_mode"`
	Capabilities  []string `json:"capabilities"`
}

// TestMCPMintedTokenAuthenticatesAndIsTenantBoundAsAppRole is the end-to-end
// proof: mint through the mounted route, authenticate the returned plaintext
// through the production dispatch, and prove the credential reaches its own
// organisation and nothing else.
func TestMCPMintedTokenAuthenticatesAndIsTenantBoundAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	// THE ROLE IS LOAD-BEARING FOR EVERY ASSERTION BELOW. Printed, not
	// assumed: a proof running as superuser or BYPASSRLS would pass with every
	// policy switched off.
	if err := pool.InMCPTokenLookupTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InMCPTokenLookupTx")
		return nil
	}); err != nil {
		t.Fatalf("open token lookup tx: %v", err)
	}

	suffix := uuid.NewString()[:8]

	// TWO ORGANISATIONS. The second is not decoration: a token that
	// authenticates is only interesting alongside proof that it authenticates
	// NOTHING next door.
	tenantA := seedTenant(t, pool, "mcp-mint-a-"+suffix)
	tenantB := seedTenant(t, pool, "mcp-mint-b-"+suffix)
	userA := seedUserRow(t, pool, "mint-a-"+suffix+"@example.test")
	userB := seedUserRow(t, pool, "mint-b-"+suffix+"@example.test")

	siteAURL := "https://a-" + suffix + ".example.test"
	siteBURL := "https://b-" + suffix + ".example.test"
	seedSite(t, pool, tenantA, siteAURL)
	seedSite(t, pool, tenantB, siteBURL)

	rec := audit.NewRecorder(pool, domain.SystemClock{})
	// THE CONTEXT RESOLVER IS WIRED BECAUSE PRODUCTION WIRES IT: both non-test
	// call sites of mcp.NewService (cmd/wpmgr/main.go, cmd/dump-routes/routes.go)
	// chain WithContextResolver, and a Service without one refuses every fleet
	// tool call rather than serving with the operator's governance silently
	// absent. Omitting it here would make the tools/call below refuse and this
	// test would prove nothing about what it is named for. It is the REAL
	// govcontext.Repo over the same pool, so the organisation-context read runs
	// through InTenantTx as wpmgr_app under the same RLS policies as every other
	// read in this file.
	svc := auditedMCPService(pool, mcp.NewRepo(pool)).WithAudit(rec).
		WithContextResolver(&govcontext.Resolver{Store: govcontext.NewRepo(pool)})

	adminA := domain.Principal{
		Type: domain.PrincipalUser, UserID: userA, TenantID: tenantA,
		Role: "admin", Scope: domain.ScopeOrg,
	}
	adminB := domain.Principal{
		Type: domain.PrincipalUser, UserID: userB, TenantID: tenantB,
		Role: "admin", Scope: domain.ScopeOrg,
	}
	engA := mountConnectionsLikeProduction(t, svc, adminA)
	engB := mountConnectionsLikeProduction(t, svc, adminB)

	// ------------------------------------------------------------------
	// MINT, through the mounted route with its real RequirePermission.
	// ------------------------------------------------------------------
	var mintedA mintResponse
	if code := mcpDoJSON(t, engA, http.MethodPost, mintPath, map[string]any{
		"name":            "headless ci runner",
		"site_scope_mode": string(mcp.SiteScopeModeAll),
	}, nil, &mintedA); code != http.StatusCreated {
		t.Fatalf("mint answered %d, want 201", code)
	}
	if mintedA.Token == "" {
		t.Fatal("the mint returned no plaintext: there is no credential to use")
	}
	if mintedA.TokenPrefix == "" || !strings.HasPrefix(mintedA.Token, mintedA.TokenPrefix) {
		t.Fatalf("token_prefix %q is not a prefix of the plaintext", mintedA.TokenPrefix)
	}
	if len(mintedA.Capabilities) == 0 {
		t.Fatal("the grant was minted with NO capabilities: it would authenticate and reach no tool")
	}

	// ------------------------------------------------------------------
	// THE CREDENTIAL WORKS, through the same call the transport makes.
	// ------------------------------------------------------------------
	authA, err := svc.Authenticate(ctx, mintedA.Token)
	if err != nil {
		t.Fatalf("the minted token does not authenticate: %v", err)
	}
	if authA.TenantID != tenantA {
		t.Fatalf("token resolved to tenant %s, want %s", authA.TenantID, tenantA)
	}
	if authA.GrantID.String() != mintedA.GrantID {
		t.Fatalf("token resolved to grant %s, want %s", authA.GrantID, mintedA.GrantID)
	}
	if authA.Sites.IsEmpty() {
		t.Fatal("a site_scope_mode='all' grant resolved to no sites")
	}

	// ------------------------------------------------------------------
	// ONLY THE HASH IS STORED. Read back as wpmgr_app inside InTenantTx --
	// the same transaction shape the request path uses.
	// ------------------------------------------------------------------
	assertNoPlaintextStored(t, pool, tenantA, authA.GrantID, mintedA.Token, mintedA.TokenPrefix)

	// ------------------------------------------------------------------
	// CROSS-TENANT. Tenant B mints its own token; neither reaches the other.
	// ------------------------------------------------------------------
	var mintedB mintResponse
	if code := mcpDoJSON(t, engB, http.MethodPost, mintPath, map[string]any{
		"name":            "tenant b runner",
		"site_scope_mode": string(mcp.SiteScopeModeAll),
	}, nil, &mintedB); code != http.StatusCreated {
		t.Fatalf("tenant B mint answered %d, want 201", code)
	}
	authB, err := svc.Authenticate(ctx, mintedB.Token)
	if err != nil {
		t.Fatalf("tenant B's token does not authenticate: %v", err)
	}
	if authB.TenantID != tenantB {
		t.Fatalf("tenant B's token resolved to tenant %s, want %s", authB.TenantID, tenantB)
	}

	// The load-bearing half: A's credential must reach A's fleet and NOT B's.
	// Asserted on the rendered tool output, which is what a model would
	// actually receive.
	readA, err := svc.ListSitesForModel(ctx, authA)
	if err != nil {
		t.Fatalf("list sites under tenant A's token: %v", err)
	}
	if !strings.Contains(readA, siteAURL) {
		t.Fatalf("tenant A's token cannot see tenant A's own site %s:\n%s", siteAURL, readA)
	}
	if strings.Contains(readA, siteBURL) {
		t.Fatalf("CROSS-TENANT READ: tenant A's token saw tenant B's site %s:\n%s", siteBURL, readA)
	}

	readB, err := svc.ListSitesForModel(ctx, authB)
	if err != nil {
		t.Fatalf("list sites under tenant B's token: %v", err)
	}
	if strings.Contains(readB, siteAURL) {
		t.Fatalf("CROSS-TENANT READ: tenant B's token saw tenant A's site %s:\n%s", siteAURL, readB)
	}

	// Tenant B's grant is invisible to tenant A's connections list, and vice
	// versa. This reads through the operator surface rather than the token
	// surface, so both RLS paths are exercised.
	assertGrantNotVisible(t, engA, mintedB.GrantID, "tenant A")
	assertGrantNotVisible(t, engB, mintedA.GrantID, "tenant B")

	// ------------------------------------------------------------------
	// THE AUDIT ROW EXISTS, in tenant A and only in tenant A.
	// ------------------------------------------------------------------
	rows := queryMCPAuditRowsAsAppRole(t, pool, tenantA, audit.ActionMCPGrantCreated)
	var found bool
	for _, r := range rows {
		if r.targetID != mintedA.GrantID {
			continue
		}
		found = true
		if r.actorType != audit.ActorUser {
			t.Errorf("mcp.grant.created actor_type = %q, want %q", r.actorType, audit.ActorUser)
		}
		if r.actorID != userA.String() {
			t.Errorf("mcp.grant.created actor_id = %q, want the minting operator %s", r.actorID, userA)
		}
		// The issuance discriminator is what separates a headless mint from a
		// browser-consented grant in the log. Without it an auditor cannot
		// tell which authorisation produced the credential.
		if got, _ := r.metadata["issuance"].(string); got != "connection_token" {
			t.Errorf("mcp.grant.created metadata.issuance = %q, want %q", got, "connection_token")
		}
		// The PUBLIC handle travels; the plaintext must not.
		if got, _ := r.metadata["token_prefix"].(string); got != mintedA.TokenPrefix {
			t.Errorf("metadata.token_prefix = %q, want %q", got, mintedA.TokenPrefix)
		}
		meta, mErr := json.Marshal(r.metadata)
		if mErr != nil {
			t.Fatalf("re-marshal audit metadata: %v", mErr)
		}
		if strings.Contains(string(meta), mintedA.Token) {
			t.Fatalf("THE PLAINTEXT WAS WRITTEN TO THE AUDIT LOG: %s", meta)
		}
	}
	if !found {
		t.Fatalf("no %s audit row for grant %s: this credential is unexplained",
			audit.ActionMCPGrantCreated, mintedA.GrantID)
	}
	// Tenant B must not see tenant A's audit row.
	for _, r := range queryMCPAuditRowsAsAppRole(t, pool, tenantB, audit.ActionMCPGrantCreated) {
		if r.targetID == mintedA.GrantID {
			t.Fatal("CROSS-TENANT AUDIT LEAK: tenant B can read tenant A's mcp.grant.created row")
		}
	}
}

// TestMCPMintRefusesASiteScopedCollaboratorAsAppRole proves the org-scope guard
// holds through the mounted route, as wpmgr_app, AND that nothing was written.
//
// A site-scoped collaborator minting an organisation-wide grant was a live
// production defect on the consent path. This is the same class of write behind
// the same class of gate, so it gets the same proof.
func TestMCPMintRefusesASiteScopedCollaboratorAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	if err := pool.InMCPTokenLookupTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InMCPTokenLookupTx")
		return nil
	}); err != nil {
		t.Fatalf("open token lookup tx: %v", err)
	}

	suffix := uuid.NewString()[:8]
	tenantID := seedTenant(t, pool, "mcp-mint-scope-"+suffix)
	userID := seedUserRow(t, pool, "mint-scope-"+suffix+"@example.test")
	siteID := seedSite(t, pool, tenantID, "https://s-"+suffix+".example.test")

	svc := auditedMCPService(pool, mcp.NewRepo(pool))

	// An outside collaborator shared onto ONE site, holding admin ON THAT SITE.
	// Still refused: a grant is an organisation-wide credential.
	collaborator := domain.Principal{
		Type: domain.PrincipalUser, UserID: userID, TenantID: tenantID,
		Role: "admin", Scope: domain.ScopeSite, AllowedSiteIDs: []uuid.UUID{siteID},
	}
	eng := mountConnectionsLikeProduction(t, svc, collaborator)

	before := countAllGrants(t, pool, tenantID)

	var body map[string]any
	code := mcpDoJSON(t, eng, http.MethodPost, mintPath, map[string]any{
		"name":            "escalation attempt",
		"site_scope_mode": string(mcp.SiteScopeModeAll),
	}, nil, &body)
	if code == http.StatusCreated {
		t.Fatalf("A SITE-SCOPED COLLABORATOR MINTED AN ORG-WIDE CONNECTION TOKEN (201): %v", body)
	}
	if code != http.StatusForbidden {
		t.Fatalf("mint answered %d, want 403", code)
	}
	// A refusal that still wrote is a refusal of the RESPONSE only.
	if after := countAllGrants(t, pool, tenantID); after != before {
		t.Fatalf("the refused mint still wrote: grants went from %d to %d", before, after)
	}
}

// TestMCPMintScopeReferentsAsAppRole proves the two halves of the scope
// decision against a REAL tag registry:
//
//	a real tag carrying zero sites -> ACCEPTED and stored as given
//	a tag id naming no tag at all  -> REFUSED, by id
//
// Those are different questions and conflating them is the defect: the first is
// a legitimate connection that reads nothing today, and refusing it would
// contradict the owner's ruling; the second stores cleanly (scope_tag_ids is a
// uuid[] with no foreign key) and then resolves to nothing forever.
func TestMCPMintScopeReferentsAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	if err := pool.InMCPTokenLookupTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InMCPTokenLookupTx")
		return nil
	}); err != nil {
		t.Fatalf("open token lookup tx: %v", err)
	}

	suffix := uuid.NewString()[:8]
	tenantID := seedTenant(t, pool, "mcp-mint-scope-ref-"+suffix)
	userID := seedUserRow(t, pool, "mint-ref-"+suffix+"@example.test")
	seedSite(t, pool, tenantID, "https://ref-"+suffix+".example.test")

	// A REAL TAG CARRYING NO SITES. Created through the real service, so it is
	// a genuine registry row rather than a hand-inserted one.
	_, tagSvc := newSiteTagServices(pool)
	emptyTag, err := tagSvc.Create(ctx, sitetag.CreateInput{
		TenantID: tenantID, Name: "unused-" + suffix, Color: "#112233",
	})
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}

	svc := auditedMCPService(pool, mcp.NewRepo(pool))
	admin := domain.Principal{
		Type: domain.PrincipalUser, UserID: userID, TenantID: tenantID,
		Role: "admin", Scope: domain.ScopeOrg,
	}
	eng := mountConnectionsLikeProduction(t, svc, admin)

	// ---- ACCEPTED, AND STORED AS GIVEN, NEVER WIDENED --------------------
	var minted mintResponse
	if code := mcpDoJSON(t, eng, http.MethodPost, mintPath, map[string]any{
		"name":            "reads nothing yet",
		"site_scope_mode": string(mcp.SiteScopeModeTags),
		"scope_tag_ids":   []string{emptyTag.ID.String()},
	}, nil, &minted); code != http.StatusCreated {
		t.Fatalf("a real tag carrying zero sites was refused: mint answered %d, want 201", code)
	}

	grantID, err := uuid.Parse(minted.GrantID)
	if err != nil {
		t.Fatalf("parse grant id: %v", err)
	}
	mode, tagIDs := readStoredScope(t, pool, tenantID, grantID)
	if mode != string(mcp.SiteScopeModeTags) {
		t.Fatalf("THE SCOPE WAS WIDENED: stored site_scope_mode = %q, want %q",
			mode, mcp.SiteScopeModeTags)
	}
	if len(tagIDs) != 1 || tagIDs[0] != emptyTag.ID {
		t.Fatalf("the tag payload was not stored as given: %v", tagIDs)
	}

	// The connection authenticates, and its site scope resolves to NOTHING --
	// which the read tool then refuses BY NAME rather than reporting as an
	// empty fleet. That is the shape of a legitimately empty connection.
	auth, err := svc.Authenticate(ctx, minted.Token)
	if err != nil {
		t.Fatalf("the empty-scope token does not authenticate: %v", err)
	}
	if !auth.Sites.IsEmpty() {
		t.Fatalf("a tag carrying no sites resolved to %d sites", auth.Sites.Len())
	}
	if _, err := svc.ListSitesForModel(ctx, auth); err == nil {
		t.Fatal("an empty site scope returned an empty LIST instead of a named refusal")
	}

	// ---- REFUSED, BY ID --------------------------------------------------
	ghost := uuid.New()
	var body map[string]any
	code := mcpDoJSON(t, eng, http.MethodPost, mintPath, map[string]any{
		"name":            "typo scope",
		"site_scope_mode": string(mcp.SiteScopeModeTags),
		"scope_tag_ids":   []string{ghost.String()},
	}, nil, &body)
	if code == http.StatusCreated {
		t.Fatalf("a tag id naming NO TAG was accepted (201): the scope is silently narrowed to nothing: %v", body)
	}
	if code != http.StatusUnprocessableEntity && code != http.StatusBadRequest {
		t.Fatalf("unknown tag answered %d, want a 4xx validation refusal", code)
	}
	raw, mErr := json.Marshal(body)
	if mErr != nil {
		t.Fatalf("marshal refusal body: %v", mErr)
	}
	if !strings.Contains(string(raw), ghost.String()) {
		t.Fatalf("the refusal does not name the offending id %s, so it cannot be acted on: %s",
			ghost, raw)
	}
}

// ---------------------------------------------------------------------------
// Helpers. All read THROUGH THE POOL as wpmgr_app, inside the tenant
// transaction the request path uses -- never a raw connection, which would read
// past RLS and could report rows the application can never see.
// ---------------------------------------------------------------------------

// assertNoPlaintextStored proves the token row holds a hash and a prefix and
// not the credential, and that the hash satisfies the column's CHECK shape.
func assertNoPlaintextStored(t *testing.T, pool *db.Pool, tenantID, grantID uuid.UUID, plaintext, prefix string) {
	t.Helper()
	var storedHash, storedPrefix string
	err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT token_hash, token_prefix FROM mcp_connection_tokens
			  WHERE tenant_id = $1 AND grant_id = $2`,
			tenantID, grantID).Scan(&storedHash, &storedPrefix)
	})
	if err != nil {
		t.Fatalf("read back the token row: %v", err)
	}
	if storedHash == plaintext {
		t.Fatal("THE PLAINTEXT IS IN token_hash")
	}
	if storedPrefix != prefix {
		t.Fatalf("stored token_prefix %q disagrees with the returned one %q", storedPrefix, prefix)
	}
	if storedPrefix == plaintext {
		t.Fatal("token_prefix is the WHOLE credential, so the public handle is the secret")
	}
	// The column's CHECK is '^[0-9a-f]{64}$'. Asserted here too, because a
	// value that satisfies it is by construction not the base64url plaintext.
	if len(storedHash) != 64 {
		t.Fatalf("token_hash is %d chars, want 64", len(storedHash))
	}
	for _, r := range storedHash {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			t.Fatalf("token_hash %q is not lower-case hex", storedHash)
		}
	}
}

// readStoredScope returns the grant's stored site scope, so a test can prove
// the payload was stored AS GIVEN rather than widened.
func readStoredScope(t *testing.T, pool *db.Pool, tenantID, grantID uuid.UUID) (string, []uuid.UUID) {
	t.Helper()
	var mode string
	var tagIDs []uuid.UUID
	err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT site_scope_mode, scope_tag_ids FROM mcp_grants
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, grantID).Scan(&mode, &tagIDs)
	})
	if err != nil {
		t.Fatalf("read stored scope: %v", err)
	}
	return mode, tagIDs
}

// countGrants counts the tenant's grants as wpmgr_app, so "nothing was written"
// is a statement about what the application can see.
func countAllGrants(t *testing.T, pool *db.Pool, tenantID uuid.UUID) int64 {
	t.Helper()
	var n int64
	err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM mcp_grants WHERE tenant_id = $1`, tenantID).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count grants: %v", err)
	}
	return n
}

// assertGrantNotVisible proves one organisation's connections list does not
// carry another's grant id.
func assertGrantNotVisible(t *testing.T, eng *gin.Engine, grantID, who string) {
	t.Helper()
	var list struct {
		Connections []struct {
			ID string `json:"id"`
		} `json:"connections"`
	}
	if code := mcpDoJSON(t, eng, http.MethodGet, mintPath, nil, nil, &list); code != http.StatusOK {
		t.Fatalf("%s list answered %d, want 200", who, code)
	}
	for _, c := range list.Connections {
		if c.ID == grantID {
			t.Fatalf("CROSS-TENANT LEAK: %s can see grant %s", who, grantID)
		}
	}
}
