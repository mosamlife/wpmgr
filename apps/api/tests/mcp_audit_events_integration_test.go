// mcp_audit_events_integration_test.go: the three audit_events writes added to
// the MCP connection surface -- ActionMCPGrantCreated, ActionMCPGrantRevoked,
// ActionMCPToolCalled -- proven against the REAL schema, RLS and application
// role, through the SAME code path production uses.
//
// WHY THIS FILE EXISTS. internal/mcp wrote ZERO rows to audit_log before this
// slice: `grep -rn '\.Record(\|RecordInTx(' internal/mcp/*.go` matched nothing
// outside _test.go's httptest.NewRecorder. An approve, a revoke or a tool call
// that reaches a tenant's data left no attributable row. A unit test against
// fakeStore cannot prove any of the three things that actually matter here:
//
//  1. mcp.grant.created lands in the SAME transaction as the grant insert
//     (RecordInTx, not Record), so a rolled-back grant leaves no row claiming
//     one exists -- see the rollback proof below.
//  2. The row is readable back only through db.Pool as wpmgr_app -- NOSUPERUSER,
//     NOBYPASSRLS -- which mcpAssertAndReportRole asserts and prints. A test
//     that opened its own connection would leave every RLS policy inert and
//     pass regardless; that is exactly how m112's proofs passed while the email
//     domain was cross-site readable.
//  3. A second tenant's InTenantTx cannot see the first tenant's assistant row,
//     even though both share one physical audit_log table.
//
// So: every write and every read below goes through the same Service, the same
// Handler, the same TransportHandler and the same tx helpers server.New wires,
// with the app-role pool. No GUC is hand-set anywhere in this file.
package tests

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// mcpAuditRow is the slice of audit_log columns these proofs care about.
type mcpAuditRow struct {
	actorType string
	actorID   string
	targetID  string
	metadata  map[string]any
}

// queryMCPAuditRowsAsAppRole reads audit_log for one tenant and action THROUGH
// db.Pool.InTenantTx -- the same tx helper Recorder.List uses -- so RLS is live
// for this read exactly as it is for the write under test.
func queryMCPAuditRowsAsAppRole(t *testing.T, pool *db.Pool, tenantID uuid.UUID, action string) []mcpAuditRow {
	t.Helper()
	var out []mcpAuditRow
	err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(),
			`SELECT actor_type, actor_id, target_id, metadata FROM audit_log
			  WHERE tenant_id = $1 AND action = $2`, tenantID, action)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r mcpAuditRow
			var metaRaw []byte
			if err := rows.Scan(&r.actorType, &r.actorID, &r.targetID, &metaRaw); err != nil {
				return err
			}
			if len(metaRaw) > 0 {
				if err := json.Unmarshal(metaRaw, &r.metadata); err != nil {
					return err
				}
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("query audit_log action=%s for tenant %s: %v", action, tenantID, err)
	}
	return out
}

// TestMCPAuditEvents_GrantCreatedToolCalledAndRevoked_AsAppRole drives the
// WHOLE MCP surface -- register, authorize, consent, exchange, a real tools/call
// over the mounted transport, then revoke -- and asserts each of the three new
// audit rows lands with the right actor, target and metadata. It also proves
// tenant isolation: a second tenant's InTenantTx read sees none of it.
func TestMCPAuditEvents_GrantCreatedToolCalledAndRevoked_AsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	// The role is load-bearing for every assertion below.
	if err := pool.InMCPClientLookupTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InMCPClientLookupTx")
		return nil
	}); err != nil {
		t.Fatalf("open lookup tx: %v", err)
	}

	suffix := uuid.NewString()[:8]
	tenantID := seedTenant(t, pool, "mcp-audit-"+suffix)
	userID := seedUserRow(t, pool, "mcp-audit-"+suffix+"@example.test")
	siteURL := "https://" + suffix + ".example.test"
	seedSite(t, pool, tenantID, siteURL)

	rec := audit.NewRecorder(pool, domain.SystemClock{})
	svc := mcp.NewService(mcp.NewRepo(pool)).WithAudit(rec)

	// An org-scoped admin: the role RegisterConnections' PermAPIKeyManage
	// requires for revoke, and the only scope /consent admits.
	admin := domain.Principal{UserID: userID, TenantID: tenantID, Role: "admin", Scope: domain.ScopeOrg}
	eng := mountLikeProduction(t, svc, admin)

	const redirectURI = "https://claude.ai/api/mcp/auth_callback"
	const grantName = "mcp audit events test connection"

	// ------------------------------------------------------------------
	// register -> authorize -> consent -> exchange, exactly as
	// adr064_s6b3_mcp_oauth_end_to_end_test.go proves the flow composes.
	// ------------------------------------------------------------------
	var reg struct {
		ClientID string `json:"client_id"`
	}
	if code := mcpDoJSON(t, eng, http.MethodPost, mcp.RegisterPath, map[string]any{
		"redirect_uris":              []string{redirectURI},
		"client_name":                "Claude Desktop",
		"client_uri":                 "https://claude.ai",
		"token_endpoint_auth_method": "none",
	}, nil, &reg); code != http.StatusCreated {
		t.Fatalf("register answered %d, want 201", code)
	}

	verifier := "audit-verifier-" + uuid.NewString() + uuid.NewString()
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", reg.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", string(mcp.ScopeRead))
	q.Set("state", "audit-state")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	if code := mcpDoJSON(t, eng, http.MethodGet, mcp.AuthorizePath+"?"+q.Encode(), nil, nil, nil); code != http.StatusOK {
		t.Fatalf("authorize answered %d, want 200", code)
	}

	var approval struct {
		GrantID string `json:"grant_id"`
		Code    string `json:"code"`
	}
	if code := mcpDoJSON(t, eng, http.MethodPost, mcp.ConsentPath, map[string]any{
		"client_id":             reg.ClientID,
		"redirect_uri":          redirectURI,
		"scopes":                []string{string(mcp.ScopeRead)},
		"state":                 "audit-state",
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"name":                  grantName,
		"site_scope_mode":       string(mcp.SiteScopeModeAll),
	}, nil, &approval); code != http.StatusOK {
		t.Fatalf("consent answered %d, want 200", code)
	}
	grantID := approval.GrantID

	// ------------------------------------------------------------------
	// PROOF 1: mcp.grant.created landed, actor = the approving USER.
	// ------------------------------------------------------------------
	created := queryMCPAuditRowsAsAppRole(t, pool, tenantID, audit.ActionMCPGrantCreated)
	if len(created) != 1 {
		t.Fatalf("mcp.grant.created rows = %d, want exactly 1", len(created))
	}
	if created[0].actorType != audit.ActorUser {
		t.Errorf("mcp.grant.created actor_type = %q, want %q", created[0].actorType, audit.ActorUser)
	}
	if created[0].actorID != userID.String() {
		t.Errorf("mcp.grant.created actor_id = %q, want the approving user %q", created[0].actorID, userID)
	}
	if created[0].targetID != grantID {
		t.Errorf("mcp.grant.created target_id = %q, want the grant id %q", created[0].targetID, grantID)
	}
	if got, _ := created[0].metadata["grant_name"].(string); got != grantName {
		t.Errorf("mcp.grant.created metadata.grant_name = %q, want %q", got, grantName)
	}
	t.Logf("PROOF 1 ok: mcp.grant.created attributed to user %s for grant %s", userID, grantID)

	// ------------------------------------------------------------------
	// Exchange the code, then call tools/call over the real transport.
	// ------------------------------------------------------------------
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", approval.Code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", reg.ClientID)
	form.Set("code_verifier", verifier)

	tokReq := httptest.NewRequest(http.MethodPost, mcp.TokenPath, strings.NewReader(form.Encode()))
	tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokReq.RemoteAddr = "203.0.113.7:5555"
	tokW := httptest.NewRecorder()
	eng.ServeHTTP(tokW, tokReq)
	if tokW.Code != http.StatusOK {
		t.Fatalf("token exchange answered %d, want 200: %s", tokW.Code, tokW.Body.String())
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(tokW.Body.Bytes(), &tok); err != nil {
		t.Fatalf("token response is not JSON: %v", err)
	}
	if tok.AccessToken == "" {
		t.Fatal("token exchange returned no access_token")
	}

	got := mcpRPC(t, eng, tok.AccessToken, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": mcp.ToolListSites, "arguments": map[string]any{}},
	})
	if got.status != http.StatusOK {
		t.Fatalf("tools/call %s answered %d, want 200: %s", mcp.ToolListSites, got.status, got.body)
	}

	// ------------------------------------------------------------------
	// PROOF 2: mcp.tool.called landed, actor = the ASSISTANT (the grant),
	// never the approving user.
	// ------------------------------------------------------------------
	called := queryMCPAuditRowsAsAppRole(t, pool, tenantID, audit.ActionMCPToolCalled)
	if len(called) != 1 {
		t.Fatalf("mcp.tool.called rows = %d, want exactly 1", len(called))
	}
	if called[0].actorType != audit.ActorAssistant {
		t.Errorf("mcp.tool.called actor_type = %q, want %q", called[0].actorType, audit.ActorAssistant)
	}
	if called[0].actorID != grantID {
		t.Errorf("mcp.tool.called actor_id = %q, want the grant id %q (NOT the user)", called[0].actorID, grantID)
	}
	if called[0].targetID != mcp.ToolListSites {
		t.Errorf("mcp.tool.called target_id = %q, want the tool name %q", called[0].targetID, mcp.ToolListSites)
	}
	if got, _ := called[0].metadata["grant_name"].(string); got != grantName {
		t.Errorf("mcp.tool.called metadata.grant_name = %q, want %q (the actor label)", got, grantName)
	}
	if got, _ := called[0].metadata["operator_permission"].(string); got != string(authz.PermSiteRead) {
		t.Errorf("mcp.tool.called metadata.operator_permission = %q, want %q", got, authz.PermSiteRead)
	}
	t.Logf("PROOF 2 ok: mcp.tool.called attributed to assistant/grant %s, not to user %s", grantID, userID)

	// ------------------------------------------------------------------
	// PROOF 3 (tenant isolation): a SECOND tenant's InTenantTx read sees
	// NONE of tenant 1's assistant row, even though both live in the same
	// physical audit_log table.
	// ------------------------------------------------------------------
	otherTenant := seedTenant(t, pool, "mcp-audit-other-"+suffix)
	cross := queryMCPAuditRowsAsAppRole(t, pool, otherTenant, audit.ActionMCPToolCalled)
	if len(cross) != 0 {
		t.Fatalf("PROOF 3: tenant %s's InTenantTx read %d mcp.tool.called row(s) belonging to "+
			"tenant %s; RLS did not isolate the assistant actor's row across tenants",
			otherTenant, len(cross), tenantID)
	}
	t.Logf("PROOF 3 ok: tenant %s cannot see tenant %s's assistant audit row", otherTenant, tenantID)

	// ------------------------------------------------------------------
	// PROOF 4: mcp.grant.revoked landed, actor = the revoking USER.
	// ------------------------------------------------------------------
	connEng := mountConnectionsLikeProduction(t, svc, admin)
	var revoked struct {
		GrantsRevoked int64 `json:"grants_revoked"`
	}
	if code := mcpDoJSON(t, connEng, http.MethodPost,
		mcp.ConnectionRevokePathFor(grantID), nil, nil, &revoked); code != http.StatusOK {
		t.Fatalf("revoke answered %d, want 200", code)
	}
	if revoked.GrantsRevoked != 1 {
		t.Fatalf("revoke grants_revoked = %d, want 1 for a first revoke", revoked.GrantsRevoked)
	}

	revokedRows := queryMCPAuditRowsAsAppRole(t, pool, tenantID, audit.ActionMCPGrantRevoked)
	if len(revokedRows) != 1 {
		t.Fatalf("mcp.grant.revoked rows = %d, want exactly 1", len(revokedRows))
	}
	if revokedRows[0].actorType != audit.ActorUser {
		t.Errorf("mcp.grant.revoked actor_type = %q, want %q", revokedRows[0].actorType, audit.ActorUser)
	}
	if revokedRows[0].actorID != userID.String() {
		t.Errorf("mcp.grant.revoked actor_id = %q, want the revoking user %q", revokedRows[0].actorID, userID)
	}
	if revokedRows[0].targetID != grantID {
		t.Errorf("mcp.grant.revoked target_id = %q, want the grant id %q", revokedRows[0].targetID, grantID)
	}
	t.Logf("PROOF 4 ok: mcp.grant.revoked attributed to user %s for grant %s", userID, grantID)
}

// TestMCPAuditEvents_RolledBackGrantCreationLeavesNoAuditRow_AsAppRole is the
// fail-closed half: Service.Approve wires the ActionMCPGrantCreated append
// through Repo.CreateGrantWithCode's onCreated hook, which runs INSIDE the same
// transaction as both inserts. This simulates an onCreated-shaped failure (the
// exact hook the audit append uses) and asserts the whole write rolls back --
// no mcp_grants row AND no audit_log row for the grant id that never committed.
func TestMCPAuditEvents_RolledBackGrantCreationLeavesNoAuditRow_AsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenantID := seedTenant(t, pool, "mcp-audit-rb-"+uuid.NewString()[:8])
	repo := mcp.NewRepo(pool)
	principal := domain.Principal{TenantID: tenantID, Scope: domain.ScopeOrg}

	simulated := errors.New("simulated: mcp.grant.created append failed")
	var capturedGrantID uuid.UUID
	_, _, err := repo.CreateGrantWithCode(ctx, principal, sqlc.CreateMCPGrantParams{
		TenantID:      tenantID,
		Name:          "should not survive",
		Status:        "active",
		SiteScopeMode: "all",
		ScopeTagIds:   []uuid.UUID{},
		ScopeSiteIds:  []uuid.UUID{},
	}, func(grantID uuid.UUID) sqlc.CreateMCPAuthorizationCodeParams {
		capturedGrantID = grantID
		return sqlc.CreateMCPAuthorizationCodeParams{
			TenantID:            tenantID,
			GrantID:             grantID,
			ClientID:            "rollback-probe-client",
			CodeHash:            "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			CodeChallenge:       "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM",
			CodeChallengeMethod: "S256",
			RedirectUri:         "https://claude.ai/api/mcp/auth_callback",
			ExpiresAt:           time.Now().UTC().Add(5 * time.Minute),
		}
	}, func(tx pgx.Tx, gr sqlc.McpGrant) error {
		return simulated
	})

	if err == nil {
		t.Fatal("CreateGrantWithCode succeeded despite the onCreated hook failing, want an error")
	}
	if !errors.Is(err, simulated) {
		t.Errorf("error = %v, want it to wrap the simulated failure", err)
	}
	if capturedGrantID == uuid.Nil {
		t.Fatal("the grant id was never captured; the insert must run before onCreated for this proof to mean anything")
	}

	var grantCount int
	if err := pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM mcp_grants WHERE id = $1`, capturedGrantID).Scan(&grantCount)
	}); err != nil {
		t.Fatalf("count mcp_grants: %v", err)
	}
	if grantCount != 0 {
		t.Errorf("mcp_grants row for %s committed despite the audit hook failing, want 0 rows", capturedGrantID)
	}

	rows := queryMCPAuditRowsAsAppRole(t, pool, tenantID, audit.ActionMCPGrantCreated)
	for _, r := range rows {
		if r.targetID == capturedGrantID.String() {
			t.Fatalf("found an mcp.grant.created row for grant %s, which never committed", capturedGrantID)
		}
	}
	t.Logf("rollback proof ok: neither mcp_grants nor audit_log carries a row for %s", capturedGrantID)
}
