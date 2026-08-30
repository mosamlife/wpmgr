// adr064_s16_mcp_connections_integration_test.go: the list-and-revoke slice
// proven against the REAL SCHEMA, through the MOUNTED ROUTES, as wpmgr_app.
//
// WHY THIS FILE AND NOT ANOTHER UNIT TEST. internal/mcp/connections_test.go
// proves the Go branch structure against fakeStore, and a fake has no RLS
// policies -- so it cannot prove either of the two things that actually matter
// here:
//
//  1. THE REVOKE CASCADE. The grant and every token beneath it must die in one
//     statement. A fake returns whatever counts the test set.
//  2. THE SITE-SCOPE POLICY. mcp_grants_site_scope_select is RESTRICTIVE and
//     keys on the app.site_scope GUC, which only db.Pool.InScopedTenantTx sets.
//     A test that opens its own connection, or that reaches the table through
//     InTenantTx, leaves the policy INERT and passes regardless. That is
//     precisely how m112's proofs passed while the email domain was
//     cross-site readable.
//
// So: no connection is opened here that the request path does not open. Every
// read and write lands through db.Pool as wpmgr_app -- NOSUPERUSER, NOBYPASSRLS
// -- which mcpAssertAndReportRole asserts and prints inside the transactions
// actually used.
//
// ONE HONEST LIMIT, STATED RATHER THAN GLOSSED. Service.Authenticate cannot on
// its own distinguish a cascading revoke from a grant-only one: the
// ReCheckMCPRequestAuthorizationInTenantTx predicate is
// `g.status='active' AND t.status='active' AND ...`, so a revoked GRANT already
// refuses even with a live token. Driving Authenticate is therefore necessary
// (it proves revocation lands on the NEXT REQUEST) but not sufficient. The
// cascade itself is proven by reading mcp_connection_tokens back and asserting
// no active token survives -- see TestMCPRevokeCascadesToTokensAsAppRole's
// STEP 5. A proof that stopped at Authenticate would go green against a
// grant-only revoke, which is the exact half-revoked state a security review
// already observed on this stack.
package tests

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// connectedGrant is everything the OAuth flow produced, so the tests below can
// act on a REAL grant with a REAL token rather than on rows inserted by hand.
// Hand-inserted rows would skip the constraints and the RLS that the flow
// actually crosses.
type connectedGrant struct {
	grantID     uuid.UUID
	bearerToken string
	tenantID    uuid.UUID
	userID      uuid.UUID
	siteURL     string
}

// ---------------------------------------------------------------------------
// PROOF 1 -- REVOKE CASCADES TO TOKENS, AND REVOCATION LANDS ON THE NEXT
// REQUEST.
//
// The most important requirement in the slice: "a revoke that leaves live
// tokens is a UI button that lies."
// ---------------------------------------------------------------------------

func TestMCPRevokeCascadesToTokensAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	// The role is load-bearing: either privilege makes every assertion below
	// vacuous. Asserted inside a transaction the path actually uses.
	if err := pool.InMCPTokenLookupTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InMCPTokenLookupTx")
		return nil
	}); err != nil {
		t.Fatalf("open token lookup tx: %v", err)
	}

	svc := mcp.NewService(mcp.NewRepo(pool))
	g := connectRealGrant(t, pool, svc)
	eng := mountConnectionsLikeProduction(t, svc, adminPrincipal(g.tenantID, g.userID))

	// -----------------------------------------------------------------------
	// STEP 1: THE POSITIVE CONTROL. The token must work BEFORE the revoke.
	//
	// Without this the refusal in STEP 4 proves nothing: a token that never
	// worked is also refused after a revoke, and the test would go green
	// against a revoke that does absolutely nothing.
	// -----------------------------------------------------------------------
	auth, err := svc.Authenticate(ctx, g.bearerToken)
	if err != nil {
		t.Fatalf("STEP 1 control: the freshly minted token does NOT authenticate, "+
			"so nothing below can prove a revoke did anything: %v", err)
	}
	if auth.GrantID != g.grantID {
		t.Fatalf("STEP 1 control: token resolved to grant %s, want %s", auth.GrantID, g.grantID)
	}
	t.Logf("STEP 1 control ok: the token authenticates and resolves to grant %s", g.grantID)

	// -----------------------------------------------------------------------
	// STEP 2: and there IS a live token in the database to cascade to. Another
	// control -- if the flow had minted none, STEP 5 would pass vacuously.
	// -----------------------------------------------------------------------
	activeBefore := countTokens(t, pool, g.tenantID, g.grantID, "active")
	if activeBefore == 0 {
		t.Fatal("STEP 2 control: the OAuth flow left NO active token, so the " +
			"cascade assertion in STEP 5 would pass without testing anything")
	}
	t.Logf("STEP 2 control ok: %d active token(s) exist under the grant", activeBefore)

	// -----------------------------------------------------------------------
	// STEP 3: revoke, THROUGH THE MOUNTED ROUTE.
	// -----------------------------------------------------------------------
	var revoked struct {
		Status         string `json:"status"`
		GrantsRevoked  int64  `json:"grants_revoked"`
		TokensRevoked  int64  `json:"tokens_revoked"`
		AlreadyRevoked bool   `json:"already_revoked"`
	}
	code := mcpDoJSON(t, eng, http.MethodPost,
		mcp.ConnectionRevokePathFor(g.grantID.String()), nil, nil, &revoked)
	if code != http.StatusOK {
		t.Fatalf("STEP 3 revoke answered %d, want 200", code)
	}
	if revoked.GrantsRevoked != 1 {
		t.Errorf("STEP 3 grants_revoked = %d, want 1 for a first revoke of a live grant",
			revoked.GrantsRevoked)
	}
	if revoked.TokensRevoked != activeBefore {
		t.Errorf("STEP 3 tokens_revoked = %d but %d were active; the response "+
			"under-reports what it killed", revoked.TokensRevoked, activeBefore)
	}
	if revoked.AlreadyRevoked {
		t.Error("STEP 3 reported already_revoked=true for a first revoke")
	}
	t.Logf("STEP 3 ok: revoke flipped %d grant and %d token(s)",
		revoked.GrantsRevoked, revoked.TokensRevoked)

	// -----------------------------------------------------------------------
	// STEP 4: REVOCATION LANDS ON THE NEXT REQUEST. Driven through
	// Service.Authenticate -- the real credential path, not a fake.
	// -----------------------------------------------------------------------
	if _, err := svc.Authenticate(ctx, g.bearerToken); err == nil {
		t.Fatal("STEP 4: the revoked connection's token STILL AUTHENTICATES. " +
			"The revoke button lies.")
	} else {
		t.Logf("STEP 4 ok: the token is now refused: %v", err)
	}

	// -----------------------------------------------------------------------
	// STEP 5: THE CASCADE ITSELF. This is the assertion STEP 4 cannot make.
	//
	// The recheck predicate ANDs the grant status and the token status, so a
	// grant-only revoke also fails STEP 4. What separates the two is whether
	// the TOKEN ROW died. A live token on a revoked grant is the exact state a
	// security review observed here (grant_status revoked / token_status
	// active), and it matters because the token is a bearer credential sitting
	// in a client's config file: anything that ever checks it without joining
	// the grant hands back a working session.
	// -----------------------------------------------------------------------
	if stillActive := countTokens(t, pool, g.tenantID, g.grantID, "active"); stillActive != 0 {
		t.Fatalf("STEP 5 CASCADE FAILED: %d token(s) are still status='active' "+
			"under a revoked grant. The revoke did not cascade.", stillActive)
	}
	revokedTokens := countTokens(t, pool, g.tenantID, g.grantID, "revoked")
	if revokedTokens != activeBefore {
		t.Fatalf("STEP 5 CASCADE FAILED: %d token(s) are status='revoked' but %d "+
			"were active before; some token is in neither state",
			revokedTokens, activeBefore)
	}
	// revoked_at must be stamped with the status. The schema's
	// mcp_connection_tokens_revoked_at_matches_status_check requires it, so a
	// row with one and not the other cannot exist -- asserting it here proves
	// the constraint is actually on this table rather than assumed.
	if missing := countRevokedTokensWithoutTimestamp(t, pool, g.tenantID, g.grantID); missing != 0 {
		t.Errorf("STEP 5: %d revoked token(s) carry no revoked_at, so nothing "+
			"records WHEN the credential died", missing)
	}
	t.Logf("STEP 5 ok: all %d token(s) are revoked with a revoked_at stamp", revokedTokens)

	// -----------------------------------------------------------------------
	// STEP 6: the idempotent retry. Two zeroes is a SUCCESS, not a 404.
	//
	// A handler that mapped this to 404 or 500 would tell an operator their
	// correctly revoked credential failed to revoke -- inviting them to believe
	// it is still live.
	// -----------------------------------------------------------------------
	var retry struct {
		Status         string `json:"status"`
		GrantsRevoked  int64  `json:"grants_revoked"`
		TokensRevoked  int64  `json:"tokens_revoked"`
		AlreadyRevoked bool   `json:"already_revoked"`
	}
	code = mcpDoJSON(t, eng, http.MethodPost,
		mcp.ConnectionRevokePathFor(g.grantID.String()), nil, nil, &retry)
	if code != http.StatusOK {
		t.Fatalf("STEP 6 re-revoking an already-revoked grant answered %d, want 200. "+
			"The requested end state holds, so this is an idempotent success.", code)
	}
	if retry.GrantsRevoked != 0 || retry.TokensRevoked != 0 {
		t.Errorf("STEP 6 re-revoke reported (%d, %d), want (0, 0)",
			retry.GrantsRevoked, retry.TokensRevoked)
	}
	if !retry.AlreadyRevoked {
		t.Error("STEP 6 re-revoke did not report already_revoked=true")
	}
	t.Log("STEP 6 ok: the idempotent retry succeeded and reported it changed nothing")

	// -----------------------------------------------------------------------
	// STEP 7: a grant id that does not exist is a 404, and NOTHING was written.
	// This is what makes "matched zero rows" a failure in the one reading where
	// it IS one.
	// -----------------------------------------------------------------------
	code = mcpDoJSON(t, eng, http.MethodPost,
		mcp.ConnectionRevokePathFor(uuid.NewString()), nil, nil, nil)
	if code != http.StatusNotFound {
		t.Fatalf("STEP 7 revoking an unknown grant answered %d, want 404", code)
	}
	t.Log("STEP 7 ok: an unknown grant id is 404, distinguished from an idempotent retry")
}

// ---------------------------------------------------------------------------
// PROOF 2 -- THE LIST IS REAL, AND THE THREE-WAY FACTS SURVIVE THE ROUND TRIP.
// ---------------------------------------------------------------------------

func TestMCPConnectionsListThroughMountedRouteAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	if err := pool.InTenantTx(ctx, uuid.New(), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx")
		return nil
	}); err != nil {
		t.Fatalf("open tenant tx: %v", err)
	}

	svc := mcp.NewService(mcp.NewRepo(pool))
	g := connectRealGrant(t, pool, svc)
	eng := mountConnectionsLikeProduction(t, svc, adminPrincipal(g.tenantID, g.userID))

	var list struct {
		Connections []struct {
			ID            string  `json:"id"`
			Name          string  `json:"name"`
			Status        string  `json:"status"`
			SiteScopeMode string  `json:"site_scope_mode"`
			LastUsedAt    *string `json:"last_used_at"`
			RevokedAt     *string `json:"revoked_at"`
			ReportedName  *string `json:"reported_client_name"`
			Protocol      struct {
				State   string  `json:"state"`
				Version *string `json:"version"`
			} `json:"protocol"`
		} `json:"connections"`
	}
	code := mcpDoJSON(t, eng, http.MethodGet, mcp.ConnectionsPath, nil, nil, &list)
	if code != http.StatusOK {
		t.Fatalf("list answered %d, want 200", code)
	}
	if len(list.Connections) != 1 {
		t.Fatalf("list returned %d connections, want the 1 the OAuth flow just "+
			"minted. An empty list here is the whole defect this slice fixes.",
			len(list.Connections))
	}

	got := list.Connections[0]
	if got.ID != g.grantID.String() {
		t.Errorf("listed connection id %q, want %q", got.ID, g.grantID)
	}
	if got.Status != string(mcp.GrantStatusActive) {
		t.Errorf("a freshly consented grant lists as %q", got.Status)
	}
	// The grant was consented but never CONNECTED: no client has opened a
	// session, so client_identity_recorded_at is NULL. That is
	// "never_connected", NOT "absent" -- absence of a header is something only a
	// client that actually connected can express.
	if got.Protocol.State != string(mcp.ClientProtocolNeverConnected) {
		t.Errorf("protocol.state = %q for a grant nothing has connected to; want %q",
			got.Protocol.State, mcp.ClientProtocolNeverConnected)
	}
	if got.Protocol.Version != nil {
		t.Errorf("protocol.version = %q for a never-connected grant; NULL means the "+
			"client sent no protocol header, and rendering a version here puts a "+
			"claim in its mouth", *got.Protocol.Version)
	}
	if got.LastUsedAt != nil {
		t.Errorf("last_used_at = %q for a connection that has never been used; "+
			"want null", *got.LastUsedAt)
	}
	if got.RevokedAt != nil {
		t.Errorf("revoked_at = %q for a live connection", *got.RevokedAt)
	}
	if got.ReportedName != nil {
		t.Errorf("reported_client_name = %q before any client reported one", *got.ReportedName)
	}
	t.Logf("list ok: grant %s, status=%s, protocol.state=%s, last_used_at=null",
		got.ID, got.Status, got.Protocol.State)

	// -----------------------------------------------------------------------
	// Now record a connect with NO protocol header -- the case most real
	// clients produce -- and prove the list moves from never_connected to
	// absent WITHOUT inventing a version.
	// -----------------------------------------------------------------------
	auth, err := svc.Authenticate(ctx, g.bearerToken)
	if err != nil {
		t.Fatalf("authenticate before recording connect: %v", err)
	}
	// nil protocol header: the client sent none.
	if err := svc.RecordConnect(ctx, auth, "Claude Desktop", "1.4.0", nil); err != nil {
		t.Fatalf("record connect: %v", err)
	}

	code = mcpDoJSON(t, eng, http.MethodGet, mcp.ConnectionsPath, nil, nil, &list)
	if code != http.StatusOK {
		t.Fatalf("second list answered %d", code)
	}
	after := list.Connections[0]
	if after.Protocol.State != string(mcp.ClientProtocolAbsent) {
		t.Errorf("after a header-less connect, protocol.state = %q; want %q. "+
			"'connected and sent no header' and 'has never connected' are "+
			"different facts and m124 stores two columns to keep them apart",
			after.Protocol.State, mcp.ClientProtocolAbsent)
	}
	if after.Protocol.Version != nil {
		t.Errorf("protocol.version = %q after a header-less connect; the client "+
			"sent nothing and the negotiated floor is not its claim", *after.Protocol.Version)
	}
	// t.Fatal AND NOT t.Error, because the t.Logf at the end of this function
	// dereferences after.ReportedName. On t.Error execution continues, the
	// deref panics, and the panic is what gets reported -- so the one thing the
	// reader needs, WHICH assertion failed, is the one thing buried. Fail here
	// or the failure lies about itself.
	if after.ReportedName == nil || *after.ReportedName != "Claude Desktop" {
		t.Fatalf("the client's self-reported name did not survive to the list: %v",
			after.ReportedName)
	}
	// The operator's name is a different assertion and must not have been
	// overwritten by the client's.
	if after.Name == "Claude Desktop" {
		t.Error("the operator's connection name was replaced by the client's " +
			"self-reported name; those are two different claims")
	}
	t.Logf("list ok after connect: protocol.state=%s, version=null, reported_client_name=%q",
		after.Protocol.State, *after.ReportedName)
}

// ---------------------------------------------------------------------------
// PROOF 3 -- THE SITE-SCOPE GATE, AT BOTH LAYERS.
//
// Constraint 2. mcp_grants_site_scope_select is RESTRICTIVE and refuses by
// returning ZERO ROWS WITH NO ERROR, which on a list path is indistinguishable
// from an empty organisation. So both halves are proven: the policy is LIVE
// (the repo really does see nothing), and the service REFUSES OUT LOUD rather
// than reporting that nothing as an empty list.
// ---------------------------------------------------------------------------

func TestMCPGrantsSiteScopePolicyIsLiveAndTheServiceRefusesOutLoud(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	svc := mcp.NewService(mcp.NewRepo(pool))
	repo := mcp.NewRepo(pool)
	g := connectRealGrant(t, pool, svc)

	orgP := adminPrincipal(g.tenantID, g.userID)

	// -----------------------------------------------------------------------
	// CONTROL: the org member DOES see the grant through the very same repo
	// method. Without this, "the collaborator sees nothing" could equally mean
	// the query is broken for everyone.
	// -----------------------------------------------------------------------
	orgRows, err := repo.ListGrants(ctx, orgP)
	if err != nil {
		t.Fatalf("CONTROL: the org member's list failed: %v", err)
	}
	if len(orgRows) != 1 {
		t.Fatalf("CONTROL: the org member sees %d grants, want 1. The refusal "+
			"below would prove nothing against a query that returns nothing to "+
			"anybody.", len(orgRows))
	}
	t.Logf("CONTROL ok: the org member reads %d grant through repo.ListGrants", len(orgRows))

	// -----------------------------------------------------------------------
	// THE POLICY. A site-scoped collaborator reaching the repo directly --
	// which is what would happen if the handler's permission gate were ever
	// removed -- must see NOTHING. This runs through RunTenantTx, so
	// InScopedTenantTx sets app.site_scope='on' and the RESTRICTIVE policy is
	// genuinely evaluated. Reaching the table through InTenantTx instead would
	// leave it inert and this assertion would fail.
	// -----------------------------------------------------------------------
	scopedP := domain.Principal{
		Type:     domain.PrincipalUser,
		UserID:   g.userID,
		TenantID: g.tenantID,
		Role:     "admin", // admin on the one site it can see, and still refused
		Scope:    domain.ScopeSite,
		// A real site in this very tenant: the collaborator is legitimately
		// shared onto it, and must still not read the org's grant list.
		AllowedSiteIDs: []uuid.UUID{siteIDForURL(t, pool, g.tenantID, g.siteURL)},
	}

	scopedRows, err := repo.ListGrants(ctx, scopedP)
	if err != nil {
		t.Fatalf("the site-scoped read errored rather than being filtered: %v", err)
	}
	if len(scopedRows) != 0 {
		t.Fatalf("mcp_grants_site_scope_select IS INERT: a site-scoped principal "+
			"read %d grant(s), including scope_site_ids, which enumerates every "+
			"site the organisation has granted MCP access to (PR #569 finding F1)",
			len(scopedRows))
	}
	t.Log("POLICY ok: the RESTRICTIVE site-scope policy refused the read (0 rows)")

	// -----------------------------------------------------------------------
	// AND THE SERVICE MUST NOT REPORT THAT ZERO AS AN EMPTY LIST. This is the
	// half RLS structurally cannot do: it refuses silently, so the Go layer has
	// to be the one that SAYS "refused".
	// -----------------------------------------------------------------------
	conns, err := svc.ListConnections(ctx, scopedP)
	if err == nil {
		t.Fatalf("the service returned %d connections and no error for a "+
			"site-scoped principal. RLS gave it zero rows and it reported that "+
			"as 'you have no connections' -- a false statement produced by a "+
			"security control working correctly.", len(conns))
	}
	if conns != nil {
		t.Errorf("the refusal came with a non-nil slice of len %d", len(conns))
	}
	// The refusal must be a NAMED domain error, not a generic failure. An
	// operator who sees "something went wrong" retries; one who sees
	// "requires full organisation membership" understands and stops.
	if de, ok := domain.AsDomain(err); !ok || de.Code != mcp.ErrCodeOrgScopeRequired {
		t.Fatalf("want a %s domain error, got %v", mcp.ErrCodeOrgScopeRequired, err)
	}
	t.Logf("SERVICE ok: the site-scoped principal is refused by name: %v", err)

	// -----------------------------------------------------------------------
	// The same principal must not be able to REVOKE either. The insert/update
	// gates exist for the same reason as the select one.
	// -----------------------------------------------------------------------
	if _, err := svc.RevokeConnection(ctx, scopedP, g.grantID); err == nil {
		t.Fatal("a site-scoped collaborator revoked an organisation-wide credential")
	}
	// And the grant must still be live: the refusal must have written nothing.
	stillActive := countGrants(t, pool, g.tenantID, g.grantID, "active")
	if stillActive != 1 {
		t.Fatalf("after a refused revoke the grant's active count is %d, want 1; "+
			"the refusal wrote something", stillActive)
	}
	t.Log("REVOKE ok: the site-scoped principal is refused and the grant is untouched")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// adminPrincipal is a full organisation member at the role
// authz.minRoleFor[PermAPIKeyRead] requires.
func adminPrincipal(tenantID, userID uuid.UUID) domain.Principal {
	return domain.Principal{
		Type:     domain.PrincipalUser,
		UserID:   userID,
		TenantID: tenantID,
		Role:     "admin",
		Scope:    "org",
	}
}

// connectRealGrant drives the WHOLE OAuth flow through the mounted routes and
// returns the grant and bearer token it produced.
//
// It goes through the flow rather than inserting rows because a hand-inserted
// grant skips every CHECK, every RLS policy and every service invariant the
// real path crosses -- and the thing under test here is what happens to a real
// credential.
func connectRealGrant(t *testing.T, pool *db.Pool, svc *mcp.Service) connectedGrant {
	t.Helper()

	suffix := uuid.NewString()[:8]
	tenantID := seedTenant(t, pool, "mcp-s16-"+suffix)
	userID := seedUserRow(t, pool, "mcp-s16-"+suffix+"@example.test")
	siteURL := "https://" + uuid.NewString()[:8] + ".example.test"
	seedSite(t, pool, tenantID, siteURL)

	eng := mountOAuthLikeProduction(t, svc, tenantID, userID)
	const redirectURI = "https://claude.ai/api/mcp/auth_callback"

	var reg struct {
		ClientID string `json:"client_id"`
	}
	if code := mcpDoJSON(t, eng, http.MethodPost, mcp.RegisterPath, map[string]any{
		"redirect_uris":              []string{redirectURI},
		"client_name":                "Claude Desktop",
		"client_uri":                 "https://claude.ai",
		"token_endpoint_auth_method": "none",
	}, nil, &reg); code != http.StatusCreated {
		t.Fatalf("fixture: registration answered %d, want 201", code)
	}

	verifier := "s16-verifier-" + uuid.NewString() + uuid.NewString()
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", reg.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", string(mcp.ScopeRead))
	q.Set("state", "s16-state")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	if code := mcpDoJSON(t, eng, http.MethodGet,
		mcp.AuthorizePath+"?"+q.Encode(), nil, nil, nil); code != http.StatusOK {
		t.Fatalf("fixture: authorize answered %d, want 200", code)
	}

	var approval struct {
		GrantID string `json:"grant_id"`
		Code    string `json:"code"`
	}
	if code := mcpDoJSON(t, eng, http.MethodPost, mcp.ConsentPath, map[string]any{
		"client_id":             reg.ClientID,
		"redirect_uri":          redirectURI,
		"scopes":                []string{string(mcp.ScopeRead)},
		"state":                 "s16-state",
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"name":                  "s16 connection under test",
		"site_scope_mode":       string(mcp.SiteScopeModeAll),
	}, nil, &approval); code != http.StatusOK {
		t.Fatalf("fixture: consent answered %d, want 200", code)
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", approval.Code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", reg.ClientID)
	form.Set("code_verifier", verifier)

	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if code := mcpPostForm(t, eng, mcp.TokenPath, form, &tok); code != http.StatusOK {
		t.Fatalf("fixture: token exchange answered %d, want 200", code)
	}
	if tok.AccessToken == "" {
		t.Fatal("fixture: token exchange returned no access_token")
	}

	grantID, err := uuid.Parse(approval.GrantID)
	if err != nil {
		t.Fatalf("fixture: grant id %q is not a uuid: %v", approval.GrantID, err)
	}

	return connectedGrant{
		grantID:     grantID,
		bearerToken: tok.AccessToken,
		tenantID:    tenantID,
		userID:      userID,
		siteURL:     siteURL,
	}
}

// mountOAuthLikeProduction mounts the four OAuth endpoints the way server.New
// does, with a principal standing in for RequireAuth + RequireTenant.
func mountOAuthLikeProduction(t *testing.T, svc *mcp.Service, tenantID, userID uuid.UUID) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	eng := gin.New()
	h := mcp.NewHandler(svc)
	h.RegisterPublic(eng.Group(mcp.APIV1Prefix))

	authed := eng.Group(mcp.APIV1Prefix)
	authed.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(),
			domain.Principal{UserID: userID, TenantID: tenantID}))
		c.Next()
	})
	h.Register(authed)
	return eng
}

// mountConnectionsLikeProduction mounts the connections surface the way
// server.New does: on the /api/v1 group, behind a principal, with
// RegisterConnections installing its OWN authz.RequirePermission middleware.
//
// The permission gate is NOT stubbed. It is the site-scope gate at the
// permission layer (PermAPIKeyRead and PermAPIKeyManage are both members of
// authz.orgLevelPerms), so stubbing it out would remove one of the three layers
// under test.
func mountConnectionsLikeProduction(t *testing.T, svc *mcp.Service, p domain.Principal) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	eng := gin.New()
	g := eng.Group(mcp.APIV1Prefix)
	g.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	mcp.NewHandler(svc).RegisterConnections(g)
	return eng
}

// countTokens counts this grant's tokens in a given status, THROUGH THE POOL as
// wpmgr_app and inside InTenantTx -- the same transaction shape the request
// path uses. A raw connection here would read past RLS and could report rows
// the application can never see.
func countTokens(t *testing.T, pool *db.Pool, tenantID, grantID uuid.UUID, status string) int64 {
	t.Helper()
	var n int64
	err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM mcp_connection_tokens
			  WHERE tenant_id = $1 AND grant_id = $2 AND status = $3`,
			tenantID, grantID, status).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count %s tokens: %v", status, err)
	}
	return n
}

// countRevokedTokensWithoutTimestamp finds revoked tokens carrying no
// revoked_at -- the half-state the schema CHECK is supposed to make impossible.
func countRevokedTokensWithoutTimestamp(t *testing.T, pool *db.Pool, tenantID, grantID uuid.UUID) int64 {
	t.Helper()
	var n int64
	err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM mcp_connection_tokens
			  WHERE tenant_id = $1 AND grant_id = $2
			    AND status = 'revoked' AND revoked_at IS NULL`,
			tenantID, grantID).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count revoked tokens without a timestamp: %v", err)
	}
	return n
}

// countGrants counts a grant in a given status, same transaction shape.
func countGrants(t *testing.T, pool *db.Pool, tenantID, grantID uuid.UUID, status string) int64 {
	t.Helper()
	var n int64
	err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM mcp_grants
			  WHERE tenant_id = $1 AND id = $2 AND status = $3`,
			tenantID, grantID, status).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count %s grants: %v", status, err)
	}
	return n
}

// siteIDForURL resolves the seeded site's id through the pool as wpmgr_app.
func siteIDForURL(t *testing.T, pool *db.Pool, tenantID uuid.UUID, siteURL string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT id FROM sites WHERE tenant_id = $1 AND url = $2`,
			tenantID, siteURL).Scan(&id)
	})
	if err != nil {
		t.Fatalf("resolve site id for %s: %v", siteURL, err)
	}
	return id
}

// mcpPostForm posts an application/x-www-form-urlencoded body, which is what
// RFC 6749 specifies for the token endpoint and what every client library
// sends. A non-2xx is returned to the caller rather than swallowed here.
func mcpPostForm(t *testing.T, eng *gin.Engine, path string, form url.Values, out any) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "203.0.113.7:5555"

	w := httptest.NewRecorder()
	eng.ServeHTTP(w, req)

	if out != nil && w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("POST %s returned %d with a body that is not JSON: %v\nbody: %s",
				path, w.Code, err, w.Body.String())
		}
	}
	return w.Code
}
