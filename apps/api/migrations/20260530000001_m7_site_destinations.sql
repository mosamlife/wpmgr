-- M7 / ADR-036 P1 storage adapter foundation.
--
-- Adds the site_destinations table so an operator can route a site's backup
-- chunks to a per-site Local folder or a customer-owned S3-compatible bucket
-- instead of the WPMgr-managed CP bucket. The backup_snapshots row now
-- references the destination it was taken against (NULL = legacy CP-global).
--
-- Threat model:
--   - secret_key_enc is age-encrypted at rest with the CP's identity. The
--     plaintext customer S3 secret never sits on disk in clear.
--   - RLS isolates rows per tenant; the partial unique index enforces at most
--     one default destination per (tenant_id, site_id).
--   - Local destinations carry no S3 credentials (the columns stay empty).

CREATE TABLE "public"."site_destinations" (
    "id"               uuid NOT NULL DEFAULT gen_random_uuid(),
    "tenant_id"        uuid NOT NULL,
    -- Nullable so a future flow can introduce tenant-wide defaults (V1 always
    -- writes a non-null site_id).
    "site_id"          uuid NULL,
    "kind"             text NOT NULL CHECK (kind IN ('cp', 'local', 's3_compat')),
    "label"            text NOT NULL,
    "endpoint"         text NOT NULL DEFAULT '',
    "region"           text NOT NULL DEFAULT '',
    "bucket"           text NOT NULL DEFAULT '',
    "path_prefix"      text NOT NULL DEFAULT '',
    "access_key_id"    text NOT NULL DEFAULT '',
    "secret_key_enc"   bytea NULL,
    "force_path_style" boolean NOT NULL DEFAULT FALSE,
    "is_default"       boolean NOT NULL DEFAULT FALSE,
    "created_at"       timestamptz NOT NULL DEFAULT now(),
    "updated_at"       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY ("id"),
    CONSTRAINT "site_destinations_tenant_id_fkey" FOREIGN KEY ("tenant_id")
        REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
    CONSTRAINT "site_destinations_site_id_fkey" FOREIGN KEY ("site_id")
        REFERENCES "public"."sites" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);

CREATE INDEX "site_destinations_site_idx"
    ON "public"."site_destinations" ("site_id")
    WHERE site_id IS NOT NULL;

-- Exactly one default per (tenant, site). NULL site_id rows can't have a
-- default yet (and we don't create any in V1) — the partial filter scopes the
-- uniqueness correctly.
CREATE UNIQUE INDEX "site_destinations_default_idx"
    ON "public"."site_destinations" ("tenant_id", "site_id")
    WHERE is_default = TRUE AND site_id IS NOT NULL;

-- Snapshots remember which destination they were taken against so the
-- presign router can route reads to the right bucket. NULL means "default
-- CP bucket" — the value every pre-P1 row has.
ALTER TABLE "public"."backup_snapshots"
    ADD COLUMN "destination_id" uuid NULL
    REFERENCES "public"."site_destinations" ("id") ON DELETE SET NULL;

-- ---------------------------------------------------------------------------
-- Row-Level Security (ADR-002: Atlas CE can't diff policies; hand-appended).
-- ---------------------------------------------------------------------------
ALTER TABLE "public"."site_destinations" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."site_destinations" FORCE ROW LEVEL SECURITY;

CREATE POLICY "site_destinations_tenant_isolation" ON "public"."site_destinations"
    USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- The agent-facing presign routing reads destinations under the agent GUC
-- when no tenant is set; the agent path always knows the tenant from the
-- verified Ed25519 identity though, so this policy stays SELECT-only as a
-- defence-in-depth read for cross-tenant maintenance jobs (mirrors the M4
-- backup_snapshots_gc pattern).
CREATE POLICY "site_destinations_agent" ON "public"."site_destinations"
    FOR SELECT
    USING (current_setting('app.agent', true) = 'on');
