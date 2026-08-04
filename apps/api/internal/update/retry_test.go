package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// GH #336 retry. Every test below pins one of the properties the design is
// built on: the server decides what may be retried, the retry creates a NEW
// run, enrollment and the published agent version are re-resolved rather than
// copied, an agent retry re-runs the wave structure, and NOTHING is dropped
// without the response saying so.

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// countingSiteLookup wraps fakeSiteLookup to count GetSiteInfo calls, so a test
// can prove the retry resolves each DISTINCT site once rather than once per
// task.
type countingSiteLookup struct {
	inner *fakeSiteLookup
	calls int
}

func (c *countingSiteLookup) GetSiteInfo(ctx context.Context, tenantID, siteID uuid.UUID) (SiteInfo, error) {
	c.calls++
	return c.inner.GetSiteInfo(ctx, tenantID, siteID)
}

func (c *countingSiteLookup) ListSiteInfoByTag(ctx context.Context, tenantID uuid.UUID, tag string) ([]SiteInfo, error) {
	return c.inner.ListSiteInfoByTag(ctx, tenantID, tag)
}

// retryFixture wires a Service over the in-memory repo/lookup/enqueuer used by
// the planning tests, with no DB.
type retryFixture struct {
	svc    *Service
	repo   *fakeCreateRepo
	lookup *fakeSiteLookup
	enq    *countingEnqueuer
	tenant uuid.UUID
}

func newRetryFixture(t *testing.T, sites map[uuid.UUID]SiteInfo) *retryFixture {
	t.Helper()
	tenant := uuid.New()
	repo := &fakeCreateRepo{tenantID: tenant}
	lookup := &fakeSiteLookup{sites: sites}
	enq := &countingEnqueuer{}
	return &retryFixture{
		svc:    NewService(repo, lookup, enq, domain.NewValidator(), domain.SystemClock{}),
		repo:   repo,
		lookup: lookup,
		enq:    enq,
		tenant: tenant,
	}
}

// enrolledSite is the ordinary shape: enrolled, with one plugin installed.
func enrolledSite(id uuid.UUID, slug, version string) SiteInfo {
	return SiteInfo{
		ID: id, Name: "Site " + slug, Enrolled: true,
		Components: []Component{{Type: TargetPlugin, Slug: slug, Version: version}},
	}
}

func pluginTask(siteID uuid.UUID, slug, status string) Task {
	return Task{
		SiteID:         siteID,
		TargetType:     TargetPlugin,
		TargetSlug:     slug,
		DesiredVersion: "latest",
		FromVersion:    "1.0.0",
		Status:         status,
	}
}

// excludedByTask indexes a result's exclusions for assertion.
func excludedByTask(res RetryResult) map[uuid.UUID]RetryExclusion {
	out := make(map[uuid.UUID]RetryExclusion, len(res.Excluded))
	for _, ex := range res.Excluded {
		out[ex.TaskID] = ex
	}
	return out
}

// assertAccountsForEveryTask is THE invariant of the response shape: created +
// excluded == requested. If this ever fails, a task the operator selected went
// missing with nothing anywhere to say so, which is the exact defect this
// endpoint's design exists to prevent.
func assertAccountsForEveryTask(t *testing.T, res RetryResult) {
	t.Helper()
	if res.Created+len(res.Excluded) != res.Requested {
		t.Fatalf("accounting broken: requested=%d created=%d excluded=%d (a selected task vanished silently)",
			res.Requested, res.Created, len(res.Excluded))
	}
}

// ---------------------------------------------------------------------------
// The classifier (A3 / A6)
// ---------------------------------------------------------------------------

// TestRetryClassifyIsTheOneDefinition pins the policy the whole feature reads
// from: which statuses are retryable at all, and what class each one carries.
// The DEFAULT selection a client offers is failed + cancelled, so those two
// must be retryable and must be distinguishable from each other (cancelled was
// never attempted, which is the safer of the two); succeeded, pending and
// running must never be retryable.
func TestRetryClassifyIsTheOneDefinition(t *testing.T) {
	cases := []struct {
		status        string
		wantRetryable bool
		wantClass     string
	}{
		{TaskFailed, true, RetryClassFailed},
		{TaskCancelled, true, RetryClassNeverRan},
		{TaskRolledBack, true, RetryClassReverted},
		{TaskSkipped, true, RetryClassSkipped},
		{TaskSucceeded, false, RetryClassNotApplicable},
		{TaskPending, false, RetryClassNotApplicable},
		{TaskRunning, false, RetryClassNotApplicable},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			retryable, class := retryClassify(tc.status)
			if retryable != tc.wantRetryable {
				t.Fatalf("retryable = %v, want %v", retryable, tc.wantRetryable)
			}
			if class != tc.wantClass {
				t.Fatalf("class = %q, want %q", class, tc.wantClass)
			}
		})
	}

	// The default set is exactly the two "never succeeded" classes, and they are
	// distinct values: a client that wants to default-select them can, and one
	// that wants to warn specifically about a rollback still can too.
	if retryable, _ := retryClassify(TaskFailed); !retryable {
		t.Fatal("failed must be part of the default retry set")
	}
	if retryable, _ := retryClassify(TaskCancelled); !retryable {
		t.Fatal("cancelled must be part of the default retry set")
	}
	if RetryClassFailed == RetryClassNeverRan || RetryClassReverted == RetryClassFailed {
		t.Fatal("the retry classes must be distinguishable from one another")
	}

	// Every terminal status must map to a class other than not_applicable
	// EXCEPT succeeded. This is what keeps the classifier honest when a new
	// terminal status is added: it will be un-retryable by default, but the
	// author of that status has to come here to decide.
	for _, s := range []string{TaskFailed, TaskCancelled, TaskRolledBack, TaskSkipped} {
		if !terminal(s) {
			t.Fatalf("%s must be terminal", s)
		}
	}
}

// TestTaskWireShapeCarriesTheServerDecision proves the run detail response
// carries the site NAME and the server-computed retry decision on every task,
// so a client renders identity and retryability instead of reconstructing them
// (site identity from a paginated site list, retryability from agent-authored
// prose).
func TestTaskWireShapeCarriesTheServerDecision(t *testing.T) {
	base := Task{
		ID: uuid.New(), RunID: uuid.New(), TenantID: uuid.New(), SiteID: uuid.New(),
		SiteName: "客 Client Site", TargetType: TargetPlugin, TargetSlug: "akismet",
	}

	base.Status = TaskCancelled
	out := toAPITask(base)
	if !out.SiteName.IsSet() || out.SiteName.Value != "客 Client Site" {
		t.Fatalf("site_name = %+v, want the task row's own site name", out.SiteName)
	}
	if !out.Retryable {
		t.Fatal("a cancelled task must be reported retryable")
	}
	if string(out.RetryClass) != RetryClassNeverRan {
		t.Fatalf("retry_class = %q, want %q", out.RetryClass, RetryClassNeverRan)
	}

	base.Status = TaskSucceeded
	if out := toAPITask(base); out.Retryable || string(out.RetryClass) != RetryClassNotApplicable {
		t.Fatalf("a succeeded task must never be retryable: retryable=%v class=%q", out.Retryable, out.RetryClass)
	}

	// A task with no joined name (the create response's fresh rows) omits the
	// field rather than sending an empty string that looks like a name.
	base.SiteName = ""
	if out := toAPITask(base); out.SiteName.IsSet() {
		t.Fatal("site_name must be omitted when the server has no name, not sent empty")
	}
}

// ---------------------------------------------------------------------------
// The default set, end to end (A6)
// ---------------------------------------------------------------------------

// TestRetryCreatesANewRunForFailedAndCancelledTasks is the reporter's case: a
// run where the canary failed and cancelled the rest. Retrying failed +
// cancelled must create a NEW run covering exactly those (site, target) pairs,
// leave the source run untouched, and account for every requested task.
func TestRetryCreatesANewRunForFailedAndCancelledTasks(t *testing.T) {
	siteA, siteB, siteC := uuid.New(), uuid.New(), uuid.New()
	fx := newRetryFixture(t, map[uuid.UUID]SiteInfo{
		siteA: enrolledSite(siteA, "akismet", "1.0.0"),
		siteB: enrolledSite(siteB, "akismet", "1.0.0"),
		siteC: enrolledSite(siteC, "akismet", "1.0.0"),
	})

	failed := pluginTask(siteA, "akismet", TaskFailed)
	cancelled := pluginTask(siteB, "akismet", TaskCancelled)
	succeeded := pluginTask(siteC, "akismet", TaskSucceeded)
	source := fx.repo.seedRun(fx.tenant, false, []Task{failed, cancelled, succeeded})

	// seedRun assigned the ids.
	tasks, _ := fx.repo.ListTasks(context.Background(), fx.tenant, source.ID)
	var failedID, cancelledID uuid.UUID
	for _, tk := range tasks {
		switch tk.Status {
		case TaskFailed:
			failedID = tk.ID
		case TaskCancelled:
			cancelledID = tk.ID
		}
	}

	res, err := fx.svc.RetryRun(context.Background(), RetryRunInput{
		TenantID: fx.tenant, RunID: source.ID,
		TaskIDs: []uuid.UUID{failedID, cancelledID},
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	assertAccountsForEveryTask(t, res)
	if res.Requested != 2 || res.Created != 2 || len(res.Excluded) != 0 {
		t.Fatalf("want 2 requested / 2 created / 0 excluded, got %d/%d/%d (%+v)",
			res.Requested, res.Created, len(res.Excluded), res.Excluded)
	}
	if res.RunID == uuid.Nil {
		t.Fatal("a retry that created work must report the new run id")
	}
	if res.RunID == source.ID {
		t.Fatal("a retry must create a NEW run, never reuse the source run id")
	}

	// The source run's tasks are untouched: the failure it records is still
	// there to read.
	after, _ := fx.repo.ListTasks(context.Background(), fx.tenant, source.ID)
	if len(after) != 3 {
		t.Fatalf("source run task count = %d, want 3 (unchanged)", len(after))
	}
	for _, tk := range after {
		if tk.ID == failedID && tk.Status != TaskFailed {
			t.Fatalf("the source run's failed task was mutated to %q", tk.Status)
		}
	}

	// The new run repeats exactly the two selected (site, target) pairs.
	fresh, _ := fx.repo.ListTasks(context.Background(), fx.tenant, res.RunID)
	if len(fresh) != 2 {
		t.Fatalf("new run task count = %d, want 2", len(fresh))
	}
	sites := map[uuid.UUID]bool{}
	for _, tk := range fresh {
		sites[tk.SiteID] = true
		if tk.TargetType != TargetPlugin || tk.TargetSlug != "akismet" {
			t.Fatalf("new task targets %s/%s, want the original plugin/akismet", tk.TargetType, tk.TargetSlug)
		}
		if tk.DesiredVersion != "latest" {
			t.Fatalf("desired_version = %q, want the source task's own target", tk.DesiredVersion)
		}
	}
	if !sites[siteA] || !sites[siteB] || sites[siteC] {
		t.Fatalf("new run must cover exactly the failed + cancelled sites, got %+v", sites)
	}
	if fx.enq.n != 2 {
		t.Fatalf("enqueued %d jobs, want one per created task", fx.enq.n)
	}
}

// TestRetryRefusesTasksThatCanNeverBeRetried: succeeded, pending and running
// are reported back with a reason and create NOTHING. In particular no run is
// created at all, so a selection of only these does not leave an empty run
// sitting pending forever.
func TestRetryRefusesTasksThatCanNeverBeRetried(t *testing.T) {
	site := uuid.New()
	fx := newRetryFixture(t, map[uuid.UUID]SiteInfo{site: enrolledSite(site, "akismet", "1.0.0")})

	source := fx.repo.seedRun(fx.tenant, false, []Task{
		pluginTask(site, "akismet", TaskSucceeded),
		pluginTask(site, "jetpack", TaskPending),
		pluginTask(site, "wordfence", TaskRunning),
	})
	tasks, _ := fx.repo.ListTasks(context.Background(), fx.tenant, source.ID)

	ids := make([]uuid.UUID, 0, len(tasks))
	for _, tk := range tasks {
		ids = append(ids, tk.ID)
	}
	res, err := fx.svc.RetryRun(context.Background(), RetryRunInput{
		TenantID: fx.tenant, RunID: source.ID, TaskIDs: ids,
	})
	if err != nil {
		t.Fatalf("retry must answer, not error: %v", err)
	}
	assertAccountsForEveryTask(t, res)
	if res.Created != 0 || res.RunID != uuid.Nil {
		t.Fatalf("nothing may be created: created=%d run_id=%s", res.Created, res.RunID)
	}
	if len(res.Excluded) != 3 {
		t.Fatalf("want all 3 accounted for, got %d", len(res.Excluded))
	}
	for _, ex := range res.Excluded {
		if ex.Reason != ExcludeNotRetryable {
			t.Fatalf("reason = %q, want %q", ex.Reason, ExcludeNotRetryable)
		}
		if ex.Message == "" {
			t.Fatal("every exclusion must carry a sentence the operator can read")
		}
	}
	if fx.enq.n != 0 {
		t.Fatalf("enqueued %d jobs for a retry that created nothing", fx.enq.n)
	}
}

// TestRetryExcludesATaskThatIsNotPartOfTheRun: an id from another run (or a
// made-up one) is reported, not silently ignored and not a whole-request
// rejection that would lose the rest of the selection.
func TestRetryExcludesATaskThatIsNotPartOfTheRun(t *testing.T) {
	site := uuid.New()
	fx := newRetryFixture(t, map[uuid.UUID]SiteInfo{site: enrolledSite(site, "akismet", "1.0.0")})
	source := fx.repo.seedRun(fx.tenant, false, []Task{pluginTask(site, "akismet", TaskFailed)})
	tasks, _ := fx.repo.ListTasks(context.Background(), fx.tenant, source.ID)

	stranger := uuid.New()
	res, err := fx.svc.RetryRun(context.Background(), RetryRunInput{
		TenantID: fx.tenant, RunID: source.ID,
		// The same real id twice, plus one that is not in this run: repeats
		// collapse (requested counts DISTINCT ids) and the stranger is reported.
		TaskIDs: []uuid.UUID{tasks[0].ID, tasks[0].ID, stranger},
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	assertAccountsForEveryTask(t, res)
	if res.Requested != 2 {
		t.Fatalf("requested = %d, want 2 distinct ids", res.Requested)
	}
	if res.Created != 1 {
		t.Fatalf("created = %d, want 1", res.Created)
	}
	ex := excludedByTask(res)[stranger]
	if ex.Reason != ExcludeNotInRun {
		t.Fatalf("reason = %q, want %q", ex.Reason, ExcludeNotInRun)
	}
}

// TestRetryRunNotFound: a run in another tenant is a 404, exactly as on the
// detail read.
func TestRetryRunNotFound(t *testing.T) {
	fx := newRetryFixture(t, nil)
	_, err := fx.svc.RetryRun(context.Background(), RetryRunInput{
		TenantID: fx.tenant, RunID: uuid.New(), TaskIDs: []uuid.UUID{uuid.New()},
	})
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindNotFound {
		t.Fatalf("want a KindNotFound domain error, got %v", err)
	}
}

// TestRetryRequiresASelection: there is no implicit server-side default set. A
// body naming nothing is a validation error, not a fleet-wide retry.
func TestRetryRequiresASelection(t *testing.T) {
	site := uuid.New()
	fx := newRetryFixture(t, map[uuid.UUID]SiteInfo{site: enrolledSite(site, "akismet", "1.0.0")})
	source := fx.repo.seedRun(fx.tenant, false, []Task{pluginTask(site, "akismet", TaskFailed)})

	_, err := fx.svc.RetryRun(context.Background(), RetryRunInput{TenantID: fx.tenant, RunID: source.ID})
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindValidation || de.Code != "task_ids_required" {
		t.Fatalf("want task_ids_required validation error, got %v", err)
	}
	if len(fx.repo.runs) != 1 {
		t.Fatalf("a refused retry must create no run (runs=%d)", len(fx.repo.runs))
	}
}

// ---------------------------------------------------------------------------
// Re-resolution: enrollment (A4)
// ---------------------------------------------------------------------------

// TestRetryExcludesASiteUnenrolledSinceTheRun proves enrollment is RE-RESOLVED
// and not copied off the task row. Service.CreateRun applies the same gate but
// drops such a site silently; here it is named.
func TestRetryExcludesASiteUnenrolledSinceTheRun(t *testing.T) {
	gone, live := uuid.New(), uuid.New()
	fx := newRetryFixture(t, map[uuid.UUID]SiteInfo{
		// Unenrolled since the run: the row still exists, Enrolled is false.
		gone: {ID: gone, Name: "Departed", Enrolled: false},
		live: enrolledSite(live, "akismet", "1.0.0"),
	})
	source := fx.repo.seedRun(fx.tenant, false, []Task{
		pluginTask(gone, "akismet", TaskFailed),
		pluginTask(live, "akismet", TaskFailed),
	})
	tasks, _ := fx.repo.ListTasks(context.Background(), fx.tenant, source.ID)
	ids := []uuid.UUID{tasks[0].ID, tasks[1].ID}

	res, err := fx.svc.RetryRun(context.Background(), RetryRunInput{
		TenantID: fx.tenant, RunID: source.ID, TaskIDs: ids,
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	assertAccountsForEveryTask(t, res)
	if res.Created != 1 {
		t.Fatalf("created = %d, want 1 (only the still-enrolled site)", res.Created)
	}
	if len(res.Excluded) != 1 || res.Excluded[0].Reason != ExcludeSiteNotEnrolled {
		t.Fatalf("want one site_not_enrolled exclusion, got %+v", res.Excluded)
	}
	if !strings.Contains(res.Excluded[0].Message, "enrolled") {
		t.Fatalf("message must say what happened, got %q", res.Excluded[0].Message)
	}

	// And the created task is the live site's.
	fresh, _ := fx.repo.ListTasks(context.Background(), fx.tenant, res.RunID)
	if len(fresh) != 1 || fresh[0].SiteID != live {
		t.Fatalf("new run must target only the enrolled site, got %+v", fresh)
	}
}

// TestRetryExcludesASiteThatNoLongerResolves: a site lookup that 404s is one
// site's problem, not the whole request's.
func TestRetryExcludesASiteThatNoLongerResolves(t *testing.T) {
	missing, live := uuid.New(), uuid.New()
	fx := newRetryFixture(t, map[uuid.UUID]SiteInfo{live: enrolledSite(live, "akismet", "1.0.0")})
	source := fx.repo.seedRun(fx.tenant, false, []Task{
		pluginTask(missing, "akismet", TaskFailed),
		pluginTask(live, "akismet", TaskCancelled),
	})
	tasks, _ := fx.repo.ListTasks(context.Background(), fx.tenant, source.ID)

	res, err := fx.svc.RetryRun(context.Background(), RetryRunInput{
		TenantID: fx.tenant, RunID: source.ID,
		TaskIDs: []uuid.UUID{tasks[0].ID, tasks[1].ID},
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	assertAccountsForEveryTask(t, res)
	if res.Created != 1 {
		t.Fatalf("created = %d, want 1", res.Created)
	}
	if len(res.Excluded) != 1 || res.Excluded[0].Reason != ExcludeSiteNotFound {
		t.Fatalf("want one site_not_found exclusion, got %+v", res.Excluded)
	}
}

// ---------------------------------------------------------------------------
// Re-resolution: the published agent version (A4)
// ---------------------------------------------------------------------------

// TestRetryAgentExcludesASiteWhoseTargetMoved is the incident case the design
// calls out: the operator REVERTED the published agent release while the
// original rollout was failing. A retry must re-classify every site against the
// version published NOW, exclude the ones that are no longer behind WITH A
// REASON, and record the freshly-resolved version on the tasks it does create.
//
// Copying desired_version off the old task row would upgrade sites to a build
// the control plane has deliberately withdrawn, and produce no signal at all.
func TestRetryAgentExcludesASiteWhoseTargetMoved(t *testing.T) {
	// behind still runs an old build; caughtUp already runs something at least
	// as new as the (reverted) published version.
	behind, caughtUp := uuid.New(), uuid.New()
	fx := newRetryFixture(t, map[uuid.UUID]SiteInfo{
		behind:   {ID: behind, Name: "Behind", Enrolled: true, AgentVersion: "0.61.100"},
		caughtUp: {ID: caughtUp, Name: "Caught up", Enrolled: true, AgentVersion: "0.61.119"},
	})
	// The run was planned against 0.61.119; the release has since been reverted
	// to 0.61.118.
	fx.svc.SetAgentSelfUpdate(true, fixedReleases{version: "0.61.118"})

	agentTask := func(site uuid.UUID, from string) Task {
		return Task{
			SiteID: site, TargetType: TargetAgent, TargetSlug: AgentTargetSlug,
			DesiredVersion: "0.61.119", FromVersion: from, Status: TaskCancelled,
		}
	}
	source := fx.repo.seedRun(fx.tenant, false, []Task{
		agentTask(behind, "0.61.100"),
		agentTask(caughtUp, "0.61.119"),
	})
	tasks, _ := fx.repo.ListTasks(context.Background(), fx.tenant, source.ID)
	bySite := map[uuid.UUID]Task{}
	for _, tk := range tasks {
		bySite[tk.SiteID] = tk
	}

	res, err := fx.svc.RetryRun(context.Background(), RetryRunInput{
		TenantID: fx.tenant, RunID: source.ID,
		TaskIDs: []uuid.UUID{bySite[behind].ID, bySite[caughtUp].ID},
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	assertAccountsForEveryTask(t, res)
	if res.Created != 1 {
		t.Fatalf("created = %d, want 1 (only the site still behind)", res.Created)
	}
	ex := excludedByTask(res)[bySite[caughtUp].ID]
	if ex.Reason != ExcludeAgentCurrent {
		t.Fatalf("reason = %q, want %q", ex.Reason, ExcludeAgentCurrent)
	}
	// The message must name BOTH versions: "why is this one excluded" is the
	// question at 3am, and the answer is the pair.
	if !strings.Contains(ex.Message, "0.61.119") || !strings.Contains(ex.Message, "0.61.118") {
		t.Fatalf("message must name the reported and published versions, got %q", ex.Message)
	}

	// The created task carries the version published NOW, never the row's copy.
	fresh, _ := fx.repo.ListTasks(context.Background(), fx.tenant, res.RunID)
	if len(fresh) != 1 {
		t.Fatalf("new run task count = %d, want 1", len(fresh))
	}
	if fresh[0].DesiredVersion != "0.61.118" {
		t.Fatalf("desired_version = %q, want the RE-RESOLVED published 0.61.118, not the old run's 0.61.119",
			fresh[0].DesiredVersion)
	}
	if fresh[0].FromVersion != "0.61.100" {
		t.Fatalf("from_version = %q, want the version the site reports now", fresh[0].FromVersion)
	}
}

// TestRetryAgentRefusedWhenTheChannelIsOff / when no version is published: both
// are whole-request refusals, because neither is a property of one task.
func TestRetryAgentRefusedWhenTheChannelCannotRun(t *testing.T) {
	site := uuid.New()
	seed := func(fx *retryFixture) (uuid.UUID, uuid.UUID) {
		source := fx.repo.seedRun(fx.tenant, false, []Task{{
			SiteID: site, TargetType: TargetAgent, TargetSlug: AgentTargetSlug,
			DesiredVersion: "0.61.119", FromVersion: "0.61.100", Status: TaskCancelled,
		}})
		tasks, _ := fx.repo.ListTasks(context.Background(), fx.tenant, source.ID)
		return source.ID, tasks[0].ID
	}
	sites := map[uuid.UUID]SiteInfo{site: {ID: site, Enrolled: true, AgentVersion: "0.61.100"}}

	t.Run("channel disabled", func(t *testing.T) {
		fx := newRetryFixture(t, sites)
		runID, taskID := seed(fx)
		_, err := fx.svc.RetryRun(context.Background(), RetryRunInput{
			TenantID: fx.tenant, RunID: runID, TaskIDs: []uuid.UUID{taskID},
		})
		de, ok := domain.AsDomain(err)
		if !ok || de.Code != "agent_self_update_disabled" {
			t.Fatalf("want agent_self_update_disabled, got %v", err)
		}
	})

	t.Run("no published version", func(t *testing.T) {
		fx := newRetryFixture(t, sites)
		fx.svc.SetAgentSelfUpdate(true, fixedReleases{version: ""})
		runID, taskID := seed(fx)
		_, err := fx.svc.RetryRun(context.Background(), RetryRunInput{
			TenantID: fx.tenant, RunID: runID, TaskIDs: []uuid.UUID{taskID},
		})
		de, ok := domain.AsDomain(err)
		if !ok || de.Code != "agent_release_unknown" {
			t.Fatalf("want agent_release_unknown, got %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// The wave structure (A8)
// ---------------------------------------------------------------------------

// TestRetryAgentReRunsTheWaveStructureWithAFreshCanary: retrying the 20 sites a
// canary failure cancelled must NOT dispatch 20 sites at once. It enqueues wave
// 0 only, exactly as an original agent rollout does, so the retry proves itself
// again from one site before anything else is touched.
func TestRetryAgentReRunsTheWaveStructureWithAFreshCanary(t *testing.T) {
	const fleet = 20
	sites := map[uuid.UUID]SiteInfo{}
	seedTasks := make([]Task, 0, fleet)
	for i := 0; i < fleet; i++ {
		id := uuid.New()
		sites[id] = SiteInfo{ID: id, Name: fmt.Sprintf("site-%02d", i), Enrolled: true, AgentVersion: "0.61.100"}
		seedTasks = append(seedTasks, Task{
			SiteID: id, TargetType: TargetAgent, TargetSlug: AgentTargetSlug,
			DesiredVersion: "0.61.119", FromVersion: "0.61.100", Status: TaskCancelled,
		})
	}
	fx := newRetryFixture(t, sites)
	fx.svc.SetAgentSelfUpdate(true, fixedReleases{version: "0.61.119"})
	source := fx.repo.seedRun(fx.tenant, false, seedTasks)

	tasks, _ := fx.repo.ListTasks(context.Background(), fx.tenant, source.ID)
	ids := make([]uuid.UUID, 0, len(tasks))
	for _, tk := range tasks {
		ids = append(ids, tk.ID)
	}

	res, err := fx.svc.RetryRun(context.Background(), RetryRunInput{
		TenantID: fx.tenant, RunID: source.ID, TaskIDs: ids,
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	assertAccountsForEveryTask(t, res)
	if res.Created != fleet {
		t.Fatalf("created = %d, want %d", res.Created, fleet)
	}

	// THE property: one job, not twenty. The wave plan for 20 tasks is
	// canary [0,1), pilot [1,4), remainder [4,20); only the canary is enqueued
	// now, and the claim-time gate holds the rest.
	if fx.enq.n != waveCanarySize {
		t.Fatalf("enqueued %d jobs for a %d-site agent retry, want %d (the canary only): a retry must not blast the fleet",
			fx.enq.n, fleet, waveCanarySize)
	}

	// And the new run's own wave state agrees: wave 0 open, everything else shut.
	fresh, _ := fx.repo.ListTasks(context.Background(), fx.tenant, res.RunID)
	st := DeriveAgentWaveState(fresh)
	if st.Halt {
		t.Fatal("a freshly created retry run must not start halted")
	}
	if st.OpenThrough != 1 {
		t.Fatalf("OpenThrough = %d, want 1: only the canary wave may be open on a fresh run", st.OpenThrough)
	}
	if len(st.Waves) != 3 {
		t.Fatalf("wave plan = %+v, want three waves for %d tasks", st.Waves, fleet)
	}
	for i, tk := range st.Order {
		if i < waveCanarySize {
			continue
		}
		if st.GateOpenFor(tk.ID) {
			t.Fatalf("task at position %d is dispatchable on a fresh retry run: the wave gate was bypassed", i)
		}
	}
}

// TestRetryRefusesToMixTheAgentTargetWithAnythingElse: the wave gate is defined
// over "the run", so a mixed selection has no meaningful canary and no
// denominator. A source run cannot mix them, so this only guards a hand-built
// request, which is exactly when a guard matters.
func TestRetryRefusesToMixTheAgentTargetWithAnythingElse(t *testing.T) {
	site := uuid.New()
	fx := newRetryFixture(t, map[uuid.UUID]SiteInfo{
		site: {ID: site, Name: "Mixed", Enrolled: true, AgentVersion: "0.61.100",
			Components: []Component{{Type: TargetPlugin, Slug: "akismet", Version: "1.0.0"}}},
	})
	fx.svc.SetAgentSelfUpdate(true, fixedReleases{version: "0.61.119"})
	source := fx.repo.seedRun(fx.tenant, false, []Task{
		pluginTask(site, "akismet", TaskFailed),
		{SiteID: site, TargetType: TargetAgent, TargetSlug: AgentTargetSlug,
			DesiredVersion: "0.61.119", FromVersion: "0.61.100", Status: TaskCancelled},
	})
	tasks, _ := fx.repo.ListTasks(context.Background(), fx.tenant, source.ID)

	_, err := fx.svc.RetryRun(context.Background(), RetryRunInput{
		TenantID: fx.tenant, RunID: source.ID,
		TaskIDs: []uuid.UUID{tasks[0].ID, tasks[1].ID},
	})
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "agent_target_exclusive" {
		t.Fatalf("want agent_target_exclusive, got %v", err)
	}
	if len(fx.repo.runs) != 1 {
		t.Fatalf("a refused retry must create no run (runs=%d)", len(fx.repo.runs))
	}
}

// ---------------------------------------------------------------------------
// Partial exclusion is REPORTED, never dropped (A5)
// ---------------------------------------------------------------------------

// TestRetryReportsATargetAlreadyInFlight: the (site, target) has a
// pending/running task in another run, so this one cannot be created. The
// operator must see which, not just a smaller run.
func TestRetryReportsATargetAlreadyInFlight(t *testing.T) {
	busy, free := uuid.New(), uuid.New()
	fx := newRetryFixture(t, map[uuid.UUID]SiteInfo{
		busy: enrolledSite(busy, "akismet", "1.0.0"),
		free: enrolledSite(free, "akismet", "1.0.0"),
	})
	source := fx.repo.seedRun(fx.tenant, false, []Task{
		pluginTask(busy, "akismet", TaskFailed),
		pluginTask(free, "akismet", TaskFailed),
	})
	// Another run already has an in-flight task for (busy, plugin/akismet).
	fx.repo.seedRun(fx.tenant, false, []Task{pluginTask(busy, "akismet", TaskRunning)})

	tasks, _ := fx.repo.ListTasks(context.Background(), fx.tenant, source.ID)
	bySite := map[uuid.UUID]Task{}
	for _, tk := range tasks {
		bySite[tk.SiteID] = tk
	}

	res, err := fx.svc.RetryRun(context.Background(), RetryRunInput{
		TenantID: fx.tenant, RunID: source.ID,
		TaskIDs: []uuid.UUID{bySite[busy].ID, bySite[free].ID},
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	assertAccountsForEveryTask(t, res)
	if res.Created != 1 {
		t.Fatalf("created = %d, want 1", res.Created)
	}
	ex := excludedByTask(res)[bySite[busy].ID]
	if ex.Reason != ExcludeTargetInFlight {
		t.Fatalf("reason = %q, want %q", ex.Reason, ExcludeTargetInFlight)
	}
	if !strings.Contains(ex.Message, "akismet") {
		t.Fatalf("message must name the target, got %q", ex.Message)
	}
}

// TestRetryReportsARowTheInsertDropped is the harder half of the same problem:
// the pre-check passed, and the (site, target) went in flight in the window
// before the insert, so the partial unique index turned that insert into a
// no-op. The repo `continue`s over it, which used to mean a run with fewer
// tasks than the operator selected and nothing anywhere saying which.
func TestRetryReportsARowTheInsertDropped(t *testing.T) {
	raced, fine := uuid.New(), uuid.New()
	fx := newRetryFixture(t, map[uuid.UUID]SiteInfo{
		raced: enrolledSite(raced, "akismet", "1.0.0"),
		fine:  enrolledSite(fine, "akismet", "1.0.0"),
	})
	source := fx.repo.seedRun(fx.tenant, false, []Task{
		pluginTask(raced, "akismet", TaskFailed),
		pluginTask(fine, "akismet", TaskFailed),
	})
	tasks, _ := fx.repo.ListTasks(context.Background(), fx.tenant, source.ID)
	bySite := map[uuid.UUID]Task{}
	for _, tk := range tasks {
		bySite[tk.SiteID] = tk
	}
	// The index rejects this one at insert time, invisible to the pre-check.
	fx.repo.dropOnInsert = map[InFlightKey]struct{}{
		{SiteID: raced, TargetType: TargetPlugin, TargetSlug: "akismet"}: {},
	}

	res, err := fx.svc.RetryRun(context.Background(), RetryRunInput{
		TenantID: fx.tenant, RunID: source.ID,
		TaskIDs: []uuid.UUID{bySite[raced].ID, bySite[fine].ID},
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	assertAccountsForEveryTask(t, res)
	if res.Requested != 2 || res.Created != 1 {
		t.Fatalf("want 2 requested / 1 created, got %d/%d", res.Requested, res.Created)
	}
	ex := excludedByTask(res)[bySite[raced].ID]
	if ex.Reason != ExcludeTargetInFlight {
		t.Fatalf("the dropped row must be attributed back to the task the operator selected, got %+v", res.Excluded)
	}
	if res.RunID == uuid.Nil {
		t.Fatal("the run that WAS created must still be reported")
	}
}

// TestRetryReportsWhenEveryRowLostTheInsertRace: nothing was committed, so
// there is no run to report, but the answer is still complete rather than an
// opaque 409.
func TestRetryReportsWhenEveryRowLostTheInsertRace(t *testing.T) {
	site := uuid.New()
	fx := newRetryFixture(t, map[uuid.UUID]SiteInfo{site: enrolledSite(site, "akismet", "1.0.0")})
	source := fx.repo.seedRun(fx.tenant, false, []Task{pluginTask(site, "akismet", TaskFailed)})
	tasks, _ := fx.repo.ListTasks(context.Background(), fx.tenant, source.ID)
	fx.repo.dropOnInsert = map[InFlightKey]struct{}{
		{SiteID: site, TargetType: TargetPlugin, TargetSlug: "akismet"}: {},
	}

	res, err := fx.svc.RetryRun(context.Background(), RetryRunInput{
		TenantID: fx.tenant, RunID: source.ID, TaskIDs: []uuid.UUID{tasks[0].ID},
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	assertAccountsForEveryTask(t, res)
	if res.RunID != uuid.Nil || res.Created != 0 {
		t.Fatalf("nothing was committed: run_id=%s created=%d", res.RunID, res.Created)
	}
	if len(res.Excluded) != 1 || res.Excluded[0].Reason != ExcludeTargetInFlight {
		t.Fatalf("want the collision reported per task, got %+v", res.Excluded)
	}
}

// TestRetryReportsTheRunWhenAnEnqueueFails pins the same contract the create
// path fixed in 0.61.117: the run and its tasks are COMMITTED before the
// enqueue, so a failure there must still report the run. Telling the operator
// nothing happened would leave real pending tasks nobody goes looking for.
func TestRetryReportsTheRunWhenAnEnqueueFails(t *testing.T) {
	site := uuid.New()
	tenant := uuid.New()
	repo := &fakeCreateRepo{tenantID: tenant}
	svc := NewService(repo, &fakeSiteLookup{sites: map[uuid.UUID]SiteInfo{
		site: enrolledSite(site, "akismet", "1.0.0"),
	}}, &failingEnqueuer{}, domain.NewValidator(), domain.SystemClock{})

	source := repo.seedRun(tenant, false, []Task{pluginTask(site, "akismet", TaskFailed)})
	tasks, _ := repo.ListTasks(context.Background(), tenant, source.ID)

	res, err := svc.RetryRun(context.Background(), RetryRunInput{
		TenantID: tenant, RunID: source.ID, TaskIDs: []uuid.UUID{tasks[0].ID},
	})
	if err == nil {
		t.Fatal("want the enqueue failure surfaced")
	}
	if res.RunID == uuid.Nil {
		t.Fatal("a committed run was thrown away with its error")
	}
	if res.Created != 1 {
		t.Fatalf("created = %d, want 1: the committed task must be reported", res.Created)
	}
	if res.Warning == "" {
		t.Fatal("the operator must be told the run exists but its jobs may not be scheduled")
	}
}

// TestRetryInheritsTheSourceRunsDryRunFlag: retrying a rehearsal is another
// rehearsal. A dry run that quietly became a live change would be the worst
// possible surprise from a button labelled "retry".
func TestRetryInheritsTheSourceRunsDryRunFlag(t *testing.T) {
	site := uuid.New()
	fx := newRetryFixture(t, map[uuid.UUID]SiteInfo{site: enrolledSite(site, "akismet", "1.0.0")})
	source := fx.repo.seedRun(fx.tenant, true /* dry run */, []Task{pluginTask(site, "akismet", TaskFailed)})
	tasks, _ := fx.repo.ListTasks(context.Background(), fx.tenant, source.ID)

	res, err := fx.svc.RetryRun(context.Background(), RetryRunInput{
		TenantID: fx.tenant, RunID: source.ID, TaskIDs: []uuid.UUID{tasks[0].ID},
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	newRun, err := fx.repo.GetRun(context.Background(), fx.tenant, res.RunID)
	if err != nil {
		t.Fatalf("get new run: %v", err)
	}
	if !newRun.DryRun {
		t.Fatal("retrying a dry run must produce another dry run, never a live change")
	}
}

// ---------------------------------------------------------------------------
// Scale (the 21 / 300 / 2000 question)
// ---------------------------------------------------------------------------

// TestRetryAtScaleAccountsForEveryTask: 2000 tasks in one request is accepted
// and answered completely. It also pins the cost shape: sites are resolved once
// per DISTINCT site, not once per task, so a 2000-task run over 100 sites is
// 100 lookups and not 2000.
func TestRetryAtScaleAccountsForEveryTask(t *testing.T) {
	const (
		siteCount     = 100
		targetsPerSit = 20
		taskCount     = siteCount * targetsPerSit // 2000
	)
	sites := map[uuid.UUID]SiteInfo{}
	seedTasks := make([]Task, 0, taskCount)
	for i := 0; i < siteCount; i++ {
		id := uuid.New()
		comps := make([]Component, 0, targetsPerSit)
		for j := 0; j < targetsPerSit; j++ {
			comps = append(comps, Component{Type: TargetPlugin, Slug: fmt.Sprintf("plugin-%02d", j), Version: "1.0.0"})
		}
		sites[id] = SiteInfo{ID: id, Name: fmt.Sprintf("site-%03d", i), Enrolled: true, Components: comps}
		for j := 0; j < targetsPerSit; j++ {
			seedTasks = append(seedTasks, pluginTask(id, fmt.Sprintf("plugin-%02d", j), TaskFailed))
		}
	}

	tenant := uuid.New()
	repo := &fakeCreateRepo{tenantID: tenant}
	lookup := &countingSiteLookup{inner: &fakeSiteLookup{sites: sites}}
	enq := &countingEnqueuer{}
	svc := NewService(repo, lookup, enq, domain.NewValidator(), domain.SystemClock{})

	source := repo.seedRun(tenant, false, seedTasks)
	tasks, _ := repo.ListTasks(context.Background(), tenant, source.ID)
	if len(tasks) != taskCount {
		t.Fatalf("seeded %d tasks, want %d", len(tasks), taskCount)
	}
	ids := make([]uuid.UUID, 0, len(tasks))
	for _, tk := range tasks {
		ids = append(ids, tk.ID)
	}

	res, err := svc.RetryRun(context.Background(), RetryRunInput{
		TenantID: tenant, RunID: source.ID, TaskIDs: ids,
	})
	if err != nil {
		t.Fatalf("retry %d tasks: %v", taskCount, err)
	}
	assertAccountsForEveryTask(t, res)
	if res.Requested != taskCount || res.Created != taskCount {
		t.Fatalf("want %d requested / %d created, got %d/%d", taskCount, taskCount, res.Requested, res.Created)
	}
	if len(res.Excluded) != 0 {
		t.Fatalf("nothing should be excluded, got %d", len(res.Excluded))
	}
	if enq.n != taskCount {
		t.Fatalf("enqueued %d jobs, want one per created task (%d)", enq.n, taskCount)
	}
	if lookup.calls != siteCount {
		t.Fatalf("resolved %d sites for %d tasks, want one lookup per DISTINCT site (%d)",
			lookup.calls, taskCount, siteCount)
	}
}

// TestRetryRefusesAnAbsurdlyLargeSelection: the request is bounded so a
// hand-built or replayed body cannot ask the server to resolve an unbounded
// number of sites inside one HTTP request. Nothing is created.
func TestRetryRefusesAnAbsurdlyLargeSelection(t *testing.T) {
	site := uuid.New()
	fx := newRetryFixture(t, map[uuid.UUID]SiteInfo{site: enrolledSite(site, "akismet", "1.0.0")})
	source := fx.repo.seedRun(fx.tenant, false, []Task{pluginTask(site, "akismet", TaskFailed)})

	ids := make([]uuid.UUID, maxRetryTaskIDs+1)
	for i := range ids {
		ids[i] = uuid.New()
	}
	_, err := fx.svc.RetryRun(context.Background(), RetryRunInput{
		TenantID: fx.tenant, RunID: source.ID, TaskIDs: ids,
	})
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "too_many_tasks" {
		t.Fatalf("want too_many_tasks, got %v", err)
	}
	if len(fx.repo.runs) != 1 {
		t.Fatalf("a refused retry must create no run (runs=%d)", len(fx.repo.runs))
	}
}

// ---------------------------------------------------------------------------
// The HTTP surface
// ---------------------------------------------------------------------------

// TestRetryHandlerReportsAPartialRetryAsSuccessWithTheAccounting: the response
// body carries the full account on the SAME status code as a complete retry, so
// no client can branch on the status and drop the exclusions. That branch is
// exactly how a partial commit becomes invisible.
func TestRetryHandlerReportsAPartialRetryAsSuccessWithTheAccounting(t *testing.T) {
	gin.SetMode(gin.TestMode)

	live, gone := uuid.New(), uuid.New()
	fx := newRetryFixture(t, map[uuid.UUID]SiteInfo{
		live: enrolledSite(live, "akismet", "1.0.0"),
		gone: {ID: gone, Name: "Departed", Enrolled: false},
	})
	source := fx.repo.seedRun(fx.tenant, false, []Task{
		pluginTask(live, "akismet", TaskFailed),
		pluginTask(gone, "akismet", TaskCancelled),
	})
	tasks, _ := fx.repo.ListTasks(context.Background(), fx.tenant, source.ID)

	h := NewHandler(fx.svc, nil, nil)
	body, _ := json.Marshal(map[string]any{"task_ids": []string{tasks[0].ID.String(), tasks[1].ID.String()}})
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/updates/runs/"+source.ID.String()+"/retry", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(domain.WithTenantID(req.Context(), fx.tenant))
	ctx.Request = req
	ctx.Params = gin.Params{{Key: "id", Value: source.ID.String()}}

	h.retry(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var out gen.UpdateRunRetryResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, rec.Body.String())
	}
	if out.Requested != 2 || out.Created != 1 {
		t.Fatalf("want 2 requested / 1 created, got %d/%d", out.Requested, out.Created)
	}
	if len(out.Excluded) != 1 {
		t.Fatalf("the excluded task must be in the response, got %+v", out.Excluded)
	}
	if string(out.Excluded[0].Reason) != ExcludeSiteNotEnrolled || out.Excluded[0].Message == "" {
		t.Fatalf("exclusion must carry both a machine reason and a readable sentence, got %+v", out.Excluded[0])
	}
	if !out.RunID.IsSet() {
		t.Fatal("the new run id must be returned so the client can navigate to it")
	}
}

// TestRetryHandlerRejectsAMalformedRunID: a bad path id is a validation error,
// not a 500 and not a lookup.
func TestRetryHandlerRejectsAMalformedRunID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := newRetryFixture(t, nil)
	h := NewHandler(fx.svc, nil, nil)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/updates/runs/not-a-uuid/retry", strings.NewReader(`{"task_ids":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(domain.WithTenantID(req.Context(), fx.tenant))
	ctx.Request = req
	ctx.Params = gin.Params{{Key: "id", Value: "not-a-uuid"}}

	h.retry(ctx)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body: %s)", rec.Code, rec.Body.String())
	}
}
