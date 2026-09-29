package site

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	agentpkg "github.com/mosamlife/wpmgr/apps/api/internal/agent"
)

// blockingProber answers neither probe until release is closed, the caller's
// context ends, or 15s pass, the way a slow site answers a signed ping.
type blockingProber struct {
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func newBlockingProber() *blockingProber { return &blockingProber{release: make(chan struct{})} }

func (p *blockingProber) wait(ctx context.Context) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	select {
	case <-p.release:
	case <-ctx.Done():
	case <-time.After(15 * time.Second):
	}
}

func (p *blockingProber) CommandRedirectTarget(ctx context.Context, _ uuid.UUID, _ string) (string, bool) {
	p.wait(ctx)
	return "", false
}

func (p *blockingProber) CommandPingOK(ctx context.Context, _ uuid.UUID, _ string) bool {
	p.wait(ctx)
	return false
}

func (p *blockingProber) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// recordingQueue is an AdoptURLEnqueuer that records each job, or fails.
type recordingQueue struct {
	mu   sync.Mutex
	jobs []AdoptReportedURLArgs
	err  error
}

func (q *recordingQueue) EnqueueAdoptURL(_ context.Context, a AdoptReportedURLArgs) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return q.err
	}
	q.jobs = append(q.jobs, a)
	return nil
}

// TestAdoptReportedURL_PushesDoNotWaitForTheProbe: a push that reports an
// address the site would have to confirm returns at once, with one job
// queued and no probe sent, even when the site takes 15s to answer a probe.
// The diagnostics push reaches the site service through
// EnqueueAdoptReportedURL, the metadata push through ApplyAgentMetadata.
func TestAdoptReportedURL_PushesDoNotWaitForTheProbe(t *testing.T) {
	ctx := context.Background()
	clk := &manualClock{t: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	tenant, site := uuid.New(), uuid.New()

	prober := newBlockingProber()
	defer close(prober.release)
	repo := &adoptRepo{fakeRepo: &fakeRepo{}, url: "http://example.com", state: StateConnected, adoptResult: true}
	svc := NewService(repo, nil, clk)
	svc.SetCommandRedirectProber(prober)
	q := &recordingQueue{}
	svc.SetAdoptURLEnqueuer(q)

	start := time.Now()
	if err := svc.EnqueueAdoptReportedURL(ctx, tenant, site, " https://example.com ", "agent_diagnostics", ""); err != nil {
		t.Fatalf("diagnostics enqueue: %v", err)
	}
	if _, err := svc.ApplyAgentMetadata(ctx, tenant, site, agentpkg.Metadata{HomeURL: "https://example.com", AgentVersion: "0.61.150"}); err != nil {
		t.Fatalf("metadata push: %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the two pushes took %v; a push must not wait for the probe", took)
	}
	if n := prober.callCount(); n != 0 {
		t.Errorf("a push sent %d probes; only the job may probe", n)
	}
	if len(repo.adoptCalls) != 0 {
		t.Errorf("a push wrote the address: %v", repo.adoptCalls)
	}
	want := []AdoptReportedURLArgs{
		{TenantID: tenant, SiteID: site, Reported: "https://example.com", Source: "agent_diagnostics"},
		{TenantID: tenant, SiteID: site, Reported: "https://example.com", Source: urlSourceAgentMetadata, AgentVersion: "0.61.150"},
	}
	if !reflect.DeepEqual(q.jobs, want) {
		t.Errorf("queued jobs = %+v, want %+v", q.jobs, want)
	}
}

// TestAdoptReportedURLWorker_Adopts: the job, when run, performs the
// adoption AdoptReportedURL decides, with the source and agent version it was
// queued with.
func TestAdoptReportedURLWorker_Adopts(t *testing.T) {
	clk := &manualClock{t: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	prober := &fakeProber{pingOK: true}
	svc, repo := newAdoptService("http://example.com", prober, clk)

	job := &river.Job[AdoptReportedURLArgs]{Args: AdoptReportedURLArgs{
		TenantID: uuid.New(), SiteID: uuid.New(), Reported: "https://example.com/", Source: "agent_diagnostics",
	}}
	if err := NewAdoptReportedURLWorker(svc).Work(context.Background(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(prober.pingCalls) != 1 || prober.pingCalls[0] != "https://example.com" {
		t.Errorf("ping calls = %v, want one to https://example.com", prober.pingCalls)
	}
	if len(repo.adoptCalls) != 1 || repo.adoptCalls[0] != "https://example.com" {
		t.Errorf("address writes = %v, want one to https://example.com", repo.adoptCalls)
	}
	if got := NewAdoptReportedURLWorker(svc).Timeout(job); got != adoptURLJobTimeout {
		t.Errorf("job timeout = %v, want %v", got, adoptURLJobTimeout)
	}
}

// TestAdoptReportedURL_EnqueueFailureNeverFailsThePush: a queue that refuses
// the job leaves the metadata push succeeding, and nothing is written.
func TestAdoptReportedURL_EnqueueFailureNeverFailsThePush(t *testing.T) {
	ctx := context.Background()
	clk := &manualClock{t: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	prober := &fakeProber{pingOK: true}
	svc, repo := newAdoptService("http://example.com", prober, clk)
	svc.SetAdoptURLEnqueuer(&recordingQueue{err: errors.New("queue unavailable")})

	if _, err := svc.ApplyAgentMetadata(ctx, uuid.New(), uuid.New(), agentpkg.Metadata{HomeURL: "https://example.com"}); err != nil {
		t.Fatalf("metadata push failed on an enqueue failure: %v", err)
	}
	if err := svc.EnqueueAdoptReportedURL(ctx, uuid.New(), uuid.New(), "https://example.com", "agent_diagnostics", ""); err == nil {
		t.Error("EnqueueAdoptReportedURL hid the enqueue failure from its caller")
	}
	if len(repo.adoptCalls)+len(prober.pingCalls)+len(prober.redirectCalls) != 0 {
		t.Errorf("an enqueue failure probed or wrote: writes=%v pings=%v redirects=%v",
			repo.adoptCalls, prober.pingCalls, prober.redirectCalls)
	}
}

// TestAdoptReportedURLArgs_UniquePerSiteAndAddress: the job is unique by its
// site and reported address only, within an hour, so repeated pushes of one
// address queue one job however their source or agent version differs.
func TestAdoptReportedURLArgs_UniquePerSiteAndAddress(t *testing.T) {
	opts := AdoptReportedURLArgs{}.InsertOpts()
	if !opts.UniqueOpts.ByArgs || opts.UniqueOpts.ByPeriod != time.Hour {
		t.Errorf("unique opts = %+v, want ByArgs over one hour", opts.UniqueOpts)
	}
	var unique []string
	rt := reflect.TypeOf(AdoptReportedURLArgs{})
	for i := 0; i < rt.NumField(); i++ {
		if rt.Field(i).Tag.Get("river") == "unique" {
			unique = append(unique, rt.Field(i).Tag.Get("json"))
		}
	}
	sort.Strings(unique)
	if want := []string{"reported", "site_id"}; !reflect.DeepEqual(unique, want) {
		t.Errorf("unique fields = %v, want %v", unique, want)
	}
}
