package agentupstream

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
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

// JobInserter is the one River client method EnqueueBootCheck needs.
// Satisfied by *river.Client[pgx.Tx]; an interface so a test can capture the
// insert without a database.
type JobInserter interface {
	Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

var _ JobInserter = (*river.Client[pgx.Tx])(nil)

// BootCheck reports what EnqueueBootCheck did.
type BootCheck struct {
	// ScheduledAt is when the boot check was asked to run.
	ScheduledAt time.Time
	// Queued is false when an earlier boot check was still outstanding, in
	// which case that one stands and nothing new was inserted.
	Queued bool
	// Deferred is true when the persisted request time pushed the check past
	// the end of the request-spacing window.
	Deferred bool
}

// EnqueueBootCheck queues one mirror check to run a few minutes after this
// process starts (GH #428). cmd/wpmgr calls it once, after River has started,
// when the mirror is enabled.
//
// The periodic job cannot do this. River schedules its first run a full
// MirrorInterval after its scheduler starts, so every restart pushed the next
// check six hours out, and a self-hoster who restarted to upgrade to a release
// carrying a new agent was not offered that agent for six hours or more.
//
// The check runs at a random BootCheckMinDelay..BootCheckMaxDelay after now,
// or, when the persisted last_request_at says an upstream request was made
// less than minRequestSpacing ago, the same delay after that window ends.
// Deferring rather than skipping matters: a restart that lands just after a
// check (a scheduled tick, or a restart moments earlier) must still be
// followed by one, or a release published in between waits a full interval.
// The spacing itself is enforced again when the job runs (MirrorWorker seeds
// the request clock from the same persisted value), so a request is never
// spent early whatever this schedule says.
//
// A failure to read the persisted state is not a reason to skip the check; it
// is scheduled as if no request had been recorded, and the run-time guard still
// applies. now is a parameter so a test can pin it; production passes
// time.Now().
func EnqueueBootCheck(ctx context.Context, ins JobInserter, state StateLoader, now time.Time) (BootCheck, error) {
	if ins == nil {
		return BootCheck{}, fmt.Errorf("enqueue agent release mirror boot check: no job inserter")
	}

	start, deferred := now, false
	if state != nil {
		if st, err := state.Load(ctx); err == nil && st.LastRequestAt != nil {
			last := *st.LastRequestAt
			if last.After(now) {
				// A future request time can only be a skewed clock. Treat it as
				// now so it delays this check by one window at most.
				last = now
			}
			if open := last.Add(minRequestSpacing); open.After(now) {
				start, deferred = open, true
			}
		}
	}

	at := start.Add(bootDelay())
	res, err := ins.Insert(ctx, MirrorArgs{Trigger: TriggerPeriodic, Boot: true}, BootInsertOpts(at))
	if err != nil {
		return BootCheck{}, fmt.Errorf("enqueue agent release mirror boot check: %w", err)
	}
	queued := res == nil || !res.UniqueSkippedAsDuplicate
	return BootCheck{ScheduledAt: at, Queued: queued, Deferred: deferred}, nil
}
