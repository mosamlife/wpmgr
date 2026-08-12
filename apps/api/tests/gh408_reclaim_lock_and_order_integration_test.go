package tests

// gh408_reclaim_lock_and_order_integration_test.go: the two PR #410 review
// findings, each proved against real Postgres.
//
//  1. THE DRAIN DELETED WITHOUT THE LOCK THE REST OF THIS CODEBASE USES FOR
//     TENANT LIFECYCLE. reclaimOne checked its guards and then deleted, with
//     nothing holding the two together. An organisation restored inside that
//     window lost the chunks, manifests and client report PDFs the guards had
//     just concluded were nobody's, and the task closed completed=true with an
//     empty last_error. Object deletion is irreversible, so that is not a race
//     that resolves badly, it is data gone. The fix takes the same per-tenant
//     org_lifecycle advisory lock DELETE /orgs/{orgId}, POST /orgs/{orgId}/restore
//     and the Lane B purge worker take.
//
//  2. `reclaim discover` RANGED A MAP. Identical bucket contents produced a
//     different report every run, so two runs could not be diffed and an
//     operator could not tell a changed bucket from a reshuffled list.
//
// NOT RUN BY CI (apps/api/tests is excluded from the fast lane by owner
// decision). Run with `make test-integration` from the repository root.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	adminpkg "github.com/mosamlife/wpmgr/apps/api/internal/admin"
	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/org"
)

// ---------------------------------------------------------------------------
// 1. The lock.
// ---------------------------------------------------------------------------

// TestGH408_DrainYieldsWhileAnOrgLifecycleOperationHoldsTheLock is the race,
// constructed rather than argued.
//
// It holds the per-tenant org_lifecycle lock from a SECOND connection exactly as
// POST /orgs/{orgId}/restore holds it (pg_advisory_xact_lock, for the length of
// its transaction, delete_handler.go), runs a real drain tick against a tenant
// with objects in storage, and requires that tick to delete NOTHING and to leave
// the task open, unattempted and unmarked. It then releases the lock and requires
// the very next tick to complete normally, because a drain that yielded forever
// would be a leak dressed up as safety.
//
// The assertions are on stored objects and on the row, never on logs: the defect
// this is about was silent, and a proof that reads log lines could pass while the
// objects went.
func TestGH408_DrainYieldsWhileAnOrgLifecycleOperationHoldsTheLock(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	// A real Lane A hard delete, so the task under test is the one production
	// writes: admin_delete_empty_tenant records it in the delete's own
	// transaction (m116) and the tenants row is genuinely gone.
	tenant := seedTenant(t, pool, "gh408-lock-"+uuid.NewString()[:8])
	if deleted, err := adminpkg.NewRepo(pool).DeleteEmptyTenant(ctx, tenant); err != nil || !deleted {
		t.Fatalf("delete empty tenant: deleted=%v err=%v", deleted, err)
	}
	gh408MakeTenantTaskDue(t, admin, tenant)
	var task uuid.UUID
	if err := admin.QueryRow(ctx,
		`SELECT id FROM tenant_object_reclaim WHERE tenant_id = $1`, tenant).Scan(&task); err != nil {
		t.Fatalf("read task id: %v", err)
	}

	store := newGH402Store()
	var keys []string
	for i := 0; i < 6; i++ {
		k := gh408ChunkKey(tenant, gh408Hash(fmt.Sprintf("lock-%d-%s", i, tenant)))
		store.put(k)
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// THE RESTORE ARRIVES FIRST and takes the lock. A transaction-scoped lock on
	// its own connection, held open, is what the restore endpoint does for the
	// length of its transaction, and it is the harder case for the drain: the
	// drain must not block on it either.
	conn, aerr := admin.Acquire(ctx)
	if aerr != nil {
		t.Fatalf("acquire the restore connection: %v", aerr)
	}
	tx, berr := conn.Begin(ctx)
	if berr != nil {
		conn.Release()
		t.Fatalf("begin the restore transaction: %v", berr)
	}
	if _, lerr := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`,
		org.LifecycleLockKey, tenant.String()); lerr != nil {
		_ = tx.Rollback(ctx)
		conn.Release()
		t.Fatalf("take the org lifecycle lock as a restore would: %v", lerr)
	}
	t.Logf("a restore holds %s for tenant %s", org.LifecycleLockKey, tenant)

	// TICK 1, contended. This is the tick that used to delete.
	gh408DrainTenantTasks(t, pool, store)

	var survived int
	for _, k := range keys {
		if store.has(k) {
			survived++
		}
	}
	if survived != len(keys) {
		t.Fatalf("the contended tick deleted %d of %d objects while an organisation lifecycle "+
			"operation held this tenant's lock. A restore landing in that window loses them "+
			"permanently, and nothing else in the database names them",
			len(keys)-survived, len(keys))
	}
	attempts, completed, lastErr := gh408TaskState(t, admin, task)
	if completed {
		t.Fatal("the contended tick CLOSED the task. The work would then be forgotten, which is " +
			"worse than the delete it did not do")
	}
	if attempts != 0 {
		t.Errorf("attempts = %d after a contended tick, want 0: lock contention is not a failed "+
			"attempt, and burning attempts on it walks a healthy task to the cap", attempts)
	}
	if lastErr != "" {
		t.Errorf("last_error = %q after a contended tick, want empty: nothing failed", lastErr)
	}
	if due := gh408NextAttemptIn(t, admin, tenant); due > 0 {
		t.Errorf("the task was backed off by %s; a yielded task must stay due for the next tick", due)
	}
	t.Logf("tick 1 (lock held): objects %d/%d survive, task open, attempts=%d, last_error=%q, still due",
		survived, len(keys), attempts, lastErr)

	// THE RESTORE FINISHES and the lock goes with its transaction.
	if rerr := tx.Rollback(ctx); rerr != nil {
		t.Fatalf("end the restore transaction: %v", rerr)
	}
	conn.Release()
	t.Logf("the restore transaction ended, %s released", org.LifecycleLockKey)

	// TICK 2, uncontended. The yield must be a yield, not a stall.
	gh408DrainTenantTasks(t, pool, store)

	var left []string
	for _, k := range keys {
		if store.has(k) {
			left = append(left, k)
		}
	}
	if len(left) != 0 {
		t.Fatalf("the uncontended tick left %d object(s) behind: %v", len(left), left)
	}
	attempts, completed, lastErr = gh408TaskState(t, admin, task)
	if !completed {
		t.Fatalf("the uncontended tick did not close the task: attempts=%d last_error=%q", attempts, lastErr)
	}
	t.Logf("tick 2 (lock released): objects 0/%d remain, task completed=%v, attempts=%d, last_error=%q",
		len(keys), completed, attempts, lastErr)
}

// TestGH408_TheDrainTakesTheSameLockTheOrgLifecycleUses proves the two lanes
// share one lock rather than two that merely look alike.
//
// It takes the drain's own lock through the production store, then tries to take
// the ORG side's lock from another connection and requires it to be refused. A
// second spelling of the key would compile, pass every behavioural test, and take
// a different lock, and the only symptom would be missing objects.
func TestGH408_TheDrainTakesTheSameLockTheOrgLifecycleUses(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenant := uuid.New()
	state := backup.NewTenantReclaimStore(pool)

	release, acquired, err := state.LockTenantLifecycle(ctx, tenant)
	if err != nil {
		t.Fatalf("take the lock: %v", err)
	}
	if !acquired {
		t.Fatal("an uncontended lock was not acquired")
	}

	var got bool
	if qerr := admin.QueryRow(ctx,
		`SELECT pg_try_advisory_lock(hashtext($1), hashtext($2))`,
		org.LifecycleLockKey, tenant.String()).Scan(&got); qerr != nil {
		t.Fatalf("probe the org side of the lock: %v", qerr)
	}
	if got {
		// Release what the probe took, so a failing test does not leak a lock into
		// the rest of the run.
		_, _ = admin.Exec(ctx, `SELECT pg_advisory_unlock(hashtext($1), hashtext($2))`,
			org.LifecycleLockKey, tenant.String())
		release()
		t.Fatal("the org lifecycle key was still free while the drain held its lock, so the drain " +
			"is taking a DIFFERENT lock. Delete and restore would then run straight through it")
	}
	t.Logf("with the drain holding it, the org lifecycle key is refused to everyone else")

	// And it must come back afterwards, or one drain would wedge every later
	// delete, restore and purge of that organisation.
	release()
	if qerr := admin.QueryRow(ctx,
		`SELECT pg_try_advisory_lock(hashtext($1), hashtext($2))`,
		org.LifecycleLockKey, tenant.String()).Scan(&got); qerr != nil {
		t.Fatalf("re-probe the lock: %v", qerr)
	}
	if !got {
		t.Fatal("the drain did not release the lock, so every later lifecycle operation on this " +
			"organisation would be refused")
	}
	_, _ = admin.Exec(ctx, `SELECT pg_advisory_unlock(hashtext($1), hashtext($2))`,
		org.LifecycleLockKey, tenant.String())
	t.Logf("after release the key is free again")
}

// ---------------------------------------------------------------------------
// 2. The ordering.
// ---------------------------------------------------------------------------

// gh408RenderDiscover renders candidates the way `reclaim discover` prints them
// (cmd/wpmgr-cli/reclaim.go), because the output an operator reads is the thing
// that has to be stable, not some internal ordering only a test can see.
func gh408RenderDiscover(found []backup.DiscoverCandidate) string {
	var b strings.Builder
	for _, c := range found {
		fmt.Fprintf(&b, "  %s  %d object(s) under %v\n", c.TenantID, c.Keys, c.Roots)
	}
	return b.String()
}

// TestGH408_DiscoverOutputIsOrderedAndRepeatable runs discovery repeatedly
// against IDENTICAL seeded state and requires byte-identical output every time.
//
// One run proves nothing here: Go randomises map iteration per range, so the old
// code could produce the expected order by luck. The seed therefore includes
// several candidates with EQUAL key counts, which is where an ordering that only
// sorts on the obvious key still shuffles.
func TestGH408_DiscoverOutputIsOrderedAndRepeatable(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	store := newGH402Store()

	// Nine orphans. Counts 5, 4 and 3 are unique; the 2s and the 1s are ties, and
	// the ties are the point.
	counts := []int{5, 4, 3, 2, 2, 2, 1, 1, 1}
	orphans := make([]uuid.UUID, 0, len(counts))
	for i, n := range counts {
		id := uuid.New()
		orphans = append(orphans, id)
		for k := 0; k < n; k++ {
			store.put(gh408ChunkKey(id, gh408Hash(fmt.Sprintf("disc-%d-%d-%s", i, k, id))))
		}
	}

	// One LIVE organisation with storage in the same bucket, which must be
	// filtered out and must not perturb the order of what remains.
	live := seedTenant(t, pool, "gh408-disc-live-"+uuid.NewString()[:8])
	store.put(gh408ChunkKey(live, gh408Hash("disc-live-"+live.String())))

	const runs = 5
	var rendered []string
	for i := 0; i < runs; i++ {
		found, err := backup.DiscoverOrphanTenantPrefixes(ctx, pool, store)
		if err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		if len(found) != len(orphans) {
			t.Fatalf("run %d: found %d candidates, want %d", i+1, len(found), len(orphans))
		}
		for _, c := range found {
			if c.TenantID == live {
				t.Fatalf("run %d: a live organisation's storage was reported as an orphan", i+1)
			}
		}
		// Descending by key count, so the biggest reclaim is at the top where an
		// operator scanning the list meets it first.
		for j := 1; j < len(found); j++ {
			if found[j-1].Keys < found[j].Keys {
				t.Fatalf("run %d: candidates are not ordered by key count: %d before %d",
					i+1, found[j-1].Keys, found[j].Keys)
			}
			if found[j-1].Keys == found[j].Keys &&
				found[j-1].TenantID.String() > found[j].TenantID.String() {
				t.Fatalf("run %d: equal-sized candidates are not broken by tenant id: %s before %s",
					i+1, found[j-1].TenantID, found[j].TenantID)
			}
		}
		rendered = append(rendered, gh408RenderDiscover(found))
	}

	for i := 1; i < runs; i++ {
		if rendered[i] != rendered[0] {
			t.Fatalf("run %d differs from run 1 for identical bucket contents.\n"+
				"run 1:\n%s\nrun %d:\n%s", i+1, rendered[0], i+1, rendered[i])
		}
	}
	t.Logf("%d runs, identical bucket contents, byte-identical output.\nrun 1:\n%srun 2:\n%s",
		runs, rendered[0], rendered[1])
}

// The fake is held to the interface discovery actually takes: ObjectLister, the
// read-only half, which cannot delete.
var _ backup.ObjectLister = (*gh402Store)(nil)
