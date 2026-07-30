package agentupstream

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/riverqueue/river"
)

// MirrorQueue is the dedicated River queue for the agent-release mirror. It runs
// with MaxWorkers=1: one install, one upstream, one mirror at a time.
const MirrorQueue = "agent_release_mirror"

// MirrorInterval is how often the mirror job is scheduled. Six hours is far
// inside the unauthenticated GitHub API's 60-requests-per-hour-per-IP limit, and
// an install that is up to six hours behind a release it did not know about ten
// minutes ago has lost nothing.
const MirrorInterval = 6 * time.Hour

// MirrorJitter is the maximum random delay added to each scheduled run. Every
// self-hosted install in the world would otherwise fetch on the same wall-clock
// boundary derived from its own boot time, and the ones that boot together (a
// coordinated upgrade, a shared host restarting) would hit GitHub in lockstep.
const MirrorJitter = 30 * time.Minute

// mirrorUniqueWindow deduplicates accidental double-inserts (a manual trigger
// landing next to a periodic tick). It is deliberately SHORTER than
// MirrorInterval so two legitimate consecutive ticks can never fall in the same
// unique window and silently swallow a run.
const mirrorUniqueWindow = 5 * time.Hour

// mirrorMaxAttempts is 1, and stays 1.
//
// The first reason is mechanical: Work never returns an error (see Work), so
// River has nothing to retry, and stating the cap makes that explicit rather
// than leaving River's default 25 sitting behind a job that can never use it.
//
// The second reason is that a retry here could not help even if the job did
// return errors. River's backoff puts attempt 2 seconds after attempt 1, and
// Mirror.Run refuses to spend a GitHub request less than minRequestSpacing (30
// minutes) after the last one, so every quick retry would return ErrRateLimited
// without learning anything. The upstream API allows 60 requests per hour per
// IP; the retry that is actually useful is the next scheduled run.
//
// That posture was worth re-examining, because it used to have a sharp edge: a
// transient failure banked the release-document ETag on the way past, so the
// next run short-circuited on a 304 and that version was never mirrored again
// for the life of the process. With MaxAttempts=1 and no retry, "the next run
// recovers it" was simply untrue. The ETag is now committed only on a terminal
// success (see Mirror.Run), so the next scheduled run genuinely does re-examine
// the same release from scratch, and 1 attempt is the right number.
const mirrorMaxAttempts = 1

// MirrorArgs is the River payload for the agent-release mirror. It is empty
// because this job is ONE PER INSTALL, not one per site or per tenant: it mirrors
// a single public release into a single bucket.
type MirrorArgs struct{}

// Kind implements river.JobArgs. Must stay stable — changing it orphans
// in-flight jobs.
func (MirrorArgs) Kind() string { return "agent_release_mirror" }

// InsertOpts pins every mirror job to its own queue with a bounded attempt count
// and a dedupe window.
func (MirrorArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       MirrorQueue,
		MaxAttempts: mirrorMaxAttempts,
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByPeriod: mirrorUniqueWindow,
		},
	}
}

// PeriodicInsertOpts returns the per-tick insert options, with a fresh random
// delay of up to MirrorJitter applied to each insert (see MirrorJitter). Called
// once per periodic tick by cmd/wpmgr/main.go.
//
// The jitter is applied as ScheduledAt rather than as a sleep inside Work so the
// delay does not hold a worker slot.
func PeriodicInsertOpts() *river.InsertOpts {
	opts := MirrorArgs{}.InsertOpts()
	opts.ScheduledAt = time.Now().Add(jitterDelay())
	return &opts
}

// jitterDelay returns a random delay in [0, MirrorJitter).
func jitterDelay() time.Duration {
	return time.Duration(rand.Int64N(int64(MirrorJitter)))
}

// MirrorWorker runs the agent-release mirror on a schedule.
//
// It is OFF BY DEFAULT: enabled comes from the boot config snapshot
// (cfg.Update.AgentMirrorEnabled, WPMGR_UPDATE_AGENT_MIRROR_ENABLED, default
// false), exactly like the agent self-update kill switch it sits next to. The
// worker is registered whether or not the flag is set, so jobs already in the
// queue drain cleanly during a rolling redeploy either way, and the flag is
// checked at DISPATCH time, immediately before any work happens.
type MirrorWorker struct {
	river.WorkerDefaults[MirrorArgs]
	enabled bool
	mirror  *Mirror
	logger  *slog.Logger
}

// NewMirrorWorker builds the worker. mirror may be nil (object storage not
// configured); Work then no-ops. A nil logger is replaced with the default.
func NewMirrorWorker(enabled bool, mirror *Mirror, logger *slog.Logger) *MirrorWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &MirrorWorker{enabled: enabled, mirror: mirror, logger: logger}
}

// Timeout bounds the whole job. The work is one small API request, one tiny
// manifest download, one few-MB package download, and two uploads; ten minutes is
// generous enough to ride out a slow link and short enough that a wedged run
// cannot sit on the queue until the next tick.
func (w *MirrorWorker) Timeout(*river.Job[MirrorArgs]) time.Duration {
	return 10 * time.Minute
}

// Work runs one mirror cycle and ALWAYS RETURNS NIL.
//
// That is deliberate, not laziness. This job must never break boot, never break
// the dashboard, and never surface as a 500; it is not on any request path, and
// the only thing a failed run costs is that no new release was mirrored, after
// which the existing source ladder (own-bucket manifest, then newest-in-fleet,
// then none) applies unchanged. Returning an error would buy nothing except a
// retry storm against an API that allows 60 requests per hour, and River's
// backoff would keep spending that budget on an upstream that is either down or
// publishing something this control plane has already refused. The next scheduled
// run six hours later is the correct retry, and it is a fresh, fully verified
// attempt.
//
// Failures are LOUD IN THE LOG instead, and split by severity: a refusal (the
// upstream release failed verification, previous mirror left in place) logs at
// error, a degradation (GitHub unreachable, rate limited) logs at warn or debug.
func (w *MirrorWorker) Work(ctx context.Context, _ *river.Job[MirrorArgs]) error {
	if !w.enabled {
		w.logger.Debug("agent release mirror disabled; skipping (WPMGR_UPDATE_AGENT_MIRROR_ENABLED)")
		return nil
	}
	if w.mirror == nil {
		w.logger.Debug("agent release mirror not wired (object storage not configured); skipping")
		return nil
	}

	res, err := w.mirror.Run(ctx)
	switch {
	case err == nil:
		if res.Mirrored {
			w.logger.Info("agent release mirror: published upstream release into this install's storage",
				slog.String("version", res.Version))
		} else {
			w.logger.Debug("agent release mirror: nothing to do",
				slog.String("reason", res.Reason),
				slog.String("version", res.Version))
		}
	case errors.Is(err, ErrForeignChannel):
		// Not a failure of any kind: this install publishes its own agent
		// releases, so the mirror deliberately leaves that channel alone. Warn,
		// not error, because nothing is broken and nothing needs fixing unless
		// the operator actually wanted the mirror to take over.
		w.logger.Warn("agent release mirror STOOD DOWN: this install publishes its own agent releases, so the mirror will not overwrite them",
			slog.String("error", err.Error()),
			slog.String("version", res.Version))
	case errors.Is(err, ErrDowngrade):
		// The upstream release is older than what is already published here
		// (typically a yanked release). Refusing is the correct outcome, so this
		// is a warning rather than an error.
		w.logger.Warn("agent release mirror refused an upstream release that is not newer than the one already mirrored",
			slog.String("error", err.Error()),
			slog.String("version", res.Version))
	case errors.Is(err, ErrUnorderable):
		// Neither a bad release nor a downgrade as such: this run could not
		// order the two versions, so it could not show upstream is newer and
		// stood down rather than guess. Warn, because the operator has to
		// choose (clear the pointer, or allow rollback), and nothing is broken
		// meanwhile.
		w.logger.Warn("agent release mirror refused an upstream release it cannot order against the one already mirrored",
			slog.String("error", err.Error()),
			slog.String("version", res.Version))
	case errors.Is(err, ErrRefused):
		w.logger.Error("agent release mirror REFUSED the upstream release; the previous mirror is unchanged",
			slog.String("error", err.Error()),
			slog.String("version", res.Version))
	case errors.Is(err, ErrPointerUnreadable):
		w.logger.Warn("agent release mirror: the currently published pointer could not be read, so nothing was written this run",
			slog.String("error", err.Error()),
			slog.String("version", res.Version))
	case errors.Is(err, ErrRateLimited):
		w.logger.Debug("agent release mirror: rate limited, retrying on the next scheduled run",
			slog.String("error", err.Error()))
	case errors.Is(err, ErrNotConfigured):
		w.logger.Warn("agent release mirror: not configured",
			slog.String("error", err.Error()))
	case errors.Is(err, ErrUpstreamUnavailable):
		w.logger.Warn("agent release mirror: upstream unreachable, retrying on the next scheduled run",
			slog.String("error", err.Error()))
	default:
		// Anything left is a storage write failure. The previous mirror still
		// stands (the package is written before the pointer), so this degrades to
		// "no new release mirrored" like every other failure here.
		w.logger.Error("agent release mirror failed to write to object storage",
			slog.String("error", err.Error()),
			slog.String("version", res.Version))
	}
	return nil
}
