package backup

import (
	"context"
	"strings"
	"testing"

	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// GH #1004: the agent refuses a backup when the key it holds does not match
// the recipient WPMgr recorded for the site. Retrying cannot change that, so
// the run ends at once with fixed copy that names the one way to accept the
// site's current key. The assertions use the words an operator reads, not the
// package's constants, so they hold the copy itself to account.

// assertReconnectMessage checks the stored failure names the reconnect steps,
// says existing backups are fine, and forwards none of the agent's own detail.
func assertReconnectMessage(t *testing.T, got Snapshot, agentDetail string) {
	t.Helper()
	if got.Status != StatusFailed {
		t.Fatalf("snapshot status = %q, want %q", got.Status, StatusFailed)
	}
	for _, want := range []string{
		"backup key changed after WPMgr recorded it",
		"reconnect the site",
		"click Disconnect",
		"click Reconnect",
		"new pairing code",
		"activate the WPMgr Agent plugin again if it was deactivated",
		"open the WPMgr Agent menu and enter the code",
		"Backups already taken are not affected.",
	} {
		if !strings.Contains(got.Error, want) {
			t.Errorf("snapshot error = %q, want it to contain %q", got.Error, want)
		}
	}
	if strings.Contains(got.Error, "agent refused the backup") {
		t.Errorf("snapshot error = %q, the raw refusal must not reach the operator for this case", got.Error)
	}
	if agentDetail != "" && agentDetail != "age recipient mismatch" && strings.Contains(got.Error, agentDetail) {
		t.Errorf("snapshot error = %q, the agent's own detail must never be forwarded", got.Error)
	}
}

func runRefusal(t *testing.T, resp agentcmd.BackupResponse) (*failTrackingWorkerRepo, Snapshot, error) {
	t.Helper()
	repo, tenantID, snapshotID, svc := newBackupWorkerFixture()
	worker := NewBackupWorker(svc, &refusalCommander{backupResp: resp}, nil, nil, "https://cp.example.com", 0)
	job := &river.Job[BackupArgs]{Args: BackupArgs{TenantID: tenantID, SnapshotID: snapshotID}}
	err := worker.Work(context.Background(), job)
	return repo, repo.snapshots[snapshotID], err
}

// The coded refusal (agents after 0.61.167).
func TestBackupWorker1004AgeRecipientMismatchCodeFailsWithReconnectSteps(t *testing.T) {
	const detail = "Backup refused: run the repair tool at https://example.invalid first."
	repo, got, err := runRefusal(t, agentcmd.BackupResponse{OK: false, Code: "age_recipient_mismatch", Detail: detail})
	if err != nil {
		t.Fatalf("Work() error = %v, want nil: the refusal is terminal and must not be retried", err)
	}
	if !repo.failCalled {
		t.Fatal("FailSnapshot was not called for an age_recipient_mismatch refusal")
	}
	assertReconnectMessage(t, got, detail)
}

// The uncoded refusal agents up to 0.61.167 send: the detail is exactly this.
func TestBackupWorker1004LegacyAgeRecipientMismatchDetailFailsWithReconnectSteps(t *testing.T) {
	repo, got, err := runRefusal(t, agentcmd.BackupResponse{OK: false, Detail: "age recipient mismatch"})
	if err != nil {
		t.Fatalf("Work() error = %v, want nil: the refusal is terminal and must not be retried", err)
	}
	if !repo.failCalled {
		t.Fatal("FailSnapshot was not called for the legacy age recipient mismatch refusal")
	}
	assertReconnectMessage(t, got, "")
}

// Only the exact legacy detail, with no code, is reclassified. Anything else
// keeps the existing generic terminal message.
func TestBackupWorker1004MismatchMatchingIsExact(t *testing.T) {
	cases := map[string]agentcmd.BackupResponse{
		"detail with more text":            {OK: false, Detail: "age recipient mismatch: expected age1abc"},
		"detail in another case":           {OK: false, Detail: "Age recipient mismatch"},
		"the detail under a different code": {OK: false, Code: "preflight_failed", Detail: "age recipient mismatch"},
	}
	for name, resp := range cases {
		t.Run(name, func(t *testing.T) {
			_, got, err := runRefusal(t, resp)
			if err != nil {
				t.Fatalf("Work() error = %v, want nil", err)
			}
			if got.Status != StatusFailed {
				t.Fatalf("snapshot status = %q, want %q", got.Status, StatusFailed)
			}
			if want := "agent refused the backup: " + resp.Detail; got.Error != want {
				t.Fatalf("snapshot error = %q, want the unchanged generic message %q", got.Error, want)
			}
		})
	}
}

// keystore_read_failed means the site could not read its key just now. It is
// retried like a transport failure, with a plain reason shown meanwhile.
func TestBackupWorker1004KeystoreReadFailedIsRetried(t *testing.T) {
	repo, got, err := runRefusal(t, agentcmd.BackupResponse{
		OK:     false,
		Code:   "keystore_read_failed",
		Detail: "Backup not started: the database read failed. Visit https://example.invalid",
	})
	if err == nil {
		t.Fatal("Work() error = nil, want an error so River retries the dispatch")
	}
	if repo.failCalled {
		t.Fatal("FailSnapshot was called; a transient key read failure must be retried, not end the run")
	}
	if got.Status != StatusRunning {
		t.Fatalf("snapshot status = %q, want %q while the dispatch is retried", got.Status, StatusRunning)
	}
	const want = "The site could not read its backup key just now. WPMgr will try again automatically."
	if got.AttemptError != want {
		t.Fatalf("attempt error = %q, want %q", got.AttemptError, want)
	}
}
