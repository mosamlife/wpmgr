package update

import (
	"context"
	"strings"

	"github.com/google/uuid"

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
}

// Component is one installed plugin/theme with its current version and its
// OWN pending-update advisory (mirrors site.Component.AvailableUpdate).
// UpdateAvailable/NewVersion are the per-site authority planTasks intersects
// a run's requested items against (#126: a target requested because another
// site in the run has it pending must not spawn a task on a site that does
// not).
type Component struct {
	Type    string // "plugin" | "theme"
	Slug    string
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

// Service holds the update orchestration logic.
type Service struct {
	repo      Repo
	sites     SiteLookup
	enqueuer  Enqueuer
	validator *domain.Validator
	clock     domain.Clock
}

// NewService builds an update Service.
func NewService(repo Repo, sites SiteLookup, enqueuer Enqueuer, v *domain.Validator, clock domain.Clock) *Service {
	return &Service{repo: repo, sites: sites, enqueuer: enqueuer, validator: v, clock: clock}
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

	tasks, skippedInFlight := s.planTasks(sites, in.Items, inFlight)
	if len(tasks) == 0 {
		if skippedInFlight > 0 {
			return Run{}, nil, domain.Conflict("targets_in_flight", "the selected updates already have an update in progress in another run")
		}
		return Run{}, nil, domain.Validation("no_tasks", "the selection produced no update tasks")
	}

	run, createdTasks, err := s.repo.CreateRunWithTasks(ctx, in, tasks)
	if err != nil {
		return Run{}, nil, err
	}

	// Enqueue one background job per task. A best-effort enqueue: a failure here
	// leaves the task pending (the caller still gets the run); we surface the
	// first enqueue error so the operator can retry.
	for _, t := range createdTasks {
		if eerr := s.enqueuer.EnqueueTask(ctx, run.TenantID, run.ID, t.ID, run.DryRun); eerr != nil {
			return run, createdTasks, eerr
		}
	}
	return run, createdTasks, nil
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
