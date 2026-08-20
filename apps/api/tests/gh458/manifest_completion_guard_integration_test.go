// This file proves the one part of the GH #458 fix that the sibling
// snapshot_state_guards_integration_test.go does not reach: the three
// RecordManifest/RecordIncrementalManifest call sites in
// internal/backup/repo.go that used to detect a rejected
// CompleteBackupSnapshot via errors.Is(err, pgx.ErrNoRows). That branch is
// unreachable now that CompleteBackupSnapshot is :execrows (Exec never
// returns pgx.ErrNoRows), so before the fix a late manifest submit against a
// cancelled or completed snapshot returned SUCCESS having written nothing.
//
// Unlike the sibling file, which drives the GENERATED sqlc queries directly,
// this file goes through backup.NewRepo(...).RecordManifest — the real
// production repo — because the bug lived in repo.go's error-detection
// branch, not in the generated query.
package gh458

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// countManifestEntries reports how many backup_manifest_entries rows exist
// for the given snapshot, read through the app pool's tenant transaction so
// RLS applies just as it would for a real request.
func (f *fixture) countManifestEntries(t *testing.T, snapshotID uuid.UUID) int64 {
	t.Helper()
	ctx := context.Background()
	var n int64
	err := f.pool.InTenantTx(ctx, f.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM backup_manifest_entries WHERE snapshot_id = $1`,
			snapshotID).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count manifest entries: %v", err)
	}
	return n
}

// chunkExists reports whether a backup_chunks row exists for hash, read
// through the app pool's tenant transaction so RLS applies.
func (f *fixture) chunkExists(t *testing.T, hash string) bool {
	t.Helper()
	ctx := context.Background()
	var n int64
	err := f.pool.InTenantTx(ctx, f.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM backup_chunks WHERE tenant_id = $1 AND blake3 = $2`,
			f.tenant, hash).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count backup_chunks for %q: %v", hash, err)
	}
	return n > 0
}

// chunkRefcount reads the current refcount for hash. The caller must already
// know the row exists.
func (f *fixture) chunkRefcount(t *testing.T, hash string) int64 {
	t.Helper()
	ctx := context.Background()
	var n int64
	err := f.pool.InTenantTx(ctx, f.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT refcount FROM backup_chunks WHERE tenant_id = $1 AND blake3 = $2`,
			f.tenant, hash).Scan(&n)
	})
	if err != nil {
		t.Fatalf("read refcount for %q: %v", hash, err)
	}
	return n
}

// seedChunkWithRefcount upserts a backup_chunks row for hash through the
// GENERATED queries (the same UpsertBackupChunk + IncrementChunkRefcount
// RecordManifest itself calls) and returns the refcount after one increment,
// so a later submit can reference an already-stored chunk instead of only a
// brand-new one.
func (f *fixture) seedChunkWithRefcount(t *testing.T, hash string) int64 {
	t.Helper()
	ctx := context.Background()
	var refcount int64
	err := f.pool.InTenantTx(ctx, f.tenant, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := q.UpsertBackupChunk(ctx, sqlc.UpsertBackupChunkParams{
			TenantID: f.tenant, Blake3: hash, S3Key: "gh458/" + hash, Size: 10,
		}); err != nil {
			return err
		}
		chunk, err := q.IncrementChunkRefcount(ctx, sqlc.IncrementChunkRefcountParams{TenantID: f.tenant, Blake3: hash})
		if err != nil {
			return err
		}
		refcount = chunk.Refcount
		return nil
	})
	if err != nil {
		t.Fatalf("seed chunk %q: %v", hash, err)
	}
	return refcount
}

// manifestInput builds a single-entry, single-chunk RecordManifestInput
// against snapshotID, distinct per call via a fresh chunk hash/path so
// re-runs across subtests never collide on the content-addressed chunk key.
func manifestInput(tenantID, snapshotID uuid.UUID) backup.RecordManifestInput {
	hash := "blake3-" + uuid.NewString()
	return backup.RecordManifestInput{
		TenantID:   tenantID,
		SnapshotID: snapshotID,
		Entries: []backup.ManifestEntryInput{
			{
				Path:        "wp-content/uploads/gh458-" + uuid.NewString() + ".txt",
				EntryKind:   "file",
				ChunkHashes: []string{hash},
				Size:        123,
				Mode:        0o644,
			},
		},
		Chunks: map[string]backup.ChunkUpload{
			hash: {Blake3: hash, Size: 123, S3Key: "gh458/" + hash},
		},
	}
}

// TestGH458RecordManifestRejectsLateSubmit proves the RecordManifest call
// site: a manifest submitted against a snapshot that is no longer
// pending/running is rejected as backup_snapshot_not_found, and — the half
// that matters — nothing from that submit is persisted. A twin proves the
// guard does not over-fire against the normal, still-running path.
func TestGH458RecordManifestRejectsLateSubmit(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	repo := backup.NewRepo(f.pool)

	t.Run("late_submit_against_terminal_snapshot_is_rejected_and_writes_nothing", func(t *testing.T) {
		// Drive the snapshot to a terminal state the way the cancel path does:
		// 'failed' with an operator error, matching the sibling test's cancel
		// fixture and the real CancelSnapshot flow.
		id := f.seedSnapshot(t, "failed", "cancelled by operator")
		before := f.read(t, id)

		// A pre-existing chunk this submit ALSO references, distinct from any
		// hash the fixture might reuse elsewhere. Its refcount before the
		// submit is the baseline the atomicity assertion below checks stays
		// unchanged.
		preHash := "blake3-pre-" + uuid.NewString()
		beforeRefcount := f.seedChunkWithRefcount(t, preHash)

		// A brand-new hash only THIS submit introduces, so its mere existence
		// afterwards is proof step 1 (UpsertBackupChunk) survived a rollback.
		freshHash := "blake3-fresh-" + uuid.NewString()

		in := backup.RecordManifestInput{
			TenantID:   f.tenant,
			SnapshotID: id,
			Entries: []backup.ManifestEntryInput{
				{
					Path:        "wp-content/uploads/gh458-" + uuid.NewString() + ".txt",
					EntryKind:   "file",
					ChunkHashes: []string{freshHash, preHash},
					Size:        123,
					Mode:        0o644,
				},
			},
			Chunks: map[string]backup.ChunkUpload{
				freshHash: {Blake3: freshHash, Size: 123, S3Key: "gh458/" + freshHash},
				preHash:   {Blake3: preHash, Size: 10, S3Key: "gh458/" + preHash},
			},
		}
		chunkRefs, storedCount, err := repo.RecordManifest(ctx, in)

		if err == nil {
			t.Fatalf("RecordManifest against a failed snapshot: err = nil (chunkRefs=%d, storedCount=%d), want backup_snapshot_not_found — a late submit SUCCEEDED", chunkRefs, storedCount)
		}
		derr, ok := domain.AsDomain(err)
		if !ok {
			t.Fatalf("RecordManifest against a failed snapshot: err = %v (not a domain.Error), want backup_snapshot_not_found", err)
		}
		if derr.Code != "backup_snapshot_not_found" {
			t.Fatalf("RecordManifest against a failed snapshot: code = %q, want %q", derr.Code, "backup_snapshot_not_found")
		}

		// The half that matters most: the transaction must have rolled back.
		// A test that only checks the returned error would still pass if the
		// manifest write happened and the error came from somewhere else.
		if n := f.countManifestEntries(t, id); n != 0 {
			t.Fatalf("manifest entries persisted for a rejected submit: count = %d, want 0", n)
		}
		after := f.read(t, id)
		if after.status != "failed" {
			t.Fatalf("snapshot status changed by a rejected submit: status = %q, want unchanged %q", after.status, "failed")
		}
		if after.totalSize != before.totalSize {
			t.Fatalf("snapshot total_size changed by a rejected submit: %d -> %d, want unchanged", before.totalSize, after.totalSize)
		}
		if after.chunkCount != before.chunkCount {
			t.Fatalf("snapshot chunk_count changed by a rejected submit: %d -> %d, want unchanged", before.chunkCount, after.chunkCount)
		}

		// The REJECTION MUST BE ATOMIC, not just manifest-free. RecordManifest
		// runs the chunk upsert (step 1), the manifest inserts and refcount
		// increments (step 2), and the guarded CompleteBackupSnapshot (step 3)
		// inside one InTenantTx. If a future change (e.g. a dedup optimisation
		// that upserts chunks outside the transaction, since chunks are shared
		// and content-addressed) moves step 1 out of that transaction, a
		// rejected submit would start leaving chunk rows and inflated
		// refcounts behind -- and the manifest-entries-only checks above would
		// still pass. These two assertions pin the transaction boundary itself,
		// not just its manifest-table side effect.
		if f.chunkExists(t, freshHash) {
			t.Fatalf("backup_chunks row exists for hash %q from a REJECTED submit: RecordManifest is not atomic -- the chunk upsert (step 1) escaped the transaction that the CompleteBackupSnapshot guard (step 3) rolled back, not merely that a count was wrong", freshHash)
		}
		if got := f.chunkRefcount(t, preHash); got != beforeRefcount {
			t.Fatalf("pre-existing chunk %q refcount changed by a REJECTED submit: %d -> %d, want unchanged at %d: RecordManifest is not atomic -- IncrementChunkRefcount (step 2) survived the rollback that the CompleteBackupSnapshot guard (step 3) triggered, not merely that a count was wrong", preHash, beforeRefcount, got, beforeRefcount)
		}
	})

	t.Run("submit_against_running_snapshot_still_succeeds", func(t *testing.T) {
		// Over-fire twin: without this, a guard that rejects everything would
		// also look correct against the terminal-snapshot case above.
		id := f.seedSnapshot(t, "running", "")

		in := manifestInput(f.tenant, id)
		chunkRefs, storedCount, err := repo.RecordManifest(ctx, in)
		if err != nil {
			t.Fatalf("RecordManifest against a running snapshot: err = %v, want nil", err)
		}
		if chunkRefs != 1 {
			t.Fatalf("chunkRefs = %d, want 1", chunkRefs)
		}
		if storedCount != 1 {
			t.Fatalf("storedCount = %d, want 1 (new chunk)", storedCount)
		}

		if n := f.countManifestEntries(t, id); n != 1 {
			t.Fatalf("manifest entries persisted for a successful submit: count = %d, want 1", n)
		}
		after := f.read(t, id)
		if after.status != "completed" {
			t.Fatalf("snapshot status = %q, want %q", after.status, "completed")
		}
		if after.totalSize != 123 {
			t.Fatalf("snapshot total_size = %d, want 123", after.totalSize)
		}
		if after.chunkCount != 1 {
			t.Fatalf("snapshot chunk_count = %d, want 1", after.chunkCount)
		}
	})
}

// TestGH458CancelSnapshotThroughServiceCancelsPending proves the reason the
// FailBackupSnapshot guard is "status IN ('pending','running')" rather than
// "status = 'running'": operator cancel of a still-QUEUED backup must still
// work. The sibling snapshot_state_guards_integration_test.go's
// "cancel_of_pending_still_works" subtest exercises the GENERATED
// FailBackupSnapshot query directly; this test goes one layer up, through
// backup.Service.CancelSnapshot exactly as the real cancel endpoint does, so a
// regression in CancelSnapshot's own precondition (snap.Status != StatusRunning
// && snap.Status != StatusPending) — not just the SQL guard — would also be
// caught here.
func TestGH458CancelSnapshotThroughServiceCancelsPending(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	svc := backup.NewService(backup.NewRepo(f.pool), nil, nil, nil, domain.SystemClock{}, backup.Config{})

	t.Run("cancel_of_pending_snapshot_succeeds_end_to_end", func(t *testing.T) {
		id := f.seedSnapshot(t, "pending", "")

		snap, err := svc.CancelSnapshot(ctx, f.tenant, id)
		if err != nil {
			t.Fatalf("CancelSnapshot on a PENDING snapshot: err = %v, want nil (a naive status='running' guard would reject this)", err)
		}
		if snap.Status != backup.StatusFailed {
			t.Fatalf("CancelSnapshot on a PENDING snapshot: returned status = %q, want %q", snap.Status, backup.StatusFailed)
		}

		after := f.read(t, id)
		if after.status != "failed" {
			t.Fatalf("snapshot status after cancel-of-pending = %q, want terminal %q", after.status, "failed")
		}
		if after.errMsg != "cancelled by operator" {
			t.Fatalf("snapshot error after cancel-of-pending = %q, want %q", after.errMsg, "cancelled by operator")
		}
	})
}
