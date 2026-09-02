// adr061_a11_connection_tools_tenant_boundary_test.go: GET
// /api/v1/mcp/connections/:connectionId/tools -- the wizard's step 10 -- proven
// against the REAL SCHEMA, through the MOUNTED ROUTE, as wpmgr_app.
//
// WHY THIS FILE EXISTS AT ALL, GIVEN THE SIBLING PROOFS.
//
// The containment guard (scripts/check-mcp-site-containment.sh) fired on
// Store.GetGrant when this route landed, and the allowlist entry that answered
// it -- infra/mcp-site-containment-allowlist.txt, `PARAM GetGrant.grantID` --
// rests on one sentence: THE REQUEST SUPPLIES THE ID, IT DOES NOT SUPPLY THE
// TENANT. That is a claim about a database boundary, and a claim about a
// database boundary is worth exactly what the proof of it is worth.
//
// The coverage that existed when the entry was written was coverage BY
// ADJACENCY: Repo.GetGrant reuses getGrantTx, which the status snapshot also
// uses, and the status snapshot's table has a proven-live site-scope policy in
// adr064_s16_mcp_connections_integration_test.go. Every link in that chain is
// true today. None of them is anchored to THIS route, so all of them are one
// refactor of getGrantTx away from being true of something else. A proof should
// be pinned to the boundary it is about, not to an implementation detail that
// currently happens to be shared.
//
// WHAT THIS PROVES, AND IN WHAT ORDER. The four steps are deliberately
// sequenced so that each one is what makes the next one mean something:
//
//  1. CONTROL. The grant's OWN organisation reads the tool list and gets 200
//     with a non-empty list. Without this, the 404 in step 2 could equally mean
//     the route is broken, unmounted, or that the fixture never created a
//     grant. A 404 is the answer a genuinely absent id gets, so a bare 404
//     assertion proves nothing at all.
//
//  2. THE BOUNDARY. A DIFFERENT organisation's admin asks for the SAME grant
//     id, on the same route, at the same role, with the same permission. 404.
//     The only thing that differs between step 1 and step 2 is which tenant is
//     asking, so the 404 is attributable to the tenant boundary and to nothing
//     else.
//
//  3. NOT AN ORACLE. The same stranger asks for an id that does not exist
//     anywhere, and gets a BYTE-IDENTICAL response. If a foreign id and an
//     absent id could be told apart -- by status, by code, by message -- the
//     endpoint would confirm which connection ids exist in other
//     organisations, which is the disclosure the 404 is chosen to avoid.
//
//  4. THE TENANT PREDICATE REMOVED, ON PURPOSE. Steps 1-3 hold if EITHER the
//     query's `tenant_id = $1` predicate or the RLS policy is doing the work,
//     and a proof that cannot say which one is a proof that will go quiet when
//     one of them is deleted. So the last step strips the predicate: it runs
//     `SELECT count(*) FROM mcp_grants WHERE id = $1` -- no tenant column
//     mentioned -- through db.Pool as wpmgr_app. The grant's own tenant sees 1
//     (so the probe is not vacuously counting zero), and the stranger's tenant
//     sees 0. RLS alone is therefore sufficient, and the predicate is defence
//     in depth rather than the only lock.
//
// NO CONNECTION IS OPENED HERE THAT THE REQUEST PATH DOES NOT OPEN. Every read
// lands through db.Pool as wpmgr_app -- NOSUPERUSER, NOBYPASSRLS -- which
// mcpAssertAndReportRole asserts and prints from INSIDE the transactions
// actually used. A test that opened its own connection would leave every policy
// below inert and pass regardless; that is exactly how m112's proofs passed
// while the email domain was cross-site readable.
package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// mcpToolsPath builds the route under test from the SAME exported constant the
// router mounts, so a rename moves the test with the code rather than leaving a
// string here that 404s for the wrong reason -- which would make this whole
// file pass while testing nothing.
func mcpToolsPath(grantID uuid.UUID) string {
	return mcp.ConnectionsPath + "/" + grantID.String() + "/tools"
}

// mcpGetRaw performs the request and returns the status AND the raw body.
//
// The raw body is the point: step 3 compares a foreign id's response with an
// absent id's BYTE FOR BYTE, and a helper that decodes into a struct would
// discard exactly the difference that would make this endpoint an existence
// oracle -- an error code, a message, a field present in one and not the other.
func mcpGetRaw(t *testing.T, eng *gin.Engine, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, strings.NewReader(""))
	req.RemoteAddr = "203.0.113.7:5555"
	w := httptest.NewRecorder()
	eng.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// countGrantsIgnoringTenant counts a grant by id with NO tenant_id predicate,
// through db.Pool as wpmgr_app, inside the same InTenantTx shape the request
// path uses.
//
// THE MISSING PREDICATE IS THE WHOLE POINT and is not an oversight to be
// tidied. GetMCPGrant carries `WHERE tenant_id = $1 AND id = $2`; this probe
// deletes the first half of that and asks what is left holding the line. If the
// answer for a foreign tenant is still 0, RLS is live and independently
// sufficient. If it were 1, the predicate would be the only lock and every
// assertion above would be resting on one `AND`.
func countGrantsIgnoringTenant(t *testing.T, pool *db.Pool, asTenant, grantID uuid.UUID, where string) int64 {
	t.Helper()
	ctx := context.Background()
	var n int64
	err := pool.InTenantTx(ctx, asTenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, where)
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM mcp_grants WHERE id = $1`, grantID).Scan(&n)
	})
	if err != nil {
		t.Fatalf("%s: count grants ignoring tenant: %v", where, err)
	}
	return n
}

// ---------------------------------------------------------------------------
// THE PROOF.
// ---------------------------------------------------------------------------

func TestMCPConnectionToolsRefusesAForeignTenantsConnectionAsAppRole(t *testing.T) {
	pool := startPostgres(t)

	svc := auditedMCPService(pool, mcp.NewRepo(pool))

	// TWO REAL ORGANISATIONS, each produced by driving the WHOLE OAuth flow
	// through the mounted routes. Hand-inserted rows would skip every CHECK and
	// every policy the real credential crosses, and the thing under test is
	// what happens to a real credential.
	owner := connectRealGrant(t, pool, svc)
	stranger := connectRealGrant(t, pool, svc)
	if owner.tenantID == stranger.tenantID {
		t.Fatalf("fixture: both grants landed in tenant %s, so there is no "+
			"boundary to cross and every assertion below is vacuous", owner.tenantID)
	}
	if owner.grantID == stranger.grantID {
		t.Fatalf("fixture: both organisations produced grant id %s", owner.grantID)
	}
	t.Logf("fixture: owner tenant=%s grant=%s / stranger tenant=%s grant=%s",
		owner.tenantID, owner.grantID, stranger.tenantID, stranger.grantID)

	// -----------------------------------------------------------------------
	// STEP 1 -- CONTROL. The grant's own organisation reads its tool list.
	//
	// This has to come FIRST and it has to be non-empty. The refusal in step 2
	// is a 404, and 404 is also what a broken route, an unmounted handler and a
	// fixture that silently created nothing all return.
	// -----------------------------------------------------------------------
	ownerEng := mountConnectionsLikeProduction(t, svc,
		adminPrincipal(owner.tenantID, owner.userID))

	var ownerTools struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}
	code := mcpDoJSON(t, ownerEng, http.MethodGet, mcpToolsPath(owner.grantID), nil, nil, &ownerTools)
	if code != http.StatusOK {
		t.Fatalf("CONTROL: the grant's OWN organisation got %d from its tool "+
			"list, want 200. The refusal below would prove nothing against a "+
			"route that answers 404 to everybody.", code)
	}
	if len(ownerTools.Tools) == 0 {
		t.Fatalf("CONTROL: the grant's own organisation got an EMPTY tool list. " +
			"An endpoint that returns nothing to its owner cannot demonstrate " +
			"that it withholds anything from a stranger.")
	}
	t.Logf("CONTROL ok: the owning organisation reads %d tool(s) for grant %s",
		len(ownerTools.Tools), owner.grantID)

	// -----------------------------------------------------------------------
	// STEP 2 -- THE BOUNDARY. A different organisation, the SAME grant id.
	//
	// Same route, same permission (PermAPIKeyRead), same role (admin), same
	// org scope. The ONLY difference from step 1 is which tenant is asking, so
	// a 404 here is attributable to the tenant boundary and to nothing else.
	// -----------------------------------------------------------------------
	strangerEng := mountConnectionsLikeProduction(t, svc,
		adminPrincipal(stranger.tenantID, stranger.userID))

	foreignCode, foreignBody := mcpGetRaw(t, strangerEng, mcpToolsPath(owner.grantID))
	if foreignCode != http.StatusNotFound {
		t.Fatalf("CROSS-TENANT READ: an admin of tenant %s got %d for tenant "+
			"%s's connection %s. body: %s\n"+
			"This is the bypass ADR-061 A11 exists to stop: the request supplied "+
			"the connection id and the tenant that bounds the lookup did not come "+
			"from the authenticated principal.",
			stranger.tenantID, foreignCode, owner.tenantID, owner.grantID, foreignBody)
	}
	t.Logf("BOUNDARY ok: tenant %s reading tenant %s's connection answered 404",
		stranger.tenantID, owner.tenantID)

	// -----------------------------------------------------------------------
	// STEP 3 -- AND IT IS NOT AN ORACLE. An id that exists nowhere must answer
	// IDENTICALLY, or the difference tells a stranger which connection ids are
	// real in organisations they cannot read.
	// -----------------------------------------------------------------------
	absentID := uuid.New()
	absentCode, absentBody := mcpGetRaw(t, strangerEng, mcpToolsPath(absentID))
	if absentCode != http.StatusNotFound {
		t.Fatalf("an id that exists nowhere answered %d, want 404. body: %s",
			absentCode, absentBody)
	}
	if foreignBody != absentBody {
		t.Fatalf("a FOREIGN connection id and an ABSENT one answer differently, "+
			"so this endpoint confirms which ids exist in other organisations.\n"+
			"  foreign (%s): %s\n  absent  (%s): %s",
			owner.grantID, foreignBody, absentID, absentBody)
	}
	t.Logf("ORACLE ok: foreign and absent are byte-identical 404s: %s", foreignBody)

	// -----------------------------------------------------------------------
	// STEP 4 -- THE TENANT PREDICATE REMOVED. Which layer is actually holding?
	//
	// Everything above passes if EITHER `tenant_id = $1` or RLS is doing the
	// work. This asks the database with the predicate deleted.
	// -----------------------------------------------------------------------
	ownerSees := countGrantsIgnoringTenant(t, pool, owner.tenantID, owner.grantID,
		"InTenantTx(owner), tenant predicate removed")
	if ownerSees != 1 {
		t.Fatalf("PROBE CONTROL: with the tenant predicate removed, the grant's "+
			"OWN tenant counts %d rows for grant %s, want 1. A probe that counts "+
			"zero for everybody proves nothing about the stranger below.",
			ownerSees, owner.grantID)
	}
	t.Logf("PROBE CONTROL ok: with no tenant_id predicate, the owning tenant counts %d row", ownerSees)

	strangerSees := countGrantsIgnoringTenant(t, pool, stranger.tenantID, owner.grantID,
		"InTenantTx(stranger), tenant predicate removed")
	if strangerSees != 0 {
		t.Fatalf("RLS IS INERT ON mcp_grants: with the `tenant_id = $1` predicate "+
			"removed, tenant %s counts %d row(s) for tenant %s's grant %s. The "+
			"query's WHERE clause is then the ONLY thing separating two "+
			"organisations, and the allowlist entry for GetGrant.grantID "+
			"(infra/mcp-site-containment-allowlist.txt) claims two layers.",
			stranger.tenantID, strangerSees, owner.tenantID, owner.grantID)
	}
	t.Logf("PROBE ok: with no tenant_id predicate at all, the stranger's tenant "+
		"still counts %d rows -- RLS alone is sufficient", strangerSees)
}
