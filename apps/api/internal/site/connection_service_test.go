package site

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// stateRepo is a stateful in-memory Repo that faithfully reproduces the real
// pgRepo's transition gate: it validates from→to via CanTransition before
// applying, so the ConnectionService's legal/illegal behaviour is exercised
// without a database. It also records published events via a stub publisher.
type stateRepo struct {
	fakeRepo
	mu     sync.Mutex
	states map[uuid.UUID]ConnectionState
	gen    map[uuid.UUID]int32
}

func newStateRepo() *stateRepo {
	return &stateRepo{states: map[uuid.UUID]ConnectionState{}, gen: map[uuid.UUID]int32{}}
}

func (r *stateRepo) set(id uuid.UUID, s ConnectionState) {
	r.mu.Lock()
	r.states[id] = s
	r.mu.Unlock()
}

func (r *stateRepo) get(id uuid.UUID) ConnectionState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.states[id]
}

func (r *stateRepo) Transition(_ context.Context, in TransitionInput) (TransitionResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	from := r.states[in.SiteID]
	if in.RequireFrom != "" && from != in.RequireFrom {
		return TransitionResult{}, domain.Conflict("illegal_transition",
			"site is "+string(from)+", expected "+string(in.RequireFrom))
	}
	if !CanTransition(from, in.To) {
		return TransitionResult{}, domain.Conflict("illegal_transition",
			"cannot transition site from "+string(from)+" to "+string(in.To))
	}
	if from != in.To {
		r.states[in.SiteID] = in.To
		if in.To == StatePendingEnrollment {
			r.gen[in.SiteID]++
		}
	}
	return TransitionResult{
		Site: Site{ID: in.SiteID, TenantID: in.TenantID, ConnectionState: in.To, ConnectionGeneration: r.gen[in.SiteID]},
		From: from,
	}, nil
}

func (r *stateRepo) Heartbeat(_ context.Context, tenantID, siteID uuid.UUID) (Site, error) {
	return Site{ID: siteID, TenantID: tenantID, ConnectionState: r.get(siteID)}, nil
}

func (r *stateRepo) ResolveTenant(_ context.Context, _ uuid.UUID) (uuid.UUID, error) {
	return uuid.New(), nil
}

// capturePub records published events for assertions.
type capturePub struct {
	mu     sync.Mutex
	events []ConnectionEvent
}

func (p *capturePub) Publish(_ context.Context, ev ConnectionEvent) error {
	p.mu.Lock()
	p.events = append(p.events, ev)
	p.mu.Unlock()
	return nil
}

func (p *capturePub) types() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.events))
	for i, e := range p.events {
		out[i] = e.Type
	}
	return out
}

func newTestConnService(repo Repo) (*connService, *capturePub) {
	pub := &capturePub{}
	cs := NewConnectionService(repo, domain.NewValidator(), nil, pub, domain.SystemClock{}, nil).(*connService)
	return cs, pub
}

func TestRevokeFromConnectedSucceeds(t *testing.T) {
	repo := newStateRepo()
	id := uuid.New()
	repo.set(id, StateConnected)
	cs, pub := newTestConnService(repo)

	if _, err := cs.Revoke(context.Background(), ActorSiteInput{TenantID: uuid.New(), SiteID: id, ActorID: uuid.New()}); err != nil {
		t.Fatalf("revoke from connected should succeed: %v", err)
	}
	if got := repo.get(id); got != StateRevoked {
		t.Fatalf("expected revoked, got %s", got)
	}
	if len(pub.types()) != 1 || pub.types()[0] != EventSiteRevoked {
		t.Fatalf("expected one %s event, got %v", EventSiteRevoked, pub.types())
	}
}

func TestIllegalTransitionsRejected(t *testing.T) {
	cases := []struct {
		name string
		from ConnectionState
		do   func(cs *connService, id uuid.UUID) error
	}{
		{"archive of already-archived is a no-op self", StateArchived, func(cs *connService, id uuid.UUID) error {
			return cs.Archive(context.Background(), ActorSiteInput{TenantID: uuid.New(), SiteID: id})
		}},
		{"restore of a connected site is illegal", StateConnected, func(cs *connService, id uuid.UUID) error {
			_, err := cs.Restore(context.Background(), ActorSiteInput{TenantID: uuid.New(), SiteID: id})
			return err
		}},
		{"re-enroll from connected is illegal", StateConnected, func(cs *connService, id uuid.UUID) error {
			_, err := cs.BeginReEnrollment(context.Background(), ActorSiteInput{TenantID: uuid.New(), SiteID: id})
			return err
		}},
		{"revoke from archived is illegal", StateArchived, func(cs *connService, id uuid.UUID) error {
			_, err := cs.Revoke(context.Background(), ActorSiteInput{TenantID: uuid.New(), SiteID: id})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := newStateRepo()
			id := uuid.New()
			repo.set(id, c.from)
			cs, _ := newTestConnService(repo)
			err := c.do(cs, id)
			// Restore-of-connected and the others should be conflicts; archive of
			// already-archived is a legal self-transition (no error).
			if c.name == "archive of already-archived is a no-op self" {
				if err != nil {
					t.Fatalf("self archive should be a no-op, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected illegal transition from %s to be rejected", c.from)
			}
			var de *domain.Error
			if !errors.As(err, &de) || de.Kind != domain.KindConflict {
				t.Fatalf("expected a conflict domain error, got %v", err)
			}
		})
	}
}

func TestHeartbeatRevokedReturnsInstruction(t *testing.T) {
	repo := newStateRepo()
	id := uuid.New()
	repo.set(id, StateRevoked)
	cs, _ := newTestConnService(repo)

	res, err := cs.RecordHeartbeat(context.Background(), HeartbeatInput{TenantID: uuid.New(), SiteID: id})
	if err != nil {
		t.Fatalf("heartbeat should not error: %v", err)
	}
	if len(res.Instructions) != 1 || res.Instructions[0] != "revoke" {
		t.Fatalf("expected [revoke] instruction, got %v", res.Instructions)
	}
}

func TestHeartbeatRecoversDegraded(t *testing.T) {
	repo := newStateRepo()
	id := uuid.New()
	repo.set(id, StateDegraded)
	cs, pub := newTestConnService(repo)

	if _, err := cs.RecordHeartbeat(context.Background(), HeartbeatInput{TenantID: uuid.New(), SiteID: id}); err != nil {
		t.Fatalf("heartbeat should not error: %v", err)
	}
	if got := repo.get(id); got != StateConnected {
		t.Fatalf("expected degraded site to recover to connected, got %s", got)
	}
	if len(pub.types()) != 1 || pub.types()[0] != EventSiteStateChanged {
		t.Fatalf("expected one state_changed event, got %v", pub.types())
	}
}

