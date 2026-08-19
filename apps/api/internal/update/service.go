package update

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentplugin"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentrelease"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

// SiteInfo is the minimal site projection the update service needs to plan and
// execute a run: identity, the agent target URL, the current component
// versions (used to seed from_version without a round-trip to the agent), and
// the site's OWN pending WordPress-core update advisory (CoreUpdateAvailable
// etc). planTasks intersects a run's requested items against each site's own
// pending set (Components[].UpdateAvailable / CoreUpdateAvailable) — see #126.
type SiteInfo struct {
	ID         uuid.UUID
	URL        string
	Name       string
	Enrolled   bool
	Components []Component

	// CoreUpdateAvailable reports whether THIS site's own inventory advertises
	// a pending WordPress core update. CoreCurrentVersion/CoreNewVersion carry
	// the versions from that advisory when set.
	CoreUpdateAvailable bool
	CoreCurrentVersion  string
	CoreNewVersion      string

	// AgentVersion is the agent version the site last reported over its signed
	// metadata push (sites.agent_version). It is the ONLY input the agent
	// self-update planner uses to decide whether a site is behind, precisely
	// because it does not come from the site's plugin inventory: that inventory
	// is built from the site's own WordPress update-check cache, which for the
	// agent is a stale advisory the control plane deliberately suppresses
	// everywhere else (see indexPending). Empty when the site has never
	// reported one.
	AgentVersion string
}

// Component is one installed plugin/theme with its current version and its
// OWN pending-update advisory (mirrors site.Component.AvailableUpdate).
// UpdateAvailable/NewVersion are the per-site authority planTasks intersects
// a run's requested items against (#126: a target requested because another
// site in the run has it pending must not spawn a task on a site that does
// not).
type Component struct {
	Type string // "plugin" | "theme"
	Slug string
	// Name is the component's plugin-header name, carried from the site
	// inventory purely so the planner can identify the agent's own plugin by
	// something the directory name cannot forge (see agentplugin). Empty when
	// the source inventory did not record one.
	Name    string
	Version string

	UpdateAvailable bool
	NewVersion      string
}

// SiteLookup resolves the target sites for a run. Implemented by the site
// service (wired in main) so the update package needs no site import.
type SiteLookup interface {
	// GetSiteInfo returns one tenant-scoped site, or a NotFound domain error.
	GetSiteInfo(ctx context.Context, tenantID, siteID uuid.UUID) (SiteInfo, error)
	// ListSiteInfoByTag returns the tenant's enrolled sites carrying tag (empty
	// tag ⇒ all enrolled sites).
	ListSiteInfoByTag(ctx context.Context, tenantID uuid.UUID, tag string) ([]SiteInfo, error)
}

// Enqueuer schedules the per-task background jobs (River, wired in main).
type Enqueuer interface {
	EnqueueTask(ctx context.Context, tenantID, runID, taskID uuid.UUID, dryRun bool) error
}

// DispatchEnqueuer schedules the one-shot job that fires a deferred run at its
// scheduled_at (GH #463). Separate from Enqueuer because it is optional: a
// Service without one still CREATES scheduled runs correctly (they are
// committed and due-scannable), it simply has nothing pushing them but the
// Phase 2 safety-net sweeper.
type DispatchEnqueuer interface {
	// EnqueueDispatch inserts a dispatch job to become runnable at `at`. The
	// job carries no task ids: what is dispatchable at fire time is not
	// knowable now.
	EnqueueDispatch(ctx context.Context, tenantID, runID uuid.UUID, at time.Time) error
}

// Service holds the update orchestration logic.
type Service struct {
	repo      Repo
	sites     SiteLookup
	enqueuer  Enqueuer
	validator *domain.Validator
	clock     domain.Clock

	// agentSelfUpdate is the fleet-wide kill switch, mirrored from the worker's
	// copy so an operator gets an immediate refusal instead of a run that
	// halts on its first task. FALSE BY DEFAULT: a Service that never has
	// SetAgentSelfUpdate called on it refuses the agent target outright.
	agentSelfUpdate bool
	// releases reports the currently published agent version, the yardstick
	// the agent planner compares each site against.
	releases AgentReleaseReader

	// dispatcher schedules the deferred-run dispatch job (GH #463). Optional:
	// nil means a scheduled run is still created and still due-scannable, but
	// nothing pushes it at its start time.
	dispatcher DispatchEnqueuer
}

// SetDispatchEnqueuer wires the deferred-dispatch job inserter. Call once at
// boot.
func (s *Service) SetDispatchEnqueuer(d DispatchEnqueuer) { s.dispatcher = d }

// NewService builds an update Service.
func NewService(repo Repo, sites SiteLookup, enqueuer Enqueuer, v *domain.Validator, clock domain.Clock) *Service {
	return &Service{repo: repo, sites: sites, enqueuer: enqueuer, validator: v, clock: clock}
}

// SetAgentSelfUpdate wires the agent self-update channel's kill switch and the
// published-version reader. Call once at boot; not calling it leaves the
// channel off.
func (s *Service) SetAgentSelfUpdate(enabled bool, releases AgentReleaseReader) {
	s.agentSelfUpdate = enabled
	s.releases = releases
}

// CreateRun validates the input, resolves the target sites, creates the run and
// its per-(site,item) tasks atomically, then enqueues a background job per task.
// from_version is seeded from the site's known component versions so the run
// records the prior state even before the worker contacts the agent.
func (s *Service) CreateRun(ctx context.Context, in CreateRunInput) (Run, []Task, error) {
	if in.TenantID == uuid.Nil {
		return Run{}, nil, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	if len(in.SiteIDs) == 0 && in.Tag == "" {
		return Run{}, nil, domain.Validation("target_required", "specify site_ids or a tag to target")
	}
	if len(in.SiteIDs) > 0 && in.Tag != "" {
		return Run{}, nil, domain.Validation("target_ambiguous", "specify either site_ids or a tag, not both")
	}
	if err := s.validator.Struct(in); err != nil {
		return Run{}, nil, err
	}
	in.Items = normalizeItems(in.Items)
	if len(in.Items) == 0 {
		return Run{}, nil, domain.Validation("items_required", "at least one update item is required")
	}
	if err := validateItems(in.Items); err != nil {
		return Run{}, nil, err
	}
	deferred, err := s.resolveSchedule(in.ScheduledAt)
	if err != nil {
		return Run{}, nil, err
	}

	sites, err := s.resolveSites(ctx, in)
	if err != nil {
		return Run{}, nil, err
	}
	if len(sites) == 0 {
		return Run{}, nil, domain.Validation("no_target_sites", "no enrolled sites matched the selection")
	}

	// Cross-run dedup pre-check (#131 hardening): find every (site, target)
	// among the resolved sites that already has a pending/running task from
	// ANY OTHER run, so planTasks can skip creating a doomed duplicate instead
	// of racing the agent's rollback-snapshot pruning / concurrent
	// Plugin_Upgrader runs against the same plugin directory. The
	// update_tasks_inflight_target_idx partial unique index (enforced inside
	// CreateRunWithTasks) is the authoritative guard for the remaining race
	// window between this check and the insert.
	siteIDs := make([]uuid.UUID, 0, len(sites))
	for _, si := range sites {
		siteIDs = append(siteIDs, si.ID)
	}
	inFlight, err := s.repo.ListInFlightTargets(ctx, in.TenantID, siteIDs)
	if err != nil {
		return Run{}, nil, err
	}

	agentRun := isAgentRun(in.Items)

	var (
		tasks           []NewTask
		skippedInFlight int
	)
	if agentRun {
		tasks, skippedInFlight, err = s.planAgentTasks(ctx, sites, in, inFlight)
		if err != nil {
			return Run{}, nil, err
		}
	} else {
		tasks, skippedInFlight = s.planTasks(sites, in.Items, inFlight)
	}
	if len(tasks) == 0 {
		if skippedInFlight > 0 {
			return Run{}, nil, domain.Conflict("targets_in_flight", "the selected updates already have an update in progress in another run")
		}
		if agentRun {
			return Run{}, nil, domain.Validation("no_tasks",
				"no selected site needs an agent upgrade: each one already runs the published version, or runs a build this channel cannot upgrade")
		}
		return Run{}, nil, domain.Validation("no_tasks", "the selection produced no update tasks")
	}

	// GH #463: a run with a future scheduled_at is born 'scheduled', with every
	// task 'scheduled' too, and NOTHING is enqueued here. The wizard has
	// offered this control since M3 and the column has been persisted since
	// then, but every task was enqueued immediately regardless: an operator who
	// picked 02:00 updated their fleet the moment they pressed the button.
	//
	// The deferral is a SEPARATE repo method rather than a flag on
	// CreateRunWithTasks because the two differ in the statement they use to
	// insert each task, not merely in a column value —
	// CreateScheduledUpdateTask's ON CONFLICT arm is unreachable where
	// CreateUpdateTask's is the authoritative in-flight guard, so the zero-row
	// result means opposite things in the two paths. Sharing one function would
	// mean one body reading a zero row two ways depending on a bool.
	if deferred {
		run, createdTasks, err := s.repo.CreateScheduledRunWithTasks(ctx, in, tasks)
		if err != nil {
			return Run{}, nil, err
		}
		// The dispatch job is the ONLY thing enqueued, and it carries no task
		// ids: the dispatcher re-reads the run's tasks at fire time, because
		// what is dispatchable then is not knowable now. A failure to enqueue
		// it is surfaced (the operator can retry the create) but is NOT fatal
		// to the run — the run row is committed and still 'scheduled', so it
		// remains due-scannable. Phase 2's safety-net sweeper is what turns
		// "the queue insert was lost" into "dispatched a little late" instead
		// of "never dispatched"; until it lands, a lost insert leaves a run
		// that an operator must re-create.
		if s.dispatcher != nil {
			if derr := s.dispatcher.EnqueueDispatch(ctx, run.TenantID, run.ID, *in.ScheduledAt); derr != nil {
				return run, createdTasks, derr
			}
		}
		return run, createdTasks, nil
	}

	run, createdTasks, err := s.repo.CreateRunWithTasks(ctx, in, tasks)
	if err != nil {
		return Run{}, nil, err
	}

	// Enqueue one background job per task. A best-effort enqueue: a failure here
	// leaves the task pending (the caller still gets the run); we surface the
	// first enqueue error so the operator can retry.
	//
	// An agent run enqueues ONLY its first wave. Later waves are enqueued by
	// the worker after the preceding wave has confirmed (see
	// Worker.evaluateAgentRun), so a rollout that must not continue never has
	// a job sitting ready to run. The worker-side gate
	// (pgRepo.ClaimAgentWaveTask) is the authoritative guard regardless, so an
	// early or duplicated enqueue still cannot dispatch out of turn.
	for _, t := range enqueueSet(createdTasks, agentRun) {
		if eerr := s.enqueuer.EnqueueTask(ctx, run.TenantID, run.ID, t.ID, run.DryRun); eerr != nil {
			return run, createdTasks, eerr
		}
	}
	return run, createdTasks, nil
}

// Schedule bounds for a one-shot deferred run (GH #463).
const (
	// scheduleMaxLead caps how far out a run may be scheduled. The bound is not
	// arithmetic squeamishness: a River job sits in the queue for the whole
	// interval, and River's own completed/scheduled-job retention is a
	// deployment setting nobody has verified against a month-long horizon. A
	// month is also past the point where the operator's reason for choosing the
	// time is still true.
	scheduleMaxLead = 30 * 24 * time.Hour

	// scheduleSkewGrace is how far in the PAST a scheduled_at may be and still
	// be read as "now" rather than refused. The client computes the instant
	// from a browser clock (update-wizard.tsx builds it from a local
	// datetime-local value), so a submission for "now" routinely lands a few
	// seconds behind the server's clock, and a strict now() comparison would
	// reject it as a past time. Anything older than this was typed, not
	// skewed.
	scheduleSkewGrace = 2 * time.Minute

	// scheduleMinLead is the lead time below which deferral is pointless and a
	// run is created immediately instead. A run born 'scheduled' but already
	// due waits for the dispatcher's next tick for no benefit, and it is the
	// one shape that can be simultaneously scheduled and past its own start —
	// which is the state the grace window has to reason about. Keeping it out
	// of existence for a sub-minute schedule is cheaper than handling it.
	scheduleMinLead = time.Minute
)

// resolveSchedule decides whether a CreateRunInput is deferred, and refuses the
// two schedules that cannot be honoured.
//
// It returns (false, nil) for BOTH a nil scheduled_at and a scheduled_at that
// is effectively now: the caller's immediate path is unchanged in either case,
// which keeps every existing run creation on exactly the code it ran on before
// #463. Only a genuinely future time returns true.
//
// Rejection is deliberately at the service and not at the dispatcher. A past
// time and a too-far time are both operator input errors, and the operator is
// standing right there at create time; discovering either one asynchronously,
// hours later, as a run that quietly expired, is the outcome this whole issue
// exists to stop.
func (s *Service) resolveSchedule(at *time.Time) (bool, error) {
	if at == nil {
		return false, nil
	}
	now := s.now()
	switch {
	case at.Before(now.Add(-scheduleSkewGrace)):
		return false, domain.Validation("schedule_in_past",
			"the scheduled time is in the past; pick a future time or leave the schedule empty to run now")
	case at.After(now.Add(scheduleMaxLead)):
		return false, domain.Validation("schedule_too_far",
			"a run cannot be scheduled more than 30 days ahead")
	case at.Before(now.Add(scheduleMinLead)):
		// Inside the skew grace, or under a minute out: run it now.
		return false, nil
	}
	return true, nil
}

// now reads the service's clock, falling back to the wall clock when none was
// wired.
//
// ONLY THE SCHEDULE VALIDATION uses it — the past/too-far bounds and the
// minimum lead, all decided at create time against an instant the operator just
// supplied. Nothing about when a run FIRES is decided here, and a skewed clock
// on this replica can therefore only reject a schedule at the boundary, never
// dispatch one early.
//
// An earlier version of this comment credited ListDueUpdateRuns' now() with
// deciding due-ness. That was true of the scan, but the dispatcher no longer
// reaches its run through the scan; what actually gates firing is River's own
// availability check, which is DB-clocked, with DispatchWorker's early-fire
// snooze behind it.
func (s *Service) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

// isAgentRun reports whether the run targets the agent's own upgrade.
// validateAgentItem already guarantees such a run carries exactly one item.
func isAgentRun(items []Item) bool {
	return len(items) == 1 && items[0].Type == TargetAgent
}

// enqueueSet returns the tasks to enqueue immediately: all of them for an
// ordinary run, and only the first wave for an agent run.
func enqueueSet(tasks []Task, agentRun bool) []Task {
	if !agentRun {
		return tasks
	}
	return DeriveAgentWaveState(tasks).DispatchableTasks()
}

// planAgentTasks builds the agent self-update run's ORDERED task set.
//
// It deliberately does NOT consult the site's plugin inventory, which is what
// every other target is planned from. That inventory is the site's own
// WordPress update-check cache, and for the agent it carries an advisory the
// control plane suppresses everywhere else precisely because it is stale,
// self-referential, and (under a renamed plugin directory) not reliably
// attributable. The authority here is instead the pair of facts the control
// plane can vouch for on its own: the version the site's agent reported over
// its signed metadata push (SiteInfo.AgentVersion) and the version this
// control plane actually publishes (AgentReleaseReader).
//
// Comparison is delegated to agentrelease.Classify, the same classifier
// behind the fleet agent-freshness dashboard, so "this site is behind" has
// exactly one definition. Only StatusOutdated produces a task:
//
//   - StatusCurrent    the site already runs the published build.
//   - StatusIneligible the site runs the plugin-directory build, which ships
//     without a self-updater and is upgraded by the directory.
//   - StatusUnknown    one of the two versions is missing or not well-formed.
//     Never guess: an unreadable version must not become an upgrade.
func (s *Service) planAgentTasks(ctx context.Context, sites []SiteInfo, in CreateRunInput, inFlight map[InFlightKey]struct{}) ([]NewTask, int, error) {
	if !s.agentSelfUpdate {
		return nil, 0, domain.Conflict("agent_self_update_disabled",
			"the agent self-update channel is disabled on this control plane")
	}
	if in.DryRun {
		return nil, 0, domain.Validation("agent_dry_run_unsupported",
			"the agent self-update channel has no dry run: arming already changes nothing on the site, and applying happens in a later request the control plane does not drive")
	}
	latest := ""
	if s.releases != nil {
		latest = s.releases.LatestVersion(ctx)
	}
	if latest == "" {
		return nil, 0, domain.Conflict("agent_release_unknown",
			"the currently published agent version could not be determined, so no site can be told it is behind")
	}

	ordered := make([]SiteInfo, len(sites))
	copy(ordered, sites)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID.String() < ordered[j].ID.String() })

	tasks := make([]NewTask, 0, len(ordered))
	skippedInFlight := 0
	for _, site := range ordered {
		if agentrelease.Classify(site.AgentVersion, latest, agentDistributionOf(site.Components)) != agentrelease.StatusOutdated {
			continue
		}
		if _, blocked := inFlight[InFlightKey{SiteID: site.ID, TargetType: TargetAgent, TargetSlug: AgentTargetSlug}]; blocked {
			skippedInFlight++
			continue
		}
		tasks = append(tasks, NewTask{
			SiteID:     site.ID,
			TargetType: TargetAgent,
			TargetSlug: AgentTargetSlug,
			// The RESOLVED published version, not the literal "latest". This is
			// the run's PREMISE recorded at the moment it was planned: "this run
			// sets out to install THIS version".
			//
			// It changes nothing on the wire. A pin is refused at the API
			// boundary (see validateAgentItem) and the arm command carries no
			// version at all: the agent installs whatever its own signed manifest
			// offers, and its downgrade guard refuses anything older than what it
			// runs. What this column buys is the ability to CHECK an answer
			// later.
			//
			// The release manifest is mutable, and reverting it is exactly what
			// an operator is expected to do during an incident. Without the
			// planned target persisted, the only thing a later "up_to_date"
			// answer could be scored against is a LIVE read of that same
			// manifest, which scores the site against a target that may have
			// moved down to meet it: every site then answers "already current",
			// every task records succeeded, and a fleet-wide run completes green
			// with not one agent moved. See Worker.upToDate.
			DesiredVersion: latest,
			FromVersion:    site.AgentVersion,
		})
	}
	return tasks, skippedInFlight, nil
}

// agentDistributionOf identifies which build of the agent a site runs, from
// the site's own plugin inventory. Used ONLY to exclude the plugin-directory
// build, which has no self-updater at all; the version comparison itself never
// touches the inventory. DistributionNone (no recognizable agent entry) falls
// through to the version comparison unchanged, exactly as the fleet dashboard
// treats it.
func agentDistributionOf(components []Component) agentplugin.Distribution {
	for _, c := range components {
		if c.Type != TargetPlugin {
			continue
		}
		if d := agentplugin.DistributionOf(c.Slug, c.Name); d != agentplugin.DistributionNone {
			return d
		}
	}
	return agentplugin.DistributionNone
}

// resolveSites returns the target SiteInfos for the run input.
func (s *Service) resolveSites(ctx context.Context, in CreateRunInput) ([]SiteInfo, error) {
	if len(in.SiteIDs) > 0 {
		out := make([]SiteInfo, 0, len(in.SiteIDs))
		for _, id := range in.SiteIDs {
			si, err := s.sites.GetSiteInfo(ctx, in.TenantID, id)
			if err != nil {
				return nil, err
			}
			if si.Enrolled {
				out = append(out, si)
			}
		}
		return out, nil
	}
	return s.sites.ListSiteInfoByTag(ctx, in.TenantID, in.Tag)
}

// planTasks expands (sites × items) into NewTask rows, INTERSECTED against
// each site's own pending-update inventory (#126, the N×M task-explosion
// bug): a default ("latest"/unset) item produces a task on a given site ONLY
// when that site's own inventory reports the item as pending. A target that
// is pending on some OTHER site in a multi-site run, but not on this one,
// produces NO task here — never a task that is doomed to fail (or, with the
// agent-side defensive fix, silently skip).
//
// An item carrying an EXPLICIT version pin (e.g. a deliberate downgrade, or a
// vuln-remediation fix version the site's own WordPress update-check has not
// yet caught up to reporting — see vuln.Service.Remediate) is a forced
// operator/system action: it is applied wherever the target is installed on
// the site, regardless of whether that site currently flags it as pending.
//
// from_version is always seeded from the SITE'S OWN known component version
// (never another site's), whichever branch created the task.
//
// inFlight is the tenant's current cross-run in-flight-task set (#131
// hardening — see Repo.ListInFlightTargets): a (site, target) pair already
// present there has a pending/running task in ANOTHER run, so planTasks skips
// it here rather than creating a duplicate task that would race the agent's
// rollback-snapshot pruning or run a concurrent Plugin_Upgrader against the
// same plugin directory. It returns the produced tasks plus how many
// candidates were skipped for this reason, so CreateRun can distinguish
// "nothing pending" from "everything is already in progress".
func (s *Service) planTasks(sites []SiteInfo, items []Item, inFlight map[InFlightKey]struct{}) ([]NewTask, int) {
	tasks := make([]NewTask, 0, len(sites)*len(items))
	skippedInFlight := 0
	for _, site := range sites {
		installed := indexVersions(site.Components)
		pending := indexPending(site)
		for _, item := range items {
			slug := item.Slug
			if item.Type == TargetCore {
				slug = "core"
			}
			key := item.Type + "/" + slug

			from, hasInstalled := installed[key]
			_, hasPending := pending[key]
			if item.Type == TargetCore {
				hasInstalled = true // core is always "installed"
				from = site.CoreCurrentVersion
			}

			pinned := item.Version != "" && item.Version != "latest"
			switch {
			case pinned:
				if !hasInstalled {
					continue
				}
			default:
				if !hasPending {
					continue
				}
			}

			if _, blocked := inFlight[InFlightKey{SiteID: site.ID, TargetType: item.Type, TargetSlug: slug}]; blocked {
				skippedInFlight++
				continue
			}

			desired := item.Version
			if desired == "" {
				desired = "latest"
			}
			tasks = append(tasks, NewTask{
				SiteID:         site.ID,
				TargetType:     item.Type,
				TargetSlug:     slug,
				DesiredVersion: desired,
				FromVersion:    from,
			})
		}
	}
	return tasks, skippedInFlight
}

// indexPending returns the site's own pending-update set keyed by "type/slug"
// ("core/core" for WordPress core), mapping to the version the site's own
// inventory advertises as available. Only entries the site itself currently
// reports as pending are present; this is the per-site authority planTasks
// intersects a run's requested items against.
func indexPending(site SiteInfo) map[string]string {
	m := make(map[string]string, len(site.Components)+1)
	for _, c := range site.Components {
		// GH #211: this is the planTasks authority — a same-version phantom
		// advisory (new_version == the component's own installed Version)
		// must never be treated as pending here, defense-in-depth against a
		// SiteLookup implementation that did not already filter it (see
		// cmd/wpmgr's toUpdateComponent).
		// The agent's own plugin is never pending from the planner's point of
		// view: a SiteLookup fed by an inventory an older agent persisted may
		// still carry the self-update advisory, and treating it as pending here
		// would let a "latest" item plan the one task that cannot be rolled
		// back. validateItems already refuses the target outright; this keeps
		// the pending authority itself honest.
		// Matched on the component's plugin-header name as well as its slug:
		// validateItems only ever sees a bare slug, so this is the one planning
		// step that can recognize an agent installed under a directory name no
		// slug list predicts.
		if c.Type == TargetPlugin && agentplugin.IsComponent(c.Slug, c.Name) {
			continue
		}
		if c.UpdateAvailable && c.NewVersion != "" && !wpversion.SameVersion(c.Version, c.NewVersion) {
			m[c.Type+"/"+c.Slug] = c.NewVersion
		}
	}
	if site.CoreUpdateAvailable && site.CoreNewVersion != "" &&
		!wpversion.SameVersion(site.CoreCurrentVersion, site.CoreNewVersion) {
		m[TargetCore+"/core"] = site.CoreNewVersion
	}
	return m
}

// CancelRunResult is what a successful cancellation reports back.
type CancelRunResult struct {
	Run Run
	// CancelledTasks is how many tasks were terminalized alongside the run.
	// Zero is legitimate: a run whose tasks had already left 'scheduled'
	// cancels with none.
	CancelledTasks int
}

// CancelScheduledRun calls back a deferred run before it fires.
//
// The 409 on a non-scheduled run is the WHOLE FEATURE, not an edge case. A
// cancel that "succeeded" on a run whose commands were already out would tell
// an operator their fleet was safe while it was being updated, which is worse
// than refusing outright. The repo's CAS is what decides, and a false from it
// becomes a typed conflict here so httpx renders 409 and the client can
// distinguish "too late" from a server fault.
//
// The NotFound check runs first and separately so the two answers stay
// distinct: a run that never existed is a 404, and a run that exists but has
// already fired is a 409 with a status the operator can act on.
func (s *Service) CancelScheduledRun(ctx context.Context, tenantID, runID uuid.UUID) (CancelRunResult, error) {
	if tenantID == uuid.Nil {
		return CancelRunResult{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	if runID == uuid.Nil {
		return CancelRunResult{}, domain.Validation("invalid_run_id", "a run id is required")
	}

	// Read first, so a genuinely missing run is a 404 rather than being folded
	// into the conflict below. This read is NOT the authority for whether the
	// cancel may proceed — the CAS is, and the row can change underneath us
	// between the two. That is intentional and safe: losing that race produces
	// the same 409 as being late, which is the correct answer either way.
	if _, err := s.repo.GetRun(ctx, tenantID, runID); err != nil {
		return CancelRunResult{}, err
	}

	run, tasks, cancelled, err := s.repo.CancelScheduledRun(ctx, tenantID, runID,
		"cancelled by an operator before the scheduled run started; nothing was sent to this site")
	if err != nil {
		return CancelRunResult{}, err
	}
	if !cancelled {
		// Too late. Re-read so the message names the state the operator is
		// actually looking at, rather than a generic refusal they cannot act
		// on. A failure to re-read must not mask the conflict, so the fallback
		// still returns one.
		status := "no longer scheduled"
		if current, rerr := s.repo.GetRun(ctx, tenantID, runID); rerr == nil {
			status = current.Status
		}
		return CancelRunResult{}, domain.Conflict("run_not_cancellable",
			"this run has already left the scheduled state (now "+status+") and cannot be cancelled; use halt if it is running")
	}
	return CancelRunResult{Run: run, CancelledTasks: tasks}, nil
}

// GetRun returns a run with its tasks.
func (s *Service) GetRun(ctx context.Context, tenantID, runID uuid.UUID) (Run, []Task, error) {
	run, err := s.repo.GetRun(ctx, tenantID, runID)
	if err != nil {
		return Run{}, nil, err
	}
	tasks, err := s.repo.ListTasks(ctx, tenantID, runID)
	if err != nil {
		return Run{}, nil, err
	}
	return run, tasks, nil
}

// ListRuns returns a page of the tenant's runs.
func (s *Service) ListRuns(ctx context.Context, tenantID uuid.UUID, limit, offset int32) ([]Run, error) {
	if tenantID == uuid.Nil {
		return nil, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	limit, offset = normalizePage(limit, offset)
	return s.repo.ListRuns(ctx, tenantID, limit, offset)
}

// ListRunSummaries returns a page of the tenant's runs with pre-computed
// per-run task aggregate counts (task_count, succeeded_count, failed_count,
// site_count) in a single query. Used by the list endpoint.
func (s *Service) ListRunSummaries(ctx context.Context, tenantID uuid.UUID, limit, offset int32) ([]RunSummary, error) {
	if tenantID == uuid.Nil {
		return nil, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	limit, offset = normalizePage(limit, offset)
	return s.repo.ListRunSummaries(ctx, tenantID, limit, offset)
}

func indexVersions(cs []Component) map[string]string {
	m := make(map[string]string, len(cs))
	for _, c := range cs {
		m[c.Type+"/"+c.Slug] = c.Version
	}
	return m
}

func normalizeItems(items []Item) []Item {
	seen := map[string]struct{}{}
	out := make([]Item, 0, len(items))
	for _, it := range items {
		it.Type = strings.TrimSpace(strings.ToLower(it.Type))
		it.Slug = strings.TrimSpace(it.Slug)
		it.Version = strings.TrimSpace(it.Version)
		if it.Type == TargetCore {
			it.Slug = "core"
		}
		// The agent target names exactly one thing, so an omitted slug is the
		// normal case and is filled in here rather than dropped. A slug the
		// caller DID supply is left alone for validateAgentItem to accept or
		// refuse; silently replacing it would hide a mistaken request.
		if it.Type == TargetAgent && it.Slug == "" {
			it.Slug = AgentTargetSlug
		}
		if it.Type != TargetCore && it.Slug == "" {
			continue
		}
		key := it.Type + "/" + it.Slug
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, it)
	}
	return out
}

func normalizePage(limit, offset int32) (int32, int32) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
