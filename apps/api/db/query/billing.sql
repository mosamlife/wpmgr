-- name: GetTenantBilling :one
-- Returns the M16 Phase A billing/plan fields used by internal/billing's
-- entitlement resolution. tenants carries no RLS (see schema.sql); every
-- caller already holds a tenant_id it is entitled to query — either the
-- acting tenant's own scope (operator paths) or the tenant_id resolved from a
-- verified pairing code (the public /enroll path, InEnrollTx).
SELECT plan, plan_status, plan_overrides, grace_until, current_period_end
FROM tenants
WHERE id = @tenant_id;

-- name: CountActiveSitesForBilling :one
-- Wraps the SECURITY DEFINER billing_count_active_sites() so the count is
-- correct under any RLS GUC context the caller happens to be running in
-- (operator app.tenant_id OR public-enroll app.enroll) — see the function's
-- own comment in schema.sql for the full rationale.
SELECT billing_count_active_sites(@tenant_id)::bigint AS count;

-- ---------------------------------------------------------------------------
-- M16 Phase B — payment-provider integration.
--
-- tenants carries NO RLS (see schema.sql header), so every query below runs
-- against the plain pool/tx (no InTenantTx/InAgentTx GUC needed) — mirrors
-- internal/tenant's pgRepo, which does the same for its own tenant-row reads.
-- billing_events, by contrast, DOES carry RLS (the m91 tenant/system pairing)
-- and every query against it below is run under InAgentTx (a webhook is a
-- cross-tenant system write, exactly like the m91 comment documents).
-- ---------------------------------------------------------------------------

-- name: GetTenantBillingProfile :one
-- The full Phase-B billing profile for one tenant: checkout/portal/webhook
-- processing need the provider identity + external ids that Phase A's
-- GetTenantBilling (the tight entitlement-resolution hot path) does not
-- select.
--
-- cancel_at_period_end, cancel_at and deleted_at are read here so a caller
-- that decides under the per-tenant billing lock sees the cancel schedule and
-- the soft-delete marker in the same read as the provider identity.
SELECT plan, plan_status, plan_overrides, grace_until,
       billing_provider, provider_customer_id, provider_subscription_id,
       current_period_end, cancel_at_period_end, cancel_at, deleted_at
FROM tenants
WHERE id = @tenant_id;

-- name: FindTenantByProviderCustomer :one
-- The "unknown tenant" fallback attribution path: resolves a tenant from
-- (provider, provider_customer_id) when a webhook event's payload carries no
-- tenant metadata (e.g. an invoice/charge event whose subscription was not
-- expanded). Returns pgx.ErrNoRows when no tenant matches — the caller then
-- treats the event as "unknown customer" (record + warn, change nothing).
SELECT id FROM tenants
WHERE billing_provider = @billing_provider
  AND provider_customer_id = @provider_customer_id;

-- name: SetTenantBillingProviderIfUnset :execrows
-- "One tenant = one provider at a time (set at first checkout)": this only
-- ever writes when billing_provider is still NULL, so a tenant can never be
-- silently re-pointed at a different provider by a later checkout attempt.
--
-- SUPERSEDED by BindCheckoutProvider below. Kept only while the current
-- checkout code still calls it; delete it in the change that moves that
-- caller to BindCheckoutProvider. New code must not call it.
UPDATE tenants
SET billing_provider = @billing_provider
WHERE id = @tenant_id AND billing_provider IS NULL;

-- name: ListTenantsWithProviderSubscription :many
-- The M16 Phase B daily reconcile sweep's tenant set: every tenant with a
-- live provider subscription reference, excluding comped tenants (immune to
-- any provider-driven mutation, webhook or reconcile alike) and any tenant
-- with no provider wired at all. Not paginated: the expected tenant count for
-- this early-stage feature is small; a future pass can add keyset pagination
-- (ORDER BY id already supports it) without changing this query's shape.
SELECT id, billing_provider, provider_subscription_id
FROM tenants
WHERE provider_subscription_id IS NOT NULL
  AND billing_provider IS NOT NULL
  AND plan_status <> 'comped'
ORDER BY id;

-- name: ApplyBillingSubscriptionState :exec
-- Persists the state machine's resolved next tenantBillingProfile
-- (nextBillingState in state_machine.go). provider_customer_id is only
-- overwritten when a non-empty value is supplied (COALESCE over NULLIF)
-- so a caller that does not yet know the customer id (should not happen once
-- a subscription exists, but keeps this query safe to reuse) cannot blank it.
UPDATE tenants
SET plan                     = @plan,
    plan_status               = @plan_status,
    grace_until               = @grace_until,
    current_period_end        = @current_period_end,
    provider_subscription_id  = @provider_subscription_id,
    provider_customer_id      = COALESCE(NULLIF(@provider_customer_id::text, ''), provider_customer_id)
WHERE id = @tenant_id;

-- name: BindCheckoutProvider :one
-- The single write that binds a tenant to the payment provider a checkout
-- uses. The caller holds the per-tenant billing lock, and passes the pin and
-- stored customer it read before any provider call (@expected_provider and
-- @expected_customer, both NULL when nothing was pinned), the provider the
-- checkout asks for (@new_provider), and the provider customer id to store
-- (@customer_id, NULL when there is none).
--
-- It writes exactly one row, and only while billing_pin_is_movable holds for
-- that row (not comped; 'canceled', or 'none' with no stored subscription id)
-- and one of these is true:
--
--   * SAME PROVIDER. The stored pin already equals @new_provider, whatever the
--     caller expected. Nothing moves: the pin is rewritten to the same value,
--     the stored customer is kept (or set to @customer_id when none is
--     stored), and the stored subscription id is kept.
--
--   * MOVE FROM THE CHECKED PAIR. The stored pin and customer still equal
--     (@expected_provider, @expected_customer), compared with IS NOT DISTINCT
--     FROM so NULL matches NULL, and the pin differs from @new_provider. The
--     pin becomes @new_provider, the customer becomes @customer_id, and the
--     subscription id is cleared.
--
-- Anything else changes nothing and returns pgx.ErrNoRows, which the caller
-- answers with 409; it must never retry the write with different expected
-- values without repeating the checks those values stand for.
--
-- The CASE expressions read the row as it was before this UPDATE (Postgres
-- evaluates SET expressions against the old row), so "billing_provider IS
-- DISTINCT FROM @new_provider" is true exactly on the move branch.
--
-- Returns the stored customer id after the write, which may be NULL.
UPDATE tenants
SET billing_provider = @new_provider::text,
    provider_customer_id = CASE
        WHEN billing_provider IS DISTINCT FROM @new_provider::text
            THEN sqlc.narg(customer_id)::text
        ELSE COALESCE(provider_customer_id, sqlc.narg(customer_id)::text)
    END,
    provider_subscription_id = CASE
        WHEN billing_provider IS DISTINCT FROM @new_provider::text
            THEN NULL
        ELSE provider_subscription_id
    END,
    updated_at = now()
WHERE id = @tenant_id
  AND billing_pin_is_movable(plan_status, provider_subscription_id)
  AND (
        billing_provider = @new_provider::text
     OR (billing_provider IS NOT DISTINCT FROM sqlc.narg(expected_provider)::text
         AND provider_customer_id IS NOT DISTINCT FROM sqlc.narg(expected_customer)::text)
  )
RETURNING provider_customer_id;

-- ---------------------------------------------------------------------------
-- billing_events (M91 ledger; M16 Phase B ingestion) — all queries below run
-- under InAgentTx (app.agent = 'on', the billing_events_system RLS policy).
-- ---------------------------------------------------------------------------

-- name: InsertBillingEvent :one
-- ON CONFLICT DO NOTHING makes a replayed webhook delivery a no-op insert
-- (Stripe, like most providers, retries on anything but a 2xx). A caller
-- distinguishes "inserted" from "duplicate" by checking for pgx.ErrNoRows,
-- exactly like email.Repo.InsertWebhookEventDedup.
--
-- @tenant_id is the tenant the event CLAIMS, which may be NULL. The stored
-- tenant_id is that id only when a tenants row with it exists at insert time,
-- and NULL otherwise, so a claim naming a tenant that was never created, or
-- was hard-deleted before the insert, still records the event (with a NULL
-- tenant) instead of failing the insert on the foreign key. The caller keeps
-- the claim in the payload and backfills tenant_id via SetBillingEventTenant
-- once attribution is resolved. A hard delete that commits between the
-- subquery and the foreign-key check can still raise 23503; the caller
-- retries that once with a NULL claim.
INSERT INTO billing_events (
    provider, provider_event_id, kind, tenant_id, payload, occurred_at
) VALUES (
    @provider, @provider_event_id, @kind,
    (SELECT t.id FROM tenants t WHERE t.id = sqlc.narg(tenant_id)::uuid),
    @payload, @occurred_at
)
ON CONFLICT (provider, provider_event_id) DO NOTHING
RETURNING id;

-- name: GetBillingEventByProviderEventID :one
-- The ledger row for one provider event, by its natural key. Intake reads it
-- on a duplicate delivery (to re-enqueue an event whose processing never
-- completed), and the apply worker reads it to load the event it was handed.
-- pgx.ErrNoRows means no such row: never recorded, or removed with its
-- tenant. Runs under InAgentTx (the billing_events_system policy).
SELECT id, provider, provider_event_id, kind, tenant_id, payload,
       occurred_at, processed_at, created_at
FROM billing_events
WHERE provider = @provider
  AND provider_event_id = @provider_event_id;

-- name: SetBillingEventTenant :exec
-- Best-effort backfill once tenant attribution is resolved AFTER the initial
-- insert (the customer-id fallback lookup path). Guarded so it never
-- clobbers an already-attributed row.
UPDATE billing_events
SET tenant_id = @tenant_id
WHERE id = @id AND tenant_id IS NULL;

-- name: MarkBillingEventProcessed :exec
UPDATE billing_events SET processed_at = now() WHERE id = @id;

-- name: GetTenantSuspension :one
-- M16 Phase C1 — the per-request suspension gate's (internal/billing's
-- SuspensionGate, wired on the tenant-scoped /api/v1 group) lightweight read.
-- tenants carries no RLS — bare pool.
SELECT suspended_at, suspended_reason
FROM tenants
WHERE id = @tenant_id;

-- name: LastProcessedBillingEventOccurredAtForTenant :one
-- The out-of-order guard: the newest occurred_at among this tenant's ALREADY
-- APPLIED events (processed_at IS NOT NULL), excluding the event currently
-- being processed. A provider's webhook delivery order is not guaranteed to
-- match event creation order (Stripe documents this explicitly), so a fresh
-- event whose occurred_at is OLDER than this must be ledgered but must NOT be
-- allowed to overwrite a later state the tenant has already reached.
--
-- Deliberately NOT a bare aggregate (SELECT max(occurred_at) ...): sqlc infers
-- an aggregate over occurred_at (a NOT NULL column) as itself NOT NULL, which
-- is wrong for the empty-result-set case (max() over zero rows is NULL) and
-- would panic the row.Scan on this tenant's very first processed event. A
-- plain ORDER BY ... LIMIT 1 instead returns zero rows in that case, which the
-- caller handles the normal way (errors.Is(err, pgx.ErrNoRows)).
SELECT occurred_at
FROM billing_events
WHERE tenant_id = @tenant_id
  AND processed_at IS NOT NULL
  AND id != @exclude_id
ORDER BY occurred_at DESC
LIMIT 1;
