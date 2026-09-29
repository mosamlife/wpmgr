-- m149 - billing: tenants.cancel_at, one tenant per provider customer and per
-- provider subscription, an index for audit-key lookups, and the predicate
-- that decides when a tenant's payment-provider binding may move.
--
-- WHAT IT ADDS.
--
--   1. tenants.cancel_at timestamptz, nullable, no default. The instant the
--      provider has scheduled the subscription to end, or ended it, when one
--      is known. NULL means no end is scheduled. It sits beside m92's
--      cancel_at_period_end, which stays as it is.
--
--   2. tenants_stripe_customer_key, UNIQUE (provider_customer_id) for rows
--      whose billing_provider is 'stripe'. A Stripe customer id names at most
--      one tenant. Other providers' customer ids are not constrained.
--
--   3. tenants_provider_subscription_key, UNIQUE (billing_provider,
--      provider_subscription_id) for rows with a stored subscription id. A
--      provider subscription names at most one tenant.
--
--   4. audit_log_audit_key_idx on (tenant_id, metadata->>'audit_key') for rows
--      that carry an audit_key. It backs AuditEntryExistsByKey, which an
--      idempotent audit writer reads under the chain lock before it appends.
--      audit_log gains no column: the key lives in metadata, so it is part of
--      the hashed content like every other metadata field.
--
--   5. billing_pin_is_movable(plan_status, provider_subscription_id), an
--      IMMUTABLE SQL function. It is true exactly when the tenant is not
--      comped and is either 'canceled', or 'none' with no stored subscription
--      id. BindCheckoutProvider and AdminClearBillingPin both call it, so the
--      checkout binding and the operator clear accept and refuse the same
--      states by construction. It reads no table, so it is inlined into the
--      caller's WHERE clause and needs no search_path setting.
--
-- NO BACKFILL. cancel_at starts NULL on every row, which is the correct value
-- until the provider reports a schedule. Nothing here writes to an existing
-- row, so the FORCE ROW LEVEL SECURITY on audit_log does not affect this file;
-- tenants carries no row security.
--
-- PRE-CHECK BEFORE DEPLOY. Items 2 and 3 fail with 23505 if two tenants
-- already share a Stripe customer id, or a provider subscription id, and a
-- failing migration fails the boot with the previous revision left serving.
-- Run the read-only duplicate checks for both keys before deploying:
--
--   SELECT provider_customer_id, count(*) FROM tenants
--    WHERE billing_provider = 'stripe' AND provider_customer_id IS NOT NULL
--    GROUP BY 1 HAVING count(*) > 1;
--   SELECT billing_provider, provider_subscription_id, count(*) FROM tenants
--    WHERE provider_subscription_id IS NOT NULL
--    GROUP BY 1, 2 HAVING count(*) > 1;
--
-- Both must return no rows.
--
-- LOCK WAIT. Each index build takes a SHARE lock on its table for the length
-- of the build. This file waits at most five seconds for any lock, and a
-- timeout rolls the whole file back and fails the boot.
--
-- IDEMPOTENT. ADD COLUMN IF NOT EXISTS, CREATE INDEX IF NOT EXISTS and CREATE
-- OR REPLACE FUNCTION, so a re-run changes nothing.
--
-- ORDINAL. 20260929130000 sorts after every migration on main and in every
-- open branch when this was written, the latest being m148 at 20260929120000.
--
-- RLS. No new table and no new policy. tenants has no row security;
-- audit_log keeps its ENABLE and FORCE ROW LEVEL SECURITY and its policies,
-- which cover an index without a statement.

SET LOCAL lock_timeout = '5s';

ALTER TABLE "public"."tenants" ADD COLUMN IF NOT EXISTS "cancel_at" timestamptz;

CREATE UNIQUE INDEX IF NOT EXISTS "tenants_stripe_customer_key"
    ON "public"."tenants" ("provider_customer_id")
    WHERE billing_provider = 'stripe' AND provider_customer_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS "tenants_provider_subscription_key"
    ON "public"."tenants" ("billing_provider", "provider_subscription_id")
    WHERE provider_subscription_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS "audit_log_audit_key_idx"
    ON "public"."audit_log" ("tenant_id", (metadata ->> 'audit_key'))
    WHERE metadata ? 'audit_key';

CREATE OR REPLACE FUNCTION "public"."billing_pin_is_movable"(
    p_plan_status text,
    p_provider_subscription_id text
)
RETURNS boolean
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT coalesce(
        p_plan_status <> 'comped'
        AND (p_plan_status = 'canceled'
             OR (p_plan_status = 'none' AND p_provider_subscription_id IS NULL)),
        false)
$$;
