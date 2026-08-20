package backup

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// RiverEnqueuer enqueues backup/restore jobs onto River. It satisfies the
// service's Enqueuer interface.
type RiverEnqueuer struct {
	client *river.Client[pgx.Tx]
}

// NewRiverEnqueuer builds an enqueuer around the River client.
func NewRiverEnqueuer(client *river.Client[pgx.Tx]) *RiverEnqueuer {
	return &RiverEnqueuer{client: client}
}

// backupInsertOpts is the InsertOpts BOTH backup enqueue paths must use.
//
// GH #458 review: BackupWorker.resumedOwnClaim treats "attempt > 1 and the row
// is still running" as proof that the running row is THIS job's own claim.
// That inference is only sound while a snapshot has exactly ONE River job. Up
// to now that held by convention — nothing re-enqueues an existing snapshot —
// which is a property of code nobody has written yet, and it fails SILENTLY if
// someone writes it: job B's attempt 2 would re-dispatch over job A's live
// claim, the log line says "resumed by the owning job" either way, and no test
// notices. UniqueOpts makes the invariant a database constraint instead, on
// the same idiom EnqueueSqlInspectLegacy already uses below.
//
// ByArgs: true — the uniqueness hash covers the encoded args. BackupArgs.
// SnapshotID carries `river:"unique"`, which narrows the hash to just that
// field, so two DIFFERENT snapshots always hash differently and both enqueue,
// regardless of what else either enqueue path sets. That is the property that
// matters most here; see TestBackupInsertOpts_DifferentSnapshotsAreNotDeduped.
//
// No ByPeriod, deliberately, and unlike EnqueueSqlInspectLegacy. A period
// re-opens exactly the window this closes: a second job for the SAME snapshot
// once the period elapses. The usual argument for a period — that the same
// logical work is legitimately re-requested later — does not apply, because
// the key here is a snapshot UUID minted per snapshot ROW. A legitimate re-run
// is a new row with a new UUID and therefore a new key; the only thing an
// unbounded window blocks is a second job for a snapshot that already has one,
// which is the thing being blocked on purpose. River's default ByState
// includes completed, so the key stays claimed until the job cleaner reaps the
// completed row.
//
// Residual, worth knowing before changing either enqueue path: BackupArgs.
// SnapshotID carries a `river:"unique"` struct tag, so the uniqueness hash
// covers ONLY the encoded snapshot_id, not the whole args blob. Before that
// tag existed, ByArgs hashed the full payload and the two enqueue paths
// disagreed for an INCREMENTAL snapshot — `omitempty` does NOT drop a zero
// uuid.UUID (it is a [16]byte, so never "empty" to encoding/json), so
// parent/base/chain serialised as zero UUIDs either way, but
// EnqueueBackupWithChain also emits is_incremental and generation, which ARE
// dropped when zero. That made a full snapshot's two paths hash the same by
// coincidence while an incremental snapshot's hashed differently, so the
// constraint held for full backups and silently didn't for incrementals. The
// tag removes the coupling: any enqueue path that sets the same SnapshotID
// collides, regardless of what else is in the args.
func backupInsertOpts() *river.InsertOpts {
	return &river.InsertOpts{
		UniqueOpts: river.UniqueOpts{ByArgs: true},
	}
}

// EnqueueBackup inserts one backup job.
func (e *RiverEnqueuer) EnqueueBackup(ctx context.Context, tenantID, snapshotID uuid.UUID) error {
	if _, err := e.client.Insert(ctx, BackupArgs{TenantID: tenantID, SnapshotID: snapshotID}, backupInsertOpts()); err != nil {
		return fmt.Errorf("enqueue backup: %w", err)
	}
	return nil
}

// EnqueueBackupWithChain inserts one backup job with ADR-048 incremental chain
// fields pre-populated from the snapshot row. For full-base snapshots all the
// incremental fields are zero/nil and the worker behaves identically to
// EnqueueBackup.
func (e *RiverEnqueuer) EnqueueBackupWithChain(ctx context.Context, snap Snapshot) error {
	args := BackupArgs{
		TenantID:      snap.TenantID,
		SnapshotID:    snap.ID,
		IsIncremental: snap.IsIncremental,
		Generation:    snap.Generation,
	}
	if snap.ParentSnapshotID != nil {
		args.ParentSnapshotID = *snap.ParentSnapshotID
	}
	if snap.BaseSnapshotID != nil {
		args.BaseSnapshotID = *snap.BaseSnapshotID
	}
	if snap.ChainID != nil {
		args.ChainID = *snap.ChainID
	}
	if _, err := e.client.Insert(ctx, args, backupInsertOpts()); err != nil {
		return fmt.Errorf("enqueue backup (incremental): %w", err)
	}
	return nil
}

// EnqueueSqlInspectLegacy inserts one M6 / Track 4 SqlInspectLegacy job.
//
// Unique opts are used so a flurry of operator-poll-driven GETs against a
// snapshot that hasn't been inspected yet don't pile up duplicate jobs in
// River (the SqlInspectLegacyArgs unique key is the snapshot ID). Older River
// versions without unique-args fall back to inserting; the worker is idempotent
// (it overwrites the same cache key) so duplicate runs are safe.
func (e *RiverEnqueuer) EnqueueSqlInspectLegacy(ctx context.Context, tenantID, snapshotID uuid.UUID) error {
	opts := &river.InsertOpts{
		Queue: SqlInspectLegacyQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByPeriod: 5 * time.Minute,
		},
	}
	if _, err := e.client.Insert(ctx, SqlInspectLegacyArgs{TenantID: tenantID, SnapshotID: snapshotID}, opts); err != nil {
		return fmt.Errorf("enqueue sql_inspect_legacy: %w", err)
	}
	return nil
}

// EnqueueRestore inserts one restore job carrying the (possibly partial)
// selection. restoreRunID is threaded through so the worker can update the
// restore_run row as it progresses. uuid.Nil is accepted gracefully.
func (e *RiverEnqueuer) EnqueueRestore(ctx context.Context, tenantID, snapshotID uuid.UUID, sel RestoreSelection, restoreRunID uuid.UUID) error {
	args := RestoreArgs{
		TenantID:     tenantID,
		SnapshotID:   snapshotID,
		Full:         sel.Full,
		Paths:        sel.Paths,
		DBTables:     sel.DBTables,
		Components:   sel.Components,
		KeepOldFiles: sel.KeepOldFiles,
		RestoreRunID: restoreRunID,
	}
	if _, err := e.client.Insert(ctx, args, nil); err != nil {
		return fmt.Errorf("enqueue restore: %w", err)
	}
	return nil
}
