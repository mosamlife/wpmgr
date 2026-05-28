package backup

import (
	"context"
	"fmt"

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

// EnqueueRestore inserts one restore job carrying the (possibly partial)
// selection.
func (e *RiverEnqueuer) EnqueueRestore(ctx context.Context, tenantID, snapshotID uuid.UUID, sel RestoreSelection) error {
	args := RestoreArgs{
		TenantID:   tenantID,
		SnapshotID: snapshotID,
		Full:       sel.Full,
		Paths:      sel.Paths,
		DBTables:   sel.DBTables,
	}
	if _, err := e.client.Insert(ctx, args, nil); err != nil {
		return fmt.Errorf("enqueue restore: %w", err)
	}
	return nil
}
