// mcp_exchange_revoked_grant_integration_test.go: a code whose connection is
// revoked between consent and exchange is refused at the token endpoint with
// RFC 6749 invalid_grant, and the exchange leaves nothing behind.
//
// A code refused because the organisation's assistant is paused is left
// redeemable: once the pause is released the same code exchanges for a token,
// exactly once.
//
// Driven the way TestMCPOAuthEndToEndThroughMountedRoutesAsAppRole drives the
// flow: through the mounted routes, to the real Service and Repo, as wpmgr_app.
// No GUC is set by hand and the flow opens no connection of its own. The row
// counts read through pool.InTenantTx, the same transaction shape the redeem
// runs in.
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

const xrvRedirectURI = "https://claude.ai/api/mcp/auth_callback"

// TestMCPExchangeRefusesARevokedGrantAsAppRole: approve, revoke, exchange.
// The exchange answers 400 invalid_grant, no token row exists for the grant,
// and the code is left unconsumed. The control, approve and exchange with no
// revoke, issues a token through the identical path.
func TestMCPExchangeRefusesARevokedGrantAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	tenantID := seedTenant(t, pool, "mcp-xrv-"+uuid.NewString()[:8])
	userID := seedUserRow(t, pool, "mcp-xrv-"+uuid.NewString()[:8]+"@example.test")

	// The role is asserted inside the transaction shape the redeem and the
	// counts below use. SUPERUSER or BYPASSRLS would make every count vacuous.
	if err := pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (redeem path)")
		return nil
	}); err != nil {
		t.Fatalf("open tenant tx: %v", err)
	}

	svc := auditedMCPService(pool, mcp.NewRepo(pool))
	operator := domain.Principal{
		Type: domain.PrincipalUser, UserID: userID, TenantID: tenantID,
		Role: "admin", Scope: domain.ScopeOrg,
	}
	eng := mountLikeProduction(t, svc, operator)
	clientID := xrvRegisterPublicClient(t, eng)

	// -----------------------------------------------------------------------
	// CONTROL: no revoke. The same client, the same routes, the same counts.
	// It proves the harness issues a token and that the count can see one, so
	// the zero below is the refusal's doing.
	// -----------------------------------------------------------------------
	ctlVerifier := "xrv-control-" + uuid.NewString() + uuid.NewString()
	ctlGrant, ctlCode := xrvApprove(t, eng, clientID, ctlVerifier, "xrv-control")

	ctl := xrvExchange(t, eng, clientID, ctlCode, ctlVerifier)
	if ctl.Code != http.StatusOK {
		t.Fatalf("CONTROL: the exchange answered %d, want 200; body: %s", ctl.Code, ctl.Body.String())
	}
	var ctlTok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(ctl.Body.Bytes(), &ctlTok); err != nil || ctlTok.AccessToken == "" {
		t.Fatalf("CONTROL: no access_token in %s (%v)", ctl.Body.String(), err)
	}
	if n := xrvCountGrantTokens(t, pool, tenantID, ctlGrant); n != 1 {
		t.Fatalf("CONTROL: %d token rows for grant %s, want 1", n, ctlGrant)
	}
	if !xrvCodeConsumed(t, pool, tenantID, ctlGrant) {
		t.Fatal("CONTROL: the redeemed code is not consumed")
	}
	t.Logf("CONTROL ok: grant %s issued 1 token and its code is consumed", ctlGrant)

	// -----------------------------------------------------------------------
	// REVOKED BETWEEN CONSENT AND EXCHANGE.
	// -----------------------------------------------------------------------
	verifier := "xrv-revoked-" + uuid.NewString() + uuid.NewString()
	grantID, code := xrvApprove(t, eng, clientID, verifier, "xrv-revoked")

	out, err := svc.RevokeConnection(ctx, operator, grantID)
	if err != nil {
		t.Fatalf("RevokeConnection: %v", err)
	}
	if out.GrantsRevoked != 1 {
		t.Fatalf("RevokeConnection revoked %d grants, want 1; the refusal below "+
			"would not be attributable to the revoke", out.GrantsRevoked)
	}

	// Presented twice: the second shows the unconsumed code still cannot
	// redeem against a revoked grant.
	for attempt := 1; attempt <= 2; attempt++ {
		w := xrvExchange(t, eng, clientID, code, verifier)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("attempt %d: the exchange for a revoked grant answered %d, want 400; body: %s",
				attempt, w.Code, w.Body.String())
		}
		var body struct {
			Error       string `json:"error"`
			AccessToken string `json:"access_token"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("attempt %d: the refusal is not JSON: %v; body: %s", attempt, err, w.Body.String())
		}
		if body.Error != "invalid_grant" {
			t.Fatalf("attempt %d: error = %q, want invalid_grant; body: %s", attempt, body.Error, w.Body.String())
		}
		if body.AccessToken != "" {
			t.Fatalf("attempt %d: a refused exchange carried an access_token", attempt)
		}
		if n := xrvCountGrantTokens(t, pool, tenantID, grantID); n != 0 {
			t.Fatalf("attempt %d: %d token rows exist for the revoked grant %s, want 0",
				attempt, n, grantID)
		}
		if xrvCodeConsumed(t, pool, tenantID, grantID) {
			t.Fatalf("attempt %d: the refused exchange left the code consumed; the "+
				"redeem transaction must roll the consume back with the insert", attempt)
		}
		t.Logf("attempt %d ok: 400 invalid_grant, 0 token rows, code unconsumed", attempt)
	}
}

// TestMCPExchangeRetriesAfterPauseReleaseAsAppRole: approve, pause the
// organisation, exchange, release the pause, exchange the same code, exchange
// it once more.
//
// An organisation pause is the one reversible reason the token endpoint refuses
// a code, so it is where "a refused exchange consumes nothing" is something a
// client acts on. While the organisation is paused the exchange answers 400
// invalid_grant with no token row and the code unconsumed. After the release the
// SAME code exchanges for a token. A further exchange of that code is refused
// and the one token row is unchanged: the code is spent exactly once. The
// control, an exchange with no pause, issues a token through the identical path.
func TestMCPExchangeRetriesAfterPauseReleaseAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	tenantID := seedTenant(t, pool, "mcp-xrp-"+uuid.NewString()[:8])
	userID := seedUserRow(t, pool, "mcp-xrp-"+uuid.NewString()[:8]+"@example.test")

	// The role is asserted inside the transaction shape the redeem and the
	// counts below use. SUPERUSER or BYPASSRLS would make every count vacuous.
	if err := pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (redeem path)")
		return nil
	}); err != nil {
		t.Fatalf("open tenant tx: %v", err)
	}

	svc := auditedMCPService(pool, mcp.NewRepo(pool))
	operator := domain.Principal{
		Type: domain.PrincipalUser, UserID: userID, TenantID: tenantID,
		Role: "admin", Scope: domain.ScopeOrg,
	}
	eng := mountLikeProduction(t, svc, operator)
	clientID := xrvRegisterPublicClient(t, eng)

	// -----------------------------------------------------------------------
	// CONTROL: no pause. It proves the harness issues a token and that the
	// count can see one, so the zero below is the refusal's doing.
	// -----------------------------------------------------------------------
	ctlVerifier := "xrp-control-" + uuid.NewString() + uuid.NewString()
	ctlGrant, ctlCode := xrvApprove(t, eng, clientID, ctlVerifier, "xrp-control")

	ctl := xrvExchange(t, eng, clientID, ctlCode, ctlVerifier)
	if ctl.Code != http.StatusOK {
		t.Fatalf("CONTROL: the exchange answered %d, want 200; body: %s", ctl.Code, ctl.Body.String())
	}
	var ctlTok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(ctl.Body.Bytes(), &ctlTok); err != nil || ctlTok.AccessToken == "" {
		t.Fatalf("CONTROL: no access_token in %s (%v)", ctl.Body.String(), err)
	}
	if n := xrvCountGrantTokens(t, pool, tenantID, ctlGrant); n != 1 {
		t.Fatalf("CONTROL: %d token rows for grant %s, want 1", n, ctlGrant)
	}
	if !xrvCodeConsumed(t, pool, tenantID, ctlGrant) {
		t.Fatal("CONTROL: the redeemed code is not consumed")
	}
	t.Logf("CONTROL ok: grant %s issued 1 token and its code is consumed", ctlGrant)

	// -----------------------------------------------------------------------
	// PAUSED BETWEEN CONSENT AND EXCHANGE.
	// -----------------------------------------------------------------------
	verifier := "xrp-paused-" + uuid.NewString() + uuid.NewString()
	grantID, code := xrvApprove(t, eng, clientID, verifier, "xrp-paused")

	m130Engage(t, pool, tenantID, "xrp integration proof: pause between consent and exchange")
	if !m130State(t, pool, tenantID).AssistantPausedAt.Valid {
		t.Fatal("assistant_paused_at is NULL after the pause; the refusal below " +
			"would not be attributable to it")
	}
	t.Logf("pause engaged: grant %s holds an issued, unredeemed code", grantID)

	xrvAssertInvalidGrant(t, "the exchange while paused",
		xrvExchange(t, eng, clientID, code, verifier))
	if n := xrvCountGrantTokens(t, pool, tenantID, grantID); n != 0 {
		t.Fatalf("the exchange while paused left %d token rows for grant %s, want 0", n, grantID)
	}
	// The stored state and what the client sees next are two views of one
	// contract: a refusal leaves the code redeemable. This check does not end
	// the test, so a failure here is reported together with the retry below.
	if xrvCodeConsumed(t, pool, tenantID, grantID) {
		t.Errorf("the exchange while paused left the code consumed; the redeem " +
			"transaction must roll the consume back with the insert")
	}
	t.Log("exchange while paused ok: 400 invalid_grant, 0 token rows, code unconsumed")

	// -----------------------------------------------------------------------
	// RELEASED: the SAME code, the SAME verifier.
	// -----------------------------------------------------------------------
	m130Release(t, pool, tenantID)
	if m130State(t, pool, tenantID).AssistantPausedAt.Valid {
		t.Fatal("assistant_paused_at is still set after the release; the exchange " +
			"below would not be testing a released organisation")
	}
	t.Log("pause released: assistant_paused_at is NULL")

	retry := xrvExchange(t, eng, clientID, code, verifier)
	if retry.Code != http.StatusOK {
		t.Fatalf("the same code after the release answered %d, want 200; a refusal "+
			"that spends the code strands a client that did nothing wrong; body: %s",
			retry.Code, retry.Body.String())
	}
	var retryTok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(retry.Body.Bytes(), &retryTok); err != nil || retryTok.AccessToken == "" {
		t.Fatalf("the same code after the release: no access_token in %s (%v)", retry.Body.String(), err)
	}
	if n := xrvCountGrantTokens(t, pool, tenantID, grantID); n != 1 {
		t.Fatalf("the same code after the release left %d token rows for grant %s, want 1", n, grantID)
	}
	if !xrvCodeConsumed(t, pool, tenantID, grantID) {
		t.Fatal("the same code after the release issued a token but is not consumed")
	}
	t.Log("same code after release ok: 200, 1 token row, code consumed")

	// -----------------------------------------------------------------------
	// SPENT: a third presentation of the code.
	// -----------------------------------------------------------------------
	xrvAssertInvalidGrant(t, "the exchange of the redeemed code",
		xrvExchange(t, eng, clientID, code, verifier))
	if n := xrvCountGrantTokens(t, pool, tenantID, grantID); n != 1 {
		t.Fatalf("the exchange of the redeemed code left %d token rows for grant %s, want 1", n, grantID)
	}
	if !xrvCodeConsumed(t, pool, tenantID, grantID) {
		t.Fatal("the code is no longer consumed after a further exchange of it")
	}
	t.Log("exchange of the redeemed code ok: 400 invalid_grant, still 1 token row, code still consumed")
}

// xrvRegisterPublicClient registers a token_endpoint_auth_method=none client
// through the unauthenticated route.
func xrvRegisterPublicClient(t *testing.T, eng *gin.Engine) string {
	t.Helper()
	var reg struct {
		ClientID string `json:"client_id"`
	}
	code := mcpDoJSON(t, eng, http.MethodPost, "/api/v1/oauth/mcp/register", map[string]any{
		"redirect_uris":              []string{xrvRedirectURI},
		"client_name":                "xrv client",
		"token_endpoint_auth_method": "none",
	}, nil, &reg)
	if code != http.StatusCreated || reg.ClientID == "" {
		t.Fatalf("register answered %d with client_id %q, want 201 and an id", code, reg.ClientID)
	}
	return reg.ClientID
}

// xrvApprove runs authorize then consent for one PKCE verifier and returns the
// new grant's id and its authorization code.
func xrvApprove(t *testing.T, eng *gin.Engine, clientID, verifier, state string) (uuid.UUID, string) {
	t.Helper()
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", xrvRedirectURI)
	q.Set("scope", string(mcp.ScopeRead))
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	var screen struct {
		ConsentTicket string `json:"consent_ticket"`
	}
	if code := mcpDoJSON(t, eng, http.MethodGet,
		"/api/v1/oauth/mcp/authorize?"+q.Encode(), nil, nil, &screen); code != http.StatusOK {
		t.Fatalf("authorize answered %d, want 200", code)
	}
	if screen.ConsentTicket == "" {
		t.Fatal("authorize returned no consent_ticket")
	}

	var approval struct {
		GrantID string `json:"grant_id"`
		Code    string `json:"code"`
	}
	if code := mcpDoJSON(t, eng, http.MethodPost, "/api/v1/oauth/mcp/consent", map[string]any{
		"client_id":             clientID,
		"redirect_uri":          xrvRedirectURI,
		"scopes":                []string{string(mcp.ScopeRead)},
		"state":                 state,
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"name":                  state + " connection",
		"site_scope_mode":       string(mcp.SiteScopeModeAll),
		"consent_ticket":        screen.ConsentTicket,
	}, nil, &approval); code != http.StatusOK {
		t.Fatalf("consent answered %d, want 200", code)
	}
	grantID, err := uuid.Parse(approval.GrantID)
	if err != nil || approval.Code == "" {
		t.Fatalf("consent returned grant_id %q and code %q", approval.GrantID, approval.Code)
	}
	return grantID, approval.Code
}

// xrvExchange posts the form-encoded token request every client library sends.
func xrvExchange(t *testing.T, eng *gin.Engine, clientID, code, verifier string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", xrvRedirectURI)
	form.Set("client_id", clientID)
	form.Set("code_verifier", verifier)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/mcp/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "203.0.113.7:5555"
	w := httptest.NewRecorder()
	eng.ServeHTTP(w, req)
	return w
}

// xrvAssertInvalidGrant fails unless w is the RFC 6749 invalid_grant refusal: a
// 400 whose JSON error is invalid_grant and which carries no access_token.
func xrvAssertInvalidGrant(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusBadRequest {
		t.Fatalf("%s answered %d, want 400; body: %s", what, w.Code, w.Body.String())
	}
	var body struct {
		Error       string `json:"error"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: the refusal is not JSON: %v; body: %s", what, err, w.Body.String())
	}
	if body.Error != "invalid_grant" {
		t.Fatalf("%s: error = %q, want invalid_grant; body: %s", what, body.Error, w.Body.String())
	}
	if body.AccessToken != "" {
		t.Fatalf("%s: a refused exchange carried an access_token", what)
	}
}

// xrvCountGrantTokens counts every token row of the grant, in any status,
// through InTenantTx as wpmgr_app.
func xrvCountGrantTokens(t *testing.T, pool *db.Pool, tenantID, grantID uuid.UUID) int64 {
	t.Helper()
	var n int64
	if err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM mcp_connection_tokens WHERE tenant_id = $1 AND grant_id = $2`,
			tenantID, grantID).Scan(&n)
	}); err != nil {
		t.Fatalf("count token rows for grant %s: %v", grantID, err)
	}
	return n
}

// xrvCodeConsumed reports whether the grant's single authorization code is
// consumed. It fails unless exactly one code row is visible, so a count that
// sees nothing cannot read as "unconsumed".
func xrvCodeConsumed(t *testing.T, pool *db.Pool, tenantID, grantID uuid.UUID) bool {
	t.Helper()
	var rows, consumed int64
	if err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*), count(*) FILTER (WHERE consumed_at IS NOT NULL)
			   FROM mcp_authorization_codes WHERE tenant_id = $1 AND grant_id = $2`,
			tenantID, grantID).Scan(&rows, &consumed)
	}); err != nil {
		t.Fatalf("read the code of grant %s: %v", grantID, err)
	}
	if rows != 1 {
		t.Fatalf("%d authorization code rows visible for grant %s, want 1", rows, grantID)
	}
	return consumed == 1
}
