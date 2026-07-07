package backup

// gh168_chain_gc_test.go — GH #168 CRITICAL regression coverage: a retention-GC
// reachability computation lost track of a within-retention parent files-list
// chunk (because a failed/aborted incremental retry reused a (chain_id,
// generation) pair) and permanently broke an incremental chain. These are the
// white-box (in-memory gcFakeRepo, no DB) counterparts to the real-Postgres
// integration tests in tests/backup_gh168_integration_test.go:
//
//   - P1a (chainGenWinner): the deterministic byGen tiebreak, tested both as a
//     pure function and end-to-end through reachableChunks.
//   - P2 (sweepChunks' ground-truth guard): proven independent of P1 by
//     directly injecting a wrong/incomplete liveSet.
//
// The "no orphan leak" (T5) property is already exercised by the EXISTING
// TestGC_CarryForwardOrigin_KeepsOldChunk ("g0only") and
// TestGC_CrossSiteSharedChunk ("a_only") in gc_mark_sweep_test.go — both
// still correctly sweep a genuinely-dead chunk with every GH #168 fix
// (including the P2 guard) fully wired in; see backup_bulk_delete/gh168
// integration tests for the real-Postgres equivalent.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// P1a: chainGenWinner — pure-function unit tests (order-independent by
// construction, no fake repo / no map-iteration involved).
// ---------------------------------------------------------------------------

// TestChainGenWinner_CompletedBeatsNonCompleted proves a completed row always
// wins over a non-completed one, regardless of which is passed as `existing`
// vs `candidate` (i.e. regardless of scan/iteration order).
func TestChainGenWinner_CompletedBeatsNonCompleted(t *testing.T) {
	completed := Snapshot{ID: uuid.New(), Status: StatusCompleted}
	failed := Snapshot{ID: uuid.New(), Status: StatusFailed}

	if got := chainGenWinner(completed, failed); got.ID != completed.ID {
		t.Errorf("chainGenWinner(completed, failed) = %s, want the completed row %s", got.ID, completed.ID)
	}
	if got := chainGenWinner(failed, completed); got.ID != completed.ID {
		t.Errorf("chainGenWinner(failed, completed) = %s, want the completed row %s (a completed snapshot must never be shadowed)", got.ID, completed.ID)
	}
}

// TestChainGenWinner_TwoCompletedStableLowestID proves that between two
// completed rows sharing a generation (the narrow legacy/race case the m96
// partial unique index closes going forward), the LOWEST snapshot ID wins,
// deterministically, regardless of argument order.
func TestChainGenWinner_TwoCompletedStableLowestID(t *testing.T) {
	a := Snapshot{ID: uuid.New(), Status: StatusCompleted}
	b := Snapshot{ID: uuid.New(), Status: StatusCompleted}
	if bytes.Compare(a.ID[:], b.ID[:]) > 0 {
		a, b = b, a
	}
	// a.ID < b.ID now.
	if got := chainGenWinner(a, b); got.ID != a.ID {
		t.Errorf("chainGenWinner(a, b) = %s, want the lower id %s", got.ID, a.ID)
	}
	if got := chainGenWinner(b, a); got.ID != a.ID {
		t.Errorf("chainGenWinner(b, a) = %s, want the lower id %s (stable regardless of argument order)", got.ID, a.ID)
	}
}

// ---------------------------------------------------------------------------
// P1a + P1b end-to-end through reachableChunks (the real oracle GC/restore
// share), via gcFakeRepo.
// ---------------------------------------------------------------------------

// buildArchiveDeltaChain seeds a completed ADR-051 archive-delta chain 0..maxGen
// in repo: each generation carries a files-list entry (hash "fl<gen>") and one
// wp-content zip-part entry (hash "part<gen>"), and both chunks are registered
// as stored. Returns the per-generation Snapshot values (index == generation).
func buildArchiveDeltaChain(repo *gcFakeRepo, tenantID, siteID, chainID uuid.UUID, maxGen int, createdAt time.Time) []Snapshot {
	snaps := make([]Snapshot, maxGen+1)
	for gen := 0; gen <= maxGen; gen++ {
		s := gcSnap(tenantID, siteID, chainID, gen, gen > 0, createdAt)
		repo.addSnap(s)
		fl := fmt.Sprintf("fl%d", gen)
		part := fmt.Sprintf("part%d", gen)
		repo.manifest[s.ID] = []ManifestEntry{
			{Path: "files.list", EntryKind: EntryKindFilesList, ChunkHashes: []string{fl}},
			{Path: fmt.Sprintf("wp-content.part%03d.zip", gen), EntryKind: EntryKindWPContent, ChunkHashes: []string{part}},
		}
		repo.addChunk(fl, createdAt)
		repo.addChunk(part, createdAt)
		snaps[gen] = s
	}
	return snaps
}

// TestReachableChunks_FailedDuplicateGenerationDoesNotShadowCompleted is the
// white-box counterpart of the GH #168 regression: a failed retry reused
// generation 4 (empty manifest) AFTER the real completed gen-4 row. Pre-P1b
// (raw ListChainSnapshots, no status filter) this could shadow the completed
// row's real manifest in byGen[4] depending on scan order; post-fix the
// completed-only SQL variant excludes the failed row outright, so this MUST
// pass regardless of insertion order — proven by running both orders.
func TestReachableChunks_FailedDuplicateGenerationDoesNotShadowCompleted(t *testing.T) {
	for _, name := range []string{"failed_inserted_after_completed", "failed_inserted_before_completed"} {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
			tenantID, siteID, chainID := uuid.New(), uuid.New(), uuid.New()
			old := now.Add(-24 * time.Hour)

			repo := newGCFakeRepo(now)

			dupFailed := gcSnap(tenantID, siteID, chainID, 4, true, now.Add(-time.Hour))
			dupFailed.Status = StatusFailed
			// dupFailed's manifest is intentionally left unset (nil == empty) —
			// exactly the #168 scenario (a failed retry with no real manifest).

			var chain []Snapshot
			if name == "failed_inserted_before_completed" {
				repo.addSnap(dupFailed)
				chain = buildArchiveDeltaChain(repo, tenantID, siteID, chainID, 4, old)
			} else {
				chain = buildArchiveDeltaChain(repo, tenantID, siteID, chainID, 4, old)
				repo.addSnap(dupFailed)
			}

			svc := buildGCSvc(repo, newGCStore(), now)
			reach, err := svc.reachableChunks(context.Background(), tenantID, chain[4], 4)
			if err != nil {
				t.Fatalf("reachableChunks error: %v", err)
			}
			for gen := 0; gen <= 4; gen++ {
				for _, want := range []string{fmt.Sprintf("fl%d", gen), fmt.Sprintf("part%d", gen)} {
					if _, ok := reach[want]; !ok {
						t.Errorf("reachableChunks missing %q — the failed gen-4 duplicate shadowed the completed row's real manifest", want)
					}
				}
			}
		})
	}
}

// TestReachableChunks_TwoCompletedDuplicateGenerationsResolveToLowestID
// exercises P1a specifically: TWO rows at generation 4 are BOTH status=
// completed (the SQL completed-only filter alone cannot disambiguate this —
// only the m96 partial unique index prevents it going forward, and only
// chainGenWinner's Go-side tiebreak makes existing/legacy duplicate data safe
// to read). The lower-id row carries the REAL manifest; the higher-id row is
// an empty-manifest duplicate. The real row must win regardless of which one
// was inserted (and therefore iterated) first.
func TestReachableChunks_TwoCompletedDuplicateGenerationsResolveToLowestID(t *testing.T) {
	for _, insertBadFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("insertBadFirst=%v", insertBadFirst), func(t *testing.T) {
			now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
			tenantID, siteID, chainID := uuid.New(), uuid.New(), uuid.New()
			old := now.Add(-24 * time.Hour)

			repo := newGCFakeRepo(now)
			buildArchiveDeltaChain(repo, tenantID, siteID, chainID, 3, old)

			good := gcSnap(tenantID, siteID, chainID, 4, true, now.Add(-time.Hour))
			bad := gcSnap(tenantID, siteID, chainID, 4, true, now.Add(-time.Hour))
			if bytes.Compare(good.ID[:], bad.ID[:]) > 0 {
				good.ID, bad.ID = bad.ID, good.ID
			}
			repo.manifest[good.ID] = []ManifestEntry{
				{Path: "files.list", EntryKind: EntryKindFilesList, ChunkHashes: []string{"fl4"}},
				{Path: "wp-content.part004.zip", EntryKind: EntryKindWPContent, ChunkHashes: []string{"part4"}},
			}
			// bad.manifest is intentionally left unset (empty duplicate).
			repo.addChunk("fl4", old)
			repo.addChunk("part4", old)

			if insertBadFirst {
				repo.addSnap(bad)
				repo.addSnap(good)
			} else {
				repo.addSnap(good)
				repo.addSnap(bad)
			}

			svc := buildGCSvc(repo, newGCStore(), now)
			reach, err := svc.reachableChunks(context.Background(), tenantID, good, 4)
			if err != nil {
				t.Fatalf("reachableChunks error: %v", err)
			}
			for _, want := range []string{"fl4", "part4"} {
				if _, ok := reach[want]; !ok {
					t.Errorf("insertBadFirst=%v: reachableChunks missing %q — the higher-id empty duplicate shadowed the lower-id completed row", insertBadFirst, want)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// P2: sweepChunks' ground-truth guard, tested INDEPENDENTLY of P1 by directly
// injecting a wrong/incomplete liveSet (simulating any future reachability-
// computation regression, not specifically #168's).
// ---------------------------------------------------------------------------

// TestSweepChunks_P2GuardKeepsManifestReferencedChunk proves the guard keeps a
// chunk the (deliberately wrong, empty) liveSet marked unreachable, because a
// retained snapshot's manifest still references it — and that it logs a WARN
// carrying the gc_run_id, so the regression is diagnosable (P6).
func TestSweepChunks_P2GuardKeepsManifestReferencedChunk(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tenantID, siteID := uuid.New(), uuid.New()

	snap := gcSnap(tenantID, siteID, uuid.New(), 0, false, now.Add(-24*time.Hour))
	snap.ChainID = nil // a non-chained, manifest-only full backup.

	repo := newGCFakeRepo(now)
	repo.addSnap(snap)
	repo.manifest[snap.ID] = []ManifestEntry{
		{Path: "full.zip", EntryKind: EntryKindFile, ChunkHashes: []string{"keep-me"}},
	}
	ancient := now.Add(-90 * 24 * time.Hour) // well past any grace floor.
	repo.addChunk("keep-me", ancient)

	store := newGCStore()
	svc := buildGCSvc(repo, store, now)

	var buf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prevLogger)

	// The caller's liveSet is EMPTY — simulating ANY reachability-computation
	// bug (independent of #168's specific root cause) that failed to mark
	// "keep-me" as reachable even though a retained manifest still needs it.
	liveSet := map[string]struct{}{}
	floor := now

	swept, err := svc.sweepChunks(context.Background(), tenantID, "test-gc-run-id", liveSet, floor)
	if err != nil {
		t.Fatalf("sweepChunks error: %v", err)
	}
	if swept != 0 {
		t.Fatalf("swept = %d, want 0 — the P2 guard must keep a manifest-referenced chunk regardless of what liveSet said", swept)
	}
	if _, ok := repo.chunks["keep-me"]; !ok {
		t.Fatal("P2 guard failed: the chunk ROW was deleted despite still being manifest-referenced")
	}
	if store.deleted[chunkS3Key(uuid.Nil, "keep-me")] {
		t.Fatal("P2 guard failed: the chunk OBJECT was deleted despite still being manifest-referenced")
	}
	logged := buf.String()
	if !strings.Contains(logged, "P2 guard kept a chunk") {
		t.Errorf("expected a P2 guard WARN log line, got: %s", logged)
	}
	if !strings.Contains(logged, "test-gc-run-id") {
		t.Errorf("expected the WARN log to carry the gc_run_id for diagnosability (P6), got: %s", logged)
	}
	if !strings.Contains(logged, "keep-me") {
		t.Errorf("expected the WARN log to name the blake3 hash, got: %s", logged)
	}
}

// TestSweepChunks_P2GuardDoesNotBlockGenuinelyUnreachableChunk is the
// adversarial counterpart (T5-class): a chunk that is BOTH absent from
// liveSet AND not referenced by any manifest must still be swept — the P2
// guard must never degrade GC into a no-op / storage leak.
func TestSweepChunks_P2GuardDoesNotBlockGenuinelyUnreachableChunk(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tenantID := uuid.New()

	repo := newGCFakeRepo(now)
	ancient := now.Add(-90 * 24 * time.Hour)
	repo.addChunk("truly-orphaned", ancient)
	// No snapshot / manifest entry anywhere references "truly-orphaned".

	store := newGCStore()
	svc := buildGCSvc(repo, store, now)

	liveSet := map[string]struct{}{}
	swept, err := svc.sweepChunks(context.Background(), tenantID, "test-gc-run-id", liveSet, now)
	if err != nil {
		t.Fatalf("sweepChunks error: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept = %d, want 1 — a genuinely orphaned chunk must still be reclaimed (P2 must not leak storage)", swept)
	}
	if _, ok := repo.chunks["truly-orphaned"]; ok {
		t.Error("genuinely orphaned chunk row should have been swept")
	}
	if !store.deleted[chunkS3Key(uuid.Nil, "truly-orphaned")] {
		t.Error("genuinely orphaned chunk object should have been deleted")
	}
}
