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
