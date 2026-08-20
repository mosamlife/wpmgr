package backup

// gh458_resume_status_guard_test.go — GH #458 review follow-up.
//
// resumedOwnClaim is `job.Attempt > 1 && cur.Status == StatusRunning`
// (worker.go). The review mutated it to `return job.Attempt > 1`, deleting the
// status half outright, and the whole package stayed green: nothing covered
// that half.
//
// TestBackupWorker_RetryDoesNotResumeATerminalSnapshotViaTheTopOfWorkGuard
// (gh458_retry_resumes_claim_test.go) does not cover it either — it seeds a
// terminal row that the terminal check at the TOP of Work catches, so Work
// returns before resumedOwnClaim is ever consulted.
//
// The uncovered case is the race the resume arm actually lives in: the row is
// non-terminal when Work reads it, and terminal by the time the claim is
// refused and Work re-reads it. That is exactly where a cancel or a late agent
// manifest submit lands. Without the status half, a retry dispatches a full
// backup for an already-terminal snapshot — the agent burns a real run whose
// manifest SubmitManifest then rejects on the terminal guard.
//
// MUTATION PROOF for this file: replace the body of resumedOwnClaim with
// `return job.Attempt > 1` and TestBackupWorker_RetryDoesNotResumeASnapshot-
// ThatWentTerminalMidWork goes red while its over-fire twin
// (TestBackupWorker_RetryResumesOwnClaimAndRedispatches) stays green.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// raceyWorkerRepo makes GetSnapshot return a DIFFERENT row on its second call,
// which is the only way to reproduce the window between Work's terminal check
// and its post-claim re-read inside a single-threaded test.
//
// Call 1 (Work's terminal check) sees the seeded 'running' row, so Work
// proceeds. MarkSnapshotRunning refuses, as the real claim guard does for any
// non-pending row. Call 2 (the re-read that feeds resumedOwnClaim) sees the
// row after the concurrent transition landed.
type raceyWorkerRepo struct {
	*fakeWorkerRepo
	getCalls   int
	afterRace  Snapshot
	raceOnCall int
}

func (r *raceyWorkerRepo) GetSnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID) (Snapshot, error) {
	r.mu.Lock()
	r.getCalls++
	n := r.getCalls
	r.mu.Unlock()
	if n >= r.raceOnCall {
		return r.afterRace, nil
	}
	return r.fakeWorkerRepo.GetSnapshot(ctx, tenantID, snapshotID)
}

// TestBackupWorker_RetryDoesNotResumeASnapshotThatWentTerminalMidWork is the
// regression for the uncovered half of resumedOwnClaim.
//
// Attempt 2 of the owning job re-enters Work. The snapshot is still 'running'
// when Work reads it at the top, so the terminal short-circuit does not fire.
// Between that read and the claim, a late agent manifest submit completes the
// snapshot. The claim is refused, Work re-reads, and finds 'completed'. Only
// the status half of resumedOwnClaim stands between that and a redundant full
// backup dispatch.
func TestBackupWorker_RetryDoesNotResumeASnapshotThatWentTerminalMidWork(t *testing.T) {
	base, cmd, svc, tenantID, snapshotID := runningSnapshotFixture(t)

	completed := base.snapshots[snapshotID]
	completed.Status = StatusCompleted
	repo := &raceyWorkerRepo{
		fakeWorkerRepo: base,
		afterRace:      completed,
		raceOnCall:     2, // the post-claim re-read, not the terminal check
	}
	svc.repo = repo

	worker := NewBackupWorker(svc, cmd, nil, nil, "https://cp.example.com", 0)
	job := &river.Job[BackupArgs]{
		JobRow: &rivertype.JobRow{Attempt: 2, MaxAttempts: 3},
		Args:   BackupArgs{TenantID: tenantID, SnapshotID: snapshotID},
	}

	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	// The fixture is only meaningful if Work actually reached the re-read;
	// if it short-circuited at the top the assertion below proves nothing.
	if repo.getCalls < 2 {
		t.Fatalf("Work made %d GetSnapshot calls, want >= 2: it never reached the "+
			"post-claim re-read, so this test is not exercising resumedOwnClaim", repo.getCalls)
	}
	if cmd.lastBackup != nil || cmd.lastIncrementalBackup != nil {
		t.Fatal("a retry dispatched a backup for a snapshot that completed mid-Work: " +
			"the agent burns a full run whose manifest SubmitManifest then rejects")
	}
}

// TestBackupWorker_RetryStillResumesWhenTheRowIsStillRunning is the over-fire
// twin for the test above, and the one that must NOT move under the mutation.
//
// Same racey repo, same two GetSnapshot calls, but the second read finds the
// row exactly as attempt 1 left it: still 'running'. This is the legitimate
// resume, and it must dispatch. A status guard that was tightened until the
// test above passed — say, by refusing every retry — would redden here.
func TestBackupWorker_RetryStillResumesWhenTheRowIsStillRunning(t *testing.T) {
	base, cmd, svc, tenantID, snapshotID := runningSnapshotFixture(t)

	stillRunning := base.snapshots[snapshotID]
	repo := &raceyWorkerRepo{
		fakeWorkerRepo: base,
		afterRace:      stillRunning,
		raceOnCall:     2,
	}
	svc.repo = repo

	worker := NewBackupWorker(svc, cmd, nil, nil, "https://cp.example.com", 0)
	job := &river.Job[BackupArgs]{
		JobRow: &rivertype.JobRow{Attempt: 2, MaxAttempts: 3},
		Args:   BackupArgs{TenantID: tenantID, SnapshotID: snapshotID},
	}

	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}
	if repo.getCalls < 2 {
		t.Fatalf("Work made %d GetSnapshot calls, want >= 2", repo.getCalls)
	}
	if cmd.lastBackup == nil {
		t.Fatal("the owning job's retry did not re-dispatch over its own still-running claim: " +
			"the run is stranded until the watchdog hard-fails it")
	}
}
