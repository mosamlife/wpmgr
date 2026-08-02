package agentupstream

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// ManualCheckEnqueuer inserts an operator-requested "check now" mirror job
// (GH #322). Lives next to MirrorArgs/ManualInsertOpts so the insert options
// a caller must use stay next to the job they belong to.
type ManualCheckEnqueuer struct {
	client *river.Client[pgx.Tx]
}

// NewManualCheckEnqueuer builds a ManualCheckEnqueuer.
func NewManualCheckEnqueuer(client *river.Client[pgx.Tx]) *ManualCheckEnqueuer {
	return &ManualCheckEnqueuer{client: client}
}

// EnqueueManualMirrorCheck inserts one manual-trigger mirror job. Returns
// queued=true when a NEW job was inserted; queued=false when an identical
// manual check is already available, pending, running, scheduled or
// retryable (see ManualInsertOpts); the caller (internal/admin) must report
// that as "a check is already in flight", never as a fresh success.
func (e *ManualCheckEnqueuer) EnqueueManualMirrorCheck(ctx context.Context) (bool, error) {
	res, err := e.client.Insert(ctx, MirrorArgs{Trigger: TriggerManual}, ManualInsertOpts())
	if err != nil {
		return false, fmt.Errorf("enqueue manual agent release mirror check: %w", err)
	}
	return !res.UniqueSkippedAsDuplicate, nil
}
