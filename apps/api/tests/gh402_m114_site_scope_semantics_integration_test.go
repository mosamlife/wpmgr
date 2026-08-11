// gh402_m114_site_scope_semantics_integration_test.go: what m114 GRANTS, not
// merely what it registers.
//
// WHY THIS FILE HAD TO EXIST SEPARATELY FROM THE OTHER m114 TEST.
//
// m114 is the only route by which a database that ran the pre-review m113 ever
// receives the collaborator site-scope policy. Its correctness was asserted
// exactly once, as "a row exists in pg_policies with that name". That is not a
// statement about access control. Dropping AS RESTRICTIVE from m114's CREATE
// POLICY passed the entire suite: a permissive policy is OR-combined with
// site_object_reclaim_tenant_isolation, so instead of subtracting it GRANTS, and
// the site boundary vanishes on precisely the databases that upgraded rather
// than started fresh.
//
// Nothing else could have caught it. Every other RLS proof in this branch runs
// against a database built by startPostgres, where m113 creates the policy and
// m114 is a no-op, so none of them ever executes m114's CREATE POLICY at all.
// The one test that does execute it looked only at the catalogue.
//
// So this file builds the affected database (fully migrated, regressed to the
// pre-review m113 shape, m114 marked unapplied), boots the migrator the way the
// server does, and then asserts the SAME SEMANTICS the fresh-database proofs
// assert, through the same repository code path and the same non-superuser role
// production uses. The policy under test here is m114's text and only m114's:
// the regression drops m113's, and migrate.go will not revisit m113.
//
// THE MUTATION THAT MUST TURN THIS RED: delete "AS RESTRICTIVE" from m114's
// CREATE POLICY. Also good: point its USING clause at a different GUC than
// app.allowed_site_ids, or drop its WITH CHECK.
//
// NOT RUN BY CI (apps/api/tests is excluded from the fast lane). Run with
// `make test-integration`.
package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// gh402ConvergeFromPreReviewM113 leaves the database in the state a developer
// machine or preview environment that ran the first m113 reaches after a
// restart on this branch: m113 still recorded as applied and never re-read, its
// defects removed by m114 alone.
func gh402ConvergeFromPreReviewM113(t *testing.T, admin *db.Pool) {
	t.Helper()

	gh402RegressToPreReviewM113(t, admin)
	if gh402SiteScopePolicyExists(t, admin) {
		t.Fatal("the regression left a site-scope policy behind, so the policy under test would " +
			"be m113's rather than m114's and this file would prove nothing")
	}
	if err := admin.Migrate(context.Background()); err != nil {
		t.Fatalf("boot migration: %v", err)
	}
	if !gh402SiteScopePolicyExists(t, admin) {
		t.Fatal("m114 did not create the site-scope policy at all")
	}
}

func TestGH402_M114_SiteScopeSemanticsOnAConvergedDatabase(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	gh402ConvergeFromPreReviewM113(t, admin)

	// A collaborator invited to siteA must see siteA's reclaim row and no other.
	t.Run("read is confined to the granted site", func(t *testing.T) {
		f := gh402SeedScopeFixture(t, pool, admin)

		var total int
		var sawB bool
		if err := gh402AsCollaborator(pool, f.tenant, []uuid.UUID{f.siteA}, func(tx pgx.Tx) error {
			if e := tx.QueryRow(ctx, `SELECT count(*) FROM site_object_reclaim`).Scan(&total); e != nil {
				return e
			}
			return tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM site_object_reclaim WHERE site_id = $1)`,
				f.siteB).Scan(&sawB)
		}); err != nil {
			t.Fatalf("collaborator read: %v", err)
		}
		if total != 1 {
			t.Errorf("on a converged database a collaborator invited to ONE site sees %d reclaim "+
				"rows, want 1. m114's policy is not subtracting, which is what a policy created "+
				"without AS RESTRICTIVE does: it is OR-combined with tenant isolation and grants "+
				"the whole organisation", total)
		}
		if sawB {
			t.Error("on a converged database a collaborator invited to siteA can read siteB's " +
				"reclaim row")
		}
	})

	// The half that costs something. Closing another site's row leaves that
	// site's manifests in the bucket with nothing left naming them, which is
	// GH #402 reinstated through a different door.
	t.Run("cannot close or delete another site's task", func(t *testing.T) {
		f := gh402SeedScopeFixture(t, pool, admin)

		// Two attempts, two transactions, two independent verdicts. A refusal by
		// error is still as good as a refusal by zero rows, but neither statement
		// can now decide whether the other is tried, and a statement that is not
		// tried is a failure rather than a zero. See gh402CollaboratorWrite.
		updated := gh402CollaboratorWrite(t, pool, f.tenant, []uuid.UUID{f.siteA},
			`UPDATE site_object_reclaim SET completed_at = now() WHERE site_id = $1`, f.siteB)
		deleted := gh402CollaboratorWrite(t, pool, f.tenant, []uuid.UUID{f.siteA},
			`DELETE FROM site_object_reclaim WHERE site_id = $1`, f.siteB)

		if updated != 0 {
			t.Errorf("on a converged database a collaborator invited to siteA closed siteB's "+
				"reclaim task (%d rows). Those manifests are now in the bucket with nothing left "+
				"that names them", updated)
		}
		if deleted != 0 {
			t.Errorf("on a converged database a collaborator invited to siteA deleted siteB's "+
				"reclaim task (%d rows)", deleted)
		}

		var completedAt *string
		switch err := admin.QueryRow(ctx,
			`SELECT completed_at::text FROM site_object_reclaim WHERE site_id = $1`, f.siteB).
			Scan(&completedAt); {
		case errors.Is(err, pgx.ErrNoRows):
			// Read back outside the attack, so this is the row's real fate and not
			// a policy hiding it. Reported here rather than left to surface as a
			// scan failure, which reads like a broken fixture.
			t.Fatal("siteB's reclaim task is GONE after the attack; those manifests are now in " +
				"the bucket with nothing left anywhere that names them")
		case err != nil:
			t.Fatalf("read siteB task: %v", err)
		}
		if completedAt != nil {
			t.Error("siteB's reclaim task is closed after the attack")
		}
	})

	// The WITH CHECK half: m114's policy must refuse a forged row too.
	t.Run("cannot forge a task for another site", func(t *testing.T) {
		f := gh402SeedScopeFixture(t, pool, admin)
		victim := gh402SeedSite(t, admin, f.tenant) // a LIVE site, still present

		var insertErr error
		_ = gh402AsCollaborator(pool, f.tenant, []uuid.UUID{f.siteA}, func(tx pgx.Tx) error {
			_, insertErr = tx.Exec(ctx,
				`INSERT INTO site_object_reclaim (tenant_id, site_id, kind)
				 VALUES ($1, $2, 'backup_manifest')`, f.tenant, victim)
			return insertErr
		})
		if insertErr == nil {
			t.Error("on a converged database a collaborator invited to siteA wrote a reclaim task " +
				"naming a site it has no access to; m114's WITH CHECK is missing or permissive")
		}
		if got := gh402CountTasks(t, admin, f.tenant, victim); got != 0 {
			t.Errorf("%d forged reclaim task(s) exist for a live site", got)
		}
	})

	// And it must only ever subtract. If the reclaim worker lost sight of these
	// rows nothing would sweep anything, which is a worse outage than the bug.
	t.Run("an ordinary org member and the worker are unaffected", func(t *testing.T) {
		f := gh402SeedScopeFixture(t, pool, admin)

		var byMember int
		if err := pool.InTenantTx(ctx, f.tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT count(*) FROM site_object_reclaim WHERE tenant_id = $1`, f.tenant).
				Scan(&byMember)
		}); err != nil {
			t.Fatalf("org member read: %v", err)
		}
		if byMember != 2 {
			t.Errorf("an ordinary org member sees %d reclaim rows, want 2; m114's policy is "+
				"subtracting from principals it must not touch", byMember)
		}

		var byWorker int
		if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT count(*) FROM site_object_reclaim WHERE tenant_id = $1`, f.tenant).
				Scan(&byWorker)
		}); err != nil {
			t.Fatalf("worker read: %v", err)
		}
		if byWorker != 2 {
			t.Errorf("the reclaim worker sees %d rows, want 2; the sweep would never run", byWorker)
		}
	})

	// Last, and last on purpose: the catalogue. This is a diagnosis aid, not the
	// proof. It names the cause when the four subtests above go red, and on its
	// own it is exactly the assertion that let the permissive-policy mutation
	// through in the first place.
	t.Run("the policy Postgres holds is RESTRICTIVE and site scoped", func(t *testing.T) {
		var permissive, cmd, using, withCheck string
		if err := admin.QueryRow(ctx,
			`SELECT permissive, cmd, coalesce(qual, ''), coalesce(with_check, '')
			   FROM pg_policies
			  WHERE schemaname = 'public' AND tablename = 'site_object_reclaim'
			    AND policyname = 'site_object_reclaim_site_scope'`).
			Scan(&permissive, &cmd, &using, &withCheck); err != nil {
			t.Fatalf("read the policy back: %v", err)
		}
		if permissive != "RESTRICTIVE" {
			t.Errorf("m114 created a %s policy, want RESTRICTIVE. A permissive policy is "+
				"OR-combined with site_object_reclaim_tenant_isolation, so it widens access "+
				"instead of narrowing it", permissive)
		}
		if cmd != "ALL" {
			t.Errorf("m114's policy applies to %s, want ALL", cmd)
		}
		if using == "" || withCheck == "" {
			t.Errorf("m114's policy is missing a USING (%q) or a WITH CHECK (%q); one of reads or "+
				"writes is then unconstrained", using, withCheck)
		}
	})
}
