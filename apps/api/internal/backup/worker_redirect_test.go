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
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
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
// attempt (never a retry), with exactly one Restore call, and the active
// restore_run finalized as failed.
//
// Two separate records of that failure are asserted by content, not merely
// by a call count:
//
//   - The restore run's EVENT row (restore_run_events, via
//     fakeRestoreRunStore.eventCalls): the per-run phase log RecordProgress
//     writes and fans out to the UI's SSE channel.
//   - The audit_log row (via recordingAuditRecorder, which stands in for the
//     worker's *audit.Recorder): the tenant-wide, hash-chained audit trail
//     w.recordAudit writes. Its action, target and metadata are checked, so a
//     build that dropped the call or recorded the wrong action, target or
//     message fails here.
func TestRestoreWorker_RedirectFailsOnFirstAttempt(t *testing.T) {
	repo, runStore, tenantID, snapshotID, svc := newRestoreWorkerFixture(t)
	cmd := &redirectCommander{}
	worker := NewRestoreWorker(svc, cmd, nil, nil, "https://cp.example.com", 0)
	rec := &recordingAuditRecorder{}
	worker.audit = rec

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

	// The restore run's EVENT row (restore_run_events, NOT audit_log — see
	// the doc comment above): the AppendRestoreEvent row's action (Phase) and
	// outcome (Status, and the operator message in Detail) must be the
	// redirect failure, by content — not merely called. The dispatch also
	// writes an unconditional "preflight" event before ever contacting the
	// agent, so the terminal one under test is the LAST call, not the only
	// one.
	if len(runStore.eventCalls) != 2 {
		t.Fatalf("expected exactly 2 AppendRestoreEvent calls (preflight, then the redirect failure event), got %d: %+v", len(runStore.eventCalls), runStore.eventCalls)
	}
	ev := runStore.eventCalls[len(runStore.eventCalls)-1]
	if ev.Phase != "failed" {
		t.Errorf("AppendRestoreEvent phase = %q, want %q", ev.Phase, "failed")
	}
	if ev.Status != "failed" {
		t.Errorf("AppendRestoreEvent status = %q, want %q", ev.Status, "failed")
	}
	if ev.RestoreRunID != runStore.active.ID {
		t.Errorf("AppendRestoreEvent restore_run_id = %s, want the active run %s", ev.RestoreRunID, runStore.active.ID)
	}
	if !strings.Contains(string(ev.Detail), "Restore not started.") || !strings.Contains(string(ev.Detail), "https://www.example.com") {
		t.Errorf("AppendRestoreEvent detail %q does not carry the operator message naming the redirect target", ev.Detail)
	}

	// The audit_log rows: the dispatch records restore.started before it
	// contacts the agent, then exactly one restore.failed for the redirect,
	// against the snapshot, carrying the operator message the run was failed
	// with.
	if len(rec.events) != 2 {
		t.Fatalf("expected exactly 2 audit rows (restore.started, then the redirect failure), got %d: %+v", len(rec.events), rec.events)
	}
	if rec.events[0].Action != ActionRestoreStarted {
		t.Errorf("first audit action = %q, want %q", rec.events[0].Action, ActionRestoreStarted)
	}
	ae := rec.events[1]
	if ae.Action != ActionRestoreFailed {
		t.Errorf("audit action = %q, want %q", ae.Action, ActionRestoreFailed)
	}
	if ae.TenantID != tenantID {
		t.Errorf("audit tenant = %s, want %s", ae.TenantID, tenantID)
	}
	if ae.ActorType != audit.ActorSystem {
		t.Errorf("audit actor type = %q, want %q", ae.ActorType, audit.ActorSystem)
	}
	if ae.TargetType != "backup_snapshot" || ae.TargetID != snapshotID.String() {
		t.Errorf("audit target = %s/%s, want backup_snapshot/%s", ae.TargetType, ae.TargetID, snapshotID)
	}
	if got, _ := ae.Metadata["error"].(string); got != runStore.statusCalls[0].Error {
		t.Errorf("audit metadata error = %q, want the operator message the run was failed with, %q", got, runStore.statusCalls[0].Error)
	}
	if got, _ := ae.Metadata["restore_id"].(string); got == "" {
		t.Errorf("audit metadata restore_id is empty: %+v", ae.Metadata)
	}
}

// recordingAuditRecorder records every audit event RestoreWorker writes.
type recordingAuditRecorder struct{ events []audit.Event }

func (r *recordingAuditRecorder) Record(_ context.Context, e audit.Event) (audit.Entry, error) {
	r.events = append(r.events, e)
	return audit.Entry{}, nil
}
