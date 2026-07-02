-- M5 uptime alerting: per-tenant alert config + per-site alert state.

-- name: GetAlertConfig :one
-- Tenant-scoped read of the tenant's default alert channel.
SELECT * FROM alert_configs
WHERE tenant_id = $1;

-- name: UpsertAlertConfig :one
-- Tenant-scoped create-or-update of the tenant's default alert channel.
INSERT INTO alert_configs (tenant_id, email_recipients, webhook_url, webhook_secret, enabled, notify_security)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (tenant_id) DO UPDATE
SET email_recipients = EXCLUDED.email_recipients,
    webhook_url       = EXCLUDED.webhook_url,
    webhook_secret    = EXCLUDED.webhook_secret,
    enabled           = EXCLUDED.enabled,
    notify_security   = EXCLUDED.notify_security,
    updated_at        = now()
RETURNING *;

-- name: ListAlertConfigsAllTenants :many
-- Cross-tenant enumeration for the evaluator (app.agent GUC). Only enabled
-- configs are returned.
SELECT * FROM alert_configs
WHERE enabled = true;

-- name: GetSiteAlertState :one
-- Cross-tenant read of one site's alert state (app.agent GUC) for the probe job.
SELECT * FROM site_alert_state
WHERE site_id = $1;

-- name: GetSiteAlertStateForUpdate :one
-- Cross-tenant read-and-LOCK of one site's alert state (app.agent GUC), used by
-- the probe worker's read-evaluate-write transition. FOR UPDATE holds the row
-- lock until the enclosing transaction commits, so a second probe sweep that
-- overlaps this one (e.g. a sweep that runs longer than the probe interval
-- during a real outage, before the next periodic sweep is enqueued) blocks on
-- this SELECT until the first sweep's UpsertSiteAlertState commits, then
-- observes the FRESH consecutive_down instead of racing on a stale read. This
-- closes a lost-update window that let consecutive_down get stuck below the
-- alert threshold under overlapping sweeps.
SELECT * FROM site_alert_state
WHERE site_id = $1
FOR UPDATE;

-- name: UpsertSiteAlertState :one
-- Cross-tenant upsert of a site's alert state (app.agent GUC). The probe worker
-- writes the new transition memory after each probe.
INSERT INTO site_alert_state (site_id, tenant_id, last_status, consecutive_down, in_incident, last_alert_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT (site_id) DO UPDATE
SET tenant_id        = EXCLUDED.tenant_id,
    last_status      = EXCLUDED.last_status,
    consecutive_down = EXCLUDED.consecutive_down,
    in_incident      = EXCLUDED.in_incident,
    last_alert_at    = EXCLUDED.last_alert_at,
    updated_at       = now()
RETURNING *;

-- ---------------------------------------------------------------------------
-- Fleet uptime queries (implemented as raw SQL in uptime/repo.go because the
-- LEFT JOIN LATERAL probe columns are nullable and sqlc cannot model that
-- correctly for the bool/time.Time scalar columns; follows the GetFleetDbHealth
-- precedent in perf/repo.go).
--
-- FleetUptimeStatus (InTenantTx, tenant-scoped):
--   SELECT s.id, s.name, s.url, s.connection_state, s.health_status,
--          p.up, p.probed_at, p.total_ms, p.tls_expiry,
--          (7d uptime_pct correlated subquery),
--          (7d avg_latency_ms correlated subquery),
--          COALESCE(ast.in_incident, false)
--   FROM sites s
--   LEFT JOIN LATERAL (latest probe row) p ON true
--   LEFT JOIN site_alert_state ast ON ast.site_id = s.id
--   WHERE s.tenant_id = $1 AND s.id = ANY($2::uuid[])
--   ORDER BY s.name ASC;
--
-- FleetUptimeIncidents (InTenantTx, tenant-scoped):
--   SELECT s.id, s.name, s.url, ast.last_status, ast.last_alert_at, ast.updated_at, p.total_ms
--   FROM site_alert_state ast
--   JOIN sites s ON s.id = ast.site_id AND s.tenant_id = ast.tenant_id
--   LEFT JOIN LATERAL (latest probe) p ON true
--   WHERE ast.tenant_id = $1 AND s.id = ANY($2::uuid[])
--     AND (ast.in_incident = true OR ast.last_alert_at >= $3)
--   ORDER BY ast.last_alert_at DESC NULLS LAST LIMIT $4;
-- ---------------------------------------------------------------------------
