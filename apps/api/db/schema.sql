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

-- ---------------------------------------------------------------------------
-- users
-- ---------------------------------------------------------------------------
-- A user is a human principal. Users span tenants (a user may belong to many
-- tenants via memberships), so the users table is NOT tenant-scoped/RLS'd.
-- password_hash is NULL for OIDC-only users; oidc_subject+oidc_issuer are NULL
-- for password-only users. A user may have both (link an OIDC identity to a
-- password account). The (oidc_issuer, oidc_subject) pair is unique when set.
CREATE TABLE users (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text        NOT NULL UNIQUE,
    password_hash text,
    oidc_subject  text,
    oidc_issuer   text,
    name          text        NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz
);

CREATE UNIQUE INDEX users_oidc_identity_key
    ON users (oidc_issuer, oidc_subject)
    WHERE oidc_issuer IS NOT NULL AND oidc_subject IS NOT NULL;

-- ---------------------------------------------------------------------------
-- memberships
-- ---------------------------------------------------------------------------
-- Join table binding a user to a tenant with a role. Tenant-scoped: RLS keeps a
-- session scoped to one tenant from reading another tenant's membership rows.
CREATE TABLE memberships (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    tenant_id  uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    role       text        NOT NULL DEFAULT 'viewer',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX memberships_user_tenant_key ON memberships (user_id, tenant_id);
CREATE INDEX memberships_tenant_id_idx ON memberships (tenant_id);
CREATE INDEX memberships_user_id_idx ON memberships (user_id);

-- ---------------------------------------------------------------------------
-- api_keys
-- ---------------------------------------------------------------------------
-- Tenant-scoped machine principals. We store only a sha256 hash of the secret
-- plus the human-visible prefix; the full key is shown once on creation.
CREATE TABLE api_keys (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name         text        NOT NULL,
    prefix       text        NOT NULL,
    key_hash     text        NOT NULL,
    role         text        NOT NULL DEFAULT 'operator',
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at   timestamptz
);

-- prefix is globally unique so the auth middleware can look a key up by prefix
-- before scoping to its tenant.
CREATE UNIQUE INDEX api_keys_prefix_key ON api_keys (prefix);
CREATE INDEX api_keys_tenant_id_idx ON api_keys (tenant_id);

-- ---------------------------------------------------------------------------
-- audit_log
-- ---------------------------------------------------------------------------
-- Append-only, hash-chained audit trail. Each row's hash chains to the previous
-- row's hash for the same tenant, so any tampering breaks the chain. UPDATE and
-- DELETE are revoked from the app role (see grants in the migration); the table
-- is insert-only at the privilege level, not just by convention.
CREATE TABLE audit_log (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    actor_type  text        NOT NULL,
    actor_id    text        NOT NULL DEFAULT '',
    action      text        NOT NULL,
    target_type text        NOT NULL DEFAULT '',
    target_id   text        NOT NULL DEFAULT '',
    metadata    jsonb       NOT NULL DEFAULT '{}'::jsonb,
    prev_hash   text        NOT NULL DEFAULT '',
    hash        text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_tenant_id_created_at_idx ON audit_log (tenant_id, created_at);

-- ---------------------------------------------------------------------------
-- Row-Level Security for the new tenant-scoped tables
-- ---------------------------------------------------------------------------
ALTER TABLE memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships FORCE ROW LEVEL SECURITY;
CREATE POLICY memberships_tenant_isolation ON memberships
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- A logged-in principal must enumerate its OWN memberships across every tenant
-- (to resolve "which tenants can I act in?" for /auth/me and tenant switching),
-- which the per-tenant policy above forbids. This second permissive SELECT-only
-- policy lets a user read membership rows that belong to them, keyed on the
-- app.user_id GUC set by InUserTx. It grants no cross-user visibility.
CREATE POLICY memberships_self_read ON memberships
    FOR SELECT
    USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

ALTER TABLE api_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE api_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY api_keys_tenant_isolation ON api_keys
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- Authenticating a presented bearer key is a chicken-and-egg with tenant
-- scoping: the auth layer must resolve a key by its (globally unique) prefix
-- BEFORE it knows which tenant the key belongs to. This narrow SELECT-only
-- policy permits exactly that lookup when the app.apikey_lookup GUC is 'on'
-- (set transaction-locally by InAPIKeyLookupTx, immediately before a
-- by-prefix read). It exposes only the prefix index path; once the key's
-- tenant is known, all further work uses the normal per-tenant policy.
CREATE POLICY api_keys_prefix_lookup ON api_keys
    FOR SELECT
    USING (current_setting('app.apikey_lookup', true) = 'on');

ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log FORCE ROW LEVEL SECURITY;
CREATE POLICY audit_log_tenant_isolation ON audit_log
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
