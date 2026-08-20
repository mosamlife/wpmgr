// PR #497 review finding on the GH #458 follow-up: backupInsertOpts (see
// internal/backup/enqueuer.go) sets UniqueOpts{ByArgs: true} on both backup
// enqueue paths to enforce one River job per snapshot. Before
// BackupArgs.SnapshotID carried the `river:"unique"` struct tag, ByArgs
// hashed the WHOLE encoded args blob, and EnqueueBackup and
// EnqueueBackupWithChain do not encode identical payloads for the same
// snapshot: EnqueueBackupWithChain also sets is_incremental and generation,
// which are `omitempty` and therefore dropped from the JSON only when zero. A
// FULL snapshot's two paths hashed the same by coincidence; an INCREMENTAL
// snapshot's did not, so the constraint silently failed to bind for exactly
// the case a re-enqueue would hit.
//
// This proves, against a real Postgres with River's own schema migrated and
// the client running on the app pool (the wpmgr_app role, same as
// production): (1) the SAME incremental snapshot, enqueued through both
// payload shapes, collides — River reports the second insert as
// UniqueSkippedAsDuplicate — and (2) a DIFFERENT incremental snapshot is not
// swallowed by the first one's key.
//
// It calls river.Client.Insert directly, with BackupArgs built the same way
// EnqueueBackup and EnqueueBackupWithChain build them (internal/backup/
// enqueuer.go), because RiverEnqueuer's own wrapper methods discard the
// *rivertype.JobInsertResult and return only an error — a skipped-as-duplicate
// insert is NOT an error, so the wrapper's return value can't distinguish the
// two outcomes. The InsertOpts here is the literal value backupInsertOpts()
// returns (UniqueOpts{ByArgs: true}, no ByPeriod/ByQueue/ByState); it isn't
// called directly because it is unexported.
package gh458

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
)

func TestGH497UniqueKeySpansBothEnqueuePaths(t *testing.T) {
	ctx := context.Background()
	appPool, adminPool := startPostgres(t)

	// Migrate River's own schema as the owner (mirrors cmd/wpmgr/main.go's
	// migrateRiver), then grant the app role access — production applies the
	// grant via the migration owner's ALTER DEFAULT PRIVILEGES; here the
	// container superuser created the tables, so grant explicitly (mirrors
	// tests.TestRiverWiringAndHealthJob).
	migrator, err := rivermigrate.New(riverpgxv5.New(adminPool.Pool), nil)
	if err != nil {
		t.Fatalf("river migrator: %v", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatalf("river migrate: %v", err)
	}
	if _, err := adminPool.Exec(ctx, "GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO wpmgr_app"); err != nil {
		t.Fatalf("grant river tables: %v", err)
	}

	// Insert-only client on the app pool: no Queues/Workers, never Started —
	// River validates Insert without the unknown-kind check in that mode.
	client, err := river.NewClient(riverpgxv5.New(appPool.Pool), &river.Config{})
	if err != nil {
		t.Fatalf("river client: %v", err)
	}

	opts := &river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true}} // == backupInsertOpts()

	tenantID := uuid.New()

	// --- (1) the point: the SAME incremental snapshot through both payload
	// shapes must collide.
	dupSnapshot := uuid.New()

	// Shape 1: EnqueueBackup's args.
	res1, err := client.Insert(ctx, backup.BackupArgs{TenantID: tenantID, SnapshotID: dupSnapshot}, opts)
	if err != nil {
		t.Fatalf("insert (plain-path args, first): %v", err)
	}
	if res1.UniqueSkippedAsDuplicate {
		t.Fatalf("first insert for a fresh snapshot was skipped as a duplicate: no prior job for it exists")
	}

	// Shape 2: EnqueueBackupWithChain's args, same snapshot ID, genuinely
	// incremental (nonzero is_incremental/generation/chain fields) — the
	// shape that diverged from shape 1 before the SnapshotID tag existed.
	res2, err := client.Insert(ctx, backup.BackupArgs{
		TenantID:         tenantID,
		SnapshotID:       dupSnapshot,
		IsIncremental:    true,
		ParentSnapshotID: uuid.New(),
		BaseSnapshotID:   uuid.New(),
		ChainID:          uuid.New(),
		Generation:       2,
	}, opts)
	if err != nil {
		t.Fatalf("insert (chain-path args, same snapshot): %v", err)
	}
	if !res2.UniqueSkippedAsDuplicate {
		t.Fatalf("SAME incremental snapshot enqueued through the chain-path args was NOT skipped as " +
			"a duplicate: the uniqueness key does not span both enqueue paths for an incremental snapshot")
	}

	// --- (2) the over-fire twin: a DIFFERENT incremental snapshot, same
	// shape, must NOT collide with the first one's key.
	otherSnapshot := uuid.New()
	res3, err := client.Insert(ctx, backup.BackupArgs{
		TenantID:         tenantID,
		SnapshotID:       otherSnapshot,
		IsIncremental:    true,
		ParentSnapshotID: uuid.New(),
		BaseSnapshotID:   uuid.New(),
		ChainID:          uuid.New(),
		Generation:       2,
	}, opts)
	if err != nil {
		t.Fatalf("insert (different snapshot): %v", err)
	}
	if res3.UniqueSkippedAsDuplicate {
		t.Fatalf("a DIFFERENT snapshot was skipped as a duplicate: the uniqueness key is too coarse " +
			"and would silently swallow a legitimate backup")
	}
}
