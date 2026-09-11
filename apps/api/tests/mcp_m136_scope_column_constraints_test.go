// m136 put four refusals on mcp_grants.oauth_scopes -- NOT NULL, a not-empty
// CHECK, a shape CHECK and a closed-vocabulary CHECK -- and until this file
// existed NOTHING EXECUTED ANY OF THEM.
//
// THE GAP THIS CLOSES, STATED PLAINLY. The scope vocabulary parity test one
// file over reads pg_get_constraintdef and compares two closed sets; it never
// inserts, so it proves the constraint's TEXT matches Go and not that the
// database refuses anything. Every other fixture in this package that names
// oauth_scopes passes the happy value {mcp:read}, so it exercises the accept
// path and no refusal. The comment on internal/mcp/authenticate_scope_column_test.go
// asserted this suite proved those refusals live as wpmgr_app. It did not.
// This file is that proof, written so the claim can be true.
//
// WHY IT MATTERS THAT THE REFUSALS ARE REAL. internal/mcp/policy.go's grantScopes
// deliberately does NOT filter: an unrecognised scope is carried through to
// OrgDefaultCapabilities, which refuses the WHOLE set rather than trimming it
// down. That design is only safe because the row is hard to write wrong in the
// first place -- and "hard to write wrong" is a property of the database, not
// of a comment about the database. A dropped or renamed constraint would leave
// every Go test in this repository green.
//
// THE ACCEPT ARM IS NOT OPTIONAL. A constraint set that refuses everything
// guards nothing, because the first legitimate write turns it off. The honest
// value is inserted last and must land.
//
// ON THE ROLE. Every transaction below is opened by db.Pool.RunTenantTx -- the
// same dispatch the grant insert uses -- and asserts current_user=wpmgr_app
// with neither rolsuper nor rolbypassrls INSIDE the transaction that runs the
// INSERT. A CHECK is enforced identically against any role, but RLS is not, and
// a fixture that reached this table as a superuser would prove nothing about
// the database an install actually connects to.
package tests

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// The four constraint names this file holds the database to. They are listed
// here once, as constants, for the m136 parity test's reason: a failure message
// naming a constraint the test did not look for sends the reader to the wrong
// place.
//
// m136 chose these names to be STABLE. A later migration widening the scope
// vocabulary must drop and re-add THESE names rather than introduce new ones,
// exactly as m131 did for capabilities.
const (
	m136ScopesNotEmptyConstraint   = "mcp_grants_oauth_scopes_not_empty_check"
	m136ScopesShapeConstraint      = "mcp_grants_oauth_scopes_shape_check"
	m136ScopesVocabularyConstraint = "mcp_grants_oauth_scopes_vocabulary_check"
)

// m136InsertGrantScopes runs ONE insert of a grant carrying the given
// oauth_scopes value and returns the raw error. Everything except the scope
// column is an ordinary healthy grant, so the scope value is the only variable.
//
// IT IS A DIRECT INSERT AND THAT IS DELIBERATE, not a shortcut past the repo.
// Three of the five refusals below are UNREACHABLE through mcp.Repo: an
// unrecognised scope is refused by ParseRequestedScopes before any SQL runs,
// and a nil or 17-member column cannot be expressed by CreateMCPGrantParams at
// all. A test that can only plant what the Go layer permits cannot check what
// the database does when something else writes the row -- which is precisely
// the independence m136 DECISION 5(d) asks for. The transaction is still the
// production dispatch and the role is still asserted inside it.
func m136InsertGrantScopes(
	t *testing.T, pool *db.Pool, tenantID uuid.UUID, where string, scopes any,
) error {
	t.Helper()
	ctx := context.Background()
	principal := domain.Principal{TenantID: tenantID, Scope: domain.ScopeOrg}

	return pool.RunTenantTx(ctx, principal, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx ("+where+")")
		_, err := tx.Exec(ctx, `
			INSERT INTO mcp_grants
			    (tenant_id, name, status, site_scope_mode,
			     capabilities, oauth_scopes, expires_at)
			VALUES ($1, $2, 'active', 'all', ARRAY['mcp.sites.read']::text[],
			        $3::text[], $4)`,
			tenantID, "m136 "+where, scopes,
			time.Now().UTC().Add(90*24*time.Hour))
		return err
	})
}

// TestScopeColumnConstraintsRefuseAsAppRole is the proof.
func TestScopeColumnConstraintsRefuseAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m136-constraints-"+uuid.NewString()[:8])

	// -----------------------------------------------------------------------
	// THE INDETERMINATE ARM, CHECKED FIRST, BEFORE ANY INSERT.
	//
	// Every assertion below identifies a refusal by CONSTRAINT NAME. If a
	// constraint has been renamed or dropped, the insert it was meant to trip
	// either succeeds -- caught -- or trips a DIFFERENT constraint whose name
	// this file would then report as a mismatch, which is a confusing way to
	// learn that the subject is gone. So the subject is confirmed to exist
	// first, by name, and a missing one fails HERE with the reason attached.
	//
	// This is also the arm that makes the file honest as a guard: point any
	// constant above at a name the database does not carry and this test goes
	// RED rather than passing by matching nothing.
	// -----------------------------------------------------------------------
	wantConstraints := []string{
		m136ScopesNotEmptyConstraint,
		m136ScopesShapeConstraint,
		m136ScopesVocabularyConstraint,
	}
	for _, name := range wantConstraints {
		var found bool
		if err := pool.RunTenantTx(ctx,
			domain.Principal{TenantID: tenant, Scope: domain.ScopeOrg},
			func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `
					SELECT EXISTS (
						SELECT 1 FROM pg_constraint
						WHERE conrelid = 'public.mcp_grants'::regclass
						  AND contype  = 'c'
						  AND conname  = $1)`, name).Scan(&found)
			}); err != nil {
			t.Fatalf("probe for constraint %q: %v", name, err)
		}
		if !found {
			t.Fatalf("INDETERMINATE: public.mcp_grants carries no CHECK named %q, "+
				"so the refusal this file attributes to it cannot be attributed to "+
				"anything.\nThis is NOT a pass. Either the constraint was renamed or "+
				"dropped -- m136 requires a widening migration to drop and re-add "+
				"THIS name -- or this file is pointed at a name that never existed. "+
				"Fix the constraint or fix the constant; do not read this as "+
				"'the database refuses it'.", name)
		}
		t.Logf("subject confirmed: public.mcp_grants carries CHECK %q", name)
	}

	// NOT NULL is not a named CHECK, so it has no pg_constraint row to probe
	// by name. Its subject is confirmed the other way: attnotnull on the
	// column itself. Same purpose -- if the column stopped being NOT NULL this
	// would say so instead of the insert quietly succeeding.
	var notNull bool
	if err := pool.RunTenantTx(ctx,
		domain.Principal{TenantID: tenant, Scope: domain.ScopeOrg},
		func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				SELECT attnotnull FROM pg_attribute
				WHERE attrelid = 'public.mcp_grants'::regclass
				  AND attname  = 'oauth_scopes'
				  AND NOT attisdropped`).Scan(&notNull)
		}); err != nil {
		t.Fatalf("probe mcp_grants.oauth_scopes for NOT NULL: %v", err)
	}
	if !notNull {
		t.Fatal("INDETERMINATE: mcp_grants.oauth_scopes is nullable. m136 DECISION 4 " +
			"makes the absent value unrepresentable precisely so an empty read means " +
			"'something is wrong' rather than 'no narrowing applied, therefore " +
			"everything'. The 23502 arm below would prove nothing.")
	}
	t.Log("subject confirmed: mcp_grants.oauth_scopes is NOT NULL")

	// -----------------------------------------------------------------------
	// THE REFUSALS. One transaction each, role asserted inside each.
	//
	// ON CONSTRAINT ORDERING, BECAUSE ONE CASE DEPENDS ON IT. ARRAY[''] is
	// refused by the shape CHECK (no empty string) AND by the vocabulary CHECK
	// ('' is not a recognised scope), and Postgres reports the violated CHECK
	// it reaches first, which is creation order. m136 creates shape before
	// vocabulary, so shape is the name reported. That is asserted rather than
	// papered over: if a later migration re-adds them in the other order this
	// test says so, and the reader is told the refusal is still live but the
	// attribution moved. Every other case below violates exactly one.
	// -----------------------------------------------------------------------
	cases := []struct {
		where       string
		scopes      any
		wantCode    string
		wantSubject string
		why         string
	}{
		{
			where:       "nil",
			scopes:      nil,
			wantCode:    "23502", // not_null_violation
			wantSubject: "oauth_scopes",
			why: "a NULL scope column is the 'unknown, therefore unrestricted' " +
				"read m136 DECISION 4 exists to make unrepresentable",
		},
		{
			where:       "the empty array",
			scopes:      []string{},
			wantCode:    "23514", // check_violation
			wantSubject: m136ScopesNotEmptyConstraint,
			why: "a grant holding no scope at all authenticates nowhere and " +
				"fails at no write, so it is refused at the write instead",
		},
		{
			where:       "an unrecognised scope",
			scopes:      []string{"mcp:write-someday"},
			wantCode:    "23514",
			wantSubject: m136ScopesVocabularyConstraint,
			why: "grantScopes does not filter, so a scope outside this build's " +
				"registry would reach OrgDefaultCapabilities and 403 a live " +
				"credential rather than failing at write time",
		},
		{
			where:       "an empty-string member",
			scopes:      []string{""},
			wantCode:    "23514",
			wantSubject: m136ScopesShapeConstraint,
			why: "an empty element is a scope name nothing can match and " +
				"nothing reports, which is the m120 shape trap one column over",
		},
		{
			where:       "17 members",
			scopes:      m136RepeatScope("mcp:read", 17),
			wantCode:    "23514",
			wantSubject: m136ScopesShapeConstraint,
			why: "the cardinality ceiling is 16; a scope list longer than the " +
				"credential's stated shape is refused whole",
		},
	}

	for _, tc := range cases {
		t.Run(tc.where, func(t *testing.T) {
			err := m136InsertGrantScopes(t, pool, tenant, tc.where, tc.scopes)
			if err == nil {
				t.Fatalf("oauth_scopes = %v was ACCEPTED. It must be refused with "+
					"SQLSTATE %s by %q -- %s.",
					tc.scopes, tc.wantCode, tc.wantSubject, tc.why)
			}

			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("oauth_scopes = %v was refused, but not by the database: %v.\n"+
					"A refusal from some other layer is not the proof this file "+
					"makes -- the whole point is that the column is hard to write "+
					"wrong even when Go is not the writer.", tc.scopes, err)
			}
			if pgErr.Code != tc.wantCode {
				t.Fatalf("oauth_scopes = %v took SQLSTATE %s (%s), want %s.",
					tc.scopes, pgErr.Code, pgErr.ConstraintName, tc.wantCode)
			}

			// NOT NULL names a COLUMN, every CHECK names a CONSTRAINT. Both are
			// asserted by name so a refusal can never be credited to the wrong
			// guard.
			got := pgErr.ConstraintName
			if tc.wantCode == "23502" {
				got = pgErr.ColumnName
			}
			if got != tc.wantSubject {
				t.Fatalf("oauth_scopes = %v was refused with SQLSTATE %s by %q, "+
					"want %q.\nThe refusal is live but it is being credited to the "+
					"wrong guard, which means one of the two is not doing the job "+
					"this file says it does.", tc.scopes, pgErr.Code, got, tc.wantSubject)
			}
			t.Logf("refused as wpmgr_app: oauth_scopes = %v -> SQLSTATE %s by %q",
				tc.scopes, pgErr.Code, got)
		})
	}

	// -----------------------------------------------------------------------
	// AND IT DOES NOT OVER-FIRE. The honest value lands.
	//
	// This arm is what keeps the five above meaningful. A column that refused
	// every value would pass all of them and the first real connection would
	// fail, so the guard would be removed and then it would guard nothing.
	// -----------------------------------------------------------------------
	honest := []string{"mcp:read"}
	if err := m136InsertGrantScopes(t, pool, tenant, "the honest value", honest); err != nil {
		t.Fatalf("oauth_scopes = %v was REFUSED: %v.\nThis is the value m136's "+
			"backfill wrote onto every grant this surface has ever minted and the "+
			"value DefaultGrantScopes() stamps on new ones. A constraint set that "+
			"refuses it refuses the whole product.", honest, err)
	}

	// Read it back through the same dispatch, so the accept is a stored row and
	// not merely an INSERT that did not raise.
	var stored []string
	if err := pool.RunTenantTx(ctx,
		domain.Principal{TenantID: tenant, Scope: domain.ScopeOrg},
		func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "RunTenantTx (read the accepted row back)")
			return tx.QueryRow(ctx, `
				SELECT oauth_scopes FROM mcp_grants
				WHERE tenant_id = $1 AND name = $2`,
				tenant, "m136 the honest value").Scan(&stored)
		}); err != nil {
		t.Fatalf("read the accepted grant back: %v", err)
	}
	if len(stored) != 1 || stored[0] != "mcp:read" {
		t.Fatalf("stored oauth_scopes = %v, want [mcp:read]", stored)
	}
	t.Logf("accepted as wpmgr_app and read back: oauth_scopes = %v", stored)
}

// m136RepeatScope builds an n-member scope list. Every member is a RECOGNISED
// scope, so the only thing wrong with the value is its length -- otherwise the
// vocabulary CHECK could be the one refusing it and the cardinality ceiling
// would go untested behind a passing assertion.
func m136RepeatScope(scope string, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, scope)
	}
	if len(out) != n {
		panic(fmt.Sprintf("m136RepeatScope built %d members, want %d", len(out), n))
	}
	return out
}
