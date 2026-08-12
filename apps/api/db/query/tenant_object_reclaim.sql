-- tenant_object_reclaim (m116 / GH #408): the durable record of tenant-wide
-- object-storage work that outlives a Lane A tenant delete.
--
-- The enqueue is NOT here. It lives inside admin_delete_empty_tenant's own body
-- (m116), so it rides the delete's transaction and covers BOTH Lane A callers
-- without either of them having to remember. The only enqueue in this file is
-- the operator backfill, for tenants deleted before m116 existed.
--
-- Everything here runs cross-tenant under InAgentTx (app.agent), which is the
-- policy this table carries for the drain, for the function's in-body insert,
-- and for `wpmgr-cli reclaim`.

-- name: EnqueueTenantObjectReclaim :execrows
-- The OPERATOR backfill: hand an already-deleted tenant to the drain rather than
-- deleting its prefixes by hand, so every worker guard stays in play.
--
-- The conflict clause REOPENS rather than doing nothing, for the reason m113
-- spells out: DO NOTHING against an already completed row silently drops the new
-- work, and silently dropping reclamation work is the failure this table exists
-- to remove. Reopening is safe in the other direction, because the drain
-- re-lists, finds the roots already empty, and closes the task again.
--
-- next_attempt_at is now(), NOT the 24 hour floor: the floor protects an
-- operator from a delete they did not mean to make, and this row IS the operator
-- saying they meant it. The caller checks that no tenants row exists first.
--
-- next_attempt_at is named EXPLICITLY in the INSERT, and that is load-bearing
-- rather than tidy. The column's DEFAULT is the m116 floor, now() + 24 hours, so
-- an INSERT that omits it takes the floor, and the INSERT branch is exactly the
-- population this command exists for: a tenant hard-deleted BEFORE m116 has no
-- row to conflict with, so the DO UPDATE clause below never runs for it. Omitted,
-- the operator's explicit instruction reported success and then did nothing for a
-- day, while the ON CONFLICT path they never take was immediate.
-- TestGH408_BackfillFromSystemAuditLogFindsLaneAOrgDeletes drains after
-- backfilling for this reason.
INSERT INTO tenant_object_reclaim (tenant_id, kind, next_attempt_at)
VALUES (@tenant_id, @kind, now())
ON CONFLICT (tenant_id, kind) DO UPDATE
SET completed_at    = NULL,
    attempts        = 0,
    next_attempt_at = now(),
    last_error      = NULL,
    updated_at      = now();

-- name: ListDueTenantObjectReclaims :many
-- The drain's batch. Past @max_attempts a task drops out of this query but is
-- NEVER deleted: it is the only remaining record that those objects exist.
SELECT id, tenant_id, kind, attempts, next_attempt_at, last_error,
       completed_at, created_at, updated_at
FROM tenant_object_reclaim
WHERE completed_at IS NULL
  AND next_attempt_at <= now()
  AND attempts < @max_attempts
ORDER BY created_at, id
LIMIT @row_limit;

-- name: ListStuckTenantObjectReclaims :many
-- The tasks that exhausted @max_attempts and so no longer appear above. Kept is
-- not the same as visible: the sweep re-reads this every tick and logs it, so a
-- stuck task is loud for as long as it is stuck.
SELECT id, tenant_id, kind, attempts, next_attempt_at, last_error,
       completed_at, created_at, updated_at
FROM tenant_object_reclaim
WHERE completed_at IS NULL
  AND attempts >= @max_attempts
ORDER BY created_at, id
LIMIT @row_limit;

-- name: ListOpenTenantObjectReclaims :many
-- Everything still open, whatever its attempt count. This is what
-- `wpmgr-cli reclaim list` shows an operator, and it is the answer to the
-- chicken-and-egg the GUC documentation option could not solve: with no GUC set
-- the table reads as empty, so an operator cannot discover the id that a
-- "SET app.tenant_id first" instruction would need them to supply.
SELECT id, tenant_id, kind, attempts, next_attempt_at, last_error,
       completed_at, created_at, updated_at
FROM tenant_object_reclaim
WHERE completed_at IS NULL
ORDER BY created_at, id
LIMIT @row_limit;

-- name: ReopenTenantObjectReclaim :execrows
-- `wpmgr-cli reclaim retry --task`: put a stuck task back in the due queue. The
-- caller reports rows=0 as a failure and exits non-zero, which is the property
-- that makes this a recovery path rather than another statement that reports
-- success having done nothing.
UPDATE tenant_object_reclaim
SET attempts        = 0,
    next_attempt_at = now(),
    last_error      = NULL,
    updated_at      = now()
WHERE id = @id AND completed_at IS NULL;

-- name: CompleteTenantObjectReclaim :execrows
-- Set ONLY after a FRESH re-list of every root returned zero keys. A partial
-- drain marked complete is GH #402 recreated exactly.
UPDATE tenant_object_reclaim
SET completed_at = now(), last_error = NULL, updated_at = now()
WHERE id = @id AND completed_at IS NULL;

-- name: FailTenantObjectReclaim :execrows
-- Records a failed or refused attempt and backs the task off. Never deletes the
-- row. There is no Cancel for this table: cancelling would close the only record
-- naming those objects, and unlike the site case there is no outcome here that
-- PROVES there is nothing to reclaim. A guard refusal leaves the task open on
-- purpose, so a restored dump makes the drain stand off rather than forget.
UPDATE tenant_object_reclaim
SET attempts        = attempts + 1,
    next_attempt_at = now() + @backoff::interval,
    last_error      = @last_error,
    updated_at      = now()
WHERE id = @id AND completed_at IS NULL;

-- name: TenantExistsForReclaim :one
-- Drain GUARD: is the tenant row BACK? A restored dump, or a control plane whose
-- database is older than the bucket it is pointed at (the control-plane store is
-- built with no PathPrefix, so every key sits at bucket root and there is no
-- second containment layer). The tenant-level analogue of the site guard m113
-- calls GUARD 3.
SELECT EXISTS (SELECT 1 FROM tenants WHERE id = @tenant_id) AS tenant_exists;

-- name: SitesExistForTenantReclaim :one
-- Drain GUARD: does any site still name this tenant? Impossible via the cascade,
-- and cheap belt and braces against a partial dump restore. Reads the raw sites
-- table cross-tenant under app.agent, deliberately NOT a helper that filters
-- connection_state: an archived site is LIVE and restorable.
SELECT EXISTS (SELECT 1 FROM sites WHERE tenant_id = @tenant_id) AS sites_exist;

-- name: ChunkRowsExistForTenantReclaim :one
-- Drain GUARD: does any chunk row still name this tenant? If the inventory is
-- back then something restored it, and the mark-and-sweep owns those objects
-- again, not this drain.
SELECT EXISTS (SELECT 1 FROM backup_chunks WHERE tenant_id = @tenant_id) AS chunk_rows_exist;

-- name: ListHardDeletedTenantsFromSystemAudit :many
-- `wpmgr-cli reclaim backfill-tenants`: the tenants that Lane A hard-deleted
-- BEFORE m116 existed, recovered from the database rather than from a bucket
-- scan.
--
-- DELETE /orgs/{orgId} writes org.deleted to system_audit_log with the lane in
-- its metadata, and m93 gave that table a tenant_id column with NO foreign key
-- to tenants precisely so the record survives the delete it describes. So every
-- Lane A hard delete since m93 left a durable, tenant-independent record of its
-- tenant id, and this query is that population exactly.
--
-- It does NOT cover the superadmin orphan cleanup, which writes no system audit
-- event at all; those tenant ids exist nowhere in the database and the only
-- surviving evidence is the bucket. That is what `reclaim discover` is for, and
-- it is report-only on purpose.
--
-- NOT EXISTS against tenants is what makes this evidence-based rather than a
-- guess: a soft-deleted org that was later restored still has its row and is
-- skipped, and so is any tenant that is merely mid-grace-window.
SELECT DISTINCT s.tenant_id
FROM system_audit_log s
WHERE s.action = 'org.deleted'
  AND s.metadata ->> 'lane' = 'hard'
  AND NOT EXISTS (SELECT 1 FROM tenants t WHERE t.id = s.tenant_id)
ORDER BY s.tenant_id;
