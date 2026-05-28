-- Modify "sites" table: add the per-site age PUBLIC recipient (client-side
-- encryption is on the agent; the control plane stores ONLY the public
-- recipient and never the matching identity, so it cannot decrypt backups).
ALTER TABLE "public"."sites" ADD COLUMN "age_recipient" text NOT NULL DEFAULT '';

-- Create "backup_chunks" table
CREATE TABLE "public"."backup_chunks" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "tenant_id" uuid NOT NULL,
  "blake3" text NOT NULL,
  "s3_key" text NOT NULL,
  "size" bigint NOT NULL DEFAULT 0,
  "refcount" bigint NOT NULL DEFAULT 0,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "backup_chunks_tenant_id_fkey" FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
CREATE UNIQUE INDEX "backup_chunks_tenant_blake3_key" ON "public"."backup_chunks" ("tenant_id", "blake3");
CREATE INDEX "backup_chunks_tenant_id_idx" ON "public"."backup_chunks" ("tenant_id");

-- Create "backup_snapshots" table
CREATE TABLE "public"."backup_snapshots" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "tenant_id" uuid NOT NULL,
  "site_id" uuid NOT NULL,
  "created_by" uuid NULL,
  "kind" text NOT NULL,
  "status" text NOT NULL DEFAULT 'pending',
  "age_recipient" text NOT NULL DEFAULT '',
  "total_size" bigint NOT NULL DEFAULT 0,
  "chunk_count" bigint NOT NULL DEFAULT 0,
  "error" text NOT NULL DEFAULT '',
  "archived" boolean NOT NULL DEFAULT false,
  "started_at" timestamptz NULL,
  "finished_at" timestamptz NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "backup_snapshots_created_by_fkey" FOREIGN KEY ("created_by") REFERENCES "public"."users" ("id") ON UPDATE NO ACTION ON DELETE SET NULL,
  CONSTRAINT "backup_snapshots_site_id_fkey" FOREIGN KEY ("site_id") REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "backup_snapshots_tenant_id_fkey" FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
CREATE INDEX "backup_snapshots_tenant_site_idx" ON "public"."backup_snapshots" ("tenant_id", "site_id", "created_at" DESC);
CREATE INDEX "backup_snapshots_tenant_created_idx" ON "public"."backup_snapshots" ("tenant_id", "created_at" DESC);

-- Create "backup_manifest_entries" table
CREATE TABLE "public"."backup_manifest_entries" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "snapshot_id" uuid NOT NULL,
  "tenant_id" uuid NOT NULL,
  "path" text NOT NULL,
  "entry_kind" text NOT NULL DEFAULT 'file',
  "table_name" text NOT NULL DEFAULT '',
  "chunk_hashes" text[] NOT NULL DEFAULT '{}',
  "size" bigint NOT NULL DEFAULT 0,
  "mode" integer NOT NULL DEFAULT 0,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "backup_manifest_entries_snapshot_id_fkey" FOREIGN KEY ("snapshot_id") REFERENCES "public"."backup_snapshots" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "backup_manifest_entries_tenant_id_fkey" FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
CREATE INDEX "backup_manifest_entries_snapshot_idx" ON "public"."backup_manifest_entries" ("snapshot_id");
CREATE INDEX "backup_manifest_entries_tenant_id_idx" ON "public"."backup_manifest_entries" ("tenant_id");

-- Create "backup_schedules" table
CREATE TABLE "public"."backup_schedules" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "tenant_id" uuid NOT NULL,
  "site_id" uuid NOT NULL,
  "cadence" text NOT NULL DEFAULT 'daily',
  "kind" text NOT NULL DEFAULT 'full',
  "enabled" boolean NOT NULL DEFAULT true,
  "retention_days" integer NOT NULL DEFAULT 30,
  "monthly_archive_keep" integer NOT NULL DEFAULT 12,
  "next_run_at" timestamptz NOT NULL DEFAULT now(),
  "last_run_at" timestamptz NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "backup_schedules_site_id_fkey" FOREIGN KEY ("site_id") REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "backup_schedules_tenant_id_fkey" FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
CREATE UNIQUE INDEX "backup_schedules_site_key" ON "public"."backup_schedules" ("site_id");
CREATE INDEX "backup_schedules_tenant_id_idx" ON "public"."backup_schedules" ("tenant_id");
CREATE INDEX "backup_schedules_due_idx" ON "public"."backup_schedules" ("next_run_at") WHERE enabled;

-- ---------------------------------------------------------------------------
-- Row-Level Security (hand-appended; Atlas CE cannot diff policies — ADR-002).
-- ---------------------------------------------------------------------------
-- All four M4 tables are tenant-scoped. Enable + FORCE RLS and isolate on the
-- app.tenant_id GUC, mirroring sites/update_runs/etc.
ALTER TABLE "public"."backup_chunks" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."backup_chunks" FORCE ROW LEVEL SECURITY;
CREATE POLICY "backup_chunks_tenant_isolation" ON "public"."backup_chunks"
  USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE "public"."backup_snapshots" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."backup_snapshots" FORCE ROW LEVEL SECURITY;
CREATE POLICY "backup_snapshots_tenant_isolation" ON "public"."backup_snapshots"
  USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- Cross-tenant SELECT-only enumeration for the periodic retention GC (under
-- app.agent); the prune itself runs per tenant under the isolation policy.
CREATE POLICY "backup_snapshots_gc" ON "public"."backup_snapshots"
  FOR SELECT
  USING (current_setting('app.agent', true) = 'on');

ALTER TABLE "public"."backup_manifest_entries" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."backup_manifest_entries" FORCE ROW LEVEL SECURITY;
CREATE POLICY "backup_manifest_entries_tenant_isolation" ON "public"."backup_manifest_entries"
  USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE "public"."backup_schedules" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."backup_schedules" FORCE ROW LEVEL SECURITY;
CREATE POLICY "backup_schedules_tenant_isolation" ON "public"."backup_schedules"
  USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- The periodic scheduler enumerates DUE schedules cross-tenant under app.agent
-- (mirrors the health job). SELECT-only; the enqueued backup work runs tenant-
-- scoped under the isolation policy above.
CREATE POLICY "backup_schedules_scheduler" ON "public"."backup_schedules"
  FOR SELECT
  USING (current_setting('app.agent', true) = 'on');

-- The app role's privileges on these new tables are covered by the
-- ALTER DEFAULT PRIVILEGES grant established in the M1 auth migration (the
-- migration owner creates these tables). No extra GRANT needed.
