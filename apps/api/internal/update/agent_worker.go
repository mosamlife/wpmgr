package update

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentplugin"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentrelease"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Audit actions for the agent self-update channel.
const (
	// ActionRunHalted records the wave gate stopping a run. Emitted exactly
	// once per run (see AgentRunEvaluation.Changed).
	ActionRunHalted = "update.run.halted"
	// ActionAgentSelfUpdateArmed records a successful beat 1: the agent
	// verified a newer build and began applying it inside the same request.
	// It is NOT a success record, the task is still running at this point.
	ActionAgentSelfUpdateArmed = "update.agent.armed"
)

// Confirmation timing. These bound beat 2: how long the control plane waits
// for the NEW agent code to phone home before declaring the upgrade
// unconfirmed. The arm-then-apply request (beats 1 and 2) runs inline and has
// already finished, one way or another, long before either window below
// expires; both only bound how long the CP waits for the site's own
// confirmation, not whether or when the apply itself runs.
const (
	// agentConfirmPoll is how often the confirm job re-reads the site's
	// reported agent version. The signal it waits for is a push from the site
	// (a version-changed boot pushes metadata immediately), so this only
	// bounds how quickly the CP notices, not how quickly the site upgrades.
	agentConfirmPoll = 30 * time.Second

	// agentConfirmDeadline is the confirmation window when the agent reported
	// ordinary loopback cron: the site's own periodic metadata heartbeat runs
	// on WordPress's own schedule, so anything past this window with no
	// version-changed push means the apply never completed.
	agentConfirmDeadline = 20 * time.Minute

	// agentConfirmDeadlineExternalCron is the confirmation window when the
	// agent reported that loopback cron cannot run (agentcmd.CronModeExternal).
	// The site's periodic metadata heartbeat then depends on an external
	// scheduler whose period the control plane cannot see, commonly every 15
	// minutes but sometimes hourly. Using the narrow window here would declare
	// a healthy site that simply has not had its heartbeat run yet
	// "unconfirmed" and halt a rollout that was fine.
	agentConfirmDeadlineExternalCron = 90 * time.Minute

	// agentWaveGatePoll is how often a task whose wave has not opened yet
	// re-checks the gate.
	agentWaveGatePoll = 30 * time.Second
)

// AgentSelfUpdateCommander sends beat 1 (ARM) of the agent self-update
// protocol. Deliberately a DIFFERENT interface from Commander: nothing that
// can send an `update` or `rollback` command is reachable from the agent
// target's code path, and nothing on this interface can be used to update an
// ordinary plugin.
type AgentSelfUpdateCommander interface {
	AgentSelfUpdate(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.AgentSelfUpdateRequest) (agentcmd.AgentSelfUpdateResponse, error)
}

// AgentVersionLookup reads a site's last self-reported agent version
// (sites.agent_version), written by the agent's signed metadata push. This is
// the ONLY channel beat 2 trusts: it is the new code speaking for itself.
type AgentVersionLookup interface {
	AgentVersion(ctx context.Context, tenantID, siteID uuid.UUID) (string, error)
}

// AgentApplyResult is the agent's OWN account of the apply. The agent records
// it on disk as the apply finishes and replays it on its next signed metadata
// push.
//
// It is the only way a failed apply reaches the control plane at all: the apply
// releases its response before the swap begins, so nothing else carries the
// outcome back. Status is one of applied|failed|expired|
// already_applied, kept as a plain string so a status a newer agent introduces
// reaches an operator verbatim.
type AgentApplyResult struct {
	Status      string
	FromVersion string
	ToVersion   string
	Detail      string
	// At is when the agent stamped the record; the zero time when it did not say.
	At time.Time
	// ApplyID is the opaque per-apply identifier the agent stamped this record
	// with; "" for a record written by an agent that predates it, or one that
	// simply never said. Compared whole against AgentConfirmArgs.ApplyID by
	// agentApplyAttributed; never parsed as a version or a time, and never
	// compared to either.
	ApplyID string
}

// AgentApplyResultLookup reads the last apply-beat record a site reported.
// Optional throughout: an unwired lookup, a site that never ran an upgrade,
// and an agent too old to send the record all produce (zero, false, nil), and
// the channel behaves exactly as it did before the record existed.
type AgentApplyResultLookup interface {
	AgentSelfUpdateResult(ctx context.Context, tenantID, siteID uuid.UUID) (AgentApplyResult, bool, error)
}

// AgentReleaseReader reports the currently published agent version, or "" when
// it cannot be determined. Satisfied by *agentrelease.Reader.
type AgentReleaseReader interface {
	LatestVersion(ctx context.Context) string
}

// AgentConfirmEnqueuer schedules the beat-2 confirmation poll job.
type AgentConfirmEnqueuer interface {
	EnqueueAgentConfirm(ctx context.Context, args AgentConfirmArgs) error
}

// AgentSelfUpdateDeps bundles everything the agent self-update channel needs.
// Wired once at boot via Worker.SetAgentSelfUpdate; the zero value leaves the
// channel disabled, which is the shipped default.
type AgentSelfUpdateDeps struct {
	// Enabled is the fleet-wide kill switch. FALSE BY DEFAULT. While false no
	// agent self-update command is sent to any site, whatever is already
	// enqueued and whatever the release manifest says.
	Enabled bool
	// Cmd sends beat 1. A nil Cmd disables the channel exactly like Enabled
	// being false (no signing key configured means no signed command can be
	// minted, and an unsigned one must never be sent).
	Cmd AgentSelfUpdateCommander
	// Versions reads sites.agent_version for beat 2.
	Versions AgentVersionLookup
	// Waves is the run-scoped wave gate persistence.
	Waves AgentWaveRepo
	// Tasks enqueues a wave's tasks once the gate opens for it.
	Tasks Enqueuer
	// Confirms enqueues the beat-2 poll job after a successful arm.
	Confirms AgentConfirmEnqueuer
	// Releases reports the currently published agent version. It is what makes
	// an "up_to_date" answer checkable: the control plane only ever creates a
	// task for a site it classified as OUTDATED, so a site claiming to be up to
	// date means the CP and the agent's manifest disagree, and the disagreement
	// has to be resolved against the published version rather than believed.
	// A nil Releases (or an unknown published version) is fail-closed: the
	// answer confirms nothing. See Worker.upToDate.
	Releases AgentReleaseReader
	// Results reads the agent's own account of its last apply beat, used to
	// explain a confirmation TIMEOUT. Entirely optional: nil leaves the timeout
	// detail exactly as it was, and ready() deliberately does not require it, so
	// a control plane that never wires it still runs the channel.
	Results AgentApplyResultLookup
}

// ready reports whether the channel can actually dispatch.
func (d AgentSelfUpdateDeps) ready() bool {
	return d.Enabled && d.Cmd != nil && d.Versions != nil && d.Waves != nil && d.Confirms != nil
}

// SetAgentSelfUpdate wires the agent self-update channel. Call once at boot.
// Not calling it at all leaves the channel off, so a deployment that never
// opts in behaves exactly as it did before this channel existed.
func (w *Worker) SetAgentSelfUpdate(d AgentSelfUpdateDeps) { w.agent = d }

// runAgentSelfUpdate executes ONE agent self-update task: beats 1 and the
// handoff to beat 2. It is a wholly distinct branch from runApply/runDry, and
// deliberately does NOT reuse them.
//
// Two things runApply does are absent here, on purpose:
//
//   - NO post-update health probe. runApply's agent-first reachability check
//     asks the agent whether it is alive; that is precisely the signal a bad
//     self-update destroys, so a failure would be indistinguishable from the
//     upgrade being mid-flight, and a success right after ARM would only prove
//     the OLD code is still running (nothing has been applied yet at that
//     point). The signal this channel trusts instead is the new code
//     volunteering its own version, see AgentConfirmWorker.
//   - NO rollback, ever. Worker.rollback sends a signed `rollback` command to
//     the agent. If an agent self-update went wrong, the code that would
//     receive and execute that command is exactly the code that was being
//     replaced, so the command is undeliverable by construction. Attempting it
//     would add a misleading "rollback FAILED" to a task whose real problem is
//     something else entirely. The agent's own snapshot watchdog deliberately
//     does not arm for its own directory for the same reason. This branch never
//     calls w.cmd (which is what holds Rollback) at all: it uses w.agent.Cmd,
//     an interface with no rollback method on it.
func (w *Worker) runAgentSelfUpdate(ctx context.Context, args TaskArgs, task Task) error {
	// ---------------------------------------------------------------------
	// KILL SWITCH. Checked here, before the task is even claimed and long
	// before anything is sent to a site, so flipping it off stops every agent
	// self-update fleet-wide including runs already created and jobs already
	// enqueued. It has to stop DISPATCH rather than rely on the release
	// channel: repointing the published manifest at an older build does not
	// un-brick anybody, because the agent's downgrade guard refuses to install
	// anything older than what it is running.
	// ---------------------------------------------------------------------
	if !w.agent.ready() {
		return w.haltAgentRun(ctx, task,
			"the agent self-update channel is disabled on this control plane",
			"agent self-update is disabled on this control plane")
	}

	claim, claimed, err := w.agent.Waves.ClaimAgentWaveTask(ctx, args.TenantID, args.RunID, args.TaskID)
	if err != nil {
		return err
	}
	switch claim {
	case ClaimWait:
		// An earlier wave has not confirmed yet. Nothing was written and
		// nothing was sent; ask again shortly.
		return river.JobSnooze(agentWaveGatePoll)
	case ClaimAlreadyClaimed:
		// A duplicate job for a task another worker already took, or one a
		// halt already cancelled. Both are no-ops.
		return nil
	case ClaimHalted:
		w.publish(claimed, RunHalted)
		w.recordAudit(ctx, claimed)
		return nil
	}

	w.publish(claimed, RunRunning)
	w.ensureRunRunning(ctx, args.TenantID, args.RunID)

	site, err := w.sites.GetSiteInfo(ctx, args.TenantID, claimed.SiteID)
	if err != nil {
		return w.finishAgentTask(ctx, claimed, TaskFailed, claimed.FromVersion, "", "site unresolved", err.Error())
	}

	// ---- BEAT 1: ARM. Nothing on the site's disk moves in this call. ----
	resp, err := w.agent.Cmd.AgentSelfUpdate(ctx, claimed.SiteID, site.URL, agentcmd.AgentSelfUpdateRequest{})
	if err != nil {
		// An agent that predates this channel has no self-update route at all,
		// so its REST API answers 404 rest_no_route. That is not a broken site
		// and not a broken build: nothing was sent that the site could act on,
		// and nothing was applied.
		//
		// It matters most on the FIRST fleet-wide rollout of this channel, which
		// by construction targets exactly those agents. Recording it as
		// TaskFailed made the canary fail with an error an operator cannot tell
		// apart from a genuinely bad build, and made a MIXED wave (a few old
		// agents among sites that upgraded fine) trip the failure-rate gate on
		// sites that were never even eligible.
		//
		// So it is recorded non-confirming instead: skipped, which the wave
		// tally scores as neither confirmed nor failed (waveTally.Other). Note
		// this still does not let a wave that confirmed NOTHING open the next
		// one, which is the gate's whole point; it stops the run honestly
		// ("proved nothing") rather than dishonestly ("the canary failed").
		//
		// isOldAgentRouteMissing is the same predicate RefreshInventoryWorker
		// applies to the same situation (refresh.go), against the same canonical
		// agentcmd error format.
		if isOldAgentRouteMissing(err) {
			w.logger.Info("agent self-update: site's agent has no self-update route (old agent); not attempted",
				slog.String("task_id", claimed.ID.String()),
				slog.String("site_id", claimed.SiteID.String()),
				slog.String("error", err.Error()))
			return w.finishAgentTask(ctx, claimed, TaskSkipped, claimed.FromVersion, "",
				"not attempted: this site's agent predates the self-update channel and has no self-update route, "+
					"so it cannot upgrade itself and must be upgraded another way. Nothing was sent to the site and nothing was applied.", "")
		}
		return w.finishAgentTask(ctx, claimed, TaskFailed, claimed.FromVersion, "",
			"agent self-update arm command failed", err.Error())
	}

	from := fromOr(resp.FromVersion, claimed.FromVersion)
	switch resp.Status {
	case agentcmd.SelfUpdateScheduled, agentcmd.SelfUpdateAlreadyScheduled:
		// "already_scheduled" travels the SAME path as "scheduled" on purpose:
		// it carries the same to_version and expiry, and it means an EARLIER
		// arm succeeded and its upgrade is still pending. Treating it as
		// anything other than armed would fail a task whose site is doing
		// exactly what it was told, and (in wave 0) halt the whole rollout on
		// a retry, the one thing a retry must never do.
		return w.armed(ctx, args, claimed, from, resp)

	case agentcmd.SelfUpdateUpToDate:
		return w.upToDate(ctx, claimed, from, resp)

	case agentcmd.SelfUpdateNotEligible:
		// This build cannot self-update (the wordpress.org build is upgraded by
		// the directory), or the site is not enrolled. Not a site failure, so
		// it is skipped rather than failed, but it is not a confirmation
		// either, and the wave gate counts it as proving nothing.
		return w.finishAgentTask(ctx, claimed, TaskSkipped, from, "",
			detailOr(resp.Detail, "this agent build cannot self-update and is upgraded outside this channel"), "")

	case agentcmd.SelfUpdateError:
		// The agent's own words are the whole diagnostic value of this
		// outcome ("manifest signature invalid", "cron could not be
		// scheduled"). They go into BOTH the operator-facing detail and the
		// task's error, so no code path can drop them and leave an operator
		// staring at the word "error".
		return w.finishAgentTask(ctx, claimed, TaskFailed, from, resp.ToVersion,
			"the agent could not arm its self-update: "+detailOr(resp.Detail, "the agent gave no reason"),
			detailOr(resp.Detail, "agent_self_update_arm_error"))

	default:
		return w.finishAgentTask(ctx, claimed, TaskFailed, from, resp.ToVersion,
			"the agent returned an unrecognized self-update status: "+resp.Status,
			detailOr(resp.Detail, resp.Status))
	}
}

// upToDate resolves an "up_to_date" answer, which is NEVER an unqualified
// success.
//
// planAgentTasks only creates a task for a site whose REPORTED agent version is
// behind the version this control plane published AT PLAN TIME, so by the time
// this command was sent the control plane had already classified this site as
// outdated. The agent answering "my manifest offers nothing newer" therefore
// means the two disagree, which is precisely the case that must not be trusted.
//
// THE RULE: a run has a premise, "install version X", fixed when the run was
// planned and persisted on every one of its tasks (update_tasks.desired_version,
// see planAgentTasks). A confirmation must mean THE SITE REACHED X. It may never
// mean that X moved down to meet the site.
//
// That distinction is the whole fix. Scoring the answer against a LIVE read of
// the release manifest scores it against a moving target, and the manifest moves
// for a reason the design explicitly expects: an operator publishes a bad build,
// a site bricks, and the operator reverts latest.json to the last known good
// build, which is typically the very version the fleet is already running. Every
// subsequent arm then answers "up_to_date" (the agent's downgrade guard refuses
// the older manifest), every one of those answers matches the freshly-reverted
// published version, every task records succeeded, no wave fails its gate, and
// the run completes 100% green with not one agent moved. Worse, on a mixed fleet
// the wave-0 canary can itself be a site already at the reverted version: it
// "confirms" having applied nothing, and that confirmation is what authorises
// waves 1 and 2 to touch sites that genuinely are behind. A canary that applied
// nothing must never open a wave.
//
// So the answer is checked against the run's OWN target:
//
//   - The site reached the run's planned target, and its version actually moved
//     from what was recorded for it at plan time -> a genuine confirmation, and
//     the only case that may count toward a wave's evidence.
//   - Anything else, including "the site is exactly where it was at plan time
//     and now merely claims parity with a manifest that moved" -> non-confirming.
//     Recorded as TaskSkipped, which the wave tally scores as neither confirmed
//     nor failed (waveTally.Other), so a wave of these confirms nothing and the
//     gate stays shut instead of opening on a rollout that did not happen.
//
// Independently of this task's own outcome, a published version that has moved
// BELOW the run's target voids the run's premise: the version this run exists to
// install is no longer on offer, so no remaining site can possibly reach it and
// dispatching further waves would only produce more "up_to_date" answers. The
// run is halted rather than continued.
func (w *Worker) upToDate(ctx context.Context, task Task, from string, resp agentcmd.AgentSelfUpdateResponse) error {
	latest := ""
	if w.agent.Releases != nil {
		latest = w.agent.Releases.LatestVersion(ctx)
	}
	planned := task.DesiredVersion

	status, detail := TaskSkipped, upToDateNotConfirmedDetail(planned, from, latest)
	toVersion := ""
	if agentSelfUpdateReachedTarget(from, planned, task.FromVersion) {
		status, toVersion = TaskSucceeded, from
		detail = fmt.Sprintf("confirmed: the site reports %s, at or beyond %s, the version this run was planned to install (it reported %s when the run was planned)",
			from, planned, task.FromVersion)
		if resp.Detail != "" {
			detail += " (" + resp.Detail + ")"
		}
	} else {
		w.logger.Warn("agent self-update: the site did not reach the version this run was planned to install",
			slog.String("task_id", task.ID.String()),
			slog.String("site_id", task.SiteID.String()),
			slog.String("planned_version", planned),
			slog.String("planned_from_version", task.FromVersion),
			slog.String("site_version", from),
			slog.String("published_version", latest))
	}

	if agentRunPremiseVoid(planned, latest) {
		w.logger.Warn("agent self-update: the published version moved below this run's target; halting",
			slog.String("run_id", task.RunID.String()),
			slog.String("planned_version", planned),
			slog.String("published_version", latest))
		return w.haltAgentRunWith(ctx, task, status, from, toVersion, detail, "",
			fmt.Sprintf("this run was planned to install %s but this control plane now publishes %s, which is older: "+
				"the version this run exists to install is no longer offered, so no remaining site can reach it", planned, latest))
	}
	return w.finishAgentTask(ctx, task, status, from, toVersion, detail, "")
}

// agentSelfUpdateReachedTarget decides whether an "up_to_date" answer proves the
// SITE moved to the version this run set out to install.
//
// reported is the version the agent says it is running now; planned is the
// version resolved from the release manifest when the run was created and
// persisted on the task; plannedFrom is the version the site itself reported at
// that same moment.
//
// Both halves are required:
//
//   - reported >= planned: the site actually reached the run's target.
//   - reported > plannedFrom: the site's own version MOVED. For any task the
//     planner created this is implied by the first half (it only creates a task
//     for a site strictly behind the target), but it is stated independently so
//     the rule holds on its own rather than resting on that invariant, and so a
//     legacy or hand-written row cannot confirm itself by standing still.
//
// A planned target this package cannot read (an empty or literal "latest"
// desired_version, written by runs created before the target was persisted) is
// refused: an answer that cannot be checked against the run's premise proves
// nothing. Same for an unreadable version on either other side.
func agentSelfUpdateReachedTarget(reported, planned, plannedFrom string) bool {
	if !agentrelease.WellFormed(planned) || !agentrelease.WellFormed(reported) || !agentrelease.WellFormed(plannedFrom) {
		return false
	}
	if agentrelease.Classify(reported, planned, agentplugin.DistributionNone) != agentrelease.StatusCurrent {
		return false
	}
	return agentrelease.Classify(plannedFrom, reported, agentplugin.DistributionNone) == agentrelease.StatusOutdated
}

// agentRunPremiseVoid reports whether the currently published version has moved
// BELOW the version this run was planned to install, which makes the run's
// premise unsatisfiable for every site it has left.
//
// A published version that cannot be read at all is deliberately NOT treated as
// void: "the manifest is missing" is a different fact from "the manifest was
// reverted", it may be a transient read failure, and it already produces
// non-confirming outcomes that the wave gate stops on by itself. Halting the
// whole run on a blip would be the wrong direction for the one case where the
// control plane cannot see.
func agentRunPremiseVoid(planned, published string) bool {
	if !agentrelease.WellFormed(planned) || !agentrelease.WellFormed(published) {
		return false
	}
	return agentrelease.Classify(published, planned, agentplugin.DistributionNone) == agentrelease.StatusOutdated
}

// upToDateNotConfirmedDetail explains a non-confirming "up_to_date" answer in
// the three terms an operator needs to tell the two failure shapes apart: what
// this run set out to install, where the site actually is, and what this control
// plane publishes right now. A reverted manifest and a site that simply has not
// upgraded yet both produce this outcome, and only those three numbers
// distinguish them.
func upToDateNotConfirmedDetail(planned, from, published string) string {
	target := planned
	if !agentrelease.WellFormed(target) {
		target = "a version it did not record (this run predates the planned-target record)"
	}
	pub := published
	if pub == "" {
		pub = "a version it cannot read (the published release manifest is missing)"
	}
	return fmt.Sprintf("not confirmed: the agent reports it is already up to date on %s, but this run was planned to install %s "+
		"and this control plane now publishes %s. The site did not reach this run's target, so nothing was proved and nothing was "+
		"applied. A reverted or missing release manifest looks exactly like this: the published version moving is not the site moving.",
		fromOr(from, "an unreported version"), target, pub)
}

// armed handles a "scheduled" or "already_scheduled" acknowledgement. The task
// stays RUNNING: an arm ack is never success. The agent releases that ack
// BEFORE it begins the swap, precisely so the upgrade is not running against a
// connection someone is timing, which means the ack is sent while the outcome
// is still unknown and can never stand in for one. An upgrade that then fails
// is restored by WordPress from its own backup, which is safe, nothing was
// kept, but it must surface as unconfirmed rather than as a silent success.
func (w *Worker) armed(ctx context.Context, args TaskArgs, task Task, from string, resp agentcmd.AgentSelfUpdateResponse) error {
	deadline := time.Now().Add(confirmWindowFor(resp.CronMode))

	if err := w.agent.Confirms.EnqueueAgentConfirm(ctx, AgentConfirmArgs{
		TenantID:      task.TenantID,
		RunID:         task.RunID,
		TaskID:        task.ID,
		SiteID:        task.SiteID,
		FromVersion:   from,
		ExpectVersion: resp.ToVersion,
		CronMode:      resp.CronMode,
		DeadlineAt:    deadline,
		// ApplyID travels through unmodified from the beat-1 answer, on BOTH
		// the "scheduled" and "already_scheduled" paths (this function handles
		// both identically, see runAgentSelfUpdate). An agent that predates
		// this field leaves it "", which is what keeps beat 2 confirming on
		// version movement alone for that agent (see agentConfirmOutcome).
		ApplyID: resp.ApplyID,
	}); err != nil {
		// Without the confirmation job nothing would ever establish whether
		// this upgrade landed, and the task would sit running until the
		// reaper swept it. Fail it now, honestly: the agent may well apply the
		// upgrade, but this control plane can no longer prove it.
		return w.finishAgentTask(ctx, task, TaskFailed, from, resp.ToVersion,
			"the agent accepted the upgrade but the confirmation poll could not be enqueued", err.Error())
	}

	w.logger.Info("agent self-update armed",
		slog.String("task_id", task.ID.String()),
		slog.String("site_id", task.SiteID.String()),
		slog.String("arm_status", resp.Status),
		slog.String("from_version", from),
		slog.String("to_version", resp.ToVersion),
		slog.String("cron_mode", resp.CronMode),
		slog.String("apply_id", shortApplyID(resp.ApplyID)),
		slog.Time("confirm_deadline", deadline))

	if w.audit != nil {
		_, _ = w.audit.Record(ctx, audit.Event{
			TenantID:   task.TenantID,
			ActorType:  audit.ActorSystem,
			Action:     ActionAgentSelfUpdateArmed,
			TargetType: "update_task",
			TargetID:   task.ID.String(),
			Metadata: map[string]any{
				"run_id":       args.RunID.String(),
				"site_id":      task.SiteID.String(),
				"arm_status":   resp.Status,
				"from_version": from,
				"to_version":   resp.ToVersion,
				"cron_mode":    resp.CronMode,
				"deadline_at":  deadline.UTC().Format(time.RFC3339),
				// Truncated to 8 characters: enough to correlate against the
				// full id in the site's own apply record without putting a
				// whole opaque token in the audit log. A repeat of an
				// unattributed-confirmation incident is diagnosable from this
				// log alone, see agentApplyAttributed.
				"apply_id": shortApplyID(resp.ApplyID),
			},
		})
	}
	return nil
}

// confirmWindowFor picks the beat-2 deadline from the cron mode the agent
// reported. The wire carries exactly two modes; anything else (an omitted
// field, or a value this control plane does not know) gets the NARROW window,
// because widening on an unrecognized value would let a typo silently buy an
// hour and a half of false patience.
func confirmWindowFor(cronMode string) time.Duration {
	switch cronMode {
	case agentcmd.CronModeExternal:
		return agentConfirmDeadlineExternalCron
	default:
		return agentConfirmDeadline
	}
}

// haltAgentRun stops the whole run for a reason outside the wave gate (today:
// the kill switch), then records this task's own outcome. The task is SKIPPED,
// not failed: the control plane declined to act, which is not a site error.
func (w *Worker) haltAgentRun(ctx context.Context, task Task, reason, taskDetail string) error {
	w.logger.Warn("agent self-update refused before dispatch",
		slog.String("task_id", task.ID.String()),
		slog.String("site_id", task.SiteID.String()),
		slog.String("reason", reason))

	return w.haltAgentRunWith(ctx, task, TaskSkipped, task.FromVersion, "", taskDetail, "", reason)
}

// haltAgentRunWith records THIS task's outcome and then halts the whole run for
// a reason the wave gate itself cannot derive from the task rows (the kill
// switch, or a release manifest that no longer offers the version the run exists
// to install).
//
// The task is finished FIRST, and deliberately not through finishAgentTask:
//
//   - Halting first would leave every sibling cancelled by the time this task
//     finished, so the ordinary run-completion check would flip the run to
//     "completed" and the gate re-evaluation would then have to correct it back
//     to "halted", recording a second incident for one incident.
//   - finishAgentTask re-judges the wave gate, which on a CONFIRMING outcome
//     would open and enqueue the next wave a moment before the halt shuts it.
//     Those jobs are harmless (they claim, see the halt and stop) but they are
//     jobs dispatched for a rollout that is over.
//
// The halt still cancels every task nothing was sent for and leaves running ones
// to their own confirmation job (see haltLocked).
func (w *Worker) haltAgentRunWith(ctx context.Context, task Task, status, fromVersion, toVersion, detail, errMsg, reason string) error {
	if err := w.finish(ctx, task, status, fromVersion, toVersion, detail, errMsg); err != nil {
		return err
	}
	if w.agent.Waves == nil {
		// Nothing to halt against; this task's own outcome is still recorded.
		return nil
	}
	ev, err := w.agent.Waves.HaltAgentRun(ctx, task.TenantID, task.RunID, reason)
	if err != nil {
		w.logger.Warn("agent self-update: halt failed", slog.Any("error", err))
		return nil
	}
	w.recordRunHalted(ctx, task.TenantID, task.RunID, ev)
	return nil
}

// finishAgentTask records an agent task's terminal state and then re-judges
// the run's wave gate. Every agent-task terminal transition goes through here,
// which is what makes the gate self-correcting: the halt verdict is recomputed
// from the authoritative rows after every change, and the run's status is
// re-asserted afterwards (see haltLocked) so an ordinary run-completion check
// can never quietly overwrite a halt with "completed".
func (w *Worker) finishAgentTask(ctx context.Context, task Task, status, fromVersion, toVersion, detail, errMsg string) error {
	if err := w.finish(ctx, task, status, fromVersion, toVersion, detail, errMsg); err != nil {
		return err
	}
	w.evaluateAgentRun(ctx, task.TenantID, task.RunID)
	return nil
}

// evaluateAgentRun re-judges the wave gate and either records the halt or
// enqueues whatever the newly-opened wave made dispatchable. Best-effort: an
// error here never fails the River job (the task already reached a terminal
// state), and the gate is re-judged from scratch by the next terminal
// transition and by every claim attempt, so a lost evaluation self-heals.
func (w *Worker) evaluateAgentRun(ctx context.Context, tenantID, runID uuid.UUID) {
	if w.agent.Waves == nil {
		return
	}
	ev, err := w.agent.Waves.EvaluateAgentRun(ctx, tenantID, runID)
	if err != nil {
		w.logger.Warn("agent self-update: wave evaluation failed",
			slog.String("run_id", runID.String()), slog.Any("error", err))
		return
	}
	if ev.Halted {
		w.recordRunHalted(ctx, tenantID, runID, ev)
		return
	}
	if w.agent.Tasks == nil {
		return
	}
	for _, t := range ev.Dispatchable {
		if err := w.agent.Tasks.EnqueueTask(ctx, tenantID, runID, t.ID, false); err != nil {
			w.logger.Warn("agent self-update: enqueue of the next wave failed",
				slog.String("run_id", runID.String()),
				slog.String("task_id", t.ID.String()), slog.Any("error", err))
		}
	}
}

// recordRunHalted logs and audits a halt exactly once (AgentRunEvaluation.
// Changed is true only for the caller that performed the transition, so
// concurrent finishers do not produce duplicate incident records).
func (w *Worker) recordRunHalted(ctx context.Context, tenantID, runID uuid.UUID, ev AgentRunEvaluation) {
	if !ev.Changed {
		return
	}
	w.logger.Warn("agent self-update run halted",
		slog.String("run_id", runID.String()),
		slog.String("reason", ev.Reason),
		slog.Int("cancelled_tasks", ev.Cancelled))
	if w.audit == nil {
		return
	}
	_, _ = w.audit.Record(ctx, audit.Event{
		TenantID:   tenantID,
		ActorType:  audit.ActorSystem,
		Action:     ActionRunHalted,
		TargetType: "update_run",
		TargetID:   runID.String(),
		Metadata: map[string]any{
			"reason":          ev.Reason,
			"cancelled_tasks": ev.Cancelled,
		},
	})
}

func detailOr(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}

// ---------------------------------------------------------------------------
// BEAT 2, confirmation
// ---------------------------------------------------------------------------

// AgentConfirmArgs is the River job payload for the beat-2 confirmation poll.
// The deadline is an ABSOLUTE time carried in the args, so it survives the
// job snoozing itself repeatedly and cannot be reset by a retry.
type AgentConfirmArgs struct {
	TenantID uuid.UUID `json:"tenant_id"`
	RunID    uuid.UUID `json:"run_id"`
	TaskID   uuid.UUID `json:"task_id"`
	SiteID   uuid.UUID `json:"site_id"`
	// FromVersion is what the site ran when it armed.
	FromVersion string `json:"from_version,omitempty"`
	// ExpectVersion is the version the agent said it would install. Empty
	// when the agent did not say, in which case any strictly newer well-formed
	// version confirms.
	ExpectVersion string `json:"expect_version,omitempty"`
	// CronMode is carried for the operator-facing detail on a timeout, so a
	// site whose cron is disabled explains itself in the failure text.
	CronMode string `json:"cron_mode,omitempty"`
	// DeadlineAt is when an unconfirmed upgrade becomes a failure.
	DeadlineAt time.Time `json:"deadline_at"`
	// ApplyID is the opaque per-apply identifier carried unmodified from the
	// beat-1 answer that armed this task (agentcmd.AgentSelfUpdateResponse.
	// ApplyID), on BOTH the "scheduled" and "already_scheduled" paths. "" for
	// an agent that predates this field. It is compared WHOLE against the
	// ApplyID on the site's own stored apply record: only a match lets a
	// version movement be credited to THIS run rather than to something else
	// that happened to move the site's version. See agentApplyAttributed.
	ApplyID string `json:"apply_id,omitempty"`
}

// Kind implements river.JobArgs.
func (AgentConfirmArgs) Kind() string { return "update_agent_confirm" }

// InsertOpts reuses the per-tenant queue shard so confirmation polls are
// bounded by the same per-tenant parallelism limit as the tasks they belong to.
func (a AgentConfirmArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueForTenant(a.TenantID)}
}

// AgentConfirmWorker is beat 2: it waits for the NEW agent code to phone home.
//
// This is the only place an agent self-update is allowed to become
// TaskSucceeded. The ARM acknowledgement from beat 1 says only that a cron
// event was scheduled; nothing had been applied when it was sent, and on a
// site whose loopback cron is broken nothing ever will be. Recording that ack
// as success would report a fleet as upgraded while an unknown share of it
// still runs the old build.
type AgentConfirmWorker struct {
	river.WorkerDefaults[AgentConfirmArgs]
	w    *Worker
	poll time.Duration
	now  func() time.Time
}

// NewAgentConfirmWorker builds the confirmation worker on top of the same
// Worker (and therefore the same repo/hub/audit/logger and the same wave
// gate) that dispatches the tasks.
func NewAgentConfirmWorker(w *Worker) *AgentConfirmWorker {
	return &AgentConfirmWorker{w: w, poll: agentConfirmPoll, now: time.Now}
}

// SetPollInterval overrides the re-check interval. Exposed for tests so the
// poll loop does not actually wait the production 30s; production code should
// rely on the default.
func (c *AgentConfirmWorker) SetPollInterval(d time.Duration) {
	if d > 0 {
		c.poll = d
	}
}

// SetClock overrides the time source (tests).
func (c *AgentConfirmWorker) SetClock(now func() time.Time) {
	if now != nil {
		c.now = now
	}
}

// Work runs one confirmation poll.
func (c *AgentConfirmWorker) Work(ctx context.Context, job *river.Job[AgentConfirmArgs]) error {
	a := job.Args
	w := c.w

	task, err := w.repo.GetTask(ctx, a.TenantID, a.TaskID)
	if err != nil {
		return err
	}
	if terminal(task.Status) {
		// Already resolved: the run halted and cancelled it, or a duplicate
		// poll got there first.
		return nil
	}
	if w.agent.Versions == nil {
		return w.finishAgentTask(ctx, task, TaskFailed, a.FromVersion, a.ExpectVersion,
			"the agent self-update channel is disabled on this control plane", "")
	}

	reported, err := w.agent.Versions.AgentVersion(ctx, a.TenantID, a.SiteID)
	if err != nil {
		if de, ok := domain.AsDomain(err); ok && de.Kind == domain.KindNotFound {
			return w.finishAgentTask(ctx, task, TaskFailed, a.FromVersion, a.ExpectVersion,
				"site unresolved while confirming the agent self-update", err.Error())
		}
		// A transient read failure proves nothing either way. Keep waiting;
		// the deadline below is what eventually resolves this task.
		w.logger.Warn("agent self-update: version read failed",
			slog.String("site_id", a.SiteID.String()), slog.Any("error", err))
	} else if agentSelfUpdateConfirmed(reported, a.FromVersion, a.ExpectVersion) {
		// BEAT 2. The new code reported its own version over the signed
		// metadata channel. Version movement alone is the only evidence an
		// agent that predates apply ids can ever offer, so it still confirms
		// for that agent (see agentConfirmOutcome). For an agent that DOES
		// report apply ids, movement alone is not enough: it must also be
		// attributed to THIS run's own apply before it may open the next
		// wave, or a canary that moved for an unrelated reason (a person, an
		// unrelated apply, WordPress's own machinery) could authorise touching
		// every other site in the fleet on evidence this run never produced.
		//
		// Reading that attribution can itself fail transiently, and that read
		// failure must be treated exactly like the version read's above: it
		// proves nothing either way, so it must not resolve this task. Without
		// this the CP would turn a genuinely successful, confirmed upgrade
		// into a skipped task on nothing more than one unlucky poll of a
		// different table (see agentApplyResult).
		res, resErr := w.agentApplyResult(ctx, a.TenantID, a.SiteID)
		if resErr != nil {
			w.logger.Warn("agent self-update: apply-result read failed",
				slog.String("site_id", a.SiteID.String()), slog.Any("error", resErr))
		} else {
			status, toVersion, detail := agentConfirmOutcome(a, reported, res)
			return w.finishAgentTask(ctx, task, status, a.FromVersion, toVersion, detail, "")
		}
	}

	if !c.now().Before(a.DeadlineAt) {
		// The deadline has passed regardless of whether this last read of the
		// apply-result record succeeded; its error is discarded here on
		// purpose. Unlike the confirmed branch above, this task is failing
		// either way, so a failed read only means the failure explanation is
		// less detailed, never that the failure itself was wrongly declared.
		res, _ := w.agentApplyResult(ctx, a.TenantID, a.SiteID)
		return w.finishAgentTask(ctx, task, TaskFailed, a.FromVersion, a.ExpectVersion,
			unconfirmedDetail(a, reported, res), "agent_self_update_unconfirmed")
	}
	return river.JobSnooze(c.poll)
}

// agentApplyResult reads the agent's own account of its last apply beat.
//
// It returns (nil, nil) when there is genuinely NOTHING to read: the lookup
// was never wired, the site never reported an apply outcome, or it reported
// one with an empty status. Every caller may treat that case as "the agent
// said nothing" without changing any outcome, exactly as before this field
// existed.
//
// It returns (nil, err) when the READ ITSELF failed. That is a different
// fact, and callers must NOT collapse it into the case above: a transient
// database blip proves nothing about whether the apply happened, so it must
// never be read as an absent or unattributed record. The confirmed branch in
// AgentConfirmWorker.Work treats this exactly like a failed read of the
// agent's version beside it, keeping the task open rather than resolving it,
// because doing otherwise turns one unlucky poll into a halted rollout for an
// upgrade that may already have succeeded. A caller that only enriches the
// text of an ALREADY-failing timeout (unconfirmedDetail) may still discard
// this error, since nothing about that task's outcome turns on the text.
func (w *Worker) agentApplyResult(ctx context.Context, tenantID, siteID uuid.UUID) (*AgentApplyResult, error) {
	if w.agent.Results == nil {
		return nil, nil
	}
	res, ok, err := w.agent.Results.AgentSelfUpdateResult(ctx, tenantID, siteID)
	if err != nil {
		return nil, err
	}
	if !ok || res.Status == "" {
		return nil, nil
	}
	return &res, nil
}

// agentApplyAttributed reports whether the site's stored apply record was
// written by THIS run. Apply ids are opaque and compared whole. Nothing else
// participates: versions cannot, because a record may legitimately carry an
// empty to_version (a "not_eligible" or "error" record has none), and times
// cannot, because a retry re-arms with a fresh deadline against the same
// site and its earlier record is still sitting there with an earlier
// timestamp. An agent that reports no apply id at all (a.ApplyID == "") can
// never attribute anything: PROVENANCE requires a channel that can name its
// own apply, and one that cannot name it cannot be checked against it either.
func agentApplyAttributed(a AgentConfirmArgs, res *AgentApplyResult) bool {
	return a.ApplyID != "" && res != nil && res.ApplyID != "" && res.ApplyID == a.ApplyID
}

// shortApplyID truncates an opaque apply id to a stable prefix for logs, audit
// metadata and operator-facing detail text: enough to correlate two mentions
// of the SAME apply without putting a whole opaque token in a log line built
// to be read by a person.
func shortApplyID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// agentConfirmOutcome classifies a version-moved beat-2 signal into a task
// outcome. Version movement alone is never enough on its own to open the next
// wave: it must also be attributed to THIS run's own apply, or a canary that
// moved for a reason this run did not cause could authorise touching every
// other site in the fleet on no evidence at all.
//
//   - a.ApplyID == "": the agent predates the apply-id channel. Version
//     movement is the only evidence such an agent can ever offer, so it
//     still confirms, but the detail hedges the cause rather than claiming
//     it. This branch MUST exist: without it, the very release that
//     introduces apply ids could never roll out through the channel that
//     ships it, because every site in its first wave is still running the
//     agent that predates them.
//   - a.ApplyID != "" and the site's stored record is attributed to this run
//     (agentApplyAttributed) and its status is "applied": the strongest
//     case, confirmed twice over, by the signed metadata channel AND by the
//     agent's own apply record naming this exact run as the cause.
//   - a.ApplyID != "" and NOT attributed: the version moved, but nothing
//     ties the move to this run (a record from a different apply, a record
//     that carries no apply id, or no record at all). Skipped, not
//     succeeded, and ToVersion is left empty to match the non-confirming
//     precedent in upToDate: a canary here must never open the next wave.
//   - a.ApplyID != "" and attributed but the record's status is not
//     "applied": the machine signature of a record that IS this run's own
//     apply, saying it did NOT install what the site is now running (an
//     "already_applied" or "failed" record sitting next to a version that
//     nonetheless moved, for some other reason). Skipped: whatever moved
//     this site's version, this run's own apply says it was not the cause.
func agentConfirmOutcome(a AgentConfirmArgs, reported string, res *AgentApplyResult) (status, toVersion, detail string) {
	if a.ApplyID == "" {
		return TaskSucceeded, reported, fmt.Sprintf(
			"upgraded: the agent reported %s over its signed metadata channel. This site's agent does not report an apply id, "+
				"so this control plane cannot show that this run caused the move rather than a person or WordPress itself. "+
				"The version is right; the cause is unproven.", reported)
	}

	if agentApplyAttributed(a, res) {
		if res.Status == agentApplyApplied {
			return TaskSucceeded, reported, fmt.Sprintf(
				"upgraded and confirmed: the agent reported %s over its signed metadata channel, and its own apply record "+
					"for %s says this channel installed it.", reported, shortApplyID(a.ApplyID))
		}
		return TaskSkipped, "", fmt.Sprintf(
			"not confirmed: the agent now reports %s, at or beyond this run's target, but its own apply record for this run "+
				"(%s) reads %q, so this run's own apply did not install the build this site is now running.",
			reported, shortApplyID(a.ApplyID), res.Status)
	}

	return TaskSkipped, "", fmt.Sprintf(
		"not confirmed: the agent now reports %s, at or beyond this run's target, but nothing ties that move to this run, "+
			"so it proves nothing about this upgrade and no further site may be opened on the strength of it. %s",
		reported, unattributedRecordClause(a, res))
}

// unattributedRecordClause names WHICH of the three unattributed cases
// applies, so the operator reading a "not confirmed" detail knows what to go
// look at rather than just that attribution failed.
func unattributedRecordClause(a AgentConfirmArgs, res *AgentApplyResult) string {
	switch {
	case res == nil:
		return "This site holds no apply record at all."
	case res.ApplyID == "":
		return "This site's apply record carries no apply id."
	default:
		return fmt.Sprintf("This site's apply record belongs to a different apply (%s), not to the one this run armed (%s).",
			shortApplyID(res.ApplyID), shortApplyID(a.ApplyID))
	}
}

// unconfirmedDetail explains a deadline expiry in terms an operator can act
// on, including the crucial fact that nothing was necessarily applied: a site
// whose apply never ran was never touched.
//
// When the agent left an account of its own apply beat, that account is what
// turns this from a shrug into a diagnosis: "the apply never ran" and
// "the apply ran and the upgrade failed" produce the identical timeout
// here, and only the agent can tell them apart.
func unconfirmedDetail(a AgentConfirmArgs, reported string, res *AgentApplyResult) string {
	still := reported
	if still == "" {
		still = "an unknown version"
	}
	base := fmt.Sprintf("unconfirmed: the agent accepted the upgrade to %s but never reported it (still reporting %s). "+
		"The site was not necessarily modified: WordPress keeps a backup of the previous build for the duration of the swap "+
		"and restores it when an upgrade does not complete.",
		fromOr(a.ExpectVersion, "the published build"), still)
	if a.CronMode == agentcmd.CronModeExternal {
		base += " This site reports that WordPress loopback cron is unavailable. That does not hold up the upgrade itself, which runs inside the command request, " +
			"but it does mean the confirmation push depends on site traffic or on an external scheduler this control plane cannot see."
	}
	if s := applyResultSentence(a, res); s != "" {
		base += " " + s
	}
	return base
}

// applyResultSentence renders the agent's apply-beat record as one operator-
// facing sentence, or "" when there is no record. A record is only ever given
// a CAUSAL reading (this run's own apply DID this) when it is attributed to
// THIS run's own apply id (agentApplyAttributed); an unattributed record is
// still rendered in full, status, versions and detail included, never
// suppressed, but its causation is hedged rather than claimed
// (unattributedApplyResultSentence). The four attributed statuses the agent
// reports each mean something different about where to look next, so each
// gets its own wording rather than a generic dump; an unrecognized status (a
// newer agent) is still surfaced verbatim rather than swallowed.
func applyResultSentence(a AgentConfirmArgs, res *AgentApplyResult) string {
	if res == nil {
		if a.ApplyID == "" {
			// A legacy arm carries nothing to compare a record against, and an
			// old agent predating the record entirely is exactly what "no
			// record" already meant before apply ids existed. Read exactly as
			// it did before this field existed.
			return ""
		}
		return fmt.Sprintf("This site's agent reports apply ids but holds no record for %s, so this run's apply command reached the site but no "+
			"outcome was ever stored or pushed back for it. Look at whether the apply itself began, or whether its metadata push reached this control plane.",
			shortApplyID(a.ApplyID))
	}
	if res.Status == "" {
		return ""
	}
	if !agentApplyAttributed(a, res) {
		return unattributedApplyResultSentence(a, res)
	}

	when := ""
	if !res.At.IsZero() {
		when = " at " + res.At.UTC().Format(time.RFC3339)
	}
	detail := ""
	if res.Detail != "" {
		detail = ": " + res.Detail
	}
	move := fmt.Sprintf("%s to %s", fromOr(res.FromVersion, "an unreported version"), fromOr(res.ToVersion, "the target build"))
	label := fmt.Sprintf("The agent's apply record for %s", shortApplyID(a.ApplyID))

	switch res.Status {
	case agentApplyApplied:
		return fmt.Sprintf("%s says it DID apply the upgrade (%s)%s, so the apply ran and it is the version report that has not arrived; "+
			"look at the site's metadata push rather than at the upgrade%s.", label, move, when, detail)
	case agentApplyFailed:
		return fmt.Sprintf("%s says the apply FAILED (%s)%s, so the apply did run and the upgrade is what did not%s.", label, move, when, detail)
	case agentApplyExpired:
		return fmt.Sprintf("%s says the upgrade EXPIRED before it was applied%s, so nothing was applied%s.", label, when, detail)
	case agentApplyAlreadyApplied:
		return fmt.Sprintf("%s says the on-disk version was ALREADY at or past the target (%s)%s%s.", label, move, when, detail)
	default:
		return fmt.Sprintf("%s of its last apply reads %q (%s)%s%s.", label, res.Status, move, when, detail)
	}
}

// unattributedApplyResultSentence renders a record this control plane cannot
// tie to the current run. The record is still rendered IN FULL, status,
// versions, detail and time, because a hidden record helps nobody; what
// changes is that every sentence here explicitly refuses to read it as the
// cause of anything about this run's own timeout.
func unattributedApplyResultSentence(a AgentConfirmArgs, res *AgentApplyResult) string {
	when := "an unreported time"
	if !res.At.IsZero() {
		when = res.At.UTC().Format(time.RFC3339)
	}
	move := fmt.Sprintf("%s to %s", fromOr(res.FromVersion, "an unreported version"), fromOr(res.ToVersion, "the target build"))
	detail := ""
	if res.Detail != "" {
		detail = ", detail: " + res.Detail
	}

	if a.ApplyID == "" {
		// This run's own arm predates apply ids, so it has nothing to compare
		// the record against at all, whatever the record itself says.
		return fmt.Sprintf("This site's agent does not report an apply id, so no record it holds can be tied to this run. "+
			"Its last apply record reads %q (%s) at %s%s. It may or may not be an account of this run.", res.Status, move, when, detail)
	}
	if res.ApplyID == "" {
		return fmt.Sprintf("This site does hold an apply record, but it carries no apply id, not the one this run armed (%s), "+
			"so it is not an account of this run: it reads %q (%s at %s%s). Do not read it as the cause of this timeout.",
			shortApplyID(a.ApplyID), res.Status, move, when, detail)
	}
	return fmt.Sprintf("This site does hold an apply record, but it belongs to a different apply (%s) and not to the one this run armed (%s), "+
		"so it is not an account of this run: it reads %q (%s at %s%s). Do not read it as the cause of this timeout.",
		shortApplyID(res.ApplyID), shortApplyID(a.ApplyID), res.Status, move, when, detail)
}

// The apply-beat statuses the agent reports. Mirrored here as constants so the
// wire vocabulary this control plane branches on is stated in one place.
const (
	agentApplyApplied        = "applied"
	agentApplyFailed         = "failed"
	agentApplyExpired        = "expired"
	agentApplyAlreadyApplied = "already_applied"
)
