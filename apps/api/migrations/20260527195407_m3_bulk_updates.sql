-- Create "update_runs" table
CREATE TABLE "update_runs" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "tenant_id" uuid NOT NULL,
  "created_by" uuid NULL,
  "status" text NOT NULL DEFAULT 'pending',
  "dry_run" boolean NOT NULL DEFAULT false,
  "scheduled_at" timestamptz NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "update_runs_created_by_fkey" FOREIGN KEY ("created_by") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE SET NULL,
  CONSTRAINT "update_runs_tenant_id_fkey" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
-- Create index "update_runs_tenant_id_created_at_idx" to table: "update_runs"
CREATE INDEX "update_runs_tenant_id_created_at_idx" ON "update_runs" ("tenant_id", "created_at" DESC);
-- Create "update_tasks" table
CREATE TABLE "update_tasks" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "run_id" uuid NOT NULL,
  "tenant_id" uuid NOT NULL,
  "site_id" uuid NOT NULL,
  "target_type" text NOT NULL,
  "target_slug" text NOT NULL,
  "desired_version" text NOT NULL DEFAULT 'latest',
  "from_version" text NOT NULL DEFAULT '',
  "to_version" text NOT NULL DEFAULT '',
  "status" text NOT NULL DEFAULT 'pending',
  "detail" text NOT NULL DEFAULT '',
  "error" text NOT NULL DEFAULT '',
  "started_at" timestamptz NULL,
  "finished_at" timestamptz NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "update_tasks_run_id_fkey" FOREIGN KEY ("run_id") REFERENCES "update_runs" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "update_tasks_site_id_fkey" FOREIGN KEY ("site_id") REFERENCES "sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "update_tasks_tenant_id_fkey" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
-- Create index "update_tasks_run_id_idx" to table: "update_tasks"
CREATE INDEX "update_tasks_run_id_idx" ON "update_tasks" ("run_id");
-- Create index "update_tasks_site_id_idx" to table: "update_tasks"
CREATE INDEX "update_tasks_site_id_idx" ON "update_tasks" ("site_id");
-- Create index "update_tasks_tenant_id_idx" to table: "update_tasks"
CREATE INDEX "update_tasks_tenant_id_idx" ON "update_tasks" ("tenant_id");

-- ---------------------------------------------------------------------------
-- Row-Level Security (hand-appended; Atlas CE cannot diff policies — ADR-002).
-- ---------------------------------------------------------------------------
-- Both M3 tables are tenant-scoped. Enable + FORCE RLS and isolate on the
-- app.tenant_id GUC, mirroring sites/memberships/api_keys/audit_log.
ALTER TABLE "public"."update_runs" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."update_runs" FORCE ROW LEVEL SECURITY;
CREATE POLICY "update_runs_tenant_isolation" ON "public"."update_runs"
  USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE "public"."update_tasks" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."update_tasks" FORCE ROW LEVEL SECURITY;
CREATE POLICY "update_tasks_tenant_isolation" ON "public"."update_tasks"
  USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- The app role's privileges on these new tables are covered by the
-- ALTER DEFAULT PRIVILEGES grant established in the M1 auth migration (the
-- migration owner creates these tables). No extra GRANT needed.
