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
)

// ---------------------------------------------------------------------------
// THE APPROVAL PATH -- the minting path, and the one that had no tests at all.
//
// Review finding B1: /consent built its ConsentContext entirely from the POST
// body and never re-resolved the client or re-matched the redirect. Authorize's
// exact match was therefore discarded one request later, and the invariant
// actually enforced was "a consent screen was shown once" rather than "this
// code may only travel where this client registered". The minted code carries
// the APPROVING OPERATOR'S tenant, so anything able to influence that POST --
// CSRF, XSS, a consent page echoing raw query params -- yielded cross-org read
// access. A minted grant and an issued token are not undone by refusing later.
//
// Findings G and H are the same hole seen from inside: the scope gate and the
// PKCE check in Approve were unproven, so deleting either went green.
// ---------------------------------------------------------------------------

const (
	registeredRedirect = "https://claude.ai/api/mcp/auth_callback"
	registeredClientID = "client-abc"
)

func approvalStore() *fakeStore {
	return &fakeStore{clientOK: true, client: liveClient(registeredRedirect)}
}

func validConsent() ConsentContext {
	return ConsentContext{
		ClientID:            registeredClientID,
		RedirectURI:         registeredRedirect,
		Scopes:              []Scope{ScopeRead},
		State:               "opaque-state",
		CodeChallenge:       "a-real-challenge-value",
		CodeChallengeMethod: "S256",
	}
}

func validApproval() ApprovalRequest {
	return ApprovalRequest{
		TenantID:  uuid.New(),
		UserID:    uuid.New(),
		Consent:   validConsent(),
		GrantName: "Claude Desktop on my laptop",
		SiteScope: SiteScopeRequest{Mode: SiteScopeModeAll},
	}
}

// B1. A redirect_uri the client never registered must not mint a code, however
// well-formed the rest of the approval is.
func TestApprove_RefusesARedirectTheClientNeverRegistered(t *testing.T) {
	attacker := []string{
		"https://attacker.example/exfiltrate",
		"https://claude.ai/api/mcp/auth_callback.evil.com",
		"https://claude.ai.evil.com/api/mcp/auth_callback",
		"https://claude.ai/api/mcp/auth_callback?next=https://evil.com",
		"https://claude.ai/api/mcp/auth_callback/../../evil",
		"http://claude.ai/api/mcp/auth_callback",
		"",
	}

	for _, bad := range attacker {
		t.Run(bad, func(t *testing.T) {
			store := approvalStore()
			req := validApproval()
			req.Consent.RedirectURI = bad

			got, err := NewService(store).Approve(context.Background(), req)
			if err == nil {
				t.Fatalf("Approve minted code %q for unregistered redirect_uri %q; "+
					"the code would then be honoured by Exchange, because Exchange "+
					"compares against the value STORED ON THE CODE ROW", got.Code, bad)
			}
			if got.Code != "" {
				t.Fatal("a refused approval still returned a code")
			}
			for _, c := range store.calls {
				if c == "CreateGrantWithCode" {
					t.Fatal("a refused approval still created a grant; a minted grant " +
						"is not undone by refusing afterwards")
				}
			}
		})
	}
}

// B1, the other half: an unregistered client_id must not mint either. There is
// no foreign key to catch this -- mcp_authorization_codes.client_id carries
// none by design (m124 DECISION 12) -- so Go is the only place it can be
// caught.
func TestApprove_RefusesAClientThatWasNeverRegistered(t *testing.T) {
	store := &fakeStore{clientOK: false} // LookupClient returns pgx.ErrNoRows
	req := validApproval()
	req.Consent.ClientID = "client-that-was-never-registered"

	got, err := NewService(store).Approve(context.Background(), req)
	if err == nil {
		t.Fatalf("Approve minted code %q for an unregistered client_id", got.Code)
	}
	for _, c := range store.calls {
		if c == "CreateGrantWithCode" {
			t.Fatal("a refused approval still created a grant")
		}
	}
}

// B1 must be enforced by RESOLVING the client, not by trusting the body. This
// pins the mechanism: LookupClient has to actually be called on the minting
// path, so the fix cannot regress into comparing the body against itself.
func TestApprove_ResolvesTheClientBeforeMinting(t *testing.T) {
	store := approvalStore()
	if _, err := NewService(store).Approve(context.Background(), validApproval()); err != nil {
		t.Fatalf("a legitimate approval was refused: %v", err)
	}

	var sawLookup, sawCreate bool
	for _, c := range store.calls {
		switch c {
		case "LookupClient":
			sawLookup = true
		case "CreateGrantWithCode":
			if !sawLookup {
				t.Fatal("the grant was created BEFORE the client was resolved")
			}
			sawCreate = true
		}
	}
	if !sawLookup {
		t.Fatal("Approve minted a code without ever calling LookupClient; the " +
			"redirect_uri is being matched against the request's own copy")
	}
	if !sawCreate {
		t.Fatal("a valid approval did not create a grant")
	}
}

// G. The exit gate must be re-run on the approval. The scope list arrives back
// over the wire and a tampered resubmission must not widen what Authorize
// already refused.
func TestApprove_ReRunsTheScopeExitGate(t *testing.T) {
	cases := []struct {
		name   string
		scopes []Scope
	}{
		{"no scopes at all", nil},
		{"empty slice", []Scope{}},
		{"unrecognised scope", []Scope{"fleet:admin"}},
		{"a write scope smuggled into the approval", []Scope{"mcp:write"}},
		{"recognised plus unrecognised", []Scope{ScopeRead, "fleet:destroy"}},
		{"blank string scope", []Scope{""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := approvalStore()
			req := validApproval()
			req.Consent.Scopes = tc.scopes

			if _, err := NewService(store).Approve(context.Background(), req); err == nil {
				t.Fatal("Approve accepted an approval naming no recognised scope; " +
					"the exit gate must hold on the minting path too")
			}
			for _, c := range store.calls {
				if c == "CreateGrantWithCode" {
					t.Fatal("a scope-refused approval still created a grant")
				}
			}
		})
	}
}

// H. PKCE must be re-checked on the approval. A code minted with no challenge,
// or with a downgraded method, is a code redeemable without a verifier.
func TestApprove_ReChecksThePKCEChallenge(t *testing.T) {
	cases := []struct{ name, challenge, method string }{
		{"no challenge", "", "S256"},
		{"blank challenge", "   ", "S256"},
		{"no method", "abc", ""},
		{"plain downgrade", "abc", "plain"},
		{"unknown method", "abc", "S512"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := approvalStore()
			req := validApproval()
			req.Consent.CodeChallenge = tc.challenge
			req.Consent.CodeChallengeMethod = tc.method

			if _, err := NewService(store).Approve(context.Background(), req); err == nil {
				t.Fatal("Approve minted a code with no valid S256 PKCE challenge")
			}
			for _, c := range store.calls {
				if c == "CreateGrantWithCode" {
					t.Fatal("a PKCE-refused approval still created a grant")
				}
			}
		})
	}
}

// The site-scope gate on the minting path, and the empty-payload trap with it.
func TestApprove_RefusesAnIncoherentOrEmptySiteScope(t *testing.T) {
	cases := []struct {
		name  string
		scope SiteScopeRequest
	}{
		{"mode omitted", SiteScopeRequest{}},
		{"unrecognised mode", SiteScopeRequest{Mode: "everything"}},
		{"tags naming nothing", SiteScopeRequest{Mode: SiteScopeModeTags}},
		{"list naming nothing", SiteScopeRequest{Mode: SiteScopeModeList}},
		{"all carrying a payload", SiteScopeRequest{
			Mode: SiteScopeModeAll, SiteIDs: []uuid.UUID{uuid.New()}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := validApproval()
			req.SiteScope = tc.scope
			if _, err := NewService(approvalStore()).Approve(context.Background(), req); err == nil {
				t.Fatal("Approve accepted an incoherent or empty site scope")
			}
		})
	}
}

// Not over-firing: a legitimate approval still mints, and the grant records the
// RESOLVED client id rather than the body's copy.
func TestApprove_ValidApprovalMintsAndRecordsTheResolvedClient(t *testing.T) {
	store := approvalStore()
	got, err := NewService(store).Approve(context.Background(), validApproval())
	if err != nil {
		t.Fatalf("a legitimate approval was refused: %v", err)
	}
	if got.Code == "" {
		t.Fatal("no code was minted")
	}
	if got.RedirectURI != registeredRedirect {
		t.Errorf("redirect = %q, want the registered one", got.RedirectURI)
	}
	if got.State != "opaque-state" {
		t.Errorf("state = %q; state must survive the round trip", got.State)
	}
	if got.GrantID == uuid.Nil {
		t.Error("grant id is nil")
	}
}

// B1 end to end through the real handler, with a real authenticated principal.
// This is the shape the reviewer demonstrated: a 200 with a usable code for an
// attacker-controlled redirect.
func TestConsentHandler_RefusesAnUnregisteredRedirect(t *testing.T) {
	store := approvalStore()
	gin := newAuthorizeRouter(t, store)
	NewHandler(NewService(store)).Register(gin.Group("/api/v2")) // distinct mount

	body := approvalRequestDTO{
		ClientID:            registeredClientID,
		RedirectURI:         "https://attacker.example/exfiltrate",
		Scopes:              []string{"mcp:read"},
		State:               "s",
		CodeChallenge:       "challenge",
		CodeChallengeMethod: "S256",
		GrantName:           "totally normal connection",
		SiteScopeMode:       "all",
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v2/oauth/mcp/consent", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	gin.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Fatalf("POST /consent returned 200 for an unregistered redirect_uri; body = %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"code"`) {
		t.Fatalf("a refused consent leaked a code: %s", w.Body.String())
	}
	for _, c := range store.calls {
		if c == "CreateGrantWithCode" {
			t.Fatal("the handler minted a grant for an unregistered redirect")
		}
	}
}

// ---------------------------------------------------------------------------
// The Exchange layers that mutation showed were unproven (J, L, A).
// ---------------------------------------------------------------------------

// J. A stored challenge whose method is not S256 must refuse. Such a row cannot
// be written by this code, but the check is the reason it cannot be redeemed if
// one ever exists.
func TestExchange_RefusesANonS256StoredChallenge(t *testing.T) {
	const (
		verifier = "verifier-for-the-non-s256-case-00000000000000"
		clientID = registeredClientID
	)
	// The method is the ONLY thing wrong: the challenge is left as the correct
	// S256 value, so verifyPKCE would SUCCEED. If the method check is removed
	// the code redeems, which is what makes this test isolate the method rather
	// than passing on a PKCE mismatch. (Setting the challenge to the raw
	// verifier -- what 'plain' would really store -- made this test pass with
	// the check deleted, because verifyPKCE refused it instead.)
	row := redeemableCode(t, verifier, registeredRedirect, clientID)
	row.CodeChallengeMethod = "plain"

	store := &fakeStore{
		codeOK: true, code: row,
		clientOK: true, client: liveClient(registeredRedirect),
	}
	if _, err := NewService(store).Exchange(context.Background(), TokenRequest{
		GrantType: "authorization_code", Code: "c", RedirectURI: registeredRedirect,
		ClientID: clientID, CodeVerifier: verifier,
	}); err == nil {
		t.Fatal("a 'plain' stored challenge was redeemed; only S256 may be")
	}
	if store.consumeCalls != 0 {
		t.Error("the code was consumed despite the challenge method being refused")
	}
}

// L. Expiry must refuse, and it must refuse before the consume.
func TestExchange_RefusesAnExpiredCode(t *testing.T) {
	const verifier = "verifier-for-the-expired-case-0000000000000000"
	row := redeemableCode(t, verifier, registeredRedirect, registeredClientID)
	row.IsExpired = true

	store := &fakeStore{
		codeOK: true, code: row,
		clientOK: true, client: liveClient(registeredRedirect),
	}
	if _, err := NewService(store).Exchange(context.Background(), TokenRequest{
		GrantType: "authorization_code", Code: "c", RedirectURI: registeredRedirect,
		ClientID: registeredClientID, CodeVerifier: verifier,
	}); err == nil {
		t.Fatal("an expired code was redeemed")
	}
	if store.consumeCalls != 0 {
		t.Error("an expired code was still put through the compare-and-set")
	}
}

// A. The belt-and-braces IsConsumed refusal. The compare-and-set is the real
// guarantee, but this layer is what makes a replay report "already redeemed"
// rather than a generic failure, and it was unproven.
func TestExchange_RefusesAnAlreadyConsumedCodeAtTheLookup(t *testing.T) {
	const verifier = "verifier-for-the-already-consumed-case-0000000"
	store := &fakeStore{
		codeOK: true, code: redeemableCode(t, verifier, registeredRedirect, registeredClientID),
		clientOK: true, client: liveClient(registeredRedirect),
		consumed: true, // the lookup will report IsConsumed
	}
	if _, err := NewService(store).Exchange(context.Background(), TokenRequest{
		GrantType: "authorization_code", Code: "c", RedirectURI: registeredRedirect,
		ClientID: registeredClientID, CodeVerifier: verifier,
	}); err == nil {
		t.Fatal("an already-consumed code was redeemed")
	}
	if store.consumeCalls != 0 {
		t.Error("an already-consumed code still reached the compare-and-set; the " +
			"early refusal did nothing")
	}
}

// ---------------------------------------------------------------------------
// H3 -- the token endpoint authenticates the client when one registered a
// secret, and client_id is required unconditionally.
// ---------------------------------------------------------------------------

func confidentialClient(secret string) sqlc.McpOauthClient {
	h := hashCredential(secret)
	c := liveClient(registeredRedirect)
	c.TokenEndpointAuthMethod = "client_secret_basic"
	c.ClientSecretHash = &h
	return c
}

func TestExchange_RequiresClientID(t *testing.T) {
	const verifier = "verifier-for-the-missing-client-id-000000000000"
	store := &fakeStore{
		codeOK: true, code: redeemableCode(t, verifier, registeredRedirect, registeredClientID),
		clientOK: true, client: liveClient(registeredRedirect),
	}
	// client_id omitted entirely. This used to skip the code-to-client binding
	// altogether: absence read as permission.
	if _, err := NewService(store).Exchange(context.Background(), TokenRequest{
		GrantType: "authorization_code", Code: "c", RedirectURI: registeredRedirect,
		CodeVerifier: verifier,
	}); err == nil {
		t.Fatal("an exchange with no client_id succeeded; omitting it must not " +
			"skip the code-to-client binding")
	}
}

func TestExchange_ConfidentialClientMustPresentItsSecret(t *testing.T) {
	const (
		verifier = "verifier-for-the-confidential-client-0000000000"
		secret   = "the-registered-client-secret"
	)

	newStore := func() *fakeStore {
		return &fakeStore{
			codeOK: true, code: redeemableCode(t, verifier, registeredRedirect, registeredClientID),
			clientOK: true, client: confidentialClient(secret),
		}
	}
	// ClientAuthVia is set to the REGISTERED transport throughout, so each
	// subtest isolates the property it names rather than being refused by the
	// transport check before reaching it.
	base := TokenRequest{
		GrantType: "authorization_code", Code: "c", RedirectURI: registeredRedirect,
		ClientID: registeredClientID, CodeVerifier: verifier,
		ClientAuthVia: "client_secret_basic",
	}

	t.Run("no secret at all", func(t *testing.T) {
		store := newStore()
		if _, err := NewService(store).Exchange(context.Background(), base); err == nil {
			t.Fatal("a client registered client_secret_basic redeemed a code without " +
				"presenting its secret; the stored hash is then decorative")
		}
		if store.consumeCalls != 0 {
			t.Error("the code was consumed despite client authentication failing")
		}
	})

	t.Run("wrong secret", func(t *testing.T) {
		req := base
		req.ClientSecret = "not-the-secret"
		if _, err := NewService(newStore()).Exchange(context.Background(), req); err == nil {
			t.Fatal("a wrong client_secret was accepted")
		}
	})

	t.Run("correct secret still works", func(t *testing.T) {
		req := base
		req.ClientSecret = secret
		got, err := NewService(newStore()).Exchange(context.Background(), req)
		if err != nil {
			t.Fatalf("a correctly authenticated confidential client was refused: %v", err)
		}
		if got.AccessToken == "" {
			t.Fatal("no access token issued")
		}
	})
}

// A public ('none') client must still work without a secret -- the check must
// not over-fire onto the PKCE-only path, which is the common one.
func TestExchange_PublicClientNeedsNoSecret(t *testing.T) {
	const verifier = "verifier-for-the-public-client-000000000000000"
	store := &fakeStore{
		codeOK: true, code: redeemableCode(t, verifier, registeredRedirect, registeredClientID),
		clientOK: true, client: liveClient(registeredRedirect), // method "none"
	}
	got, err := NewService(store).Exchange(context.Background(), TokenRequest{
		GrantType: "authorization_code", Code: "c", RedirectURI: registeredRedirect,
		ClientID: registeredClientID, CodeVerifier: verifier,
	})
	if err != nil {
		t.Fatalf("a public PKCE client was refused: %v", err)
	}
	if got.AccessToken == "" {
		t.Fatal("no access token issued to a valid public client")
	}
}

// A non-'none' client whose stored hash is NULL is an impossible row under
// mcp_oauth_clients_secret_matches_method_check. If one is ever seen it must
// refuse, not be read as "no secret required".
func TestExchange_ConfidentialClientWithNoStoredHashIsRefused(t *testing.T) {
	const verifier = "verifier-for-the-impossible-row-000000000000000"
	c := liveClient(registeredRedirect)
	c.TokenEndpointAuthMethod = "client_secret_basic"
	c.ClientSecretHash = nil // impossible per the CHECK

	store := &fakeStore{
		codeOK: true, code: redeemableCode(t, verifier, registeredRedirect, registeredClientID),
		clientOK: true, client: c,
	}
	if _, err := NewService(store).Exchange(context.Background(), TokenRequest{
		GrantType: "authorization_code", Code: "c", RedirectURI: registeredRedirect,
		ClientID: registeredClientID, ClientSecret: "anything", CodeVerifier: verifier,
	}); err == nil {
		t.Fatal("a confidential client with a NULL secret hash authenticated; a " +
			"missing hash must refuse, never mean 'no secret required'")
	}
}
