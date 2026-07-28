package update

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// Doubles
// ---------------------------------------------------------------------------

// recordingSelfUpdater is the beat-1 commander. It records every site it was
// asked to arm, which is the fact most of these tests assert on: whether a
// site was contacted at all.
type recordingSelfUpdater struct {
	resp  agentcmd.AgentSelfUpdateResponse
	err   error
	calls []uuid.UUID
}

func (c *recordingSelfUpdater) AgentSelfUpdate(_ context.Context, siteID uuid.UUID, _ string, _ agentcmd.AgentSelfUpdateRequest) (agentcmd.AgentSelfUpdateResponse, error) {
	c.calls = append(c.calls, siteID)
	if c.err != nil {
		return agentcmd.AgentSelfUpdateResponse{}, c.err
	}
	return c.resp, nil
}

// scriptedSelfUpdater answers differently per CALL, which is what a mixed fleet
// looks like: one wave containing sites that upgrade normally alongside sites
// whose agent predates the channel entirely. Answers are consumed in call order;
// running past the end is a test bug and says so.
type scriptedSelfUpdater struct {
	answers []selfUpdateAnswer
	calls   []uuid.UUID
	t       *testing.T
}

type selfUpdateAnswer struct {
	resp agentcmd.AgentSelfUpdateResponse
	err  error
}

func (c *scriptedSelfUpdater) AgentSelfUpdate(_ context.Context, siteID uuid.UUID, _ string, _ agentcmd.AgentSelfUpdateRequest) (agentcmd.AgentSelfUpdateResponse, error) {
	i := len(c.calls)
	c.calls = append(c.calls, siteID)
	if i >= len(c.answers) {
		c.t.Fatalf("the worker contacted %d sites; only %d answers were scripted", i+1, len(c.answers))
		return agentcmd.AgentSelfUpdateResponse{}, nil
	}
	a := c.answers[i]
	return a.resp, a.err
}

// oldAgentRouteMissing is the error an agent that predates this channel really
// produces: its REST API has no such route, so the request 404s and
// agentcmd.Client.post wraps it in the canonical form the CP sniffs. Written out
// literally rather than built from a helper, so a change to that wire format
// fails this test instead of silently un-branching the code under it.
func oldAgentRouteMissing() error {
	return errors.New(`agent_self_update command rejected by agent: status 404 body={"code":"rest_no_route","message":"No route was found matching the URL and request method."}`)
}

// fakeApplyResults stands in for the site inventory's record of the agent's own
// apply beat.
type fakeApplyResults struct {
	results map[uuid.UUID]AgentApplyResult
	err     error
}

func (f *fakeApplyResults) AgentSelfUpdateResult(_ context.Context, _, siteID uuid.UUID) (AgentApplyResult, bool, error) {
	if f.err != nil {
		return AgentApplyResult{}, false, f.err
	}
	r, ok := f.results[siteID]
	return r, ok, nil
}

// explodingSelfUpdater fails the test if it is ever reached. Used wherever the
// point of the test is that NOTHING was sent to a site.
type explodingSelfUpdater struct{ t *testing.T }

func (c *explodingSelfUpdater) AgentSelfUpdate(context.Context, uuid.UUID, string, agentcmd.AgentSelfUpdateRequest) (agentcmd.AgentSelfUpdateResponse, error) {
	c.t.Fatal("no agent self-update command may be sent to a site here")
	return agentcmd.AgentSelfUpdateResponse{}, nil
}

type fakeVersions struct {
	versions map[uuid.UUID]string
	err      error
}

func (f *fakeVersions) AgentVersion(_ context.Context, _, siteID uuid.UUID) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.versions[siteID], nil
}

type recordingEnqueuer struct {
	tasks    []uuid.UUID
	confirms []AgentConfirmArgs
	err      error
}

func (e *recordingEnqueuer) EnqueueTask(_ context.Context, _, _, taskID uuid.UUID, _ bool) error {
	e.tasks = append(e.tasks, taskID)
	return nil
}

func (e *recordingEnqueuer) EnqueueAgentConfirm(_ context.Context, args AgentConfirmArgs) error {
	if e.err != nil {
		return e.err
	}
	e.confirms = append(e.confirms, args)
	return nil
}

type fixedReleases struct{ version string }

func (f fixedReleases) LatestVersion(context.Context) string { return f.version }

// agentHarness wires a Worker over an agentStore for the dispatch tests.
type agentHarness struct {
	store *agentStore
	repo  *fakeAgentRepo
	waves *fakeWaveRepo
	enq   *recordingEnqueuer
	w     *Worker
}

// newAgentHarness builds a Worker whose ORDINARY Commander is nil on purpose.
// Worker.rollback and Worker.runApply both go through w.cmd, so a nil there
// means any attempt to reach the plugin-update machinery (snapshot, health
// probe, rollback) from the agent branch panics instead of quietly working.
// The agent branch uses a different interface entirely (w.agent.Cmd), which
// has no rollback method on it at all.
func newAgentHarness(t *testing.T, siteCount int, cmd AgentSelfUpdateCommander, versions AgentVersionLookup, enabled bool) *agentHarness {
	t.Helper()
	tenant := uuid.New()
	store := newAgentStore(tenant, siteCount)
	repo := &fakeAgentRepo{s: store}
	waves := &fakeWaveRepo{s: store}
	enq := &recordingEnqueuer{}

	sites := map[uuid.UUID]SiteInfo{}
	for _, task := range store.tasks {
		sites[task.SiteID] = SiteInfo{ID: task.SiteID, URL: "https://site.example", Enrolled: true, AgentVersion: "0.61.80"}
	}

	w := NewWorker(repo, &fakeSiteLookup{sites: sites}, nil, nil, nil, nil, nil, 5, 0)
	w.SetAgentSelfUpdate(AgentSelfUpdateDeps{
		Enabled:  enabled,
		Cmd:      cmd,
		Versions: versions,
		Waves:    waves,
		Tasks:    enq,
		Confirms: enq,
		// Wired exactly as main.go wires it: the published version is what an
		// "up_to_date" answer is checked against. The store's tasks are planned
		// from 0.61.80, so 0.62.0 is the build this run is rolling out.
		Releases: fixedReleases{version: "0.62.0"},
	})
	return &agentHarness{store: store, repo: repo, waves: waves, enq: enq, w: w}
}

// withPublished overrides what this control plane believes it publishes, for
// the tests that turn the two halves against each other.
func (h *agentHarness) withPublished(version string) *agentHarness {
	deps := h.w.agent
	deps.Releases = fixedReleases{version: version}
	h.w.SetAgentSelfUpdate(deps)
	return h
}

// withoutReleases removes the published-version reader entirely, standing in
// for a control plane that cannot read its own release channel.
func (h *agentHarness) withoutReleases() *agentHarness {
	deps := h.w.agent
	deps.Releases = nil
	h.w.SetAgentSelfUpdate(deps)
	return h
}

// withPlan rewrites the plan-time record on every seeded task: the version this
// run set out to install, and the version each site reported when it was
// planned. This is the run's premise, and the thing an "up_to_date" answer is
// checked against, so a test about a moving release manifest has to be able to
// state it explicitly rather than inherit it.
//
// An empty planned target stands in for a row created before the target was
// persisted (desired_version "latest"), which is what an in-flight run looks
// like across the deploy that introduced this.
func (h *agentHarness) withPlan(planned, plannedFrom string) *agentHarness {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	for _, t := range h.store.tasks {
		if planned == "" {
			t.DesiredVersion = "latest"
		} else {
			t.DesiredVersion = planned
		}
		t.FromVersion = plannedFrom
	}
	return h
}

// withApplyResults wires the optional lookup for the agent's own account of its
// apply beat, keyed by site.
func (h *agentHarness) withApplyResults(r AgentApplyResultLookup) *agentHarness {
	deps := h.w.agent
	deps.Results = r
	h.w.SetAgentSelfUpdate(deps)
	return h
}

func (h *agentHarness) work(ctx context.Context, i int) error {
	task := h.store.Task(i)
	return h.w.Work(ctx, &river.Job[TaskArgs]{Args: TaskArgs{
		TenantID: task.TenantID, RunID: task.RunID, TaskID: task.ID,
	}})
}

func isSnooze(err error) bool {
	var snooze *rivertype.JobSnoozeError
	return errors.As(err, &snooze)
}

// ---------------------------------------------------------------------------
// Kill switch
// ---------------------------------------------------------------------------

// TestKillSwitchBlocksDispatch is the dark-ship guarantee: with the switch off
// (its default), a task that is already created and already enqueued still
// never reaches a site. The commander fails the test if it is called at all.
//
// The switch has to stop DISPATCH rather than work through the release
// channel, because repointing the published manifest backwards does not
// un-brick anyone: the agent's downgrade guard refuses to install anything
// older than what it is already running.
func TestKillSwitchBlocksDispatch(t *testing.T) {
	h := newAgentHarness(t, 10, &explodingSelfUpdater{t: t}, &fakeVersions{}, false)

	if err := h.work(context.Background(), 0); err != nil {
		t.Fatalf("Work: %v", err)
	}

	if got := h.store.Task(0).Status; got != TaskSkipped {
		t.Fatalf("a task refused by the kill switch is skipped, got %q", got)
	}
	if !strings.Contains(h.store.Task(0).Detail, "disabled") {
		t.Fatalf("the refusal must say why: %q", h.store.Task(0).Detail)
	}
	// And it stops the WHOLE run, not just this task: the remaining sites are
	// cancelled rather than left to be attempted one by one.
	if h.store.RunStatus() != RunHalted {
		t.Fatalf("run status = %q, want %q", h.store.RunStatus(), RunHalted)
	}
	for i := 1; i < 10; i++ {
		if got := h.store.Task(i).Status; got != TaskCancelled {
			t.Fatalf("task %d status = %q, want %q", i, got, TaskCancelled)
		}
	}
	if len(h.enq.confirms) != 0 {
		t.Fatalf("no confirmation poll may be scheduled when nothing was armed: %+v", h.enq.confirms)
	}
}

// TestMissingSignerBlocksDispatch: no signing key means no signed command can
// be minted, and an unsigned self-update command must never be sent. That is
// the same refusal as the kill switch, reached a different way.
func TestMissingSignerBlocksDispatch(t *testing.T) {
	h := newAgentHarness(t, 3, nil, &fakeVersions{}, true)

	if err := h.work(context.Background(), 0); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if got := h.store.Task(0).Status; got != TaskSkipped {
		t.Fatalf("status = %q, want %q", got, TaskSkipped)
	}
	if h.store.RunStatus() != RunHalted {
		t.Fatalf("run status = %q, want %q", h.store.RunStatus(), RunHalted)
	}
}

// ---------------------------------------------------------------------------
// Beat 1: arming is not success
// ---------------------------------------------------------------------------

// TestScheduledAckIsNeverSuccess is the single most important behaviour in
// this channel. The agent answering "scheduled" means only that a cron event
// was queued: nothing has been applied, and on a site with broken loopback
// cron nothing ever will be. Recording that as success would report a fleet as
// upgraded while an unknown share of it still runs the old build.
func TestScheduledAckIsNeverSuccess(t *testing.T) {
	cmd := &recordingSelfUpdater{resp: agentcmd.AgentSelfUpdateResponse{
		Status:      agentcmd.SelfUpdateScheduled,
		FromVersion: "0.61.80",
		ToVersion:   "0.62.0",
		CronMode:    agentcmd.CronModeLoopback,
	}}
	h := newAgentHarness(t, 10, cmd, &fakeVersions{}, true)

	if err := h.work(context.Background(), 0); err != nil {
		t.Fatalf("Work: %v", err)
	}

	task := h.store.Task(0)
	if terminal(task.Status) {
		t.Fatalf("a scheduled ack must leave the task un-finished, got status %q", task.Status)
	}
	if task.Status != TaskRunning {
		t.Fatalf("status = %q, want %q", task.Status, TaskRunning)
	}
	for _, fin := range h.repo.finished {
		if fin.Status == TaskSucceeded {
			t.Fatalf("a scheduled ack must never be recorded as succeeded: %+v", fin)
		}
	}
	// The run must not advance either: wave 1 is still shut behind an
	// unconfirmed canary.
	if len(h.enq.tasks) != 0 {
		t.Fatalf("no further wave may be enqueued on a scheduled ack: %+v", h.enq.tasks)
	}
	// What it MUST do is arrange for the truth to be established later.
	if len(h.enq.confirms) != 1 {
		t.Fatalf("a scheduled ack must schedule exactly one confirmation poll, got %d", len(h.enq.confirms))
	}
	if got := h.enq.confirms[0].ExpectVersion; got != "0.62.0" {
		t.Fatalf("the confirmation poll must carry the expected version, got %q", got)
	}
}

// TestConfirmDeadlineWidensForExternalCron: a site whose loopback cron cannot
// run depends on an external scheduler the control plane cannot see, so it
// gets a much wider window. Declaring such a site unconfirmed on the narrow
// window would fail a healthy upgrade and, worse, feed a false failure into
// the wave gate.
//
// The wire carries exactly two cron modes. This test previously drove
// "disabled" and "alternate", two strings the agent has never emitted, so it
// was green while covering a path production could not reach, and the one
// value production DOES send, "external", fell through to the narrow window.
func TestConfirmDeadlineWidensForExternalCron(t *testing.T) {
	cases := []struct {
		name     string
		cronMode string
		want     time.Duration
	}{
		{"loopback", agentcmd.CronModeLoopback, agentConfirmDeadline},
		{"field omitted", agentcmd.CronModeUnknown, agentConfirmDeadline},
		{"a value this control plane does not know", "something-we-do-not-know", agentConfirmDeadline},
		// The strings deleted from the contract must NOT buy the wide window:
		// they are now as unrecognized as any other typo.
		{"the deleted disabled literal", "disabled", agentConfirmDeadline},
		{"the deleted alternate literal", "alternate", agentConfirmDeadline},
		{"external", agentcmd.CronModeExternal, agentConfirmDeadlineExternalCron},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := confirmWindowFor(tc.cronMode); got != tc.want {
				t.Fatalf("confirmWindowFor(%q) = %s, want %s", tc.cronMode, got, tc.want)
			}
		})
	}
}

// TestConfirmDeadlinesFitTheStagedTTLFloor is the control plane's half of the
// timing contract. The agent's staged record must outlive the CP's patience,
// or a slow site expires its own stage, beat 2 finds nothing to apply, and the
// canary false-fails a build that was never given a chance. The agent holds a
// stage for agentcmd.SelfUpdateStagedTTLFloor; this side may never widen a
// deadline past it. Raise the agent's TTL first, then this window.
func TestConfirmDeadlinesFitTheStagedTTLFloor(t *testing.T) {
	if agentConfirmDeadline >= agentcmd.SelfUpdateStagedTTLFloor {
		t.Fatalf("the loopback confirm deadline (%s) must sit inside the agent's staged TTL (%s)",
			agentConfirmDeadline, agentcmd.SelfUpdateStagedTTLFloor)
	}
	if agentConfirmDeadlineExternalCron >= agentcmd.SelfUpdateStagedTTLFloor {
		t.Fatalf("the external-cron confirm deadline (%s) must sit inside the agent's staged TTL (%s): "+
			"a control plane that waits longer than the site holds its stage fails sites that did nothing wrong",
			agentConfirmDeadlineExternalCron, agentcmd.SelfUpdateStagedTTLFloor)
	}
	if agentConfirmDeadlineExternalCron <= agentConfirmDeadline {
		t.Fatalf("the external-cron window (%s) must be the wider of the two (%s)",
			agentConfirmDeadlineExternalCron, agentConfirmDeadline)
	}
}

// TestArmOutcomesMapToTaskStates pins the rest of the beat-1 decision table,
// against the statuses the agent ACTUALLY emits.
//
// Its predecessor asserted on a "failed" status that has never been on the
// wire: a green test for a branch production never reaches. The status the
// agent really sends for a failed arm, "error", fell into the default branch,
// which recorded the bare status string and discarded the agent's detail, the
// one piece of information that outcome exists to carry.
func TestArmOutcomesMapToTaskStates(t *testing.T) {
	cases := []struct {
		name         string
		resp         agentcmd.AgentSelfUpdateResponse
		err          error
		wantStatus   string
		wantConfirms int
		// wantInDetail and wantInError must appear in the recorded task.
		wantInDetail string
		wantInError  string
	}{
		{
			name: "an agent-reported error fails the task and keeps its words",
			resp: agentcmd.AgentSelfUpdateResponse{
				Status: agentcmd.SelfUpdateError, FromVersion: "0.61.80", Detail: "manifest signature invalid"},
			wantStatus:   TaskFailed,
			wantInDetail: "manifest signature invalid",
			wantInError:  "manifest signature invalid",
		},
		{
			name: "a build that cannot self-update is skipped, not failed",
			resp: agentcmd.AgentSelfUpdateResponse{
				Status: agentcmd.SelfUpdateNotEligible, FromVersion: "0.61.80", Detail: "this build has no self-updater"},
			wantStatus:   TaskSkipped,
			wantInDetail: "this build has no self-updater",
		},
		{
			name:         "an unrecognized status fails rather than being assumed fine",
			resp:         agentcmd.AgentSelfUpdateResponse{Status: "wat", Detail: "who knows"},
			wantStatus:   TaskFailed,
			wantInDetail: "wat",
			wantInError:  "who knows",
		},
		{
			// The status the CP used to declare and the agent never sent. It is
			// now exactly as unrecognized as any other stray string, and must
			// not be silently accepted as a known outcome.
			name:         "the deleted failed literal is not a known status",
			resp:         agentcmd.AgentSelfUpdateResponse{Status: "failed", Detail: "whatever"},
			wantStatus:   TaskFailed,
			wantInDetail: "unrecognized",
		},
		{
			name:        "a transport error fails the task",
			err:         errors.New("connection refused"),
			wantStatus:  TaskFailed,
			wantInError: "connection refused",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &recordingSelfUpdater{resp: tc.resp, err: tc.err}
			h := newAgentHarness(t, 10, cmd, &fakeVersions{}, true)

			if err := h.work(context.Background(), 0); err != nil {
				t.Fatalf("Work: %v", err)
			}
			task := h.store.Task(0)
			if task.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", task.Status, tc.wantStatus)
			}
			if tc.wantInDetail != "" && !strings.Contains(task.Detail, tc.wantInDetail) {
				t.Fatalf("detail = %q, want it to carry %q", task.Detail, tc.wantInDetail)
			}
			if tc.wantInError != "" && !strings.Contains(task.Error, tc.wantInError) {
				t.Fatalf("error = %q, want it to carry %q", task.Error, tc.wantInError)
			}
			if len(h.enq.confirms) != tc.wantConfirms {
				t.Fatalf("confirmation polls = %d, want %d: only an arm ack schedules one",
					len(h.enq.confirms), tc.wantConfirms)
			}
		})
	}
}

// TestArmErrorNeverLosesTheAgentsDetail is the narrow, load-bearing assertion
// behind the case above, stated on its own because it is the whole point of
// the "error" outcome: the agent's sentence is the entire diagnostic value of a
// failed arm, and an operator staring at a task that says only "error" has
// nothing to act on. It must survive into BOTH the detail and the error, so no
// single code path can drop it.
func TestArmErrorNeverLosesTheAgentsDetail(t *testing.T) {
	const reason = "cron could not be scheduled: wp_schedule_single_event returned false"
	cmd := &recordingSelfUpdater{resp: agentcmd.AgentSelfUpdateResponse{
		Status: agentcmd.SelfUpdateError, FromVersion: "0.61.80", Detail: reason}}
	h := newAgentHarness(t, 10, cmd, &fakeVersions{}, true)

	if err := h.work(context.Background(), 0); err != nil {
		t.Fatalf("Work: %v", err)
	}
	task := h.store.Task(0)
	if task.Status != TaskFailed {
		t.Fatalf("status = %q, want %q", task.Status, TaskFailed)
	}
	if !strings.Contains(task.Detail, reason) {
		t.Fatalf("the operator-facing detail lost the agent's reason: %q", task.Detail)
	}
	if !strings.Contains(task.Error, reason) {
		t.Fatalf("the task error lost the agent's reason: %q", task.Error)
	}
	if task.Error == agentcmd.SelfUpdateError {
		t.Fatal("the task error must be the agent's reason, not the bare status string")
	}
}

// TestAlreadyScheduledArmsRatherThanHalts covers the retry case the CP used to
// treat as garbage. "already_scheduled" means an EARLIER arm succeeded and its
// upgrade is still pending: it carries the same to_version and expiry as
// "scheduled" and is not a failure. Falling into the default branch made a
// duplicate job, a River retry, or a manual re-run FAIL the task, and in wave
// 0 that halts the whole rollout on a site that is doing exactly what it was
// told.
func TestAlreadyScheduledArmsRatherThanHalts(t *testing.T) {
	cmd := &recordingSelfUpdater{resp: agentcmd.AgentSelfUpdateResponse{
		Status:      agentcmd.SelfUpdateAlreadyScheduled,
		FromVersion: "0.61.80",
		ToVersion:   "0.62.0",
		CronMode:    agentcmd.CronModeExternal,
		ExpiresAt:   time.Now().Add(2 * time.Hour).Unix(),
		Detail:      "an upgrade to 0.62.0 is already staged",
	}}
	h := newAgentHarness(t, 10, cmd, &fakeVersions{}, true)

	if err := h.work(context.Background(), 0); err != nil {
		t.Fatalf("Work: %v", err)
	}

	task := h.store.Task(0)
	if terminal(task.Status) {
		t.Fatalf("an already-staged upgrade must leave the task un-finished, got %q (%s)", task.Status, task.Detail)
	}
	if task.Status != TaskRunning {
		t.Fatalf("status = %q, want %q", task.Status, TaskRunning)
	}
	if h.store.RunStatus() == RunHalted {
		t.Fatal("a site reporting an upgrade already staged must never halt the run")
	}
	if len(h.enq.confirms) != 1 {
		t.Fatalf("an already-staged upgrade still needs its confirmation poll, got %d", len(h.enq.confirms))
	}
	got := h.enq.confirms[0]
	if got.ExpectVersion != "0.62.0" {
		t.Fatalf("the confirmation poll must carry the staged target version, got %q", got.ExpectVersion)
	}
	// And it is the SAME path as "scheduled" in every respect that matters,
	// including the wider window an external-cron site earns.
	if want := time.Now().Add(agentConfirmDeadlineExternalCron); got.DeadlineAt.Before(want.Add(-time.Minute)) {
		t.Fatalf("deadline = %s, want the external-cron window", got.DeadlineAt)
	}
}

// ---------------------------------------------------------------------------
// Beat 1: an agent that predates the channel
// ---------------------------------------------------------------------------

// TestOldAgentWithoutTheSelfUpdateRouteIsNotAFailure covers the site every
// FIRST rollout of this channel is made of.
//
// An agent that predates this release has no self-update route, so the request
// 404s with rest_no_route. Recorded as TaskFailed, that is a canary "failure"
// an operator cannot tell apart from a genuinely broken build: the rollout stops
// with an error pointing at the release when the release is fine and the site
// simply has nothing to receive the command.
//
// It is not a failure. Nothing was sent that the site could act on and nothing
// was applied. It is not a confirmation either, so it is recorded non-confirming
// and the gate stays shut on the strength of it.
func TestOldAgentWithoutTheSelfUpdateRouteIsNotAFailure(t *testing.T) {
	cmd := &recordingSelfUpdater{err: oldAgentRouteMissing()}
	h := newAgentHarness(t, 100, cmd, &fakeVersions{}, true)

	if err := h.work(context.Background(), 0); err != nil {
		t.Fatalf("Work: %v", err)
	}

	canary := h.store.Task(0)
	if canary.Status == TaskFailed {
		t.Fatalf("an agent with no self-update route is not a site failure: %+v", canary)
	}
	if canary.Status != TaskSkipped {
		t.Fatalf("status = %q, want %q (neither confirmed nor failed)", canary.Status, TaskSkipped)
	}
	if !strings.Contains(canary.Detail, "predates the self-update channel") {
		t.Fatalf("the detail must say what actually happened: %q", canary.Detail)
	}
	if canary.Error != "" {
		t.Fatalf("a site that was never able to receive the command has no error to report, got %q", canary.Error)
	}

	// The tally is what the gate reads, and it must score this as evidence of
	// nothing rather than as a broken site.
	tally := tallyWave(waveOrder(h.store.snapshotLocked()), WaveRange{Start: 0, End: 1})
	if tally.Failed != 0 {
		t.Fatalf("an agent that predates the channel counted as %d failure(s)", tally.Failed)
	}
	if tally.Other != 1 {
		t.Fatalf("want it scored as neither, got Other=%d", tally.Other)
	}

	// A wave of ONLY such sites still stops the run, which is correct: it
	// proved nothing, and this channel cannot upgrade an agent that has no
	// self-update route. What changed is the reason, which now points at the
	// sites rather than at the build.
	if h.store.RunStatus() != RunHalted {
		t.Fatalf("run status = %q, want %q", h.store.RunStatus(), RunHalted)
	}
	if reason := h.waves.lastHaltReason(); strings.Contains(reason, "failed on") {
		t.Fatalf("the halt must not read as a canary failure: %q", reason)
	} else if !strings.Contains(reason, "not attempted") {
		t.Fatalf("the halt must say the wave attempted nothing: %q", reason)
	}
}

// TestAMixedWaveIsNotFailedByAgentsThatPredateTheChannel is where the
// classification pays for itself. A real fleet is mixed, so a pilot wave holds
// both kinds of site at once.
//
// Scored as failures, ONE old agent among three pilot sites is a 33% failure
// rate against a 10% threshold, and the rollout halts on sites that were never
// eligible to be upgraded through this channel in the first place. Scored as
// "proved nothing", the two sites that did confirm carry the wave.
func TestAMixedWaveIsNotFailedByAgentsThatPredateTheChannel(t *testing.T) {
	armed := agentcmd.AgentSelfUpdateResponse{
		Status: agentcmd.SelfUpdateScheduled, FromVersion: agentPlanFrom, ToVersion: agentPlanTarget,
		CronMode: agentcmd.CronModeLoopback,
	}
	cmd := &scriptedSelfUpdater{t: t, answers: []selfUpdateAnswer{
		// wave 0: the canary upgrades normally.
		{resp: armed},
		// wave 1, first site: an agent from before this channel existed.
		{err: oldAgentRouteMissing()},
		// wave 1, the other two sites.
		{resp: armed},
		{resp: armed},
	}}
	versions := &fakeVersions{versions: map[uuid.UUID]string{}}
	h := newAgentHarness(t, 10, cmd, versions, true)
	confirm := NewAgentConfirmWorker(h.w)

	// Confirming a task the way beat 3 does: the new code reports its version.
	confirmTask := func(i int) {
		t.Helper()
		task := h.store.Task(i)
		args, found := AgentConfirmArgs{}, false
		for _, c := range h.enq.confirms {
			if c.TaskID == task.ID {
				args, found = c, true
			}
		}
		if !found {
			t.Fatalf("task %d never scheduled a confirmation poll", i)
		}
		versions.versions[args.SiteID] = agentPlanTarget
		if err := confirm.Work(context.Background(), &river.Job[AgentConfirmArgs]{Args: args}); err != nil {
			t.Fatalf("confirm task %d: %v", i, err)
		}
		if got := h.store.Task(i).Status; got != TaskSucceeded {
			t.Fatalf("task %d status = %q, want %q", i, got, TaskSucceeded)
		}
	}

	if err := h.work(context.Background(), 0); err != nil {
		t.Fatalf("canary Work: %v", err)
	}
	confirmTask(0)

	for i := 1; i <= 3; i++ {
		if err := h.work(context.Background(), i); err != nil {
			t.Fatalf("wave 1 site %d Work: %v", i, err)
		}
	}
	if got := h.store.Task(1).Status; got != TaskSkipped {
		t.Fatalf("the old agent's task is %q, want %q", got, TaskSkipped)
	}
	confirmTask(2)
	confirmTask(3)

	tally := tallyWave(waveOrder(h.store.snapshotLocked()), WaveRange{Start: 1, End: 4})
	if tally.Failed != 0 {
		t.Fatalf("the pilot recorded %d failure(s); an agent that predates the channel is not one", tally.Failed)
	}
	if tally.Confirmed != 2 || tally.Other != 1 {
		t.Fatalf("pilot tally = %+v, want 2 confirmed and 1 neither", tally)
	}
	if h.store.RunStatus() == RunHalted {
		t.Fatalf("a pilot that confirmed two of three sites must not halt the run: %q", h.waves.lastHaltReason())
	}
	// And the rollout continues: wave 2's six sites are enqueued.
	state := DeriveAgentWaveState(h.store.snapshotLocked())
	if state.OpenThrough != 3 {
		t.Fatalf("OpenThrough = %d, want 3 (wave 2 open)", state.OpenThrough)
	}
}

// ---------------------------------------------------------------------------
// Beat 1: "up_to_date" is a disagreement, not a confirmation
// ---------------------------------------------------------------------------

// TestUpToDateIsCheckedAgainstThePublishedVersion pins the rule that makes an
// "up_to_date" answer meaningful: a confirmation must prove THE SITE reached the
// version this run set out to install, never that the published version moved
// down to meet the site.
//
// Every case therefore states the run's whole premise (the plan-time target and
// the version the site itself reported at plan time) alongside what the agent
// claims now and what the control plane publishes now. The old version of this
// test named only the last two, which is exactly the pair that cannot tell a
// site that upgraded from a manifest that was reverted.
func TestUpToDateIsCheckedAgainstThePublishedVersion(t *testing.T) {
	cases := []struct {
		name string
		// planned is the version the run was created to install, recorded on
		// every task. Empty stands in for a pre-existing row that predates the
		// planned-target record.
		planned string
		// plannedFrom is what the site reported when the run was planned.
		plannedFrom string
		// published is what this control plane says the current build is NOW.
		published string
		// reported is the version the agent says it is already running NOW.
		reported   string
		noReleases bool
		wantStatus string
		wantHalted bool
		// wantDetail are substrings the operator-facing detail must carry.
		wantDetail []string
	}{
		{
			// The site MOVED: it ran 0.61.80 when the run was planned and now
			// reports the 0.62.0 this run set out to install. That, and only
			// that, is what makes this a confirmation.
			name:    "the site reached the version this run set out to install",
			planned: "0.62.0", plannedFrom: "0.61.80",
			published: "0.62.0", reported: "0.62.0",
			wantStatus: TaskSucceeded,
		},
		{
			// Past the target (a site that took a later build in the meantime)
			// still reached it.
			name:    "the site is past the version this run set out to install",
			planned: "0.62.0", plannedFrom: "0.61.80",
			published: "0.62.0", reported: "0.62.1",
			wantStatus: TaskSucceeded,
		},
		{
			// THE FAILURE THIS RULE EXISTS FOR. The operator reverted the
			// manifest to the fleet's own build, so published == reported ==
			// the version this site was already running when the run was
			// planned. Nothing moved except the manifest. Under the old rule
			// (score the answer against a live read of the manifest) this
			// recorded SUCCEEDED.
			name:    "the manifest was reverted to the version the site already ran",
			planned: "0.62.0", plannedFrom: "0.61.80",
			published: "0.61.80", reported: "0.61.80",
			wantStatus: TaskSkipped,
			// The premise is void as well: the target is no longer published.
			wantHalted: true,
			wantDetail: []string{"0.62.0", "0.61.80"},
		},
		{
			// The same revert, seen on a site that DID upgrade before the
			// revert landed. The site's own outcome is honest (it reached the
			// target), but the run is over: no remaining site can reach a
			// version that is no longer published.
			name:    "a site that did reach the target before the manifest was reverted",
			planned: "0.62.0", plannedFrom: "0.61.80",
			published: "0.61.80", reported: "0.62.0",
			wantStatus: TaskSucceeded,
			wantHalted: true,
		},
		{
			// The ordinary disagreement: the target is still published and this
			// site simply has not got there.
			name:    "the site is still behind the version this run set out to install",
			planned: "0.62.0", plannedFrom: "0.61.80",
			published: "0.62.0", reported: "0.61.80",
			wantStatus: TaskSkipped,
			wantDetail: []string{"0.62.0", "0.61.80"},
		},
		{
			// latest.json absent entirely: the manifest handler 204s and the
			// agent has nothing to offer. Proving nothing must not read as
			// proving something. It is NOT a void premise either: "cannot read
			// the manifest" is not "the manifest was reverted", and a transient
			// read failure must not halt a fleet on its own.
			name:    "the published version cannot be read",
			planned: "0.62.0", plannedFrom: "0.61.80",
			published: "", reported: "0.61.80",
			wantStatus: TaskSkipped,
			wantDetail: []string{"missing", "0.62.0"},
		},
		{
			name:    "there is no release reader at all",
			planned: "0.62.0", plannedFrom: "0.61.80",
			noReleases: true, reported: "0.62.0",
			// The site did reach the run's target, and that fact does not
			// depend on being able to read the manifest at all: the target was
			// recorded when the run was planned.
			wantStatus: TaskSucceeded,
		},
		{
			// A row created before the planned target was persisted. There is
			// nothing to check the claim against, so it confirms nothing.
			// Fail-closed is the only safe direction for an in-flight run
			// spanning the deploy that introduced the record.
			name:    "the run recorded no target at all",
			planned: "", plannedFrom: "0.61.80",
			published: "0.61.80", reported: "0.61.80",
			wantStatus: TaskSkipped,
			wantDetail: []string{"did not record"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &recordingSelfUpdater{resp: agentcmd.AgentSelfUpdateResponse{
				Status: agentcmd.SelfUpdateUpToDate, FromVersion: tc.reported}}
			h := newAgentHarness(t, 10, cmd, &fakeVersions{}, true).withPlan(tc.planned, tc.plannedFrom)
			if tc.noReleases {
				h.withoutReleases()
			} else {
				h.withPublished(tc.published)
			}

			if err := h.work(context.Background(), 0); err != nil {
				t.Fatalf("Work: %v", err)
			}
			task := h.store.Task(0)
			if task.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (detail: %s)", task.Status, tc.wantStatus, task.Detail)
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(task.Detail, want) {
					t.Fatalf("detail = %q, want it to name %q", task.Detail, want)
				}
			}
			if tc.wantStatus == TaskSkipped && !strings.Contains(task.Detail, tc.reported) {
				t.Fatalf("a non-confirming answer must name the site's own version; detail = %q", task.Detail)
			}
			if halted := h.store.RunStatus() == RunHalted; halted != tc.wantHalted && tc.wantStatus == TaskSucceeded {
				// A non-confirming canary halts through the ordinary wave gate
				// (it proved nothing), so only the CONFIRMING cases isolate the
				// void-premise halt from that.
				t.Fatalf("run halted = %v, want %v (status %q)", halted, tc.wantHalted, task.Status)
			}
		})
	}
}

// TestRevertedReleaseManifestHaltsInsteadOfReportingSuccess is the failure this
// whole check exists for, driven end to end over a real fleet.
//
// The setup is the emergency path exactly as an operator walks it: the run is
// planned while 0.62.0 is published, the canary bricks a site, and the operator
// reverts latest.json to 0.61.80, the last known good build, which is what the
// fleet is already running. That revert is the natural reflex during an
// incident, and the release reader caches for five minutes, so it propagates
// well inside any rollout window.
//
// Every subsequent arm then answers "up_to_date", because the agent's downgrade
// guard refuses the older manifest. Scored against a LIVE read of that manifest,
// all 100 sites matched the published version, all 100 recorded succeeded, the
// Failed tally stayed at zero, every wave gate passed, and the run reported
// "completed, succeeded=100" with NOT ONE AGENT MOVED. The same fleet must now
// stop at the canary.
//
// Note what makes this test the thing its name says, and what the previous
// version of it was missing: the manifest is genuinely REVERTED here (published
// drops to the fleet's own 0.61.80). Publishing something the fleet has not
// reached yet only tests "the site is still behind", which was never the risky
// case, because a still-newer published version cannot be matched by a site that
// stood still.
func TestRevertedReleaseManifestHaltsInsteadOfReportingSuccess(t *testing.T) {
	cmd := &recordingSelfUpdater{resp: agentcmd.AgentSelfUpdateResponse{
		Status: agentcmd.SelfUpdateUpToDate, FromVersion: agentPlanFrom,
		Detail: "no update available"}}
	// Planned against 0.62.0 (the store's plan-time record), then reverted to
	// the version the whole fleet already runs.
	h := newAgentHarness(t, 100, cmd, &fakeVersions{}, true).withPublished(agentPlanFrom)

	if err := h.work(context.Background(), 0); err != nil {
		t.Fatalf("Work: %v", err)
	}

	canary := h.store.Task(0)
	if canary.Status == TaskSucceeded {
		t.Fatalf("a site that stood still while the manifest moved down to meet it must never confirm itself: %+v", canary)
	}
	if canary.Status != TaskSkipped {
		t.Fatalf("canary status = %q, want %q (neither confirmed nor failed)", canary.Status, TaskSkipped)
	}
	// The detail has to name all three numbers, because they are what separates
	// "reverted manifest" from "site has not upgraded yet".
	for _, want := range []string{agentPlanTarget, agentPlanFrom} {
		if !strings.Contains(canary.Detail, want) {
			t.Fatalf("detail must name the planned target, the site's version and the published version: %q", canary.Detail)
		}
	}

	// The rollout must stop, and it must stop here.
	if h.store.RunStatus() != RunHalted {
		t.Fatalf("run status = %q, want %q: a wave that confirmed nothing may not open the next one",
			h.store.RunStatus(), RunHalted)
	}
	for i := 1; i < 100; i++ {
		if got := h.store.Task(i).Status; got != TaskCancelled {
			t.Fatalf("task %d status = %q, want %q", i, got, TaskCancelled)
		}
	}
	if len(cmd.calls) != 1 {
		t.Fatalf("exactly one site may be contacted before the halt, got %d", len(cmd.calls))
	}

	// And the tally itself: neither confirmed nor failed.
	tally := tallyWave(waveOrder(h.store.snapshotLocked()), WaveRange{Start: 0, End: 1})
	if tally.Confirmed != 0 {
		t.Fatalf("a disagreement counted as %d confirmation(s)", tally.Confirmed)
	}
	if tally.Failed != 0 {
		t.Fatalf("a disagreement counted as %d failure(s); it is evidence of nothing, not of a broken site", tally.Failed)
	}
	if tally.Other != 1 {
		t.Fatalf("want the disagreement scored as neither, got Other=%d", tally.Other)
	}
}

// TestACanaryThatAppliedNothingCannotOpenWaveOne is the consequence that makes
// the rule above load-bearing rather than cosmetic, and it is the shape a mixed
// fleet actually produces.
//
// The canary is an arbitrary site (waveOrder is a uuid ordering, not a choice),
// so on a fleet where some sites already run the reverted build the canary can
// BE one of them. It answers up_to_date, matches the reverted manifest exactly,
// and applies nothing. If that counts as a confirmation, wave 0 "proves" the
// upgrade works and waves 1 and 2 open against sites that genuinely are behind:
// a rollout authorised by a site that did nothing.
//
// The wave gate is the entire safety mechanism of this feature, so the property
// under test is stated directly: no task outside wave 0 may become dispatchable,
// be enqueued, or be contacted.
func TestACanaryThatAppliedNothingCannotOpenWaveOne(t *testing.T) {
	cmd := &recordingSelfUpdater{resp: agentcmd.AgentSelfUpdateResponse{
		Status: agentcmd.SelfUpdateUpToDate, FromVersion: agentPlanFrom}}
	h := newAgentHarness(t, 100, cmd, &fakeVersions{}, true).withPublished(agentPlanFrom)

	if err := h.work(context.Background(), 0); err != nil {
		t.Fatalf("Work: %v", err)
	}

	state := DeriveAgentWaveState(h.store.snapshotLocked())
	if !state.Halt {
		t.Fatal("a canary that applied nothing must halt the run")
	}
	if state.OpenThrough != 0 {
		t.Fatalf("OpenThrough = %d, want 0: no wave may be open behind a canary that proved nothing", state.OpenThrough)
	}
	if len(state.DispatchableTasks()) != 0 {
		t.Fatalf("a canary that applied nothing made %d task(s) dispatchable", len(state.DispatchableTasks()))
	}
	// Named directly: the first site of wave 1.
	wave1 := h.store.Task(1)
	if state.GateOpenFor(wave1.ID) {
		t.Fatal("wave 1 opened on the strength of a canary that applied nothing")
	}
	if len(h.enq.tasks) != 0 {
		t.Fatalf("no further wave may be enqueued: %+v", h.enq.tasks)
	}
	if len(cmd.calls) != 1 {
		t.Fatalf("%d sites were contacted; only the canary may be", len(cmd.calls))
	}

	// Drive the wave-1 job anyway, as a duplicate enqueue or a manual retry
	// would: the gate, not the absence of a job, is what protects the fleet.
	if err := h.work(context.Background(), 1); err != nil {
		t.Fatalf("wave 1 Work: %v", err)
	}
	if len(cmd.calls) != 1 {
		t.Fatalf("a wave-1 job reached its site after a canary that applied nothing: %d calls", len(cmd.calls))
	}
	if got := h.store.Task(1).Status; got != TaskCancelled {
		t.Fatalf("wave-1 task status = %q, want %q", got, TaskCancelled)
	}
}

// ---------------------------------------------------------------------------
// Rollback is never attempted
// ---------------------------------------------------------------------------

// TestAgentTargetNeverRollsBack proves the agent branch cannot reach the
// rollback machinery. The Worker is built with a NIL ordinary Commander, which
// is what Worker.rollback and Worker.runApply both call through, so any
// attempt to snapshot, health-probe or roll back would panic. The task is
// driven to every failure outcome the branch has, because a rollback attempt
// would be most tempting exactly there.
//
// A rollback for this target is undeliverable by construction: the code that
// would receive and execute the rollback command is the code that was being
// replaced. Attempting it would only add a misleading "rollback FAILED" to a
// task whose real problem is something else.
func TestAgentTargetNeverRollsBack(t *testing.T) {
	cases := []struct {
		name string
		resp agentcmd.AgentSelfUpdateResponse
		err  error
	}{
		{"agent reported error", agentcmd.AgentSelfUpdateResponse{Status: agentcmd.SelfUpdateError}, nil},
		{"transport error", agentcmd.AgentSelfUpdateResponse{}, errors.New("connection refused")},
		{"scheduled but never confirmed", agentcmd.AgentSelfUpdateResponse{
			Status: agentcmd.SelfUpdateScheduled, FromVersion: "0.61.80", ToVersion: "0.62.0"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &recordingSelfUpdater{resp: tc.resp, err: tc.err}
			versions := &fakeVersions{versions: map[uuid.UUID]string{}}
			h := newAgentHarness(t, 3, cmd, versions, true)

			// A nil Commander means w.cmd.Rollback would panic; recovering here
			// turns that into a readable failure rather than a crashed test run.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("the agent target reached the plugin update/rollback path: %v", r)
				}
			}()

			if err := h.work(context.Background(), 0); err != nil {
				t.Fatalf("Work: %v", err)
			}
			// Drive the scheduled case all the way through its deadline too,
			// since that is the outcome that most resembles a broken update.
			if len(h.enq.confirms) == 1 {
				args := h.enq.confirms[0]
				args.DeadlineAt = time.Now().Add(-time.Minute)
				cw := NewAgentConfirmWorker(h.w)
				if err := cw.Work(context.Background(), &river.Job[AgentConfirmArgs]{Args: args}); err != nil {
					t.Fatalf("confirm Work: %v", err)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Beat 3: confirmation
// ---------------------------------------------------------------------------

// TestConfirmationIsTheOnlySuccess walks the confirmation poll through its
// three outcomes: still waiting, confirmed, and past the deadline.
func TestConfirmationIsTheOnlySuccess(t *testing.T) {
	arm := agentcmd.AgentSelfUpdateResponse{
		Status: agentcmd.SelfUpdateScheduled, FromVersion: "0.61.80", ToVersion: "0.62.0",
		CronMode: agentcmd.CronModeLoopback,
	}

	t.Run("still on the old version keeps waiting", func(t *testing.T) {
		versions := &fakeVersions{versions: map[uuid.UUID]string{}}
		h := newAgentHarness(t, 10, &recordingSelfUpdater{resp: arm}, versions, true)
		if err := h.work(context.Background(), 0); err != nil {
			t.Fatalf("Work: %v", err)
		}
		args := h.enq.confirms[0]
		versions.versions[args.SiteID] = "0.61.80" // unchanged

		cw := NewAgentConfirmWorker(h.w)
		err := cw.Work(context.Background(), &river.Job[AgentConfirmArgs]{Args: args})
		if !isSnooze(err) {
			t.Fatalf("an unconfirmed upgrade inside its window must snooze, got %v", err)
		}
		if terminal(h.store.Task(0).Status) {
			t.Fatalf("the task must stay running while waiting, got %q", h.store.Task(0).Status)
		}
	})

	t.Run("the new version reporting in confirms", func(t *testing.T) {
		versions := &fakeVersions{versions: map[uuid.UUID]string{}}
		h := newAgentHarness(t, 10, &recordingSelfUpdater{resp: arm}, versions, true)
		if err := h.work(context.Background(), 0); err != nil {
			t.Fatalf("Work: %v", err)
		}
		args := h.enq.confirms[0]
		versions.versions[args.SiteID] = "0.62.0" // the new code phoned home

		cw := NewAgentConfirmWorker(h.w)
		if err := cw.Work(context.Background(), &river.Job[AgentConfirmArgs]{Args: args}); err != nil {
			t.Fatalf("confirm Work: %v", err)
		}
		task := h.store.Task(0)
		if task.Status != TaskSucceeded {
			t.Fatalf("status = %q, want %q", task.Status, TaskSucceeded)
		}
		if task.ToVersion != "0.62.0" {
			t.Fatalf("to_version = %q, want the version the site actually reported", task.ToVersion)
		}
		// Confirming the canary is what opens wave 1, and only wave 1.
		if len(h.enq.tasks) != 3 {
			t.Fatalf("confirming the canary must enqueue wave 1's three sites, got %d", len(h.enq.tasks))
		}
	})

	t.Run("deadline expiry fails as unconfirmed", func(t *testing.T) {
		versions := &fakeVersions{versions: map[uuid.UUID]string{}}
		h := newAgentHarness(t, 10, &recordingSelfUpdater{resp: arm}, versions, true)
		if err := h.work(context.Background(), 0); err != nil {
			t.Fatalf("Work: %v", err)
		}
		args := h.enq.confirms[0]
		versions.versions[args.SiteID] = "0.61.80"
		args.DeadlineAt = time.Now().Add(-time.Second) // window already closed

		cw := NewAgentConfirmWorker(h.w)
		if err := cw.Work(context.Background(), &river.Job[AgentConfirmArgs]{Args: args}); err != nil {
			t.Fatalf("confirm Work: %v", err)
		}
		task := h.store.Task(0)
		if task.Status != TaskFailed {
			t.Fatalf("status = %q, want %q", task.Status, TaskFailed)
		}
		if task.Error != "agent_self_update_unconfirmed" {
			t.Fatalf("an unconfirmed timeout needs its own distinct reason, got %q", task.Error)
		}
		if !strings.Contains(task.Detail, "unconfirmed") {
			t.Fatalf("detail must say unconfirmed: %q", task.Detail)
		}
		// It must also say the site was not necessarily touched, because that
		// is the difference between "go look at this site" and "this site is
		// fine, its cron is not".
		if !strings.Contains(task.Detail, "not necessarily") {
			t.Fatalf("detail must explain that nothing was necessarily applied: %q", task.Detail)
		}
		// And an unconfirmed canary halts the fleet.
		if h.store.RunStatus() != RunHalted {
			t.Fatalf("run status = %q, want %q", h.store.RunStatus(), RunHalted)
		}
	})
}

// TestConfirmTimeoutCarriesTheAgentsOwnAccountOfTheApply covers the one thing
// this control plane cannot work out for itself.
//
// Beat 2 runs inside a WordPress cron request that has no CP response to ride
// on, so from here "the cron run never happened" and "the cron run happened and
// the upgrade failed" are the SAME silence: the site keeps reporting the old
// version until the deadline expires either way. They are different incidents.
// One means the site was never touched and its cron needs looking at; the other
// means the upgrade ran and broke, on a site that was touched.
//
// The agent records which of the two it was and replays it on its next metadata
// push. Discarding that record throws away the only account of what happened.
func TestConfirmTimeoutCarriesTheAgentsOwnAccountOfTheApply(t *testing.T) {
	arm := agentcmd.AgentSelfUpdateResponse{
		Status: agentcmd.SelfUpdateScheduled, FromVersion: agentPlanFrom, ToVersion: agentPlanTarget,
		CronMode: agentcmd.CronModeLoopback,
	}
	stamped := time.Date(2026, 7, 28, 9, 30, 0, 0, time.UTC)

	cases := []struct {
		name string
		// result is what the agent reported; nil means it reported nothing,
		// which is what every agent predating the record does.
		result *AgentApplyResult
		// lookupErr stands in for the site read failing outright.
		lookupErr error
		// noLookup stands in for a control plane that never wired the lookup.
		noLookup bool
		want     []string
		notWant  []string
	}{
		{
			name:   "the apply ran and failed",
			result: &AgentApplyResult{Status: "failed", FromVersion: agentPlanFrom, ToVersion: agentPlanTarget, Detail: "Upgrader threw: could not create directory", At: stamped},
			want:   []string{"FAILED", "could not create directory", agentPlanTarget, "2026-07-28T09:30:00Z"},
		},
		{
			name:   "the staged upgrade expired before any apply ran",
			result: &AgentApplyResult{Status: "expired", FromVersion: agentPlanFrom, ToVersion: agentPlanTarget, Detail: "Staged self-update expired before the apply request ran.", At: stamped},
			want:   []string{"EXPIRED", "never happened in time"},
		},
		{
			name:   "the agent says it did apply, so the version report is what is missing",
			result: &AgentApplyResult{Status: "applied", FromVersion: agentPlanFrom, ToVersion: agentPlanTarget, At: stamped},
			want:   []string{"DID apply", "metadata push"},
		},
		{
			name:   "the on-disk version was already at the target",
			result: &AgentApplyResult{Status: "already_applied", FromVersion: agentPlanTarget, ToVersion: agentPlanTarget, At: stamped},
			want:   []string{"ALREADY at or past"},
		},
		{
			// Forward compatibility: a status a newer agent introduces must
			// reach the operator verbatim rather than being swallowed by an
			// older control plane that does not recognise it.
			name:   "a status this control plane does not recognise",
			result: &AgentApplyResult{Status: "quarantined", ToVersion: agentPlanTarget, Detail: "held by the host", At: stamped},
			want:   []string{`"quarantined"`, "held by the host"},
		},
		{
			// Old agents send nothing. The timeout must read exactly as it did
			// before this record existed.
			name:    "the agent reported nothing",
			result:  nil,
			notWant: []string{"The agent's own record"},
		},
		{
			name:      "the site read fails",
			lookupErr: errors.New("database unavailable"),
			notWant:   []string{"The agent's own record", "database unavailable"},
		},
		{
			name:     "the lookup was never wired",
			noLookup: true,
			notWant:  []string{"The agent's own record"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			versions := &fakeVersions{versions: map[uuid.UUID]string{}}
			h := newAgentHarness(t, 10, &recordingSelfUpdater{resp: arm}, versions, true)
			if err := h.work(context.Background(), 0); err != nil {
				t.Fatalf("Work: %v", err)
			}
			args := h.enq.confirms[0]
			versions.versions[args.SiteID] = agentPlanFrom // never moved

			if !tc.noLookup {
				results := &fakeApplyResults{results: map[uuid.UUID]AgentApplyResult{}, err: tc.lookupErr}
				if tc.result != nil {
					results.results[args.SiteID] = *tc.result
				}
				h.withApplyResults(results)
			}

			args.DeadlineAt = time.Now().Add(-time.Second)
			cw := NewAgentConfirmWorker(h.w)
			if err := cw.Work(context.Background(), &river.Job[AgentConfirmArgs]{Args: args}); err != nil {
				t.Fatalf("confirm Work: %v", err)
			}

			task := h.store.Task(0)
			// Whatever the agent said, the outcome itself is unchanged: an
			// unconfirmed upgrade is a failed task. The record explains it, it
			// does not excuse it.
			if task.Status != TaskFailed {
				t.Fatalf("status = %q, want %q", task.Status, TaskFailed)
			}
			if task.Error != "agent_self_update_unconfirmed" {
				t.Fatalf("error = %q, want the distinct unconfirmed reason", task.Error)
			}
			if !strings.Contains(task.Detail, "unconfirmed") {
				t.Fatalf("the base explanation must survive: %q", task.Detail)
			}
			for _, want := range tc.want {
				if !strings.Contains(task.Detail, want) {
					t.Fatalf("detail = %q, want it to carry %q", task.Detail, want)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(task.Detail, notWant) {
					t.Fatalf("detail = %q, must not carry %q", task.Detail, notWant)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Wave gating through the worker
// ---------------------------------------------------------------------------

// TestWaveZeroFailureHaltsRunThroughWorker is the end-to-end version of the
// canary gate: one failing site, and the other 99 are cancelled without ever
// being contacted.
func TestWaveZeroFailureHaltsRunThroughWorker(t *testing.T) {
	cmd := &recordingSelfUpdater{resp: agentcmd.AgentSelfUpdateResponse{
		Status: agentcmd.SelfUpdateError, FromVersion: "0.61.80", Detail: "manifest fetch failed"}}
	h := newAgentHarness(t, 100, cmd, &fakeVersions{}, true)

	if err := h.work(context.Background(), 0); err != nil {
		t.Fatalf("Work: %v", err)
	}

	if got := h.store.Task(0).Status; got != TaskFailed {
		t.Fatalf("the canary status = %q, want %q", got, TaskFailed)
	}
	if h.store.RunStatus() != RunHalted {
		t.Fatalf("run status = %q, want %q", h.store.RunStatus(), RunHalted)
	}
	for i := 1; i < 100; i++ {
		if got := h.store.Task(i).Status; got != TaskCancelled {
			t.Fatalf("task %d status = %q, want %q", i, got, TaskCancelled)
		}
	}
	if len(cmd.calls) != 1 {
		t.Fatalf("exactly one site may be contacted before the halt, got %d", len(cmd.calls))
	}
	if len(h.enq.tasks) != 0 {
		t.Fatalf("a halted run enqueues nothing further: %+v", h.enq.tasks)
	}
}

// TestWaveGateSnoozesUntilPriorWaveConfirms is the end-to-end ordering proof:
// a wave-1 job that runs before the canary confirmed snoozes WITHOUT
// contacting its site, then proceeds once the canary is confirmed.
func TestWaveGateSnoozesUntilPriorWaveConfirms(t *testing.T) {
	cmd := &recordingSelfUpdater{resp: agentcmd.AgentSelfUpdateResponse{
		Status: agentcmd.SelfUpdateScheduled, FromVersion: "0.61.80", ToVersion: "0.62.0"}}
	h := newAgentHarness(t, 10, cmd, &fakeVersions{}, true)

	// The canary arms and is awaiting confirmation.
	if err := h.work(context.Background(), 0); err != nil {
		t.Fatalf("Work: %v", err)
	}
	armed := len(cmd.calls)

	// A wave-1 job arrives early (a duplicate enqueue, a manual retry, a
	// leftover job). It must not touch its site.
	err := h.work(context.Background(), 1)
	if !isSnooze(err) {
		t.Fatalf("a task whose wave has not opened must snooze, got %v", err)
	}
	if len(cmd.calls) != armed {
		t.Fatalf("a gated task contacted its site: %d calls, want %d", len(cmd.calls), armed)
	}
	if got := h.store.Task(1).Status; got != TaskPending {
		t.Fatalf("a gated task stays pending, got %q", got)
	}

	// The canary confirms; now the same job proceeds.
	h.store.setStatus(0, TaskSucceeded)
	if err := h.work(context.Background(), 1); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(cmd.calls) != armed+1 {
		t.Fatalf("the wave-1 task must dispatch once its wave opened: %d calls", len(cmd.calls))
	}

	// Wave 2 is still shut behind the unconfirmed pilot.
	if err := h.work(context.Background(), 4); !isSnooze(err) {
		t.Fatalf("wave 2 must stay gated behind an unconfirmed pilot, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Halting an in-flight rollout
// ---------------------------------------------------------------------------

// TestHaltCancelsOnlyTasksNothingWasSentFor is the kill switch's honesty
// property. A halt cancels what was never dispatched and LEAVES RUNNING TASKS
// ALONE.
//
// Cancelling a running task is wrong twice over. It records a falsehood:
// model.go defines TaskCancelled as "nothing was ever sent to this site", and a
// running agent self-update task has already had its command delivered and a
// cron event spawned on the site. And it blinds the control plane:
// AgentConfirmWorker.Work short-circuits on a terminal status, so cancelling
// the row stops the poll that would have established whether that site upgraded
// or bricked, at the exact moment an operator hit the kill switch and most
// needs to know.
func TestHaltCancelsOnlyTasksNothingWasSentFor(t *testing.T) {
	tenant := uuid.New()
	s := newAgentStore(tenant, 10)
	repo := &fakeWaveRepo{s: s}

	// A realistic in-flight moment: the canary confirmed, one wave-1 site is
	// armed and awaiting beat 3, one already failed, and the rest are pending.
	s.setStatus(0, TaskSucceeded)
	s.setStatus(1, TaskRunning)
	s.setStatus(2, TaskFailed)

	ev, err := repo.HaltAgentRun(context.Background(), tenant, s.run.ID, "stopped by the operator")
	if err != nil {
		t.Fatalf("halt: %v", err)
	}

	if got := s.Task(1).Status; got != TaskRunning {
		t.Fatalf("the armed, in-flight task was moved to %q: a halt must not claim nothing was sent to a site that was contacted", got)
	}
	if got := s.Task(0).Status; got != TaskSucceeded {
		t.Fatalf("task 0 status = %q, want the recorded outcome untouched", got)
	}
	if got := s.Task(2).Status; got != TaskFailed {
		t.Fatalf("task 2 status = %q, want the recorded outcome untouched", got)
	}
	for i := 3; i < 10; i++ {
		if got := s.Task(i).Status; got != TaskCancelled {
			t.Fatalf("task %d status = %q, want %q: nothing was ever sent for it", i, got, TaskCancelled)
		}
	}
	if ev.Cancelled != 7 {
		t.Fatalf("cancelled %d tasks, want the 7 that were still pending", ev.Cancelled)
	}
	if s.RunStatus() != RunHalted {
		t.Fatalf("run status = %q, want %q", s.RunStatus(), RunHalted)
	}
}

// TestHaltLeavesTheConfirmPollAbleToResolveARunningTask is the consequence that
// makes the rule above matter, driven through the real confirmation worker: a
// site that was mid-upgrade when the run halted still gets its outcome
// established. Cancelling the row would have made Work return immediately on a
// terminal status, and the control plane would never have learned that this
// site did in fact come back on the new build.
func TestHaltLeavesTheConfirmPollAbleToResolveARunningTask(t *testing.T) {
	arm := agentcmd.AgentSelfUpdateResponse{
		Status: agentcmd.SelfUpdateScheduled, FromVersion: "0.61.80", ToVersion: "0.62.0",
		CronMode: agentcmd.CronModeLoopback,
	}
	versions := &fakeVersions{versions: map[uuid.UUID]string{}}
	h := newAgentHarness(t, 10, &recordingSelfUpdater{resp: arm}, versions, true)

	// The canary arms and is awaiting confirmation.
	if err := h.work(context.Background(), 0); err != nil {
		t.Fatalf("Work: %v", err)
	}
	args := h.enq.confirms[0]

	// The operator hits the kill switch mid-flight.
	if _, err := h.waves.HaltAgentRun(context.Background(), args.TenantID, args.RunID, "stopped by the operator"); err != nil {
		t.Fatalf("halt: %v", err)
	}
	if got := h.store.Task(0).Status; got != TaskRunning {
		t.Fatalf("the in-flight canary is %q; the halt must leave it for its confirm job", got)
	}

	// The site comes back on the new build. The CP must still record it.
	versions.versions[args.SiteID] = "0.62.0"
	cw := NewAgentConfirmWorker(h.w)
	if err := cw.Work(context.Background(), &river.Job[AgentConfirmArgs]{Args: args}); err != nil {
		t.Fatalf("confirm Work: %v", err)
	}
	task := h.store.Task(0)
	if task.Status != TaskSucceeded {
		t.Fatalf("status = %q, want %q: the halt must not stop the CP learning what happened to a site it had already touched",
			task.Status, TaskSucceeded)
	}
	if task.ToVersion != "0.62.0" {
		t.Fatalf("to_version = %q, want the version the site actually reported", task.ToVersion)
	}
}

// TestAPostHaltWorkerCannotOverwriteACancelledTask covers the other side of the
// same race, one layer down. A worker that was already in flight when the halt
// landed comes back and reports its outcome. The task row was cancelled while
// it was away, and the first recorded outcome must win: overwriting 'cancelled'
// with 'succeeded' would make the kill switch look like it stopped a rollout
// that then reported itself a success.
//
// The precondition is enforced in SQL (db/query/updates.sql: FinishUpdateTask
// matches only status IN ('pending','running')), so it is atomic against the
// halt rather than a check-then-write in Go. fakeAgentRepo.FinishTask mirrors
// it, including returning ErrTaskNotOpen with the row unchanged.
func TestAPostHaltWorkerCannotOverwriteACancelledTask(t *testing.T) {
	arm := agentcmd.AgentSelfUpdateResponse{
		Status: agentcmd.SelfUpdateScheduled, FromVersion: "0.61.80", ToVersion: "0.62.0",
		CronMode: agentcmd.CronModeLoopback,
	}
	versions := &fakeVersions{versions: map[uuid.UUID]string{}}
	h := newAgentHarness(t, 10, &recordingSelfUpdater{resp: arm}, versions, true)
	if err := h.work(context.Background(), 0); err != nil {
		t.Fatalf("Work: %v", err)
	}
	args := h.enq.confirms[0]

	// The task is cancelled out from under the in-flight worker (the halt path
	// only cancels PENDING rows, so this stands in for a task that was still
	// pending when the halt landed and whose duplicate job arrived afterwards).
	h.store.setStatus(0, TaskCancelled)

	// The worker returns with a success it can no longer record.
	versions.versions[args.SiteID] = "0.62.0"
	cw := NewAgentConfirmWorker(h.w)
	if err := cw.Work(context.Background(), &river.Job[AgentConfirmArgs]{Args: args}); err != nil {
		t.Fatalf("confirm Work: %v", err)
	}
	if got := h.store.Task(0).Status; got != TaskCancelled {
		t.Fatalf("status = %q, want %q: a cancelled task must not be overwritten by a worker that finished after the halt", got, TaskCancelled)
	}

	// The same must hold for the beat-1 path, which reaches Worker.finish by a
	// different route.
	if err := h.w.finish(context.Background(), h.store.Task(0), TaskSucceeded, "0.61.80", "0.62.0", "late success", ""); err != nil {
		t.Fatalf("finish must treat an already-terminal task as a no-op, got %v", err)
	}
	if got := h.store.Task(0).Status; got != TaskCancelled {
		t.Fatalf("status = %q, want %q", got, TaskCancelled)
	}
}

// ---------------------------------------------------------------------------
// Dispatch cost
// ---------------------------------------------------------------------------

// TestWaveDispatchIsLinearInFleetSize is a performance property with a
// correctness consequence, so it is asserted like one.
//
// Every agent-task terminal transition re-judges the wave gate, and the gate
// used to answer with EVERY still-pending task of the open wave. On a fleet of
// n sites that means each of the final wave's completions re-enqueued its
// ~n still-pending siblings: O(n^2) River inserts, on the one feature whose
// entire purpose is fleet-wide rollout. At 1000 sites that is roughly half a
// million jobs for 1000 units of work.
//
// The driver below is the real state machine (DeriveAgentWaveState through the
// claim/evaluate repo) walked to completion, counting enqueues. Each task must
// be enqueued exactly once, as its wave opens.
func TestWaveDispatchIsLinearInFleetSize(t *testing.T) {
	const n = 200
	ctx := context.Background()
	tenant := uuid.New()
	s := newAgentStore(tenant, n)
	repo := &fakeWaveRepo{s: s}

	enqueues := map[uuid.UUID]int{}
	total := 0
	enqueue := func(id uuid.UUID) {
		enqueues[id]++
		total++
	}

	// Service.CreateRun enqueues only the canary; every later job exists
	// because a wave opened.
	queue := []uuid.UUID{s.tasks[0].ID}
	enqueue(queue[0])

	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]

		claim, _, err := repo.ClaimAgentWaveTask(ctx, tenant, s.run.ID, id)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if claim != ClaimProceed {
			continue
		}
		// The site upgrades and confirms.
		s.setStatusByID(id, TaskSucceeded)

		ev, err := repo.EvaluateAgentRun(ctx, tenant, s.run.ID)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if ev.Halted {
			t.Fatalf("a fleet that confirmed every site must not halt: %s", ev.Reason)
		}
		for _, task := range ev.Dispatchable {
			enqueue(task.ID)
			queue = append(queue, task.ID)
		}
	}

	// Every site ran...
	for i := 0; i < n; i++ {
		if got := s.Task(i).Status; got != TaskSucceeded {
			t.Fatalf("task %d status = %q: the rollout did not reach every site", i, got)
		}
	}
	// ...and each was enqueued exactly once.
	for i := 0; i < n; i++ {
		if got := enqueues[s.tasks[i].ID]; got != 1 {
			t.Fatalf("task %d was enqueued %d times, want exactly 1", i, got)
		}
	}
	if total != n {
		t.Fatalf("%d enqueues for %d sites: dispatch work must be linear in fleet size, not quadratic", total, n)
	}
}

// TestWaveGatingIsUnchangedByTheDispatchFix guards the boundary of that
// performance fix: the ORDER and the GATE are untouched. A wave still opens
// only when the wave before it has confirmed, still dispatches exactly its own
// sites, and a duplicate evaluation still produces no extra dispatch.
func TestWaveGatingIsUnchangedByTheDispatchFix(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()
	s := newAgentStore(tenant, 10) // waves: [0,1) [1,4) [4,10)
	repo := &fakeWaveRepo{s: s}

	// Nothing opens behind an unfinished canary.
	ev, err := repo.EvaluateAgentRun(ctx, tenant, s.run.ID)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(ev.Dispatchable) != 1 || ev.Dispatchable[0].ID != s.tasks[0].ID {
		t.Fatalf("wave 0 is open on sight and is the only dispatchable wave, got %d task(s)", len(ev.Dispatchable))
	}

	// The canary confirms: exactly wave 1's three sites, and nothing else.
	s.setStatus(0, TaskSucceeded)
	ev, err = repo.EvaluateAgentRun(ctx, tenant, s.run.ID)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(ev.Dispatchable) != 3 {
		t.Fatalf("want wave 1's three sites, got %d", len(ev.Dispatchable))
	}
	for _, task := range ev.Dispatchable {
		if task.ID == s.tasks[0].ID || task.ID == s.tasks[4].ID {
			t.Fatalf("the dispatch set leaked outside wave 1: %s", task.ID)
		}
	}

	// Wave 1 starts moving. Its jobs already exist, so nothing more is
	// dispatched, this is the quadratic re-enqueue that used to happen here.
	s.setStatus(1, TaskRunning)
	ev, err = repo.EvaluateAgentRun(ctx, tenant, s.run.ID)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(ev.Dispatchable) != 0 {
		t.Fatalf("a wave already being worked must not be re-dispatched, got %d task(s)", len(ev.Dispatchable))
	}

	// And wave 2 stays shut until wave 1 has confirmed, gate unchanged.
	st := DeriveAgentWaveState(s.snapshotLocked())
	if st.GateOpenFor(s.tasks[4].ID) {
		t.Fatal("wave 2 must stay shut behind an unconfirmed pilot")
	}
	s.setStatus(1, TaskSucceeded)
	s.setStatus(2, TaskSucceeded)
	s.setStatus(3, TaskSucceeded)
	ev, err = repo.EvaluateAgentRun(ctx, tenant, s.run.ID)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(ev.Dispatchable) != 6 {
		t.Fatalf("want wave 2's six sites once the pilot confirmed, got %d", len(ev.Dispatchable))
	}
}

// ---------------------------------------------------------------------------
// Planning / API boundary
// ---------------------------------------------------------------------------

func agentServiceFixture(t *testing.T, enabled bool, latest string, sites map[uuid.UUID]SiteInfo) (*Service, *countingEnqueuer, *fakeCreateRepo) {
	t.Helper()
	repo := &fakeCreateRepo{}
	enq := &countingEnqueuer{}
	svc := NewService(repo, &fakeSiteLookup{sites: sites}, enq, domain.NewValidator(), domain.SystemClock{})
	svc.SetAgentSelfUpdate(enabled, fixedReleases{version: latest})
	return svc, enq, repo
}

// TestAgentTargetRejectsVersionPin is the API-boundary refusal the design
// insists on: a pin cannot be honoured (the agent's manifest only ever points
// at the published build, and its downgrade guard refuses older ones), so
// accepting one and installing something else would lie to the operator.
func TestAgentTargetRejectsVersionPin(t *testing.T) {
	siteID := uuid.New()
	sites := map[uuid.UUID]SiteInfo{
		siteID: {ID: siteID, Enrolled: true, AgentVersion: "0.61.80"},
	}

	cases := []struct {
		name     string
		version  string
		wantCode string
	}{
		{"explicit pin", "0.62.0", "agent_version_pin_unsupported"},
		{"older pin", "0.61.70", "agent_version_pin_unsupported"},
		{"a pin that would otherwise pass the charset check", "1.2.3", "agent_version_pin_unsupported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, enq, _ := agentServiceFixture(t, true, "0.62.0", sites)
			_, tasks, err := svc.CreateRun(context.Background(), CreateRunInput{
				TenantID: uuid.New(),
				SiteIDs:  []uuid.UUID{siteID},
				Items:    []Item{{Type: TargetAgent, Version: tc.version}},
			})
			de, ok := domain.AsDomain(err)
			if !ok || de.Kind != domain.KindValidation {
				t.Fatalf("want a validation domain error, got %v", err)
			}
			if de.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", de.Code, tc.wantCode)
			}
			if len(tasks) != 0 || enq.n != 0 {
				t.Fatalf("a rejected pin must create nothing: %d tasks, %d jobs", len(tasks), enq.n)
			}
		})
	}

	// "latest" and the unset default describe what the channel actually does,
	// so both are accepted.
	for _, version := range []string{"", "latest"} {
		t.Run("accepted version "+version, func(t *testing.T) {
			svc, _, _ := agentServiceFixture(t, true, "0.62.0", sites)
			_, tasks, err := svc.CreateRun(context.Background(), CreateRunInput{
				TenantID: uuid.New(),
				SiteIDs:  []uuid.UUID{siteID},
				Items:    []Item{{Type: TargetAgent, Version: version}},
			})
			if err != nil {
				t.Fatalf("version %q must be accepted: %v", version, err)
			}
			if len(tasks) != 1 {
				t.Fatalf("want one task, got %d", len(tasks))
			}
			// Accepting "latest" from the operator and RECORDING "latest" on the
			// task are different things. What the run must persist is the
			// version "latest" resolved to at plan time: that is the run's
			// premise, and it is the only fixed thing an "up_to_date" answer can
			// later be checked against. Storing the word "latest" leaves nothing
			// to check but a live read of a mutable manifest.
			if tasks[0].DesiredVersion != "0.62.0" {
				t.Fatalf("desired_version = %q, want the resolved published version 0.62.0: the run's target must survive a manifest change",
					tasks[0].DesiredVersion)
			}
			if tasks[0].FromVersion != "0.61.80" {
				t.Fatalf("from_version = %q, want the version the site reported at plan time", tasks[0].FromVersion)
			}
			if tasks[0].TargetSlug != AgentTargetSlug {
				t.Fatalf("target_slug = %q, want %q", tasks[0].TargetSlug, AgentTargetSlug)
			}
		})
	}
}

// TestAgentRunRecordsItsPlannedTarget states the persistence rule on its own,
// because everything the confirmation check does rests on it: a run has a
// premise, "install version X", and X has to survive the manifest changing
// under it.
//
// The published version is deliberately read ONCE per run here (planAgentTasks
// resolves it, then every task carries it), so two sites planned in the same run
// can never end up scored against two different targets.
func TestAgentRunRecordsItsPlannedTarget(t *testing.T) {
	siteA, siteB := uuid.New(), uuid.New()
	sites := map[uuid.UUID]SiteInfo{
		siteA: {ID: siteA, Enrolled: true, AgentVersion: "0.61.80"},
		siteB: {ID: siteB, Enrolled: true, AgentVersion: "0.60.1"},
	}
	svc, _, _ := agentServiceFixture(t, true, "0.62.0", sites)

	_, tasks, err := svc.CreateRun(context.Background(), CreateRunInput{
		TenantID: uuid.New(),
		SiteIDs:  []uuid.UUID{siteA, siteB},
		Items:    []Item{{Type: TargetAgent}},
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("want two tasks, got %d", len(tasks))
	}
	for _, task := range tasks {
		if task.DesiredVersion != "0.62.0" {
			t.Fatalf("site %s: desired_version = %q, want the one resolved target 0.62.0", task.SiteID, task.DesiredVersion)
		}
		// And the plan-time from_version stays per-site: it is what makes "this
		// site moved" checkable, so it must be this site's own version.
		if want := sites[task.SiteID].AgentVersion; task.FromVersion != want {
			t.Fatalf("site %s: from_version = %q, want its own reported %q", task.SiteID, task.FromVersion, want)
		}
	}
}

// TestAgentTargetBoundaryRefusals covers the remaining refusals at the API
// boundary, each of which exists because the alternative would be a silent
// half-truth.
func TestAgentTargetBoundaryRefusals(t *testing.T) {
	siteID := uuid.New()
	sites := map[uuid.UUID]SiteInfo{
		siteID: {ID: siteID, Enrolled: true, AgentVersion: "0.61.80"},
	}

	cases := []struct {
		name     string
		enabled  bool
		latest   string
		in       CreateRunInput
		wantCode string
	}{
		{
			name: "the kill switch refuses the run outright", enabled: false, latest: "0.62.0",
			in:       CreateRunInput{SiteIDs: []uuid.UUID{siteID}, Items: []Item{{Type: TargetAgent}}},
			wantCode: "agent_self_update_disabled",
		},
		{
			name: "an unknown published version cannot call anyone behind", enabled: true, latest: "",
			in:       CreateRunInput{SiteIDs: []uuid.UUID{siteID}, Items: []Item{{Type: TargetAgent}}},
			wantCode: "agent_release_unknown",
		},
		{
			name: "the agent target cannot share a run", enabled: true, latest: "0.62.0",
			in: CreateRunInput{SiteIDs: []uuid.UUID{siteID}, Items: []Item{
				{Type: TargetAgent},
				{Type: TargetPlugin, Slug: "akismet/akismet.php"},
			}},
			wantCode: "agent_target_exclusive",
		},
		{
			name: "there is no dry run", enabled: true, latest: "0.62.0",
			in:       CreateRunInput{SiteIDs: []uuid.UUID{siteID}, Items: []Item{{Type: TargetAgent}}, DryRun: true},
			wantCode: "agent_dry_run_unsupported",
		},
		{
			name: "a slug is not accepted for this target", enabled: true, latest: "0.62.0",
			in:       CreateRunInput{SiteIDs: []uuid.UUID{siteID}, Items: []Item{{Type: TargetAgent, Slug: "some-other-plugin"}}},
			wantCode: "agent_slug_unsupported",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, enq, _ := agentServiceFixture(t, tc.enabled, tc.latest, sites)
			in := tc.in
			in.TenantID = uuid.New()
			_, tasks, err := svc.CreateRun(context.Background(), in)
			de, ok := domain.AsDomain(err)
			if !ok {
				t.Fatalf("want a domain error, got %v", err)
			}
			if de.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q (%s)", de.Code, tc.wantCode, de.Message)
			}
			if len(tasks) != 0 || enq.n != 0 {
				t.Fatalf("a refused run must create nothing: %d tasks, %d jobs", len(tasks), enq.n)
			}
		})
	}
}

// TestAgentPlanUsesReportedVersionNotInventory pins the planning authority.
// Only a site whose REPORTED agent version is behind the PUBLISHED one gets a
// task; a site already current, one running the build that has no self-updater
// at all, and one whose version cannot be read are all left alone. In
// particular the site's plugin inventory (which carries an agent self-update
// advisory the control plane suppresses everywhere else) is never the input.
func TestAgentPlanUsesReportedVersionNotInventory(t *testing.T) {
	outdated := uuid.New()
	current := uuid.New()
	directory := uuid.New()
	unknown := uuid.New()
	staleInventory := uuid.New()

	sites := map[uuid.UUID]SiteInfo{
		outdated: {ID: outdated, Enrolled: true, AgentVersion: "0.61.80"},
		current:  {ID: current, Enrolled: true, AgentVersion: "0.62.0"},
		// The plugin-directory build ships without a self-updater and is
		// upgraded by the directory, so it can never consume this channel.
		directory: {ID: directory, Enrolled: true, AgentVersion: "0.61.80", Components: []Component{
			{Type: TargetPlugin, Slug: "fleet-agent-site-manager/fleet-agent-site-manager.php", Name: "Fleet Agent Site Manager"},
		}},
		// Never reported a version: never guess.
		unknown: {ID: unknown, Enrolled: true, AgentVersion: ""},
		// Reported version is CURRENT, but its inventory still carries a stale
		// self-update advisory. The reported version must win.
		staleInventory: {ID: staleInventory, Enrolled: true, AgentVersion: "0.62.0", Components: []Component{
			{Type: TargetPlugin, Slug: "wpmgr-agent/wpmgr-agent.php", Name: "WPMgr Agent",
				Version: "0.61.80", UpdateAvailable: true, NewVersion: "0.62.0"},
		}},
	}

	svc, enq, _ := agentServiceFixture(t, true, "0.62.0", sites)
	_, tasks, err := svc.CreateRun(context.Background(), CreateRunInput{
		TenantID: uuid.New(),
		SiteIDs:  []uuid.UUID{outdated, current, directory, unknown, staleInventory},
		Items:    []Item{{Type: TargetAgent}},
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("only the genuinely outdated site may be planned, got %d tasks", len(tasks))
	}
	if tasks[0].SiteID != outdated {
		t.Fatalf("planned the wrong site: %s", tasks[0].SiteID)
	}
	if tasks[0].FromVersion != "0.61.80" {
		t.Fatalf("from_version = %q, want the site's own reported version", tasks[0].FromVersion)
	}
	if tasks[0].TargetType != TargetAgent {
		t.Fatalf("target_type = %q, want %q", tasks[0].TargetType, TargetAgent)
	}
	if enq.n != 1 {
		t.Fatalf("one planned task, one enqueued job; got %d", enq.n)
	}
}

// TestAgentRunEnqueuesOnlyTheFirstWave: a rollout that must not continue never
// has a job sitting ready to run. Only the canary is enqueued at creation; the
// rest of the fleet is enqueued wave by wave as each one confirms.
func TestAgentRunEnqueuesOnlyTheFirstWave(t *testing.T) {
	sites := map[uuid.UUID]SiteInfo{}
	ids := make([]uuid.UUID, 0, 40)
	for i := 0; i < 40; i++ {
		id := uuid.New()
		ids = append(ids, id)
		sites[id] = SiteInfo{ID: id, Enrolled: true, AgentVersion: "0.61.80"}
	}

	svc, enq, _ := agentServiceFixture(t, true, "0.62.0", sites)
	_, tasks, err := svc.CreateRun(context.Background(), CreateRunInput{
		TenantID: uuid.New(),
		SiteIDs:  ids,
		Items:    []Item{{Type: TargetAgent}},
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if len(tasks) != 40 {
		t.Fatalf("want 40 planned tasks, got %d", len(tasks))
	}
	if enq.n != 1 {
		t.Fatalf("only the canary may be enqueued at creation, got %d job(s)", enq.n)
	}

	// The control: an ORDINARY run still enqueues everything at once, so this
	// staging is specific to the agent channel and did not change bulk updates.
	pluginSites := map[uuid.UUID]SiteInfo{}
	pluginIDs := make([]uuid.UUID, 0, 5)
	for i := 0; i < 5; i++ {
		id := uuid.New()
		pluginIDs = append(pluginIDs, id)
		pluginSites[id] = SiteInfo{ID: id, Enrolled: true, Components: []Component{
			{Type: TargetPlugin, Slug: "akismet/akismet.php", Name: "Akismet Anti-spam",
				Version: "5.3.1", UpdateAvailable: true, NewVersion: "5.3.2"},
		}}
	}
	svc2, enq2, _ := agentServiceFixture(t, true, "0.62.0", pluginSites)
	if _, _, err := svc2.CreateRun(context.Background(), CreateRunInput{
		TenantID: uuid.New(),
		SiteIDs:  pluginIDs,
		Items:    []Item{{Type: TargetPlugin, Slug: "akismet/akismet.php"}},
	}); err != nil {
		t.Fatalf("CreateRun (plugin control): %v", err)
	}
	if enq2.n != 5 {
		t.Fatalf("an ordinary run must still enqueue every task, got %d", enq2.n)
	}
}

// TestReaperSparesAnAgentTaskWithinItsOwnThreshold: an agent task is
// legitimately slow, a later wave waits for every earlier wave, and a site on
// external cron waits up to 90 minutes for beat 3 alone. The 45-minute
// stale-task threshold would reap those as failures and, worse, feed a false
// failure into the wave gate and halt a healthy rollout.
func TestReaperSparesAnAgentTaskWithinItsOwnThreshold(t *testing.T) {
	tenant := uuid.New()
	store := newAgentStore(tenant, 3)
	repo := &reaperFakeRepo{fakeAgentRepo: fakeAgentRepo{s: store}}

	// Both rows are well past the ordinary threshold, so the SQL sweep returns
	// them; only the agent one is spared here.
	agentTask := store.Task(0)
	agentTask.UpdatedAt = time.Now().Add(-2 * time.Hour)
	pluginTask := store.Task(1)
	pluginTask.TargetType = TargetPlugin
	pluginTask.TargetSlug = "akismet/akismet.php"
	pluginTask.UpdatedAt = time.Now().Add(-2 * time.Hour)
	repo.stale = []Task{agentTask, pluginTask}

	w := NewWorker(repo, nil, nil, nil, nil, nil, nil, 5, 0)
	if err := NewReaperWorker(w).Work(context.Background(), &river.Job[ReapStaleTasksArgs]{}); err != nil {
		t.Fatalf("reaper Work: %v", err)
	}

	if len(repo.finished) != 1 {
		t.Fatalf("only the ordinary task may be reaped, got %d terminal transitions", len(repo.finished))
	}
	if repo.finished[0].TaskID != pluginTask.ID {
		t.Fatalf("the reaper terminalized the agent task, which is legitimately slow")
	}

	// Past its OWN threshold, an agent task is still reaped: it must not hold
	// its in-flight slot forever.
	repo.finished = nil
	agentTask.UpdatedAt = time.Now().Add(-agentStaleTaskThreshold - time.Minute)
	repo.stale = []Task{agentTask}
	if err := NewReaperWorker(w).Work(context.Background(), &river.Job[ReapStaleTasksArgs]{}); err != nil {
		t.Fatalf("reaper Work: %v", err)
	}
	if len(repo.finished) != 1 {
		t.Fatalf("a genuinely stuck agent task must still be reaped, got %d", len(repo.finished))
	}
}

// reaperFakeRepo serves a fixed stale-task list to the reaper.
type reaperFakeRepo struct {
	fakeAgentRepo
	stale []Task
}

func (f *reaperFakeRepo) ListStaleUpdateTasks(context.Context, time.Duration, int32) ([]Task, error) {
	return f.stale, nil
}
