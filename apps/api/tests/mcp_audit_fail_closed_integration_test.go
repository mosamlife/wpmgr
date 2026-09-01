// mcp_audit_fail_closed_integration_test.go: ADR-061 A10's fail-closed half,
// against a real database, as the role every install runs as.
//
// WHY THIS FILE EXISTS SEPARATELY FROM THE UNIT PROOFS. internal/mcp proves the
// same property against a fake recorder that returns an error on command. That
// proves Go error propagation and NOTHING BELOW IT: it cannot fail for the
// reason production would fail, it never touches audit_log, and it would pass
// unchanged if the append were routed to a table that does not exist. "No
// answer is served whose audit record was lost" is a claim about a database, so
// it is proven against one here.
//
// HOW THE APPEND IS MADE TO FAIL, AND WHY THIS MECHANISM. The superuser pool
// REVOKEs INSERT on audit_log from the application role. That is honest in the
// way a closed pool is not: the connection stays healthy, the tenant's rows
// stay readable, the tool's own read still succeeds, and the ONLY thing that
// breaks is the audit append -- which is precisely the production failure this
// posture exists for, and precisely the one a fail-open surface would answer
// straight through. A closed pool would also break the read, and a test where
// the read fails cannot tell "the answer was withheld" from "there was no
// answer to withhold".
//
// The grant is restored afterwards and the SAME request is replayed, so the
// over-fire half runs against the same database, the same connection and the
// same bearer token: the only variable between "no answer" and "an answer" is
// whether audit_log would accept the row.
//
// THE ROLE IS ASSERTED AND PRINTED FROM INSIDE THE TRANSACTION UNDER TEST. A
// REVOKE against wpmgr_app is inert if the code under test connects as a
// superuser or a BYPASSRLS role, and this whole file would then pass by
// answering every call successfully in both halves.
package tests

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// auditedMCPService is the ONE way this package builds an mcp.Service, and it
// exists because thirteen integration tests were building one without a
// recorder and exercising consent, mint and revoke against it.
//
// THOSE THIRTEEN FAILURES WERE THE GATE WORKING. Under ADR-061 A10 and the
// owner's ruling, a Service with no recorder may not approve, mint or revoke:
// an unaudited assistant surface presenting as a working one is the state the
// ruling exists to eliminate. The fixtures had been silently in that state, and
// making it loud is what surfaced them. The fix is therefore to wire the
// recorder production always wires -- cmd/wpmgr/main.go has never not called
// WithAudit -- and NOT to soften Service.requireRecorder.
//
// One helper, deliberately, rather than a second one alongside internal/mcp's
// auditedService: two helpers that must agree is a shape this repo has produced
// repeatedly, and they drift on the day one of them gains an argument. This is
// the integration-side equivalent and the only one in package tests.
//
// The store is passed rather than derived because callers already hold one --
// several assert against the same repo they hand in -- and re-deriving it here
// would give them a Service talking to a different object than the one they
// then inspect.
func auditedMCPService(pool *db.Pool, store mcp.Store) *mcp.Service {
	return mcp.NewService(store).WithAudit(audit.NewRecorder(pool, domain.SystemClock{}))
}

// auditInsertGrant flips INSERT on audit_log for the application role, through
// the superuser pool. It returns nothing and fails loudly: a REVOKE that
// silently did not apply would make the fail-closed half of this test pass for
// the wrong reason -- the append would succeed, the answer would be served, and
// only the assertion's wording would be wrong.
func auditInsertGrant(t *testing.T, admin *db.Pool, appRole string, allow bool) {
	t.Helper()
	verb := "REVOKE INSERT ON audit_log FROM " + pgQuoteIdent(appRole)
	if allow {
		verb = "GRANT INSERT ON audit_log TO " + pgQuoteIdent(appRole)
	}
	if _, err := admin.Exec(context.Background(), verb); err != nil {
		t.Fatalf("SETUP FAILURE: %q: %v", verb, err)
	}
	// Read the privilege back rather than trusting the DDL returned no error:
	// this is the single fact the fail-closed half depends on.
	var has bool
	if err := admin.QueryRow(context.Background(),
		`SELECT has_table_privilege($1, 'audit_log', 'INSERT')`, appRole).Scan(&has); err != nil {
		t.Fatalf("SETUP FAILURE: read back audit_log INSERT privilege: %v", err)
	}
	if has != allow {
		t.Fatalf("SETUP FAILURE: after %q, has_table_privilege(%q,'audit_log','INSERT') = %v, want %v",
			verb, appRole, has, allow)
	}
	t.Logf("audit_log INSERT for %q is now %v (verified with has_table_privilege)", appRole, has)
}

// pgQuoteIdent double-quotes an identifier read out of pg_roles. The role name
// comes from the database, not from a request, but interpolating an identifier
// unquoted is the habit that eventually meets one that needs it.
func pgQuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// TestMCPAuditFailClosed_NoAnswerIsServedWhenTheAppendCannotCommit is the
// proof the unit tests cannot give.
func TestMCPAuditFailClosed_NoAnswerIsServedWhenTheAppendCannotCommit(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()

	// THE ROLE, FROM INSIDE A TRANSACTION THE REQUEST PATH ITSELF OPENS, and
	// captured here because the REVOKE below has to name it. Reading it from
	// the database rather than hard-coding "wpmgr_app" means this proof cannot
	// drift into revoking a privilege from a role nothing connects as.
	var appRole string
	var super, bypass bool
	if err := pool.InTenantTx(ctx, uuid.Nil, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT current_user, rolsuper, rolbypassrls
			   FROM pg_roles WHERE rolname = current_user`).Scan(&appRole, &super, &bypass)
	}); err != nil {
		t.Fatalf("read current role inside the tx under test: %v", err)
	}
	t.Logf("connected as %q rolsuper=%v rolbypassrls=%v", appRole, super, bypass)
	if super || bypass {
		t.Fatalf("this proof runs as %q with rolsuper=%v rolbypassrls=%v; either privilege "+
			"makes the REVOKE below inert and every assertion in this file vacuous",
			appRole, super, bypass)
	}

	suffix := uuid.NewString()[:8]
	tenantID := seedTenant(t, pool, "mcp-failclosed-"+suffix)
	userID := seedUserRow(t, admin, "mcp-failclosed-"+suffix+"@example.test")
	siteID := seedSite(t, pool, tenantID, "https://"+suffix+".failclosed.example.test")
	t.Logf("seeded tenant=%s site=%s", tenantID, siteID)

	rec := audit.NewRecorder(pool, domain.SystemClock{})
	svc := mcp.NewService(mcp.NewRepo(pool)).WithAudit(rec)

	principal := domain.Principal{UserID: userID, TenantID: tenantID, Role: "admin", Scope: domain.ScopeOrg}
	connEng := mountConnectionsLikeProduction(t, svc, principal)
	transportEng := mountLikeProduction(t, svc, principal)

	_, token := mintForRefusal(t, connEng, "fail-closed proof connection", map[string]any{
		"site_scope_mode": string(mcp.SiteScopeModeAll),
	})

	call := map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": mcp.ToolListSites, "arguments": map[string]any{}},
	}

	// ------------------------------------------------------------------
	// RED: audit_log will not accept the row. The answer must not be served.
	// ------------------------------------------------------------------
	auditInsertGrant(t, admin, appRole, false)

	broken := mcpRefusalRPC(t, transportEng, token, "", call)
	t.Logf("append-refused response: HTTP %d body=%s", broken.status, broken.body)

	// THE LOAD-BEARING ASSERTION, and it is about the site payload rather than
	// the JSON-RPC code: a surface that serialised the answer and bolted an
	// error beside it would satisfy a code-only check while disclosing exactly
	// what A10 says must not be disclosed unrecorded.
	if strings.Contains(broken.body, suffix) {
		t.Fatalf("the tool's answer was served while audit_log was refusing the row; "+
			"this is the fail-open behaviour A10 forbids\nbody: %s", broken.body)
	}
	if !strings.Contains(broken.body, "-32603") {
		t.Errorf("want the internal code -32603 when the append cannot commit, got: %s", broken.body)
	}
	// The wire must not name the audit system: that is a poll on whether this
	// surface is currently recording.
	if strings.Contains(strings.ToLower(broken.body), "audit") {
		t.Errorf("the response names the audit system, handing a caller a liveness "+
			"oracle: %s", broken.body)
	}

	// AND NOTHING LANDED. Counted as the app role, through the tenant helper,
	// not inferred from the response.
	if rows := queryMCPAuditRowsAsAppRole(t, pool, tenantID, audit.ActionMCPToolCalled); len(rows) != 0 {
		t.Fatalf("mcp.tool.called rows while INSERT was revoked = %d, want 0", len(rows))
	}

	// ------------------------------------------------------------------
	// GREEN: restore the privilege, replay the SAME request.
	// ------------------------------------------------------------------
	auditInsertGrant(t, admin, appRole, true)

	healthy := mcpRefusalRPC(t, transportEng, token, "", call)
	if healthy.status != http.StatusOK {
		t.Fatalf("HTTP %d after the privilege was restored: %s", healthy.status, healthy.body)
	}
	if !strings.Contains(healthy.body, suffix) {
		t.Fatalf("the seeded site is missing from the answer on a healthy audit path; "+
			"the guard is over-firing on correct work: %s", healthy.body)
	}

	// EXACTLY ONE ROW, counted with a query. Zero would mean the fail-closed
	// change quietly stopped recording; two would mean the record is written on
	// both sides of the gate. Both are silent defects that "the call succeeded"
	// cannot see.
	called := queryMCPAuditRowsAsAppRole(t, pool, tenantID, audit.ActionMCPToolCalled)
	if len(called) != 1 {
		t.Fatalf("after ONE successful tool call, mcp.tool.called rows = %d, want exactly 1", len(called))
	}
	if called[0].actorType != audit.ActorAssistant {
		t.Errorf("actor_type = %q, want %q", called[0].actorType, audit.ActorAssistant)
	}
	if called[0].targetID != mcp.ToolListSites {
		t.Errorf("target_id = %q, want %q", called[0].targetID, mcp.ToolListSites)
	}
	t.Logf("GREEN: answer served, exactly %d mcp.tool.called row, actor=%s/%s",
		len(called), called[0].actorType, called[0].actorID)
}

// TestMCPAuditFailClosed_RefusalAndInternalErrorAreByteIdentical checks the
// disclosure claim the fail-closed posture rests on: a caller must not be able
// to tell "the audit log is down" from any other server-side failure, because
// that is a poll on whether this surface is currently recording.
//
// It compares the response to an UNRECORDABLE call against the response to a
// call that fails for an unrelated internal reason, byte for byte including the
// headers. Anything that differs is the oracle, and is reported rather than
// asserted away.
func TestMCPAuditFailClosed_RefusalAndInternalErrorAreByteIdentical(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()

	var appRole string
	var super, bypass bool
	if err := pool.InTenantTx(ctx, uuid.Nil, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT current_user, rolsuper, rolbypassrls
			   FROM pg_roles WHERE rolname = current_user`).Scan(&appRole, &super, &bypass)
	}); err != nil {
		t.Fatalf("read current role inside the tx under test: %v", err)
	}
	t.Logf("connected as %q rolsuper=%v rolbypassrls=%v", appRole, super, bypass)
	if super || bypass {
		t.Fatalf("proof runs as %q rolsuper=%v rolbypassrls=%v; the REVOKE would be inert",
			appRole, super, bypass)
	}

	suffix := uuid.NewString()[:8]
	tenantID := seedTenant(t, pool, "mcp-oracle-"+suffix)
	userID := seedUserRow(t, admin, "mcp-oracle-"+suffix+"@example.test")
	seedSite(t, pool, tenantID, "https://"+suffix+".oracle.example.test")

	rec := audit.NewRecorder(pool, domain.SystemClock{})
	svc := mcp.NewService(mcp.NewRepo(pool)).WithAudit(rec)
	principal := domain.Principal{UserID: userID, TenantID: tenantID, Role: "admin", Scope: domain.ScopeOrg}
	connEng := mountConnectionsLikeProduction(t, svc, principal)
	transportEng := mountLikeProduction(t, svc, principal)

	_, token := mintForRefusal(t, connEng, "oracle proof connection", map[string]any{
		"site_scope_mode": string(mcp.SiteScopeModeAll),
	})

	call := map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": mcp.ToolListSites, "arguments": map[string]any{}},
	}

	// (a) unrecordable: INSERT revoked, the read itself is fine.
	auditInsertGrant(t, admin, appRole, false)
	unrecordable := mcpRefusalRPC(t, transportEng, token, "", call)
	auditInsertGrant(t, admin, appRole, true)

	// (b) the shape ANY other server-side tool failure takes. This is a literal
	// rather than a second provoked failure, and the choice is deliberate after
	// the first attempt got it wrong: revoking SELECT on `sites` looked like an
	// unrelated internal failure but actually broke AUTHENTICATION, which
	// answers 401/500 from a different handler entirely and compares two things
	// that were never comparable. The bytes below are what
	// TransportHandler.toolError emits for an infra failure that is not a
	// domain error -- the branch every non-audit internal fault lands on -- so
	// matching them is exactly the claim being made: an audit outage is
	// indistinguishable from any other server-side failure.
	//
	// Written out in full, with the same request id, so a drift in the code, the
	// message or the envelope fails here loudly instead of being paraphrased.
	const genericInternal = `{"jsonrpc":"2.0","id":7,"error":{"code":-32603,"message":"the tool call failed"}}`

	t.Logf("(a) audit unavailable : HTTP %d body=%s", unrecordable.status, unrecordable.body)
	t.Logf("(b) generic internal  : HTTP %d body=%s", http.StatusOK, genericInternal)

	if unrecordable.status != http.StatusOK {
		t.Errorf("HTTP status = %d, want %d — a JSON-RPC error rides a 200 like every "+
			"other tool failure; a different status is itself the oracle",
			unrecordable.status, http.StatusOK)
	}
	if strings.TrimSpace(unrecordable.body) != genericInternal {
		t.Errorf("the unrecordable response is distinguishable from an ordinary internal "+
			"failure, so a caller can poll it to learn when the audit log is down:\n"+
			"  got  : %s\n  want : %s", unrecordable.body, genericInternal)
	}
	// Belt and braces on the specific leak the first run of this test caught:
	// a real append failure returns a TYPED domain error from internal/audit
	// (audit_insert_failed), and before auditFailure normalised it the wire
	// carried "failed to append audit entry" and the -32602 invalid-params code.
	for _, leak := range []string{"audit", "append", "-32602"} {
		if strings.Contains(strings.ToLower(unrecordable.body), leak) {
			t.Errorf("the response leaks %q: %s", leak, unrecordable.body)
		}
	}
}
