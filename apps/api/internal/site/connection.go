package site

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ----------------------------------------------------------------------------
// Connection-state machine (Phase 5.7 — ADR-041)
//
// connection_state is the single source of truth for a site's agent connection.
// The legal-transition table below is enforced in code (the ConnectionService
// validates the source state before any write); the DB CHECK constraint is a
// belt-and-braces backstop, not the gate.
// ----------------------------------------------------------------------------

// ConnectionState is the lifecycle state of a site's agent connection.
type ConnectionState string

const (
	// StatePendingEnrollment — a pairing code is minted (site-bound) but no agent
	// has enrolled yet.
	StatePendingEnrollment ConnectionState = "pending_enrollment"
	// StateConnected — agent enrolled, heartbeat fresh (≤180s).
	StateConnected ConnectionState = "connected"
	// StateDegraded — heartbeat missed ≥180s and <360s (ADR-039 sweeper).
	StateDegraded ConnectionState = "degraded"
	// StateDisconnected — heartbeat missed ≥360s OR agent posted a last-will.
	StateDisconnected ConnectionState = "disconnected"
	// StateRevoked — operator explicitly disconnected the site from the dashboard.
	StateRevoked ConnectionState = "revoked"
	// StateArchived — terminal soft-delete; history preserved.
	StateArchived ConnectionState = "archived"
)

// Valid reports whether s is a known connection state.
func (s ConnectionState) Valid() bool {
	_, ok := legalTransitions[s]
	return ok
}

// legalTransitions is the connection state machine (ADR-041). A (from→to) pair
// absent here is rejected by the service before any DB write.
//
//	pending_enrollment → connected | archived
//	connected          → degraded | disconnected | revoked | archived
//	degraded           → connected | disconnected | revoked | archived
//	disconnected       → connected | pending_enrollment | revoked | archived
//	revoked            → pending_enrollment | archived
//	archived           → disconnected | pending_enrollment   (restore / re-enroll)
var legalTransitions = map[ConnectionState]map[ConnectionState]bool{
	StatePendingEnrollment: {StateConnected: true, StateArchived: true},
	StateConnected:         {StateDegraded: true, StateDisconnected: true, StateRevoked: true, StateArchived: true},
	StateDegraded:          {StateConnected: true, StateDisconnected: true, StateRevoked: true, StateArchived: true},
	StateDisconnected:      {StateConnected: true, StatePendingEnrollment: true, StateRevoked: true, StateArchived: true},
	StateRevoked:           {StatePendingEnrollment: true, StateArchived: true},
	StateArchived:          {StateDisconnected: true, StatePendingEnrollment: true},
}

// CanTransition reports whether from→to is a legal connection-state move.
// from == to is always allowed (idempotent no-op transitions, e.g. a heartbeat
// arriving while already connected).
func CanTransition(from, to ConnectionState) bool {
	if from == to {
		return true
	}
	return legalTransitions[from][to]
}

// ----------------------------------------------------------------------------
// SSE event envelope (ADR-038)
// ----------------------------------------------------------------------------

// Connection-event type strings carried in ConnectionEvent.Type.
const (
	EventSiteCreated      = "site.created"
	EventSiteEnrolled     = "site.enrolled"
	EventSiteHeartbeat    = "site.heartbeat"
	EventSiteStateChanged = "site.state_changed"
	EventSiteRevoked      = "site.revoked"
	EventSiteDisconnected = "site.disconnected"
	EventSiteArchived     = "site.archived"
	EventSiteRestored     = "site.restored"
)

// ConnectionEvent is the envelope published to the tenant SSE channel. ID is an
// app-minted ULID (monotonic per tenant) used for ?since= replay (ADR-038).
type ConnectionEvent struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	TenantID uuid.UUID      `json:"tenant_id"`
	SiteID   uuid.UUID      `json:"site_id"`
	TS       time.Time      `json:"ts"`
	Data     map[string]any `json:"data"`
}

// EventPublisher fans a connection event out to subscribers. The production
// implementation persists to site_events and emits a Postgres NOTIFY so every
// API instance can deliver to its local SSE streams (ADR-038). Implemented in
// Phase 3 (internal/site/events).
type EventPublisher interface {
	Publish(ctx context.Context, ev ConnectionEvent) error
}

// ----------------------------------------------------------------------------
// Service inputs / outputs
// ----------------------------------------------------------------------------

// MintEnrollmentInput creates a site row in pending_enrollment plus a site-bound
// single-use enrollment code (the site-first "Add site" flow).
type MintEnrollmentInput struct {
	TenantID  uuid.UUID
	CreatedBy uuid.UUID
	URL       string
	Name      string
	Tags      []string
}

// EnrollmentCode is a freshly minted code returned to the operator once.
type EnrollmentCode struct {
	SiteID    uuid.UUID
	Plaintext string // shown once, never stored
	ExpiresAt time.Time
}

// ConsumeEnrollmentInput is the agent's enroll payload (post pairing-code).
type ConsumeEnrollmentInput struct {
	CodeHash       string // sha256 of the presented code
	AgentPublicKey string
	SiteURL        string
	ConsumedFromIP string
	Meta           Metadata
}

// HeartbeatInput is a single agent heartbeat.
type HeartbeatInput struct {
	TenantID uuid.UUID
	SiteID   uuid.UUID
	Payload  map[string]any
}

// HeartbeatResult carries any pending instructions for the agent (e.g. revoke).
// RevokeToken, when set, is a short-lived Ed25519 JWT (aud=site_id, cmd="revoke")
// the agent MUST verify with the CP public key before acting on the revoke
// instruction — so a MITM cannot forge a destructive self-teardown over the
// (otherwise TLS-only) heartbeat response. (Phase 6 security review, finding B.)
type HeartbeatResult struct {
	Instructions []string // e.g. ["revoke"]
	RevokeToken  string   // signed proof for a "revoke" instruction; "" otherwise
}

// RevokeTokenMinter mints a short-lived signed command token authorizing a
// destructive agent instruction. Reuses the existing agentcmd Ed25519 JWT
// mechanism (ADR-031): Mint(now, aud=site_id, cmd="revoke"). nil = the CP has no
// signing key configured, in which case no signed revoke is issued.
type RevokeTokenMinter interface {
	Mint(now time.Time, aud, cmd string) (token string, jti string, err error)
}

// ActorSiteInput is a tenant-scoped, operator-initiated action on one site.
type ActorSiteInput struct {
	TenantID uuid.UUID
	SiteID   uuid.UUID
	ActorID  uuid.UUID
	Reason   string
}

// ----------------------------------------------------------------------------
// ConnectionService — single owner of every connection-state transition.
//
// Each mutating method: (1) validates the source state via CanTransition,
// (2) writes the new state + a site_connection_history row + a hash-chained
// audit entry in ONE transaction, (3) publishes the SSE event AFTER commit.
// Concrete implementation + sqlc wiring land in Phase 3.
// ----------------------------------------------------------------------------

type ConnectionService interface {
	// MintEnrollmentCode creates a pending_enrollment site + a site-bound code.
	MintEnrollmentCode(ctx context.Context, in MintEnrollmentInput) (EnrollmentCode, error)

	// ConsumeEnrollmentCode atomically consumes a code and transitions the bound
	// site pending_enrollment→connected, storing the agent key and bumping
	// connection_generation. Exactly one concurrent caller wins.
	ConsumeEnrollmentCode(ctx context.Context, in ConsumeEnrollmentInput) (Site, error)

	// RecordHeartbeat refreshes last_seen_at and recovers degraded/disconnected→
	// connected. Returns pending agent instructions (e.g. a queued revoke).
	RecordHeartbeat(ctx context.Context, in HeartbeatInput) (HeartbeatResult, error)

	// MarkDegraded / MarkDisconnected are the timeout-sweeper transitions
	// (ADR-039). The sweeper is their ONLY caller.
	MarkDegraded(ctx context.Context, siteID uuid.UUID) error
	MarkDisconnected(ctx context.Context, siteID uuid.UUID, reason string) error

	// RecordLastWill handles a signed agent disconnect (deactivate/uninstall) —
	// connected/degraded→disconnected with the supplied reason (ADR-040).
	RecordLastWill(ctx context.Context, siteID uuid.UUID, reason string) error

	// Revoke (operator) → revoked, queues an agent revoke instruction, and nulls
	// agent_public_key so a later re-enroll can't collide on the unique index.
	Revoke(ctx context.Context, in ActorSiteInput) (Site, error)

	// Archive → terminal soft-delete (hidden from the default list).
	Archive(ctx context.Context, in ActorSiteInput) error

	// Restore un-archives a site back to disconnected.
	Restore(ctx context.Context, in ActorSiteInput) (Site, error)

	// BeginReEnrollment mints a fresh code bound to an existing revoked/
	// disconnected site, moving it back to pending_enrollment (generation bumps
	// on the next consume).
	BeginReEnrollment(ctx context.Context, in ActorSiteInput) (EnrollmentCode, error)
}
