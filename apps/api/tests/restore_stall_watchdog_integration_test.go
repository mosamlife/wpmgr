// restore_stall_watchdog_integration_test.go: GH #794. A queued or running
// restore run that stopped reporting blocks snapshot deletion and
// organisation deletion. One pass of the PRODUCTION backup progress watchdog
// fails it, after which both deletes go through. Runs against a real
// Postgres 16 (testcontainers) through backup.NewRestoreRunRepo(pool), the
// store the production watchdog uses, as wpmgr_app under RLS.
package tests

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

const restoreStallOperatorMessage = "The restore stopped reporting progress and was marked failed. Check the site, then start the restore again from the backup."

// requireAppRole fails the test unless pool connects as wpmgr_app without
// superuser or BYPASSRLS, so the RLS policies are live for every query.
func requireAppRole(t *testing.T, pool *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
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
}

// seedRestoreRunAt creates a restore run for snapshotID with the given status
// and moves its updated_at back by age, through the production repo and the
// tenant transaction.
func seedRestoreRunAt(t *testing.T, pool *db.Pool, runs *backup.RestoreRunRepo, tenantID, siteID, snapshotID uuid.UUID, status string, age time.Duration) backup.RestoreRun {
	t.Helper()
	ctx := context.Background()
	run, err := runs.CreateRestoreRun(ctx, backup.CreateRestoreRunInput{
		TenantID: tenantID, SiteID: siteID, SnapshotID: snapshotID, Mode: "full",
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
	if err := pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE restore_runs SET updated_at = now() - make_interval(secs => $2) WHERE id = $1 AND tenant_id = $3`,
			run.ID, age.Seconds(), tenantID)
		if err == nil && tag.RowsAffected() != 1 {
			t.Fatalf("backdate restore run %s: %d rows, want 1", run.ID, tag.RowsAffected())
		}
		return err
	}); err != nil {
		t.Fatalf("backdate restore run: %v", err)
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

func mustGetRestoreRun(t *testing.T, runs *backup.RestoreRunRepo, tenantID, runID uuid.UUID) backup.RestoreRun {
	t.Helper()
	got, err := runs.GetRestoreRun(context.Background(), tenantID, runID)
	if err != nil {
		t.Fatalf("read restore run %s: %v", runID, err)
	}
	return got
}

// failedEventCount counts the failed events on a run that carry the
// watchdog's message.
func failedEventCount(t *testing.T, runs *backup.RestoreRunRepo, tenantID, runID uuid.UUID) int {
	t.Helper()
	events, err := runs.ListRestoreEvents(context.Background(), tenantID, runID, 0, 100)
	if err != nil {
		t.Fatalf("list restore events: %v", err)
	}
	n := 0
	for _, ev := range events {
		if ev.Phase == "failed" && ev.Status == "failed" && ev.Message == restoreStallOperatorMessage {
			n++
		}
	}
	return n
}

// TestRestoreStallWatchdog_FailsStuckRunsAndUnblocksDeletes: a running run
// and a queued run, each without an update for three hours, refuse the
// organisation delete (409 restore_in_progress) and make the snapshot delete
// skip their snapshots. One watchdog pass fails both with the operator
// message, finished_at and a failed event, and both deletes then go through.
// A run updated five minutes ago and a completed run are left as they were,
// and the restored-from snapshot keeps its status.
func TestRestoreStallWatchdog_FailsStuckRunsAndUnblocksDeletes(t *testing.T) {
	pool := startPostgres(t)
	store := startBlobstore(t)
	admin := connectAdmin(t, pool)
	authSvc, rec := odNewAuthSvc(pool)
	ctx := context.Background()

	slug := "restore-stall-" + uuid.NewString()[:8]
	tenantA := seedTenant(t, pool, slug)
	requireAppRole(t, pool, tenantA)
	urlA := "https://restore-stall-a.example.com"
	siteA := seedSite(t, pool, tenantA, urlA)
	owner := seedUserRow(t, admin, "restore-stall-owner-"+uuid.NewString()[:8]+"@example.com")
	odSeedMembership(t, admin, owner, tenantA, "owner")
	home := seedTenant(t, pool, "restore-stall-home-"+uuid.NewString()[:8])
	odSeedMembership(t, admin, owner, home, "owner")

	tenantB := seedTenant(t, pool, "restore-stall-b-"+uuid.NewString()[:8])
	siteB := seedSite(t, pool, tenantB, "https://restore-stall-b.example.com")

	svc := newBackupService(t, pool, store, stubSiteLookup{info: enrolledSiteInfo(siteA, urlA)}, &stubEnqueuer{})
	runs := backup.NewRestoreRunRepo(pool)
	svc.SetRestoreRunStore(runs)

	snapRunning, snapQueued := uuid.New(), uuid.New()
	seedChainSnapshot(t, pool, tenantA, siteA, snapRunning, snapRunning, 0, 100)
	seedChainSnapshot(t, pool, tenantA, siteA, snapQueued, snapQueued, 0, 100)
	snapFresh, snapDone := uuid.New(), uuid.New()
	seedChainSnapshot(t, pool, tenantB, siteB, snapFresh, snapFresh, 0, 100)
	seedChainSnapshot(t, pool, tenantB, siteB, snapDone, snapDone, 0, 100)

	stuckRunning := seedRestoreRunAt(t, pool, runs, tenantA, siteA, snapRunning, backup.RestoreStatusRunning, 3*time.Hour)
	stuckQueued := seedRestoreRunAt(t, pool, runs, tenantA, siteA, snapQueued, backup.RestoreStatusQueued, 3*time.Hour)
	fresh := seedRestoreRunAt(t, pool, runs, tenantB, siteB, snapFresh, backup.RestoreStatusRunning, 5*time.Minute)
	done := seedRestoreRunAt(t, pool, runs, tenantB, siteB, snapDone, backup.RestoreStatusCompleted, 3*time.Hour)

	p := domain.Principal{Type: domain.PrincipalUser, UserID: owner, TenantID: home, Role: "owner", Scope: domain.ScopeOrg}
	engine := buildOrgEngine(t, pool, authSvc, rec, false, p)
	deleteOrg := func() int {
		w := odDo(engine, http.MethodDelete, "/api/v1/orgs/"+tenantA.String(), `{"confirm_name":"`+slug+`"}`)
		t.Logf("DELETE org: %d %s", w.Code, w.Body.String())
		return w.Code
	}

	// Before the pass: both stuck runs block both deletes.
	if code := deleteOrg(); code != http.StatusConflict {
		t.Fatalf("org delete with stuck restores = %d, want 409", code)
	}
	ids := []uuid.UUID{snapRunning, snapQueued}
	before, err := svc.BulkDeleteSnapshots(ctx, tenantA, siteA, ids, false)
	if err != nil {
		t.Fatalf("snapshot delete with stuck restores: %v", err)
	}
	if before.Deleted != 0 || before.Skipped != 2 {
		t.Fatalf("snapshot delete with stuck restores: deleted %d skipped %d, want 0 and 2", before.Deleted, before.Skipped)
	}
	for _, r := range before.Results {
		if r.Code != backup.SkipRestoreInProgress {
			t.Fatalf("snapshot %s skipped with %q, want %q", r.ID, r.Code, backup.SkipRestoreInProgress)
		}
	}

	// One pass of the production watchdog, with its default thresholds.
	if err := backup.NewProgressWatchdogWorker(svc, 0, 0, nil).Work(ctx, nil); err != nil {
		t.Fatalf("watchdog pass: %v", err)
	}

	for _, run := range []backup.RestoreRun{stuckRunning, stuckQueued} {
		got := mustGetRestoreRun(t, runs, tenantA, run.ID)
		if got.Status != backup.RestoreStatusFailed {
			t.Errorf("stuck %s run: status = %q, want failed", run.Status, got.Status)
		}
		if got.Error != restoreStallOperatorMessage {
			t.Errorf("stuck %s run: error = %q, want %q", run.Status, got.Error, restoreStallOperatorMessage)
		}
		if got.FinishedAt == nil {
			t.Errorf("stuck %s run: finished_at is not set", run.Status)
		}
		if n := failedEventCount(t, runs, tenantA, run.ID); n != 1 {
			t.Errorf("stuck %s run: failed events with the operator message = %d, want 1", run.Status, n)
		}
	}

	gotFresh := mustGetRestoreRun(t, runs, tenantB, fresh.ID)
	if gotFresh.Status != backup.RestoreStatusRunning || gotFresh.Error != "" || gotFresh.FinishedAt != nil {
		t.Errorf("run updated 5 minutes ago: status %q error %q finished_at %v, want it still running", gotFresh.Status, gotFresh.Error, gotFresh.FinishedAt)
	}
	if n := failedEventCount(t, runs, tenantB, fresh.ID); n != 0 {
		t.Errorf("run updated 5 minutes ago: failed events = %d, want 0", n)
	}
	gotDone := mustGetRestoreRun(t, runs, tenantB, done.ID)
	if gotDone.Status != backup.RestoreStatusCompleted || gotDone.Error != "" || !gotDone.UpdatedAt.Equal(done.UpdatedAt) {
		t.Errorf("completed run: status %q error %q updated_at %v, want it unchanged (completed, no error, %v)", gotDone.Status, gotDone.Error, gotDone.UpdatedAt, done.UpdatedAt)
	}

	for _, id := range ids {
		snap, _, err := svc.GetSnapshot(ctx, tenantA, id)
		if err != nil {
			t.Fatalf("read snapshot %s: %v", id, err)
		}
		if snap.Status != backup.StatusCompleted {
			t.Errorf("snapshot %s status = %q, want it still completed", id, snap.Status)
		}
	}

	// After the pass: both deletes go through.
	after, err := svc.BulkDeleteSnapshots(ctx, tenantA, siteA, ids, false)
	if err != nil {
		t.Fatalf("snapshot delete after the watchdog: %v", err)
	}
	if after.Deleted != 2 || after.Skipped != 0 {
		t.Fatalf("snapshot delete after the watchdog: deleted %d skipped %d, want 2 and 0 (%+v)", after.Deleted, after.Skipped, after.Results)
	}
	if code := deleteOrg(); code != http.StatusOK {
		t.Fatalf("org delete after the watchdog = %d, want 200", code)
	}
	if odTenantDeletedAt(t, admin, tenantA) == nil {
		t.Fatal("org delete answered 200 but the organisation is not marked deleted")
	}
}

// TestRestoreStallWatchdog_RunThatReportsAfterTheListIsNotFailed: a run that
// was listed as stalled and then reported progress, through the production
// progress write, is not failed by the fail that follows, and gains no
// failed event. A fail under another tenant does not reach it either.
func TestRestoreStallWatchdog_RunThatReportsAfterTheListIsNotFailed(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenantID := seedTenant(t, pool, "restore-stall-race-"+uuid.NewString()[:8])
	requireAppRole(t, pool, tenantID)
	other := seedTenant(t, pool, "restore-stall-other-"+uuid.NewString()[:8])
	siteID := seedSite(t, pool, tenantID, "https://restore-stall-race.example.com")
	runs := backup.NewRestoreRunRepo(pool)
	run := seedRestoreRunAt(t, pool, runs, tenantID, siteID, uuid.New(), backup.RestoreStatusRunning, 3*time.Hour)

	stalled, err := runs.ListStalledRestoreRuns(ctx, 2*time.Hour, 100)
	if err != nil {
		t.Fatalf("list stalled restore runs: %v", err)
	}
	listed := false
	for _, s := range stalled {
		if s.ID == run.ID && s.TenantID == tenantID && s.Status == backup.RestoreStatusRunning {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("a run without an update for 3h is not listed as stalled: %+v", stalled)
	}

	in := backup.FailStalledRestoreRunInput{TenantID: other, RunID: run.ID, StallAfter: 2 * time.Hour, Message: restoreStallOperatorMessage}
	if ok, err := runs.FailStalledRestoreRun(ctx, in); err != nil || ok {
		t.Fatalf("fail under another tenant = (%v, %v), want (false, nil)", ok, err)
	}

	// The agent reports a phase between the list and the fail.
	if err := runs.UpdateRestoreRunPhase(ctx, tenantID, run.ID, "download_artifacts"); err != nil {
		t.Fatalf("report progress: %v", err)
	}
	in.TenantID = tenantID
	ok, err := runs.FailStalledRestoreRun(ctx, in)
	if err != nil {
		t.Fatalf("fail after progress: %v", err)
	}
	if ok {
		t.Error("fail after progress reported a change, want none")
	}
	got := mustGetRestoreRun(t, runs, tenantID, run.ID)
	if got.Status != backup.RestoreStatusRunning || got.Error != "" || got.FinishedAt != nil {
		t.Errorf("run that reported progress: status %q error %q finished_at %v, want it still running", got.Status, got.Error, got.FinishedAt)
	}
	if n := failedEventCount(t, runs, tenantID, run.ID); n != 0 {
		t.Errorf("run that reported progress: failed events = %d, want 0", n)
	}
}
