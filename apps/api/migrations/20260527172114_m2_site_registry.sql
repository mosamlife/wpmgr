-- Modify "sites" table
ALTER TABLE "public"."sites" ADD COLUMN "agent_public_key" text NOT NULL DEFAULT '', ADD COLUMN "enrolled_at" timestamptz NULL, ADD COLUMN "last_seen_at" timestamptz NULL, ADD COLUMN "health_status" text NOT NULL DEFAULT 'unknown', ADD COLUMN "server_info" text NOT NULL DEFAULT '', ADD COLUMN "multisite" boolean NOT NULL DEFAULT false, ADD COLUMN "active_theme" text NOT NULL DEFAULT '', ADD COLUMN "components" jsonb NOT NULL DEFAULT '{}', ADD COLUMN "tags" text[] NOT NULL DEFAULT '{}';
-- Create index "sites_agent_public_key_key" to table: "sites"
CREATE UNIQUE INDEX "sites_agent_public_key_key" ON "public"."sites" ("agent_public_key") WHERE (agent_public_key <> ''::text);
-- Create index "sites_tags_idx" to table: "sites"
CREATE INDEX "sites_tags_idx" ON "public"."sites" USING GIN ("tags");
-- Create "agent_nonces" table
CREATE TABLE "public"."agent_nonces" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "site_id" uuid NOT NULL,
  "nonce" text NOT NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "agent_nonces_site_id_fkey" FOREIGN KEY ("site_id") REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
-- Create index "agent_nonces_created_at_idx" to table: "agent_nonces"
CREATE INDEX "agent_nonces_created_at_idx" ON "public"."agent_nonces" ("created_at");
-- Create index "agent_nonces_site_nonce_key" to table: "agent_nonces"
CREATE UNIQUE INDEX "agent_nonces_site_nonce_key" ON "public"."agent_nonces" ("site_id", "nonce");
-- Create "pairing_codes" table
CREATE TABLE "public"."pairing_codes" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "tenant_id" uuid NOT NULL,
  "code_hash" text NOT NULL,
  "created_by" uuid NULL,
  "site_name" text NOT NULL DEFAULT '',
  "tags" text[] NOT NULL DEFAULT '{}',
  "expires_at" timestamptz NOT NULL,
  "consumed_at" timestamptz NULL,
  "attempts" integer NOT NULL DEFAULT 0,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "pairing_codes_created_by_fkey" FOREIGN KEY ("created_by") REFERENCES "public"."users" ("id") ON UPDATE NO ACTION ON DELETE SET NULL,
  CONSTRAINT "pairing_codes_tenant_id_fkey" FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
-- Create index "pairing_codes_code_hash_key" to table: "pairing_codes"
CREATE UNIQUE INDEX "pairing_codes_code_hash_key" ON "public"."pairing_codes" ("code_hash");
-- Create index "pairing_codes_tenant_id_idx" to table: "pairing_codes"
CREATE INDEX "pairing_codes_tenant_id_idx" ON "public"."pairing_codes" ("tenant_id");

-- ---------------------------------------------------------------------------
-- Row-Level Security (hand-appended; Atlas CE cannot diff policies — ADR-002).
-- ---------------------------------------------------------------------------

-- New policies on the existing sites table for the public enroll path and the
-- agent-auth path, which both precede any tenant scope (gated by GUCs set
-- transaction-locally in InEnrollTx / InAgentTx).
CREATE POLICY "sites_enroll" ON "public"."sites"
  USING (current_setting('app.enroll', true) = 'on')
  WITH CHECK (current_setting('app.enroll', true) = 'on');
CREATE POLICY "sites_agent" ON "public"."sites"
  USING (current_setting('app.agent', true) = 'on')
  WITH CHECK (current_setting('app.agent', true) = 'on');

-- pairing_codes: tenant isolation + the public enroll by-hash path.
ALTER TABLE "public"."pairing_codes" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."pairing_codes" FORCE ROW LEVEL SECURITY;
CREATE POLICY "pairing_codes_tenant_isolation" ON "public"."pairing_codes"
  USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY "pairing_codes_enroll" ON "public"."pairing_codes"
  USING (current_setting('app.enroll', true) = 'on')
  WITH CHECK (current_setting('app.enroll', true) = 'on');

-- agent_nonces: gated solely on the agent path (no tenant scope; the agent
-- identity is the site, resolved by public key).
ALTER TABLE "public"."agent_nonces" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."agent_nonces" FORCE ROW LEVEL SECURITY;
CREATE POLICY "agent_nonces_agent" ON "public"."agent_nonces"
  USING (current_setting('app.agent', true) = 'on')
  WITH CHECK (current_setting('app.agent', true) = 'on');

-- The app role's privileges on these new tables are already covered by the
-- ALTER DEFAULT PRIVILEGES grant established in the M1 auth migration (the
-- migration owner creates these tables). No extra GRANT needed.
