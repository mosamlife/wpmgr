package tests

// migration_late_run_lock_test.go — shared proof helpers for the three m141
// (update_tasks), m143 (backup_snapshots) and m145 (site_vulnerabilities)
// prefill migrations' "late run" regression tests
// (update_m88_dedup_test.go, backup_m96_migration_test.go,
// vuln_alerting_m103_test.go). Each prefill's own doc comment claims that once
// its target index/column already exists, a late run "takes no lock and
// changes no data". Before this file, none of the three late-run tests could
// actually fail on the "no lock" half: nothing read pg_locks, nothing held a
// conflicting lock from a second connection, so a check-less version of any of
// the three files (see the fires half below) still passed them.
//
// THE PROOF, PER TEST:
//  1. hold ROW EXCLUSIVE on the target table from a second connection, in an
//     open (uncommitted) transaction — holdRowExclusiveOpen below.
//  2. fires: run an IN-MEMORY copy of the migration's SQL with its
//     early-return/probe check stripped (never the committed file — see
//     CLAUDE.md's routing table; editing an applied migration is
//     database-engineer's territory) against the same held lock, and require
//     it to fail with a specific, expected SQLSTATE —
//     mutatedMigrationMustBlockOrError.
//  3. does-not-over-fire: run the REAL migration late, through owner.Migrate,
//     and require it to finish well inside the bound a lock wait would blow —
//     assertMigrateStaysUnderLockBound.
//  4. re-apply the REAL file's own SQL text inside an explicit transaction and
//     assert pg_locks shows no entry for the target relation on that backend
//     before commit — assertFileTakesNoLockOnRelation.
//
// A test that only ever ran the real, unmutated file was never seen to fail,
// so step 2 is not optional: it is what makes steps 3 and 4 mean anything.
import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// sqlStateLockNotAvailable is Postgres's SQLSTATE for a lock_timeout abort
// (55P03) — the expected proof for m141 and m143, which set their own 5s
// lock_timeout inside the guard-stripped body before touching the table.
const sqlStateLockNotAvailable = "55P03"

// sqlStateDuplicateColumn is Postgres's SQLSTATE for "column already exists"
// (42701) — the expected proof for m145. m145 sets no lock_timeout of its
// own, and its guard-stripped body is a bare
// `ALTER TABLE ... ADD COLUMN notified_at` with no IF NOT EXISTS: once the
// target column already exists (as it does by the time this fires check
// runs — m103 added it first), that ADD COLUMN fails on the duplicate
// column, not on the held lock.
const sqlStateDuplicateColumn = "42701"

// lateRunLockBound is the wall-clock ceiling for the REAL (unmutated) late
// run of a converged prefill while a concurrent ROW EXCLUSIVE holder is open
// on its target table. A converged late run's probe never references the
// table at all, so this is generous headroom over an in-process no-op, and
// still comfortably under the 5s lock_timeout m141/m143 set for themselves.
const lateRunLockBound = 2 * time.Second

// mutatedMigrationBound is the wall-clock ceiling given to a MUTATED
// (guard-stripped) migration body before this harness's own context cancels
// it. 6s comfortably exceeds m141/m143's internal 5s lock_timeout, so their
// own 55P03 fires first when their fires check is run against a held
// ROW EXCLUSIVE holder, as designed. m145's fires check needs no such
// ceiling to turn a hang into a red result — its guard-stripped body fails
// fast on a duplicate column and is deliberately run before any holder is
// taken — but this bound still applies to it as the general ceiling on
// every call through mutatedMigrationMustBlockOrError.
const mutatedMigrationBound = 6 * time.Second

// holdRowExclusiveOpen opens a second connection on pool (the SAME role
// owner.Migrate runs as — "as the app role or owner") and leaves an
// uncommitted UPDATE ... WHERE false open on table. WHERE false is
// deliberate: it matches zero rows, so it needs no app.* GUC and cannot trip
// a WITH CHECK policy under FORCE ROW LEVEL SECURITY the way a real INSERT or
// a WHERE-bearing UPDATE would (owner carries neither BYPASSRLS nor any
// tenant scope) — while still taking Postgres's ROW EXCLUSIVE TABLE lock for
// an UPDATE, which is acquired before row filtering and held for the life of
// the transaction regardless of how many rows end up matching.
//
// Returns a release func that rolls back (never commits — no row is ever
// really written) and must be called before the test ends.
func holdRowExclusiveOpen(t *testing.T, pool *db.Pool, table, anyColumn string) (release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open second connection to hold %s: %v", table, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE `+table+` SET `+anyColumn+` = `+anyColumn+` WHERE false`); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("take ROW EXCLUSIVE on %s (UPDATE ... WHERE false): %v", table, err)
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("release the %s ROW EXCLUSIVE holder: %v", table, err)
		}
	}
}

// assertMigrateStaysUnderLockBound calls owner.Migrate and fails the test if
// it takes longer than lateRunLockBound. Meant to be called while a
// holdRowExclusiveOpen on the migration's target table is still open — a
// converged late run must never wait on it.
func assertMigrateStaysUnderLockBound(t *testing.T, owner *db.Pool, ctx context.Context) {
	t.Helper()
	start := time.Now()
	if err := owner.Migrate(ctx); err != nil {
		t.Fatalf("late migration run with a concurrent ROW EXCLUSIVE holder open: %v", err)
	}
	if elapsed := time.Since(start); elapsed > lateRunLockBound {
		t.Fatalf("late migration run took %s (> %s) with a concurrent ROW EXCLUSIVE holder open; "+
			"a converged late run must take no lock on the target table and therefore never wait on one",
			elapsed, lateRunLockBound)
	}
}

// assertFileTakesNoLockOnRelation applies body (a migration's own, UNMUTATED
// SQL text) inside an explicit transaction on a fresh connection, then — still
// inside that same transaction, before committing — asserts pg_locks holds no
// entry for relation for this backend. Meant to be called while a
// holdRowExclusiveOpen on the same table is still open: if body's probe ever
// referenced the table at all, this call would itself block or time out, or
// would leave a lock behind, so this simultaneously re-proves the
// no-blocking claim from the opposite direction.
func assertFileTakesNoLockOnRelation(t *testing.T, pool *db.Pool, relation, body string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explicit tx to apply the migration file: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := tx.Exec(ctx, body); err != nil {
		t.Fatalf("apply the migration file inside an explicit tx: %v", err)
	}
	var n int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM pg_locks WHERE pid = pg_backend_pid() AND relation = $1::regclass`,
		relation,
	).Scan(&n); err != nil {
		t.Fatalf("read pg_locks for %s: %v", relation, err)
	}
	if n != 0 {
		t.Fatalf("applying the migration file holds %d lock(s) on %s before commit, want 0", n, relation)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	committed = true
}

// mutatedMigrationMustBlockOrError runs mutatedBody (an IN-MEMORY copy of a
// migration's SQL with its early-return/probe check stripped — never the
// committed file) as a single implicit-transaction statement, bounded by
// mutatedMigrationBound, and fails the test unless it fails with EXACTLY
// wantSQLState. For m141 and m143, meant to be called while a
// holdRowExclusiveOpen on the migration's target table is open — their
// expected SQLSTATE is a proof of blocking against that hold. m145's fires
// proof is state-based, not lock-based (see below), so its caller runs this
// deliberately BEFORE taking any hold.
//
// Accepting any non-nil error here is not a proof: for m145, once the target
// column already exists, the guard-stripped body is a bare
// `ADD COLUMN notified_at` with no IF NOT EXISTS, which fails with
// sqlStateDuplicateColumn (42701) whether or not any lock is held at all —
// that check would pass just as well with holdRowExclusiveOpen's real
// ROW EXCLUSIVE swapped for a non-locking no-op, which proves nothing about
// the guard being load-bearing against contention.
//
// m141 and m143 set their own 5s lock_timeout inside the guard-stripped body
// and are expected to fail with sqlStateLockNotAvailable (55P03); if this
// call's own mutatedMigrationBound context deadline fires first instead
// (this harness's cancellation racing the migration's own lock_timeout), a
// wrapped context.DeadlineExceeded is accepted in its place as the same
// proof of blocking. m145 sets no lock_timeout of its own, so
// sqlStateDuplicateColumn is the only accepted proof for it — the honest one:
// without its guard, a late run aborts the boot on a column that already
// exists.
//
// This is the fires half of the proof; the callers' own
// assertMigrateStaysUnderLockBound / assertFileTakesNoLockOnRelation calls
// against the REAL file are the does-not-over-fire half.
func mutatedMigrationMustBlockOrError(t *testing.T, pool *db.Pool, mutatedBody, wantSQLState string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mutatedMigrationBound)
	defer cancel()
	start := time.Now()
	_, err := pool.Exec(ctx, mutatedBody)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("mutated migration (early-return check stripped) SUCCEEDED in %s despite a concurrent "+
			"ROW EXCLUSIVE holder on its target table; the guard is not proven load-bearing", elapsed)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == wantSQLState {
		t.Logf("mutated migration correctly failed after %s with SQLSTATE %s, the expected proof (guard-is-load-bearing): %v", elapsed, pgErr.Code, err)
		return
	}
	if wantSQLState == sqlStateLockNotAvailable && errors.Is(err, context.DeadlineExceeded) {
		t.Logf("mutated migration correctly failed after %s via this harness's own context deadline, standing in for the %s lock_timeout it raced against (guard-is-load-bearing): %v", elapsed, wantSQLState, err)
		return
	}
	t.Fatalf("mutated migration failed after %s, but not with the expected proof (want SQLSTATE %s): %v", elapsed, wantSQLState, err)
}

// stripOnce removes exactly one occurrence of needle from body and fails the
// test if needle was not found — the positive control for the mutation: if a
// later edit to the real migration's wording makes this silently match zero
// times, the fires proof would otherwise mutate nothing and pass for the
// wrong reason.
func stripOnce(t *testing.T, body, needle string) string {
	t.Helper()
	if !strings.Contains(body, needle) {
		t.Fatalf("expected substring not found in migration body; the guard text likely changed — update this test's copy:\n%s", needle)
	}
	mutated := strings.Replace(body, needle, "", 1)
	if mutated == body {
		t.Fatalf("stripping the guard text made no change to the migration body")
	}
	return mutated
}
