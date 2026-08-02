package agentupstream

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentmirror"
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

// manualUniqueWindow collapses an accidental double-click of the manual
// "check now" action into one job. Deliberately SHORT (contrast
// mirrorUniqueWindow's 5h): a manual check is a deliberate, immediate act, not
// a scheduled tick, and jitter/dedupe reasoning that applies to the periodic
// path has no meaning here.
const manualUniqueWindow = 5 * time.Minute

// TriggerPeriodic / TriggerManual are the two values MirrorArgs.Trigger may
// hold. Derived as compile-time constants from agentmirror.Trigger's
// vocabulary (GH #322's persisted outcome record), so the River job payload
// and the persisted state can never drift apart on the spelling of these two
// words.
const (
	TriggerPeriodic = string(agentmirror.TriggerPeriodic)
	TriggerManual   = string(agentmirror.TriggerManual)
)

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
//
// GH #322 (persisting the outcome instead of failing) does not change any of
// this: MaxAttempts stays 1 for both the periodic AND the manual insert path.
const mirrorMaxAttempts = 1

// MirrorArgs is the River payload for the agent-release mirror. It carries no
// site/tenant identity because this job is ONE PER INSTALL: it mirrors a
// single public release into a single bucket.
type MirrorArgs struct {
	// Trigger distinguishes a scheduled periodic run (TriggerPeriodic) from an
	// operator-requested one (TriggerManual), GH #322.
	//
	// This exists so the two form DIFFERENT River unique keys: River's
	// ByArgs uniqueness hashes the full encoded args, so a manual check
	// enqueued with a distinct Trigger value can never be silently
	// swallowed by the periodic job's up-to-5-hour dedupe window (and a
	// scheduled tick, symmetrically, can never be blocked by a manual
	// check's own short one, see ManualInsertOpts). It is also persisted
	// (agentmirror.AttemptInput.Trigger) so the operator-facing freshness
	// line can say WHICH KIND of check last ran.
	//
	// Empty decodes as TriggerPeriodic (see MirrorWorker.Work): any job
	// enqueued before this field existed was, definitionally, a periodic
	// tick, so that is the correct default for a mid-deploy in-flight job.
	Trigger string `json:"trigger"`
}

// Kind implements river.JobArgs. Must stay stable — changing it orphans
// in-flight jobs.
func (MirrorArgs) Kind() string { return "agent_release_mirror" }

// InsertOpts pins every mirror job to its own queue with a bounded attempt
// count and the PERIODIC dedupe window. Used for the scheduled tick
// (PeriodicInsertOpts); the manual check path uses ManualInsertOpts instead,
// which is deliberately a different (and shorter) unique window, see its doc.
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

// ManualInsertOpts returns the insert options for an operator-requested
// "check now" (GH #322), distinct from the scheduled tick's InsertOpts in two
// load-bearing ways:
//
//  1. ByPeriod is manualUniqueWindow (5 minutes), not mirrorUniqueWindow (5
//     hours): a manual check is a one-off act, and a 5-hour dedupe window
//     would make a second deliberate click inside that window silently do
//     nothing for up to 5 hours.
//  2. ByState EXCLUDES rivertype.JobStateCompleted. River's own default
//     ByState (rivertype.UniqueOptsByStateDefault) INCLUDES Completed, which
//     would mean: a manual check that finished 4 hours ago still counts as
//     "the same job" for uniqueness purposes, so a second manual click would
//     be silently deduplicated against a job that already finished, and the
//     caller (which reports InsertResult.UniqueSkippedAsDuplicate as "already
//     running") would tell the operator a check is in flight when nothing is
//     running at all. Excluding Completed makes a duplicate genuinely mean
//     "already queued or running".
//
// The four states River requires ByState to always include (Available,
// Pending, Running, Scheduled, see river.UniqueOpts.ByState's own doc) are
// kept; Retryable is added since mirrorMaxAttempts never lets a job actually
// reach it, but omitting a state River doesn't strictly require is not worth
// the risk of drifting from a future River version's requirements. Discarded
// and Cancelled are excluded for the same reason as Completed: both are
// terminal, "not running" states.
func ManualInsertOpts() *river.InsertOpts {
	return &river.InsertOpts{
		Queue:       MirrorQueue,
		MaxAttempts: mirrorMaxAttempts,
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByPeriod: manualUniqueWindow,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
				rivertype.JobStateRetryable,
			},
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
	enabled  bool
	mirror   *Mirror
	recorder AttemptRecorder
	logger   *slog.Logger
}

// AttemptRecorder persists the outcome of one mirror attempt (GH #322).
// Satisfied by *agentmirror.Repo. Declared as an interface (not the concrete
// type) purely so worker_test.go can inject a fake recorder without a real
// Postgres connection.
//
// nil is safe: Work simply does not persist anything, matching this
// package's existing "must never block on persistence, must never turn into
// an error" posture, see the package doc and mirrorMaxAttempts's doc.
type AttemptRecorder interface {
	RecordAttempt(ctx context.Context, in agentmirror.AttemptInput) error
}

var _ AttemptRecorder = (*agentmirror.Repo)(nil)

// NewMirrorWorker builds the worker. mirror may be nil (object storage not
// configured); Work then no-ops except to record OutcomeNotConfigured.
// recorder may be nil (persistence not wired); Work then simply does not
// persist anything. A nil logger is replaced with the default.
func NewMirrorWorker(enabled bool, mirror *Mirror, recorder AttemptRecorder, logger *slog.Logger) *MirrorWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &MirrorWorker{enabled: enabled, mirror: mirror, recorder: recorder, logger: logger}
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
//
// GH #322: every branch below also records its outcome via w.recorder (when
// wired), so the outcome of THIS run is persisted rather than only logged,
// see internal/agentmirror. Persisting the outcome instead of making Work
// fail is deliberate and load-bearing: it is what lets this job keep
// returning nil unconditionally while still giving the operator an honest,
// durable answer to "was this checked, and did it work".
func (w *MirrorWorker) Work(ctx context.Context, job *river.Job[MirrorArgs]) error {
	if !w.enabled {
		w.logger.Debug("agent release mirror disabled; skipping (WPMGR_UPDATE_AGENT_MIRROR_ENABLED)")
		// Deliberately NOT recorded: mirroring is off entirely, and stamping
		// an attempt for a run that did nothing would be the same lie in
		// miniature this feature exists to remove.
		return nil
	}

	trigger := job.Args.Trigger
	if trigger == "" {
		trigger = TriggerPeriodic
	}

	if w.mirror == nil {
		w.logger.Debug("agent release mirror not wired (object storage not configured); skipping")
		w.record(ctx, trigger, agentmirror.OutcomeNotConfigured,
			"object storage is not configured (WPMGR_S3_*)", "", time.Time{})
		return nil
	}

	res, err := w.mirror.Run(ctx)
	// Read the request-spacing clock AFTER Run, whatever the outcome: it only
	// ever advances when an actual HTTP request completed (see
	// Mirror.LastRequestAt's doc), so this is always the correct value to
	// persist, including when this run's own local guard refused to spend a
	// request at all (in which case it is simply unchanged from before).
	lastRequestAt := w.mirror.LastRequestAt()

	var (
		outcome agentmirror.Outcome
		detail  string
	)
	switch {
	case err == nil && res.Mirrored:
		outcome = agentmirror.OutcomeMirrored
		w.logger.Info("agent release mirror: published upstream release into this install's storage",
			slog.String("version", res.Version))
	case err == nil && res.Reason == "already_current":
		outcome = agentmirror.OutcomeCurrent
		w.logger.Debug("agent release mirror: nothing to do",
			slog.String("reason", res.Reason), slog.String("version", res.Version))
	case err == nil:
		// "not_modified" (a 304), or any future no-op reason: still a
		// genuine CONFIRMATION against upstream, not merely an attempt, see
		// agentmirror.OutcomeUnchanged's doc.
		outcome = agentmirror.OutcomeUnchanged
		w.logger.Debug("agent release mirror: nothing to do",
			slog.String("reason", res.Reason), slog.String("version", res.Version))
	case errors.Is(err, ErrForeignChannel):
		// Not a failure of any kind: this install publishes its own agent
		// releases, so the mirror deliberately leaves that channel alone. Warn,
		// not error, because nothing is broken and nothing needs fixing unless
		// the operator actually wanted the mirror to take over.
		outcome = agentmirror.OutcomeForeignChannel
		detail = "this install publishes its own agent releases; agent-releases/latest.json was not written by this mirror"
		w.logger.Warn("agent release mirror STOOD DOWN: this install publishes its own agent releases, so the mirror will not overwrite them",
			slog.String("error", err.Error()),
			slog.String("version", res.Version))
	case errors.Is(err, ErrDowngrade):
		// The upstream release is older than what is already published here
		// (typically a yanked release). Refusing is the correct outcome, so this
		// is a warning rather than an error.
		outcome = agentmirror.OutcomeRefused
		detail = "the upstream release is not newer than the version already mirrored"
		w.logger.Warn("agent release mirror refused an upstream release that is not newer than the one already mirrored",
			slog.String("error", err.Error()),
			slog.String("version", res.Version))
	case errors.Is(err, ErrUnorderable):
		// Neither a bad release nor a downgrade as such: this run could not
		// order the two versions, so it could not show upstream is newer and
		// stood down rather than guess. Warn, because the operator has to
		// choose (clear the pointer, or allow rollback), and nothing is broken
		// meanwhile.
		outcome = agentmirror.OutcomeRefused
		detail = "the mirrored and upstream versions cannot be ordered against each other"
		w.logger.Warn("agent release mirror refused an upstream release it cannot order against the one already mirrored",
			slog.String("error", err.Error()),
			slog.String("version", res.Version))
	case errors.Is(err, ErrRefused):
		outcome = agentmirror.OutcomeRefused
		detail = "the upstream release failed verification"
		w.logger.Error("agent release mirror REFUSED the upstream release; the previous mirror is unchanged",
			slog.String("error", err.Error()),
			slog.String("version", res.Version))
	case errors.Is(err, ErrPointerUnreadable):
		outcome = agentmirror.OutcomeStorageError
		detail = "could not read the currently published pointer"
		w.logger.Warn("agent release mirror: the currently published pointer could not be read, so nothing was written this run",
			slog.String("error", err.Error()),
			slog.String("version", res.Version))
	case errors.Is(err, ErrRateLimited):
		// NEVER a failure, quiet, expected. Deliberately no dynamic detail
		// text parsed from err here (see agentmirror.OutcomeRateLimited).
		outcome = agentmirror.OutcomeRateLimited
		detail = "waiting for the upstream request-spacing window to pass"
		w.logger.Debug("agent release mirror: rate limited, retrying on the next scheduled run",
			slog.String("error", err.Error()))
	case errors.Is(err, ErrNotConfigured):
		outcome = agentmirror.OutcomeNotConfigured
		detail = "object storage or the HTTP client is not configured"
		w.logger.Warn("agent release mirror: not configured",
			slog.String("error", err.Error()))
	case errors.Is(err, ErrUpstreamUnavailable):
		outcome = agentmirror.OutcomeUpstreamUnavailable
		detail = "the upstream release could not be reached"
		w.logger.Warn("agent release mirror: upstream unreachable, retrying on the next scheduled run",
			slog.String("error", err.Error()))
	default:
		// Anything left is a storage write failure. The previous mirror still
		// stands (the package is written before the pointer), so this degrades to
		// "no new release mirrored" like every other failure here.
		//
		// detail is a FIXED string, never err.Error(): this default branch is
		// exactly where a wrapped blobstore error can carry a presigned URL's
		// signature, and this row is rendered on a tenant-scoped dashboard
		// endpoint.
		outcome = agentmirror.OutcomeStorageError
		detail = "writing to this install's object storage failed"
		w.logger.Error("agent release mirror failed to write to object storage",
			slog.String("error", err.Error()),
			slog.String("version", res.Version))
	}

	w.record(ctx, trigger, outcome, detail, res.Version, lastRequestAt)
	return nil
}

// record persists one attempt via w.recorder, when wired. A persistence
// failure is logged and otherwise swallowed; it must never make Work return
// an error (see Work's doc and mirrorMaxAttempts's doc).
func (w *MirrorWorker) record(ctx context.Context, trigger string, outcome agentmirror.Outcome, detail, version string, lastRequestAt time.Time) {
	if w.recorder == nil {
		return
	}
	if err := w.recorder.RecordAttempt(ctx, agentmirror.AttemptInput{
		Trigger:       agentmirror.Trigger(trigger),
		Outcome:       outcome,
		Detail:        detail,
		Version:       version,
		LastRequestAt: lastRequestAt,
	}); err != nil {
		w.logger.Error("agent release mirror: failed to persist attempt state",
			slog.String("error", err.Error()))
	}
}
