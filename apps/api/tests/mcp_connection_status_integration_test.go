// mcp_connection_status_integration_test.go: the add-connection wizard's Step 8
// and Step 9 read (S29), proven against the REAL schema, the REAL RLS policies
// and the REAL application role, through the SAME dispatch production uses.
//
// WHAT A UNIT TEST CANNOT PROVE HERE, AND THIS FILE THEREFORE MUST:
//
//  1. The read runs as wpmgr_app -- NOSUPERUSER, NOBYPASSRLS. Every assertion
//     below is worthless under a superuser, because mcp_grants' RESTRICTIVE
//     _site_scope policy and its tenant-isolation policy are both inert for a
//     role that bypasses RLS. mcpAssertAndReportRole prints current_user,
//     rolsuper and rolbypassrls from pg_roles INSIDE the transaction under
//     test, so the proof states its own preconditions rather than assuming
//     them. That is exactly how m112's proofs passed while the email domain was
//     cross-site readable.
//  2. One tenant learns NOTHING about another's connection -- not the state,
//     not the client name, not even that the id exists.
//  3. A site-scoped collaborator is refused OUTRIGHT. Minting an org-wide grant
//     from a site-scoped session was a live defect on this exact surface, and
//     this endpoint READS the same object.
//  4. THE last_used_at TRAP. tools/list stamps last_used_at, and every client
//     issues tools/list unprompted right after initialize. A Step 9 derived
//     from that column would report "it read your fleet" for a client that read
//     nothing. Proving this needs the real transport stamping the real column,
//     which no fake can do.
//
// Every state is asserted BY VALUE -- the state string, the recorded fields,
// the protocol block -- never by "a response arrived".
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
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// statusBody is the wire shape the wizard consumes. It is decoded into typed
// POINTERS wherever the endpoint promises null, so that a field which wrongly
// serialises as a zero value fails a nil check here instead of reading as a
// plausible date or an empty string.
type statusBody struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Handshake struct {
		State                 string  `json:"state"`
		RecordedAt            *string `json:"recorded_at"`
		ReportedClientName    *string `json:"reported_client_name"`
		ReportedClientVersion *string `json:"reported_client_version"`
		Protocol              struct {
			State     string   `json:"state"`
			Version   *string  `json:"version"`
			Assumed   *string  `json:"assumed"`
			Floor     string   `json:"floor"`
			Target    string   `json:"target"`
			Supported []string `json:"supported"`
		} `json:"protocol"`
		Refusal any `json:"refusal"`
	} `json:"handshake"`
	FirstCall struct {
		State        string  `json:"state"`
		CalledAt     *string `json:"called_at"`
		ToolName     *string `json:"tool_name"`
		AuditEventID *string `json:"audit_event_id"`
		LastUsedAt   *string `json:"last_used_at"`
		Partial      any     `json:"partial"`
	} `json:"first_call"`
	PollAfterMs int `json:"poll_after_ms"`
}

// mcpRPCWithProtocol is mcpRPC with EXPLICIT control of the
// MCP-Protocol-Version header, including sending none at all.
//
// mcpRPC cannot be reused: it sets no protocol header, so every call through it
// lands in NegotiateProtocol's absent branch. This file has to distinguish
// "sent 2025-11-25", "sent nothing" and "sent 2024-11-05" -- three different
// stored outcomes and the whole substance of Step 8 -- so the header has to be
// a parameter. An empty protocol means the header is NOT SET, which is a
// different request from one carrying an empty value.
func mcpRPCWithProtocol(t *testing.T, eng *gin.Engine, bearer, protocol string,
	payload map[string]any) rpcResult {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal rpc payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, mcp.TransportPath, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.RemoteAddr = "203.0.113.7:5555"
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if protocol != "" {
		req.Header.Set(mcp.ProtocolHeader, protocol)
	}
	w := httptest.NewRecorder()
	eng.ServeHTTP(w, req)
	return rpcResult{status: w.Code, body: w.Body.String()}
}

// mountStatusLikeProduction mounts the connections surface WITH THE REAL
// authz.RequirePermission middleware.
//
// This differs from mountLikeProduction deliberately: that helper mounts
// Handler.Register (the OAuth half) and does NOT call RegisterConnections, so
// it cannot exercise the permission gate at all. The gate is the authorisation
// half of this proof, so it has to be the real one -- a stand-in middleware
// here would prove only that this file can add middleware.
func mountStatusLikeProduction(t *testing.T, svc *mcp.Service, principal domain.Principal) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	eng := gin.New()
	authed := eng.Group("/api/v1")
	authed.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), principal))
		c.Next()
	})
	// The REAL RegisterConnections, carrying its own RequirePermission per
	// route. Nothing here relaxes it.
	mcp.NewHandler(svc).RegisterConnections(authed)
	return eng
}

// getStatus issues the wizard's poll and returns the code with the decoded body.
func getStatus(t *testing.T, eng *gin.Engine, grantID string) (int, statusBody) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/connections/"+grantID+"/status", nil)
	w := httptest.NewRecorder()
	eng.ServeHTTP(w, req)

	var body statusBody
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("status response is not JSON: %v (%s)", err, w.Body.String())
		}
	}
	return w.Code, body
}

// mintGrant creates a connection through the REAL mint endpoint and returns the
// grant id and the plaintext token.
func mintGrant(t *testing.T, eng *gin.Engine, name string) (string, string) {
	t.Helper()
	var out struct {
		GrantID string `json:"grant_id"`
		Token   string `json:"token"`
	}
	body, err := json.Marshal(map[string]any{
		"name":            name,
		"site_scope_mode": string(mcp.SiteScopeModeAll),
	})
	if err != nil {
		t.Fatalf("marshal mint request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/connections", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	eng.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("mint answered %d, want 201: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("mint response is not JSON: %v", err)
	}
	if out.GrantID == "" || out.Token == "" {
		t.Fatalf("mint returned grant_id=%q token present=%t", out.GrantID, out.Token != "")
	}
	return out.GrantID, out.Token
}

func TestMCPConnectionStatus_StepsEightAndNine_AsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	// The role is load-bearing for every assertion below, printed from inside a
	// transaction opened by the same helper the read under test uses.
	if err := pool.InTenantTx(ctx, uuid.New(), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (connection status read path)")
		return nil
	}); err != nil {
		t.Fatalf("open tenant tx: %v", err)
	}

	suffix := uuid.NewString()[:8]
	tenantID := seedTenant(t, pool, "mcp-status-"+suffix)
	userID := seedUserRow(t, pool, "mcp-status-"+suffix+"@example.test")
	seedSite(t, pool, tenantID, "https://"+suffix+".example.test")

	rec := audit.NewRecorder(pool, domain.SystemClock{})
	svc := mcp.NewService(mcp.NewRepo(pool)).WithAudit(rec)

	admin := domain.Principal{UserID: userID, TenantID: tenantID, Role: "admin", Scope: domain.ScopeOrg}
	eng := mountStatusLikeProduction(t, svc, admin)
	// The transport, mounted exactly as server.New does, so initialize and
	// tools/* drive the real columns.
	transportEng := mountLikeProduction(t, svc, admin)

	// ==================================================================
	// PROOF 1 -- NOT-YET. A freshly minted grant has heard from nobody.
	// Both steps must say so, and neither may render as a failure.
	// ==================================================================
	freshID, freshTok := mintGrant(t, eng, "status fresh "+suffix)

	code, got := getStatus(t, eng, freshID)
	if code != http.StatusOK {
		t.Fatalf("status on a fresh grant answered %d, want 200", code)
	}
	if got.Handshake.State != "awaiting_client" {
		t.Errorf("fresh handshake.state = %q, want %q", got.Handshake.State, "awaiting_client")
	}
	if got.Handshake.Protocol.State != string(mcp.ClientProtocolNeverConnected) {
		t.Errorf("fresh protocol.state = %q, want %q",
			got.Handshake.Protocol.State, mcp.ClientProtocolNeverConnected)
	}
	// The NULLs that keep an absence from reading as a fact.
	if got.Handshake.RecordedAt != nil {
		t.Errorf("fresh recorded_at = %q, want null -- nothing has connected", *got.Handshake.RecordedAt)
	}
	if got.Handshake.Protocol.Version != nil {
		t.Errorf("fresh protocol.version = %q, want null", *got.Handshake.Protocol.Version)
	}
	if got.Handshake.Protocol.Assumed != nil {
		t.Errorf("fresh protocol.assumed = %q, want null -- nothing was assumed for a client "+
			"that never spoke", *got.Handshake.Protocol.Assumed)
	}
	if got.FirstCall.State != "awaiting_call" {
		t.Errorf("fresh first_call.state = %q, want %q", got.FirstCall.State, "awaiting_call")
	}
	if got.FirstCall.LastUsedAt != nil {
		t.Errorf("fresh last_used_at = %q, want null", *got.FirstCall.LastUsedAt)
	}
	// The server's own facts are present even before any client speaks -- this
	// is what lets the wizard render the floor without claiming a refusal.
	if got.Handshake.Protocol.Floor != mcp.ProtocolFloor {
		t.Errorf("protocol.floor = %q, want %q", got.Handshake.Protocol.Floor, mcp.ProtocolFloor)
	}
	if got.Handshake.Protocol.Target != mcp.ProtocolTarget {
		t.Errorf("protocol.target = %q, want %q", got.Handshake.Protocol.Target, mcp.ProtocolTarget)
	}
	if len(got.Handshake.Protocol.Supported) != len(mcp.SupportedRevisions()) {
		t.Errorf("protocol.supported = %v, want %v",
			got.Handshake.Protocol.Supported, mcp.SupportedRevisions())
	}
	if got.PollAfterMs <= 0 {
		t.Errorf("poll_after_ms = %d, want a positive interval", got.PollAfterMs)
	}
	t.Logf("PROOF 1 ok: fresh grant reports awaiting_client/awaiting_call with every "+
		"client-reported field null (floor=%s target=%s)",
		got.Handshake.Protocol.Floor, got.Handshake.Protocol.Target)

	// ==================================================================
	// PROOF 2 -- THE last_used_at TRAP. initialize WITH a recognised
	// header, then tools/list AND NOTHING ELSE.
	//
	// Step 8 must flip to connected. Step 9 MUST NOT: no tool has been
	// called. last_used_at is non-null by now, which is precisely why
	// deriving Step 9 from it would be a false success.
	// ==================================================================
	initRes := mcpRPCWithProtocol(t, transportEng, freshTok, mcp.ProtocolTarget, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": mcp.ProtocolTarget,
			"clientInfo":      map[string]any{"name": "claude-code", "version": "2.4.1"},
			"capabilities":    map[string]any{},
		},
	})
	if initRes.status != http.StatusOK {
		t.Fatalf("initialize answered %d, want 200: %s", initRes.status, initRes.body)
	}
	listRes := mcpRPCWithProtocol(t, transportEng, freshTok, mcp.ProtocolTarget, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{},
	})
	if listRes.status != http.StatusOK {
		t.Fatalf("tools/list answered %d, want 200: %s", listRes.status, listRes.body)
	}

	code, got = getStatus(t, eng, freshID)
	if code != http.StatusOK {
		t.Fatalf("status after initialize answered %d, want 200", code)
	}
	if got.Handshake.State != "connected" {
		t.Errorf("after initialize handshake.state = %q, want %q", got.Handshake.State, "connected")
	}
	if got.Handshake.Protocol.State != string(mcp.ClientProtocolRecognised) {
		t.Errorf("after initialize protocol.state = %q, want %q",
			got.Handshake.Protocol.State, mcp.ClientProtocolRecognised)
	}
	if got.Handshake.Protocol.Version == nil || *got.Handshake.Protocol.Version != mcp.ProtocolTarget {
		t.Errorf("after initialize protocol.version = %v, want %q",
			got.Handshake.Protocol.Version, mcp.ProtocolTarget)
	}
	// What the client SAID, recorded verbatim -- the wireframe's "we wrote
	// down what it said".
	if got.Handshake.ReportedClientName == nil || *got.Handshake.ReportedClientName != "claude-code" {
		t.Errorf("reported_client_name = %v, want %q", got.Handshake.ReportedClientName, "claude-code")
	}
	if got.Handshake.ReportedClientVersion == nil || *got.Handshake.ReportedClientVersion != "2.4.1" {
		t.Errorf("reported_client_version = %v, want %q", got.Handshake.ReportedClientVersion, "2.4.1")
	}
	if got.Handshake.RecordedAt == nil {
		t.Error("recorded_at is null after a successful initialize, want a timestamp")
	}
	// THE ASSERTION THIS WHOLE FILE IS FOR.
	if got.FirstCall.LastUsedAt == nil {
		t.Fatal("last_used_at is null after tools/list -- the premise of the next " +
			"assertion is gone, so it would pass vacuously")
	}
	if got.FirstCall.State != "awaiting_call" {
		t.Errorf("first_call.state = %q after tools/list with NO tools/call, want %q. "+
			"last_used_at is set (%s), so this is the false success that would be "+
			"produced by deriving Step 9 from that column",
			got.FirstCall.State, "awaiting_call", *got.FirstCall.LastUsedAt)
	}
	if got.FirstCall.ToolName != nil {
		t.Errorf("first_call.tool_name = %q with no tool called, want null", *got.FirstCall.ToolName)
	}
	t.Logf("PROOF 2 ok: tools/list stamped last_used_at=%s and Step 9 correctly stayed "+
		"awaiting_call; Step 8 reports connected as claude-code 2.4.1 on %s",
		*got.FirstCall.LastUsedAt, *got.Handshake.Protocol.Version)

	// ==================================================================
	// PROOF 3 -- IT WORKED. One real tools/call flips Step 9, by value.
	// ==================================================================
	callRes := mcpRPCWithProtocol(t, transportEng, freshTok, mcp.ProtocolTarget, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": mcp.ToolListSites, "arguments": map[string]any{}},
	})
	if callRes.status != http.StatusOK {
		t.Fatalf("tools/call answered %d, want 200: %s", callRes.status, callRes.body)
	}

	code, got = getStatus(t, eng, freshID)
	if code != http.StatusOK {
		t.Fatalf("status after tools/call answered %d, want 200", code)
	}
	if got.FirstCall.State != "succeeded" {
		t.Errorf("first_call.state = %q after a real tools/call, want %q",
			got.FirstCall.State, "succeeded")
	}
	if got.FirstCall.ToolName == nil || *got.FirstCall.ToolName != mcp.ToolListSites {
		t.Errorf("first_call.tool_name = %v, want %q", got.FirstCall.ToolName, mcp.ToolListSites)
	}
	if got.FirstCall.CalledAt == nil {
		t.Error("first_call.called_at is null on a succeeded call, want the audit row's timestamp")
	}
	if got.FirstCall.AuditEventID == nil {
		t.Error("first_call.audit_event_id is null on a succeeded call, want the audit row id")
	}
	// The gap that must stay a gap rather than becoming a clean success.
	if got.FirstCall.Partial != nil {
		t.Errorf("first_call.partial = %v, want null -- internal/mcp has no typed "+
			"per-site partial, so any value here is invented", got.FirstCall.Partial)
	}
	t.Logf("PROOF 3 ok: Step 9 succeeded on tool %q with audit row %s, partial correctly null",
		*got.FirstCall.ToolName, *got.FirstCall.AuditEventID)

	// ==================================================================
	// PROOF 4 -- NO PROTOCOL HEADER AT ALL. A separate grant, initialized
	// with NO MCP-Protocol-Version header. This is a SUCCESS state and it
	// must not print a version the client never sent.
	// ==================================================================
	bareID, bareTok := mintGrant(t, eng, "status bare "+suffix)
	bareRes := mcpRPCWithProtocol(t, transportEng, bareTok, "", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"clientInfo":   map[string]any{"name": "wpmgr-example-client", "version": "0.1.0"},
			"capabilities": map[string]any{},
		},
	})
	if bareRes.status != http.StatusOK {
		t.Fatalf("header-less initialize answered %d, want 200 (absence is NOT an error): %s",
			bareRes.status, bareRes.body)
	}

	code, got = getStatus(t, eng, bareID)
	if code != http.StatusOK {
		t.Fatalf("status on the header-less grant answered %d, want 200", code)
	}
	if got.Handshake.State != "connected_protocol_assumed" {
		t.Errorf("header-less handshake.state = %q, want %q",
			got.Handshake.State, "connected_protocol_assumed")
	}
	if got.Handshake.Protocol.State != string(mcp.ClientProtocolAbsent) {
		t.Errorf("header-less protocol.state = %q, want %q",
			got.Handshake.Protocol.State, mcp.ClientProtocolAbsent)
	}
	// version stays NULL and assumed carries the floor. Collapsing these two
	// would print a header the client never sent.
	if got.Handshake.Protocol.Version != nil {
		t.Errorf("header-less protocol.version = %q, want null -- the client sent no header",
			*got.Handshake.Protocol.Version)
	}
	if got.Handshake.Protocol.Assumed == nil || *got.Handshake.Protocol.Assumed != mcp.ProtocolFloor {
		t.Errorf("header-less protocol.assumed = %v, want %q",
			got.Handshake.Protocol.Assumed, mcp.ProtocolFloor)
	}
	// It is CONNECTED, not waiting: the two absences stay apart.
	if got.Handshake.RecordedAt == nil {
		t.Error("header-less recorded_at is null; a client that connected without a " +
			"header must stay distinguishable from one that never connected")
	}
	t.Logf("PROOF 4 ok: header-less client reports connected_protocol_assumed, "+
		"version=null assumed=%s", *got.Handshake.Protocol.Assumed)

	// ==================================================================
	// PROOF 5 -- BELOW THE FLOOR. The known gap, asserted as a gap.
	//
	// A client speaking 2024-11-05 is refused at the transport BEFORE
	// RecordConnect, so nothing is written and the row still reads
	// awaiting_client. This asserts the CURRENT TRUTH so that the day a
	// refusal is recorded, this test fails and is updated deliberately
	// rather than the gap being discovered by an operator.
	// ==================================================================
	floorID, floorTok := mintGrant(t, eng, "status floor "+suffix)
	refused := mcpRPCWithProtocol(t, transportEng, floorTok, "2024-11-05", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"clientInfo":      map[string]any{"name": "ancient-client", "version": "0.0.1"},
			"capabilities":    map[string]any{},
		},
	})
	if refused.status != http.StatusBadRequest {
		t.Fatalf("below-floor initialize answered %d, want 400: %s", refused.status, refused.body)
	}

	code, got = getStatus(t, eng, floorID)
	if code != http.StatusOK {
		t.Fatalf("status on the refused grant answered %d, want 200", code)
	}
	if got.Handshake.State != "awaiting_client" {
		t.Errorf("refused handshake.state = %q, want %q -- the refusal is NOT recorded, "+
			"so the truthful reading of the row is still the not-yet",
			got.Handshake.State, "awaiting_client")
	}
	if got.Handshake.Refusal != nil {
		t.Errorf("handshake.refusal = %v, want null -- nothing records a below-floor "+
			"refusal today, so any value here is invented from a NULL", got.Handshake.Refusal)
	}
	if got.Handshake.ReportedClientName != nil {
		t.Errorf("refused reported_client_name = %q, want null -- a refused client "+
			"never reaches RecordConnect", *got.Handshake.ReportedClientName)
	}
	t.Logf("PROOF 5 ok: a 400-refused below-floor client leaves the row untouched; " +
		"status reports awaiting_client with refusal=null (documented gap, needs a migration)")

	// ==================================================================
	// PROOF 6 -- TENANT ISOLATION. A second organisation's admin must
	// learn NOTHING about the first's connection, including whether the
	// id exists.
	// ==================================================================
	otherTenant := seedTenant(t, pool, "mcp-status-other-"+suffix)
	otherUser := seedUserRow(t, pool, "mcp-status-other-"+suffix+"@example.test")
	otherAdmin := domain.Principal{
		UserID: otherUser, TenantID: otherTenant, Role: "admin", Scope: domain.ScopeOrg,
	}
	otherEng := mountStatusLikeProduction(t, svc, otherAdmin)

	code, _ = getStatus(t, otherEng, freshID)
	if code != http.StatusNotFound {
		t.Errorf("tenant %s reading tenant %s's connection answered %d, want 404",
			otherTenant, tenantID, code)
	}
	// And the same principal CAN read its own, so the 404 above is isolation
	// and not a broken mount that 404s everything.
	otherID, _ := mintGrant(t, otherEng, "status other "+suffix)
	if code, _ = getStatus(t, otherEng, otherID); code != http.StatusOK {
		t.Fatalf("tenant %s reading its OWN connection answered %d, want 200 -- the 404 "+
			"above would otherwise prove nothing", otherTenant, code)
	}
	t.Logf("PROOF 6 ok: cross-tenant read 404s while the same principal's own read 200s")

	// ==================================================================
	// PROOF 7 -- AUTHORISATION. A site-scoped collaborator is refused
	// outright, by the same org-level permission the list and the mint
	// take.
	// ==================================================================
	siteScoped := domain.Principal{
		UserID:   userID,
		TenantID: tenantID,
		Role:     "admin", // admin ON ONE SITE -- the role is not what refuses it
		Scope:    domain.ScopeSite,
	}
	scopedEng := mountStatusLikeProduction(t, svc, siteScoped)
	if code, _ = getStatus(t, scopedEng, freshID); code != http.StatusForbidden {
		t.Errorf("site-scoped principal reading a connection status answered %d, want 403. "+
			"A grant is an ORGANISATION-wide credential and a site-scoped session must not "+
			"read it -- minting one from such a session was a live defect on this surface",
			code)
	}
	t.Logf("PROOF 7 ok: site-scoped principal refused 403 by RequirePermission(%s)",
		authz.PermAPIKeyRead)

	// A closing role print, so the file states its precondition at the end as
	// well as the start: nothing above ran as a superuser.
	if err := pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (after all proofs)")
		return nil
	}); err != nil {
		t.Fatalf("closing tenant tx: %v", err)
	}
}
