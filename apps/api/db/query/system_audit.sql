-- name: InsertSystemAuditEvent :exec
-- GH #152 — a durable, tenant-INDEPENDENT record of an action whose subject
-- tenant's own hash-chained audit_log is going away (org hard/soft-delete).
-- system_audit_log carries no FK to tenants (see its schema.sql comment) so
-- this row survives both the empty-org (Lane A) immediate hard delete AND the
-- grace-window purge (Lane B) that eventually wipes the tenant + its own
-- audit_log entirely. Called by internal/org.Handler.recordSystemAudit.
INSERT INTO system_audit_log (actor_type, actor_id, action, tenant_id, tenant_name, metadata)
VALUES (@actor_type, @actor_id, @action, @tenant_id, @tenant_name, @metadata);

-- name: ListSystemAuditEvents :many
-- The READER for system_audit_log, served by GET /api/v1/admin/system-audit.
--
-- A log with no reader is not oversight. This table now carries the auth events
-- of accounts that belong to no organisation (a brand new social account, a
-- site collaborator, a portal user, anyone mid soft-delete grace window), and
-- those cannot appear in any tenant's own audit_log by construction, so without
-- this query the population with the least visibility would have had none at
-- all. Superadmin-gated, because the rows span every account on the install.
--
-- Newest first with an id tiebreaker: rows written in one action share
-- occurred_at, and a bare occurred_at sort would let paging skip or repeat them.
SELECT * FROM system_audit_log
ORDER BY occurred_at DESC, id DESC
LIMIT @row_limit OFFSET @row_offset;

-- name: CountSystemAuditEvents :one
SELECT count(*) FROM system_audit_log;
