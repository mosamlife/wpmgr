package backup

// gh402_gc_roster_test.go: the second-order leak, and the negative that proves
// fixing it did not widen anything dangerous.
//
// THE LEAK. The retention GC's tenant roster was
// `SELECT DISTINCT tenant_id FROM backup_snapshots WHERE status = 'completed'`.
// Deleting the site that held a tenant's LAST completed snapshot therefore
// dropped that tenant off the roster permanently, and its chunk bytes were
// never swept again. backup_chunks has no FK to sites so it survives the
// cascade intact, which is exactly why unioning it in reaches the emptied
// tenant. The SQL itself is proved in apps/api/tests against real Postgres.
//
// THE RISK, AND THE NEGATIVE. Widening enumeration means the destructive sweep
// now VISITS tenants it never visited before. These two tests are the pair: one
// asserts the emptied tenant's dead bytes are finally reclaimed, the other
// asserts that the same shape with work in flight reclaims nothing. Enumeration
// widened; the delete decision did not.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A tenant whose only site was deleted: chunk rows survive the cascade, no
// snapshot rows remain, nothing is in flight. Those bytes are genuinely dead
// and the sweep should reclaim them.
func TestGH402_GCRoster_EmptiedTenantChunksAreReclaimed(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	old := now.Add(-90 * 24 * time.Hour)

	repo := newGCFakeRepo(now)
	// No snapshots at all: the site delete cascaded every row away.
	repo.addChunk("orphan1", old)
	repo.addChunk("orphan2", old)

	store := newGCStore()
	store.put(chunkS3Key(uuid.Nil, "orphan1"))
	store.put(chunkS3Key(uuid.Nil, "orphan2"))

	svc := buildGCSvc(repo, store, now)
	_, chunksDel, err := svc.RunRetentionGC(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("RunRetentionGC error: %v", err)
	}

	if chunksDel != 2 {
		t.Errorf("expected the emptied tenant's 2 dead chunks to be swept, got %d", chunksDel)
	}
	for _, h := range []string{"orphan1", "orphan2"} {
		if _, ok := repo.chunks[h]; ok {
			t.Errorf("chunk row %q survived, its bytes still leak", h)
		}
		if store.has(chunkS3Key(uuid.Nil, h)) {
			t.Errorf("chunk object %q survived, its bytes still leak", h)
		}
	}
}

// THE PAIRED NEGATIVE. Same emptied-tenant shape, but a backup is RUNNING. The
// in-flight floor pins the deletion horizon to that snapshot's created_at, so
// nothing newer is touched, and the ground-truth manifest guard keeps anything
// the running snapshot already references. Reaching this tenant must not mean
// sweeping it.
func TestGH402_GCRoster_WideningDoesNotSweepInFlight(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	siteID := uuid.New()
	chainID := uuid.New()

	// The running backup started an hour ago; its chunks are landing now.
	started := now.Add(-time.Hour)

	repo := newGCFakeRepo(now)
	repo.inflightFloor = started

	// A pending/running snapshot has no completed row, so the roster only
	// reaches this tenant because of the chunk union.
	running := gcSnap(tenantID, siteID, chainID, 0, false, started)
	running.Status = StatusRunning
	repo.addSnap(running)
	// Its manifest rows are already landing: the P2 ground-truth guard sees them.
	repo.manifest[running.ID] = []ManifestEntry{
		{Path: "in-flight.zip", EntryKind: EntryKindFile, ChunkHashes: []string{"landing"}},
	}

	// "landing" is brand new. "revived" is an OLD chunk the running backup
	// re-referenced through tenant-global dedup, so the dedup oracle bumped its
	// last_referenced_at to now: ancient created_at, live all the same.
	repo.addChunk("landing", started)
	repo.addChunkRef("revived", now.Add(-200*24*time.Hour), now)

	store := newGCStore()
	store.put(chunkS3Key(uuid.Nil, "landing"))
	store.put(chunkS3Key(uuid.Nil, "revived"))

	svc := buildGCSvc(repo, store, now)
	_, chunksDel, err := svc.RunRetentionGC(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("RunRetentionGC error: %v", err)
	}

	if chunksDel != 0 {
		t.Errorf("the widened roster swept %d chunks out of a tenant with a backup in flight", chunksDel)
	}
	for _, h := range []string{"landing", "revived"} {
		if _, ok := repo.chunks[h]; !ok {
			t.Errorf("chunk row %q was swept out from under a running backup", h)
		}
		if store.has(chunkS3Key(uuid.Nil, h)) != true {
			t.Errorf("chunk object %q was swept out from under a running backup", h)
		}
	}
}
