package update

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// The agent must never be updated through an update run: the task would have
// the agent overwrite its own running code, and the snapshot/rollback that
// guards every other update cannot be delivered by the process being replaced.
// The control plane therefore refuses the target at the planning boundary,
// independently of the agent's own refusal.
//
// The keys below are the real inventory keys WordPress reports for the two
// agent distributions, pinned literally so a rename of the shared constants
// cannot silently stop matching what a site actually sends.
const (
	agentSelfHostedKey = "wpmgr-agent/wpmgr-agent.php"
	agentDirectoryKey  = "fleet-agent-site-manager/fleet-agent-site-manager.php"

	// The key a site reports when the release asset was unpacked verbatim
	// instead of into the folder name the Makefile chose. No slug allowlist can
	// enumerate this form, so only the plugin-header name below identifies it.
	agentRenamedKey = "wpmgr-agent-0.61.88/wpmgr-agent.php"

	// The self-hosted build's plugin-header name, as WordPress reports it.
	agentHeaderName = "WPMgr Agent"
)

// TestCreateRunRejectsAgentSelfUpdateTarget proves the refusal is a clear
// domain error at the planning boundary, not a silent drop: every form the
// agent's plugin key can take is rejected, whether the item asks for "latest"
// or pins an explicit version (a pin bypasses the pending-set intersection, so
// it must be refused on its own).
//
// Each subcase seeds the site's inventory with a pending advisory for the EXACT
// key under test, and leaves its header name empty so the slug matcher is the
// only thing that can refuse it. Without that, a key absent from the site's
// pending set would produce no task for a wholly unrelated reason (nothing to
// plan) and the subcase would assert an error code while demonstrating no
// prevention at all.
func TestCreateRunRejectsAgentSelfUpdateTarget(t *testing.T) {
	cases := []struct {
		name    string
		slug    string
		version string
	}{
		{"self hosted plugin file", agentSelfHostedKey, "latest"},
		{"plugin directory build", agentDirectoryKey, "latest"},
		{"bare directory key", "wpmgr-agent", "latest"},
		{"mixed case key", "WPMGR-Agent/WPMGR-Agent.php", "latest"},
		{"explicit version pin", agentSelfHostedKey, "0.62.0"},
		{"unset version", agentSelfHostedKey, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenant := uuid.New()
			siteID := uuid.New()
			lookup := &fakeSiteLookup{sites: map[uuid.UUID]SiteInfo{
				siteID: {
					ID: siteID, Enrolled: true,
					Components: []Component{
						// A stale advisory an older agent persisted for itself,
						// under the very key this subcase asks to update: both
						// the pending ("latest") and the installed (version
						// pin) planning branches would have produced a task.
						{Type: TargetPlugin, Slug: tc.slug, Version: "0.61.88",
							UpdateAvailable: true, NewVersion: "0.62.0"},
					},
				},
			}}
			enq := &countingEnqueuer{}
			svc := NewService(&fakeCreateRepo{}, lookup, enq, domain.NewValidator(), domain.SystemClock{})

			_, tasks, err := svc.CreateRun(context.Background(), CreateRunInput{
				TenantID: tenant,
				SiteIDs:  []uuid.UUID{siteID},
				Items:    []Item{{Type: TargetPlugin, Slug: tc.slug, Version: tc.version}},
			})
			if err == nil {
				t.Fatalf("want a refusal, got %d tasks", len(tasks))
			}
			de, ok := domain.AsDomain(err)
			if !ok || de.Kind != domain.KindValidation {
				t.Fatalf("want a KindValidation domain error, got %v", err)
			}
			if de.Code != "agent_self_update_forbidden" {
				t.Fatalf("want code agent_self_update_forbidden, got %q (%s)", de.Code, de.Message)
			}
			if len(tasks) != 0 {
				t.Fatalf("no task may be created for the agent: %+v", tasks)
			}
			if enq.n != 0 {
				t.Fatalf("no job may be enqueued for the agent: %d", enq.n)
			}
		})
	}
}

// TestCreateRunPlansNoTaskForAgentUnderRenamedDirectory covers the case a slug
// allowlist structurally cannot: the agent installed under a directory name
// nobody predicted. validateItems sees only a bare slug and correctly lets such
// an item through (it cannot tell it from any other plugin), so prevention here
// is the pending authority's job, and what must be proven is that NO task and
// NO job are produced.
//
// The control subcase is what makes this a proof rather than a coincidence: the
// same key, the same pending advisory, the same request, differing ONLY in the
// plugin-header name, does plan its task. So the missing task in the first
// subcase is caused by recognizing the agent, not by anything about the slug.
func TestCreateRunPlansNoTaskForAgentUnderRenamedDirectory(t *testing.T) {
	cases := []struct {
		name      string
		header    string
		wantTasks int
	}{
		{"agent under a renamed directory is never planned", agentHeaderName, 0},
		{"an ordinary plugin under the same key is planned", "Some Other Plugin", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenant := uuid.New()
			siteID := uuid.New()
			lookup := &fakeSiteLookup{sites: map[uuid.UUID]SiteInfo{
				siteID: {
					ID: siteID, Enrolled: true,
					Components: []Component{
						{Type: TargetPlugin, Slug: agentRenamedKey, Name: tc.header,
							Version: "0.61.88", UpdateAvailable: true, NewVersion: "0.62.0"},
					},
				},
			}}
			enq := &countingEnqueuer{}
			svc := NewService(&fakeCreateRepo{}, lookup, enq, domain.NewValidator(), domain.SystemClock{})

			_, tasks, err := svc.CreateRun(context.Background(), CreateRunInput{
				TenantID: tenant,
				SiteIDs:  []uuid.UUID{siteID},
				Items:    []Item{{Type: TargetPlugin, Slug: agentRenamedKey, Version: "latest"}},
			})

			if tc.wantTasks == 0 {
				if err == nil {
					t.Fatalf("want no planned task, got %d tasks", len(tasks))
				}
				de, ok := domain.AsDomain(err)
				if !ok || de.Code != "no_tasks" {
					t.Fatalf("want the planner to produce nothing (no_tasks), got %v", err)
				}
				if len(tasks) != 0 {
					t.Fatalf("no task may be created for the agent: %+v", tasks)
				}
				if enq.n != 0 {
					t.Fatalf("no job may be enqueued for the agent: %d", enq.n)
				}
				return
			}

			if err != nil {
				t.Fatalf("the control plugin must still be planned: %v", err)
			}
			if len(tasks) != tc.wantTasks {
				t.Fatalf("got %d tasks, want %d", len(tasks), tc.wantTasks)
			}
		})
	}
}

// TestIndexPendingExcludesAgentSelfUpdate pins the planner's pending authority:
// even when a site's stored inventory still advertises an agent self-update
// (an older agent, or a replayed payload), it is never counted as pending, so a
// "latest" item can never resolve into the one task that cannot be rolled back.
// Ordinary components are unaffected.
func TestIndexPendingExcludesAgentSelfUpdate(t *testing.T) {
	site := SiteInfo{
		ID: uuid.New(), Enrolled: true,
		Components: []Component{
			{Type: TargetPlugin, Slug: agentSelfHostedKey, Name: agentHeaderName, Version: "0.61.88", UpdateAvailable: true, NewVersion: "0.62.0"},
			{Type: TargetPlugin, Slug: agentDirectoryKey, Name: "Fleet Agent Site Manager", Version: "0.61.88", UpdateAvailable: true, NewVersion: "0.62.0"},
			// Recognized by its plugin header alone: the directory name is one
			// no allowlist could have contained.
			{Type: TargetPlugin, Slug: agentRenamedKey, Name: agentHeaderName, Version: "0.61.88", UpdateAvailable: true, NewVersion: "0.62.0"},
			// An older agent that never reported a header name: the slug still
			// has to carry the exclusion.
			{Type: TargetPlugin, Slug: agentSelfHostedKey, Version: "0.61.88", UpdateAvailable: true, NewVersion: "0.62.0"},
			{Type: TargetPlugin, Slug: "akismet/akismet.php", Name: "Akismet Anti-spam", Version: "5.3.1", UpdateAvailable: true, NewVersion: "5.3.2"},
			// A neighbouring name must NOT be swept in: withholding a real
			// update from a third-party plugin is the opposite failure.
			{Type: TargetPlugin, Slug: "wpmgr-agent-pro/wpmgr-agent-pro.php", Name: "WPMgr Agent Pro", Version: "2.0.0", UpdateAvailable: true, NewVersion: "2.0.1"},
		},
	}

	pending := indexPending(site)

	for _, key := range []string{agentSelfHostedKey, agentDirectoryKey, agentRenamedKey} {
		if _, ok := pending[TargetPlugin+"/"+key]; ok {
			t.Fatalf("the agent must never be indexed as pending (%s): %+v", key, pending)
		}
	}
	if v := pending[TargetPlugin+"/akismet/akismet.php"]; v != "5.3.2" {
		t.Fatalf("an ordinary pending update must be unaffected: %+v", pending)
	}
	if v := pending[TargetPlugin+"/wpmgr-agent-pro/wpmgr-agent-pro.php"]; v != "2.0.1" {
		t.Fatalf("a plugin merely named like the agent must keep its update: %+v", pending)
	}
}

// selfUpdateFakeRepo serves one pre-existing task row (as if it had been
// created before the planning-boundary guard existed) and otherwise reuses
// probeFakeRepo's finish-path fakes.
type selfUpdateFakeRepo struct {
	probeFakeRepo
	task Task
}

func (f *selfUpdateFakeRepo) GetTask(context.Context, uuid.UUID, uuid.UUID) (Task, error) {
	return f.task, nil
}

// CountRunningTasksForTenant is reached only by the renamed-directory case,
// which has to travel past the slug pre-check to get to the site lookup.
func (f *selfUpdateFakeRepo) CountRunningTasksForTenant(context.Context, uuid.UUID) (int64, error) {
	return 0, nil
}

// TestWorkerRefusesToDispatchAgentSelfUpdateTask covers the last line of
// defense: a persisted or replayed task naming the agent is finished as skipped
// without ever contacting the site. The Worker is built with a nil Commander
// and a nil SiteLookup on purpose, so any attempt to dispatch would panic.
func TestWorkerRefusesToDispatchAgentSelfUpdateTask(t *testing.T) {
	task := Task{
		ID:          uuid.New(),
		RunID:       uuid.New(),
		TenantID:    uuid.New(),
		SiteID:      uuid.New(),
		TargetType:  TargetPlugin,
		TargetSlug:  agentSelfHostedKey,
		FromVersion: "0.61.88",
		Status:      TaskPending,
	}
	repo := &selfUpdateFakeRepo{task: task}
	w := NewWorker(repo, nil, nil, nil, nil, nil, nil, 5, 0)

	err := w.Work(context.Background(), &river.Job[TaskArgs]{
		Args: TaskArgs{TenantID: task.TenantID, RunID: task.RunID, TaskID: task.ID},
	})
	if err != nil {
		t.Fatalf("Work: %v", err)
	}

	if len(repo.finished) != 1 {
		t.Fatalf("want exactly one terminal transition, got %d", len(repo.finished))
	}
	if got := repo.finished[0].Status; got != TaskSkipped {
		t.Fatalf("status = %q, want %q", got, TaskSkipped)
	}
	if repo.finished[0].Detail == "" {
		t.Fatal("the refusal must record a detail the operator can read")
	}
}

// TestWorkerRefusesToDispatchAgentUnderRenamedDirectory covers the same last
// line of defense for a target whose slug gives nothing away. The worker gets
// past the slug pre-check, resolves the site, and recognizes the agent in the
// site's OWN inventory by its plugin header. The Worker is built with a nil
// Commander, so a dispatch would panic rather than quietly pass; MarkTaskRunning
// panics too, which pins the refusal as happening before the task is ever moved
// out of pending.
func TestWorkerRefusesToDispatchAgentUnderRenamedDirectory(t *testing.T) {
	task := Task{
		ID:          uuid.New(),
		RunID:       uuid.New(),
		TenantID:    uuid.New(),
		SiteID:      uuid.New(),
		TargetType:  TargetPlugin,
		TargetSlug:  agentRenamedKey,
		FromVersion: "0.61.88",
		Status:      TaskPending,
	}
	repo := &selfUpdateFakeRepo{task: task}
	lookup := &fakeSiteLookup{sites: map[uuid.UUID]SiteInfo{
		task.SiteID: {
			ID: task.SiteID, Enrolled: true,
			Components: []Component{
				{Type: TargetPlugin, Slug: agentRenamedKey, Name: agentHeaderName, Version: "0.61.88"},
			},
		},
	}}
	w := NewWorker(repo, lookup, nil, nil, nil, nil, nil, 5, 0)

	err := w.Work(context.Background(), &river.Job[TaskArgs]{
		Args: TaskArgs{TenantID: task.TenantID, RunID: task.RunID, TaskID: task.ID},
	})
	if err != nil {
		t.Fatalf("Work: %v", err)
	}

	if len(repo.finished) != 1 {
		t.Fatalf("want exactly one terminal transition, got %d", len(repo.finished))
	}
	if got := repo.finished[0].Status; got != TaskSkipped {
		t.Fatalf("status = %q, want %q", got, TaskSkipped)
	}
}

// TestWorkerDispatchesOrdinaryPluginUnderAgentLikeKey is the control for the
// test above: the identical task and site, with only the plugin-header name
// changed, is NOT refused. It reaches MarkTaskRunning, which the fake panics
// on, proving the refusal above came from recognizing the agent and not from
// the target key or the inventory lookup itself.
func TestWorkerDispatchesOrdinaryPluginUnderAgentLikeKey(t *testing.T) {
	task := Task{
		ID:          uuid.New(),
		RunID:       uuid.New(),
		TenantID:    uuid.New(),
		SiteID:      uuid.New(),
		TargetType:  TargetPlugin,
		TargetSlug:  agentRenamedKey,
		FromVersion: "1.0.0",
		Status:      TaskPending,
	}
	repo := &selfUpdateFakeRepo{task: task}
	lookup := &fakeSiteLookup{sites: map[uuid.UUID]SiteInfo{
		task.SiteID: {
			ID: task.SiteID, Enrolled: true,
			Components: []Component{
				{Type: TargetPlugin, Slug: agentRenamedKey, Name: "Some Other Plugin", Version: "1.0.0"},
			},
		},
	}}
	w := NewWorker(repo, lookup, nil, nil, nil, nil, nil, 5, 0)

	defer func() {
		if recover() == nil {
			t.Fatal("an ordinary plugin must not be refused as the agent")
		}
		if len(repo.finished) != 0 {
			t.Fatalf("an ordinary plugin must not be finished as skipped: %+v", repo.finished)
		}
	}()

	_ = w.Work(context.Background(), &river.Job[TaskArgs]{
		Args: TaskArgs{TenantID: task.TenantID, RunID: task.RunID, TaskID: task.ID},
	})
}
