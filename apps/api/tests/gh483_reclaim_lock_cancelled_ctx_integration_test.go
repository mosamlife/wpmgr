package tests

// gh483_reclaim_lock_cancelled_ctx_integration_test.go — GH #483, the second
// converted site.
//
// 71fed5ef's commit message said backup/tenant_reclaim_worker.go's
// org_lifecycle lock (like update/dispatch_sweeper.go's) was "covered by
// inspection only" because no seam existed to cancel the caller's context
// between the lock and its release without reshaping production code. A
// security review on PR #495 established that claim was wrong for this site: no
// reshaping is needed, because LockTenantLifecycle's returned closure captures
// its ctx PARAMETER by reference, not by value. A caller can cancel that same
// context object after LockTenantLifecycle returns (lock already taken) and
// before invoking the returned release func — at which point the closure's
// deferred db.CleanupContext(ctx) sees an already-cancelled ctx, exactly the
// shape GH #483 is about, with nothing in tenant_reclaim_worker.go touched.
//
// See gh483_advisory_unlock_cancelled_ctx_integration_test.go for the full
// defect writeup and gh483OrgLockHeld, reused here unmodified.
//
// NOT RUN BY CI (apps/api/tests is excluded from the fast lane by owner
// decision). Run with `make test-integration` from the repository root, or
// directly with the command in each test's doc comment.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
)

// TestGH483_ReclaimLockCancelledBeforeReleaseDoesNotLeakTheOrgLifecycleLock is
// the regression for backup.pgTenantReclaimStore.LockTenantLifecycle.
//
// It takes the lock on a cancellable context, cancels that SAME context object
// after the lock is confirmed acquired, and only then calls the returned
// release func — so release's deferred unlock runs on a context that is
// already Err() != nil by the time db.CleanupContext reads it, matching a
// drain cancelled mid-flight (rolling deploy, River job timeout) between
// acquiring the lock and reaching its own release path.
//
// Against the unfixed code (conn.Exec(ctx, ...) in the release closure) the
// unlock never reaches the wire on a dead ctx, the pooled connection goes back
// healthy and still locked, and this fails. Against the fix
// (db.CleanupContext) it is released.
//
//	go test ./tests/ -run TestGH483_ReclaimLockCancelledBeforeReleaseDoesNotLeakTheOrgLifecycleLock -v -count=1
func TestGH483_ReclaimLockCancelledBeforeReleaseDoesNotLeakTheOrgLifecycleLock(t *testing.T) {
	pool := startPostgres(t) // wpmgr_app: NOSUPERUSER, NOBYPASSRLS
	admin := connectAdmin(t, pool)
	defer admin.Close()

	tenant := uuid.New()
	if gh483OrgLockHeld(t, pool, tenant) {
		t.Fatalf("the org_lifecycle lock for %s was already held before the test ran; this test "+
			"cannot attribute anything it observes afterwards", tenant)
	}

	store := backup.NewTenantReclaimStore(pool)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release, acquired, err := store.LockTenantLifecycle(ctx, tenant)
	if err != nil {
		t.Fatalf("take the lock: %v", err)
	}
	if !acquired {
		t.Fatal("an uncontended lock was not acquired")
	}

	// Cancel the SAME ctx object LockTenantLifecycle was called with, BEFORE
	// calling release. release's closure captured this ctx variable by
	// reference (it is a func literal over the enclosing parameter, not a
	// value copied at call time), so the deferred db.CleanupContext(ctx) inside
	// it observes cancellation that happened after the lock was taken and
	// before release ran — not cancellation racing the lock acquisition
	// itself.
	cancel()
	if ctx.Err() == nil {
		t.Fatal("cancel() did not mark ctx done; the release below would run on a live context and " +
			"this test would prove nothing")
	}

	release()

	if gh483OrgLockHeld(t, pool, tenant) {
		t.Fatalf("GH #483: the org_lifecycle advisory lock for tenant %s is STILL HELD after "+
			"release() ran on an already-cancelled context. The connection went back to the "+
			"pool healthy and still locked, so DELETE /orgs/{orgId}, POST "+
			"/orgs/{orgId}/restore and the Lane B purge worker for this tenant all block on "+
			"it until db.MaxConnLifetime (30m) recycles the connection", tenant)
	}
}

// TestGH483_ReclaimLockOnALiveContextStillReleases is the over-fire guard for
// the same seam: cancelling ctx must not become the ONLY path that releases
// the lock. A normal caller that never cancels anything — the drain's actual
// steady state — must still get the lock back.
//
// This is a second, direct proof of the same fact
// TestGH408_TheDrainTakesTheSameLockTheOrgLifecycleUses already establishes via
// its post-release probe; it is kept here, alongside the cancelled-path test
// above, so the pair sits together and so deleting the unlock line (the
// over-fire check on THIS pair, see the PR body / worklog) has an
// uncancelled-path failure to point at without depending on an unrelated file.
//
//	go test ./tests/ -run TestGH483_ReclaimLockOnALiveContextStillReleases -v -count=1
func TestGH483_ReclaimLockOnALiveContextStillReleases(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()

	tenant := uuid.New()
	if gh483OrgLockHeld(t, pool, tenant) {
		t.Fatalf("the org_lifecycle lock for %s was already held before the test ran", tenant)
	}

	store := backup.NewTenantReclaimStore(pool)
	ctx := context.Background()

	release, acquired, err := store.LockTenantLifecycle(ctx, tenant)
	if err != nil {
		t.Fatalf("take the lock: %v", err)
	}
	if !acquired {
		t.Fatal("an uncontended lock was not acquired")
	}
	release()

	if gh483OrgLockHeld(t, pool, tenant) {
		t.Fatalf("a NORMAL release on a live, uncancelled context left the org_lifecycle "+
			"advisory lock for tenant %s held. The unlock is broken on the ordinary path, "+
			"not just the cancelled one", tenant)
	}
}
