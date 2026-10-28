-- Client Reports queries (M64 — White-label client reports Phase 2).
-- All queries are tenant-scoped both explicitly (tenant_id in WHERE/VALUES)
-- and by RLS. The repo wraps each call in the appropriate tx helper
-- (InTenantTx for operator path, InAgentTx for the due-scanner / worker).
-- updated_at is set by now() in mutations (project convention, no trigger).

-- ---------------------------------------------------------------------------
-- Shared tenant name lookup (used by the aggregator for agency branding).
-- ---------------------------------------------------------------------------

-- name: GetTenantName :one
SELECT name FROM tenants WHERE id = @id;

-- ---------------------------------------------------------------------------
-- clients.timezone (D-0: client-level timezone field, decision 6)
-- ---------------------------------------------------------------------------

-- name: UpdateClientTimezone :one
UPDATE clients
SET timezone   = @timezone,
    updated_at = now()
WHERE id = @id AND tenant_id = @tenant_id
RETURNING *;

-- name: GetClientWithTimezone :one
SELECT * FROM clients WHERE id = @id AND tenant_id = @tenant_id;

-- ---------------------------------------------------------------------------
-- report_schedules — singleton schedule per client
-- ---------------------------------------------------------------------------

-- name: GetReportSchedule :one
SELECT * FROM report_schedules
WHERE client_id = @client_id AND tenant_id = @tenant_id;

-- name: UpsertReportSchedule :one
INSERT INTO report_schedules (
    tenant_id, client_id, enabled, cadence, send_day, send_hour,
    recipients, sections, intro_text, closing_text, powered_by_removed,
    next_run_at, updated_at
) VALUES (
    @tenant_id, @client_id, @enabled, @cadence, @send_day, @send_hour,
    @recipients, @sections, @intro_text, @closing_text, @powered_by_removed,
    @next_run_at, now()
)
ON CONFLICT (client_id) DO UPDATE
    SET enabled            = EXCLUDED.enabled,
        cadence            = EXCLUDED.cadence,
        send_day           = EXCLUDED.send_day,
        send_hour          = EXCLUDED.send_hour,
        recipients         = EXCLUDED.recipients,
        sections           = EXCLUDED.sections,
        intro_text         = EXCLUDED.intro_text,
        closing_text       = EXCLUDED.closing_text,
        powered_by_removed = EXCLUDED.powered_by_removed,
        next_run_at        = EXCLUDED.next_run_at,
        updated_at         = now()
RETURNING *;

-- name: ListDueReportSchedules :many
-- Returns up to @limit enabled schedules where next_run_at <= now(), JOINed
-- with the client for timezone/name/contact_email. Runs under InAgentTx.
SELECT
    rs.*,
    c.name          AS client_name,
    c.contact_email AS client_contact_email,
    c.timezone      AS client_timezone
FROM report_schedules rs
JOIN clients c ON c.id = rs.client_id AND c.tenant_id = rs.tenant_id
WHERE rs.enabled
  AND rs.next_run_at <= now()
ORDER BY rs.next_run_at ASC
LIMIT @row_limit;

-- name: ClaimAdvanceReportSchedule :one
-- Atomically advances next_run_at to @new_next_run_at for a schedule row,
-- recording last_run_at = now(). Returns no row when the claim races
-- (already claimed by another worker). Runs under InAgentTx.
UPDATE report_schedules
SET next_run_at = @new_next_run_at,
    last_run_at = now(),
    updated_at  = now()
WHERE id = @id
  AND tenant_id = @tenant_id
  AND enabled
  AND next_run_at <= now()
RETURNING *;

-- ---------------------------------------------------------------------------
-- generated_reports — one row per rendered report
-- ---------------------------------------------------------------------------

-- name: CreateReport :one
INSERT INTO generated_reports (
    tenant_id, client_id, schedule_id, period_start, period_end, status
) VALUES (
    @tenant_id, @client_id, @schedule_id, @period_start, @period_end, 'pending'
)
RETURNING *;

-- name: MarkReportGenerating :one
UPDATE generated_reports
SET status = 'generating',
    updated_at = now()
WHERE id = @id AND tenant_id = @tenant_id
RETURNING *;

-- name: CompleteReport :one
UPDATE generated_reports
SET status        = 'completed',
    html_blob_key = @html_blob_key,
    pdf_blob_key  = @pdf_blob_key,
    data_snapshot = @data_snapshot,
    completed_at  = now(),
    updated_at    = now()
WHERE id = @id AND tenant_id = @tenant_id
RETURNING *;

-- name: FailReport :one
UPDATE generated_reports
SET status     = 'failed',
    error      = @error,
    updated_at = now()
WHERE id = @id AND tenant_id = @tenant_id
RETURNING *;

-- name: GetReport :one
SELECT * FROM generated_reports
WHERE id = @id AND tenant_id = @tenant_id AND client_id = @client_id;

-- name: ListReports :many
-- Keyset cursor pagination: composite predicate (created_at, id) < (cursor_at, cursor_id)
-- because batch inserts can share created_at and a bare compare skips co-timestamped rows
-- (standing keyset-cursor-composite rule).
SELECT * FROM generated_reports
WHERE tenant_id  = @tenant_id
  AND client_id  = @client_id
  AND (
        sqlc.narg('cursor_created_at')::timestamptz IS NULL
        OR (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
      )
ORDER BY created_at DESC, id DESC
LIMIT @row_limit;

-- name: DeleteReport :execrows
DELETE FROM generated_reports
WHERE id = @id AND tenant_id = @tenant_id AND client_id = @client_id;

-- ---------------------------------------------------------------------------
-- Per-section data queries for the aggregator
-- ---------------------------------------------------------------------------

-- name: GetBackupReportStats :one
-- Backup section: completed snapshots in [from, to) for a site.
SELECT
    COUNT(*)::bigint                    AS completed_count,
    COALESCE(SUM(total_size), 0)::bigint AS total_bytes,
    MAX(finished_at)                    AS last_completed_at
FROM backup_snapshots
WHERE tenant_id  = @tenant_id
  AND site_id    = @site_id
  AND status     = 'completed'
  AND finished_at >= @from_time
  AND finished_at  < @to_time;

-- name: GetUpdateReportStats :many
-- Update section: succeeded/failed tasks grouped by target_type in [from, to).
-- Only plugin, theme and core tasks count (GH #969): the agent's own
-- self-update tasks, and any other target_type, are not updates a client
-- report describes, so they are in neither column.
SELECT
    target_type,
    COUNT(*) FILTER (WHERE status = 'succeeded')               AS succeeded,
    COUNT(*) FILTER (WHERE status IN ('failed','rolled_back')) AS failed
FROM update_tasks
WHERE tenant_id  = @tenant_id
  AND site_id    = @site_id
  AND finished_at >= @from_time
  AND finished_at  < @to_time
  AND target_type IN ('plugin','theme','core')
GROUP BY target_type;

-- name: ListReportUpdateItems :many
-- Update section version table (GH #969): one row per finished plugin, theme
-- or core task on the given sites in [from, to), oldest first within a site.
-- Same population as GetUpdateReportStats, so the table and the counts agree:
-- succeeded, failed and rolled_back only, agent self-updates excluded.
--
-- Runs under InAgentTx, where RLS admits every tenant's update_tasks rows, so
-- tenant_id = @tenant_id is the tenant boundary: a site id belonging to
-- another tenant in @site_ids matches nothing. @row_limit caps the result.
--
-- from_version and to_version may each be the empty string (version unknown),
-- and a rollback records from_version = to_version. `, id` is the project
-- ORDER BY tiebreaker.
SELECT site_id, target_type, target_slug, from_version, to_version, status, finished_at
FROM update_tasks
WHERE tenant_id   = @tenant_id
  AND site_id     = ANY(@site_ids::uuid[])
  AND finished_at >= @from_time
  AND finished_at  < @to_time
  AND target_type IN ('plugin','theme','core')
  AND status      IN ('succeeded','failed','rolled_back')
ORDER BY site_id, finished_at, id
LIMIT @row_limit;

-- ---------------------------------------------------------------------------
-- m66 — Portal report queries
-- ---------------------------------------------------------------------------

-- name: ListCompletedReportsForClients :many
-- Portal report list: completed reports for a set of client IDs. The
-- client_id = ANY(@client_ids) predicate is the cross-client isolation gate
-- (generated_reports has no site_scope restrictive policy). Only
-- status='completed' rows are returned — the portal must not surface
-- pending/generating/failed states (locked decision, section 8).
-- Keyset cursor: composite (created_at, id) predicate matches ListReports.
SELECT id, tenant_id, client_id, period_start, period_end,
       status, created_at, completed_at
FROM generated_reports
WHERE tenant_id = @tenant_id
  AND client_id = ANY(@client_ids::uuid[])
  AND status    = 'completed'
  AND (
        sqlc.narg('cursor_created_at')::timestamptz IS NULL
        OR (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
      )
ORDER BY created_at DESC, id DESC
LIMIT @row_limit;

-- name: GetCompletedReportForPortal :one
-- Load a single completed report checking client_id membership in the caller's
-- client list. Returns ErrNoRows when the report is not found, not completed,
-- or the client_id is not in the provided list (cross-client IDOR guard).
SELECT id, tenant_id, client_id, period_start, period_end,
       status, html_blob_key, pdf_blob_key, created_at, completed_at
FROM generated_reports
WHERE id        = @id
  AND tenant_id = @tenant_id
  AND client_id = ANY(@client_ids::uuid[])
  AND status    = 'completed';
