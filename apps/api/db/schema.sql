-- WPMgr database schema — single source of truth.
--
-- This file is consumed by BOTH sqlc (query codegen) and Atlas (versioned
-- migration diffing). Keep it declarative: it describes the desired end state
-- of the schema, not incremental changes.
--
-- Multi-tenancy is enforced at the database layer via Postgres Row-Level
-- Security (RLS). Every tenant-scoped table has RLS enabled with a policy
-- keyed on the `app.tenant_id` runtime setting, which the application sets
-- per request/transaction (see internal/db.InTenantTx). This makes cross-tenant
-- data leakage impossible even if an application query forgets a WHERE clause.
--
-- IMPORTANT: RLS is bypassed for Postgres SUPERUSERs and roles with the
-- BYPASSRLS attribute. The application MUST therefore connect as a dedicated,
-- non-superuser, non-BYPASSRLS role (e.g. `wpmgr_app`). Use the bootstrap
-- superuser only to run migrations and provision that app role. The default
-- `postgres`/container superuser will silently bypass these policies.

-- ---------------------------------------------------------------------------
-- tenants
-- ---------------------------------------------------------------------------
CREATE TABLE tenants (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL,
    slug       text        NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- sites
-- ---------------------------------------------------------------------------
CREATE TABLE sites (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    url         text        NOT NULL,
    name        text        NOT NULL,
    status      text        NOT NULL DEFAULT 'pending',
    wp_version  text        NOT NULL DEFAULT '',
    php_version text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX sites_tenant_id_idx ON sites (tenant_id);
CREATE UNIQUE INDEX sites_tenant_id_url_key ON sites (tenant_id, url);

-- ---------------------------------------------------------------------------
-- Row-Level Security
-- ---------------------------------------------------------------------------
-- The `sites` table is tenant-scoped. We enable RLS and FORCE it so that even
-- the table owner is subject to the policy (FORCE is required because the app
-- typically connects as the owner of these tables). The policy compares each
-- row's tenant_id against the `app.tenant_id` GUC. We use the two-argument
-- form of current_setting with missing_ok = true so an unset GUC yields NULL
-- (which fails the equality and returns zero rows) rather than erroring.

ALTER TABLE sites ENABLE ROW LEVEL SECURITY;
ALTER TABLE sites FORCE ROW LEVEL SECURITY;

CREATE POLICY sites_tenant_isolation ON sites
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
