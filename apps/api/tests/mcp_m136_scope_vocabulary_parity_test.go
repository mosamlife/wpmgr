// m136 added mcp_grants.oauth_scopes and closed its vocabulary with a CHECK.
// recognisedScopes in apps/api/internal/mcp/scope.go is the other closed set,
// and m136 DECISION 2(c) and DECISION 5 owe this file: two closed sets with two
// answers is worse than one open set unless something EXECUTES the comparison.
//
// THE HAZARD IS m131 DECISION 5's, ONE COLUMN OVER. A scope the database
// accepts and Go does not is stored and then refused at a different layer with
// a different error -- and because grantScopes deliberately does NOT filter, it
// is refused as a WHOLE-SET refusal at OrgDefaultCapabilities, which is correct
// but is a 403 on a live credential rather than a failure at write time. A
// scope Go accepts and the database does not is a 23514 at INSERT, on the
// consent path, after the operator already approved the connection.
//
// THE EXTRACTION IS m131's, VERBATIM AND NOT A REWRITE. It is the same query
// against a different constraint name -- that is exactly what m136 DECISION
// 2(c) chose text[]-plus-CHECK to buy, one mechanism covering two constraints
// rather than a second catalogue query for an enum. pg_get_constraintdef
// renders the STORED expression tree, so this reads what the database enforces
// and cannot be fooled by editing the migration file's text or its comments.
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

// m136ScopeConstraint is the name the extraction looks up AND the name the
// INDETERMINATE message prints. One constant, bound as $1 rather than
// interpolated, for m131's reason: a failure message naming a constraint the
// query did not look for sends the reader to the wrong place, and the whole
// point of the INDETERMINATE arm is that the reader believes it.
//
// m136 chose this name to be STABLE: a later migration widening the vocabulary
// must drop and re-add THIS name, exactly as m131 did for capabilities.
const m136ScopeConstraint = "mcp_grants_oauth_scopes_vocabulary_check"

// TestScopeVocabularyMatchesTheDatabaseCheckAsAppRole is the parity proof.
//
// ON THE ROLE, PLAINLY, AND WITHOUT OVERCLAIMING -- the sibling capability
// parity test says the same and it is still true here: pg_constraint and
// pg_get_constraintdef are readable by every role, carry no RLS, and a CHECK is
// enforced identically against a superuser and against wpmgr_app, so this
// particular read would return the same strings connected as anyone. It is
// asserted and printed anyway, for two reasons that are real. First, it proves
// the constraint exists in the database the application actually connects to
// rather than in some other one a privileged fixture reached. Second, the tests
// this file sits beside INSERT and AUTHENTICATE, where the role is entirely
// load-bearing, and a file whose evidence is uniform is one where nobody has to
// work out which transaction was the honest one.
func TestScopeVocabularyMatchesTheDatabaseCheckAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m136-parity-"+uuid.NewString()[:8])

	var inDB []string
	// Through InTenantTx -- a production dispatch -- and not a connection this
	// test opened. A test that dials its own connection proves something about
	// a database, not about the one the request path reaches.
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (scope vocabulary parity)")
		rows, err := tx.Query(ctx, m131VocabularyExtraction, m136ScopeConstraint)
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
		t.Fatalf("read %s: %v", m136ScopeConstraint, err)
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
			"Looked for constraint %q on public.mcp_grants.\n"+
			"This is NOT 'the sets agree' -- both difference arms are empty "+
			"because there is nothing to difference against. Either the "+
			"constraint was renamed or dropped, or pg_get_constraintdef no "+
			"longer renders the vocabulary as quoted ::text literals. Fix the "+
			"extraction or the constraint; do not read this as a pass.",
			m136ScopeConstraint)
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

	// FAILURE MODE 1: Go recognises a scope the CHECK does not. ParseRequested-
	// Scopes accepts it at the authorize endpoint and the operator approves the
	// connection; the INSERT then takes 23514, after consent was given.
	var goOnly []string
	for _, s := range inGo {
		if _, ok := dbSet[s]; !ok {
			goOnly = append(goOnly, s)
		}
	}

	// FAILURE MODE 2: the CHECK holds a scope Go does not recognise. The
	// database stores it, and grantScopes carries it through UNFILTERED, so
	// OrgDefaultCapabilities refuses the whole set -- a live credential that
	// authenticates nowhere, with no write-time failure anywhere to point at.
	var dbOnly []string
	for _, s := range inDB {
		if _, ok := goSet[s]; !ok {
			dbOnly = append(dbOnly, s)
		}
	}

	if len(goOnly) > 0 || len(dbOnly) > 0 {
		t.Fatalf("the two closed scope vocabularies disagree.\n"+
			"  in Go but NOT in the CHECK: %v\n"+
			"      -> the authorize endpoint accepts it and the INSERT takes "+
			"23514, after the operator approved the connection\n"+
			"  in the CHECK but NOT in Go:  %v\n"+
			"      -> the row stores it, grantScopes carries it through "+
			"unfiltered, and OrgDefaultCapabilities refuses the WHOLE set: a "+
			"credential that authenticates nowhere and fails at no write\n"+
			"  Go:       %v\n"+
			"  database: %v\n"+
			"Widening either set is a change to the other. The database half is "+
			"a migration (database-engineer); the Go half is recognisedScopes "+
			"in apps/api/internal/mcp/scope.go -- and a scope that is not a read "+
			"scope owes its own review before either moves.",
			goOnly, dbOnly, inGo, inDB)
	}

	t.Logf("scope vocabularies agree on %d scopes, read from "+
		"pg_get_constraintdef inside InTenantTx: %s",
		len(inDB), strings.Join(inDB, " "))
}
