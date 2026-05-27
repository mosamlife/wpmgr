-- Create "tenants" table
CREATE TABLE "public"."tenants" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "name" text NOT NULL,
  "slug" text NOT NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "tenants_slug_key" UNIQUE ("slug")
);
-- Create "sites" table
CREATE TABLE "public"."sites" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "tenant_id" uuid NOT NULL,
  "url" text NOT NULL,
  "name" text NOT NULL,
  "status" text NOT NULL DEFAULT 'pending',
  "wp_version" text NOT NULL DEFAULT '',
  "php_version" text NOT NULL DEFAULT '',
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "sites_tenant_id_fkey" FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
-- Create index "sites_tenant_id_idx" to table: "sites"
CREATE INDEX "sites_tenant_id_idx" ON "public"."sites" ("tenant_id");
-- Create index "sites_tenant_id_url_key" to table: "sites"
CREATE UNIQUE INDEX "sites_tenant_id_url_key" ON "public"."sites" ("tenant_id", "url");
-- Enable Row-Level Security on tenant-scoped "sites" and add the isolation
-- policy keyed on the app.tenant_id runtime setting. FORCE applies the policy
-- even to the table owner (the role the application connects as).
ALTER TABLE "public"."sites" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."sites" FORCE ROW LEVEL SECURITY;
CREATE POLICY "sites_tenant_isolation" ON "public"."sites"
  USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
