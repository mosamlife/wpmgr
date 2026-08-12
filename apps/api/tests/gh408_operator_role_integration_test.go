package tests

// gh408_operator_role_integration_test.go, GH #408 findings 2 and 3: the
// documented recovery statements do not work on the connection a self-hoster
// actually has.
//
// site_object_reclaim is ENABLE + FORCE ROW LEVEL SECURITY. The remedy printed
// in the m113 header, repeated in the CHANGELOG, and the UPDATE the reclaim
// worker writes into last_error were both authored against a superuser
// connection. As wpmgr_app (NOSUPERUSER, NOBYPASSRLS) with no GUC set:
//
//	the INSERT is refused outright by the WITH CHECK (SQLSTATE 42501)
//	the UPDATE is HIDDEN by the USING clause and reports success having changed
//	nothing (rows=0, err=nil)
//
// The second is the worse of the two and is this project's signature defect:
// announcing success over having done nothing. It is also the ONLY remedy for
// objects orphaned before 0.61.132, and the reporting account has 90 of them.
//
// THE CARDINAL TEST IN THIS FILE is
// TestGH408_ReclaimCommandWorksAsTheApplicationRole. It runs against the pool
// startPostgres returns, which is wpmgr_app, and it must NEVER use connectAdmin
// for the statements under test: a superuser connection is exactly what makes
// the shipped remedy look like it works. The existing
// gh402_reclaim_kind_check_integration_test.go asserts "the operator's
// correction is accepted" using admin.Exec, which is why it could never have
// caught either finding.
//
// NOT RUN BY CI (apps/api/tests is excluded from the fast lane by owner
// decision). Run with `make test-integration` from the repository root.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// gh408RequireApplicationRole fails the test unless the pool really is a role
// RLS applies to. Without this the whole file could pass vacuously, which is
// precisely how the shipped remedy was validated.
func gh408RequireApplicationRole(t *testing.T, pool *db.Pool) {
	t.Helper()
	var name string
	var super, bypass bool
	if err := pool.QueryRow(context.Background(),
		`SELECT current_user, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&name, &super, &bypass); err != nil {
		t.Fatalf("read the connected role: %v", err)
	}
	if super || bypass {
		t.Fatalf("connected as %q (rolsuper=%v, rolbypassrls=%v): RLS does not apply to this role, "+
			"so nothing in this file proves anything", name, super, bypass)
	}
	t.Logf("connected as %q (rolsuper=%v, rolbypassrls=%v)", name, super, bypass)
}

// gh408DocumentedBackfillInsert is the m113 header statement, verbatim, as an
// operator would paste it.
const gh408DocumentedBackfillInsert = `INSERT INTO site_object_reclaim (tenant_id, site_id, kind)
   VALUES ($1, $2, 'backup_manifest')
   ON CONFLICT (tenant_id, site_id, kind) DO UPDATE
     SET completed_at = NULL, attempts = 0, next_attempt_at = now()`

// gh408WorkerPrintedUpdate is the statement reclaim_worker.go writes into
// last_error, verbatim.
const gh408WorkerPrintedUpdate = `UPDATE site_object_reclaim
   SET kind = 'backup_manifest', attempts = 0, next_attempt_at = now()
   WHERE id = $1`

// ---------------------------------------------------------------------------
// THE CARDINAL TEST
// ---------------------------------------------------------------------------

func TestGH408_ReclaimCommandWorksAsTheApplicationRole(t *testing.T) {
	pool := startPostgres(t) // wpmgr_app: NOSUPERUSER, NOBYPASSRLS
	admin := connectAdmin(t, pool)
	defer admin.Close()
	gh408RequireApplicationRole(t, pool)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh408-op-"+uuid.NewString()[:8])
	orphaned := uuid.New() // a site deleted long ago: the whole point of a backfill

	// FINDING 2: backfilling a site orphaned before m113 must work for the role
	// the self-hoster has.
	rows, err := gh408BackfillOrphanedSite(t, pool, tenant, orphaned)
	if err != nil {
		t.Fatalf("backfilling an orphaned site failed as the application role: %v\n"+
			"This is the ONLY remedy for objects orphaned before 0.61.132", err)
	}
	if rows != 1 {
		t.Fatalf("backfilling an orphaned site affected %d rows, want 1. A recovery path that "+
			"reports success having done nothing is worse than none", rows)
	}
	if got := gh402CountTasks(t, admin, tenant, orphaned); got != 1 {
		t.Fatalf("the backfill claimed to work but the table holds %d rows for that site", got)
	}

	// It is re-runnable: against an already completed row it REOPENS rather than
	// silently dropping the work, which is the m113 ON CONFLICT contract.
	if _, cerr := admin.Exec(ctx,
		`UPDATE site_object_reclaim SET completed_at = now() WHERE tenant_id = $1 AND site_id = $2`,
		tenant, orphaned); cerr != nil {
		t.Fatalf("close the task out of band: %v", cerr)
	}
	rows, err = gh408BackfillOrphanedSite(t, pool, tenant, orphaned)
	if err != nil || rows != 1 {
		t.Fatalf("reopening an already completed task as the application role: rows=%d err=%v, want rows=1 err=nil", rows, err)
	}
	var open bool
	if err := admin.QueryRow(ctx,
		`SELECT completed_at IS NULL FROM site_object_reclaim WHERE tenant_id = $1 AND site_id = $2`,
		tenant, orphaned).Scan(&open); err != nil {
		t.Fatalf("read the reopened task: %v", err)
	}
	if !open {
		t.Error("the task was not reopened, so the backfill silently dropped the work")
	}

	// FINDING 3: correcting a stuck task must work for that same role. This is
	// the statement the worker itself hands the operator.
	stuck := gh408SeedStuckTask(t, admin, tenant)
	rows, err = gh408RetryStuckTask(t, pool, stuck)
	if err != nil {
		t.Fatalf("correcting a stuck task failed as the application role: %v", err)
	}
	if rows != 1 {
		t.Fatalf("correcting a stuck task affected %d rows, want 1. RLS hides the row from the "+
			"USING clause, so the UPDATE matches nothing and Postgres reports no error: "+
			"the remedy announces success over having done nothing", rows)
	}
	var attempts int32
	var kind string
	if err := admin.QueryRow(ctx,
		`SELECT attempts, kind FROM site_object_reclaim WHERE id = $1`, stuck).Scan(&attempts, &kind); err != nil {
		t.Fatalf("read the corrected task: %v", err)
	}
	if attempts != 0 || kind != backup.ReclaimKindBackupManifest {
		t.Errorf("corrected task has attempts=%d kind=%q, want 0 and %q", attempts, kind, backup.ReclaimKindBackupManifest)
	}
}

// ---------------------------------------------------------------------------
// The negative controls: what the SHIPPED remedies do on that same connection.
//
// These are the tests that establish WHY a supported entry point exists, and
// they run the documented statements verbatim so they cannot drift from the
// documentation. Every claim in them is ASSERTED. A control that logs its
// finding instead of asserting it is a control that cannot fail, and one of
// those in the file documenting a defect is the defect's own shape applied to
// its proof.
// ---------------------------------------------------------------------------

func TestGH408_TheShippedRemediesDoNotWorkAsTheApplicationRole(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	gh408RequireApplicationRole(t, pool)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh408-neg-"+uuid.NewString()[:8])
	orphaned := uuid.New()

	// The m113 header INSERT: refused, loudly. 42501 is insufficient_privilege,
	// which is what a WITH CHECK violation reports.
	_, ierr := pool.Exec(ctx, gh408DocumentedBackfillInsert, tenant, orphaned)
	if ierr == nil {
		t.Error("the m113 header INSERT succeeded as the application role. If that is now true, " +
			"finding 2 is fixed by something other than this change and this control is stale")
	} else {
		t.Logf("m113 header INSERT as %s: %v", "wpmgr_app", ierr)
	}

	// The worker's printed UPDATE: silent. This is the dangerous one, and it is
	// the entire reason the wpmgr-cli reclaim family exists, so it is ASSERTED
	// rather than logged.
	//
	// It used to be recorded with t.Logf inside `if uerr == nil && rows == 0`,
	// which is a negative control that cannot fail: the behaviour this whole
	// change is premised on could have changed under it and the test would still
	// have been green. The claim has three independent parts and each fails on
	// its own here, because each would falsify the premise differently. An error
	// would mean Postgres does complain after all and a caveat WOULD have been an
	// adequate fix; a non-zero row count would mean the statement works as
	// documented and no command family was needed; an altered row would mean it
	// half-worked, which is worse than either.
	stuck := gh408SeedStuckTask(t, admin, tenant)
	beforeKind, beforeAttempts, beforeNext, beforeUpdated := gh408ReadTaskRow(t, admin, stuck)

	tag, uerr := pool.Exec(ctx, gh408WorkerPrintedUpdate, stuck)
	t.Logf("worker printed UPDATE as wpmgr_app: rows=%d err=%v", tag.RowsAffected(), uerr)

	if uerr != nil {
		t.Errorf("the statement the worker printed returned an error as wpmgr_app: %v\n"+
			"If Postgres now refuses it, finding 3 is no longer that the remedy reports success "+
			"having changed nothing, and the reasoning behind this whole change needs rereading", uerr)
	}
	if rows := tag.RowsAffected(); rows != 0 {
		t.Errorf("the statement the worker printed affected %d rows as wpmgr_app, want 0.\n"+
			"If it now works on the connection a self-hoster has, the premise of GH #408 finding 3 "+
			"is false and this control is stale", rows)
	}
	afterKind, afterAttempts, afterNext, afterUpdated := gh408ReadTaskRow(t, admin, stuck)
	if afterKind != beforeKind || afterAttempts != beforeAttempts ||
		!afterNext.Equal(beforeNext) || !afterUpdated.Equal(beforeUpdated) {
		t.Errorf("the row CHANGED under a statement that reported zero rows affected:\n"+
			"  before kind=%q attempts=%d next_attempt_at=%s updated_at=%s\n"+
			"  after  kind=%q attempts=%d next_attempt_at=%s updated_at=%s\n"+
			"rows=0 with no error must mean the row is byte-for-byte untouched",
			beforeKind, beforeAttempts, beforeNext, beforeUpdated,
			afterKind, afterAttempts, afterNext, afterUpdated)
	}

	// And the operator cannot even READ the table to discover the id that a
	// "just set app.tenant_id first" instruction would need.
	var visible int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM site_object_reclaim`).Scan(&visible); err != nil {
		t.Fatalf("count visible rows: %v", err)
	}
	t.Logf("rows visible to wpmgr_app with no GUC: %d", visible)
	if visible != 0 {
		t.Errorf("expected 0 rows visible with no GUC, got %d: the site-scope and tenant policies "+
			"are not doing what this file assumes", visible)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// gh408SeedStuckTask writes a task in the state the worker's GUARD 1 produces:
// open, past the attempt cap, with a kind no prefix can be derived for. Seeded
// out of band because the database now refuses that kind, which is m115 working.
func gh408SeedStuckTask(t *testing.T, admin *db.Pool, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	if _, err := admin.Exec(ctx,
		`ALTER TABLE site_object_reclaim DROP CONSTRAINT IF EXISTS site_object_reclaim_kind_check`); err != nil {
		t.Fatalf("open the kind set to seed a pre-constraint row: %v", err)
	}
	var id uuid.UUID
	err := admin.QueryRow(ctx,
		`INSERT INTO site_object_reclaim (tenant_id, site_id, kind, attempts, last_error)
		 VALUES ($1, $2, 'backup_manifests', 8, 'unknown kind')
		 RETURNING id`, tenant, uuid.New()).Scan(&id)
	if err != nil {
		t.Fatalf("seed a stuck task: %v", err)
	}
	if _, rerr := admin.Exec(ctx,
		`ALTER TABLE site_object_reclaim
		   ADD CONSTRAINT site_object_reclaim_kind_check CHECK (kind IN ('backup_manifest')) NOT VALID`); rerr != nil {
		t.Fatalf("restore the kind constraint: %v", rerr)
	}
	return id
}

// gh408ReadTaskRow reads every column the printed UPDATE claims to set, through
// the ADMIN pool so RLS cannot hide the answer. Reading it as the application
// role would return no row at all, which is the very effect under test and would
// make "unchanged" indistinguishable from "invisible".
func gh408ReadTaskRow(t *testing.T, admin *db.Pool, id uuid.UUID) (string, int32, time.Time, time.Time) {
	t.Helper()
	var kind string
	var attempts int32
	var next, updated time.Time
	if err := admin.QueryRow(context.Background(),
		`SELECT kind, attempts, next_attempt_at, updated_at FROM site_object_reclaim WHERE id = $1`,
		id).Scan(&kind, &attempts, &next, &updated); err != nil {
		t.Fatalf("read the task row: %v", err)
	}
	return kind, attempts, next, updated
}

// gh408BackfillOrphanedSite is `wpmgr-cli reclaim site`'s code path, run against
// the application-role pool. It must NEVER be given an admin pool.
func gh408BackfillOrphanedSite(t *testing.T, pool *db.Pool, tenant, siteID uuid.UUID) (int64, error) {
	t.Helper()
	return backup.EnqueueSiteReclaim(context.Background(), pool, tenant, siteID)
}

// gh408RetryStuckTask is `wpmgr-cli reclaim retry`'s code path, same rule.
func gh408RetryStuckTask(t *testing.T, pool *db.Pool, taskID uuid.UUID) (int64, error) {
	t.Helper()
	return backup.RetryReclaimTask(context.Background(), pool, taskID, backup.ReclaimKindBackupManifest)
}

// ---------------------------------------------------------------------------
// THE SECOND DANGER: the operator path must not become the eighth door.
// ---------------------------------------------------------------------------

// TestGH408_SiteScopeStillRefusesTheAgentLane is the regression guard on the one
// thing this change must not do.
//
// m113 added a RESTRICTIVE site-scope policy because a collaborator invited to
// exactly one site could otherwise reach reclaim rows naming every other site in
// the organisation, and m112 shipped the same week after seven
// privilege-escalation doors kept appearing in a domain missing exactly that
// policy. Making the operator path work must not open the eighth.
//
// It cannot, structurally: RESTRICTIVE policies are AND-combined with permissive
// ones, so the agent branch can only ever be INTERSECTED with site-scope, never
// unioned with it. This asserts that rather than trusting it.
func TestGH408_SiteScopeStillRefusesTheAgentLane(t *testing.T) {
	pool := startPostgres(t)
	gh408RequireApplicationRole(t, pool)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh408-scope-"+uuid.NewString()[:8])
	mine := uuid.New()
	theirs := uuid.New()

	// A site-scoped collaborator's GUCs, PLUS the agent GUC, which is the
	// strongest thing a confused caller could set. The write must still be
	// refused, BY NAME.
	err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		for _, stmt := range []struct{ sql, arg string }{
			{"SELECT set_config('app.site_scope', 'on', true)", ""},
			{"SELECT set_config('app.allowed_site_ids', $1, true)", mine.String()},
		} {
			var e error
			if stmt.arg == "" {
				_, e = tx.Exec(ctx, stmt.sql)
			} else {
				_, e = tx.Exec(ctx, stmt.sql, stmt.arg)
			}
			if e != nil {
				return e
			}
		}
		_, e := tx.Exec(ctx,
			`INSERT INTO site_object_reclaim (tenant_id, site_id, kind) VALUES ($1, $2, 'backup_manifest')`,
			tenant, theirs)
		return e
	})
	if err == nil {
		t.Fatal("a caller with app.site_scope='on' and a non-matching allow list WROTE a reclaim row " +
			"for another site. That is the eighth door m112 exists to prevent")
	}
	if !strings.Contains(err.Error(), "site_object_reclaim_site_scope") {
		t.Errorf("the write was refused by something other than the site-scope policy: %v", err)
	}
	t.Logf("agent GUC + site_scope + non-matching allow list: %v", err)

	// And the ordinary operator path leaves app.site_scope UNSET, which is what
	// makes the restrictive policy a tautology for it (m113 says the same of its
	// own two writers). Asserted, not assumed.
	var scope string
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(current_setting('app.site_scope', true), '')`).Scan(&scope)
	}); err != nil {
		t.Fatalf("read app.site_scope inside InAgentTx: %v", err)
	}
	if scope != "" {
		t.Errorf("InAgentTx set app.site_scope=%q; the operator lane must leave it unset", scope)
	}
}
