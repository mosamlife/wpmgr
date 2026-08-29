package mcp

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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// fakeStore models the SQL contract of db/query/mcp_connections.sql, not a
// convenient approximation of it. The two behaviours that decide this slice are
// reproduced exactly:
//
//   - ConsumeAuthorizationCode is an atomic compare-and-set over
//     `consumed_at IS NULL AND expires_at > now()`, declared :one, so a LOSING
//     call returns pgx.ErrNoRows rather than a zero-value row. That is what
//     makes "no row" mean refuse.
//   - ReCheckAuthorization returns a ROW EVEN WHEN THE GRANT IS REVOKED, with
//     Authorized=false. Branching on row presence would therefore still honour
//     a revoked grant, which is the bug the test below exists to catch.
//
// It also records which calls happened, so a test can prove the consume ran at
// all -- an UPDATE that silently matched zero rows is invisible otherwise, and
// that invisibility is m124 obligation 1's entire hazard.
// ---------------------------------------------------------------------------

type fakeStore struct {
	client   sqlc.McpOauthClient
	clientOK bool

	// registerRows is what the :execrows INSERT reports. Defaults to 0, so a
	// test that means "registration succeeds" must SAY 1 -- the honest default,
	// since a fake that silently succeeds is what hid the last defect.
	registerRows int64
	// registerErr forces the write itself to fail (e.g. 42501).
	registerErr error
	// registerReadBackMissing writes the row but makes the follow-up
	// LookupClient find nothing: the insert reports success and the read-back
	// comes up empty. A broken invariant, not an empty result.
	registerReadBackMissing bool

	code     sqlc.GetMCPAuthorizationCodeByHashForLookupRow
	codeOK   bool
	consumed bool // the compare-and-set has already won once

	// raceLost models the TOCTOU window that the compare-and-set exists to
	// close: our lookup transaction committed while the code was still
	// redeemable, and ANOTHER transaction consumed it before our consume ran.
	// The lookup therefore reports a perfectly healthy, redeemable code and the
	// consume still matches zero rows.
	//
	// Without this the replay tests never reach the consume at all -- they are
	// refused by the earlier IsConsumed check and would pass even with the
	// ErrNoRows branch deleted, which is precisely what planting that defect
	// revealed.
	raceLost bool

	// tokenPersistErr makes the token INSERT fail inside the redeem
	// transaction. The consume rolls back with it, which is the property the
	// atomicity proof asserts.
	tokenPersistErr error

	recheck   sqlc.ReCheckMCPRequestAuthorizationInTenantTxRow
	recheckOK bool

	token   sqlc.GetMCPConnectionTokenByHashForLookupRow
	tokenOK bool

	scopeSites []uuid.UUID

	// S6b transport surface.
	//
	// identityErr forces the connect record to fail, which the transport must
	// surface as a refused session rather than swallow.
	identityErr error
	// identityCalls records every RecordClientIdentity argument set, so a test
	// can assert that an ABSENT protocol header was persisted as nil and not
	// coerced into a string.
	identityCalls []recordedIdentity

	// sites is what ListSitesForRead returns; sitesMore is its page-bound
	// overflow flag; sitesErr forces the read to fail.
	sites     []sqlc.ListSitesRow
	sitesMore bool
	sitesErr  error

	// call log
	consumeCalls int
	tokensMinted int
	calls        []string
}

func (f *fakeStore) note(s string) { f.calls = append(f.calls, s) }

// RegisterClient models the :execrows query: it reports ROWS WRITTEN and can
// never hand back the row, because the register GUC enables no SELECT policy
// and RETURNING raises 42501 against the real database.
//
// THE PREVIOUS FAKE RETURNED A FULLY POPULATED ROW, which is why a P1 that
// broke every single registration reached review with a green suite: the fake
// said yes to something Postgres refuses outright. Modelling the count is only
// half the repair -- registerRows and registerReadBackMissing exist so the
// FAILURE modes are reachable too, since a fake that models the new shape but
// not the new failures leaves exactly the same hole.
func (f *fakeStore) RegisterClient(_ context.Context, arg sqlc.RegisterMCPOAuthClientParams) (int64, error) {
	f.note("RegisterClient")
	if f.registerErr != nil {
		return 0, f.registerErr
	}
	// Record what was written so LookupClient can serve a faithful read-back.
	if !f.registerReadBackMissing {
		f.client = sqlc.McpOauthClient{
			ID:                      uuid.New(),
			ClientID:                arg.ClientID,
			ClientSecretHash:        arg.ClientSecretHash,
			TokenEndpointAuthMethod: arg.TokenEndpointAuthMethod,
			RedirectUris:            arg.RedirectUris,
			ClientName:              arg.ClientName,
			ClientUri:               arg.ClientUri,
		}
		f.clientOK = true
	}
	return f.registerRows, nil
}

func (f *fakeStore) LookupClient(_ context.Context, _ string) (sqlc.McpOauthClient, error) {
	f.note("LookupClient")
	if !f.clientOK {
		return sqlc.McpOauthClient{}, pgx.ErrNoRows
	}
	return f.client, nil
}

func (f *fakeStore) LookupAuthorizationCode(_ context.Context, _ string) (sqlc.GetMCPAuthorizationCodeByHashForLookupRow, error) {
	f.note("LookupAuthorizationCode")
	if !f.codeOK {
		return sqlc.GetMCPAuthorizationCodeByHashForLookupRow{}, pgx.ErrNoRows
	}
	row := f.code
	// The lookup reports current state, exactly as the generated columns do.
	// Under raceLost it reports the state as of OUR transaction: still
	// redeemable, because the winner had not committed yet.
	if f.raceLost {
		row.IsConsumed = false
		row.IsRedeemable = !row.IsExpired
		return row, nil
	}
	row.IsConsumed = f.consumed
	row.IsRedeemable = !f.consumed && !row.IsExpired
	return row, nil
}

// RedeemAuthorizationCode models ONE TRANSACTION: the compare-and-set and the
// token insert either both land or neither does.
//
// The rollback is what makes this fake honest. When tokenPersistErr is set the
// insert fails, so `consumed` is NOT flipped -- exactly as the real transaction
// would roll the UPDATE back. A fake that marked the code consumed and then
// returned the error would model two commits, which is the defect being fixed,
// and the test would pass against the broken code.
func (f *fakeStore) RedeemAuthorizationCode(_ context.Context, _, _ uuid.UUID, tok sqlc.CreateMCPConnectionTokenParams) (sqlc.McpConnectionToken, error) {
	f.note("RedeemAuthorizationCode")
	f.consumeCalls++

	// The compare-and-set runs first, inside the transaction.
	if f.raceLost || f.consumed {
		// `consumed_at IS NULL` matched nothing; :one turns that into ErrNoRows.
		return sqlc.McpConnectionToken{}, pgx.ErrNoRows
	}
	// Then the insert. If it fails the whole transaction rolls back, so the
	// consume never becomes visible and the code remains redeemable.
	if f.tokenPersistErr != nil {
		return sqlc.McpConnectionToken{}, f.tokenPersistErr
	}

	f.consumed = true
	f.tokensMinted++
	return sqlc.McpConnectionToken{ID: uuid.New(), TenantID: tok.TenantID, GrantID: tok.GrantID}, nil
}

func (f *fakeStore) CreateGrantWithCode(_ context.Context, g sqlc.CreateMCPGrantParams, mk func(uuid.UUID) sqlc.CreateMCPAuthorizationCodeParams) (sqlc.McpGrant, sqlc.McpAuthorizationCode, error) {
	f.note("CreateGrantWithCode")
	id := uuid.New()
	cp := mk(id)
	return sqlc.McpGrant{ID: id, TenantID: g.TenantID, Name: g.Name},
		sqlc.McpAuthorizationCode{ID: uuid.New(), TenantID: cp.TenantID}, nil
}

func (f *fakeStore) LookupConnectionToken(_ context.Context, _ string) (sqlc.GetMCPConnectionTokenByHashForLookupRow, error) {
	f.note("LookupConnectionToken")
	if !f.tokenOK {
		return sqlc.GetMCPConnectionTokenByHashForLookupRow{}, pgx.ErrNoRows
	}
	return f.token, nil
}

func (f *fakeStore) ReCheckAuthorization(_ context.Context, _, _ uuid.UUID) (sqlc.ReCheckMCPRequestAuthorizationInTenantTxRow, error) {
	f.note("ReCheckAuthorization")
	if !f.recheckOK {
		return sqlc.ReCheckMCPRequestAuthorizationInTenantTxRow{}, pgx.ErrNoRows
	}
	return f.recheck, nil
}

func (f *fakeStore) ResolveScopeSites(_ context.Context, _ uuid.UUID, _ string, _, _ []uuid.UUID) ([]uuid.UUID, error) {
	f.note("ResolveScopeSites")
	return f.scopeSites, nil
}

// recordedIdentity is one captured RecordClientIdentity call. ProtocolVersion
// is a *string so a test can tell "sent no header" (nil) from "sent the empty
// string", which is the distinction the column exists to preserve.
type recordedIdentity struct {
	Name            string
	Version         string
	ProtocolVersion *string
}

func (f *fakeStore) RecordClientIdentity(
	_ context.Context, _, _ uuid.UUID, name, version string, protocolVersion *string,
) error {
	f.note("RecordClientIdentity")
	f.identityCalls = append(f.identityCalls, recordedIdentity{
		Name: name, Version: version, ProtocolVersion: protocolVersion,
	})
	return f.identityErr
}

func (f *fakeStore) ListSitesForRead(_ context.Context, _ uuid.UUID, limit int32) ([]sqlc.ListSitesRow, bool, error) {
	f.note("ListSitesForRead")
	if f.sitesErr != nil {
		return nil, false, f.sitesErr
	}
	// Model the real repo's limit+1 overflow: a fake that ignored the bound
	// would let a page-bound bug pass.
	if int32(len(f.sites)) > limit {
		return f.sites[:limit], true, nil
	}
	return f.sites, f.sitesMore, nil
}

// ---------------------------------------------------------------------------
// PROOF 1 -- THE EXIT GATE, END TO END THROUGH A REAL HANDLER.
//
// #571 proved the parser refuses. This proves the refusal survives the whole
// request path: routing, the authenticated principal, the service, and the
// RFC 6749 error envelope. A gate that holds in a unit test and is bypassed by
// the handler that mounts it guards nothing.
// ---------------------------------------------------------------------------

func newAuthorizeRouter(t *testing.T, store Store) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		ctx := domain.WithPrincipal(c.Request.Context(), domain.Principal{
			Type:     domain.PrincipalUser,
			UserID:   uuid.New(),
			TenantID: uuid.New(),
			Role:     "owner",
		})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	NewHandler(NewService(store)).Register(r.Group("/api/v1"))
	return r
}

func liveClient(redirect string) sqlc.McpOauthClient {
	name := "Claude Desktop"
	return sqlc.McpOauthClient{
		ID:                      uuid.New(),
		ClientID:                "client-abc",
		TokenEndpointAuthMethod: "none",
		RedirectUris:            []string{redirect},
		ClientName:              &name,
	}
}

func TestAuthorizeHandler_NoRecognisedScopeIsRefusedNotDefaulted(t *testing.T) {
	const redirect = "https://claude.ai/api/mcp/auth_callback"

	cases := []struct {
		name  string
		scope string
	}{
		{"scope parameter absent entirely", ""},
		{"scope blank", "   "},
		{"scope unrecognised", "fleet:admin"},
		{"scope is a write the surface never grants", "mcp:write"},
		{"scope case-mismatched", "MCP:READ"},
		{"recognised scope mixed with an unrecognised one", "mcp:read fleet:destroy"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{client: liveClient(redirect), clientOK: true}
			r := newAuthorizeRouter(t, store)

			q := url.Values{}
			q.Set("response_type", "code")
			q.Set("client_id", "client-abc")
			q.Set("redirect_uri", redirect)
			q.Set("code_challenge", "abc123challenge")
			q.Set("code_challenge_method", "S256")
			if tc.scope != "" {
				q.Set("scope", tc.scope)
			}

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/oauth/mcp/authorize?"+q.Encode(), nil)
			r.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s\n"+
					"a client naming no recognised scope MUST be refused by the "+
					"handler, not handed a default", w.Code, w.Body.String())
			}

			var body oauthErrorDTO
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not the RFC 6749 error envelope: %v (%s)", err, w.Body.String())
			}
			if body.Err != "invalid_scope" {
				t.Errorf("error = %q, want %q (body %s)", body.Err, "invalid_scope", w.Body.String())
			}

			// The refusal must not have leaked a consent screen.
			if strings.Contains(w.Body.String(), "client_name_unverified") {
				t.Error("a refused authorize request still rendered consent context")
			}
		})
	}
}

// The gate must not over-fire, and the consent screen must present the
// registration-supplied identity as UNVERIFIED (m124 obligation 7).
func TestAuthorizeHandler_ValidRequestReturnsUnverifiedConsentContext(t *testing.T) {
	const redirect = "https://claude.ai/api/mcp/auth_callback"
	store := &fakeStore{client: liveClient(redirect), clientOK: true}
	r := newAuthorizeRouter(t, store)

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", "client-abc")
	q.Set("redirect_uri", redirect)
	q.Set("scope", "mcp:read")
	q.Set("code_challenge", "abc123challenge")
	q.Set("code_challenge_method", "S256")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/oauth/mcp/authorize?"+q.Encode(), nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	var body consentResponseDTO
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.IdentityVerified {
		t.Error("consent context claims the client identity is verified; registration " +
			"is unauthenticated and client_name is attacker-controlled")
	}
	if body.ClientNameUnverified != "Claude Desktop" {
		t.Errorf("client_name_unverified = %q", body.ClientNameUnverified)
	}
	if body.RedirectHost != "claude.ai" {
		t.Errorf("redirect_host = %q, want claude.ai; the host is the part a human can judge",
			body.RedirectHost)
	}
	if len(body.Scopes) != 1 || body.Scopes[0] != "mcp:read" {
		t.Errorf("scopes = %v", body.Scopes)
	}
}

// A redirect_uri that is not an EXACT match must be refused. Every one of these
// passes a prefix, suffix or host-only comparison, and each is a redirector.
func TestAuthorize_RedirectURIIsExactMatchedOnly(t *testing.T) {
	const registered = "https://claude.ai/api/mcp/auth_callback"
	attacker := []string{
		"https://claude.ai/api/mcp/auth_callback/../../evil",
		"https://claude.ai/api/mcp/auth_callback.evil.com",
		"https://claude.ai/api/mcp/auth_callback?next=https://evil.com",
		"https://claude.ai.evil.com/api/mcp/auth_callback",
		"https://evil.com/api/mcp/auth_callback",
		"https://claude.ai/api/mcp/auth_callbac",
		"http://claude.ai/api/mcp/auth_callback",
		"",
	}
	svc := NewService(&fakeStore{client: liveClient(registered), clientOK: true})

	for _, bad := range attacker {
		t.Run(bad, func(t *testing.T) {
			_, err := svc.Authorize(context.Background(), AuthorizeRequest{
				ResponseType: "code", ClientID: "client-abc", RedirectURI: bad,
				Scope: "mcp:read", CodeChallenge: "c", CodeChallengeMethod: "S256",
			})
			if err == nil {
				t.Fatalf("redirect_uri %q was accepted; only an exact match may be", bad)
			}
		})
	}
}

// PKCE is mandatory and S256 only. A missing method must not decay to 'plain'.
func TestAuthorize_PKCEIsMandatoryAndS256Only(t *testing.T) {
	const redirect = "https://claude.ai/cb"
	svc := NewService(&fakeStore{client: liveClient(redirect), clientOK: true})

	cases := []struct{ name, challenge, method string }{
		{"no challenge at all", "", "S256"},
		{"challenge but no method", "abc", ""},
		{"plain is not supported", "abc", "plain"},
		{"unknown method", "abc", "S512"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Authorize(context.Background(), AuthorizeRequest{
				ResponseType: "code", ClientID: "client-abc", RedirectURI: redirect,
				Scope: "mcp:read", CodeChallenge: tc.challenge, CodeChallengeMethod: tc.method,
			}); err == nil {
				t.Fatal("accepted a request without a valid S256 PKCE challenge")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PROOF 2 -- A CONSUMED AUTHORIZATION CODE CANNOT BE REPLAYED, AND THE CONSUME
// ACTUALLY WROTE.
//
// m124 obligation 1. The half that fails silently is the consume matching zero
// rows and raising no error; the test therefore asserts BOTH that the second
// exchange is refused AND that the consume was really attempted and really won
// exactly once. A version that never called consume at all would pass a
// refusal-only assertion while leaving the code replayable in production.
// ---------------------------------------------------------------------------

func redeemableCode(t *testing.T, verifier, redirect, clientID string) sqlc.GetMCPAuthorizationCodeByHashForLookupRow {
	t.Helper()
	sum := sha256.Sum256([]byte(verifier))
	return sqlc.GetMCPAuthorizationCodeByHashForLookupRow{
		ID:                  uuid.New(),
		TenantID:            uuid.New(),
		GrantID:             uuid.New(),
		ClientID:            clientID,
		CodeChallenge:       base64.RawURLEncoding.EncodeToString(sum[:]),
		CodeChallengeMethod: "S256",
		RedirectUri:         redirect,
		ExpiresAt:           time.Now().Add(5 * time.Minute),
		IsRedeemable:        true,
	}
}

func TestExchange_ConsumedCodeCannotBeReplayed(t *testing.T) {
	const (
		verifier = "a-high-entropy-code-verifier-value-0123456789"
		redirect = "https://claude.ai/cb"
		clientID = "client-abc"
	)
	store := &fakeStore{
		codeOK: true, code: redeemableCode(t, verifier, redirect, clientID),
		clientOK: true, client: liveClient(redirect),
	}
	svc := NewService(store)

	req := TokenRequest{
		GrantType: "authorization_code", Code: "the-plaintext-code",
		RedirectURI: redirect, ClientID: clientID, CodeVerifier: verifier,
	}

	// First exchange: succeeds and mints exactly one token.
	first, err := svc.Exchange(context.Background(), req)
	if err != nil {
		t.Fatalf("first exchange failed: %v", err)
	}
	if first.AccessToken == "" {
		t.Fatal("first exchange returned an empty access token")
	}

	// THE CONSUME MUST HAVE ACTUALLY WRITTEN. If this is 0 the code was never
	// consumed and single-use is a fiction, however the refusal below behaves.
	if store.consumeCalls != 1 {
		t.Fatalf("consume was attempted %d times on the first exchange, want 1", store.consumeCalls)
	}
	if !store.consumed {
		t.Fatal("the compare-and-set did not mark the code consumed; the UPDATE " +
			"matched zero rows and the code is still replayable")
	}

	// Second exchange of the SAME code: refused.
	second, err := svc.Exchange(context.Background(), req)
	if err == nil {
		t.Fatalf("the code was redeemed a second time and returned %+v; "+
			"single-use silently became multi-use", second)
	}
	if second.AccessToken != "" {
		t.Fatal("a refused exchange still returned an access token")
	}
	if store.tokensMinted != 1 {
		t.Fatalf("%d connection tokens were minted across two exchanges of one code, want 1",
			store.tokensMinted)
	}
}

// The compare-and-set is the guarantee, not the lookup. This drives the branch
// a racing exchange takes: the lookup still reports the code redeemable (it was
// when the transaction started) and the consume returns pgx.ErrNoRows. Reading
// that as "already fine" is how the loser of a race also gets a token.
func TestExchange_LosingCompareAndSetIsRefusedNotShrugged(t *testing.T) {
	const (
		verifier = "verifier-for-the-racing-exchange-000000000000"
		redirect = "https://claude.ai/cb"
		clientID = "client-abc"
	)
	// ANOTHER transaction consumed the code between our lookup and our consume.
	// Our lookup therefore sees a healthy, redeemable code -- every check
	// before the compare-and-set passes -- and only the UPDATE matches nothing.
	// This is the ONLY path that exercises the ErrNoRows branch, which is why
	// raceLost exists: the plain replay case is refused earlier, by IsConsumed.
	store := &fakeStore{
		codeOK:   true,
		code:     redeemableCode(t, verifier, redirect, clientID),
		raceLost: true,
		clientOK: true, client: liveClient(redirect),
	}
	svc := NewService(store)

	got, err := svc.Exchange(context.Background(), TokenRequest{
		GrantType: "authorization_code", Code: "c", RedirectURI: redirect,
		ClientID: clientID, CodeVerifier: verifier,
	})
	if err == nil {
		t.Fatalf("the losing exchange was granted %+v; a zero-row consume means "+
			"REFUSE, never 'already fine'", got)
	}
	if got.AccessToken != "" {
		t.Fatal("the losing exchange returned an access token")
	}

	// It must have genuinely attempted the compare-and-set -- otherwise this
	// test is passing on an earlier check and proves nothing about the consume.
	if store.consumeCalls != 1 {
		t.Fatalf("consume was attempted %d times, want 1; this test must reach the "+
			"compare-and-set or it is not testing it", store.consumeCalls)
	}
	if store.tokensMinted != 0 {
		t.Fatalf("the losing exchange minted %d tokens, want 0", store.tokensMinted)
	}
}

// The code is bound to the client and the redirect it was issued for, and PKCE
// must actually verify. Each of these must refuse BEFORE any consume happens --
// a failed exchange must not burn the code, or an attacker can grief a real
// client by replaying garbage at the token endpoint.
func TestExchange_RefusesBeforeConsumingOnEveryBindingFailure(t *testing.T) {
	const (
		verifier = "the-real-verifier-value-aaaaaaaaaaaaaaaaaaaaa"
		redirect = "https://claude.ai/cb"
		clientID = "client-abc"
	)
	cases := []struct {
		name string
		req  TokenRequest
	}{
		{"wrong grant_type", TokenRequest{GrantType: "password", Code: "c", RedirectURI: redirect, ClientID: clientID, CodeVerifier: verifier}},
		{"no verifier", TokenRequest{GrantType: "authorization_code", Code: "c", RedirectURI: redirect, ClientID: clientID}},
		{"wrong verifier", TokenRequest{GrantType: "authorization_code", Code: "c", RedirectURI: redirect, ClientID: clientID, CodeVerifier: "not-the-verifier"}},
		{"wrong client", TokenRequest{GrantType: "authorization_code", Code: "c", RedirectURI: redirect, ClientID: "someone-else", CodeVerifier: verifier}},
		{"wrong redirect", TokenRequest{GrantType: "authorization_code", Code: "c", RedirectURI: "https://evil.com/cb", ClientID: clientID, CodeVerifier: verifier}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{
				codeOK: true, code: redeemableCode(t, verifier, redirect, clientID),
				clientOK: true, client: liveClient(redirect),
			}
			svc := NewService(store)
			if _, err := svc.Exchange(context.Background(), tc.req); err == nil {
				t.Fatal("exchange succeeded on a binding failure")
			}
			if store.consumeCalls != 0 {
				t.Errorf("the code was consumed (%d calls) despite the request being "+
					"refused; a bad request must not burn a legitimate code",
					store.consumeCalls)
			}
			if store.consumed {
				t.Error("the code was marked consumed by a refused exchange")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PROOF 3 -- REVOCATION TAKES EFFECT ON THE NEXT REQUEST, NOT AT TOKEN EXPIRY.
//
// m124 obligation 4. ReCheckAuthorization returns a ROW for a revoked grant,
// with Authorized=false, so a service branching on row presence still honours
// it. The token's own expiry is deliberately far in the future in every case
// here: if revocation only bit at expiry, these would pass.
// ---------------------------------------------------------------------------

func liveToken(tenantID uuid.UUID) sqlc.GetMCPConnectionTokenByHashForLookupRow {
	return sqlc.GetMCPConnectionTokenByHashForLookupRow{
		ID:        uuid.New(),
		TenantID:  tenantID,
		GrantID:   uuid.New(),
		Status:    "active",
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(90 * 24 * time.Hour), Valid: true},
		IsLive:    true,
	}
}

func TestAuthenticate_RevocationBitesOnTheNextRequest(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	tok := liveToken(tenantID)

	store := &fakeStore{
		tokenOK: true, token: tok,
		recheckOK: true,
		recheck: sqlc.ReCheckMCPRequestAuthorizationInTenantTxRow{
			GrantID: tok.GrantID, GrantStatus: "active",
			SiteScopeMode: "all", TokenID: tok.ID, TokenStatus: "active",
			TokenExpiresAt: tok.ExpiresAt,
			Authorized:     true,
		},
		scopeSites: []uuid.UUID{siteID},
	}
	svc := NewService(store)

	// Request 1: authorized.
	got, err := svc.Authenticate(context.Background(), "the-bearer-token")
	if err != nil {
		t.Fatalf("a live connection was refused: %v", err)
	}
	if !got.Sites.Allows(siteID) {
		t.Fatal("the resolved site set does not contain the granted site")
	}

	// The operator revokes. The TOKEN IS UNCHANGED and still unexpired -- only
	// the grant's status moved. This is exactly the state where checking
	// expiry, or checking that a row came back, still says "yes".
	store.recheck.GrantStatus = "revoked"
	store.recheck.Authorized = false

	// Request 2: must be refused, on this request, not at expiry.
	if _, err := svc.Authenticate(context.Background(), "the-bearer-token"); err == nil {
		t.Fatal("a REVOKED grant was still honoured on the next request; revocation " +
			"must not wait for token expiry")
	}

	// And prove the re-check actually ran on every request rather than being
	// cached from the first.
	n := 0
	for _, c := range store.calls {
		if c == "ReCheckAuthorization" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("ReCheckAuthorization ran %d times across 2 requests, want 2; "+
			"the grant must be re-checked on EVERY request", n)
	}
}

// Authorized=false must refuse whatever else the row says. This is the
// branch-on-presence bug stated directly: every field below looks healthy.
func TestAuthenticate_BranchesOnAuthorizedNotOnRowPresence(t *testing.T) {
	tenantID := uuid.New()
	tok := liveToken(tenantID)
	store := &fakeStore{
		tokenOK: true, token: tok,
		recheckOK: true,
		recheck: sqlc.ReCheckMCPRequestAuthorizationInTenantTxRow{
			GrantID:        tok.GrantID,
			GrantName:      "looks entirely fine",
			GrantStatus:    "active",
			SiteScopeMode:  "all",
			TokenID:        tok.ID,
			TokenStatus:    "active",
			TokenExpiresAt: tok.ExpiresAt,
			Authorized:     false, // the only field that matters
		},
		scopeSites: []uuid.UUID{uuid.New()},
	}
	if _, err := NewService(store).Authenticate(context.Background(), "tok"); err == nil {
		t.Fatal("a row with Authorized=false was honoured; the service is branching " +
			"on row presence")
	}
}

// An empty resolved scope must NOT widen to every site (m124 obligation 2).
// This is the tag-matches-no-site case, arriving through the real service path.
func TestAuthenticate_EmptyResolvedScopeGrantsNoSites(t *testing.T) {
	tenantID := uuid.New()
	tok := liveToken(tenantID)
	store := &fakeStore{
		tokenOK: true, token: tok,
		recheckOK: true,
		recheck: sqlc.ReCheckMCPRequestAuthorizationInTenantTxRow{
			GrantID: tok.GrantID, GrantStatus: "active",
			SiteScopeMode: "tags", ScopeTagIds: []uuid.UUID{uuid.New()},
			TokenID: tok.ID, TokenStatus: "active",
			TokenExpiresAt: tok.ExpiresAt, Authorized: true,
		},
		scopeSites: nil, // the tag matched no site
	}
	got, err := NewService(store).Authenticate(context.Background(), "tok")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if !got.Sites.IsEmpty() {
		t.Fatal("an unresolved tag scope did not produce an empty site set")
	}
	if got.Sites.Allows(uuid.New()) {
		t.Fatal("an EMPTY resolved scope allowed a site; empty must mean no sites, never all")
	}
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

func TestRegister_RefusesRedirectURIsThatCannotBeTrusted(t *testing.T) {
	svc := NewService(&fakeStore{})
	cases := []struct{ name, uri string }{
		{"no redirect at all", ""},
		{"relative", "/callback"},
		{"plain http on a public host", "http://evil.com/cb"},
		{"carries a fragment", "https://claude.ai/cb#tok"},
		{"not a URI", "://::"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uris := []string{tc.uri}
			if tc.uri == "" {
				uris = nil
			}
			if _, err := svc.Register(context.Background(), RegistrationRequest{RedirectURIs: uris}); err == nil {
				t.Fatalf("registration accepted redirect_uri %q", tc.uri)
			}
		})
	}
}

func TestRegister_PublicClientGetsNoSecretAndConfidentialDoes(t *testing.T) {
	svc := NewService(&fakeStore{registerRows: 1})

	pub, err := svc.Register(context.Background(), RegistrationRequest{
		RedirectURIs: []string{"https://claude.ai/cb"}, TokenEndpointAuthMethod: "none",
	})
	if err != nil {
		t.Fatalf("register public: %v", err)
	}
	if pub.ClientSecret != "" {
		t.Error("a 'none' client was issued a secret; 'none' means there is no secret")
	}

	conf, err := svc.Register(context.Background(), RegistrationRequest{
		RedirectURIs: []string{"https://claude.ai/cb"}, TokenEndpointAuthMethod: "client_secret_basic",
	})
	if err != nil {
		t.Fatalf("register confidential: %v", err)
	}
	if conf.ClientSecret == "" {
		t.Error("a confidential client got no secret; the secret comparison would " +
			"then have nothing to compare against")
	}
	if conf.ClientID == pub.ClientID {
		t.Error("two registrations shared a client_id")
	}
}

// Loopback http is the native-client case RFC 8252 requires and must still work.
func TestRegister_AllowsLoopbackHTTPForNativeClients(t *testing.T) {
	svc := NewService(&fakeStore{registerRows: 1})
	for _, uri := range []string{"http://localhost:8765/cb", "http://127.0.0.1:1455/cb"} {
		if _, err := svc.Register(context.Background(), RegistrationRequest{
			RedirectURIs: []string{uri}, TokenEndpointAuthMethod: "none",
		}); err != nil {
			t.Errorf("registration refused legitimate native redirect %q: %v", uri, err)
		}
	}
}
