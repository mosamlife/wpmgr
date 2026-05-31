package media

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/media/model"
)

// RiverEnqueuer inserts media_encode jobs into River. It lives in the PURE-Go
// top-level media package (NO encoder import) so BOTH the main API and the
// media-encoder process can construct it. The main API registers the
// media_encode queue with MaxWorkers=0 (insert-only); the encoder process runs
// the workers. It satisfies service.EncodeEnqueuer.
type RiverEnqueuer struct {
	client *river.Client[pgx.Tx]
}

// NewRiverEnqueuer builds an enqueuer around the River client.
func NewRiverEnqueuer(client *river.Client[pgx.Tx]) *RiverEnqueuer {
	return &RiverEnqueuer{client: client}
}

// EnqueueEncode inserts one media_encode job. The args' InsertOpts pin it to the
// MediaEncodeQueue.
func (e *RiverEnqueuer) EnqueueEncode(ctx context.Context, args model.EncodeArgs) error {
	if _, err := e.client.Insert(ctx, args, nil); err != nil {
		return fmt.Errorf("enqueue media_encode: %w", err)
	}
	return nil
}
