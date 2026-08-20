package backup

// gh458_retry_resumes_claim_test.go — GH #458 follow-up regression.
//
// PR #497 added the "AND status='pending'" claim precondition to
// MarkBackupSnapshotRunning and wired BackupWorker.Work to return nil when the
// claim is refused. That is right for a genuinely lost claim and WRONG for the
// common case it also swallowed: a River retry of the SAME job re-entering
// Work over the row it already claimed on attempt 1. The snapshot is
// 'running', the guard matches nothing, and the pre-fix worker returned
// success without dispatching — so a transient transport error left the
// snapshot stranded 'running' with no backup behind it until the watchdog
// hard-failed it.
//
// The pair below is the fix's fire/over-fire proof. They must move in
// opposite directions ONLY across the fix: Resume goes red-to-green,
// LostToAnotherOwner must stay green in both.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// runningSnapshotFixture registers one snapshot in the fake repo in the state a
// worker retry finds it: already claimed and 'running'.
func runningSnapshotFixture(t *testing.T) (*fakeWorkerRepo, *fakeCommander, *Service, uuid.UUID, uuid.UUID) {
	t.Helper()
	repo := &fakeWorkerRepo{fakeRepo: newFakeRepo()}
	cmd := &fakeCommander{ok: true}
	tenantID := uuid.New()
	snapshotID := uuid.New()

	repo.setSnapshot(Snapshot{
		ID:           snapshotID,
		TenantID:     tenantID,
		SiteID:       uuid.New(),
		Kind:         KindFull,
		Status:       StatusRunning, // attempt 1 already claimed it
		AgeRecipient: "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p",
	})

	svc := &Service{repo: repo, sites: fakeWorkerSiteLookup{}, clock: fakeClock{t: time.Now()}}
	return repo, cmd, svc, tenantID, snapshotID
}

// TestBackupWorker_RetryResumesOwnClaimAndRedispatches is the regression.
//
// Attempt 1 claimed the snapshot ('running') and then hit a transient
// transport error, so River scheduled a retry. Attempt 2 re-enters Work: the
// claim guard refuses (the row is no longer 'pending'), and the worker must
// recognise the row as its own and dispatch the backup command again rather
// than reporting success and stranding the run.
func TestBackupWorker_RetryResumesOwnClaimAndRedispatches(t *testing.T) {
	repo, cmd, svc, tenantID, snapshotID := runningSnapshotFixture(t)

	worker := NewBackupWorker(svc, cmd, nil, nil, "https://cp.example.com", 0)
	job := &river.Job[BackupArgs]{
		// Attempt 2 == River retrying the job that made the claim. River
		// guarantees a single execution of a given job at a time, so this
		// cannot be racing attempt 1.
		JobRow: &rivertype.JobRow{Attempt: 2, MaxAttempts: 3},
		Args:   BackupArgs{TenantID: tenantID, SnapshotID: snapshotID},
	}

	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() on retry error: %v", err)
	}

	if cmd.lastBackup == nil {
		t.Fatal("retry of the owning job did not re-dispatch the backup command: " +
			"the snapshot is left 'running' with nothing behind it until the watchdog hard-fails it")
	}
	if cmd.lastBackup.SnapshotID != snapshotID.String() {
		t.Errorf("re-dispatched the wrong snapshot: got %q, want %q", cmd.lastBackup.SnapshotID, snapshotID.String())
	}
	// And the run must not have been stranded or dragged backwards.
	got, err := repo.GetSnapshot(context.Background(), tenantID, snapshotID)
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if got.Status != StatusRunning {
		t.Errorf("snapshot status after resumed retry = %q, want %q", got.Status, StatusRunning)
	}
}

// TestBackupWorker_FirstAttemptDoesNotStealAnotherOwnersClaim is the over-fire
// twin. Same 'running' row, but this job is on its FIRST attempt, so it never
// claimed anything: the running row belongs to someone else. It must NOT
// dispatch — otherwise the fix has reopened the double-claim hole the guard in
// MarkBackupSnapshotRunning exists to close.
//
// This test is green before the fix and must stay green after it.
func TestBackupWorker_FirstAttemptDoesNotStealAnotherOwnersClaim(t *testing.T) {
	_, cmd, svc, tenantID, snapshotID := runningSnapshotFixture(t)

	worker := NewBackupWorker(svc, cmd, nil, nil, "https://cp.example.com", 0)
	job := &river.Job[BackupArgs]{
		// Attempt 1: River's first execution of this job. It has made no claim
		// of its own, so a 'running' row is another owner's.
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 3},
		Args:   BackupArgs{TenantID: tenantID, SnapshotID: snapshotID},
	}

	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() on a lost claim must succeed without dispatching, got error: %v", err)
	}
	if cmd.lastBackup != nil || cmd.lastIncrementalBackup != nil {
		t.Fatal("a job that never held the claim dispatched a backup anyway: two live backups for one snapshot")
	}
}

// TestBackupWorker_RetryDoesNotResumeATerminalSnapshotViaTheTopOfWorkGuard
// asserts what its name says and nothing more: a retry whose snapshot was
// cancelled or watchdog-failed BEFORE Work ran finds a terminal row at the top
// of Work and returns there, dispatching nothing.
//
// It is named for the guard it exercises because the guard it does NOT
// exercise is resumedOwnClaim's status half. Work short-circuits at
// worker.go's terminal check before the claim is ever attempted, so this test
// passes against a resumedOwnClaim with the status check deleted, and it
// passed against the pre-GH#458 worker too. The review found it being read as
// coverage of the resume arm under its old name
// (…_RetryDoesNotResumeATerminalSnapshot). Coverage of the resume arm's status
// half lives in gh458_resume_status_guard_test.go, which reproduces the row
// going terminal BETWEEN the two reads.
func TestBackupWorker_RetryDoesNotResumeATerminalSnapshotViaTheTopOfWorkGuard(t *testing.T) {
	repo, cmd, svc, tenantID, snapshotID := runningSnapshotFixture(t)
	snap := repo.snapshots[snapshotID]
	snap.Status = StatusFailed
	snap.Error = "cancelled by operator"
	repo.setSnapshot(snap)

	worker := NewBackupWorker(svc, cmd, nil, nil, "https://cp.example.com", 0)
	job := &river.Job[BackupArgs]{
		JobRow: &rivertype.JobRow{Attempt: 2, MaxAttempts: 3},
		Args:   BackupArgs{TenantID: tenantID, SnapshotID: snapshotID},
	}

	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() on a terminal snapshot error: %v", err)
	}
	if cmd.lastBackup != nil || cmd.lastIncrementalBackup != nil {
		t.Fatal("a retry resurrected a cancelled/watchdog-failed snapshot and dispatched a backup for it")
	}
}
