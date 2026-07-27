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

// applyOnlyCommander is a Commander test double that implements ONLY
// Update/Rollback, deliberately WITHOUT VerifyReachableWithReason (GH #291
// Phase 4). It exercises runApply end-to-end (unlike fakeCommander's Update,
// which panics) while proving the agent-first type assertion in
// Worker.verifyAgentHealth fails gracefully for a Commander that cannot run
// the signed check at all (mirrors the real "no signing key configured"
// case, where the wired commander is main.go's disabledCommander).
type applyOnlyCommander struct {
	rollbackCalled bool
	rollbackErr    error
}

func (c *applyOnlyCommander) Update(_ context.Context, _ uuid.UUID, _ string, req agentcmd.UpdateRequest) (agentcmd.UpdateResponse, error) {
	item := req.Items[0]
	return agentcmd.UpdateResponse{OK: true, Results: []agentcmd.ItemResult{{
		Type: item.Type, Slug: item.Slug, FromVersion: "1.9.9", ToVersion: item.Version,
		Status: agentcmd.ItemSucceeded, SnapshotID: "snap-1",
	}}}, nil
}

func (c *applyOnlyCommander) Rollback(context.Context, uuid.UUID, string, agentcmd.RollbackRequest) (agentcmd.RollbackResponse, error) {
	c.rollbackCalled = true
	if c.rollbackErr != nil {
		return agentcmd.RollbackResponse{}, c.rollbackErr
	}
	return agentcmd.RollbackResponse{OK: true, RestoredVersion: "1.9.9"}, nil
}

// verifyingCommander embeds applyOnlyCommander's Update/Rollback behaviour and
// additionally implements VerifyReachableWithReason with a single fixed
// scripted outcome returned on every call, so runApply's GH #291 Phase 4
// agent-first check can be driven precisely without a real agentcmd.Client or
// live HTTP. verifyCalls counts every invocation so a test can assert the
// retry discipline in verifyAgentHealthWithRetry actually ran (or did not).
type verifyingCommander struct {
	applyOnlyCommander
	alive        bool
	fallbackUsed bool
	reason       agentcmd.ReachabilityReason
	verifyErr    error
	verifyCalled bool
	verifyCalls  int
}

func (c *verifyingCommander) VerifyReachableWithReason(_ context.Context, _ uuid.UUID, _ string) (bool, bool, agentcmd.ReachabilityReason, error) {
	c.verifyCalled = true
	c.verifyCalls++
	return c.alive, c.fallbackUsed, c.reason, c.verifyErr
}

// verifyStep is one scripted VerifyReachableWithReason outcome.
type verifyStep struct {
	alive        bool
	fallbackUsed bool
	reason       agentcmd.ReachabilityReason
	err          error
}

// scriptedVerifyCommander embeds applyOnlyCommander's Update/Rollback
// behaviour and returns a queued SEQUENCE of VerifyReachableWithReason
// outcomes, one per call; the last entry repeats once the queue is exhausted.
// Used to pin GH #291 Phase 4 fix 1: a TRANSIENT agent 5xx that clears within
// verifyAgentHealthWithRetry's retry window must not roll back, while a
// PERSISTENT one (a script that never clears) must.
type scriptedVerifyCommander struct {
	applyOnlyCommander
	script []verifyStep
	calls  int
}

func (c *scriptedVerifyCommander) VerifyReachableWithReason(_ context.Context, _ uuid.UUID, _ string) (bool, bool, agentcmd.ReachabilityReason, error) {
	i := c.calls
	if i >= len(c.script) {
		i = len(c.script) - 1
	}
	c.calls++
	s := c.script[i]
	return s.alive, s.fallbackUsed, s.reason, s.err
}

func healthyVerifyStep() verifyStep {
	return verifyStep{alive: true, reason: agentcmd.ReasonAlive}
}

func unhealthyVerifyStep() verifyStep {
	return verifyStep{alive: false, reason: agentcmd.ReasonHTTP5xx}
}

// panicProber fails the test immediately if Get is ever called. Used to prove
// the agent-first check (GH #291 Phase 4 Change 1), when conclusive, skips
// the public homepage probe entirely rather than merely racing through it.
type panicProber struct{ t *testing.T }

func (p *panicProber) Get(context.Context, string) (agentcmd.ProbeResult, error) {
	p.t.Fatalf("public probe must not be called: the agent-first check already reached a conclusive verdict")
	return agentcmd.ProbeResult{}, nil
}

// newApplyTestWorker builds a Worker around any Commander implementation with
// a tiny, non-sleeping probe-retry schedule (mirrors newTestWorker, but
// generalized to Commander so the GH #291 Phase 4 test doubles above, which
// are not *fakeCommander, can be used).
func newApplyTestWorker(repo *probeFakeRepo, cmd Commander, prober HealthProber) *Worker {
	w := NewWorker(repo, nil /* sites: unused by runApply */, cmd, prober, nil /* hub */, nil /* audit */, nil, 5, 0)
	w.SetProbeRetryDelays([]time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond})
	return w
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
	w := NewWorker(repo, nil /* sites: unused by runApply */, cmd, prober, nil /* hub */, nil /* audit */, nil, 5, 0)
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
	if err := w.rollback(context.Background(), task, "https://example.test", item, res, probe, false, reason); err != nil {
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

	if err := w.rollback(context.Background(), task, "https://example.test", item, res, fatalProbe, false, "post-update health failed"); err != nil {
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

	if err := w.rollback(context.Background(), task, "https://example.test", item, res, statusOnlyProbe, false, "post-update health failed"); err != nil {
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

	if err := w.rollback(context.Background(), task, "https://example.test", item, res, transportErrProbe, false, "post-update probe error"); err != nil {
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

// ----------------------------------------------------------------------------
// GH #291 Phase 4: agent-first post-update health check + fixed public-probe
// classification. See docs/security/uptime-app-health-design-2026-07-27.md
// section 4. Every test below fails against the pre-fix code where noted.
// ----------------------------------------------------------------------------

// TestRunApply_AgentHealthy_PublicProbeAlsoConfirms_Succeeds proves the
// agent-first check does NOT end the health decision by itself: when the
// signed check reports alive, the public probe is still consulted (fix 2),
// and when it too reports healthy the task succeeds. Fails against the
// pre-fix code only in spirit (the pre-fix code also succeeds here), but
// pins the requirement that the probe is actually called by using a prober
// that fails the test if it is NOT.
func TestRunApply_AgentHealthy_PublicProbeAlsoConfirms_Succeeds(t *testing.T) {
	repo := &probeFakeRepo{}
	cmd := &verifyingCommander{alive: true, reason: agentcmd.ReasonAlive}
	prober := &scriptedProber{script: []probeStep{healthyStep()}}
	w := newApplyTestWorker(repo, cmd, prober)

	task := testTask()
	item := updateItem()
	if err := w.runApply(context.Background(), task, "https://example.test", item); err != nil {
		t.Fatalf("runApply: %v", err)
	}
	if !cmd.verifyCalled {
		t.Fatalf("expected the agent-first reachability check to have been called")
	}
	if prober.calls == 0 {
		t.Fatalf("expected the public probe to have been consulted even though the agent-first check reported healthy")
	}
	if cmd.rollbackCalled {
		t.Fatalf("rollback must not be called when both the agent-first check and the public probe report healthy")
	}
	if len(repo.finished) != 1 || repo.finished[0].Status != TaskSucceeded {
		t.Fatalf("expected one TaskSucceeded finish, got %+v", repo.finished)
	}
}

// TestRunApply_AgentHealthy_FrontEndFatal_StillRollsBack pins fix 2: an
// agent-healthy PLUGIN update whose homepage is fatal must still be caught
// and rolled back. The signed agent route only proves PHP booted and the
// plugin loaded; it does not prove the site renders. Fails against the
// pre-fix code, which returns "healthy, public probe skipped" as soon as the
// agent-first check is alive and would never see the fatal homepage at all.
func TestRunApply_AgentHealthy_FrontEndFatal_StillRollsBack(t *testing.T) {
	repo := &probeFakeRepo{}
	cmd := &verifyingCommander{alive: true, reason: agentcmd.ReasonAlive}
	fatalStep := probeStep{result: agentcmd.ProbeResult{StatusCode: 200, Fatal: true, Detail: "fatal-error signature in response body"}}
	prober := &scriptedProber{script: []probeStep{fatalStep}} // repeats: the front end never recovers
	w := newApplyTestWorker(repo, cmd, prober)

	task := testTask() // TargetType: TargetPlugin
	item := updateItem()
	if err := w.runApply(context.Background(), task, "https://example.test", item); err != nil {
		t.Fatalf("runApply: %v", err)
	}
	if !cmd.rollbackCalled {
		t.Fatalf("expected rollback: the agent reported healthy but the homepage is fatal, a front-end-only failure the agent route cannot see")
	}
	if len(repo.finished) != 1 || repo.finished[0].Status != TaskRolledBack {
		t.Fatalf("expected one TaskRolledBack finish, got %+v", repo.finished)
	}
	if !strings.Contains(repo.finished[0].Detail, "front-end") {
		t.Fatalf("expected the rollback detail to note the front-end-only nature of the failure, got %q", repo.finished[0].Detail)
	}
}

// TestRunApply_AgentHealthy_NoExtraLatency is the golden test: a NORMAL
// healthy update, using the REAL (non-overridden) probeRetryDelays schedule,
// still completes quickly and without a rollback. It proves that consulting
// the public probe after an agent-healthy verdict (fix 2) costs no meaningful
// extra latency on the common happy path: probeHealthWithRetry only invokes
// its backoff schedule when the FIRST attempt is not already healthy, so one
// fast, healthy probe call is all this costs here.
func TestRunApply_AgentHealthy_NoExtraLatency(t *testing.T) {
	repo := &probeFakeRepo{}
	cmd := &verifyingCommander{alive: true, reason: agentcmd.ReasonAlive}
	prober := &scriptedProber{script: []probeStep{healthyStep()}}
	// Deliberately built WITHOUT newApplyTestWorker/SetProbeRetryDelays: this
	// worker uses the production probeRetryDelays schedule (~21s worst case)
	// so the assertion below actually proves that schedule is never invoked
	// on the happy path, not just that it is shortened.
	w := NewWorker(repo, nil, cmd, prober, nil, nil, nil, 5, 0)

	task := testTask()
	item := updateItem()
	start := time.Now()
	if err := w.runApply(context.Background(), task, "https://example.test", item); err != nil {
		t.Fatalf("runApply: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("runApply took %s using the PRODUCTION probe-retry schedule; "+
			"a healthy agent-first check plus a healthy public probe must not burn the ~21s retry window", elapsed)
	}
	if prober.calls != 1 {
		t.Fatalf("expected exactly one public-probe call on the happy path, got %d", prober.calls)
	}
	if cmd.rollbackCalled {
		t.Fatalf("rollback must not be called on the happy path")
	}
	if len(repo.finished) != 1 || repo.finished[0].Status != TaskSucceeded {
		t.Fatalf("expected one TaskSucceeded finish, got %+v", repo.finished)
	}
}

// TestRunApply_AgentHTTP5xxPersistent_RollsBackWithoutPublicProbe pins fix 1's
// "persistent" half: a signed agent route that returns a server error on
// EVERY attempt across the retry window is treated as authoritative proof of
// a broken update (PHP fatal-ing even on its own agent route) and rolls back
// WITHOUT ever consulting the public probe, which could only read a stale
// cached copy anyway. This is exactly THE BUG scenario: a page-cached site
// whose public GET / would return a stale healthy-looking 200.
func TestRunApply_AgentHTTP5xxPersistent_RollsBackWithoutPublicProbe(t *testing.T) {
	repo := &probeFakeRepo{}
	cmd := &verifyingCommander{alive: false, reason: agentcmd.ReasonHTTP5xx}
	w := newApplyTestWorker(repo, cmd, &panicProber{t: t})

	task := testTask()
	item := updateItem()
	if err := w.runApply(context.Background(), task, "https://example.test", item); err != nil {
		t.Fatalf("runApply: %v", err)
	}
	if cmd.verifyCalls < 2 {
		t.Fatalf("expected the agent-first check to have been retried before concluding persistent, got %d call(s)", cmd.verifyCalls)
	}
	if !cmd.rollbackCalled {
		t.Fatalf("expected rollback to be called: the signed agent route returned a 5xx on every attempt")
	}
	if len(repo.finished) != 1 || repo.finished[0].Status != TaskRolledBack {
		t.Fatalf("expected one TaskRolledBack finish, got %+v", repo.finished)
	}
}

// TestRunApply_AgentHTTP5xxTransient_ClearsWithinRetryWindow_DoesNotRollBack
// pins fix 1's "transient" half and is the direct regression test for GH #127
// Defect 2 reintroduced at the agent layer: a signed agent route that returns
// a server error on its FIRST couple of attempts, then recovers within the
// retry window (e.g. a major-version upgrade running a synchronous DB
// migration on activation), must NOT roll back. Fails against the pre-fix
// code, which rolls back on the single first observation with zero retries.
func TestRunApply_AgentHTTP5xxTransient_ClearsWithinRetryWindow_DoesNotRollBack(t *testing.T) {
	repo := &probeFakeRepo{}
	cmd := &scriptedVerifyCommander{script: []verifyStep{
		unhealthyVerifyStep(), unhealthyVerifyStep(), healthyVerifyStep(),
	}}
	prober := &scriptedProber{script: []probeStep{healthyStep()}}
	w := newApplyTestWorker(repo, cmd, prober)

	task := testTask()
	item := updateItem()
	if err := w.runApply(context.Background(), task, "https://example.test", item); err != nil {
		t.Fatalf("runApply: %v", err)
	}
	if cmd.calls < 3 {
		t.Fatalf("expected at least 3 agent-first attempts (2 transient failures + 1 recovery), got %d", cmd.calls)
	}
	if cmd.rollbackCalled {
		t.Fatalf("rollback must NOT be called: the agent-first check recovered within the retry window, exactly like a transient migration 503")
	}
	if len(repo.finished) != 1 || repo.finished[0].Status != TaskSucceeded {
		t.Fatalf("expected one TaskSucceeded finish, got %+v", repo.finished)
	}
}

// TestRunApply_AgentAbsent_FallsThroughAndDoesNotItselfRollBack proves Change
// 1's third decision-table row: a Commander with no agent-first capability at
// all (mirrors "no signing key configured" / an old agent) must NOT roll back
// on that absence alone. The outcome is decided entirely by the public probe
// below it, which here reports healthy.
func TestRunApply_AgentAbsent_FallsThroughAndDoesNotItselfRollBack(t *testing.T) {
	repo := &probeFakeRepo{}
	cmd := &applyOnlyCommander{} // no VerifyReachableWithReason method at all
	prober := &scriptedProber{script: []probeStep{healthyStep()}}
	w := newApplyTestWorker(repo, cmd, prober)

	task := testTask()
	item := updateItem()
	if err := w.runApply(context.Background(), task, "https://example.test", item); err != nil {
		t.Fatalf("runApply: %v", err)
	}
	if prober.calls == 0 {
		t.Fatalf("expected the public probe to have been consulted (agent-first check was unavailable)")
	}
	if cmd.rollbackCalled {
		t.Fatalf("an absent agent-first check must NOT by itself trigger a rollback")
	}
	if len(repo.finished) != 1 || repo.finished[0].Status != TaskSucceeded {
		t.Fatalf("expected one TaskSucceeded finish, got %+v", repo.finished)
	}
}

// TestRunApply_CachedPublicProbe_IsInconclusiveNotHealthy proves a public
// probe response flagged CacheHit (a stale cached 200) must NOT be read as
// proof of health, and must NOT itself trigger a rollback either (a cache
// hit is common and expected). Fails against the pre-fix code because
// ProbeResult.Healthy() has no CacheHit concept at all and would
// unconditionally record "updated and healthy".
func TestRunApply_CachedPublicProbe_IsInconclusiveNotHealthy(t *testing.T) {
	repo := &probeFakeRepo{}
	cmd := &applyOnlyCommander{}
	cached := probeStep{result: agentcmd.ProbeResult{StatusCode: 200, CacheHit: true, Detail: "cache hit (cf-cache-status: HIT)"}}
	prober := &scriptedProber{script: []probeStep{cached}} // repeats: cache never busts
	w := newApplyTestWorker(repo, cmd, prober)

	task := testTask()
	item := updateItem()
	if err := w.runApply(context.Background(), task, "https://example.test", item); err != nil {
		t.Fatalf("runApply: %v", err)
	}
	if cmd.rollbackCalled {
		t.Fatalf("a cache-hit response must NOT trigger a rollback (it is inconclusive, not unhealthy)")
	}
	if len(repo.finished) != 1 {
		t.Fatalf("expected one finish call, got %d", len(repo.finished))
	}
	got := repo.finished[0]
	if got.Status != TaskSucceeded {
		t.Fatalf("expected TaskSucceeded (cannot roll back on an inconclusive result), got %s", got.Status)
	}
	if got.Detail == "updated and healthy" {
		t.Fatalf("a cache-hit result must NOT be recorded as plain \"updated and healthy\": %q", got.Detail)
	}
	if !strings.Contains(got.Detail, "inconclusive") {
		t.Fatalf("expected the detail to record the check was inconclusive, got %q", got.Detail)
	}
}

// TestRunApply_PublicProbe404Root_DoesNotRollBack pins fix 3: a 404 on the
// site root after an update must NOT roll back. There is no pre-update
// baseline recorded, so a site that already returned 404 on its root BEFORE
// the update (a headless install, a site whose root is intentionally empty,
// a redirect-only domain) would otherwise be rolled back for a condition the
// update did not cause. The task still records the check as inconclusive,
// not healthy, so an operator is not falsely told the update was confirmed
// fine. Fails against the pre-fix code, which treats 404 as unhealthy and
// rolls back.
func TestRunApply_PublicProbe404Root_DoesNotRollBack(t *testing.T) {
	repo := &probeFakeRepo{}
	cmd := &applyOnlyCommander{}
	prober := &scriptedProber{script: []probeStep{{result: agentcmd.ProbeResult{StatusCode: 404}}}}
	w := newApplyTestWorker(repo, cmd, prober)

	task := testTask()
	item := updateItem()
	if err := w.runApply(context.Background(), task, "https://example.test", item); err != nil {
		t.Fatalf("runApply: %v", err)
	}
	if cmd.rollbackCalled {
		t.Fatalf("a 404 on the site root must NOT trigger a rollback: there is no pre-update baseline to know the update caused it")
	}
	if len(repo.finished) != 1 || repo.finished[0].Status != TaskSucceeded {
		t.Fatalf("expected one TaskSucceeded finish, got %+v", repo.finished)
	}
	if !strings.Contains(repo.finished[0].Detail, "inconclusive") {
		t.Fatalf("expected the detail to record the check was inconclusive (not a confirmed-healthy claim), got %q", repo.finished[0].Detail)
	}
}

// TestRunApply_PublicProbe401Or403_DoesNotTriggerRollback proves the other
// half: 401/403 on the site root is common and legitimate (staging HTTP
// auth, a members-only site, a security plugin) and must NOT be treated as a
// broken-update signal, unlike 404/410.
func TestRunApply_PublicProbe401Or403_DoesNotTriggerRollback(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			repo := &probeFakeRepo{}
			cmd := &applyOnlyCommander{}
			prober := &scriptedProber{script: []probeStep{{result: agentcmd.ProbeResult{StatusCode: status}}}}
			w := newApplyTestWorker(repo, cmd, prober)

			task := testTask()
			item := updateItem()
			if err := w.runApply(context.Background(), task, "https://example.test", item); err != nil {
				t.Fatalf("runApply: %v", err)
			}
			if cmd.rollbackCalled {
				t.Fatalf("a %d on the site root must NOT trigger a rollback", status)
			}
			if len(repo.finished) != 1 || repo.finished[0].Status != TaskSucceeded {
				t.Fatalf("expected one TaskSucceeded finish, got %+v", repo.finished)
			}
		})
	}
}

// TestClassifyPostUpdateProbe_CacheHitCheckedBeforeStatus pins fix 4: a
// response flagged CacheHit must classify inconclusive REGARDLESS of a
// status code that would otherwise look conclusive on its own (a 5xx that
// would otherwise be unhealthy, or a 404 that would otherwise be
// inconclusive-for-a-different-reason). A cached response proves nothing
// about the CURRENT backend state, so the cache check must run before, not
// after, any status-code based classification. Fails against the pre-fix
// code, which checked Fatal/5xx/404 before CacheHit and would classify a
// cached 500 as unhealthy.
func TestClassifyPostUpdateProbe_CacheHitCheckedBeforeStatus(t *testing.T) {
	cases := []struct {
		name string
		in   agentcmd.ProbeResult
		want postUpdateVerdict
	}{
		{"cache_hit_500_is_inconclusive_not_unhealthy", agentcmd.ProbeResult{StatusCode: 500, CacheHit: true}, postUpdateInconclusive},
		{"cache_hit_404_is_inconclusive", agentcmd.ProbeResult{StatusCode: 404, CacheHit: true}, postUpdateInconclusive},
		{"cache_hit_200_is_inconclusive", agentcmd.ProbeResult{StatusCode: 200, CacheHit: true}, postUpdateInconclusive},
		{"cache_hit_fatal_is_inconclusive_not_unhealthy", agentcmd.ProbeResult{StatusCode: 200, Fatal: true, CacheHit: true}, postUpdateInconclusive},
		{"no_cache_500_is_unhealthy", agentcmd.ProbeResult{StatusCode: 500}, postUpdateUnhealthy},
		{"no_cache_404_is_inconclusive", agentcmd.ProbeResult{StatusCode: 404}, postUpdateInconclusive},
		{"no_cache_200_is_healthy", agentcmd.ProbeResult{StatusCode: 200}, postUpdateHealthy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyPostUpdateProbe(tc.in)
			if got != tc.want {
				t.Fatalf("classifyPostUpdateProbe(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestRunApply_CachedFiveHundred_IsInconclusiveNotUnhealthy is the full-stack
// (runApply) pin for fix 4, proving the reordering matters end to end, not
// just at the classify-function level: a probe response that is BOTH
// CacheHit AND a 500 status must not roll back, because the cache check runs
// first. Fails against the pre-fix code, which would classify this
// unhealthy and roll back.
func TestRunApply_CachedFiveHundred_IsInconclusiveNotUnhealthy(t *testing.T) {
	repo := &probeFakeRepo{}
	cmd := &applyOnlyCommander{}
	cachedFatal := probeStep{result: agentcmd.ProbeResult{StatusCode: 500, CacheHit: true, Detail: "cache hit (x-cache-status: HIT); server returned status 500"}}
	prober := &scriptedProber{script: []probeStep{cachedFatal}} // repeats: cache never busts
	w := newApplyTestWorker(repo, cmd, prober)

	task := testTask()
	item := updateItem()
	if err := w.runApply(context.Background(), task, "https://example.test", item); err != nil {
		t.Fatalf("runApply: %v", err)
	}
	if cmd.rollbackCalled {
		t.Fatalf("a cache-hit response must NOT trigger a rollback even when its status code is 500: the cache check must be evaluated first")
	}
	if len(repo.finished) != 1 || repo.finished[0].Status != TaskSucceeded {
		t.Fatalf("expected one TaskSucceeded finish, got %+v", repo.finished)
	}
	if !strings.Contains(repo.finished[0].Detail, "inconclusive") {
		t.Fatalf("expected the detail to record the check was inconclusive, got %q", repo.finished[0].Detail)
	}
}

// TestWorker_Timeout is the job-timeout-mismatch regression lock: the
// update-task worker's River job used to inherit River's own 60s default
// (Worker embedded river.WorkerDefaults[TaskArgs] with no Timeout override),
// even though a real runApply can spend cfg.Update.ApplyHTTPTimeout (5m by
// default) on the apply call ALONE, before the GH #291 Phase 4 post-update
// health check even starts. This asserts NewWorker threads its jobTimeout
// argument straight through to Timeout(), and that a zero jobTimeout (the
// zero value, and what every other test in this file passes since they don't
// care about the job deadline) reports back as 0, the documented signal
// (see Worker.Timeout's doc comment) for "fall back to river.Config.
// JobTimeout" rather than an accidental instant timeout.
func TestWorker_Timeout(t *testing.T) {
	repo := &probeFakeRepo{}
	cmd := &applyOnlyCommander{}
	prober := &scriptedProber{}

	t.Run("threads a positive jobTimeout through to Timeout()", func(t *testing.T) {
		w := NewWorker(repo, nil, cmd, prober, nil, nil, nil, 5, 42*time.Minute)
		if got := w.Timeout(nil); got != 42*time.Minute {
			t.Fatalf("Timeout() = %v, want 42m (the jobTimeout passed to NewWorker)", got)
		}
	})

	t.Run("zero jobTimeout falls back to River's default, not an instant timeout", func(t *testing.T) {
		w := NewWorker(repo, nil, cmd, prober, nil, nil, nil, 5, 0)
		if got := w.Timeout(nil); got != 0 {
			t.Fatalf("Timeout() = %v, want 0 (river's job executor falls back to river.Config.JobTimeout on exactly 0, per cmp.Or)", got)
		}
	})
}

// TestDeriveApplyJobTimeout pins the arithmetic DeriveApplyJobTimeout uses to
// size the update-task job's River Timeout(): applyHTTPTimeout (the apply
// call) plus BOTH post-update health-check retry ladders (read from the
// actual probeRetryDelays/agentVerifyTimeout constants, not a hardcoded
// number) plus a fixed headroom, and a zero applyHTTPTimeout falling back to
// 0 (river.Config.JobTimeout) rather than a partial, misleadingly-small
// budget that omits the apply round trip.
func TestDeriveApplyJobTimeout(t *testing.T) {
	var delaySum time.Duration
	for _, d := range probeRetryDelays {
		delaySum += d
	}
	attempts := time.Duration(len(probeRetryDelays) + 1)

	t.Run("production defaults: apply(5m) + agent ladder + probe ladder + 2m headroom", func(t *testing.T) {
		applyHTTPTimeout := 5 * time.Minute
		probeHTTPTimeout := 30 * time.Second

		want := applyHTTPTimeout +
			(attempts*agentVerifyTimeout + delaySum) +
			(attempts*probeHTTPTimeout + delaySum) +
			2*time.Minute

		got := DeriveApplyJobTimeout(applyHTTPTimeout, probeHTTPTimeout)
		if got != want {
			t.Fatalf("DeriveApplyJobTimeout(%v, %v) = %v, want %v", applyHTTPTimeout, probeHTTPTimeout, got, want)
		}
		// Pin the concrete number too, so a future change to probeRetryDelays or
		// agentVerifyTimeout is caught even if it happens to keep the formula above
		// (copy-pasted from the function) in sync with itself.
		if got != 10*time.Minute+52*time.Second {
			t.Fatalf("DeriveApplyJobTimeout(5m, 30s) = %v, want 10m52s with current ladder constants", got)
		}
	})

	t.Run("result comfortably fits under staleTaskThreshold", func(t *testing.T) {
		got := DeriveApplyJobTimeout(5*time.Minute, 30*time.Second)
		if got >= staleTaskThreshold {
			t.Fatalf("DeriveApplyJobTimeout() = %v, want < staleTaskThreshold (%v): the reaper must never terminalize a task still legitimately within its own job deadline", got, staleTaskThreshold)
		}
	})

	t.Run("zero applyHTTPTimeout falls back to 0, not a partial budget", func(t *testing.T) {
		if got := DeriveApplyJobTimeout(0, 30*time.Second); got != 0 {
			t.Fatalf("DeriveApplyJobTimeout(0, 30s) = %v, want 0", got)
		}
	})

	t.Run("zero probeHTTPTimeout does not zero out the whole result", func(t *testing.T) {
		got := DeriveApplyJobTimeout(5*time.Minute, 0)
		if got <= 5*time.Minute {
			t.Fatalf("DeriveApplyJobTimeout(5m, 0) = %v, want > 5m (probeHTTPTimeout=0 should fall back internally, not collapse the probe ladder to 0)", got)
		}
	})
}
