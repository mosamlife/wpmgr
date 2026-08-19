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

		in := manifestInput(f.tenant, id)
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
