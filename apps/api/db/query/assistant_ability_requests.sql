-- assistant_ability_requests (m156): every statement over the kind-generic
-- ability request table.
--
-- WHICH TRANSACTION, PER STATEMENT. As db/query/assistant_requests.sql (m151),
-- whose header defines the principals named here: connection-scoped,
-- single-site, approver, revoker, tenant, agent scan. The advisory locks are
-- m151's TakeAssistantRequestXactLock / TryAssistantRequestXactLock, reused.
--
-- TIME IS THE DATABASE'S. Every window is measured against now() in the
-- database; lengths arrive as whole seconds. decided_at, dispatch_deadline_at
-- and claimed_at are always computed in the statement that sets them.
--
-- digest_nonce and input_json appear only in full-row reads (`*`). The status
-- reads return a narrow projection with no digest, nonce, input, grant label,
-- setup client, decider or site-reported text.

-- ---------------------------------------------------------------------------
-- Creation (internal/mcp write rail), connection-scoped, after the grant lock
-- ---------------------------------------------------------------------------

-- name: ExpireLapsedPendingAbilityRequestsForGrantSite :many
-- This connection's own lapsed waiting rows on this site become 'expired'
-- before the caps are counted and the insert runs.
UPDATE assistant_ability_requests
SET state = 'expired'
WHERE tenant_id = @tenant_id
  AND site_id = @site_id
  AND proposed_by_grant_id = @proposed_by_grant_id
  AND state = 'pending'
  AND expires_at <= now()
RETURNING id;

-- name: CountLivePendingAbilityRequestsForGrant :one
-- The per-connection waiting cap (10). Counts every site the transaction
-- admits, so it must run connection-scoped.
SELECT count(*)::bigint AS pending
FROM assistant_ability_requests
WHERE tenant_id = @tenant_id
  AND proposed_by_grant_id = @proposed_by_grant_id
  AND state = 'pending'
  AND expires_at > now();

-- name: CountLivePendingAbilityRequestsForGrantSite :one
-- The per-connection per-site waiting cap, narrowed to one ability when
-- ability_name is given (5 pending creations per connection per site).
SELECT count(*)::bigint AS pending
FROM assistant_ability_requests
WHERE tenant_id = @tenant_id
  AND site_id = @site_id
  AND proposed_by_grant_id = @proposed_by_grant_id
  AND (sqlc.narg(ability_name)::text IS NULL OR ability_name = sqlc.narg(ability_name)::text)
  AND state = 'pending'
  AND expires_at > now();

-- name: CountAbilityRequestsForGrantSince :one
-- The per-connection daily cap (30), shared across entries: rows created in
-- the window, any state, any site the transaction admits. Connection-scoped.
SELECT count(*)::bigint AS created
FROM assistant_ability_requests
WHERE tenant_id = @tenant_id
  AND proposed_by_grant_id = @proposed_by_grant_id
  AND created_at > now() - (sqlc.arg(window_seconds)::int * interval '1 second');

-- name: CountAbilityRequestsOnSiteSince :one
-- The per-site hourly cap (12), every connection. Run it under a principal
-- that admits the site.
SELECT count(*)::bigint AS created
FROM assistant_ability_requests
WHERE tenant_id = @tenant_id
  AND site_id = @site_id
  AND created_at > now() - (sqlc.arg(window_seconds)::int * interval '1 second');

-- name: InsertAbilityRequest :one
-- id is the request id the site receives with the write. A caller that
-- prechecked the request under an id it generated passes that id, so the
-- write names the request its precheck was made under; NULL takes the
-- column default, gen_random_uuid().
-- ON CONFLICT names the one-pending index's columns and predicate. A conflict
-- inserts nothing and returns NO ROW (pgx.ErrNoRows), whatever id was passed:
-- the caller reads the waiting row, which keeps its own id, with
-- GetPendingAbilityRequestForTarget. An id that already names a row is not
-- that conflict: it fails with 23505. state is always 'pending'; target_key
-- is generated and never written. m161: route_id, route_sha256 and
-- card_facts are set together for wpmgr/rest-write and are NULL for every
-- other ability (the table's CHECKs refuse anything else).
INSERT INTO assistant_ability_requests (
    id,
    tenant_id, site_id, proposed_by_grant_id,
    entry_id, entry_sha256, ability_name, operator_permission,
    input_json, input_sha256, target_post_id,
    precheck_digest, preview_digest, base_fingerprint,
    site_label, site_host, grant_label, grant_via, setup_client,
    title_excerpt, editor, post_type, effect_copy, snapshot, card_copy_version,
    digest_nonce, presented_digest, state, expires_at,
    route_id, route_sha256, card_facts
) VALUES (
    COALESCE(sqlc.narg(id)::uuid, gen_random_uuid()),
    @tenant_id, @site_id, @proposed_by_grant_id,
    @entry_id, @entry_sha256, @ability_name, @operator_permission,
    @input_json, @input_sha256, sqlc.narg(target_post_id),
    @precheck_digest, sqlc.narg(preview_digest), @base_fingerprint,
    @site_label, @site_host, @grant_label, @grant_via, sqlc.narg(setup_client),
    sqlc.narg(title_excerpt), sqlc.narg(editor), sqlc.narg(post_type),
    @effect_copy, @snapshot, @card_copy_version,
    @digest_nonce, @presented_digest, 'pending', @expires_at,
    sqlc.narg(route_id), sqlc.narg(route_sha256), sqlc.narg(card_facts)
)
ON CONFLICT (tenant_id, site_id, proposed_by_grant_id, ability_name, target_key)
    WHERE state = 'pending'
DO NOTHING
RETURNING *;

-- name: GetPendingAbilityRequestForTarget :one
-- The dedupe read after a conflict. target_key is 'post:<id>' or
-- 'new:<input_sha256>' (m156 header). pgx.ErrNoRows means a person decided
-- the row in between; the caller retries the insert once.
SELECT *
FROM assistant_ability_requests
WHERE tenant_id = @tenant_id
  AND site_id = @site_id
  AND proposed_by_grant_id = @proposed_by_grant_id
  AND ability_name = @ability_name
  AND target_key = @target_key
  AND state = 'pending';

-- ---------------------------------------------------------------------------
-- Status tool (internal/mcp), connection-scoped. Narrow projection.
-- site_id = ANY(@site_ids) is the connection's current scope.
-- ---------------------------------------------------------------------------

-- name: GetAbilityRequestStatusForGrant :one
SELECT id, site_id, site_label, ability_name, title_excerpt, editor, post_type,
       effect_copy, state, created_at, expires_at, decided_at, withdrawn_at,
       claimed_at, last_attempt_at, last_attempt_code, unknown_since,
       outcome, outcome_code, not_sent_reason, outcome_at,
       created_post_id, restored, trashed, undo_state, undo_available_until
FROM assistant_ability_requests
WHERE tenant_id = @tenant_id
  AND id = @id
  AND proposed_by_grant_id = @proposed_by_grant_id
  AND site_id = ANY(@site_ids::uuid[]);

-- name: ListOpenAbilityRequestStatusForGrant :many
-- List mode: this connection's requests with no final result yet, newest
-- first (engine v4 §1.5: at most 20).
SELECT id, site_id, site_label, ability_name, title_excerpt, editor, post_type,
       effect_copy, state, created_at, expires_at, decided_at, withdrawn_at,
       claimed_at, last_attempt_at, last_attempt_code, unknown_since,
       outcome, outcome_code, not_sent_reason, outcome_at,
       created_post_id, restored, trashed, undo_state, undo_available_until
FROM assistant_ability_requests
WHERE tenant_id = @tenant_id
  AND proposed_by_grant_id = @proposed_by_grant_id
  AND site_id = ANY(@site_ids::uuid[])
  AND (state IN ('pending', 'approved', 'dispatched')
       OR (state = 'outcome_unknown' AND outcome IS NULL))
ORDER BY created_at DESC, id DESC
LIMIT @row_limit;

-- ---------------------------------------------------------------------------
-- Revoke cascade, in the revoke transaction, org-scoped revoker.
-- ---------------------------------------------------------------------------

-- name: WithdrawPendingAbilityRequestsForGrant :many
-- Names nobody: decided_by_user_id and decided_at stay NULL.
UPDATE assistant_ability_requests
SET state = 'withdrawn', withdrawn_at = now()
WHERE tenant_id = @tenant_id
  AND proposed_by_grant_id = @proposed_by_grant_id
  AND state = 'pending'
RETURNING id;

-- name: CloseApprovedAbilityRequestsForGrant :many
-- An approved row not yet reserved is closed as not sent; its approver stays
-- recorded. A reserved row is left alone: that write has started.
UPDATE assistant_ability_requests
SET state = 'not_sent', outcome = 'not_sent',
    not_sent_reason = 'grant_inactive', outcome_at = now()
WHERE tenant_id = @tenant_id
  AND proposed_by_grant_id = @proposed_by_grant_id
  AND state = 'approved'
RETURNING id;

-- ---------------------------------------------------------------------------
-- Approve and decline, approver's own principal. The service re-checks the
-- approver holds operator_permission on the site (W1) before either.
-- ---------------------------------------------------------------------------

-- name: GetPendingAbilityRequestForSite :one
SELECT *
FROM assistant_ability_requests
WHERE tenant_id = @tenant_id
  AND id = @id
  AND site_id = @site_id
  AND state = 'pending';

-- name: ApproveAbilityRequest :one
-- The digest, the state and the window are all in the WHERE clause;
-- decided_at and dispatch_deadline_at come from now(). No row
-- (pgx.ErrNoRows) is the refusal: already decided, withdrawn, expired, or the
-- card the approver saw is not the one stored.
UPDATE assistant_ability_requests
SET state = 'approved', decided_at = now(),
    decided_by_user_id = sqlc.arg(decided_by_user_id)::uuid,
    dispatch_deadline_at = now() + (sqlc.arg(dispatch_window_seconds)::int * interval '1 second')
WHERE tenant_id = @tenant_id
  AND id = @id
  AND site_id = @site_id
  AND presented_digest = @presented_digest
  AND state = 'pending'
  AND expires_at > now()
RETURNING *;

-- name: DeclineAbilityRequest :one
-- A lapsed row is not declined; the sweeper expires it. No row is the refusal.
UPDATE assistant_ability_requests
SET state = 'declined', decided_at = now(),
    decided_by_user_id = sqlc.arg(decided_by_user_id)::uuid
WHERE tenant_id = @tenant_id
  AND id = @id
  AND site_id = @site_id
  AND state = 'pending'
  AND expires_at > now()
RETURNING *;

-- ---------------------------------------------------------------------------
-- Queue and banner, caller's own principal. Row security narrows a site
-- collaborator to their own sites.
-- ---------------------------------------------------------------------------

-- name: ListAbilityRequests :many
SELECT *
FROM assistant_ability_requests
WHERE tenant_id = @tenant_id
ORDER BY created_at DESC, id DESC
LIMIT @row_limit OFFSET @row_offset;

-- name: ListAbilityRequestsForSite :many
SELECT *
FROM assistant_ability_requests
WHERE tenant_id = @tenant_id
  AND site_id = @site_id
ORDER BY created_at DESC, id DESC
LIMIT @row_limit OFFSET @row_offset;

-- name: ListOrgAbilityRequests :many
-- The org-wide AI requests queue (GH #828): every site the caller's row
-- security admits, optionally narrowed to one state, newest first. A NULL
-- state_filter lists every state. Same columns as ListAbilityRequestsForSite.
-- The badge count is CountLivePendingAbilityRequests.
SELECT *
FROM assistant_ability_requests
WHERE tenant_id = @tenant_id
  AND (sqlc.narg(state_filter)::text IS NULL OR state = sqlc.narg(state_filter)::text)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit OFFSET @row_offset;

-- name: GetAbilityRequestForSite :one
-- One row for the detail card and the undo action.
SELECT *
FROM assistant_ability_requests
WHERE tenant_id = @tenant_id
  AND id = @id
  AND site_id = @site_id;

-- name: CountLivePendingAbilityRequests :one
SELECT count(*)::bigint AS pending
FROM assistant_ability_requests
WHERE tenant_id = @tenant_id
  AND state = 'pending'
  AND expires_at > now();

-- name: CountLivePendingAbilityRequestsForSite :one
SELECT count(*)::bigint AS pending
FROM assistant_ability_requests
WHERE tenant_id = @tenant_id
  AND site_id = @site_id
  AND state = 'pending'
  AND expires_at > now();

-- ---------------------------------------------------------------------------
-- Dispatch worker
-- ---------------------------------------------------------------------------

-- name: ScanDueApprovedAbilityRequests :many
-- Agent scan (InAgentTx), plain SELECT. Backoff per row as m151's.
SELECT id, tenant_id, site_id, proposed_by_grant_id
FROM assistant_ability_requests
WHERE state = 'approved'
  AND (last_attempt_at IS NULL
       OR last_attempt_at < now() - LEAST(
            sqlc.arg(backoff_cap_seconds)::int * interval '1 second',
            sqlc.arg(backoff_base_seconds)::int * interval '1 second'
              * power(2, LEAST(GREATEST(dispatch_attempts - 1, 0), 16))))
ORDER BY decided_at ASC, id ASC
LIMIT @row_limit;

-- name: GetApprovedAbilityRequestForDispatch :one
-- Single-site. past_deadline and entry_current are computed in the database:
-- entry_current is false when the catalogue entry this row was approved
-- against has a different entry_sha256 now, is disabled, is no longer
-- admitted, or is gone (W1: close not_sent / entry_changed or
-- entry_disabled). ability_catalogue is global and readable in any
-- transaction.
SELECT sqlc.embed(r),
       (r.dispatch_deadline_at <= now())::boolean AS past_deadline,
       EXISTS (
           SELECT 1 FROM ability_catalogue c
           WHERE c.entry_id = r.entry_id
             AND c.entry_sha256 = r.entry_sha256
       )::boolean AS entry_hash_current,
       EXISTS (
           SELECT 1 FROM ability_catalogue c
           WHERE c.entry_id = r.entry_id
             AND c.enabled
             AND c.status = 'admitted'
       )::boolean AS entry_enabled,
       -- m161: true for a row that names no route; otherwise the route still
       -- carries the hash the row was approved against (route_changed) and is
       -- an enabled write route (route_disabled).
       (r.route_id IS NULL OR EXISTS (
           SELECT 1 FROM rest_route_catalogue rr
           WHERE rr.route_id = r.route_id
             AND rr.route_sha256 = r.route_sha256
       ))::boolean AS route_hash_current,
       (r.route_id IS NULL OR EXISTS (
           SELECT 1 FROM rest_route_catalogue rr
           WHERE rr.route_id = r.route_id
             AND rr.enabled
             AND rr.class = 'write'
       ))::boolean AS route_enabled
FROM assistant_ability_requests r
WHERE r.tenant_id = @tenant_id
  AND r.id = @id
  AND r.state = 'approved';

-- name: CloseApprovedAbilityRequestNotSent :execrows
-- A terminal close before anything is reserved, for every pre-send reason
-- (grant_inactive, assistant_paused, organisation_deleted,
-- capability_not_held, site_absent, forbidden_by_context, agent_outdated,
-- dispatch_deadline_passed, entry_changed, entry_disabled, and m161's
-- route_changed, route_disabled). 0 rows: another
-- path got there first; write nothing. The approver stays.
UPDATE assistant_ability_requests
SET state = 'not_sent', outcome = 'not_sent',
    not_sent_reason = sqlc.arg(not_sent_reason)::text, outcome_at = now()
WHERE tenant_id = @tenant_id
  AND id = @id
  AND state = 'approved';

-- name: RecordAbilityRequestDispatchAttempt :execrows
-- A transient reason; the row stays approved. Single-site.
UPDATE assistant_ability_requests
SET dispatch_attempts = dispatch_attempts + 1,
    last_attempt_at = now(),
    last_attempt_code = sqlc.arg(last_attempt_code)::text
WHERE tenant_id = @tenant_id
  AND id = @id
  AND state = 'approved';

-- name: AbilityRequestInFlightOnSite :one
-- The per-site serialisation (ADR-061 A4): another ability write on this
-- site was sent and has no outcome yet, including one whose outcome is
-- being resolved through the ledger. Single-site.
SELECT EXISTS (
    SELECT 1
    FROM assistant_ability_requests
    WHERE tenant_id = @tenant_id
      AND site_id = @site_id
      AND state IN ('dispatched', 'outcome_unknown')
      AND outcome IS NULL
)::boolean AS busy;

-- name: ReserveAbilityRequestForDispatch :execrows
-- The one reservation point. The deadline and the approved entry hash are in
-- the WHERE clause, so nothing is reserved after the deadline or against an
-- entry that changed. m161: a row naming a REST route is reserved only while
-- that route carries the approved route_sha256 and is an enabled write route. 0 rows: another replica, the revoke cascade, a close,
-- the sweeper or a catalogue change won; roll back.
UPDATE assistant_ability_requests r
SET state = 'dispatched', claimed_at = now()
WHERE r.tenant_id = @tenant_id
  AND r.id = @id
  AND r.state = 'approved'
  AND r.dispatch_deadline_at > now()
  AND EXISTS (
      SELECT 1 FROM ability_catalogue c
      WHERE c.entry_id = r.entry_id
        AND c.entry_sha256 = r.entry_sha256
        AND c.enabled
        AND c.status = 'admitted'
  )
  AND (r.route_id IS NULL OR EXISTS (
      SELECT 1 FROM rest_route_catalogue rr
      WHERE rr.route_id = r.route_id
        AND rr.route_sha256 = r.route_sha256
        AND rr.enabled
        AND rr.class = 'write'
  ));

-- name: MarkAbilityRequestOutcomeUnknown :execrows
-- The send's reply was lost or timed out. The row waits in 'outcome_unknown'
-- with outcome NULL while the ledger is polled (engine v4 §2.7).
UPDATE assistant_ability_requests
SET state = 'outcome_unknown', unknown_since = now()
WHERE tenant_id = @tenant_id
  AND id = @id
  AND state = 'dispatched'
  AND outcome IS NULL;

-- name: RecordAbilityRequestOutcome :execrows
-- Compare-and-set on "sent with no outcome", from 'dispatched' (the worker's
-- reply) or a resolving 'outcome_unknown' (the ledger's answer). 0 rows:
-- another path already recorded one; write nothing. The state is derived
-- from the outcome; the table CHECK enforces the pairing. Passing 'not_sent'
-- needs not_sent_reason 'transport_pre_send'. undo_available_until set
-- opens the person's undo (undo_state 'available') on a done row. A row with
-- any undo_state already set is never matched (GH #826): this statement
-- rewrites the undo columns, and must not reset an undo that is running.
-- m169: snapshot_sha256 is the hash of the copy the agent kept before an
-- applied page edit, NULL for every other outcome. It is written once: a row
-- that already carries one is never matched. An applied page edit with no
-- hash must be recorded with undo_available_until NULL (no undo); the
-- table's page_edit_undo_hash_check refuses anything else.
UPDATE assistant_ability_requests
SET state = CASE sqlc.arg(outcome)::text
                WHEN 'created' THEN 'done'
                WHEN 'applied' THEN 'done'
                WHEN 'not_sent' THEN 'not_sent'
                WHEN 'outcome_unknown' THEN 'outcome_unknown'
                ELSE 'failed'
            END,
    outcome = sqlc.arg(outcome)::text,
    outcome_at = now(),
    unknown_since = CASE WHEN sqlc.arg(outcome)::text = 'outcome_unknown'
                         THEN coalesce(unknown_since, now())
                         ELSE unknown_since END,
    outcome_code = sqlc.narg(outcome_code),
    not_sent_reason = sqlc.narg(not_sent_reason),
    created_post_id = sqlc.narg(created_post_id),
    restored = sqlc.narg(restored),
    trashed = sqlc.narg(trashed),
    site_reported_text = sqlc.narg(site_reported_text),
    undo_state = CASE WHEN sqlc.narg(undo_available_until)::timestamptz IS NULL
                      THEN NULL ELSE 'available' END,
    undo_available_until = sqlc.narg(undo_available_until)::timestamptz,
    snapshot_sha256 = sqlc.narg(snapshot_sha256)::text
WHERE tenant_id = @tenant_id
  AND id = @id
  AND state IN ('dispatched', 'outcome_unknown')
  AND outcome IS NULL
  AND undo_state IS NULL
  AND snapshot_sha256 IS NULL;

-- ---------------------------------------------------------------------------
-- Sweeper and reconciler. Each is a plain agent scan, then a per-row
-- compare-and-set in a tenant transaction.
-- ---------------------------------------------------------------------------

-- name: ScanLapsedPendingAbilityRequests :many
SELECT id, tenant_id
FROM assistant_ability_requests
WHERE state = 'pending'
  AND expires_at <= now()
ORDER BY expires_at ASC, id ASC
LIMIT @row_limit;

-- name: ExpireAbilityRequest :execrows
UPDATE assistant_ability_requests
SET state = 'expired'
WHERE tenant_id = @tenant_id
  AND id = @id
  AND state = 'pending'
  AND expires_at <= now();

-- name: ScanApprovedAbilityRequestsPastDeadline :many
SELECT id, tenant_id
FROM assistant_ability_requests
WHERE state = 'approved'
  AND dispatch_deadline_at <= now()
ORDER BY dispatch_deadline_at ASC, id ASC
LIMIT @row_limit;

-- name: CloseAbilityRequestPastDeadline :execrows
UPDATE assistant_ability_requests
SET state = 'not_sent', outcome = 'not_sent',
    not_sent_reason = 'dispatch_deadline_passed', outcome_at = now()
WHERE tenant_id = @tenant_id
  AND id = @id
  AND state = 'approved'
  AND dispatch_deadline_at <= now();

-- name: ScanStaleDispatchedAbilityRequests :many
-- Sent, no reply recorded, older than the write budget: the worker died
-- between send and record. Each is moved to outcome_unknown with
-- MarkAbilityRequestOutcomeUnknown, never retried.
SELECT id, tenant_id
FROM assistant_ability_requests
WHERE state = 'dispatched'
  AND outcome IS NULL
  AND claimed_at < now() - (sqlc.arg(stale_after_seconds)::int * interval '1 second')
ORDER BY claimed_at ASC, id ASC
LIMIT @row_limit;

-- name: ScanResolvingAbilityRequests :many
-- Rows waiting in outcome_unknown whose ledger was not checked in the last
-- poll interval. The caller sends ability_run{mode:"ledger"} and records the
-- answer with RecordAbilityRequestOutcome, or the check with
-- RecordAbilityRequestLedgerCheck.
SELECT id, tenant_id, site_id, unknown_since
FROM assistant_ability_requests
WHERE state = 'outcome_unknown'
  AND outcome IS NULL
  AND (ledger_checked_at IS NULL
       OR ledger_checked_at < now() - (sqlc.arg(poll_interval_seconds)::int * interval '1 second'))
ORDER BY unknown_since ASC, id ASC
LIMIT @row_limit;

-- name: RecordAbilityRequestLedgerCheck :execrows
UPDATE assistant_ability_requests
SET ledger_checked_at = now()
WHERE tenant_id = @tenant_id
  AND id = @id
  AND state = 'outcome_unknown'
  AND outcome IS NULL;

-- name: GiveUpAbilityRequestOutcome :execrows
-- "Could not tell" after the resolution window (engine v4 §2.7: 15 minutes).
UPDATE assistant_ability_requests
SET outcome = 'outcome_unknown', outcome_at = now()
WHERE tenant_id = @tenant_id
  AND id = @id
  AND state = 'outcome_unknown'
  AND outcome IS NULL
  AND unknown_since <= now() - (sqlc.arg(window_seconds)::int * interval '1 second');

-- ---------------------------------------------------------------------------
-- A person's undo of a done request, approver-side principal. The agent
-- takes the object id from its own ledger row for this request id (W3).
-- ---------------------------------------------------------------------------

-- name: BeginAbilityRequestUndo :execrows
-- Compare-and-set: one undo per request, inside the window.
UPDATE assistant_ability_requests
SET undo_state = 'in_progress', undo_started_at = now(),
    undo_by_user_id = sqlc.arg(undo_by_user_id)::uuid
WHERE tenant_id = @tenant_id
  AND id = @id
  AND site_id = @site_id
  AND state = 'done'
  AND undo_state = 'available'
  AND undo_available_until > now();

-- name: BeginAbilityRequestRecoveryUndo :execrows
-- GH #826. Compare-and-set from no undo to in_progress on a write that
-- failed or was given up on but left the post it created (a draft) on the
-- site. Only a final outcome qualifies: a resolving outcome_unknown row
-- (outcome NULL) may still be answered by the ledger through
-- RecordAbilityRequestOutcome, so it is refused here. The undo window opens
-- now, window_seconds long; a zero or negative window matches nothing.
UPDATE assistant_ability_requests
SET undo_state = 'in_progress', undo_started_at = now(),
    undo_by_user_id = sqlc.arg(undo_by_user_id)::uuid,
    undo_available_until = now() + (sqlc.arg(window_seconds)::int * interval '1 second')
WHERE tenant_id = @tenant_id
  AND id = @id
  AND site_id = @site_id
  AND state IN ('failed', 'outcome_unknown')
  AND created_post_id IS NOT NULL
  AND outcome IS NOT NULL
  AND undo_state IS NULL
  AND sqlc.arg(window_seconds)::int > 0;

-- name: FinishAbilityRequestUndo :execrows
-- undo_result is one of undone, refused_conflict, refused_published, failed.
-- Covers a done row's undo and a recovery undo (GH #826) alike.
-- restored is the agent's revert report (false: other post columns the site
-- changed remain); NULL, as on a failure or refusal, keeps the stored value.
UPDATE assistant_ability_requests
SET undo_state = sqlc.arg(undo_result)::text, undo_finished_at = now(),
    restored = COALESCE(sqlc.narg(restored)::boolean, restored)
WHERE tenant_id = @tenant_id
  AND id = @id
  AND (state = 'done'
       OR (state IN ('failed', 'outcome_unknown')
           AND created_post_id IS NOT NULL
           AND outcome IS NOT NULL))
  AND undo_state = 'in_progress';

-- name: ReleaseAbilityRequestUndo :execrows
-- A retryable undo failure (GH #824): in_progress goes back to available,
-- still inside the undo window, so the person can try again. The CHECKs
-- require the starter and start time to be NULL whenever undo is available,
-- and undo_finished_at is already NULL while in progress. Past the window
-- this matches nothing; the caller then finishes the undo as 'failed'.
-- A recovery undo (GH #826) goes back to no undo at all, window cleared,
-- because BeginAbilityRequestRecoveryUndo starts only from undo_state NULL;
-- the person can begin it again, with a fresh window.
UPDATE assistant_ability_requests
SET undo_state = CASE WHEN state = 'done' THEN 'available' ELSE NULL END,
    undo_available_until = CASE WHEN state = 'done' THEN undo_available_until ELSE NULL END,
    undo_started_at = NULL, undo_by_user_id = NULL
WHERE tenant_id = @tenant_id
  AND id = @id
  AND (state = 'done'
       OR (state IN ('failed', 'outcome_unknown')
           AND created_post_id IS NOT NULL
           AND outcome IS NOT NULL))
  AND undo_state = 'in_progress'
  AND undo_available_until > now();

-- name: ListStuckAbilityRequestUndos :many
-- Agent scan (InAgentTx), plain SELECT, as ScanResolvingAbilityRequests: undos
-- started longer ago than the threshold with nothing recorded since (GH
-- #824). The caller checks the site ledger and records the answer per row
-- with FinishAbilityRequestUndo or ReleaseAbilityRequestUndo in a tenant
-- transaction. Includes recovery undos (GH #826).
-- Random order, not oldest first: a row whose site stays unreachable keeps
-- its in_progress state, so a fixed order would hand the same row_limit rows
-- to every pass and never reach the rest. Random sampling gives every stuck
-- row an equal chance on each pass without a cursor column.
SELECT id, tenant_id, site_id, undo_started_at, undo_available_until
FROM assistant_ability_requests
WHERE (state = 'done'
      OR (state IN ('failed', 'outcome_unknown')
          AND created_post_id IS NOT NULL
          AND outcome IS NOT NULL))
  AND undo_state = 'in_progress'
  AND undo_started_at < now() - (sqlc.arg(stale_after_seconds)::int * interval '1 second')
ORDER BY random()
LIMIT @row_limit;

-- ---------------------------------------------------------------------------
-- Page edit (m169). Reads over one post on one site, in any principal's
-- tenant transaction; the table's tenant and site-scope policies apply on
-- top of the predicates here.
-- ---------------------------------------------------------------------------

-- name: GetEligibleCreatedDraft :one
-- The draft on this site that WPMgr created with wpmgr/page-create as post
-- post_id, while it is still WPMgr's to change: a done creation of that post
-- whose undo has not trashed it and is not trashing it now. The newest such
-- creation, at most one row; pgx.ErrNoRows means the control plane names no
-- draft for this post. The agent checks the post itself before it reads or
-- changes it.
SELECT id, created_post_id, created_at
FROM assistant_ability_requests
WHERE tenant_id = @tenant_id
  AND site_id = @site_id
  AND ability_name = 'wpmgr/page-create'
  AND state = 'done'
  AND outcome = 'created'
  AND created_post_id = sqlc.arg(post_id)::bigint
  AND coalesce(undo_state, '') NOT IN ('undone', 'in_progress')
  AND trashed IS NOT TRUE
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: ListEditRequestsForPost :many
-- Every wpmgr/page-edit request for post post_id on this site, oldest first
-- (created_at, then id), with what each one did and where its undo stands.
-- At most 200 rows: a caller that needs every edit of the post treats 200
-- rows as possibly incomplete.
SELECT id, state, outcome, undo_state, snapshot_sha256, created_at
FROM assistant_ability_requests
WHERE tenant_id = @tenant_id
  AND site_id = @site_id
  AND ability_name = 'wpmgr/page-edit'
  AND target_post_id = sqlc.arg(post_id)::bigint
ORDER BY created_at, id
LIMIT 200;

-- name: NewestUndoableEditForPost :one
-- The newest applied wpmgr/page-edit of post post_id on this site that has
-- not been undone, whatever its own undo state: undo goes newest first, so
-- only this edit may be offered an undo, and only while its own undo is
-- available and inside its window. pgx.ErrNoRows: no applied edit of the post
-- is still in effect.
SELECT id, undo_state, undo_available_until, snapshot_sha256, created_at
FROM assistant_ability_requests
WHERE tenant_id = @tenant_id
  AND site_id = @site_id
  AND ability_name = 'wpmgr/page-edit'
  AND target_post_id = sqlc.arg(post_id)::bigint
  AND state = 'done'
  AND outcome = 'applied'
  AND undo_state IS DISTINCT FROM 'undone'
ORDER BY created_at DESC, id DESC
LIMIT 1;
