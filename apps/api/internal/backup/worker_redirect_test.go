package backup

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// redirectCommander answers every command the way agentcmd does when the
// site redirects its command address, wrapped as a caller might see it.
type redirectCommander struct{ calls int }

func (r *redirectCommander) redirect(cmd string) error {
	r.calls++
	return fmt.Errorf("wrapped: %w", &agentcmd.RedirectError{
		Command:          cmd,
		Status:           301,
		From:             "https://example.com/wp-json/wpmgr/v1/command/" + cmd,
		To:               "https://www.example.com/wp-json/wpmgr/v1/command/" + cmd,
		SuggestedSiteURL: "https://www.example.com",
	})
}

func (r *redirectCommander) Backup(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.BackupRequest) (agentcmd.BackupResponse, error) {
	return agentcmd.BackupResponse{}, r.redirect("backup")
}

func (r *redirectCommander) IncrementalBackup(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.IncrementalBackupRequest) (agentcmd.BackupResponse, error) {
	return agentcmd.BackupResponse{}, r.redirect("backup")
}

func (r *redirectCommander) Restore(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.RestoreRequest) (agentcmd.RestoreResponse, error) {
	return agentcmd.RestoreResponse{}, r.redirect("restore")
}

// TestBackupWorker_RedirectFailsSnapshotOnFirstAttempt: a site that redirects
// its command address fails the backup on the first attempt, with a message
// naming the target and the remedy, and the job itself succeeds so River does
// not retry into the watchdog's generic stall message.
func TestBackupWorker_RedirectFailsSnapshotOnFirstAttempt(t *testing.T) {
	repo := &fakeWorkerRepo{fakeRepo: newFakeRepo()}
	cmd := &redirectCommander{}
	tenantID := uuid.New()
	snapshotID := uuid.New()
	repo.setSnapshot(Snapshot{
		ID:           snapshotID,
		TenantID:     tenantID,
		SiteID:       uuid.New(),
		Kind:         KindFull,
		Status:       StatusPending,
		AgeRecipient: "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p",
	})
	svc := &Service{repo: repo, sites: fakeWorkerSiteLookup{}, clock: fakeClock{t: time.Now()}}
	worker := NewBackupWorker(svc, cmd, nil, nil, "https://cp.example.com", 0)

	err := worker.Work(context.Background(), &river.Job[BackupArgs]{
		Args: BackupArgs{TenantID: tenantID, SnapshotID: snapshotID},
	})
	if err != nil {
		t.Fatalf("Work() returned %v; a redirect must be a terminal failure, not a retry", err)
	}
	if cmd.calls != 1 {
		t.Errorf("backup command sent %d times, want 1", cmd.calls)
	}
	got := repo.snapshots[snapshotID]
	if got.Status != StatusFailed {
		t.Fatalf("snapshot status = %q, want %q after one attempt", got.Status, StatusFailed)
	}
	for _, want := range []string{"Backup not started.", "https://www.example.com", "updates to https://www.example.com automatically"} {
		if !strings.Contains(got.Error, want) {
			t.Errorf("snapshot error %q does not contain %q", got.Error, want)
		}
	}
	if strings.Contains(got.Error, stallTimeoutMsg) || strings.Contains(got.Error, "status 404") {
		t.Errorf("snapshot error %q reads as a stall or a 404", got.Error)
	}
}

// TestRestoreWorker_RedirectFailsOnFirstAttempt is RestoreWorker's mirror of
// TestBackupWorker_RedirectFailsSnapshotOnFirstAttempt above: a site that
// redirects its command address fails the restore dispatch on the first
// attempt (never a retry), with exactly one Restore call, a "failed" progress
// event carrying the operator message, and the active restore_run finalized
// as failed — the same terminal-finalize assertion
// TestRestoreWorker_RefuseWithoutCode_StillFailsTerminal makes for an
// ordinary agent refusal, which is how that sibling test's own doc comment
// describes "recording ActionRestoreFailed": the audit call and the
// RecordProgress("failed", ...) call that drives this finalize are
// unconditional and adjacent in the source, so proving the finalize ran is
// proof the whole terminal-failure branch — audit call included — executed.
func TestRestoreWorker_RedirectFailsOnFirstAttempt(t *testing.T) {
	repo, runStore, tenantID, snapshotID, svc := newRestoreWorkerFixture(t)
	cmd := &redirectCommander{}
	worker := NewRestoreWorker(svc, cmd, nil, nil, "https://cp.example.com", 0)

	job := &river.Job[RestoreArgs]{Args: RestoreArgs{TenantID: tenantID, SnapshotID: snapshotID, Full: true}}
	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() returned %v; a redirect must be a terminal failure, not a retry", err)
	}
	if cmd.calls != 1 {
		t.Errorf("restore command sent %d times, want 1", cmd.calls)
	}
	if len(runStore.statusCalls) != 1 {
		t.Fatalf("expected exactly 1 MarkRestoreRunStatus call (the terminal finalize), got %d: %+v", len(runStore.statusCalls), runStore.statusCalls)
	}
	if runStore.statusCalls[0].Status != RestoreStatusFailed {
		t.Errorf("MarkRestoreRunStatus status = %q, want %q", runStore.statusCalls[0].Status, RestoreStatusFailed)
	}
	if !strings.HasPrefix(runStore.statusCalls[0].Error, "Restore not started.") {
		t.Errorf("MarkRestoreRunStatus error = %q, want a prefix of %q", runStore.statusCalls[0].Error, "Restore not started.")
	}
	if repo.failCalled {
		t.Error("FailSnapshot must not be called on the backup snapshot for a redirect")
	}
}
