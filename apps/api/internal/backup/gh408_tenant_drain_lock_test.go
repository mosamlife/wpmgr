package backup

// gh408_tenant_drain_lock_test.go, PR #410 review finding 1: the tenant drain
// deleted object storage without the per-tenant org_lifecycle advisory lock that
// DELETE /orgs/{orgId}, POST /orgs/{orgId}/restore and the Lane B purge worker
// all take.
//
// The demonstrated consequence was an organisation restored mid-sweep losing
// chunks whose objects the drain had already removed, with the task closed
// completed=true and last_error empty. Silent, and unrecoverable.
//
// The measured proof against real Postgres, with the lock genuinely held on a
// second connection, is
// apps/api/tests/gh408_reclaim_lock_and_order_integration_test.go. THIS is the
// containerless half, and it runs in the CI lane every pull request pays for.
// It asserts the behaviour a lock is only worth having if it has: contention
// deletes NOTHING, closes nothing, spends no attempt, and the lock comes back on
// every path including the failing ones.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
)

func gh408QuietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// gh408LockTestTask seeds one due tenant task whose four guards would all pass,
// so that anything the drain declines to do is the lock's doing and nothing
// else's.
func gh408LockTestTask(state *fakeTenantReclaimStore, tenantID uuid.UUID) TenantReclaimTask {
	task := TenantReclaimTask{ID: uuid.New(), TenantID: tenantID, Kind: TenantReclaimKindStorage}
	state.tasks = []TenantReclaimTask{task}
	state.tenantExists[tenantID] = false
	state.sitesExist[tenantID] = false
	state.chunksExist[tenantID] = false
	return task
}

// TestGH408_ContendedTenantLockDeletesNothingAndKeepsTheTaskOpen is the finding
// stated as behaviour.
func TestGH408_ContendedTenantLockDeletesNothingAndKeepsTheTaskOpen(t *testing.T) {
	tenantID := uuid.New()
	state := newFakeTenantReclaimStore()
	task := gh408LockTestTask(state, tenantID)

	store := newGCStore()
	key := "chunks/" + tenantID.String() + "/deadbeef"
	store.put(key)

	// Another organisation lifecycle operation, a restore say, already holds it.
	state.lockHeldBy[tenantID] = true

	w := NewTenantReclaimWorker(state, store, gh408QuietLogger())
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work: %v", err)
	}

	if !store.has(key) {
		t.Fatal("the drain deleted storage while another organisation lifecycle operation held " +
			"this tenant's lock. That is the restored-organisation data loss this lock exists to stop")
	}
	if state.completed[task.ID] {
		t.Error("a yielded task was CLOSED, so its objects would be forgotten")
	}
	if state.failures[task.ID] != 0 {
		t.Errorf("attempts = %d after contention, want 0: contention is not a failed attempt, and "+
			"counting it as one walks a healthy task to the give-up cap", state.failures[task.ID])
	}
	if state.lastError[task.ID] != "" {
		t.Errorf("last_error = %q after contention, want empty: nothing failed", state.lastError[task.ID])
	}

	// And the yield must be a yield. Once the holder is done, the next tick does
	// the work: a drain that gave up permanently would be a leak wearing the
	// costume of safety.
	state.lockHeldBy[tenantID] = false
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("second Work: %v", err)
	}
	if store.has(key) {
		t.Error("the uncontended tick did not drain the tenant's storage")
	}
	if !state.completed[task.ID] {
		t.Error("the uncontended tick did not close the task")
	}
}

// TestGH408_TenantDrainTakesTheLockBeforeItsGuards pins the ORDER.
//
// The guards read tenant lifecycle state, so taking the lock after them would
// leave exactly the window the finding is about: a read that has already gone
// stale by the time the first Delete runs. A refactor that moves the lock down
// one guard would still pass a test that only checked the lock was taken at all,
// which is why this asserts the guards were never reached.
func TestGH408_TenantDrainTakesTheLockBeforeItsGuards(t *testing.T) {
	tenantID := uuid.New()
	state := newFakeTenantReclaimStore()
	gh408LockTestTask(state, tenantID)
	state.lockHeldBy[tenantID] = true

	probed := &probingTenantStore{fakeTenantReclaimStore: state}
	w := NewTenantReclaimWorker(probed, newGCStore(), gh408QuietLogger())
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if probed.guardCalls != 0 {
		t.Errorf("the drain ran %d guard check(s) on a tenant whose lock it does not hold. The "+
			"guards must be taken UNDER the lock or their answer is stale before it is used",
			probed.guardCalls)
	}
}

// TestGH408_TenantLockIsAlwaysReleased covers the paths a lock is usually leaked
// on: a guard that refuses, a storage fault, and a clean completion. A leaked
// session lock would wedge every later delete, restore and purge of that
// organisation, which is a different outage from the one being fixed.
func TestGH408_TenantLockIsAlwaysReleased(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakeTenantReclaimStore, *gcStore, uuid.UUID)
	}{
		{"clean drain", func(*fakeTenantReclaimStore, *gcStore, uuid.UUID) {}},
		{"guard refuses", func(s *fakeTenantReclaimStore, _ *gcStore, id uuid.UUID) {
			s.tenantExists[id] = true
		}},
		{"storage fault", func(_ *fakeTenantReclaimStore, st *gcStore, id uuid.UUID) {
			st.failKey = "chunks/" + id.String() + "/deadbeef"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenantID := uuid.New()
			state := newFakeTenantReclaimStore()
			gh408LockTestTask(state, tenantID)
			store := newGCStore()
			store.put("chunks/" + tenantID.String() + "/deadbeef")
			tc.setup(state, store, tenantID)

			w := NewTenantReclaimWorker(state, store, gh408QuietLogger())
			if err := w.Work(context.Background(), nil); err != nil {
				t.Fatalf("Work: %v", err)
			}
			if state.locked != 1 {
				t.Fatalf("locked %d time(s), want 1", state.locked)
			}
			if state.released != state.locked {
				t.Errorf("locked %d, released %d: a lock left held wedges every later delete, "+
					"restore and purge of this organisation", state.locked, state.released)
			}
		})
	}
}

// TestGH408_TenantLockErrorFailsTheTaskRatherThanDraining is the third answer the
// lock can give. A database that cannot be asked whether a restore is in flight
// is not permission to assume there is not one.
func TestGH408_TenantLockErrorFailsTheTaskRatherThanDraining(t *testing.T) {
	tenantID := uuid.New()
	state := newFakeTenantReclaimStore()
	task := gh408LockTestTask(state, tenantID)
	state.lockErr = errors.New("connection refused")

	store := newGCStore()
	key := "chunks/" + tenantID.String() + "/deadbeef"
	store.put(key)

	w := NewTenantReclaimWorker(state, store, gh408QuietLogger())
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if !store.has(key) {
		t.Fatal("the drain deleted storage after failing to take the lock")
	}
	if state.completed[task.ID] {
		t.Error("the task was closed after a lock failure, so its objects would be forgotten")
	}
	if state.failures[task.ID] != 1 {
		t.Errorf("attempts = %d, want 1: an unavailable lock is a real failure and is retried "+
			"with backoff", state.failures[task.ID])
	}
}

// probingTenantStore counts guard reads, so the ORDER assertion above can be made
// about behaviour rather than about source text.
type probingTenantStore struct {
	*fakeTenantReclaimStore
	guardCalls int
}

func (p *probingTenantStore) TenantExists(ctx context.Context, id uuid.UUID) (bool, error) {
	p.guardCalls++
	return p.fakeTenantReclaimStore.TenantExists(ctx, id)
}

func (p *probingTenantStore) SitesExist(ctx context.Context, id uuid.UUID) (bool, error) {
	p.guardCalls++
	return p.fakeTenantReclaimStore.SitesExist(ctx, id)
}

func (p *probingTenantStore) ChunkRowsExist(ctx context.Context, id uuid.UUID) (bool, error) {
	p.guardCalls++
	return p.fakeTenantReclaimStore.ChunkRowsExist(ctx, id)
}
