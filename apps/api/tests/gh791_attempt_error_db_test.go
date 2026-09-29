// gh791_attempt_error_db_test.go: GH #791, the database half. m148 adds
// attempt_error to backup_snapshots and backup_schedule_runs, the last failed
// attempt to start a backup while the control plane is still retrying, kept
// apart from error (the final failure reason).
//
// Every assertion runs as wpmgr_app (NOSUPERUSER, NOBYPASSRLS), through the
// tenant transaction the repository layer uses (db.Pool.InTenantTx) and the
// generated sqlc queries, or through the backup repository and the
// production ProgressWatchdogWorker where those already exist. The admin pool
// only seeds fixtures a test cannot reach otherwise (a schedule, a linked run,
// a backdated start).
package tests

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// gh791AttemptMsg is a neutral control-plane description of a failed attempt.
const gh791AttemptMsg = "The site returned a server error (HTTP 502) that did not come from the WPMgr agent."

type gh791Fixture struct {
	app    *db.Pool
	admin  *db.Pool
	repo   backup.Repo
	tenant uuid.UUID
	site   uuid.UUID
}

func newGH791Fixture(t *testing.T, slug string) gh791Fixture {
	t.Helper()
	app := startPostgres(t)
	admin := connectAdmin(t, app)
	t.Cleanup(admin.Close)
	tenant := seedTenant(t, app, slug+"-"+uuid.NewString())
	site := seedSite(t, app, tenant, "")
	return gh791Fixture{app: app, admin: admin, repo: backup.NewRepo(app), tenant: tenant, site: site}
}

// runningSnapshot seeds a site of its own and a pending snapshot on it as
// wpmgr_app, then claims the snapshot through the repository, which is how a
// real run reaches 'running'. A site holds at most one in-flight snapshot
// (backup_snapshots_one_inflight_per_site), so every running snapshot a test
// needs gets a fresh site.
func (f gh791Fixture) runningSnapshot(t *testing.T) (snap, site uuid.UUID) {
	t.Helper()
	site = seedSite(t, f.app, f.tenant, "")
	snap = seedBackupSnapshotAt(t, f.app, f.tenant, site, time.Now())
	if _, claimed, err := f.repo.MarkSnapshotRunning(context.Background(), f.tenant, snap); err != nil || !claimed {
		t.Fatalf("MarkSnapshotRunning: claimed=%t err=%v", claimed, err)
	}
	return snap, site
}

// linkedRun seeds a schedule on site and a run linked to snap in the given
// status.
func (f gh791Fixture) linkedRun(t *testing.T, snap, site uuid.UUID, status string) uuid.UUID {
	t.Helper()
	sched := seedGuardSchedule(t, f.admin, f.tenant, site, time.Now().Add(time.Hour))
	var id uuid.UUID
	if err := f.admin.QueryRow(context.Background(),
		`INSERT INTO backup_schedule_runs (tenant_id, site_id, schedule_id, snapshot_id, scheduled_for, status)
		 VALUES ($1, $2, $3, $4, now(), $5) RETURNING id`,
		f.tenant, site, sched, snap, status).Scan(&id); err != nil {
		t.Fatalf("seed schedule run: %v", err)
	}
	return id
}

func (f gh791Fixture) setSnapAttempt(t *testing.T, snap uuid.UUID, msg string) int64 {
	t.Helper()
	var n int64
	if err := f.app.InTenantTx(context.Background(), f.tenant, func(tx pgx.Tx) error {
		var err error
		n, err = sqlc.New(tx).SetBackupSnapshotAttemptError(context.Background(), sqlc.SetBackupSnapshotAttemptErrorParams{
			AttemptError: msg, ID: snap, TenantID: f.tenant,
		})
		return err
	}); err != nil {
		t.Fatalf("SetBackupSnapshotAttemptError: %v", err)
	}
	return n
}

func (f gh791Fixture) setRunAttempt(t *testing.T, snap uuid.UUID, msg string) int64 {
	t.Helper()
	var n int64
	if err := f.app.InTenantTx(context.Background(), f.tenant, func(tx pgx.Tx) error {
		var err error
		n, err = sqlc.New(tx).SetScheduleRunAttemptErrorBySnapshot(context.Background(), sqlc.SetScheduleRunAttemptErrorBySnapshotParams{
			AttemptError: msg, SnapshotID: pgtype.UUID{Bytes: snap, Valid: true}, TenantID: f.tenant,
		})
		return err
	}); err != nil {
		t.Fatalf("SetScheduleRunAttemptErrorBySnapshot: %v", err)
	}
	return n
}

func (f gh791Fixture) clearRunAttempt(t *testing.T, snap uuid.UUID) int64 {
	t.Helper()
	var n int64
	if err := f.app.InTenantTx(context.Background(), f.tenant, func(tx pgx.Tx) error {
		var err error
		n, err = sqlc.New(tx).ClearScheduleRunAttemptErrorBySnapshot(context.Background(), sqlc.ClearScheduleRunAttemptErrorBySnapshotParams{
			SnapshotID: pgtype.UUID{Bytes: snap, Valid: true}, TenantID: f.tenant,
		})
		return err
	}); err != nil {
		t.Fatalf("ClearScheduleRunAttemptErrorBySnapshot: %v", err)
	}
	return n
}

func (f gh791Fixture) snap(t *testing.T, id uuid.UUID) sqlc.BackupSnapshot {
	t.Helper()
	var row sqlc.BackupSnapshot
	if err := f.app.InTenantTx(context.Background(), f.tenant, func(tx pgx.Tx) error {
		var err error
		row, err = sqlc.New(tx).GetBackupSnapshot(context.Background(), sqlc.GetBackupSnapshotParams{ID: id, TenantID: f.tenant})
		return err
	}); err != nil {
		t.Fatalf("GetBackupSnapshot: %v", err)
	}
	return row
}

func (f gh791Fixture) run(t *testing.T, id uuid.UUID) sqlc.BackupScheduleRun {
	t.Helper()
	var row sqlc.BackupScheduleRun
	if err := f.app.InTenantTx(context.Background(), f.tenant, func(tx pgx.Tx) error {
		var err error
		row, err = sqlc.New(tx).GetScheduleRun(context.Background(), sqlc.GetScheduleRunParams{ID: id, TenantID: f.tenant})
		return err
	}); err != nil {
		t.Fatalf("GetScheduleRun: %v", err)
	}
	return row
}

// TestGH791_AttemptErrorRecordedOnRunningRow: on a running snapshot and its
// running schedule run, the attempt error is recorded (1 row each), status
// stays 'running' and error stays empty, so nothing reads the row as failed.
// The proof-of-life clear then empties it on both.
func TestGH791_AttemptErrorRecordedOnRunningRow(t *testing.T) {
	f := newGH791Fixture(t, "gh791-set")
	ctx := context.Background()
	snapID, site := f.runningSnapshot(t)
	runID := f.linkedRun(t, snapID, site, backup.ScheduleRunStatusRunning)

	if n := f.setSnapAttempt(t, snapID, gh791AttemptMsg); n != 1 {
		t.Fatalf("SetBackupSnapshotAttemptError on a running row affected %d rows, want 1", n)
	}
	if n := f.setRunAttempt(t, snapID, gh791AttemptMsg); n != 1 {
		t.Fatalf("SetScheduleRunAttemptErrorBySnapshot on a running run affected %d rows, want 1", n)
	}
	s := f.snap(t, snapID)
	if s.Status != "running" || s.AttemptError != gh791AttemptMsg || s.Error != "" {
		t.Fatalf("snapshot after attempt error: status=%q attempt_error=%q error=%q; want running, the message, and an empty error",
			s.Status, s.AttemptError, s.Error)
	}
	r := f.run(t, runID)
	if r.Status != "running" || r.AttemptError != gh791AttemptMsg || (r.Error != nil && *r.Error != "") {
		t.Fatalf("run after attempt error: status=%q attempt_error=%q error=%v; want running, the message, and no error",
			r.Status, r.AttemptError, r.Error)
	}

	// Proof of life clears the snapshot's attempt error through the repository.
	cleared, err := f.repo.ClearSnapshotStalled(ctx, f.tenant, snapID)
	if err != nil || !cleared {
		t.Fatalf("ClearSnapshotStalled with an attempt error outstanding: cleared=%t err=%v, want true", cleared, err)
	}
	if got := f.snap(t, snapID).AttemptError; got != "" {
		t.Fatalf("attempt_error after proof of life = %q, want empty", got)
	}
	if cleared, err = f.repo.ClearSnapshotStalled(ctx, f.tenant, snapID); err != nil || cleared {
		t.Fatalf("second ClearSnapshotStalled with nothing outstanding: cleared=%t err=%v, want false", cleared, err)
	}
	if n := f.clearRunAttempt(t, snapID); n != 1 {
		t.Fatalf("ClearScheduleRunAttemptErrorBySnapshot affected %d rows, want 1", n)
	}
	if got := f.run(t, runID).AttemptError; got != "" {
		t.Fatalf("run attempt_error after clear = %q, want empty", got)
	}
	if n := f.clearRunAttempt(t, snapID); n != 0 {
		t.Fatalf("second ClearScheduleRunAttemptErrorBySnapshot affected %d rows, want 0", n)
	}
}

// TestGH791_AttemptErrorRefusedUnlessRunning: a pending row and a failed row
// are never matched. The failed row keeps its final reason, and its linked
// failed run is untouched too.
func TestGH791_AttemptErrorRefusedUnlessRunning(t *testing.T) {
	f := newGH791Fixture(t, "gh791-refuse")
	ctx := context.Background()

	pending := seedBackupSnapshotAt(t, f.app, f.tenant, f.site, time.Now())
	if n := f.setSnapAttempt(t, pending, gh791AttemptMsg); n != 0 {
		t.Fatalf("SetBackupSnapshotAttemptError on a pending row affected %d rows, want 0", n)
	}
	if got := f.snap(t, pending).AttemptError; got != "" {
		t.Fatalf("pending row attempt_error = %q, want empty", got)
	}

	const finalReason = "Backup failed: the agent reported an error at includes/commands/class-backup-command.php:239."
	failed, failedSite := f.runningSnapshot(t)
	runID := f.linkedRun(t, failed, failedSite, backup.ScheduleRunStatusFailed)
	if _, ok, err := f.repo.FailSnapshot(ctx, f.tenant, failed, finalReason); err != nil || !ok {
		t.Fatalf("FailSnapshot: ok=%t err=%v", ok, err)
	}
	if n := f.setSnapAttempt(t, failed, gh791AttemptMsg); n != 0 {
		t.Fatalf("SetBackupSnapshotAttemptError on a failed row affected %d rows, want 0", n)
	}
	s := f.snap(t, failed)
	if s.Status != "failed" || s.Error != finalReason || s.AttemptError != "" {
		t.Fatalf("failed row after refused write: status=%q error=%q attempt_error=%q; want failed, the final reason, empty",
			s.Status, s.Error, s.AttemptError)
	}
	if n := f.setRunAttempt(t, failed, gh791AttemptMsg); n != 0 {
		t.Fatalf("SetScheduleRunAttemptErrorBySnapshot on a failed run affected %d rows, want 0", n)
	}
	if r := f.run(t, runID); r.Status != "failed" || r.AttemptError != "" {
		t.Fatalf("failed run after refused write: status=%q attempt_error=%q; want failed and empty", r.Status, r.AttemptError)
	}
}

// TestGH791_CompleteClearsAttemptError: completing a running snapshot that
// carries an attempt error clears it, and so does completing its run.
func TestGH791_CompleteClearsAttemptError(t *testing.T) {
	f := newGH791Fixture(t, "gh791-complete")
	ctx := context.Background()
	snapID, site := f.runningSnapshot(t)
	runID := f.linkedRun(t, snapID, site, backup.ScheduleRunStatusRunning)
	if n := f.setSnapAttempt(t, snapID, gh791AttemptMsg); n != 1 {
		t.Fatalf("seed attempt error on snapshot: %d rows, want 1", n)
	}
	if n := f.setRunAttempt(t, snapID, gh791AttemptMsg); n != 1 {
		t.Fatalf("seed attempt error on run: %d rows, want 1", n)
	}

	if _, ok, err := f.repo.CompleteSnapshot(ctx, f.tenant, snapID, 10, 1); err != nil || !ok {
		t.Fatalf("CompleteSnapshot: ok=%t err=%v", ok, err)
	}
	if s := f.snap(t, snapID); s.Status != "completed" || s.AttemptError != "" || s.Error != "" {
		t.Fatalf("completed snapshot: status=%q attempt_error=%q error=%q; want completed and both empty", s.Status, s.AttemptError, s.Error)
	}

	runs := backup.NewScheduleRunRepo(f.app)
	if _, err := runs.SetScheduleRunStatusBySnapshot(ctx, f.tenant, snapID, backup.SetScheduleRunStatusInput{
		TenantID: f.tenant, Status: backup.ScheduleRunStatusCompleted, SetFinished: true,
	}); err != nil {
		t.Fatalf("SetScheduleRunStatusBySnapshot(completed): %v", err)
	}
	if r := f.run(t, runID); r.Status != "completed" || r.AttemptError != "" {
		t.Fatalf("completed run: status=%q attempt_error=%q; want completed and empty", r.Status, r.AttemptError)
	}
}

// TestGH791_WatchdogHardFailKeepsLastError runs the production progress
// watchdog over three runs past the hard deadline: one with an attempt error
// (its failure reason keeps it after the watchdog's message), one without
// (the reason is the watchdog's message alone), and one whose attempt error is
// at the length cap (the stored reason is capped at 1024 characters).
func TestGH791_WatchdogHardFailKeepsLastError(t *testing.T) {
	f := newGH791Fixture(t, "gh791-watchdog")
	ctx := context.Background()

	withErr, _ := f.runningSnapshot(t)
	plain, _ := f.runningSnapshot(t)
	long, _ := f.runningSnapshot(t)
	if n := f.setSnapAttempt(t, withErr, gh791AttemptMsg); n != 1 {
		t.Fatalf("seed attempt error: %d rows, want 1", n)
	}
	longMsg := strings.Repeat("x", 2000)
	if n := f.setSnapAttempt(t, long, longMsg); n != 1 {
		t.Fatalf("seed long attempt error: %d rows, want 1", n)
	}
	if got := len(f.snap(t, long).AttemptError); got != 1024 {
		t.Fatalf("stored attempt_error length = %d, want 1024 (the write caps it)", got)
	}
	if _, err := f.admin.Exec(ctx,
		`UPDATE backup_snapshots SET started_at = now() - interval '2 hours', progress_updated_at = NULL
		  WHERE id = ANY($1)`, []uuid.UUID{withErr, plain, long}); err != nil {
		t.Fatalf("backdate started_at: %v", err)
	}

	svc := backup.NewService(f.repo, stubSiteLookup{info: enrolledSiteInfo(f.site, "https://gh791.example.com")},
		&stubEnqueuer{}, nil, domain.SystemClock{}, backup.Config{})
	w := backup.NewProgressWatchdogWorker(svc, time.Minute, 10*time.Minute, nil)
	if err := w.Work(ctx, &river.Job[backup.ProgressWatchdogArgs]{}); err != nil {
		t.Fatalf("ProgressWatchdogWorker.Work: %v", err)
	}

	p := f.snap(t, plain)
	if p.Status != "failed" || p.Error == "" || strings.Contains(p.Error, "Last error") {
		t.Fatalf("plain run: status=%q error=%q; want failed with the watchdog's message alone", p.Status, p.Error)
	}
	stallMsg := p.Error

	s := f.snap(t, withErr)
	want := stallMsg + ". Last error: " + gh791AttemptMsg
	if s.Status != "failed" || s.Error != want {
		t.Fatalf("run with attempt error: status=%q error=%q; want failed and %q", s.Status, s.Error, want)
	}

	l := f.snap(t, long)
	if l.Status != "failed" || len([]rune(l.Error)) != 1024 || !strings.HasPrefix(l.Error, stallMsg+". Last error: x") {
		t.Fatalf("run with capped attempt error: status=%q error length=%d; want failed, 1024 characters, the watchdog message first",
			l.Status, len([]rune(l.Error)))
	}
}
