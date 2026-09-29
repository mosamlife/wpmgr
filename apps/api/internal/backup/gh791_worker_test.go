package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// errCommander answers every command with the error it holds.
type errCommander struct {
	err   error
	calls int
}

func (c *errCommander) Backup(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.BackupRequest) (agentcmd.BackupResponse, error) {
	c.calls++
	return agentcmd.BackupResponse{}, c.err
}

func (c *errCommander) IncrementalBackup(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.IncrementalBackupRequest) (agentcmd.BackupResponse, error) {
	c.calls++
	return agentcmd.BackupResponse{}, c.err
}

func (c *errCommander) Restore(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.RestoreRequest) (agentcmd.RestoreResponse, error) {
	c.calls++
	return agentcmd.RestoreResponse{}, c.err
}

// gh791AgentProse is free-form text the agent put in its failure message. It
// may appear, sanitised, in the dashboard, and never in the email.
const gh791AgentProse = "disk quota exceeded while writing chunk"

// agentFailedErr is a genuine agent-side command failure (AgentFailed()==true)
// as the client returns it, wrapped the way a caller sees it.
func agentFailedErr(command string) error {
	return fmt.Errorf("wrapped: %w", &agentcmd.CommandError{
		Command:     command,
		Status:      500,
		Code:        "wpmgr_command_failed",
		Message:     "Command execution failed: RuntimeException: " + gh791AgentProse + ", call www.evil-example.com/help",
		DataCommand: command,
		Exception:   "RuntimeException",
		At:          "includes/commands/class-backup-command.php:239",
		DataStatus:  500,
	})
}

// recordingMailer records every Enqueue call with its data.
type recordingMailer struct {
	mu    sync.Mutex
	calls []map[string]any
	tmpls []string
}

func (m *recordingMailer) Enqueue(_ context.Context, _ uuid.UUID, _ []string, template string, data map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, data)
	m.tmpls = append(m.tmpls, template)
	return nil
}

// notifyingWorkerRepo is a worker repo whose site always notifies on every
// outcome, so sendBackupEmail reaches the mailer.
type notifyingWorkerRepo struct{ *fakeWorkerRepo }

func (r *notifyingWorkerRepo) GetBackupSettings(_ context.Context, tenantID, siteID uuid.UUID) (SiteBackupSettings, error) {
	return SiteBackupSettings{
		TenantID:           tenantID,
		SiteID:             siteID,
		NotifyOnCompletion: "always",
		NotifyRecipients:   []string{"ops@example.com"},
	}, nil
}

// countingRunStore counts the run-side attempt-error clears.
type countingRunStore struct {
	*fakeScheduleRunStore
	clears int
}

func (s *countingRunStore) ClearScheduleRunAttemptErrorBySnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID) (int64, error) {
	s.clears++
	return s.fakeScheduleRunStore.ClearScheduleRunAttemptErrorBySnapshot(ctx, tenantID, snapshotID)
}

// drain returns every event already buffered on ch.
func drain(ch <-chan BackupEvent) []BackupEvent {
	var out []BackupEvent
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func phasesOf(evs []BackupEvent) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Phase)
	}
	return out
}

// runningBackupFixture seeds one pending snapshot and a running schedule run
// linked to it, and returns a service wired with a hub and the run store.
func runningBackupFixture(t *testing.T) (*notifyingWorkerRepo, *countingRunStore, *Service, *Hub, uuid.UUID, uuid.UUID) {
	t.Helper()
	repo := &notifyingWorkerRepo{&fakeWorkerRepo{fakeRepo: newFakeRepo(), workerManifests: map[uuid.UUID][]ManifestEntry{}}}
	tenantID := uuid.New()
	snapshotID := uuid.New()
	siteID := uuid.New()
	repo.setSnapshot(Snapshot{
		ID:           snapshotID,
		TenantID:     tenantID,
		SiteID:       siteID,
		Kind:         KindFull,
		Status:       StatusPending,
		AgeRecipient: "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p",
	})
	sid := snapshotID
	runs := &countingRunStore{fakeScheduleRunStore: &fakeScheduleRunStore{rows: []ScheduleRun{{
		ID:         uuid.New(),
		TenantID:   tenantID,
		SiteID:     siteID,
		SnapshotID: &sid,
		Status:     ScheduleRunStatusRunning,
		Kind:       "full",
	}}}}
	hub := NewHub()
	svc := &Service{repo: repo, sites: fakeWorkerSiteLookup{}, clock: fakeClock{t: time.Now()}, scheduleRuns: runs}
	svc.SetHub(hub)
	return repo, runs, svc, hub, tenantID, snapshotID
}

// ---------------------------------------------------------------------------
// Backup worker
// ---------------------------------------------------------------------------

// TestBackupWorker_AgentFailedKeepsAgentProseOutOfEmail: an agent-reported
// failure fails the backup on the first attempt. The stored reason (dashboard,
// schedule run) carries the sanitised agent message; the failure email carries
// only the control-plane wording, the exception class and the location.
func TestBackupWorker_AgentFailedKeepsAgentProseOutOfEmail(t *testing.T) {
	repo, _, svc, _, tenantID, snapshotID := runningBackupFixture(t)
	mailer := &recordingMailer{}
	svc.SetMailer(mailer)
	cmd := &errCommander{err: agentFailedErr("backup")}
	worker := NewBackupWorker(svc, cmd, nil, nil, "https://cp.example.com", 0)

	if err := worker.Work(context.Background(), &river.Job[BackupArgs]{
		Args: BackupArgs{TenantID: tenantID, SnapshotID: snapshotID},
	}); err != nil {
		t.Fatalf("Work() = %v; an agent-reported failure is terminal, not a retry", err)
	}
	if cmd.calls != 1 {
		t.Errorf("backup command sent %d times, want 1", cmd.calls)
	}
	got := repo.snapshots[snapshotID]
	if got.Status != StatusFailed {
		t.Fatalf("snapshot status = %q, want failed", got.Status)
	}
	for _, want := range []string{"Backup failed:", "RuntimeException at includes/commands/class-backup-command.php:239", gh791AgentProse, "[link]"} {
		if !strings.Contains(got.Error, want) {
			t.Errorf("stored reason %q does not contain %q", got.Error, want)
		}
	}
	if strings.Contains(got.Error, "evil-example") {
		t.Errorf("stored reason %q kept the hostname", got.Error)
	}
	if len(mailer.calls) != 1 || mailer.tmpls[0] != "backup_failed" {
		t.Fatalf("mailer calls = %v, want exactly one backup_failed", mailer.tmpls)
	}
	mailErr, _ := mailer.calls[0]["Error"].(string)
	for _, want := range []string{"Backup failed:", "Agent error: RuntimeException at includes/commands/class-backup-command.php:239.", "dashboard"} {
		if !strings.Contains(mailErr, want) {
			t.Errorf("email reason %q does not contain %q", mailErr, want)
		}
	}
	for _, leak := range []string{gh791AgentProse, "evil-example", "[link]", "www."} {
		if strings.Contains(mailErr, leak) {
			t.Errorf("email reason %q carries agent prose %q", mailErr, leak)
		}
	}
}

// TestBackupWorker_RetryRecordsAttemptErrorThenSuccessClearsIt: a retryable
// failure leaves the backup running with the control plane's reason on the
// snapshot and the schedule run, publishes 'retrying', and returns the error
// so River retries. The next attempt that reaches the agent clears both and
// publishes 'resumed'.
func TestBackupWorker_RetryRecordsAttemptErrorThenSuccessClearsIt(t *testing.T) {
	repo, runs, svc, hub, tenantID, snapshotID := runningBackupFixture(t)
	ch, unsub := hub.Subscribe(snapshotID)
	defer unsub()

	bad := &errCommander{err: fmt.Errorf("wrapped: %w", &agentcmd.CommandError{Command: "backup", Status: 502})}
	const want502 = "The site returned a server error (HTTP 502) that did not come from the WPMgr agent."
	err := NewBackupWorker(svc, bad, nil, nil, "https://cp.example.com", 0).Work(context.Background(), &river.Job[BackupArgs]{
		JobRow: newJobRow(1, 25),
		Args:   BackupArgs{TenantID: tenantID, SnapshotID: snapshotID},
	})
	if err == nil {
		t.Fatal("Work() = nil; a 502 must be retried")
	}
	got := repo.snapshots[snapshotID]
	if got.Status != StatusRunning || got.AttemptError != want502 {
		t.Fatalf("snapshot = (%q, %q), want (running, %q)", got.Status, got.AttemptError, want502)
	}
	if got.Error != "" {
		t.Errorf("snapshot error = %q; a retrying backup has no final reason", got.Error)
	}
	if runs.rows[0].AttemptError != want502 {
		t.Errorf("schedule run attempt_error = %q, want %q", runs.rows[0].AttemptError, want502)
	}
	var retrying *BackupEvent
	for _, ev := range drain(ch) {
		if ev.Phase == "retrying" {
			ev := ev
			retrying = &ev
		}
		if ev.Phase == "failed" {
			t.Errorf("a retryable attempt published a 'failed' frame: %+v", ev)
		}
	}
	if retrying == nil {
		t.Fatal("no 'retrying' frame published")
	}
	if e, _ := retrying.PhaseDetail["error"].(string); e != want502 {
		t.Errorf("retrying frame error = %q, want %q", e, want502)
	}

	ok := &fakeCommander{ok: true}
	if err := NewBackupWorker(svc, ok, nil, nil, "https://cp.example.com", 0).Work(context.Background(), &river.Job[BackupArgs]{
		JobRow: newJobRow(2, 25),
		Args:   BackupArgs{TenantID: tenantID, SnapshotID: snapshotID},
	}); err != nil {
		t.Fatalf("second attempt Work() = %v", err)
	}
	if a := repo.snapshots[snapshotID].AttemptError; a != "" {
		t.Errorf("snapshot attempt_error = %q after a successful attempt, want empty", a)
	}
	if a := runs.rows[0].AttemptError; a != "" {
		t.Errorf("schedule run attempt_error = %q after a successful attempt, want empty", a)
	}
	if phases := phasesOf(drain(ch)); !contains(phases, "resumed") {
		t.Errorf("phases after the successful attempt = %v, want a 'resumed'", phases)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// TestClearSnapshotStalledIfRunning_TouchesRunOnlyWhenSnapshotHadSomething:
// proof of life on a healthy running backup costs one guarded snapshot
// UPDATE; the schedule run is only touched when the snapshot had an attempt
// error or a stall to clear.
func TestClearSnapshotStalledIfRunning_TouchesRunOnlyWhenSnapshotHadSomething(t *testing.T) {
	repo, runs, svc, _, tenantID, snapshotID := runningBackupFixture(t)
	s := repo.snapshots[snapshotID]
	s.Status = StatusRunning
	repo.snapshots[snapshotID] = s
	ctx := context.Background()

	cleared, err := svc.ClearSnapshotStalledIfRunning(ctx, tenantID, snapshotID)
	if err != nil || cleared {
		t.Fatalf("clear on a healthy row = (%v, %v), want (false, nil)", cleared, err)
	}
	if runs.clears != 0 {
		t.Fatalf("run-side clear ran %d times on a healthy row, want 0", runs.clears)
	}

	if err := svc.RecordAttemptError(ctx, tenantID, snapshotID, "Could not connect to the site."); err != nil {
		t.Fatalf("RecordAttemptError: %v", err)
	}
	if runs.rows[0].AttemptError == "" || repo.snapshots[snapshotID].AttemptError == "" {
		t.Fatal("RecordAttemptError did not record on both rows")
	}
	cleared, err = svc.ClearSnapshotStalledIfRunning(ctx, tenantID, snapshotID)
	if err != nil || !cleared {
		t.Fatalf("clear after an attempt error = (%v, %v), want (true, nil)", cleared, err)
	}
	if runs.clears != 1 || runs.rows[0].AttemptError != "" {
		t.Fatalf("run clears = %d, run attempt_error = %q; want 1 and empty", runs.clears, runs.rows[0].AttemptError)
	}
}

// TestRecordAttemptError_TerminalSnapshotPublishesNothing: a snapshot that is
// no longer running is left alone and nothing is published for it.
func TestRecordAttemptError_TerminalSnapshotPublishesNothing(t *testing.T) {
	repo, _, svc, hub, tenantID, snapshotID := runningBackupFixture(t)
	s := repo.snapshots[snapshotID]
	s.Status = StatusFailed
	s.Error = "final reason"
	repo.snapshots[snapshotID] = s
	ch, unsub := hub.Subscribe(snapshotID)
	defer unsub()

	if err := svc.RecordAttemptError(context.Background(), tenantID, snapshotID, "Could not connect to the site."); err != nil {
		t.Fatalf("RecordAttemptError: %v", err)
	}
	if got := repo.snapshots[snapshotID]; got.AttemptError != "" || got.Error != "final reason" {
		t.Errorf("failed snapshot changed: attempt_error=%q error=%q", got.AttemptError, got.Error)
	}
	if evs := drain(ch); len(evs) != 0 {
		t.Errorf("published %v for a failed snapshot, want nothing", phasesOf(evs))
	}
}

// statusRecordingRunStore records every run status write the service makes.
type statusRecordingRunStore struct {
	*fakeScheduleRunStore
	statusInputs []SetScheduleRunStatusInput
}

func (s *statusRecordingRunStore) SetScheduleRunStatusBySnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID, in SetScheduleRunStatusInput) (ScheduleRun, error) {
	s.statusInputs = append(s.statusInputs, in)
	return s.fakeScheduleRunStore.SetScheduleRunStatusBySnapshot(ctx, tenantID, snapshotID, in)
}

// TestProgressWatchdog_HardFailSendsLastError: when the watchdog hard-fails a
// backup that holds an attempt error, the SSE 'failed' frame and the schedule
// run both carry the combined reason with the last error, not the bare stall
// message.
func TestProgressWatchdog_HardFailSendsLastError(t *testing.T) {
	repo := newWatchdogFakeRepo()
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	const lastErr = "The site did not answer in time."
	repo.setSnapshot(Snapshot{ID: snapID, TenantID: tenantID, SiteID: siteID, Status: StatusRunning, AttemptError: lastErr})
	repo.stalledFeed = []StalledSnapshot{{ID: snapID, TenantID: tenantID, SiteID: siteID, Hard: true}}

	hub := NewHub()
	svc := newWatchdogTestService(repo, hub)
	sid := snapID
	runs := &statusRecordingRunStore{fakeScheduleRunStore: &fakeScheduleRunStore{rows: []ScheduleRun{{
		ID: uuid.New(), TenantID: tenantID, SiteID: siteID, SnapshotID: &sid, Status: ScheduleRunStatusRunning, Kind: "full",
	}}}}
	svc.scheduleRuns = runs
	ch, unsub := hub.Subscribe(snapID)
	defer unsub()

	if err := NewProgressWatchdogWorker(svc, time.Minute, time.Hour, nil).Work(context.Background(), nil); err != nil {
		t.Fatalf("Work: %v", err)
	}

	want := stallTimeoutMsg + ". Last error: " + lastErr
	if got := repo.mustGet(t, snapID); got.Status != StatusFailed || got.Error != want {
		t.Fatalf("snapshot = (%q, %q), want (failed, %q)", got.Status, got.Error, want)
	}
	evs := drain(ch)
	if len(evs) != 1 || evs[0].Phase != "failed" {
		t.Fatalf("events = %v, want exactly one 'failed'", phasesOf(evs))
	}
	if e, _ := evs[0].PhaseDetail["error"].(string); e != want {
		t.Errorf("SSE 'failed' error = %q, want %q", e, want)
	}
	if len(runs.statusInputs) != 1 || runs.statusInputs[0].Status != ScheduleRunStatusFailed || runs.statusInputs[0].Error == nil {
		t.Fatalf("schedule run status writes = %+v, want one 'failed' with an error", runs.statusInputs)
	}
	if e := *runs.statusInputs[0].Error; e != want {
		t.Errorf("schedule run error = %q, want %q", e, want)
	}
}

// ---------------------------------------------------------------------------
// Restore worker
// ---------------------------------------------------------------------------

func newJobRow(attempt, max int) *rivertype.JobRow {
	return &rivertype.JobRow{Attempt: attempt, MaxAttempts: max}
}

func restoreJob(tenantID, snapshotID uuid.UUID, attempt, max int) *river.Job[RestoreArgs] {
	job := &river.Job[RestoreArgs]{Args: RestoreArgs{TenantID: tenantID, SnapshotID: snapshotID, Full: true}}
	job.JobRow = newJobRow(attempt, max)
	return job
}

// restoreJobForRun is restoreJob with the restore run's id threaded in, as
// CreateRestore enqueues it.
func restoreJobForRun(tenantID, snapshotID, runID uuid.UUID, attempt, max int) *river.Job[RestoreArgs] {
	job := restoreJob(tenantID, snapshotID, attempt, max)
	job.Args.RestoreRunID = runID
	return job
}

// TestRestoreArgs_InsertOptsLimitsToOneAttempt: every backup_restore job is
// inserted with MaxAttempts 1. River reads this through
// JobArgsWithInsertOpts, and an insert-time MaxAttempts would override it, so
// EnqueueRestore must pass none.
func TestRestoreArgs_InsertOptsLimitsToOneAttempt(t *testing.T) {
	withOpts, ok := any(RestoreArgs{}).(river.JobArgsWithInsertOpts)
	if !ok {
		t.Fatal("RestoreArgs does not implement river.JobArgsWithInsertOpts; its attempt limit is never applied")
	}
	if got := withOpts.InsertOpts().MaxAttempts; got != 1 {
		t.Fatalf("RestoreArgs.InsertOpts().MaxAttempts = %d, want 1", got)
	}
	// The backup kind keeps River's default retries.
	if got := backupInsertOpts().MaxAttempts; got != 0 {
		t.Errorf("backupInsertOpts().MaxAttempts = %d, want 0 (River's default)", got)
	}
}

// restoreTransportErr is a dispatch error that never reached the agent.
func restoreTransportErr() error {
	return errors.New("restore command transport: dial tcp 203.0.113.9:443: connect: connection refused")
}

// assertJobCancel fails unless err is a river.JobCancel error.
func assertJobCancel(t *testing.T, err error) {
	t.Helper()
	var cancelErr *river.JobCancelError
	if !errors.As(err, &cancelErr) {
		t.Fatalf("Work() returned %T (%v), want *river.JobCancelError", err, err)
	}
}

// TestRestoreWorker_TransportErrorFailsOnFirstAttempt: a dispatch error ends
// the restore on the attempt that hit it, whatever attempt limit the job row
// carries. The run is finalised as failed with the control plane's
// description, a 'failed' frame is published, no 'retrying' frame exists, and
// the job is cancelled.
func TestRestoreWorker_TransportErrorFailsOnFirstAttempt(t *testing.T) {
	for _, max := range []int{1, 25} {
		t.Run(fmt.Sprintf("max_attempts_%d", max), func(t *testing.T) {
			repo, runStore, tenantID, snapshotID, svc := newRestoreWorkerFixture(t)
			hub := NewHub()
			svc.SetHub(hub)
			ch, unsub := hub.Subscribe(snapshotID)
			defer unsub()
			cmd := &errCommander{err: restoreTransportErr()}

			err := NewRestoreWorker(svc, cmd, nil, nil, "https://cp.example.com", 0).Work(context.Background(), restoreJob(tenantID, snapshotID, 1, max))
			assertJobCancel(t, err)
			if cmd.calls != 1 {
				t.Errorf("restore command sent %d times, want 1", cmd.calls)
			}
			if len(runStore.statusCalls) != 1 || runStore.statusCalls[0].Status != RestoreStatusFailed {
				t.Fatalf("restore run status calls = %+v, want one 'failed'", runStore.statusCalls)
			}
			if e := runStore.statusCalls[0].Error; e != "Could not connect to the site." {
				t.Errorf("restore run error = %q, want the control-plane text", e)
			}
			if repo.failCalled {
				t.Error("FailSnapshot called on the restored-from snapshot")
			}
			phases := phasesOf(drain(ch))
			if contains(phases, "retrying") || !contains(phases, "failed") {
				t.Errorf("phases = %v, want 'failed' and no 'retrying'", phases)
			}
		})
	}
}

// TestRestoreWorker_AgentFailedIsTerminal: an agent-reported failure ends the
// restore on the first attempt with OperatorMessage("Restore"), and no
// 'retrying' frame.
func TestRestoreWorker_AgentFailedIsTerminal(t *testing.T) {
	_, runStore, tenantID, snapshotID, svc := newRestoreWorkerFixture(t)
	hub := NewHub()
	svc.SetHub(hub)
	ch, unsub := hub.Subscribe(snapshotID)
	defer unsub()
	agentErr := agentFailedErr("restore")
	cmd := &errCommander{err: agentErr}

	if err := NewRestoreWorker(svc, cmd, nil, nil, "https://cp.example.com", 0).Work(context.Background(), restoreJob(tenantID, snapshotID, 1, 1)); err != nil {
		t.Fatalf("Work() = %v; an agent-reported failure is terminal", err)
	}
	if cmd.calls != 1 {
		t.Errorf("restore command sent %d times, want 1", cmd.calls)
	}
	if len(runStore.statusCalls) != 1 || runStore.statusCalls[0].Status != RestoreStatusFailed {
		t.Fatalf("restore run status calls = %+v, want one 'failed'", runStore.statusCalls)
	}
	ce, _ := agentcmd.AsCommandError(agentErr)
	e := runStore.statusCalls[0].Error
	if want := ce.OperatorMessage("Restore"); e != want {
		t.Errorf("restore run error = %q, want OperatorMessage(\"Restore\") %q", e, want)
	}
	if strings.Contains(e, "body=") || strings.Contains(e, "evil-example") {
		t.Errorf("restore run error = %q, want the sanitised operator message", e)
	}
	phases := phasesOf(drain(ch))
	if contains(phases, "retrying") || !contains(phases, "failed") {
		t.Errorf("phases = %v, want 'failed' and no 'retrying'", phases)
	}
}

// TestRestoreWorker_LaterAttemptCancelsWithoutDispatch: a restore job that
// reaches a second attempt (one inserted with a higher limit) sends nothing to
// the site, marks its queued or running run failed with the control plane's
// wording, and is cancelled. The repo's refusal to change a finished run is
// proved against Postgres in TestGH791_RestoreLaterAttemptFailsRun.
func TestRestoreWorker_LaterAttemptCancelsWithoutDispatch(t *testing.T) {
	const want = "The restore was interrupted before it finished and was not retried. Start it again from the backup."
	for _, status := range []string{RestoreStatusQueued, RestoreStatusRunning} {
		t.Run(status, func(t *testing.T) {
			_, runStore, tenantID, snapshotID, svc := newRestoreWorkerFixture(t)
			runStore.active.Status = status
			runID := runStore.active.ID
			hub := NewHub()
			svc.SetHub(hub)
			ch, unsub := hub.Subscribe(snapshotID)
			defer unsub()
			cmd := &errCommander{err: restoreTransportErr()}

			err := NewRestoreWorker(svc, cmd, nil, nil, "https://cp.example.com", 0).Work(context.Background(), restoreJobForRun(tenantID, snapshotID, runID, 2, 25))
			assertJobCancel(t, err)
			if cmd.calls != 0 {
				t.Errorf("restore command sent %d times on a second attempt, want 0", cmd.calls)
			}
			if len(runStore.statusCalls) != 1 {
				t.Fatalf("restore run status calls = %+v, want exactly one", runStore.statusCalls)
			}
			got := runStore.statusCalls[0]
			if got.RunID != runID || got.TenantID != tenantID {
				t.Errorf("status call targets run %s tenant %s, want run %s tenant %s", got.RunID, got.TenantID, runID, tenantID)
			}
			if got.Status != RestoreStatusFailed || !got.SetFinished || got.SetStarted {
				t.Errorf("status call = %+v, want failed with finished_at set and started_at untouched", got)
			}
			if got.Error != want {
				t.Errorf("restore run error = %q, want %q", got.Error, want)
			}
			if len(runStore.eventCalls) != 0 {
				t.Errorf("run events appended on a cancelled attempt: %+v", runStore.eventCalls)
			}
			if evs := drain(ch); len(evs) != 0 {
				t.Errorf("published %v on a cancelled attempt, want nothing", phasesOf(evs))
			}
		})
	}
}

// TestRestoreWorker_LaterAttemptWithoutRunID: a later attempt with no run id
// has no run to finish; it is still cancelled with nothing sent.
func TestRestoreWorker_LaterAttemptWithoutRunID(t *testing.T) {
	_, runStore, tenantID, snapshotID, svc := newRestoreWorkerFixture(t)
	cmd := &errCommander{err: restoreTransportErr()}

	err := NewRestoreWorker(svc, cmd, nil, nil, "https://cp.example.com", 0).Work(context.Background(), restoreJob(tenantID, snapshotID, 2, 25))
	assertJobCancel(t, err)
	if cmd.calls != 0 {
		t.Errorf("restore command sent %d times on a second attempt, want 0", cmd.calls)
	}
	if len(runStore.statusCalls) != 0 || len(runStore.eventCalls) != 0 {
		t.Errorf("run touched without a run id: status %+v, events %+v", runStore.statusCalls, runStore.eventCalls)
	}
}

// TestRestoreWorker_PlanErrorCancels: a restore that cannot be planned is
// cancelled, not returned for River to retry, and nothing is sent.
func TestRestoreWorker_PlanErrorCancels(t *testing.T) {
	_, _, tenantID, _, svc := newRestoreWorkerFixture(t)
	cmd := &errCommander{}

	err := NewRestoreWorker(svc, cmd, nil, nil, "https://cp.example.com", 0).Work(context.Background(), restoreJob(tenantID, uuid.New(), 1, 1))
	assertJobCancel(t, err)
	if cmd.calls != 0 {
		t.Errorf("restore command sent %d times for an unplannable restore, want 0", cmd.calls)
	}
}

// TestRestoreWorker_PlanErrorFailsRun: a planning error finishes the run as
// failed with the control plane's wording. The run never records the raw Go
// error, and never the connection copy, because nothing reached the site.
func TestRestoreWorker_PlanErrorFailsRun(t *testing.T) {
	_, runStore, tenantID, _, svc := newRestoreWorkerFixture(t)
	runID := runStore.active.ID
	missingSnapshot := uuid.New()
	cmd := &errCommander{}

	err := NewRestoreWorker(svc, cmd, nil, nil, "https://cp.example.com", 0).Work(context.Background(), restoreJobForRun(tenantID, missingSnapshot, runID, 1, 1))
	assertJobCancel(t, err)
	if cmd.calls != 0 {
		t.Errorf("restore command sent %d times for an unplannable restore, want 0", cmd.calls)
	}
	// The worker marks the run running before it plans, then finishes it.
	if n := len(runStore.statusCalls); n != 2 {
		t.Fatalf("restore run status calls = %+v, want running then failed", runStore.statusCalls)
	}
	if first := runStore.statusCalls[0]; first.Status != RestoreStatusRunning || !first.SetStarted {
		t.Errorf("first status call = %+v, want running with started_at set", first)
	}
	last := runStore.statusCalls[1]
	if last.RunID != runID || last.TenantID != tenantID {
		t.Errorf("final status call targets run %s tenant %s, want run %s tenant %s", last.RunID, last.TenantID, runID, tenantID)
	}
	if last.Status != RestoreStatusFailed || !last.SetFinished {
		t.Errorf("final status call = %+v, want failed with finished_at set", last)
	}
	if want := "WPMgr could not prepare this restore."; last.Error != want {
		t.Errorf("restore run error = %q, want %q", last.Error, want)
	}
	for _, leak := range []string{err.Error(), missingSnapshot.String(), "Could not connect to the site"} {
		if strings.Contains(last.Error, leak) {
			t.Errorf("restore run error %q contains %q", last.Error, leak)
		}
	}
}

// ---------------------------------------------------------------------------
// Wire shape
// ---------------------------------------------------------------------------

// TestHandlers_AttemptErrorOnTheWire: GET /backups/:id and
// GET /schedule-runs/:id carry attempt_error while the row is running, and
// leave it out once the row is terminal.
func TestHandlers_AttemptErrorOnTheWire(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo, runs, svc, hub, tenantID, snapshotID := runningBackupFixture(t)
	const attempt = "The site did not answer in time."

	withTenant := func(c *gin.Context) {
		ctx := domain.WithTenantID(c.Request.Context(), tenantID)
		ctx = domain.WithPrincipal(ctx, domain.Principal{TenantID: tenantID})
		c.Request = c.Request.WithContext(ctx)
	}
	r := gin.New()
	r.Use(withTenant)
	h := NewHandler(svc, hub, nil)
	rh := NewScheduleRunHandler(svc)
	r.GET("/backups/:snapshotId", h.getBackup)
	r.GET("/schedule-runs/:runId", rh.getByID)

	get := func(path string) map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("GET %s: body is not JSON: %v", path, err)
		}
		return out
	}
	snapPath := "/backups/" + snapshotID.String()
	runPath := "/schedule-runs/" + runs.rows[0].ID.String()

	s := repo.snapshots[snapshotID]
	s.Status = StatusRunning
	s.AttemptError = attempt
	repo.snapshots[snapshotID] = s
	runs.rows[0].AttemptError = attempt

	snap, _ := get(snapPath)["snapshot"].(map[string]any)
	if got, _ := snap["attempt_error"].(string); got != attempt {
		t.Errorf("running snapshot attempt_error on the wire = %q, want %q (body %v)", got, attempt, snap)
	}
	if _, ok := snap["error"]; ok {
		t.Errorf("running snapshot carries error on the wire: %v", snap["error"])
	}
	if got, _ := get(runPath)["attempt_error"].(string); got != attempt {
		t.Errorf("running schedule run attempt_error on the wire = %q, want %q", got, attempt)
	}

	s.Status = StatusFailed
	s.Error = "final reason"
	repo.snapshots[snapshotID] = s
	runs.rows[0].Status = ScheduleRunStatusFailed
	final := "final reason"
	runs.rows[0].Error = &final

	snap, _ = get(snapPath)["snapshot"].(map[string]any)
	if _, ok := snap["attempt_error"]; ok {
		t.Errorf("failed snapshot still carries attempt_error on the wire: %v", snap["attempt_error"])
	}
	if got, _ := snap["error"].(string); got != "final reason" {
		t.Errorf("failed snapshot error on the wire = %q, want the final reason", got)
	}
	run := get(runPath)
	if _, ok := run["attempt_error"]; ok {
		t.Errorf("failed schedule run still carries attempt_error on the wire: %v", run["attempt_error"])
	}
	if got, _ := run["error"].(string); got != "final reason" {
		t.Errorf("failed schedule run error on the wire = %q, want the final reason", got)
	}
}
