package backup

// restore_watchdog_test.go: GH #794. The progress watchdog fails queued and
// running restore runs that stopped reporting, so they stop blocking
// snapshot and organisation deletion. These are the fake-store halves: the
// threshold's default and floor, what one pass asks the store to do, and a
// restore job whose run already finished or cannot be read. The SQL guards
// are proved against Postgres in
// tests/restore_stall_watchdog_integration_test.go.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

func TestEffectiveRestoreStallTimeout_DefaultAndFloor(t *testing.T) {
	cases := []struct {
		name       string
		configured time.Duration
		presignTTL time.Duration
		want       time.Duration
		wantRaised bool
	}{
		{"unset uses the default", 0, time.Hour, 2 * time.Hour, false},
		{"negative uses the default", -5 * time.Minute, time.Hour, 2 * time.Hour, false},
		{"longer than the floor is kept", 3 * time.Hour, time.Hour, 3 * time.Hour, false},
		{"equal to the floor is kept", 2 * time.Hour, time.Hour, 2 * time.Hour, false},
		{"below the floor is raised", 30 * time.Minute, time.Hour, 2 * time.Hour, true},
		{"a short presign TTL lowers the floor", 90 * time.Minute, 30 * time.Minute, 90 * time.Minute, false},
		{"below a lower floor is still raised", time.Hour, 30 * time.Minute, 90 * time.Minute, true},
		{"a long presign TTL raises the default without a warning", 0, 6 * time.Hour, 7 * time.Hour, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, raised := effectiveRestoreStallTimeout(tc.configured, tc.presignTTL)
			if got != tc.want || raised != tc.wantRaised {
				t.Fatalf("effectiveRestoreStallTimeout(%v, %v) = (%v, %v), want (%v, %v)",
					tc.configured, tc.presignTTL, got, raised, tc.want, tc.wantRaised)
			}
		})
	}
}

func TestProgressWatchdog_RestoreStallTimeoutWiring(t *testing.T) {
	svc := newWatchdogTestService(newWatchdogFakeRepo(), NewHub())
	w := NewProgressWatchdogWorker(svc, time.Minute, time.Hour, nil)
	if w.restoreStall != 2*time.Hour {
		t.Fatalf("default restore stall timeout = %v, want 2h", w.restoreStall)
	}
	w.SetRestoreStallTimeout(5 * time.Hour)
	if w.restoreStall != 5*time.Hour {
		t.Fatalf("after SetRestoreStallTimeout(5h): %v, want 5h", w.restoreStall)
	}
	w.SetRestoreStallTimeout(10 * time.Minute)
	if w.restoreStall != 2*time.Hour {
		t.Fatalf("after SetRestoreStallTimeout(10m): %v, want it raised to 2h (presign TTL 1h plus 1h)", w.restoreStall)
	}
	w.SetRestoreStallTimeout(0)
	if w.restoreStall != 2*time.Hour {
		t.Fatalf("after SetRestoreStallTimeout(0): %v, want the 2h default", w.restoreStall)
	}

	longTTL := NewService(newWatchdogFakeRepo(), &fakeSiteLookup{}, nil, &fakePresigner{}, fakeClock{t: time.Now()}, Config{PresignTTL: 3 * time.Hour})
	if got := NewProgressWatchdogWorker(longTTL, time.Minute, time.Hour, nil).restoreStall; got != 4*time.Hour {
		t.Fatalf("default with a 3h presign TTL = %v, want 4h", got)
	}
}

// TestProgressWatchdog_FailsStalledRestoreRuns: one pass lists with the
// configured threshold and fails every listed run with the operator message,
// passing the same threshold so the store can repeat the guard. A run the
// store reports unchanged (it reported progress after the list) is skipped
// without an error.
func TestProgressWatchdog_FailsStalledRestoreRuns(t *testing.T) {
	stuck, moved := uuid.New(), uuid.New()
	tenantID := uuid.New()
	store := &fakeRestoreRunStore{
		stalled: []StalledRestoreRun{
			{ID: stuck, TenantID: tenantID, SiteID: uuid.New(), SnapshotID: uuid.New(), Status: RestoreStatusRunning},
			{ID: moved, TenantID: tenantID, SiteID: uuid.New(), SnapshotID: uuid.New(), Status: RestoreStatusQueued},
		},
		failResult: map[uuid.UUID]bool{stuck: true, moved: false},
	}
	svc := newWatchdogTestService(newWatchdogFakeRepo(), NewHub())
	svc.SetRestoreRunStore(store)
	w := NewProgressWatchdogWorker(svc, time.Minute, time.Hour, nil)
	w.SetRestoreStallTimeout(3 * time.Hour)

	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(store.stallLists) != 1 || store.stallLists[0] != 3*time.Hour {
		t.Fatalf("ListStalledRestoreRuns thresholds = %v, want one call with 3h", store.stallLists)
	}
	if len(store.failCalls) != 2 {
		t.Fatalf("FailStalledRestoreRun calls = %d, want 2 (one per listed run): %+v", len(store.failCalls), store.failCalls)
	}
	for i, want := range []uuid.UUID{stuck, moved} {
		c := store.failCalls[i]
		if c.RunID != want || c.TenantID != tenantID {
			t.Errorf("fail call %d = run %s tenant %s, want run %s tenant %s", i, c.RunID, c.TenantID, want, tenantID)
		}
		if c.StallAfter != 3*time.Hour {
			t.Errorf("fail call %d StallAfter = %v, want 3h", i, c.StallAfter)
		}
		if c.Message != restoreStallMessage {
			t.Errorf("fail call %d Message = %q, want %q", i, c.Message, restoreStallMessage)
		}
	}
	if len(store.statusCalls) != 0 {
		t.Errorf("the watchdog must fail runs only through the guarded fail, got MarkRestoreRunStatus calls %+v", store.statusCalls)
	}
}

// snapshotListErrorRepo fails the snapshot half of the watchdog pass.
type snapshotListErrorRepo struct{ *watchdogFakeRepo }

func (snapshotListErrorRepo) ListStalledRunningSnapshots(context.Context, time.Duration, time.Duration) ([]StalledSnapshot, error) {
	return nil, errors.New("snapshot list failed")
}

// TestProgressWatchdog_RestorePassRunsWhenSnapshotPassFails: an error
// listing stalled snapshots is returned, and the restore pass still runs.
func TestProgressWatchdog_RestorePassRunsWhenSnapshotPassFails(t *testing.T) {
	stuck := uuid.New()
	store := &fakeRestoreRunStore{
		stalled:    []StalledRestoreRun{{ID: stuck, TenantID: uuid.New(), Status: RestoreStatusRunning}},
		failResult: map[uuid.UUID]bool{stuck: true},
	}
	svc := NewService(snapshotListErrorRepo{newWatchdogFakeRepo()}, &fakeSiteLookup{}, nil, &fakePresigner{}, fakeClock{t: time.Now()}, Config{})
	svc.SetRestoreRunStore(store)

	err := NewProgressWatchdogWorker(svc, time.Minute, time.Hour, nil).Work(context.Background(), nil)
	if err == nil {
		t.Fatal("Work = nil, want the snapshot list error")
	}
	if len(store.failCalls) != 1 || store.failCalls[0].RunID != stuck {
		t.Fatalf("FailStalledRestoreRun calls = %+v, want one for the stuck run", store.failCalls)
	}
}

// countingRestoreCommander counts restore dispatches and accepts each one.
type countingRestoreCommander struct{ restores int }

func (c *countingRestoreCommander) Backup(context.Context, uuid.UUID, string, agentcmd.BackupRequest) (agentcmd.BackupResponse, error) {
	return agentcmd.BackupResponse{}, errors.New("backup not used by this test")
}

func (c *countingRestoreCommander) IncrementalBackup(context.Context, uuid.UUID, string, agentcmd.IncrementalBackupRequest) (agentcmd.BackupResponse, error) {
	return agentcmd.BackupResponse{}, errors.New("incremental backup not used by this test")
}

func (c *countingRestoreCommander) Restore(context.Context, uuid.UUID, string, agentcmd.RestoreRequest) (agentcmd.RestoreResponse, error) {
	c.restores++
	return agentcmd.RestoreResponse{OK: true}, nil
}

// TestRestoreWorker_FinishedRunIsNotDispatched: a restore job whose run is
// already finished (the watchdog failed it while the job waited) cancels
// without sending the restore. A run that is still queued or running is
// dispatched as before.
func TestRestoreWorker_FinishedRunIsNotDispatched(t *testing.T) {
	for _, tc := range []struct {
		status       string
		wantDispatch bool
	}{
		{RestoreStatusFailed, false},
		{RestoreStatusCompleted, false},
		{RestoreStatusRolledBack, false},
		{RestoreStatusQueued, true},
		{RestoreStatusRunning, true},
	} {
		t.Run(tc.status, func(t *testing.T) {
			_, runStore, tenantID, snapshotID, svc := newRestoreWorkerFixture(t)
			runID := uuid.New()
			runStore.stored = RestoreRun{ID: runID, TenantID: tenantID, SnapshotID: snapshotID, Status: tc.status}
			cmd := &countingRestoreCommander{}
			worker := NewRestoreWorker(svc, cmd, nil, nil, "https://cp.example.com", 0)

			job := &river.Job[RestoreArgs]{
				JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 1},
				Args:   RestoreArgs{TenantID: tenantID, SnapshotID: snapshotID, Full: true, RestoreRunID: runID},
			}
			err := worker.Work(context.Background(), job)
			if tc.wantDispatch {
				if err != nil {
					t.Fatalf("Work = %v, want nil for a %s run", err, tc.status)
				}
				if cmd.restores != 1 {
					t.Fatalf("restores sent = %d, want 1 for a %s run", cmd.restores, tc.status)
				}
				return
			}
			var cancel *river.JobCancelError
			if !errors.As(err, &cancel) {
				t.Fatalf("Work = %v, want a JobCancel for a %s run", err, tc.status)
			}
			if cmd.restores != 0 {
				t.Fatalf("restores sent = %d, want 0 for a %s run", cmd.restores, tc.status)
			}
		})
	}
}

// unreadableRunStore is fakeRestoreRunStore with a GetRestoreRun that fails,
// as it does while the database is briefly unavailable.
type unreadableRunStore struct {
	*fakeRestoreRunStore
	err error
}

func (s unreadableRunStore) GetRestoreRun(context.Context, uuid.UUID, uuid.UUID) (RestoreRun, error) {
	return RestoreRun{}, s.err
}

// TestRestoreWorker_UnreadableRunIsNotDispatched: a restore job that cannot
// read its run's status sends nothing to the site, because the run may
// already be finished and its snapshot no longer protected from deletion.
// The run is marked failed with the control plane's wording, and the job is
// cancelled with the read error recorded on it.
func TestRestoreWorker_UnreadableRunIsNotDispatched(t *testing.T) {
	_, runStore, tenantID, snapshotID, svc := newRestoreWorkerFixture(t)
	runID := uuid.New()
	readErr := errors.New("read restore run: conn closed")
	svc.SetRestoreRunStore(unreadableRunStore{fakeRestoreRunStore: runStore, err: readErr})
	cmd := &countingRestoreCommander{}

	err := NewRestoreWorker(svc, cmd, nil, nil, "https://cp.example.com", 0).Work(context.Background(), restoreJobForRun(tenantID, snapshotID, runID, 1, 1))
	if cmd.restores != 0 {
		t.Fatalf("restores sent = %d, want 0 when the run's status cannot be read", cmd.restores)
	}
	assertJobCancel(t, err)
	if !errors.Is(err, readErr) {
		t.Errorf("Work = %v, want the read error recorded on the cancelled job", err)
	}
	// The worker marks the run running before it reads it, then finishes it.
	if n := len(runStore.statusCalls); n != 2 {
		t.Fatalf("restore run status calls = %+v, want running then failed", runStore.statusCalls)
	}
	last := runStore.statusCalls[1]
	if last.RunID != runID || last.TenantID != tenantID {
		t.Errorf("final status call targets run %s tenant %s, want run %s tenant %s", last.RunID, last.TenantID, runID, tenantID)
	}
	if last.Status != RestoreStatusFailed || !last.SetFinished || last.SetStarted {
		t.Errorf("final status call = %+v, want failed with finished_at set and started_at untouched", last)
	}
	if last.Error != restorePlanFailedMessage {
		t.Errorf("restore run error = %q, want %q", last.Error, restorePlanFailedMessage)
	}
}
