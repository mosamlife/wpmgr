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
-- M91 (M16 Phase A) — hosted-billing entitlement substrate. tenants is the
-- billable root; it carries NO RLS (see the file header — it is gated by
-- membership/ownership checks in the handler, not a row-level policy), so the
-- new columns below are plain, ungated fields. Ships DARK behind WPMGR_HOSTED
-- (default false): self-host and current prod see zero behavior change until
-- the flag is turned on. Provider-agnostic on purpose — billing_provider /
-- provider_customer_id / provider_subscription_id are generic so a future
-- payment provider (Phase B) can be wired without another schema change; they
-- are never provider-prefixed (e.g. never "stripe_...").
--   plan                     — the tier: free/starter/agency/scale. Defaults
--                              to 'free' so every existing tenant becomes free
--                              at cutover (see the grandfather backfill below).
--   plan_status              — none/trialing/active/past_due/canceled/paused/
--                              comped. Defaults to 'none': a tenant that has
--                              never seen a billing event is simply free, per
--                              internal/billing's status gate.
--   plan_overrides           — jsonb per-key delta overlay (e.g.
--                              {"max_sites": 25}) resolved on top of the plan
--                              ladder by internal/billing.Entitlements. Used by
--                              the grandfather backfill and future manual comps.
--   grace_until              — a past_due tenant keeps its paid limits until
--                              this instant (internal/billing's status gate),
--                              then falls back to free.
--   current_period_end       — Phase-B webhook-consumer bookkeeping.
--
-- M92 (M16 Phase C1) — superadmin billing-admin panel. Four more manual-
-- override fields, all superadmin-only (internal/admin), layered on top of
-- Phase A/B without touching their vocabulary:
--   comp_reason              — free-text reason recorded alongside a manual
--                              plan_status='comped' grant (admin.billing.comp.*).
--   suspended_at              — a SEPARATE hard-lockout flag, NOT a plan_status
--                              value: a suspended tenant's underlying billing
--                              state (plan/plan_status/grace_until) is left
--                              completely untouched so "restore" is a clean,
--                              lossless un-suspend. NULL = not suspended.
--   suspended_reason          — free-text reason recorded alongside suspend.
--   cancel_at_period_end      — mirrors the payment provider's own flag
--                              (Stripe's subscription.cancel_at_period_end) so
--                              the admin detail view can show "will cancel on
--                              <date>" without an extra provider round-trip.
--                              Defaults false; Phase B's webhook/reconcile
--                              paths do not yet write it (display-only today).
--
-- M93 (GH #152 part 2) — owner-facing organisation deletion.
--   deleted_at               — nullable; NULL = live (the default). Set by
--                              DELETE /api/v1/orgs/{orgId}'s Lane B (populated
--                              org, internal/org) to soft-delete: the org
--                              becomes invisible everywhere the instant this
--                              is set (every membership/org-list/tenant-
--                              lookup read path filters deleted_at IS NULL —
--                              see db/query/tenants.sql, memberships.sql,
--                              api_keys.sql). Cleared by
--                              POST /orgs/{orgId}/restore within the grace
--                              window; the row itself is destroyed by
--                              internal/org.PurgeWorker (admin_purge_tenant,
--                              below) once the grace window elapses. An empty
--                              org (Lane A) never sets this — it is
--                              hard-deleted immediately via the existing
--                              admin_delete_empty_tenant.
--   purge_started_at         — nullable point-of-no-return marker, distinct
--                              from deleted_at (adversarial-review fast-follow
--                              M2). internal/org.PurgeWorker sets it
--                              (MarkPurgeStarted) BEFORE the FIRST
--                              object-storage delete of its 7 tenant prefixes
--                              — object deletion is irreversible, but a
--                              DB-only soft-delete is not, so without this
--                              marker a transient storage fault mid-purge
--                              (deleted_at still set, some-but-not-all
--                              objects gone) leaves a window where restore
--                              would resurrect a tenant whose
--                              backup_chunks/snapshot rows now point at
--                              partially-missing objects. RestoreTenant's
--                              WHERE clause also requires this to be NULL, so
--                              a purge already touching object storage
--                              refuses restore (409 purge_in_progress).
CREATE TABLE tenants (
    id                       uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name                     text        NOT NULL,
    slug                     text        NOT NULL UNIQUE,
    plan                     text        NOT NULL DEFAULT 'free'
        CHECK (plan IN ('free', 'starter', 'agency', 'scale')),
    plan_status              text        NOT NULL DEFAULT 'none'
        CHECK (plan_status IN ('none', 'trialing', 'active', 'past_due', 'canceled', 'paused', 'comped')),
    plan_overrides           jsonb       NOT NULL DEFAULT '{}'::jsonb,
    grace_until              timestamptz,
    billing_provider         text,
    provider_customer_id     text,
    provider_subscription_id text,
    current_period_end       timestamptz,
    comp_reason              text,
    suspended_at             timestamptz,
    suspended_reason         text,
    cancel_at_period_end     boolean     NOT NULL DEFAULT false,
    deleted_at               timestamptz,
    purge_started_at         timestamptz,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now()
);

-- billing_events (M91) — the Phase-B webhook/event ledger, created now so a
-- future payment-provider integration has a home to land in immediately.
-- UNIQUE(provider, provider_event_id) makes a replayed webhook a no-op insert
-- (ON CONFLICT DO NOTHING at the call site). tenant_id is nullable because a
-- provider event may arrive before it can be matched to a tenant (e.g. an
-- unrecognized customer id).
--
-- RLS (security review Finding C): the standard tenant/system pairing already
-- used by site_events and sites (m36 pattern) — billing_events_tenant_isolation
-- scopes any future tenant-facing read (e.g. an operator billing-history view)
-- to app.tenant_id (a NULL tenant_id row never matches, so an unmatched event
-- stays invisible to every tenant); billing_events_system is the write path
-- for the Phase-B webhook consumer, which processes events across many
-- tenants in one pass and is NOT a single tenant's request scope, so it runs
-- under InAgentTx (app.agent='on') — the same cross-tenant GUC every other
-- system/worker write already uses. ENABLE + FORCE so the table owner is also
-- subject to RLS.
CREATE TABLE billing_events (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    provider           text        NOT NULL,
    provider_event_id  text        NOT NULL,
    kind               text        NOT NULL,
    tenant_id          uuid        REFERENCES tenants (id) ON DELETE CASCADE,
    payload            jsonb       NOT NULL DEFAULT '{}'::jsonb,
    occurred_at        timestamptz NOT NULL,
    processed_at       timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX billing_events_provider_event_key ON billing_events (provider, provider_event_id);
CREATE INDEX billing_events_tenant_id_idx ON billing_events (tenant_id);

ALTER TABLE billing_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing_events FORCE ROW LEVEL SECURITY;
CREATE POLICY billing_events_tenant_isolation ON billing_events
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY billing_events_system ON billing_events
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- billing_count_active_sites (M91): SECURITY DEFINER site-cap counter for
-- internal/billing.CheckSiteCreate. "Active" mirrors the sites-list default
-- filter exactly (ListSites/ListAllSiteIDs, ADR-041): every connection_state
-- except 'archived'.
--
-- CheckSiteCreate is invoked from BOTH the operator path (app.tenant_id set,
-- InTenantTx — CreatePending, site.Service.Create, the Restore un-archive
-- pre-check) and the public /enroll path (app.enroll set, InEnrollTx —
-- CreateSiteForEnroll), which activate different RLS policies on sites.
-- Rather than depend on either policy's current shape to expose the right
-- rows, this function sets app.agent='on' in its own body (mirrors
-- admin_delete_empty_tenant's technique, m35) to activate the unconditionally
-- tenant-agnostic sites_agent policy, then applies the same explicit
-- tenant_id + connection_state filter a real caller would use — so the count
-- is correct under FORCE ROW LEVEL SECURITY in every calling context, present
-- or future. search_path is pinned; EXECUTE is granted only to wpmgr_app (the
-- migration); the function is not otherwise privileged (a single explicit
-- tenant_id in, a count out — no data exposure).
--
-- Security review Finding A: v_prev captures whatever app.agent was already
-- set to in the CALLER's transaction and restores it before returning, on the
-- function's single return path. set_config(...,true)'s in-body write is NOT
-- rolled back at function exit (the "true"/is_local flag scopes the change to
-- the transaction, not the function invocation) — an unrestored 'on' would
-- otherwise leak into every statement that runs after this call in the same
-- caller transaction (e.g. the rest of a CreatePending/Transition site-birth
-- tx), silently disabling that transaction's tenant-isolation RLS check.
CREATE OR REPLACE FUNCTION billing_count_active_sites(p_tenant uuid)
RETURNS bigint
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_count bigint;
    v_prev text := current_setting('app.agent', true);
BEGIN
    PERFORM set_config('app.agent', 'on', true);
    SELECT count(*) INTO v_count
    FROM sites
    WHERE tenant_id = p_tenant
      AND connection_state <> 'archived';
    PERFORM set_config('app.agent', coalesce(v_prev, ''), true);
    RETURN v_count;
END;
$$;
-- Mirror the migration's grants so this reference stays faithful (the runtime
-- DB is built from migrations, not this file): the function is NOT
-- PUBLIC-callable.
REVOKE ALL ON FUNCTION billing_count_active_sites(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION billing_count_active_sites(uuid) TO wpmgr_app;

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
    -- M27 — current WPMgr agent plugin version, reported on each metadata push.
    agent_version text      NOT NULL DEFAULT '',
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
    -- M28 host provider: inferred CP-side from the agent's observed public
    -- egress IP via an offline ASN lookup. Best-effort hint, shown only when no
    -- managed-host defined()-flag matched. Empty until the first diagnostics push.
    host_provider            text NOT NULL DEFAULT '',
    host_provider_org        text NOT NULL DEFAULT '',
    host_provider_ip         text NOT NULL DEFAULT '',
    host_provider_checked_at timestamptz,
    -- M21 connection lifecycle: connection_state is the single source of truth
    -- (legacy status/health_status kept for compat). See ADR-041.
    connection_state      text    NOT NULL DEFAULT 'pending_enrollment'
        CHECK (connection_state IN
            ('pending_enrollment','connected','degraded','disconnected','revoked','archived')),
    connection_generation integer NOT NULL DEFAULT 0,
    disconnected_at       timestamptz,
    disconnected_reason   text,
    archived_at           timestamptz,
    -- M58 hysteresis: counts consecutive sweeper overdue evaluations.
    -- Reset to 0 on every heartbeat; the sweeper degrades only after N misses.
    missed_heartbeats     integer NOT NULL DEFAULT 0,
    -- M63 clients: optional client grouping (1 site has AT MOST 1 client).
    -- The composite FK to clients (id, tenant_id) is added after the clients
    -- table definition below (ON DELETE SET NULL — unassign, never cascade).
    client_id   uuid,
    -- m107 (GH #291 Phase 2): B3 override for the application-health probe.
    -- When set, the app prober requests this path instead of auto-detecting
    -- /wp-json/ (with the ?rest_route=/ fallback). NULL/empty means
    -- auto-detect. Not yet operator-settable via the API (planned for Phase
    -- 3's per-site override UI); the column and probe support exist now so
    -- Phase 3 needs no further schema change.
    app_probe_path text,
    -- m108 (GH #291 Phase 3): per-site opt-out for app-health ALERTING only.
    -- The app probe keeps running (the dashboard stays accurate) and this
    -- flag never touches app_probe_path. Operator-settable via
    -- GET/PUT /sites/{siteId}/app-health-settings.
    app_alerts_disabled boolean NOT NULL DEFAULT false,
    -- m117 (GH #414 Phase 1): "pause monitoring". SCHEMA ONLY in this phase —
    -- no scheduler reads these yet, so nothing pauses. Pause is orthogonal to
    -- connection_state (ADR-041): connection_state says whether the AGENT IS
    -- REACHABLE, pause says whether WE CHOOSE TO ACT, and a connected site can
    -- be paused while a paused site can lose its agent.
    --
    -- Pause will eventually stop uptime probes and their alerts, update
    -- inventory refresh, scheduled security scans, vulnerability rescans and
    -- their alerts, and screenshots. It must NEVER stop backups (data
    -- protection is not monitoring), the connection sweep (site_connection_sweep
    -- / site_health_check — stopping it would freeze a paused site at
    -- connection_state 'connected' forever after its agent died; pause means
    -- "do not tell me", never "lie to me"), RUM ingestion, retention/cleanup, or
    -- anything a person clicks. Pause governs the SCHEDULE, never the operator.
    --
    -- NULL = monitoring active. Non-NULL = paused, and the value is when: the
    -- flag and the since-when in one column so they cannot disagree.
    monitoring_paused_at     timestamptz,
    -- Who paused it, for the badge. The FK to users (id) ON DELETE SET NULL is
    -- added after the users table below, matching the sites_client_tenant_fkey
    -- pattern: users is defined later in this file and a forward reference here
    -- would not resolve.
    monitoring_paused_by     uuid,
    -- Optional free text, shown on hover. NOT NULL DEFAULT '' like every other
    -- optional text column here, so readers never distinguish NULL from empty.
    monitoring_paused_reason text NOT NULL DEFAULT '',
    -- Optional auto-resume instant; NULL means "until someone resumes it".
    monitoring_resume_at     timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    -- m117: a resume time with no pause is incoherent, and a later phase reads
    -- it as an instruction — the auto-resume sweep's predicate is
    -- "monitoring_resume_at <= now()", so a dangling resume instant on an
    -- unpaused site is a phantom state transition, not a cosmetic
    -- inconsistency. Enforced in the schema rather than the service because
    -- four writers will exist (pause route, resume route, auto-resume worker,
    -- bulk action) and three do not exist yet; m115 exists because m113 left a
    -- check constraint open. It deliberately does NOT tie monitoring_paused_by
    -- to the pause — the FK's own ON DELETE SET NULL would violate that —
    -- nor require an empty reason while active, nor require resume_at to be
    -- after paused_at (a past instant sanely means "due now").
    CONSTRAINT sites_monitoring_resume_requires_pause_check
        CHECK (monitoring_resume_at IS NULL OR monitoring_paused_at IS NOT NULL)
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

-- m66 sites_client_read — PERMISSIVE SELECT-only policy. Mirrors sites_shared_read.
-- Under InUserTx (auth-time client-member expansion) there is no app.tenant_id,
-- so sites_tenant_isolation hides every row. This policy lets a client member
-- read site rows for sites belonging to their client only. Still AND-gated by
-- the RESTRICTIVE sites_site_scope policy (m19). archived_at gate ensures
-- members of an archived client lose access instantly.
CREATE POLICY sites_client_read ON sites
    FOR SELECT
    USING (EXISTS (
        SELECT 1
        FROM client_members cm
        JOIN clients cl
          ON cl.id = cm.client_id AND cl.tenant_id = cm.tenant_id
        WHERE cm.client_id = sites.client_id
          AND cm.tenant_id = sites.tenant_id
          AND cm.user_id   = nullif(current_setting('app.user_id', true), '')::uuid
          AND cl.archived_at IS NULL
    ));

-- sites_site_scope (m19) — the RESTRICTIVE site-scope gate. Referred to by
-- name three times in the comments above; declared here as of GH #470, having
-- been live in every database since m19 and absent from this file until now.
-- RESTRICTIVE, so it AND-combines with every permissive policy above and can
-- only narrow: it is what stops sites_shared_read or sites_client_read
-- widening a site-scoped principal's reach.
CREATE POLICY sites_site_scope ON sites
    AS RESTRICTIVE FOR ALL
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR id = ANY (
            string_to_array(
                nullif(current_setting('app.allowed_site_ids', true), ''), ','
            )::uuid[]
        )
    )
    WITH CHECK (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR id = ANY (
            string_to_array(
                nullif(current_setting('app.allowed_site_ids', true), ''), ','
            )::uuid[]
        )
    );

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
-- user_identities (m110)
-- ---------------------------------------------------------------------------
-- One row per external identity a user can sign in with, replacing the single
-- (oidc_issuer, oidc_subject) pair above. With Google and GitHub both offered,
-- one slot is not enough: the ordinary case is somebody who signed up with one
-- and later clicks the other, and with a single slot the second would overwrite
-- the first.
--
-- (provider, subject, issuer) is the identity. The email column records what
-- the provider asserted and is deliberately NOT unique: emails change, get
-- reassigned inside a Workspace, and repeat across providers. Matching on email
-- is how account takeovers happen.
--
-- ISSUER IS PART OF THE KEY, AND HAS TO BE. A subject is unique only within the
-- issuer that minted it; two IdPs can hand out the same opaque string for two
-- different people. Keying on (provider, subject) alone would make that
-- collision resolve to a silent sign-in as somebody else, which is a worse
-- failure than any lockout.
--
-- The lockout that argument is usually raised against is real, and is answered
-- elsewhere: an operator who repoints WPMGR_OIDC_ISSUER strands every
-- generic-OIDC row at once. internal/auth treats that as a MIGRATION rather
-- than a lookup rule. A difference that is purely cosmetic (trailing slash,
-- host case) is not a change at all, and a genuine change of issuer is carried
-- by WPMGR_OIDC_PREVIOUS_ISSUER, which the operator sets deliberately; each row
-- is then moved to the new issuer once, on that person's next sign-in, and the
-- move is written to the audit log. Ambiguity never resolves to a guess.
--
-- email_verified is the PROVIDER's assertion at link time. users.email_verified_at
-- is our own. The linking rules need both, separately.
--
-- Not RLS-scoped, matching users: a user spans tenants, so neither the user nor
-- their means of authenticating belongs to one.
CREATE TABLE user_identities (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider       text        NOT NULL,
    subject        text        NOT NULL,
    issuer         text        NOT NULL DEFAULT '',
    email          text        NOT NULL DEFAULT '',
    email_verified boolean     NOT NULL DEFAULT false,
    created_at     timestamptz NOT NULL DEFAULT now(),
    last_login_at  timestamptz
);

CREATE UNIQUE INDEX user_identities_provider_subject_key
    ON user_identities (provider, subject, issuer);

CREATE UNIQUE INDEX user_identities_user_provider_key
    ON user_identities (user_id, provider);

CREATE INDEX user_identities_user_id_idx
    ON user_identities (user_id);

-- ---------------------------------------------------------------------------
-- m117 (GH #414 Phase 1): sites.monitoring_paused_by → users. Declared here
-- rather than inline on sites because users is defined after sites in this
-- file (same reason as sites_client_tenant_fkey).
--
-- ON DELETE SET NULL. sites rows outlive users, and the pause must survive the
-- deletion of whoever set it: CASCADE would delete the SITE, and RESTRICT would
-- make an old pause block account deletion with 23503. SET NULL keeps the
-- operational fact (this site is paused) and drops only the display fact (who
-- paused it), which is the right degradation and this schema's unanimous
-- convention for an actor column — update_runs.created_by, invitations.revoked_by,
-- site_connection_events.actor_user_id, client_members.invited_by. The CASCADE
-- references to users are ownership rows (client_members.user_id,
-- user_identities.user_id), which this is not.
--
-- Not composite with tenant_id: users are tenant-agnostic here (membership is a
-- join table) so there is no users (id, tenant_id) key to reference, and the
-- database therefore cannot enforce that the pauser belongs to the site's
-- tenant. The service must set this from the authenticated actor, never from
-- request input.
ALTER TABLE sites
    ADD CONSTRAINT sites_monitoring_paused_by_fkey
    FOREIGN KEY (monitoring_paused_by) REFERENCES users (id)
    ON DELETE SET NULL;

-- m117: the auto-resume sweep's index, and deliberately NOT an index on
-- "monitoring_paused_at IS NULL". That predicate matches ~99% of rows, so a
-- partial index on it is a full-size index wearing a WHERE clause, paid for on
-- every write to the hottest-written table in this schema; and no scheduler
-- could use it, because the fleet-wide enumerations in db/query/sites.sql
-- (ListEnrolledSitesAllTenants, ListEnrolledSitesForProbe,
-- ListConnectedSiteIDsForScreenshot) are uncapped sequential scans with no
-- index on enrolled_at at all. This index is the inverse and is near-empty by
-- construction: it covers only sites with a live pause AND a scheduled resume,
-- which is what the phase-2 auto-resume sweep ("resume_at <= now()") reads.
CREATE INDEX sites_monitoring_resume_due_idx ON sites (monitoring_resume_at)
    WHERE monitoring_resume_at IS NOT NULL
      AND monitoring_paused_at IS NOT NULL;

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
    revoked_at   timestamptz,
    -- m120 (#510): explicit capability set + site allowlist, so a machine
    -- principal can be granted least privilege instead of a whole role rank.
    -- What sort of principal this key represents. Closed discriminator.
    kind             text   NOT NULL DEFAULT 'integration',
    -- How permissions are computed: 'role' (legacy, authz.Allows over role) or
    -- 'capability' (the capabilities set is authoritative and role is NOT
    -- consulted). Deliberately redundant with `capabilities IS NOT NULL` and
    -- held in lockstep by api_keys_auth_model_capabilities_check: SQL NULL and
    -- '{}' both scan into a zero-length []string in Go, and that collapse is
    -- the fail-open this column exists to prevent.
    auth_model       text   NOT NULL DEFAULT 'role',
    -- Nullable, NO default. '{}' as a default would give every pre-existing key
    -- a zero-length capability set and strip the fleet's authority at boot.
    capabilities     text[],
    -- 'org' (tenant-wide, the existing behaviour) or 'site'. Values are exactly
    -- domain.ScopeOrg / domain.ScopeSite.
    site_scope       text   NOT NULL DEFAULT 'org',
    -- Site allowlist. STORED HERE, NOT ENFORCED HERE: no RLS policy reads it.
    -- The boundary is application-enforced in Go at one audited chokepoint in
    -- v1; database-level site scoping for API-key principals is v2. NOT NULL
    -- with an empty default because site_scope carries the "is restricted"
    -- signal, so this column never needs NULL to mean "unrestricted".
    allowed_site_ids uuid[] NOT NULL DEFAULT '{}'::uuid[],

    -- m120: closed structural discriminators ARE constrained here.
    CONSTRAINT api_keys_kind_check       CHECK (kind IN ('integration', 'agent')),
    CONSTRAINT api_keys_auth_model_check CHECK (auth_model IN ('role', 'capability')),
    CONSTRAINT api_keys_site_scope_check CHECK (site_scope IN ('org', 'site')),
    -- Shape of the capability set only. The 30-string permission vocabulary is
    -- owned by internal/authz/role.go and grows with ordinary feature work;
    -- enumerating it in a CHECK would make every new permission fail 23514 in
    -- production until a migration caught up, and migrations apply inside
    -- main() at boot. Uses IMMUTABLE primitives exclusively (array_to_string is
    -- STABLE, so a whole-array regex is not available here).
    CONSTRAINT api_keys_capabilities_shape_check CHECK (
        capabilities IS NULL
        OR (
            coalesce(array_ndims(capabilities), 1) = 1
            AND cardinality(capabilities) <= 64
            AND array_position(capabilities, NULL) IS NULL
            AND NOT ('' = ANY (capabilities))
        )
    ),
    -- Keeps auth_model and capabilities from ever diverging.
    CONSTRAINT api_keys_auth_model_capabilities_check CHECK (
        (auth_model = 'capability' AND capabilities IS NOT NULL)
        OR (auth_model = 'role' AND capabilities IS NULL)
    ),
    -- An org-scoped key must not carry an allowlist: that half-state invites two
    -- readers to disagree about where the boundary is.
    CONSTRAINT api_keys_site_scope_allowlist_check CHECK (
        site_scope = 'site' OR cardinality(allowed_site_ids) = 0
    ),
    -- The least-privilege guarantee in the database: an agent key can never
    -- fall back to whole-role authority.
    CONSTRAINT api_keys_agent_capability_check CHECK (
        kind <> 'agent' OR auth_model = 'capability'
    )
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
-- m34: read-only cross-tenant SELECT for the app.agent scope (set by InAgentTx),
-- mirroring sites_agent. Lets backend-only paths — notably the superadmin
-- orphaned-org cleanup on user delete — see membership counts across tenants to
-- decide which now-memberless orgs are safe to remove. SELECT-only: no
-- cross-tenant writes.
CREATE POLICY memberships_agent ON memberships
    FOR SELECT
    USING (current_setting('app.agent', true) = 'on');

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
-- M92 (M16 Phase C1): the superadmin accounts-list aggregate query
-- (internal/admin) computes each tenant's last_activity (GREATEST across
-- audit_log/sites/memberships) across ALL tenants in one query — mirrors
-- memberships_agent/sites_agent exactly. SELECT-only: no cross-tenant writes,
-- and this policy grants no bypass of the append-only privilege revocation
-- (UPDATE/DELETE remain revoked from wpmgr_app regardless of RLS).
CREATE POLICY audit_log_agent ON audit_log
    FOR SELECT
    USING (current_setting('app.agent', true) = 'on');

-- admin_delete_empty_tenant (m35): SECURITY DEFINER helper for the superadmin
-- orphaned-org cleanup. Verified prod failure (Cloud SQL log): a direct
-- "DELETE FROM tenants" by wpmgr_app cascades to the RI delete
-- 'DELETE FROM ONLY public.audit_log WHERE $1 = tenant_id' and fails with
-- "permission denied for table audit_log" (42501), because audit_log is
-- insert-only for wpmgr_app (m1 revokes UPDATE/DELETE/TRUNCATE — it is
-- tamper-evident) and that cascade is privilege-checked against the caller.
--
-- This function runs as its OWNER (the migration role, which retains DELETE on
-- audit_log) and removes the tenant's audit rows EXPLICITLY first, so the tenant
-- delete's ON DELETE CASCADE never has to touch audit_log — making the delete
-- correct regardless of whether the FK cascade is privilege-checked against the
-- caller or the owner, and WITHOUT granting wpmgr_app any standing delete on
-- audit_log. app.agent='on' (set in-body, NOT via a function SET clause — that
-- would require superuser ownership or GRANT SET ON PARAMETER and abort the
-- migration under the prod non-superuser owner) lets the emptiness checks see
-- rows across tenants under FORCE RLS (memberships_agent + sites_agent);
-- app.tenant_id is scoped locally around the audit_log delete so
-- the audit_log_tenant_isolation USING clause matches the target rows when the
-- owner is itself subject to FORCE RLS. search_path is pinned. EXECUTE is granted
-- only to wpmgr_app (in the migration). Note: deleting an orphaned org also
-- discards any pending invitations to it (invitations cascades from tenants) —
-- acceptable, since the org's only member was just deleted. Guards: removes a
-- tenant ONLY when it has zero memberships and zero sites. Returns whether a
-- tenant row was deleted.
--
-- Security review Finding A (m91): the original body set app.agent='on' and
-- returned on either branch WITHOUT restoring it — set_config(...,true) is
-- NOT rolled back at function exit, so 'on' leaked into the rest of the
-- caller's transaction for every call, disabling that transaction's
-- tenant-isolation RLS check on every statement afterward. v_prev_agent now
-- captures the caller's prior app.agent value and both branches fall through
-- to a single RETURN that restores it first. (m35 already ran in prod before
-- this fix existed; the corrected body is re-issued via CREATE OR REPLACE in
-- m91 so prod actually receives it on next boot — see m91's migration file.)
CREATE OR REPLACE FUNCTION admin_delete_empty_tenant(p_tenant_id uuid)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_count integer;
    v_result boolean := false;
    v_prev_agent text := current_setting('app.agent', true);
BEGIN
    -- app.agent='on' makes the emptiness checks see rows across tenants under
    -- FORCE RLS. Set in-body (set_config needs no special privilege; InAgentTx
    -- uses it too), NOT as a function SET clause — a SET clause on the custom
    -- app.agent placeholder GUC requires superuser ownership / GRANT SET ON
    -- PARAMETER, which the prod non-superuser owner lacks and which would abort
    -- this CREATE FUNCTION and roll back the migration.
    PERFORM set_config('app.agent', 'on', true);
    IF NOT (
        EXISTS (SELECT 1 FROM memberships m WHERE m.tenant_id = p_tenant_id)
        OR EXISTS (SELECT 1 FROM sites s WHERE s.tenant_id = p_tenant_id)
    ) THEN
        -- Explicitly remove the tenant's append-only audit rows as the function
        -- owner (which keeps DELETE on audit_log). app.tenant_id is scoped to
        -- this tenant so the FORCE-RLS USING clause matches; reset immediately
        -- after. Doing this before the tenant delete keeps audit_log out of the
        -- cascade entirely.
        PERFORM set_config('app.tenant_id', p_tenant_id::text, true);
        DELETE FROM audit_log WHERE tenant_id = p_tenant_id;
        PERFORM set_config('app.tenant_id', '', true);
        -- Now delete the tenant; remaining cascades hit only tables the owner
        -- may delete and no longer include any audit_log rows.
        DELETE FROM tenants t WHERE t.id = p_tenant_id;
        GET DIAGNOSTICS v_count = ROW_COUNT;
        v_result := v_count > 0;
    END IF;
    -- Single return path: always restore app.agent to whatever the caller's
    -- transaction had before this call, whether the tenant was empty or not.
    PERFORM set_config('app.agent', coalesce(v_prev_agent, ''), true);
    RETURN v_result;
END;
$$;
-- Mirror the migration's grants so this reference stays faithful (the runtime DB
-- is built from migrations, not this file): the function is NOT PUBLIC-callable.
REVOKE ALL ON FUNCTION admin_delete_empty_tenant(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION admin_delete_empty_tenant(uuid) TO wpmgr_app;

-- ---------------------------------------------------------------------------
-- system_audit_log  (M93 — GH #152 part 2)
-- ---------------------------------------------------------------------------
-- A durable, tenant-INDEPENDENT audit trail for events that no single tenant's
-- own hash-chained audit_log can hold. tenant_id carries NO FK to tenants (a
-- plain column) and tenant_name is denormalized, so a row here survives BOTH
-- the Lane-A empty-org immediate hard-delete (which wipes that tenant's own
-- audit_log outright, via admin_delete_empty_tenant) and the Lane-B
-- grace-window purge (admin_purge_tenant, below, which eventually does the
-- same).
--
-- TWO WRITERS, and the second is why the nil tenant_id below is a real value
-- rather than an accident:
--   * org deletion (internal/org.Handler.recordSystemAudit), the original
--     writer, recording an action whose subject organisation is going away.
--   * tenantless authentication events (internal/auth.Repo
--     .RecordTenantlessAuthEvent): an account with no membership at all has no
--     tenant to file against, and audit_log.tenant_id references tenants, so
--     those events were previously rejected by the database and dropped. They
--     land here with tenant_id = the nil uuid and an empty tenant_name.
--
-- ONE READER: GET /api/v1/admin/system-audit (internal/admin.Handler
-- .systemAudit), superadmin-gated, newest first, keyset-paged on
-- (occurred_at, id).
--
-- No RLS: mirrors `tenants` itself (see the file header): this is not
-- tenant-scoped data, is written only by trusted CP code, and is never exposed
-- to a tenant-scoped request. The one reader is gated by the superadmin
-- middleware on its route, not by a policy on this table, because these rows
-- deliberately span every account on the install.
CREATE TABLE system_audit_log (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    occurred_at timestamptz NOT NULL DEFAULT now(),
    actor_type  text        NOT NULL,
    actor_id    uuid,
    action      text        NOT NULL,
    tenant_id   uuid        NOT NULL,
    tenant_name text        NOT NULL,
    metadata    jsonb       NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX system_audit_log_tenant_id_idx ON system_audit_log (tenant_id, occurred_at);

-- admin_purge_tenant (M93): SECURITY DEFINER helper for internal/org.PurgeWorker
-- — the grace-window destructive purge of a POPULATED tenant (Lane B). Modeled
-- on admin_delete_empty_tenant above but WITHOUT the emptiness guard: it is
-- called only after an owner already confirmed deletion via
-- DELETE /api/v1/orgs/{orgId} and the grace window has elapsed. Like
-- admin_delete_empty_tenant it explicitly clears audit_log first (wpmgr_app
-- has no DELETE grant on the append-only trail — see that function's comment
-- for the full 42501 story), as the function OWNER, before the tenant delete;
-- every other row cascades from that single DELETE FROM tenants.
--
-- GUC handling is DELIBERATELY DIFFERENT from admin_delete_empty_tenant, and
-- the difference matters: that function only ever purges an EMPTY tenant (its
-- guard proves zero memberships/sites), so it is safe for it to blank
-- app.tenant_id back to '' before its own tenant delete — no child-table
-- cascade rows exist there to protect. This function purges a POPULATED
-- tenant: every tenant-scoped table's baseline permissive
-- "<table>_tenant_isolation" policy (USING tenant_id = app.tenant_id) must see
-- p_tenant_id for the ENTIRE cascade the final DELETE triggers — exactly as it
-- would if ordinary application code ran the same cascade under InTenantTx
-- (itself just a transaction-scoped SET of this same GUC). So app.tenant_id is
-- set ONCE at the top and is NEVER blanked before the tenants delete — only
-- restored, once, on the single return path (M91 Finding A GUC-leak lesson:
-- set_config(...,true) is NOT rolled back at function exit — the "true"/
-- is_local flag scopes the change to the CALLER's transaction, not the
-- function invocation — so an unrestored value would leak into every
-- statement the caller's transaction runs afterward). Blanking it early here,
-- as admin_delete_empty_tenant safely does for its always-empty target, would
-- make every cascaded child-table DELETE see zero visible rows under FORCE
-- ROW LEVEL SECURITY — silently leaving every one of that tenant's rows
-- behind (an orphan leak, not a hard failure) while `tenants` itself still
-- gets removed. No app.agent is needed here at all (unlike
-- admin_delete_empty_tenant, which uses it for its own cross-tenant emptiness
-- EXISTS checks) — this function only ever touches rows scoped to the single
-- p_tenant_id it is given.
CREATE OR REPLACE FUNCTION admin_purge_tenant(p_tenant_id uuid)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_count integer;
    v_prev_tenant text := current_setting('app.tenant_id', true);
BEGIN
    PERFORM set_config('app.tenant_id', p_tenant_id::text, true);
    DELETE FROM audit_log WHERE tenant_id = p_tenant_id;
    DELETE FROM tenants t WHERE t.id = p_tenant_id;
    GET DIAGNOSTICS v_count = ROW_COUNT;
    PERFORM set_config('app.tenant_id', coalesce(v_prev_tenant, ''), true);
    RETURN v_count > 0;
END;
$$;
REVOKE ALL ON FUNCTION admin_purge_tenant(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION admin_purge_tenant(uuid) TO wpmgr_app;

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

-- agent_nonces_site_scope (m19) — RESTRICTIVE site-scope gate. Live since m19,
-- declared here as of GH #470. See the note on update_tasks_site_scope for why
-- the absence mattered: it made this file answer "not site-scoped" to a
-- question where that is the dangerous direction to be wrong in.
CREATE POLICY agent_nonces_site_scope ON agent_nonces
    AS RESTRICTIVE FOR ALL
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(
                nullif(current_setting('app.allowed_site_ids', true), ''), ','
            )::uuid[]
        )
    )
    WITH CHECK (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(
                nullif(current_setting('app.allowed_site_ids', true), ''), ','
            )::uuid[]
        )
    );

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
    -- status. NO CHECK CONSTRAINT EXISTS on this column, so this comment (and
    -- the matching COMMENT ON COLUMN, installed by m118 and REPLACED by m119,
    -- which is what \d+ shows in a live database) is the ONLY contract. A
    -- typo'd value stores fine and silently never dispatches.
    --
    -- This list is reconciled against internal/update/model.go's "Run statuses"
    -- const block. m119/#482 added 'halted', which the wave machine had been
    -- writing since long before #463 while appearing in no migration at all.
    --   pending      Created, tasks enqueued for immediate execution (the m3
    --                default). The worker advances it.
    --   scheduled    (m118/#463) Created with a future scheduled_at, not yet
    --                handed to the worker. The dispatcher's due-scan selects
    --                exactly these; update_runs_due_idx is partial on it.
    --   dispatching  (m118/#463) Claimed by the dispatcher for this tick. The
    --                row has left update_runs_due_idx, so a concurrent tick, a
    --                second replica or a restart cannot claim it twice.
    --   running      >=1 task running.
    --   completed    All tasks reached a terminal state.
    --   halted       (m119/#482) Terminal. The run was STOPPED, not finished,
    --                and is deliberately not 'completed', which would erase
    --                that. Two writers: a wave gate refusing to advance an
    --                agent self-update rollout (agent_repo.go haltLocked), and
    --                an operator cancelling a scheduled run before it fired
    --                (cancel_repo.go CancelScheduledRun, #463). The run
    --                vocabulary has no separate 'cancelled' — the task statuses
    --                underneath distinguish the two cases (a halt leaves a
    --                mixture of real outcomes; a cancel leaves them uniformly
    --                'cancelled' with nothing ever sent).
    --   expired      (m118/#463) Passed its dispatch window undispatched.
    --                Terminal and never retried: a deferred bulk update that
    --                fires days late is a surprise, not a service. Distinct
    --                from 'halted', which was stopped by a gate or a human.
    status       text        NOT NULL DEFAULT 'pending',
    dry_run      boolean     NOT NULL DEFAULT false,
    -- scheduled_at is when the run should execute; NULL/now() means immediately.
    scheduled_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX update_runs_tenant_id_created_at_idx ON update_runs (tenant_id, created_at DESC);

-- update_runs_due_idx (m118) serves the #463 dispatcher's cross-tenant tick,
-- "WHERE status = 'scheduled' AND scheduled_at <= now()". The only other index
-- here leads on tenant_id, which that query does not filter on at all, so
-- without this the tick is a sequential scan over every run ever created. A run
-- leaves the index the instant it is claimed ('scheduled' -> 'dispatching'), so
-- it stays proportional to the pending queue, not to the run history. Mirrors
-- backup_schedules_due_idx. The predicate is only the status, because that
-- clause appears verbatim in the consumer's WHERE and so needs no redundant
-- clause repeated by the caller to keep the index usable.
CREATE INDEX update_runs_due_idx ON update_runs (scheduled_at) WHERE status = 'scheduled';

ALTER TABLE update_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE update_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY update_runs_tenant_isolation ON update_runs
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- update_runs_agent (m118) lets the #463 deferred-dispatch scan read AND CLAIM
-- across all tenants under InAgentTx. m3 shipped only the tenant_isolation
-- policy, so under FORCE ROW LEVEL SECURITY a cross-tenant tick satisfied no
-- policy and returned zero rows with no error — the third occurrence of the
-- m84/#96 (backup_schedules) and m89/#131 (update_tasks, this table's sibling)
-- bug class.
--
-- FOR ALL, NOT FOR SELECT, and that is the whole point. The dispatcher claims
-- rows ('scheduled' -> 'dispatching'); a FOR SELECT policy admits the read and
-- admits nothing to the UPDATE, so the claim matches zero rows silently. And
-- PostgreSQL applies the UPDATE policy to SELECT … FOR UPDATE too, so with a
-- read-only policy even the locking SELECT returns nothing. That is exactly how
-- Issue #96 stopped every backup schedule from advancing.
CREATE POLICY update_runs_agent ON update_runs
    FOR ALL
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

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
    -- NO CHECK CONSTRAINT EXISTS; this comment and the COMMENT ON COLUMN
    -- (m118, REPLACED by m119) are the contract. This list is reconciled
    -- against internal/update/model.go's "Task statuses" const block.
    -- m118/#463 adds 'scheduled' and 'expired'; m119/#482 adds 'cancelled',
    -- which the wave machine had been writing all along undeclared:
    --   cancelled  (m119/#482) Terminal. NOTHING WAS EVER SENT TO THIS SITE
    --              and a human or a gate decided that. Written by
    --              agent_repo.go haltLocked (over tasks still 'pending' only —
    --              a 'running' task is left alone, because its command is
    --              already delivered and cancelling it would both record a
    --              falsehood and stop the confirm poll) and by cancel_repo.go
    --              CancelScheduledRun (#463). Distinct from 'skipped' (the CP
    --              declined the target, no human stopped anything), from
    --              'failed' (the site WAS contacted) and from 'expired'.
    --   scheduled  Belongs to a run that is still 'scheduled', not yet
    --              eligible. WARNING: 'scheduled' is NOT in
    --              update_tasks_inflight_target_idx's predicate below
    --              (status IN ('pending','running')), so a scheduled task does
    --              NOT reserve its (tenant, site, target) pair against a
    --              concurrent immediate run. Widening that unique index is a
    --              separate migration with a data-dedup step.
    --   expired    The parent run expired without dispatching, so this task was
    --              never attempted. Terminal. NOT a spelling of 'cancelled':
    --              'cancelled' records a decision somebody made, 'expired'
    --              records that the window closed.
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

-- update_tasks_inflight_target_idx (m88) is the authoritative cross-run dedup
-- guard: at most one pending/running task may exist per (tenant, site,
-- target_type, target_slug) at a time, across ALL runs. Without it, a
-- scheduled auto-update, an operator bulk "Update all", and a client-portal
-- trigger can each create a task for the SAME (site, plugin) within the same
-- ~1s window; the River queue only caps per-tenant concurrency, so several
-- can run concurrently — racing the agent's own rollback-snapshot pruning (a
-- task's own snapshot gets pruned by a sibling task, producing "rollback
-- FAILED / Invalid snapshot id") and running concurrent WordPress
-- Plugin_Upgrader instances against the same plugin directory (can corrupt
-- wp-content/plugins/<slug>). CreateUpdateTask relies on this index as an ON
-- CONFLICT ... DO NOTHING arbiter; the service-level ListInFlightUpdateTargets
-- pre-check (planTasks) narrows the race further but cannot itself be atomic
-- without this index. A task falls out of the partial index once it reaches a
-- terminal status, so a sequential re-update after the prior run finished is
-- unaffected. The versioned m88 migration additionally pre-dedups any
-- pending/running rows a live database already accumulated under the
-- pre-m88 bug before creating this index (schema.sql only carries the
-- resulting end state; see the migration file for the one-time data fix).
CREATE UNIQUE INDEX update_tasks_inflight_target_idx ON update_tasks
    (tenant_id, site_id, target_type, target_slug)
    WHERE status IN ('pending', 'running');

ALTER TABLE update_tasks ENABLE ROW LEVEL SECURITY;
ALTER TABLE update_tasks FORCE ROW LEVEL SECURITY;
CREATE POLICY update_tasks_tenant_isolation ON update_tasks
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- update_tasks_agent (m89) lets the #131 stale-task reaper (ReaperWorker ->
-- ListStaleUpdateTasks) read across ALL tenants in one sweep under InAgentTx.
-- Added after the fact because M3 only shipped the tenant_isolation policy;
-- every query against this table was tenant-scoped until the reaper's
-- cross-tenant sweep needed it (same bug class as m84's backup_schedules fix).
CREATE POLICY update_tasks_agent ON update_tasks
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- update_tasks_site_scope (m19) — the RESTRICTIVE site-scope gate every
-- site-keyed table carries. RESTRICTIVE, so it AND-combines with the
-- permissive policies above and can only ever narrow, never grant: when
-- app.site_scope is 'on' (set by InScopedTenantTx for a site-scoped
-- principal) a row is reachable only if its site_id is in
-- app.allowed_site_ids. When the GUC is unset the first disjunct is true and
-- the policy is inert, so unscoped paths are unaffected.
--
-- DECLARED HERE AS OF GH #470. It has been live in every database since m19,
-- and the m118 and m119 headers both refer to it by name, but this file never
-- declared it — so grepping this file to decide whether update_tasks was
-- site-scoped returned nothing and concluded it was not. That is the exact
-- opposite of the truth, in the wrong direction for a tenant-boundary
-- question. The migrations are authoritative; this file follows them.
CREATE POLICY update_tasks_site_scope ON update_tasks
    AS RESTRICTIVE FOR ALL
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(
                nullif(current_setting('app.allowed_site_ids', true), ''), ','
            )::uuid[]
        )
    )
    WITH CHECK (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(
                nullif(current_setting('app.allowed_site_ids', true), ''), ','
            )::uuid[]
        )
    );

-- ---------------------------------------------------------------------------
-- backup_chunks  (M4 — incremental, content-addressed dedup + GC)
-- ---------------------------------------------------------------------------
-- One row per UNIQUE (tenant, blake3) ciphertext chunk stored in object
-- storage. Chunks are content-addressed by the BLAKE3 hash of their CIPHERTEXT
-- (the agent encrypts client-side with age, then hashes; the CP and S3 only
-- ever see ciphertext). Tenant-scoped + RLS: a tenant can never see or target
-- another tenant's chunks, and the s3_key is namespaced by tenant so a presign
-- for one tenant cannot address another's chunk prefix.
--
-- refcount IS OBSERVABILITY ONLY. It counts ORIGIN references (how many
-- manifest entries introduced the chunk), not live ones, and ADR-050 retracted
-- the rule this comment used to state, that "GC deletes a chunk from S3 only
-- when refcount reaches zero". It was wrong in the dangerous direction: the
-- agent only re-submits changed or new files, so a carry-forward chunk's origin
-- row lives in exactly ONE generation and its refcount can sit at zero while a
-- live snapshot still needs it. No delete decision consults refcount. The real
-- rule is mark-and-sweep (internal/backup/gc.go): a chunk is deleted only when
-- it is unreachable from EVERY retained snapshot across ALL sites in the tenant
-- (dedup is tenant-global), AND its GREATEST(created_at, last_referenced_at)
-- predates the grace floor, AND a ground-truth re-check against live
-- backup_manifest_entries under the row lock agrees. Anyone designing near this
-- table should read that file, not this column.
--
-- NOTE for GH #402: there is deliberately NO foreign key from here to sites.
-- Deleting one site cascades its backup_snapshots away but leaves this
-- inventory intact, which is exactly what lets the tenant-wide sweep recompute
-- reachability and spare a chunk the deleted site shared with a LIVE one.
--
-- The tenant_id foreign key below IS a cascade, and it still is: deleting an
-- org's last site and then the emptied org destroys this inventory outright,
-- because admin_delete_empty_tenant hard-deletes the tenant row while freeing no
-- object storage. That WAS a permanent leak (GH #408). It is closed by
-- tenant_object_reclaim (m116) rather than by changing this key: the same
-- statement now writes a record naming the tenant, in its own transaction and
-- with no foreign key of its own, and the tenant drain frees chunks/<tenant>/
-- afterwards. Removing the cascade here is NOT the fix and would leave chunk
-- rows pointing at a tenant that no longer exists.
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
-- M92 (M16 Phase C1): the superadmin accounts-list aggregate query
-- (internal/admin) sums managed-storage bytes across ALL tenants in one
-- query — mirrors memberships_agent/sites_agent exactly. SELECT-only: no
-- cross-tenant writes.
CREATE POLICY backup_chunks_agent ON backup_chunks
    FOR SELECT
    USING (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- site_destinations  (M7 / ADR-036 P1 storage adapter)
-- ---------------------------------------------------------------------------
-- Where a site's backup chunks should land: the WPMgr-managed CP bucket
-- (kind=cp, the historical default), a per-site Local folder on the agent host
-- (kind=local), or a customer-owned S3-compatible bucket (kind=s3_compat).
-- backup_snapshots.destination_id (below) references the row a given snapshot
-- was taken against; NULL means the legacy CP-global bucket.
--
-- Threat model: secret_key_enc is age-encrypted at rest with the CP's shared
-- identity (internal/cryptbox); the plaintext customer S3 secret never sits on
-- disk in clear. RLS isolates rows per tenant; the partial unique index
-- enforces at most one default destination per (tenant_id, site_id).
CREATE TABLE site_destinations (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    -- site_id is nullable so a future flow can introduce tenant-wide defaults;
    -- V1 always writes a non-null site_id.
    site_id          uuid        REFERENCES sites (id) ON DELETE CASCADE,
    kind             text        NOT NULL CHECK (kind IN ('cp', 'local', 's3_compat')),
    label            text        NOT NULL,
    endpoint         text        NOT NULL DEFAULT '',
    region           text        NOT NULL DEFAULT '',
    bucket           text        NOT NULL DEFAULT '',
    path_prefix      text        NOT NULL DEFAULT '',
    access_key_id    text        NOT NULL DEFAULT '',
    secret_key_enc   bytea,
    force_path_style boolean     NOT NULL DEFAULT false,
    is_default       boolean     NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX site_destinations_site_idx ON site_destinations (site_id)
    WHERE site_id IS NOT NULL;

-- Exactly one default per (tenant, site). NULL site_id rows can't have a
-- default yet (and V1 never creates any) — the partial filter scopes the
-- uniqueness correctly.
CREATE UNIQUE INDEX site_destinations_default_idx ON site_destinations (tenant_id, site_id)
    WHERE is_default = true AND site_id IS NOT NULL;

ALTER TABLE site_destinations ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_destinations FORCE ROW LEVEL SECURITY;

CREATE POLICY site_destinations_tenant_isolation ON site_destinations
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- The agent-facing presign routing reads destinations under the agent GUC when
-- no tenant is set; the agent path always knows the tenant from the verified
-- Ed25519 identity though, so this stays SELECT-only defence-in-depth for
-- cross-tenant maintenance jobs (mirrors backup_chunks_agent).
CREATE POLICY site_destinations_agent ON site_destinations
    FOR SELECT
    USING (current_setting('app.agent', true) = 'on');

-- m19 AS RESTRICTIVE collaborator site-scope policy (per-site sharing): a
-- site-scoped principal may only see destinations for a site they were
-- explicitly granted. site_id is nullable so the ANY check is skipped safely
-- for a (V1-unused) tenant-wide NULL-site_id row.
CREATE POLICY site_destinations_site_scope ON site_destinations
    AS RESTRICTIVE FOR ALL
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
        )
    )
    WITH CHECK (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
        )
    );

-- ---------------------------------------------------------------------------
-- site_object_reclaim  (m113 / GH #402)
-- ---------------------------------------------------------------------------
-- The durable record of object-storage work that outlives a site delete.
--
-- DELETE /sites/{id} cascades backup_snapshots away, and those rows were the
-- only database record naming the site's per-snapshot manifest.json objects.
-- After the cascade nothing could name them again, so they leaked forever. A
-- row here is INSERTed in the SAME TRANSACTION as the delete (see the site
-- repo's Delete), so it exists if and only if the site row is really gone, and
-- an async worker reclaims the site's storage prefix afterwards.
--
-- NO FOREIGN KEY AT ALL, DELIBERATELY, TO EITHER sites OR tenants.
--
-- A site_id FK with ON DELETE CASCADE would destroy this row in the very
-- statement it exists to survive. A tenant_id FK with ON DELETE CASCADE is the
-- same mistake one level up, and it was real: admin_delete_empty_tenant (org
-- delete Lane A, and the superadmin orphan cleanup) hard-deletes a tenant row
-- with ZERO object-storage cleanup, because an org with no sites and no members
-- was assumed to own no objects. An org whose sites were all deleted first is
-- exactly that shape and does own objects, so the cascade destroyed the
-- reclaim record by precisely the operation that should have triggered it. Only
-- the grace-window PurgeWorker (Lane B) sweeps the seven tenant-scoped roots
-- before its hard delete; Lane A does not, and cannot without putting unbounded
-- network work inside an HTTP request.
--
-- The rule this table exists to embody: a record of what to clean up must
-- outlive the deletion it describes. Referential tidiness is worth less than
-- that. The tenant may be gone by the time the worker runs, and that is a
-- reclaim signal, not an error (see the worker's tenant-state guard).
--
-- The row stores IDENTITY, never a prefix string: the worker derives the
-- prefix from a code constant plus two validated UUIDs, because a stored prefix
-- would turn a corrupt row into an arbitrary-prefix delete instruction and the
-- adjacency is one character ("tenant/" backups, "tenants/" client report PDFs
-- with client PII). kind is the extension point for the other site-scoped
-- roots (rucss/, screenshots/). destination_kind is diagnostic only and never a
-- credential.
--
-- Past the retry cap a task is LEFT VISIBLE, never deleted: a stuck task is the
-- only remaining record that those objects exist.
CREATE TABLE site_object_reclaim (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- No REFERENCES. See the header: a cascade from either parent destroys the
    -- record in the operation it is supposed to survive.
    tenant_id        uuid        NOT NULL,
    site_id          uuid        NOT NULL,
    -- Which site-scoped storage root to reclaim ('backup_manifest' today).
    --
    -- A CLOSED SET, enforced here rather than only in the worker. The operator
    -- remedy for objects orphaned before m113 is a hand-written INSERT into this
    -- table (the statement is in the m113 header), so a typo in this column is a
    -- realistic event with an unrealistic cost: the worker cannot derive a prefix
    -- for a kind it does not know, and those objects have no other record
    -- anywhere. Refusing the row outright puts the failure in front of the person
    -- typing it, at the moment they can fix it, instead of parking it in a table
    -- nobody reads. backup.ReclaimKinds is the code-side set and tests/contract
    -- holds the two together.
    kind             text        NOT NULL DEFAULT 'backup_manifest'
        CONSTRAINT site_object_reclaim_kind_check CHECK (kind IN ('backup_manifest')),
    -- 'cp' | 'local' | 's3_compat' | NULL for the legacy CP-global bucket.
    destination_kind text,
    attempts         int         NOT NULL DEFAULT 0,
    next_attempt_at  timestamptz NOT NULL DEFAULT now(),
    last_error       text,
    completed_at     timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX site_object_reclaim_site_kind_key
    ON site_object_reclaim (tenant_id, site_id, kind);
CREATE INDEX site_object_reclaim_tenant_idx ON site_object_reclaim (tenant_id);
CREATE INDEX site_object_reclaim_due_idx ON site_object_reclaim (next_attempt_at)
    WHERE completed_at IS NULL;

ALTER TABLE site_object_reclaim ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_object_reclaim FORCE ROW LEVEL SECURITY;

CREATE POLICY site_object_reclaim_tenant_isolation ON site_object_reclaim
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- The reclaim sweep is cross-tenant and runs under InAgentTx.
CREATE POLICY site_object_reclaim_agent ON site_object_reclaim
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- m19 AS RESTRICTIVE collaborator site-scope policy, the same shape
-- site_destinations carries a few hundred lines up. This table is site-keyed,
-- so without it the only thing the database would refuse is another TENANT, and
-- a collaborator invited to exactly one site could read or write reclaim rows
-- naming every other site in the organisation. That is the class m112 closed
-- for the email domain after seven separate handler-level doors kept appearing;
-- this is the same class, and it gets the policy on the way in rather than
-- after the eighth. site_id is NOT NULL here, so unlike the email tables there
-- is no inheriting row to keep readable and one FOR ALL policy is the whole
-- answer. Both the operator enqueue (InTenantTx) and the worker (InAgentTx)
-- leave app.site_scope unset, so for them the first branch is a tautology.
CREATE POLICY site_object_reclaim_site_scope ON site_object_reclaim
    AS RESTRICTIVE FOR ALL
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
        )
    )
    WITH CHECK (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
        )
    );

-- ---------------------------------------------------------------------------
-- tenant_object_reclaim  (m116 / GH #408)
-- ---------------------------------------------------------------------------
-- The durable record of object-storage work that outlives a TENANT delete.
--
-- The sibling of site_object_reclaim above, one level up, for the same reason.
-- backup_chunks.tenant_id is ON DELETE CASCADE (m4), so admin_delete_empty_tenant
-- destroys the whole chunk inventory for a tenant while freeing zero object
-- storage: after it, chunks/<tenant_id>/ is named by nothing. A row here is
-- INSERTed by admin_delete_empty_tenant itself, in the delete's own transaction
-- and gated on the delete having affected a row, so it exists if and only if the
-- tenants row is really gone. The drain runs asynchronously afterwards.
--
-- NO FOREIGN KEY, DELIBERATELY. A tenant_id FK with ON DELETE CASCADE would
-- destroy this row in the exact statement it exists to survive. That is not
-- hypothetical: it is what m113's first version did, and it reinstated GH #402
-- at tenant level.
--
-- The row stores IDENTITY, one uuid, never a prefix string. The worker derives
-- every root from org.ObjectStoragePrefixes, shared with the Lane B purge so the
-- two can never disagree, because a stored prefix would turn a corrupt row into
-- an arbitrary-prefix delete instruction and the adjacency is one character.
--
-- NO SITE-SCOPE POLICY, and that is deliberate rather than the m112 omission:
-- there is no site_id column here, so these rows name no site a collaborator
-- could be denied. m113's restrictive policy exists because ITS rows name other
-- sites. tenant_isolation is vestigial by construction (these rows' tenants are
-- gone) and is kept because a tenant_id column with no isolation policy is the
-- pattern the house rule forbids.
--
-- Past the retry cap a task is LEFT VISIBLE, never deleted: it is the only
-- remaining record that those objects exist.
CREATE TABLE tenant_object_reclaim (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- No REFERENCES. See the header.
    tenant_id       uuid        NOT NULL,
    -- 'tenant_storage' means all seven roots org.ObjectStoragePrefixes returns,
    -- which is every root Lane B sweeps, including chunks/<tenant>/. A CLOSED
    -- set for the same reason site_object_reclaim.kind is closed; the code-side
    -- copy is backup.TenantReclaimKinds and tests/contract holds them together.
    kind            text        NOT NULL DEFAULT 'tenant_storage'
        CONSTRAINT tenant_object_reclaim_kind_check CHECK (kind IN ('tenant_storage')),
    attempts        int         NOT NULL DEFAULT 0,
    -- The 24 hour floor: defence in depth, not the safety proof, so an operator
    -- who deleted the wrong organisation has a day to restore a pre-delete dump
    -- before the bytes go. Lane B effectively has seven days; Lane A has none.
    next_attempt_at timestamptz NOT NULL DEFAULT now() + interval '24 hours',
    last_error      text,
    completed_at    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX tenant_object_reclaim_tenant_kind_key
    ON tenant_object_reclaim (tenant_id, kind);
CREATE INDEX tenant_object_reclaim_due_idx ON tenant_object_reclaim (next_attempt_at)
    WHERE completed_at IS NULL;

ALTER TABLE tenant_object_reclaim ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_object_reclaim FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_object_reclaim_tenant_isolation ON tenant_object_reclaim
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- The drain is cross-tenant and runs under InAgentTx; so does the enqueue inside
-- admin_delete_empty_tenant (m91 sets app.agent='on' in-body) and the
-- `wpmgr-cli reclaim` operator path.
CREATE POLICY tenant_object_reclaim_agent ON tenant_object_reclaim
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

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
    -- locked: operator pin (m49). When true, the retention GC MUST skip this
    -- snapshot regardless of retention_days/keep_last. Explicit unlock required.
    locked        boolean     NOT NULL DEFAULT false,
    -- destination_id (M7 / ADR-036 P1): which site_destinations row this
    -- snapshot's chunks were stored against. NULL = the legacy CP-managed
    -- bucket — the value every pre-P1 snapshot carries and still the default
    -- for a site with no configured destination.
    destination_id uuid       REFERENCES site_destinations (id) ON DELETE SET NULL,
    -- progress: phpbu-engine real-time progress (M5.6 / ADR-032). Latest phase
    -- payload posted by the agent runner. Shape:
    --   {"phase": "uploading", "phase_detail": {"chunks_done": 17, ...}}
    -- The watchdog (backup_progress_watchdog River periodic) scans for stalled
    -- runs via progress_updated_at. GH #279 two-tier policy: past the SOFT
    -- threshold the watchdog stamps stalled_at below (status stays 'running',
    -- a slow-but-alive run can still complete); only past the much longer HARD
    -- threshold does it fail the run, with a distinct stall-timeout reason. JSONB
    -- so we can evolve the payload shape without migrations.
    progress             jsonb       NOT NULL DEFAULT '{}'::jsonb,
    progress_updated_at  timestamptz,
    -- stalled_at (m104 / GH #279): set by the watchdog when a running snapshot
    -- has gone quiet past the soft threshold but is not yet hard-failed. NULL
    -- means healthy. Cleared by the next proof of life (a presign, manifest
    -- submit, or progress POST) so the run resumes silently. Never touched by
    -- the hard-fail path — a failed snapshot's stalled_at is left as-is since
    -- status alone drives every downstream check.
    stalled_at    timestamptz,
    started_at    timestamptz,
    finished_at   timestamptz,
    -- m44 incremental backup columns (ADR-048)
    is_incremental       boolean     NOT NULL DEFAULT false,
    parent_snapshot_id   uuid        REFERENCES backup_snapshots (id) ON DELETE SET NULL,
    base_snapshot_id     uuid        REFERENCES backup_snapshots (id) ON DELETE SET NULL,
    chain_id             uuid,
    generation           integer     NOT NULL DEFAULT 0,
    cycle_files_scanned  bigint      NOT NULL DEFAULT 0,
    cycle_files_changed  bigint      NOT NULL DEFAULT 0,
    cycle_files_deleted  bigint      NOT NULL DEFAULT 0,
    cycle_bytes_uploaded bigint      NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX backup_snapshots_tenant_site_idx ON backup_snapshots (tenant_id, site_id, created_at DESC);
CREATE INDEX backup_snapshots_tenant_created_idx ON backup_snapshots (tenant_id, created_at DESC);
-- Watchdog scan: pick running snapshots whose latest progress is older than the
-- stall threshold. Filtered btree on status keeps the predicate selective.
CREATE INDEX backup_snapshots_running_progress_idx ON backup_snapshots (progress_updated_at)
    WHERE status = 'running';
-- m44: chain lookup for incremental GC and restore planning.
CREATE INDEX backup_snapshots_chain_id_idx ON backup_snapshots (chain_id)
    WHERE chain_id IS NOT NULL;
CREATE INDEX backup_snapshots_parent_id_idx ON backup_snapshots (parent_snapshot_id)
    WHERE parent_snapshot_id IS NOT NULL;
-- m45: composite index for ListChainSnapshots (chain_id + generation predicate).
CREATE INDEX backup_snapshots_chain_gen_idx ON backup_snapshots (chain_id, generation)
    WHERE chain_id IS NOT NULL;
-- m49: index for the GC locked-pin check.
CREATE INDEX backup_snapshots_locked_idx ON backup_snapshots (tenant_id, locked)
    WHERE locked = true;
-- m75 (issue #68): belt-and-suspenders guard against duplicate in-flight backups.
-- At most one pending-or-running snapshot per site at a time. Partial so
-- completed/failed rows are unconstrained and retention GC is unaffected.
CREATE UNIQUE INDEX backup_snapshots_one_inflight_per_site ON backup_snapshots (site_id)
    WHERE status IN ('pending', 'running');
-- M7 / ADR-036 P1: presign routing + destination-CRUD cascade lookups.
CREATE INDEX backup_snapshots_destination_id_idx ON backup_snapshots (destination_id)
    WHERE destination_id IS NOT NULL;
-- m96 (GH #168): at most one COMPLETED row per (chain_id, generation). A
-- failed/pending/running retry that reused a generation is unconstrained —
-- only two COMPLETED rows at the same slot are rejected. Closes the
-- duplicate-row gap that let a retention-GC reachability computation lose
-- track of a within-retention chunk (see the m96 migration for the full
-- root-cause writeup).
CREATE UNIQUE INDEX backup_snapshots_chain_gen_completed_uidx ON backup_snapshots (chain_id, generation)
    WHERE status = 'completed';

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
-- backup_file_index  (m44 — per-file delta tracking for incremental backups)
-- ---------------------------------------------------------------------------
-- One row per file per snapshot (or tombstone for deleted files). chunk_hashes
-- is the ORDERED list of BLAKE3 hashes that reassemble the file. Used by the
-- chain-restore planner and the incremental GC reachability walk.
CREATE TABLE backup_file_index (
    id           uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL,
    snapshot_id  uuid        NOT NULL,
    file_path    text        NOT NULL,
    file_size    bigint      NOT NULL DEFAULT 0,
    file_mtime   bigint      NOT NULL DEFAULT 0,
    file_blake3  text        NOT NULL DEFAULT '',
    chunk_hashes text[]      NOT NULL DEFAULT '{}',
    is_tombstone boolean     NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id),
    FOREIGN KEY (snapshot_id) REFERENCES backup_snapshots (id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id)   REFERENCES tenants (id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX backup_file_index_snapshot_path_key ON backup_file_index (snapshot_id, file_path);
CREATE INDEX backup_file_index_snapshot_path_idx ON backup_file_index (snapshot_id, file_path);
CREATE INDEX backup_file_index_tenant_id_idx ON backup_file_index (tenant_id);

ALTER TABLE backup_file_index ENABLE ROW LEVEL SECURITY;
ALTER TABLE backup_file_index FORCE ROW LEVEL SECURITY;
CREATE POLICY backup_file_index_tenant_isolation ON backup_file_index
    USING  (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY backup_file_index_agent ON backup_file_index
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
-- m96 (GH #168 P2): serves the retention GC's ground-truth guard
-- (ChunkStillReferenced: "is this hash still referenced by ANY manifest row
-- for the tenant?") via an index scan instead of a sequential scan.
CREATE INDEX backup_manifest_entries_chunk_hashes_gin ON backup_manifest_entries USING gin (chunk_hashes);

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
    -- ADR-048 P5: per-schedule incremental opt-in. Default false preserves the
    -- full-backup behaviour for every existing/new schedule (no regression).
    incremental_enabled boolean NOT NULL DEFAULT false,
    -- Optional override of BackupBaseWindowDays (7). NULL = use the constant.
    base_window_days    integer NULL CHECK (base_window_days IS NULL OR base_window_days BETWEEN 1 AND 365),
    -- m49 — per-schedule notification settings (Track B).
    notify_on_completion text    NOT NULL DEFAULT 'never'
        CHECK (notify_on_completion IN ('always', 'on_failure', 'never')),
    notify_recipients    jsonb   NOT NULL DEFAULT '[]'::jsonb,
    -- m49 — backup composition / exclusions (Track A).
    backup_components    jsonb   NULL,
    exclude_paths        jsonb   NULL,
    exclude_extensions   jsonb   NULL,
    exclude_file_size_mb integer NULL CHECK (exclude_file_size_mb > 0),
    include_core         boolean NOT NULL DEFAULT false,
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

-- The periodic scheduler enumerates DUE schedules, claims them with FOR UPDATE,
-- and advances next_run_at — all inside InAgentTx (app.agent='on').
-- PostgreSQL applies both SELECT and UPDATE policies to SELECT … FOR UPDATE, so
-- this must be FOR ALL (not FOR SELECT) so the UPDATE USING is satisfied and the
-- locking query returns rows. Mirrors backup_schedule_runs_agent exactly.
-- (Issue #96: the original FOR SELECT policy silently returned 0 rows on every
-- FOR UPDATE attempt, causing ClaimAndAdvanceDueSchedules to never advance any
-- schedule even when due rows existed.)
CREATE POLICY backup_schedules_scheduler ON backup_schedules
    FOR ALL
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

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
-- app_alert_rollout  (m108 - GH #291 Phase 3: app-health alerting rollout)
-- ---------------------------------------------------------------------------
-- A single global row recording whether this deployment already had sites at
-- the moment m108 ran (the "fresh install vs upgrade" decision that gates the
-- app-health alerting default - see alert_configs.app_alerts_enabled below
-- and the design doc's "measure first, alert later" rollout section).
-- Deliberately NOT RLS-scoped: it carries one global, non-tenant,
-- non-sensitive fact, mirroring the `tenants` table's own no-RLS rationale.
-- A schema built from THIS file (rather than replayed from the migrations)
-- has no history to consult, so fresh_install defaults true here - exactly
-- what a from-scratch build represents.
CREATE TABLE app_alert_rollout (
    singleton     boolean     PRIMARY KEY DEFAULT true,
    fresh_install boolean     NOT NULL DEFAULT true,
    decided_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT app_alert_rollout_singleton_chk CHECK (singleton)
);

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
    -- m103 (GH #247) — vulnerability alerting is the THIRD signal on this same
    -- per-tenant channel. notify_vulns is opt-in (default off), mirroring
    -- notify_security. vuln_min_severity is the operator-configurable alert
    -- threshold ('unknown' is deliberately NOT a selectable option here — an
    -- unknown-severity finding always alerts regardless of threshold; see
    -- internal/vuln/alertdispatch.go). vuln_include_in_digest gates a
    -- "new vulnerabilities" section on the existing email digest and defaults
    -- ON so an operator who has digests enabled sees vuln activity by default.
    notify_vulns     boolean   NOT NULL DEFAULT false,
    vuln_min_severity text     NOT NULL DEFAULT 'high'
        CONSTRAINT alert_configs_vuln_min_severity_chk
        CHECK (vuln_min_severity IN ('critical', 'high', 'medium', 'low')),
    vuln_include_in_digest boolean NOT NULL DEFAULT true,
    -- m108 (GH #291 Phase 3) - app-health alerting is the FOURTH signal on
    -- this same per-tenant channel, independent of `enabled` (the existing
    -- reachability channel) so a tenant that already has downtime alerts on
    -- does not silently start receiving app-health alerts too. The literal
    -- DEFAULT here (true) matches a from-scratch build of this file (see
    -- app_alert_rollout above); a REAL upgrade deployment gets this column's
    -- default computed dynamically by m108 from whether it already had
    -- sites, per the design's "measure first, alert later" rollout rule.
    app_alerts_enabled boolean NOT NULL DEFAULT true,
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
-- site_app_alert_state  (m108 - GH #291 Phase 3: app-health alert state)
-- ---------------------------------------------------------------------------
-- Per-site application-health alert state machine - mirrors site_alert_state
-- above exactly, plus ever_app_up: a STICKY flag (set once on the first
-- conclusive app_up=true verdict, never reset false again) gating whether
-- this site may EVER fire an app alert. A site that has never cleared that
-- bar is almost always one whose REST route is blocked/disabled, not one
-- that broke - see internal/uptime/app_alerts.go EvaluateApp. Written inside
-- the SAME TransitionAlertState transaction as site_alert_state (never a
-- separate round-trip), so the two stay consistent and race-free.
CREATE TABLE site_app_alert_state (
    site_id          uuid        PRIMARY KEY REFERENCES sites (id) ON DELETE CASCADE,
    tenant_id        uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    last_status      text        NOT NULL DEFAULT 'unknown',
    consecutive_down integer     NOT NULL DEFAULT 0,
    in_incident      boolean     NOT NULL DEFAULT false,
    ever_app_up      boolean     NOT NULL DEFAULT false,
    last_alert_at    timestamptz,
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX site_app_alert_state_tenant_id_idx ON site_app_alert_state (tenant_id);

ALTER TABLE site_app_alert_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_app_alert_state FORCE ROW LEVEL SECURITY;
CREATE POLICY site_app_alert_state_tenant_isolation ON site_app_alert_state
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- The probe worker updates this state cross-tenant, exactly like
-- site_alert_state_agent.
CREATE POLICY site_app_alert_state_agent ON site_app_alert_state
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- tenant_app_alert_breaker  (m108 - GH #291 Phase 3: fleet circuit breaker)
-- ---------------------------------------------------------------------------
-- One row per tenant: the fleet circuit-breaker's own transition memory.
-- When more than a configurable ratio of a tenant's alert-eligible sites are
-- simultaneously app-down, individual per-site alerts collapse into ONE
-- aggregate notification instead of a page-per-site storm - far more likely
-- to be our own monitoring, or a shared host/network, having a bad day than
-- N unrelated clients breaking at once. tripped is transition-only, exactly
-- like site_alert_state.in_incident: one aggregate alert when it trips, one
-- when it recovers, never a repeating flood - EXCEPT while it stays tripped
-- across many sweeps, an "updated" aggregate CAN fire when the suppressed
-- population materially worsens (see last_down_count below and
-- internal/uptime/app_alerts.go EvaluateAppBreaker's FireUpdate, GH #291
-- Phase 3 Fix 3), so an operator is never left with only the stale count
-- from the moment the breaker first tripped while things get worse.
CREATE TABLE tenant_app_alert_breaker (
    tenant_id       uuid        PRIMARY KEY REFERENCES tenants (id) ON DELETE CASCADE,
    tripped         boolean     NOT NULL DEFAULT false,
    tripped_at      timestamptz,
    last_alert_at   timestamptz,
    -- last_down_count is the down count AT THE TIME OF THE LAST
    -- notification (trip, update, or recovery) - never bumped on a silent
    -- steady-state tick - so a later tick can tell "materially worse since
    -- we last said anything" without a second table.
    last_down_count integer     NOT NULL DEFAULT 0,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE tenant_app_alert_breaker ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_app_alert_breaker FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_app_alert_breaker_tenant_isolation ON tenant_app_alert_breaker
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_app_alert_breaker_agent ON tenant_app_alert_breaker
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
    -- m107 (GH #291 Phase 2): the application-health probe rides the SAME row
    -- as the reachability probe it was piggybacked onto (see
    -- uptime.ProbeWorker.Sweep / appProbeDue) instead of a second row, so no
    -- aggregate that counts/sums site_uptime_probes rows is ever affected by
    -- app-health data. app_up is NULL on every row where no app probe was
    -- attempted (the common case: the app probe runs on a slower cadence
    -- than this reachability probe) and tri-state (true/false/NULL=unknown)
    -- on a row where one was. app_probe_reason is NULL exactly when app_up
    -- is NULL-because-not-attempted, and non-NULL (one of the
    -- AppProbeReason* constants) whenever an app probe actually ran on this
    -- row, including a conclusive "unknown" verdict - see
    -- metrics.Check.AppProbeReason.
    app_up            boolean,
    app_probe_reason  text,
    PRIMARY KEY (id)
);

-- Covering index for QueryFleetUptime: both the lat LATERAL (LIMIT 1 latest
-- probe) and the agg LATERAL (30-day COUNT/AVG) filter on (site_id, tenant_id,
-- probed_at). INCLUDE (up, total_ms) satisfies those columns from the index
-- without a heap fetch, making both laterals index-only scans on all-visible
-- pages. This collapses the cold-cache cost from ~8s to sub-second (m85).
CREATE INDEX site_uptime_probes_agg_idx
    ON site_uptime_probes (site_id, tenant_id, probed_at DESC)
    INCLUDE (up, total_ms);

-- Tenant-wide recent scans (summary endpoints).
CREATE INDEX site_uptime_probes_tenant_time_idx
    ON site_uptime_probes (tenant_id, probed_at DESC);

-- Retention GC delete (WHERE probed_at < cutoff) — leads on probed_at so the
-- daily prune is an index range scan, not a full table scan (m86).
CREATE INDEX site_uptime_probes_probed_at_idx
    ON site_uptime_probes (probed_at);

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
-- Note: the live schema also carries a site_uptime_probes_site_scope
-- RESTRICTIVE policy (m19 20260531050000_m19_orgs_sharing.sql section 5h) —
-- pre-existing drift versus this declarative file that predates this change
-- and is out of scope here; the two NEW tables below include the equivalent
-- policy inline since m19's own header convention requires it on every
-- direct site-keyed table.

-- ---------------------------------------------------------------------------
-- site_uptime_daily  (M99 — durable rollup replacing the interim keep-warm)
-- ---------------------------------------------------------------------------
-- One row per (site, calendar day, UTC), incremented once per probe by the
-- probe worker (metrics.pgStore.UpsertRollup, called from
-- uptime.ProbeWorker.Sweep right after InsertChecks writes the raw probe).
-- QueryFleetUptime (the /api/v1/sites uptime-enrichment query) reads ONLY
-- this table + site_uptime_status below — never the raw site_uptime_probes
-- table — so the query cost is O(days-in-window) per site instead of
-- O(probes-in-window) (~43k rows/site for a 30-day window at a 60s probe
-- cadence). This is the durable fix; it replaces the interim
-- WPMGR_UPTIME_KEEPWARM refresher (removed in the same change).
--
-- Columns mirror exactly what the old per-request aggregate computed so
-- historical uptime % / avg latency are preserved:
--   up_checks        count(*) FILTER (WHERE up)
--   total_checks      count(*)
--   sum_latency_ms    sum(total_ms) FILTER (WHERE up AND total_ms <> 0)
--   latency_samples   count(*) FILTER (WHERE up AND total_ms <> 0)
-- sum_latency_ms / latency_samples reproduces the old
-- AVG(NULLIF(total_ms, 0)) FILTER (WHERE up) exactly — NULLIF(total_ms, 0)
-- excludes zero-latency readings from both the numerator and the
-- denominator, which is why latency_samples cannot be derived from
-- up_checks alone.
CREATE TABLE site_uptime_daily (
    tenant_id       uuid             NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    site_id         uuid             NOT NULL REFERENCES sites   (id) ON DELETE CASCADE,
    day             date             NOT NULL,
    up_checks       integer          NOT NULL DEFAULT 0,
    total_checks    integer          NOT NULL DEFAULT 0,
    sum_latency_ms  double precision NOT NULL DEFAULT 0,
    latency_samples integer          NOT NULL DEFAULT 0,
    -- m107 (GH #291 Phase 2): additive app-health counters riding the SAME
    -- (site_id, day) row - NOT a second row, which would double total_checks
    -- and blend two different meanings into every uptime percentage in the
    -- product (this table has no "kind" dimension to disambiguate rows).
    -- Nullable with no default (mirrors this migration's other new columns);
    -- the upsert COALESCEs against NULL so a pre-migration/never-touched row
    -- adds correctly. app_total_checks counts every app probe ATTEMPT
    -- (including an inconclusive "unknown" verdict); app_up_checks counts
    -- only a conclusive true verdict - mirrors up_checks/total_checks
    -- exactly, just for the app-health signal.
    app_up_checks    integer,
    app_total_checks integer,
    updated_at      timestamptz      NOT NULL DEFAULT now(),
    PRIMARY KEY (site_id, day)
);

-- PRIMARY KEY (site_id, day) already covers "WHERE site_id = $1 AND day >=
-- $2" (the QueryFleetUptime access path); this index serves tenant-wide
-- reads (mirrors every other tenant-scoped table's defense-in-depth index).
CREATE INDEX site_uptime_daily_tenant_idx ON site_uptime_daily (tenant_id);

ALTER TABLE site_uptime_daily ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_uptime_daily FORCE ROW LEVEL SECURITY;
CREATE POLICY site_uptime_daily_tenant_isolation ON site_uptime_daily
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- The probe worker upserts cross-tenant under app.agent (one batch per
-- sweep); QueryFleetUptime also reads under app.agent with an explicit
-- tenant_id predicate — same convention as site_uptime_probes.
CREATE POLICY site_uptime_daily_agent ON site_uptime_daily
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');
CREATE POLICY site_uptime_daily_site_scope ON site_uptime_daily
    AS RESTRICTIVE FOR ALL
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(
                nullif(current_setting('app.allowed_site_ids', true), ''), ','
            )::uuid[]
        )
    )
    WITH CHECK (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(
                nullif(current_setting('app.allowed_site_ids', true), ''), ','
            )::uuid[]
        )
    );

-- ---------------------------------------------------------------------------
-- site_uptime_status  (M99 — per-site current-status stamp)
-- ---------------------------------------------------------------------------
-- One row per site, upserted every sweep alongside site_uptime_daily above.
-- Holds exactly the "latest probe" fields QueryFleetUptime needs (latest_up,
-- last_probed_at, tls_expiry) so the query never has to read
-- site_uptime_probes even for the "most recent probe" half of the old
-- LEFT JOIN LATERAL. Row absence means "never probed" (mirrors the old
-- code's "latest_up IS NULL => omit from map" contract).
--
-- last_probed_at is used as a freshness guard on UPDATE (WHERE
-- EXCLUDED.last_probed_at >= site_uptime_status.last_probed_at in the
-- upsert) so an overlapping/delayed sweep can never regress a fresher
-- status with stale data — see metrics.pgStore.UpsertRollup.
CREATE TABLE site_uptime_status (
    site_id        uuid        PRIMARY KEY REFERENCES sites (id) ON DELETE CASCADE,
    tenant_id      uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    latest_up      boolean     NOT NULL,
    last_probed_at timestamptz NOT NULL,
    tls_expiry     timestamptz,
    -- m107 (GH #291 Phase 2): the application-health snapshot, upserted
    -- alongside the reachability fields above but COALESCE/CASE-guarded
    -- against NULL independently of them (see metrics.pgStore.UpsertRollup).
    -- The app probe runs on a slower cadence than the reachability probe
    -- (default 300s vs 60s), so most sweeps that reach this upsert carry NO
    -- app-health opinion at all - a plain "latest_app_up = EXCLUDED.
    -- latest_app_up" would clobber a known value with NULL on ~4 of every 5
    -- sweeps. last_app_probed_at is separate from last_probed_at (the
    -- reachability timestamp this row's freshness guard is keyed on)
    -- because the two probes do not always coincide.
    latest_app_up      boolean,
    app_probe_reason   text,
    last_app_probed_at timestamptz,
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX site_uptime_status_tenant_idx ON site_uptime_status (tenant_id);

ALTER TABLE site_uptime_status ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_uptime_status FORCE ROW LEVEL SECURITY;
CREATE POLICY site_uptime_status_tenant_isolation ON site_uptime_status
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY site_uptime_status_agent ON site_uptime_status
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');
CREATE POLICY site_uptime_status_site_scope ON site_uptime_status
    AS RESTRICTIVE FOR ALL
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(
                nullif(current_setting('app.allowed_site_ids', true), ''), ','
            )::uuid[]
        )
    )
    WITH CHECK (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(
                nullif(current_setting('app.allowed_site_ids', true), ''), ','
            )::uuid[]
        )
    );

-- ---------------------------------------------------------------------------
-- site_incidents  (M94 — GH #148: persisted incident history)
-- ---------------------------------------------------------------------------
-- One row PER INCIDENT (open or closed), keyed by its own uuid id — unlike
-- site_alert_state above (which stores only the CURRENT transition memory),
-- this table lets the fleet incidents list and the per-incident detail
-- endpoint read real history instead of estimating ended_at/duration from a
-- single mutable row. Written ALONGSIDE site_alert_state inside the same
-- TransitionAlertState transaction (internal/uptime/repo.go); the state
-- machine's de-dupe logic (Evaluate in alerts.go) is unchanged.
--
-- ended_at IS NULL means the incident is open/ongoing. tenant_id is
-- denormalized (mirrors site_alert_state) for a join-free RLS policy and
-- join-free tenant-scoped queries.
CREATE TABLE site_incidents (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    site_id          uuid        NOT NULL REFERENCES sites   (id) ON DELETE CASCADE,
    started_at       timestamptz NOT NULL DEFAULT now(),
    ended_at         timestamptz,
    -- peak_status is reserved for a future degraded-vs-down incident severity
    -- distinction; the alert state machine only ever opens a 'down' incident
    -- today, so this is always 'down' for now.
    peak_status      text        NOT NULL DEFAULT 'down',
    last_http_status integer     NOT NULL DEFAULT 0,
    -- probe_count/down_count are reserved rollup counters, not yet populated.
    probe_count      integer     NOT NULL DEFAULT 0,
    down_count       integer     NOT NULL DEFAULT 0,
    -- opened_by distinguishes a real probe-detected incident ('probe') from
    -- the m94 day-1 backfill of already-open incidents ('seed').
    opened_by        text        NOT NULL DEFAULT 'probe',
    reason           text        NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX site_incidents_site_started_idx ON site_incidents (site_id, started_at DESC);
CREATE INDEX site_incidents_tenant_started_idx ON site_incidents (tenant_id, started_at DESC);

-- Race-safety guard: at most one OPEN incident per site (mirrors m75's
-- backup_snapshots_one_inflight_per_site). The state machine only ever
-- transitions one site's alert state under a row lock, so this is
-- belt-and-suspenders, not the primary defense.
CREATE UNIQUE INDEX site_incidents_one_open_per_site ON site_incidents (site_id)
    WHERE ended_at IS NULL;

ALTER TABLE site_incidents ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_incidents FORCE ROW LEVEL SECURITY;
CREATE POLICY site_incidents_tenant_isolation ON site_incidents
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- The probe worker opens/closes incidents cross-tenant inside the same
-- TransitionAlertState transaction that writes site_alert_state (app.agent).
CREATE POLICY site_incidents_agent ON site_incidents
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- m19 AS RESTRICTIVE collaborator site-scope policy (mirrors
-- site_alert_state_site_scope verbatim — site_id is a direct column here too).
CREATE POLICY site_incidents_site_scope ON site_incidents
    AS RESTRICTIVE FOR ALL
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(
                nullif(current_setting('app.allowed_site_ids', true), ''), ','
            )::uuid[]
        )
    )
    WITH CHECK (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(
                nullif(current_setting('app.allowed_site_ids', true), ''), ','
            )::uuid[]
        )
    );

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
    -- default_wp_user_login (m105 / GH #286): the per-site default WP login the
    -- mint path injects when the operator's request omits target_wp_user_login.
    -- Empty string preserves the pre-existing "agent picks the first admin"
    -- fallback.
    default_wp_user_login   text        NOT NULL DEFAULT '',
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
    -- m66: widened from ('org','site') to include 'client' portal invitations.
    scope            text        NOT NULL CHECK (scope IN ('org', 'site', 'client')),
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
    created_at       timestamptz NOT NULL DEFAULT now(),
    -- m66: portal invitation client binding. ON DELETE CASCADE removes pending
    -- invites when a client is deleted (composite FK mirrors m63 pattern).
    client_id        uuid        NULL
);

-- m66: composite FK so deleting a client cascades pending client invitations.
ALTER TABLE invitations
    ADD CONSTRAINT invitations_client_tenant_fkey
    FOREIGN KEY (client_id, tenant_id)
    REFERENCES clients (id, tenant_id)
    ON DELETE CASCADE;

CREATE INDEX invitations_tenant_id_idx ON invitations (tenant_id);
CREATE INDEX invitations_email_idx ON invitations (email);
CREATE INDEX invitations_site_id_idx ON invitations (site_id, created_at DESC) WHERE scope = 'site';
-- m66: pending client invitation listing per client.
CREATE INDEX invitations_client_id_idx ON invitations (client_id, created_at DESC) WHERE scope = 'client';

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

-- ---------------------------------------------------------------------------
-- smtp_settings  (m30 / ADR-045 — UI-configured instance SMTP)
-- ---------------------------------------------------------------------------
-- A SINGLE instance-level SMTP relay, configured by an owner in the UI instead
-- of env vars. Exactly one row exists: the `singleton` column is constant true
-- and UNIQUE, so an INSERT of a second row violates the constraint and an upsert
-- always targets the same row. The relay password is age(X25519)-encrypted at
-- rest in `password_enc` (the same secret-at-rest pattern as site destinations,
-- internal/cryptbox); the API never echoes it back. Instance-global, so NOT
-- tenant-scoped: the only RLS escape is app.agent='on', set by the settings
-- handler (which is already gated at the HTTP layer by PermSMTPManage) and by
-- the mailer's resolver at send time, both via Pool.InAgentTx.
CREATE TABLE smtp_settings (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    singleton          boolean     NOT NULL DEFAULT true,
    enabled            boolean     NOT NULL DEFAULT false,
    host               text        NOT NULL DEFAULT '',
    port               integer     NOT NULL DEFAULT 587,
    username           text        NOT NULL DEFAULT '',
    password_enc       bytea,
    from_address       text        NOT NULL DEFAULT '',
    from_name          text        NOT NULL DEFAULT '',
    tls_mode           text        NOT NULL DEFAULT 'starttls'
        CHECK (tls_mode IN ('starttls', 'tls', 'none')),
    allow_insecure_tls boolean     NOT NULL DEFAULT false,
    updated_by         uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX smtp_settings_singleton_key ON smtp_settings (singleton);

ALTER TABLE smtp_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE smtp_settings FORCE ROW LEVEL SECURITY;
-- Instance-global infra row: readable/writable only under app.agent='on' (set by
-- Pool.InAgentTx). HTTP-layer PermSMTPManage gating is the real access control.
CREATE POLICY smtp_settings_agent ON smtp_settings
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- email_log  (m30 / ADR-045 — transactional email audit + retry ledger)
-- ---------------------------------------------------------------------------
-- One row per outbound transactional email. tenant_id is NULL for instance/auth
-- mail (password reset, activation) that is sent before any tenant scope exists.
-- NEVER store the rendered body or any token here — only the subject + template
-- name. The send_email River worker flips status pending -> sent|failed.
CREATE TABLE email_log (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        REFERENCES tenants (id) ON DELETE CASCADE,
    to_addresses  text[]      NOT NULL,
    subject       text        NOT NULL,
    template      text        NOT NULL,
    status        text        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'sent', 'failed')),
    error         text,
    attempts      integer     NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now(),
    sent_at       timestamptz
);

CREATE INDEX email_log_tenant_created_idx ON email_log (tenant_id, created_at DESC);
CREATE INDEX email_log_status_failed_idx ON email_log (status) WHERE status = 'failed';

ALTER TABLE email_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE email_log FORCE ROW LEVEL SECURITY;
-- Tenant-scoped rows (tenant_id set) are isolated to their tenant for a future
-- per-tenant email-log UI.
CREATE POLICY email_log_tenant_isolation ON email_log
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- The send_email worker (and auth mail with tenant_id NULL) read/write under
-- app.agent='on' regardless of tenant.
CREATE POLICY email_log_agent ON email_log
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- password_changed_at  (m31 / ADR-045 Phase 2 — session invalidation)
-- ---------------------------------------------------------------------------
-- Set whenever a user's password changes (reset or change). The Authenticator
-- rejects any session whose login timestamp predates this, the only portable
-- way to invalidate a user's OTHER sessions given the SCS/Redis store cannot
-- enumerate per-user sessions. NULL = never changed (no invalidation).
ALTER TABLE users ADD COLUMN IF NOT EXISTS password_changed_at timestamptz;

-- ---------------------------------------------------------------------------
-- password_reset_tokens  (m31 / ADR-045 Phase 2)
-- ---------------------------------------------------------------------------
-- One row per issued reset link. token_hash is sha256(raw token) (the raw token
-- lives only in the emailed URL). Single-use + short TTL + atomically consumed.
-- Keyed on user_id (not tenant); the unauthenticated forgot/reset flow reads and
-- writes under app.agent='on' (Pool.InAgentTx), pre-tenant.
CREATE TABLE password_reset_tokens (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash   bytea       NOT NULL,
    expires_at   timestamptz NOT NULL,
    used_at      timestamptz,
    attempts     integer     NOT NULL DEFAULT 0,
    requested_ip inet,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX password_reset_tokens_token_hash_key ON password_reset_tokens (token_hash);
CREATE INDEX password_reset_tokens_user_active_idx ON password_reset_tokens (user_id) WHERE used_at IS NULL;

ALTER TABLE password_reset_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE password_reset_tokens FORCE ROW LEVEL SECURITY;
CREATE POLICY password_reset_tokens_agent ON password_reset_tokens
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- users: status + email_verified_at  (m32 / ADR-045 Phase 3 — open signup)
-- ---------------------------------------------------------------------------
-- status 'pending' = self-registered but not yet email-verified (cannot log in);
-- 'active' = usable; 'disabled' = soft-locked. email_verified_at is set when an
-- invited/self-serve user activates, or at trusted bootstrap/OIDC sign-in.
ALTER TABLE users ADD COLUMN IF NOT EXISTS status text NOT NULL DEFAULT 'active'
    CHECK (status IN ('active', 'pending', 'disabled'));
ALTER TABLE users ADD COLUMN IF NOT EXISTS email_verified_at timestamptz;

-- ---------------------------------------------------------------------------
-- email_verification_tokens  (m32 / ADR-045 Phase 3)
-- ---------------------------------------------------------------------------
-- Same hashed/TTL/single-use model as password_reset_tokens but purpose=verify.
-- Consumed under app.agent='on' (pre-tenant, unauthenticated activation).
CREATE TABLE email_verification_tokens (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash    bytea       NOT NULL,
    expires_at    timestamptz NOT NULL,
    used_at       timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- m98 — "sign up into a plan" Phase 0. A hosted-billing paid-tier hint
    -- captured at self-serve registration time (validated against
    -- internal/billing's plan ladder — never a free-form value; NULL means no
    -- intent). Carried here (not on users) so it is naturally scoped to THIS
    -- registration and disappears with the single-use token once consumed by
    -- VerifyEmail. A resend (ResendVerification) copies the prior value
    -- forward onto the new token instead of losing it.
    desired_plan  text
);

CREATE UNIQUE INDEX email_verification_tokens_token_hash_key ON email_verification_tokens (token_hash);
CREATE INDEX email_verification_tokens_user_active_idx ON email_verification_tokens (user_id) WHERE used_at IS NULL;

ALTER TABLE email_verification_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE email_verification_tokens FORCE ROW LEVEL SECURITY;
CREATE POLICY email_verification_tokens_agent ON email_verification_tokens
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- m33 — superadmin flag. Written only by the boot seeder; never by any API.
ALTER TABLE users ADD COLUMN IF NOT EXISTS is_superadmin boolean NOT NULL DEFAULT false;

-- ---------------------------------------------------------------------------
-- Performance Suite  (m36 / ADR-046)
-- ---------------------------------------------------------------------------
-- Agent-side page cache + asset optimization config, cache stats/purge audit,
-- and pure-Go RUCSS (Used CSS) results/jobs. Every table is tenant-scoped with
-- a tenant_isolation policy (operator/API path) + an app.agent policy
-- (cross-tenant worker/agent path). No _site_scope RESTRICTIVE policy:
-- collaborator gating is done in-app via authz.RequireSiteAccess(:siteId) on
-- the routes (m23 precedent). updated_at is set by repo code (no trigger).

-- site_perf_config — one row per site (PK = site_id). The full performance
-- configuration the agent reads on the request fast-path; the CP is the source
-- of truth, the agent mirrors it via a sync_perf_config command.
CREATE TABLE site_perf_config (
    site_id                       uuid        PRIMARY KEY REFERENCES sites (id) ON DELETE CASCADE,
    tenant_id                     uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    -- Caching
    cache_enabled                 boolean     NOT NULL DEFAULT false,
    cache_logged_in               boolean     NOT NULL DEFAULT false,
    cache_mobile                  boolean     NOT NULL DEFAULT false,
    cache_refresh                 boolean     NOT NULL DEFAULT false,
    cache_refresh_interval        text        NOT NULL DEFAULT '2hours',
    cache_link_prefetch           boolean     NOT NULL DEFAULT true,
    cache_bypass_urls             text[]      NOT NULL DEFAULT '{}',
    cache_bypass_cookies          text[]      NOT NULL DEFAULT '{}',
    cache_include_queries         text[]      NOT NULL DEFAULT '{}',
    cache_include_cookies         text[]      NOT NULL DEFAULT '{}',
    -- CSS / JS
    css_js_minify                 boolean     NOT NULL DEFAULT true,
    css_rucss                     boolean     NOT NULL DEFAULT false,
    css_rucss_include_selectors   text[]      NOT NULL DEFAULT '{}',
    css_js_self_host_third_party  boolean     NOT NULL DEFAULT false,
    js_delay                      boolean     NOT NULL DEFAULT false,
    js_delay_method               text        NOT NULL DEFAULT 'defer',
    js_delay_excludes             text[]      NOT NULL DEFAULT '{}',
    js_delay_third_party          boolean     NOT NULL DEFAULT false,
    js_delay_third_party_excludes text[]      NOT NULL DEFAULT '{}',
    -- Fonts
    fonts_display_swap            boolean     NOT NULL DEFAULT true,
    fonts_optimize_google         boolean     NOT NULL DEFAULT false,
    fonts_preload                 boolean     NOT NULL DEFAULT false,
    -- Media / lazy-load
    lazy_load                     boolean     NOT NULL DEFAULT true,
    lazy_load_exclusions          text[]      NOT NULL DEFAULT '{}',
    properly_size_images          boolean     NOT NULL DEFAULT true,
    youtube_placeholder           boolean     NOT NULL DEFAULT false,
    self_host_gravatars           boolean     NOT NULL DEFAULT false,
    -- CDN
    cdn_enabled                   boolean     NOT NULL DEFAULT false,
    cdn_url                       text,
    cdn_file_types                text        NOT NULL DEFAULT 'all',
    cdn_provider                  text,
    cdn_credentials_encrypted     bytea,
    -- Database cleanup
    db_auto_clean                 boolean     NOT NULL DEFAULT false,
    db_auto_clean_interval        text        NOT NULL DEFAULT 'weekly',
    db_post_revisions             boolean     NOT NULL DEFAULT false,
    db_post_auto_drafts           boolean     NOT NULL DEFAULT false,
    db_post_trashed               boolean     NOT NULL DEFAULT false,
    db_comments_spam              boolean     NOT NULL DEFAULT false,
    db_comments_trashed           boolean     NOT NULL DEFAULT false,
    db_transients_expired         boolean     NOT NULL DEFAULT false,
    db_optimize_tables            boolean     NOT NULL DEFAULT false,
    -- DB-clean scheduling (M38): CP-owned; NULL = no pending auto-clean.
    next_db_clean_at              timestamptz,
    -- Bloat removal
    bloat_disable_block_css       boolean     NOT NULL DEFAULT false,
    bloat_disable_dashicons       boolean     NOT NULL DEFAULT false,
    bloat_disable_emojis          boolean     NOT NULL DEFAULT false,
    bloat_disable_jquery_migrate  boolean     NOT NULL DEFAULT false,
    bloat_disable_xml_rpc         boolean     NOT NULL DEFAULT false,
    bloat_disable_rss_feed        boolean     NOT NULL DEFAULT false,
    bloat_disable_oembeds         boolean     NOT NULL DEFAULT false,
    bloat_heartbeat_control       boolean     NOT NULL DEFAULT false,
    bloat_post_revisions_control  boolean     NOT NULL DEFAULT false,
    -- Preload (cache-warm) throttle (M37) — operator-tunable queue drain knobs.
    preload_concurrency           integer     NOT NULL DEFAULT 1,
    preload_delay_ms              integer     NOT NULL DEFAULT 500,
    preload_batch_size            integer     NOT NULL DEFAULT 50,
    preload_max_load              real        NOT NULL DEFAULT 0,
    -- Server / install state (agent-reported)
    server_software               text,
    dropin_installed              boolean     NOT NULL DEFAULT false,
    wp_cache_constant_set         boolean     NOT NULL DEFAULT false,
    htaccess_managed              boolean     NOT NULL DEFAULT false,
    config_version                integer     NOT NULL DEFAULT 1,
    -- Watchdog columns (M39): track in-flight db_clean/db_scan jobs so the
    -- periodic DBCleanWatchdogWorker can detect stalled jobs and emit
    -- db.clean.failed / db.scan.failed SSE to un-stick the UI.
    active_db_clean_job_id        text,
    active_db_clean_started       timestamptz,
    active_db_scan_job_id         text,
    active_db_scan_started        timestamptz,
    active_orphan_delete_job_id   text,
    active_orphan_delete_started  timestamptz,
    -- M54 Phase 1 — WOFF2 transcoding for self-hosted fonts.
    fonts_transcode_woff2         boolean     NOT NULL DEFAULT false,
    -- M55 Phase 2 — font subsetting (experimental, default OFF).
    -- fonts_subset enables the subset-WOFF2 path in the media-encoder worker.
    -- fonts_subset_mode: "range" (fixed unicode-range) or "used" (aggressive opt-in).
    -- fonts_subset_range: the named range to subset to; "latin-ext" is the safe default.
    fonts_subset                  boolean     NOT NULL DEFAULT false,
    fonts_subset_mode             text        NOT NULL DEFAULT 'range',
    fonts_subset_range            text        NOT NULL DEFAULT 'latin-ext',
    -- M53 / #169 — WooCommerce cacheable-session.
    woo_cacheable_session         boolean     NOT NULL DEFAULT false,
    -- M67 — tri-state: NULL = never probed, false = probed unsupported, true = probed supported.
    -- NOT NULL and DEFAULT false were dropped (m67); existing false rows reset to NULL.
    woo_theme_fragments_supported boolean,
    -- M67 — timestamp of the last agent probe; NULL when never probed.
    woo_fragments_probed_at       timestamptz,
    -- M56 — Real User Monitoring (RUM). All off by default (opt-in per site).
    -- rum_enabled: per-site toggle; must be true for the beacon to be injected.
    -- rum_sample_rate: fraction of beacons the CP keeps after server-side sampling.
    -- max_distinct_countries: country-dimension cap; others fold to '__other__'.
    -- min_sample_count: display floor below which no p75 is shown (CrUX-style).
    -- beacon_key_hash: sha256(plaintext_beacon_key); used for anonymous ingest
    --   resolution. The plaintext is NEVER stored here — it is only returned
    --   transiently to the agent on perf-config push/ack.
    -- beacon_key_hash_prev: grace-window previous hash (nullable; cleared after
    --   the grace window expires so old cached pages still resolve during rotation).
    rum_enabled               boolean  NOT NULL DEFAULT false,
    rum_sample_rate           real     NOT NULL DEFAULT 1.0,
    max_distinct_countries    integer  NOT NULL DEFAULT 8,
    min_sample_count          integer  NOT NULL DEFAULT 30,
    beacon_key_hash           bytea,
    beacon_key_hash_prev      bytea,
    -- GH #174 — ack-based beacon-key re-mint. beacon_key_acked_present records
    -- whether the agent's most recent config-ack (POST
    -- /agent/v1/perf/config-ack, field rum_beacon_present) reported that it
    -- currently holds a non-empty rum_beacon_key. Combined with
    -- beacon_key_hash IS NOT NULL, false here identifies the exact "hash
    -- committed but the agent never got/kept the plaintext" stuck state (the
    -- one best-effort mint+push on first RUM-enable was lost — agent
    -- down/unreachable — and nothing before this column ever re-minted). Agent
    -- write path only (never operator-settable). Default false so a config row
    -- that predates this migration is treated as "unknown/not-yet-acked" and
    -- is eligible for the self-heal re-mint on the next operator save or ack.
    beacon_key_acked_present  boolean  NOT NULL DEFAULT false,
    beacon_key_acked_at       timestamptz,
    created_at                    timestamptz NOT NULL DEFAULT now(),
    updated_at                    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX site_perf_config_tenant_idx ON site_perf_config (tenant_id);
-- M56: unique index for beacon-key → site/tenant resolution in InRumIngestLookupTx.
-- A point lookup on sha256(presented_key) resolves to exactly one site.
CREATE UNIQUE INDEX site_perf_config_beacon_key_hash_uniq
    ON site_perf_config (beacon_key_hash)
    WHERE beacon_key_hash IS NOT NULL;

ALTER TABLE site_perf_config ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_perf_config FORCE ROW LEVEL SECURITY;
CREATE POLICY site_perf_config_tenant_isolation ON site_perf_config
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY site_perf_config_agent ON site_perf_config
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');
-- M56: SELECT-only RUM beacon-key lookup policy. Enables InRumIngestLookupTx to
-- read (site_id, tenant_id, rum_enabled, rum_sample_rate) by beacon_key_hash
-- BEFORE any tenant GUC is set. Mirrors api_keys_prefix_lookup exactly.
CREATE POLICY site_perf_config_rum_lookup ON site_perf_config
    FOR SELECT
    USING (current_setting('app.rum_lookup', true) = 'on');

-- site_db_scan_results — latest db_scan output per site (M39 + M41).
-- One row per site, upserted on every scan. Holds the full per-category
-- count/bytes preview so the operator can confirm before running a clean.
-- M41 (Phase 3.3) adds three JSONB columns for orphan-scan output:
--   orphaned_options_json  — wp_options rows attributable to no installed plugin.
--   orphaned_cron_json     — WP-Cron events attributable to no installed plugin/core.
--   installed_plugins_json — full installed-plugin snapshot at scan time (P3.8 gate).
CREATE TABLE IF NOT EXISTS site_db_scan_results (
    site_id                uuid        NOT NULL,
    tenant_id              uuid        NOT NULL,
    job_id                 text        NOT NULL,
    categories_json        jsonb       NOT NULL DEFAULT '{}',
    tables_json            jsonb       NOT NULL DEFAULT '[]',
    db_size_bytes          bigint      NOT NULL DEFAULT 0,
    table_count            int         NOT NULL DEFAULT 0,
    scanned_at             timestamptz NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now(),
    -- M41 Phase 3.3: orphan-enumeration columns (DEFAULT '[]' so rows from
    -- agents < 0.16.0 return an empty array rather than NULL).
    orphaned_options_json  jsonb       NOT NULL DEFAULT '[]',
    orphaned_cron_json     jsonb       NOT NULL DEFAULT '[]',
    installed_plugins_json jsonb       NOT NULL DEFAULT '[]',
    CONSTRAINT site_db_scan_results_pkey PRIMARY KEY (site_id)
);
CREATE INDEX IF NOT EXISTS site_db_scan_results_tenant_idx
    ON site_db_scan_results (tenant_id);
ALTER TABLE site_db_scan_results ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_db_scan_results FORCE ROW LEVEL SECURITY;
CREATE POLICY site_db_scan_results_tenant_isolation ON site_db_scan_results
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY site_db_scan_results_agent ON site_db_scan_results
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- site_db_clean_results — latest db_clean output per site (M71).
-- One row per site, upserted on every completed clean run. Holds the structured
-- per-category result from the final done=true progress push so GET /perf/db/clean
-- can serve the last-run summary without relying on SSE delivery.
CREATE TABLE IF NOT EXISTS site_db_clean_results (
    site_id      uuid        NOT NULL,
    tenant_id    uuid        NOT NULL,
    job_id       text        NOT NULL,
    result_json  jsonb       NOT NULL DEFAULT '{}',
    rows_deleted bigint      NOT NULL DEFAULT 0,
    bytes_freed  bigint      NOT NULL DEFAULT 0,
    cleaned_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT site_db_clean_results_pkey PRIMARY KEY (site_id)
);
CREATE INDEX IF NOT EXISTS site_db_clean_results_tenant_idx
    ON site_db_clean_results (tenant_id);
ALTER TABLE site_db_clean_results ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_db_clean_results FORCE ROW LEVEL SECURITY;
CREATE POLICY site_db_clean_results_tenant_isolation ON site_db_clean_results
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY site_db_clean_results_agent ON site_db_clean_results
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- site_cache_hit_ratio_history — M52 / #162: append-only hit-ratio trend.
-- One row per cache-stats report cycle when the agent supplies a non-zero delta.
-- Mirrors site_db_size_history RLS EXACTLY.
CREATE TABLE site_cache_hit_ratio_history (
    id         uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    site_id    uuid          NOT NULL REFERENCES sites   (id) ON DELETE CASCADE,
    tenant_id  uuid          NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    hit_count  bigint        NOT NULL DEFAULT 0,
    miss_count bigint        NOT NULL DEFAULT 0,
    ratio_pct  numeric(5,2),
    sampled_at timestamptz   NOT NULL,
    created_at timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT site_cache_hit_ratio_history_site_sampled_uniq UNIQUE (site_id, sampled_at)
);

CREATE INDEX site_cache_hit_ratio_history_site_sampled_idx
    ON site_cache_hit_ratio_history (site_id, sampled_at DESC);
CREATE INDEX site_cache_hit_ratio_history_created_idx
    ON site_cache_hit_ratio_history (created_at);

ALTER TABLE site_cache_hit_ratio_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_cache_hit_ratio_history FORCE ROW LEVEL SECURITY;
CREATE POLICY site_cache_hit_ratio_history_tenant_isolation ON site_cache_hit_ratio_history
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- GC path only deletes; inserts flow through tenant_isolation via InTenantTx.
CREATE POLICY site_cache_hit_ratio_history_agent ON site_cache_hit_ratio_history
    USING (current_setting('app.agent', true) = 'on');

-- site_cache_stats — latest cache gauges the agent reports (overwritten in place).
CREATE TABLE site_cache_stats (
    site_id            uuid        PRIMARY KEY REFERENCES sites (id) ON DELETE CASCADE,
    tenant_id          uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    cached_pages_count integer     NOT NULL DEFAULT 0,
    cache_size_bytes   bigint      NOT NULL DEFAULT 0,
    last_purged_at     timestamptz,
    last_purge_kind    text,
    last_preload_at    timestamptz,
    preload_pending    integer     NOT NULL DEFAULT 0,
    preload_total      integer     NOT NULL DEFAULT 0,
    reported_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX site_cache_stats_tenant_idx ON site_cache_stats (tenant_id);

ALTER TABLE site_cache_stats ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_cache_stats FORCE ROW LEVEL SECURITY;
CREATE POLICY site_cache_stats_tenant_isolation ON site_cache_stats
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY site_cache_stats_agent ON site_cache_stats
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- cache_purge_audit — append-style log of every purge (operator or system).
CREATE TABLE cache_purge_audit (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    site_id           uuid        NOT NULL REFERENCES sites (id) ON DELETE CASCADE,
    kind              text        NOT NULL,
    initiator_user_id uuid        REFERENCES users (id) ON DELETE SET NULL,
    target_urls       text[]      NOT NULL DEFAULT '{}',
    urls_count        integer     NOT NULL DEFAULT 0,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_cache_purge_site ON cache_purge_audit (site_id, created_at DESC);
CREATE INDEX cache_purge_audit_tenant_idx ON cache_purge_audit (tenant_id);

ALTER TABLE cache_purge_audit ENABLE ROW LEVEL SECURITY;
ALTER TABLE cache_purge_audit FORCE ROW LEVEL SECURITY;
CREATE POLICY cache_purge_audit_tenant_isolation ON cache_purge_audit
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY cache_purge_audit_agent ON cache_purge_audit
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- rucss_results — per (site, structure_hash) Used-CSS result; the CSS itself
-- lives in object storage (used_css_s3_key). UNIQUE(site_id, structure_hash).
CREATE TABLE rucss_results (
    id                 uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          uuid          NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    site_id            uuid          NOT NULL REFERENCES sites (id) ON DELETE CASCADE,
    structure_hash     text          NOT NULL,
    url                text,
    original_css_bytes integer,
    used_css_bytes     integer,
    reduction_pct      numeric(5,2),
    used_css_s3_key    text          NOT NULL,
    selectors_total    integer,
    selectors_kept     integer,
    selectors_dropped  integer,
    compute_ms         integer,
    created_at         timestamptz   NOT NULL DEFAULT now(),
    last_used_at       timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT rucss_results_site_hash_uniq UNIQUE (site_id, structure_hash)
);

CREATE INDEX idx_rucss_results_site ON rucss_results (site_id, last_used_at DESC);
CREATE INDEX rucss_results_tenant_idx ON rucss_results (tenant_id);

ALTER TABLE rucss_results ENABLE ROW LEVEL SECURITY;
ALTER TABLE rucss_results FORCE ROW LEVEL SECURITY;
CREATE POLICY rucss_results_tenant_isolation ON rucss_results
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY rucss_results_agent ON rucss_results
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- rucss_jobs — one RUCSS compute job (id is a ULID, text). Tracks the lifecycle.
CREATE TABLE rucss_jobs (
    id             text        PRIMARY KEY,
    tenant_id      uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    site_id        uuid        NOT NULL REFERENCES sites (id) ON DELETE CASCADE,
    structure_hash text,
    url            text,
    state          text        NOT NULL DEFAULT 'queued',
    error_reason   text,
    result_id      uuid        REFERENCES rucss_results (id) ON DELETE SET NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    completed_at   timestamptz
);

CREATE INDEX idx_rucss_jobs_site_state ON rucss_jobs (site_id, state);
CREATE INDEX rucss_jobs_tenant_idx ON rucss_jobs (tenant_id);

ALTER TABLE rucss_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE rucss_jobs FORCE ROW LEVEL SECURITY;
CREATE POLICY rucss_jobs_tenant_isolation ON rucss_jobs
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY rucss_jobs_agent ON rucss_jobs
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- plugin_signatures — M40 corpus table (global, no tenant_id).
-- ---------------------------------------------------------------------------
-- Global read-only reference data used by the DB-Cleaner corpus classifier.
-- One row per wordpress.org plugin slug; stores known option/transient/table/
-- cron-hook name patterns. ENABLE RLS (not FORCE) so the migration owner can
-- INSERT the seed; wpmgr_app has SELECT only (see m40 migration REVOKE).
CREATE TABLE plugin_signatures (
    slug               text        NOT NULL,
    corpus_version     integer     NOT NULL DEFAULT 1,
    option_patterns    jsonb       NOT NULL DEFAULT '[]',
    transient_patterns jsonb       NOT NULL DEFAULT '[]',
    table_patterns     jsonb       NOT NULL DEFAULT '[]',
    cron_hook_patterns jsonb       NOT NULL DEFAULT '[]',
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT plugin_signatures_pkey PRIMARY KEY (slug)
);

CREATE INDEX plugin_signatures_corpus_version_idx ON plugin_signatures (corpus_version);

ALTER TABLE plugin_signatures ENABLE ROW LEVEL SECURITY;

CREATE POLICY plugin_signatures_read ON plugin_signatures
    FOR SELECT USING (true);

-- The wpmgr_app role has INSERT/UPDATE/DELETE revoked on plugin_signatures.
-- The ALTER DEFAULT PRIVILEGES in m1 grants wpmgr_app DML on all new tables;
-- m40 explicitly undoes that for this table because corpus writes must only
-- happen via the owner/superuser DSN at migration time. The ENABLE (not FORCE)
-- RLS posture means the owner bypasses RLS at seed time; wpmgr_app's write
-- attempts fail at the privilege level before RLS is evaluated.
-- This REVOKE is the PRIMARY write guard; RLS SELECT policy is the second layer.
REVOKE INSERT, UPDATE, DELETE ON plugin_signatures FROM wpmgr_app;

-- ---------------------------------------------------------------------------
-- site_db_size_history — M42 Phase 3.4: DB-size trend (append-only).
-- ---------------------------------------------------------------------------
-- One row per successful db_scan execution. The CP writes it from the same
-- InTenantTx as UpsertDBScanResult (atomic with the scan row). The agent
-- NEVER writes this table directly.
--
-- RLS mirrors site_cache_stats EXACTLY (m36 precedent).
-- Defense-in-depth note: the agent policy is intentionally cross-tenant so the
-- River GC worker can sweep the whole table in a single pass without enumerating
-- tenant IDs (same pattern as backup_retention_gc, php_errors retention GC,
-- site_events prune). The GC worker only deletes — never constructs
-- user-visible output from rows it touches.
CREATE TABLE site_db_size_history (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    site_id        uuid        NOT NULL
                   REFERENCES sites (id) ON DELETE CASCADE,
    tenant_id      uuid        NOT NULL
                   REFERENCES tenants (id) ON DELETE CASCADE,
    db_size_bytes  bigint      NOT NULL DEFAULT 0,
    table_count    int         NOT NULL DEFAULT 0,
    scanned_at     timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT site_db_size_history_site_scanned_uniq
        UNIQUE (site_id, scanned_at)
);

-- Serves the GET /perf/db/health ORDER BY + LIMIT query efficiently.
CREATE INDEX site_db_size_history_site_scanned_idx
    ON site_db_size_history (site_id, scanned_at DESC);

-- Serves the GC prune worker's WHERE created_at < cutoff scan.
CREATE INDEX site_db_size_history_created_idx
    ON site_db_size_history (created_at);

ALTER TABLE site_db_size_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_db_size_history FORCE ROW LEVEL SECURITY;

CREATE POLICY site_db_size_history_tenant_isolation ON site_db_size_history
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- No WITH CHECK: the GC path only deletes; inserts flow through the
-- tenant_isolation policy via InTenantTx.
CREATE POLICY site_db_size_history_agent ON site_db_size_history
    USING (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- font_transcode_results  (m54 — job-control layer for WOFF2 transcoding)
-- ---------------------------------------------------------------------------
-- One row per content-addressed source font hash per tenant. Stores the River
-- job ID for in-flight jobs and the produced woff2 asset key on completion.
-- PK = (source_hash, tenant_id).
CREATE TABLE IF NOT EXISTS font_transcode_results (
    source_hash    text        NOT NULL,
    tenant_id      uuid        NOT NULL,
    site_id        uuid        NOT NULL,
    river_job_id   bigint,
    woff2_key      text,
    negative       boolean     NOT NULL DEFAULT false,
    error_detail   text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (source_hash, tenant_id),
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    FOREIGN KEY (site_id)   REFERENCES sites   (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS font_transcode_results_site_id_idx
    ON font_transcode_results (tenant_id, site_id);

ALTER TABLE font_transcode_results ENABLE ROW LEVEL SECURITY;
ALTER TABLE font_transcode_results FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON font_transcode_results
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY agent_access ON font_transcode_results
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- font_results  (m55 — per-site dashboard catalog for font processing)
-- ---------------------------------------------------------------------------
-- One row per (site_id, source_hash). The dashboard read-model: tracks family,
-- sizes, subset state, and CP-derived savings_pct. Distinct from
-- font_transcode_results (job control). State: pending|ready|subset|negative.
-- savings_pct is CP-derived at upsert: 1 - min(woff2_size, subset_size) / original_size.
CREATE TABLE IF NOT EXISTS font_results (
    id             uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id      uuid        NOT NULL,
    site_id        uuid        NOT NULL,
    source_hash    text        NOT NULL,
    family         text,
    source_file    text,
    original_ext   text,
    original_size  integer,
    woff2_size     integer,
    subset_size    integer,
    unicode_range  text,
    state          text        NOT NULL DEFAULT 'pending',
    error_detail   text,
    savings_pct    numeric(5,2),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT font_results_pkey        PRIMARY KEY (id),
    CONSTRAINT font_results_site_hash_uniq UNIQUE (site_id, source_hash),
    CONSTRAINT font_results_state_check CHECK (state IN ('pending','ready','subset','negative')),
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    FOREIGN KEY (site_id)   REFERENCES sites   (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_font_results_site
    ON font_results (site_id, updated_at DESC);

CREATE INDEX IF NOT EXISTS font_results_tenant_idx
    ON font_results (tenant_id);

ALTER TABLE font_results ENABLE ROW LEVEL SECURITY;
ALTER TABLE font_results FORCE ROW LEVEL SECURITY;

CREATE POLICY font_results_tenant_isolation ON font_results
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY font_results_agent_access ON font_results
    USING (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- RUM tables  (m56 — Real User Monitoring)
-- ---------------------------------------------------------------------------
-- Three-table design: raw events (short-retention drill-down buffer),
-- hourly rollups, and daily rollups. All use the RUM-specific RLS policy set:
--   tenant_isolation (app.tenant_id) — dashboard read path. KEEP.
--   rum_ingest (app.rum_ingest)      — INSERT-only, WITH CHECK only, no USING.
--                                      The anonymous browser write path. ADD.
--   agent_access                     — OMIT. The agent never touches RUM.
-- This is NOT the m55 template verbatim; it deliberately excludes agent_access.

-- rum_add_int_arrays: element-wise sum of two same-length integer arrays.
-- Used by the ON CONFLICT DO UPDATE clauses in UpsertRumRollup*.
CREATE OR REPLACE FUNCTION rum_add_int_arrays(a integer[], b integer[])
RETURNS integer[]
LANGUAGE sql
IMMUTABLE STRICT PARALLEL SAFE
AS $$
    SELECT array_agg(ai + bi ORDER BY idx)
    FROM unnest(a, b) WITH ORDINALITY AS t(ai, bi, idx)
$$;

-- rum_events_raw: 48h (SaaS) / 24h (self-host) rolling drill-down buffer.
-- RANGE partitioning by day on received_at keeps partition drops cheap (no DELETE).
-- In Phase 1 we create a non-partitioned table; partitioning is a later migration
-- once the ingest volume justifies it and Atlas can diff it safely. The BRIN and
-- composite index are what matter for correctness.
CREATE TABLE IF NOT EXISTS rum_events_raw (
    id           uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL,
    site_id      uuid        NOT NULL,
    url_pattern  text        NOT NULL DEFAULT '',
    metric       text        NOT NULL CHECK (metric IN ('lcp','inp','cls','ttfb','fcp')),
    value_milli  integer     NOT NULL DEFAULT 0,
    device       text        NOT NULL DEFAULT 'desktop',
    country      text        NOT NULL DEFAULT '__other__',
    conn         text        NOT NULL DEFAULT 'unknown',
    received_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id),
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    FOREIGN KEY (site_id)   REFERENCES sites   (id) ON DELETE CASCADE
);

-- BRIN for time-range scans on the rolling buffer (efficient when writes are
-- time-ordered, which they are for an ingest-only table).
CREATE INDEX IF NOT EXISTS rum_events_raw_received_at_brin
    ON rum_events_raw USING BRIN (received_at);

CREATE INDEX IF NOT EXISTS rum_events_raw_site_received_idx
    ON rum_events_raw (site_id, received_at);

CREATE INDEX IF NOT EXISTS rum_events_raw_tenant_idx
    ON rum_events_raw (tenant_id);

ALTER TABLE rum_events_raw ENABLE ROW LEVEL SECURITY;
ALTER TABLE rum_events_raw FORCE ROW LEVEL SECURITY;

CREATE POLICY rum_events_raw_tenant_isolation ON rum_events_raw
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- INSERT-only: the anonymous browser beacon writes under app.rum_ingest.
-- WITH CHECK only (no USING) so this policy cannot be used to read rows.
-- M3: tenant_id and site_id are pinned to the GUC-resolved values so a Go-layer
-- bug that passes wrong IDs is caught by the DB rather than writing cross-tenant.
CREATE POLICY rum_events_raw_rum_ingest ON rum_events_raw
    FOR INSERT
    WITH CHECK (
        current_setting('app.rum_ingest', true) = 'on'
        AND tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
        AND site_id   = nullif(current_setting('app.site_id',   true), '')::uuid
    );

-- rum_rollup_hourly: PK (site_id, url_pattern, metric, device, country, bucket_hour).
-- bucket_counts is the CrUX-anchored histogram as a fixed-width integer array.
-- sample_rate is persisted so read-time code can scale counts back to true volume.
CREATE TABLE IF NOT EXISTS rum_rollup_hourly (
    tenant_id    uuid        NOT NULL,
    site_id      uuid        NOT NULL,
    url_pattern  text        NOT NULL,
    metric       text        NOT NULL,
    device       text        NOT NULL,
    country      text        NOT NULL,
    bucket_hour  timestamptz NOT NULL,
    sample_count bigint      NOT NULL DEFAULT 0,
    sample_rate  real        NOT NULL DEFAULT 1.0,
    bucket_counts integer[]  NOT NULL DEFAULT '{}',
    sum_value    bigint      NOT NULL DEFAULT 0,
    min_value    integer     NOT NULL DEFAULT 0,
    max_value    integer     NOT NULL DEFAULT 0,
    PRIMARY KEY (site_id, url_pattern, metric, device, country, bucket_hour),
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    FOREIGN KEY (site_id)   REFERENCES sites   (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS rum_rollup_hourly_tenant_idx
    ON rum_rollup_hourly (tenant_id);

ALTER TABLE rum_rollup_hourly ENABLE ROW LEVEL SECURITY;
ALTER TABLE rum_rollup_hourly FORCE ROW LEVEL SECURITY;

CREATE POLICY rum_rollup_hourly_tenant_isolation ON rum_rollup_hourly
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY rum_rollup_hourly_rum_ingest ON rum_rollup_hourly
    FOR INSERT
    WITH CHECK (
        current_setting('app.rum_ingest', true) = 'on'
        AND tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
        AND site_id   = nullif(current_setting('app.site_id',   true), '')::uuid
    );

-- rum_rollup_daily: identical to hourly but bucket_day is a date.
CREATE TABLE IF NOT EXISTS rum_rollup_daily (
    tenant_id    uuid        NOT NULL,
    site_id      uuid        NOT NULL,
    url_pattern  text        NOT NULL,
    metric       text        NOT NULL,
    device       text        NOT NULL,
    country      text        NOT NULL,
    bucket_day   date        NOT NULL,
    sample_count bigint      NOT NULL DEFAULT 0,
    sample_rate  real        NOT NULL DEFAULT 1.0,
    bucket_counts integer[]  NOT NULL DEFAULT '{}',
    sum_value    bigint      NOT NULL DEFAULT 0,
    min_value    integer     NOT NULL DEFAULT 0,
    max_value    integer     NOT NULL DEFAULT 0,
    PRIMARY KEY (site_id, url_pattern, metric, device, country, bucket_day),
    FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    FOREIGN KEY (site_id)   REFERENCES sites   (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS rum_rollup_daily_tenant_idx
    ON rum_rollup_daily (tenant_id);

ALTER TABLE rum_rollup_daily ENABLE ROW LEVEL SECURITY;
ALTER TABLE rum_rollup_daily FORCE ROW LEVEL SECURITY;

CREATE POLICY rum_rollup_daily_tenant_isolation ON rum_rollup_daily
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY rum_rollup_daily_rum_ingest ON rum_rollup_daily
    FOR INSERT
    WITH CHECK (
        current_setting('app.rum_ingest', true) = 'on'
        AND tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
        AND site_id   = nullif(current_setting('app.site_id',   true), '')::uuid
    );

-- ---------------------------------------------------------------------------
-- Per-site Email / SMTP Management  (m59)
-- ---------------------------------------------------------------------------
-- Three tables for the per-site outgoing email feature.
-- RLS mirrors m36 exactly: ENABLE + FORCE + tenant_isolation + agent policies.
-- updated_at is set by repo code (no trigger — project convention).

-- site_email_config — one row per site (or per tenant with site_id NULL for the
-- org-wide default). Surrogate PK + partial unique indexes enforce the constraint
-- that each tenant has at most one org-wide default and at most one per-site row.
CREATE TABLE IF NOT EXISTS site_email_config (
    id                        uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id                 uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    site_id                   uuid        REFERENCES sites (id) ON DELETE CASCADE,
    provider                  text        NOT NULL DEFAULT 'smtp',
    from_address              text        NOT NULL DEFAULT '',
    from_name                 text        NOT NULL DEFAULT '',
    force_from_email          boolean     NOT NULL DEFAULT false,
    force_from_name           boolean     NOT NULL DEFAULT false,
    return_path               boolean     NOT NULL DEFAULT false,
    config                    jsonb       NOT NULL DEFAULT '{}'::jsonb,
    provider_secret_encrypted bytea,
    oauth_refresh_encrypted   bytea,
    oauth_access_encrypted    bytea,
    oauth_expires_at          timestamptz,
    mappings                  jsonb       NOT NULL DEFAULT '{}'::jsonb,
    default_connection        text,
    fallback_connection       text,
    log_emails                boolean     NOT NULL DEFAULT true,
    store_body                boolean     NOT NULL DEFAULT false,
    retention_days            integer     NOT NULL DEFAULT 14,
    -- m61: per-row webhook security fields.
    -- webhook_route_token_hash is SHA-256(random 32-byte token); the token is
    -- embedded in the webhook URL and never stored at rest.
    webhook_route_token_hash  bytea,
    -- webhook_signing_key_enc is the age-encrypted per-provider signing key
    -- (SendGrid ECDSA pubkey / Mailgun HMAC key / Postmark secret).
    webhook_signing_key_enc   bytea,
    -- ses_topic_arns is the allowlist of SNS TopicArns for SES users.
    ses_topic_arns            text[],
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT site_email_config_pkey PRIMARY KEY (id)
);

CREATE UNIQUE INDEX IF NOT EXISTS site_email_config_per_site_idx
    ON site_email_config (tenant_id, site_id)
    WHERE site_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS site_email_config_org_default_idx
    ON site_email_config (tenant_id)
    WHERE site_id IS NULL;

CREATE INDEX IF NOT EXISTS site_email_config_tenant_idx
    ON site_email_config (tenant_id);

-- m61: unique index for constant-time route-token lookup.
CREATE UNIQUE INDEX IF NOT EXISTS site_email_config_route_token_hash_idx
    ON site_email_config (webhook_route_token_hash)
    WHERE webhook_route_token_hash IS NOT NULL;

ALTER TABLE site_email_config ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_email_config FORCE ROW LEVEL SECURITY;

CREATE POLICY site_email_config_tenant_isolation ON site_email_config
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY site_email_config_agent ON site_email_config
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- m112 (GH #380): the app.site_scope RESTRICTIVE policies, SPLIT into a read
-- policy and three write policies. The split is the whole point and is not a
-- stylistic choice.
--
-- The org-wide row (site_id IS NULL) is inherited by every site that has no
-- config of its own, and that inherited row is what the site actually sends
-- mail with. A site-scoped collaborator must therefore be able to READ it
-- (GET /sites/:siteId/email/config and the connections list both surface it
-- legitimately) and must NOT be able to WRITE it. site_destinations_site_scope
-- is a single FOR ALL policy with one predicate, which can only do one or the
-- other: permit the org row and the collaborator can repoint the organisation's
-- mail server, or refuse it and inheritance breaks.
--
-- RESTRICTIVE policies AND together, so a FOR ALL write policy would also
-- narrow SELECT. Four operation-specific policies are the only shape that
-- separates the two. See migrations/20260814000000_m112_email_site_scope_rls.sql
-- for the door history that produced this.
--
-- All four are no-ops whenever app.site_scope is not 'on', which is every
-- org-member, worker and agent transaction.
CREATE POLICY site_email_config_site_scope_read ON site_email_config
    AS RESTRICTIVE FOR SELECT
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id IS NULL
        OR site_id = ANY (
            string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
        )
    );

CREATE POLICY site_email_config_site_scope_insert ON site_email_config
    AS RESTRICTIVE FOR INSERT
    WITH CHECK (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
        )
    );

-- USING refuses the org row as an update target; WITH CHECK refuses turning
-- one's own site row into an org row (the two-step route to the same place).
CREATE POLICY site_email_config_site_scope_update ON site_email_config
    AS RESTRICTIVE FOR UPDATE
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
        )
    )
    WITH CHECK (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
        )
    );

CREATE POLICY site_email_config_site_scope_delete ON site_email_config
    AS RESTRICTIVE FOR DELETE
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
        )
    );

-- site_email_log — per-send audit trail; agent pushes rows, CP stores them.
-- Queried in Phase 3 (ingest + viewer). Created now so RLS + indexes exist.
CREATE TABLE IF NOT EXISTS site_email_log (
    id            uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    site_id       uuid        NOT NULL REFERENCES sites   (id) ON DELETE CASCADE,
    agent_seq     bigint,
    message_id    text,
    to_addresses  text[]      NOT NULL DEFAULT '{}',
    from_address  text        NOT NULL DEFAULT '',
    subject       text        NOT NULL DEFAULT '',
    provider      text        NOT NULL DEFAULT '',
    status        text        NOT NULL DEFAULT 'pending',
    response      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    error         text        NOT NULL DEFAULT '',
    retries       integer     NOT NULL DEFAULT 0,
    resent_count  integer     NOT NULL DEFAULT 0,
    body_stored    boolean     NOT NULL DEFAULT false,
    body           text,
    -- m62: connection_key identifies which named connection sent this email.
    connection_key text        NOT NULL DEFAULT '',
    -- m62: attachments metadata — JSON array of {name, size_bytes} objects.
    attachments    jsonb       NOT NULL DEFAULT '[]'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT site_email_log_pkey PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS site_email_log_site_time_idx
    ON site_email_log (tenant_id, site_id, created_at DESC);

CREATE INDEX IF NOT EXISTS site_email_log_tenant_time_idx
    ON site_email_log (tenant_id, created_at DESC);

CREATE INDEX IF NOT EXISTS site_email_log_failed_idx
    ON site_email_log (tenant_id, created_at DESC)
    WHERE status = 'failed';

CREATE UNIQUE INDEX IF NOT EXISTS site_email_log_seq_idx
    ON site_email_log (tenant_id, site_id, agent_seq)
    WHERE agent_seq IS NOT NULL;

ALTER TABLE site_email_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_email_log FORCE ROW LEVEL SECURITY;

CREATE POLICY site_email_log_tenant_isolation ON site_email_log
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY site_email_log_agent ON site_email_log
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- m112 (GH #380): site_id is NOT NULL here, so there is no org row and nothing
-- to inherit. The read/write split is kept anyway so all four email tables read
-- identically; the read predicate simply has no IS NULL branch.
CREATE POLICY site_email_log_site_scope_read ON site_email_log
    AS RESTRICTIVE FOR SELECT
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
        )
    );

CREATE POLICY site_email_log_site_scope_insert ON site_email_log
    AS RESTRICTIVE FOR INSERT
    WITH CHECK (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
        )
    );

CREATE POLICY site_email_log_site_scope_update ON site_email_log
    AS RESTRICTIVE FOR UPDATE
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
        )
    )
    WITH CHECK (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
        )
    );

CREATE POLICY site_email_log_site_scope_delete ON site_email_log
    AS RESTRICTIVE FOR DELETE
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
        )
    );

-- email_suppression — org-wide and per-site bounce/complaint/unsubscribe list.
-- Queried in Phase 4 (webhooks + pre-send check). Created now for RLS.
CREATE TABLE IF NOT EXISTS email_suppression (
    id                uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id         uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    site_id           uuid        REFERENCES sites (id) ON DELETE CASCADE,
    email_hash        bytea       NOT NULL,
    email             text,
    reason            text        NOT NULL DEFAULT 'manual',
    provider          text        NOT NULL DEFAULT '',
    event_at          timestamptz,
    source_message_id text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT email_suppression_pkey PRIMARY KEY (id)
);

CREATE UNIQUE INDEX IF NOT EXISTS email_suppression_site_hash_idx
    ON email_suppression (tenant_id, site_id, email_hash)
    WHERE site_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS email_suppression_fleet_hash_idx
    ON email_suppression (tenant_id, email_hash)
    WHERE site_id IS NULL;

CREATE INDEX IF NOT EXISTS email_suppression_tenant_idx
    ON email_suppression (tenant_id);

ALTER TABLE email_suppression ENABLE ROW LEVEL SECURITY;
ALTER TABLE email_suppression FORCE ROW LEVEL SECURITY;

CREATE POLICY email_suppression_tenant_isolation ON email_suppression
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY email_suppression_agent ON email_suppression
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- m112 (GH #380): fleet-wide entries carry site_id IS NULL and are read by
-- every site (IsSuppressed matches site_id IS NULL OR site_id = @site_id), so
-- this is site_email_config's shape and gets its split. The DELETE policy is
-- the load-bearing one: a fleet-wide suppression row is what stops the whole
-- organisation mailing an address that complained, and removing it is an
-- organisation-level act, not a per-site one.
CREATE POLICY email_suppression_site_scope_read ON email_suppression
    AS RESTRICTIVE FOR SELECT
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id IS NULL
        OR site_id = ANY (
            string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
        )
    );

CREATE POLICY email_suppression_site_scope_insert ON email_suppression
    AS RESTRICTIVE FOR INSERT
    WITH CHECK (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
        )
    );

CREATE POLICY email_suppression_site_scope_update ON email_suppression
    AS RESTRICTIVE FOR UPDATE
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
        )
    )
    WITH CHECK (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
        )
    );

CREATE POLICY email_suppression_site_scope_delete ON email_suppression
    AS RESTRICTIVE FOR DELETE
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
        )
    );

-- ---------------------------------------------------------------------------
-- email_webhook_events — dedup + audit table for provider webhook events (m60).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS email_webhook_events (
    id                uuid        NOT NULL DEFAULT gen_random_uuid(),
    provider_event_id text        NOT NULL,
    provider          text        NOT NULL,
    tenant_id         uuid,
    site_id           uuid,
    -- m61: email stored as SHA-256 hash only (PII reduction; SHOULD-FIX #2).
    -- The plaintext email column was dropped in m61.
    email_hash        bytea,
    event_type        text        NOT NULL DEFAULT '',
    suppression_id    uuid,
    created_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT email_webhook_events_pkey PRIMARY KEY (id)
);

CREATE UNIQUE INDEX IF NOT EXISTS email_webhook_events_dedup_idx
    ON email_webhook_events (provider, provider_event_id);

CREATE INDEX IF NOT EXISTS email_webhook_events_created_idx
    ON email_webhook_events (created_at);

CREATE INDEX IF NOT EXISTS email_webhook_events_tenant_idx
    ON email_webhook_events (tenant_id)
    WHERE tenant_id IS NOT NULL;

ALTER TABLE email_webhook_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE email_webhook_events FORCE ROW LEVEL SECURITY;

CREATE POLICY email_webhook_events_agent ON email_webhook_events
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

CREATE POLICY email_webhook_events_tenant_isolation ON email_webhook_events
    USING (
        tenant_id IS NULL
        OR tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
    );

-- ---------------------------------------------------------------------------
-- m62 — site_email_connection (multi-connection + failover)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS site_email_connection (
    id                        uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id                 uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    config_id                 uuid        NOT NULL REFERENCES site_email_config (id) ON DELETE CASCADE,
    connection_key            text        NOT NULL,
    provider                  text        NOT NULL DEFAULT 'smtp',
    from_address              text        NOT NULL DEFAULT '',
    from_name                 text        NOT NULL DEFAULT '',
    config                    jsonb       NOT NULL DEFAULT '{}'::jsonb,
    provider_secret_encrypted bytea,
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT site_email_connection_pkey PRIMARY KEY (id),
    CONSTRAINT site_email_connection_key_check
        CHECK (connection_key ~ '^[a-z0-9][a-z0-9_-]{0,31}$' AND connection_key <> 'default')
);

CREATE UNIQUE INDEX IF NOT EXISTS site_email_connection_cfg_key_idx
    ON site_email_connection (config_id, connection_key);

CREATE INDEX IF NOT EXISTS site_email_connection_tenant_idx
    ON site_email_connection (tenant_id);

ALTER TABLE site_email_connection ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_email_connection FORCE ROW LEVEL SECURITY;

CREATE POLICY site_email_connection_tenant_isolation ON site_email_connection
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY site_email_connection_agent ON site_email_connection
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- m112 (GH #380): this table has NO site_id column, so the site-scope
-- predicate reaches through config_id to site_email_config (the indirect-join
-- approach m19 uses for its own child tables). The read predicate resolves the
-- org row and the write predicates require a non-NULL site_id in the
-- allowlist, so an org connection is readable and not writable, exactly
-- matching its parent. This table stores provider_secret_encrypted per
-- connection, so gating the parent while leaving the child open would have
-- left the credential reachable through the child.
CREATE POLICY site_email_connection_site_scope_read ON site_email_connection
    AS RESTRICTIVE FOR SELECT
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR EXISTS (
            SELECT 1 FROM site_email_config c
            WHERE c.id = site_email_connection.config_id
              AND (
                  c.site_id IS NULL
                  OR c.site_id = ANY (
                      string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
                  )
              )
        )
    );

CREATE POLICY site_email_connection_site_scope_insert ON site_email_connection
    AS RESTRICTIVE FOR INSERT
    WITH CHECK (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR EXISTS (
            SELECT 1 FROM site_email_config c
            WHERE c.id = site_email_connection.config_id
              AND c.site_id = ANY (
                  string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
              )
        )
    );

CREATE POLICY site_email_connection_site_scope_update ON site_email_connection
    AS RESTRICTIVE FOR UPDATE
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR EXISTS (
            SELECT 1 FROM site_email_config c
            WHERE c.id = site_email_connection.config_id
              AND c.site_id = ANY (
                  string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
              )
        )
    )
    WITH CHECK (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR EXISTS (
            SELECT 1 FROM site_email_config c
            WHERE c.id = site_email_connection.config_id
              AND c.site_id = ANY (
                  string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
              )
        )
    );

CREATE POLICY site_email_connection_site_scope_delete ON site_email_connection
    AS RESTRICTIVE FOR DELETE
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR EXISTS (
            SELECT 1 FROM site_email_config c
            WHERE c.id = site_email_connection.config_id
              AND c.site_id = ANY (
                  string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
              )
        )
    );

-- ---------------------------------------------------------------------------
-- m62 — email_notify_settings (org-level alert + digest preferences)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS email_notify_settings (
    tenant_id              uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    enabled                boolean     NOT NULL DEFAULT false,
    recipients             jsonb       NOT NULL DEFAULT '[]'::jsonb,
    alert_on_failure       boolean     NOT NULL DEFAULT true,
    alert_throttle_minutes integer     NOT NULL DEFAULT 60
        CONSTRAINT email_notify_settings_throttle_range CHECK (alert_throttle_minutes BETWEEN 15 AND 1440),
    digest_enabled         boolean     NOT NULL DEFAULT false,
    digest_cadence         text        NOT NULL DEFAULT 'weekly'
        CONSTRAINT email_notify_settings_digest_cadence CHECK (digest_cadence IN ('daily', 'weekly', 'monthly')),
    digest_day             integer     NOT NULL DEFAULT 1
        CONSTRAINT email_notify_settings_digest_day CHECK (digest_day BETWEEN 0 AND 28),
    digest_hour            integer     NOT NULL DEFAULT 8
        CONSTRAINT email_notify_settings_digest_hour CHECK (digest_hour BETWEEN 0 AND 23),
    timezone               text        NOT NULL DEFAULT 'UTC',
    next_digest_at         timestamptz,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT email_notify_settings_pkey PRIMARY KEY (tenant_id)
);

CREATE INDEX IF NOT EXISTS email_notify_settings_due_idx
    ON email_notify_settings (next_digest_at)
    WHERE digest_enabled AND enabled;

ALTER TABLE email_notify_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE email_notify_settings FORCE ROW LEVEL SECURITY;

CREATE POLICY email_notify_settings_tenant_isolation ON email_notify_settings
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY email_notify_settings_agent ON email_notify_settings
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- m62 — email_alert_state (per-site durable alert throttle state)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS email_alert_state (
    tenant_id            uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    site_id              uuid        NOT NULL REFERENCES sites  (id) ON DELETE CASCADE,
    last_alert_at        timestamptz,
    failures_since_alert bigint      NOT NULL DEFAULT 0,
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT email_alert_state_pkey PRIMARY KEY (tenant_id, site_id)
);

ALTER TABLE email_alert_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE email_alert_state FORCE ROW LEVEL SECURITY;

CREATE POLICY email_alert_state_tenant_isolation ON email_alert_state
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY email_alert_state_agent ON email_alert_state
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- m63 — clients (Clients Foundation: per-tenant client records for grouping
-- sites under agency customers; soft-delete via archived_at)
-- ---------------------------------------------------------------------------
CREATE TABLE clients (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name          text        NOT NULL,
    contact_email citext,
    company       text,
    phone         text,
    notes         text,
    color         text,
    logo_url      text,
    archived_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    -- Backs the composite FK on sites (prevents tenant drift, mirrors
    -- sites_id_tenant_key used by site_shares in m19).
    CONSTRAINT clients_id_tenant_key UNIQUE (id, tenant_id)
);

-- Fast tenant-scoped list + assignment lookups.
CREATE INDEX clients_tenant_idx ON clients (tenant_id);

-- Composite FK from sites.client_id (declared in the sites table above):
-- cross-tenant-proof because (client_id, tenant_id) must exist in clients.
-- Deleting a client unassigns its sites. The SET NULL column list is
-- load-bearing (m66 repair): a bare SET NULL on a composite FK nulls every
-- referencing column including the NOT NULL tenant_id, breaking client
-- deletion for any client with assigned sites.
ALTER TABLE sites
    ADD CONSTRAINT sites_client_tenant_fkey
    FOREIGN KEY (client_id, tenant_id)
    REFERENCES clients (id, tenant_id)
    ON DELETE SET NULL (client_id);

-- Partial index: only rows that have a client assigned benefit.
CREATE INDEX sites_client_idx ON sites (client_id)
    WHERE client_id IS NOT NULL;

-- RLS mirrors m36: tenant isolation + agent path. No site_scope RESTRICTIVE
-- policy — a site-scoped collaborator must never enumerate the client roster;
-- org access is gated in-app via RequireOrgScope + PermClientRead/Manage.
ALTER TABLE clients ENABLE ROW LEVEL SECURITY;
ALTER TABLE clients FORCE ROW LEVEL SECURITY;

CREATE POLICY clients_tenant_isolation ON clients
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY clients_agent ON clients
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- m66 clients_member_read — tables referenced INSIDE a policy expression are
-- subject to their own RLS. sites_client_read JOINs clients, and under
-- InUserTx (no app.tenant_id) clients_tenant_isolation hides every row, which
-- would leave portal principals with zero sites. SELECT-only; admits exactly
-- the clients rows of the caller's own memberships.
CREATE POLICY clients_member_read ON clients
    FOR SELECT
    USING (EXISTS (
        SELECT 1 FROM client_members cm
        WHERE cm.client_id = clients.id
          AND cm.tenant_id = clients.tenant_id
          AND cm.user_id   = nullif(current_setting('app.user_id', true), '')::uuid
    ));

-- m64: client-level timezone governs report send-time (decision 6).
-- IANA names validated app-side (time.LoadLocation → UTC fallback on failure).
ALTER TABLE clients
    ADD COLUMN IF NOT EXISTS timezone text NOT NULL DEFAULT 'UTC';

-- ---------------------------------------------------------------------------
-- report_schedules  (m64 — White-label client reports Phase 2)
-- One row per client: cadence, recipients, section flags, branding, powered-by.
-- ---------------------------------------------------------------------------
CREATE TABLE report_schedules (
    id                 uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id          uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    client_id          uuid        NOT NULL,
    enabled            boolean     NOT NULL DEFAULT false,
    cadence            text        NOT NULL DEFAULT 'monthly'
        CONSTRAINT report_schedules_cadence CHECK (cadence IN ('weekly','monthly')),
    send_day           integer     NOT NULL DEFAULT 1
        CONSTRAINT report_schedules_send_day CHECK (send_day BETWEEN 0 AND 28),
    send_hour          integer     NOT NULL DEFAULT 8
        CONSTRAINT report_schedules_send_hour CHECK (send_hour BETWEEN 0 AND 23),
    recipients         jsonb       NOT NULL DEFAULT '[]'::jsonb,
    sections           jsonb       NOT NULL DEFAULT '{}'::jsonb,
    intro_text         text        NOT NULL DEFAULT '',
    closing_text       text        NOT NULL DEFAULT '',
    powered_by_removed boolean     NOT NULL DEFAULT false,
    next_run_at        timestamptz,
    last_run_at        timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT report_schedules_pkey           PRIMARY KEY (id),
    CONSTRAINT report_schedules_client_key     UNIQUE (client_id),
    CONSTRAINT report_schedules_client_tenant_fkey
        FOREIGN KEY (client_id, tenant_id)
        REFERENCES clients (id, tenant_id) ON DELETE CASCADE
);

CREATE INDEX report_schedules_tenant_idx ON report_schedules (tenant_id);
CREATE INDEX report_schedules_due_idx ON report_schedules (next_run_at) WHERE enabled;

ALTER TABLE report_schedules ENABLE ROW LEVEL SECURITY;
ALTER TABLE report_schedules FORCE ROW LEVEL SECURITY;

CREATE POLICY report_schedules_tenant_isolation ON report_schedules
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY report_schedules_agent ON report_schedules
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- generated_reports  (m64 — White-label client reports Phase 2)
-- One row per rendered report: status, period, blob keys, data snapshot.
-- ---------------------------------------------------------------------------
CREATE TABLE generated_reports (
    id             uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id      uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    client_id      uuid        NOT NULL,
    -- NULL = on-demand. ON DELETE SET NULL: deleting/recreating a schedule
    -- must never destroy report history.
    schedule_id    uuid        REFERENCES report_schedules (id) ON DELETE SET NULL,
    period_start   timestamptz NOT NULL,
    period_end     timestamptz NOT NULL,
    status         text        NOT NULL DEFAULT 'pending'
        CONSTRAINT generated_reports_status
        CHECK (status IN ('pending','generating','completed','failed')),
    data_snapshot  jsonb       NOT NULL DEFAULT '{}'::jsonb,
    html_blob_key  text        NOT NULL DEFAULT '',
    pdf_blob_key   text        NOT NULL DEFAULT '',
    error          text        NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    -- m65: updated_at was omitted from m64 while every report mutation writes
    -- it (sqlc does not validate UPDATE SET columns, so this only failed at
    -- runtime). Keep in lockstep with the migration.
    updated_at     timestamptz NOT NULL DEFAULT now(),
    completed_at   timestamptz,

    CONSTRAINT generated_reports_pkey PRIMARY KEY (id),

    -- ON DELETE CASCADE: reports are only reachable through the client detail page.
    CONSTRAINT generated_reports_client_tenant_fkey
        FOREIGN KEY (client_id, tenant_id)
        REFERENCES clients (id, tenant_id) ON DELETE CASCADE
);

-- Keyset cursor index. List queries use composite predicate (created_at,id)<.
CREATE INDEX generated_reports_list_idx
    ON generated_reports (tenant_id, client_id, created_at DESC, id DESC);

ALTER TABLE generated_reports ENABLE ROW LEVEL SECURITY;
ALTER TABLE generated_reports FORCE ROW LEVEL SECURITY;

CREATE POLICY generated_reports_tenant_isolation ON generated_reports
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY generated_reports_agent ON generated_reports
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- m66 — client_members (Client portal Phase 3)
-- ---------------------------------------------------------------------------
-- Portal user roster per client. user_id refers to a users row that has NO
-- tenant membership; access is resolved entirely at auth time by the
-- client_members_self_read + sites_client_read RLS policies.
-- Deleting a client CASCADEs client_members and pending client invitations.
CREATE TABLE client_members (
    id         uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id  uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    client_id  uuid        NOT NULL,
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    invited_by uuid        NULL     REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT client_members_pkey PRIMARY KEY (id),
    CONSTRAINT client_members_client_user_key UNIQUE (client_id, user_id),
    CONSTRAINT client_members_client_tenant_fkey
        FOREIGN KEY (client_id, tenant_id)
        REFERENCES clients (id, tenant_id)
        ON DELETE CASCADE
);

-- Auth-time lookup: (user_id, tenant_id) on every portal request.
CREATE INDEX client_members_user_tenant_idx ON client_members (user_id, tenant_id);
-- Roster listing per client (agency UI).
CREATE INDEX client_members_client_idx ON client_members (client_id);

ALTER TABLE client_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE client_members FORCE ROW LEVEL SECURITY;

-- Operator / API path: full read+write scoped to the current tenant.
CREATE POLICY client_members_tenant_isolation ON client_members
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- Agent / worker path: cross-tenant reads/writes when app.agent='on'.
CREATE POLICY client_members_agent ON client_members
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- Self-read: auth-time lookup runs under InUserTx (only app.user_id is set).
-- SELECT-only; mirrors site_shares_self_read (m19).
CREATE POLICY client_members_self_read ON client_members
    FOR SELECT
    USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- site_object_cache_config -- M68: per-site object cache connection config.
-- One row per site (site_id PK). Password stored age-encrypted via cryptbox
-- (nil-sentinel: NULL means "keep stored secret on update"). Heartbeat-sourced
-- status columns enable SSE state-transition detection without hitting the agent.
-- ---------------------------------------------------------------------------
CREATE TABLE site_object_cache_config (
    site_id                 uuid        NOT NULL,
    tenant_id               uuid        NOT NULL,
    enabled                 boolean     NOT NULL DEFAULT false,
    scheme                  text        NOT NULL DEFAULT 'tcp',
    host                    text        NOT NULL DEFAULT '',
    port                    integer     NOT NULL DEFAULT 6379,
    socket_path             text        NOT NULL DEFAULT '',
    database                integer     NOT NULL DEFAULT 0,
    username                text        NOT NULL DEFAULT '',
    password_encrypted      bytea,
    prefix                  text        NOT NULL DEFAULT '',
    maxttl_seconds          integer     NOT NULL DEFAULT 604800,
    queryttl_seconds        integer     NOT NULL DEFAULT 86400,
    connect_timeout_ms      integer     NOT NULL DEFAULT 1000,
    read_timeout_ms         integer     NOT NULL DEFAULT 1000,
    retry_count             integer     NOT NULL DEFAULT 3,
    retry_interval_ms       integer     NOT NULL DEFAULT 25,
    serializer              text        NOT NULL DEFAULT 'php',
    compression             text        NOT NULL DEFAULT 'none',
    async_flush             boolean     NOT NULL DEFAULT false,
    flush_strategy          text        NOT NULL DEFAULT 'auto',
    shared                  boolean     NOT NULL DEFAULT true,
    flush_on_failback       boolean     NOT NULL DEFAULT true,
    analytics_enabled       boolean     NOT NULL DEFAULT true,
    last_test_config_hash   text,
    last_test_result_json   jsonb       NOT NULL DEFAULT '{}'::jsonb,
    last_tested_at          timestamptz,
    oc_state                text        NOT NULL DEFAULT '',
    oc_latency_ms           integer     NOT NULL DEFAULT 0,
    oc_last_error_class     text        NOT NULL DEFAULT '',
    oc_used_memory_bytes    bigint      NOT NULL DEFAULT 0,
    oc_hit_ratio_pct        numeric(5,2),
    -- M69: true when the agent's last heartbeat config_hash differs from the
    -- CP-computed hash of the stored config (indicates a live/stored drift).
    oc_config_drift         boolean     NOT NULL DEFAULT false,
    -- M70: when true, the apply_config push includes this flag so the drop-in
    -- emits a per-request X-WPMgr-Cache debug response header. Default false.
    debug_header_enabled    boolean     NOT NULL DEFAULT false,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT site_object_cache_config_pkey PRIMARY KEY (site_id),
    CONSTRAINT site_object_cache_config_site_fkey
        FOREIGN KEY (site_id) REFERENCES sites (id) ON DELETE CASCADE,
    CONSTRAINT site_object_cache_config_tenant_fkey
        FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
);

CREATE INDEX site_object_cache_config_tenant_idx
    ON site_object_cache_config (tenant_id);

ALTER TABLE site_object_cache_config ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_object_cache_config FORCE ROW LEVEL SECURITY;
CREATE POLICY site_object_cache_config_tenant_isolation ON site_object_cache_config
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY site_object_cache_config_agent ON site_object_cache_config
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- site_object_cache_stats_history -- M68: append-only hit-ratio + server-metric
-- time-series. Mirrors site_cache_hit_ratio_history (m52) exactly.
-- Retention: 7 days raw + 90 days daily downsample (River GC sweep, D4).
-- ---------------------------------------------------------------------------
CREATE TABLE site_object_cache_stats_history (
    id                  uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    site_id             uuid          NOT NULL REFERENCES sites   (id) ON DELETE CASCADE,
    tenant_id           uuid          NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    hit_count           bigint        NOT NULL DEFAULT 0,
    miss_count          bigint        NOT NULL DEFAULT 0,
    ratio_pct           numeric(5,2),
    used_memory_bytes   bigint        NOT NULL DEFAULT 0,
    avg_wait_ms         numeric(8,3)  NOT NULL DEFAULT 0,
    ops_per_sec         integer       NOT NULL DEFAULT 0,
    evicted_keys_delta  bigint        NOT NULL DEFAULT 0,
    connected_clients   integer       NOT NULL DEFAULT 0,
    sampled_at          timestamptz   NOT NULL,
    created_at          timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT site_object_cache_stats_history_site_sampled_uniq UNIQUE (site_id, sampled_at)
);

CREATE INDEX site_object_cache_stats_history_site_sampled_idx
    ON site_object_cache_stats_history (site_id, sampled_at DESC);
CREATE INDEX site_object_cache_stats_history_created_idx
    ON site_object_cache_stats_history (created_at);

ALTER TABLE site_object_cache_stats_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_object_cache_stats_history FORCE ROW LEVEL SECURITY;
CREATE POLICY site_object_cache_stats_history_tenant_isolation ON site_object_cache_stats_history
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- GC path only deletes; inserts flow through tenant_isolation via InTenantTx.
CREATE POLICY site_object_cache_stats_history_agent ON site_object_cache_stats_history
    USING (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- site_screenshots -- M72: one row per site, upserted on each capture.
-- The screenshot bytes live in object storage (screenshots/{tenant_id}/{site_id}/{ulid}.webp).
-- A ULID in the key makes every capture a unique object (no CDN staleness);
-- the worker deletes the prior key on success (best-effort).
-- ---------------------------------------------------------------------------
CREATE TABLE site_screenshots (
    site_id           uuid        NOT NULL,
    tenant_id         uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    screenshot_key    text        NOT NULL DEFAULT '',
    screenshot_key_2x text        NOT NULL DEFAULT '',
    width             integer     NOT NULL DEFAULT 0,
    height            integer     NOT NULL DEFAULT 0,
    status            text        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','ready','failed')),
    failed_reason     text,
    captured_at       timestamptz,
    etag              text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT site_screenshots_pkey PRIMARY KEY (site_id)
);

CREATE INDEX site_screenshots_tenant_idx ON site_screenshots (tenant_id);

ALTER TABLE site_screenshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_screenshots FORCE ROW LEVEL SECURITY;

CREATE POLICY site_screenshots_tenant_isolation ON site_screenshots
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY site_screenshots_agent ON site_screenshots
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- M3: AS RESTRICTIVE collaborator site-scope policy (mirrors backup_snapshots_site_scope).
CREATE POLICY "site_screenshots_site_scope" ON "public"."site_screenshots"
    AS RESTRICTIVE FOR ALL
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR "site_id" = ANY (
            string_to_array(
                nullif(current_setting('app.allowed_site_ids', true), ''), ','
            )::uuid[]
        )
    )
    WITH CHECK (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR "site_id" = ANY (
            string_to_array(
                nullif(current_setting('app.allowed_site_ids', true), ''), ','
            )::uuid[]
        )
    );

-- ---------------------------------------------------------------------------
-- m73: two-factor authentication (ADR-056)
-- users columns, six new tables; all RLS under app.agent='on' (pre-tenant auth flow).
-- ---------------------------------------------------------------------------

ALTER TABLE users ADD COLUMN IF NOT EXISTS two_factor_enabled    bool   NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_secret_encrypted bytea;
ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_confirmed_at     timestamptz;
-- totp_last_step: last accepted TOTP time-step for replay protection (Phase 2).
ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_last_step                    bigint;
-- Provisional TOTP secret: unconfirmed secret between BeginRegistration and
-- FinishRegistration. Cleared on confirmation or TTL expiry.
ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_provisional_secret_encrypted bytea;
ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_provisional_expires_at       timestamptz;

-- user_recovery_codes: 10 single-use account-level backup codes.
-- code_hash: argon2id hash of the plaintext code (never stored plaintext).
-- used_at IS NULL = available; IS NOT NULL = consumed.
CREATE TABLE IF NOT EXISTS user_recovery_codes (
    id         uuid        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash  text        NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS user_recovery_codes_user_idx
    ON user_recovery_codes (user_id);

ALTER TABLE user_recovery_codes ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_recovery_codes FORCE ROW LEVEL SECURITY;

CREATE POLICY user_recovery_codes_agent ON user_recovery_codes
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- webauthn_credentials: registered passkeys / FIDO2 hardware keys per user.
-- credential_id: the credential ID returned by the authenticator (variable length bytes).
-- sign_count: must strictly increase on each assertion; tracked for clone detection.
-- transports: authenticator transport hints (e.g. ["internal","usb"]).
CREATE TABLE IF NOT EXISTS webauthn_credentials (
    id               uuid        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    credential_id    bytea       NOT NULL UNIQUE,
    public_key       bytea       NOT NULL,
    attestation_type text        NOT NULL DEFAULT '',
    aaguid           bytea       NOT NULL DEFAULT ''::bytea,
    sign_count       bigint      NOT NULL DEFAULT 0,
    transports       text[],
    name             text        NOT NULL DEFAULT '',
    backup_eligible  bool        NOT NULL DEFAULT false,
    backup_state     bool        NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    last_used_at     timestamptz
);

CREATE INDEX IF NOT EXISTS webauthn_credentials_user_idx
    ON webauthn_credentials (user_id);

ALTER TABLE webauthn_credentials ENABLE ROW LEVEL SECURITY;
ALTER TABLE webauthn_credentials FORCE ROW LEVEL SECURITY;

CREATE POLICY webauthn_credentials_agent ON webauthn_credentials
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- two_factor_challenges: transient factor-agnostic login challenges.
-- kind: 'login' (the only kind in Phase 1; 'recover' added in Phase 2).
-- webauthn_session: go-webauthn SessionData JSON, populated only for kind='webauthn'.
-- attempts: incremented on each failed verify; challenge locked after 5 attempts.
CREATE TABLE IF NOT EXISTS two_factor_challenges (
    id               uuid        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    challenge_nonce  text        NOT NULL,
    kind             text        NOT NULL DEFAULT 'login',
    webauthn_session jsonb,
    expires_at       timestamptz NOT NULL,
    used_at          timestamptz,
    attempts         integer     NOT NULL DEFAULT 0,
    requested_ip     inet,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS two_factor_challenges_nonce_key
    ON two_factor_challenges (challenge_nonce) WHERE used_at IS NULL;

CREATE INDEX IF NOT EXISTS two_factor_challenges_user_active_idx
    ON two_factor_challenges (user_id)
    WHERE used_at IS NULL;

ALTER TABLE two_factor_challenges ENABLE ROW LEVEL SECURITY;
ALTER TABLE two_factor_challenges FORCE ROW LEVEL SECURITY;

CREATE POLICY two_factor_challenges_agent ON two_factor_challenges
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- webauthn_registration_sessions: stash go-webauthn SessionData during credential
-- registration (BeginRegistration -> FinishRegistration). TTL'd, account-scoped.
CREATE TABLE IF NOT EXISTS webauthn_registration_sessions (
    id         uuid        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    session    jsonb       NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS webauthn_registration_sessions_user_idx
    ON webauthn_registration_sessions (user_id);

ALTER TABLE webauthn_registration_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE webauthn_registration_sessions FORCE ROW LEVEL SECURITY;

CREATE POLICY webauthn_registration_sessions_agent ON webauthn_registration_sessions
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- trusted_devices: "remember this device" entries per user.
-- token_hash: argon2id hash of the opaque device trust token stored in the cookie.
-- expires_at: when the trust expires (user-chosen window, default 30 days).
-- revoked_at: soft-delete; NULL = active.
CREATE TABLE IF NOT EXISTS trusted_devices (
    id           uuid        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash   text        NOT NULL UNIQUE,
    label        text        NOT NULL DEFAULT '',
    user_agent   text        NOT NULL DEFAULT '',
    ip           inet,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    last_used_at timestamptz,
    revoked_at   timestamptz
);

CREATE INDEX IF NOT EXISTS trusted_devices_user_idx
    ON trusted_devices (user_id);

ALTER TABLE trusted_devices ENABLE ROW LEVEL SECURITY;
ALTER TABLE trusted_devices FORCE ROW LEVEL SECURITY;

CREATE POLICY trusted_devices_agent ON trusted_devices
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- m78 — Security Suite Phase 3: per-site user 2FA + password policy
-- ---------------------------------------------------------------------------

-- site_security_policy: one row per site; the CP source of truth for the
-- site-user 2FA + password + hide-backend policy knobs. All knobs default OFF.
CREATE TABLE IF NOT EXISTS site_security_policy (
    site_id                         uuid PRIMARY KEY,
    tenant_id                       uuid NOT NULL,
    two_factor_enabled              boolean NOT NULL DEFAULT false,
    two_factor_methods              text[] NOT NULL DEFAULT '{totp,email,backup}',
    two_factor_required_roles       text[] NOT NULL DEFAULT '{}',
    two_factor_grace_logins         int NOT NULL DEFAULT 3
        CONSTRAINT site_security_policy_grace_logins_chk
        CHECK (two_factor_grace_logins >= 0 AND two_factor_grace_logins <= 100),
    two_factor_remember_device_days int NOT NULL DEFAULT 30
        CONSTRAINT site_security_policy_remember_device_days_chk
        CHECK (two_factor_remember_device_days >= 0 AND two_factor_remember_device_days <= 365),
    block_xmlrpc_for_2fa_users      boolean NOT NULL DEFAULT true,
    password_min_zxcvbn_score       int NOT NULL DEFAULT 0
        CONSTRAINT site_security_policy_zxcvbn_score_chk
        CHECK (password_min_zxcvbn_score >= 0 AND password_min_zxcvbn_score <= 4),
    password_min_zxcvbn_roles       text[] NOT NULL DEFAULT '{}',
    password_block_compromised      boolean NOT NULL DEFAULT false,
    password_reuse_block_count      int NOT NULL DEFAULT 0
        CONSTRAINT site_security_policy_reuse_block_count_chk
        CHECK (password_reuse_block_count >= 0 AND password_reuse_block_count <= 50),
    password_max_age_days           int NOT NULL DEFAULT 0
        CONSTRAINT site_security_policy_max_age_days_chk
        CHECK (password_max_age_days >= 0 AND password_max_age_days <= 3650),
    password_expiry_roles           text[] NOT NULL DEFAULT '{}',
    hide_backend_enabled            boolean NOT NULL DEFAULT false,
    hide_backend_slug               text NOT NULL DEFAULT '',
    hide_backend_redirect           text NOT NULL DEFAULT '',
    updated_at                      timestamptz NOT NULL DEFAULT now(),
    actor_type                      text,
    actor_id                        text,
    CONSTRAINT site_security_policy_tenant_fkey
        FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT site_security_policy_site_fkey
        FOREIGN KEY (site_id) REFERENCES sites (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS site_security_policy_tenant_idx
    ON site_security_policy (tenant_id);

ALTER TABLE site_security_policy ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_security_policy FORCE ROW LEVEL SECURITY;

CREATE POLICY site_security_policy_tenant_isolation ON site_security_policy
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY site_security_policy_agent ON site_security_policy
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- site_security_policy_groups: per-role policy overrides.
-- One row per (site_id, role); nullable override columns.
CREATE TABLE IF NOT EXISTS site_security_policy_groups (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         uuid NOT NULL,
    site_id           uuid NOT NULL,
    role              text NOT NULL,
    require_2fa       boolean,
    allowed_methods   text[],
    min_zxcvbn_score  int
        CONSTRAINT site_security_policy_groups_zxcvbn_score_chk
        CHECK (min_zxcvbn_score IS NULL OR (min_zxcvbn_score >= 0 AND min_zxcvbn_score <= 4)),
    block_compromised boolean,
    max_age_days      int
        CONSTRAINT site_security_policy_groups_max_age_days_chk
        CHECK (max_age_days IS NULL OR (max_age_days >= 0 AND max_age_days <= 3650)),
    created_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT site_security_policy_groups_tenant_fkey
        FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT site_security_policy_groups_site_fkey
        FOREIGN KEY (site_id) REFERENCES sites (id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX IF NOT EXISTS site_security_policy_groups_site_role_idx
    ON site_security_policy_groups (site_id, role);

CREATE INDEX IF NOT EXISTS site_security_policy_groups_tenant_site_idx
    ON site_security_policy_groups (tenant_id, site_id);

ALTER TABLE site_security_policy_groups ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_security_policy_groups FORCE ROW LEVEL SECURITY;

CREATE POLICY site_security_policy_groups_tenant_isolation ON site_security_policy_groups
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY site_security_policy_groups_agent ON site_security_policy_groups
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- hibp_breach_cache: global CP-side cache for the HIBP Pwned Passwords range API.
-- Public breach data; no tenant association; no RLS.
CREATE TABLE IF NOT EXISTS hibp_breach_cache (
    prefix     char(5) PRIMARY KEY,
    body       text NOT NULL,
    fetched_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS hibp_breach_cache_fetched_at_idx
    ON hibp_breach_cache (fetched_at);

-- ---------------------------------------------------------------------------
-- wporg_plugin_checksums / wporg_plugin_checksums_meta
-- (m77: plugin/theme checksum cache; m106: added sha256)
-- ---------------------------------------------------------------------------
-- No RLS. Public reference data from the wp.org plugin-checksums endpoint,
-- not tenant-scoped. md5 is part of the primary key because wp.org may list
-- multiple accepted md5 variants per file (line-ending / build differences);
-- sha256 is the variant's paired stronger hash, nullable because rows fetched
-- before m106 have none. See internal/scan/checksums.go for the ingest path
-- and the negative-filter-only trust rule this sets up.
-- kind: 'plugin' or 'theme'. Shares the table; the kind column disambiguates.
CREATE TABLE IF NOT EXISTS wporg_plugin_checksums (
    kind       text NOT NULL
        CONSTRAINT wporg_plugin_checksums_kind_chk
        CHECK (kind IN ('plugin', 'theme')),
    slug       text NOT NULL,
    version    text NOT NULL,
    path       text NOT NULL,
    md5        text NOT NULL,
    sha256     text,
    fetched_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT wporg_plugin_checksums_pkey
        PRIMARY KEY (kind, slug, version, path, md5)
);

CREATE INDEX IF NOT EXISTS wporg_plugin_checksums_lookup_idx
    ON wporg_plugin_checksums (kind, slug, version, path);

-- Freshness / negative-cache sentinel per (kind, slug, version). No RLS.
CREATE TABLE IF NOT EXISTS wporg_plugin_checksums_meta (
    kind       text NOT NULL
        CONSTRAINT wporg_plugin_checksums_meta_kind_chk
        CHECK (kind IN ('plugin', 'theme')),
    slug       text NOT NULL,
    version    text NOT NULL,
    fetched_at timestamptz NOT NULL DEFAULT now(),
    ok         boolean NOT NULL DEFAULT true,

    CONSTRAINT wporg_plugin_checksums_meta_pkey
        PRIMARY KEY (kind, slug, version)
);

-- ---------------------------------------------------------------------------
-- instance_settings  (m80 — UI-configurable instance-level secrets)
-- ---------------------------------------------------------------------------
-- Generic key/value store for INSTANCE-GLOBAL (non-tenant-scoped) settings
-- that require encrypted-at-rest storage. The first consumer is the Wordfence
-- Intelligence API key.
--
-- No tenant_id column — intentionally instance-global. RLS mirrors
-- smtp_settings (m30): ENABLE + FORCE + single _agent policy. Real access
-- control is the HTTP-layer requireSuperadmin middleware. value_enc holds the
-- age-encrypted ciphertext; NULL means the key is unset for that setting.
-- updated_at is set in SQL (now()); no trigger.
CREATE TABLE IF NOT EXISTS instance_settings (
    key        text        PRIMARY KEY,
    value_enc  bytea,      -- age-encrypted ciphertext; NULL = unset
    updated_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE instance_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE instance_settings FORCE ROW LEVEL SECURITY;

-- Instance-global infra row: readable/writable only under app.agent='on'.
-- HTTP-layer requireSuperadmin gating is the real access control.
CREATE POLICY instance_settings_agent ON instance_settings
    USING  (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- wordfence_vuln_feed  (m79 — global public vulnerability record cache)
-- ---------------------------------------------------------------------------
-- No RLS. Public reference data from the Wordfence Intelligence V3 feed.
-- Written by the ingester via InAgentTx; read by inventory matching directly.
-- reference_urls (renamed from "references" in m81 — "references" is a
-- reserved keyword in PostgreSQL that causes SQLSTATE 42601 when unquoted).
CREATE TABLE IF NOT EXISTS wordfence_vuln_feed (
    vuln_id        text PRIMARY KEY,
    title          text NOT NULL DEFAULT '',
    cve            text,
    cve_link       text,
    cvss_score     numeric(3,1),
    cvss_rating    text,
    cwe            jsonb,
    informational  boolean NOT NULL DEFAULT false,
    reference_urls jsonb NOT NULL DEFAULT '[]',
    published      timestamptz,
    updated        timestamptz,
    raw            jsonb NOT NULL DEFAULT '{}',
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- wordfence_vuln_software  (m79 — per-software index)
-- ---------------------------------------------------------------------------
-- No RLS. One row per software[] entry per vuln. Serves (kind, slug) lookup.
CREATE TABLE IF NOT EXISTS wordfence_vuln_software (
    vuln_id           text NOT NULL
        REFERENCES wordfence_vuln_feed (vuln_id) ON DELETE CASCADE,
    kind              text NOT NULL,
    slug              text NOT NULL,
    affected_versions jsonb NOT NULL,
    patched           boolean NOT NULL DEFAULT false,
    patched_versions  jsonb NOT NULL DEFAULT '[]',

    CONSTRAINT wordfence_vuln_software_pkey
        PRIMARY KEY (vuln_id, kind, slug)
);

CREATE INDEX IF NOT EXISTS idx_wf_vuln_software_lookup
    ON wordfence_vuln_software (kind, slug);

-- ---------------------------------------------------------------------------
-- wordfence_vuln_feed_meta  (m79 — freshness + attribution sentinel)
-- ---------------------------------------------------------------------------
-- No RLS. Single row (id=1). Freshness timestamp, attribution notices, error.
CREATE TABLE IF NOT EXISTS wordfence_vuln_feed_meta (
    id                 integer PRIMARY KEY DEFAULT 1
        CONSTRAINT wordfence_vuln_feed_meta_singleton_chk CHECK (id = 1),
    fetched_at         timestamptz,
    ok                 boolean NOT NULL DEFAULT false,
    record_count       integer NOT NULL DEFAULT 0,
    defiant_notice     text,
    defiant_license    text,
    mitre_notice       text,
    last_error         text,
    -- m101 — Production-enrichment health, tracked independently of
    -- ok/fetched_at/record_count (which track Scanner-driven detection
    -- freshness and gate RescanSite). See internal/vuln/model.go FeedMeta doc.
    enrichment_ok      boolean NOT NULL DEFAULT false,
    last_enrichment_at timestamptz,
    -- m101 — Scanner/Production alternation cursor (internal worker state
    -- only; never exposed via the API). Advances only inside StampFeedMeta,
    -- on a successful non-empty ingest — see internal/vuln/repo.go GetFeedGate.
    next_feed_kind     text NOT NULL DEFAULT 'scanner'
        CONSTRAINT wordfence_vuln_feed_meta_next_feed_chk
        CHECK (next_feed_kind IN ('scanner', 'production')),
    -- m102 — wall-clock time of the last ACTUAL Wordfence HTTP request (any
    -- status code). Internal worker state only; never exposed via the API.
    -- FeedWorker.Work() gates every run on this, not on assumed job cadence
    -- (GH #245 post-deploy: a manually triggered sync landed 6 minutes after
    -- the periodic tick, well inside Wordfence's ~30-min rate-limit window).
    last_request_at    timestamptz
);

-- ---------------------------------------------------------------------------
-- site_vulnerabilities  (m79 — per-site matched findings, tenant-RLS)
-- ---------------------------------------------------------------------------
-- One row per (site_id, vuln_id, kind, slug). RLS mirrors m76/m77.
CREATE TABLE IF NOT EXISTS site_vulnerabilities (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         uuid NOT NULL,
    site_id           uuid NOT NULL
        REFERENCES sites (id) ON UPDATE NO ACTION ON DELETE CASCADE,
    vuln_id           text NOT NULL,
    kind              text NOT NULL,
    slug              text NOT NULL,
    name              text NOT NULL,
    installed_version text NOT NULL,
    fixed_version     text,
    severity          text NOT NULL
        CONSTRAINT site_vulnerabilities_severity_chk
        -- m101 — 'unknown': a finding with neither a cvss_rating nor a
        -- cvss_score is genuinely "no data", not a confirmed low severity
        -- (GH #245). See SeverityFromRating in internal/vuln/model.go.
        CHECK (severity IN ('critical', 'high', 'medium', 'low', 'unknown')),
    cvss_score        numeric(3,1),
    cve               text,
    title             text NOT NULL,
    status            text NOT NULL DEFAULT 'open'
        CONSTRAINT site_vulnerabilities_status_chk
        CHECK (status IN ('open', 'dismissed', 'resolved')),
    first_seen        timestamptz NOT NULL DEFAULT now(),
    last_seen         timestamptz NOT NULL DEFAULT now(),
    resolved_at       timestamptz,
    dismissed_at      timestamptz,
    dismissed_by      uuid,
    -- m103 (GH #247) — vulnerability alerting debounce/claim stamp. NULL means
    -- "not yet alerted"; the dispatch job claims a batch by atomically setting
    -- this to now() (see internal/vuln.Repo.ClaimUnnotifiedFindings). A
    -- resolved->open re-open (UpsertFinding) resets this to NULL so a
    -- reappearing vulnerability alerts again. A fresh migration backfills this
    -- to now() for every pre-existing row so the first-ever dispatch does not
    -- email every operator their entire historical backlog.
    notified_at       timestamptz,

    CONSTRAINT site_vulnerabilities_tenant_fkey
        FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT site_vulnerabilities_uq
        UNIQUE (site_id, vuln_id, kind, slug)
);

CREATE INDEX IF NOT EXISTS idx_site_vuln_site_open
    ON site_vulnerabilities (site_id)
    WHERE status = 'open';

CREATE INDEX IF NOT EXISTS idx_site_vuln_tenant_sev
    ON site_vulnerabilities (tenant_id, severity)
    WHERE status = 'open';

CREATE INDEX IF NOT EXISTS site_vulnerabilities_tenant_idx
    ON site_vulnerabilities (tenant_id, site_id);

-- m103: the dispatch job's claim query filters on exactly this predicate
-- (status='open' AND notified_at IS NULL) grouped by tenant_id.
CREATE INDEX IF NOT EXISTS idx_site_vuln_tenant_unnotified
    ON site_vulnerabilities (tenant_id)
    WHERE status = 'open' AND notified_at IS NULL;

ALTER TABLE site_vulnerabilities ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_vulnerabilities FORCE ROW LEVEL SECURITY;

CREATE POLICY site_vulnerabilities_tenant_isolation ON site_vulnerabilities
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY site_vulnerabilities_agent ON site_vulnerabilities
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- site_file_manager  (m82 — per-site opt-in flag, tenant-RLS)
-- ---------------------------------------------------------------------------
-- One row per site (PK = site_id). The CP is the source of truth; the agent
-- reads files_enabled on every signed file_* command. Default OFF.
CREATE TABLE IF NOT EXISTS site_file_manager (
    site_id        uuid PRIMARY KEY,
    tenant_id      uuid NOT NULL,
    files_enabled       boolean NOT NULL DEFAULT false,
    -- files_write_enabled is the SEPARATE P2 opt-in for write operations.
    -- Both files_enabled AND files_write_enabled must be true before the CP
    -- will sign any file_write / file_mkdir / file_rename / file_delete /
    -- file_chmod / file_upload_apply command.  Default: false.
    files_write_enabled boolean NOT NULL DEFAULT false,
    root_jail      text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT site_file_manager_tenant_fkey
        FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON UPDATE NO ACTION ON DELETE CASCADE,
    CONSTRAINT site_file_manager_site_fkey
        FOREIGN KEY (site_id) REFERENCES sites (id) ON UPDATE NO ACTION ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS site_file_manager_tenant_idx
    ON site_file_manager (tenant_id);

ALTER TABLE site_file_manager ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_file_manager FORCE ROW LEVEL SECURITY;

CREATE POLICY site_file_manager_tenant_isolation ON site_file_manager
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY site_file_manager_agent ON site_file_manager
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- file_transfers  (m82 — download/upload transfer bookkeeping, tenant-RLS)
-- ---------------------------------------------------------------------------
-- Short-lived rows created when the CP mints presigned URLs for a file
-- download. Rows are GC-eligible after expires_at. RLS mirrors backup_snapshots.
CREATE TABLE IF NOT EXISTS file_transfers (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid NOT NULL,
    site_id      uuid NOT NULL
        REFERENCES sites (id) ON UPDATE NO ACTION ON DELETE CASCADE,
    direction    text NOT NULL
        CONSTRAINT file_transfers_direction_chk
        CHECK (direction IN ('download', 'upload')),
    rel_path     text NOT NULL,
    status       text NOT NULL DEFAULT 'done'
        CONSTRAINT file_transfers_status_chk
        CHECK (status IN ('staged', 'active', 'done', 'failed')),
    object_key   text NOT NULL DEFAULT '',
    size_bytes   bigint NOT NULL DEFAULT 0,
    chunk_count  integer NOT NULL DEFAULT 0,
    created_by   uuid NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,

    CONSTRAINT file_transfers_tenant_fkey
        FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON UPDATE NO ACTION ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS file_transfers_tenant_site_idx
    ON file_transfers (tenant_id, site_id, created_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS file_transfers_expires_at_idx
    ON file_transfers (expires_at)
    WHERE status IN ('staged', 'done');

ALTER TABLE file_transfers ENABLE ROW LEVEL SECURITY;
ALTER TABLE file_transfers FORCE ROW LEVEL SECURITY;

CREATE POLICY file_transfers_tenant_isolation ON file_transfers
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY file_transfers_agent ON file_transfers
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- audit_integrity_baseline  (m90 — audit-integrity re-baseline)
-- ---------------------------------------------------------------------------
-- audit_log is append-only and hash-chained (see above); a historical break
-- caused by a race that pre-dates the per-tenant advisory-lock fix in
-- Recorder.Record can never be repaired in-place, so Verify would report it
-- forever with no way for an operator to acknowledge it. This table holds, at
-- most, ONE row per tenant: the chain-head snapshot (id/created_at/hash) an
-- operator moved the trusted "verify from here forward" anchor to, plus who
-- did it and when. Verify (internal/audit/audit.go) walks the FULL chain from
-- genesis when no row exists here (unchanged default behaviour); when a row
-- exists, it seeds the running hash with baseline_hash and only walks entries
-- STRICTLY AFTER (baseline_created_at, baseline_id) — so a break BEFORE the
-- baseline is permanently acknowledged (not re-reported) while any tampering
-- AFTER the baseline is still caught. Re-baselining never alters or deletes
-- any audit_log row; it only moves this anchor, and it is itself recorded as
-- a normal hash-chained audit_log entry (action
-- "audit.integrity.rebaselined") so the acknowledgment lives in the tamper-
-- evident trail too. Operator-only (no app.agent policy — the agent has no
-- reason to read or write this table).
CREATE TABLE IF NOT EXISTS audit_integrity_baseline (
    tenant_id           uuid        PRIMARY KEY,
    baseline_created_at timestamptz NOT NULL,
    baseline_id         uuid        NOT NULL,
    baseline_hash       text        NOT NULL,
    set_by              uuid        REFERENCES users (id) ON DELETE SET NULL,
    set_at              timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT audit_integrity_baseline_tenant_fkey
        FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON UPDATE NO ACTION ON DELETE CASCADE
);

ALTER TABLE audit_integrity_baseline ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_integrity_baseline FORCE ROW LEVEL SECURITY;

CREATE POLICY audit_integrity_baseline_tenant_isolation ON audit_integrity_baseline
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- m100 — GH #230 "rich tags": tenant-level tag registry. sites.tags (text[],
-- above) remains the assignment store; site_tags owns existence/color/
-- canonical name. No join table — sites.tags is the sole source of truth for
-- "which sites carry this tag". Tag names are CASE-SENSITIVE, matching the
-- existing site.normalizeTags + `= ANY(tags)` semantics.
-- ---------------------------------------------------------------------------
CREATE TABLE site_tags (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name       text        NOT NULL,
    -- '' = auto (client derives a deterministic color from the name); else a
    -- lowercase '#rrggbb' hex code (app-layer normalizes to lowercase).
    color      text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- Exact-case unique per tenant.
    CONSTRAINT site_tags_tenant_name_key UNIQUE (tenant_id, name),
    -- Backs a future composite FK the same way clients_id_tenant_key does; not
    -- referenced by any FK today (sites.tags has no join table).
    CONSTRAINT site_tags_id_tenant_key UNIQUE (id, tenant_id),
    CONSTRAINT site_tags_name_nonempty CHECK (btrim(name) != '' AND char_length(name) <= 64),
    CONSTRAINT site_tags_color_format CHECK (color = '' OR color ~* '^#[0-9a-f]{6}$')
);

CREATE INDEX site_tags_tenant_idx ON site_tags (tenant_id);

-- RLS mirrors m63 clients exactly: tenant isolation + agent path. No
-- site_scope RESTRICTIVE policy — this is a tenant-level registry, not a
-- site-keyed table.
ALTER TABLE site_tags ENABLE ROW LEVEL SECURITY;
ALTER TABLE site_tags FORCE ROW LEVEL SECURITY;

CREATE POLICY site_tags_tenant_isolation ON site_tags
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY site_tags_agent ON site_tags
    USING      (current_setting('app.agent', true) = 'on')
    WITH CHECK (current_setting('app.agent', true) = 'on');

-- ---------------------------------------------------------------------------
-- agent_mirror_state  (m109, GH #322: upstream agent-release mirror freshness)
-- ---------------------------------------------------------------------------
-- ONE ROW PER INSTALL (id=1 enforced by CHECK), NOT per tenant: the mirror
-- (internal/agentupstream) fetches one public GitHub release and writes into
-- one bucket, so it has no tenant to key on. No RLS: this table carries no
-- tenant_id, no PII and no secrets (last_attempt_detail is a curated,
-- non-secret string, never a raw wrapped error), and follows the same
-- posture as its structural sibling wordfence_vuln_feed_meta (m79/m102),
-- read/written via bare pool queries (internal/agentmirror.Repo). Contrast
-- instance_settings (m80), which DOES carry RLS because it stores encrypted
-- secrets.
--
-- last_attempt_* describes the LAST run that actually executed, whatever its
-- result; last_success_* advances ONLY when that run CONFIRMED what upstream
-- publishes (mirrored, current, or an unchanged 304); an operator-facing
-- "checked N ago" age must always be computed from last_success_at, never
-- last_attempt_at. last_mirrored_* tracks only the runs that actually
-- published something new. last_request_at is the persisted, cross-replica
-- form of the in-process request-spacing clock (agentupstream.Mirror),
-- letting the manual check endpoint answer honestly from a different
-- process than the one that will work the job.
CREATE TABLE agent_mirror_state (
    id integer PRIMARY KEY DEFAULT 1
        CONSTRAINT agent_mirror_state_singleton_chk CHECK (id = 1),

    last_request_at timestamptz,

    last_attempt_at      timestamptz,
    last_attempt_outcome text
        CONSTRAINT agent_mirror_state_attempt_outcome_chk
        CHECK (last_attempt_outcome IS NULL OR last_attempt_outcome IN (
            'mirrored', 'current', 'unchanged', 'rate_limited', 'refused',
            'foreign_channel', 'upstream_unavailable', 'storage_error',
            'not_configured'
        )),
    last_attempt_detail  text,
    last_attempt_trigger text
        CONSTRAINT agent_mirror_state_trigger_chk
        CHECK (last_attempt_trigger IS NULL OR last_attempt_trigger IN ('periodic', 'manual')),

    last_success_at      timestamptz,
    last_success_outcome text
        CONSTRAINT agent_mirror_state_success_outcome_chk
        CHECK (last_success_outcome IS NULL OR last_success_outcome IN ('mirrored', 'current', 'unchanged')),
    last_success_version text,

    last_mirrored_at      timestamptz,
    last_mirrored_version text,

    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Seed the single row so every write is a plain UPDATE ... WHERE id = 1.
INSERT INTO agent_mirror_state (id) VALUES (1) ON CONFLICT (id) DO NOTHING;
