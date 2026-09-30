-- assistant_cache_purge_requests (m151): every statement over the request
-- table, plus the cache_purge_audit statements the AI clear path needs.
--
-- Both internal/mcp (creation, status, the revoke cascade) and
-- internal/assistantrequest (approve, decline, queue, worker, sweeper,
-- reconciler) call these directly; neither calls the other's store for m151.
--
-- WHICH TRANSACTION, PER STATEMENT. Every statement names the transaction it
-- is written for. Row security decides what each one can see, so the same SQL
-- under a different principal is a different statement:
--   * connection-scoped  RunTenantTx(connectionScopedPrincipal(auth))
--   * single-site        RunTenantTx(mcp.SingleSitePrincipal(auth, site_id)),
--                        opened with mcp.AssertSingleSiteTx
--   * approver           RunTenantTx(the approving person's own principal)
--   * revoker            the revoke transaction, org-scoped revoker
--   * tenant             InTenantTx(tenant) -- the sweeper, the reconciler and
--                        closeWithoutSite only
--   * agent scan         InAgentTx -- plain SELECT, never a locking read
--
-- TIME IS THE DATABASE'S. Every window below is measured against now() in
-- the database; lengths arrive as whole seconds so the policy lives in Go and
-- the clock does not. decided_at is always now() in the statement that
-- decides, never a parameter (m133 (7)(c)).
--
-- digest_nonce appears only in full-row reads (`*`), and no caller may put it
-- in any response. The status-tool reads below return a narrow projection
-- with no digest, nonce, grant label, setup client, decider or site-reported
-- text, so the model-facing path cannot carry them.

-- ---------------------------------------------------------------------------
-- Advisory locks. Both arguments are bound AS TEXT: the `::text` casts make
-- sqlc type them as string, so a caller must pass key strings and id.String(),
-- and hashtext(uuid), which does not exist, cannot be reached.
-- ---------------------------------------------------------------------------

-- name: TakeAssistantRequestXactLock :exec
-- Blocking, transaction-scoped. Used for ('assistant_request_grant', grant id)
-- at creation and ('assistant_site_dispatch', site id) at the reservation.
SELECT pg_advisory_xact_lock(hashtext(sqlc.arg(lock_key)::text), hashtext(sqlc.arg(lock_id)::text));

-- name: TryAssistantRequestXactLock :one
-- Non-blocking, transaction-scoped. Used for (org.LifecycleLockKey, tenant id)
-- at the reservation: false is the transient org_busy. A statement ERROR is
-- not false and must never be reported as org_busy.
SELECT pg_try_advisory_xact_lock(hashtext(sqlc.arg(lock_key)::text), hashtext(sqlc.arg(lock_id)::text))::boolean AS acquired;

-- ---------------------------------------------------------------------------
-- Creation (internal/mcp write rail), connection-scoped, after the grant lock
-- ---------------------------------------------------------------------------

-- name: ExpireLapsedPendingAssistantCachePurgeRequest :many
-- This connection's own lapsed waiting row for this site, if any, becomes
-- 'expired' before the insert. At most one row (the one-pending index).
UPDATE assistant_cache_purge_requests
SET state = 'expired'
WHERE tenant_id = @tenant_id
  AND site_id = @site_id
  AND proposed_by_grant_id = @proposed_by_grant_id
  AND state = 'pending'
  AND expires_at <= now()
RETURNING id;

-- name: CountLivePendingAssistantCachePurgeRequestsForGrant :one
-- The per-connection waiting cap. Counts across EVERY site the transaction
-- admits, so it must run connection-scoped: under a single-site principal it
-- would count one site. A lapsed row cannot be approved and is not counted.
SELECT count(*)::bigint AS pending
FROM assistant_cache_purge_requests
WHERE tenant_id = @tenant_id
  AND proposed_by_grant_id = @proposed_by_grant_id
  AND state = 'pending'
  AND expires_at > now();

-- name: CountAssistantCachePurgeRequestsForGrantSince :one
-- The per-connection daily cap: rows created within the window, any state,
-- any site the transaction admits. Connection-scoped, as above.
SELECT count(*)::bigint AS created
FROM assistant_cache_purge_requests
WHERE tenant_id = @tenant_id
  AND proposed_by_grant_id = @proposed_by_grant_id
  AND created_at > now() - (sqlc.arg(window_seconds)::int * interval '1 second');

-- name: InsertAssistantCachePurgeRequest :one
-- ON CONFLICT names the one-pending index's columns and predicate. A conflict
-- inserts nothing and returns NO ROW (pgx.ErrNoRows): the caller then reads
-- the waiting row with GetPendingAssistantCachePurgeRequestForGrantSite.
-- state is always 'pending' here; nothing inserts a decided row.
INSERT INTO assistant_cache_purge_requests (
    tenant_id, site_id, proposed_by_grant_id, scope, url,
    site_label, site_host, grant_label, grant_via, setup_client,
    digest_nonce, presented_digest, state, expires_at
) VALUES (
    @tenant_id, @site_id, @proposed_by_grant_id, @scope, sqlc.narg(url),
    @site_label, @site_host, @grant_label, @grant_via, sqlc.narg(setup_client),
    @digest_nonce, @presented_digest, 'pending', @expires_at
)
ON CONFLICT (tenant_id, site_id, proposed_by_grant_id) WHERE state = 'pending'
DO NOTHING
RETURNING *;

-- name: GetPendingAssistantCachePurgeRequestForGrantSite :one
-- The dedupe read after a conflict. pgx.ErrNoRows means a person decided the
-- row in between; the caller retries the insert once.
SELECT *
FROM assistant_cache_purge_requests
WHERE tenant_id = @tenant_id
  AND site_id = @site_id
  AND proposed_by_grant_id = @proposed_by_grant_id
  AND state = 'pending';

-- ---------------------------------------------------------------------------
-- Status tool (internal/mcp), connection-scoped. Narrow projection.
-- site_id = ANY(@site_ids) is the connection's current scope, passed from
-- auth.Sites, so a row on a site that has left the scope reads as absent.
-- ---------------------------------------------------------------------------

-- name: GetAssistantCachePurgeRequestStatusForGrant :one
SELECT id, site_id, site_label, scope, url, state, created_at, expires_at,
       decided_at, withdrawn_at, claimed_at, last_attempt_at, last_attempt_code,
       outcome, not_sent_reason, outcome_at, hosting_caches_cleared,
       hosting_caches_skipped, origin_only_confirmed, wpmgr_cdn
FROM assistant_cache_purge_requests
WHERE tenant_id = @tenant_id
  AND id = @id
  AND proposed_by_grant_id = @proposed_by_grant_id
  AND site_id = ANY(@site_ids::uuid[]);

-- name: ListOpenAssistantCachePurgeRequestStatusForGrant :many
-- List mode: this connection's requests that have no final result yet
-- (waiting, approved, or sent with no outcome), newest first.
SELECT id, site_id, site_label, scope, url, state, created_at, expires_at,
       decided_at, withdrawn_at, claimed_at, last_attempt_at, last_attempt_code,
       outcome, not_sent_reason, outcome_at, hosting_caches_cleared,
       hosting_caches_skipped, origin_only_confirmed, wpmgr_cdn
FROM assistant_cache_purge_requests
WHERE tenant_id = @tenant_id
  AND proposed_by_grant_id = @proposed_by_grant_id
  AND site_id = ANY(@site_ids::uuid[])
  AND (state IN ('pending', 'approved_undispatched')
       OR (state = 'dispatched' AND outcome IS NULL))
ORDER BY created_at DESC, id DESC
LIMIT @row_limit;

-- ---------------------------------------------------------------------------
-- Revoke cascade (mcp.Repo.CloseAssistantRequestsForGrantTx), in the revoke
-- transaction, org-scoped revoker. Pending rows first, then approved rows.
-- ---------------------------------------------------------------------------

-- name: WithdrawPendingAssistantCachePurgeRequestsForGrant :many
-- Names nobody: decided_by_user_id and decided_at stay NULL.
UPDATE assistant_cache_purge_requests
SET state = 'withdrawn', withdrawn_at = now()
WHERE tenant_id = @tenant_id
  AND proposed_by_grant_id = @proposed_by_grant_id
  AND state = 'pending'
RETURNING id;

-- name: CloseApprovedAssistantCachePurgeRequestsForGrant :many
-- An approved row not yet reserved is closed as not sent; its approver stays
-- recorded. A row already 'dispatched' is left alone: that clear has started.
UPDATE assistant_cache_purge_requests
SET state = 'dispatched', claimed_at = now(), outcome = 'not_sent',
    not_sent_reason = 'grant_inactive', outcome_at = now()
WHERE tenant_id = @tenant_id
  AND proposed_by_grant_id = @proposed_by_grant_id
  AND state = 'approved_undispatched'
RETURNING id;

-- ---------------------------------------------------------------------------
-- Approve and decline (internal/assistantrequest), approver's own principal
-- ---------------------------------------------------------------------------

-- name: GetPendingAssistantCachePurgeRequestForSite :one
SELECT *
FROM assistant_cache_purge_requests
WHERE tenant_id = @tenant_id
  AND id = @id
  AND site_id = @site_id
  AND state = 'pending';

-- name: ApproveAssistantCachePurgeRequest :one
-- The digest, the state and the window are all in the WHERE clause, and
-- decided_at is now(). No row (pgx.ErrNoRows) is the refusal: already
-- decided, withdrawn, expired, or the facts changed under the reader.
UPDATE assistant_cache_purge_requests
SET state = 'approved_undispatched', decided_at = now(),
    decided_by_user_id = sqlc.arg(decided_by_user_id)::uuid
WHERE tenant_id = @tenant_id
  AND id = @id
  AND site_id = @site_id
  AND presented_digest = @presented_digest
  AND state = 'pending'
  AND expires_at > now()
RETURNING *;

-- name: DeclineAssistantCachePurgeRequest :one
-- A lapsed row is not declined: its honest state is 'expired', which the
-- sweeper writes. No row (pgx.ErrNoRows) is the refusal.
UPDATE assistant_cache_purge_requests
SET state = 'rejected', decided_at = now(),
    decided_by_user_id = sqlc.arg(decided_by_user_id)::uuid
WHERE tenant_id = @tenant_id
  AND id = @id
  AND site_id = @site_id
  AND state = 'pending'
  AND expires_at > now()
RETURNING *;

-- ---------------------------------------------------------------------------
-- Queue and banner (internal/assistantrequest), caller's own principal. Row
-- security narrows a site collaborator to their own sites. None of these
-- reads mcp_grants.
-- ---------------------------------------------------------------------------

-- name: ListAssistantCachePurgeRequests :many
SELECT *
FROM assistant_cache_purge_requests
WHERE tenant_id = @tenant_id
ORDER BY created_at DESC, id DESC
LIMIT @row_limit OFFSET @row_offset;

-- name: ListAssistantCachePurgeRequestsForSite :many
SELECT *
FROM assistant_cache_purge_requests
WHERE tenant_id = @tenant_id
  AND site_id = @site_id
ORDER BY created_at DESC, id DESC
LIMIT @row_limit OFFSET @row_offset;

-- name: CountLivePendingAssistantCachePurgeRequests :one
-- The queue badge.
SELECT count(*)::bigint AS pending
FROM assistant_cache_purge_requests
WHERE tenant_id = @tenant_id
  AND state = 'pending'
  AND expires_at > now();

-- name: CountLivePendingAssistantCachePurgeRequestsForSite :one
-- The site banner.
SELECT count(*)::bigint AS pending
FROM assistant_cache_purge_requests
WHERE tenant_id = @tenant_id
  AND site_id = @site_id
  AND state = 'pending'
  AND expires_at > now();

-- ---------------------------------------------------------------------------
-- Dispatch worker (internal/assistantrequest)
-- ---------------------------------------------------------------------------

-- name: ScanDueApprovedAssistantCachePurgeRequests :many
-- Agent scan (InAgentTx), plain SELECT. Returns what the per-row job needs to
-- decide in Go, before any site principal exists. Backoff per row: base
-- doubling per attempt, capped; the exponent is capped too so no interval can
-- overflow.
SELECT id, tenant_id, site_id, proposed_by_grant_id
FROM assistant_cache_purge_requests
WHERE state = 'approved_undispatched'
  AND (last_attempt_at IS NULL
       OR last_attempt_at < now() - LEAST(
            sqlc.arg(backoff_cap_seconds)::int * interval '1 second',
            sqlc.arg(backoff_base_seconds)::int * interval '1 second'
              * power(2, LEAST(GREATEST(dispatch_attempts - 1, 0), 16))))
ORDER BY decided_at ASC, id ASC
LIMIT @row_limit;

-- name: GetApprovedAssistantCachePurgeRequestForDispatch :one
-- Single-site, opened with AssertSingleSiteTx. past_deadline is computed by
-- the database clock. No row: the row is no longer approved (another path
-- closed or reserved it) or is not visible under this principal.
SELECT sqlc.embed(assistant_cache_purge_requests),
       (decided_at <= now() - (sqlc.arg(deadline_seconds)::int * interval '1 second'))::boolean AS past_deadline
FROM assistant_cache_purge_requests
WHERE tenant_id = @tenant_id
  AND id = @id
  AND state = 'approved_undispatched';

-- name: CloseApprovedAssistantCachePurgeRequestNotSent :execrows
-- A terminal close before anything is reserved. The SAME statement serves:
--   * closeWithoutSite (exception 11), tenant transaction, reasons
--     grant_inactive, assistant_paused, capability_not_held, site_absent;
--   * the single-site checks, reasons site_absent, forbidden_by_context,
--     agent_outdated, dispatch_deadline_passed;
--   * the reservation, reasons assistant_paused, organisation_deleted.
-- 0 rows: another path got there first; write nothing. The approver stays.
UPDATE assistant_cache_purge_requests
SET state = 'dispatched', claimed_at = now(), outcome = 'not_sent',
    not_sent_reason = sqlc.arg(not_sent_reason)::text, outcome_at = now()
WHERE tenant_id = @tenant_id
  AND id = @id
  AND state = 'approved_undispatched';

-- name: RecordAssistantCachePurgeDispatchAttempt :execrows
-- A transient reason; the row stays approved. Single-site.
UPDATE assistant_cache_purge_requests
SET dispatch_attempts = dispatch_attempts + 1,
    last_attempt_at = now(),
    last_attempt_code = sqlc.arg(last_attempt_code)::text
WHERE tenant_id = @tenant_id
  AND id = @id
  AND state = 'approved_undispatched';

-- name: GetTenantAssistantLifecycleForShare :one
-- The reservation's organisation re-read. tenants has no row security. FOR
-- SHARE makes a pause or delete that arrives later wait for this reservation
-- to commit.
SELECT (assistant_paused_at IS NOT NULL)::boolean AS assistant_paused,
       (deleted_at IS NOT NULL)::boolean AS deleted
FROM tenants
WHERE id = @id
FOR SHARE;

-- name: AssistantCachePurgeInFlightOnSite :one
-- The per-site busy check: another clear on this site was sent and has no
-- outcome yet. Single-site.
SELECT EXISTS (
    SELECT 1
    FROM assistant_cache_purge_requests
    WHERE tenant_id = @tenant_id
      AND site_id = @site_id
      AND state = 'dispatched'
      AND outcome IS NULL
)::boolean AS busy;

-- name: ReserveAssistantCachePurgeRequest :execrows
-- The one reservation point. Moves the row to 'dispatched' naming the
-- cache_purge_audit row written in the same transaction. The deadline is in
-- the WHERE clause, so nothing is reserved after it. 0 rows: another replica,
-- the revoke cascade, a close or the sweeper won; roll back.
UPDATE assistant_cache_purge_requests
SET state = 'dispatched', claimed_at = now(),
    cache_purge_audit_id = sqlc.arg(cache_purge_audit_id)::uuid
WHERE tenant_id = @tenant_id
  AND id = @id
  AND state = 'approved_undispatched'
  AND decided_at > now() - (sqlc.arg(deadline_seconds)::int * interval '1 second');

-- name: RecordAssistantCachePurgeOutcome :execrows
-- Compare-and-set on "sent with no outcome". 0 rows: the reconciler already
-- closed it; write nothing. not_sent_reason is set only with outcome
-- 'not_sent' (transport_pre_send); the table CHECK enforces the pairing.
UPDATE assistant_cache_purge_requests
SET outcome = sqlc.arg(outcome)::text,
    outcome_at = now(),
    not_sent_reason = sqlc.narg(not_sent_reason),
    hosting_caches_cleared = sqlc.narg(hosting_caches_cleared),
    hosting_caches_skipped = sqlc.narg(hosting_caches_skipped),
    origin_only_confirmed = sqlc.narg(origin_only_confirmed),
    wpmgr_cdn = sqlc.narg(wpmgr_cdn),
    site_reported_text = sqlc.narg(site_reported_text)
WHERE tenant_id = @tenant_id
  AND id = @id
  AND state = 'dispatched'
  AND outcome IS NULL;

-- ---------------------------------------------------------------------------
-- Sweeper and reconciler (internal/assistantrequest). Each is a plain agent
-- scan, then a per-row compare-and-set in a tenant transaction.
-- ---------------------------------------------------------------------------

-- name: ScanLapsedPendingAssistantCachePurgeRequests :many
SELECT id, tenant_id
FROM assistant_cache_purge_requests
WHERE state = 'pending'
  AND expires_at <= now()
ORDER BY expires_at ASC, id ASC
LIMIT @row_limit;

-- name: ExpireAssistantCachePurgeRequest :execrows
-- Never writes decided_by_user_id (m133 (7)(f)).
UPDATE assistant_cache_purge_requests
SET state = 'expired'
WHERE tenant_id = @tenant_id
  AND id = @id
  AND state = 'pending'
  AND expires_at <= now();

-- name: ScanApprovedAssistantCachePurgeRequestsPastDeadline :many
SELECT id, tenant_id
FROM assistant_cache_purge_requests
WHERE state = 'approved_undispatched'
  AND decided_at <= now() - (sqlc.arg(deadline_seconds)::int * interval '1 second')
ORDER BY decided_at ASC, id ASC
LIMIT @row_limit;

-- name: CloseAssistantCachePurgeRequestPastDeadline :execrows
-- The backstop that closes every approved row within the deadline, whatever
-- kept it from starting. last_attempt_code is kept.
UPDATE assistant_cache_purge_requests
SET state = 'dispatched', claimed_at = now(), outcome = 'not_sent',
    not_sent_reason = 'dispatch_deadline_passed', outcome_at = now()
WHERE tenant_id = @tenant_id
  AND id = @id
  AND state = 'approved_undispatched'
  AND decided_at <= now() - (sqlc.arg(deadline_seconds)::int * interval '1 second');

-- name: ScanStaleDispatchedAssistantCachePurgeRequests :many
SELECT id, tenant_id
FROM assistant_cache_purge_requests
WHERE state = 'dispatched'
  AND outcome IS NULL
  AND claimed_at < now() - (sqlc.arg(stale_after_seconds)::int * interval '1 second')
ORDER BY claimed_at ASC, id ASC
LIMIT @row_limit;

-- name: CloseStaleDispatchedAssistantCachePurgeRequest :execrows
UPDATE assistant_cache_purge_requests
SET outcome = 'outcome_unknown', outcome_at = now()
WHERE tenant_id = @tenant_id
  AND id = @id
  AND state = 'dispatched'
  AND outcome IS NULL
  AND claimed_at < now() - (sqlc.arg(stale_after_seconds)::int * interval '1 second');

-- ---------------------------------------------------------------------------
-- cache_purge_audit on the AI clear path. Tx-taking replacements for
-- perf.Repo.RecordPurge. The site-scope policy filters by site, not by
-- initiator (m150 DECISION 3).
-- ---------------------------------------------------------------------------

-- name: InsertAssistantCachePurgeAudit :one
-- The reservation's attempt record. initiator_user_id is the approver if that
-- account still exists and NULL otherwise: the subselect yields NULL for a
-- deleted account, where the id itself would fail the users foreign key.
-- users has no row security, so this read does not depend on the principal.
INSERT INTO cache_purge_audit (
    tenant_id, site_id, kind, initiator_user_id, initiator_grant_id,
    target_urls, urls_count
) VALUES (
    @tenant_id, @site_id, @kind,
    (SELECT u.id FROM users u WHERE u.id = sqlc.arg(approver_user_id)::uuid),
    sqlc.arg(initiator_grant_id)::uuid, @target_urls, @urls_count
)
RETURNING *;

-- name: CountAssistantCachePurgesOnSiteSince :one
-- The per-site cap on AI clears: rows with a grant initiator in the window.
-- Dashboard purges (initiator_grant_id NULL) never count.
SELECT count(*)::bigint AS purges
FROM cache_purge_audit
WHERE tenant_id = @tenant_id
  AND site_id = @site_id
  AND initiator_grant_id IS NOT NULL
  AND created_at > now() - (sqlc.arg(window_seconds)::int * interval '1 second');

-- name: GetLatestWholeSitePurgeOnSiteSince :one
-- The whole-site cooldown: when the newest whole-site clear by ANY initiator
-- in the window happened. No row (pgx.ErrNoRows): none in the window.
-- Preloads and page clears are other kinds and do not count.
SELECT created_at
FROM cache_purge_audit
WHERE tenant_id = @tenant_id
  AND site_id = @site_id
  AND kind = 'all'
  AND created_at > now() - (sqlc.arg(window_seconds)::int * interval '1 second')
ORDER BY created_at DESC
LIMIT 1;
