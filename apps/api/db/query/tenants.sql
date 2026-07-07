-- name: CreateTenant :one
INSERT INTO tenants (name, slug)
VALUES ($1, $2)
RETURNING *;

-- name: GetTenant :one
SELECT * FROM tenants
WHERE id = $1;

-- name: ListTenants :many
SELECT * FROM tenants
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- UpdateTenantName renames a tenant. tenants has no RLS, so the handler verifies
-- the caller's membership + admin/owner role before calling this.
-- name: UpdateTenantName :one
UPDATE tenants
SET name = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- ListOrgsForUser returns the user's organisations with their role in each, for
-- the org switcher + settings (real names, not bare ids). Joins memberships under
-- the memberships_self_read policy (app.user_id GUC) so it MUST run via InUserTx.
-- GH #152: t.deleted_at IS NULL excludes a soft-deleted org — it must vanish
-- from the switcher/list the instant DELETE /orgs/{orgId} commits.
-- name: ListOrgsForUser :many
SELECT t.id, t.name, t.slug, m.role, t.created_at
FROM tenants t
JOIN memberships m ON m.tenant_id = t.id
WHERE m.user_id = $1 AND t.deleted_at IS NULL
ORDER BY t.created_at ASC;

-- ListTenantsForUser returns only the tenants the given user is a member of.
-- It joins memberships under the memberships_self_read policy (app.user_id GUC),
-- so it MUST be run via InUserTx; the join itself restricts the result to the
-- caller's own memberships, preventing cross-tenant enumeration.
-- GH #152: t.deleted_at IS NULL — see ListOrgsForUser.
-- name: ListTenantsForUser :many
SELECT t.id, t.name, t.slug, t.created_at, t.updated_at
FROM tenants t
JOIN memberships m ON m.tenant_id = t.id
WHERE m.user_id = $1 AND t.deleted_at IS NULL
ORDER BY t.created_at DESC
LIMIT $2 OFFSET $3;

-- GetTenantForUser returns a tenant by id only when the given user is a member.
-- Like ListTenantsForUser it relies on the memberships_self_read policy and must
-- be run via InUserTx; a non-member (or unknown tenant) yields no rows.
-- GH #152: t.deleted_at IS NULL means a soft-deleted org's own owner cannot
-- switchOrg into it either (POST /orgs/switch uses this exact query as its
-- sole membership gate) — restoring it first via POST /orgs/{orgId}/restore
-- is required.
-- name: GetTenantForUser :one
SELECT t.id, t.name, t.slug, t.created_at, t.updated_at
FROM tenants t
JOIN memberships m ON m.tenant_id = t.id
WHERE t.id = $1 AND m.user_id = $2 AND t.deleted_at IS NULL;

-- SoftDeleteTenant sets deleted_at (GH #152 Lane B — populated org). The read-
-- path filters above (plus ListMembershipsForUser / GetAPIKeyByPrefix) hide the
-- org everywhere the instant this commits. The WHERE guard makes a concurrent
-- double-delete attempt a no-op (0 rows) rather than a double-stamp; callers
-- run this under the per-tenant org_lifecycle advisory lock (see
-- internal/org/delete_handler.go) so the guard is authoritative, not racy.
-- name: SoftDeleteTenant :one
UPDATE tenants
SET deleted_at = now()
WHERE id = @tenant_id AND deleted_at IS NULL
RETURNING *;

-- RestoreTenant clears deleted_at within the grace window (GH #152 undelete).
-- Also requires purge_started_at IS NULL (adversarial-review fast-follow M2):
-- once internal/org.PurgeWorker has begun deleting this tenant's object-storage
-- prefixes, restore must be refused (409 purge_in_progress) rather than
-- resurrecting a tenant whose backup_chunks/snapshot rows may now point at
-- partially-deleted objects. 0 rows means one of: never soft-deleted, already
-- hard-purged, or a purge is in progress — the caller distinguishes via a
-- follow-up GetTenant (deleted_at/purge_started_at).
-- name: RestoreTenant :one
UPDATE tenants
SET deleted_at = NULL
WHERE id = @tenant_id AND deleted_at IS NOT NULL AND purge_started_at IS NULL
RETURNING *;

-- ListTenantsPendingPurge returns tenants whose grace window has elapsed,
-- ready for internal/org.PurgeWorker. tenants carries no RLS (see the schema
-- file header), so this reads across every tenant directly; the caller is a
-- trusted background job, never a per-request handler. Includes tenants whose
-- purge_started_at is already set (a resumed, previously-interrupted purge).
-- name: ListTenantsPendingPurge :many
SELECT * FROM tenants
WHERE deleted_at IS NOT NULL AND deleted_at < @cutoff
ORDER BY deleted_at ASC;

-- MarkPurgeStarted sets the point-of-no-return marker (adversarial-review
-- fast-follow M2). internal/org.PurgeWorker calls this BEFORE the first
-- object-storage delete of its purge pass; RestoreTenant then refuses once
-- this is set. Idempotent by design: 0 rows affected means a PRIOR purge
-- attempt already set it (a resume after a crash) — the caller does not treat
-- that as an error, it just proceeds with the purge.
-- name: MarkPurgeStarted :execrows
UPDATE tenants
SET purge_started_at = now()
WHERE id = @tenant_id AND deleted_at IS NOT NULL AND purge_started_at IS NULL;

-- AdminPurgeTenant delegates to the SECURITY DEFINER admin_purge_tenant
-- function (see its schema.sql doc comment for the full GUC-handling
-- rationale). Unlike AdminDeleteEmptyTenant this has NO emptiness guard: call
-- it ONLY from internal/org.PurgeWorker, after the grace window has elapsed
-- on an org an owner already confirmed deleting.
-- name: AdminPurgeTenant :one
SELECT admin_purge_tenant(@tenant_id) AS purged;
