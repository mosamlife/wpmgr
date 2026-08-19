package backup

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// GH #458 Go-layer contract tests.
//
// The SQL half (apps/api/db/query/backups.sql) added status preconditions to
// MarkBackupSnapshotRunning, CompleteBackupSnapshot and FailBackupSnapshot and
// moved all three from :one to :execrows, so rows-affected (0 or 1) — not
// pgx.ErrNoRows — is now the signal that a transition did or did not happen.
// tests/gh458 proves the predicates block in real Postgres. These tests prove
// the Go layer WIRES that zero-row contract: a guard that blocks the write but
// still fires the side effects is worse than no guard, because it reports a
// state change that did not happen.
//
// The fakes these use mirror the real guards exactly (see watchdogFakeRepo's
// MarkSnapshotRunning / FailSnapshot), so a fake can never report a transition
// the database would have refused.

// TestFailSnapshot_LateFailAgainstCompletedIsSilent is the primary Part 3
// proof. A worker error, an agent-reported failure or an operator cancel can
// all land after the snapshot already completed. The guarded UPDATE refuses to
// overwrite the completed row, and the service must then publish no 'failed'
// SSE event and send no failure email — announcing a failure for a backup that
// actually succeeded is the worst outcome available here.
func TestFailSnapshot_LateFailAgainstCompletedIsSilent(t *testing.T) {
	repo := newWatchdogFakeRepo()
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	repo.setSnapshot(Snapshot{
		ID: snapID, TenantID: tenantID, SiteID: siteID,
		Status: StatusCompleted, TotalSize: 4242, ChunkCount: 7,
	})

	mailer := &fakeMailer{}
	hub := NewHub()
	svc := newWatchdogTestService(repo, hub)
	svc.SetMailer(mailer)
	ch, unsub := hub.Subscribe(snapID)
	defer unsub()

	snap, transitioned, err := svc.FailSnapshot(context.Background(), tenantID, snapID, "worker error arriving late")
	if err != nil {
		t.Fatalf("FailSnapshot: unexpected error: %v", err)
	}
	if transitioned {
		t.Fatalf("transitioned = true, want false — a completed row must not be failable")
	}
	if snap.Status != "" {
		t.Fatalf("snapshot = %+v, want the zero Snapshot on a no-op transition", snap)
	}

	// The stored row is untouched: status, error and the recorded counters.
	got := repo.mustGet(t, snapID)
	if got.Status != StatusCompleted {
		t.Fatalf("status = %q, want %q — a late fail must not overwrite a completed run", got.Status, StatusCompleted)
	}
	if got.Error != "" {
		t.Fatalf("error = %q, want empty — a late fail must not stamp an error on a completed run", got.Error)
	}
	if got.TotalSize != 4242 || got.ChunkCount != 7 {
		t.Fatalf("total_size/chunk_count = %d/%d, want 4242/7 — the completed run's counters must survive", got.TotalSize, got.ChunkCount)
	}

	// And no side effect fired.
	if events := drainEvents(ch); len(events) != 0 {
		t.Fatalf("events = %+v, want none — a no-op fail must not publish 'failed'", events)
	}
	if n := mailer.callCount(); n != 0 {
		t.Fatalf("mailer calls = %d, want 0 — a no-op fail must not send a failure email", n)
	}
}

// TestFailSnapshot_GenuineFailStillFiresEverything is the over-fire check for
// the test above: a row that IS still running must fail exactly as before, with
// its 'failed' event and its failure email. A guard that reddens correct work
// gets switched off, and then it guards nothing.
func TestFailSnapshot_GenuineFailStillFiresEverything(t *testing.T) {
	repo := newWatchdogFakeRepo()
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	repo.setSnapshot(Snapshot{ID: snapID, TenantID: tenantID, SiteID: siteID, Status: StatusRunning})

	mailer := &fakeMailer{}
	hub := NewHub()
	svc := newWatchdogTestService(repo, hub)
	svc.SetMailer(mailer)
	ch, unsub := hub.Subscribe(snapID)
	defer unsub()

	snap, transitioned, err := svc.FailSnapshot(context.Background(), tenantID, snapID, "disk full")
	if err != nil {
		t.Fatalf("FailSnapshot: unexpected error: %v", err)
	}
	if !transitioned {
		t.Fatalf("transitioned = false, want true — a running row must still be failable")
	}
	if snap.Status != StatusFailed {
		t.Fatalf("returned status = %q, want %q", snap.Status, StatusFailed)
	}
	if got := repo.mustGet(t, snapID); got.Status != StatusFailed || got.Error != "disk full" {
		t.Fatalf("stored row = %+v, want failed with the real reason", got)
	}
	events := drainEvents(ch)
	if len(events) != 1 || events[0].Phase != "failed" {
		t.Fatalf("events = %+v, want exactly one 'failed' event", events)
	}
	if n := mailer.callCount(); n != 1 {
		t.Fatalf("mailer calls = %d, want exactly 1 — a genuine failure must still notify", n)
	}
}

// TestMarkRunning_LostClaimPublishesNothing covers the claim precondition. The
// worker reads the snapshot in one transaction and claims it in another, so the
// row can be cancelled or watchdog-failed in between. MarkBackupSnapshotRunning
// carries "AND status='pending'", so the claim returns 0 rows; the service must
// then publish no 'started' event and reconcile no schedule run, both of which
// would drag a terminal snapshot's UI back to "running".
func TestMarkRunning_LostClaimPublishesNothing(t *testing.T) {
	repo := newWatchdogFakeRepo()
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	repo.setSnapshot(Snapshot{
		ID: snapID, TenantID: tenantID, SiteID: siteID,
		Status: StatusFailed, Error: cancelByOperatorMsg,
	})

	hub := NewHub()
	svc := newWatchdogTestService(repo, hub)
	ch, unsub := hub.Subscribe(snapID)
	defer unsub()

	snap, claimed, err := svc.MarkRunning(context.Background(), tenantID, snapID)
	if err != nil {
		t.Fatalf("MarkRunning: unexpected error: %v", err)
	}
	if claimed {
		t.Fatalf("claimed = true, want false — a cancelled snapshot must not be claimable")
	}
	if snap.Status != "" {
		t.Fatalf("snapshot = %+v, want the zero Snapshot on a lost claim", snap)
	}
	if got := repo.mustGet(t, snapID); got.Status != StatusFailed || got.Error != cancelByOperatorMsg {
		t.Fatalf("stored row = %+v, want the cancel preserved — a claim must never revive a terminal row", got)
	}
	if events := drainEvents(ch); len(events) != 0 {
		t.Fatalf("events = %+v, want none — a lost claim must not publish 'started'", events)
	}
}

// TestMarkRunning_PendingClaimStillPublishesStarted is the over-fire check: the
// normal claim of a pending snapshot is unchanged.
func TestMarkRunning_PendingClaimStillPublishesStarted(t *testing.T) {
	repo := newWatchdogFakeRepo()
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	repo.setSnapshot(Snapshot{ID: snapID, TenantID: tenantID, SiteID: siteID, Status: StatusPending})

	hub := NewHub()
	svc := newWatchdogTestService(repo, hub)
	ch, unsub := hub.Subscribe(snapID)
	defer unsub()

	snap, claimed, err := svc.MarkRunning(context.Background(), tenantID, snapID)
	if err != nil {
		t.Fatalf("MarkRunning: unexpected error: %v", err)
	}
	if !claimed {
		t.Fatalf("claimed = false, want true — a pending snapshot must still be claimable")
	}
	if snap.Status != StatusRunning {
		t.Fatalf("returned status = %q, want %q", snap.Status, StatusRunning)
	}
	events := drainEvents(ch)
	if len(events) != 1 || events[0].Phase != "started" {
		t.Fatalf("events = %+v, want exactly one 'started' event", events)
	}
}

// TestMarkRunning_ClaimIsExactlyOnce proves the duplicate-job case: two workers
// racing the same snapshot, only one of which owns the run.
func TestMarkRunning_ClaimIsExactlyOnce(t *testing.T) {
	repo := newWatchdogFakeRepo()
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	repo.setSnapshot(Snapshot{ID: snapID, TenantID: tenantID, SiteID: siteID, Status: StatusPending})

	hub := NewHub()
	svc := newWatchdogTestService(repo, hub)
	ctx := context.Background()

	if _, claimed, err := svc.MarkRunning(ctx, tenantID, snapID); err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v, want true/nil", claimed, err)
	}
	if _, claimed, err := svc.MarkRunning(ctx, tenantID, snapID); err != nil || claimed {
		t.Fatalf("second claim: claimed=%v err=%v, want false/nil — the claim must be exactly-once", claimed, err)
	}
}

// racingCancelRepo makes GetSnapshot report a stale 'running' row while the
// stored row has already moved on. That is exactly the CancelSnapshot TOCTOU
// window: the pre-flight status check passes, and only the guarded UPDATE can
// tell the operator that there was nothing left to cancel.
type racingCancelRepo struct {
	*watchdogFakeRepo
	stale Snapshot
}

func (r *racingCancelRepo) GetSnapshot(_ context.Context, _, _ uuid.UUID) (Snapshot, error) {
	return r.stale, nil
}

// TestCancelSnapshot_LosesRaceSurfacesConflict is the TOCTOU proof. Before
// GH #458 the blind UPDATE overwrote whatever the row had become — a completed
// run's status and finished_at were replaced with "cancelled by operator" and
// the operator was told the cancel worked. Now the guard refuses, and the
// service surfaces the EXISTING snapshot_not_cancelable Conflict rather than a
// new error code: a lost race is indistinguishable to the operator from a
// snapshot that was already terminal when they clicked, which is what it is.
func TestCancelSnapshot_LosesRaceSurfacesConflict(t *testing.T) {
	inner := newWatchdogFakeRepo()
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	// The row really completed...
	inner.setSnapshot(Snapshot{
		ID: snapID, TenantID: tenantID, SiteID: siteID,
		Status: StatusCompleted, TotalSize: 999, ChunkCount: 3,
	})
	// ...but the operator's read still says it is running.
	repo := &racingCancelRepo{
		watchdogFakeRepo: inner,
		stale:            Snapshot{ID: snapID, TenantID: tenantID, SiteID: siteID, Status: StatusRunning},
	}

	mailer := &fakeMailer{}
	hub := NewHub()
	// Built inline rather than via newWatchdogTestService: that helper is typed
	// to *watchdogFakeRepo, and this test needs the racing wrapper.
	svc := NewService(repo, &fakeSiteLookup{}, nil, &fakePresigner{}, fakeClock{t: time.Now()}, Config{})
	svc.SetHub(hub)
	svc.SetMailer(mailer)
	ch, unsub := hub.Subscribe(snapID)
	defer unsub()

	_, err := svc.CancelSnapshot(context.Background(), tenantID, snapID)
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindConflict || de.Code != "snapshot_not_cancelable" {
		t.Fatalf("err = %v, want Conflict snapshot_not_cancelable", err)
	}

	got := inner.mustGet(t, snapID)
	if got.Status != StatusCompleted || got.Error != "" {
		t.Fatalf("stored row = %+v, want the completed run untouched by the lost cancel", got)
	}
	if got.TotalSize != 999 || got.ChunkCount != 3 {
		t.Fatalf("counters = %d/%d, want 999/3 — a lost cancel must not rewrite them", got.TotalSize, got.ChunkCount)
	}
	if events := drainEvents(ch); len(events) != 0 {
		t.Fatalf("events = %+v, want none — a lost cancel must not publish 'failed'", events)
	}
	if n := mailer.callCount(); n != 0 {
		t.Fatalf("mailer calls = %d, want 0 — a lost cancel must not email a failure", n)
	}
}

// TestCancelSnapshot_PendingStillCancelable is the non-negotiable over-fire
// check named in the GH #458 brief. FailBackupSnapshot's guard is
// IN ('pending','running') and not just 'running' precisely so operator cancel
// of a QUEUED backup keeps working. If this reddens, the guard was narrowed
// wrongly or the wiring is inverted.
func TestCancelSnapshot_PendingStillCancelable(t *testing.T) {
	repo := newWatchdogFakeRepo()
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	repo.setSnapshot(Snapshot{ID: snapID, TenantID: tenantID, SiteID: siteID, Status: StatusPending})

	hub := NewHub()
	svc := newWatchdogTestService(repo, hub)
	ch, unsub := hub.Subscribe(snapID)
	defer unsub()

	out, err := svc.CancelSnapshot(context.Background(), tenantID, snapID)
	if err != nil {
		t.Fatalf("CancelSnapshot(pending): unexpected error: %v — cancel of a QUEUED backup must keep working", err)
	}
	if out.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", out.Status, StatusFailed)
	}
	got := repo.mustGet(t, snapID)
	if got.Status != StatusFailed || got.Error != cancelByOperatorMsg {
		t.Fatalf("stored row = %+v, want failed with %q", got, cancelByOperatorMsg)
	}
	events := drainEvents(ch)
	if len(events) != 1 || events[0].Phase != "failed" {
		t.Fatalf("events = %+v, want exactly one 'failed' event for a real cancel", events)
	}
}

// TestCancelSnapshot_RunningStillCancelable is the other half of the over-fire
// check.
func TestCancelSnapshot_RunningStillCancelable(t *testing.T) {
	repo := newWatchdogFakeRepo()
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	repo.setSnapshot(Snapshot{ID: snapID, TenantID: tenantID, SiteID: siteID, Status: StatusRunning})

	svc := newWatchdogTestService(repo, NewHub())
	out, err := svc.CancelSnapshot(context.Background(), tenantID, snapID)
	if err != nil {
		t.Fatalf("CancelSnapshot(running): unexpected error: %v", err)
	}
	if out.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", out.Status, StatusFailed)
	}
}
