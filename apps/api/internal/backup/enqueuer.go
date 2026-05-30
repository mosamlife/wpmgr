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

// EnqueueBackup inserts one backup job.
func (e *RiverEnqueuer) EnqueueBackup(ctx context.Context, tenantID, snapshotID uuid.UUID) error {
	if _, err := e.client.Insert(ctx, BackupArgs{TenantID: tenantID, SnapshotID: snapshotID}, nil); err != nil {
		return fmt.Errorf("enqueue backup: %w", err)
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
