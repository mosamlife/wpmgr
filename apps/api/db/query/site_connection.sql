-- M21 — Connection lifecycle (Phase 5.7, ADR-038/039/040/041).
--
-- These queries drive the ConnectionService state machine, the SSE event
-- journal, and the timeout sweeper. The state machine is enforced in Go
-- (site.CanTransition); the SQL here performs the load + the single-state
-- write inside one InTenantTx (or InEnrollTx for the pre-tenant consume path).

-- ---------------------------------------------------------------------------
-- Site load for a read-modify-write transition (tenant-scoped, FOR UPDATE).
-- ---------------------------------------------------------------------------

-- name: GetSiteForTransition :one
-- Loads a site under the tenant scope with a row lock so the load → validate →
-- write sequence is serialized against concurrent transitions on the same row.
SELECT * FROM sites
WHERE id = $1 AND tenant_id = $2
FOR UPDATE;

-- ---------------------------------------------------------------------------
-- Connection-state transitions. Each writes connection_state plus the derived
-- legacy status/health_status (ADR-041 keeps the legacy columns in sync), and
-- the relevant timestamp/reason/generation fields.
-- ---------------------------------------------------------------------------

-- name: MarkSiteConnected :one
-- pending_enrollment/degraded/disconnected → connected. Refreshes liveness and
-- clears the disconnected_at/reason set by a prior down transition. The legacy
-- status='active'/health_status='healthy' mirror the new connection_state.
UPDATE sites
SET connection_state   = 'connected',
    status             = 'active',
    health_status      = 'healthy',
    last_seen_at       = now(),
    disconnected_at    = NULL,
    disconnected_reason = NULL,
    updated_at         = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: MarkSiteDegraded :one
-- connected → degraded (timeout sweeper only). Legacy health_status mirrors it.
UPDATE sites
SET connection_state = 'degraded',
    health_status    = 'unreachable',
    updated_at       = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: MarkSiteDisconnected :one
-- degraded → disconnected (timeout sweeper) OR connected/degraded → disconnected
-- (signed agent last-will). Records disconnected_at + the reason.
UPDATE sites
SET connection_state    = 'disconnected',
    status              = 'disabled',
    health_status       = 'unreachable',
    disconnected_at     = now(),
    disconnected_reason = $3,
    updated_at          = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: MarkSiteRevoked :one
-- connected/degraded/disconnected → revoked (operator). The agent key is KEPT
-- (NOT nulled) so the agent can still authenticate its next heartbeat and
-- RECEIVE the signed revoke token to tear itself down (Phase 6 finding C). A
-- later re-enroll overwrites agent_public_key on the same row (no unique-index
-- collision), so keeping it is safe. The agent learns of the revoke on its next
-- heartbeat (derived instruction from connection_state='revoked').
UPDATE sites
SET connection_state    = 'revoked',
    status              = 'disabled',
    health_status       = 'unreachable',
    disconnected_at     = now(),
    disconnected_reason = $3,
    updated_at          = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: ArchiveSite :one
-- any-non-archived → archived (operator terminal soft-delete). Sets archived_at;
-- hidden from the default list (connection_state <> 'archived').
UPDATE sites
SET connection_state = 'archived',
    status           = 'disabled',
    archived_at      = now(),
    updated_at       = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: RestoreSite :one
-- archived → disconnected (operator un-archive). Clears archived_at.
UPDATE sites
SET connection_state = 'disconnected',
    status           = 'disabled',
    health_status    = 'unreachable',
    archived_at      = NULL,
    updated_at       = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: BeginSiteReEnrollment :one
-- revoked/disconnected/archived → pending_enrollment (operator). Bumps the
-- generation counter and clears archived_at so the row leaves the archived list.
-- The next consume increments nothing further — generation already advanced.
UPDATE sites
SET connection_state = 'pending_enrollment',
    status           = 'pending',
    archived_at      = NULL,
    connection_generation = connection_generation + 1,
    updated_at       = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: AttachAgentAndConnect :one
-- Enroll path (app.enroll GUC): the site-first consume transition. Stores the
-- agent key on the pre-existing pending_enrollment site and moves it to
-- connected in one statement. The generation was already advanced at re-enroll
-- mint time (BeginSiteReEnrollment), so we do not bump it here. Mirrors the
-- legacy AttachAgentToSite but driving connection_state.
UPDATE sites
SET agent_public_key = $3,
    connection_state = 'connected',
    status           = 'active',
    health_status    = 'healthy',
    enrolled_at      = now(),
    last_seen_at     = now(),
    disconnected_at  = NULL,
    disconnected_reason = NULL,
    wp_version       = $4,
    php_version      = $5,
    updated_at       = now()
-- Defense-in-depth (Phase 6 review, finding E): consume only from
-- 'pending_enrollment'. A code is bound to a site BeginReEnrollment already moved
-- to pending_enrollment, so this holds on the happy path; the guard stops a
-- stale-but-valid code from forcing a connected/degraded/revoked/archived site
-- back to 'connected' out of sequence (a loser yields ErrNoRows like an expired code).
WHERE id = $1 AND tenant_id = $2 AND connection_state = 'pending_enrollment'
RETURNING *;

-- name: CreatePendingSite :one
-- Site-first "Add site" flow (ADR-041): create the sites row in
-- pending_enrollment BEFORE the agent enrolls, so the enrollment code can be
-- bound to a real site_id and the dashboard can subscribe to it immediately.
INSERT INTO sites (tenant_id, url, name, status, connection_state, tags)
VALUES ($1, $2, $3, 'pending', 'pending_enrollment', $4)
RETURNING *;

-- ---------------------------------------------------------------------------
-- Heartbeat liveness (tenant-scoped). Returns the current connection_state so
-- the service can decide whether a recovery transition is needed and whether
-- to hand the agent a pending instruction.
-- ---------------------------------------------------------------------------

-- name: TouchSiteHeartbeat :one
-- Bumps last_seen_at and returns the post-update row. Does NOT change
-- connection_state (a recovery from degraded/disconnected is a separate,
-- audited transition the service performs explicitly).
UPDATE sites
SET last_seen_at = now(),
    updated_at   = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- ---------------------------------------------------------------------------
-- Timeout-sweeper selects (cross-tenant, app.agent GUC). Both scan the partial
-- idx_sites_last_seen index (connected/degraded only).
-- ---------------------------------------------------------------------------

-- name: GetSiteTenant :one
-- Resolves a site's tenant by id (cross-tenant, app.agent GUC). Used by the
-- tenant-less ConnectionService entry points (MarkDegraded/MarkDisconnected/
-- RecordLastWill) to recover the tenant scope before the tenant-scoped write.
SELECT tenant_id FROM sites WHERE id = $1;

-- name: ListSitesToDegrade :many
-- connected sites whose last heartbeat is older than the degrade cutoff.
SELECT id, tenant_id FROM sites
WHERE connection_state = 'connected'
  AND (last_seen_at IS NULL OR last_seen_at < $1);

-- name: ListSitesToDisconnect :many
-- degraded sites whose last heartbeat is older than the disconnect cutoff.
SELECT id, tenant_id FROM sites
WHERE connection_state = 'degraded'
  AND (last_seen_at IS NULL OR last_seen_at < $1);

-- ---------------------------------------------------------------------------
-- site_connection_history — append-only transition log (tenant-scoped).
-- ---------------------------------------------------------------------------

-- name: InsertConnectionHistory :one
INSERT INTO site_connection_history
    (tenant_id, site_id, from_state, to_state, reason, actor_user_id, generation, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ListConnectionHistory :many
-- Newest first; used by the per-site lifecycle timeline.
SELECT * FROM site_connection_history
WHERE site_id = $1 AND tenant_id = $2
ORDER BY occurred_at DESC
LIMIT $3 OFFSET $4;

-- ---------------------------------------------------------------------------
-- site_events — durable SSE journal (tenant-scoped insert/replay, cross-tenant
-- prune). The app mints the ULID event_id; NOTIFY carries only the id.
-- ---------------------------------------------------------------------------

-- name: InsertSiteEvent :one
INSERT INTO site_events (event_id, tenant_id, site_id, type, data)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetSiteEvent :one
-- Loads one event under the tenant scope (the LISTEN listener uses this after a
-- NOTIFY to fetch the body it must fan out).
SELECT * FROM site_events
WHERE event_id = $1 AND tenant_id = $2;

-- name: ReplaySiteEvents :many
-- Replays events after a client cursor (?since / Last-Event-ID). ULIDs sort
-- lexicographically, so event_id > $2 is monotonic-after.
SELECT * FROM site_events
WHERE tenant_id = $1 AND event_id > $2
ORDER BY event_id
LIMIT $3;

-- name: PruneSiteEvents :execrows
-- Ring-buffer prune: drop events older than the replay window (cross-tenant,
-- app.agent GUC). Bounds table growth.
DELETE FROM site_events
WHERE created_at < $1;
