// m133 proofs: assistant_update_proposals is invisible outside its tenant and
// outside its site scope, and running out of time can never become consent.
//
// WHY THIS FILE EXISTS RATHER THAN A PARAGRAPH IN THE MIGRATION. m133 argues
// each property from its own constraints and policies. That is an argument, not
// a proof, and this project has spent a day finding guards that could not fire.
// Every assertion below has had its failure planted and watched; the mutation
// each one caught is named in a comment above it.
//
// THAT SENTENCE WAS ONCE FALSE OF PROOF 1 AND IS WORTH KEEPING THE SCAR FOR.
// Its mutation was documented as a DROP of the only permissive policy, which
// narrows rather than widens: the leak assertion passed vacuously at 0 rows and
// the positive control was what broke. The assertion had never been watched to
// fail. Dropping a PERMISSIVE policy narrows; dropping a RESTRICTIVE one
// widens; only a widening can fire a leak assertion. Proof 1's mutation is now
// an ALTER POLICY ... USING (true).
//
// THREE THINGS THIS FILE REFUSES TO DO, EACH BECAUSE DOING IT MAKES THE PROOF
// VACUOUS:
//
//  1. It never opens its own connection. Every read and every write goes
//     through pool.InTenantTx or pool.InScopedTenantTx -- the production
//     dispatch, and the only thing that sets app.tenant_id, app.site_scope and
//     app.allowed_site_ids. A test that dials its own connection leaves every
//     policy inert and passes having proved nothing.
//  2. It never runs as superuser or as the table owner. The table is FORCE ROW
//     LEVEL SECURITY, so an owner connection bypasses every policy below.
//     mcpAssertAndReportRole runs INSIDE each transaction under test and fails
//     unless it finds wpmgr_app with rolsuper=false and rolbypassrls=false.
//     The connectAdmin pool appears only to plant and restore a mutation, never
//     to make an assertion.
//  3. It never puts a leak assertion behind a row count. In each proof the
//     FIRST thing asserted is that the foreign row is absent. A leak check
//     sitting behind a count is a leak check that never executes on the run
//     where the count is what breaks, which leaves the leak assertion itself
//     unproven -- three agents shipped exactly that shape today.
//
// BUILT TO SURVIVE A SECOND PROPOSAL TYPE. component_type is a one-member
// closed set today and the owner has ruled the plan is sequenced, not skipped,
// so themes and core arrive later. Nothing here hard-codes 'plugin' as an
// assumption: the constant is named once, the proofs assert visibility and
// transitions rather than contents, and a widened component_type does not touch
// a line of this file.
package tests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// m133IsPermissionDenied reports whether err is PostgreSQL's insufficient
// privilege, 42501, read from the SQLSTATE rather than from the message text.
//
// The code is the mechanism; the message is a rendering of it, and it is
// localisable and version-dependent. A test that matches "permission denied"
// asserts on a string PostgreSQL never promised to keep.
func m133IsPermissionDenied(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42501"
}

// m133IsForeignKeyViolation reports whether err is 23503, and whether it came
// from the named constraint. The constraint name matters: a proof that only
// checked for "some FK complained" would pass if a different foreign key on the
// same statement fired instead.
func m133IsForeignKeyViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503" &&
		pgErr.ConstraintName == constraint
}

// m133ComponentType is the single member component_type admits today. Named
// once so that widening the CHECK is a one-line change here and not a hunt.
const m133ComponentType = "plugin"

// m133Digest builds a syntactically valid presented_digest. The shape check is
// ^[0-9a-f]{64}$; a real digest is computed server-side over the
// control-plane-derived facts, which is Session B's work and not what these
// proofs are about.
func m133Digest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// m133InsertPending writes one pending proposal through InTenantTx and returns
// its id. Through the real dispatch, so the row is written under the same
// policies everything else in this file reads under.
func m133InsertPending(t *testing.T, pool *db.Pool, tenant, site, grant uuid.UUID, slug string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m133 insert pending)")
		return tx.QueryRow(context.Background(), `
			INSERT INTO assistant_update_proposals
			    (tenant_id, site_id, proposed_by_grant_id, component_type,
			     component_slug, from_version, to_version, presented_digest,
			     state, expires_at)
			VALUES ($1, $2, $3, $4, $5, '1.0.0', '1.1.0', $6, 'pending', now() + interval '1 hour')
			RETURNING id`,
			tenant, site, grant, m133ComponentType, slug, m133Digest(slug)).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed a pending proposal for site %s: %v", site, err)
	}
	return id
}

// m133ExpectRefused runs a statement that MUST be refused, inside a savepoint,
// and returns the refusal.
//
// THE SAVEPOINT IS NOT COSMETIC. A failed statement aborts the enclosing pgx
// transaction, so a proof that simply execs the forbidden statement and carries
// on gets "commit unexpectedly resulted in rollback" at the end -- a failure
// that looks like the assertion failing when the assertion in fact succeeded.
// That shape is worse than a broken test: the message points at the wrong
// thing. Rolling back to the savepoint leaves the outer transaction usable, so
// each proof can attempt several refusals and still assert a positive control
// afterwards.
//
// It returns nil when the statement SUCCEEDED, which is the caller's failure
// case and is why this returns an error rather than fataling on one.
func m133ExpectRefused(t *testing.T, tx pgx.Tx, sql string, args ...any) error {
	t.Helper()
	ctx := context.Background()
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("open savepoint: %v", err)
	}
	_, execErr := sp.Exec(ctx, sql, args...)
	// Rollback whether or not it failed. If the statement unexpectedly
	// SUCCEEDED, rolling back is what stops this probe from mutating the row
	// the caller is about to assert on.
	if rbErr := sp.Rollback(ctx); rbErr != nil && execErr == nil {
		t.Fatalf("release savepoint after an unexpectedly successful statement: %v", rbErr)
	}
	return execErr
}

// m133ExpectNoRows runs a statement that RLS must refuse by admitting no rows,
// inside a savepoint, and returns how many rows it touched.
//
// THIS IS A DIFFERENT SHAPE FROM m133ExpectRefused AND THE DIFFERENCE MATTERS.
// A privilege refusal (42501) and a CHECK violation (23514) raise an error, so
// m133ExpectRefused asserts on the error. An RLS policy refusing an UPDATE or a
// DELETE raises NOTHING: PostgreSQL applies the policy as a row filter, the
// statement succeeds, and it matches ZERO ROWS. Only INSERT is loud, because a
// WITH CHECK violation has no row to filter and raises instead.
//
// So a test that asserts "this errored" against an RLS-refused UPDATE fails on
// a correctly-narrowed policy and passes on a missing one -- backwards in the
// direction that costs a tenant boundary. That silence is exactly the m84/#96
// failure .claude/rules/go-control-plane.md is about, and the first draft of
// TestAgentContextCanScanButCannotDecideAsAppRole made the mistake it exists to
// catch: it reported the FOR SELECT policy as FOR ALL because the refusal was
// quiet.
func m133ExpectNoRows(t *testing.T, tx pgx.Tx, sql string, args ...any) (int64, error) {
	t.Helper()
	ctx := context.Background()
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("open savepoint: %v", err)
	}
	tag, execErr := sp.Exec(ctx, sql, args...)
	if rbErr := sp.Rollback(ctx); rbErr != nil && execErr == nil {
		t.Fatalf("release savepoint: %v", rbErr)
	}
	if execErr != nil {
		return 0, execErr
	}
	return tag.RowsAffected(), nil
}

// m133CountVisible counts how many times a specific proposal id is visible from
// inside the transaction it is handed. Returns a count rather than a boolean so
// a failure message can say "saw it N times" instead of "saw it".
func m133CountVisible(t *testing.T, tx pgx.Tx, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(context.Background(),
		`SELECT count(*) FROM assistant_update_proposals WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count visible proposals: %v", err)
	}
	return n
}

// ---------------------------------------------------------------------------
// PROOF 1 -- TENANT ISOLATION
// ---------------------------------------------------------------------------

// TestProposalIsInvisibleToAForeignTenantAsAppRole proves the permissive
// assistant_update_proposals_tenant_isolation policy is load-bearing.
//
// Mutation 1, planted and watched. THE MUTATION IS A WIDENING, NOT A DROP, AND
// THE DIFFERENCE IS THE WHOLE POINT:
//
//	ALTER POLICY assistant_update_proposals_tenant_isolation
//	    ON assistant_update_proposals USING (true) WITH CHECK (true);
//
// through connectAdmin before the read makes the FIRST assertion fail with
// "TENANCY LEAK: tenant B sees tenant A's proposal (1 rows)".
//
// AN EARLIER VERSION OF THIS COMMENT NAMED A DROP, AND A REVIEW SHOWED THE DROP
// CANNOT FIRE THIS ASSERTION. tenant_isolation is the only PERMISSIVE policy on
// the table, so dropping it makes the table invisible to everyone: the leak
// assertion then passes vacuously at 0 rows and the POSITIVE CONTROL is what
// breaks, with "OVER-FIRING: tenant A cannot see its own proposal". The words
// "TENANCY LEAK" never appear. The assertion was fine; the documented mutation
// was testing a different property.
//
// THE GENERAL RULE, because it caught this file once and will again: dropping a
// PERMISSIVE policy narrows and dropping a RESTRICTIVE one widens. Only a
// widening can produce a leak, so only a widening can fire a leak assertion.
// Mutation 2 below drops a RESTRICTIVE policy and is correct as written.
func TestProposalIsInvisibleToAForeignTenantAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	tenantA := seedTenant(t, pool, "m133-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "m133-b-"+uuid.NewString()[:8])
	siteA := seedSite(t, pool, tenantA, "")
	proposalA := m133InsertPending(t, pool, tenantA, siteA, uuid.New(), "akismet")

	if err := pool.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (foreign tenant read)")

		// THE LEAK ASSERTION, FIRST AND BEFORE ANY OTHER CHECK.
		if n := m133CountVisible(t, tx, proposalA); n != 0 {
			t.Fatalf("TENANCY LEAK: tenant B sees tenant A's proposal %s (%d rows). "+
				"assistant_update_proposals_tenant_isolation is missing or not "+
				"enforced. A proposal names a site, a plugin and a version, so this "+
				"leaks the existence and composition of another organisation's fleet.",
				proposalA, n)
		}

		// Only now the weaker check: tenant B sees nothing at all here, which
		// catches a policy that somehow admits rows without matching the id.
		var total int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM assistant_update_proposals`).Scan(&total); err != nil {
			return err
		}
		if total != 0 {
			t.Fatalf("tenant B sees %d proposals in a table where it has written none", total)
		}
		return nil
	}); err != nil {
		t.Fatalf("foreign tenant read: %v", err)
	}

	// POSITIVE CONTROL. A policy that hides everything from everyone would pass
	// every assertion above while breaking the product. Tenant A must still see
	// its own row.
	if err := pool.InTenantTx(ctx, tenantA, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (owning tenant read)")
		if n := m133CountVisible(t, tx, proposalA); n != 1 {
			t.Fatalf("OVER-FIRING: tenant A cannot see its own proposal %s (%d rows). "+
				"The isolation policy is refusing the owner, which would make the "+
				"approval surface permanently empty.", proposalA, n)
		}
		return nil
	}); err != nil {
		t.Fatalf("owning tenant read: %v", err)
	}
}

// ---------------------------------------------------------------------------
// PROOF 2 -- SITE SCOPE
// ---------------------------------------------------------------------------

// TestProposalOutsideTheSiteScopeIsInvisibleAsAppRole proves the RESTRICTIVE
// assistant_update_proposals_site_scope policy is load-bearing, in BOTH
// directions: USING hides the foreign site's row, and WITH CHECK refuses to let
// a scoped principal write one.
//
// This is the m112 failure exactly: without this policy the database refuses
// another TENANT and has no opinion about another SITE, so a connection scoped
// to one site reads -- and once Session B lands, proposes against -- every
// other site in the same organisation.
//
// Mutation 2, planted and watched. Planting
//
//	DROP POLICY assistant_update_proposals_site_scope ON assistant_update_proposals;
//
// makes the FIRST assertion fail with "site-scoped principal sees a proposal
// for a site outside its scope", and makes the WITH CHECK arm fail too.
func TestProposalOutsideTheSiteScopeIsInvisibleAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	tenant := seedTenant(t, pool, "m133-scope-"+uuid.NewString()[:8])
	inScope := seedSite(t, pool, tenant, "")
	outOfScope := seedSite(t, pool, tenant, "")

	// Same tenant, deliberately. Tenant isolation cannot help here and must not
	// be what makes this proof pass -- if both sites were in different tenants
	// this test would be Proof 1 wearing a different name.
	inScopeProposal := m133InsertPending(t, pool, tenant, inScope, uuid.New(), "in-scope-plugin")
	outOfScopeProposal := m133InsertPending(t, pool, tenant, outOfScope, uuid.New(), "out-of-scope-plugin")

	user := uuid.New()
	allowed := []uuid.UUID{inScope}

	if err := pool.InScopedTenantTx(ctx, tenant, user, allowed, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InScopedTenantTx (site-scoped read)")

		// THE LEAK ASSERTION, FIRST. Nothing precedes it -- not a count, not a
		// sanity read, not the positive control.
		if n := m133CountVisible(t, tx, outOfScopeProposal); n != 0 {
			t.Fatalf("SITE-SCOPE LEAK: a principal scoped to site %s sees the proposal "+
				"%s belonging to site %s (%d rows), inside the same organisation. "+
				"assistant_update_proposals_site_scope is missing or not RESTRICTIVE. "+
				"This is the m112 defect: the database refuses another tenant and has "+
				"no opinion about another site.",
				inScope, outOfScopeProposal, outOfScope, n)
		}

		// POSITIVE CONTROL, second. A policy that returns nothing to anyone
		// passes the assertion above and guards nothing.
		if n := m133CountVisible(t, tx, inScopeProposal); n != 1 {
			t.Fatalf("OVER-FIRING: a principal scoped to site %s cannot see that site's "+
				"own proposal %s (%d rows). A scope policy that refuses correct work "+
				"gets switched off, and then it guards nothing.",
				inScope, inScopeProposal, n)
		}

		// THE WITH CHECK ARM. Hiding a row a principal may not read while
		// letting it WRITE one for the same site is a policy that closes the
		// read door and leaves the write door open -- and the write is the half
		// that leads to a change on a live site.
		writeErr := m133ExpectRefused(t, tx, `
			INSERT INTO assistant_update_proposals
			    (tenant_id, site_id, proposed_by_grant_id, component_type,
			     component_slug, from_version, to_version, presented_digest,
			     state, expires_at)
			VALUES ($1, $2, $3, $4, 'smuggled', '1.0.0', '1.1.0', $5, 'pending',
			        now() + interval '1 hour')`,
			tenant, outOfScope, uuid.New(), m133ComponentType, m133Digest("smuggled"))
		if writeErr == nil {
			t.Fatalf("SITE-SCOPE WRITE LEAK: a principal scoped to site %s inserted a "+
				"proposal for site %s. The RESTRICTIVE policy's WITH CHECK arm is "+
				"missing. Once the propose tool exists this is a change proposed "+
				"against a site the connection was never granted.",
				inScope, outOfScope)
		}
		if !strings.Contains(strings.ToLower(writeErr.Error()), "row-level security") {
			t.Fatalf("the out-of-scope INSERT was refused, but not by RLS: %v\n"+
				"A refusal from a CHECK constraint or a foreign key would leave the "+
				"policy itself unproven.", writeErr)
		}
		return nil
	}); err != nil {
		// FAIL ON ANY ERROR HERE. This clause used to swallow anything whose
		// message mentioned "row-level security", on the reasoning that such an
		// error was the proof succeeding. It is not: every refusal this proof
		// cares about is asserted INSIDE the closure, which returns nil. An
		// error reaching this point came from the dispatch itself -- a failed
		// InScopedTenantTx, a broken GUC, a rolled-back transaction -- and
		// treating it as success passes the proof on a path that never ran it.
		t.Fatalf("site-scoped read: %v", err)
	}
}

// ---------------------------------------------------------------------------
// PROOF 3 -- RUNNING OUT OF TIME IS NEVER CONSENT
// ---------------------------------------------------------------------------

// TestExpiredProposalCannotBecomeApprovedAsAppRole proves
// assistant_update_proposals_consent_within_window_check is terminal in the one
// direction that matters, and tries three separate ways to defeat it.
//
// "There is no timer anywhere in this product that turns waiting into consent."
// That sentence is only true if the database holds it, because a handler that
// forgets is a handler, and this is the constraint that does not forget.
//
// Mutation 3, planted and watched. Planting
//
//	ALTER TABLE assistant_update_proposals
//	    DROP CONSTRAINT assistant_update_proposals_consent_within_window_check;
//
// makes attempt (a) succeed, and the assertion below fails with "an expired
// proposal was approved".
func TestExpiredProposalCannotBecomeApprovedAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	tenant := seedTenant(t, pool, "m133-expiry-"+uuid.NewString()[:8])
	// A real approver, so the only thing wrong with each refused UPDATE below
	// is the one property that proof is testing.
	expiryAdmin := connectAdmin(t, pool)
	approver := seedUserRow(t, expiryAdmin, "m133-expiry-"+uuid.NewString()[:8]+"@example.com")
	expiryAdmin.Close()
	site := seedSite(t, pool, tenant, "")

	// A proposal whose window has already closed. created_at is backdated too,
	// because window_is_positive_check requires expires_at > created_at -- a
	// row whose window never opened is a different defect and is not what this
	// proof is about.
	var expired uuid.UUID
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (seed an expired proposal)")
		return tx.QueryRow(ctx, `
			INSERT INTO assistant_update_proposals
			    (tenant_id, site_id, proposed_by_grant_id, component_type,
			     component_slug, from_version, to_version, presented_digest,
			     state, created_at, expires_at)
			VALUES ($1, $2, $3, $4, 'stale-plugin', '1.0.0', '1.1.0', $5, 'pending',
			        now() - interval '2 hours', now() - interval '1 hour')
			RETURNING id`,
			tenant, site, uuid.New(), m133ComponentType, m133Digest("stale")).Scan(&expired)
	}); err != nil {
		t.Fatalf("seed an expired proposal: %v", err)
	}

	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (attempt to approve an expired proposal)")

		// (a) THE HONEST APPROVE STATEMENT. This is the exact shape Session B
		// will write, and on a row whose window has closed the database must
		// refuse it. THIS ASSERTION IS FIRST.
		// decided_by_user_id IS SUPPLIED DELIBERATELY. Without it this UPDATE
		// violates approval_names_a_human_check as well, and Postgres would be
		// free to report either constraint -- so a proof asserting on the
		// window constraint's name would pass or fail depending on evaluation
		// order rather than on the property under test. Supplying an approver
		// leaves the closed window as the ONLY thing wrong with the row.
		err := m133ExpectRefused(t, tx, `
			UPDATE assistant_update_proposals
			   SET state = 'approved_undispatched', decided_at = now(),
			       decided_by_user_id = $2
			 WHERE id = $1 AND state = 'pending'`, expired, approver)
		if err == nil {
			t.Fatalf("CONSENT FROM SILENCE: proposal %s expired an hour ago and the "+
				"database accepted it into 'approved_undispatched' anyway. "+
				"assistant_update_proposals_consent_within_window_check is missing. "+
				"Waiting has just become consent, which is the one thing the "+
				"approval model may not do.", expired)
		}
		if !strings.Contains(err.Error(), "consent_within_window") {
			t.Fatalf("the late approval was refused, but not by the window constraint: %v\n"+
				"A refusal from the state CHECK or from RLS would leave the window "+
				"constraint itself unproven.", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("late approval attempt: %v", err)
	}

	// (b) VIA THE TERMINAL 'expired' STATE. A sweeper marks the row, and the
	// question is whether anything can walk it back out.
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (sweep then attempt to revive)")
		if _, err := tx.Exec(ctx,
			`UPDATE assistant_update_proposals SET state = 'expired' WHERE id = $1`, expired); err != nil {
			t.Fatalf("the expiry sweep could not mark proposal %s expired: %v", expired, err)
		}
		err := m133ExpectRefused(t, tx, `
			UPDATE assistant_update_proposals
			   SET state = 'approved_undispatched', decided_at = now(),
			       decided_by_user_id = $2
			 WHERE id = $1`, expired, approver)
		if err == nil {
			t.Fatalf("EXPIRY IS NOT TERMINAL: proposal %s was marked 'expired' and was "+
				"then transitioned to 'approved_undispatched'. An expired proposal that "+
				"can be revived is an expiry that means nothing.", expired)
		}
		if !strings.Contains(err.Error(), "consent_within_window") {
			t.Fatalf("reviving an expired proposal was refused, but not by the window "+
				"constraint: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("revive attempt: %v", err)
	}

	// (c) EXPIRY MAY NOT NAME A HUMAN. A sweeper that attributes the outcome to
	// the person who did not answer produces an audit row a later read reports
	// as a decision. Separate constraint, separate proof.
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (attempt to attribute an expiry)")
		err := m133ExpectRefused(t, tx, `
			UPDATE assistant_update_proposals
			   SET decided_by_user_id = $2
			 WHERE id = $1`, expired, uuid.New())
		if err == nil {
			t.Fatalf("ATTRIBUTED SILENCE: expired proposal %s now names a human as its "+
				"decider. assistant_update_proposals_expiry_is_not_a_decision_check is "+
				"missing, and the audit trail will report a timeout as somebody's "+
				"decision.", expired)
		}
		// ONE ACCEPTED MECHANISM, DELIBERATELY. An earlier version also accepted
		// a "foreign key" refusal, which DECISION 7 had already removed -- and a
		// review showed the cost was not cosmetic: with the constraint dropped
		// and an FK added back, this proof went GREEN. It was accepting a
		// refusal from precisely the mechanism this migration argues against.
		if !strings.Contains(err.Error(), "expiry_is_not_a_decision") {
			t.Fatalf("attributing an expiry was refused, but not by the constraint "+
				"that exists to refuse it: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("attribution attempt: %v", err)
	}

	// POSITIVE CONTROL, LAST AND DELIBERATELY SO. A window constraint that
	// refuses EVERY approval would pass all three attempts above while making
	// the product unusable. A proposal inside its window must approve cleanly.
	var live uuid.UUID
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (approve a live proposal)")
		if err := tx.QueryRow(ctx, `
			INSERT INTO assistant_update_proposals
			    (tenant_id, site_id, proposed_by_grant_id, component_type,
			     component_slug, from_version, to_version, presented_digest,
			     state, expires_at)
			VALUES ($1, $2, $3, $4, 'live-plugin', '1.0.0', '1.1.0', $5, 'pending',
			        now() + interval '1 hour')
			RETURNING id`,
			tenant, site, uuid.New(), m133ComponentType, m133Digest("live")).Scan(&live); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE assistant_update_proposals
			   SET state = 'approved_undispatched', decided_at = now(),
			       decided_by_user_id = $2
			 WHERE id = $1 AND state = 'pending'`, live, approver)
		if err != nil {
			t.Fatalf("OVER-FIRING: a proposal inside its window could not be approved: %v\n"+
				"A constraint that refuses correct work gets switched off, and then it "+
				"guards nothing.", err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("OVER-FIRING: approving a live proposal affected %d rows, not 1",
				tag.RowsAffected())
		}
		return nil
	}); err != nil {
		t.Fatalf("live approval: %v", err)
	}

	// And the approved row is genuinely in the committed handoff state ADR-061
	// requires, not merely un-refused.
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (read back the handoff state)")
		var state string
		var decidedAt time.Time
		if err := tx.QueryRow(ctx,
			`SELECT state, decided_at FROM assistant_update_proposals WHERE id = $1`,
			live).Scan(&state, &decidedAt); err != nil {
			return err
		}
		if state != "approved_undispatched" {
			t.Fatalf("the approved proposal committed as %q, not 'approved_undispatched'. "+
				"ADR-061 requires the handoff state to be committed and named, so a crash "+
				"in the gap is an unclaimed row rather than a lost or doubled action.", state)
		}
		if decidedAt.IsZero() {
			t.Fatalf("the approved proposal has no decided_at, so the audit trail cannot "+
				"say when the human decided")
		}
		return nil
	}); err != nil {
		t.Fatalf("handoff state read-back: %v", err)
	}
}

// ---------------------------------------------------------------------------
// PROOF 4 -- AN APPROVAL NAMES A HUMAN, AND KEEPS NAMING THEM
// ---------------------------------------------------------------------------

// TestApprovedProposalMustNameAHumanAsAppRole proves m133 DECISION 8:
// assistant_update_proposals_approval_names_a_human_check makes an approved
// proposal with no named approver unrepresentable, and the absence of a foreign
// key means deleting that user later cannot erase who approved.
//
// THE TWO HALVES ARE ONE DECISION AND ARE PROVED TOGETHER ON PURPOSE. Requiring
// the approver and keeping a foreign key are mutually exclusive: a users DELETE
// would drive every approved row into violation and the DELETE would fail, so
// offboarding an employee would be blocked by the evidence of their own
// approvals. A proof of either half alone would pass on a schema where the
// other half is broken.
//
// WHY THIS IS THE STRONGEST FORM THE RULE CAN TAKE. An API-key principal has no
// user id, so a machine credential cannot satisfy this constraint at all. "A
// machine approved this" is not something a handler refuses; it is a row the
// database will not store.
//
// Mutation 4, planted and watched. Planting
//
//	ALTER TABLE assistant_update_proposals
//	    DROP CONSTRAINT assistant_update_proposals_approval_names_a_human_check;
//
// makes the FIRST assertion fail with "an approval was stored naming nobody".
func TestApprovedProposalMustNameAHumanAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	tenant := seedTenant(t, pool, "m133-approver-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")

	admin := connectAdmin(t, pool)
	approver := seedUserRow(t, admin, "m133-approver-"+uuid.NewString()[:8]+"@example.com")
	admin.Close()

	pending := m133InsertPending(t, pool, tenant, site, uuid.New(), "needs-a-human")

	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (approval must name a human)")

		// THE LEAK ASSERTION, FIRST. An UPDATE into the approved state that
		// names nobody. This is the exact shape an API-key principal would
		// produce, because it has no user id to supply.
		anonUpdate := m133ExpectRefused(t, tx, `
			UPDATE assistant_update_proposals
			   SET state = 'approved_undispatched', decided_at = now()
			 WHERE id = $1 AND state = 'pending'`, pending)
		if anonUpdate == nil {
			t.Fatalf("APPROVAL BY NOBODY: proposal %s was moved into "+
				"'approved_undispatched' with decided_by_user_id left NULL. "+
				"assistant_update_proposals_approval_names_a_human_check is missing. "+
				"An approval whose approver is unrecoverable is not an approval, and "+
				"this is precisely the row an API-key principal writes, since it has "+
				"no user id to supply.", pending)
		}
		if !strings.Contains(anonUpdate.Error(), "approval_names_a_human") {
			t.Fatalf("the anonymous approval was refused, but not by the constraint "+
				"that exists to refuse it: %v", anonUpdate)
		}

		// The same hole via INSERT rather than UPDATE. A constraint that only
		// catches the transition leaves the row writable directly.
		anonInsert := m133ExpectRefused(t, tx, `
			INSERT INTO assistant_update_proposals
			    (tenant_id, site_id, proposed_by_grant_id, component_type,
			     component_slug, from_version, to_version, presented_digest,
			     state, decided_at, expires_at)
			VALUES ($1, $2, $3, $4, 'inserted-approved', '1.0.0', '1.1.0', $5,
			        'approved_undispatched', now(), now() + interval '1 hour')`,
			tenant, site, uuid.New(), m133ComponentType, m133Digest("inserted-approved"))
		if anonInsert == nil {
			t.Fatalf("APPROVAL BY NOBODY, INSERTED DIRECTLY: a row was written straight " +
				"into 'approved_undispatched' with no approver. The constraint must " +
				"hold on INSERT as well as on the transition, or the transition guard " +
				"is just a speed bump.")
		}
		if !strings.Contains(anonInsert.Error(), "approval_names_a_human") {
			t.Fatalf("the anonymous approved INSERT was refused, but not by the "+
				"constraint that exists to refuse it: %v", anonInsert)
		}

		// POSITIVE CONTROL. A constraint refusing every approval would pass both
		// assertions above and make the product unusable.
		tag, err := tx.Exec(ctx, `
			UPDATE assistant_update_proposals
			   SET state = 'approved_undispatched', decided_at = now(),
			       decided_by_user_id = $2
			 WHERE id = $1 AND state = 'pending'`, pending, approver)
		if err != nil {
			t.Fatalf("OVER-FIRING: a proposal approved by a named human was refused: %v", err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("OVER-FIRING: approving with a named human affected %d rows, not 1",
				tag.RowsAffected())
		}
		return nil
	}); err != nil {
		t.Fatalf("approval-names-a-human: %v", err)
	}

	// THE SECOND HALF: deleting the approver must SUCCEED and must NOT erase the
	// record. With a foreign key this DELETE either fails (RESTRICT) or blanks
	// the column (SET NULL); both are the defect DECISION 7 rejects.
	deleteAdmin := connectAdmin(t, pool)
	if _, err := deleteAdmin.Pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, approver); err != nil {
		t.Fatalf("OFFBOARDING BLOCKED: deleting the approving user failed: %v\n"+
			"A foreign key is still present. Routine account cleanup must not be "+
			"blocked by the approvals that user gave.", err)
	}
	deleteAdmin.Close()

	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (approver survives user deletion)")
		var stored *uuid.UUID
		if err := tx.QueryRow(ctx,
			`SELECT decided_by_user_id FROM assistant_update_proposals WHERE id = $1`,
			pending).Scan(&stored); err != nil {
			return err
		}
		if stored == nil {
			t.Fatalf("AUDIT ERASED BY OFFBOARDING: proposal %s still says it was "+
				"approved, but no longer says by whom, because the approving user was "+
				"deleted. An ON DELETE SET NULL foreign key is present. An audit record "+
				"whose subject can be removed by routine account cleanup is not an "+
				"audit record.", pending)
		}
		if *stored != approver {
			t.Fatalf("the approver changed from %s to %s across the user deletion",
				approver, *stored)
		}
		return nil
	}); err != nil {
		t.Fatalf("approver survival read-back: %v", err)
	}
}

// ---------------------------------------------------------------------------
// PROOF 5 -- THE WINDOW CANNOT BE MOVED
// ---------------------------------------------------------------------------

// TestExpiryWindowCannotBeExtendedAsAppRole proves m133 section (5).
//
// THIS PROOF EXISTS BECAUSE A REVIEW DISPROVED A CLAIM THIS MIGRATION MADE.
// consent_within_window_check compares decided_at to expires_at on the finished
// row and cannot see that either column has just changed, so "an expired
// proposal cannot be approved" held only for a row nobody touched.
//
// PROOF 3 COULD NOT CATCH IT, AND THAT IS THE PART WORTH REMEMBERING: it never
// tried to move the window, because the migration said the window could not
// move. A proof written from the same belief as the thing it is proving
// inherits its blind spot. The gap is closed below; the assertion is what keeps
// it closed.
//
// The fix is a column privilege, not a constraint and not a trigger, so the
// refusal below is a PostgreSQL privilege error (42501) rather than a check
// violation -- asserted specifically, because a refusal from any other
// mechanism would mean the privilege is not what is holding.
//
// WHAT THIS PROOF WOULD MISS WITHOUT THE HARNESS CHANGE: rls_integration_test's
// blanket GRANT re-adds table-level UPDATE after the migrations run. If the
// re-revoke there were removed, expires_at would be updatable in tests and not
// in production, and this proof would fail loudly rather than pass vacuously --
// which is the right way round, and is why the assertion is on the refusal.
func TestExpiryWindowCannotBeExtendedAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	tenant := seedTenant(t, pool, "m133-window-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")
	proposal := m133InsertPending(t, pool, tenant, site, uuid.New(), "immovable")

	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (attempt to extend the window)")

		// THE LEAK ASSERTION, FIRST.
		moved := m133ExpectRefused(t, tx, `
			UPDATE assistant_update_proposals
			   SET expires_at = now() + interval '10 years'
			 WHERE id = $1`, proposal)
		if moved == nil {
			t.Fatalf("WINDOW EXTENDED: expires_at on proposal %s was moved by an "+
				"ordinary UPDATE as wpmgr_app. consent_within_window_check then "+
				"guards nothing but the relation between two mutable columns, and "+
				"an expired proposal can be approved by moving the window forward "+
				"to meet now(). There is supposed to be no 'extend' in this product.",
				proposal)
		}
		if !m133IsPermissionDenied(moved) {
			t.Fatalf("moving the window was refused, but not by a column privilege "+
				"-- expected 42501: %v\n"+
				"Section (5) holds this with REVOKE UPDATE / GRANT UPDATE (cols). A "+
				"refusal from anything else means that privilege is not what is "+
				"holding, and the harness re-revoke may have been lost.", moved)
		}

		// THE FINGERPRINT IS COVERED BY THE SAME REVOKE, and it is the column
		// with the most to lose: rewriting it would let a re-rendered screen
		// claim to be the one the human read.
		digest := m133ExpectRefused(t, tx, `
			UPDATE assistant_update_proposals
			   SET presented_digest = $2
			 WHERE id = $1`, proposal, m133Digest("rewritten"))
		if digest == nil {
			t.Fatalf("FINGERPRINT REWRITTEN: presented_digest on proposal %s was "+
				"updated. The column whose entire purpose is to prove what was "+
				"displayed now proves whatever the last writer wanted.", proposal)
		}
		// AND REFUSED BY THE COLUMN PRIVILEGE, not by whatever else happened to
		// be in the way. Accepting any error here would pass if the REVOKE were
		// gone and the statement failed for an unrelated reason -- a test that
		// names one mechanism and asserts another.
		if !m133IsPermissionDenied(digest) {
			t.Fatalf("the presented_digest UPDATE was refused, but not by the column "+
				"privilege that exists to refuse it -- expected 42501: %v\n"+
				"Section (5)'s REVOKE is what holds this column still; a refusal from "+
				"anything else means it is not what is holding.", digest)
		}

		// POSITIVE CONTROL. The workflow columns must still move, or the REVOKE
		// has taken the table with it and nothing can ever be approved.
		tag, err := tx.Exec(ctx,
			`UPDATE assistant_update_proposals SET note = 'still writable' WHERE id = $1`,
			proposal)
		if err != nil {
			t.Fatalf("OVER-FIRING: the column REVOKE also blocked a legitimate write "+
				"to note: %v", err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("OVER-FIRING: writing note affected %d rows, not 1", tag.RowsAffected())
		}
		return nil
	}); err != nil {
		t.Fatalf("window immutability: %v", err)
	}
}

// m133DropForMutation is the planting tool the mutation comments above
// refer to. It is deliberately NOT called by any test: a mutation that lives in
// the committed tree is a test that fails on main. To plant one, call it from
// the proof under test through connectAdmin, watch the run go red, delete the
// call, and watch it go green.
//
// THE PROSE IN THIS FILE DELIBERATELY AVOIDS THE MARKER TOKEN ITSELF. The
// convention is that a planted mutation carries an all-caps marker comment, and
// the sweep proving none survived greps for that token and must find nothing.
// Any comment that spells the token makes the sweep match documentation instead
// of a live mutation, and it then reports a mutation left behind on every clean
// tree forever. A guard reddened by prose is a guard someone switches off, and
// then it guards nothing -- so the write-ups above spell it "Mutation 1",
// "Mutation 2" and "Mutation 3", and only a genuinely planted call ever carries
// the marker.
//
// This is not hypothetical: the first draft of this file wrote the token in all
// three write-ups and turned the sweep permanently green-blind.
//
// It takes the admin pool because dropping a policy is an owner operation and
// wpmgr_app cannot do it -- which is itself worth knowing: the application role
// cannot disable its own guards.
func m133DropForMutation(t *testing.T, admin *db.Pool, stmt string) {
	t.Helper()
	if _, err := admin.Pool.Exec(context.Background(), stmt); err != nil {
		t.Fatalf("plant mutation %q: %v", stmt, err)
	}
}

// TestProposalSiteMustBelongToItsTenantAsAppRole proves m133 DECISION 10: every
// proposal names a site belonging to the tenant that owns the proposal.
//
// WHY THIS NEEDS ITS OWN PROOF. tenant_id and site_id were each a foreign key
// to their own table, and each was satisfied by a real row while their
// COMBINATION was unchecked. Two reviewers raised it independently. Neither
// tenant isolation nor the site-scope policy covers it: tenant_isolation tests
// the proposal's own tenant_id against the connection's, and site_scope is
// RESTRICTIVE on app.site_scope and inert on every path that does not set it.
// The composite FK is what makes the pair itself the thing checked.
//
// THE PAIR IS CHECKED FOR EVERY WRITER, which is why this proof runs as an
// ORDINARY tenant-scoped connection with no site scope set -- the widest
// legitimate writer, and the one both policies admit. A foreign key is enforced
// before any policy or CHECK of ours is consulted, so there is no writer for
// whom this is optional.
//
// Mutation 9, planted and watched. Planting
//
//	ALTER TABLE assistant_update_proposals
//	    DROP CONSTRAINT assistant_update_proposals_site_within_tenant_fkey;
//
// makes the first assertion fail with "a proposal was stored naming another
// tenant's site".
func TestProposalSiteMustBelongToItsTenantAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	// Two organisations, each with a site. The second is the one that must
	// never be nameable from the first.
	tenantA := seedTenant(t, pool, "m133-pairA-"+uuid.NewString()[:8])
	siteA := seedSite(t, pool, tenantA, "")
	tenantB := seedTenant(t, pool, "m133-pairB-"+uuid.NewString()[:8])
	siteB := seedSite(t, pool, tenantB, "")

	if err := pool.InTenantTx(ctx, tenantA, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (the pair must be one fact)")

		// THE LEAK ASSERTION, FIRST. Tenant A's proposal naming tenant B's
		// site. Both halves exist; the pair does not.
		crossed := m133ExpectRefused(t, tx, `
			INSERT INTO assistant_update_proposals
			    (tenant_id, site_id, proposed_by_grant_id, component_type,
			     component_slug, from_version, to_version, presented_digest,
			     state, expires_at)
			VALUES ($1, $2, $3, $4, 'crossed-pair', '1.0.0', '1.1.0', $5, 'pending',
			        now() + interval '1 hour')`,
			tenantA, siteB, uuid.New(), m133ComponentType, m133Digest("crossed-pair"))
		if crossed == nil {
			t.Fatalf("A PROPOSAL WAS STORED NAMING ANOTHER TENANT'S SITE: tenant %s "+
				"holds a proposal about site %s, which belongs to tenant %s. "+
				"assistant_update_proposals_site_within_tenant_fkey is missing. The "+
				"whole point of this row is to record that a human decided something "+
				"about a SPECIFIC SITE; presented_digest fingerprints the screen they "+
				"were shown, and a digest over the right screen attached to the wrong "+
				"site_id proves nothing at all.", tenantA, siteB, tenantB)
		}
		if !m133IsForeignKeyViolation(crossed,
			"assistant_update_proposals_site_within_tenant_fkey") {
			t.Fatalf("the mismatched pair was refused, but not by the composite foreign "+
				"key that exists to refuse it -- expected 23503 naming "+
				"assistant_update_proposals_site_within_tenant_fkey: %v", crossed)
		}

		// THE SAME PAIR MUST NOT BE REACHABLE BY UPDATE EITHER. site_id is
		// outside section (5)'s column grant, so this is expected to be refused
		// by the privilege first -- but a proof that only covered INSERT would
		// leave the door open the day that grant widens.
		movedSite := m133ExpectRefused(t, tx, `
			UPDATE assistant_update_proposals SET site_id = $2 WHERE tenant_id = $1`,
			tenantA, siteB)
		if movedSite == nil {
			t.Fatalf("A PROPOSAL WAS REPOINTED AT ANOTHER TENANT'S SITE by UPDATE. "+
				"Neither the column privilege nor the composite foreign key stopped "+
				"it, so a decided proposal can be made to name site %s after the fact.",
				siteB)
		}

		// POSITIVE CONTROL. The matching pair must still be insertable, or the
		// constraint refuses correct work and gets switched off.
		var ok uuid.UUID
		if err := tx.QueryRow(ctx, `
			INSERT INTO assistant_update_proposals
			    (tenant_id, site_id, proposed_by_grant_id, component_type,
			     component_slug, from_version, to_version, presented_digest,
			     state, expires_at)
			VALUES ($1, $2, $3, $4, 'matching-pair', '1.0.0', '1.1.0', $5, 'pending',
			        now() + interval '1 hour')
			RETURNING id`,
			tenantA, siteA, uuid.New(), m133ComponentType, m133Digest("matching-pair")).
			Scan(&ok); err != nil {
			t.Fatalf("OVER-FIRING: a proposal naming its own tenant's site %s was "+
				"refused: %v", siteA, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("site-must-belong-to-tenant: %v", err)
	}

	// THE SECOND-ORDER CONSEQUENCE. With the pair unbound, one organisation's
	// proposal could be reached by a cascade from another organisation's site
	// delete. Bound, deleting tenant B's site cannot touch tenant A's rows,
	// because tenant A's rows can never have named it.
	deleteAdmin := connectAdmin(t, pool)
	if _, err := deleteAdmin.Pool.Exec(ctx, `DELETE FROM sites WHERE id = $1`, siteB); err != nil {
		t.Fatalf("deleting the other tenant's site failed: %v", err)
	}
	deleteAdmin.Close()

	if err := pool.InTenantTx(ctx, tenantA, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (survives the other tenant's site delete)")
		var n int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM assistant_update_proposals WHERE tenant_id = $1`,
			tenantA).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			t.Fatalf("CROSS-TENANT CASCADE: tenant %s has %d proposals after tenant "+
				"%s deleted site %s, want 1. The lifetime of one organisation's "+
				"approval record must not be governed by a row in another "+
				"organisation's account.", tenantA, n, tenantB, siteB)
		}
		return nil
	}); err != nil {
		t.Fatalf("read back after the other tenant's site delete: %v", err)
	}
}

// TestAgentContextCanScanButCannotDecideAsAppRole proves the shape of
// assistant_update_proposals_agent: the cross-tenant service context may SEE
// proposals and may not DECIDE them.
//
// WHY THE POLICY IS FOR SELECT AND NOT m118's FOR ALL. `CREATE POLICY` with no
// FOR clause means FOR ALL, and this policy was patterned on m118's
// update_runs_agent, which is FOR ALL on purpose -- the deferred dispatcher
// really does claim update_runs rows under InAgentTx. But update_runs records
// what a MACHINE DID, and this table records what a HUMAN DECIDED: three of its
// five writable columns are the decision itself. `app.agent` is also the scope
// an authenticated agent->CP request runs under (db.go:429), not only the
// background workers, so FOR ALL would put decided_by_user_id on every tenant's
// proposals inside reach of a request path a customer's plugin drives -- and
// DECISION 7 records that nothing enforces that column names a real user. The
// workers need the SCAN, which is the one statement that cannot name a tenant;
// every write after it names the tenant and runs under InTenantTx.
//
// THE ORDER OF THE TWO HALVES IS THE POINT, and it is the opposite of the usual
// leak-first rule for a reason worth stating. The failure this narrowing could
// introduce is the m84/m89/m118 silence: a policy that admits the read and
// nothing else makes a cross-tenant write match ZERO ROWS WITH NO ERROR. A test
// that only asserted "the agent cannot write" would pass just as happily
// against a policy that had been dropped altogether, or misspelled, or gated on
// a GUC nothing sets -- the app.service defect this migration already made once
// and documents in DECISION 5. So the POSITIVE half runs first: prove the agent
// context can really see the row, then prove it cannot decide it.
//
// Mutations 7 and 8, planted and watched. Planting
//
//	DROP POLICY assistant_update_proposals_agent ON assistant_update_proposals;
//
// makes the FIRST assertion fail with "the agent context saw nothing", and
//
//	DROP POLICY assistant_update_proposals_agent ON assistant_update_proposals;
//	CREATE POLICY assistant_update_proposals_agent ON assistant_update_proposals
//	    USING (current_setting('app.agent', true) = 'on')
//	    WITH CHECK (current_setting('app.agent', true) = 'on');
//
// restores the FOR ALL form this test exists to forbid and makes the second
// fail with "the agent context decided a proposal".
//
// THE WRITE ASSERTIONS CHECK ROWS TOUCHED, NOT ERRORS, and that is not a style
// choice -- see m133ExpectNoRows. An RLS-refused UPDATE is silent. The first
// draft of this test asserted on an error and reported this correctly-narrowed
// policy as FOR ALL, which is the same silence, one level up.
func TestAgentContextCanScanButCannotDecideAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	tenant := seedTenant(t, pool, "m133-agentscan-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")

	admin := connectAdmin(t, pool)
	impostor := seedUserRow(t, admin, "m133-impostor-"+uuid.NewString()[:8]+"@example.com")
	admin.Close()

	pending := m133InsertPending(t, pool, tenant, site, uuid.New(), "agent-scan-target")

	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InAgentTx (the agent context scans)")

		// THE POSITIVE HALF, FIRST. The dispatch worker's and the expiry
		// sweep's scan is the one statement in either path that cannot name a
		// tenant. If this comes back empty the policy is not doing its job, and
		// every refusal asserted below would be vacuous.
		if n := m133CountVisible(t, tx, pending); n != 1 {
			t.Fatalf("THE AGENT CONTEXT SAW NOTHING: proposal %s is invisible under "+
				"app.agent, saw it %d times, want 1. assistant_update_proposals_agent "+
				"is missing, misspelled, or gated on a GUC nothing sets. This is the "+
				"m84/m89/m118 silence: the dispatch worker and the expiry sweep would "+
				"find this table empty at 3am, with no error, and every proposal would "+
				"sit at 'approved_undispatched' forever.", pending, n)
		}

		// A cross-tenant scan really is cross-tenant: the row is visible with
		// no app.tenant_id set at all, which is what the workers rely on.
		var scanned int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM assistant_update_proposals
			 WHERE state = 'pending'`).Scan(&scanned); err != nil {
			t.Fatalf("the due-scan statement itself failed: %v", err)
		}
		if scanned < 1 {
			t.Fatalf("THE DUE SCAN RETURNED NOTHING: the workers' actual query shape "+
				"found %d pending proposals across all tenants, want at least 1", scanned)
		}

		// THE NEGATIVE HALF. The decision itself must be out of reach. This is
		// the exact statement a compromised or buggy agent-request path would
		// issue, and DECISION 8's guarantee depends on it not landing.
		//
		// ASSERTED ON ROWS TOUCHED, NOT ON AN ERROR. An RLS policy refusing an
		// UPDATE raises nothing at all; it filters the row away and the
		// statement succeeds against zero rows. See m133ExpectNoRows.
		decided, err := m133ExpectNoRows(t, tx, `
			UPDATE assistant_update_proposals
			   SET state = 'approved_undispatched', decided_at = now(),
			       decided_by_user_id = $2
			 WHERE id = $1`, pending, impostor)
		if err != nil {
			// Louder than silence and still a refusal, but it must not be a
			// CHECK catching this particular statement by luck -- that would
			// leave the policy itself untested.
			if strings.Contains(err.Error(), "approval_names_a_human") ||
				strings.Contains(err.Error(), "consent_within_window") {
				t.Fatalf("the agent write was stopped by a CHECK constraint rather than "+
					"by the absence of a write policy, so this proof does not test the "+
					"policy at all: %v", err)
			}
			t.Logf("the agent decision attempt errored rather than matching zero rows, "+
				"which is the louder of the two refusals: %v", err)
		} else if decided != 0 {
			t.Fatalf("THE AGENT CONTEXT DECIDED A PROPOSAL: proposal %s was moved to "+
				"'approved_undispatched' under app.agent, naming %s as the approver, "+
				"touching %d row(s). assistant_update_proposals_agent is FOR ALL rather "+
				"than FOR SELECT. app.agent is the scope an authenticated agent->CP "+
				"request runs under, so this is a machine approving a change to a live "+
				"site -- the one thing ADR-061 and DECISION 8 exist to make "+
				"unrepresentable.", pending, impostor, decided)
		}

		// The same must hold for the sweep's own write, which has to go through
		// InTenantTx instead. If this were admitted here the policy would be
		// FOR ALL in all but name.
		swept, err := m133ExpectNoRows(t, tx, `
			UPDATE assistant_update_proposals
			   SET state = 'expired'
			 WHERE id = $1 AND state = 'pending'`, pending)
		if err == nil && swept != 0 {
			t.Fatalf("THE AGENT CONTEXT EXPIRED A PROPOSAL: the sweep's own write was "+
				"admitted under app.agent, touching %d row(s). It must run under "+
				"InTenantTx, where tenant_isolation admits it and the tenant is named. "+
				"See (7)(g).", swept)
		}

		// THE TRAP, DEMONSTRATED ON PURPOSE. .claude/rules/go-control-plane.md
		// carries "An RLS policy scoped FOR SELECT breaks a locking read":
		// PostgreSQL applies the UPDATE policy to SELECT ... FOR UPDATE, so a
		// locking read under app.agent matches nothing here -- with NO ERROR.
		// That is m84/#96, where scheduled backups stopped firing for every
		// install, silently.
		//
		// This table accepts that trade because nothing may write it
		// cross-tenant (the rule's precondition is deliberately false here) and
		// the claim is a tenant-scoped compare-and-set instead, per (7)(g). The
		// behaviour is asserted rather than described so the next person to
		// write the dispatch worker meets it in a test name and not at 3am.
		var locked int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM (
				SELECT 1 FROM assistant_update_proposals
				 WHERE id = $1 FOR UPDATE
			) t`, pending).Scan(&locked); err != nil {
			// An error here is also acceptable and is the louder failure mode;
			// what must never happen is a silent zero that reads as "nothing due".
			t.Logf("the cross-tenant locking read errored rather than returning zero, "+
				"which is the louder of the two failure modes: %v", err)
		} else if locked != 0 {
			t.Fatalf("THE CROSS-TENANT LOCKING READ RETURNED %d ROWS. The agent policy "+
				"admits an UPDATE, so it is FOR ALL rather than FOR SELECT and the "+
				"decision columns are reachable from the service context. See m133 "+
				"DECISION 5.", locked)
		}

		inserted := m133ExpectRefused(t, tx, `
			INSERT INTO assistant_update_proposals
			    (tenant_id, site_id, proposed_by_grant_id, component_type,
			     component_slug, from_version, to_version, presented_digest,
			     state, expires_at)
			VALUES ($1, $2, $3, $4, 'agent-inserted', '1.0.0', '1.1.0', $5,
			        'pending', now() + interval '1 hour')`,
			tenant, site, uuid.New(), m133ComponentType, m133Digest("agent-inserted"))
		if inserted == nil {
			t.Fatalf("THE AGENT CONTEXT CREATED A PROPOSAL: a cross-tenant service " +
				"context inserted a proposal for a tenant it was not acting for. " +
				"Proposals are created by the propose tool under a tenant scope.")
		}
		return nil
	}); err != nil {
		t.Fatalf("agent-context-can-scan-but-cannot-decide: %v", err)
	}

	// AND THE ROW IS UNCHANGED. The refusals above must have been refusals, not
	// writes that happened to roll back with the savepoint.
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (read back after the agent attempts)")
		var state string
		var decider *uuid.UUID
		if err := tx.QueryRow(ctx, `
			SELECT state, decided_by_user_id
			  FROM assistant_update_proposals WHERE id = $1`, pending).
			Scan(&state, &decider); err != nil {
			return err
		}
		if state != "pending" {
			t.Fatalf("the proposal left 'pending' after the agent attempts: now %q", state)
		}
		if decider != nil {
			t.Fatalf("the proposal names a decider (%s) after the agent attempts", *decider)
		}
		return nil
	}); err != nil {
		t.Fatalf("read back after the agent attempts: %v", err)
	}
}

// TestApprovalRecordCannotBeDestroyedAsAppRole proves the two erasure paths a
// review reached by execution against the pre-review schema, both closed by
// m133 DECISION 9.
//
// WHY ONE TEST AND NOT TWO. They are the same defect at two grains. Section (5)
// of the migration argues that presented_digest and expires_at cannot be
// rewritten, and DECISION 8 argues that an approval always names a human. Both
// arguments are about a COLUMN, and a column is only as durable as the row that
// carries it and the states that row can be moved into. Two ways to erase the
// record therefore remained open:
//
//	the whole row, by DELETE -- the application role held it, observed
//	relacl wpmgr_app=ard/wpmgr, where the 'd' is DELETE;
//
//	the approver alone, in place, by moving an approved row to 'rejected'
//	while nulling decided_by_user_id -- legal before DECISION 9, because
//	'rejected' was the one decided state with no requirement to name anyone.
//
// A proof of either half alone passes on a schema where the other is open, and
// in both cases what is destroyed is the evidence the approval was supposed to
// leave behind.
//
// WHAT THIS DOES NOT CLAIM. It does not prove the decision is immutable. The
// decider can still be REPLACED with a different human, and states are still
// not terminal; both need a transition guard, which needs a trigger this tree
// does not have. Section (6) of the migration names them in full. This proves
// erasure specifically: the record cannot be made to disappear.
//
// Mutations 5 and 6, planted and watched. Planting
//
//	GRANT DELETE ON assistant_update_proposals TO wpmgr_app;
//
// makes the FIRST assertion fail with "the approval record was deleted", and
//
//	ALTER TABLE assistant_update_proposals
//	    DROP CONSTRAINT assistant_update_proposals_rejection_names_a_human_check;
//
// makes the third fail with "the approver was erased in place".
func TestApprovalRecordCannotBeDestroyedAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	tenant := seedTenant(t, pool, "m133-durable-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")

	admin := connectAdmin(t, pool)
	approver := seedUserRow(t, admin, "m133-durable-"+uuid.NewString()[:8]+"@example.com")
	rejecter := seedUserRow(t, admin, "m133-rejecter-"+uuid.NewString()[:8]+"@example.com")
	admin.Close()

	approved := m133InsertPending(t, pool, tenant, site, uuid.New(), "durable-record")
	spare := m133InsertPending(t, pool, tenant, site, uuid.New(), "spare-for-rejection")
	// Seeded out here, not inside the proof transaction below: m133InsertPending
	// opens its own InTenantTx, and acquiring a second connection from inside a
	// transaction that already holds one is how a pool-sized-1 run deadlocks.
	sweepControl := m133InsertPending(t, pool, tenant, site, uuid.New(), "sweep-control")

	// Approve it through the production dispatch, the way DECISION 4 and (7)(c)
	// require: decided_at evaluated by the database, never supplied.
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (approve, so there is a record to destroy)")
		tag, err := tx.Exec(ctx, `
			UPDATE assistant_update_proposals
			   SET state = 'approved_undispatched', decided_at = now(),
			       decided_by_user_id = $2
			 WHERE id = $1 AND state = 'pending'`, approved, approver)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("seeding an approved row affected %d rows, not 1", tag.RowsAffected())
		}
		return nil
	}); err != nil {
		t.Fatalf("seed an approved proposal: %v", err)
	}

	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (the approval record cannot be destroyed)")

		// THE LEAK ASSERTION, FIRST. The whole row.
		gone := m133ExpectRefused(t, tx,
			`DELETE FROM assistant_update_proposals WHERE id = $1`, approved)
		if gone == nil {
			t.Fatalf("THE APPROVAL RECORD WAS DELETED: proposal %s was removed by "+
				"wpmgr_app. DELETE is granted where the migration revokes it. Every "+
				"immutability guarantee in section (5) is void against a writer who "+
				"can drop the row instead of editing it: the approval simply stops "+
				"having happened, and no audit read can tell that it ever did.", approved)
		}
		if !m133IsPermissionDenied(gone) {
			t.Fatalf("the DELETE was refused, but not by the privilege that exists to "+
				"refuse it -- expected 42501: %v", gone)
		}

		// TRUNCATE is DELETE without a WHERE clause and is revoked with it.
		truncated := m133ExpectRefused(t, tx, `TRUNCATE assistant_update_proposals`)
		if truncated == nil {
			t.Fatalf("EVERY APPROVAL RECORD WAS DELETED AT ONCE: wpmgr_app truncated " +
				"assistant_update_proposals. Revoking DELETE while leaving TRUNCATE " +
				"grants the same erasure with a shorter statement.")
		}

		// THE SECOND ERASURE PATH: the approver alone, in place. This is the
		// statement the review executed against the pre-review schema.
		erased := m133ExpectRefused(t, tx, `
			UPDATE assistant_update_proposals
			   SET state = 'rejected', decided_by_user_id = NULL
			 WHERE id = $1`, approved)
		if erased == nil {
			t.Fatalf("THE APPROVER WAS ERASED IN PLACE: proposal %s was moved to "+
				"'rejected' with decided_by_user_id set to NULL, so who approved it is "+
				"unrecoverable while the row still reads as an ordinary decided row. "+
				"assistant_update_proposals_rejection_names_a_human_check is missing. "+
				"decided_by_user_id has to stay inside the column UPDATE grant for an "+
				"approval to write it at all, so the STATE it lands in is what has to "+
				"require a name.", approved)
		}
		if !strings.Contains(erased.Error(), "rejection_names_a_human") {
			t.Fatalf("the anonymising rejection was refused, but not by the constraint "+
				"that exists to refuse it: %v", erased)
		}

		// The same hole written straight in rather than transitioned into.
		anonInsert := m133ExpectRefused(t, tx, `
			INSERT INTO assistant_update_proposals
			    (tenant_id, site_id, proposed_by_grant_id, component_type,
			     component_slug, from_version, to_version, presented_digest,
			     state, decided_at, expires_at)
			VALUES ($1, $2, $3, $4, 'inserted-rejected', '1.0.0', '1.1.0', $5,
			        'rejected', now(), now() + interval '1 hour')`,
			tenant, site, uuid.New(), m133ComponentType, m133Digest("inserted-rejected"))
		if anonInsert == nil {
			t.Fatalf("AN ANONYMOUS REJECTION WAS INSERTED DIRECTLY: a decided row was " +
				"written with no decider. The constraint must hold on INSERT as well " +
				"as on the transition, or the transition guard is just a speed bump.")
		}
		if !strings.Contains(anonInsert.Error(), "rejection_names_a_human") {
			t.Fatalf("the anonymous rejected INSERT was refused, but not by the "+
				"constraint that exists to refuse it: %v", anonInsert)
		}

		// POSITIVE CONTROL 1. A REAL rejection, naming its human, must still
		// work. A constraint refusing every rejection would pass every
		// assertion above and make the product unusable.
		tag, err := tx.Exec(ctx, `
			UPDATE assistant_update_proposals
			   SET state = 'rejected', decided_at = now(), decided_by_user_id = $2
			 WHERE id = $1 AND state = 'pending'`, spare, rejecter)
		if err != nil {
			t.Fatalf("OVER-FIRING: a rejection naming a human was refused: %v", err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("OVER-FIRING: rejecting with a named human affected %d rows, not 1",
				tag.RowsAffected())
		}

		// POSITIVE CONTROL 2. Expiry still names nobody, and the sweep's own
		// statement must stay legal -- requiring a human of 'rejected' must not
		// leak onto the one decided-looking state that has no human to name.
		swept, err := tx.Exec(ctx, `
			UPDATE assistant_update_proposals
			   SET state = 'expired'
			 WHERE id = $1 AND state = 'pending'`, sweepControl)
		if err != nil {
			t.Fatalf("OVER-FIRING: the expiry sweep's statement was refused: %v\n"+
				"Expiry names nobody by design (DECISION 4); DECISION 9 must not have "+
				"made an unattributed outcome impossible in general.", err)
		}
		if swept.RowsAffected() != 1 {
			t.Fatalf("OVER-FIRING: expiring a pending proposal affected %d rows, not 1",
				swept.RowsAffected())
		}

		// AND THE RECORD IS STILL THERE, naming the same human. The point of
		// everything above.
		var state string
		var decider *uuid.UUID
		if err := tx.QueryRow(ctx, `
			SELECT state, decided_by_user_id
			  FROM assistant_update_proposals WHERE id = $1`, approved).
			Scan(&state, &decider); err != nil {
			t.Fatalf("read back the approval record: %v", err)
		}
		if state != "approved_undispatched" {
			t.Fatalf("the approval record survived but its state moved to %q", state)
		}
		if decider == nil || *decider != approver {
			t.Fatalf("the approval record survived but no longer names the approver: got %v, want %s",
				decider, approver)
		}
		return nil
	}); err != nil {
		t.Fatalf("approval-record-cannot-be-destroyed: %v", err)
	}
}
