package tests

// gh483_advisory_unlock_cancelled_ctx_integration_test.go — GH #483.
//
// THE DEFECT. Three workers took a SESSION-scoped advisory lock on a pinned
// pooled connection and released it from a defer that ran on the CALLER'S OWN
// context:
//
//	defer func() { _, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(...)`) }()
//
// When ctx is already cancelled by the time that defer runs — a graceful
// shutdown mid-pass, a River job timeout, a client disconnect — pgx returns
// immediately WITHOUT PUTTING THE UNLOCK ON THE WIRE. Nothing errors. The
// connection is not broken, so pgxpool takes it back as healthy and hands it to
// the next caller, still holding the lock. Every later pass runs on a DIFFERENT
// connection, gets false from pg_try_advisory_lock, concludes a peer replica is
// working, and skips. The only recovery is MaxConnLifetime (db.go, 30 min)
// eventually closing the connection.
//
// This file proves it against real Postgres for org.PurgeWorker, whose key is
// the shared org_lifecycle one — so the leak does not merely stall the next
// purge tick, it blocks DELETE /orgs/{orgId} and POST /orgs/{orgId}/restore for
// the tenant, both of which block on the same key.
//
// WHY THE PURGE WORKER IS THE SITE THAT GETS THE TEST. All three sites are the
// same three lines with the same fix, but only this one has a seam where the
// context can be cancelled while the pinned connection is IDLE and CLEAN, which
// is what the production failure looks like. purgeOne's object-storage step runs
// through the injectable ObjectPurger, outside any transaction on the pinned
// conn, so cancelling there leaves that conn exactly as a mid-pass SIGTERM would.
// That distinction is load-bearing rather than cosmetic: if the cancellation
// instead landed inside a transaction on the pinned conn, pgxpool.Conn.Release
// would see TxStatus != 'I' and DESTROY the connection, which closes the session
// and drops the lock — and the test would pass against the unfixed code for a
// reason that has nothing to do with the fix. The two backup sites
// (internal/backup/worker.go's scheduler lock, internal/backup/repo.go's
// per-tenant GC lock) have no such seam without reshaping production code to
// suit a test, and are covered by inspection; see the PR body.
//
// NOT RUN BY CI (apps/api/tests is excluded from the fast lane by owner
// decision). Run with `make test-integration` from the repository root, or
// directly with the command in each test's doc comment.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/org"
)

// gh483CancellingStore is the seam. Its List cancels the context the purge is
// running under and then fails, which is what a SIGTERM landing between the
// "mark purge started" write and the first object delete does to a real sweep:
// the pass unwinds, and purgeOne's deferred pg_advisory_unlock is reached with a
// dead context. Failing before any Delete also keeps the test repeatable and
// destroys nothing — the tenant row survives, because purgeOne's hard delete is
// the step after this one.
type gh483CancellingStore struct {
	cancel   context.CancelFunc
	listCall int
}

func (s *gh483CancellingStore) List(_ context.Context, _ string) ([]string, error) {
	s.listCall++
	s.cancel()
	return nil, errors.New("gh483: object storage unreachable (test injects the cancellation here)")
}

func (s *gh483CancellingStore) Delete(_ context.Context, _ string) error {
	return errors.New("gh483: Delete must not be reached; List fails first")
}

// gh483OrgLockHeld reports whether the two-int org_lifecycle advisory lock for
// tenant is held by ANY session, read from pg_locks THROUGH THE APPLICATION POOL
// as the application role — the same view the next purge tick, the delete
// endpoint and the restore endpoint get.
//
// Two-int advisory locks appear in pg_locks as classid = key1, objid = key2,
// objsubid = 2. hashtext returns int4 and is routinely negative; int4 -> oid is
// a binary coercion, so the casts below reinterpret the bits exactly as the lock
// manager stored them rather than erroring on the sign.
//
// This reads pg_locks rather than probing with pg_try_advisory_lock because the
// probe would be unsound here: the pool can hand back the very connection that
// leaked the lock, and a second acquisition on the SAME session succeeds
// (advisory locks are re-entrant per session), so a probe can report "free"
// while the lock is held.
func gh483OrgLockHeld(t *testing.T, pool *db.Pool, tenant uuid.UUID) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_locks
		  WHERE locktype = 'advisory'
		    AND classid  = hashtext($1)::oid
		    AND objid    = hashtext($2)::oid
		    AND objsubid = 2`,
		org.LifecycleLockKey, tenant.String()).Scan(&n); err != nil {
		t.Fatalf("read pg_locks for the org_lifecycle advisory lock: %v", err)
	}
	return n > 0
}

// TestGH483_PurgeCancelledMidPassDoesNotLeakTheOrgLifecycleLock is the
// regression. It runs the real org.PurgeWorker over a real tenant, cancels the
// sweep's context mid-pass at a real seam, and then requires the tenant's
// org_lifecycle advisory lock to be gone.
//
// Against the unfixed code (conn.Exec(ctx, ...)) the lock is still held when
// Work returns and this fails. Against the fix (db.CleanupContext) it is
// released.
//
//	go test ./tests/ -run TestGH483_PurgeCancelledMidPassDoesNotLeakTheOrgLifecycleLock -v -count=1
func TestGH483_PurgeCancelledMidPassDoesNotLeakTheOrgLifecycleLock(t *testing.T) {
	pool := startPostgres(t) // wpmgr_app: NOSUPERUSER, NOBYPASSRLS
	admin := connectAdmin(t, pool)
	defer admin.Close()

	tenant := seedTenant(t, pool, "gh483-cancel-"+uuid.NewString()[:8])
	odBackdateDeletedAt(t, admin, tenant, 30*24*time.Hour)

	if gh483OrgLockHeld(t, pool, tenant) {
		t.Fatalf("the org_lifecycle lock for %s was already held before the sweep ran; "+
			"this test cannot attribute anything it observes afterwards", tenant)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &gh483CancellingStore{cancel: cancel}

	// grace=time.Hour matches the existing purge integration test; the tenant is
	// back-dated 30 days so it is unambiguously due. sites/revoker nil: the
	// revoke step is best-effort and not what is under test.
	worker := org.NewPurgeWorker(pool, nil, nil, store, time.Hour, nil)

	// Work never returns the per-tenant error (a failed tenant is logged and the
	// sweep continues), so the assertion cannot be on its return value. It is on
	// the database.
	if err := worker.Work(ctx, nil); err != nil {
		t.Fatalf("purge sweep returned an error at the sweep level: %v", err)
	}
	if store.listCall == 0 {
		t.Fatal("the purge never reached the object-storage step, so the context was never " +
			"cancelled mid-pass and this test proved nothing. Check that the tenant is due " +
			"(deleted_at back-dated past the grace window) and that it still exists")
	}
	if ctx.Err() == nil {
		t.Fatal("the context was not cancelled, so the deferred unlock ran on a live context " +
			"and this test proved nothing")
	}

	if gh483OrgLockHeld(t, pool, tenant) {
		t.Fatalf("GH #483: the org_lifecycle advisory lock for tenant %s is STILL HELD after a "+
			"purge pass that was cancelled mid-flight. The connection went back to the pool "+
			"healthy and still locked, so every later purge tick reads false from "+
			"pg_try_advisory_lock and skips, and DELETE /orgs/{orgId} and POST "+
			"/orgs/{orgId}/restore — which take this same key — block until "+
			"MaxConnLifetime (30m) recycles the connection", tenant)
	}
}

// TestGH483_PurgeOnALiveContextStillReleasesTheLock is the over-fire guard. The
// fix must not be a fix that only works when something went wrong: the ordinary
// completed sweep, on a context nobody cancelled, must still leave the lock
// released, and the purge must still do its job.
//
// It passes both before and after the fix — that is the point of it. It fails if
// a future "fix" releases the lock only on the cancelled path, or drops the
// unlock altogether.
//
//	go test ./tests/ -run TestGH483_PurgeOnALiveContextStillReleasesTheLock -v -count=1
func TestGH483_PurgeOnALiveContextStillReleasesTheLock(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()

	tenant := seedTenant(t, pool, "gh483-live-"+uuid.NewString()[:8])
	odBackdateDeletedAt(t, admin, tenant, 30*24*time.Hour)

	store := &gh483EmptyStore{}
	worker := org.NewPurgeWorker(pool, nil, nil, store, time.Hour, nil)

	ctx := context.Background()
	if err := worker.Work(ctx, nil); err != nil {
		t.Fatalf("purge sweep: %v", err)
	}
	if store.listCall == 0 {
		t.Fatal("the purge never reached the object-storage step, so this ran over a tenant " +
			"that was not due and proved nothing")
	}

	// The purge completed, so the tenant row is gone.
	var alive int
	if err := admin.QueryRow(context.Background(),
		`SELECT count(*) FROM tenants WHERE id = $1`, tenant).Scan(&alive); err != nil {
		t.Fatalf("count the purged tenant: %v", err)
	}
	if alive != 0 {
		t.Fatalf("the uncancelled sweep left tenant %s in place (%d rows); the lock assertion "+
			"below would be meaningless because the purge did not run", tenant, alive)
	}

	if gh483OrgLockHeld(t, pool, tenant) {
		t.Fatalf("a NORMAL, uncancelled purge pass left the org_lifecycle advisory lock for "+
			"tenant %s held. The unlock is broken on the ordinary path, not just the "+
			"cancelled one", tenant)
	}
}

// gh483EmptyStore is a storage backend with nothing in it: every prefix lists
// empty, so the purge walks all seven roots and completes.
type gh483EmptyStore struct{ listCall int }

func (s *gh483EmptyStore) List(_ context.Context, _ string) ([]string, error) {
	s.listCall++
	return nil, nil
}

func (s *gh483EmptyStore) Delete(_ context.Context, _ string) error { return nil }
