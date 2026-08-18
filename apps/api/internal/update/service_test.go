package update

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// fakeSiteLookup is an in-memory SiteLookup keyed by site ID, used by the
// planning-level CreateRun tests below (no DB needed).
type fakeSiteLookup struct {
	sites map[uuid.UUID]SiteInfo
}

func (f *fakeSiteLookup) GetSiteInfo(_ context.Context, _, siteID uuid.UUID) (SiteInfo, error) {
	si, ok := f.sites[siteID]
	if !ok {
		return SiteInfo{}, domain.NotFound("site_not_found", "no such site")
	}
	return si, nil
}

func (f *fakeSiteLookup) ListSiteInfoByTag(_ context.Context, _ uuid.UUID, _ string) ([]SiteInfo, error) {
	out := make([]SiteInfo, 0, len(f.sites))
	for _, si := range f.sites {
		out = append(out, si)
	}
	return out, nil
}

// fakeCreateRepo is a minimal Repo that implements CreateRunWithTasks and
// ListInFlightTargets — the two methods Service.CreateRun's planning path
// reaches (the latter added for the #131 cross-run dedup guard) — assembling
// Task rows from the NewTask rows planTasks produced so the test can assert
// on the exact composed set. All other methods are unused by CreateRun.
//
// tasks accumulates every task ever created across calls to
// CreateRunWithTasks (mirroring the real update_tasks table across runs), so
// ListInFlightTargets can compute the in-flight set the way the real
// cross-run query (backed by update_tasks_inflight_target_idx) would.
type fakeCreateRepo struct {
	tenantID uuid.UUID
	tasks    []Task
	// runs mirrors update_runs so GetRun/ListTasks can serve the run-detail
	// reads the retry path (GH #336) makes before it plans anything.
	runs []Run
	// dropOnInsert simulates the update_tasks_inflight_target_idx partial unique
	// index rejecting an insert as a no-op (ON CONFLICT ... DO NOTHING): the
	// target went in flight in another run between the service's pre-check and
	// this transaction. The real repo `continue`s over exactly this case.
	dropOnInsert map[InFlightKey]struct{}
}

func (f *fakeCreateRepo) CreateRunWithTasks(_ context.Context, in CreateRunInput, tasks []NewTask) (Run, []Task, error) {
	run := Run{ID: uuid.New(), TenantID: in.TenantID, Status: RunPending, DryRun: in.DryRun}
	out := make([]Task, 0, len(tasks))
	for _, nt := range tasks {
		if _, drop := f.dropOnInsert[(InFlightKey{SiteID: nt.SiteID, TargetType: nt.TargetType, TargetSlug: nt.TargetSlug})]; drop {
			continue
		}
		t := Task{
			ID:             uuid.New(),
			RunID:          run.ID,
			TenantID:       in.TenantID,
			SiteID:         nt.SiteID,
			TargetType:     nt.TargetType,
			TargetSlug:     nt.TargetSlug,
			DesiredVersion: nt.DesiredVersion,
			FromVersion:    nt.FromVersion,
			Status:         TaskPending,
		}
		out = append(out, t)
		f.tasks = append(f.tasks, t)
	}
	if len(out) == 0 {
		// Mirrors the real repo: a run with zero tasks is never committed.
		return Run{}, nil, domain.Conflict("targets_in_flight", "the selected updates already have an update in progress in another run")
	}
	f.runs = append(f.runs, run)
	return run, out, nil
}

// seedRun records a run and its tasks as if a previous run had produced them,
// so a retry test can start from any mix of terminal states.
func (f *fakeCreateRepo) seedRun(tenantID uuid.UUID, dryRun bool, tasks []Task) Run {
	run := Run{ID: uuid.New(), TenantID: tenantID, Status: RunCompleted, DryRun: dryRun}
	for i := range tasks {
		if tasks[i].ID == uuid.Nil {
			tasks[i].ID = uuid.New()
		}
		tasks[i].RunID = run.ID
		tasks[i].TenantID = tenantID
		f.tasks = append(f.tasks, tasks[i])
	}
	f.runs = append(f.runs, run)
	return run
}

// completeTask marks every tracked task for (siteID, targetType, targetSlug)
// as terminal (succeeded), simulating the real update_tasks_inflight_target_idx
// partial index dropping the row once its status leaves pending/running — so
// a SEQUENTIAL re-update after completion is no longer blocked.
func (f *fakeCreateRepo) completeTask(siteID uuid.UUID, targetType, targetSlug string) {
	for i := range f.tasks {
		t := &f.tasks[i]
		if t.SiteID == siteID && t.TargetType == targetType && t.TargetSlug == targetSlug {
			t.Status = TaskSucceeded
		}
	}
}

func (f *fakeCreateRepo) ListInFlightTargets(_ context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID) (map[InFlightKey]struct{}, error) {
	allowed := make(map[uuid.UUID]struct{}, len(siteIDs))
	for _, id := range siteIDs {
		allowed[id] = struct{}{}
	}
	out := map[InFlightKey]struct{}{}
	for _, t := range f.tasks {
		if t.TenantID != tenantID {
			continue
		}
		if _, ok := allowed[t.SiteID]; !ok {
			continue
		}
		if t.Status != TaskPending && t.Status != TaskRunning {
			continue
		}
		out[InFlightKey{SiteID: t.SiteID, TargetType: t.TargetType, TargetSlug: t.TargetSlug}] = struct{}{}
	}
	return out, nil
}

func (f *fakeCreateRepo) GetRun(_ context.Context, tenantID, runID uuid.UUID) (Run, error) {
	for _, r := range f.runs {
		if r.ID == runID && r.TenantID == tenantID {
			return r, nil
		}
	}
	return Run{}, domain.NotFound("update_run_not_found", "update run not found")
}
func (f *fakeCreateRepo) ListRuns(context.Context, uuid.UUID, int32, int32) ([]Run, error) {
	panic("not used")
}
func (f *fakeCreateRepo) ListRunSummaries(context.Context, uuid.UUID, int32, int32) ([]RunSummary, error) {
	panic("not used")
}
func (f *fakeCreateRepo) ListTasks(_ context.Context, tenantID, runID uuid.UUID) ([]Task, error) {
	out := make([]Task, 0, len(f.tasks))
	for _, t := range f.tasks {
		if t.RunID == runID && t.TenantID == tenantID {
			out = append(out, t)
		}
	}
	return out, nil
}
func (f *fakeCreateRepo) GetTask(context.Context, uuid.UUID, uuid.UUID) (Task, error) {
	panic("not used")
}
func (f *fakeCreateRepo) MarkTaskRunning(context.Context, uuid.UUID, uuid.UUID, time.Duration) (Task, error) {
	panic("not used")
}
func (f *fakeCreateRepo) FinishTask(context.Context, FinishTaskInput) (Task, error) {
	panic("not used")
}
func (f *fakeCreateRepo) SetRunStatus(context.Context, uuid.UUID, uuid.UUID, string) (Run, error) {
	panic("not used")
}
func (f *fakeCreateRepo) CountUnfinishedTasks(context.Context, uuid.UUID, uuid.UUID) (int64, error) {
	panic("not used")
}
func (f *fakeCreateRepo) CountRunningTasksForTenant(context.Context, uuid.UUID) (int64, error) {
	panic("not used")
}
func (f *fakeCreateRepo) ListStaleUpdateTasks(context.Context, time.Duration, int32) ([]Task, error) {
	panic("not used")
}
func (f *fakeCreateRepo) SiteHasRunningTask(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, time.Duration) (bool, error) {
	panic("not used")
}
func (f *fakeCreateRepo) DeferTaskToPending(context.Context, DeferTaskInput) (Task, error) {
	panic("not used")
}

// countingEnqueuer records how many tasks were enqueued without touching River.
type countingEnqueuer struct{ n int }

func (e *countingEnqueuer) EnqueueTask(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, bool) error {
	e.n++
	return nil
}

// TestIndexPendingDropsSameVersionPhantom proves GH #210/#211's planTasks
// authority checkpoint: indexPending (which planTasks intersects a run's
// requested items against) excludes a component/core advisory whose
// NewVersion normalizes equal to its own Version — a same-version phantom
// (e.g. Kadence's observed "1.5.1 -> 1.5.1") must never be treated as
// pending, or planTasks would plan a doomed/no-op update task for it. A
// legitimate newer advisory (both component and core) is unaffected, and an
// advisory on a component reporting an EMPTY installed Version is kept
// (fail open — wpversion.SameVersion returns false when either side is
// empty).
func TestIndexPendingDropsSameVersionPhantom(t *testing.T) {
	site := SiteInfo{
		ID: uuid.New(), Enrolled: true,
		Components: []Component{
			// Phantom: NewVersion == Version — must not appear as pending.
			{Type: TargetPlugin, Slug: "kadence", Version: "1.5.1", UpdateAvailable: true, NewVersion: "1.5.1"},
			// Legitimate newer update — must appear as pending.
			{Type: TargetPlugin, Slug: "wp-rocket", Version: "3.16.1", UpdateAvailable: true, NewVersion: "3.16.2"},
			// Fail-open: empty installed Version must never suppress a real update.
			{Type: TargetPlugin, Slug: "mystery", Version: "", UpdateAvailable: true, NewVersion: "2.0"},
		},
		CoreUpdateAvailable: true,
		CoreCurrentVersion:  "6.4.3",
		CoreNewVersion:      "6.4.3", // phantom core advisory
	}

	pending := indexPending(site)

	if _, ok := pending[TargetPlugin+"/kadence"]; ok {
		t.Fatalf("same-version phantom must not be indexed as pending: %+v", pending)
	}
	if _, ok := pending[TargetCore+"/core"]; ok {
		t.Fatalf("same-version phantom core advisory must not be indexed as pending: %+v", pending)
	}
	if v, ok := pending[TargetPlugin+"/wp-rocket"]; !ok || v != "3.16.2" {
		t.Fatalf("legitimate newer advisory must be indexed as pending: %+v", pending)
	}
	if v, ok := pending[TargetPlugin+"/mystery"]; !ok || v != "2.0" {
		t.Fatalf("fail-open (empty installed version) advisory must be indexed as pending: %+v", pending)
	}
}

// TestCreateRunNoTaskForSameVersionPhantomUpdate is the end-to-end companion
// to TestIndexPendingDropsSameVersionPhantom: a site whose only "pending"
// component is a same-version phantom produces NO task through the full
// CreateRun -> planTasks -> indexPending path, exactly like a site with
// nothing pending at all.
func TestCreateRunNoTaskForSameVersionPhantomUpdate(t *testing.T) {
	tenant := uuid.New()
	siteA := uuid.New()
	lookup := &fakeSiteLookup{sites: map[uuid.UUID]SiteInfo{
		siteA: {
			ID: siteA, Enrolled: true,
			Components: []Component{
				{Type: TargetPlugin, Slug: "kadence", Version: "1.5.1", UpdateAvailable: true, NewVersion: "1.5.1"},
			},
		},
	}}
	svc := NewService(&fakeCreateRepo{}, lookup, &countingEnqueuer{}, domain.NewValidator(), domain.SystemClock{})

	_, _, err := svc.CreateRun(context.Background(), CreateRunInput{
		TenantID: tenant,
		SiteIDs:  []uuid.UUID{siteA},
		Items:    []Item{{Type: "plugin", Slug: "kadence", Version: "latest"}},
	})
	if err == nil {
		t.Fatal("want an error: a same-version phantom advisory must not plan a task")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindValidation {
		t.Fatalf("want a KindValidation domain error, got %v", err)
	}
}

// TestCreateRunIntersectsPerSitePendingUpdates is the regression test for
// #126 (Problem 1: the N×M task-explosion bug). Two sites are selected in one
// run: site A has 3 of its own pending updates, site B has 20 (2 of which
// overlap with A's). The operator's item selection is the UNION across both
// sites (mirroring what the web wizard sends today). CreateRun must produce
// tasks ONLY for the per-site INTERSECTION — never the raw (sites × items)
// cross-product — and each task's from_version/desired_version must come from
// the site whose task it is, not from another site's inventory.
func TestCreateRunIntersectsPerSitePendingUpdates(t *testing.T) {
	tenant := uuid.New()
	siteA := uuid.New()
	siteB := uuid.New()

	// Site A: 3 pending updates (x, y, and the shared "shared-plugin").
	infoA := SiteInfo{
		ID: siteA, Enrolled: true,
		Components: []Component{
			{Type: TargetPlugin, Slug: "x", Version: "1.0.0", UpdateAvailable: true, NewVersion: "1.1.0"},
			{Type: TargetPlugin, Slug: "y", Version: "2.0.0", UpdateAvailable: true, NewVersion: "2.1.0"},
			{Type: TargetPlugin, Slug: "shared-plugin", Version: "3.0.0", UpdateAvailable: true, NewVersion: "3.1.0"},
			// z is installed but NOT pending on A — must never produce a task on A.
			{Type: TargetPlugin, Slug: "z", Version: "4.0.0"},
		},
	}

	// Site B: 20 pending updates, including "shared-plugin" (at a DIFFERENT
	// from_version than site A) and "z" (which A does not have pending).
	bComponents := []Component{
		{Type: TargetPlugin, Slug: "shared-plugin", Version: "3.0.0", UpdateAvailable: true, NewVersion: "3.2.0"},
		{Type: TargetPlugin, Slug: "z", Version: "9.0.0", UpdateAvailable: true, NewVersion: "9.1.0"},
	}
	for i := 0; i < 18; i++ {
		bComponents = append(bComponents, Component{
			Type: TargetPlugin, Slug: "b-only-" + string(rune('a'+i)), Version: "1.0.0",
			UpdateAvailable: true, NewVersion: "1.0.1",
		})
	}
	infoB := SiteInfo{ID: siteB, Enrolled: true, Components: bComponents}

	lookup := &fakeSiteLookup{sites: map[uuid.UUID]SiteInfo{siteA: infoA, siteB: infoB}}
	repo := &fakeCreateRepo{tenantID: tenant}
	enq := &countingEnqueuer{}
	svc := NewService(repo, lookup, enq, domain.NewValidator(), domain.SystemClock{})

	// The operator's selection is the UNION of everything with an update
	// across both sites: x, y, z, shared-plugin, and the 18 b-only-* items —
	// 22 distinct items total, exactly mirroring what the web wizard's
	// componentOptions() union-builder sends today.
	items := []Item{
		{Type: "plugin", Slug: "x", Version: "latest"},
		{Type: "plugin", Slug: "y", Version: "latest"},
		{Type: "plugin", Slug: "z", Version: "latest"},
		{Type: "plugin", Slug: "shared-plugin", Version: "latest"},
	}
	for i := 0; i < 18; i++ {
		items = append(items, Item{Type: "plugin", Slug: "b-only-" + string(rune('a'+i)), Version: "latest"})
	}

	run, tasks, err := svc.CreateRun(context.Background(), CreateRunInput{
		TenantID: tenant,
		SiteIDs:  []uuid.UUID{siteA, siteB},
		Items:    items,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if run.TenantID != tenant {
		t.Fatalf("run tenant = %s, want %s", run.TenantID, tenant)
	}

	// The cross-product would be 2 sites * 22 items = 44 tasks. The correct
	// intersection is: A gets {x, y, shared-plugin} = 3, B gets
	// {shared-plugin, z, 18x b-only} = 20. Total 23 — NOT 44, and NOT the
	// literal "~22" from the issue's 2-site/3-vs-20 example because here the
	// two sites additionally overlap on one item (shared-plugin), which
	// rightly produces one task per site (each intersects independently).
	wantTotal := 23
	if len(tasks) != wantTotal {
		t.Fatalf("want %d tasks (per-site intersection), got %d: %+v", wantTotal, len(tasks), tasks)
	}

	bySite := map[uuid.UUID][]Task{}
	for _, tk := range tasks {
		bySite[tk.SiteID] = append(bySite[tk.SiteID], tk)
	}

	if got := len(bySite[siteA]); got != 3 {
		t.Fatalf("site A task count = %d, want 3 (its own pending set)", got)
	}
	if got := len(bySite[siteB]); got != 20 {
		t.Fatalf("site B task count = %d, want 20 (its own pending set)", got)
	}

	// Site A must NEVER get a task for "z" — z is pending only on B.
	for _, tk := range bySite[siteA] {
		if tk.TargetSlug == "z" {
			t.Fatalf("site A got a task for %q, which is only pending on site B (the N×M explosion bug)", tk.TargetSlug)
		}
	}
	// Site B must NEVER get a task for "x" or "y" — those are pending only on A.
	for _, tk := range bySite[siteB] {
		if tk.TargetSlug == "x" || tk.TargetSlug == "y" {
			t.Fatalf("site B got a task for %q, which is only pending on site A (the N×M explosion bug)", tk.TargetSlug)
		}
	}

	// Each site's "shared-plugin" task must carry ITS OWN from_version, not
	// the other site's — proves version data isn't cross-contaminated by the
	// intersection logic.
	fromVersionOn := func(site uuid.UUID, slug string) string {
		for _, tk := range bySite[site] {
			if tk.TargetSlug == slug {
				return tk.FromVersion
			}
		}
		t.Fatalf("no task for %q on site %s", slug, site)
		return ""
	}
	if v := fromVersionOn(siteA, "shared-plugin"); v != "3.0.0" {
		t.Fatalf("site A shared-plugin from_version = %q, want 3.0.0 (site A's own installed version)", v)
	}
	if v := fromVersionOn(siteB, "shared-plugin"); v != "3.0.0" {
		t.Fatalf("site B shared-plugin from_version = %q, want 3.0.0 (site B's own installed version)", v)
	}

	if enq.n != wantTotal {
		t.Fatalf("enqueued %d tasks, want %d (one enqueue per created task)", enq.n, wantTotal)
	}
}

// TestCreateRunNoTasksWhenNothingPendingAnywhere: selecting an item that is
// pending on NO targeted site must fail with a clear domain error rather than
// silently creating doomed tasks.
func TestCreateRunNoTasksWhenNothingPendingAnywhere(t *testing.T) {
	tenant := uuid.New()
	siteA := uuid.New()
	// Site A has "akismet" installed but with NO pending update.
	lookup := &fakeSiteLookup{sites: map[uuid.UUID]SiteInfo{
		siteA: {
			ID: siteA, Enrolled: true,
			Components: []Component{{Type: TargetPlugin, Slug: "akismet", Version: "1.0.0"}},
		},
	}}
	svc := NewService(&fakeCreateRepo{}, lookup, &countingEnqueuer{}, domain.NewValidator(), domain.SystemClock{})

	_, _, err := svc.CreateRun(context.Background(), CreateRunInput{
		TenantID: tenant,
		SiteIDs:  []uuid.UUID{siteA},
		Items:    []Item{{Type: "plugin", Slug: "akismet", Version: "latest"}},
	})
	if err == nil {
		t.Fatal("want an error when the selection produces no tasks on any targeted site")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindValidation {
		t.Fatalf("want a KindValidation domain error, got %v", err)
	}
}

// TestCreateRunExplicitPinAppliesRegardlessOfPendingFlag proves an explicit
// version pin (e.g. vuln.Service.Remediate applying a fixed_version, or a
// deliberate downgrade) still produces a task when the target is installed
// on the site, even though the site's own inventory does not currently flag
// it as pending an update — the #126 intersection only gates the DEFAULT
// ("latest"/unset) case, not a forced explicit pin.
func TestCreateRunExplicitPinAppliesRegardlessOfPendingFlag(t *testing.T) {
	tenant := uuid.New()
	siteID := uuid.New()
	lookup := &fakeSiteLookup{sites: map[uuid.UUID]SiteInfo{
		siteID: {
			ID: siteID, Enrolled: true,
			// Installed, but no AvailableUpdate is flagged — mirrors a vuln fix
			// version WordPress's own update-check has not yet caught up to.
			Components: []Component{{Type: TargetPlugin, Slug: "vulnerable-plugin", Version: "1.0.0"}},
		},
	}}
	svc := NewService(&fakeCreateRepo{}, lookup, &countingEnqueuer{}, domain.NewValidator(), domain.SystemClock{})

	_, tasks, err := svc.CreateRun(context.Background(), CreateRunInput{
		TenantID: tenant,
		SiteIDs:  []uuid.UUID{siteID},
		Items:    []Item{{Type: "plugin", Slug: "vulnerable-plugin", Version: "1.0.1"}},
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("want 1 task for an explicit pin on an installed target, got %d", len(tasks))
	}
	if tasks[0].DesiredVersion != "1.0.1" {
		t.Fatalf("desired_version = %q, want the pinned 1.0.1", tasks[0].DesiredVersion)
	}
	if tasks[0].FromVersion != "1.0.0" {
		t.Fatalf("from_version = %q, want the site's own installed 1.0.0", tasks[0].FromVersion)
	}

	// The same pin against a site that does NOT have the target installed at
	// all must produce no task.
	otherSite := uuid.New()
	lookup.sites[otherSite] = SiteInfo{ID: otherSite, Enrolled: true}
	_, _, err = svc.CreateRun(context.Background(), CreateRunInput{
		TenantID: tenant,
		SiteIDs:  []uuid.UUID{otherSite},
		Items:    []Item{{Type: "plugin", Slug: "vulnerable-plugin", Version: "1.0.1"}},
	})
	if err == nil {
		t.Fatal("want an error: the pinned target is not installed on the site at all")
	}
}

// TestCreateRunSkipsDuplicateInFlightTarget is the regression test for the
// #131 hardening: multiple update runs targeting the SAME (site, target) must
// not be allowed to have concurrently in-flight tasks. A scheduled
// auto-update, an operator bulk update, and a portal trigger for the same
// (site, plugin) racing within the same window must produce exactly ONE
// in-flight task — never two tasks the worker could run concurrently (which
// would race the agent's rollback-snapshot pruning and could run concurrent
// WordPress Plugin_Upgrader instances against the same plugin directory).
//
// A SEQUENTIAL re-update — a new run for the same target created AFTER the
// prior run's task reached a terminal state — must still succeed; only
// pending/running tasks block a duplicate.
func TestCreateRunSkipsDuplicateInFlightTarget(t *testing.T) {
	tenant := uuid.New()
	siteID := uuid.New()
	lookup := &fakeSiteLookup{sites: map[uuid.UUID]SiteInfo{
		siteID: {
			ID: siteID, Enrolled: true,
			Components: []Component{
				{Type: TargetPlugin, Slug: "akismet", Version: "1.0.0", UpdateAvailable: true, NewVersion: "1.1.0"},
			},
		},
	}}
	repo := &fakeCreateRepo{tenantID: tenant}
	svc := NewService(repo, lookup, &countingEnqueuer{}, domain.NewValidator(), domain.SystemClock{})
	items := []Item{{Type: "plugin", Slug: "akismet", Version: "latest"}}

	// First run creates the (only) in-flight task for this target.
	run1, tasks1, err := svc.CreateRun(context.Background(), CreateRunInput{
		TenantID: tenant, SiteIDs: []uuid.UUID{siteID}, Items: items,
	})
	if err != nil {
		t.Fatalf("first create run: %v", err)
	}
	if len(tasks1) != 1 {
		t.Fatalf("first run task count = %d, want 1", len(tasks1))
	}

	// A second run for the SAME (site, target) while the first task is still
	// pending/running must NOT create a duplicate task — it must be rejected.
	_, _, err = svc.CreateRun(context.Background(), CreateRunInput{
		TenantID: tenant, SiteIDs: []uuid.UUID{siteID}, Items: items,
	})
	if err == nil {
		t.Fatal("want an error: the target already has an in-flight task from another run")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindConflict {
		t.Fatalf("want a KindConflict domain error, got %v", err)
	}
	if got := len(repo.tasks); got != 1 {
		t.Fatalf("total tasks ever created = %d, want 1 (no duplicate in-flight task)", got)
	}

	// Once the first run's task reaches a terminal state, a NEW run for the
	// same target is a legitimate sequential re-update and must succeed.
	repo.completeTask(siteID, TargetPlugin, "akismet")
	run2, tasks2, err := svc.CreateRun(context.Background(), CreateRunInput{
		TenantID: tenant, SiteIDs: []uuid.UUID{siteID}, Items: items,
	})
	if err != nil {
		t.Fatalf("create run after the prior task completed: %v", err)
	}
	if len(tasks2) != 1 {
		t.Fatalf("post-completion run task count = %d, want 1", len(tasks2))
	}
	if run2.ID == run1.ID {
		t.Fatal("the post-completion run must be a distinct run from the first")
	}
}

// failingEnqueuer fails every enqueue, standing in for River being unreachable
// at exactly the moment a run has already been committed.
type failingEnqueuer struct{ n int }

func (e *failingEnqueuer) EnqueueTask(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, bool) error {
	e.n++
	return errors.New("river unavailable")
}

// TestCreateRunReturnsTheCommittedRunWhenEnqueueFails pins the contract
// CreateRun documents in its own comment: "a failure here leaves the task
// pending (the caller still gets the run)".
//
// This matters because the run and its tasks are ALREADY COMMITTED by the time
// the enqueue runs. A caller that treats the error as "nothing happened" leaves
// a real run in the database with pending tasks that nothing will move until
// the reaper fails them, 45 minutes later for ordinary tasks and 6 hours for
// agent ones. The operator, told it failed, creates a second run, which the m88
// in-flight unique index can then reject. A recoverable hiccup becomes a
// product that looks broken.
func TestCreateRunReturnsTheCommittedRunWhenEnqueueFails(t *testing.T) {
	tenant := uuid.New()
	site := uuid.New()

	info := SiteInfo{
		ID: site, Enrolled: true,
		Components: []Component{
			{Type: TargetPlugin, Slug: "akismet", Version: "1.0.0", UpdateAvailable: true, NewVersion: "1.1.0"},
		},
	}
	lookup := &fakeSiteLookup{sites: map[uuid.UUID]SiteInfo{site: info}}
	repo := &fakeCreateRepo{tenantID: tenant}
	enq := &failingEnqueuer{}
	svc := NewService(repo, lookup, enq, domain.NewValidator(), domain.SystemClock{})

	run, tasks, err := svc.CreateRun(context.Background(), CreateRunInput{
		TenantID: tenant,
		SiteIDs:  []uuid.UUID{site},
		Items:    []Item{{Type: TargetPlugin, Slug: "akismet"}},
	})

	if err == nil {
		t.Fatal("want the enqueue error surfaced, got nil")
	}
	// The whole point: the run and its tasks come back ANYWAY.
	if run.ID == uuid.Nil {
		t.Fatal("run.ID is nil: a committed run was thrown away with its error")
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1: the committed tasks must come back too", len(tasks))
	}
	if enq.n == 0 {
		t.Fatal("the enqueuer was never called, so this test proves nothing")
	}
}

// TestCreateRunReturnsNoRunWhenItWasNeverCommitted is the other side of the
// discriminator the handler relies on. A failure BEFORE the commit must return
// a zero run, so "run.ID != uuid.Nil" is a sound test for "this actually
// exists in the database".
func TestCreateRunReturnsNoRunWhenItWasNeverCommitted(t *testing.T) {
	tenant := uuid.New()
	site := uuid.New()

	// Nothing pending anywhere, so planning yields no tasks and CreateRun
	// fails before it ever writes a run.
	info := SiteInfo{
		ID: site, Enrolled: true,
		Components: []Component{{Type: TargetPlugin, Slug: "akismet", Version: "1.0.0"}},
	}
	lookup := &fakeSiteLookup{sites: map[uuid.UUID]SiteInfo{site: info}}
	svc := NewService(&fakeCreateRepo{tenantID: tenant}, lookup, &failingEnqueuer{},
		domain.NewValidator(), domain.SystemClock{})

	run, _, err := svc.CreateRun(context.Background(), CreateRunInput{
		TenantID: tenant,
		SiteIDs:  []uuid.UUID{site},
		Items:    []Item{{Type: TargetPlugin, Slug: "akismet"}},
	})

	if err == nil {
		t.Fatal("want an error when nothing is pending")
	}
	if run.ID != uuid.Nil {
		t.Fatalf("run.ID = %s, want nil: nothing was committed, so nothing may be reported", run.ID)
	}
}
