// gh414_m117_monitoring_pause_test.go — m117 schema proof (GH #414 Phase 1).
//
// m117 adds four columns, one CHECK constraint and one partial index to sites
// and DELIBERATELY CHANGES NO BEHAVIOUR: no scheduler reads the flag yet. What
// there is to prove is therefore entirely about the schema, and every assertion
// below is reached the way production reaches it — through site.Repo and
// db.Pool.InTenantTx, on the pool startPostgres hands back, which is connected
// as the NON-superuser wpmgr_app role. A test that opened its own connection,
// or connected as the container's bootstrap superuser, would leave every policy
// on sites inert and pass vacuously; startPostgres exists precisely so this one
// does not.
//
// Note the container in startPostgres runs adminPool.Migrate(ctx), which is the
// same internal/db path main() runs at boot. So every test here also asserts
// that m117 applies cleanly, in ordinal order, against a fresh database.
package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// pauseColumns is the row shape every subtest below reads back. pgtype is
// avoided in favour of plain pointers so a NULL is unambiguous in a failure
// message.
type pauseColumns struct {
	PausedAt *string
	PausedBy *uuid.UUID
	Reason   string
	ResumeAt *string
}

// readPauseColumns reads the four m117 columns for one site through
// InTenantTx — the real dispatch, which sets app.tenant_id and therefore
// activates sites_tenant_isolation. Returns false if RLS hid the row.
func readPauseColumns(t *testing.T, pool *db.Pool, tenantID, siteID uuid.UUID) (pauseColumns, bool) {
	t.Helper()
	var got pauseColumns
	found := false
	err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `
			SELECT monitoring_paused_at::text,
			       monitoring_paused_by,
			       monitoring_paused_reason,
			       monitoring_resume_at::text
			FROM sites WHERE id = $1`, siteID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			found = true
			if err := rows.Scan(&got.PausedAt, &got.PausedBy, &got.Reason, &got.ResumeAt); err != nil {
				return err
			}
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read pause columns: %v", err)
	}
	return got, found
}

// TestGH414_M117_ColumnsExistAndDefaultToActive is the "does the migration do
// what it says" proof: a site created through the ordinary repository path
// lands unpaused, with the reason defaulting to the empty string rather than
// NULL, and no scheduler has to special-case a pre-m117 row.
func TestGH414_M117_ColumnsExistAndDefaultToActive(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenantID := seedTenant(t, pool, "m117-defaults")
	repo := site.NewRepo(pool)

	created, err := repo.Create(ctx, site.CreateInput{
		TenantID: tenantID,
		URL:      "https://m117-defaults.example.com",
		Name:     "m117 defaults",
	})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}

	got, found := readPauseColumns(t, pool, tenantID, created.ID)
	if !found {
		t.Fatal("the site we just created was not readable in its own tenant")
	}

	t.Logf("monitoring_paused_at=%v monitoring_paused_by=%v monitoring_paused_reason=%q monitoring_resume_at=%v",
		got.PausedAt, got.PausedBy, got.Reason, got.ResumeAt)

	if got.PausedAt != nil {
		t.Errorf("monitoring_paused_at = %v, want NULL (a new site is NOT paused)", *got.PausedAt)
	}
	if got.PausedBy != nil {
		t.Errorf("monitoring_paused_by = %v, want NULL", *got.PausedBy)
	}
	if got.Reason != "" {
		t.Errorf("monitoring_paused_reason = %q, want \"\" (NOT NULL DEFAULT '')", got.Reason)
	}
	if got.ResumeAt != nil {
		t.Errorf("monitoring_resume_at = %v, want NULL", *got.ResumeAt)
	}
}

// TestGH414_M117_ResumeRequiresPauseConstraint proves the CHECK both FIRES on
// the incoherent row and does NOT over-fire on the three coherent ones. A
// constraint nobody has seen reject anything is not known to constrain
// anything; a constraint that rejects correct work gets dropped, and then it
// constrains nothing either.
func TestGH414_M117_ResumeRequiresPauseConstraint(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenantID := seedTenant(t, pool, "m117-check")
	repo := site.NewRepo(pool)
	created, err := repo.Create(ctx, site.CreateInput{
		TenantID: tenantID,
		URL:      "https://m117-check.example.com",
		Name:     "m117 check",
	})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}

	// FIRES: a resume instant with no pause is the phantom-transition row the
	// phase-2 auto-resume sweep would pick up and act on.
	err = pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			"UPDATE sites SET monitoring_resume_at = now() + interval '1 hour' WHERE id = $1",
			created.ID)
		return err
	})
	if err == nil {
		t.Fatal("a resume time with no pause was accepted; the CHECK constraint is not enforcing")
	}
	var pgErr *pgconn.PgError
	if !asPgError(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("want SQLSTATE 23514 (check_violation), got %v", err)
	}
	if pgErr.ConstraintName != "sites_monitoring_resume_requires_pause_check" {
		t.Errorf("violated constraint = %q, want sites_monitoring_resume_requires_pause_check",
			pgErr.ConstraintName)
	}
	t.Logf("CHECK fired as intended: %s %s", pgErr.Code, pgErr.ConstraintName)

	// DOES NOT OVER-FIRE, case 1: pause with no auto-resume — the common case.
	mustExec(t, pool, tenantID, `
		UPDATE sites SET monitoring_paused_at = now(),
		                 monitoring_paused_reason = 'pre-migration maintenance'
		WHERE id = $1`, created.ID)

	// case 2: adding an auto-resume to an already-paused site.
	mustExec(t, pool, tenantID, `
		UPDATE sites SET monitoring_resume_at = now() + interval '2 hours'
		WHERE id = $1`, created.ID)

	// case 3: a resume instant in the PAST on a paused site. Deliberately legal
	// — it means "due now", which is how a "resume_at <= now()" sweep reads it,
	// and it is what an operator ending a pause on the next tick produces.
	mustExec(t, pool, tenantID, `
		UPDATE sites SET monitoring_resume_at = now() - interval '5 minutes'
		WHERE id = $1`, created.ID)

	// case 4: the resume path clearing both columns in one statement.
	mustExec(t, pool, tenantID, `
		UPDATE sites SET monitoring_paused_at = NULL,
		                 monitoring_resume_at = NULL,
		                 monitoring_paused_reason = ''
		WHERE id = $1`, created.ID)

	got, found := readPauseColumns(t, pool, tenantID, created.ID)
	if !found {
		t.Fatal("site vanished")
	}
	if got.PausedAt != nil || got.ResumeAt != nil || got.Reason != "" {
		t.Fatalf("resume did not return the row to active: %+v", got)
	}
}

// TestGH414_M117_PausedByFKSetsNullOnUserDelete proves the attribution decision
// in the migration header: sites outlive users, so deleting the person who
// paused a site must keep the PAUSE (the operational fact) and drop only the
// NAME (the display fact). A CASCADE here would delete the site; a RESTRICT
// would make an old pause block account deletion with 23503.
func TestGH414_M117_PausedByFKSetsNullOnUserDelete(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenantID := seedTenant(t, pool, "m117-fk")
	repo := site.NewRepo(pool)
	created, err := repo.Create(ctx, site.CreateInput{
		TenantID: tenantID,
		URL:      "https://m117-fk.example.com",
		Name:     "m117 fk",
	})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}

	// users is not RLS-scoped (a user spans tenants), so this is a plain insert
	// on the same app-role pool.
	var userID uuid.UUID
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, name) VALUES ($1, $2) RETURNING id",
		"m117-fk@example.com", "Pauser").Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	mustExec(t, pool, tenantID, `
		UPDATE sites SET monitoring_paused_at = now(),
		                 monitoring_paused_by = $2,
		                 monitoring_paused_reason = 'noisy staging box'
		WHERE id = $1`, created.ID, userID)

	before, _ := readPauseColumns(t, pool, tenantID, created.ID)
	if before.PausedBy == nil || *before.PausedBy != userID {
		t.Fatalf("monitoring_paused_by = %v, want %v", before.PausedBy, userID)
	}

	if _, err := pool.Exec(ctx, "DELETE FROM users WHERE id = $1", userID); err != nil {
		t.Fatalf("delete user (a RESTRICT/NO ACTION fkey would fail here with 23503): %v", err)
	}

	after, found := readPauseColumns(t, pool, tenantID, created.ID)
	if !found {
		t.Fatal("the SITE was deleted with the user; the fkey is ON DELETE CASCADE, which is catastrophic")
	}
	if after.PausedAt == nil {
		t.Error("the pause was lost when the pauser was deleted; pause must survive attribution")
	}
	if after.Reason != "noisy staging box" {
		t.Errorf("monitoring_paused_reason = %q, want it preserved", after.Reason)
	}
	if after.PausedBy != nil {
		t.Errorf("monitoring_paused_by = %v, want NULL after the user was deleted", *after.PausedBy)
	}
	t.Logf("after user delete: paused_at=%v paused_by=%v reason=%q",
		after.PausedAt, after.PausedBy, after.Reason)
}

// TestGH414_M117_PauseColumnsAreCoveredByExistingRLS proves the migration's RLS
// claim rather than assuming it: policies are ROW level, so the four new
// columns are hidden from another tenant by exactly the same
// sites_tenant_isolation policy that hides the rest of the row. Nothing about
// a paused site — that it is paused, who paused it, or the free-text reason —
// crosses the tenant boundary.
func TestGH414_M117_PauseColumnsAreCoveredByExistingRLS(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenantA := seedTenant(t, pool, "m117-rls-a")
	tenantB := seedTenant(t, pool, "m117-rls-b")
	repo := site.NewRepo(pool)

	siteA, err := repo.Create(ctx, site.CreateInput{
		TenantID: tenantA,
		URL:      "https://m117-rls-a.example.com",
		Name:     "A",
	})
	if err != nil {
		t.Fatalf("create site A: %v", err)
	}
	mustExec(t, pool, tenantA, `
		UPDATE sites SET monitoring_paused_at = now(),
		                 monitoring_paused_reason = 'tenant A private note'
		WHERE id = $1`, siteA.ID)

	// The owning tenant reads its own pause state.
	own, found := readPauseColumns(t, pool, tenantA, siteA.ID)
	if !found || own.PausedAt == nil || own.Reason != "tenant A private note" {
		t.Fatalf("tenant A cannot read its own pause state: found=%v %+v", found, own)
	}

	// Tenant B, going through the identical dispatch, sees nothing at all — not
	// a row with nulled columns, no row.
	_, foundByB := readPauseColumns(t, pool, tenantB, siteA.ID)
	if foundByB {
		t.Fatal("tenant B read tenant A's pause columns; RLS does not cover them")
	}

	// And the reason text is not reachable by a filter either, which is the
	// shape that would leak it a character at a time.
	if err := pool.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx,
			"SELECT count(*) FROM sites WHERE monitoring_paused_reason LIKE 'tenant A%'").Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Fatalf("tenant B matched %d of tenant A's pause reasons", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("cross-tenant probe: %v", err)
	}

	// FORCE ROW LEVEL SECURITY and the RESTRICTIVE sites_site_scope policy are
	// what make the above true for the table owner and for a site-scoped
	// collaborator respectively. Assert they are still on the table rather than
	// trusting the migration comment.
	var relrowsecurity, relforcerowsecurity bool
	if err := pool.QueryRow(ctx,
		"SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = 'public.sites'::regclass").
		Scan(&relrowsecurity, &relforcerowsecurity); err != nil {
		t.Fatalf("read pg_class: %v", err)
	}
	if !relrowsecurity || !relforcerowsecurity {
		t.Fatalf("sites: ENABLE=%v FORCE=%v, want both true", relrowsecurity, relforcerowsecurity)
	}
	var restrictive string
	if err := pool.QueryRow(ctx,
		"SELECT permissive FROM pg_policies WHERE schemaname='public' AND tablename='sites' AND policyname='sites_site_scope'").
		Scan(&restrictive); err != nil {
		t.Fatalf("sites_site_scope policy not found on sites: %v", err)
	}
	if restrictive != "RESTRICTIVE" {
		t.Fatalf("sites_site_scope is %s, want RESTRICTIVE", restrictive)
	}
	t.Logf("sites: ENABLE=%v FORCE=%v sites_site_scope=%s", relrowsecurity, relforcerowsecurity, restrictive)
}

// TestGH414_M117_AutoResumeIndexExists asserts the index the migration argues
// for is the index that shipped — the near-empty one on the auto-resume sweep's
// predicate, and NOT an index on "monitoring_paused_at IS NULL", which would
// match nearly every row and be paid for on every write to this table.
func TestGH414_M117_AutoResumeIndexExists(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	var def string
	if err := pool.QueryRow(ctx,
		"SELECT indexdef FROM pg_indexes WHERE schemaname='public' AND tablename='sites' AND indexname='sites_monitoring_resume_due_idx'").
		Scan(&def); err != nil {
		t.Fatalf("sites_monitoring_resume_due_idx missing: %v", err)
	}
	t.Logf("indexdef: %s", def)

	var wrongIdx int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_indexes
		WHERE schemaname='public' AND tablename='sites'
		  AND indexdef LIKE '%monitoring_paused_at IS NULL%'`).Scan(&wrongIdx); err != nil {
		t.Fatalf("scan pg_indexes: %v", err)
	}
	if wrongIdx != 0 {
		t.Errorf("found %d index(es) on the monitoring_paused_at IS NULL predicate; "+
			"m117 argues against that index — see DECISION 2 in the migration header", wrongIdx)
	}
}

// mustExec runs one statement through InTenantTx (the real dispatch) and fails
// the test if it errors or matches no row.
func mustExec(t *testing.T, pool *db.Pool, tenantID uuid.UUID, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	err := pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, sql, args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			t.Fatalf("statement matched no rows: %s", sql)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
}

// asPgError is errors.As specialised to *pgconn.PgError, kept local so this
// file adds no shared helper to the package.
func asPgError(err error, target **pgconn.PgError) bool {
	for err != nil {
		if pe, ok := err.(*pgconn.PgError); ok {
			*target = pe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
