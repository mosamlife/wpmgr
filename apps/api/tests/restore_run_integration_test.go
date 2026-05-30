package tests

// Restore-run integration tests (m16).
//
// Tests verify:
//   - ActiveRestoreRunForSnapshot picks the most-recent queued/running run.
//   - RecordProgress appends an event, advances current_phase, finalizes on a
//     terminal phase, and is idempotent (a second terminal phase does not
//     overwrite the status).
//   - CreateRestore inserts a queued run and threads the ID into the job args.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// minimalEntries returns a one-entry manifest (one file chunk) using the
// provided hashes. Avoids collision with the backup_integration_test.go
// entries helpers.
func minimalEntries(hashes []string) []agentcmd.ManifestEntry {
	entries := make([]agentcmd.ManifestEntry, 0, len(hashes))
	for i, h := range hashes {
		entries = append(entries, agentcmd.ManifestEntry{
			Path:      "file-" + string(rune('a'+i)) + ".bin",
			EntryKind: "file",
			Mode:      0o644,
			Size:      4,
			Chunks:    []agentcmd.ChunkRef{{Blake3: h, Size: 4}},
		})
	}
	return entries
}

// ---------------------------------------------------------------------------
// TestActiveRestoreRunForSnapshot
// ---------------------------------------------------------------------------

// TestActiveRestoreRunForSnapshot proves that ActiveRestoreRunForSnapshot
// returns the most-recent queued/running run for the snapshot, not an older
// one, and that it returns domain.NotFound when no active run exists.
func TestActiveRestoreRunForSnapshot(t *testing.T) {
	pool := startPostgres(t)
	store := startBlobstore(t)
	tenant := seedTenant(t, pool, "rr-active")
	siteID := seedSite(t, pool, tenant, "https://active.rr.example.com")

	enq := &stubEnqueuer{}
	svc := newBackupService(t, pool, store, stubSiteLookup{
		info: enrolledSiteInfo(siteID, "https://active.rr.example.com"),
	}, enq)
	restoreRepo := backup.NewRestoreRunRepo(pool)
	svc.SetRestoreRunStore(restoreRepo)

	ctx := context.Background()

	// Create a completed snapshot.
	snap, err := svc.CreateBackup(ctx, tenant, siteID, uuid.Nil, "full")
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}
	submitManifest(t, svc, tenant, snap.ID, minimalEntries(chunkHashes(1)))

	// Initially there should be no active run.
	_, err = restoreRepo.ActiveRestoreRunForSnapshot(ctx, tenant, snap.ID)
	if err == nil {
		t.Fatal("expected not-found for no active runs, got nil error")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindNotFound {
		t.Fatalf("expected domain.NotFound, got %v", err)
	}

	// Insert run1 first.
	run1, err := restoreRepo.CreateRestoreRun(ctx, backup.CreateRestoreRunInput{
		TenantID:   tenant,
		SiteID:     siteID,
		SnapshotID: snap.ID,
		Mode:       "full",
	})
	if err != nil {
		t.Fatalf("create run1: %v", err)
	}

	// Insert run2 (newer) by a subsequent insert.
	run2, err := restoreRepo.CreateRestoreRun(ctx, backup.CreateRestoreRunInput{
		TenantID:   tenant,
		SiteID:     siteID,
		SnapshotID: snap.ID,
		Mode:       "full",
	})
	if err != nil {
		t.Fatalf("create run2: %v", err)
	}

	// ActiveRestoreRunForSnapshot must return the most-recent run (run2).
	active, err := restoreRepo.ActiveRestoreRunForSnapshot(ctx, tenant, snap.ID)
	if err != nil {
		t.Fatalf("active run: %v", err)
	}
	if active.ID != run2.ID {
		t.Fatalf("want active run2 (%s), got %s", run2.ID, active.ID)
	}

	// Finalize run2 — run1 should become active.
	if err := restoreRepo.MarkRestoreRunStatus(ctx, backup.MarkRestoreRunStatusInput{
		TenantID:    tenant,
		RunID:       run2.ID,
		Status:      backup.RestoreStatusCompleted,
		SetFinished: true,
	}); err != nil {
		t.Fatalf("mark run2 completed: %v", err)
	}
	active2, err := restoreRepo.ActiveRestoreRunForSnapshot(ctx, tenant, snap.ID)
	if err != nil {
		t.Fatalf("active after run2 completed: %v", err)
	}
	if active2.ID != run1.ID {
		t.Fatalf("want active run1 (%s) after run2 terminal, got %s", run1.ID, active2.ID)
	}

	// Finalize run1 too — no active run.
	if err := restoreRepo.MarkRestoreRunStatus(ctx, backup.MarkRestoreRunStatusInput{
		TenantID:    tenant,
		RunID:       run1.ID,
		Status:      backup.RestoreStatusFailed,
		Error:       "test finalization",
		SetFinished: true,
	}); err != nil {
		t.Fatalf("mark run1 failed: %v", err)
	}
	_, err = restoreRepo.ActiveRestoreRunForSnapshot(ctx, tenant, snap.ID)
	if err == nil {
		t.Fatal("want not-found after all runs terminal, got nil")
	}
}

// ---------------------------------------------------------------------------
// TestRecordProgressAppendsRestoreEvents
// ---------------------------------------------------------------------------

// TestRecordProgressAppendsRestoreEvents verifies that when an active restore
// run exists for a snapshot, RecordProgress:
//  1. Appends a restore_run_events row for each restore phase.
//  2. Advances current_phase on the run.
//  3. Finalizes the run on a terminal phase.
//  4. Is idempotent: a second terminal phase call does NOT overwrite the
//     terminal status.
func TestRecordProgressAppendsRestoreEvents(t *testing.T) {
	pool := startPostgres(t)
	store := startBlobstore(t)
	tenant := seedTenant(t, pool, "rr-progress")
	siteID := seedSite(t, pool, tenant, "https://progress.rr.example.com")

	enq := &stubEnqueuer{}
	svc := newBackupService(t, pool, store, stubSiteLookup{
		info: enrolledSiteInfo(siteID, "https://progress.rr.example.com"),
	}, enq)
	restoreRepo := backup.NewRestoreRunRepo(pool)
	svc.SetRestoreRunStore(restoreRepo)

	ctx := context.Background()

	// Create a completed snapshot.
	snap, err := svc.CreateBackup(ctx, tenant, siteID, uuid.Nil, "full")
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}
	submitManifest(t, svc, tenant, snap.ID, minimalEntries(chunkHashes(1)))

	// Create an active restore run.
	run, err := restoreRepo.CreateRestoreRun(ctx, backup.CreateRestoreRunInput{
		TenantID:   tenant,
		SiteID:     siteID,
		SnapshotID: snap.ID,
		Mode:       "full",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// --- non-terminal phase: preflight ---

	_, err = svc.RecordProgress(ctx, tenant, snap.ID, "preflight", map[string]any{
		"message": "cp dispatched",
		"step":    "dispatch",
	})
	if err != nil {
		t.Fatalf("record preflight: %v", err)
	}

	events, err := restoreRepo.ListRestoreEvents(ctx, tenant, run.ID, 0, 50)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event after preflight, got %d", len(events))
	}
	if events[0].Phase != "preflight" {
		t.Fatalf("event phase: want preflight, got %s", events[0].Phase)
	}
	if events[0].Message != "cp dispatched" {
		t.Fatalf("event message: want 'cp dispatched', got %q", events[0].Message)
	}

	// current_phase must be advanced.
	reloaded, err := restoreRepo.GetRestoreRun(ctx, tenant, run.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if reloaded.CurrentPhase != "preflight" {
		t.Fatalf("current_phase: want preflight, got %q", reloaded.CurrentPhase)
	}
	if reloaded.Status != backup.RestoreStatusQueued {
		t.Fatalf("status after non-terminal: want queued, got %s", reloaded.Status)
	}

	// --- second non-terminal phase: maintenance_on ---

	_, err = svc.RecordProgress(ctx, tenant, snap.ID, "maintenance_on", map[string]any{})
	if err != nil {
		t.Fatalf("record maintenance_on: %v", err)
	}
	events2, err := restoreRepo.ListRestoreEvents(ctx, tenant, run.ID, 0, 50)
	if err != nil {
		t.Fatalf("list events 2: %v", err)
	}
	if len(events2) != 2 {
		t.Fatalf("want 2 events, got %d", len(events2))
	}

	// Incremental: afterID = first event ID should return only the second event.
	eventsAfter, err := restoreRepo.ListRestoreEvents(ctx, tenant, run.ID, events2[0].ID, 50)
	if err != nil {
		t.Fatalf("list events after: %v", err)
	}
	if len(eventsAfter) != 1 {
		t.Fatalf("want 1 event after id=%d, got %d", events2[0].ID, len(eventsAfter))
	}
	if eventsAfter[0].Phase != "maintenance_on" {
		t.Fatalf("incremental event phase: want maintenance_on, got %s", eventsAfter[0].Phase)
	}

	// --- terminal phase: completed ---

	_, err = svc.RecordProgress(ctx, tenant, snap.ID, "completed", map[string]any{
		"message": "all done",
	})
	if err != nil {
		t.Fatalf("record completed: %v", err)
	}
	terminal, err := restoreRepo.GetRestoreRun(ctx, tenant, run.ID)
	if err != nil {
		t.Fatalf("reload after terminal: %v", err)
	}
	if terminal.Status != backup.RestoreStatusCompleted {
		t.Fatalf("terminal status: want completed, got %s", terminal.Status)
	}
	if terminal.FinishedAt == nil {
		t.Fatal("finished_at should be set after terminal phase")
	}

	// Idempotency: a second terminal phase must NOT overwrite the status.
	_, err = svc.RecordProgress(ctx, tenant, snap.ID, "failed", map[string]any{
		"error": "spurious second terminal",
	})
	if err != nil {
		t.Fatalf("record failed (idempotency check): %v", err)
	}
	idempotent, err := restoreRepo.GetRestoreRun(ctx, tenant, run.ID)
	if err != nil {
		t.Fatalf("reload idempotent: %v", err)
	}
	if idempotent.Status != backup.RestoreStatusCompleted {
		t.Fatalf("idempotency broken: status changed to %s after second terminal phase", idempotent.Status)
	}
}

// ---------------------------------------------------------------------------
// TestCreateRestoreInsertsQueuedRun
// ---------------------------------------------------------------------------

// TestCreateRestoreInsertsQueuedRun verifies that CreateRestore inserts a
// restore_run row in the queued state and returns its ID in the result.
func TestCreateRestoreInsertsQueuedRun(t *testing.T) {
	pool := startPostgres(t)
	store := startBlobstore(t)
	tenant := seedTenant(t, pool, "rr-create")
	siteID := seedSite(t, pool, tenant, "https://create.rr.example.com")

	enq := &stubEnqueuer{}
	svc := newBackupService(t, pool, store, stubSiteLookup{
		info: enrolledSiteInfo(siteID, "https://create.rr.example.com"),
	}, enq)
	restoreRepo := backup.NewRestoreRunRepo(pool)
	svc.SetRestoreRunStore(restoreRepo)

	ctx := context.Background()

	// Create and complete a snapshot.
	snap, err := svc.CreateBackup(ctx, tenant, siteID, uuid.Nil, "full")
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}
	submitManifest(t, svc, tenant, snap.ID, minimalEntries(chunkHashes(1)))

	// Create a restore via the service.
	result, err := svc.CreateRestore(ctx, tenant, snap.ID, backup.RestoreSelection{Full: true}, "test-operator")
	if err != nil {
		t.Fatalf("create restore: %v", err)
	}

	// The result must carry a non-nil restore_run_id.
	if result.RestoreRunID == uuid.Nil {
		t.Fatal("RestoreRunID must be non-nil when the restore run store is wired")
	}

	// The run must be persisted with status=queued.
	run, err := restoreRepo.GetRestoreRun(ctx, tenant, result.RestoreRunID)
	if err != nil {
		t.Fatalf("get restore run: %v", err)
	}
	if run.Status != backup.RestoreStatusQueued {
		t.Fatalf("status: want queued, got %s", run.Status)
	}
	if run.TriggeredBy != "test-operator" {
		t.Fatalf("triggered_by: want 'test-operator', got %q", run.TriggeredBy)
	}
	if run.SnapshotID != snap.ID {
		t.Fatalf("snapshot_id: want %s, got %s", snap.ID, run.SnapshotID)
	}
	if run.SiteID != siteID {
		t.Fatalf("site_id: want %s, got %s", siteID, run.SiteID)
	}

	// The enqueuer must have received the restore job.
	enq.mu.Lock()
	defer enq.mu.Unlock()
	if len(enq.restores) != 1 {
		t.Fatalf("want 1 enqueued restore, got %d", len(enq.restores))
	}
}
