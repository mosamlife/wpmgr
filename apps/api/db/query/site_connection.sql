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
--
-- url is optional. NULL keeps the stored address. A non-NULL value is written
-- only when no other site in the same tenant already holds it, so an address
-- conflict leaves the stored url in place and the enrollment still succeeds
-- instead of failing on sites_tenant_id_url_key. The caller learns whether the
-- address was adopted by comparing the returned url with the one it passed.
-- Deciding WHICH address may be passed is the caller's job, not this query's.
UPDATE sites
SET agent_public_key = @agent_public_key,
    connection_state = 'connected',
    status           = 'active',
    health_status    = 'healthy',
    enrolled_at      = now(),
    last_seen_at     = now(),
    disconnected_at  = NULL,
    disconnected_reason = NULL,
    wp_version       = @wp_version,
    php_version      = @php_version,
    url              = CASE
        WHEN sqlc.narg('url')::text IS NOT NULL
         AND NOT EXISTS (
             SELECT 1 FROM sites AS other
             WHERE other.tenant_id = sites.tenant_id
               AND other.url = sqlc.narg('url')::text
               AND other.id <> sites.id
         )
        THEN sqlc.narg('url')::text
        ELSE url
    END,
    updated_at       = now()
-- Defense-in-depth (Phase 6 review, finding E): consume only from
-- 'pending_enrollment'. A code is bound to a site BeginReEnrollment already moved
-- to pending_enrollment, so this holds on the happy path; the guard stops a
-- stale-but-valid code from forcing a connected/degraded/revoked/archived site
-- back to 'connected' out of sequence (a loser yields ErrNoRows like an expired code).
WHERE sites.id = @id AND sites.tenant_id = @tenant_id
  AND sites.connection_state = 'pending_enrollment'
RETURNING *;

-- name: AdoptSiteURL :one
-- Replaces a site's stored address with a new one, compare-and-set on the
-- address the caller last read. Matches only when the site is still in the
-- tenant, still holds "from", and is in an enrolled state (connected, degraded
-- or disconnected); a pending, revoked or archived site is never re-addressed
-- here. A non-matching site, a changed address, or another site in the same
-- tenant already holding "to" all yield pgx.ErrNoRows and leave the row as it
-- was. The NOT EXISTS guard sees only rows the caller's RLS scope can see, and
-- it cannot see a concurrent uncommitted write, so sites_tenant_id_url_key
-- remains the authority: a caller must also treat a unique violation on that
-- index as "address in use". Returns the url now stored.
-- "to" and "from" are reserved words, so they are bound with sqlc.arg('...')
-- rather than the @name shorthand, which does not parse for them.
UPDATE sites
SET url = sqlc.arg('to')::text, updated_at = now()
WHERE sites.id = @id AND sites.tenant_id = @tenant_id
  AND sites.url = sqlc.arg('from')::text
  AND sites.connection_state IN ('connected', 'degraded', 'disconnected')
  AND NOT EXISTS (
      SELECT 1 FROM sites o
      WHERE o.tenant_id = sites.tenant_id
        AND o.url = sqlc.arg('to')::text
        AND o.id <> sites.id
  )
RETURNING url;

-- name: CreatePendingSite :one
-- Site-first "Add site" flow (ADR-041): create the sites row in
-- pending_enrollment BEFORE the agent enrolls, so the enrollment code can be
-- bound to a real site_id and the dashboard can subscribe to it immediately.
INSERT INTO sites (tenant_id, url, name, status, connection_state, tags)
VALUES ($1, $2, $3, 'pending', 'pending_enrollment', $4)
RETURNING *;

-- name: GetSiteByURLForMint :one
-- URL-dedup check before MintEnrollmentCode. Tenant-scoped; includes ALL states
-- (archived, pending, etc.) so that a tombstone from a previously-cancelled or
-- archived site is visible and the caller can return a structured 409 with
-- site_id + connection_state instead of hitting the unique-index violation.
SELECT id, connection_state FROM sites
WHERE tenant_id = $1 AND url = $2
LIMIT 1;

-- name: GetSiteByAnyURL :one
-- Variant-aware URL-dedup check before MintEnrollmentCode. The caller passes
-- every spelling it treats as the same site (for example with and without a
-- leading "www.", http and https), in priority order. Tenant-scoped and, like
-- GetSiteByURLForMint, includes ALL states so the caller can answer a
-- structured 409. When several variants exist, the one listed first in urls
-- wins, so passing the exact URL first reports an exact match ahead of a
-- variant. Served by sites_tenant_id_url_key.
SELECT id, url, connection_state FROM sites
WHERE tenant_id = @tenant_id AND url = ANY(@urls::text[])
ORDER BY array_position(@urls::text[], url), id
LIMIT 1;

-- ---------------------------------------------------------------------------
-- Heartbeat liveness (tenant-scoped). Returns the current connection_state so
-- the service can decide whether a recovery transition is needed and whether
-- to hand the agent a pending instruction.
-- ---------------------------------------------------------------------------

-- name: TouchSiteHeartbeat :one
-- Bumps last_seen_at, resets the consecutive-miss counter (M58 hysteresis),
-- and returns the post-update row. Does NOT change connection_state (a
-- recovery from degraded/disconnected is a separate, audited transition the
-- service performs explicitly).
UPDATE sites
SET last_seen_at      = now(),
    missed_heartbeats = 0,
    updated_at        = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: IncrementSiteMissedHeartbeats :one
-- Increments the consecutive-miss counter for a connected site (cross-tenant,
-- app.agent GUC). The sweeper calls this on each overdue evaluation pass
-- instead of immediately degrading. Returns the updated missed_heartbeats
-- value so the caller can decide whether the threshold has been reached.
UPDATE sites
SET missed_heartbeats = missed_heartbeats + 1,
    updated_at        = now()
WHERE id = @id AND tenant_id = @tenant_id
RETURNING missed_heartbeats;

-- name: ResetSiteMissedHeartbeats :exec
-- Resets the consecutive-miss counter to 0 (cross-tenant, app.agent GUC).
-- Called by the heartbeat recovery path (RecordHeartbeat) in addition to the
-- existing TouchSiteHeartbeat reset, so the counter is cleared regardless of
-- which code path handles the recovery.
UPDATE sites
SET missed_heartbeats = 0,
    updated_at        = now()
WHERE id = @id AND tenant_id = @tenant_id;

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
-- url is included so the active-verify sweeper can dial the agent without a
-- secondary tenant-scoped lookup.
SELECT id, tenant_id, url FROM sites
WHERE connection_state = 'connected'
  AND (last_seen_at IS NULL OR last_seen_at < $1);

-- name: ListSitesToDisconnect :many
-- degraded sites whose last heartbeat is older than the disconnect cutoff.
-- url is included so the active-verify sweeper can dial the agent without a
-- secondary tenant-scoped lookup.
SELECT id, tenant_id, url FROM sites
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

-- name: DeleteCancellableSite :execrows
-- Hard-delete a site that has NEVER connected: the delete is conditional on
-- all three never-connected predicates so the check and the delete are atomic
-- in the same tenant-scoped tx. A concurrent AttachAgentAndConnect (enroll)
-- that lands between a hypothetical separate-tx load and this DELETE cannot
-- slip through because this single statement either matches and deletes the
-- row (predicates still hold) or returns 0 rows (the agent already enrolled).
-- rowsAffected==0 must be treated as not_cancellable by the service layer.
DELETE FROM sites
WHERE id = $1 AND tenant_id = $2
  AND connection_state = 'pending_enrollment'
  AND enrolled_at IS NULL
  AND (agent_public_key IS NULL OR agent_public_key = '');

-- name: PruneSiteEvents :execrows
-- Ring-buffer prune: drop events older than the replay window (cross-tenant,
-- app.agent GUC). Bounds table growth.
DELETE FROM site_events
WHERE created_at < $1;
