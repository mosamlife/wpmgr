-- site_object_reclaim (m113 / GH #402): the durable record of object-storage
-- work that outlives a site delete.
--
-- The enqueue runs inside DELETE /sites/{id}'s own InTenantTx (operator path,
-- app.tenant_id). Everything else runs cross-tenant in the reclaim worker under
-- InAgentTx (app.agent), which is why this table carries both policies.

-- name: EnqueueSiteObjectReclaim :execrows
-- Records the storage prefix work for a site that is being deleted. MUST be
-- called in the SAME transaction as the DELETE, and only when that DELETE
-- actually affected a row: the record must exist if and only if the site is
-- really gone.
--
-- The conflict clause REOPENS rather than doing nothing. DO NOTHING was the
-- first shape and it quietly falsified the invariant above: against an already
-- COMPLETED or CANCELLED row for the same (tenant, site, kind) it dropped the
-- new work on the floor, so a site row that came back (a restored dump, an
-- operator re-creating a site with a preserved id) and was deleted again would
-- leave its second generation of manifests orphaned exactly as before. Silently
-- discarding reclamation work is the failure mode this whole table exists to
-- remove, so it must not be the conflict behaviour.
--
-- Reopening is cheap and safe in the other direction: the worker re-lists the
-- prefix, finds it already empty, and closes the task again. That also makes an
-- operator backfill of already-orphaned sites a single re-runnable statement.
INSERT INTO site_object_reclaim (tenant_id, site_id, kind, destination_kind)
VALUES (@tenant_id, @site_id, @kind, @destination_kind)
ON CONFLICT (tenant_id, site_id, kind) DO UPDATE
SET completed_at     = NULL,
    attempts         = 0,
    next_attempt_at  = now(),
    last_error       = NULL,
    destination_kind = EXCLUDED.destination_kind,
    updated_at       = now();

-- name: ListDueSiteObjectReclaims :many
-- The worker's batch. Cross-tenant (app.agent). Past @max_attempts a task drops
-- out of this query but is NEVER deleted: it is the only remaining record that
-- those objects exist, so it stays visible for an operator to find.
SELECT id, tenant_id, site_id, kind, destination_kind, attempts,
       next_attempt_at, last_error, completed_at, created_at, updated_at
FROM site_object_reclaim
WHERE completed_at IS NULL
  AND next_attempt_at <= now()
  AND attempts < @max_attempts
ORDER BY created_at, id
LIMIT @row_limit;

-- name: ListStuckSiteObjectReclaims :many
-- The tasks that have exhausted @max_attempts and therefore no longer appear in
-- the due query above. The rows are kept deliberately (they are the last record
-- that those objects exist), but kept is not the same as visible: without this
-- a task that gave up produced one Error log line, once, and then nothing ever
-- again. The sweep re-reads this every tick and logs it, so the condition
-- persists in the logs for as long as it persists in the table.
SELECT id, tenant_id, site_id, kind, destination_kind, attempts,
       next_attempt_at, last_error, completed_at, created_at, updated_at
FROM site_object_reclaim
WHERE completed_at IS NULL
  AND attempts >= @max_attempts
ORDER BY created_at, id
LIMIT @row_limit;

-- name: ListOpenSiteObjectReclaims :many
-- Everything still open, whatever its attempt count. What
-- `wpmgr-cli reclaim list` shows an operator (GH #408).
--
-- It exists because with no GUC set this table reads as EMPTY to the application
-- role, so an operator cannot discover the task id that a hand-written
-- correction needs them to supply. That chicken-and-egg is why documenting
-- "SET app.tenant_id first" was not an adequate answer to GH #408 finding 3.
SELECT id, tenant_id, site_id, kind, destination_kind, attempts,
       next_attempt_at, last_error, completed_at, created_at, updated_at
FROM site_object_reclaim
WHERE completed_at IS NULL
ORDER BY created_at, id
LIMIT @row_limit;

-- name: ReopenSiteObjectReclaim :execrows
-- `wpmgr-cli reclaim retry --task`: put a stuck task back in the due queue,
-- correcting its kind on the way (GH #408 finding 3).
--
-- This replaces the bare UPDATE the worker used to print into last_error. That
-- statement was authored for a superuser connection: as the application role
-- with no GUC the RLS USING clause hides the row, so it matched nothing and
-- Postgres reported no error, and the operator was told the correction had been
-- applied when the row was byte-for-byte unchanged. This statement is identical
-- in effect and runs under InAgentTx, where the m113 _agent policy makes the row
-- visible and writable; the caller reports rows=0 as a failure and exits
-- non-zero, so it cannot repeat the trick.
--
-- kind is set rather than left alone because the realistic reason a task is
-- stuck is a kind the worker cannot derive a prefix for, which is exactly what
-- the worker's GUARD 1 reports. The kind check constraint still applies, so a
-- wrong value here is refused by the database rather than silently accepted.
UPDATE site_object_reclaim
SET kind            = @kind,
    attempts        = 0,
    next_attempt_at = now(),
    last_error      = NULL,
    updated_at      = now()
WHERE id = @id AND completed_at IS NULL;

-- name: CompleteSiteObjectReclaim :execrows
-- Marks a task done. Set ONLY after the whole prefix drained with no error, so
-- a crash partway leaves completed_at NULL and the next tick re-lists (a
-- shorter list) and finishes.
UPDATE site_object_reclaim
SET completed_at = now(), last_error = NULL, updated_at = now()
WHERE id = @id AND completed_at IS NULL;

-- name: CancelSiteObjectReclaim :execrows
-- Closes a task WITHOUT deleting anything from storage. Used when the guard
-- finds the site row still present (a restored dump, or a staging control plane
-- pointed at a production bucket). last_error carries the reason so the row
-- reads as a refusal rather than a success.
UPDATE site_object_reclaim
SET completed_at = now(), last_error = @last_error, updated_at = now()
WHERE id = @id AND completed_at IS NULL;

-- name: FailSiteObjectReclaim :execrows
-- Records a failed attempt and backs the task off. The row is left incomplete
-- so the next tick retries; it is never deleted.
UPDATE site_object_reclaim
SET attempts = attempts + 1,
    next_attempt_at = now() + @backoff::interval,
    last_error = @last_error,
    updated_at = now()
WHERE id = @id AND completed_at IS NULL;

-- name: SiteExistsForReclaim :one
-- The reclaim worker's re-verify guard: is the site GENUINELY gone? Reads the
-- raw sites table cross-tenant under app.agent. Deliberately NOT ListAllSiteIDs,
-- which filters connection_state <> 'archived' and would make a LIVE archived
-- site's manifests look orphaned.
SELECT EXISTS (
    SELECT 1 FROM sites WHERE id = @site_id AND tenant_id = @tenant_id
) AS site_exists;

-- name: GetTenantDeletionStateForReclaim :one
-- The tenant's deletion state, which the worker reads as THREE outcomes, not
-- two. A row with deleted_at or purge_started_at set means Lane B (the org
-- purge worker) owns the whole tenant/<id>/ root and holds its own session
-- advisory lock in a different namespace, so this worker skips rather than
-- trying to serialise across two lock namespaces. NO ROW AT ALL means the
-- tenant was hard-deleted, which since m113 dropped the tenant foreign key is
-- the Lane A case (admin_delete_empty_tenant sweeps no storage): the task row
-- survived on purpose and is now the only thing that names those objects, so
-- the worker proceeds. Treating a missing tenant as "skip" would strand it.
SELECT deleted_at, purge_started_at FROM tenants WHERE id = @tenant_id;

-- name: GetSiteDefaultDestinationKind :one
-- The kind of the site's DEFAULT backup destination, read in the delete's own
-- transaction BEFORE the cascade takes the row away. Diagnostic only: it lets
-- the reclaim log say plainly where a site's backup payload lived.
--
-- is_default = true is the correct filter and not an oversight: a new snapshot
-- only ever resolves the default destination (backup.Service.CreateBackup and
-- EnqueueScheduledBackup both go through resolveDefaultDestination), so a
-- non-default row is a destination the site's backups never used and reporting
-- it would be actively misleading. No row therefore means "no default
-- destination", which is the control-plane bucket, whether the site has no
-- destination rows at all or only non-default ones.
SELECT kind FROM site_destinations
WHERE tenant_id = @tenant_id AND site_id = @site_id AND is_default = true
LIMIT 1;
