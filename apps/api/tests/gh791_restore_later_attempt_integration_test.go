// gh791_restore_later_attempt_integration_test.go: GH #791 — a restore job
// that reaches a later attempt fails its queued or running run without
// sending anything to the site, and leaves a finished run alone; a planning
// error fails the run with the control plane's wording. Drives the
// PRODUCTION backup.RestoreWorker.Work against a real Postgres 16
// (testcontainers) through backup.NewRestoreRunRepo(pool), the store the
// production worker uses, as wpmgr_app under RLS.
package tests

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
)

// gh791RestoreCommander counts every command it is asked to send and fails
// each one, so a dispatch that should not happen is both counted and visible.
type gh791RestoreCommander struct{ calls int }

func (c *gh791RestoreCommander) Backup(context.Context, uuid.UUID, string, agentcmd.BackupRequest) (agentcmd.BackupResponse, error) {
	c.calls++
	return agentcmd.BackupResponse{}, fmt.Errorf("backup not used by this test")
}

func (c *gh791RestoreCommander) IncrementalBackup(context.Context, uuid.UUID, string, agentcmd.IncrementalBackupRequest) (agentcmd.BackupResponse, error) {
	c.calls++
	return agentcmd.BackupResponse{}, fmt.Errorf("incremental backup not used by this test")
}

func (c *gh791RestoreCommander) Restore(context.Context, uuid.UUID, string, agentcmd.RestoreRequest) (agentcmd.RestoreResponse, error) {
	c.calls++
	return agentcmd.RestoreResponse{}, fmt.Errorf("restore command transport: connection refused")
}

func gh791RestoreJob(tenantID, snapshotID, runID uuid.UUID, attempt, maxAttempts int) *river.Job[backup.RestoreArgs] {
	return &river.Job[backup.RestoreArgs]{
		JobRow: &rivertype.JobRow{Attempt: attempt, MaxAttempts: maxAttempts},
		Args: backup.RestoreArgs{
			TenantID:     tenantID,
			SnapshotID:   snapshotID,
			Full:         true,
			RestoreRunID: runID,
		},
	}
}

// TestGH791_RestoreLaterAttemptFailsRun: at attempt 2, a queued run and a
// running run are both finished as failed with the interruption copy, a
// completed run keeps its status and has no error, and no command is sent.
// At attempt 1, a planning error fails the run with the control plane's
// wording and never the raw error.
func TestGH791_RestoreLaterAttemptFailsRun(t *testing.T) {
	t.Parallel()
	const (
		interrupted = "The restore was interrupted before it finished and was not retried. Start it again from the backup."
		planFailed  = "WPMgr could not prepare this restore."
	)
	pool := startPostgres(t)
	store := startBlobstore(t)
	ctx := context.Background()

	tenantID := seedTenant(t, pool, "gh791-restore-later")
	siteID := seedSite(t, pool, tenantID, "https://restore-later.example.com")

	var role string
	var super, bypass bool
	if err := pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT current_user, rolsuper, rolbypassrls
			   FROM pg_roles WHERE rolname = current_user`).Scan(&role, &super, &bypass)
	}); err != nil {
		t.Fatalf("read current role: %v", err)
	}
	t.Logf("connected as %q rolsuper=%v rolbypassrls=%v", role, super, bypass)
	if role != "wpmgr_app" || super || bypass {
		t.Fatalf("this proof must run as wpmgr_app without superuser or BYPASSRLS; got %q super=%v bypass=%v", role, super, bypass)
	}

	svc := newBackupService(t, pool, store, stubSiteLookup{
		info: enrolledSiteInfo(siteID, "https://restore-later.example.com"),
	}, &stubEnqueuer{})
	runs := backup.NewRestoreRunRepo(pool)
	svc.SetRestoreRunStore(runs)

	snap, err := svc.CreateBackup(ctx, tenantID, siteID, uuid.Nil, "full")
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}
	submitManifest(t, svc, tenantID, snap.ID, minimalEntries(chunkHashes(1)))

	newRun := func(status string) backup.RestoreRun {
		t.Helper()
		run, err := runs.CreateRestoreRun(ctx, backup.CreateRestoreRunInput{
			TenantID:   tenantID,
			SiteID:     siteID,
			SnapshotID: snap.ID,
			Mode:       "full",
		})
		if err != nil {
			t.Fatalf("create restore run: %v", err)
		}
		switch status {
		case backup.RestoreStatusQueued:
		case backup.RestoreStatusRunning:
			err = runs.MarkRestoreRunStatus(ctx, backup.MarkRestoreRunStatusInput{
				TenantID: tenantID, RunID: run.ID, Status: status, SetStarted: true,
			})
		default:
			err = runs.MarkRestoreRunStatus(ctx, backup.MarkRestoreRunStatusInput{
				TenantID: tenantID, RunID: run.ID, Status: status, SetStarted: true, SetFinished: true,
			})
		}
		if err != nil {
			t.Fatalf("mark restore run %s: %v", status, err)
		}
		got, err := runs.GetRestoreRun(ctx, tenantID, run.ID)
		if err != nil {
			t.Fatalf("read seeded restore run: %v", err)
		}
		if got.Status != status {
			t.Fatalf("seeded restore run status = %q, want %q", got.Status, status)
		}
		return got
	}

	cmd := &gh791RestoreCommander{}
	worker := backup.NewRestoreWorker(svc, cmd, nil, nil, "https://cp.example.com", 0)

	for _, status := range []string{backup.RestoreStatusQueued, backup.RestoreStatusRunning} {
		run := newRun(status)
		err := worker.Work(ctx, gh791RestoreJob(tenantID, snap.ID, run.ID, 2, 25))
		var cancelErr *river.JobCancelError
		if !errors.As(err, &cancelErr) {
			t.Fatalf("%s run, attempt 2: Work() = %v, want a JobCancel", status, err)
		}
		got, err := runs.GetRestoreRun(ctx, tenantID, run.ID)
		if err != nil {
			t.Fatalf("%s run: read after Work(): %v", status, err)
		}
		if got.Status != backup.RestoreStatusFailed {
			t.Errorf("%s run, attempt 2: status = %q, want %q", status, got.Status, backup.RestoreStatusFailed)
		}
		if got.Error != interrupted {
			t.Errorf("%s run, attempt 2: error = %q, want %q", status, got.Error, interrupted)
		}
		if got.FinishedAt == nil {
			t.Errorf("%s run, attempt 2: finished_at is not set", status)
		}
	}

	done := newRun(backup.RestoreStatusCompleted)
	if err := worker.Work(ctx, gh791RestoreJob(tenantID, snap.ID, done.ID, 2, 25)); err == nil {
		t.Fatal("completed run, attempt 2: Work() = nil, want a JobCancel")
	}
	got, err := runs.GetRestoreRun(ctx, tenantID, done.ID)
	if err != nil {
		t.Fatalf("completed run: read after Work(): %v", err)
	}
	if got.Status != backup.RestoreStatusCompleted || got.Error != "" {
		t.Errorf("completed run, attempt 2: status %q error %q, want it unchanged (completed, no error)", got.Status, got.Error)
	}

	if cmd.calls != 0 {
		t.Errorf("commands sent on a later attempt = %d, want 0", cmd.calls)
	}

	// A planning error at attempt 1: the job points at a snapshot that does
	// not exist, so nothing can be planned and nothing is sent.
	planRun := newRun(backup.RestoreStatusQueued)
	missing := uuid.New()
	perr := worker.Work(ctx, gh791RestoreJob(tenantID, missing, planRun.ID, 1, 1))
	var planCancel *river.JobCancelError
	if !errors.As(perr, &planCancel) {
		t.Fatalf("planning error: Work() = %v, want a JobCancel", perr)
	}
	got, err = runs.GetRestoreRun(ctx, tenantID, planRun.ID)
	if err != nil {
		t.Fatalf("planning error: read after Work(): %v", err)
	}
	if got.Status != backup.RestoreStatusFailed || got.Error != planFailed {
		t.Errorf("planning error: status %q error %q, want %q %q", got.Status, got.Error, backup.RestoreStatusFailed, planFailed)
	}
	if cmd.calls != 0 {
		t.Errorf("commands sent for an unplannable restore = %d, want 0", cmd.calls)
	}
}
