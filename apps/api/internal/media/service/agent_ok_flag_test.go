package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/model"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/repo"
)

// These tests cover a control-plane blind spot shared by every media dispatch:
// the agent answers HTTP 200 (so the transport error is nil) with an ack body of
// {"ok":false}, meaning it REFUSED the command. All of this work is asynchronous
// and the agent reports completion by calling a CP status endpoint, so a refused
// dispatch means no callback ever arrives. Branching only on the transport error
// therefore left the batch reported as queued and its jobs running forever.

// refusingAgent acks every command with a 200 whose body says ok=false.
type refusingAgent struct {
	optimized chan struct{}
}

func (a *refusingAgent) MediaOptimize(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaOptimizeRequest) (agentcmd.MediaOptimizeResponse, error) {
	if a.optimized != nil {
		a.optimized <- struct{}{}
	}
	return agentcmd.MediaOptimizeResponse{OK: false, Detail: "body must be an empty object"}, nil
}

func (a *refusingAgent) MediaSync(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaSyncRequest) (agentcmd.MediaSyncResponse, error) {
	return agentcmd.MediaSyncResponse{OK: false, Detail: "body must be an empty object"}, nil
}

func (a *refusingAgent) MediaRestore(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaRestoreRequest) (agentcmd.MediaRestoreResponse, error) {
	return agentcmd.MediaRestoreResponse{OK: false, Detail: "snapshot unavailable"}, nil
}

func (a *refusingAgent) MediaDeleteOriginals(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaDeleteOriginalsRequest) (agentcmd.MediaDeleteOriginalsResponse, error) {
	return agentcmd.MediaDeleteOriginalsResponse{OK: false, Detail: "refused"}, nil
}

func (a *refusingAgent) SyncMediaConfig(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaConfigRequest) (agentcmd.MediaConfigResult, error) {
	return agentcmd.MediaConfigResult{OK: true, Detail: "applied"}, nil
}

// TestSync_AgentOkFalseIsNotReportedAsStarted proves a media sync the agent
// refused surfaces as an error instead of a started sync whose job never ends.
func TestSync_AgentOkFalseIsNotReportedAsStarted(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	r := newFakeRepo()
	svc := newTestService(r, &fakeStore{}, &fakeEnqueuer{}, &refusingAgent{}, fakeSites{enrolled: true})

	if _, err := svc.Sync(context.Background(), tenantID, siteID, userPrincipal(tenantID)); err == nil {
		t.Fatal("a 200 carrying ok=false is the agent refusing the sync; Sync must not report success")
	}
	assertNoJobLeftRunning(t, r)
}

// TestStartRestore_AgentOkFalseIsNotReportedAsQueued proves a refused restore is
// reported as a failure rather than a queued batch. Claiming the originals are
// being restored when the agent refused is the visible half of the harm.
func TestStartRestore_AgentOkFalseIsNotReportedAsQueued(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	r := newFakeRepo()
	a1 := model.Asset{ID: uuid.New(), SiteID: siteID, WPAttachmentID: 1, OriginalMime: "image/jpeg", Status: model.AssetOptimized}
	r.assets[a1.ID] = a1
	svc := newTestService(r, &fakeStore{}, &fakeEnqueuer{}, &refusingAgent{}, fakeSites{enrolled: true})

	if _, err := svc.StartRestore(context.Background(), tenantID, siteID, []uuid.UUID{a1.ID}, userPrincipal(tenantID)); err == nil {
		t.Fatal("a restore the agent refused must not be reported as queued")
	}
	assertNoJobLeftRunning(t, r)
}

// TestStartDeleteOriginals_AgentOkFalseIsNotReportedAsQueued covers the
// irreversible command. The operator's destructive consent is already audited by
// the time the ack arrives, so silently reporting a refused delete as queued
// leaves the audit trail claiming an action that never happened.
func TestStartDeleteOriginals_AgentOkFalseIsNotReportedAsQueued(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	r := newFakeRepo()
	a1 := model.Asset{ID: uuid.New(), SiteID: siteID, WPAttachmentID: 1, OriginalMime: "image/jpeg", Status: model.AssetOptimized}
	r.assets[a1.ID] = a1
	svc := newTestService(r, &fakeStore{}, &fakeEnqueuer{}, &refusingAgent{}, fakeSites{enrolled: true})

	if _, err := svc.StartDeleteOriginals(context.Background(), tenantID, siteID, []uuid.UUID{a1.ID}, userPrincipal(tenantID)); err == nil {
		t.Fatal("a delete-originals the agent refused must not be reported as queued")
	}
	assertNoJobLeftRunning(t, r)
}

// TestStartOptimize_AgentOkFalseFailsTheJobs proves the detached optimize
// dispatch fails its jobs on a refusal, exactly as it already did on a transport
// error, so the UI does not show them stuck "optimizing" forever.
func TestStartOptimize_AgentOkFalseFailsTheJobs(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	r := newFakeRepo()
	a1 := model.Asset{ID: uuid.New(), SiteID: siteID, WPAttachmentID: 1, OriginalMime: "image/jpeg", Status: model.AssetPending}
	r.assets[a1.ID] = a1
	agent := &refusingAgent{}
	// The optimize dispatch runs in a DETACHED goroutine, so the assertion must
	// synchronise on the failJob write itself rather than poll the fake repo
	// (which has no mutex and would data-race under -race).
	sig := &signalingRepo{fakeRepo: r, finalized: make(chan repo.FinalizeJobInput, 4)}
	svc := newTestService(sig, &fakeStore{}, &fakeEnqueuer{}, agent, fakeSites{enrolled: true})

	if _, err := svc.StartOptimize(context.Background(), tenantID, siteID, []uuid.UUID{a1.ID}, false, "avif", "lossy", userPrincipal(tenantID)); err != nil {
		t.Fatalf("StartOptimize: %v", err)
	}

	select {
	case in := <-sig.finalized:
		if in.State != model.JobFailed {
			t.Fatalf("expected the job to be failed after the agent refused, got %q", in.State)
		}
		if !strings.Contains(in.ErrorReason, "rejected by agent") {
			t.Fatalf("failure reason must name the refusal, got %q", in.ErrorReason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the agent refused the optimize dispatch but no job was ever failed; it would sit optimizing forever")
	}
}

// signalingRepo wraps fakeRepo so a test can await the detached optimize
// dispatch's failJob write without racing on the fake's unsynchronised maps.
type signalingRepo struct {
	*fakeRepo
	finalized chan repo.FinalizeJobInput
}

func (r *signalingRepo) FinalizeJobAgent(ctx context.Context, jobID string, in repo.FinalizeJobInput) (model.Job, error) {
	j, err := r.fakeRepo.FinalizeJobAgent(ctx, jobID, in)
	r.finalized <- in
	return j, err
}

// assertNoJobLeftRunning fails the test if any job is still in a non-terminal
// state. A refused dispatch that leaves a job running is precisely the silent
// hang this class of bug produces.
func assertNoJobLeftRunning(t *testing.T, r *fakeRepo) {
	t.Helper()
	for id, j := range r.jobs {
		if j.State != model.JobFailed && j.State != model.JobSucceeded &&
			j.State != model.JobPartiallySucceeded && j.State != model.JobCancelled {
			t.Fatalf("job %s left in non-terminal state %q after the agent refused the command", id, j.State)
		}
	}
}
