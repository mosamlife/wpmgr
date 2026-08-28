package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---------------------------------------------------------------------------
// 1. THE TOKEN EXCHANGE IS ATOMIC.
//
// Burning the code before minting the token is the correct ORDER -- the reverse
// risks two tokens from one code -- but as two separate commits a failure in
// between left a state that is secure and useless: the code permanently
// consumed, no token issued, and a client that did nothing wrong unable to
// retry and forced to restart the whole browser flow with nothing explaining
// why. Both steps now share one InTenantTx.
//
// The fake models the ROLLBACK rather than the happy path: when the token
// insert fails it does not flip `consumed`, exactly as the real transaction
// rolls the UPDATE back. A fake that consumed anyway would model two commits --
// the defect itself -- and this test would pass against the broken code.
// ---------------------------------------------------------------------------

func TestExchange_TokenPersistenceFailureLeavesTheCodeRedeemable(t *testing.T) {
	const (
		verifier = "verifier-for-the-atomicity-case-0000000000000"
		redirect = "https://claude.ai/cb"
		clientID = "client-abc"
	)
	newStore := func() *fakeStore {
		return &fakeStore{
			codeOK: true, code: redeemableCode(t, verifier, redirect, clientID),
			clientOK: true, client: liveClient(redirect),
		}
	}
	req := TokenRequest{
		GrantType: "authorization_code", Code: "the-code", RedirectURI: redirect,
		ClientID: clientID, CodeVerifier: verifier,
	}

	// The token INSERT fails inside the redeem transaction.
	store := newStore()
	store.tokenPersistErr = &pgconn.PgError{Code: "53100", Message: "disk full"}

	got, err := NewService(store).Exchange(context.Background(), req)
	if err == nil {
		t.Fatalf("a failed token persistence still reported success: %+v", got)
	}
	if got.AccessToken != "" {
		t.Fatal("a failed exchange returned an access token")
	}
	if store.tokensMinted != 0 {
		t.Fatalf("%d tokens were minted despite the insert failing", store.tokensMinted)
	}

	// THE ASSERTION THAT MATTERS. The consume must have rolled back with it, so
	// the code is still redeemable and the client may retry the same request.
	if store.consumed {
		t.Fatal("the authorization code was left CONSUMED after the token insert " +
			"failed. It can never be redeemed, the client did nothing wrong, and it " +
			"must restart the entire browser flow. Consume and issue must share one " +
			"transaction")
	}

	// And prove it really is retryable: the same code, against a healthy store,
	// now succeeds. This is the half a rollback-less fake would fail.
	healthy := newStore()
	retried, err := NewService(healthy).Exchange(context.Background(), req)
	if err != nil {
		t.Fatalf("the code was not redeemable on retry after a transient failure: %v", err)
	}
	if retried.AccessToken == "" {
		t.Fatal("the retry produced no access token")
	}
	if healthy.tokensMinted != 1 {
		t.Fatalf("retry minted %d tokens, want 1", healthy.tokensMinted)
	}
}

// The transient failure must surface as an infra error, not as a domain
// refusal. A client reading "invalid_grant" concludes its code is bad and
// restarts the flow; this one should retry the same request.
func TestExchange_TransientFailureIsNotReportedAsAnInvalidGrant(t *testing.T) {
	const (
		verifier = "verifier-for-the-error-shape-case-000000000000"
		redirect = "https://claude.ai/cb"
		clientID = "client-abc"
	)
	store := &fakeStore{
		codeOK: true, code: redeemableCode(t, verifier, redirect, clientID),
		clientOK: true, client: liveClient(redirect),
		tokenPersistErr: &pgconn.PgError{Code: "53100", Message: "disk full"},
	}
	_, err := NewService(store).Exchange(context.Background(), TokenRequest{
		GrantType: "authorization_code", Code: "c", RedirectURI: redirect,
		ClientID: clientID, CodeVerifier: verifier,
	})
	if err == nil {
		t.Fatal("no error from a failed token insert")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Errorf("the underlying failure was lost; got %v", err)
	}
	if strings.Contains(err.Error(), "already used") {
		t.Error("a transient infrastructure failure was reported as a spent code; " +
			"the client would restart the flow instead of retrying")
	}
}

// ---------------------------------------------------------------------------
// 2. THE REGISTERED token_endpoint_auth_method IS ENFORCED.
//
// The column is NOT NULL over a closed set with no default, so the value is
// always a decision somebody made. Accepting a secret through a transport the
// client did not register makes the column decorative -- the same shape as a
// column no statement could write, which m126 deleted rather than leave lying
// around.
// ---------------------------------------------------------------------------

func TestExchange_CredentialTransportMustMatchTheRegisteredMethod(t *testing.T) {
	const (
		verifier = "verifier-for-the-transport-case-00000000000000"
		secret   = "the-registered-client-secret"
	)
	cases := []struct {
		name       string
		registered string
		presented  string
		wantOK     bool
	}{
		{"basic client using basic", "client_secret_basic", "client_secret_basic", true},
		{"post client using post", "client_secret_post", "client_secret_post", true},
		{"basic client posting body credentials", "client_secret_basic", "client_secret_post", false},
		{"post client using HTTP Basic", "client_secret_post", "client_secret_basic", false},
		{"credentials presented twice", "client_secret_basic", AuthViaMultiple, false},
		{"basic client presenting nothing", "client_secret_basic", "none", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := confidentialClient(secret)
			client.TokenEndpointAuthMethod = tc.registered

			store := &fakeStore{
				codeOK: true, code: redeemableCode(t, verifier, registeredRedirect, registeredClientID),
				clientOK: true, client: client,
			}
			_, err := NewService(store).Exchange(context.Background(), TokenRequest{
				GrantType: "authorization_code", Code: "c", RedirectURI: registeredRedirect,
				ClientID: registeredClientID, ClientSecret: secret,
				CodeVerifier: verifier, ClientAuthVia: tc.presented,
			})

			if tc.wantOK {
				if err != nil {
					t.Fatalf("a client using its OWN registered method was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("a client registered %s authenticated via %s; the stored "+
					"method governs nothing", tc.registered, tc.presented)
			}
			if store.consumeCalls != 0 {
				t.Error("the code was consumed despite client authentication failing")
			}
		})
	}
}

// A public client must not be able to present a secret at all. 'none' means
// there is no stored hash to compare against, so accepting one would be
// accepting a credential that nothing verifies.
func TestExchange_PublicClientPresentingASecretIsRefused(t *testing.T) {
	const verifier = "verifier-for-the-public-with-secret-0000000000"
	store := &fakeStore{
		codeOK: true, code: redeemableCode(t, verifier, registeredRedirect, registeredClientID),
		clientOK: true, client: liveClient(registeredRedirect), // method "none"
	}
	if _, err := NewService(store).Exchange(context.Background(), TokenRequest{
		GrantType: "authorization_code", Code: "c", RedirectURI: registeredRedirect,
		ClientID: registeredClientID, ClientSecret: "invented",
		CodeVerifier: verifier, ClientAuthVia: "client_secret_post",
	}); err == nil {
		t.Fatal("a client registered token_endpoint_auth_method=none presented a " +
			"secret and was accepted; there is no stored hash to compare it against")
	}
}

// The handler must DERIVE the transport, and must refuse a request carrying
// both. Driven through the real gin handler, because only the handler can see
// the transport at all.
func TestTokenHandler_RefusesCredentialsPresentedTwice(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &fakeStore{clientOK: true, client: liveClient(registeredRedirect)}
	r := gin.New()
	NewHandler(NewService(store)).RegisterPublic(r.Group("/api/v1"))

	form := strings.NewReader("grant_type=authorization_code&code=c" +
		"&redirect_uri=" + registeredRedirect +
		"&client_id=" + registeredClientID +
		"&client_secret=body-secret&code_verifier=v")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/mcp/token", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(registeredClientID, "header-secret")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Fatalf("a request presenting credentials via BOTH Basic and the body "+
			"succeeded; RFC 6749 2.3.1 forbids more than one method per request. "+
			"body=%s", w.Body.String())
	}
	var body oauthErrorDTO
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("not an OAuth error envelope: %s", w.Body.String())
	}
	if body.Err != "invalid_client" {
		t.Errorf("error = %q, want invalid_client (%s)", body.Err, w.Body.String())
	}
}
