-- M5 uptime monitoring alerting: per-tenant alert channel config + per-site
-- alert transition state. (Uptime check time-series live in ClickHouse, not
-- Postgres; Postgres stays the system of record for config + transition memory.)

-- Create "alert_configs" table
CREATE TABLE "public"."alert_configs" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "tenant_id" uuid NOT NULL,
  "email_recipients" text[] NOT NULL DEFAULT '{}',
  "webhook_url" text NOT NULL DEFAULT '',
  "webhook_secret" text NOT NULL DEFAULT '',
  "enabled" boolean NOT NULL DEFAULT true,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "alert_configs_tenant_id_fkey" FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
CREATE UNIQUE INDEX "alert_configs_tenant_key" ON "public"."alert_configs" ("tenant_id");

-- Create "site_alert_state" table
CREATE TABLE "public"."site_alert_state" (
  "site_id" uuid NOT NULL,
  "tenant_id" uuid NOT NULL,
  "last_status" text NOT NULL DEFAULT 'unknown',
  "consecutive_down" integer NOT NULL DEFAULT 0,
  "in_incident" boolean NOT NULL DEFAULT false,
  "last_alert_at" timestamptz NULL,
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("site_id"),
  CONSTRAINT "site_alert_state_site_id_fkey" FOREIGN KEY ("site_id") REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "site_alert_state_tenant_id_fkey" FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
CREATE INDEX "site_alert_state_tenant_id_idx" ON "public"."site_alert_state" ("tenant_id");

-- ---------------------------------------------------------------------------
-- Row-Level Security (hand-appended; Atlas does not diff RLS policies).
-- ---------------------------------------------------------------------------
ALTER TABLE "public"."alert_configs" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."alert_configs" FORCE ROW LEVEL SECURITY;
CREATE POLICY "alert_configs_tenant_isolation" ON "public"."alert_configs"
  USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- The alert evaluator enumerates configs cross-tenant under app.agent (mirrors
-- the health/scheduler jobs). SELECT-only.
CREATE POLICY "alert_configs_evaluator" ON "public"."alert_configs"
  FOR SELECT
  USING (current_setting('app.agent', true) = 'on');

ALTER TABLE "public"."site_alert_state" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."site_alert_state" FORCE ROW LEVEL SECURITY;
CREATE POLICY "site_alert_state_tenant_isolation" ON "public"."site_alert_state"
  USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- The probe worker upserts state cross-tenant under app.agent (it iterates all
-- enrolled sites, like the health job). Full lifecycle permitted under the GUC.
CREATE POLICY "site_alert_state_agent" ON "public"."site_alert_state"
  USING (current_setting('app.agent', true) = 'on')
  WITH CHECK (current_setting('app.agent', true) = 'on');

-- The app role's privileges on these new tables are covered by the
-- ALTER DEFAULT PRIVILEGES grant established in the M1 auth migration (the
-- migration owner creates these tables). No extra GRANT needed.
