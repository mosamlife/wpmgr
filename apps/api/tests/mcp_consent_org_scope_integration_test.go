// mcp_consent_org_scope_integration_test.go: the MCP consent route is
// ORG-LEVEL, executed against the REAL schema as wpmgr_app.
//
// WHY THIS FILE EXISTS. An MCP grant carries a site_scope_mode of
// 'all' | 'tags' | 'sites'. 'all' is resolved at read time against the whole
// organisation, so the grant is an org-level object no matter which sites the
// approver can personally see. The question this file answers is therefore not
// "does the grant read the right sites" but "who is allowed to mint one at
// all", and until this file existed nothing executed that question.
//
// Everything above the SQL layer on this surface runs against fakeStore, which
// opens no transaction, sets no GUC and evaluates no policy. A fake cannot
// distinguish InTenantTx from InScopedTenantTx, so it reports success for both
// and the site-scope RLS is invisible to it. A proof that sets app.site_scope
// by hand and watches mcp_grants_site_scope_insert refuse is a true statement
// about a transaction, but not about the transaction this call path opens --
// the same shape as m112, where every proof opened its own connection, so every
// policy was inert and every test was green.
//
// WHAT IS ACTUALLY UNDER TEST, and neither half is decidable by reading:
//
//  1. THE REFUSAL. A site-scoped collaborator drives the mounted /consent
//     route and asks for site_scope_mode 'all'. It must be REFUSED and it must
//     leave no row behind. Refusal alone is not enough: a route that answers
//     403 after committing has still written the row.
//
//  2. THE OVER-FIRE. An ordinary org member drives the same route and must
//     still complete consent and receive a code. A gate that refuses everyone
//     is not a gate, and this half is what stops the fix being switched off.
//
// HOW IT IS TESTED. Through the mounted gin routes, via mountLikeProduction --
// the same Handler.Register call server.New makes, the same Service, the same
// Repo, the same tx helpers. No GUC is ever hand-set here and this file opens
// no connection of its own.
//
// The role is load-bearing: wpmgr_app is NOSUPERUSER NOBYPASSRLS, and either
// privilege would make the RLS half pass vacuously. It is asserted, and printed.
package tests

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// mcpCountGrants counts the tenant's grants through InTenantTx -- the same
// helper the operator read path uses, as wpmgr_app. It is the "left no row
// behind" half of the escalation proof: a 403 that committed first is still an
// escalation, and only a count can tell the two apart.
func mcpCountGrants(t *testing.T, pool *db.Pool, tenantID uuid.UUID) int {
	t.Helper()
	var n int
	err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			"SELECT count(*) FROM mcp_grants WHERE tenant_id = $1", tenantID).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count grants for tenant %s: %v", tenantID, err)
	}
	return n
}

// mcpRegisterClient drives the UNAUTHENTICATED RFC 7591 registration endpoint
// and returns the client_id. It is unauthenticated by specification, so a
// site-scoped collaborator can reach it exactly as an org member can -- which
// is why the escalation below needs no help from an operator to obtain a
// client, and why /consent cannot rely on client registration as a gate.
func mcpRegisterClient(t *testing.T, eng *gin.Engine, redirectURI string) string {
	t.Helper()
	var reg struct {
		ClientID string `json:"client_id"`
	}
	code := mcpDoJSON(t, eng, http.MethodPost, "/api/v1/oauth/mcp/register",
		map[string]any{
			"redirect_uris":              []string{redirectURI},
			"client_name":                "Consent scope probe",
			"token_endpoint_auth_method": "none",
		}, nil, &reg)
	if code != http.StatusCreated || reg.ClientID == "" {
		t.Fatalf("register client answered %d with client_id=%q, want 201 and an id",
			code, reg.ClientID)
	}
	return reg.ClientID
}

// TestMCPConsentRefusesASiteScopedCollaboratorAsAppRole is half 1. A
// collaborator shared onto ONE site must not be able to mint a grant, and least
// of all one scoped to 'all'.
//
// Three layers have to hold for this to pass and each is independently
// sufficient to fail it: authz.RequireOrgScope on the mounted route, the
// IsSiteConstrained check in Approve, and RunTenantTx in the repo. Deleting any
// one of them should still leave this green; deleting all three must not.
func TestMCPConsentRefusesASiteScopedCollaboratorAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	// Either privilege would make the RLS layer pass vacuously.
	if err := pool.InMCPClientLookupTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InMCPClientLookupTx")
		return nil
	}); err != nil {
		t.Fatalf("open lookup tx: %v", err)
	}

	tenantID := seedTenant(t, pool, "mcp-orgscope-"+uuid.NewString()[:8])
	userID := seedUserRow(t, pool, "mcp-orgscope-"+uuid.NewString()[:8]+"@example.test")

	// TWO sites. The collaborator is shared onto sharedSite only; hiddenSite is
	// the site the escalation would have reached. Both exist so that
	// site_scope_mode 'all' is genuinely wider than the allowlist -- with one
	// site the grant would be no wider than the sharing and the test would
	// prove nothing about scope.
	sharedSite := seedSite(t, pool, tenantID, "https://shared-"+uuid.NewString()[:8]+".example.test")
	hiddenSite := seedSite(t, pool, tenantID, "https://hidden-"+uuid.NewString()[:8]+".example.test")
	t.Logf("tenant %s: collaborator is shared onto %s and NOT onto %s",
		tenantID, sharedSite, hiddenSite)

	// The site-scoped principal, exactly as authz builds one for a collaborator:
	// Scope "site" plus an allowlist of the shared site only.
	collaborator := domain.Principal{
		UserID:         userID,
		TenantID:       tenantID,
		Scope:          domain.ScopeSite,
		AllowedSiteIDs: []uuid.UUID{sharedSite},
	}
	if !collaborator.IsSiteConstrained() {
		t.Fatal("the principal under test is not site-constrained; the test would " +
			"prove nothing")
	}

	svc := mcp.NewService(mcp.NewRepo(pool))
	eng := mountLikeProduction(t, svc, collaborator)

	const redirectURI = "https://claude.ai/api/mcp/auth_callback"
	clientID := mcpRegisterClient(t, eng, redirectURI)

	verifier := "orgscope-verifier-" + uuid.NewString() + uuid.NewString()
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	before := mcpCountGrants(t, pool, tenantID)

	// -----------------------------------------------------------------------
	// THE ESCALATION, posted directly at /consent.
	//
	// Deliberately NOT preceded by a successful /authorize. Approve re-resolves
	// the client and re-matches the redirect from the body, so /consent is
	// reachable on its own -- an attacker never has to pass the consent screen.
	// Testing the pair in sequence would have hidden that.
	// -----------------------------------------------------------------------
	var consentErr struct {
		Err     string `json:"error"`
		ErrDesc string `json:"error_description"`
		Code    string `json:"code"`
	}
	status := mcpDoJSON(t, eng, http.MethodPost, "/api/v1/oauth/mcp/consent", map[string]any{
		"client_id":             clientID,
		"redirect_uri":          redirectURI,
		"scopes":                []string{string(mcp.ScopeRead)},
		"state":                 "orgscope-state",
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		// "name", the field approvalRequestDTO actually binds. It matters that
		// this body is otherwise VALID: a body the handler would reject anyway
		// makes the refusal ambiguous, and this test would then pass on an
		// unrelated 400 while proving nothing about scope.
		"name": "org scope probe",
		// The whole point: an ORG-WIDE grant, requested by a principal that can
		// see one site.
		"site_scope_mode": "all",
	}, nil, &consentErr)

	if status == http.StatusOK {
		t.Fatalf("ESCALATION: a site-scoped collaborator minted a grant through "+
			"POST /consent with site_scope_mode 'all'; the route answered 200. "+
			"tenant=%s allowlist=[%s] unreachable_site=%s",
			tenantID, sharedSite, hiddenSite)
	}
	if status != http.StatusForbidden {
		t.Errorf("POST /consent answered %d for a site-scoped collaborator, want 403; "+
			"body error=%q code=%q desc=%q",
			status, consentErr.Err, consentErr.Code, consentErr.ErrDesc)
	}
	// THE REFUSAL MUST BE LEGIBLE TO THE CONSENT SCREEN, not merely correct.
	// apps/web/src/features/mcp-consent/use-consent.ts reads `error` and
	// `error_description` and falls back to "server_error" when neither is
	// present, so a refusal in the generic {code, message} envelope reaches the
	// user as an unexplained server fault. Asserting only the status would let
	// that regress silently.
	if consentErr.Err != "access_denied" {
		t.Errorf("POST /consent refused with error=%q, want %q. The consent screen "+
			"parses this field and shows a server fault when it is absent.",
			consentErr.Err, "access_denied")
	}
	if consentErr.ErrDesc == "" {
		t.Error("POST /consent refused with an empty error_description; the screen " +
			"has nothing to tell the user beyond the error code")
	}
	if consentErr.Code != "" {
		t.Errorf("POST /consent answered in the generic {code, message} envelope "+
			"(code=%q); these routes answer in the OAuth envelope", consentErr.Code)
	}
	t.Logf("POST /consent refused the collaborator with %d (error=%q desc=%q)",
		status, consentErr.Err, consentErr.ErrDesc)

	// A 403 that committed first is still an escalation.
	if after := mcpCountGrants(t, pool, tenantID); after != before {
		t.Fatalf("ESCALATION: /consent was refused but the grant count moved "+
			"%d -> %d; the refusal happened after the write", before, after)
	}

	// -----------------------------------------------------------------------
	// /authorize, for the blast radius. It mints nothing -- it validates PKCE
	// and the redirect and returns the consent screen's contents -- so on its
	// own it is not an escalation. It carries the same gate anyway: a route
	// that renders the consent screen to a principal who may not consent
	// invites the exact confusion this defect was.
	// -----------------------------------------------------------------------
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", string(mcp.ScopeRead))
	q.Set("state", "orgscope-state")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")

	var authErr struct {
		Err     string `json:"error"`
		ErrDesc string `json:"error_description"`
		Code    string `json:"code"`
	}
	authStatus := mcpDoJSON(t, eng, http.MethodGet,
		"/api/v1/oauth/mcp/authorize?"+q.Encode(), nil, nil, &authErr)
	if authStatus != http.StatusForbidden {
		t.Errorf("GET /authorize answered %d for a site-scoped collaborator, want 403",
			authStatus)
	}
	if authErr.Err != "access_denied" || authErr.Code != "" {
		t.Errorf("GET /authorize refused with error=%q code=%q, want the OAuth "+
			"envelope carrying access_denied", authErr.Err, authErr.Code)
	}
	t.Logf("GET /authorize refused the collaborator with %d (error=%q)",
		authStatus, authErr.Err)

	if final := mcpCountGrants(t, pool, tenantID); final != before {
		t.Fatalf("grant count moved %d -> %d across the whole flow", before, final)
	}
}

// TestMCPCreateGrantWithCodeRefusesASiteScopedPrincipalAsAppRole pins the
// DEEPEST layer on its own, at the repo, with no middleware anywhere near it.
//
// It exists because the two route-level tests above cannot see this layer while
// the route-level gates hold: the middleware refuses first, so swapping
// RunTenantTx back to InTenantTx changes nothing they observe and the RLS layer
// would be unpinned -- defence in depth that no test defends. Here the call is
// made directly, so the only thing that can refuse it is the database.
//
// The failure it guards against is silent by construction: with app.site_scope
// unset the RESTRICTIVE policy admits the row and the write simply succeeds.
func TestMCPCreateGrantWithCodeRefusesASiteScopedPrincipalAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	if err := pool.InMCPClientLookupTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InMCPClientLookupTx")
		return nil
	}); err != nil {
		t.Fatalf("open lookup tx: %v", err)
	}

	tenantID := seedTenant(t, pool, "mcp-repolayer-"+uuid.NewString()[:8])
	userID := seedUserRow(t, pool, "mcp-repolayer-"+uuid.NewString()[:8]+"@example.test")
	siteID := seedSite(t, pool, tenantID, "https://repolayer-"+uuid.NewString()[:8]+".example.test")

	repo := mcp.NewRepo(pool)
	before := mcpCountGrants(t, pool, tenantID)

	collaborator := domain.Principal{
		UserID:         userID,
		TenantID:       tenantID,
		Scope:          domain.ScopeSite,
		AllowedSiteIDs: []uuid.UUID{siteID},
	}

	clientID := "repolayer-client-" + uuid.NewString()
	secretHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if n, err := repo.RegisterClient(ctx, sqlc.RegisterMCPOAuthClientParams{
		ClientID:                clientID,
		ClientSecretHash:        &secretHash,
		TokenEndpointAuthMethod: "client_secret_basic",
		RedirectUris:            []string{"https://claude.ai/api/mcp/auth_callback"},
	}); err != nil || n != 1 {
		t.Fatalf("seed client: affected=%d err=%v", n, err)
	}

	_, _, err := repo.CreateGrantWithCode(ctx, collaborator, sqlc.CreateMCPGrantParams{
		TenantID:      tenantID,
		Name:          "repo layer probe",
		Status:        "active",
		SiteScopeMode: "all",
		ScopeTagIds:   []uuid.UUID{},
		ScopeSiteIds:  []uuid.UUID{},
		ClientID:      &clientID,
	}, func(grantID uuid.UUID) sqlc.CreateMCPAuthorizationCodeParams {
		return sqlc.CreateMCPAuthorizationCodeParams{
			TenantID:            tenantID,
			GrantID:             grantID,
			ClientID:            clientID,
			CodeHash:            "beefcafe0123456789abcdef0123456789abcdef0123456789abcdef01234567",
			CodeChallenge:       "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM",
			CodeChallengeMethod: "S256",
			RedirectUri:         "https://claude.ai/api/mcp/auth_callback",
			ExpiresAt:           time.Now().UTC().Add(5 * time.Minute),
		}
	})
	if err == nil {
		t.Fatal("CreateGrantWithCode accepted a site-scoped principal. The write " +
			"reached mcp_grants with app.site_scope unset, so the RESTRICTIVE " +
			"site-scope policy was not live for this transaction.")
	}
	t.Logf("the database refused the write: %v", err)

	if after := mcpCountGrants(t, pool, tenantID); after != before {
		t.Fatalf("the call errored but the grant count moved %d -> %d", before, after)
	}
}

// TestMCPConsentAdmitsAnOrgMemberAsAppRole is half 2: the over-fire. The gate
// must refuse the collaborator WITHOUT refusing the operator the feature
// exists for. Without this, "return 403 always" would pass half 1.
func TestMCPConsentAdmitsAnOrgMemberAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	if err := pool.InMCPClientLookupTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InMCPClientLookupTx")
		return nil
	}); err != nil {
		t.Fatalf("open lookup tx: %v", err)
	}

	tenantID := seedTenant(t, pool, "mcp-orgmember-"+uuid.NewString()[:8])
	userID := seedUserRow(t, pool, "mcp-orgmember-"+uuid.NewString()[:8]+"@example.test")
	seedSite(t, pool, tenantID, "https://member-"+uuid.NewString()[:8]+".example.test")

	// An ordinary org member: Scope "org", no allowlist.
	member := domain.Principal{UserID: userID, TenantID: tenantID, Scope: domain.ScopeOrg}
	if member.IsSiteConstrained() {
		t.Fatal("the org member under test reports as site-constrained; the " +
			"over-fire half would pass for the wrong reason")
	}

	svc := mcp.NewService(mcp.NewRepo(pool))
	eng := mountLikeProduction(t, svc, member)

	const redirectURI = "https://claude.ai/api/mcp/auth_callback"
	clientID := mcpRegisterClient(t, eng, redirectURI)

	verifier := "orgmember-verifier-" + uuid.NewString() + uuid.NewString()
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	before := mcpCountGrants(t, pool, tenantID)

	// The consent screen first, the way a real operator arrives.
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", string(mcp.ScopeRead))
	q.Set("state", "orgmember-state")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")

	if s := mcpDoJSON(t, eng, http.MethodGet,
		"/api/v1/oauth/mcp/authorize?"+q.Encode(), nil, nil, nil); s != http.StatusOK {
		t.Fatalf("OVER-FIRE: GET /authorize answered %d for an ordinary org "+
			"member, want 200", s)
	}

	var approval struct {
		GrantID string `json:"grant_id"`
		Code    string `json:"code"`
		State   string `json:"state"`
	}
	status := mcpDoJSON(t, eng, http.MethodPost, "/api/v1/oauth/mcp/consent", map[string]any{
		"client_id":             clientID,
		"redirect_uri":          redirectURI,
		"scopes":                []string{string(mcp.ScopeRead)},
		"state":                 "orgmember-state",
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"name":                  "my laptop",
		"site_scope_mode":       "all",
	}, nil, &approval)

	if status != http.StatusOK {
		t.Fatalf("OVER-FIRE: POST /consent answered %d for an ordinary org "+
			"member, want 200. The gate refuses the people the feature is for.",
			status)
	}
	if approval.GrantID == "" || approval.Code == "" {
		t.Fatalf("OVER-FIRE: /consent answered 200 but returned grant_id=%q code=%q; "+
			"a consent that mints nothing is not a success",
			approval.GrantID, approval.Code)
	}
	if approval.State != "orgmember-state" {
		t.Errorf("/consent returned state %q, want the state it was given", approval.State)
	}

	// The row is really there, read back as wpmgr_app.
	if after := mcpCountGrants(t, pool, tenantID); after != before+1 {
		t.Fatalf("OVER-FIRE: /consent answered 200 with a grant_id but the grant "+
			"count moved %d -> %d, want %d", before, after, before+1)
	}
	t.Logf("org member completed consent: grant_id=%s, grants %d -> %d",
		approval.GrantID, before, before+1)
}
