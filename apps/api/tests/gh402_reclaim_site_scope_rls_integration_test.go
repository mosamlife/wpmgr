// gh402_reclaim_site_scope_rls_integration_test.go: m113's site-scope
// isolation, asserted against the REAL schema (migrations + RLS) as a
// NON-SUPERUSER role.
//
// WHY THIS FILE EXISTS, AND WHY IT LOOKS LIKE email_site_scope_rls.
//
// site_object_reclaim is SITE-KEYED. Tenant isolation alone therefore refuses
// only another organisation, and leaves a collaborator invited to exactly one
// site able to reach the reclaim rows naming every other site in the
// organisation. m112 shipped one migration earlier for precisely this shape:
// three review rounds closed seven privilege-escalation doors in the email
// handlers, and the fourth round worked out that they kept appearing because
// the DATABASE had no opinion. m113 gets the policy on the way in instead of
// after the eighth door.
//
// The escalation here is not a data leak so much as a way to reinstate GH #402
// on someone else's site: marking another site's reclaim row completed, or
// cancelling it, leaves that site's manifests in the bucket with nothing left
// anywhere that names them. So the write assertions matter at least as much as
// the read one.
//
// HOW IT IS TESTED. The rows under attack are created THROUGH THE REPO, by the
// real DELETE /sites/{id} path (site.NewRepo(pool).Delete), not by a hand
// INSERT: a fake cannot fail to have a policy, and neither can a row the test
// wrote itself under superuser privileges. The attack then runs through the
// real transaction wrapper a site-scoped collaborator gets in production
// (db.Pool.InScopedTenantTx, which db.Pool.RunTenantTx dispatches to for
// Scope == "site"), never by hand-setting GUCs, so the test is wired to the
// same thing production is wired to.
//
// The non-superuser role is load-bearing and not incidental: a superuser, or
// any role with BYPASSRLS, ignores every policy in this file and all of it
// would pass vacuously. startPostgres provisions the plain wpmgr_app role.
package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// gh402AsCollaborator runs fn under the exact GUCs a site-scoped collaborator
// granted only `sites` gets in production.
//
// THIS IS THE MUTATION POINT for this file. Swapping InScopedTenantTx for the
// unscoped pool.InTenantTx leaves app.site_scope unset, which turns the
// RESTRICTIVE policy's first branch into a tautology and hands the caller the
// whole organisation. Every assertion below must go red when that is done; if
// any of them still passes, it is not testing the policy.
func gh402AsCollaborator(pool *db.Pool, tenant uuid.UUID, sites []uuid.UUID, fn func(tx pgx.Tx) error) error {
	return pool.InScopedTenantTx(context.Background(), tenant, uuid.New(), sites, fn)
}

// gh402ScopeFixture is one organisation with two sites, BOTH deleted through
// the real repo, so there are two genuine reclaim rows to reach for.
type gh402ScopeFixture struct {
	tenant       uuid.UUID
	siteA, siteB uuid.UUID
}

func gh402SeedScopeFixture(t *testing.T, pool *db.Pool, admin *db.Pool) gh402ScopeFixture {
	t.Helper()
	ctx := context.Background()

	f := gh402ScopeFixture{tenant: seedTenant(t, pool, "gh402-scope-"+uuid.NewString()[:8])}
	f.siteA = gh402SeedSite(t, admin, f.tenant)
	f.siteB = gh402SeedSite(t, admin, f.tenant)

	// Through the repo, which is the only thing that writes this table in
	// production.
	for _, s := range []uuid.UUID{f.siteA, f.siteB} {
		if err := site.NewRepo(pool).Delete(ctx, f.tenant, s); err != nil {
			t.Fatalf("delete site %s: %v", s, err)
		}
	}
	if got := gh402CountTasks(t, admin, f.tenant, f.siteA) + gh402CountTasks(t, admin, f.tenant, f.siteB); got != 2 {
		t.Fatalf("expected 2 reclaim rows to attack, got %d; this fixture is wrong", got)
	}
	return f
}

// A collaborator invited to siteA sees siteA's reclaim row and NOT siteB's.
func TestGH402_ReclaimSiteScope_ReadIsConfinedToTheGrantedSite(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	f := gh402SeedScopeFixture(t, pool, admin)

	var total int
	var sawB bool
	if err := gh402AsCollaborator(pool, f.tenant, []uuid.UUID{f.siteA}, func(tx pgx.Tx) error {
		if e := tx.QueryRow(ctx, `SELECT count(*) FROM site_object_reclaim`).Scan(&total); e != nil {
			return e
		}
		return tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM site_object_reclaim WHERE site_id = $1)`, f.siteB).Scan(&sawB)
	}); err != nil {
		t.Fatalf("collaborator read: %v", err)
	}
	if total != 1 {
		t.Errorf("a collaborator invited to ONE site sees %d reclaim rows, want 1", total)
	}
	if sawB {
		t.Error("a collaborator invited to siteA can read the reclaim row of siteB. " +
			"site_object_reclaim is site-keyed, so tenant isolation alone is not the boundary")
	}
}

// The one that actually costs something: a collaborator must not be able to
// close, cancel or back off another site's reclaim row. Closing it leaves that
// site's manifests in the bucket with nothing naming them, which is GH #402
// reinstated through a different door.
func TestGH402_ReclaimSiteScope_CannotCloseAnotherSitesTask(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	f := gh402SeedScopeFixture(t, pool, admin)

	var updated, deleted int64
	if err := gh402AsCollaborator(pool, f.tenant, []uuid.UUID{f.siteA}, func(tx pgx.Tx) error {
		tag, e := tx.Exec(ctx,
			`UPDATE site_object_reclaim SET completed_at = now() WHERE site_id = $1`, f.siteB)
		if e != nil {
			// A refusal by error is just as good an outcome as a refusal by
			// zero rows; record it as zero and let the assertions run.
			updated = 0
			return nil
		}
		updated = tag.RowsAffected()

		tag, e = tx.Exec(ctx, `DELETE FROM site_object_reclaim WHERE site_id = $1`, f.siteB)
		if e != nil {
			deleted = 0
			return nil
		}
		deleted = tag.RowsAffected()
		return nil
	}); err != nil {
		t.Fatalf("collaborator write: %v", err)
	}
	if updated != 0 {
		t.Errorf("a collaborator invited to siteA closed siteB's reclaim task (%d rows). "+
			"Those manifests are now in the bucket with nothing left that names them", updated)
	}
	if deleted != 0 {
		t.Errorf("a collaborator invited to siteA deleted siteB's reclaim task (%d rows)", deleted)
	}

	// The row is genuinely untouched, read back outside the attack.
	var completedAt *string
	if err := admin.QueryRow(ctx,
		`SELECT completed_at::text FROM site_object_reclaim WHERE site_id = $1`, f.siteB).
		Scan(&completedAt); err != nil {
		t.Fatalf("read siteB task: %v", err)
	}
	if completedAt != nil {
		t.Error("siteB's reclaim task is closed after the attack")
	}
}

// And must not be able to invent one for a site it was not invited to. A
// forged row naming a LIVE site is refused later by the worker's site-existence
// guard, but defence in depth is the point: the database should not accept it
// in the first place.
func TestGH402_ReclaimSiteScope_CannotForgeATaskForAnotherSite(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	f := gh402SeedScopeFixture(t, pool, admin)
	victim := gh402SeedSite(t, admin, f.tenant) // a LIVE site, still present

	// A refused INSERT aborts the Postgres transaction, so the error is
	// returned from fn (which rolls the wrapper back) rather than swallowed:
	// swallowing it just moves the failure to the commit and reports it as
	// something else entirely.
	var insertErr error
	_ = gh402AsCollaborator(pool, f.tenant, []uuid.UUID{f.siteA}, func(tx pgx.Tx) error {
		_, insertErr = tx.Exec(ctx,
			`INSERT INTO site_object_reclaim (tenant_id, site_id, kind)
			 VALUES ($1, $2, 'backup_manifest')`, f.tenant, victim)
		return insertErr
	})
	if insertErr == nil {
		t.Error("a collaborator invited to siteA wrote a reclaim task naming a site it has no " +
			"access to; the RESTRICTIVE policy's WITH CHECK is missing or permissive")
	}
	if got := gh402CountTasks(t, admin, f.tenant, victim); got != 0 {
		t.Errorf("%d forged reclaim task(s) exist for a live site", got)
	}
}

// The policy must only ever SUBTRACT. An ordinary org member and the reclaim
// worker itself both leave app.site_scope unset, so both must be completely
// unaffected: if the worker lost sight of these rows nothing would ever sweep
// anything, which is a worse outage than the bug.
func TestGH402_ReclaimSiteScope_OrgMemberAndWorkerAreUnaffected(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	f := gh402SeedScopeFixture(t, pool, admin)

	var byMember int
	if err := pool.InTenantTx(ctx, f.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM site_object_reclaim`).Scan(&byMember)
	}); err != nil {
		t.Fatalf("org member read: %v", err)
	}
	if byMember != 2 {
		t.Errorf("an ordinary org member sees %d reclaim rows, want 2; the restrictive policy "+
			"is subtracting from principals it must not touch", byMember)
	}

	var byWorker int
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM site_object_reclaim WHERE tenant_id = $1`, f.tenant).Scan(&byWorker)
	}); err != nil {
		t.Fatalf("worker read: %v", err)
	}
	if byWorker != 2 {
		t.Errorf("the reclaim worker sees %d rows, want 2; the sweep would never run", byWorker)
	}
}
