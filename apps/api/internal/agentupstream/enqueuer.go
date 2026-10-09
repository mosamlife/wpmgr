package agentupstream

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentmirror"
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

// RequestClockStore is the persisted request clock as EnqueueBootCheck uses it:
// read, and lowered when it is ahead of the clock that reads it. Satisfied by
// *agentmirror.Repo; an interface so a test can supply the state without a
// database.
type RequestClockStore interface {
	StateLoader
	// ClampLastRequestAt lowers the stored request time to ceiling when it is
	// later than ceiling, and reports whether it changed anything.
	ClampLastRequestAt(ctx context.Context, ceiling time.Time) (bool, error)
}

var _ RequestClockStore = (*agentmirror.Repo)(nil)

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
	// RequestTimeLowered is true when the persisted request time was ahead of
	// this process's clock and was lowered to it. Worth a log line: it means a
	// clock somewhere on this install is, or was, wrong.
	RequestTimeLowered bool
	// LowerErr is set when the persisted request time was ahead of this
	// process's clock and could not be lowered. The check is queued regardless,
	// but the request spacing may then refuse it when it runs.
	LowerErr error
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
// A persisted request time AHEAD of now can only be a skewed clock on whichever
// host recorded it. It is read as now, which delays this check by one window at
// most, and it is lowered to now in storage as well. The second half is what
// keeps the schedule honest: the job reads the stored value again when it runs,
// and a value still ahead of that later clock would be read as that later now,
// so the check would be refused for the very spacing window it was scheduled to
// wait out. Lowered once here, the job reads the value the schedule was
// computed from. A value at or before now is never written.
//
// A failure to read the persisted state is not a reason to skip the check; it
// is scheduled as if no request had been recorded, and the run-time guard still
// applies. Nor is a failure to lower it (BootCheck.LowerErr). now is a
// parameter so a test can pin it; production passes time.Now().
func EnqueueBootCheck(ctx context.Context, ins JobInserter, state RequestClockStore, now time.Time) (BootCheck, error) {
	if ins == nil {
		return BootCheck{}, fmt.Errorf("enqueue agent release mirror boot check: no job inserter")
	}

	var out BootCheck
	start := now
	if state != nil {
		if st, err := state.Load(ctx); err == nil && st.LastRequestAt != nil {
			last := *st.LastRequestAt
			if last.After(now) {
				last = now
				out.RequestTimeLowered, out.LowerErr = state.ClampLastRequestAt(ctx, now)
			}
			if open := last.Add(minRequestSpacing); open.After(now) {
				start, out.Deferred = open, true
			}
		}
	}

	at := start.Add(bootDelay())
	res, err := ins.Insert(ctx, MirrorArgs{Trigger: TriggerPeriodic, Boot: true}, BootInsertOpts(at))
	if err != nil {
		return BootCheck{}, fmt.Errorf("enqueue agent release mirror boot check: %w", err)
	}
	out.ScheduledAt = at
	out.Queued = res == nil || !res.UniqueSkippedAsDuplicate
	return out, nil
}
