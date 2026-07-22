package backup

// worker_test.go — GH #274 unit tests.
//
// On a slow host (most commonly OpenLiteSpeed missing
// fastcgi_finish_request), the agent's synchronous ack of a backup/restore
// dispatch can take long enough that the CP's HTTP round-trip times out
// before the ack arrives. River then retries the job with a fresh dispatch,
// and THAT retry hits the still-running original run's own single-flight
// dedup guard, which the agent refuses with a STABLE machine-readable code:
//
//	{"ok": false, "detail": "runner already in flight for this snapshot", "code": "runner_in_flight"}
//
// Before this fix, BackupWorker.Work / RestoreWorker.Work treated ANY
// ok=false as a terminal failure. For the in-flight case that is wrong and
// destructive: it flips a healthy running snapshot to failed (backup) or
// finalizes the genuinely-active restore_run row as failed (restore), even
// though the ORIGINAL run is still executing and will complete normally.
//
// These tests prove:
//  1. A "runner_in_flight" refusal is treated as benign — Work() returns nil,
//     but does NOT call FailSnapshot / terminally finalize the restore run.
//  2. The benign branch is NARROW: it keys on the STABLE Code field only. Any
//     other ok=false refusal (no code, or a different code) still takes the
//     existing terminal-failure path, unchanged — a regression guard so a
//     mistaken widening of the branch (e.g. matching on Detail/Log text)
//     would be caught here.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// refusalCommander — a Commander double whose Backup/IncrementalBackup/Restore
// responses are configured per-test. fakeCommander (worker_incremental_test.go)
// only models a boolean ok/not-ok; these tests need to control Detail/Log/Code
// on the refusal wire shape, so they use this sibling double instead.
// ---------------------------------------------------------------------------

type refusalCommander struct {
	backupResp  agentcmd.BackupResponse
	restoreResp agentcmd.RestoreResponse
}

func (c *refusalCommander) Backup(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.BackupRequest) (agentcmd.BackupResponse, error) {
	return c.backupResp, nil
}

func (c *refusalCommander) IncrementalBackup(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.IncrementalBackupRequest) (agentcmd.BackupResponse, error) {
	return c.backupResp, nil
}

func (c *refusalCommander) Restore(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.RestoreRequest) (agentcmd.RestoreResponse, error) {
	return c.restoreResp, nil
}

// ---------------------------------------------------------------------------
// failTrackingWorkerRepo wraps fakeWorkerRepo (worker_incremental_test.go) and
// additionally:
//   - tracks whether FailSnapshot was ever called, so tests can assert on the
//     terminal-failure side effect directly rather than inferring it from
//     status alone.
//   - implements UpdateSnapshotProgress, which RestoreWorker.Work calls
//     UNCONDITIONALLY (the "preflight" progress tick fires before the agent
//     dispatch, regardless of how the agent ultimately responds) and which
//     the base fakeRepo leaves panicking as "not implemented".
// ---------------------------------------------------------------------------

type failTrackingWorkerRepo struct {
	*fakeWorkerRepo
	failCalled bool
}

func newFailTrackingWorkerRepo() *failTrackingWorkerRepo {
	return &failTrackingWorkerRepo{
		fakeWorkerRepo: &fakeWorkerRepo{fakeRepo: newFakeRepo(), workerManifests: map[uuid.UUID][]ManifestEntry{}},
	}
}

func (r *failTrackingWorkerRepo) FailSnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID, msg string) (Snapshot, error) {
	r.failCalled = true
	return r.fakeWorkerRepo.FailSnapshot(ctx, tenantID, snapshotID, msg)
}

func (r *failTrackingWorkerRepo) UpdateSnapshotProgress(_ context.Context, _, snapshotID uuid.UUID, _ []byte) (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshots[snapshotID], nil
}

// ---------------------------------------------------------------------------
// fakeRestoreRunStore — a minimal RestoreRunStore double that records every
// MarkRestoreRunStatus call. `active`, when set, is returned by
// ActiveRestoreRunForSnapshot so persistRestoreRunEvent finds a run to
// (possibly) finalize — mirroring the real ORIGINAL, still-running restore
// this scenario is about.
// ---------------------------------------------------------------------------

type fakeRestoreRunStore struct {
	active      RestoreRun
	hasActive   bool
	statusCalls []MarkRestoreRunStatusInput
}

func (f *fakeRestoreRunStore) CreateRestoreRun(_ context.Context, _ CreateRestoreRunInput) (RestoreRun, error) {
	return RestoreRun{}, nil
}
func (f *fakeRestoreRunStore) GetRestoreRun(_ context.Context, _, _ uuid.UUID) (RestoreRun, error) {
	return RestoreRun{}, nil
}
func (f *fakeRestoreRunStore) ListRestoreRunsBySite(_ context.Context, _, _ uuid.UUID, _ int) ([]RestoreRun, error) {
	return nil, nil
}
func (f *fakeRestoreRunStore) ActiveRestoreRunForSnapshot(_ context.Context, _, _ uuid.UUID) (RestoreRun, error) {
	if !f.hasActive {
		return RestoreRun{}, domain.NotFound("restore_run_not_found", "no active restore run")
	}
	return f.active, nil
}
func (f *fakeRestoreRunStore) AppendRestoreEvent(_ context.Context, _ AppendRestoreEventInput) (RestoreRunEvent, error) {
	return RestoreRunEvent{}, nil
}
func (f *fakeRestoreRunStore) UpdateRestoreRunPhase(_ context.Context, _, _ uuid.UUID, _ string) error {
	return nil
}
func (f *fakeRestoreRunStore) MarkRestoreRunStatus(_ context.Context, in MarkRestoreRunStatusInput) error {
	f.statusCalls = append(f.statusCalls, in)
	return nil
}
func (f *fakeRestoreRunStore) ListRestoreEvents(_ context.Context, _, _ uuid.UUID, _ int64, _ int) ([]RestoreRunEvent, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// BackupWorker.Work — runner_in_flight (GH #274)
// ---------------------------------------------------------------------------

func newBackupWorkerFixture() (*failTrackingWorkerRepo, uuid.UUID, uuid.UUID, *Service) {
	repo := newFailTrackingWorkerRepo()
	tenantID := uuid.New()
	snapshotID := uuid.New()
	snap := Snapshot{
		ID:           snapshotID,
		TenantID:     tenantID,
		SiteID:       uuid.New(),
		Kind:         KindFull,
		Status:       StatusPending,
		AgeRecipient: "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p",
	}
	repo.setSnapshot(snap)
	svc := &Service{repo: repo, sites: fakeWorkerSiteLookup{}, clock: fakeClock{t: time.Now()}}
	return repo, tenantID, snapshotID, svc
}

// TestBackupWorker_RunnerInFlight_DoesNotFailSnapshot: an agent refusal
// carrying Code="runner_in_flight" must be treated as benign — Work() returns
// nil (so River does not retry a job that would only be refused again), but
// must NOT call FailSnapshot, and the snapshot must stay `running` so the
// still-in-flight original run's own SubmitManifest completes it normally.
func TestBackupWorker_RunnerInFlight_DoesNotFailSnapshot(t *testing.T) {
	repo, tenantID, snapshotID, svc := newBackupWorkerFixture()
	cmd := &refusalCommander{backupResp: agentcmd.BackupResponse{
		OK:     false,
		Detail: "runner already in flight for this snapshot",
		Code:   "runner_in_flight",
	}}
	worker := NewBackupWorker(svc, cmd, nil, nil, "https://cp.example.com", 0)

	job := &river.Job[BackupArgs]{Args: BackupArgs{TenantID: tenantID, SnapshotID: snapshotID}}
	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error: %v (a runner_in_flight refusal must be swallowed, not returned as a retryable error)", err)
	}
	if repo.failCalled {
		t.Error("FailSnapshot must NOT be called for a runner_in_flight refusal")
	}
	got := repo.snapshots[snapshotID]
	if got.Status != StatusRunning {
		t.Errorf("snapshot status = %q, want %q (must stay running so the in-flight run's own manifest submission completes it)", got.Status, StatusRunning)
	}
}

// TestBackupWorker_RefuseWithoutCode_StillFailsTerminal is the narrow-branch
// regression guard: an ordinary ok=false refusal with no Code, or a Code that
// is not "runner_in_flight", must still terminal-fail exactly as before.
func TestBackupWorker_RefuseWithoutCode_StillFailsTerminal(t *testing.T) {
	tests := []struct {
		name string
		resp agentcmd.BackupResponse
	}{
		{"no code at all", agentcmd.BackupResponse{OK: false, Detail: "preflight_failed: disk full"}},
		{"a different, unrelated code", agentcmd.BackupResponse{OK: false, Detail: "disk full", Code: "preflight_failed"}},
		{"detail text happens to mention in flight, but no code", agentcmd.BackupResponse{OK: false, Detail: "runner already in flight for this snapshot"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, tenantID, snapshotID, svc := newBackupWorkerFixture()
			cmd := &refusalCommander{backupResp: tt.resp}
			worker := NewBackupWorker(svc, cmd, nil, nil, "https://cp.example.com", 0)

			job := &river.Job[BackupArgs]{Args: BackupArgs{TenantID: tenantID, SnapshotID: snapshotID}}
			if err := worker.Work(context.Background(), job); err != nil {
				t.Fatalf("Work() error: %v (a terminal refusal is recorded via w.fail and returns nil so River does not retry)", err)
			}
			if !repo.failCalled {
				t.Error("FailSnapshot must be called for a code-less/unrelated-code refusal (this is the narrow-branch regression guard)")
			}
			got := repo.snapshots[snapshotID]
			if got.Status != StatusFailed {
				t.Errorf("snapshot status = %q, want %q", got.Status, StatusFailed)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// RestoreWorker.Work — runner_in_flight (GH #274), symmetric to the backup
// case above but the destructive side effect is different: instead of
// flipping backup_snapshots.status, the pre-fix code finalized the ACTIVE
// restore_runs row (found via ActiveRestoreRunForSnapshot — i.e. the
// genuinely in-flight ORIGINAL restore) as failed via persistRestoreRunEvent.
// ---------------------------------------------------------------------------

func newRestoreWorkerFixture(t *testing.T) (*failTrackingWorkerRepo, *fakeRestoreRunStore, uuid.UUID, uuid.UUID, *Service) {
	t.Helper()
	repo := newFailTrackingWorkerRepo()
	tenantID := uuid.New()
	siteID := uuid.New()
	snapshotID := uuid.New()

	// The restored-FROM snapshot is a completed backup (the only kind that is
	// ever restorable) — this matters: RecordProgress's own guard only calls
	// FailSnapshot for a phase="failed" event when the snapshot is NOT already
	// completed, so a completed backup's status is never at risk here. The
	// real GH #274 blast radius for restore is the ACTIVE restore_runs row.
	snap := Snapshot{
		ID:           snapshotID,
		TenantID:     tenantID,
		SiteID:       siteID,
		Kind:         KindFull,
		Status:       StatusCompleted,
		AgeRecipient: "age1test",
	}
	repo.setSnapshot(snap)
	repo.workerManifests[snapshotID] = []ManifestEntry{
		{Path: "wp-content/plugins/foo/foo.php", EntryKind: EntryKindFile, ChunkHashes: []string{"aaa"}, Size: 100},
	}

	runStore := &fakeRestoreRunStore{
		hasActive: true,
		active: RestoreRun{
			ID:         uuid.New(),
			TenantID:   tenantID,
			SiteID:     siteID,
			SnapshotID: snapshotID,
			Status:     RestoreStatusRunning,
		},
	}

	fp := &fakePresigner{}
	svc := &Service{
		repo:        repo,
		sites:       &fakeSiteLookup{},
		store:       &tenantPresigner{tenantID: tenantID, inner: fp},
		clock:       fakeClock{t: time.Now()},
		presignTTL:  time.Hour,
		restoreRuns: runStore,
	}
	return repo, runStore, tenantID, snapshotID, svc
}

// TestRestoreWorker_RunnerInFlight_DoesNotFailRestoreRun: an agent refusal
// carrying Code="runner_in_flight" must be treated as benign — Work() returns
// nil, but must NOT record ActionRestoreFailed / emit a "failed" progress
// event, and therefore must NOT finalize the genuinely-active restore_run row
// (asserted here via the fake store's MarkRestoreRunStatus call log, which
// must stay empty) nor flip the snapshot to failed.
func TestRestoreWorker_RunnerInFlight_DoesNotFailRestoreRun(t *testing.T) {
	repo, runStore, tenantID, snapshotID, svc := newRestoreWorkerFixture(t)
	cmd := &refusalCommander{restoreResp: agentcmd.RestoreResponse{
		OK:   false,
		Log:  "runner already in flight for this restore",
		Code: "runner_in_flight",
	}}
	worker := NewRestoreWorker(svc, cmd, nil, nil, "https://cp.example.com", 0)

	job := &river.Job[RestoreArgs]{Args: RestoreArgs{TenantID: tenantID, SnapshotID: snapshotID, Full: true}}
	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error: %v (a runner_in_flight refusal must be swallowed, not returned as a retryable error)", err)
	}
	if repo.failCalled {
		t.Error("FailSnapshot must NOT be called for a runner_in_flight refusal")
	}
	if len(runStore.statusCalls) != 0 {
		t.Errorf("MarkRestoreRunStatus must NOT be called for a runner_in_flight refusal (the active restore_run is genuinely still running), got %+v", runStore.statusCalls)
	}
	got := repo.snapshots[snapshotID]
	if got.Status != StatusCompleted {
		t.Errorf("snapshot status = %q, want unchanged %q", got.Status, StatusCompleted)
	}
}

// TestRestoreWorker_RefuseWithoutCode_StillFailsTerminal is the narrow-branch
// regression guard for restore: an ordinary ok=false refusal with no Code, or
// an unrelated Code, must still terminal-fail exactly as before — recording
// ActionRestoreFailed and finalizing the active restore_run row as failed.
func TestRestoreWorker_RefuseWithoutCode_StillFailsTerminal(t *testing.T) {
	tests := []struct {
		name string
		resp agentcmd.RestoreResponse
	}{
		{"no code at all", agentcmd.RestoreResponse{OK: false, Log: "some other refusal"}},
		{"a different, unrelated code", agentcmd.RestoreResponse{OK: false, Log: "preflight failed", Code: "preflight_failed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, runStore, tenantID, snapshotID, svc := newRestoreWorkerFixture(t)
			cmd := &refusalCommander{restoreResp: tt.resp}
			worker := NewRestoreWorker(svc, cmd, nil, nil, "https://cp.example.com", 0)

			job := &river.Job[RestoreArgs]{Args: RestoreArgs{TenantID: tenantID, SnapshotID: snapshotID, Full: true}}
			if err := worker.Work(context.Background(), job); err != nil {
				t.Fatalf("Work() error: %v", err)
			}
			if len(runStore.statusCalls) != 1 {
				t.Fatalf("expected exactly 1 MarkRestoreRunStatus call (the terminal finalize), got %d: %+v", len(runStore.statusCalls), runStore.statusCalls)
			}
			if runStore.statusCalls[0].Status != RestoreStatusFailed {
				t.Errorf("MarkRestoreRunStatus status = %q, want %q", runStore.statusCalls[0].Status, RestoreStatusFailed)
			}
			// The restored-FROM snapshot is already `completed` (see fixture doc
			// comment) — RecordProgress's own guard means FailSnapshot is never
			// reached for a restore refusal either way; confirm that holds.
			if repo.failCalled {
				t.Error("FailSnapshot must not be called on the backup snapshot for a restore refusal")
			}
		})
	}
}
