// mcp_refusal_audit_integration_test.go: ADR-061 A10's refusal half.
//
// WHAT WAS MISSING. internal/mcp wrote exactly ONE audit row on the transport
// path, ActionMCPToolCalled, and only after a successful invoke. Every refusal
// -- an unregistered tool name, a capability the grant does not hold, a
// site-keyed tool on an empty resolved scope, a protocol revision below the
// floor -- produced an slog line and NOTHING durable. A10 calls that record
// "the boundary's evidence", and the boundary had none.
//
// WHY THESE PROOFS ARE HERE AND NOT IN internal/mcp. A unit test with a
// fakeStore cannot construct an audit.Recorder (it needs a real *db.Pool), so
// it can assert at most that a method was called. The three facts that matter
// are all database facts:
//
//  1. The row actually lands in audit_log, hash-chained, with the right actor
//     pair -- ActorAssistant over the GRANT id, never the human who approved
//     the connection.
//  2. It is readable back only through db.Pool as wpmgr_app -- rolsuper=f,
//     rolbypassrls=f, printed from pg_roles INSIDE the transaction under test
//     by mcpAssertAndReportRole. A test that opened its own connection would
//     leave every RLS policy inert and pass regardless.
//  3. A second tenant cannot see it, though both tenants' rows share one
//     physical audit_log table.
//
// Every refusal below is driven through the SAME mounted transport a client
// reaches, with a real minted bearer token. No GUC is hand-set in this file.
package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// mcpRefusalRPC is mcpRPC plus the MCP-Protocol-Version header, which the
// shared helper cannot set and which one of the refusals below is entirely
// about. Same transport, same engine, same bearer.
func mcpRefusalRPC(t *testing.T, eng *gin.Engine, bearer, protoHeader string, payload map[string]any) rpcResult {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal rpc payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, mcp.TransportPath, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.RemoteAddr = "203.0.113.9:5555"
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if protoHeader != "" {
		req.Header.Set(mcp.ProtocolHeader, protoHeader)
	}
	w := httptest.NewRecorder()
	eng.ServeHTTP(w, req)
	return rpcResult{status: w.Code, body: w.Body.String()}
}

// mintForRefusal mints a connection through the real POST
// /api/v1/mcp/connections route and returns (grantID, token).
func mintForRefusal(t *testing.T, eng *gin.Engine, name string, body map[string]any) (string, string) {
	t.Helper()
	body["name"] = name
	var out struct {
		GrantID string `json:"grant_id"`
		Token   string `json:"token"`
	}
	if code := mcpDoJSON(t, eng, http.MethodPost, mcp.ConnectionsPath, body, nil, &out); code != http.StatusCreated {
		t.Fatalf("mint %q answered %d, want 201", name, code)
	}
	if out.Token == "" || out.GrantID == "" {
		t.Fatalf("mint %q returned grant_id=%q token empty=%t", name, out.GrantID, out.Token == "")
	}
	return out.GrantID, out.Token
}

// seedEmptyTag inserts a real site_tags row carrying NO sites, through the same
// tenant tx helper the request path uses. A grant scoped to it is legitimate
// and stores cleanly (verifyScopeReferents only refuses ids naming no row at
// all), and it resolves to zero sites -- which is the site_scope_empty refusal
// an operator actually hits.
func seedEmptyTag(t *testing.T, pool *db.Pool, tenantID uuid.UUID, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`INSERT INTO site_tags (tenant_id, name) VALUES ($1, $2) RETURNING id`,
			tenantID, name).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed empty tag %q: %v", name, err)
	}
	return id
}

// TestMCPRefusalAudit_EveryRefusalWritesItsRow_AsAppRole is the A10 proof.
func TestMCPRefusalAudit_EveryRefusalWritesItsRow_AsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	// The role is load-bearing for every assertion in this file: printed from
	// pg_roles inside a transaction opened by the same helper the request path
	// opens, so "these policies were live" is an executed fact and not a claim.
	if err := pool.InMCPClientLookupTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InMCPClientLookupTx")
		return nil
	}); err != nil {
		t.Fatalf("open lookup tx: %v", err)
	}

	suffix := uuid.NewString()[:8]
	tenantID := seedTenant(t, pool, "mcp-refusal-"+suffix)
	userID := seedUserRow(t, pool, "mcp-refusal-"+suffix+"@example.test")
	seedSite(t, pool, tenantID, "https://"+suffix+".example.test")

	rec := audit.NewRecorder(pool, domain.SystemClock{})
	svc := mcp.NewService(mcp.NewRepo(pool)).WithAudit(rec)

	admin := domain.Principal{UserID: userID, TenantID: tenantID, Role: "admin", Scope: domain.ScopeOrg}
	connEng := mountConnectionsLikeProduction(t, svc, admin)
	transportEng := mountLikeProduction(t, svc, admin)

	const wideName = "refusal audit wide connection"
	const emptyName = "refusal audit empty-scope connection"

	wideGrantID, wideToken := mintForRefusal(t, connEng, wideName, map[string]any{
		"site_scope_mode": string(mcp.SiteScopeModeAll),
	})
	emptyTagID := seedEmptyTag(t, pool, tenantID, "refusal-audit-"+suffix)
	emptyGrantID, emptyToken := mintForRefusal(t, connEng, emptyName, map[string]any{
		"site_scope_mode": string(mcp.SiteScopeModeTags),
		"scope_tag_ids":   []string{emptyTagID.String()},
	})

	// ------------------------------------------------------------------
	// REFUSAL 1: a tool name that does not exist. reasonUnregistered.
	// ------------------------------------------------------------------
	const guessed = "sites.restart"
	got := mcpRefusalRPC(t, transportEng, wideToken, "", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": guessed, "arguments": map[string]any{}},
	})
	if got.status != http.StatusOK {
		t.Fatalf("tools/call %q answered HTTP %d, want 200 with a JSON-RPC error: %s", guessed, got.status, got.body)
	}

	denied := queryMCPAuditRowsAsAppRole(t, pool, tenantID, audit.ActionMCPToolDenied)
	if len(denied) != 1 {
		t.Fatalf("after one unregistered-name refusal, mcp.tool.denied rows = %d, want exactly 1", len(denied))
	}
	// ASSERT BY VALUE, never that a row exists.
	if denied[0].actorType != audit.ActorAssistant {
		t.Errorf("mcp.tool.denied actor_type = %q, want %q", denied[0].actorType, audit.ActorAssistant)
	}
	if denied[0].actorID != wideGrantID {
		t.Errorf("mcp.tool.denied actor_id = %q, want the grant id %q (NOT the approving user %q)",
			denied[0].actorID, wideGrantID, userID)
	}
	if denied[0].targetID != guessed {
		t.Errorf("mcp.tool.denied target_id = %q, want the name the caller spelled, %q", denied[0].targetID, guessed)
	}
	if r, _ := denied[0].metadata["refusal_reason"].(string); r != "unregistered" {
		t.Errorf("mcp.tool.denied metadata.refusal_reason = %q, want %q", r, "unregistered")
	}
	if n, _ := denied[0].metadata["grant_name"].(string); n != wideName {
		t.Errorf("mcp.tool.denied metadata.grant_name = %q, want %q", n, wideName)
	}
	t.Logf("REFUSAL 1 ok: mcp.tool.denied reason=unregistered target=%q actor=assistant/%s", guessed, wideGrantID)

	// ------------------------------------------------------------------
	// REFUSAL 2: a tool the grant HOLDS, on a resolved site scope of zero
	// sites. reasonSiteScopeEmpty. This is the refusal ADR-061 A11 §3 calls
	// "the single most likely place for a fail-open default to survive
	// review", so its record is the one most worth having.
	// ------------------------------------------------------------------
	got = mcpRefusalRPC(t, transportEng, emptyToken, "", map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": mcp.ToolFleetSitesList, "arguments": map[string]any{}},
	})
	if got.status != http.StatusOK {
		t.Fatalf("empty-scope tools/call answered HTTP %d, want 200: %s", got.status, got.body)
	}
	if !strings.Contains(got.body, "-32002") {
		t.Fatalf("empty-scope tools/call did not answer the named scope-empty code -32002; "+
			"this proof depends on that branch being the one taken: %s", got.body)
	}

	denied = queryMCPAuditRowsAsAppRole(t, pool, tenantID, audit.ActionMCPToolDenied)
	if len(denied) != 2 {
		t.Fatalf("after the empty-scope refusal, mcp.tool.denied rows = %d, want 2", len(denied))
	}
	var scopeRow *mcpAuditRow
	for i := range denied {
		if denied[i].actorID == emptyGrantID {
			scopeRow = &denied[i]
		}
	}
	if scopeRow == nil {
		t.Fatalf("no mcp.tool.denied row attributed to the empty-scope grant %s; rows: %+v", emptyGrantID, denied)
	}
	if scopeRow.actorType != audit.ActorAssistant {
		t.Errorf("empty-scope row actor_type = %q, want %q", scopeRow.actorType, audit.ActorAssistant)
	}
	if scopeRow.targetID != mcp.ToolFleetSitesList {
		t.Errorf("empty-scope row target_id = %q, want %q", scopeRow.targetID, mcp.ToolFleetSitesList)
	}
	if r, _ := scopeRow.metadata["refusal_reason"].(string); r != "site_scope_empty" {
		t.Errorf("empty-scope row metadata.refusal_reason = %q, want %q", r, "site_scope_empty")
	}
	if n, ok := scopeRow.metadata["scoped_sites"].(float64); !ok || n != 0 {
		t.Errorf("empty-scope row metadata.scoped_sites = %v, want 0 -- the number that MAKES it a refusal",
			scopeRow.metadata["scoped_sites"])
	}
	t.Logf("REFUSAL 2 ok: mcp.tool.denied reason=site_scope_empty scoped_sites=0 actor=assistant/%s", emptyGrantID)

	// ------------------------------------------------------------------
	// REFUSAL 3: a protocol revision below the floor, on the header.
	// ------------------------------------------------------------------
	const belowFloor = "2024-11-05"
	got = mcpRefusalRPC(t, transportEng, wideToken, belowFloor, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/list",
	})
	if got.status != http.StatusBadRequest {
		t.Fatalf("below-floor revision answered HTTP %d, want 400: %s", got.status, got.body)
	}

	proto := queryMCPAuditRowsAsAppRole(t, pool, tenantID, audit.ActionMCPProtocolDenied)
	if len(proto) != 1 {
		t.Fatalf("mcp.protocol.denied rows = %d, want exactly 1", len(proto))
	}
	if proto[0].actorType != audit.ActorAssistant {
		t.Errorf("mcp.protocol.denied actor_type = %q, want %q", proto[0].actorType, audit.ActorAssistant)
	}
	if proto[0].actorID != wideGrantID {
		t.Errorf("mcp.protocol.denied actor_id = %q, want the grant id %q", proto[0].actorID, wideGrantID)
	}
	if proto[0].targetID != belowFloor {
		t.Errorf("mcp.protocol.denied target_id = %q, want the revision asked for, %q", proto[0].targetID, belowFloor)
	}
	if r, _ := proto[0].metadata["refusal_reason"].(string); r != "below_floor" {
		t.Errorf("mcp.protocol.denied metadata.refusal_reason = %q, want %q", r, "below_floor")
	}
	if p, _ := proto[0].metadata["phase"].(string); p != "header" {
		t.Errorf("mcp.protocol.denied metadata.phase = %q, want %q", p, "header")
	}
	if f, _ := proto[0].metadata["floor"].(string); f != mcp.ProtocolFloor {
		t.Errorf("mcp.protocol.denied metadata.floor = %q, want %q", f, mcp.ProtocolFloor)
	}
	t.Logf("REFUSAL 3 ok: mcp.protocol.denied reason=below_floor phase=header requested=%s", belowFloor)

	// ------------------------------------------------------------------
	// REFUSAL 4: an unsupported revision in the initialize PARAMS, which is
	// the other of the two negotiation sites and a different phase.
	// ------------------------------------------------------------------
	got = mcpRefusalRPC(t, transportEng, wideToken, "", map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "initialize",
		"params": map[string]any{"protocolVersion": "not-a-revision"},
	})
	if got.status != http.StatusBadRequest {
		t.Fatalf("unparseable initialize revision answered HTTP %d, want 400: %s", got.status, got.body)
	}
	proto = queryMCPAuditRowsAsAppRole(t, pool, tenantID, audit.ActionMCPProtocolDenied)
	if len(proto) != 2 {
		t.Fatalf("after the initialize-params refusal, mcp.protocol.denied rows = %d, want 2", len(proto))
	}
	var paramRow *mcpAuditRow
	for i := range proto {
		if p, _ := proto[i].metadata["phase"].(string); p == "initialize_params" {
			paramRow = &proto[i]
		}
	}
	if paramRow == nil {
		t.Fatalf("no mcp.protocol.denied row with phase=initialize_params; rows: %+v", proto)
	}
	if paramRow.targetID != "not-a-revision" {
		t.Errorf("initialize-params row target_id = %q, want %q", paramRow.targetID, "not-a-revision")
	}
	if r, _ := paramRow.metadata["refusal_reason"].(string); r != "unsupported" {
		t.Errorf("initialize-params row metadata.refusal_reason = %q, want %q", r, "unsupported")
	}
	t.Logf("REFUSAL 4 ok: mcp.protocol.denied reason=unsupported phase=initialize_params")

	// ------------------------------------------------------------------
	// OVER-FIRE CHECK: a SUCCESSFUL tool call must write mcp.tool.called and
	// must NOT add an mcp.tool.denied row. A refusal recorder that fires on
	// the success path would make every denial row meaningless, and the
	// count assertions above would still pass.
	// ------------------------------------------------------------------
	deniedBefore := len(queryMCPAuditRowsAsAppRole(t, pool, tenantID, audit.ActionMCPToolDenied))
	got = mcpRefusalRPC(t, transportEng, wideToken, "", map[string]any{
		"jsonrpc": "2.0", "id": 5, "method": "tools/call",
		"params": map[string]any{"name": mcp.ToolFleetSitesList, "arguments": map[string]any{}},
	})
	if got.status != http.StatusOK || strings.Contains(got.body, `"error"`) {
		t.Fatalf("the success-path call did not succeed, so the over-fire check proves nothing: HTTP %d %s",
			got.status, got.body)
	}
	called := queryMCPAuditRowsAsAppRole(t, pool, tenantID, audit.ActionMCPToolCalled)
	if len(called) != 1 {
		t.Fatalf("mcp.tool.called rows = %d, want exactly 1", len(called))
	}
	if called[0].actorID != wideGrantID || called[0].targetID != mcp.ToolFleetSitesList {
		t.Errorf("mcp.tool.called actor_id/target_id = %q/%q, want %q/%q",
			called[0].actorID, called[0].targetID, wideGrantID, mcp.ToolFleetSitesList)
	}
	deniedAfter := len(queryMCPAuditRowsAsAppRole(t, pool, tenantID, audit.ActionMCPToolDenied))
	if deniedAfter != deniedBefore {
		t.Fatalf("OVER-FIRE: a SUCCESSFUL tools/call added %d mcp.tool.denied row(s) (%d -> %d); "+
			"the refusal recorder is firing on the success path",
			deniedAfter-deniedBefore, deniedBefore, deniedAfter)
	}
	t.Logf("OVER-FIRE ok: a successful call wrote mcp.tool.called and left mcp.tool.denied at %d", deniedAfter)

	// ------------------------------------------------------------------
	// TENANT ISOLATION, on BOTH new actions. Same physical audit_log table.
	// ------------------------------------------------------------------
	otherTenant := seedTenant(t, pool, "mcp-refusal-other-"+suffix)
	for _, action := range []string{audit.ActionMCPToolDenied, audit.ActionMCPProtocolDenied} {
		cross := queryMCPAuditRowsAsAppRole(t, pool, otherTenant, action)
		if len(cross) != 0 {
			t.Fatalf("tenant %s's InTenantTx read %d %s row(s) belonging to tenant %s; "+
				"RLS did not isolate the refusal record across tenants",
				otherTenant, len(cross), action, tenantID)
		}
	}
	t.Logf("ISOLATION ok: tenant %s sees none of tenant %s's refusal rows", otherTenant, tenantID)
}
