// Package agentmirror is the persisted freshness sentinel for the upstream
// agent-release mirror (internal/agentupstream), GH #322.
//
// The problem: on a self-hosted install with WPMGR_UPDATE_AGENT_MIRROR_ENABLED
// on, the fleet agent-version reference (agentrelease.Service.FleetRollup)
// comes from a periodic job that runs at most every six hours. Nothing about a
// mirror run was ever persisted, so between an upstream release existing and
// the mirror picking it up, every site was shown as a plain green "current"
// against a reference that was itself stale, with no signal anywhere that it
// might be behind.
//
// This package is a LEAF: it depends only on internal/db and the standard
// library, so both internal/agentupstream (which WRITES it, once per mirror
// attempt) and internal/agentrelease (which READS it, to add a freshness
// signal to GET /api/v1/fleet/agents) can import it without creating a cycle.
//
// ONE ROW PER INSTALL. The mirror fetches one public GitHub release and writes
// into one bucket; it has no tenant. See migrations/*_m109_agent_mirror_state.sql
// for the full placement rationale (including why the table carries no RLS).
package agentmirror

import "time"

// Outcome is the result of one mirror attempt, in the ONLY vocabulary this
// package or its callers should use to describe one. Nine values, in three
// groups:
//
//   - SUCCESS (IsSuccess() true): this install CONFIRMED what upstream
//     publishes, one way or another. OutcomeMirrored, OutcomeCurrent,
//     OutcomeUnchanged.
//   - EXPECTED NON-SUCCESS: nothing was learned, but nothing is wrong either.
//     OutcomeRateLimited (never an alarm, see agentupstream.ErrRateLimited),
//     OutcomeRefused (upstream was reached and deliberately not published,
//     because it is not newer, or unorderable), OutcomeForeignChannel (this install
//     publishes its own agent releases; the mirror is correctly standing
//     down, permanently, and that is not a fault to report).
//   - REAL FAILURE: something was tried and broke. OutcomeUpstreamUnavailable,
//     OutcomeStorageError, OutcomeNotConfigured.
type Outcome string

const (
	// OutcomeMirrored: a new release was verified end to end and published
	// into this install's own storage.
	OutcomeMirrored Outcome = "mirrored"
	// OutcomeCurrent: upstream was examined and this install already
	// publishes exactly that release (no bytes moved).
	OutcomeCurrent Outcome = "current"
	// OutcomeUnchanged: upstream answered 304 (Not Modified). This counts as
	// a genuine confirmation, not merely an attempt: the conditional request
	// is only replayed while the locally-published pointer still matches the
	// fingerprint the ETag was banked against (see agentupstream.Mirror.Run),
	// so a 304 is a true statement about BOTH halves of the publish decision,
	// not only about upstream.
	OutcomeUnchanged Outcome = "unchanged"
	// OutcomeRateLimited: either this process's own request-spacing guard
	// refused to spend another upstream request yet, or GitHub itself
	// rate-limited the request. NEVER a failure: quiet, expected, and must
	// never be presented to an operator as an error.
	OutcomeRateLimited Outcome = "rate_limited"
	// OutcomeRefused: upstream was reached and verified, and the release was
	// deliberately NOT published: it is not strictly newer than what is
	// already mirrored, or the two versions cannot be ordered at all.
	OutcomeRefused Outcome = "refused"
	// OutcomeForeignChannel: the pointer this install currently publishes was
	// not written by this mirror, so this install publishes its own agent
	// releases and the mirror stands down permanently. Not a fault.
	OutcomeForeignChannel Outcome = "foreign_channel"
	// OutcomeUpstreamUnavailable: GitHub could not be reached, or answered
	// with a status this job cannot use. A real failure, but external.
	OutcomeUpstreamUnavailable Outcome = "upstream_unavailable"
	// OutcomeStorageError: this install's OWN object storage could not be
	// read or written. A real failure, and this install's to fix.
	OutcomeStorageError Outcome = "storage_error"
	// OutcomeNotConfigured: the mirror is enabled but cannot run at all
	// (object storage or the HTTP client is not wired, or owner/repo is not
	// usable). Never self-heals; an operator must act.
	OutcomeNotConfigured Outcome = "not_configured"
)

// IsSuccess reports whether this outcome means the install CONFIRMED what
// upstream publishes (whether or not anything new was mirrored).
func (o Outcome) IsSuccess() bool {
	switch o {
	case OutcomeMirrored, OutcomeCurrent, OutcomeUnchanged:
		return true
	default:
		return false
	}
}

// Trigger names what caused a mirror attempt.
type Trigger string

const (
	// TriggerPeriodic: the scheduled River periodic job (every
	// agentupstream.MirrorInterval, plus jitter).
	TriggerPeriodic Trigger = "periodic"
	// TriggerManual: an operator-requested check-now (superadmin,
	// POST /api/v1/admin/agent-mirror/check).
	TriggerManual Trigger = "manual"
)

// Status is the single server-computed roll-up GET /api/v1/fleet/agents
// exposes, so the staleness threshold and every other judgement call have
// exactly one definition shared by every caller.
type Status string

const (
	// StatusDisabled: this install does not run the mirror at all
	// (WPMGR_UPDATE_AGENT_MIRROR_ENABLED is false: the default, and the
	// hosted service's setting). Every other field is meaningless here.
	StatusDisabled Status = "disabled"
	// StatusPending: the mirror is enabled but has never attempted a run yet.
	StatusPending Status = "pending"
	// StatusOK: the mirror is enabled and confirmed against upstream within
	// StalenessThreshold.
	StatusOK Status = "ok"
	// StatusStale: the mirror is enabled, but nothing has confirmed against
	// upstream within StalenessThreshold (or ever). A newer agent release may
	// exist that this install has not seen.
	StatusStale Status = "stale"
	// StatusStandingDown: this install publishes its own agent releases and
	// the mirror is correctly, permanently refusing to overwrite that
	// channel. Informational, never a warning.
	StatusStandingDown Status = "standing_down"
	// StatusMisconfigured: the mirror is enabled but cannot run at all. Never
	// self-heals; warns immediately regardless of how recently it was tried.
	StatusMisconfigured Status = "misconfigured"
)

// StalenessThreshold is how long a mirror may go without CONFIRMING against
// upstream before GET /api/v1/fleet/agents reports StatusStale.
//
// 13 hours, not 6 (the schedule) and not 7. The reasoning, grounded in the
// actual schedule and in how River's periodic-job scheduler behaves on
// restart (internal/maintenance/periodic_job_enqueuer.go: a periodic job
// added without a durable ID, which is how agentupstream.PeriodicInsertOpts
// is registered, recomputes its next run as `now + interval` on every
// restart/leader-election, rather than resuming wherever the previous
// process's clock had reached):
//
//   - The schedule is agentupstream.MirrorInterval (6h) plus up to
//     agentupstream.MirrorJitter (30m), so 6h30m between successes is
//     completely normal. Warning at 6h would fire on almost every cycle.
//   - A control-plane restart landing shortly before a due tick resets the
//     "next run" clock to `restart-time + 6h`, so the gap since the LAST
//     success can legitimately stretch to nearly 6h30m (time already elapsed
//     before the restart) + 6h30m (the new cycle) ~= 12h30m, with nothing
//     actually wrong. A 7h threshold would fire on ordinary deploy days.
//   - 13h is two nominal cycles (2 x 6h30m) and just clears that
//     restart-stretched gap. Reaching it means at least two consecutive
//     scheduled runs failed to confirm anything against upstream, which is
//     evidence of a persistent problem rather than a blip.
const StalenessThreshold = 13 * time.Hour

// State is the persisted mirror sentinel, at the domain layer (DTO-agnostic;
// callers map this to their own wire shape). Every pointer field is nil when
// that fact has never been recorded: the singleton row always exists (the
// migration seeds it), but a fresh install has attempted nothing yet.
type State struct {
	// LastRequestAt is the wall-clock time this install last issued an ACTUAL
	// upstream HTTP request (any status code: 200, 304, 403, 429). This is
	// the persisted, cross-replica, cross-restart form of the same
	// request-spacing clock agentupstream.Mirror keeps in memory per
	// process (see agentupstream.MinRequestSpacing). Internal state; never
	// rendered directly, only used to compute an honest 429 for a manual
	// check request.
	LastRequestAt *time.Time

	// LastAttemptAt/LastAttemptOutcome/LastAttemptDetail/LastAttemptTrigger
	// describe the LAST mirror run that actually executed, whatever its
	// result. Never conflate this with LastSuccessAt: a run that failed ten
	// minutes ago must never be reported as "checked ten minutes ago".
	LastAttemptAt      *time.Time
	LastAttemptOutcome Outcome
	// LastAttemptDetail is a short, CURATED, non-secret reason, composed by
	// the application, never a raw wrapped error (which could carry a
	// presigned URL's signature from a storage-layer failure). Capped at 200
	// characters by the repo layer.
	LastAttemptDetail  string
	LastAttemptTrigger Trigger

	// LastSuccessAt/LastSuccessOutcome/LastSuccessVersion describe the LAST
	// time this install CONFIRMED what upstream publishes (Outcome.IsSuccess
	// true: mirrored, current, or unchanged). This is the ONLY timestamp an
	// operator-facing "checked N ago" age may ever be computed from.
	LastSuccessAt      *time.Time
	LastSuccessOutcome Outcome
	// LastSuccessVersion is not overwritten on an "unchanged" (304) success,
	// because a 304 carries no body: it naturally carries forward the
	// version named by the last full examination, which is exactly correct.
	LastSuccessVersion string

	// LastMirroredAt/LastMirroredVersion record the last time a NEW release
	// was actually published into this install's storage, as distinct from
	// merely confirming the existing one. "Published 0.61.112 two days ago,
	// confirmed against upstream 12 minutes ago" is the complete honest
	// sentence; folding this into LastSuccessAt would erase the publish event
	// on every later confirming-but-not-mirroring run.
	LastMirroredAt      *time.Time
	LastMirroredVersion string
}

// Status derives the single server-computed roll-up for this state. enabled
// must be the SAME cfg.Update.AgentMirrorEnabled value the mirror worker
// itself gates dispatch on. now is passed explicitly so this stays a pure,
// easily-tested function.
//
// Evaluated in this order, first match wins:
//  1. disabled: mirroring is off entirely.
//  2. misconfigured: the last attempt could not even run (never
//     self-heals, so this warns immediately, before any staleness check).
//  3. standing_down: the last attempt found a foreign channel (correct,
//     permanent, and not a fault, so it must never be reported as a warning).
//  4. pending: enabled, but no attempt has ever run.
//  5. stale: enabled and has attempted, but has never confirmed
//     against upstream, or the last confirmation is older than
//     StalenessThreshold.
//  6. ok: confirmed against upstream within the threshold.
func (s State) Status(enabled bool, now time.Time) Status {
	if !enabled {
		return StatusDisabled
	}
	if s.LastAttemptOutcome == OutcomeNotConfigured {
		return StatusMisconfigured
	}
	if s.LastAttemptOutcome == OutcomeForeignChannel {
		return StatusStandingDown
	}
	if s.LastAttemptAt == nil {
		return StatusPending
	}
	if s.LastSuccessAt == nil || now.Sub(*s.LastSuccessAt) >= StalenessThreshold {
		return StatusStale
	}
	return StatusOK
}
