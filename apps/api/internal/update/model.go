// Package update implements the M3 bulk plugin/theme/core update feature: an
// operator creates an update run targeting a selection of sites and items; the
// control plane fans the work out into per-(site,item) tasks executed by a
// River worker that snapshots, applies the update via a signed CP->agent
// command, health-probes the site, and auto-rolls-back on failure. Live
// progress is streamed over SSE from an in-process pub/sub hub.
//
// Every query is tenant-scoped both explicitly (tenant_id in the WHERE clause)
// and by Postgres RLS (the app.tenant_id policy on update_runs/update_tasks).
package update

import (
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentplugin"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Run statuses.
const (
	RunPending   = "pending"
	RunRunning   = "running"
	RunCompleted = "completed"
	// RunHalted is terminal and specific to an agent self-update run: a wave
	// gate refused to advance, so every task the run had not already dispatched
	// was cancelled. It is deliberately distinct from RunCompleted, which would
	// erase the fact that the run was stopped rather than finished.
	RunHalted = "halted"

	// RunScheduled is a run created with a future scheduled_at that has not
	// fired. NOTHING HAS BEEN SENT TO ANY SITE, and that is the property the
	// rest of the system reads off this value: its tasks are TaskScheduled and
	// therefore outside update_tasks_inflight_target_idx (so they reserve
	// nothing) and outside the stale-task reaper's sweep (so they are not
	// failed for not having progressed). It is the only status
	// ListDueUpdateRuns scans for, and the only precondition
	// ClaimUpdateRunForDispatch, ExpireDueUpdateRun and
	// CancelScheduledUpdateRun accept.
	RunScheduled = "scheduled"
	// RunDispatching is TRANSIENT AND HELD ONLY INSIDE ONE TRANSACTION: the
	// dispatcher claims the run into it, enqueues, and leaves it again before
	// committing. It is deliberately absent from update_runs_due_idx, so a run
	// left sitting here is never scanned again — which is exactly why the
	// claim and the FinishUpdateRunDispatch that closes it must share a
	// transaction, so a crash rolls the claim back rather than stranding the
	// run forever. A reader that observes it should re-read.
	RunDispatching = "dispatching"
	// RunExpired is terminal: the run came due while the control plane was
	// unavailable and was still undispatched more than dispatchGraceWindow
	// later, so running it now is no longer what the operator asked for. No
	// site was contacted. Distinct from RunHalted, which stops a rollout that
	// already has commands out on real sites, and from RunCompleted.
	RunExpired = "expired"
)

// Task statuses.
const (
	TaskPending    = "pending"
	TaskRunning    = "running"
	TaskSucceeded  = "succeeded"
	TaskFailed     = "failed"
	TaskRolledBack = "rolled_back"
	TaskSkipped    = "skipped"
	// TaskCancelled is terminal: the task was never dispatched because its run
	// halted first, or because an operator cancelled the scheduled run before
	// it fired. Distinct from TaskSkipped (the control plane declined this
	// particular target) and from TaskFailed (the site was contacted and did
	// not come back): nothing was ever sent to this site.
	TaskCancelled = "cancelled"

	// TaskScheduled is the birth status of every task of a deferred run, and
	// it is NOT terminal — see terminal(). Two exclusions are load-bearing and
	// both are by construction rather than by a second predicate anybody has
	// to remember to keep in sync:
	//
	//   update_tasks_inflight_target_idx is partial on ('pending','running'),
	//   so a task waiting for 02:00 does NOT hold the (tenant, site, target)
	//   slot and the operator's urgent 10:00 update of that same plugin is
	//   still accepted rather than 409 targets_in_flight.
	//
	//   ListStaleUpdateTasks sweeps the same two statuses, so a task waiting
	//   longer than staleTaskThreshold is not reaped as "stale: task exceeded
	//   max runtime" hours before it was ever meant to run.
	//
	// The price of both is that a scheduled task cannot pre-verify its target
	// will be free when it fires; MarkScheduledUpdateTaskPending resolves that
	// at dispatch time, and the loser becomes TaskSkipped.
	TaskScheduled = "scheduled"
	// TaskExpired is terminal: the parent run expired without dispatching, so
	// this task was never attempted and nothing was sent to its site.
	//
	// IT IS NOT A SPELLING OF TaskCancelled, and collapsing the two would tell
	// an operator that somebody stopped their run when in fact the control
	// plane was simply not up in time to start it. 'cancelled' records a
	// decision a human made; 'expired' records that the window closed.
	// schema.sql declares both, so neither needs a migration.
	TaskExpired = "expired"
)

// Target types. plugin/theme/core mirror agentcmd.TargetPlugin/Theme/Core.
const (
	TargetPlugin = "plugin"
	TargetTheme  = "theme"
	TargetCore   = "core"
	// TargetAgent is the agent's OWN upgrade, shipped over the dedicated
	// three-beat signed channel (agentcmd.AgentSelfUpdateRequest), never over
	// the plugin update path. It is a separate target type rather than a
	// plugin slug precisely so no code that handles a plugin update can ever
	// reach it: the snapshot/rollback that guards every plugin update cannot be
	// delivered by the process being replaced.
	//
	// update_tasks.target_type is plain text NOT NULL (m3), so this value needs
	// no migration.
	TargetAgent = "agent"
)

// AgentTargetSlug is the fixed target_slug every agent self-update task
// carries. The agent target names exactly one thing, this control plane's own
// plugin, so the operator never supplies a slug and the planner never derives
// one from a site's inventory (which is what a renamed plugin directory or a
// stale advisory could poison). It is pinned to the self-hosted distribution
// slug because that is the build the release channel publishes for; the
// plugin-directory build has no self-updater and is excluded at planning time.
const AgentTargetSlug = agentplugin.SlugSelfHosted

// Run is an update run: a tenant-scoped unit grouping per-(site,item) tasks.
type Run struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	CreatedBy   *uuid.UUID
	Status      string
	DryRun      bool
	ScheduledAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// RunSummary is a Run with pre-computed task aggregate counts, returned by
// the list endpoint so callers do not need a separate per-run tasks fetch.
type RunSummary struct {
	Run
	TaskCount      int64
	SucceededCount int64
	FailedCount    int64
	SiteCount      int64
}

// Task is one unit of work: apply one item on one site.
type Task struct {
	ID       uuid.UUID
	RunID    uuid.UUID
	TenantID uuid.UUID
	SiteID   uuid.UUID
	// SiteName is the site's display name, carried on the task row itself so a
	// caller never has to join a task list against a separately-fetched (and
	// therefore paginated) site list to say which site a task belongs to. Only
	// the DETAIL read populates it (Repo.ListTasks, via
	// ListUpdateTasksForRunWithSiteName); the wave machine and the per-task
	// worker paths do not need it and leave it empty.
	SiteName       string
	TargetType     string
	TargetSlug     string
	DesiredVersion string
	FromVersion    string
	ToVersion      string
	Status         string
	Detail         string
	Error          string
	StartedAt      *time.Time
	FinishedAt     *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Item is one requested update target within a CreateRunInput.
type Item struct {
	Type    string `json:"type" validate:"required,oneof=plugin theme core agent"`
	Slug    string `json:"slug" validate:"max=200"`
	Version string `json:"version" validate:"max=64"`
}

// versionPattern bounds the update version to "latest" or a conservative
// version-pin charset (leading alnum then a small safe set). It deliberately
// forbids whitespace, ';', '&', and '--' so a value cannot smuggle extra
// arguments into the agent's WP-CLI invocation (e.g. "latest --activate" or
// "1.0; rm -rf"). The agent re-validates as defense-in-depth (ADR contract).
var versionPattern = regexp.MustCompile(`^(latest|[0-9][0-9A-Za-z.\-]{0,63})$`)

// slugPattern bounds plugin/theme slugs to a safe filesystem-ish charset,
// optionally one path segment (e.g. "akismet" or "akismet/akismet"). No spaces,
// shell metacharacters, or path traversal sequences are allowed.
var slugPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)?$`)

// validateItems enforces the CP-side safe charset on each update item's version
// and slug AFTER normalization, returning a KindValidation (HTTP 422) domain
// error on the first offending value. This is the control-plane guard against
// argument injection into the agent's WP-CLI; the agent validates again.
//
// It is also the planning boundary's refusal point for the agent's own plugin:
// the control plane never offers that target (see site.actionableUpdate), so an
// item naming it can only come from a stale advisory persisted by an older
// agent, a hand-built request, or a replayed payload. It is rejected outright
// rather than dispatched, because applying it would have the agent overwrite
// its own running code with no deliverable rollback.
//
// An item is a bare (type, slug, version) triple with no inventory entry behind
// it, so this check can only use agentplugin's slug form. An agent installed
// under an unexpected directory name is therefore NOT refused here; it is
// stopped one step later by indexPending, which sees the site's inventory and
// matches the plugin-header name, and again by the worker before dispatch.
func validateItems(items []Item) error {
	for _, it := range items {
		if it.Type == TargetAgent {
			if err := validateAgentItem(it, len(items)); err != nil {
				return err
			}
			continue
		}
		if len(it.Slug) > 200 || !slugPattern.MatchString(it.Slug) {
			return domain.Validation("invalid_slug", "update item slug contains an unsafe value: "+it.Slug)
		}
		if it.Type == TargetPlugin && agentplugin.Is(it.Slug) {
			return domain.Validation("agent_self_update_forbidden",
				"the site agent cannot be updated through an update run: it upgrades itself over its own signed channel")
		}
		if it.Version != "" && !versionPattern.MatchString(it.Version) {
			return domain.Validation("invalid_version", "update item version contains an unsafe value: "+it.Version)
		}
	}
	return nil
}

// validateAgentItem is the API boundary for the agent self-update target.
//
// A version pin is REJECTED rather than ignored. The release manifest the
// agent verifies only ever points at the currently published build, and the
// agent's own downgrade guard refuses anything older than what it is running,
// so a pin cannot be honoured by construction. Accepting one and quietly
// installing something else would tell the operator they pinned a version when
// they did not; refusing is the only honest answer. "latest" (and the unset
// default, which means the same thing) is accepted because it is what the
// channel actually does.
//
// The slug is not operator-supplied either: this target names exactly one
// thing. An empty slug is accepted and normalized to AgentTargetSlug; anything
// else is refused rather than silently replaced, for the same reason.
//
// The agent target must also be the ONLY item in its run. The wave gate that
// makes this channel safe is defined over "the run", wave 0 is one site, wave
// 1 is 5% of the run, and a run also carrying plugin/theme/core tasks has no
// well-defined denominator, no meaningful canary, and would let an unrelated
// plugin failure halt an agent rollout (or an agent failure cancel unrelated
// plugin work). Splitting them is one extra request and keeps both machines
// honest.
func validateAgentItem(it Item, itemCount int) error {
	if itemCount != 1 {
		return domain.Validation("agent_target_exclusive",
			"the agent self-update target must be the only item in its run: its wave gate is defined over the whole run")
	}
	if it.Version != "" && it.Version != "latest" {
		return domain.Validation("agent_version_pin_unsupported",
			"the agent self-update channel cannot install a pinned version: the release manifest only ever points at the published build and the agent refuses to downgrade")
	}
	if it.Slug != "" && it.Slug != AgentTargetSlug {
		return domain.Validation("agent_slug_unsupported",
			"the agent self-update target takes no slug: it names this control plane's own agent plugin")
	}
	return nil
}

// CreateRunInput is the validated input for creating an update run. Exactly one
// of SiteIDs or Tag selects the target sites.
type CreateRunInput struct {
	TenantID    uuid.UUID
	CreatedBy   uuid.UUID
	SiteIDs     []uuid.UUID
	Tag         string
	Items       []Item `validate:"required,min=1,max=200,dive"`
	DryRun      bool
	ScheduledAt *time.Time
}

// terminal reports whether a task status is a final state.
//
// TaskExpired is here and TaskScheduled deliberately is not, and the asymmetry
// is the whole point of the pair. Every caller of this function uses it to
// decide "is there anything left to do for this row?", and the two answers are
// opposite: an expired task will never be attempted and must stop every worker
// that meets it, whereas a scheduled task has not been attempted YET. Calling
// TaskScheduled terminal would make Worker.Work return nil for a task whose
// whole run is still ahead of it.
func terminal(status string) bool {
	switch status {
	case TaskSucceeded, TaskFailed, TaskRolledBack, TaskSkipped, TaskCancelled, TaskExpired:
		return true
	default:
		return false
	}
}

// Retry classes. A task's retry class is the SERVER's answer to "what happened
// to this task, and what would retrying it mean" (GH #336). It exists so no
// client has to infer that answer from prose: the two things a retry decision
// actually turns on, whether the update was ever attempted and whether it was
// applied and then taken back, are recorded as machine values here and read as
// machine values everywhere else.
//
// It is deliberately NOT the same axis as `status`: `cancelled` and `pending`
// are different statuses that both mean "nothing was sent to this site", but
// only one of them is over.
const (
	// RetryClassNeverRan: the task reached a terminal state without the control
	// plane ever sending anything to the site. Today that is exactly
	// `cancelled` (its run halted first). Nothing on the site changed, which
	// makes this the LOWEST-risk thing a retry can pick up.
	RetryClassNeverRan = "never_ran"
	// RetryClassFailed: the site was contacted and the update did not succeed.
	// The primary retry case.
	RetryClassFailed = "failed"
	// RetryClassReverted: the update APPLIED and was then rolled back, because
	// the post-update health probe or the wave gate refused it. Retrying walks
	// the identical path, so it may reproduce the identical break. Offered, but
	// never part of a default set.
	RetryClassReverted = "reverted"
	// RetryClassSkipped: the control plane declined this target (already up to
	// date, the site stayed busy, the agent's own plugin, an agent build this
	// channel cannot upgrade). Usually correct, but not always: a stale
	// WordPress transient can report "already current" when it is not (GH
	// #211/#212), which is why a skip is selectable rather than hidden.
	RetryClassSkipped = "skipped"
	// RetryClassNotApplicable: retrying is not a meaningful action. Either the
	// task succeeded (a retry would re-touch a working site for nothing) or it
	// has not finished yet (pending/running), in which case there is nothing to
	// retry, only something to wait for.
	RetryClassNotApplicable = "not_applicable"
)

// retryClassify is the ONE definition of "may this task be retried, and why".
// Both the read path (the retryable/retry_class fields on every task in the run
// detail response) and the write path (which requested tasks RetryRun will
// actually plan) call it, so the button the operator sees and the decision the
// server makes cannot disagree.
//
// It is built on the existing terminal() predicate rather than a second list of
// statuses: a status that is not final is not retryable, whatever it is, and
// that stays true for any status added later.
func retryClassify(status string) (retryable bool, class string) {
	if !terminal(status) {
		return false, RetryClassNotApplicable // pending, running
	}
	switch status {
	case TaskCancelled:
		return true, RetryClassNeverRan
	case TaskFailed:
		return true, RetryClassFailed
	case TaskRolledBack:
		return true, RetryClassReverted
	case TaskSkipped:
		return true, RetryClassSkipped
	default: // TaskSucceeded
		return false, RetryClassNotApplicable
	}
}
