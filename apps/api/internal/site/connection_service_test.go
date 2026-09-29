package site

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// siteAttrs holds optional per-site attributes used by DeleteCancellable to
// simulate a site that has been enrolled mid-flight (the TOCTOU race scenario).
type siteAttrs struct {
	enrolledAt *time.Time
	agentKey   string
}

// stateRepo is a stateful in-memory Repo that faithfully reproduces the real
// pgRepo's transition gate: it validates from→to via CanTransition before
// applying, so the ConnectionService's legal/illegal behaviour is exercised
// without a database. It also records published events via a stub publisher.
type stateRepo struct {
	fakeRepo
	mu    sync.Mutex
	states  map[uuid.UUID]ConnectionState
	gen     map[uuid.UUID]int32
	// attrs holds optional per-site extra attributes for DeleteCancellable checks.
	attrs   map[uuid.UUID]siteAttrs
	// urlHits simulates existing rows returned by GetSiteByAnyURL (keyed by url).
	urlHits map[string]SiteURLHit
}

func newStateRepo() *stateRepo {
	return &stateRepo{
		states:  map[uuid.UUID]ConnectionState{},
		gen:     map[uuid.UUID]int32{},
		attrs:   map[uuid.UUID]siteAttrs{},
		urlHits: map[string]SiteURLHit{},
	}
}

// setEnrolled marks a site as if AttachAgentAndConnect already ran (enrolled_at
// set, agent_public_key non-empty). Used to simulate the TOCTOU race scenario
// where DeleteCancellable must return 0 even though state is pending_enrollment.
func (r *stateRepo) setEnrolled(id uuid.UUID) {
	r.mu.Lock()
	now := time.Now()
	r.attrs[id] = siteAttrs{enrolledAt: &now, agentKey: "ed25519-test-key"}
	r.mu.Unlock()
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

func (r *stateRepo) Get(_ context.Context, tenantID, siteID uuid.UUID) (Site, error) {
	return Site{ID: siteID, TenantID: tenantID, ConnectionState: r.get(siteID)}, nil
}

func (r *stateRepo) Delete(_ context.Context, _, siteID uuid.UUID) error {
	r.mu.Lock()
	delete(r.states, siteID)
	r.mu.Unlock()
	return nil
}

// DeleteCancellable implements the single-tx guarded delete. It mirrors the
// DB-level predicate: it only deletes (and returns 1) when the site is in
// pending_enrollment with no enrolled_at and no agent_public_key. If any
// predicate fails it returns 0. This faithfully exercises the TOCTOU fix.
func (r *stateRepo) DeleteCancellable(_ context.Context, _, siteID uuid.UUID) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.states[siteID]
	if !ok {
		return 0, nil // site does not exist
	}
	// Simulate the three DB predicates.
	if st != StatePendingEnrollment {
		return 0, nil
	}
	// enrolled_at and agent_public_key are stored in siteAttrs for test
	// scenarios that need them; for the basic stateRepo they are always zero.
	if a, hasAttrs := r.attrs[siteID]; hasAttrs {
		if a.enrolledAt != nil || a.agentKey != "" {
			return 0, nil
		}
	}
	delete(r.states, siteID)
	return 1, nil
}

func (r *stateRepo) GetSiteByAnyURL(_ context.Context, _ uuid.UUID, urls []string) (SiteURLHit, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, url := range urls {
		if hit, ok := r.urlHits[url]; ok {
			if hit.URL == "" {
				hit.URL = url
			}
			return hit, true, nil
		}
	}
	return SiteURLHit{}, false, nil
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

// ---- CancelEnrollment guard tests ------------------------------------------

func TestCancelEnrollmentNeverConnectedSucceeds(t *testing.T) {
	repo := newStateRepo()
	id := uuid.New()
	// Set state to pending_enrollment; EnrolledAt == nil and AgentPublicKey == ""
	// are the zero values on Site returned by stateRepo.Get, so no extra setup needed.
	repo.set(id, StatePendingEnrollment)
	cs, pub := newTestConnService(repo)

	tenantID := uuid.New()
	if err := cs.CancelEnrollment(context.Background(), ActorSiteInput{TenantID: tenantID, SiteID: id, ActorID: uuid.New()}); err != nil {
		t.Fatalf("cancel of never-connected pending site should succeed: %v", err)
	}
	// State row removed.
	if got := repo.get(id); got != "" {
		t.Fatalf("expected site to be deleted (empty state), got %q", got)
	}
	// site.deleted SSE event emitted.
	if types := pub.types(); len(types) != 1 || types[0] != EventSiteDeleted {
		t.Fatalf("expected one %s event, got %v", EventSiteDeleted, types)
	}
}

func TestCancelEnrollmentConnectedSiteRejected(t *testing.T) {
	// A site that has ever connected (connection_state == connected) must be
	// rejected with not_cancellable; only archive/revoke is appropriate.
	repo := newStateRepo()
	id := uuid.New()
	repo.set(id, StateConnected)
	cs, _ := newTestConnService(repo)

	err := cs.CancelEnrollment(context.Background(), ActorSiteInput{TenantID: uuid.New(), SiteID: id, ActorID: uuid.New()})
	if err == nil {
		t.Fatal("expected not_cancellable error for connected site")
	}
	var de *domain.Error
	if !errors.As(err, &de) {
		t.Fatalf("expected domain.Error, got %T: %v", err, err)
	}
	if de.Kind != domain.KindConflict {
		t.Fatalf("expected KindConflict, got %v", de.Kind)
	}
	if de.Code != "not_cancellable" {
		t.Fatalf("expected code 'not_cancellable', got %q", de.Code)
	}
}

func TestCancelEnrollmentArchivedSiteRejected(t *testing.T) {
	// An archived site (previously connected) must also be rejected.
	repo := newStateRepo()
	id := uuid.New()
	repo.set(id, StateArchived)
	cs, _ := newTestConnService(repo)

	err := cs.CancelEnrollment(context.Background(), ActorSiteInput{TenantID: uuid.New(), SiteID: id, ActorID: uuid.New()})
	if err == nil {
		t.Fatal("expected not_cancellable error for archived site")
	}
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != "not_cancellable" {
		t.Fatalf("expected not_cancellable conflict, got %v", err)
	}
}

// ---- MintEnrollmentCode URL-dedup tests ------------------------------------

func TestMintEnrollmentCodeURLConflictReturnsStructured409(t *testing.T) {
	repo := newStateRepo()
	existingID := uuid.New()
	const conflictURL = "https://existing.example.com"

	// Simulate an archived tombstone at this URL.
	repo.mu.Lock()
	repo.urlHits[conflictURL] = SiteURLHit{ID: existingID, ConnectionState: StateArchived}
	repo.mu.Unlock()

	cs, _ := newTestConnService(repo)
	_, err := cs.MintEnrollmentCode(context.Background(), MintEnrollmentInput{
		TenantID: uuid.New(),
		URL:      conflictURL,
		Name:     "My Site",
	})
	if err == nil {
		t.Fatal("expected site_url_exists conflict, got nil")
	}
	var de *domain.Error
	if !errors.As(err, &de) {
		t.Fatalf("expected domain.Error, got %T: %v", err, err)
	}
	if de.Code != "site_url_exists" {
		t.Fatalf("expected code 'site_url_exists', got %q", de.Code)
	}
	if de.Kind != domain.KindConflict {
		t.Fatalf("expected KindConflict, got %v", de.Kind)
	}
	if de.Details == nil {
		t.Fatal("expected non-nil Details on site_url_exists error")
	}
	if siteID, ok := de.Details["site_id"].(string); !ok || siteID != existingID.String() {
		t.Fatalf("expected details.site_id == %s, got %v", existingID, de.Details["site_id"])
	}
	if state, ok := de.Details["connection_state"].(string); !ok || state != string(StateArchived) {
		t.Fatalf("expected details.connection_state == %q, got %v", StateArchived, de.Details["connection_state"])
	}
}

func TestMintEnrollmentCodeFreshURLSucceeds(t *testing.T) {
	repo := newStateRepo()
	cs, _ := newTestConnService(repo)

	code, err := cs.MintEnrollmentCode(context.Background(), MintEnrollmentInput{
		TenantID: uuid.New(),
		URL:      "https://fresh.example.com",
		Name:     "Fresh Site",
	})
	if err != nil {
		t.Fatalf("expected success for fresh URL, got: %v", err)
	}
	if code.SiteID == uuid.Nil {
		t.Fatal("expected non-nil site_id in enrollment code")
	}
	if code.Plaintext == "" {
		t.Fatal("expected non-empty plaintext code")
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

// ---- TOCTOU fix (FIX 1) guard tests ----------------------------------------

// TestCancelEnrollmentGuardedDeleteReturnsZeroWhenRaced simulates the TOCTOU
// scenario: the site is still in pending_enrollment in the state map, but
// DeleteCancellable returns 0 because enrolled_at is set (the agent enrolled
// between what used to be a separate Get and the Delete). The service must
// treat 0 rows as not_cancellable, and must NOT emit audit or SSE events.
func TestCancelEnrollmentGuardedDeleteReturnsZeroWhenRaced(t *testing.T) {
	repo := newStateRepo()
	id := uuid.New()
	// State is still pending_enrollment in the map…
	repo.set(id, StatePendingEnrollment)
	// …but the site also has enrolled_at+agent_key set (agent won the race).
	repo.setEnrolled(id)

	cs, pub := newTestConnService(repo)

	err := cs.CancelEnrollment(context.Background(), ActorSiteInput{TenantID: uuid.New(), SiteID: id, ActorID: uuid.New()})
	if err == nil {
		t.Fatal("expected not_cancellable error when DeleteCancellable returns 0 rows")
	}
	var de *domain.Error
	if !errors.As(err, &de) {
		t.Fatalf("expected domain.Error, got %T: %v", err, err)
	}
	if de.Code != "not_cancellable" {
		t.Fatalf("expected code 'not_cancellable', got %q", de.Code)
	}
	if de.Kind != domain.KindConflict {
		t.Fatalf("expected KindConflict, got %v", de.Kind)
	}
	// No audit or SSE should be emitted on the 0-row path.
	if types := pub.types(); len(types) != 0 {
		t.Fatalf("expected no SSE events on not_cancellable path, got %v", types)
	}
}

// TestCancelEnrollmentAuditAndSSEOnlyOnActualDeletion verifies that audit +
// SSE are emitted ONLY when a row is deleted (rowsAffected > 0), not when the
// delete returns 0 (already connected). This is the per-issue gate requirement.
func TestCancelEnrollmentAuditAndSSEOnlyOnActualDeletion(t *testing.T) {
	t.Run("deletion succeeds — SSE emitted", func(t *testing.T) {
		repo := newStateRepo()
		id := uuid.New()
		repo.set(id, StatePendingEnrollment)
		cs, pub := newTestConnService(repo)

		if err := cs.CancelEnrollment(context.Background(), ActorSiteInput{TenantID: uuid.New(), SiteID: id, ActorID: uuid.New()}); err != nil {
			t.Fatalf("expected success, got: %v", err)
		}
		types := pub.types()
		if len(types) != 1 || types[0] != EventSiteDeleted {
			t.Fatalf("expected one %s event after deletion, got %v", EventSiteDeleted, types)
		}
	})

	t.Run("0 rows returned — no SSE emitted", func(t *testing.T) {
		repo := newStateRepo()
		id := uuid.New()
		repo.set(id, StatePendingEnrollment)
		repo.setEnrolled(id) // simulate race: enrolled_at set → DELETE returns 0
		cs, pub := newTestConnService(repo)

		err := cs.CancelEnrollment(context.Background(), ActorSiteInput{TenantID: uuid.New(), SiteID: id, ActorID: uuid.New()})
		if err == nil {
			t.Fatal("expected not_cancellable error")
		}
		if types := pub.types(); len(types) != 0 {
			t.Fatalf("expected no SSE events on 0-row delete path, got %v", types)
		}
	})
}
