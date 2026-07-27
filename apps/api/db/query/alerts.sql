-- M5 uptime alerting: per-tenant alert config + per-site alert state.

-- name: GetAlertConfig :one
-- Tenant-scoped read of the tenant's default alert channel.
SELECT * FROM alert_configs
WHERE tenant_id = $1;

-- name: UpsertAlertConfig :one
-- Tenant-scoped create-or-update of the tenant's default alert channel.
-- m103 (GH #247): notify_vulns/vuln_min_severity/vuln_include_in_digest are
-- the vulnerability-alerting fields. m108 (GH #291 Phase 3): app_alerts_enabled
-- is the FOURTH signal on this row - independent of `enabled` (the existing
-- reachability channel). The service layer (uptime.Service.SaveAlertConfig)
-- is responsible for merging omitted-on-PUT fields from the existing row
-- before calling this query (see the mergeAlertConfigUpdate / Or(existing.X)
-- pattern in handler.go); this query always writes exactly what it is given,
-- INCLUDING app_alerts_enabled - the caller (uptime.Service.SaveAlertConfig,
-- via the same merge) is responsible for resolving the deployment-fresh
-- default (uptime.Service.GetAlertConfig reads app_alert_rollout for a
-- tenant with no existing row) before this query ever runs, so the column's
-- own DEFAULT only backstops rows written outside the application layer.
INSERT INTO alert_configs (
    tenant_id, email_recipients, webhook_url, webhook_secret, enabled,
    notify_security, notify_vulns, vuln_min_severity, vuln_include_in_digest,
    app_alerts_enabled
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (tenant_id) DO UPDATE
SET email_recipients       = EXCLUDED.email_recipients,
    webhook_url             = EXCLUDED.webhook_url,
    webhook_secret          = EXCLUDED.webhook_secret,
    enabled                 = EXCLUDED.enabled,
    notify_security         = EXCLUDED.notify_security,
    notify_vulns            = EXCLUDED.notify_vulns,
    vuln_min_severity       = EXCLUDED.vuln_min_severity,
    vuln_include_in_digest  = EXCLUDED.vuln_include_in_digest,
    app_alerts_enabled      = EXCLUDED.app_alerts_enabled,
    updated_at              = now()
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
-- site_app_alert_state (m108 - GH #291 Phase 3: app-health alert state,
-- written alongside site_alert_state inside the SAME TransitionAlertState
-- transaction - never a separate round-trip).
-- ---------------------------------------------------------------------------

-- name: GetSiteAppAlertStateForUpdate :one
-- Cross-tenant read-and-LOCK of one site's app-health alert state
-- (app.agent GUC). Mirrors GetSiteAlertStateForUpdate exactly - same
-- overlapping-sweep race the FOR UPDATE lock closes.
SELECT * FROM site_app_alert_state
WHERE site_id = $1
FOR UPDATE;

-- name: UpsertSiteAppAlertState :one
-- Cross-tenant upsert of a site's app-health alert state (app.agent GUC).
INSERT INTO site_app_alert_state (site_id, tenant_id, last_status, consecutive_down, in_incident, ever_app_up, last_alert_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, now())
ON CONFLICT (site_id) DO UPDATE
SET tenant_id        = EXCLUDED.tenant_id,
    last_status      = EXCLUDED.last_status,
    consecutive_down = EXCLUDED.consecutive_down,
    in_incident      = EXCLUDED.in_incident,
    ever_app_up      = EXCLUDED.ever_app_up,
    last_alert_at    = EXCLUDED.last_alert_at,
    updated_at       = now()
RETURNING *;

-- name: GetTenantAppAlertRatio :one
-- The fleet circuit breaker's denominator/numerator (app.agent GUC), scoped
-- to ONE tenant at a time and evaluated once per sweep tick (not per site).
-- Denominator (eligible): sites that could EVER contribute an app alert -
-- ever_app_up = true (the same gate EvaluateApp enforces before firing),
-- app alerting not individually disabled for the site, and not revoked/
-- archived (an operator-initiated "stop managing this site", not a
-- connectivity problem - mirrors deriveFleetStatus's identical exclusion).
-- Numerator (down): the subset of those currently in an open app incident.
-- A site with ever_app_up=false is excluded from BOTH sides: it can never
-- reach the numerator (the same gate), and counting it in the denominator
-- would dilute the ratio and mask a real "every site we can actually watch
-- just went down together" event behind a large population of sites nobody
-- can alert on anyway (e.g. every site with REST permanently blocked).
SELECT
    count(*) AS eligible,
    count(*) FILTER (WHERE sas.in_incident) AS down
FROM site_app_alert_state sas
JOIN sites s ON s.id = sas.site_id AND s.tenant_id = sas.tenant_id
WHERE sas.tenant_id = @tenant_id
  AND sas.ever_app_up = true
  AND s.app_alerts_disabled = false
  AND s.connection_state NOT IN ('revoked', 'archived');

-- name: GetTenantAppAlertBreakerForUpdate :one
-- Cross-tenant read-and-LOCK of one tenant's circuit-breaker state
-- (app.agent GUC). A missing row reads as pgx.ErrNoRows (never tripped yet).
SELECT * FROM tenant_app_alert_breaker
WHERE tenant_id = $1
FOR UPDATE;

-- name: UpsertTenantAppAlertBreaker :one
-- Cross-tenant upsert of a tenant's circuit-breaker state (app.agent GUC).
-- last_down_count (GH #291 Phase 3 Fix 3) is the down count AT THE TIME OF
-- THE LAST NOTIFICATION (trip, update, or recovery) - never bumped on a
-- silent steady-state tick - so the caller can detect "materially worse
-- since we last said anything" without a second table.
INSERT INTO tenant_app_alert_breaker (tenant_id, tripped, tripped_at, last_alert_at, last_down_count, updated_at)
VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (tenant_id) DO UPDATE
SET tripped         = EXCLUDED.tripped,
    tripped_at      = EXCLUDED.tripped_at,
    last_alert_at   = EXCLUDED.last_alert_at,
    last_down_count = EXCLUDED.last_down_count,
    updated_at      = now()
RETURNING *;

-- name: ListTenantAppDownSites :many
-- Cross-tenant (app.agent GUC) listing of every site CURRENTLY counted in
-- GetTenantAppAlertRatio's "down" numerator for one tenant - the IDENTICAL
-- eligibility predicate, joined to sites for a human-readable display name.
-- Used ONLY for the fleet circuit breaker's "updated" aggregate notification
-- (GH #291 Phase 3 Fix 3), which can fire several sweep ticks after the
-- sites it describes actually went down (the update min-interval throttle
-- delays it) - so "this tick's pending transitions" is the WRONG data
-- source for "what is currently affected"; this query reads the live,
-- current truth instead, so the notification never names a stale or
-- incomplete set. row_limit bounds the list for a large tenant; the
-- notification's DownCount/EligibleCount (not this list) are always the
-- authoritative, complete counts.
SELECT COALESCE(NULLIF(s.name, ''), s.url)::text AS display_name
FROM site_app_alert_state sas
JOIN sites s ON s.id = sas.site_id AND s.tenant_id = sas.tenant_id
WHERE sas.tenant_id = @tenant_id
  AND sas.ever_app_up = true
  AND sas.in_incident = true
  AND s.app_alerts_disabled = false
  AND s.connection_state NOT IN ('revoked', 'archived')
ORDER BY s.name ASC, s.url ASC
LIMIT @row_limit;

-- name: ListTrippedAppAlertBreakerTenants :many
-- Cross-tenant enumeration (app.agent GUC) of every tenant whose fleet
-- circuit breaker is CURRENTLY tripped - GH #291 Phase 3 Fix 4. Called ONCE
-- per sweep tick (never per-tenant): a tenant whose down sites simply stop
-- transitioning (already down and staying down - no new FireDown/
-- FireRecovery ever lands in `pending` for it again) would otherwise never
-- get its breaker re-evaluated, and the ratio CAN move for reasons that
-- never touch an individual site's app-alert state at all (a down site gets
-- archived/revoked, or per-site alerting gets disabled, shrinking the
-- eligible/down counts with no AppTransition). The tripped set is small by
-- definition (a breaker only trips when a meaningful fraction of a tenant's
-- sites are simultaneously down), so this stays one cheap, bounded query.
SELECT tenant_id FROM tenant_app_alert_breaker WHERE tripped = true;

-- name: GetAppAlertRolloutDefault :one
-- The m108 deployment-fresh decision. app_alert_rollout carries no tenant
-- dimension and no RLS at all (see its schema.sql doc comment), so this is
-- safe to run under ANY transaction context - callers use whichever
-- tx wrapper they already hold open (uptime.Repo.GetAlertConfig runs it
-- inside the same InTenantTx as its alert_configs read) rather than opening
-- a second transaction. Read by the synthesized zero-value AlertConfig
-- default for a tenant with no persisted alert_configs row yet, so that path
-- and the persisted column's own DEFAULT never disagree.
SELECT fresh_install FROM app_alert_rollout WHERE singleton = true;

-- ---------------------------------------------------------------------------
-- site_incidents (M94 — GH #148: persisted incident history, written
-- alongside site_alert_state inside TransitionAlertState).
-- ---------------------------------------------------------------------------

-- name: OpenIncident :exec
-- Opens a new incident row for a site. Called from TransitionAlertState
-- (app.agent GUC) on a FireDown transition (started_at = now()), and
-- defensively on the "adopt" path when the alert state is already in_incident
-- but no open site_incidents row exists yet — e.g. a site that was already
-- down when m94 shipped and is not covered by the migration's day-1 seed, or
-- a lost FireDown (started_at = the state's last_alert_at in that case). The
-- partial unique index site_incidents_one_open_per_site makes this
-- idempotent: if an incident is already open for the site, the insert is a
-- silent no-op.
INSERT INTO site_incidents (tenant_id, site_id, started_at, peak_status, last_http_status, reason, opened_by)
VALUES (@tenant_id, @site_id, @started_at, 'down', @last_http_status, @reason, 'probe')
ON CONFLICT (site_id) WHERE ended_at IS NULL DO NOTHING;

-- name: CloseIncident :exec
-- Closes the open incident for a site on a FireRecovery transition
-- (app.agent GUC, called from the same TransitionAlertState transaction as
-- OpenIncident above). A no-op (0 rows affected) if no incident is open,
-- which is defensive only — FireRecovery only ever fires when prev.InIncident
-- was true, so a matching open row should already exist.
UPDATE site_incidents
SET ended_at = now(), last_http_status = @last_http_status, updated_at = now()
WHERE site_id = @site_id AND ended_at IS NULL;

-- name: GetIncidentByID :one
-- Tenant-scoped read of one incident, joined with its site's name/url, for
-- GET /api/v1/fleet/incidents/:incidentId (InTenantTx). RLS additionally
-- scopes this by tenant_id; the explicit predicate here is defense-in-depth +
-- index use (site_incidents_tenant_started_idx), matching project convention.
SELECT
    si.id,
    si.site_id,
    si.started_at,
    si.ended_at,
    si.peak_status,
    si.last_http_status,
    si.reason,
    s.name AS site_name,
    s.url  AS site_url
FROM site_incidents si
JOIN sites s ON s.id = si.site_id AND s.tenant_id = si.tenant_id
WHERE si.id = @id AND si.tenant_id = @tenant_id;

-- name: CountRecentIncidents :one
-- 30-day flapping count for the incident-detail endpoint: how many incidents
-- (open or closed) have STARTED for this site in the last 30 days, including
-- the incident being viewed itself.
SELECT count(*) FROM site_incidents
WHERE site_id = @site_id AND tenant_id = @tenant_id
  AND started_at >= now() - interval '30 days';

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
-- GetFleetIncidents (InTenantTx, tenant-scoped) — reads site_incidents
-- directly (M94), not site_alert_state: real persisted incident rows instead
-- of an estimate derived from a single mutable transition-memory row.
--   SELECT si.id, si.site_id, s.name, s.url, si.started_at, si.ended_at,
--          si.last_http_status, (si.ended_at IS NULL) AS ongoing, p.total_ms
--   FROM site_incidents si
--   JOIN sites s ON s.id = si.site_id AND s.tenant_id = si.tenant_id
--   LEFT JOIN LATERAL (latest probe) p ON true
--   WHERE si.tenant_id = $1 AND si.site_id = ANY($2::uuid[])
--     AND (si.ended_at IS NULL OR si.started_at >= $3)
--   ORDER BY si.started_at DESC LIMIT $4;
-- ---------------------------------------------------------------------------
