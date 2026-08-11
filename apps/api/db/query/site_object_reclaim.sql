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
-- really gone. ON CONFLICT DO NOTHING makes it safely re-runnable (a future
-- backfill of already-orphaned sites needs that too).
INSERT INTO site_object_reclaim (tenant_id, site_id, kind, destination_kind)
VALUES (@tenant_id, @site_id, @kind, @destination_kind)
ON CONFLICT (tenant_id, site_id, kind) DO NOTHING;

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
-- Whether the tenant is itself soft-deleted. The org purge worker owns the
-- whole tenant/<id>/ root for those and holds its own session advisory lock in
-- a different namespace, so this worker skips them rather than trying to
-- serialise across two lock namespaces.
SELECT deleted_at, purge_started_at FROM tenants WHERE id = @tenant_id;

-- name: GetSiteDefaultDestinationKind :one
-- The site's default backup destination kind, read in the delete's own
-- transaction BEFORE the cascade takes the row away. Diagnostic only: it lets
-- the reclaim log and a future delete response say plainly that a site's
-- backups lived in the operator's own bucket and were not touched. No rows
-- means the site used the legacy control-plane-global bucket.
SELECT kind FROM site_destinations
WHERE tenant_id = @tenant_id AND site_id = @site_id AND is_default = true
LIMIT 1;
