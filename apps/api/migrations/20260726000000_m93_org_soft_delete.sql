-- M93 — owner-facing organisation deletion (GH #152 part 2): soft-delete +
-- grace-window purge.
--
-- Two-lane model (internal/org):
--   Lane A (empty org: zero sites, zero memberships) — unchanged: the
--     existing admin_delete_empty_tenant (m35/m91) hard-deletes immediately.
--   Lane B (populated org) — NEW: tenants.deleted_at is set (soft delete).
--     The org becomes invisible everywhere the instant that commits — every
--     read path that lists a user's orgs, resolves membership, or looks up a
--     tenant by (user, tenant) now excludes deleted_at IS NOT NULL rows (see
--     db/query/tenants.sql, memberships.sql, api_keys.sql). A periodic
--     PurgeWorker (internal/org/purge_worker.go) then does the destructive
--     purge — revoke every connected site, delete the tenant's
--     object-storage prefixes, then admin_purge_tenant (below) — once the
--     grace window (default 7d, WPMGR_ORG_PURGE_GRACE_DAYS) elapses. An owner
--     can undelete via POST /orgs/{orgId}/restore until the worker runs.
--
-- tenants.deleted_at: nullable, NULL = live (the overwhelming default). No
-- RLS change needed — tenants carries no RLS at all (see db/schema.sql's file
-- header); visibility is entirely enforced by the read-path query filters
-- above, not a row-level policy.
--
-- tenants.purge_started_at (adversarial-review fast-follow M2): nullable
-- point-of-no-return marker, distinct from deleted_at. PurgeWorker sets it
-- (MarkPurgeStarted) BEFORE the FIRST object-storage delete of its 7 tenant
-- prefixes — object deletion is irreversible, but a DB-only soft-delete is
-- not, so without this marker a transient storage fault mid-purge (deleted_at
-- still set, some-but-not-all objects gone, lock released) leaves a window
-- where POST /orgs/{orgId}/restore would happily "undelete" an org whose
-- backup_chunks/snapshot rows now point at partially-missing objects —
-- unrestorable backups masquerading as restored ones. RestoreTenant's WHERE
-- clause now also requires purge_started_at IS NULL, so once a purge attempt
-- has begun touching object storage, restore is refused (409
-- purge_in_progress) rather than resurrecting a tenant with silently
-- corrupted backups.
--
-- system_audit_log is a NEW, deliberately tenant-INDEPENDENT log: it carries
-- NO FK to tenants (a plain uuid column) and is NEVER reached by
-- admin_purge_tenant's cascade, so an "org.deleted" / "org.restored" record
-- survives BOTH the Lane-A immediate hard-delete (which wipes the tenant's
-- own audit_log outright) and the Lane-B grace-window purge (which
-- eventually does the same). It carries no RLS for the same reason `tenants`
-- itself carries none: it is not tenant-scoped data, has no per-tenant
-- reader today, and is written only by trusted CP code
-- (internal/org.Handler.recordSystemAudit) — never exposed to a tenant-scoped
-- request.
--
-- admin_purge_tenant (SECURITY DEFINER) is modeled on admin_delete_empty_tenant
-- (m35/m91) but WITHOUT the emptiness guard — it purges a POPULATED tenant,
-- called only by org.PurgeWorker after the grace window and only for a
-- tenant an owner already confirmed deleting via DELETE /api/v1/orgs/{orgId}.
-- It explicitly clears audit_log first (wpmgr_app has no DELETE grant on the
-- append-only trail — the same 42501 story admin_delete_empty_tenant's own
-- comment documents), then deletes the tenants row; every other row the
-- tenant owns (memberships, sites, api_keys, backup_snapshots, backup_chunks,
-- restore_runs, invitations, pairing_codes, billing_events, site_shares,
-- client_members, ...) is removed by that single statement's ON DELETE
-- CASCADE.
--
-- GUC handling — deliberately DIFFERENT from admin_delete_empty_tenant, and
-- this difference matters: admin_delete_empty_tenant only ever purges an
-- EMPTY tenant (its own guard proves zero memberships/sites), so it is safe
-- for it to blank app.tenant_id back to '' before its own `DELETE FROM
-- tenants` — no child-table cascade rows exist there to protect. This
-- function purges a POPULATED tenant: every tenant-scoped table's baseline
-- permissive "<table>_tenant_isolation" policy (USING tenant_id =
-- current_setting('app.tenant_id')) must see p_tenant_id for the ENTIRE
-- cascade the final `DELETE FROM tenants` triggers, exactly as it would if
-- ordinary application code ran the same cascade under InTenantTx (which is
-- just a transaction-scoped SET of this same GUC). So app.tenant_id is set
-- ONCE, at the top, and is NEVER blanked before the tenants delete — only
-- restored, once, on the single return path, per the M91 Finding A GUC-leak
-- lesson (set_config(...,true) is NOT rolled back at function exit — the
-- "true"/is_local flag scopes the change to the CALLER's transaction, not to
-- the function invocation — so an unrestored value would leak into every
-- statement the caller's transaction runs afterward). Blanking it early here
-- — as admin_delete_empty_tenant safely does for its always-empty target —
-- would make every cascaded child-table DELETE see zero visible rows under
-- FORCE ROW LEVEL SECURITY, silently leaving every one of that tenant's rows
-- behind (an orphan leak, not a hard failure) while `tenants` itself still
-- gets removed. No app.agent is needed here at all (unlike
-- admin_delete_empty_tenant, which uses it for its own cross-tenant
-- emptiness EXISTS checks) — this function only ever touches rows scoped to
-- the one p_tenant_id it is given.
--
-- Idempotent throughout: ADD COLUMN IF NOT EXISTS / CREATE TABLE IF NOT
-- EXISTS / CREATE OR REPLACE FUNCTION, safe to re-run (mirrors m91/m92).

ALTER TABLE "public"."tenants" ADD COLUMN IF NOT EXISTS "deleted_at" timestamptz;
ALTER TABLE "public"."tenants" ADD COLUMN IF NOT EXISTS "purge_started_at" timestamptz;

CREATE TABLE IF NOT EXISTS "public"."system_audit_log" (
    "id"          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    "occurred_at" timestamptz NOT NULL DEFAULT now(),
    "actor_type"  text        NOT NULL,
    "actor_id"    uuid,
    "action"      text        NOT NULL,
    "tenant_id"   uuid        NOT NULL,
    "tenant_name" text        NOT NULL,
    "metadata"    jsonb       NOT NULL DEFAULT '{}'::jsonb
);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND indexname = 'system_audit_log_tenant_id_idx'
    ) THEN
        CREATE INDEX "system_audit_log_tenant_id_idx" ON "public"."system_audit_log" ("tenant_id", "occurred_at");
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION "public"."admin_purge_tenant"(p_tenant_id uuid)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_count integer;
    v_prev_tenant text := current_setting('app.tenant_id', true);
BEGIN
    -- Set for the WHOLE function body (see the top-of-file rationale): every
    -- tenant-scoped table's tenant_isolation policy must see p_tenant_id for
    -- the cascade the final DELETE triggers.
    PERFORM set_config('app.tenant_id', p_tenant_id::text, true);

    -- audit_log is insert-only for wpmgr_app (m1 revokes UPDATE/DELETE/
    -- TRUNCATE), so it is cleared explicitly here, as the function OWNER
    -- (which retains DELETE) — mirrors admin_delete_empty_tenant exactly.
    DELETE FROM audit_log WHERE tenant_id = p_tenant_id;

    -- Every other row the tenant owns cascades from this single statement.
    DELETE FROM tenants t WHERE t.id = p_tenant_id;
    GET DIAGNOSTICS v_count = ROW_COUNT;

    -- Single return path: restore the caller's prior app.tenant_id exactly
    -- once (M91 Finding A GUC-leak lesson).
    PERFORM set_config('app.tenant_id', coalesce(v_prev_tenant, ''), true);
    RETURN v_count > 0;
END;
$$;
REVOKE ALL ON FUNCTION "public"."admin_purge_tenant"(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION "public"."admin_purge_tenant"(uuid) TO "wpmgr_app";
