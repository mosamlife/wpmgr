-- name: InsertSystemAuditEvent :exec
-- GH #152 — a durable, tenant-INDEPENDENT record of an action whose subject
-- tenant's own hash-chained audit_log is going away (org hard/soft-delete).
-- system_audit_log carries no FK to tenants (see its schema.sql comment) so
-- this row survives both the empty-org (Lane A) immediate hard delete AND the
-- grace-window purge (Lane B) that eventually wipes the tenant + its own
-- audit_log entirely. Called by internal/org.Handler.recordSystemAudit.
INSERT INTO system_audit_log (actor_type, actor_id, action, tenant_id, tenant_name, metadata)
VALUES (@actor_type, @actor_id, @action, @tenant_id, @tenant_name, @metadata);
