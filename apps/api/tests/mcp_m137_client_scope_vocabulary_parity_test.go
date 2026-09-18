// m137 added mcp_oauth_clients.registered_scopes and closed its vocabulary with
// a CHECK. recognisedScopes in apps/api/internal/mcp/scope.go is the other
// closed set, and m137 DECISION 2(c) and section (5)(f) owe this file: two
// closed sets with two answers is worse than one open set unless something
// EXECUTES the comparison.
//
// ===========================================================================
// THERE ARE NOW TWO CONSTRAINTS TRACKING recognisedScopes. WIDENING THE
// REGISTRY MEANS WIDENING BOTH.
// ===========================================================================
//
//	mcp_grants_oauth_scopes_vocabulary_check          (m136, on mcp_grants)
//	mcp_oauth_clients_registered_scopes_vocabulary_check  (m137, here)
//
// They are checked by two files -- this one and
// mcp_m136_scope_vocabulary_parity_test.go -- each of which names both, so a
// reader who arrives at either learns about the other.
//
// TWO IS ACCEPTABLE PRECISELY BECAUSE MISSING ONE FAILS CLOSED. A migration
// that widens only one leaves the other refusing the new scope with SQLSTATE
// 23514 at INSERT: loud, immediate, and at a named constraint. Nothing widens
// silently and no scope becomes grantable by half. Both constraint names were
// chosen to be STABLE, so a widening migration drops and re-adds THESE names
// rather than introducing new ones -- exactly as m131 did for capabilities.
//
// WHY THE SECOND ONE EXISTS AT ALL, rather than deriving the client column from
// the grant column: they bound different things. m136's column records what a
// GRANT was issued for; m137's records what a CLIENT may ever ask for, and it
// is the right-hand side of the containment comparison Authorize and Approve
// perform (internal/mcp/service.go, requireScopesWithinRegistration). A value
// the database would refuse on one column and accept on the other would mean
// the comparison could be fed something the grant could never store.
//
// THE HAZARD THIS FILE CATCHES IS m131 DECISION 5's, and on this column it has
// its own shape. A scope the DATABASE accepts and Go does not can be stored in
// registered_scopes and then never matches anything, because containment
// compares against ParseRequestedScopes' output and that output can only hold
// scopes Go recognises -- so the client is registered for something it can
// never request, silently, with no error anywhere. A scope GO accepts and the
// database does not is a 23514 at registration: RegisterMCPOAuthClient writes
// registeredScopesForOmittedRequest(), which is SupportedScopes(), so a Go-only
// scope breaks EVERY dynamic client registration on the installation rather
// than one request.
//
// THE EXTRACTION IS m131's AND m136's MECHANISM, WITH THE REGCLASS CHANGED AND
// NOTHING ELSE -- see m137ClientVocabularyExtraction, which says why it is a
// separate constant rather than the shared one. That is still what m137
// DECISION 2(c) chose text[]-plus-CHECK to buy: one mechanism covering three
// constraints, rather than a second catalogue query for an enum.
// pg_get_constraintdef renders the STORED expression tree, so this reads what
// the database enforces and cannot be fooled by editing the migration file's
// text or its comments.
//
// THE FAILURE MODE THAT LOOKS LIKE A PASS IS CHECKED FIRST. Rename or drop the
// constraint and the extraction matches nothing: both difference arms come back
// empty and "no differences" reads as "the sets agree" while nothing has been
// compared. That is INDETERMINATE and it fails, before either difference is
// computed. A guard that cannot find its subject must go red.
package tests

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// m137ClientScopeConstraint is the name the extraction looks up AND the name
// the INDETERMINATE message prints. One constant, bound as $1 rather than
// interpolated, for m131's and m136's reason: a failure message naming a
// constraint the query did not look for sends the reader to the wrong place,
// and the whole point of the INDETERMINATE arm is that the reader believes it.
const m137ClientScopeConstraint = "mcp_oauth_clients_registered_scopes_vocabulary_check"

// m137ClientVocabularyExtraction is m131VocabularyExtraction with ONE token
// changed: the regclass. It is not shared with the m131 and m136 tests because
// that constant pins `c.conrelid = 'public.mcp_grants'::regclass` and
// parameterises only the constraint NAME -- both of those constraints live on
// mcp_grants, and this one does not.
//
// THAT DETAIL IS NOT PEDANTRY, IT IS THE BUG THIS FILE HIT ON ITS FIRST RUN.
// Reusing the shared constant with only the conname changed looks right, runs
// without error, and returns ZERO ROWS: the name exists, but not on mcp_grants.
// The INDETERMINATE arm below is what turned that into a failure instead of a
// green test that had compared nothing -- which is precisely the failure mode
// it was written for, arriving before the constraint it guards ever drifted.
// m137 DECISION 2(c) did say "the same extraction works verbatim with only the
// regclass and conname changed"; the regclass is the half that is easy to miss,
// because nothing about the query looks table-specific at the call site.
const m137ClientVocabularyExtraction = `
	SELECT m[1] AS scope
	  FROM pg_constraint c
	  CROSS JOIN LATERAL regexp_matches(
	           pg_get_constraintdef(c.oid), '''([^'']*)''::text', 'g') AS m
	 WHERE c.conrelid = 'public.mcp_oauth_clients'::regclass
	   AND c.conname  = $1`

// TestClientRegisteredScopeVocabularyMatchesTheDatabaseCheckAsAppRole is the
// parity proof for m137's column.
//
// ON THE ROLE, PLAINLY, AND WITHOUT OVERCLAIMING -- the m131 and m136 parity
// tests say the same and it is still true here: pg_constraint and
// pg_get_constraintdef are readable by every role, carry no RLS, and a CHECK is
// enforced identically against a superuser and against wpmgr_app, so this
// particular read would return the same strings connected as anyone. It is
// asserted and printed anyway, for two reasons that are real. First, it proves
// the constraint exists in the database the application actually connects to
// rather than in some other one a privileged fixture reached. Second, the tests
// this file sits beside INSERT and AUTHENTICATE, where the role is entirely
// load-bearing, and a file whose evidence is uniform is one where nobody has to
// work out which transaction was the honest one.
func TestClientRegisteredScopeVocabularyMatchesTheDatabaseCheckAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m137-parity-"+uuid.NewString()[:8])

	var inDB []string
	// Through InTenantTx -- a production dispatch -- and not a connection this
	// test opened. A test that dials its own connection proves something about
	// a database, not about the one the request path reaches.
	//
	// mcp_oauth_clients carries no tenant_id (m124's opening question), so the
	// tenant here is scaffolding for the helper rather than a scope on the
	// read; the catalogue tables it queries are not tenant-scoped either.
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (client registered-scope vocabulary parity)")
		rows, err := tx.Query(ctx, m137ClientVocabularyExtraction, m137ClientScopeConstraint)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			inDB = append(inDB, s)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read %s: %v", m137ClientScopeConstraint, err)
	}

	// THE INDETERMINATE ARM, CHECKED BEFORE EITHER DIFFERENCE.
	//
	// Zero rows means the constraint was not found or its definition no longer
	// renders as quoted ::text literals -- a rename, a drop, a schema change,
	// or an extraction that has stopped matching. Every arm below would then be
	// empty and this test would report a pass having compared nothing against
	// nothing.
	if len(inDB) == 0 {
		t.Fatalf("INDETERMINATE: the extraction returned no rows, so this test "+
			"compared nothing.\n"+
			"Looked for constraint %q on public.mcp_oauth_clients.\n"+
			"This is NOT 'the sets agree' -- both difference arms are empty "+
			"because there is nothing to difference against. Either the "+
			"constraint was renamed or dropped, or pg_get_constraintdef no "+
			"longer renders the vocabulary as quoted ::text literals. Fix the "+
			"extraction or the constraint; do not read this as a pass.",
			m137ClientScopeConstraint)
	}

	sort.Strings(inDB)
	inGo := append([]string(nil), mcp.SupportedScopes()...)
	sort.Strings(inGo)

	// A SECOND EMPTY-SET GUARD, ON THE OTHER SIDE. SupportedScopes ranges over
	// recognisedScopes, so an emptied registry would make the "in Go but not in
	// the database" arm vacuously empty in exactly the same way.
	if len(inGo) == 0 {
		t.Fatal("INDETERMINATE: mcp.SupportedScopes() is empty, so the Go half " +
			"of this comparison contributed nothing")
	}

	dbSet := map[string]struct{}{}
	for _, s := range inDB {
		dbSet[s] = struct{}{}
	}
	goSet := map[string]struct{}{}
	for _, s := range inGo {
		goSet[s] = struct{}{}
	}

	// FAILURE MODE 1: Go recognises a scope the CHECK does not. Registration
	// writes SupportedScopes() into registered_scopes, so EVERY dynamic client
	// registration on the installation takes 23514 -- not one request, all of
	// them, at an unauthenticated endpoint with no operator to read the error.
	var goOnly []string
	for _, s := range inGo {
		if _, ok := dbSet[s]; !ok {
			goOnly = append(goOnly, s)
		}
	}

	// FAILURE MODE 2: the CHECK holds a scope Go does not recognise. The column
	// stores it and containment can never match it, because the requested set
	// it is compared against comes from ParseRequestedScopes and holds only
	// scopes Go recognises. The client is registered for something it can never
	// request, and nothing anywhere says so.
	var dbOnly []string
	for _, s := range inDB {
		if _, ok := goSet[s]; !ok {
			dbOnly = append(dbOnly, s)
		}
	}

	if len(goOnly) > 0 || len(dbOnly) > 0 {
		t.Fatalf("the two closed scope vocabularies disagree on %s.\n"+
			"  in Go but NOT in the CHECK: %v\n"+
			"      -> registration writes SupportedScopes() into "+
			"registered_scopes, so EVERY dynamic client registration takes "+
			"23514 at an unauthenticated endpoint\n"+
			"  in the CHECK but NOT in Go:  %v\n"+
			"      -> the column stores it and containment can never match it: "+
			"a client registered for a scope it can never request, with no "+
			"error anywhere\n"+
			"  Go:       %v\n"+
			"  database: %v\n"+
			"Widening either set is a change to the other -- AND TO "+
			"mcp_grants_oauth_scopes_vocabulary_check, which tracks the same Go "+
			"registry on mcp_grants and is checked by "+
			"mcp_m136_scope_vocabulary_parity_test.go. The database half is a "+
			"migration (database-engineer); the Go half is recognisedScopes in "+
			"apps/api/internal/mcp/scope.go -- and a scope that is not a read "+
			"scope owes its own review before any of them move.",
			m137ClientScopeConstraint, goOnly, dbOnly, inGo, inDB)
	}

	t.Logf("client registered-scope vocabulary agrees with Go on %d scopes, read "+
		"from pg_get_constraintdef inside InTenantTx: %s",
		len(inDB), strings.Join(inDB, " "))
}
