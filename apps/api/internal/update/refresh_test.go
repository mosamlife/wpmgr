package update

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// fakeRefreshCmd records the last RefreshInventory call and returns a canned
// response/error, so the worker's branching on 404 vs other errors can be
// exercised without a real HTTP transport.
type fakeRefreshCmd struct {
	gotSiteID  uuid.UUID
	gotSiteURL string
	resp       agentcmd.RefreshInventoryResponse
	err        error
	calls      int
}

func (f *fakeRefreshCmd) RefreshInventory(_ context.Context, siteID uuid.UUID, siteURL string, _ agentcmd.RefreshInventoryRequest) (agentcmd.RefreshInventoryResponse, error) {
	f.calls++
	f.gotSiteID = siteID
	f.gotSiteURL = siteURL
	return f.resp, f.err
}

func TestRefreshInventoryWorkerHappyPath(t *testing.T) {
	cmd := &fakeRefreshCmd{resp: agentcmd.RefreshInventoryResponse{OK: true, Detail: "queued"}}
	w := NewRefreshInventoryWorker(cmd, nil, nil)
	siteID := uuid.New()
	err := w.Work(context.Background(), &river.Job[RefreshInventoryArgs]{
		Args: RefreshInventoryArgs{TenantID: uuid.New(), SiteID: siteID, SiteURL: "https://example.com", Source: "api"},
	})
	if err != nil {
		t.Fatalf("happy-path Work returned %v", err)
	}
	if cmd.calls != 1 || cmd.gotSiteID != siteID {
		t.Fatalf("commander not called as expected: %+v", cmd)
	}
}

// TestRefreshInventoryWorkerOldAgent404IsSuccess proves that an agent returning
// 404 from its REST API (no refresh route on old agents) is treated as a soft
// no-op: the job succeeds so River does not retry it forever. This matches the
// spec's "OLD agents must still work" requirement.
func TestRefreshInventoryWorkerOldAgent404IsSuccess(t *testing.T) {
	cmd := &fakeRefreshCmd{err: errors.New("refresh_inventory command rejected by agent: status 404 body={\"code\":\"rest_no_route\"}")}
	w := NewRefreshInventoryWorker(cmd, nil, nil)
	err := w.Work(context.Background(), &river.Job[RefreshInventoryArgs]{
		Args: RefreshInventoryArgs{TenantID: uuid.New(), SiteID: uuid.New(), SiteURL: "https://example.com"},
	})
	if err != nil {
		t.Fatalf("404 from old agent must be a soft success, got %v", err)
	}
}

// TestRefreshInventoryWorkerTransportErrorRetries proves a transport-level
// error (or a non-404 agent status) bubbles out so River retries.
func TestRefreshInventoryWorkerTransportErrorRetries(t *testing.T) {
	cmd := &fakeRefreshCmd{err: errors.New("refresh_inventory command transport: connection refused")}
	w := NewRefreshInventoryWorker(cmd, nil, nil)
	err := w.Work(context.Background(), &river.Job[RefreshInventoryArgs]{
		Args: RefreshInventoryArgs{TenantID: uuid.New(), SiteID: uuid.New(), SiteURL: "https://example.com"},
	})
	if err == nil {
		t.Fatalf("transport error must be returned for River to retry")
	}
}

// TestRefreshInventoryWorker500FromAgentRetries proves a 5xx from the agent
// (e.g. WP crash) is still a retryable error, not the 404 special case.
func TestRefreshInventoryWorker500FromAgentRetries(t *testing.T) {
	cmd := &fakeRefreshCmd{err: errors.New("refresh_inventory command rejected by agent: status 500 body=oops")}
	w := NewRefreshInventoryWorker(cmd, nil, nil)
	err := w.Work(context.Background(), &river.Job[RefreshInventoryArgs]{
		Args: RefreshInventoryArgs{TenantID: uuid.New(), SiteID: uuid.New(), SiteURL: "https://example.com"},
	})
	if err == nil {
		t.Fatalf("agent 500 must be returned for River to retry")
	}
}

// TestRefreshInventoryWorkerNoCommanderCancels proves a nil commander (CP
// signing disabled) cancels the job rather than retrying forever.
func TestRefreshInventoryWorkerNoCommanderCancels(t *testing.T) {
	w := NewRefreshInventoryWorker(nil, nil, nil)
	err := w.Work(context.Background(), &river.Job[RefreshInventoryArgs]{
		Args: RefreshInventoryArgs{TenantID: uuid.New(), SiteID: uuid.New(), SiteURL: "https://example.com"},
	})
	if err == nil {
		t.Fatalf("nil commander must surface an error so River cancels")
	}
}

func TestRefreshDebouncerSuppressesWithinWindow(t *testing.T) {
	d := NewRefreshDebouncer(50 * time.Millisecond)
	now := time.Now()
	d.now = func() time.Time { return now }
	siteID := uuid.New()
	if !d.Allow(siteID) {
		t.Fatalf("first Allow must succeed")
	}
	if d.Allow(siteID) {
		t.Fatalf("second Allow within window must be suppressed")
	}
	// Different site: allowed.
	if !d.Allow(uuid.New()) {
		t.Fatalf("different site must be allowed independently")
	}
	// Advance past the window.
	d.now = func() time.Time { return now.Add(100 * time.Millisecond) }
	if !d.Allow(siteID) {
		t.Fatalf("Allow past window must succeed")
	}
}

// TestRefreshInventoryArgsQueueShard proves the InsertOpts pin the refresh job
// to the tenant's queue shard, matching update tasks. Same tenant ⇒ same shard.
func TestRefreshInventoryArgsQueueShard(t *testing.T) {
	tenantID := uuid.New()
	a := RefreshInventoryArgs{TenantID: tenantID, SiteID: uuid.New(), SiteURL: "https://example.com"}
	if a.InsertOpts().Queue != QueueForTenant(tenantID) {
		t.Fatalf("queue shard not pinned to tenant")
	}
	if a.Kind() != "refresh_inventory_command" {
		t.Fatalf("kind = %q", a.Kind())
	}
}
