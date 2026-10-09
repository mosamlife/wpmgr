package update

// post_update_refresh_test.go: GH #892. Whether an update requests the
// post-update inventory refresh follows whether the site changed, not the
// task status. An update that applied and was not rolled back (core left as
// is, a refused rollback, an undeliverable rollback) is a failed task on a
// changed site, and refreshes it. A core apply the agent reported as no
// change requests nothing.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// postUpdateRefreshRecorder records every refresh the worker enqueues.
type postUpdateRefreshRecorder struct{ calls []RefreshInventoryArgs }

func (r *postUpdateRefreshRecorder) EnqueueRefresh(_ context.Context, a RefreshInventoryArgs) error {
	r.calls = append(r.calls, a)
	return nil
}

// refusingRollbackCommander answers every Rollback with the agent's ok=false.
type refusingRollbackCommander struct{ itemCommander }

func (c *refusingRollbackCommander) Rollback(_ context.Context, _ uuid.UUID, _ string, req agentcmd.RollbackRequest) (agentcmd.RollbackResponse, error) {
	c.rollbacks = append(c.rollbacks, req)
	return agentcmd.RollbackResponse{OK: false, Log: "snapshot missing"}, nil
}

// newRefreshTestWorker is newApplyTestWorker with the refresher wired, an
// enrolled site for task, and a repo whose finished row carries the task's
// site, as FinishTask's real row does.
func newRefreshTestWorker(task Task, cmd Commander, prober HealthProber) (*Worker, *probeFakeRepo, *postUpdateRefreshRecorder) {
	repo := &probeFakeRepo{}
	repo.finishTask = func(in FinishTaskInput) (Task, error) {
		t := task
		t.Status = in.Status
		t.FromVersion = in.FromVersion
		t.ToVersion = in.ToVersion
		t.Detail = in.Detail
		t.Error = in.Error
		return t, nil
	}
	w := newApplyTestWorker(repo, cmd, prober)
	w.sites = &fakeSiteLookup{sites: map[uuid.UUID]SiteInfo{
		task.SiteID: {ID: task.SiteID, URL: "https://example.test", Enrolled: true},
	}}
	rec := &postUpdateRefreshRecorder{}
	w.SetRefreshEnqueuer(rec, nil)
	return w, repo, rec
}

func pluginUpdated() agentcmd.ItemResult {
	return agentcmd.ItemResult{Type: TargetPlugin, Slug: "suremail", FromVersion: "1.9.9", ToVersion: "2.0.0", Status: agentcmd.ItemSucceeded, SnapshotID: "snap-1"}
}

// TestRunApply_PostUpdateRefresh_FollowsWhetherTheSiteChanged: each failed
// update that left the site changed requests one refresh while its task stays
// failed, a rolled-back or healthy update still requests one, and a failure
// that changed nothing requests none. wantDetail is the start of the
// recorded detail, which pins the branch each case takes.
func TestRunApply_PostUpdateRefresh_FollowsWhetherTheSiteChanged(t *testing.T) {
	cases := []struct {
		name        string
		task        Task
		item        agentcmd.UpdateItem
		cmd         func() Commander
		probe       probeStep
		wantStatus  string
		wantDetail  string
		wantRefresh bool
	}{
		{
			name:        "core left as is after an unreachable homepage",
			task:        coreTask(),
			item:        coreItem(),
			cmd:         func() Commander { return &itemCommander{result: coreUpdated()} },
			probe:       errorStep(context.DeadlineExceeded),
			wantStatus:  TaskFailed,
			wantDetail:  coreLeftAsIsDetail,
			wantRefresh: true,
		},
		{
			name:        "plugin rollback refused by the agent",
			task:        testTask(),
			item:        updateItem(),
			cmd:         func() Commander { return &refusingRollbackCommander{itemCommander{result: pluginUpdated()}} },
			probe:       unhealthyStep(503),
			wantStatus:  TaskFailed,
			wantDetail:  "rollback REFUSED by agent after unhealthy update",
			wantRefresh: true,
		},
		{
			name: "plugin rollback that could not be delivered",
			task: testTask(),
			item: updateItem(),
			cmd: func() Commander {
				return &itemCommander{result: pluginUpdated(), rollbackErr: errors.New("dial tcp: connection refused")}
			},
			probe:       unhealthyStep(503),
			wantStatus:  TaskFailed,
			wantDetail:  "rollback FAILED after unhealthy update",
			wantRefresh: true,
		},
		{
			name: "core rollback that could not be delivered after a confirmed crash",
			task: coreTask(),
			item: coreItem(),
			cmd: func() Commander {
				return &itemCommander{result: coreUpdated(), rollbackErr: errors.New("dial tcp: connection refused")}
			},
			probe:       unhealthyStep(500),
			wantStatus:  TaskFailed,
			wantDetail:  coreRollbackUndeliverableDetail,
			wantRefresh: true,
		},
		{
			name:        "plugin rolled back",
			task:        testTask(),
			item:        updateItem(),
			cmd:         func() Commander { return &itemCommander{result: pluginUpdated()} },
			probe:       unhealthyStep(503),
			wantStatus:  TaskRolledBack,
			wantRefresh: true,
		},
		{
			name:        "plugin updated and healthy",
			task:        testTask(),
			item:        updateItem(),
			cmd:         func() Commander { return &itemCommander{result: pluginUpdated()} },
			probe:       healthyStep(),
			wantStatus:  TaskSucceeded,
			wantRefresh: true,
		},
		{
			name:        "core reported no change, then failed its check",
			task:        coreTask(),
			item:        coreItem(),
			cmd:         func() Commander { return &itemCommander{result: coreReportedUpToDate()} },
			probe:       errorStep(context.DeadlineExceeded),
			wantStatus:  TaskFailed,
			wantDetail:  coreNoChangeUnhealthyDetail,
			wantRefresh: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, repo, rec := newRefreshTestWorker(tc.task, tc.cmd(), &scriptedProber{script: []probeStep{tc.probe}})

			if err := w.runApply(context.Background(), tc.task, "https://example.test", tc.item); err != nil {
				t.Fatalf("runApply: %v", err)
			}
			got := onlyFinish(t, repo)
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (detail %q)", got.Status, tc.wantStatus, got.Detail)
			}
			if tc.wantDetail != "" && !strings.HasPrefix(got.Detail, tc.wantDetail) {
				t.Fatalf("detail = %q, want it to start with %q", got.Detail, tc.wantDetail)
			}
			if !tc.wantRefresh {
				if len(rec.calls) != 0 {
					t.Fatalf("refreshes enqueued = %+v, want none", rec.calls)
				}
				return
			}
			if len(rec.calls) != 1 {
				t.Fatalf("refreshes enqueued = %d (%+v), want exactly 1", len(rec.calls), rec.calls)
			}
			want := RefreshInventoryArgs{TenantID: tc.task.TenantID, SiteID: tc.task.SiteID, SiteURL: "https://example.test", Source: "post_update"}
			if rec.calls[0] != want {
				t.Fatalf("refresh = %+v, want %+v", rec.calls[0], want)
			}
		})
	}
}
