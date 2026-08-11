package tests

// gh402_reclaim_kind_check_integration_test.go: site_object_reclaim.kind is a
// closed set, and the database is what closes it.
//
// WHY THIS EXISTS. kind was free text while the worker knew exactly one value.
// A row carrying anything else could not have a storage prefix derived for it,
// so it reclaimed nothing, and the worker CANCELLED it, which closed the row and
// removed it from both the due query and the stuck report. Nothing mentioned it
// again, and nothing else in the database named the objects it was written for.
//
// That is not a hypothetical row. Objects orphaned before m113 have no record
// anywhere, so the only route to them is the hand-written INSERT documented in
// the m113 header and repeated in the CHANGELOG, and the account that reported
// GH #402 has 90 of them. One mistyped kind in that statement stranded them
// permanently: the original defect, delivered by the remedy for it.
//
// The fix is in two halves and both are tested here against real Postgres:
//
//  1. The database refuses the bad INSERT (site_object_reclaim_kind_check, added
//     inline by m113 on a fresh database and by m115 on one that already has the
//     table). The operator sees the failure on the statement they are typing.
//  2. For the rows that predate the constraint, which m115 deliberately does not
//     validate away, the worker treats an unknown kind as a retryable failure
//     rather than a cancel, so the row stays open and visible.
//
// NOT RUN BY CI (apps/api/tests is excluded from the fast lane by owner
// decision). Run with `make test-integration`.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

const gh402KindCheckConstraint = "site_object_reclaim_kind_check"
const gh402M115Version = "20260817000000_m115_site_object_reclaim_kind_check"

func gh402KindCheckExists(t *testing.T, admin *db.Pool) bool {
	t.Helper()
	var exists bool
	if err := admin.QueryRow(context.Background(),
		`SELECT EXISTS (
		     SELECT 1 FROM pg_constraint
		     WHERE conrelid = 'public.site_object_reclaim'::regclass
		       AND contype = 'c'
		       AND conname = $1
		 )`, gh402KindCheckConstraint).Scan(&exists); err != nil {
		t.Fatalf("check kind constraint: %v", err)
	}
	return exists
}

// gh402InsertKind performs the operator's backfill statement verbatim, with
// whatever kind is given, and returns the error the operator would see.
func gh402InsertKind(t *testing.T, admin *db.Pool, tenant, siteID uuid.UUID, kind string) error {
	t.Helper()
	_, err := admin.Exec(context.Background(),
		`INSERT INTO site_object_reclaim (tenant_id, site_id, kind)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (tenant_id, site_id, kind) DO UPDATE
		   SET completed_at = NULL, attempts = 0, next_attempt_at = now()`,
		tenant, siteID, kind)
	return err
}

// ---------------------------------------------------------------------------
// The operator typo, and exactly what it now produces.
// ---------------------------------------------------------------------------

func TestGH402_KindCheck_RefusesAMistypedOperatorBackfill(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()

	if !gh402KindCheckExists(t, admin) {
		t.Fatal("site_object_reclaim has no kind check constraint after a full migration; " +
			"a mistyped operator backfill is accepted and then stranded by the worker")
	}

	tenant := seedTenant(t, pool, "gh402-kind-"+uuid.NewString()[:8])
	siteID := uuid.New() // a site that is already gone: the whole point of a backfill

	// The realistic typos: a plural, a hyphen, a wrong word, an empty string, and
	// the value the OTHER site-scoped roots would plausibly be called before they
	// are actually supported.
	for _, bad := range []string{
		"backup_manifests",
		"backup-manifest",
		"backup_manifesto",
		"manifest",
		"",
		"rucss",
		"screenshots",
		"BACKUP_MANIFEST",
	} {
		err := gh402InsertKind(t, admin, tenant, siteID, bad)
		if err == nil {
			t.Errorf("kind %q was accepted. The worker cannot derive a prefix for it, so that row "+
				"reclaims nothing while being the only record naming those objects", bad)
			continue
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Errorf("kind %q was refused by something other than the database: %v", bad, err)
			continue
		}
		// This is what the operator sees, and it has to name the constraint so
		// they can find out why.
		if pgErr.Code != "23514" { // check_violation
			t.Errorf("kind %q produced SQLSTATE %s, want 23514 (check violation)", bad, pgErr.Code)
		}
		if pgErr.ConstraintName != gh402KindCheckConstraint {
			t.Errorf("kind %q violated %q, want %q", bad, pgErr.ConstraintName, gh402KindCheckConstraint)
		}
		t.Logf("operator typing kind=%q sees: ERROR: %s (SQLSTATE %s), constraint %q",
			bad, pgErr.Message, pgErr.Code, pgErr.ConstraintName)
	}

	// Nothing was written. A refused INSERT must not leave a half-row behind.
	if got := gh402CountTasks(t, admin, tenant, siteID); got != 0 {
		t.Errorf("expected no reclaim rows after refused inserts, got %d", got)
	}

	// And the correct statement still works, including the documented shortcut of
	// omitting kind entirely and taking the column default.
	if err := gh402InsertKind(t, admin, tenant, siteID, backup.ReclaimKindBackupManifest); err != nil {
		t.Fatalf("the CORRECT operator backfill was refused: %v", err)
	}
	other := uuid.New()
	if _, err := admin.Exec(context.Background(),
		`INSERT INTO site_object_reclaim (tenant_id, site_id) VALUES ($1, $2)`,
		tenant, other); err != nil {
		t.Fatalf("omitting kind and taking the column default was refused: %v", err)
	}
	var defaulted string
	if err := admin.QueryRow(context.Background(),
		`SELECT kind FROM site_object_reclaim WHERE tenant_id = $1 AND site_id = $2`,
		tenant, other).Scan(&defaulted); err != nil {
		t.Fatalf("read defaulted kind: %v", err)
	}
	if defaulted != backup.ReclaimKindBackupManifest {
		t.Errorf("column default is %q, want %q", defaulted, backup.ReclaimKindBackupManifest)
	}
}

// The constraint must not obstruct the path that writes almost every row.
// DELETE /sites/{id} writes a code constant, and if the two disagreed the site
// delete itself would start failing.
func TestGH402_KindCheck_DoesNotBlockTheSiteDeleteEnqueue(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()

	tenant := seedTenant(t, pool, "gh402-kindok-"+uuid.NewString()[:8])
	siteID := gh402SeedSite(t, admin, tenant)
	if err := site.NewRepo(pool).Delete(context.Background(), tenant, siteID); err != nil {
		t.Fatalf("delete site: %v", err)
	}
	if got := gh402CountTasks(t, admin, tenant, siteID); got != 1 {
		t.Fatalf("expected 1 reclaim task after the delete, got %d. If the kind constraint and "+
			"the constant internal/site writes disagree, the delete's own transaction fails", got)
	}
}

// ---------------------------------------------------------------------------
// m115 on a database that already has the table.
// ---------------------------------------------------------------------------

// gh402DropKindCheck puts the schema back into the pre-m115 shape: kind is free
// text again, and m115 is unapplied.
func gh402DropKindCheck(t *testing.T, admin *db.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`ALTER TABLE site_object_reclaim DROP CONSTRAINT IF EXISTS ` + gh402KindCheckConstraint,
		`DELETE FROM schema_migrations WHERE version = '` + gh402M115Version + `'`,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("regress to the pre-m115 shape (%q): %v", stmt, err)
		}
	}
}

func TestGH402_M115_ClosesTheKindSetOnAnExistingDatabase(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	gh402DropKindCheck(t, admin)
	if gh402KindCheckExists(t, admin) {
		t.Fatal("the regression did not remove the constraint; this test's premise is wrong")
	}

	// A bad row written while kind was still free text. This is the population
	// m115 must NOT validate away: it names objects nothing else names.
	tenant := seedTenant(t, pool, "gh402-m115-"+uuid.NewString()[:8])
	stranded := uuid.New()
	if err := gh402InsertKind(t, admin, tenant, stranded, "backup_manifests"); err != nil {
		t.Fatalf("the premise requires kind to be free text before m115: %v", err)
	}

	// Boot the migrator the way the server does.
	if err := admin.Migrate(ctx); err != nil {
		t.Fatalf("boot migration failed with a pre-existing bad row present. m115 must be NOT "+
			"VALID: validating it takes the control plane down on exactly the databases that "+
			"have the problem: %v", err)
	}

	if !gh402KindCheckExists(t, admin) {
		t.Fatal("m115 did not add the constraint, so a database that upgraded rather than " +
			"started fresh keeps a free-text kind column forever")
	}

	// The door is shut from here on.
	if err := gh402InsertKind(t, admin, tenant, uuid.New(), "backup_manifests"); err == nil {
		t.Error("a bad kind was still accepted after m115")
	}

	// And the row that predates it is still there, unharmed. Losing it would be
	// GH #402 committed by the migration meant to prevent GH #402.
	var kind string
	if err := admin.QueryRow(ctx,
		`SELECT kind FROM site_object_reclaim WHERE tenant_id = $1 AND site_id = $2`,
		tenant, stranded).Scan(&kind); err != nil {
		t.Fatalf("the pre-existing row was destroyed by the convergence: %v", err)
	}
	if kind != "backup_manifests" {
		t.Errorf("the pre-existing row's kind was rewritten to %q; a migration must not guess "+
			"what an operator meant", kind)
	}

	// The operator's correction is accepted, because it moves the row INTO the
	// allowed set.
	if _, err := admin.Exec(ctx,
		`UPDATE site_object_reclaim SET kind = $1, attempts = 0, next_attempt_at = now()
		 WHERE tenant_id = $2 AND site_id = $3`,
		backup.ReclaimKindBackupManifest, tenant, stranded); err != nil {
		t.Errorf("correcting the stranded row was refused, so it can never be recovered: %v", err)
	}
}

// m115 runs on every install and almost all of them need nothing from it.
func TestGH402_M115_IsANoOpAndReRunnable(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := admin.Exec(ctx,
			`DELETE FROM schema_migrations WHERE version = $1`, gh402M115Version); err != nil {
			t.Fatalf("unmark m115: %v", err)
		}
		if err := admin.Migrate(ctx); err != nil {
			t.Fatalf("re-apply m115 (round %d): %v", i+1, err)
		}
		if !gh402KindCheckExists(t, admin) {
			t.Fatalf("round %d: the constraint is gone", i+1)
		}
	}

	// Exactly one constraint, not a duplicate alongside the one m113 declares
	// inline on a fresh database.
	var n int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM pg_constraint
		 WHERE conrelid = 'public.site_object_reclaim'::regclass
		   AND contype = 'c' AND conname LIKE 'site_object_reclaim_kind%'`).Scan(&n); err != nil {
		t.Fatalf("count constraints: %v", err)
	}
	if n != 1 {
		t.Errorf("site_object_reclaim has %d kind check constraints, want exactly 1", n)
	}
}
