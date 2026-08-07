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
-- Newest first, paged by a COMPOSITE (occurred_at, id) keyset cursor, not by
-- OFFSET. This list grows at the head continuously (every tenantless auth event
-- lands here as it happens), so an offset counts from a boundary that has
-- already moved by the time the reader asks for page two, and the rows that
-- shifted past it are shown twice while nothing warns anyone. The cursor names
-- the last row the reader actually saw, so what comes back next is what follows
-- it no matter how much arrived above. The id half of the pair is load-bearing
-- and not decoration: one action writes several rows sharing an occurred_at, and
-- a bare `occurred_at <` would step over the rest of that group (see
-- wpmgr-keyset-cursor-composite).
--
-- First page: pass a far-future @cursor_ts and the max uuid, so the predicate is
-- true for every row.
SELECT * FROM system_audit_log
WHERE (occurred_at, id) < (@cursor_ts::timestamptz, @cursor_id::uuid)
ORDER BY occurred_at DESC, id DESC
LIMIT @row_limit;

-- name: CountSystemAuditEvents :one
SELECT count(*) FROM system_audit_log;
