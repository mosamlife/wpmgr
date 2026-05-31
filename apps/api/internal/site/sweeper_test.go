package site

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// sweepRepo is a fake Repo whose ListToDegrade/ListToDisconnect return the refs
// the test seeds, modelling the partial-index selects against the cutoffs.
type sweepRepo struct {
	fakeRepo
	toDegrade    []SiteRef
	toDisconnect []SiteRef
	degCutoff    time.Time
	disCutoff    time.Time
}

func (r *sweepRepo) ListToDegrade(_ context.Context, cutoff time.Time) ([]SiteRef, error) {
	r.degCutoff = cutoff
	return r.toDegrade, nil
}

func (r *sweepRepo) ListToDisconnect(_ context.Context, cutoff time.Time) ([]SiteRef, error) {
	r.disCutoff = cutoff
	return r.toDisconnect, nil
}

// recordTransitioner records the degraded/disconnected transitions the sweeper
// drives and mutates a shared state map so a multi-pass walk is observable.
type recordTransitioner struct {
	states       map[uuid.UUID]ConnectionState
	degraded     []uuid.UUID
	disconnected []uuid.UUID
}

func (t *recordTransitioner) MarkDegradedTenant(_ context.Context, _, siteID uuid.UUID) error {
	t.degraded = append(t.degraded, siteID)
	if t.states != nil {
		t.states[siteID] = StateDegraded
	}
	return nil
}

func (t *recordTransitioner) MarkDisconnectedTenant(_ context.Context, _, siteID uuid.UUID, _ string) error {
	t.disconnected = append(t.disconnected, siteID)
	if t.states != nil {
		t.states[siteID] = StateDisconnected
	}
	return nil
}

func TestSweepDegradesAndDisconnects(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	degradeID := uuid.New()
	disconnectID := uuid.New()

	repo := &sweepRepo{
		toDegrade:    []SiteRef{{ID: degradeID, TenantID: uuid.New()}},
		toDisconnect: []SiteRef{{ID: disconnectID, TenantID: uuid.New()}},
	}
	tr := &recordTransitioner{states: map[uuid.UUID]ConnectionState{}}
	sw := NewSweeper(repo, tr, nil)

	deg, dis, err := sw.Sweep(context.Background(), now)
	if err != nil {
		t.Fatalf("sweep error: %v", err)
	}
	if deg != 1 || dis != 1 {
		t.Fatalf("expected (1 degraded, 1 disconnected), got (%d, %d)", deg, dis)
	}
	// Cutoffs must reflect the ADR-039 thresholds relative to now.
	if want := now.Add(-degradeAfter); !repo.degCutoff.Equal(want) {
		t.Fatalf("degrade cutoff = %v, want %v", repo.degCutoff, want)
	}
	if want := now.Add(-disconnectAfter); !repo.disCutoff.Equal(want) {
		t.Fatalf("disconnect cutoff = %v, want %v", repo.disCutoff, want)
	}
	if len(tr.degraded) != 1 || tr.degraded[0] != degradeID {
		t.Fatalf("expected degrade of %s, got %v", degradeID, tr.degraded)
	}
	if len(tr.disconnected) != 1 || tr.disconnected[0] != disconnectID {
		t.Fatalf("expected disconnect of %s, got %v", disconnectID, tr.disconnected)
	}
}

// TestSweepWalksConnectedToDisconnectedOverPasses proves a single stale site
// moves connected→degraded→disconnected across successive sweeps (never
// skipping a state): pass 1 only degrades it; once it is degraded and stays
// stale, a later pass disconnects it.
func TestSweepWalksConnectedToDisconnectedOverPasses(t *testing.T) {
	id := uuid.New()
	tenant := uuid.New()
	states := map[uuid.UUID]ConnectionState{id: StateConnected}
	tr := &recordTransitioner{states: states}

	// repo derives its lists from the shared state map + the supplied cutoffs,
	// modelling the partial-index selects: a connected stale site shows up in
	// ListToDegrade; once degraded it shows up in ListToDisconnect.
	repo := &stateBackedSweepRepo{states: states, id: id, tenant: tenant}
	sw := NewSweeper(repo, tr, nil)
	now := time.Now()

	// Pass 1: connected → degraded.
	if _, _, err := sw.Sweep(context.Background(), now); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if states[id] != StateDegraded {
		t.Fatalf("after pass 1 expected degraded, got %s", states[id])
	}

	// Pass 2: degraded → disconnected.
	if _, _, err := sw.Sweep(context.Background(), now); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if states[id] != StateDisconnected {
		t.Fatalf("after pass 2 expected disconnected, got %s", states[id])
	}
}

type stateBackedSweepRepo struct {
	fakeRepo
	states map[uuid.UUID]ConnectionState
	id     uuid.UUID
	tenant uuid.UUID
}

func (r *stateBackedSweepRepo) ListToDegrade(_ context.Context, _ time.Time) ([]SiteRef, error) {
	if r.states[r.id] == StateConnected {
		return []SiteRef{{ID: r.id, TenantID: r.tenant}}, nil
	}
	return nil, nil
}

func (r *stateBackedSweepRepo) ListToDisconnect(_ context.Context, _ time.Time) ([]SiteRef, error) {
	if r.states[r.id] == StateDegraded {
		return []SiteRef{{ID: r.id, TenantID: r.tenant}}, nil
	}
	return nil, nil
}
