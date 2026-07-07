package tests

// backup_gh168_integration_test.go — GH #168 CRITICAL regression: a retention-
// GC reachability computation lost track of a within-retention parent
// files-list chunk (because a failed/aborted incremental retry reused a
// (chain_id, generation) pair) and permanently broke an incremental chain.
// Runs against a REAL Postgres (testcontainers) so the actual m96 migration,
// the real ListCompletedChainSnapshots/chunkStillReferencedOnTx SQL, and the
// real RLS-scoped repo methods are exercised — not just an in-memory fake. See
// internal/backup/gh168_chain_gc_test.go for the white-box (no-DB) coverage
// of the same properties (chainGenWinner, the P2 guard in isolation).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/blobstore"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// gh168Entry is one backup_manifest_entries row to seed for a GH #168 fixture
// snapshot.
type gh168Entry struct {
	path      string
	entryKind string
	hashes    []string
}

// seedGH168Chunk inserts a backup_chunks row (idempotent — ON CONFLICT DO
// NOTHING, since a test may reference the same hash from more than one call
// site) and its matching placeholder object in the blobstore, mirroring
// putChunkObjects' convention.
func seedGH168Chunk(t *testing.T, pool *db.Pool, store *blobstore.Store, tenant uuid.UUID, hash string, createdAt time.Time) {
	t.Helper()
	key := "chunks/" + tenant.String() + "/" + hash
	err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `
			INSERT INTO backup_chunks (tenant_id, blake3, s3_key, size, refcount, created_at, updated_at, last_referenced_at)
			VALUES ($1, $2, $3, 4, 1, $4, $4, $4)
			ON CONFLICT (tenant_id, blake3) DO NOTHING`,
			tenant, hash, key, createdAt,
		)
		return err
	})
	if err != nil {
		t.Fatalf("seed gh168 chunk %s: %v", hash, err)
	}
	if err := store.Put(context.Background(), key, strings.NewReader("ct"), 2); err != nil {
		t.Fatalf("put gh168 chunk object %s: %v", hash, err)
	}
}

// seedGH168Snapshot inserts a backup_snapshots row directly with explicit id/
// status/chain fields, plus its backup_manifest_entries rows (if any). This is
// the raw-SQL fixture builder — NOT the real submission pipeline — because the
// whole point of this test is to construct the EXACT duplicate-generation DB
// state #168 left behind (a real completed row plus a failed/aborted retry
// that reused its generation), which the real SubmitManifest/CompleteBackup
// flow has no way to reach directly.
func seedGH168Snapshot(t *testing.T, pool *db.Pool, tenant, siteID, id, chainID uuid.UUID, generation int, status string, isIncremental bool, entries []gh168Entry, createdAt time.Time) {
	t.Helper()
	err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), `
			INSERT INTO backup_snapshots
				(id, tenant_id, site_id, kind, status, age_recipient,
				 is_incremental, chain_id, generation, created_at, finished_at)
			VALUES ($1,$2,$3,'files',$4,$5,$6,$7,$8,$9,$9)`,
			id, tenant, siteID, status, testRecipient, isIncremental, chainID, generation, createdAt,
		); err != nil {
			return err
		}
		for _, e := range entries {
			if _, err := tx.Exec(context.Background(), `
				INSERT INTO backup_manifest_entries (snapshot_id, tenant_id, path, entry_kind, chunk_hashes, size)
				VALUES ($1,$2,$3,$4,$5,$6)`,
				id, tenant, e.path, e.entryKind, e.hashes, int64(len(e.hashes))*4,
			); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed gh168 snapshot (gen %d, status %s): %v", generation, status, err)
	}
}

// archiveDeltaEntries returns the standard [files-list, wp-content-part]
// manifest entries for one ADR-051 archive-delta generation.
func archiveDeltaEntries(gen int, flHash, partHash string) []gh168Entry {
	return []gh168Entry{
		{path: "files.list", entryKind: backup.EntryKindFilesList, hashes: []string{flHash}},
		{path: "wp-content.partNNN.zip", entryKind: backup.EntryKindWPContent, hashes: []string{partHash}},
	}
}

// TestRetentionGC_GH168_DuplicateGenerationDoesNotOrphanFilesListChunk is the
// exact reported regression: a completed 5-generation (0-4) archive-delta
// chain, PLUS a failed duplicate at generation 4 (empty manifest — a retry
// that reused the generation after the real gen-4 attempt had already
// completed) and a failed generation 5 (the tip attempt that ultimately
// failed). Deleting the failed gen-5 snapshot fires a synchronous
// RunRetentionGC (DeleteSnapshotForUser). Pre-fix, the reachability walk could
// let the failed gen-4 duplicate shadow the real completed gen-4 row's
// manifest in byGen[4] — silently excluding its files-list/part chunk hashes
// from the live set for every retained snapshot, so Phase 5 (now Phase 4 —
// gc.go) swept them as "unreachable", permanently breaking the incremental
// chain. Post-fix (P1a/P1b + the P2 ground-truth guard), they MUST survive.
func TestRetentionGC_GH168_DuplicateGenerationDoesNotOrphanFilesListChunk(t *testing.T) {
	pool := startPostgres(t)
	store := startBlobstore(t)
	tenant := seedTenant(t, pool, "gh168-dupgen")
	siteID := seedSite(t, pool, tenant, "https://gh168.example.com")

	svc := newBackupService(t, pool, store, stubSiteLookup{info: enrolledSiteInfo(siteID, "https://gh168.example.com")}, &stubEnqueuer{})

	now := time.Now()
	gen0ID := uuid.New()
	chainID := gen0ID // base's chain_id is its own id, by project convention.
	genIDs := []uuid.UUID{gen0ID, uuid.New(), uuid.New(), uuid.New(), uuid.New()}

	// Seed the completed chain, generations 0..4, each with a files-list +
	// wp-content-part chunk. All well within the default 30-day retention
	// window, so ordinary age-based expiry never touches them — isolating the
	// duplicate-generation defect as the ONLY variable under test.
	for gen := 0; gen <= 4; gen++ {
		fl := "fl-gen" + string(rune('0'+gen))
		part := "part-gen" + string(rune('0'+gen))
		seedGH168Chunk(t, pool, store, tenant, fl, now)
		seedGH168Chunk(t, pool, store, tenant, part, now)
		seedGH168Snapshot(t, pool, tenant, siteID, genIDs[gen], chainID, gen, backup.StatusCompleted, gen > 0,
			archiveDeltaEntries(gen, fl, part), now)
	}

	// The exact #168 scenario: a FAILED duplicate at generation 4 (a retry
	// that reused the generation after the real gen-4 attempt had already
	// completed), with an EMPTY manifest, plus a failed generation 5 (the
	// eventual tip attempt that failed and is the target of the user's
	// delete). Both inserted AFTER their same/lower-generation completed
	// siblings.
	gen4DupID := uuid.New()
	gen5ID := uuid.New()
	seedGH168Snapshot(t, pool, tenant, siteID, gen4DupID, chainID, 4, backup.StatusFailed, true, nil, now.Add(time.Second))
	seedGH168Snapshot(t, pool, tenant, siteID, gen5ID, chainID, 5, backup.StatusFailed, true, nil, now.Add(2*time.Second))

	// Delete the failed gen-5 snapshot — this fires a synchronous
	// RunRetentionGC (DeleteSnapshotForUser), exactly as the reporter's chain
	// broke.
	if err := svc.DeleteSnapshotForUser(context.Background(), tenant, gen5ID); err != nil {
		t.Fatalf("DeleteSnapshotForUser(gen5 failed): %v", err)
	}

	// --- THE core #168 assertion -------------------------------------------
	// The completed gen-4 snapshot row itself must still exist (it is nowhere
	// near expiry).
	if got := countSnapshotRows(t, pool, tenant, []uuid.UUID{genIDs[4]}); got != 1 {
		t.Fatalf("completed gen-4 snapshot row missing after GC (found %d), want 1", got)
	}
	// Its files-list AND wp-content-part chunks — the ones a broken chain
	// would have permanently lost — must survive in BOTH the DB row and the
	// object store.
	for _, hash := range []string{"fl-gen4", "part-gen4"} {
		key := "chunks/" + tenant.String() + "/" + hash
		if exists, _, _ := store.Head(context.Background(), key); !exists {
			t.Errorf("REGRESSION (GH #168): gen-4's %q chunk object was swept — the incremental chain is now broken", hash)
		}
		existing, err := backup.NewRepo(pool).ExistingChunkHashes(context.Background(), tenant, []string{hash})
		if err != nil {
			t.Fatalf("existing chunk hashes: %v", err)
		}
		if _, ok := existing[hash]; !ok {
			t.Errorf("REGRESSION (GH #168): gen-4's %q chunk ROW was swept — the incremental chain is now broken", hash)
		}
	}
	// Every earlier generation's chunks must ALSO have survived — the bug
	// class is not specific to generation 4.
	for gen := 0; gen <= 3; gen++ {
		for _, hash := range []string{"fl-gen" + string(rune('0'+gen)), "part-gen" + string(rune('0'+gen))} {
			key := "chunks/" + tenant.String() + "/" + hash
			if exists, _, _ := store.Head(context.Background(), key); !exists {
				t.Errorf("gen-%d's %q chunk object was wrongly swept", gen, hash)
			}
		}
	}

	// --- BONUS: restore-oracle parity ---------------------------------------
	// reachableChunks is the SAME oracle GC and PlanRestore share. Clean up
	// the lingering failed gen-4 DUPLICATE (a separate, cosmetic anomaly:
	// PlanRestore's own strict chain-integrity CHECK 1 correctly refuses to
	// restore over ANY duplicate-generation layout, by design, regardless of
	// this fix) so the chain is a clean 0..4 with one row per generation, then
	// prove a full restore plan for gen-4 still resolves every chunk.
	if err := svc.DeleteSnapshotForUser(context.Background(), tenant, gen4DupID); err != nil {
		t.Fatalf("DeleteSnapshotForUser(gen4 failed duplicate): %v", err)
	}
	plan, _, _, err := svc.PlanRestore(context.Background(), tenant, genIDs[4],
		backup.RestoreSelection{Full: true}, "restore-gh168", "")
	if err != nil {
		t.Fatalf("PlanRestore(gen4) after cleanup: unexpected error: %v", err)
	}
	gotHashes := map[string]bool{}
	for _, e := range plan.Manifest.Entries {
		for _, c := range e.Chunks {
			gotHashes[c.Hash] = true
			if c.URL == "" {
				t.Errorf("restore chunk %q missing a presigned GET URL", c.Hash)
			}
		}
	}
	for gen := 0; gen <= 4; gen++ {
		part := "part-gen" + string(rune('0'+gen))
		if !gotHashes[part] {
			t.Errorf("restore plan missing gen-%d's part chunk %q — GC/restore reachability disagree", gen, part)
		}
	}
}

// TestRetentionGC_GH168_GenuineOrphanStillSwept is the adversarial (T5-class)
// counterpart, run against the SAME real-Postgres path: a chunk that belongs
// ONLY to a snapshot that genuinely expires (age-based retention, no
// duplicate-generation entanglement) must still be reclaimed even with the
// P2 ground-truth guard fully wired in. Proves the GH #168 hardening did not
// turn the retention GC into a no-op / storage leak. (TestRetentionGC in
// backup_integration_test.go already covers this exact property against the
// same real-Postgres path; this is a GH #168-scoped restatement kept
// alongside the duplicate-generation regression test above for traceability.)
func TestRetentionGC_GH168_GenuineOrphanStillSwept(t *testing.T) {
	pool := startPostgres(t)
	store := startBlobstore(t)
	tenant := seedTenant(t, pool, "gh168-orphan")
	siteID := seedSite(t, pool, tenant, "https://gh168-orphan.example.com")

	// MonthlyArchiveKeep=0 isolates the rolling-window prune from the monthly-
	// archive rule (mirrors TestRetentionGC in backup_integration_test.go) —
	// otherwise the 60d-old snapshot below would be flagged as the newest
	// backup in its calendar month and exempted from pruning, which is a
	// different rule than the one under test here.
	svc := backup.NewService(backup.NewRepo(pool), stubSiteLookup{info: enrolledSiteInfo(siteID, "https://gh168-orphan.example.com")}, &stubEnqueuer{}, store, domain.SystemClock{}, backup.Config{
		PresignTTL:         10 * time.Minute,
		RetentionDays:      30,
		MonthlyArchiveKeep: 0,
	})

	h := chunkHashes(3)
	putChunkObjects(t, store, tenant, h)

	// Old snapshot (60d, > the default 30d retention window) references
	// h[0], h[1], h[2] — a genuine, non-chained full backup with no
	// duplicate-generation entanglement whatsoever.
	oldSnap := seedBackupSnapshotAt(t, pool, tenant, siteID, time.Now().Add(-60*24*time.Hour))
	submitManifest(t, svc, tenant, oldSnap, []agentcmd.ManifestEntry{
		{Path: "old.php", EntryKind: "file", Size: 12, Chunks: refs(h[0], h[1], h[2])},
	})
	// Recent, retained snapshot references h[2] only (shared with the old one).
	newSnap, err := svc.CreateBackup(context.Background(), tenant, siteID, uuid.Nil, "files")
	if err != nil {
		t.Fatalf("create new snapshot: %v", err)
	}
	submitManifest(t, svc, tenant, newSnap.ID, []agentcmd.ManifestEntry{
		{Path: "new.php", EntryKind: "file", Size: 4, Chunks: refs(h[2])},
	})

	snaps, chunks, err := svc.RunRetentionGC(context.Background(), tenant)
	if err != nil {
		t.Fatalf("RunRetentionGC: %v", err)
	}
	if snaps != 1 {
		t.Fatalf("snapshots deleted = %d, want 1 (the genuinely expired old snapshot)", snaps)
	}
	if chunks != 2 {
		t.Fatalf("chunks deleted = %d, want 2 (h0,h1 genuinely orphaned; h2 shared+retained) — the P2 guard must not leak storage", chunks)
	}
	for _, gone := range []string{h[0], h[1]} {
		if exists, _, _ := store.Head(context.Background(), "chunks/"+tenant.String()+"/"+gone); exists {
			t.Errorf("genuinely orphaned chunk %s still in S3 — GH #168 hardening must not turn GC into a no-op", gone)
		}
	}
	if exists, _, _ := store.Head(context.Background(), "chunks/"+tenant.String()+"/"+h[2]); !exists {
		t.Error("shared, still-referenced chunk h2 was wrongly deleted from S3")
	}
}
