package backup

// gh458_enqueue_uniqueness_test.go — GH #458 review follow-up.
//
// resumedOwnClaim's soundness rests on "a snapshot has exactly one River job".
// backupInsertOpts turns that from a convention into a River-enforced
// constraint. These tests pin the two halves of it.
//
// The over-fire twin (DifferentSnapshotsAreNotDeduped) is the one that
// matters: a uniqueness key that failed to distinguish two snapshots would
// swallow legitimate backups, which is a far worse failure than the duplicate
// job the key exists to block.

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river/rivertype"
)

// TestBackupInsertOpts_EnforcesOneJobPerSnapshot pins the constraint itself.
func TestBackupInsertOpts_EnforcesOneJobPerSnapshot(t *testing.T) {
	opts := backupInsertOpts()
	if opts == nil {
		t.Fatal("backupInsertOpts() returned nil: the backup inserts carry no uniqueness " +
			"constraint, so a second job for a live snapshot re-dispatches over its claim")
	}
	if !opts.UniqueOpts.ByArgs {
		t.Fatal("UniqueOpts.ByArgs is false: uniqueness would key on the job KIND alone, " +
			"which would dedupe every backup in the fleet against the first one")
	}
	// No ByPeriod, deliberately — a period re-opens the window for a second
	// job on the same snapshot once it elapses. See backupInsertOpts's doc.
	if opts.UniqueOpts.ByPeriod != 0 {
		t.Errorf("UniqueOpts.ByPeriod = %v, want 0: a bounded window lets a second job for "+
			"the same snapshot in once the period elapses, which is the hole this closes",
			opts.UniqueOpts.ByPeriod)
	}
	// River's default ByState must stay in force: it covers running, which is
	// the state a snapshot is in for the whole window resumedOwnClaim cares
	// about. An explicit ByState that dropped running would make the
	// constraint inert exactly when it is needed.
	if opts.UniqueOpts.ByState != nil {
		var hasRunning bool
		for _, s := range opts.UniqueOpts.ByState {
			if s == rivertype.JobStateRunning {
				hasRunning = true
			}
		}
		if !hasRunning {
			t.Error("UniqueOpts.ByState is set but omits running: the constraint would not " +
				"bind while a backup is actually in flight")
		}
	}
}

// TestBackupInsertOpts_DifferentSnapshotsAreNotDeduped is the over-fire twin,
// and the more important of the pair.
//
// ByArgs keys on the ENCODED args, so the property that keeps two unrelated
// backups from colliding is that BackupArgs carries the snapshot ID and
// therefore encodes differently per snapshot. Assert that on the encoding
// River hashes, for both enqueue shapes: the plain EnqueueBackup args and the
// ADR-048 chain args EnqueueBackupWithChain builds.
func TestBackupInsertOpts_DifferentSnapshotsAreNotDeduped(t *testing.T) {
	tenantID := uuid.New()
	snapA, snapB := uuid.New(), uuid.New()

	encode := func(t *testing.T, a BackupArgs) string {
		t.Helper()
		b, err := json.Marshal(a)
		if err != nil {
			t.Fatalf("marshal BackupArgs: %v", err)
		}
		return string(b)
	}

	// Shape 1: EnqueueBackup.
	plainA := encode(t, BackupArgs{TenantID: tenantID, SnapshotID: snapA})
	plainB := encode(t, BackupArgs{TenantID: tenantID, SnapshotID: snapB})
	if plainA == plainB {
		t.Fatalf("two different snapshots encode to identical args (%s): with UniqueOpts.ByArgs "+
			"the second backup would be silently swallowed as a duplicate", plainA)
	}

	// Shape 2: EnqueueBackupWithChain, same chain, different snapshots. This
	// is the one that would break if a future edit dropped SnapshotID from the
	// args in favour of the chain identifiers.
	chainID := uuid.New()
	baseID := uuid.New()
	chainArgs := func(id uuid.UUID) BackupArgs {
		return BackupArgs{
			TenantID:         tenantID,
			SnapshotID:       id,
			IsIncremental:    true,
			ParentSnapshotID: baseID,
			BaseSnapshotID:   baseID,
			ChainID:          chainID,
			Generation:       2,
		}
	}
	if a, b := encode(t, chainArgs(snapA)), encode(t, chainArgs(snapB)); a == b {
		t.Fatalf("two incremental snapshots in one chain encode to identical args (%s): "+
			"the second would be deduped away and never run", a)
	}

	// And the same snapshot must encode identically across calls, or the
	// constraint never binds at all.
	if a1, a2 := plainA, encode(t, BackupArgs{TenantID: tenantID, SnapshotID: snapA}); a1 != a2 {
		t.Errorf("one snapshot encodes unstably (%s vs %s): the uniqueness key would never match", a1, a2)
	}
}

// TestBackupInsertOpts_KindIsStable guards the other input to River's
// uniqueness hash. The key is hash(kind, unique properties); an empty or
// drifting kind breaks the constraint without touching UniqueOpts at all.
func TestBackupInsertOpts_KindIsStable(t *testing.T) {
	if k := (BackupArgs{}).Kind(); k != "backup_snapshot" {
		t.Fatalf("BackupArgs.Kind() = %q, want %q: River hashes the kind into the uniqueness "+
			"key, and changing it silently orphans every already-enqueued job's key", k, "backup_snapshot")
	}
}
