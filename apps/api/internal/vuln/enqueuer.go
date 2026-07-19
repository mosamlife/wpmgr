package vuln

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// RiverRescanEnqueuer satisfies RescanEnqueuer using a River client.
type RiverRescanEnqueuer struct {
	client *river.Client[pgx.Tx]
}

// NewRiverRescanEnqueuer builds a RiverRescanEnqueuer.
func NewRiverRescanEnqueuer(client *river.Client[pgx.Tx]) *RiverRescanEnqueuer {
	return &RiverRescanEnqueuer{client: client}
}

// EnqueueRescanSite inserts a RescanSiteArgs job into the rescan River queue.
func (e *RiverRescanEnqueuer) EnqueueRescanSite(ctx context.Context, args RescanSiteArgs) error {
	_, err := e.client.Insert(ctx, args, &river.InsertOpts{
		Queue: RescanSiteQueue,
	})
	if err != nil {
		return fmt.Errorf("enqueue vuln rescan site: %w", err)
	}
	return nil
}

// FeedRefreshEnqueuer enqueues an immediate Wordfence feed refresh job.
// Implemented by RiverFeedRefreshEnqueuer; the admin package depends on this
// interface (not the concrete type) to avoid an import cycle.
type FeedRefreshEnqueuer interface {
	EnqueueFeedRefresh(ctx context.Context) error
}

// RiverFeedRefreshEnqueuer satisfies FeedRefreshEnqueuer using a River client.
type RiverFeedRefreshEnqueuer struct {
	client *river.Client[pgx.Tx]
}

// NewRiverFeedRefreshEnqueuer builds a RiverFeedRefreshEnqueuer.
func NewRiverFeedRefreshEnqueuer(client *river.Client[pgx.Tx]) *RiverFeedRefreshEnqueuer {
	return &RiverFeedRefreshEnqueuer{client: client}
}

// EnqueueFeedRefresh inserts a FeedRefreshArgs job. The job is deduplicated by
// ByArgs so at most one is pending/running at a time; a no-op insert is returned
// when one is already queued.
func (e *RiverFeedRefreshEnqueuer) EnqueueFeedRefresh(ctx context.Context) error {
	_, err := e.client.Insert(ctx, FeedRefreshArgs{}, &river.InsertOpts{
		Queue: FeedRefreshQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByPeriod: 5 * 60 * 1_000_000_000, // 5 minutes — same-key re-trigger is idempotent
		},
	})
	if err != nil {
		return fmt.Errorf("enqueue vuln feed refresh: %w", err)
	}
	return nil
}

// AlertDispatchEnqueuer enqueues the debounced, batched vuln-alert dispatch
// job (m103, GH #247). Implemented by RiverAlertDispatchEnqueuer.
type AlertDispatchEnqueuer interface {
	EnqueueAlertDispatch(ctx context.Context) error
}

// alertDispatchDelay is the debounce settle window: RescanSiteWorker.Work
// enqueues a dispatch job scheduled this far in the future (not immediately)
// so a whole rescan wave — many per-site RescanSiteWorker jobs completing
// close together after a feed refresh — collapses into ONE dispatch instead
// of one per site.
const alertDispatchDelay = 5 * time.Minute

// alertDispatchDedupeWindow bounds how often a NEW dispatch job can be queued
// while one is already pending/scheduled/running for the SAME kind — a
// second rescan wave that starts before the first dispatch has fired must not
// pile up duplicate jobs.
const alertDispatchDedupeWindow = 10 * time.Minute

// RiverAlertDispatchEnqueuer satisfies AlertDispatchEnqueuer using a River client.
type RiverAlertDispatchEnqueuer struct {
	client *river.Client[pgx.Tx]
}

// NewRiverAlertDispatchEnqueuer builds a RiverAlertDispatchEnqueuer.
func NewRiverAlertDispatchEnqueuer(client *river.Client[pgx.Tx]) *RiverAlertDispatchEnqueuer {
	return &RiverAlertDispatchEnqueuer{client: client}
}

// EnqueueAlertDispatch inserts an AlertDispatchArgs job (empty args — the job
// is cross-tenant, enumerating every tenant with unnotified findings itself),
// scheduled alertDispatchDelay in the future and deduplicated within
// alertDispatchDedupeWindow so a whole rescan wave collapses to ONE dispatch.
func (e *RiverAlertDispatchEnqueuer) EnqueueAlertDispatch(ctx context.Context) error {
	_, err := e.client.Insert(ctx, AlertDispatchArgs{}, &river.InsertOpts{
		Queue:       AlertDispatchQueue,
		ScheduledAt: time.Now().Add(alertDispatchDelay),
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByPeriod: alertDispatchDedupeWindow,
		},
	})
	if err != nil {
		return fmt.Errorf("enqueue vuln alert dispatch: %w", err)
	}
	return nil
}
