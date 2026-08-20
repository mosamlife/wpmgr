package backup

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// raceLosingRepo reproduces the exact TOCTOU window RecordProgress sits in:
// UpdateSnapshotProgress reads and returns the row as it looked BEFORE another
// writer went terminal, and the row is only then flipped terminal — so the
// `snap` RecordProgress holds is stale by the time FailSnapshot's guarded
// UPDATE runs and reports transitioned == false.
//
// The winner in production is an operator cancel, a watchdog hard-fail, or a
// completion; StatusCompleted is used here because it is the case where a
// stale trailing publish is most obviously wrong (it would announce "failed"
// for a backup that actually succeeded).
type raceLosingRepo struct {
	*watchdogFakeRepo
	winner string // status the concurrent writer lands on the row
}

func (r *raceLosingRepo) UpdateSnapshotProgress(ctx context.Context, tenantID, snapshotID uuid.UUID, progress []byte) (Snapshot, error) {
	stale, err := r.watchdogFakeRepo.UpdateSnapshotProgress(ctx, tenantID, snapshotID, progress)
	if err != nil {
		return stale, err
	}
	// The concurrent writer commits here, after our read.
	won := stale
	won.Status = r.winner
	r.setSnapshot(won)
	// Return the pre-race row: this is what RecordProgress will hold.
	return stale, nil
}

func newLostRaceService(repo *raceLosingRepo, hub *Hub) *Service {
	svc := NewService(repo, &fakeSiteLookup{}, nil, &fakePresigner{}, fakeClock{t: time.Now()}, Config{})
	svc.SetHub(hub)
	return svc
}

// TestRecordProgress_FailedPhase_LostRace_PublishesNoEvent is the regression
// lock for the SSE half of GH #458: when FailSnapshot reports transitioned ==
// false, RecordProgress made no state change and the winner already published
// its own terminal event, so RecordProgress must publish NOTHING.
//
// Before the fix the transitioned == false case fell through to the trailing
// s.publish, which emitted phase="failed" carrying snap.Status — the status
// read BEFORE the race was lost. Subscribers therefore saw a "failed" event
// with a stale running status land after the real terminal event, which is
// precisely the stream regression the transitioned == true early return exists
// to prevent.
func TestRecordProgress_FailedPhase_LostRace_PublishesNoEvent(t *testing.T) {
	repo := &raceLosingRepo{watchdogFakeRepo: newWatchdogFakeRepo(), winner: StatusCompleted}
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	repo.setSnapshot(Snapshot{ID: snapID, TenantID: tenantID, SiteID: siteID, Status: StatusRunning})

	hub := NewHub()
	svc := newLostRaceService(repo, hub)
	ch, unsub := hub.Subscribe(snapID)
	defer unsub()

	if _, err := svc.RecordProgress(context.Background(), tenantID, snapID, "failed",
		map[string]any{"message": "php fatal error"}); err != nil {
		t.Fatalf("RecordProgress: %v", err)
	}

	// The winner's own terminal state must survive untouched.
	if got := repo.mustGet(t, snapID); got.Status != StatusCompleted {
		t.Fatalf("snapshot status = %q, want %q (the lost race must not overwrite the winner)", got.Status, StatusCompleted)
	}

	events := drainEvents(ch)
	if len(events) != 0 {
		t.Fatalf("events = %+v, want 0: RecordProgress lost the transition race, so it has nothing to announce; "+
			"the winner already published its own terminal event", events)
	}
}

// TestRecordProgress_FailedPhase_WonRace_PublishesExactlyOneTerminalEvent is
// the over-fire twin: a genuine agent-reported failure that DOES transition
// must still publish exactly one terminal event — the one FailSnapshot emits.
// A fix that simply stopped publishing on phase=="failed" would break this.
func TestRecordProgress_FailedPhase_WonRace_PublishesExactlyOneTerminalEvent(t *testing.T) {
	repo := newWatchdogFakeRepo()
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	repo.setSnapshot(Snapshot{ID: snapID, TenantID: tenantID, SiteID: siteID, Status: StatusRunning})

	hub := NewHub()
	svc := newWatchdogTestService(repo, hub)
	ch, unsub := hub.Subscribe(snapID)
	defer unsub()

	if _, err := svc.RecordProgress(context.Background(), tenantID, snapID, "failed",
		map[string]any{"message": "php fatal error"}); err != nil {
		t.Fatalf("RecordProgress: %v", err)
	}

	if got := repo.mustGet(t, snapID); got.Status != StatusFailed {
		t.Fatalf("snapshot status = %q, want %q", got.Status, StatusFailed)
	}

	events := drainEvents(ch)
	if len(events) != 1 {
		t.Fatalf("events = %+v, want exactly 1 terminal event", events)
	}
	if events[0].Phase != "failed" {
		t.Fatalf("events[0].Phase = %q, want failed", events[0].Phase)
	}
	if events[0].Status != StatusFailed {
		t.Fatalf("events[0].Status = %q, want %q (a terminal event must never carry the pre-failure status)",
			events[0].Status, StatusFailed)
	}
}
