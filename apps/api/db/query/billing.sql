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

-- name: FindTenantsByProviderCustomer :many
-- Every tenant pinned to @billing_provider whose stored customer id is
-- @provider_customer_id, ordered by id. Webhook attribution and ownership
-- classification use it when an event names a customer but no tenant.
--
-- The caller decides on the number of rows: none, exactly one, or more than
-- one, and must treat each case explicitly. A Stripe customer id names at most
-- one tenant (tenants_stripe_customer_key); other providers' customer ids are
-- not unique, so more than one row is a real result there.
--
-- Soft-deleted tenants are included on purpose: an event for a deleted
-- workspace must still resolve to it so the caller can alert. tenants has no
-- row security, so this runs on the plain pool or inside any transaction.
SELECT id FROM tenants
WHERE billing_provider = @billing_provider::text
  AND provider_customer_id = @provider_customer_id::text
ORDER BY id;

-- name: LockTenantBilling :exec
-- Takes the per-tenant billing advisory lock for the rest of the calling
-- transaction. Every writer of a tenant's billing state (webhook apply,
-- reconcile, checkout binding, cancel, the org-delete billing check and
-- operator billing writes) serialises on this one key, so the key lives here
-- and nowhere else: a caller that spelled it differently would stop
-- serialising with the others without any error. Must run inside a
-- transaction; outside one the lock is released at once.
SELECT pg_advisory_xact_lock(hashtext('wpmgr_billing:' || (sqlc.arg(tenant_id)::uuid)::text));

-- name: SetCancelRequested :execrows
-- Records a cancel the caller has just asked the provider for, when the
-- provider will not report it back as a schedule. Two uses, chosen by
-- @cancel_now:
--
--   * @cancel_now = true: the Cancel-now marker. Only for a Stripe tenant
--     whose status is still 'past_due'. Sets cancel_at to the transaction's
--     now(), so every later read sees a cancel_at at or before its own now().
--
--   * @cancel_now = false: the local period-end cancel flag. Only for a
--     Razorpay tenant with a live status ('active', 'trialing', 'past_due' or
--     'paused'). cancel_at becomes @cancel_at when that is strictly in the
--     future at the moment of the write, and NULL otherwise, so this form
--     never stores a cancel_at in the past.
--
-- Both set cancel_at_period_end to true. Both write only while the stored
-- subscription id is still @provider_subscription_id, the one the caller
-- cancelled, so a late write can never mark a subscription that replaced it.
-- The caller holds LockTenantBilling. 0 rows means nothing was written: the
-- tenant, its provider, its status or its subscription no longer match.
UPDATE tenants
SET cancel_at_period_end = true,
    cancel_at = CASE
        WHEN @cancel_now::boolean THEN now()
        WHEN sqlc.narg(cancel_at)::timestamptz > clock_timestamp()
            THEN sqlc.narg(cancel_at)::timestamptz
        ELSE NULL
    END,
    updated_at = now()
WHERE id = @tenant_id
  AND provider_subscription_id = @provider_subscription_id::text
  AND CASE
        WHEN @cancel_now::boolean
            THEN billing_provider = 'stripe' AND plan_status = 'past_due'
        ELSE billing_provider = 'razorpay'
             AND plan_status IN ('active', 'trialing', 'past_due', 'paused')
      END;

-- name: ListTenantsForReconcile :many
-- The daily reconcile sweep's tenant set: every tenant that is not comped and
-- has a provider pinned, and either a stored subscription id, or, for Stripe
-- only, a stored customer id with no subscription id. The second half finds a
-- Stripe subscription whose activation never reached this database; the
-- caller looks it up by the returned customer id and nothing else.
--
-- Soft-deleted tenants are included, so a subscription still live on a
-- deleted workspace is found. Not paginated, ordered by id.
SELECT id, billing_provider, provider_customer_id, provider_subscription_id
FROM tenants
WHERE billing_provider IS NOT NULL
  AND plan_status <> 'comped'
  AND (provider_subscription_id IS NOT NULL
       OR (billing_provider = 'stripe' AND provider_customer_id IS NOT NULL))
ORDER BY id;

-- name: ApplyBillingSubscriptionStateForProvider :execrows
-- Persists the state machine's resolved next billing state for one tenant,
-- including the cancel schedule (cancel_at_period_end, cancel_at). The caller
-- holds LockTenantBilling.
--
-- It writes only while the tenant is still pinned to @billing_provider, the
-- provider whose subscription was just read. The caller requires exactly 1
-- row: 0 rows means the pin moved (or the tenant is gone) and nothing was
-- written.
--
-- provider_customer_id is write-once: a stored value is always kept, and
-- @provider_customer_id (ignored when empty) only fills a NULL. A new customer
-- id is stored only by BindCheckoutProvider, never by an apply.
UPDATE tenants
SET plan                     = @plan,
    plan_status              = @plan_status,
    grace_until              = @grace_until,
    current_period_end       = @current_period_end,
    provider_subscription_id = @provider_subscription_id,
    provider_customer_id     = COALESCE(provider_customer_id, NULLIF(@provider_customer_id::text, '')),
    cancel_at_period_end     = @cancel_at_period_end,
    cancel_at                = @cancel_at,
    updated_at               = now()
WHERE id = @tenant_id
  AND billing_provider = @billing_provider::text;

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
--     subscription id and the stored cancel schedule (cancel_at_period_end,
--     cancel_at) are cleared: a schedule belongs to the old provider's
--     subscription, never to the one the new provider will create.
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
    cancel_at_period_end = CASE
        WHEN billing_provider IS DISTINCT FROM @new_provider::text
            THEN false
        ELSE cancel_at_period_end
    END,
    cancel_at = CASE
        WHEN billing_provider IS DISTINCT FROM @new_provider::text
            THEN NULL
        ELSE cancel_at
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
