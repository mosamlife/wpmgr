-- Phase 5.5 One-Click Login: per-site policy + single-use nonce table.
-- ADR-030/031: the nonce id is the JWT jti; PG is the source of truth, Redis is
-- the sub-ms hot-path consume (both SET on mint, atomically GETDEL'd on consume,
-- with PG UPDATE consumed_at on either path so the audit row reflects truth).

-- Create "autologin_policies" table
CREATE TABLE "public"."autologin_policies" (
  "site_id" uuid NOT NULL,
  "tenant_id" uuid NOT NULL,
  "enabled" boolean NOT NULL DEFAULT true,
  "allowed_wp_roles" text[] NOT NULL DEFAULT ARRAY['administrator'],
  "require_2fa_step_up" boolean NOT NULL DEFAULT false,
  "max_session_age_minutes" integer NOT NULL DEFAULT 30,
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("site_id"),
  CONSTRAINT "autologin_policies_site_id_fkey" FOREIGN KEY ("site_id") REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "autologin_policies_tenant_id_fkey" FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
CREATE INDEX "autologin_policies_tenant_id_idx" ON "public"."autologin_policies" ("tenant_id");

-- Create "autologin_tokens" table
CREATE TABLE "public"."autologin_tokens" (
  "id" text NOT NULL,
  "tenant_id" uuid NOT NULL,
  "site_id" uuid NOT NULL,
  "initiator_user_id" uuid NOT NULL,
  "target_wp_user_login" text NOT NULL DEFAULT '',
  "initiator_ip" inet NULL,
  "initiator_user_agent" text NOT NULL DEFAULT '',
  "expires_at" timestamptz NOT NULL,
  "consumed_at" timestamptz NULL,
  "consumed_from_ip" inet NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "autologin_tokens_initiator_user_id_fkey" FOREIGN KEY ("initiator_user_id") REFERENCES "public"."users" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "autologin_tokens_site_id_fkey" FOREIGN KEY ("site_id") REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "autologin_tokens_tenant_id_fkey" FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
CREATE INDEX "autologin_tokens_tenant_id_idx" ON "public"."autologin_tokens" ("tenant_id");
CREATE INDEX "autologin_tokens_pending_expiry_idx" ON "public"."autologin_tokens" ("expires_at") WHERE (consumed_at IS NULL);

-- ---------------------------------------------------------------------------
-- Row-Level Security (hand-appended; Atlas does not diff RLS policies).
-- ---------------------------------------------------------------------------
ALTER TABLE "public"."autologin_tokens" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."autologin_tokens" FORCE ROW LEVEL SECURITY;
-- Operator/mint path: tenant isolation (the mint runs under app.tenant_id).
CREATE POLICY "autologin_tokens_tenant_isolation" ON "public"."autologin_tokens"
  USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- Consume path: the agent presents the nonce + verified site_id, BEFORE any
-- tenant scope exists. Mirrors sites_agent / agent_nonces_agent. SELECT+UPDATE
-- only; INSERT/DELETE remain tenant-scoped.
CREATE POLICY "autologin_tokens_agent" ON "public"."autologin_tokens"
  FOR SELECT
  USING (current_setting('app.agent', true) = 'on');
CREATE POLICY "autologin_tokens_agent_consume" ON "public"."autologin_tokens"
  FOR UPDATE
  USING (current_setting('app.agent', true) = 'on')
  WITH CHECK (current_setting('app.agent', true) = 'on');

ALTER TABLE "public"."autologin_policies" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."autologin_policies" FORCE ROW LEVEL SECURITY;
CREATE POLICY "autologin_policies_tenant_isolation" ON "public"."autologin_policies"
  USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- The consume path needs allowed_wp_roles before tenant scope is known. SELECT
-- only — the agent never mutates policies.
CREATE POLICY "autologin_policies_agent" ON "public"."autologin_policies"
  FOR SELECT
  USING (current_setting('app.agent', true) = 'on');

-- The app role's privileges on these new tables are covered by the
-- ALTER DEFAULT PRIVILEGES grant established in the M1 auth migration (the
-- migration owner creates these tables). No extra GRANT needed.
