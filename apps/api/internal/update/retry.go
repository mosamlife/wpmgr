package update

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentrelease"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// GH #336: retry an update run's tasks from the run itself.
//
// The shape of this file follows one rule: the SERVER decides, and NOTHING is
// silently dropped. A retry answers for every task it was asked about, either
// by creating work for it or by naming the reason it did not, and it says so in
// the response rather than in a log line the operator will never read.
//
// Two invariants shape the rest:
//
//  1. A retry CREATES A NEW RUN. It never mutates the run it was launched from.
//     The original stays exactly as it happened, which is the only reason the
//     word "retry" can be used honestly at all: the failure it records is still
//     there to read afterwards.
//
//  2. A retry is PLANNED FROM THE TASK ROWS, not by replaying the original
//     request through Service.CreateRun. Every field NewTask needs (site,
//     target type, target slug, desired version, from version) is already a
//     column on the task row, so the retry set is exactly the (site, target)
//     pairs the operator selected. Re-running the item planner instead would
//     re-expand items across sites and re-intersect them against each site's
//     CURRENT pending set, which is a different question from "do this one
//     again" and would both add and drop work nobody asked for.
//
// Two things are nevertheless RE-RESOLVED rather than copied off the row,
// because they are facts about the world now and not about the old run:
// enrollment, and (for an agent rollout) the currently published agent version.
// Copying either one would let a retry act on a premise that has since expired.

// Exclusion reasons. Machine values, stable, safe to group and count on. Every
// one of them is paired with a server-authored sentence in the response so a
// client can render the truth without maintaining its own copy table.
const (
	// ExcludeNotInRun: the requested task id is not a task of this run (or not
	// of this tenant, which is indistinguishable and deliberately so).
	ExcludeNotInRun = "not_in_run"
	// ExcludeNotRetryable: the task succeeded, or has not finished yet. See
	// retryClassify.
	ExcludeNotRetryable = "not_retryable"
	// ExcludeSiteNotFound: the site no longer resolves.
	ExcludeSiteNotFound = "site_not_found"
	// ExcludeSiteNotEnrolled: the site is no longer enrolled, so no signed
	// command can be delivered to it. Service.CreateRun applies the same gate
	// (resolveSites) but drops such a site silently; a retry reports it.
	ExcludeSiteNotEnrolled = "site_not_enrolled"
	// ExcludeAgentCurrent: the site already runs the published agent build, so
	// there is nothing to upgrade it to. This is the case that fires when an
	// operator REVERTED the release mid-incident, which is exactly what they
	// are expected to do; without this reason the retry would simply produce
	// nothing and say nothing.
	ExcludeAgentCurrent = "agent_current"
	// ExcludeAgentIneligible: the site runs a build with no self-updater (the
	// plugin-directory distribution), which this channel never upgrades.
	ExcludeAgentIneligible = "agent_ineligible"
	// ExcludeAgentVersionUnknown: the site's reported version or the published
	// version is missing or not well-formed. Never guess: an unreadable version
	// must not become an upgrade.
	ExcludeAgentVersionUnknown = "agent_version_unknown"
	// ExcludeTargetInFlight: this (site, target) already has a pending or
	// running task in another run. Creating a second one would race the agent's
	// own snapshot pruning and run concurrent upgraders against the same
	// directory, so it is refused (#131 hardening).
	ExcludeTargetInFlight = "target_in_flight"
	// ExcludeDuplicateTarget: two selected tasks name the SAME (site, target),
	// so retrying both would mean doing one thing twice.
	ExcludeDuplicateTarget = "duplicate_target"
)

// maxRetryTaskIDs bounds one retry request. It is far above any run this
// control plane realistically produces (the wave gate alone makes a fleet-wide
// agent rollout a few hundred tasks, and the widest plugin run observed is a
// few thousand), and exists so a hand-built or replayed body cannot ask the
// server to resolve an unbounded number of sites inside one HTTP request. A run
// larger than this can still be retried in slices.
const maxRetryTaskIDs = 5000

// RetryRunInput is the validated input for retrying a run's tasks.
type RetryRunInput struct {
	TenantID  uuid.UUID
	CreatedBy uuid.UUID
	// RunID is the run the selected tasks belong to.
	RunID uuid.UUID
	// TaskIDs is the operator's explicit selection. There is no implicit
	// default set on the server: the client knows every task's retry_class from
	// the run detail response and names the ones it means. A server-side
	// default would be a second policy that could disagree with the checkboxes
	// the operator actually saw.
	TaskIDs []uuid.UUID
}

// RetryExclusion is one requested task that produced no work, and why.
type RetryExclusion struct {
	TaskID uuid.UUID
	// Reason is the machine value (one of the Exclude* constants).
	Reason string
	// Message is the server-authored sentence for that reason, including the
	// specifics (which versions, which status), so the client renders the truth
	// instead of reconstructing it.
	Message string
}

// RetryResult accounts for EVERY requested task: Created + len(Excluded)
// always equals Requested. That identity is the whole point of the type.
type RetryResult struct {
	// RunID is the new run. uuid.Nil when nothing was created, which is a
	// legitimate outcome (everything was excluded) and not an error: the
	// exclusions say why.
	RunID uuid.UUID
	// Requested is the number of DISTINCT task ids in the request.
	Requested int
	// Created is the number of tasks in the new run.
	Created int
	// Excluded names every requested task that produced no task in the new run.
	Excluded []RetryExclusion
	// Warning is set when the run exists but something after the commit did not
	// go to plan (today: a background enqueue failed, so its tasks sit pending
	// until the reaper). Empty on the ordinary path.
	Warning string
}

func (r *RetryResult) exclude(taskID uuid.UUID, reason, message string) {
	r.Excluded = append(r.Excluded, RetryExclusion{TaskID: taskID, Reason: reason, Message: message})
}

// RetryRun creates a NEW run repeating the selected tasks of an existing one.
//
// It returns a result that accounts for every requested task even when it also
// returns an error: a non-nil RunID means the new run IS committed (its tasks
// exist and are visible) and only a best-effort step after the commit failed,
// exactly like Service.CreateRun. A caller must report the run in that case
// rather than telling the operator nothing happened.
func (s *Service) RetryRun(ctx context.Context, in RetryRunInput) (RetryResult, error) {
	var res RetryResult
	if in.TenantID == uuid.Nil {
		return res, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	if in.RunID == uuid.Nil {
		return res, domain.Validation("invalid_run_id", "a run id is required")
	}

	ids := dedupeIDs(in.TaskIDs)
	if len(ids) == 0 {
		return res, domain.Validation("task_ids_required", "select at least one task to retry")
	}
	if len(ids) > maxRetryTaskIDs {
		return res, domain.Validation("too_many_tasks",
			fmt.Sprintf("a single retry may name at most %d tasks; retry the run in smaller selections", maxRetryTaskIDs))
	}
	res.Requested = len(ids)

	// The source run, tenant-scoped: a run in another tenant is a 404 here,
	// exactly as it is on the detail read.
	sourceRun, tasks, err := s.GetRun(ctx, in.TenantID, in.RunID)
	if err != nil {
		return res, err
	}
	byID := make(map[uuid.UUID]Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}

	// Pass 1: resolve each requested id to a task of this run, and drop the
	// ones whose recorded outcome is not retryable at all.
	candidates := make([]Task, 0, len(ids))
	for _, id := range ids {
		t, ok := byID[id]
		if !ok {
			res.exclude(id, ExcludeNotInRun, "this task is not part of the run being retried")
			continue
		}
		retryable, class := retryClassify(t.Status)
		if !retryable {
			res.exclude(id, ExcludeNotRetryable, notRetryableMessage(t.Status, class))
			continue
		}
		candidates = append(candidates, t)
	}
	if len(candidates) == 0 {
		// Nothing to do, and every requested task already carries its reason.
		// Deliberately NOT an error: the caller asked a well-formed question and
		// this is the complete answer to it.
		return res, nil
	}

	// An agent rollout and ordinary plugin/theme/core work cannot share a run:
	// the wave gate is defined over "the run", so a mixed run has no meaningful
	// canary and no denominator (see validateAgentItem). A source run cannot mix
	// them either, so this can only be reached by a hand-built selection.
	agentRun, err := retrySelectionIsAgent(candidates)
	if err != nil {
		return res, err
	}

	latest := ""
	if agentRun {
		if !s.agentSelfUpdate {
			return res, domain.Conflict("agent_self_update_disabled",
				"the agent self-update channel is disabled on this control plane")
		}
		if s.releases != nil {
			latest = s.releases.LatestVersion(ctx)
		}
		if latest == "" {
			return res, domain.Conflict("agent_release_unknown",
				"the currently published agent version could not be determined, so no site can be told it is behind")
		}
	}

	// Pass 2: RE-RESOLVE every distinct site. Enrollment is a fact about now,
	// not about the run: a site unenrolled since then must not be retried, and
	// (for an agent rollout) the version it currently reports is the only input
	// the classifier may use.
	sites, siteBlocked, err := s.resolveRetrySites(ctx, in.TenantID, candidates)
	if err != nil {
		return res, err
	}

	// Cross-run dedup pre-check over the sites that survived, same guard
	// Service.CreateRun applies. The partial unique index is still the
	// authority; this just lets the response name the collision.
	siteIDs := make([]uuid.UUID, 0, len(sites))
	for id := range sites {
		siteIDs = append(siteIDs, id)
	}
	inFlight, err := s.repo.ListInFlightTargets(ctx, in.TenantID, siteIDs)
	if err != nil {
		return res, err
	}

	// Pass 3: plan. planned[i] is the task to create for sourceTaskIDs[i], so a
	// row lost at insert time can be attributed back to the task the operator
	// selected.
	planned := make([]NewTask, 0, len(candidates))
	sourceTaskIDs := make([]uuid.UUID, 0, len(candidates))
	seen := make(map[InFlightKey]struct{}, len(candidates))
	for _, t := range candidates {
		if block, isBlocked := siteBlocked[t.SiteID]; isBlocked {
			res.exclude(t.ID, block.Reason, block.Message)
			continue
		}
		site := sites[t.SiteID]
		key := InFlightKey{SiteID: t.SiteID, TargetType: t.TargetType, TargetSlug: t.TargetSlug}
		if _, dup := seen[key]; dup {
			res.exclude(t.ID, ExcludeDuplicateTarget,
				fmt.Sprintf("another selected task already retries %s on this site", targetLabel(t)))
			continue
		}
		if _, blocked := inFlight[key]; blocked {
			res.exclude(t.ID, ExcludeTargetInFlight,
				fmt.Sprintf("%s already has an update in progress on this site in another run", targetLabel(t)))
			continue
		}

		nt := NewTask{
			SiteID:     t.SiteID,
			TargetType: t.TargetType,
			TargetSlug: t.TargetSlug,
			// Copied from the row for plugin/theme/core: the retry repeats the
			// SAME target the original run set out to install, including an
			// explicit version pin, which is the only reading of "retry" that
			// does not quietly change what was asked for.
			DesiredVersion: t.DesiredVersion,
			FromVersion:    t.FromVersion,
		}
		if agentRun {
			status := agentrelease.Classify(site.AgentVersion, latest, agentDistributionOf(site.Components))
			if status != agentrelease.StatusOutdated {
				reason, message := agentExclusion(status, site.AgentVersion, latest)
				res.exclude(t.ID, reason, message)
				continue
			}
			// NEVER the row's copy. The release manifest is mutable and
			// reverting it mid-incident is an expected operator move, so the
			// target is whatever is published NOW, and from_version is whatever
			// the site reports NOW.
			nt.DesiredVersion = latest
			nt.FromVersion = site.AgentVersion
		}

		seen[key] = struct{}{}
		planned = append(planned, nt)
		sourceTaskIDs = append(sourceTaskIDs, t.ID)
	}
	if len(planned) == 0 {
		return res, nil
	}

	runIn := CreateRunInput{
		TenantID:  in.TenantID,
		CreatedBy: in.CreatedBy,
		// The retry inherits the source run's dry-run flag, so a retry of a
		// rehearsal is another rehearsal and cannot become a live change by
		// accident. An agent run can never be a dry run (the channel refuses
		// one at creation), so agentRun forces it false rather than carrying a
		// flag the agent path has no meaning for.
		DryRun: sourceRun.DryRun && !agentRun,
	}
	newRun, created, err := s.repo.CreateRunWithTasks(ctx, runIn, planned)
	if err != nil {
		// Every planned insert lost the in-flight race to a concurrent run, so
		// nothing was committed. That is the same fact as the pre-check's
		// target_in_flight, just observed one step later, so report it the same
		// way instead of turning a complete answer into an opaque 409.
		if de, ok := domain.AsDomain(err); ok && de.Code == "targets_in_flight" {
			for i := range planned {
				res.exclude(sourceTaskIDs[i], ExcludeTargetInFlight,
					"an update for this target started in another run while this retry was being planned")
			}
			return res, nil
		}
		return res, err
	}
	res.RunID = newRun.ID
	res.Created = len(created)

	// Attribute anything the insert dropped. CreateUpdateTask is ON CONFLICT DO
	// NOTHING against the in-flight partial unique index, so a target that went
	// in flight between the pre-check and the insert simply does not come back.
	// Without this the operator would see a run with fewer tasks than they
	// selected and nothing anywhere saying which ones or why.
	if len(created) < len(planned) {
		createdKeys := make(map[InFlightKey]struct{}, len(created))
		for _, t := range created {
			createdKeys[InFlightKey{SiteID: t.SiteID, TargetType: t.TargetType, TargetSlug: t.TargetSlug}] = struct{}{}
		}
		for i, nt := range planned {
			key := InFlightKey{SiteID: nt.SiteID, TargetType: nt.TargetType, TargetSlug: nt.TargetSlug}
			if _, ok := createdKeys[key]; ok {
				continue
			}
			res.exclude(sourceTaskIDs[i], ExcludeTargetInFlight,
				"an update for this target started in another run while this retry was being planned")
		}
	}

	// Enqueue. An agent retry enqueues ONLY its first wave: the retry has proven
	// nothing about this attempt, so it re-runs the whole wave structure with a
	// fresh canary rather than blasting every previously-cancelled site at once.
	// enqueueSet is the same function the create path uses, and the claim-time
	// gate (ClaimAgentWaveTask) is authoritative regardless, so there is no way
	// for a retry to dispatch out of turn even if this enqueue were wrong.
	for _, t := range enqueueSet(created, agentRun) {
		if eerr := s.enqueuer.EnqueueTask(ctx, newRun.TenantID, newRun.ID, t.ID, newRun.DryRun); eerr != nil {
			res.Warning = "the retry run was created but a background job could not be scheduled; some tasks may sit pending until they are reaped"
			return res, eerr
		}
	}
	return res, nil
}

// retrySiteBlock is one site-level reason, reused for every task on that site.
type retrySiteBlock struct {
	Reason  string
	Message string
}

// resolveRetrySites reads each DISTINCT site of the selection once, through the
// same SiteLookup seam Service.CreateRun resolves targets with. It returns the
// usable sites and, separately, the ones that are blocked and why.
//
// One lookup per distinct site, serially, is the same cost Service.CreateRun
// already pays for the same selection: a 21-site retry is 21 lookups. A
// selection spanning thousands of distinct sites is therefore slow in the same
// way creating that run was, and if the request times out nothing has been
// committed yet (the run is written after this step), so the operator can retry
// safely.
func (s *Service) resolveRetrySites(ctx context.Context, tenantID uuid.UUID, candidates []Task) (map[uuid.UUID]SiteInfo, map[uuid.UUID]retrySiteBlock, error) {
	sites := map[uuid.UUID]SiteInfo{}
	blocked := map[uuid.UUID]retrySiteBlock{}
	for _, t := range candidates {
		if _, done := sites[t.SiteID]; done {
			continue
		}
		if _, done := blocked[t.SiteID]; done {
			continue
		}
		si, err := s.sites.GetSiteInfo(ctx, tenantID, t.SiteID)
		if err != nil {
			if de, ok := domain.AsDomain(err); ok && de.Kind == domain.KindNotFound {
				blocked[t.SiteID] = retrySiteBlock{
					Reason:  ExcludeSiteNotFound,
					Message: "this site no longer exists",
				}
				continue
			}
			return nil, nil, err
		}
		if !si.Enrolled {
			blocked[t.SiteID] = retrySiteBlock{
				Reason:  ExcludeSiteNotEnrolled,
				Message: "this site is no longer enrolled, so no command can be delivered to it",
			}
			continue
		}
		sites[t.SiteID] = si
	}
	return sites, blocked, nil
}

// retrySelectionIsAgent reports whether the selection is an agent rollout, and
// refuses a selection that mixes the agent target with any other.
func retrySelectionIsAgent(candidates []Task) (bool, error) {
	agent, other := 0, 0
	for _, t := range candidates {
		if t.TargetType == TargetAgent {
			agent++
			continue
		}
		other++
	}
	if agent > 0 && other > 0 {
		return false, domain.Validation("agent_target_exclusive",
			"the agent self-update target must be the only item in its run: its wave gate is defined over the whole run. Retry the agent tasks separately")
	}
	return agent > 0, nil
}

// agentExclusion turns a non-upgradeable classification into the operator's
// answer, naming the two versions the verdict was reached from.
func agentExclusion(status agentrelease.Status, reported, latest string) (string, string) {
	switch status {
	case agentrelease.StatusCurrent:
		return ExcludeAgentCurrent, fmt.Sprintf(
			"this site reports agent %s and the published agent version is now %s, so it is not behind", reported, latest)
	case agentrelease.StatusIneligible:
		return ExcludeAgentIneligible,
			"this site runs the plugin-directory build of the agent, which ships without a self-updater and is upgraded by the directory"
	default:
		return ExcludeAgentVersionUnknown, fmt.Sprintf(
			"the agent version could not be compared (site reports %q, published is %q), and an unreadable version must never become an upgrade",
			reported, latest)
	}
}

// notRetryableMessage explains why a status is not offered for retry, in the
// operator's terms rather than the enum's.
func notRetryableMessage(status, class string) string {
	switch status {
	case TaskSucceeded:
		return "this update already succeeded; retrying it would re-touch a working site for nothing"
	case TaskPending, TaskRunning:
		return "this update has not finished yet, so there is nothing to retry"
	default:
		return "this update is " + status + " (" + class + ") and cannot be retried"
	}
}

// targetLabel names a task's target the way an operator reads it.
func targetLabel(t Task) string {
	switch t.TargetType {
	case TargetCore:
		return "WordPress core"
	case TargetAgent:
		return "the site agent"
	default:
		return t.TargetType + " " + t.TargetSlug
	}
}

// dedupeIDs removes repeats while preserving the caller's order, so "requested"
// counts distinct tasks and a body that names the same task twice does not
// report a phantom exclusion for it.
func dedupeIDs(ids []uuid.UUID) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ids))
	seen := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		if id == uuid.Nil {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
