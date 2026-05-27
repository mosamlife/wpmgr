-- Phase 5 / M1: Auth & Multi-Tenancy.
--
-- This migration runs as the database OWNER (DB.MigrationDSN / bootstrap
-- superuser), NOT as the application role. It:
--   1. Provisions the dedicated application role `wpmgr_app`
--      (NOSUPERUSER NOBYPASSRLS, NOLOGIN — the deployment grants LOGIN+password
--      externally) so the app can connect as a role that RLS actually applies
--      to. Idempotent via a DO block.
--   2. Creates the users / memberships / api_keys / audit_log tables + indexes.
--   3. Enables and FORCEs RLS on the tenant-scoped tables.
--   4. Grants table privileges (and ALTER DEFAULT PRIVILEGES for future tables)
--      to wpmgr_app, and REVOKEs UPDATE/DELETE on audit_log so it is
--      append-only at the privilege level.

-- 1. Application role (idempotent; no hardcoded password — infra grants LOGIN).
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wpmgr_app') THEN
    CREATE ROLE wpmgr_app NOLOGIN NOSUPERUSER NOBYPASSRLS;
  END IF;
END
$$;

-- 2. Tables.
CREATE TABLE "public"."users" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "email" text NOT NULL,
  "password_hash" text NULL,
  "oidc_subject" text NULL,
  "oidc_issuer" text NULL,
  "name" text NOT NULL DEFAULT '',
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  "last_login_at" timestamptz NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "users_email_key" UNIQUE ("email")
);
CREATE UNIQUE INDEX "users_oidc_identity_key" ON "public"."users" ("oidc_issuer", "oidc_subject")
  WHERE (oidc_issuer IS NOT NULL AND oidc_subject IS NOT NULL);

CREATE TABLE "public"."memberships" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "user_id" uuid NOT NULL,
  "tenant_id" uuid NOT NULL,
  "role" text NOT NULL DEFAULT 'viewer',
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "memberships_user_id_fkey" FOREIGN KEY ("user_id") REFERENCES "public"."users" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "memberships_tenant_id_fkey" FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
CREATE UNIQUE INDEX "memberships_user_tenant_key" ON "public"."memberships" ("user_id", "tenant_id");
CREATE INDEX "memberships_tenant_id_idx" ON "public"."memberships" ("tenant_id");
CREATE INDEX "memberships_user_id_idx" ON "public"."memberships" ("user_id");

CREATE TABLE "public"."api_keys" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "tenant_id" uuid NOT NULL,
  "name" text NOT NULL,
  "prefix" text NOT NULL,
  "key_hash" text NOT NULL,
  "role" text NOT NULL DEFAULT 'operator',
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "last_used_at" timestamptz NULL,
  "revoked_at" timestamptz NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "api_keys_tenant_id_fkey" FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
CREATE UNIQUE INDEX "api_keys_prefix_key" ON "public"."api_keys" ("prefix");
CREATE INDEX "api_keys_tenant_id_idx" ON "public"."api_keys" ("tenant_id");

CREATE TABLE "public"."audit_log" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "tenant_id" uuid NOT NULL,
  "actor_type" text NOT NULL,
  "actor_id" text NOT NULL DEFAULT '',
  "action" text NOT NULL,
  "target_type" text NOT NULL DEFAULT '',
  "target_id" text NOT NULL DEFAULT '',
  "metadata" jsonb NOT NULL DEFAULT '{}'::jsonb,
  "prev_hash" text NOT NULL DEFAULT '',
  "hash" text NOT NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "audit_log_tenant_id_fkey" FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
CREATE INDEX "audit_log_tenant_id_created_at_idx" ON "public"."audit_log" ("tenant_id", "created_at");

-- 3. Row-Level Security on the new tenant-scoped tables.
ALTER TABLE "public"."memberships" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."memberships" FORCE ROW LEVEL SECURITY;
CREATE POLICY "memberships_tenant_isolation" ON "public"."memberships"
  USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- Lets a logged-in principal read its OWN memberships across all tenants (for
-- /auth/me + tenant switching), keyed on the app.user_id GUC.
CREATE POLICY "memberships_self_read" ON "public"."memberships"
  FOR SELECT
  USING ("user_id" = nullif(current_setting('app.user_id', true), '')::uuid);

ALTER TABLE "public"."api_keys" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."api_keys" FORCE ROW LEVEL SECURITY;
CREATE POLICY "api_keys_tenant_isolation" ON "public"."api_keys"
  USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- SELECT-only by-prefix lookup for bearer-key authentication (see schema.sql).
CREATE POLICY "api_keys_prefix_lookup" ON "public"."api_keys"
  FOR SELECT
  USING (current_setting('app.apikey_lookup', true) = 'on');

ALTER TABLE "public"."audit_log" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."audit_log" FORCE ROW LEVEL SECURITY;
CREATE POLICY "audit_log_tenant_isolation" ON "public"."audit_log"
  USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- 4. Privileges for the application role.
GRANT USAGE ON SCHEMA "public" TO wpmgr_app;
-- Existing + new tables created in THIS migration:
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA "public" TO wpmgr_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA "public" TO wpmgr_app;
-- Future tables/sequences created by the migration owner default-grant to the app:
ALTER DEFAULT PRIVILEGES IN SCHEMA "public"
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO wpmgr_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA "public"
  GRANT USAGE, SELECT ON SEQUENCES TO wpmgr_app;

-- audit_log is append-only: revoke mutation so neither application bugs nor a
-- compromised app role can rewrite history. INSERT + SELECT only.
REVOKE UPDATE, DELETE, TRUNCATE ON "public"."audit_log" FROM wpmgr_app;
