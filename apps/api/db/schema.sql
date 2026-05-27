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
    -- M2 enrollment + agent identity.
    -- agent_public_key is the agent's own Ed25519 public key (base64 std), stored
    -- so the control plane can verify signed agent->CP requests. Empty until the
    -- site is enrolled; rotated on re-enrollment.
    agent_public_key text       NOT NULL DEFAULT '',
    enrolled_at      timestamptz,
    last_seen_at     timestamptz,
    -- health_status reflects agent heartbeat freshness (M2): unknown until first
    -- contact, healthy while heartbeats are fresh, unreachable when stale. Active
    -- external probing is deferred to M5.
    health_status text NOT NULL DEFAULT 'unknown',
    -- M2 site metadata pushed by the agent.
    server_info text    NOT NULL DEFAULT '',
    multisite   boolean NOT NULL DEFAULT false,
    active_theme text   NOT NULL DEFAULT '',
    -- components holds the installed plugins/themes inventory as JSONB (M2): a
    -- normalized child table can come later; JSONB is sufficient for M2.
    components  jsonb       NOT NULL DEFAULT '{}'::jsonb,
    tags        text[]      NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX sites_tenant_id_idx ON sites (tenant_id);
CREATE UNIQUE INDEX sites_tenant_id_url_key ON sites (tenant_id, url);
-- GIN index over tags so tenant-scoped tag filtering stays cheap.
CREATE INDEX sites_tags_idx ON sites USING gin (tags);
-- Resolve an enrolled site by its agent public key (agent-auth path). Unique
-- across the deployment: a given keypair identifies exactly one site.
CREATE UNIQUE INDEX sites_agent_public_key_key ON sites (agent_public_key)
    WHERE agent_public_key <> '';

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

-- M2 enrollment: /enroll resolves/creates/attaches a site by URL BEFORE any
-- tenant scope exists (the agent presents only a pairing code). This policy
-- permits the full enroll lifecycle on sites when the app.enroll GUC is 'on'
-- (set transaction-locally by InEnrollTx). Scope is otherwise unchanged.
CREATE POLICY sites_enroll ON sites
    USING (current_setting('app.enroll', true) = 'on')
    WITH CHECK (current_setting('app.enroll', true) = 'on');

-- M2 agent-auth: an authenticated agent->CP request is identified by the site's
-- agent_public_key, resolved before any tenant scope. This policy permits the
-- agent path (metadata/heartbeat updates) when the app.agent GUC is 'on' (set
-- transaction-locally by InAgentTx). The resolved site's tenant is then trusted.
CREATE POLICY sites_agent ON sites
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

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

-- ---------------------------------------------------------------------------
-- pairing_codes  (M2 — agent enrollment)
-- ---------------------------------------------------------------------------
-- A one-time, short-TTL, high-entropy code an operator generates for a tenant.
-- An (untrusted) agent presents it once at /enroll to bind itself to the
-- tenant. We store only a sha256 hash of the code; the plaintext is shown once.
-- Tenant-scoped + RLS for the operator-facing creation/listing path.
CREATE TABLE pairing_codes (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    code_hash    text        NOT NULL,
    created_by   uuid        REFERENCES users (id) ON DELETE SET NULL,
    site_name    text        NOT NULL DEFAULT '',
    tags         text[]      NOT NULL DEFAULT '{}',
    expires_at   timestamptz NOT NULL,
    consumed_at  timestamptz,
    attempts     integer     NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- code_hash is globally unique so /enroll can resolve a presented code to its
-- tenant before any tenant scope exists (mirrors api_keys prefix lookup).
CREATE UNIQUE INDEX pairing_codes_code_hash_key ON pairing_codes (code_hash);
CREATE INDEX pairing_codes_tenant_id_idx ON pairing_codes (tenant_id);

ALTER TABLE pairing_codes ENABLE ROW LEVEL SECURITY;
ALTER TABLE pairing_codes FORCE ROW LEVEL SECURITY;
CREATE POLICY pairing_codes_tenant_isolation ON pairing_codes
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- The /enroll endpoint is PUBLIC (the agent has no session/tenant yet) and must
-- resolve + consume a code by its (globally unique) hash before the tenant is
-- known. This narrow policy permits SELECT/INSERT/UPDATE only when the
-- app.enroll GUC is 'on' (set transaction-locally by InEnrollTx, immediately
-- around the enroll work). It exposes only the by-hash path.
CREATE POLICY pairing_codes_enroll ON pairing_codes
    USING (current_setting('app.enroll', true) = 'on')
    WITH CHECK (current_setting('app.enroll', true) = 'on');

-- ---------------------------------------------------------------------------
-- agent_nonces  (M2 — agent-auth anti-replay)
-- ---------------------------------------------------------------------------
-- Each signed agent->CP request carries a unique nonce (jti). We persist seen
-- nonces within the signature freshness window so a captured request cannot be
-- replayed. Rows are scoped to a site and pruned by created_at. Resolution of
-- the verifying request happens outside any tenant scope (the agent presents no
-- tenant), so this table is gated by the same app.enroll/app.agent GUC.
CREATE TABLE agent_nonces (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    site_id    uuid        NOT NULL REFERENCES sites (id) ON DELETE CASCADE,
    nonce      text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX agent_nonces_site_nonce_key ON agent_nonces (site_id, nonce);
CREATE INDEX agent_nonces_created_at_idx ON agent_nonces (created_at);

-- agent_nonces is written/read only on the agent-auth path, which has no tenant
-- scope. Gate it on the app.agent GUC ('on' inside InAgentTx). No tenant policy
-- is needed: the agent identity is the site, resolved by public key.
ALTER TABLE agent_nonces ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_nonces FORCE ROW LEVEL SECURITY;
CREATE POLICY agent_nonces_agent ON agent_nonces
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- update_runs  (M3 — bulk plugin/theme/core updates with rollback)
-- ---------------------------------------------------------------------------
-- An update_run groups a single operator-initiated bulk update across one or
-- more sites/items into a unit with an overall status. Each (site, item) pair
-- becomes an update_task. Tenant-scoped + RLS so a run (and its tasks) is only
-- visible within the owning tenant.
CREATE TABLE update_runs (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    -- created_by is the acting user (NULL for an API-key principal); SET NULL on
    -- user deletion so the run history survives.
    created_by   uuid        REFERENCES users (id) ON DELETE SET NULL,
    -- status: pending (created, tasks enqueued), running (>=1 task running),
    -- completed (all tasks reached a terminal state). The worker advances it.
    status       text        NOT NULL DEFAULT 'pending',
    dry_run      boolean     NOT NULL DEFAULT false,
    -- scheduled_at is when the run should execute; NULL/now() means immediately.
    scheduled_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX update_runs_tenant_id_created_at_idx ON update_runs (tenant_id, created_at DESC);

ALTER TABLE update_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE update_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY update_runs_tenant_isolation ON update_runs
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- update_tasks  (M3)
-- ---------------------------------------------------------------------------
-- One unit of work: apply one item (a plugin/theme/core) on one site. Carries
-- the from/to versions and a per-task terminal status. Tenant-scoped + RLS; the
-- redundant tenant_id (also on the parent run) lets the RLS policy and the
-- worker's by-key updates stay tenant-scoped without a join.
CREATE TABLE update_tasks (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id       uuid        NOT NULL REFERENCES update_runs (id) ON DELETE CASCADE,
    tenant_id    uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    site_id      uuid        NOT NULL REFERENCES sites (id) ON DELETE CASCADE,
    -- target_type: plugin | theme | core. target_slug is the plugin/theme slug;
    -- for core it is the sentinel 'core'.
    target_type  text        NOT NULL,
    target_slug  text        NOT NULL,
    -- desired_version is the operator's requested target ('latest' or a pin).
    desired_version text     NOT NULL DEFAULT 'latest',
    from_version text        NOT NULL DEFAULT '',
    to_version   text        NOT NULL DEFAULT '',
    -- status: pending | running | succeeded | failed | rolled_back | skipped.
    status       text        NOT NULL DEFAULT 'pending',
    detail       text        NOT NULL DEFAULT '',
    error        text        NOT NULL DEFAULT '',
    started_at   timestamptz,
    finished_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX update_tasks_run_id_idx ON update_tasks (run_id);
CREATE INDEX update_tasks_tenant_id_idx ON update_tasks (tenant_id);
CREATE INDEX update_tasks_site_id_idx ON update_tasks (site_id);

ALTER TABLE update_tasks ENABLE ROW LEVEL SECURITY;
ALTER TABLE update_tasks FORCE ROW LEVEL SECURITY;
CREATE POLICY update_tasks_tenant_isolation ON update_tasks
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
