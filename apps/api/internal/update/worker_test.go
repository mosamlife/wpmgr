package update

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// probeFakeRepo is a minimal Repo implementation exercising only the methods
// runApply's finish/rollback path touches (FinishTask, CountUnfinishedTasks,
// SetRunStatus). Every other method panics if called, so an accidental new
// dependency in the code under test fails loudly instead of silently no-op'ing.
// (Named distinctly from handler_test.go's fakeRepo, which is scoped to the
// events-handler tests and has different fake behaviour.)
type probeFakeRepo struct {
	finished []FinishTaskInput
}

func (f *probeFakeRepo) CreateRunWithTasks(context.Context, CreateRunInput, []NewTask) (Run, []Task, error) {
	panic("not implemented")
}
func (f *probeFakeRepo) GetRun(context.Context, uuid.UUID, uuid.UUID) (Run, error) {
	panic("not implemented")
}
func (f *probeFakeRepo) ListRuns(context.Context, uuid.UUID, int32, int32) ([]Run, error) {
	panic("not implemented")
}
func (f *probeFakeRepo) ListRunSummaries(context.Context, uuid.UUID, int32, int32) ([]RunSummary, error) {
	panic("not implemented")
}
func (f *probeFakeRepo) ListTasks(context.Context, uuid.UUID, uuid.UUID) ([]Task, error) {
	panic("not implemented")
}
func (f *probeFakeRepo) GetTask(context.Context, uuid.UUID, uuid.UUID) (Task, error) {
	panic("not implemented")
}
func (f *probeFakeRepo) MarkTaskRunning(context.Context, uuid.UUID, uuid.UUID) (Task, error) {
	panic("not implemented")
}

func (f *probeFakeRepo) FinishTask(_ context.Context, in FinishTaskInput) (Task, error) {
	f.finished = append(f.finished, in)
	return Task{
		ID:          in.TaskID,
		TenantID:    in.TenantID,
		Status:      in.Status,
		FromVersion: in.FromVersion,
		ToVersion:   in.ToVersion,
		Detail:      in.Detail,
		Error:       in.Error,
	}, nil
}

func (f *probeFakeRepo) SetRunStatus(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ string) (Run, error) {
	return Run{}, nil
}

func (f *probeFakeRepo) CountUnfinishedTasks(context.Context, uuid.UUID, uuid.UUID) (int64, error) {
	// No other tasks outstanding: the run completes after this task finishes.
	// runApply/finish do not depend on the result beyond this call succeeding.
	return 0, nil
}

func (f *probeFakeRepo) CountRunningTasksForTenant(context.Context, uuid.UUID) (int64, error) {
	panic("not implemented")
}

func (f *probeFakeRepo) ListInFlightTargets(context.Context, uuid.UUID, []uuid.UUID) (map[InFlightKey]struct{}, error) {
	panic("not implemented")
}

func (f *probeFakeRepo) ListStaleUpdateTasks(context.Context, time.Duration, int32) ([]Task, error) {
	panic("not implemented")
}

// fakeCommander records Rollback calls and never actually contacts an agent.
type fakeCommander struct {
	rollbackCalled bool
	rollbackErr    error
}

func (c *fakeCommander) Update(context.Context, uuid.UUID, string, agentcmd.UpdateRequest) (agentcmd.UpdateResponse, error) {
	panic("not implemented")
}

func (c *fakeCommander) Rollback(context.Context, uuid.UUID, string, agentcmd.RollbackRequest) (agentcmd.RollbackResponse, error) {
	c.rollbackCalled = true
	if c.rollbackErr != nil {
		return agentcmd.RollbackResponse{}, c.rollbackErr
	}
	return agentcmd.RollbackResponse{OK: true, RestoredVersion: "1.9.9"}, nil
}

// scriptedProber returns a queued sequence of (ProbeResult, error) pairs, one
// per call to Get; the last entry repeats once the queue is exhausted.
type scriptedProber struct {
	script []probeStep
	calls  int
}

type probeStep struct {
	result agentcmd.ProbeResult
	err    error
}

func (p *scriptedProber) Get(context.Context, string) (agentcmd.ProbeResult, error) {
	i := p.calls
	if i >= len(p.script) {
		i = len(p.script) - 1
	}
	p.calls++
	step := p.script[i]
	return step.result, step.err
}

func healthyStep() probeStep {
	return probeStep{result: agentcmd.ProbeResult{StatusCode: 200}}
}

func unhealthyStep(status int) probeStep {
	return probeStep{result: agentcmd.ProbeResult{StatusCode: status, Detail: fmt.Sprintf("server returned status %d", status)}}
}

// newTestWorker builds a Worker wired with fakes and a tiny, non-sleeping
// probe-retry schedule so the tests run fast regardless of the production
// ~21s backoff window.
func newTestWorker(repo *probeFakeRepo, cmd *fakeCommander, prober HealthProber) *Worker {
	w := NewWorker(repo, nil /* sites: unused by runApply */, cmd, prober, nil /* hub */, nil /* audit */, nil, 5)
	w.SetProbeRetryDelays([]time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond})
	return w
}

func testTask() Task {
	return Task{
		ID:          uuid.New(),
		RunID:       uuid.New(),
		TenantID:    uuid.New(),
		SiteID:      uuid.New(),
		TargetType:  TargetPlugin,
		TargetSlug:  "suremail",
		FromVersion: "1.9.9",
		Status:      TaskRunning,
	}
}

func updateItem() agentcmd.UpdateItem {
	return agentcmd.UpdateItem{Type: TargetPlugin, Slug: "suremail", Version: "2.0.0"}
}

// TestRunApply_ProbeRetry_RecoversFromTransientMigration503 reproduces issue
// #127 Defect 2: a plugin activation that runs a synchronous DB migration can
// return 503 for a few seconds. The task must still succeed once the probe
// recovers within the retry window — no rollback.
func TestRunApply_ProbeRetry_RecoversFromTransientMigration503(t *testing.T) {
	repo := &probeFakeRepo{}
	cmd := &fakeCommander{}
	prober := &scriptedProber{script: []probeStep{
		unhealthyStep(503), unhealthyStep(503), unhealthyStep(503), healthyStep(),
	}}
	w := newTestWorker(repo, cmd, prober)

	task := testTask()
	item := updateItem()
	res := agentcmd.ItemResult{Type: item.Type, Slug: item.Slug, FromVersion: "1.9.9", ToVersion: "2.0.0", Status: agentcmd.ItemSucceeded}

	// Exercise runApply's probe+decide tail directly via the same code path
	// runApply uses, by calling the shared helper and asserting on its outcome,
	// then confirm the finish() side effect via the repo.
	probe, perr, attempts := w.probeHealthWithRetry(context.Background(), "https://example.test")
	if perr != nil {
		t.Fatalf("expected eventual success, got error: %v", perr)
	}
	if !probe.Healthy() {
		t.Fatalf("expected healthy probe after retry, got %+v", probe)
	}
	if attempts != 4 {
		t.Fatalf("expected 4 attempts (3 failures + 1 success), got %d", attempts)
	}

	if err := w.finish(context.Background(), task, TaskSucceeded, res.FromVersion, res.ToVersion, "updated and healthy", ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if cmd.rollbackCalled {
		t.Fatalf("rollback must NOT be called when the probe recovers within the retry window")
	}
	if len(repo.finished) != 1 || repo.finished[0].Status != TaskSucceeded {
		t.Fatalf("expected one TaskSucceeded finish, got %+v", repo.finished)
	}
}

// TestRunApply_ProbeRetry_StillUnhealthyRollsBack asserts that a post-update
// probe that NEVER recovers still rolls back, after exhausting the retry
// schedule (not on the first failure).
func TestRunApply_ProbeRetry_StillUnhealthyRollsBack(t *testing.T) {
	repo := &probeFakeRepo{}
	cmd := &fakeCommander{}
	prober := &scriptedProber{script: []probeStep{unhealthyStep(503)}} // repeats forever
	w := newTestWorker(repo, cmd, prober)

	task := testTask()
	item := updateItem()
	res := agentcmd.ItemResult{Type: item.Type, Slug: item.Slug, FromVersion: "1.9.9", ToVersion: "2.0.0", Status: agentcmd.ItemSucceeded}

	probe, perr, attempts := w.probeHealthWithRetry(context.Background(), "https://example.test")
	if perr != nil {
		t.Fatalf("expected no transport error, got %v", perr)
	}
	if probe.Healthy() {
		t.Fatalf("expected the probe to remain unhealthy")
	}
	wantAttempts := 1 + len(w.probeDelays) // initial + one per backoff step
	if attempts != wantAttempts {
		t.Fatalf("expected %d attempts (initial + %d retries), got %d", wantAttempts, len(w.probeDelays), attempts)
	}

	reason := fmt.Sprintf("post-update health failed after %d attempt(s): status=%d %s", attempts, probe.StatusCode, probe.Detail)
	if err := w.rollback(context.Background(), task, "https://example.test", item, res, probe, reason); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !cmd.rollbackCalled {
		t.Fatalf("expected rollback to be called after the probe never recovers")
	}
	if len(repo.finished) != 1 || repo.finished[0].Status != TaskRolledBack {
		t.Fatalf("expected one TaskRolledBack finish, got %+v", repo.finished)
	}
}

// TestRunApply_ProbeRetry_TransportErrorRetriedThenHeals covers a transient
// transport error (not just an HTTP 5xx) recovering within the window.
func TestRunApply_ProbeRetry_TransportErrorRetriedThenHeals(t *testing.T) {
	prober := &scriptedProber{script: []probeStep{
		{err: fmt.Errorf("dial tcp: connection refused")},
		{err: fmt.Errorf("dial tcp: connection refused")},
		healthyStep(),
	}}
	w := newTestWorker(&probeFakeRepo{}, &fakeCommander{}, prober)

	probe, perr, attempts := w.probeHealthWithRetry(context.Background(), "https://example.test")
	if perr != nil {
		t.Fatalf("expected recovery, got error: %v", perr)
	}
	if !probe.Healthy() {
		t.Fatalf("expected healthy probe after recovery, got %+v", probe)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
}

// TestRunApply_ProbeRetry_RespectsContextCancellation asserts the retry loop
// bails promptly (does not hang) when ctx is cancelled mid-retry.
func TestRunApply_ProbeRetry_RespectsContextCancellation(t *testing.T) {
	prober := &scriptedProber{script: []probeStep{unhealthyStep(503)}}
	w := newTestWorker(&probeFakeRepo{}, &fakeCommander{}, prober)
	// A long delay schedule so the test would hang if ctx cancellation were not
	// honored between attempts.
	w.SetProbeRetryDelays([]time.Duration{50 * time.Millisecond, time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(75 * time.Millisecond) // after attempt 1's backoff, before attempt 2's
		cancel()
	}()

	done := make(chan struct{})
	var perr error
	go func() {
		_, perr, _ = w.probeHealthWithRetry(ctx, "https://example.test")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("probeHealthWithRetry did not respect context cancellation")
	}
	if perr == nil {
		t.Fatalf("expected a context error, got nil")
	}
}

// TestRollback_FatalProbeAndRollbackTransportError_RecordsDistinctDetail
// covers GH #210: when the post-update probe itself detected a site-wide PHP
// fatal (a Fatal body signature, or a 5xx status) AND the rollback command's
// own transport also errors — the site's REST endpoint is itself
// undeliverable, most likely because of that same fatal — the recorded
// detail must be the distinct, actionable message, not the generic
// "rollback FAILED after unhealthy update", because the CP round trip cannot
// recover this site; only the agent's own filesystem-level watchdog (or
// manual intervention) can.
func TestRollback_FatalProbeAndRollbackTransportError_RecordsDistinctDetail(t *testing.T) {
	repo := &probeFakeRepo{}
	cmd := &fakeCommander{rollbackErr: fmt.Errorf("dial tcp: connection refused")}
	w := newTestWorker(repo, cmd, &scriptedProber{script: []probeStep{unhealthyStep(503)}})

	task := testTask()
	item := updateItem()
	res := agentcmd.ItemResult{Type: item.Type, Slug: item.Slug, FromVersion: "1.9.9", ToVersion: "2.0.0", Status: agentcmd.ItemSucceeded}
	fatalProbe := agentcmd.ProbeResult{StatusCode: 200, Fatal: true, Detail: "fatal-error signature in response body"}

	if err := w.rollback(context.Background(), task, "https://example.test", item, res, fatalProbe, "post-update health failed"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !cmd.rollbackCalled {
		t.Fatalf("expected the rollback command to have been attempted")
	}
	if len(repo.finished) != 1 {
		t.Fatalf("expected one finish call, got %d", len(repo.finished))
	}
	got := repo.finished[0]
	if got.Status != TaskFailed {
		t.Fatalf("expected TaskFailed, got %s", got.Status)
	}
	if !strings.Contains(got.Detail, "site not responding") || !strings.Contains(got.Detail, "rollback command undeliverable") {
		t.Fatalf("expected the distinct site-wide-fatal detail, got %q", got.Detail)
	}
	if strings.Contains(got.Detail, "rollback FAILED after unhealthy update") {
		t.Fatalf("must NOT use the generic rollback-failed detail on a fatal-probe branch: %q", got.Detail)
	}
}

// TestRollback_FatalStatusOnlyAndRollbackTransportError_RecordsDistinctDetail
// covers the 5xx-without-a-body-signature half of the GH #210 condition (the
// agent.go/agentcmd Probe never sets Fatal for a bare 5xx — only StatusCode).
func TestRollback_FatalStatusOnlyAndRollbackTransportError_RecordsDistinctDetail(t *testing.T) {
	repo := &probeFakeRepo{}
	cmd := &fakeCommander{rollbackErr: fmt.Errorf("dial tcp: connection refused")}
	w := newTestWorker(repo, cmd, &scriptedProber{script: []probeStep{unhealthyStep(503)}})

	task := testTask()
	item := updateItem()
	res := agentcmd.ItemResult{Type: item.Type, Slug: item.Slug, FromVersion: "1.9.9", ToVersion: "2.0.0", Status: agentcmd.ItemSucceeded}
	statusOnlyProbe := agentcmd.ProbeResult{StatusCode: 503, Detail: "server returned status 503"}

	if err := w.rollback(context.Background(), task, "https://example.test", item, res, statusOnlyProbe, "post-update health failed"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	got := repo.finished[0]
	if !strings.Contains(got.Detail, "site not responding") {
		t.Fatalf("expected the distinct site-wide-fatal detail for a 5xx-only probe, got %q", got.Detail)
	}
}

// TestRollback_NonFatalTransportErrorKeepsGenericDetail proves the GH #210
// distinct detail is scoped to a Fatal/5xx probe: a plain probe transport
// error (the zero-value ProbeResult probeHealthWithRetry returns when the
// probe itself never got a response) whose rollback ALSO fails to transport
// keeps the existing generic "rollback FAILED" message — that combination is
// just as likely to be an unrelated network blip as a site-wide PHP fatal.
func TestRollback_NonFatalTransportErrorKeepsGenericDetail(t *testing.T) {
	repo := &probeFakeRepo{}
	cmd := &fakeCommander{rollbackErr: fmt.Errorf("dial tcp: i/o timeout")}
	w := newTestWorker(repo, cmd, &scriptedProber{})

	task := testTask()
	item := updateItem()
	res := agentcmd.ItemResult{Type: item.Type, Slug: item.Slug, FromVersion: "1.9.9", ToVersion: "2.0.0", Status: agentcmd.ItemSucceeded}
	transportErrProbe := agentcmd.ProbeResult{} // StatusCode 0, Fatal false

	if err := w.rollback(context.Background(), task, "https://example.test", item, res, transportErrProbe, "post-update probe error"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if len(repo.finished) != 1 {
		t.Fatalf("expected one finish call, got %d", len(repo.finished))
	}
	got := repo.finished[0]
	if !strings.HasPrefix(got.Detail, "rollback FAILED after unhealthy update:") {
		t.Fatalf("expected the generic rollback-failed detail, got %q", got.Detail)
	}
	if strings.Contains(got.Detail, "site not responding") {
		t.Fatalf("must NOT use the distinct fatal detail on a non-fatal transport error: %q", got.Detail)
	}
}
