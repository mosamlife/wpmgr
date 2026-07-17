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
	// EventSiteDeleted is published after a never-connected pending enrollment
	// is hard-deleted via CancelEnrollment. The web removes the row from the
	// sites list on receipt.
	EventSiteDeleted = "site.deleted"

	// Media Optimizer (ADR-043 §7) event types, published on the shared tenant
	// SSE bus and filtered by site_id. The frontend (Phase 5) must add these
	// strings to SITE_EVENT_TYPES in use-site-events.ts to receive them.
	EventMediaSyncStarted              = "media.sync.started"
	EventMediaSyncCompleted            = "media.sync.completed"
	EventMediaOptimizeStarted          = "media.optimize.started"
	EventMediaOptimizeProgress         = "media.optimize.progress"
	EventMediaOptimizeAssetDone        = "media.optimize.asset_done"
	EventMediaOptimizeCompleted        = "media.optimize.completed"
	EventMediaRestoreStarted           = "media.restore.started"
	EventMediaRestoreAssetDone         = "media.restore.asset_done"
	EventMediaRestoreCompleted         = "media.restore.completed"
	EventMediaDeleteOriginalsCompleted = "media.delete_originals.completed"
	EventMediaJobFailed                = "media.job.failed"
	EventMediaAssetDeleted             = "media.asset.deleted"

	// Performance Suite (ADR-046) event types, published on the shared tenant
	// SSE bus and filtered by site_id. The frontend must add these strings to
	// SITE_EVENT_TYPES in use-site-events.ts to receive them.
	EventRucssQueued    = "rucss.queued"
	EventRucssComputing = "rucss.computing"
	EventRucssCompleted = "rucss.completed"
	EventRucssFailed    = "rucss.failed"

	// Cache / perf-config lifecycle events (Phase 6). Emitted by the perf service
	// at the corresponding orchestration points.
	EventCacheEnabled          = "cache.enabled"
	EventCacheDisabled         = "cache.disabled"
	EventCachePurgeStarted     = "cache.purge.started"
	EventCachePurgeCompleted   = "cache.purge.completed"
	EventCachePreloadStarted   = "cache.preload.started"
	EventCachePreloadProgress  = "cache.preload.progress"
	EventCachePreloadCompleted = "cache.preload.completed"
	EventCacheStatsUpdated     = "cache.stats.updated"
	EventPerfConfigUpdated     = "perf.config.updated"
	EventDbCleanCompleted = "db.clean.completed"
	EventDbCleanStarted   = "db.clean.started"
	EventDbCleanProgress  = "db.clean.progress"
	EventDbCleanFailed    = "db.clean.failed"

	// DB scan events (M39 Phase 2) — synchronous read-only scan lifecycle.
	// db.scan.started   emitted before the CP sends the db_scan command.
	// db.scan.completed emitted after the synchronous ACK returns ok=true.
	// db.scan.failed    emitted on transport error, ok=false, or watchdog stall.
	EventDbScanStarted   = "db.scan.started"
	EventDbScanCompleted = "db.scan.completed"
	EventDbScanFailed    = "db.scan.failed"

	// DB table-action events (Phase 2.2) — per-table DDL operations.
	// db.table.action.completed emitted after a synchronous db_table_action ACK
	//   returns the full per-table results.
	// db.table.action.failed    emitted on transport error or top-level ok=false.
	EventDbTableActionCompleted = "db.table.action.completed"
	EventDbTableActionFailed    = "db.table.action.failed"

	// Orphan delete lifecycle events (P3.8). These mirror the db.clean.* naming
	// convention.
	// db.orphan.delete.started   — emitted synchronously by the CP handler before
	//   the db_orphan_delete command is sent to the agent.
	// db.orphan.delete.progress  — emitted by the CP on each intermediate progress
	//   POST (done=false) from the agent.
	// db.orphan.delete.completed — emitted by the CP on the final progress POST
	//   (done=true, no error state).
	// db.orphan.delete.failed    — emitted on agent transport error, ok=false ACK,
	//   or CP watchdog stall.
	EventDbOrphanDeleteStarted   = "db.orphan.delete.started"
	EventDbOrphanDeleteProgress  = "db.orphan.delete.progress"
	EventDbOrphanDeleteCompleted = "db.orphan.delete.completed"
	EventDbOrphanDeleteFailed    = "db.orphan.delete.failed"

	// Search-replace tool events (#188). Synchronous command — only two events:
	// db.search.replace.completed is emitted when ok=true (with counts).
	// db.search.replace.failed    is emitted on transport error or ok=false.
	EventDbSearchReplaceCompleted = "db.search.replace.completed"
	EventDbSearchReplaceFailed    = "db.search.replace.failed"

	// Font processing events (M55 — Phase 2 font subsetting). Published on the
	// shared tenant SSE bus and filtered by site_id. The frontend must add these
	// strings to SITE_EVENT_TYPES in use-site-events.ts to receive them.
	// font.queued     — a new font_results row was created (state=pending).
	// font.converting — the media-encoder has picked up the job (state=pending, in progress).
	// font.ready      — the full WOFF2 is available (state=ready).
	// font.subset     — the subset WOFF2 is also available (state=subset).
	// font.skipped    — the font was skipped (e.g. already in negative state).
	// font.failed     — permanent failure (state=negative; error_detail set).
	// font.completed  — the batch of font results for this build is complete.
	EventFontQueued     = "font.queued"
	EventFontConverting = "font.converting"
	EventFontReady      = "font.ready"
	EventFontSubset     = "font.subset"
	EventFontSkipped    = "font.skipped"
	EventFontFailed     = "font.failed"
	EventFontCompleted  = "font.completed"

	// Media Cleaner tool events (#190). Four lifecycle events:
	// media.clean.scan.completed  — scan page returned ok=true; includes candidate count.
	// media.clean.scan.failed     — transport error or ok=false on a scan request.
	// media.clean.isolate.done    — isolate completed ok=true; includes quarantined IDs.
	// media.clean.isolate.failed  — transport error or ok=false on an isolate request.
	// media.clean.restore.done    — restore completed ok=true; includes restored IDs.
	// media.clean.restore.failed  — transport error or ok=false on a restore request.
	// media.clean.delete.done     — delete completed ok=true; includes deleted IDs.
	// media.clean.delete.failed   — transport error or ok=false on a delete request.
	EventMediaCleanScanCompleted    = "media.clean.scan.completed"
	EventMediaCleanScanFailed       = "media.clean.scan.failed"
	EventMediaCleanIsolateDone      = "media.clean.isolate.done"
	EventMediaCleanIsolateFailed    = "media.clean.isolate.failed"
	EventMediaCleanRestoreDone      = "media.clean.restore.done"
	EventMediaCleanRestoreFailed    = "media.clean.restore.failed"
	EventMediaCleanDeleteDone       = "media.clean.delete.done"
	EventMediaCleanDeleteFailed     = "media.clean.delete.failed"

	// RUM events (M56). Emitted by the RUM rollup worker after a site's hourly
	// rollups have been updated. The payload carries the site_id and the
	// bucket_hour that was folded so the dashboard can refetch only the
	// affected time window.
	// rum.rollup_updated — throttled emit (at most once per 30s per site) after
	//                      the rollup worker folds raw events into hourly buckets.
	EventRumRollupUpdated = "rum.rollup_updated"

	// Email events (m59 Phase 4 SSE). Published on the shared tenant SSE bus
	// and filtered by site_id. The frontend useEmailEvents hook must include
	// these strings in the event type filter to receive them.
	//
	// email.log_ingested      — emitted from the agent log-ingest handler after a
	//                           batch of email log entries lands; throttled to at
	//                           most once per LogIngestedThrottle per site.
	//                           Payload: {"site_id": "<uuid>", "count": <int>}
	//
	// email.suppression_updated — emitted when a webhook writes a suppression row
	//                             OR when an operator manually adds/deletes a
	//                             suppression. Payload:
	//                             {"site_id": "<uuid|null>", "email": "<masked>",
	//                              "reason": "<hard_bounce|complaint|unsubscribe|manual>"}
	//
	// email.bounce            — emitted when a webhook flips a site_email_log row
	//                           to bounced or complained. Payload:
	//                           {"site_id": "<uuid>", "message_id": "<string>",
	//                            "status": "<bounced|complained>"}
	EventEmailLogIngested        = "email.log_ingested"
	EventEmailSuppressionUpdated = "email.suppression_updated"
	EventEmailBounce             = "email.bounce"
	// email.config_propagated — emitted after an org-config propagation fan-out.
	// SiteID=uuid.Nil (NULL) → tenant-wide fan-out to all active streams.
	// Payload: {"synced": int, "failed": int, "total": int}
	EventEmailConfigPropagated = "email.config_propagated"

	// report.completed — emitted when a generated report reaches a terminal
	// state (completed or failed). SiteID=uuid.Nil (NULL) → tenant-wide fan-out.
	// Payload: {"report_id": string, "client_id": string, "status": string}
	EventReportCompleted = "report.completed"

	// Object Cache events (M68). Published on the shared tenant SSE bus and
	// filtered by site_id. The frontend must add these strings to SITE_EVENT_TYPES
	// in use-site-events.ts to receive them.
	//
	// objectcache.status_changed -- published IMMEDIATELY on any state transition
	//   (connected->degraded, degraded->down, down->connected, etc.) detected
	//   during heartbeat ingest. Transition events bypass the 10s debounce.
	//   Payload: {"from_state": string, "to_state": string,
	//             "latency_ms": int, "last_error_class": string}
	//
	// objectcache.stats_updated -- published at most once per heartbeat for
	//   non-transition updates (latency/memory drift). Subject to the 10s badge-
	//   debounce throttle on the web side.
	//   Payload: {"state": string, "latency_ms": int,
	//             "used_memory_bytes": int64, "hit_ratio_pct": float}
	//
	// objectcache.flushed -- published after a successful objectcache.flush
	//   command completes. Payload: {"scope": string, "strategy": string,
	//   "keys_deleted": int64, "actor_id": string}
	//
	// objectcache.config_applied -- published after a successful
	//   objectcache.apply_config command. Payload: {"config_hash": string}
	//
	// objectcache.test_completed -- published after objectcache.test returns
	//   from the agent. Payload: {"ok": bool, "config_hash": string,
	//   "latency_ms": float, "eviction_policy": string}
	EventObjectCacheStatusChanged  = "objectcache.status_changed"
	EventObjectCacheStatsUpdated   = "objectcache.stats_updated"
	EventObjectCacheFlushed        = "objectcache.flushed"
	EventObjectCacheConfigApplied  = "objectcache.config_applied"
	EventObjectCacheTestCompleted  = "objectcache.test_completed"
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
//
// Tags carries the same cap as CreatePairingCodeInput.Tags (m100 follow-up,
// GH #230 security review): this flow's CreatePending write (step 1, its own
// committed transaction) used to run BEFORE any tag validation, so an
// over-length tag could 500 out of MintSiteBoundCode (step 2) and strand an
// orphaned pending_enrollment site with no code. connService.MintEnrollmentCode
// now validates this struct FIRST, before any write.
type MintEnrollmentInput struct {
	TenantID  uuid.UUID
	CreatedBy uuid.UUID
	URL       string
	Name      string
	Tags      []string `validate:"max=50,dive,min=1,max=64"`
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

	// CancelEnrollment hard-deletes a site that is in pending_enrollment AND has
	// never connected (enrolled_at IS NULL, agent_public_key empty). This is the
	// "Cancel" action in the enrollment modal. If the site has ever connected or
	// an agent key is present the call returns a 409 with code "not_cancellable"
	// — archive/revoke must be used instead.
	CancelEnrollment(ctx context.Context, in ActorSiteInput) error

	// SetOnEnrollHook wires a post-enroll callback (e.g. screenshot capture trigger).
	// Must be called before the server starts accepting requests. The hook fires
	// best-effort in a goroutine after the enrollment state transition commits.
	// Passing nil is a no-op.
	SetOnEnrollHook(hook OnEnrollHook)
}
