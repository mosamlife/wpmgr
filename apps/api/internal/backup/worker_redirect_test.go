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
	for _, want := range []string{"Backup not started.", "https://www.example.com", "Reconnect the site"} {
		if !strings.Contains(got.Error, want) {
			t.Errorf("snapshot error %q does not contain %q", got.Error, want)
		}
	}
	if strings.Contains(got.Error, stallTimeoutMsg) || strings.Contains(got.Error, "status 404") {
		t.Errorf("snapshot error %q reads as a stall or a 404", got.Error)
	}
}
