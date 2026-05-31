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
    -- M4 backups: the age PUBLIC recipient (X25519, "age1...") backups for this
    -- site are encrypted to. Client-side encryption is on the AGENT; the control
    -- plane stores ONLY this public recipient and never the matching identity
    -- (private key). Empty until a recipient is set. The CP cannot decrypt
    -- backups: it never holds the identity (ADR — trust model).
    age_recipient text      NOT NULL DEFAULT '',
    -- M17 backup-schedule: timezone fields captured from diagnostics identity
    -- category (timezone_string / gmt_offset). Used by the backup scheduler to
    -- compute the next run instant in the site's own WordPress timezone.
    wp_timezone   text      NOT NULL DEFAULT '',
    wp_gmt_offset real      NOT NULL DEFAULT 0,
    -- M21 connection lifecycle: connection_state is the single source of truth
    -- (legacy status/health_status kept for compat). See ADR-041.
    connection_state      text    NOT NULL DEFAULT 'pending_enrollment'
        CHECK (connection_state IN
            ('pending_enrollment','connected','degraded','disconnected','revoked','archived')),
    connection_generation integer NOT NULL DEFAULT 0,
    disconnected_at       timestamptz,
    disconnected_reason   text,
    archived_at           timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_sites_connection_state ON sites (tenant_id, connection_state);
CREATE INDEX idx_sites_last_seen ON sites (last_seen_at)
    WHERE connection_state IN ('connected','degraded');

CREATE INDEX sites_tenant_id_idx ON sites (tenant_id);
CREATE UNIQUE INDEX sites_tenant_id_url_key ON sites (tenant_id, url);
-- GIN index over tags so tenant-scoped tag filtering stays cheap.
CREATE INDEX sites_tags_idx ON sites USING gin (tags);
-- Resolve an enrolled site by its agent public key (agent-auth path). Unique
-- across the deployment: a given keypair identifies exactly one site.
CREATE UNIQUE INDEX sites_agent_public_key_key ON sites (agent_public_key)
    WHERE agent_public_key <> '';
-- M19: backs the composite FK on site_shares (prevents tenant drift).
ALTER TABLE sites ADD CONSTRAINT sites_id_tenant_key UNIQUE (id, tenant_id);

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

-- M22 shared-read: a site-scoped collaborator (no membership in the owning org)
-- may READ the metadata of sites shared with them, for the "Shared with me"
-- surface. Self-read style, keyed on app.user_id via a non-expired site_shares
-- grant. SELECT-only and PERMISSIVE — therefore OR-combined with the other
-- permissive policies but still AND-gated by the RESTRICTIVE sites_site_scope
-- policy (M19), so it CANNOT widen a site-scoped read. On bare-tenant/agent/
-- enroll paths app.user_id is unset → the subquery matches nothing. It only adds
-- visibility under InUserTx (the self-read context with no site_scope gate).
CREATE POLICY sites_shared_read ON sites
    FOR SELECT
    USING (EXISTS (
        SELECT 1 FROM site_shares s
        WHERE s.site_id = sites.id
          AND s.user_id = nullif(current_setting('app.user_id', true), '')::uuid
          AND (s.expires_at IS NULL OR s.expires_at > now())
    ));

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
-- M19: role vocabulary enforcement.
ALTER TABLE memberships ADD CONSTRAINT memberships_role_check
    CHECK (role IN ('owner', 'admin', 'operator', 'viewer'));

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
    -- M21: NULL = legacy tenant-scoped create-at-enroll flow; set = code bound to
    -- an existing pending_enrollment site (live-enroll + re-enrollment). ADR-041.
    site_id          uuid REFERENCES sites (id) ON DELETE CASCADE,
    consumed_from_ip inet,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- code_hash is globally unique so /enroll can resolve a presented code to its
-- tenant before any tenant scope exists (mirrors api_keys prefix lookup).
CREATE UNIQUE INDEX pairing_codes_code_hash_key ON pairing_codes (code_hash);
CREATE INDEX idx_pairing_codes_site ON pairing_codes (site_id) WHERE site_id IS NOT NULL;
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

-- ---------------------------------------------------------------------------
-- backup_chunks  (M4 — incremental, content-addressed dedup + GC)
-- ---------------------------------------------------------------------------
-- One row per UNIQUE (tenant, blake3) ciphertext chunk stored in object
-- storage. Chunks are content-addressed by the BLAKE3 hash of their CIPHERTEXT
-- (the agent encrypts client-side with age, then hashes; the CP and S3 only
-- ever see ciphertext). refcount tracks how many manifest entries across all of
-- the tenant's snapshots reference the chunk; GC deletes a chunk from S3 only
-- when refcount reaches zero. Tenant-scoped + RLS: a tenant can never see or
-- target another tenant's chunks, and the s3_key is namespaced by tenant so a
-- presign for one tenant cannot address another's chunk prefix.
CREATE TABLE backup_chunks (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    -- blake3 is the lowercase hex BLAKE3-256 digest of the chunk ciphertext.
    blake3     text        NOT NULL,
    -- s3_key is the object-storage key (always 'chunks/<tenant_id>/<blake3>').
    s3_key     text        NOT NULL,
    size       bigint      NOT NULL DEFAULT 0,
    refcount   bigint      NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- A chunk is unique per (tenant, blake3): dedup is scoped to the tenant so no
-- cross-tenant inference of stored content is possible.
CREATE UNIQUE INDEX backup_chunks_tenant_blake3_key ON backup_chunks (tenant_id, blake3);
CREATE INDEX backup_chunks_tenant_id_idx ON backup_chunks (tenant_id);

ALTER TABLE backup_chunks ENABLE ROW LEVEL SECURITY;
ALTER TABLE backup_chunks FORCE ROW LEVEL SECURITY;
CREATE POLICY backup_chunks_tenant_isolation ON backup_chunks
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- backup_snapshots  (M4)
-- ---------------------------------------------------------------------------
-- One backup of a site: files, db, or full. The manifest (ordered per-path
-- chunk lists) lives in backup_manifest_entries. Status advances pending ->
-- running -> completed | failed. age_recipient records the public recipient the
-- agent encrypted to (provenance; the CP never holds the identity). Tenant-
-- scoped + RLS.
CREATE TABLE backup_snapshots (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    site_id       uuid        NOT NULL REFERENCES sites (id) ON DELETE CASCADE,
    created_by    uuid        REFERENCES users (id) ON DELETE SET NULL,
    -- kind: files | db | full.
    kind          text        NOT NULL,
    -- status: pending | running | completed | failed.
    status        text        NOT NULL DEFAULT 'pending',
    -- age_recipient is the public X25519 recipient the chunks were encrypted to
    -- (echoed from the site at backup time for provenance/restore targeting).
    age_recipient text        NOT NULL DEFAULT '',
    total_size    bigint      NOT NULL DEFAULT 0,
    chunk_count   bigint      NOT NULL DEFAULT 0,
    error         text        NOT NULL DEFAULT '',
    -- archived marks a snapshot kept by the monthly-archive retention rule so GC
    -- spares it even once it falls outside the rolling window.
    archived      boolean     NOT NULL DEFAULT false,
    -- progress: phpbu-engine real-time progress (M5.6 / ADR-032). Latest phase
    -- payload posted by the agent runner. Shape:
    --   {"phase": "uploading", "phase_detail": {"chunks_done": 17, ...}}
    -- The watchdog (backup_progress_watchdog River periodic) scans for stalled
    -- runs via progress_updated_at; >120s without an update on a status='running'
    -- snapshot marks it failed with error='stalled'. JSONB so we can evolve the
    -- payload shape without migrations.
    progress             jsonb       NOT NULL DEFAULT '{}'::jsonb,
    progress_updated_at  timestamptz,
    started_at    timestamptz,
    finished_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX backup_snapshots_tenant_site_idx ON backup_snapshots (tenant_id, site_id, created_at DESC);
CREATE INDEX backup_snapshots_tenant_created_idx ON backup_snapshots (tenant_id, created_at DESC);
-- Watchdog scan: pick running snapshots whose latest progress is older than the
-- stall threshold. Filtered btree on status keeps the predicate selective.
CREATE INDEX backup_snapshots_running_progress_idx ON backup_snapshots (progress_updated_at)
    WHERE status = 'running';

ALTER TABLE backup_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE backup_snapshots FORCE ROW LEVEL SECURITY;
CREATE POLICY backup_snapshots_tenant_isolation ON backup_snapshots
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- The periodic retention GC enumerates which tenants have prunable snapshots
-- across ALL tenants (no tenant scope yet), then runs the actual prune per
-- tenant under the isolation policy. Permit that read-only enumeration when the
-- app.agent GUC is 'on' (set by InAgentTx), mirroring the health/scheduler jobs.
CREATE POLICY backup_snapshots_gc ON backup_snapshots
    FOR SELECT
    USING (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- backup_manifest_entries  (M4)
-- ---------------------------------------------------------------------------
-- One row per file (or db dump) in a snapshot: the relative path, the ORDERED
-- list of BLAKE3 chunk hashes that reassemble it (a text[] preserving order),
-- the total size, the file mode, and an optional kind tag ('file' | 'db'). To
-- restore a path the CP looks up each hash's s3_key in backup_chunks and issues
-- a presigned GET; the agent downloads, decrypts (age), verifies BLAKE3, and
-- concatenates in order. Tenant-scoped + RLS (redundant tenant_id avoids a join
-- in the policy and worker queries).
CREATE TABLE backup_manifest_entries (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    snapshot_id uuid        NOT NULL REFERENCES backup_snapshots (id) ON DELETE CASCADE,
    tenant_id   uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    -- path is the site-relative file path; for a db dump it is the sentinel
    -- 'database.sql'. table_name is set for db entries to support partial
    -- restore-by-table (empty for file entries).
    path        text        NOT NULL,
    entry_kind  text        NOT NULL DEFAULT 'file',
    table_name  text        NOT NULL DEFAULT '',
    -- chunk_hashes is the ordered list of BLAKE3 hex digests reassembling path.
    chunk_hashes text[]     NOT NULL DEFAULT '{}',
    size        bigint      NOT NULL DEFAULT 0,
    mode        integer     NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX backup_manifest_entries_snapshot_idx ON backup_manifest_entries (snapshot_id);
CREATE INDEX backup_manifest_entries_tenant_id_idx ON backup_manifest_entries (tenant_id);

ALTER TABLE backup_manifest_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE backup_manifest_entries FORCE ROW LEVEL SECURITY;
CREATE POLICY backup_manifest_entries_tenant_isolation ON backup_manifest_entries
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- backup_schedules  (M4)
-- ---------------------------------------------------------------------------
-- A per-site backup schedule: cadence (daily|weekly|monthly), the snapshot kind
-- to take, retention overrides, an enabled flag, and next_run_at which the
-- periodic scheduler advances after each enqueue. One schedule per site.
-- Tenant-scoped + RLS.
CREATE TABLE backup_schedules (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    site_id       uuid        NOT NULL REFERENCES sites (id) ON DELETE CASCADE,
    -- cadence: hourly | every_n_hours | daily | weekly | monthly.
    cadence       text        NOT NULL DEFAULT 'daily'
                  CHECK (cadence IN ('hourly','every_n_hours','daily','weekly','monthly')),
    -- kind: files | db | full (the snapshot kind each scheduled run takes).
    kind          text        NOT NULL DEFAULT 'full'
                  CHECK (kind IN ('files','db','full')),
    enabled       boolean     NOT NULL DEFAULT true,
    retention_days        integer NOT NULL DEFAULT 30,
    monthly_archive_keep  integer NOT NULL DEFAULT 12,
    -- M17 time-of-day / day-of-week / day-of-month fields.
    run_hour      smallint    NOT NULL DEFAULT 2   CHECK (run_hour   BETWEEN 0 AND 23),
    run_minute    smallint    NOT NULL DEFAULT 0   CHECK (run_minute BETWEEN 0 AND 59),
    day_of_week   smallint    NULL                 CHECK (day_of_week  BETWEEN 0 AND 6),
    day_of_month  smallint    NULL                 CHECK (day_of_month BETWEEN 1 AND 28),
    frequency_hours smallint  NULL                 CHECK (frequency_hours BETWEEN 1 AND 24),
    keep_last     integer     NOT NULL DEFAULT 7   CHECK (keep_last >= 0),
    next_run_at   timestamptz NOT NULL DEFAULT now(),
    last_run_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX backup_schedules_site_key ON backup_schedules (site_id);
CREATE INDEX backup_schedules_tenant_id_idx ON backup_schedules (tenant_id);
CREATE INDEX backup_schedules_due_idx ON backup_schedules (next_run_at) WHERE enabled;

ALTER TABLE backup_schedules ENABLE ROW LEVEL SECURITY;
ALTER TABLE backup_schedules FORCE ROW LEVEL SECURITY;
CREATE POLICY backup_schedules_tenant_isolation ON backup_schedules
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- The periodic scheduler enumerates DUE schedules across ALL tenants (it has no
-- tenant scope yet), mirroring the cross-tenant health job. Permit that read
-- when the app.agent GUC is 'on' (set by InAgentTx). The per-site backup work
-- it enqueues then runs tenant-scoped under the normal isolation policy.
CREATE POLICY backup_schedules_scheduler ON backup_schedules
    FOR SELECT
    USING (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- backup_schedule_runs  (M17 — materialized schedule queue)
-- ---------------------------------------------------------------------------
-- One row per scheduled or past backup fire for a site schedule. Mirrors
-- restore_runs. A 'scheduled' row is pre-inserted for the next upcoming fire;
-- the scheduler advances it to 'queued' then the worker transitions it to
-- running/completed/failed/skipped. The UNIQUE(schedule_id, scheduled_for)
-- constraint makes the pre-insert idempotent across CP restarts.
CREATE TABLE backup_schedule_runs (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    site_id       uuid        NOT NULL REFERENCES sites (id) ON DELETE CASCADE,
    schedule_id   uuid        NOT NULL REFERENCES backup_schedules (id) ON DELETE CASCADE,
    snapshot_id   uuid        REFERENCES backup_snapshots (id) ON DELETE SET NULL,
    scheduled_for timestamptz NOT NULL,
    status        text        NOT NULL DEFAULT 'scheduled'
                  CHECK (status IN ('scheduled','queued','running','completed','failed','skipped','canceled')),
    kind          text        NOT NULL DEFAULT 'full',
    error         text,
    triggered_by  text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    started_at    timestamptz,
    finished_at   timestamptz,
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX backup_schedule_runs_tenant_site_for_idx
    ON backup_schedule_runs (tenant_id, site_id, scheduled_for DESC);
CREATE INDEX backup_schedule_runs_status_for_idx
    ON backup_schedule_runs (status, scheduled_for);
CREATE INDEX backup_schedule_runs_schedule_id_idx
    ON backup_schedule_runs (schedule_id);
CREATE UNIQUE INDEX backup_schedule_runs_schedule_for_key
    ON backup_schedule_runs (schedule_id, scheduled_for);

ALTER TABLE backup_schedule_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE backup_schedule_runs FORCE ROW LEVEL SECURITY;

CREATE POLICY backup_schedule_runs_tenant_isolation ON backup_schedule_runs
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- FOR ALL: the scheduler INSERTs and UPDATEs rows cross-tenant under
-- app.agent='on'. Unlike restore_runs (agent reads only), the schedule
-- materializer both writes (pre-insert upcoming run) and updates (transition
-- to queued/running/completed/failed/skipped) across tenant boundaries.
CREATE POLICY backup_schedule_runs_agent ON backup_schedule_runs
    FOR ALL
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- alert_configs  (M5 — uptime downtime/recovery alerting)
-- ---------------------------------------------------------------------------
-- A per-tenant default alert channel (V0): the email recipients and webhook URL
-- a downtime/recovery alert is delivered to. webhook_secret signs the webhook
-- payload (HMAC-SHA256); it is a credential — never log it or return it in API
-- responses. One config row per tenant. Tenant-scoped + RLS.
CREATE TABLE alert_configs (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    -- email_recipients is the set of addresses downtime/recovery emails go to.
    email_recipients text[]    NOT NULL DEFAULT '{}',
    -- webhook_url is the single endpoint a signed alert POST is delivered to
    -- (empty disables the webhook). Reuses the SSRF-hardened client.
    webhook_url      text      NOT NULL DEFAULT '',
    -- webhook_secret keys the HMAC signature header on the webhook POST.
    webhook_secret   text      NOT NULL DEFAULT '',
    enabled          boolean   NOT NULL DEFAULT true,
    -- notify_security routes high-severity ADR-037 activity-log events into the
    -- SAME alert channel (email + webhook) as downtime/recovery. Default off so
    -- existing tenants do not start receiving security alerts unexpectedly.
    notify_security  boolean   NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX alert_configs_tenant_key ON alert_configs (tenant_id);

ALTER TABLE alert_configs ENABLE ROW LEVEL SECURITY;
ALTER TABLE alert_configs FORCE ROW LEVEL SECURITY;
CREATE POLICY alert_configs_tenant_isolation ON alert_configs
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- The alert evaluator enumerates configs across ALL tenants (cross-tenant
-- periodic job, like the health/scheduler jobs) under the app.agent GUC.
CREATE POLICY alert_configs_evaluator ON alert_configs
    FOR SELECT
    USING (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- site_alert_state  (M5 — incident transition tracking + alert de-dupe)
-- ---------------------------------------------------------------------------
-- Per-site uptime alert state machine. consecutive_down counts back-to-back DOWN
-- probe results; in_incident is true once an incident has been alerted (so we
-- de-dupe: alert ONLY on transition, not every interval). last_status records
-- the last classified state ('up'|'down'|'unknown'). This is the durable
-- transition memory the evaluator reads/writes. Tenant-scoped + RLS; the
-- redundant tenant_id keeps the RLS policy + cross-tenant evaluator queries
-- join-free.
CREATE TABLE site_alert_state (
    site_id          uuid        PRIMARY KEY REFERENCES sites (id) ON DELETE CASCADE,
    tenant_id        uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    last_status      text        NOT NULL DEFAULT 'unknown',
    consecutive_down integer     NOT NULL DEFAULT 0,
    in_incident      boolean     NOT NULL DEFAULT false,
    last_alert_at    timestamptz,
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX site_alert_state_tenant_id_idx ON site_alert_state (tenant_id);

ALTER TABLE site_alert_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_alert_state FORCE ROW LEVEL SECURITY;
CREATE POLICY site_alert_state_tenant_isolation ON site_alert_state
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- The probe worker updates this state cross-tenant (it iterates all enrolled
-- sites under app.agent, like the health job). Permit the full upsert lifecycle
-- when the app.agent GUC is 'on'.
CREATE POLICY site_alert_state_agent ON site_alert_state
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- site_uptime_probes  (M6 — Postgres-backed uptime time-series)
-- ---------------------------------------------------------------------------
-- One row per uptime probe. Replaces the M5 ClickHouse store as the DEFAULT
-- backend (ClickHouse remains available when WPMGR_CLICKHOUSE_ADDR is set).
-- Postgres comfortably handles the write rate at WPMgr's expected scale
-- (≤100 sites × ~1 probe/60s → ≤5M rows/year). The cert columns make a
-- separate cert-collection table unnecessary; the dashboard reads
-- issuer/subject/not_after from the latest probe row for the site.
CREATE TABLE site_uptime_probes (
    id           uuid             NOT NULL DEFAULT gen_random_uuid(),
    tenant_id    uuid             NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    site_id      uuid             NOT NULL REFERENCES sites   (id) ON DELETE CASCADE,
    probed_at    timestamptz      NOT NULL DEFAULT now(),
    up           boolean          NOT NULL,
    http_status  integer          NOT NULL DEFAULT 0,
    dns_ms       double precision NOT NULL DEFAULT 0,
    connect_ms   double precision NOT NULL DEFAULT 0,
    tls_ms       double precision NOT NULL DEFAULT 0,
    ttfb_ms      double precision NOT NULL DEFAULT 0,
    total_ms     double precision NOT NULL DEFAULT 0,
    tls_expiry   timestamptz,
    tls_issuer   text             NOT NULL DEFAULT '',
    tls_subject  text             NOT NULL DEFAULT '',
    error_text   text             NOT NULL DEFAULT '',
    PRIMARY KEY (id)
);

-- Latest-probe (per site) is a single index seek.
CREATE INDEX site_uptime_probes_site_time_idx
    ON site_uptime_probes (site_id, probed_at DESC);

-- Tenant-wide recent scans (summary endpoints).
CREATE INDEX site_uptime_probes_tenant_time_idx
    ON site_uptime_probes (tenant_id, probed_at DESC);

ALTER TABLE site_uptime_probes ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_uptime_probes FORCE ROW LEVEL SECURITY;
CREATE POLICY site_uptime_probes_tenant_isolation ON site_uptime_probes
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- Probe worker writes cross-tenant under app.agent and the metrics-read path
-- also runs under app.agent (filtered by explicit tenant_id at SQL level).
CREATE POLICY site_uptime_probes_agent ON site_uptime_probes
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- autologin_tokens  (Phase 5.5 — One-Click Login)
-- ---------------------------------------------------------------------------
-- An operator-minted, single-use, short-TTL nonce that materializes as an
-- Ed25519 JWT the WordPress agent verifies and consumes to establish an
-- authenticated wp-admin session. The PG row is the durable source of truth
-- (atomic consume); a parallel Redis key (autologin:<id>, EX 60s) is the
-- sub-millisecond hot-path consume — both are SET on mint, atomically GETDEL'd
-- on consume, and the PG row is UPDATE'd to consumed_at on either path.
--
-- The id IS the JWT jti (a base64url-encoded 32-byte random value). Storing the
-- nonce itself as the PK lets the consume RETURNING re-derive the session
-- target without any join. The token NEVER contains a session secret — the JWT
-- carries only the nonce + the target enrollment site_id; everything else (the
-- target WP login, allowed roles) is read from PG/Redis under the agent path.
CREATE TABLE autologin_tokens (
    -- id = base64url(rand_32) — the JWT jti and the Redis key suffix.
    id                    text        PRIMARY KEY,
    tenant_id             uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    site_id               uuid        NOT NULL REFERENCES sites (id) ON DELETE CASCADE,
    initiator_user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- target_wp_user_login is the WordPress username the agent should log in
    -- as; empty string means "agent picks the first administrator".
    target_wp_user_login  text        NOT NULL DEFAULT '',
    initiator_ip          inet,
    initiator_user_agent  text        NOT NULL DEFAULT '',
    expires_at            timestamptz NOT NULL,
    consumed_at           timestamptz,
    consumed_from_ip      inet,
    created_at            timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX autologin_tokens_tenant_id_idx ON autologin_tokens (tenant_id);
-- Hot path: the consume UPDATE filters on (id) and (consumed_at IS NULL); a
-- partial index over the unconsumed window keeps this cheap as the table grows.
CREATE INDEX autologin_tokens_pending_expiry_idx
    ON autologin_tokens (expires_at) WHERE consumed_at IS NULL;

ALTER TABLE autologin_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE autologin_tokens FORCE ROW LEVEL SECURITY;
-- Operator-side: tenant isolation. The mint path runs under app.tenant_id.
CREATE POLICY autologin_tokens_tenant_isolation ON autologin_tokens
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- Agent-side: the consume path resolves a nonce BEFORE any tenant scope exists
-- (the agent presents the verified site_id + nonce, not a tenant). Mirrors the
-- sites_agent / agent_nonces_agent pattern. SELECT+UPDATE only — the agent
-- never inserts/deletes autologin_tokens.
CREATE POLICY autologin_tokens_agent ON autologin_tokens
    FOR SELECT
    USING (current_setting('app.agent', true) = 'on');
CREATE POLICY autologin_tokens_agent_consume ON autologin_tokens
    FOR UPDATE
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- autologin_policies  (Phase 5.5 — One-Click Login)
-- ---------------------------------------------------------------------------
-- One row per site governs the autologin feature for that site: whether it's
-- enabled, which WP roles the agent is allowed to log in as, whether a 2FA
-- step-up is required (today inert — feature-flagged off until 2FA exists), and
-- the maximum acceptable session age in minutes. tenant_id is DENORMALISED from
-- sites.tenant_id to keep the RLS policy join-free (mirrors the M5
-- site_alert_state pattern).
CREATE TABLE autologin_policies (
    site_id                 uuid        PRIMARY KEY REFERENCES sites (id) ON DELETE CASCADE,
    tenant_id               uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    enabled                 boolean     NOT NULL DEFAULT true,
    allowed_wp_roles        text[]      NOT NULL DEFAULT ARRAY['administrator'],
    require_2fa_step_up     boolean     NOT NULL DEFAULT false,
    max_session_age_minutes integer     NOT NULL DEFAULT 30,
    updated_at              timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX autologin_policies_tenant_id_idx ON autologin_policies (tenant_id);

ALTER TABLE autologin_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE autologin_policies FORCE ROW LEVEL SECURITY;
CREATE POLICY autologin_policies_tenant_isolation ON autologin_policies
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- The consume path reads allowed_wp_roles cross-tenant under app.agent (the
-- agent identity is the site, resolved before any tenant scope). SELECT-only.
CREATE POLICY autologin_policies_agent ON autologin_policies
    FOR SELECT
    USING (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- site_shares  (M19 — per-site collaborator grants)
-- ---------------------------------------------------------------------------
-- One row per (site, user) grant. Allows an outside user (no memberships row)
-- access to exactly one site within the owning tenant, bounded by role and an
-- optional expiry. RLS: tenant isolation for org admins + self_read for the
-- grantee's cross-org discovery path (no site_scope restrictive policy here —
-- a scoped user reads their own shares via self_read; never lists others').
CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE site_shares (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid        NOT NULL,
    site_id    uuid        NOT NULL,
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role       text        NOT NULL DEFAULT 'viewer'
               CHECK (role IN ('viewer', 'operator', 'admin')),
    granted_by uuid        REFERENCES users (id) ON DELETE SET NULL,
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (site_id, user_id),
    FOREIGN KEY (site_id, tenant_id) REFERENCES sites (id, tenant_id) ON DELETE CASCADE
);

CREATE INDEX site_shares_user_id_idx ON site_shares (user_id);
CREATE INDEX site_shares_tenant_id_idx ON site_shares (tenant_id);

ALTER TABLE site_shares ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_shares FORCE ROW LEVEL SECURITY;

CREATE POLICY site_shares_tenant_isolation ON site_shares
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY site_shares_self_read ON site_shares
    FOR SELECT
    USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- invitations  (M19 — org + site invitation, tokenized)
-- ---------------------------------------------------------------------------
-- One row per invitation issued. Covers both org-level (scope='org') and
-- per-site (scope='site') invitations in a single table. token_hash is a
-- sha256 of the plaintext token (never stored); the accept endpoint looks it
-- up pre-auth via the invitations_token_lookup policy. email is citext for
-- case-insensitive matching at accept time.
CREATE TABLE invitations (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    email            citext      NOT NULL,
    scope            text        NOT NULL CHECK (scope IN ('org', 'site')),
    site_id          uuid        REFERENCES sites (id) ON DELETE CASCADE,
    role             text        NOT NULL,
    token_hash       text        NOT NULL UNIQUE,
    invited_by       uuid        REFERENCES users (id) ON DELETE SET NULL,
    expires_at       timestamptz NOT NULL,
    attempts         integer     NOT NULL DEFAULT 0,
    accepted_at      timestamptz,
    accepted_user_id uuid        REFERENCES users (id) ON DELETE SET NULL,
    revoked_at       timestamptz,
    revoked_by       uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX invitations_tenant_id_idx ON invitations (tenant_id);
CREATE INDEX invitations_email_idx ON invitations (email);
CREATE INDEX invitations_site_id_idx ON invitations (site_id, created_at DESC) WHERE scope = 'site';

ALTER TABLE invitations ENABLE ROW LEVEL SECURITY;
ALTER TABLE invitations FORCE ROW LEVEL SECURITY;

CREATE POLICY invitations_tenant_isolation ON invitations
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- Pre-auth lookup: the public /invitations/accept endpoint must resolve a
-- token before any session/tenant scope exists. Mirrors api_keys_prefix_lookup.
CREATE POLICY invitations_token_lookup ON invitations
    FOR SELECT
    USING (current_setting('app.invite_lookup', true) = 'on');

-- ---------------------------------------------------------------------------
-- site_connection_history  (M21 — connection lifecycle transition log)
-- ---------------------------------------------------------------------------
-- Append-only record of every connection-state transition (ADR-041). Powers the
-- Activity tab's connection timeline across re-enrollment generations.
CREATE TABLE site_connection_history (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    site_id       uuid        NOT NULL REFERENCES sites (id) ON DELETE CASCADE,
    from_state    text        NOT NULL,
    to_state      text        NOT NULL,
    reason        text,
    actor_user_id uuid        REFERENCES users (id) ON DELETE SET NULL,
    generation    integer     NOT NULL DEFAULT 0,
    occurred_at   timestamptz NOT NULL DEFAULT now(),
    metadata      jsonb       NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX idx_conn_history_site ON site_connection_history (site_id, occurred_at DESC);

ALTER TABLE site_connection_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_connection_history FORCE ROW LEVEL SECURITY;
CREATE POLICY conn_history_tenant_isolation ON site_connection_history
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- M21 follow-up: the site-first enroll consume appends a history row inside the
-- public enroll tx (app.enroll='on') before any tenant scope is set.
CREATE POLICY conn_history_enroll ON site_connection_history
    USING (current_setting('app.enroll', true) = 'on')
    WITH CHECK (current_setting('app.enroll', true) = 'on');

-- ---------------------------------------------------------------------------
-- site_events  (M21 — durable SSE journal for LISTEN/NOTIFY fan-out + replay)
-- ---------------------------------------------------------------------------
-- event_id is an app-minted ULID (lexicographically sortable, monotonic per
-- tenant). NOTIFY carries only '<tenant_id>:<event_id>'; API instances read the
-- body here to fan out to local SSE subscribers and to replay on ?since=
-- reconnect (~5-minute retention; periodically pruned). See ADR-038.
CREATE TABLE site_events (
    event_id   text        PRIMARY KEY,
    tenant_id  uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    site_id    uuid,
    type       text        NOT NULL,
    data       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_site_events_tenant ON site_events (tenant_id, event_id);
CREATE INDEX idx_site_events_created ON site_events (created_at);

ALTER TABLE site_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_events FORCE ROW LEVEL SECURITY;
CREATE POLICY site_events_tenant_isolation ON site_events
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- M21 follow-up: the cross-tenant ring-buffer prune runs under app.agent='on'.
CREATE POLICY site_events_agent ON site_events
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');
