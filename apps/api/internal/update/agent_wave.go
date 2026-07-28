package update

import (
	"fmt"
	"math"
	"sort"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentplugin"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentrelease"
)

// This file is the wave gate: the primary safety property of the agent
// self-update channel, and the one thing the ordinary per-task update worker
// has no equivalent of (worker.go is strictly per-task-per-site, with no
// notion of a run-wide rollout order).
//
// A fleet-wide agent upgrade is the one update whose failure mode is not
// recoverable per site: the code being replaced is the code that would report
// and repair the failure. So the rollout is staged, and each stage must PROVE
// itself, by the new code phoning home, before the next one is allowed to
// touch a site.
//
// Everything here is a pure function of the run's current task rows. There is
// no separate wave ledger to drift out of sync, no "current wave" column to
// double-advance, and no schema change: the wave a task belongs to is derived
// from its position in the run's deterministic task order, and a wave's state
// is derived from its tasks' statuses. Two workers that evaluate concurrently
// compute the SAME verdict from the same rows; the transaction-scoped advisory
// lock in the repo (see pgRepo.ClaimAgentWaveTask / EvaluateAgentRun) is what
// makes the resulting ACTION happen exactly once.

// Wave sizing. wave 0 is a single canary site; wave 1 is a small pilot; wave 2
// is everything else.
const (
	// waveCanarySize is wave 0: exactly one site. One site is the smallest
	// blast radius that can still prove the whole three-beat protocol works
	// against a real site in this fleet.
	waveCanarySize = 1

	// wavePilotFraction sizes wave 1 as a fraction of the whole run.
	wavePilotFraction = 0.05

	// wavePilotMinimum floors wave 1 at three sites. A pilot of one or two
	// cannot distinguish "the upgrade works" from "one site happened to be
	// fine": the failure-rate gate below needs a denominator.
	wavePilotMinimum = 3

	// waveFailureThreshold is the confirmation-failure rate above which a
	// completed wave halts the run. Deliberately small: this channel replaces
	// the code that would otherwise report and repair a failure, so a rate that
	// would be unremarkable for a plugin rollout is already a fleet incident
	// here. Compared with `>`, so a pilot of 3 halts on its first failure
	// (33% > 10%) while a large final wave tolerates a thin tail of sites whose
	// cron never fires.
	waveFailureThreshold = 0.10
)

// WaveRange is one wave's half-open span [Start, End) over a run's ordered
// task list.
type WaveRange struct {
	Start int
	End   int
}

// Len returns how many tasks the wave covers.
func (w WaveRange) Len() int { return w.End - w.Start }

// PlanWaves splits n ordered tasks into the canary/pilot/remainder waves.
// Empty trailing waves are dropped, so a 1-site run has exactly one wave and a
// 3-site run has two. n <= 0 yields no waves.
func PlanWaves(n int) []WaveRange {
	if n <= 0 {
		return nil
	}
	waves := make([]WaveRange, 0, 3)
	cursor := 0

	canary := waveCanarySize
	if canary > n {
		canary = n
	}
	waves = append(waves, WaveRange{Start: cursor, End: cursor + canary})
	cursor += canary
	if cursor >= n {
		return waves
	}

	pilot := int(math.Ceil(float64(n) * wavePilotFraction))
	if pilot < wavePilotMinimum {
		pilot = wavePilotMinimum
	}
	if pilot > n-cursor {
		pilot = n - cursor
	}
	waves = append(waves, WaveRange{Start: cursor, End: cursor + pilot})
	cursor += pilot
	if cursor >= n {
		return waves
	}

	waves = append(waves, WaveRange{Start: cursor, End: n})
	return waves
}

// waveOrder returns a run's agent self-update tasks in the ONE deterministic
// order the wave plan is defined over, filtering out anything that is not an
// agent task (a run cannot mix targets, see validateAgentItem, but the
// filter keeps this function total rather than trusting that invariant).
//
// The order is (created_at, id). Note that created_at is NOT a tiebreaker in
// practice: every task in a run is inserted by one transaction, and Postgres
// gives every now() in a transaction the same transaction timestamp, so all
// rows in a run share created_at exactly. The id is therefore what actually
// orders them. That is fine, the property the gate needs is a STABLE total
// order that every worker computes identically, which (created_at, id) is, // but it does mean wave 0's canary is an arbitrary site rather than a chosen
// one. Ordering by created_at alone would be worse than arbitrary: it would be
// non-deterministic, and different workers could disagree about which wave a
// task belongs to.
func waveOrder(tasks []Task) []Task {
	out := make([]Task, 0, len(tasks))
	for _, t := range tasks {
		if t.TargetType == TargetAgent {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out
}

// waveTally counts one wave's task outcomes.
type waveTally struct {
	Total int
	// InFlight is pending or running: the wave has not finished.
	InFlight int
	// Confirmed is the ONLY success: the new agent code reported its version
	// (see AgentConfirmWorker), or the site already ran the published build.
	Confirmed int
	// Failed covers every way a site did not come back: the arm command
	// failed, the agent reported a failure, or the upgrade was never confirmed
	// before its deadline.
	Failed int
	// Other is terminal-but-neither: skipped (the CP declined this site) or
	// cancelled (an earlier halt). Counted separately because it must not be
	// read as proof of anything.
	Other int
}

func tallyWave(order []Task, w WaveRange) waveTally {
	var t waveTally
	for i := w.Start; i < w.End && i < len(order); i++ {
		t.Total++
		switch order[i].Status {
		case TaskSucceeded:
			t.Confirmed++
		case TaskFailed, TaskRolledBack:
			t.Failed++
		case TaskSkipped, TaskCancelled:
			t.Other++
		default: // pending, running
			t.InFlight++
		}
	}
	return t
}

// haltReasonFor returns why a COMPLETED wave forbids the rollout continuing,
// or "" when it does not.
//
// Zero confirmations is always a halt, whatever the mix of failures and skips
// behind it. "Confirmed" is the only evidence this channel produces, so a wave
// that produced none proved nothing, and opening the next wave on the strength
// of it would be opening it on no evidence at all.
func haltReasonFor(index int, t waveTally) string {
	if t.Total == 0 {
		return ""
	}
	if t.Confirmed == 0 {
		return fmt.Sprintf("wave %d confirmed no site (%d failed, %d not attempted of %d): the upgrade proved nothing, so no further site may be touched",
			index, t.Failed, t.Other, t.Total)
	}
	if index == 0 && t.Failed > 0 {
		return fmt.Sprintf("wave 0 (the canary) failed on %d of %d site(s)", t.Failed, t.Total)
	}
	if rate := float64(t.Failed) / float64(t.Total); rate > waveFailureThreshold {
		return fmt.Sprintf("wave %d confirmation-failure rate %.0f%% (%d of %d) exceeds the %.0f%% threshold",
			index, rate*100, t.Failed, t.Total, waveFailureThreshold*100)
	}
	return ""
}

// AgentWaveState is a run's whole wave machine, derived from its task rows.
type AgentWaveState struct {
	// Order is the run's agent tasks in wave order.
	Order []Task
	// Waves is the wave plan over Order.
	Waves []WaveRange
	// Halt is true when a completed wave forbids the rollout continuing.
	Halt bool
	// HaltReason explains Halt in operator-readable terms; empty when !Halt.
	HaltReason string
	// OpenThrough is the number of waves cleared to dispatch: a task in wave
	// index i may be sent to its site iff i < OpenThrough. Wave 0 is always
	// open (nothing precedes it); each later wave opens only once every
	// preceding wave is fully terminal and passed its gate. Always 0 when Halt.
	OpenThrough int
}

// DeriveAgentWaveState computes the wave machine from the run's current task
// rows. It is deliberately a pure function with no clock, no I/O and no
// hidden state, so two workers finishing concurrently derive the identical
// verdict and the repo's advisory lock only has to make the resulting write
// happen once.
func DeriveAgentWaveState(tasks []Task) AgentWaveState {
	order := waveOrder(tasks)
	st := AgentWaveState{Order: order, Waves: PlanWaves(len(order))}
	if len(st.Waves) == 0 {
		return st
	}

	// Wave 0 has nothing before it to prove anything, so it is open on sight.
	st.OpenThrough = 1
	for i, w := range st.Waves {
		t := tallyWave(order, w)
		if t.InFlight > 0 {
			// This wave is still running: nothing after it may open, and its
			// gate cannot be judged yet.
			return st
		}
		if reason := haltReasonFor(i, t); reason != "" {
			st.Halt = true
			st.HaltReason = reason
			st.OpenThrough = 0
			return st
		}
		st.OpenThrough = i + 2
	}
	if st.OpenThrough > len(st.Waves) {
		st.OpenThrough = len(st.Waves)
	}
	return st
}

// WaveIndexOf returns the wave a task belongs to, and whether it was found in
// the run's wave order at all.
func (s AgentWaveState) WaveIndexOf(taskID uuid.UUID) (int, bool) {
	pos := -1
	for i, t := range s.Order {
		if t.ID == taskID {
			pos = i
			break
		}
	}
	if pos < 0 {
		return 0, false
	}
	for i, w := range s.Waves {
		if pos >= w.Start && pos < w.End {
			return i, true
		}
	}
	return 0, false
}

// GateOpenFor reports whether a task is cleared to dispatch right now.
func (s AgentWaveState) GateOpenFor(taskID uuid.UUID) bool {
	if s.Halt {
		return false
	}
	idx, ok := s.WaveIndexOf(taskID)
	if !ok {
		return false
	}
	return idx < s.OpenThrough
}

// DispatchableTasks returns the tasks that must be enqueued NOW: the pending
// tasks of a wave that has JUST opened, and nothing else.
//
// "Just opened" is a property of the rows, not of a remembered transition: the
// newest open wave (index OpenThrough-1) is newly open exactly when every one
// of its tasks is still pending. A task can only leave pending by being claimed
// (pgRepo.ClaimAgentWaveTask moves it pending -> running, and every terminal
// state for an agent task is reached through a claim), so an all-pending open
// wave is one that has never had a job dispatched from it. Every wave BEFORE it
// is fully terminal by construction, that is the gate's opening condition, so
// there is never a pending task in an earlier open wave to pick up here.
//
// Returning the whole open-wave pending set on EVERY terminal transition, which
// is what this used to do, is quadratic: each of a 950-site final wave's
// completions re-returned the ~950 still-pending siblings, so a fleet-wide
// rollout enqueued on the order of n^2/2 River jobs and melted the control
// plane at exactly the scale this feature exists for. Enqueueing each task once
// as its wave opens is the same dispatch set spread over the same waves, in
// linear work.
//
// The gating semantics are untouched: which tasks may run, and when, is still
// decided by GateOpenFor at claim time. Duplicate enqueues remain harmless
// (two finishers of the previous wave may both observe the same newly-opened
// wave, and the claim path only ever hands one caller the pending -> running
// transition), and a job whose wave has since shut simply snoozes.
//
// A task whose enqueue is LOST is therefore no longer re-dispatched by the next
// sibling's completion. That is deliberate: it stays pending, its wave never
// completes, and the stale-task reaper (agentStaleTaskThreshold) terminalizes
// it, which the wave gate reads as a wave that confirmed less than it should
// have and halts the run. For this channel, stopping is the safe direction.
func (s AgentWaveState) DispatchableTasks() []Task {
	if s.Halt || s.OpenThrough <= 0 || s.OpenThrough > len(s.Waves) {
		return nil
	}
	w := s.Waves[s.OpenThrough-1]

	out := make([]Task, 0, w.Len())
	for j := w.Start; j < w.End && j < len(s.Order); j++ {
		if s.Order[j].Status != TaskPending {
			// Something in this wave has already been dispatched, so this wave
			// is not newly open and its jobs already exist.
			return nil
		}
		out = append(out, s.Order[j])
	}
	return out
}

// agentSelfUpdateConfirmed decides beat 3: has the NEW code phoned home?
//
// reported is the version the site last pushed over its signed metadata
// channel (sites.agent_version). expect is the version the agent said beat 2
// would install; from is what it was running when the run was armed.
//
// The comparison is delegated to agentrelease.Classify, the same classifier
// the fleet agent-freshness dashboard uses, so "is this site on the published
// build" has exactly one definition in this codebase. Classify returns
// StatusCurrent only when BOTH versions are well-formed and reported >= the
// target, so a site reporting an empty or garbage version is never mistaken
// for a confirmed upgrade.
//
// When the agent did not tell us what it would install, the fallback is
// strictly stronger than "not equal": the reported version must be well-formed
// AND newer than what the site was running. A site that merely re-reported its
// OLD version is not a confirmation.
func agentSelfUpdateConfirmed(reported, from, expect string) bool {
	if expect != "" {
		return agentrelease.Classify(reported, expect, agentplugin.DistributionNone) == agentrelease.StatusCurrent
	}
	if from == "" {
		return false
	}
	// reported must be strictly newer than from: Classify(reported, from)
	// being Current means reported >= from, so exclude the equal case.
	if agentrelease.Classify(reported, from, agentplugin.DistributionNone) != agentrelease.StatusCurrent {
		return false
	}
	return agentrelease.Classify(from, reported, agentplugin.DistributionNone) == agentrelease.StatusOutdated
}
