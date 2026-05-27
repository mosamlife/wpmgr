package update

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// SiteInfo is the minimal site projection the update service needs to plan and
// execute a run: identity, the agent target URL, and the current component
// versions (used to seed from_version without a round-trip to the agent).
type SiteInfo struct {
	ID         uuid.UUID
	URL        string
	Name       string
	Enrolled   bool
	Components []Component
}

// Component is one installed plugin/theme with its current version.
type Component struct {
	Type    string // "plugin" | "theme"
	Slug    string
	Version string
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

	tasks := s.planTasks(sites, in.Items)
	if len(tasks) == 0 {
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

// planTasks expands (sites × items) into NewTask rows, seeding from_version from
// the site's known component version where available.
func (s *Service) planTasks(sites []SiteInfo, items []Item) []NewTask {
	tasks := make([]NewTask, 0, len(sites)*len(items))
	for _, site := range sites {
		versions := indexVersions(site.Components)
		for _, item := range items {
			slug := item.Slug
			if item.Type == TargetCore {
				slug = "core"
			}
			from := versions[item.Type+"/"+slug]
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
	return tasks
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
