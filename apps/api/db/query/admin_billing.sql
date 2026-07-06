-- M16 Phase C1 — superadmin billing-admin panel (internal/admin). tenants
-- carries no RLS (see schema.sql header), so every query below that touches
-- ONLY tenants runs on the bare pool. Queries that join sites/memberships/
-- backup_chunks/audit_log/billing_events must run under the tx wrapper noted
-- on each query: InAgentTx for a query that spans MULTIPLE tenants in one
-- pass (the *_agent SELECT-only policies — see m92 for backup_chunks_agent/
-- audit_log_agent, which this feature adds; sites_agent/memberships_agent/
-- billing_events_system already existed), InTenantTx(tenantID) for a query
-- scoped to exactly one target tenant (the ordinary tenant_isolation policy
-- already covers that — no agent scope needed).

-- name: AdminAccountPlanStatusCounts :many
-- The instance-wide (plan, plan_status) census: source data for BOTH the
-- accounts-page header tiles (MRR total via internal/billing's price ladder,
-- active subs, past_due count, accounts total) and the revenue page's plan-
-- distribution table + MRR tiles. tenants carries no RLS — bare pool.
-- Deliberately NOT filtered by the accounts-list search/status/plan filters:
-- these tiles always reflect the FULL instance regardless of what the
-- operator is currently filtering the list by.
SELECT plan, plan_status, COUNT(*)::bigint AS cnt
FROM tenants
GROUP BY plan, plan_status;

-- name: AdminListAccounts :many
-- The accounts-list aggregate: one row per tenant with every raw field the
-- superadmin billing panel's list view needs, computed via LEFT JOIN LATERAL
-- (NOT a query-per-tenant). MRR, effective caps, near-limit, and the
-- needs-attention sort are computed in Go (internal/admin/billing_service.go)
-- from internal/billing's price ladder / entitlement resolver — this query
-- only returns the raw counts/timestamps/plan fields those computations need.
--
-- DB-native filters (search/status/plan/past_due/comped/has_overrides) are
-- applied here; idle-90d (which depends on the computed last_activity
-- column) is applied in the outer SELECT over the "acct" CTE, since a
-- LATERAL-computed alias cannot be referenced in the same query's own WHERE
-- clause. near-limit is NOT filterable here (ladder-aware; computed in Go
-- from MaxSites/ManagedStorageBytes, which are Go constants).
--
-- LIMIT 5000 inside the CTE is a safety valve bounding how many tenants this
-- single query will ever join/aggregate in one pass, independent of the
-- caller's requested page size (which is applied AFTER Go-side sort/filter);
-- add real keyset pagination here if an instance ever exceeds it.
--
-- Runs under InAgentTx: sites_agent + memberships_agent + backup_chunks_agent
-- (m92) + audit_log_agent (m92) all gate on app.agent='on'.
WITH owners AS (
    -- A plain (non-LATERAL) derived table joined by an explicit ON below: sqlc
    -- correctly infers this join's columns as nullable (mirrors
    -- AdminRevenueRecentEvents' "LEFT JOIN tenants" below), unlike a scalar
    -- subquery or LATERAL join in the SELECT list, which sqlc's analyzer does
    -- NOT mark nullable even though a zero-row lateral genuinely yields NULL
    -- (the same class of gap LastProcessedBillingEventOccurredAtForTenant's
    -- doc comment describes for aggregates) — DISTINCT ON picks exactly the
    -- first-created owner per tenant.
    SELECT DISTINCT ON (m.tenant_id) m.tenant_id, u.email
    FROM memberships m
    JOIN users u ON u.id = m.user_id
    WHERE m.role = 'owner'
    ORDER BY m.tenant_id, m.created_at ASC
),
acct AS (
    SELECT
        t.id                                        AS tenant_id,
        t.name                                       AS org_name,
        t.slug                                       AS org_slug,
        t.plan,
        t.plan_status,
        t.plan_overrides,
        t.grace_until,
        t.suspended_at,
        t.created_at,
        owners.email                                 AS owner_email,
        billing_count_active_sites(t.id)::bigint      AS sites_used,
        COALESCE(chunks.total_size, 0)::bigint        AS managed_storage_bytes,
        GREATEST(audit_row.last_at, sites_row.last_seen, members_row.last_login) AS last_activity
    FROM tenants t
    LEFT JOIN owners ON owners.tenant_id = t.id
    LEFT JOIN LATERAL (
        SELECT SUM(bc.size)::bigint AS total_size
        FROM backup_chunks bc
        WHERE bc.tenant_id = t.id
    ) chunks ON true
    LEFT JOIN LATERAL (
        SELECT MAX(al.created_at) AS last_at
        FROM audit_log al
        WHERE al.tenant_id = t.id
    ) audit_row ON true
    LEFT JOIN LATERAL (
        SELECT MAX(s.last_seen_at) AS last_seen
        FROM sites s
        WHERE s.tenant_id = t.id
    ) sites_row ON true
    LEFT JOIN LATERAL (
        SELECT MAX(u.last_login_at) AS last_login
        FROM memberships m
        JOIN users u ON u.id = m.user_id
        WHERE m.tenant_id = t.id
    ) members_row ON true
    WHERE
        (@search_filter::bool = false OR
            t.name ILIKE '%' || @search || '%' OR
            t.slug ILIKE '%' || @search || '%' OR
            EXISTS (
                SELECT 1 FROM memberships m2
                JOIN users u2 ON u2.id = m2.user_id
                WHERE m2.tenant_id = t.id AND u2.email ILIKE '%' || @search || '%'
            )
        )
        AND (@status_filter::bool    = false OR t.plan_status = ANY(@statuses::text[]))
        AND (@plan_filter::bool      = false OR t.plan = ANY(@plans::text[]))
        AND (@past_due_filter::bool  = false OR t.plan_status = 'past_due')
        AND (@comped_filter::bool    = false OR t.plan_status = 'comped')
        AND (@overrides_filter::bool = false OR t.plan_overrides <> '{}'::jsonb)
    ORDER BY t.created_at DESC, t.id DESC
    LIMIT 5000
)
SELECT *
FROM acct
WHERE (@idle_filter::bool = false OR
        (last_activity IS NOT NULL AND last_activity < now() - interval '90 days') OR
        (last_activity IS NULL AND created_at < now() - interval '90 days')
      )
ORDER BY created_at DESC, tenant_id DESC;

-- name: AdminGetAccountHeader :one
-- Account-detail header: org identity, plan/status, subscription card fields,
-- comp/suspend state, + the first-owner email. Runs under
-- InTenantTx(@tenant_id): tenants/users are unRLS'd; the owner-email join
-- against memberships is covered by memberships_tenant_isolation once
-- app.tenant_id = @tenant_id (no cross-tenant agent scope needed — this is a
-- single target tenant). The "owner" CTE is joined via a plain LEFT JOIN
-- (mirroring AdminListAccounts' "owners" CTE exactly) — sqlc only infers a
-- LEFT-JOIN-ed column as nullable when the joined relation is a CTE, not an
-- inline derived-table subquery, so this MUST stay a WITH ... AS () CTE.
WITH owner AS (
    SELECT DISTINCT ON (m.tenant_id) m.tenant_id, u.email
    FROM memberships m
    JOIN users u ON u.id = m.user_id
    WHERE m.role = 'owner'
    ORDER BY m.tenant_id, m.created_at ASC
)
SELECT
    t.id, t.name, t.slug, t.plan, t.plan_status, t.plan_overrides,
    t.grace_until, t.billing_provider, t.provider_customer_id,
    t.provider_subscription_id, t.current_period_end,
    t.comp_reason, t.suspended_at, t.suspended_reason, t.cancel_at_period_end,
    t.created_at,
    owner.email AS owner_email
FROM tenants t
LEFT JOIN owner ON owner.tenant_id = t.id
WHERE t.id = @tenant_id;

-- name: AdminGetAccountUsage :one
-- Account-detail usage meters: sites/storage/seats. Runs under
-- InTenantTx(@tenant_id) — billing_count_active_sites() is self-scoping
-- (SECURITY DEFINER, see schema.sql) so it works under any tx context; the
-- storage SUM and seats COUNT are covered by backup_chunks_tenant_isolation /
-- memberships_tenant_isolation once app.tenant_id = @tenant_id.
SELECT
    billing_count_active_sites(@tenant_id)::bigint AS sites_used,
    COALESCE((SELECT SUM(size) FROM backup_chunks WHERE tenant_id = @tenant_id), 0)::bigint AS managed_storage_bytes,
    (SELECT COUNT(*) FROM memberships WHERE tenant_id = @tenant_id)::bigint AS seats_used;

-- name: AdminListAccountMembers :many
-- Account-detail member roster. Runs under InTenantTx(@tenant_id)
-- (memberships_tenant_isolation).
SELECT
    u.id, u.email, u.name, m.role,
    u.status, u.email_verified_at IS NOT NULL AS email_verified,
    u.last_login_at, m.created_at AS member_since
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.tenant_id = @tenant_id
ORDER BY m.created_at ASC, u.id ASC;

-- name: AdminListAccountSites :many
-- Account-detail compact site list. Runs under InTenantTx(@tenant_id)
-- (sites_tenant_isolation).
SELECT id, url, connection_state, created_at
FROM sites
WHERE tenant_id = @tenant_id
ORDER BY created_at DESC, id DESC;

-- name: AdminListAccountBillingEvents :many
-- Account-detail timeline (billing half). Runs under InTenantTx(@tenant_id)
-- (billing_events_tenant_isolation).
SELECT id, provider, kind, occurred_at, payload
FROM billing_events
WHERE tenant_id = @tenant_id
ORDER BY occurred_at DESC, id DESC
LIMIT @row_limit;

-- name: AdminListAccountAuditEvents :many
-- Account-detail timeline (audit half): only the admin.billing.*/billing.*
-- prefixed actions (per the spec's "merged" timeline — everything else in
-- the tenant's full audit trail is already visible via the ordinary Audit
-- Log page). Runs under InTenantTx(@tenant_id) (audit_log_tenant_isolation).
SELECT id, actor_type, actor_id, action, metadata, created_at
FROM audit_log
WHERE tenant_id = @tenant_id
  AND (action LIKE 'admin.billing.%' OR action LIKE 'billing.%')
ORDER BY created_at DESC, id DESC
LIMIT @row_limit;

-- name: AdminGetLastBillingEventForTenant :one
-- The account-detail subscription card's "last billing_events.occurred_at" +
-- staleness flag input. Runs under InTenantTx(@tenant_id)
-- (billing_events_tenant_isolation).
SELECT occurred_at
FROM billing_events
WHERE tenant_id = @tenant_id
ORDER BY occurred_at DESC
LIMIT 1;

-- ---------------------------------------------------------------------------
-- Mutations (all superadmin-only; internal/admin validates + audits around
-- each of these). Every query below runs on the bare pool (tenants carries no
-- RLS) except where noted.
-- ---------------------------------------------------------------------------

-- name: AdminSetTenantComp :exec
-- Grants a manual comp: plan_status='comped', plan=@plan, comp_reason set.
UPDATE tenants
SET plan = @plan, plan_status = 'comped', comp_reason = @comp_reason, updated_at = now()
WHERE id = @tenant_id;

-- name: AdminRevokeCompToFree :exec
-- Reverts a comp when the tenant has no live provider subscription to adopt
-- instead: falls back to the same "never subscribed" resting state a brand
-- new tenant starts in.
UPDATE tenants
SET plan = 'free', plan_status = 'none', comp_reason = NULL, grace_until = NULL, updated_at = now()
WHERE id = @tenant_id;

-- name: AdminClearCompReason :exec
-- Clears comp_reason only, leaving plan/plan_status exactly as just written
-- by billing.Service.ReconcileOneNow (used when a live provider subscription
-- was adopted instead of falling back to free).
UPDATE tenants
SET comp_reason = NULL, updated_at = now()
WHERE id = @tenant_id;

-- name: AdminSetOverrides :exec
-- Persists the FULL resolved plan_overrides object (the caller has already
-- merged the requested deltas onto the existing overrides in Go).
UPDATE tenants
SET plan_overrides = @plan_overrides, updated_at = now()
WHERE id = @tenant_id;

-- name: AdminSetGrace :exec
UPDATE tenants
SET grace_until = @grace_until, updated_at = now()
WHERE id = @tenant_id;

-- name: AdminSetSuspended :exec
UPDATE tenants
SET suspended_at = now(), suspended_reason = @suspended_reason, updated_at = now()
WHERE id = @tenant_id;

-- name: AdminClearSuspended :exec
UPDATE tenants
SET suspended_at = NULL, suspended_reason = NULL, updated_at = now()
WHERE id = @tenant_id;

-- name: AdminForceState :exec
-- The manual force-state escape hatch for webhook drift: sets plan +
-- plan_status directly, clearing grace_until (a forced state is authoritative
-- — it should not inherit a stale grace window from whatever state preceded
-- it).
UPDATE tenants
SET plan = @plan, plan_status = @plan_status, grace_until = NULL, updated_at = now()
WHERE id = @tenant_id;

-- ---------------------------------------------------------------------------
-- Revenue page (internal/admin) — local state only, zero provider API calls.
-- ---------------------------------------------------------------------------

-- name: AdminRevenuePastDueList :many
-- The past-due list: org + owner email + amount (Go computes amount from the
-- ladder price for t.plan) + grace_until (Go computes "days past due" from
-- this using billing.PastDueGracePeriod()) + last payment-failed event. Both
-- are CTEs joined via a plain LEFT JOIN (see AdminGetAccountHeader's "owner"
-- CTE comment for why this must be a CTE, not an inline derived-table
-- subquery, for sqlc to infer the columns nullable). Runs under InAgentTx:
-- the owner join spans multiple tenants (memberships_agent) and the
-- payment-failed join reads billing_events cross-tenant
-- (billing_events_system, already existed pre-m92).
WITH owner AS (
    SELECT DISTINCT ON (m.tenant_id) m.tenant_id, u.email
    FROM memberships m
    JOIN users u ON u.id = m.user_id
    WHERE m.role = 'owner'
    ORDER BY m.tenant_id, m.created_at ASC
),
last_failed AS (
    SELECT DISTINCT ON (be.tenant_id) be.tenant_id, be.occurred_at
    FROM billing_events be
    WHERE be.payload->>'normalized_kind' = 'payment_failed'
    ORDER BY be.tenant_id, be.occurred_at DESC
)
SELECT
    t.id AS tenant_id, t.name AS org_name, t.slug AS org_slug, t.plan,
    t.grace_until,
    owner.email AS owner_email,
    last_failed.occurred_at AS last_payment_failed_at
FROM tenants t
LEFT JOIN owner ON owner.tenant_id = t.id
LEFT JOIN last_failed ON last_failed.tenant_id = t.id
WHERE t.plan_status = 'past_due'
ORDER BY t.grace_until ASC NULLS LAST, t.id ASC;

-- name: AdminRevenueActivationCountsThisMonth :one
-- "new-this-month" / "canceled-this-month": distinct tenants with an
-- activation-shaped / cancellation-shaped billing event this calendar month.
-- Reads payload->>'normalized_kind' (billing.EventActivated /
-- billing.EventCanceled — the PROVIDER-AGNOSTIC normalized kind, not the raw
-- provider event-type string) rather than a provider-specific literal, so
-- this stays correct if a second provider adapter is ever wired. Runs under
-- InAgentTx (billing_events_system spans all tenants).
SELECT
    COUNT(DISTINCT tenant_id) FILTER (
        WHERE payload->>'normalized_kind' = 'activated'
          AND occurred_at >= date_trunc('month', now())
    )::bigint AS new_this_month,
    COUNT(DISTINCT tenant_id) FILTER (
        WHERE payload->>'normalized_kind' = 'canceled'
          AND occurred_at >= date_trunc('month', now())
    )::bigint AS canceled_this_month
FROM billing_events
WHERE tenant_id IS NOT NULL;

-- name: AdminRevenueRecentEvents :many
-- The last 20 billing_events across every tenant, for the revenue page's
-- recent-activity feed. Runs under InAgentTx (billing_events_system).
SELECT be.id, be.provider, be.kind, be.occurred_at, be.created_at,
       be.tenant_id, t.name AS org_name, t.slug AS org_slug
FROM billing_events be
LEFT JOIN tenants t ON t.id = be.tenant_id
ORDER BY be.occurred_at DESC, be.id DESC
LIMIT 20;

-- name: AdminRevenueLastWebhookReceivedAt :one
-- The revenue page's "last webhook received" staleness footer: the newest
-- created_at (CP receipt time, not the provider's own occurred_at) among
-- REAL provider deliveries — provider <> 'admin' excludes the superadmin's
-- own manual billing_events rows (see RecordAdminBillingEvent), which are not
-- webhook deliveries and would otherwise mask a genuinely stalled webhook
-- pipe. Deliberately NOT a bare MAX() aggregate (see
-- LastProcessedBillingEventOccurredAtForTenant's doc comment in billing.sql
-- for why): an empty billing_events table must yield "no rows" rather than a
-- NULL scan panic. Runs under InAgentTx (billing_events_system).
SELECT created_at
FROM billing_events
WHERE provider <> 'admin'
ORDER BY created_at DESC
LIMIT 1;
