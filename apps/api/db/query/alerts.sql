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
