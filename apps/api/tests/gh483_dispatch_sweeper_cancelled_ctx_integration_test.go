package tests

// gh483_dispatch_sweeper_cancelled_ctx_integration_test.go — GH #483, the
// third converted site.
//
// 71fed5ef's commit message said update/dispatch_sweeper.go's
// update_dispatch_sweeper lock (like backup/tenant_reclaim_worker.go's) was
// "covered by inspection only" because no seam existed to cancel the pass's
// context between taking the advisory lock and releasing it, without
// reshaping production code. A security review on PR #495 established that
// claim was wrong here too: update.SweepWorker already takes its Repo through
// an injectable interface (NewSweepWorker(repo Repo, enq SweepEnqueuer, pool
// *pgxpool.Pool, logger)), and ListDueRuns runs AFTER the advisory lock is
// taken and BEFORE the deferred unlock — the identical seam
// gh483_advisory_unlock_cancelled_ctx_integration_test.go already uses for
// org.PurgeWorker's ObjectPurger. A fake Repo whose ListDueRuns cancels the
// sweep's own context and then fails reaches the exact same release-on-a-
// dead-context path, with nothing in dispatch_sweeper.go touched.
//
// See gh483_advisory_unlock_cancelled_ctx_integration_test.go for the full
// defect writeup.
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
	"github.com/mosamlife/wpmgr/apps/api/internal/update"
)

// gh483CancellingSweepRepo is the seam. It embeds update.Repo as a nil
// interface so every method the sweeper does not reach (DispatchDueRun,
// ExpireDueRun, ...) is promoted and compiles the fake against the full
// interface without hand-writing panics for each; sweep() only ever calls
// ListDueRuns before reaching any of them, which the assertions below verify.
//
// ListDueRuns cancels the pass's own context and fails, which is what a
// SIGTERM landing between "advisory lock acquired" and "due-run scan
// returned" does to a real sweep tick: the pass unwinds, and the deferred
// pg_advisory_unlock in dispatch_sweeper.go's sweep() is reached with a dead
// context.
type gh483CancellingSweepRepo struct {
	update.Repo
	t       *testing.T
	pool    *db.Pool
	cancel  context.CancelFunc
	dueCall int

	// heldDuringScan is the positive control. It records whether
	// gh483SweepLockKey was observed HELD at the one point in the pass
	// where it must be: after SweepWorker.Work has taken the advisory lock
	// and before the deferred unlock runs. Without this, a rename of
	// dispatch_sweeper.go's unexported sweepLockKey out from under
	// gh483SweepLockKey makes gh483SweepLockHeld probe a key nobody holds,
	// report "not held" both here and in the pre/post checks below, and
	// every assertion in this test would then be satisfied vacuously —
	// this is the control that turns that drift into a failure instead.
	heldDuringScan bool
}

func (r *gh483CancellingSweepRepo) ListDueRuns(_ context.Context, _ int32) ([]update.Run, error) {
	r.dueCall++
	// Read BEFORE cancel(): this is the seam where the lock is live —
	// taken by sweep(), not yet released by its deferred unlock.
	r.heldDuringScan = gh483SweepLockHeld(r.t, r.pool, gh483SweepLockKey)
	r.cancel()
	return nil, errors.New("gh483: due-run scan unreachable (test injects the cancellation here)")
}

// gh483EmptySweepRepo returns no due runs and never touches the rest of the
// Repo interface, for the uncancelled over-fire proof.
type gh483EmptySweepRepo struct {
	update.Repo
	dueCall int
}

func (r *gh483EmptySweepRepo) ListDueRuns(_ context.Context, _ int32) ([]update.Run, error) {
	r.dueCall++
	return nil, nil
}

// gh483NoopSweepEnqueuer is wired only so SweepWorker.sweep passes its
// "enqueuer is wired" guard; the fakes above make sure it is never actually
// invoked (ListDueRuns fails or returns empty before any enqueue).
type gh483NoopSweepEnqueuer struct{}

func (gh483NoopSweepEnqueuer) EnqueueDispatchIfAbsent(context.Context, uuid.UUID, uuid.UUID, time.Time) (bool, error) {
	return false, nil
}

// gh483SweepLockHeld reports whether the update_dispatch_sweeper SESSION
// advisory lock (dispatch_sweeper.go's sweepLockKey, a single-bigint
// pg_advisory_lock(hashtext($1)), confirmed at dispatch_sweeper.go:232) is
// held by ANY session, read from pg_locks THROUGH THE APPLICATION POOL as the
// application role.
//
// A single-bigint advisory lock stores key = hashtext(key)::bigint (an
// implicit, sign-extending int4->int8 cast) and pg_locks exposes it as
// classid = uint32(key >> 32), objid = uint32(key), objsubid = 1 — as opposed
// to the two-int form's objsubid = 2 that gh483OrgLockHeld reads. hashtext
// returns int4 and is routinely negative; sign-extension means the high 32
// bits of the bigint are either all-zero (hashtext >= 0) or all-one (hashtext
// < 0), so classid is computed here with a bitmask rather than the
// binary-coercion cast gh483OrgLockHeld uses for the two-int form, and objid
// is the same hashtext($1)::oid bit-reinterpretation as that helper.
//
// This reads pg_locks rather than probing with pg_try_advisory_lock for the
// same reason gh483OrgLockHeld does: the pool can hand back the very
// connection that leaked the lock, and advisory locks are re-entrant per
// session, so a same-session probe can report "free" while the lock is held.
func gh483SweepLockHeld(t *testing.T, pool *db.Pool, key string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_locks
		  WHERE locktype = 'advisory'
		    AND objsubid = 1
		    AND objid    = hashtext($1)::oid
		    AND classid  = ((hashtext($1)::bigint >> 32) & 4294967295)::oid`,
		key).Scan(&n); err != nil {
		t.Fatalf("read pg_locks for the %s advisory lock: %v", key, err)
	}
	return n > 0
}

// gh483SweepLockKey mirrors the unexported sweepLockKey constant in
// dispatch_sweeper.go (":232" per the task brief), duplicated here because the
// production constant is unexported and this package cannot reach it. Its
// value is confirmed against the production source at review time; a rename
// there without an update here would make gh483SweepLockHeld probe the wrong
// key and every assertion below would report "not held" regardless of the
// actual bug, so a drift here is not silently safe.
const gh483SweepLockKey = "update_dispatch_sweeper"

// TestGH483_SweepLockCancelledMidPassDoesNotLeakTheDispatchSweepLock is the
// regression. It runs a real update.SweepWorker.Work over a real Postgres
// pool, cancels the pass's context mid-pass at the ListDueRuns seam, and then
// requires the update_dispatch_sweeper advisory lock to be gone.
//
// Against the unfixed code (conn.Exec(ctx, ...) in the release defer) the lock
// is still held when Work returns and this fails. Against the fix
// (db.CleanupContext) it is released.
//
//	go test ./tests/ -run TestGH483_SweepLockCancelledMidPassDoesNotLeakTheDispatchSweepLock -v -count=1
func TestGH483_SweepLockCancelledMidPassDoesNotLeakTheDispatchSweepLock(t *testing.T) {
	pool := startPostgres(t) // wpmgr_app: NOSUPERUSER, NOBYPASSRLS

	if gh483SweepLockHeld(t, pool, gh483SweepLockKey) {
		t.Fatalf("the %s lock was already held before the sweep ran; this test cannot "+
			"attribute anything it observes afterwards", gh483SweepLockKey)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	repo := &gh483CancellingSweepRepo{t: t, pool: pool, cancel: cancel}

	worker := update.NewSweepWorker(repo, gh483NoopSweepEnqueuer{}, pool.Pool, nil)

	// Work swallows the pass-level error (sweep() failing is logged, not
	// returned) exactly like org.PurgeWorker.Work, so the assertion is on the
	// database, not on Work's return value.
	if err := worker.Work(ctx, nil); err != nil {
		t.Fatalf("sweep pass returned an error at the Work level: %v", err)
	}
	if repo.dueCall == 0 {
		t.Fatal("the sweep never reached ListDueRuns, so the context was never cancelled " +
			"mid-pass and this test proved nothing. Check that the advisory lock was " +
			"actually acquired before the scan ran")
	}
	if ctx.Err() == nil {
		t.Fatal("the context was not cancelled, so the deferred unlock ran on a live context " +
			"and this test proved nothing")
	}
	if !repo.heldDuringScan {
		t.Fatalf("the %s advisory lock was NOT observed held during the due-run scan, before "+
			"the pass released it. Every assertion below this point is vacuous, most likely "+
			"because gh483SweepLockKey (%q) no longer matches dispatch_sweeper.go's "+
			"unexported sweepLockKey — check that constant first, before suspecting the "+
			"unlock fix itself", gh483SweepLockKey, gh483SweepLockKey)
	}

	if gh483SweepLockHeld(t, pool, gh483SweepLockKey) {
		t.Fatalf("GH #483: the %s advisory lock is STILL HELD after a sweep pass that was "+
			"cancelled mid-flight. The connection went back to the pool healthy and still "+
			"locked, so every later pass reads false from pg_try_advisory_lock, logs "+
			"\"another replica holds the lock\" and skips — and the heartbeat whose ABSENCE "+
			"is the sweeper's own dead-process detector goes quiet right along with it, "+
			"until db.MaxConnLifetime (30m) recycles the connection", gh483SweepLockKey)
	}
}

// TestGH483_SweepOnALiveContextStillReleasesTheDispatchSweepLock is the
// over-fire guard. The fix must not be a fix that only works when something
// went wrong: the ordinary completed pass, on a context nobody cancelled, must
// still leave the lock released.
//
//	go test ./tests/ -run TestGH483_SweepOnALiveContextStillReleasesTheDispatchSweepLock -v -count=1
func TestGH483_SweepOnALiveContextStillReleasesTheDispatchSweepLock(t *testing.T) {
	pool := startPostgres(t)

	if gh483SweepLockHeld(t, pool, gh483SweepLockKey) {
		t.Fatalf("the %s lock was already held before the sweep ran", gh483SweepLockKey)
	}

	repo := &gh483EmptySweepRepo{}
	worker := update.NewSweepWorker(repo, gh483NoopSweepEnqueuer{}, pool.Pool, nil)

	ctx := context.Background()
	if err := worker.Work(ctx, nil); err != nil {
		t.Fatalf("sweep pass: %v", err)
	}
	if repo.dueCall == 0 {
		t.Fatal("the sweep never reached ListDueRuns; this run proved nothing")
	}

	if gh483SweepLockHeld(t, pool, gh483SweepLockKey) {
		t.Fatalf("a NORMAL, uncancelled sweep pass left the %s advisory lock held. The unlock "+
			"is broken on the ordinary path, not just the cancelled one", gh483SweepLockKey)
	}
}
