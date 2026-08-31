// m128's setup_client, proven end to end through the SAME DISPATCH PRODUCTION
// USES -- the mounted POST /api/v1/mcp/connections and GET
// /api/v1/mcp/connections routes, over mcp.Service and mcp.Repo, as wpmgr_app
// with neither SUPERUSER nor BYPASSRLS.
//
// WHY THESE PROOFS AND NOT THE ONES NEXT DOOR.
// mcp_m127_m128_merge_roundtrip_integration_test.go proves the COLUMN survives
// the merge, and it drives sqlc.CreateMCPGrant directly because when it was
// written no Go caller populated the field. That is now the wrong surface: the
// question has moved from "does the column exist" to "does the request path
// write it, and does every path that should not write it leave it alone". A
// query-level proof cannot answer either -- it would stay green against a
// handler that drops the field on the floor, which is precisely the shape of
// GH #605, where a fully-wired read path sat over a column nothing ever
// stamped and the list confidently reported "Never used" for every connection.
//
// SO EVERY WRITE HERE ENTERS THROUGH THE HTTP ROUTE AND EVERY READ COMES BACK
// OFF THE WIRE. The DTO field, the service, the repo, the tx helper and the
// RLS policies are all inside the loop. The one direct-SQL read in this file is
// the tenant-isolation control, and it is inside InTenantTx for the same reason.
package tests

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// setupClientMintResponse is the mint response, narrowed to what these proofs
// read. The plaintext token is needed by the RecordConnect proof.
type setupClientMintResponse struct {
	GrantID string `json:"grant_id"`
	Token   string `json:"token"`
}

// setupClientListResponse mirrors connectionListDTO, narrowed the same way.
//
// SetupClient IS A *string HERE ON PURPOSE, and it is the entire point of the
// file. A plain string would decode both `"setup_client": null` and
// `"setup_client": "generic"`-less-omitted into "", collapsing the two states
// these tests exist to keep apart -- the test would then pass against exactly
// the defect it is written to catch.
type setupClientListResponse struct {
	Connections []struct {
		ID                 string  `json:"id"`
		Name               string  `json:"name"`
		SetupClient        *string `json:"setup_client"`
		ReportedClientName *string `json:"reported_client_name"`
	} `json:"connections"`
}

// ---------------------------------------------------------------------------
// PROOF 1 -- ROUND TRIP. Mint with a client id, read it back off the list.
// ---------------------------------------------------------------------------

func TestMCPSetupClientRoundTripsThroughTheMountedRoutesAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	// The role is load-bearing and is asserted INSIDE a transaction the request
	// path actually opens. Either privilege makes every RLS assertion below
	// vacuous, and this file's tenant-isolation proof would pass while the
	// policy was inert -- m112's exact failure.
	if err := pool.InTenantTx(ctx, uuid.New(), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (setup_client round trip)")
		return nil
	}); err != nil {
		t.Fatalf("open tenant tx: %v", err)
	}

	svc := mcp.NewService(mcp.NewRepo(pool))
	suffix := uuid.NewString()[:8]
	tenantID := seedTenant(t, pool, "mcp-m128-rt-"+suffix)
	userID := seedUserRow(t, pool, "mcp-m128-rt-"+suffix+"@example.test")
	eng := mountConnectionsLikeProduction(t, svc, adminPrincipal(tenantID, userID))

	var minted setupClientMintResponse
	code := mcpDoJSON(t, eng, http.MethodPost, mcp.ConnectionsPath, map[string]any{
		"name":            "round trip",
		"site_scope_mode": "all",
		"setup_client":    "claude-code",
	}, nil, &minted)
	if code != http.StatusCreated {
		t.Fatalf("mint answered %d, want 201", code)
	}

	got := setupClientFor(t, eng, minted.GrantID)
	if got == nil {
		t.Fatal("setup_client came back NULL for a connection minted WITH " +
			"\"claude-code\". The write path is not wired: this is GH #605's " +
			"defect -- a read path over a column nothing stamps.")
	}
	if *got != "claude-code" {
		t.Fatalf("setup_client = %q, want \"claude-code\"", *got)
	}
	t.Logf("PROOF 1 ok: minted with \"claude-code\", read back %q off the list route", *got)
}

// ---------------------------------------------------------------------------
// PROOF 2 -- NULL AND "generic" ARE DIFFERENT, ASSERTED BY VALUE.
//
// Both are legal stored states and they mean opposite things: NULL is "nobody
// asked", "generic" is "the operator saw nine cards and chose Other MCP
// client". S29 step 9 renders them differently and S31's filter gathers only
// the second. Asserting merely that the omitted case is "not claude-code" would
// pass against a service that defaulted it to "generic", which is the specific
// wrong answer m128 DECISION 2(b) refuses a schema default to prevent -- so
// BOTH are asserted by value, in one test, against one another.
// ---------------------------------------------------------------------------

func TestMCPSetupClientOmittedStoresNullNotGenericAsAppRole(t *testing.T) {
	pool := startPostgres(t)
	svc := mcp.NewService(mcp.NewRepo(pool))
	suffix := uuid.NewString()[:8]
	tenantID := seedTenant(t, pool, "mcp-m128-null-"+suffix)
	userID := seedUserRow(t, pool, "mcp-m128-null-"+suffix+"@example.test")
	eng := mountConnectionsLikeProduction(t, svc, adminPrincipal(tenantID, userID))

	// (a) The key is ABSENT from the body -- not null, absent. This is the
	// request every non-wizard caller sends.
	var omitted setupClientMintResponse
	if code := mcpDoJSON(t, eng, http.MethodPost, mcp.ConnectionsPath, map[string]any{
		"name":            "no client chosen",
		"site_scope_mode": "all",
	}, nil, &omitted); code != http.StatusCreated {
		t.Fatalf("mint without setup_client answered %d, want 201", code)
	}

	// (b) The operator actively chose "Other MCP client".
	var chose setupClientMintResponse
	if code := mcpDoJSON(t, eng, http.MethodPost, mcp.ConnectionsPath, map[string]any{
		"name":            "other mcp client",
		"site_scope_mode": "all",
		"setup_client":    "generic",
	}, nil, &chose); code != http.StatusCreated {
		t.Fatalf("mint with generic answered %d, want 201", code)
	}

	gotOmitted := setupClientFor(t, eng, omitted.GrantID)
	gotChose := setupClientFor(t, eng, chose.GrantID)

	if gotOmitted != nil {
		t.Fatalf("a mint that never mentioned setup_client stored %q. NULL is "+
			"the only honest value: %q asserts the operator saw the nine-card "+
			"chooser and made a choice they were never shown.",
			*gotOmitted, *gotOmitted)
	}
	if gotChose == nil {
		t.Fatal("an operator who explicitly chose \"generic\" got NULL back. " +
			"The choice was discarded and step 9 can no longer tell that " +
			"operator apart from one who was never asked.")
	}
	if *gotChose != "generic" {
		t.Fatalf("explicit \"generic\" stored as %q", *gotChose)
	}
	t.Logf("PROOF 2 ok: omitted -> null, explicit choice -> %q; the two are distinct on the wire", *gotChose)
}

// ---------------------------------------------------------------------------
// PROOF 3 -- RecordConnect DOES NOT TOUCH setup_client.
//
// THE PROPERTY MOST LIKELY TO BE BROKEN LATER BY SOMEONE TIDYING. RecordConnect
// already writes client_name and client_version, so "keep setup_client in sync
// while we are here" is the natural-looking next edit, and it silently destroys
// the column: the operator's stated choice is overwritten by the client's
// self-report on first connect, and every screen keeps rendering, wrong.
//
// The two values are deliberately DIFFERENT and both non-empty -- set up for
// Claude Desktop, connected as Cursor -- which is the legitimate permanent
// disagreement m128 DECISION 1 describes. A test using the same string for
// both would pass against an overwrite.
// ---------------------------------------------------------------------------

func TestMCPRecordConnectLeavesSetupClientAloneAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	svc := mcp.NewService(mcp.NewRepo(pool))
	suffix := uuid.NewString()[:8]
	tenantID := seedTenant(t, pool, "mcp-m128-rc-"+suffix)
	userID := seedUserRow(t, pool, "mcp-m128-rc-"+suffix+"@example.test")
	eng := mountConnectionsLikeProduction(t, svc, adminPrincipal(tenantID, userID))

	var minted setupClientMintResponse
	if code := mcpDoJSON(t, eng, http.MethodPost, mcp.ConnectionsPath, map[string]any{
		"name":            "set up for one, connected as another",
		"site_scope_mode": "all",
		"setup_client":    "claude-desktop",
	}, nil, &minted); code != http.StatusCreated {
		t.Fatalf("mint answered %d, want 201", code)
	}

	// CONTROL: it is "claude-desktop" BEFORE the connect. Without this the
	// assertion after the connect proves nothing -- a column that was never
	// written also reads "unchanged".
	before := setupClientFor(t, eng, minted.GrantID)
	if before == nil || *before != "claude-desktop" {
		t.Fatalf("CONTROL: setup_client before connect = %v, want \"claude-desktop\"; "+
			"nothing below can prove RecordConnect left it alone", derefOrNil(before))
	}

	// Connect, reporting a DIFFERENT client, through the real authenticate +
	// RecordConnect pair the transport calls.
	auth, err := svc.Authenticate(ctx, minted.Token)
	if err != nil {
		t.Fatalf("CONTROL: the freshly minted token does not authenticate, so "+
			"RecordConnect never runs and this proof is vacuous: %v", err)
	}
	proto := "2025-06-18"
	if err := svc.RecordConnect(ctx, auth, "Cursor", "1.4.2", &proto); err != nil {
		t.Fatalf("RecordConnect: %v", err)
	}

	// CONTROL: RecordConnect actually wrote something. If it silently did
	// nothing, "setup_client unchanged" would be true for the wrong reason.
	after := connectionRowFor(t, eng, minted.GrantID)
	if after.ReportedClientName == nil || *after.ReportedClientName != "Cursor" {
		t.Fatalf("CONTROL: reported_client_name = %v after RecordConnect, want "+
			"\"Cursor\"; RecordConnect did not write, so the assertion below "+
			"would pass against an inert call", derefOrNil(after.ReportedClientName))
	}

	if after.SetupClient == nil {
		t.Fatal("RecordConnect NULLED setup_client. The operator's step-2 " +
			"choice is gone and step 9 can no longer say what this connection " +
			"was set up for.")
	}
	if *after.SetupClient != "claude-desktop" {
		t.Fatalf("RecordConnect OVERWROTE setup_client: %q -> %q. These are two "+
			"different facts about two different actors -- the operator's "+
			"choice and the client's self-report -- and they disagree "+
			"legitimately. Neither may overwrite the other (m128 DECISION 1).",
			"claude-desktop", *after.SetupClient)
	}
	t.Logf("PROOF 3 ok: connected as %q, setup_client still %q",
		*after.ReportedClientName, *after.SetupClient)
}

// ---------------------------------------------------------------------------
// PROOF 4 -- TENANT ISOLATION ON THE NEW READ.
//
// setup_client is a new column on the connections list, so it is a new thing
// that could leak. The list is read by an admin of tenant B and must not carry
// tenant A's row at all -- the whole connection, not merely the column.
// ---------------------------------------------------------------------------

func TestMCPSetupClientListIsTenantIsolatedAsAppRole(t *testing.T) {
	pool := startPostgres(t)
	svc := mcp.NewService(mcp.NewRepo(pool))

	suffixA := uuid.NewString()[:8]
	tenantA := seedTenant(t, pool, "mcp-m128-a-"+suffixA)
	userA := seedUserRow(t, pool, "mcp-m128-a-"+suffixA+"@example.test")
	engA := mountConnectionsLikeProduction(t, svc, adminPrincipal(tenantA, userA))

	suffixB := uuid.NewString()[:8]
	tenantB := seedTenant(t, pool, "mcp-m128-b-"+suffixB)
	userB := seedUserRow(t, pool, "mcp-m128-b-"+suffixB+"@example.test")
	engB := mountConnectionsLikeProduction(t, svc, adminPrincipal(tenantB, userB))

	var mintedA setupClientMintResponse
	if code := mcpDoJSON(t, engA, http.MethodPost, mcp.ConnectionsPath, map[string]any{
		"name":            "tenant A windsurf",
		"site_scope_mode": "all",
		"setup_client":    "windsurf",
	}, nil, &mintedA); code != http.StatusCreated {
		t.Fatalf("tenant A mint answered %d, want 201", code)
	}

	// CONTROL: A can see its own row. Without this, B seeing nothing would be
	// explained just as well by the list being broken for everyone.
	if got := setupClientFor(t, engA, mintedA.GrantID); got == nil || *got != "windsurf" {
		t.Fatalf("CONTROL: tenant A cannot read back its own setup_client (%v); "+
			"the isolation assertion below would pass vacuously", derefOrNil(got))
	}

	var listB setupClientListResponse
	if code := mcpDoJSON(t, engB, http.MethodGet, mcp.ConnectionsPath, nil, nil, &listB); code != http.StatusOK {
		t.Fatalf("tenant B list answered %d, want 200", code)
	}
	for _, c := range listB.Connections {
		if c.ID == mintedA.GrantID {
			t.Fatalf("tenant B's connections list carries tenant A's grant %s "+
				"(setup_client=%v). RLS did not scope the read.",
				c.ID, derefOrNil(c.SetupClient))
		}
	}
	t.Logf("PROOF 4 ok: tenant B's list holds %d connection(s), none of them tenant A's",
		len(listB.Connections))
}

// ---------------------------------------------------------------------------
// PROOF 5 -- SHAPE IS REFUSED, AN UNKNOWN CLIENT IS NOT.
//
// The two halves belong in one test because each is the other's control. A
// server that refused everything unknown would pass the first half alone, and
// that is the failure m128 DECISION 3 argues against at length: the wizard
// breaking at step 2, at the end of a ten-step flow, for a client its own UI is
// offering. A server that validated nothing would pass the second half alone,
// and then 'Windsurf' and 'windsurf ' both land in the column and S31's
// equality filter says "none of them" while a matching row sits in the list.
// ---------------------------------------------------------------------------

func TestMCPSetupClientRefusesShapeButAcceptsUnknownClientAsAppRole(t *testing.T) {
	pool := startPostgres(t)
	svc := mcp.NewService(mcp.NewRepo(pool))
	suffix := uuid.NewString()[:8]
	tenantID := seedTenant(t, pool, "mcp-m128-shape-"+suffix)
	userID := seedUserRow(t, pool, "mcp-m128-shape-"+suffix+"@example.test")
	eng := mountConnectionsLikeProduction(t, svc, adminPrincipal(tenantID, userID))

	// (a) MALFORMED IS A 422, not a 5xx from a 23514 at the INSERT. The wizard
	// must be able to point at the field.
	//
	// 422 AND NOT 400 BECAUSE THAT IS THE HOUSE MAPPING, not a choice this
	// endpoint made: domain.HTTPStatus sends every KindValidation to
	// StatusUnprocessableEntity (internal/domain/errors.go), which is what the
	// blank-name and malformed-site-scope refusals on this same route already
	// answer. Asserting 400 here would have pinned this one field to a status
	// no other refusal on the endpoint uses -- the first draft did exactly
	// that and this run is what corrected it.
	//
	// The status is asserted rather than "any 4xx" because 4xx-in-general
	// includes 401 and 403, and a refusal that turned out to be an auth failure
	// would satisfy a looser check while proving nothing about validation.
	for _, bad := range []string{"Windsurf", "windsurf ", "windsurf_beta", "", "-lead", "trail-"} {
		var ignored map[string]any
		code := mcpDoJSON(t, eng, http.MethodPost, mcp.ConnectionsPath, map[string]any{
			"name":            "malformed " + bad,
			"site_scope_mode": "all",
			"setup_client":    bad,
		}, nil, &ignored)
		if code != http.StatusUnprocessableEntity {
			t.Errorf("setup_client=%q answered %d, want 422. Anything else means "+
				"either a malformed slug was STORED -- and S31's equality filter "+
				"can no longer be trusted -- or the database CHECK surfaced as a "+
				"5xx instead of a field-level refusal.", bad, code)
		}
	}

	// (b) A WELL-FORMED SLUG THE SERVER HAS NEVER HEARD OF IS ACCEPTED AND
	// STORED. This is a legitimate state, not an error: the vocabulary lives in
	// the frontend's client-table.ts and a client added there must not need a
	// control-plane release. It renders as the generic panel, which the
	// wireframes call a complete path rather than a placeholder.
	const unknown = "some-future-client"
	var minted setupClientMintResponse
	if code := mcpDoJSON(t, eng, http.MethodPost, mcp.ConnectionsPath, map[string]any{
		"name":            "a client the server has never heard of",
		"site_scope_mode": "all",
		"setup_client":    unknown,
	}, nil, &minted); code != http.StatusCreated {
		t.Fatalf("a well-formed but unrecognised slug answered %d, want 201. The "+
			"server must not hold a second copy of the nine-id vocabulary: a "+
			"stale copy refuses, at step 2, a client the wizard is offering.", code)
	}
	got := setupClientFor(t, eng, minted.GrantID)
	if got == nil || *got != unknown {
		t.Fatalf("unrecognised slug stored as %v, want %q", derefOrNil(got), unknown)
	}
	t.Logf("PROOF 5 ok: malformed shapes refused 400; unrecognised %q stored intact", unknown)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// ginEngine names the mounted router these helpers drive. Spelled as a local
// alias so the helper signatures read as "the production mount", not as a bare
// third-party type.
type ginEngine = *gin.Engine

// setupClientFor reads one connection's setup_client back OFF THE LIST ROUTE,
// returning nil for JSON null.
func setupClientFor(t *testing.T, eng ginEngine, grantID string) *string {
	t.Helper()
	return connectionRowFor(t, eng, grantID).SetupClient
}

// connectionRow is one decoded row of the list response.
type connectionRow struct {
	ID                 string
	SetupClient        *string
	ReportedClientName *string
}

// connectionRowFor GETs the mounted list route and returns the named row. It
// fails rather than returning a zero value when the row is missing: a missing
// connection and a connection with a null column are different answers, and
// collapsing them would let a caller read "null" from a list that never had the
// row at all.
func connectionRowFor(t *testing.T, eng ginEngine, grantID string) connectionRow {
	t.Helper()
	var list setupClientListResponse
	if code := mcpDoJSON(t, eng, http.MethodGet, mcp.ConnectionsPath, nil, nil, &list); code != http.StatusOK {
		t.Fatalf("list answered %d, want 200", code)
	}
	for _, c := range list.Connections {
		if c.ID == grantID {
			return connectionRow{
				ID:                 c.ID,
				SetupClient:        c.SetupClient,
				ReportedClientName: c.ReportedClientName,
			}
		}
	}
	t.Fatalf("grant %s is absent from its own organisation's connections list "+
		"(%d row(s) returned); nothing about its columns can be asserted",
		grantID, len(list.Connections))
	return connectionRow{}
}

// derefOrNil renders a *string for a failure message without panicking on nil,
// so a failed assertion prints "<nil>" rather than crashing the run and hiding
// every later proof in the file.
func derefOrNil(s *string) any {
	if s == nil {
		return "<nil>"
	}
	return *s
}
