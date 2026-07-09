-- name: InsertAuditEntry :one
INSERT INTO audit_log (
    tenant_id, actor_type, actor_id, action, target_type, target_id, metadata, prev_hash, hash, created_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetLastAuditHash :one
SELECT hash FROM audit_log
WHERE tenant_id = $1
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: ListAuditEntries :many
-- Newest-first, matching the web "Audit" page contract (page 1 = most recent;
-- Next/Prev page via OFFSET now walks BACKWARD in time). actor_user_name/
-- actor_key_name/actor_email resolve the acting user's display name + email,
-- or (for an api_key actor) the key's label, via a defensive regex-guarded
-- join: actor_id is a free-text column shared across actor_type in
-- ('user','api_key','system'), so a non-uuid or empty actor_id must never
-- reach the ::uuid cast — it should just yield NULL actor fields rather than
-- erroring the query. Note the regex guard is NOT expressed as a plain
-- `actor_id ~ regex AND id = actor_id::uuid` conjunction in the ON clause:
-- Postgres does not guarantee AND operands are evaluated left-to-right or
-- short-circuited, so the planner is free to evaluate the ::uuid cast before
-- the regex guard for every left-hand row, which throws 22P02 (invalid input
-- syntax for type uuid) for actor_type='system'/'' rows (written by uptime
-- alerts, backups, scans, updates, etc.) well before the guard would have
-- filtered them out. The CASE WHEN below forces the cast to only ever be
-- evaluated when the regex already matched, yielding NULL (safe, no cast) for
-- every non-uuid actor_id. actor_user_name and actor_key_name are mutually
-- exclusive (the two LEFT JOINs are gated on actor_type, so at most one ever
-- matches per row); the audit package merges them into one display name. All
-- three columns are NULL for actor_type='system' and for any actor row that
-- no longer exists. Two separate nullable name columns (instead of a
-- CASE/COALESCE over them) because sqlc cannot infer NULL-safe Go types
-- across a CASE/COALESCE spanning two different LEFT JOINs — see the LEFT
-- JOIN LATERAL note in alerts.sql for the same class of limitation.
SELECT
    al.id, al.tenant_id, al.actor_type, al.actor_id, al.action, al.target_type,
    al.target_id, al.metadata, al.prev_hash, al.hash, al.created_at,
    u.name  AS actor_user_name,
    ak.name AS actor_key_name,
    u.email AS actor_email
FROM audit_log al
LEFT JOIN users u
    ON al.actor_type = 'user'
   AND u.id = CASE WHEN al.actor_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                    THEN al.actor_id::uuid END
LEFT JOIN api_keys ak
    ON al.actor_type = 'api_key'
   AND ak.tenant_id = al.tenant_id
   AND ak.id = CASE WHEN al.actor_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                     THEN al.actor_id::uuid END
WHERE al.tenant_id = $1
ORDER BY al.created_at DESC, al.id DESC
LIMIT $2 OFFSET $3;

-- name: ListAuditEntriesFiltered :many
-- Optional filters: action prefix (LIKE 'prefix%'), site_id. Passing an empty
-- string for action_prefix or a zero UUID for site_id disables those filters
-- respectively. RLS is still the primary tenant-isolation gate; the explicit
-- tenant_id is defense-in-depth.
-- Newest-first (see ListAuditEntries); actor_user_name/actor_key_name/
-- actor_email resolve the same way, including the CASE-guarded ::uuid cast
-- (see ListAuditEntries for the full join contract and why a plain regex-AND
-- guard is NOT safe here).
--
-- The site_id filter (GH #201) matches THREE shapes, because target_id only
-- holds the site id for target_type='site' rows:
--   1. target_type='site' AND target_id = site_id (site lifecycle events).
--   2. metadata->>'site_id' = site_id (backup/restore/update lifecycle rows,
--      whose target_id is a snapshot/run/task id, not the site — the
--      recorders always stamp metadata.site_id for these).
--   3. target_type='backup_schedule' AND target_id = site_id (the one
--      recorder that carries the site id in target_id but never writes
--      metadata.site_id — see backup.recordScheduleChange).
-- metadata->>'site_id' is TEXT-to-TEXT on purpose: it must NEVER be cast to
-- ::uuid. metadata is free-form JSONB and a malformed/absent value must
-- degrade to NULL (never match) rather than throw 22P02 — the same class of
-- bug the CASE-guarded actor join above exists to avoid. ->>'site_id' already
-- returns NULL when the key is absent, so non-site rows are safely excluded
-- without any extra guard.
SELECT
    al.id, al.tenant_id, al.actor_type, al.actor_id, al.action, al.target_type,
    al.target_id, al.metadata, al.prev_hash, al.hash, al.created_at,
    u.name  AS actor_user_name,
    ak.name AS actor_key_name,
    u.email AS actor_email
FROM audit_log al
LEFT JOIN users u
    ON al.actor_type = 'user'
   AND u.id = CASE WHEN al.actor_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                    THEN al.actor_id::uuid END
LEFT JOIN api_keys ak
    ON al.actor_type = 'api_key'
   AND ak.tenant_id = al.tenant_id
   AND ak.id = CASE WHEN al.actor_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                     THEN al.actor_id::uuid END
WHERE al.tenant_id = @tenant_id
  AND (@action_prefix = '' OR al.action LIKE @action_prefix || '%')
  AND (
    @site_id::text = '00000000-0000-0000-0000-000000000000'
    OR (al.target_type = 'site' AND al.target_id = @site_id::text)
    OR (al.metadata->>'site_id' = @site_id::text)
    OR (al.target_type = 'backup_schedule' AND al.target_id = @site_id::text)
  )
ORDER BY al.created_at DESC, al.id DESC
LIMIT  @row_limit
OFFSET @row_offset;

-- name: ListAuditEntriesForVerify :many
SELECT * FROM audit_log
WHERE tenant_id = $1
ORDER BY created_at ASC, id ASC;

-- name: ListAuditEntriesForVerifyFromBaseline :many
-- Same forward walk as ListAuditEntriesForVerify, but starting STRICTLY AFTER
-- a previously-set integrity baseline (audit_integrity_baseline). The
-- composite predicate is required (not a bare `created_at >`) because two
-- appends can legitimately share the same created_at timestamp; a bare
-- comparison could skip or re-include a co-timestamped row. The baseline row
-- itself is excluded — Verify seeds its running prev-hash with baseline_hash
-- before consuming these rows, so this only ever returns entries written
-- after the baseline was taken.
SELECT * FROM audit_log
WHERE tenant_id = @tenant_id
  AND (created_at, id) > (@baseline_created_at::timestamptz, @baseline_id::uuid)
ORDER BY created_at ASC, id ASC;

-- name: GetLatestAuditEntry :one
-- Returns the current chain head (newest row) for a tenant — the anchor point
-- captured by an integrity re-baseline. Not used by Verify itself.
SELECT id, hash, created_at FROM audit_log
WHERE tenant_id = $1
ORDER BY created_at DESC, id DESC
LIMIT 1;
