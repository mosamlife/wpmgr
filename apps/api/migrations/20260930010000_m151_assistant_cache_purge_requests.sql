-- m151: assistant_cache_purge_requests, one AI connection's request to clear
-- one site's page cache, waiting for a person to approve it.
--
-- ===========================================================================
-- WHAT THIS TABLE IS
-- ===========================================================================
--
-- ADR-061 Decision 2, for its first action kind. An AI connection holding
-- `mcp.cache.purge` (m135, seated for OAuth by m150) may ASK to clear the page
-- cache on ONE site in its scope, either the whole site (scope 'all') or one
-- page address on it (scope 'url'). Asking writes one 'pending' row here and
-- changes nothing on the site. A person who may clear caches on that site
-- approves or declines that one row in WPMgr. Only an approved row is ever
-- sent, by a worker, after the approval commits. No automation approves a row,
-- and this table offers no state in which a machine is recorded as having
-- approved one.
--
-- It is m133's hardened shape (assistant_update_proposals), copied device by
-- device: the composite tenant/site foreign key, recorded facts with no
-- foreign key, a closed state set with no DEFAULT, a server-computed digest,
-- the window and "names a human" CHECKs, column-privilege immutability of the
-- facts, no DELETE or TRUNCATE for the application role, and a FOR SELECT
-- agent policy. m133's sections are the long-form argument for each device and
-- are not repeated here. What is NEW is listed below.
--
-- WHY A NEW TABLE AND NOT A WIDER m133. m133's facts are plugin-shaped and NOT
-- NULL (component_type, from_version, to_version), its DECISION 2 puts a
-- different blast radius in its own migration, and no Go writes m133 yet, so
-- there is nothing to share by widening it.
--
-- ===========================================================================
-- WHAT IS NEW RELATIVE TO m133
-- ===========================================================================
--
--   a. 'withdrawn'. The state a request takes when its connection is revoked
--      while it waits. It is not a decision and names nobody:
--      decided_by_user_id and decided_at are NULL, and withdrawn_at is set.
--      A revoke can be performed by an API key, which has no human to name, so
--      reusing 'rejected' (which must name a human, m133 DECISION 9) is not
--      possible and writing a placeholder id into it would be a lie.
--   b. THE ADDRESS. scope and url. url is present exactly when scope = 'url'.
--      url_backstop holds the stored value to a printable-ASCII http(s) URL of
--      at most 2048 bytes with no `?` or `#`. The Go creation path rebuilds the
--      stored value from the site row (scheme, host, port) plus the path the
--      AI supplied, and checks the same pattern before inserting; this CHECK
--      is the backstop no valid request reaches.
--   c. THE STORED FACTS THE DIGEST COVERS. site_label and grant_label are
--      sanitised in Go (humantext.Clean, then capped) before insert.
--      site_host is the dialled ASCII form of the SITE ROW's host
--      (siteaddr.HostKey output), computed for BOTH scopes; for an
--      internationalised host that is its Punycode form. site_host_ascii holds
--      it to 1..255 printable ASCII bytes and, because the column is NOT NULL,
--      cannot pass vacuously. The Go creation path checks the same pattern and
--      refuses a site whose address cannot be put in that form, so this CHECK
--      is a backstop no scope reaches. grant_via, setup_client and the digest
--      nonce are recorded beside them.
--   d. THE NONCE. digest_nonce is 32 server-random bytes, hex. The digest is
--      computed over the facts AND the nonce, so it is a value only the server
--      can produce. No route returns the nonce.
--   e. THE DISPATCH RECORD. claimed_at, cache_purge_audit_id, dispatch
--      attempts, the last transient reason, and the outcome with what the site
--      reported. 'dispatched' means "taken off the approved queue"; outcome
--      says what then happened, including 'not_sent'.
--   f. 'organisation_deleted' as a not-sent reason, for an organisation
--      deleted between approval and dispatch.
--
-- ===========================================================================
-- NOT EVERY TERMINAL STATE NAMES A HUMAN, AND THE APPROVER NAMED ON A ROW IS
-- NOT DURABLE
-- ===========================================================================
--
-- m133's rule is "every decided state names a human, and expiry, which is not
-- a decision, may name nobody". It holds here only because 'withdrawn', like
-- 'expired', is not a decision. Both require decided_by_user_id to be NULL.
--
-- This table has no transition guard (m133 section 6): the previous state of a
-- row constrains nothing about its next state. So the row is NOT the durable
-- record of who approved a clear, and nothing may be built on the assumption
-- that decided_by_user_id survives on a row. The durable record is the
-- hash-chained assistant.request.approved row in audit_log, written in the
-- same transaction as the approval. Do not read a guard into this table.
--
-- ===========================================================================
-- THE NULL SWEEP
-- ===========================================================================
--
-- Every CHECK that compares two booleans with `=` either has a NOT NULL left
-- side (state, scope) or builds both sides from IS NULL / IS NOT DISTINCT FROM,
-- so none can evaluate to NULL and pass by default. The nullable closed-set
-- columns (last_attempt_code, outcome, not_sent_reason, wpmgr_cdn) and the two
-- hosting-cache arrays admit NULL deliberately: NULL means "not reported yet"
-- or "not applicable", and the pairing CHECKs below decide when a value is
-- required.
--
-- ===========================================================================
-- cache_purge_audit AND THIS TABLE
-- ===========================================================================
--
-- A clear approved here is recorded in cache_purge_audit exactly once, by the
-- dispatch worker, in the same transaction that moves the row to 'dispatched';
-- cache_purge_audit_id then names that row. Creating or approving a request
-- never writes cache_purge_audit. cache_purge_audit carries the RESTRICTIVE
-- cache_purge_audit_site_scope policy (m132) as well as tenant isolation, and
-- that policy filters by site, not by initiator: see m150 DECISION 3.
--
-- ===========================================================================
-- THE LIFECYCLE LOCK: A TRY, NOT A WAIT
-- ===========================================================================
--
-- m133 (7)(d) says the dispatch worker takes org.LifecycleLockKey with the
-- blocking pg_advisory_xact_lock. The cache-clear worker deviates: it takes
-- the same key with pg_try_advisory_xact_lock, both arguments bound as text,
-- and treats `false` as a transient "an organisation change is in progress",
-- leaving the row approved for a later attempt. A worker slot is never parked
-- behind an organisation purge that holds the session-scoped lock for minutes.
-- The mutual exclusion is the same; only the waiting differs.

-- ===========================================================================
-- (1) THE TABLE
-- ===========================================================================

CREATE TABLE IF NOT EXISTS "public"."assistant_cache_purge_requests" (
    "id" uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Tenant and site bound as ONE fact (m133 DECISION 10). Deleting the site
    -- or the tenant deletes its requests; a request reserves no object storage
    -- and is not the record of a clear (cache_purge_audit and audit_log are),
    -- so nothing that must outlive the site dies with it.
    "tenant_id" uuid NOT NULL
        REFERENCES "public"."tenants" ("id") ON DELETE CASCADE,
    "site_id"   uuid NOT NULL,
    CONSTRAINT "assistant_cache_purge_requests_site_within_tenant_fkey"
        FOREIGN KEY ("tenant_id", "site_id")
        REFERENCES "public"."sites" ("tenant_id", "id") ON DELETE CASCADE,

    -- WHO ASKED. The mcp_grants row. No FK (m133 DECISION 7).
    "proposed_by_grant_id" uuid NOT NULL,

    -- WHAT IS ASKED.
    "scope" text NOT NULL
        CONSTRAINT "assistant_cache_purge_requests_scope_check"
        CHECK ("scope" IN ('all', 'url')),
    "url" text NULL
        CONSTRAINT "assistant_cache_purge_requests_url_backstop_check"
        CHECK ("url" IS NULL OR (
            length("url") <= 2048
            AND "url" ~ '^https?://[!-~]+$'
            AND "url" !~ '[?#]'
        )),
    CONSTRAINT "assistant_cache_purge_requests_url_matches_scope_check"
        CHECK (("scope" = 'url') = ("url" IS NOT NULL)),

    -- THE FACTS THE APPROVER IS SHOWN, and the digest covers.
    "site_label" text NOT NULL
        CONSTRAINT "assistant_cache_purge_requests_site_label_length_check"
        CHECK (char_length("site_label") <= 201),
    "site_host" text NOT NULL
        CONSTRAINT "assistant_cache_purge_requests_site_host_ascii_check"
        CHECK ("site_host" ~ '^[!-~]{1,255}$'),
    "grant_label" text NOT NULL
        CONSTRAINT "assistant_cache_purge_requests_grant_label_check"
        CHECK (length(btrim("grant_label")) > 0
               AND char_length("grant_label") <= 65),
    "grant_via" text NOT NULL
        CONSTRAINT "assistant_cache_purge_requests_grant_via_check"
        CHECK ("grant_via" IN ('token', 'browser_sign_in')),
    -- The operator's client choice, carried from the grant. Shape copied from
    -- mcp_grants_setup_client_shape_check (m128).
    "setup_client" text NULL
        CONSTRAINT "assistant_cache_purge_requests_setup_client_shape_check"
        CHECK ("setup_client" IS NULL
               OR ("setup_client" ~ '^[a-z0-9]+(-[a-z0-9]+)*$'
                   AND length("setup_client") <= 64)),

    -- THE NONCE AND THE FINGERPRINT. Both lowercase hex SHA-256 width.
    "digest_nonce" text NOT NULL
        CONSTRAINT "assistant_cache_purge_requests_digest_nonce_shape_check"
        CHECK ("digest_nonce" ~ '^[0-9a-f]{64}$'),
    "presented_digest" text NOT NULL
        CONSTRAINT "assistant_cache_purge_requests_presented_digest_shape_check"
        CHECK ("presented_digest" ~ '^[0-9a-f]{64}$'),

    -- THE STATE MACHINE. Closed, NOT NULL, NO DEFAULT.
    "state" text NOT NULL
        CONSTRAINT "assistant_cache_purge_requests_state_check"
        CHECK ("state" IN (
            'pending',
            'approved_undispatched',
            'dispatched',
            'rejected',
            'withdrawn',
            'expired'
        )),

    "created_at" timestamptz NOT NULL DEFAULT now(),

    -- THE WINDOW. NOT NULL, NO DEFAULT.
    "expires_at" timestamptz NOT NULL,
    CONSTRAINT "assistant_cache_purge_requests_window_is_positive_check"
        CHECK ("expires_at" > "created_at"),

    -- THE DECISION. The decider is a recorded fact, not a foreign key.
    "decided_at" timestamptz NULL,
    "decided_by_user_id" uuid NULL,

    CONSTRAINT "assistant_cache_purge_requests_decided_states_have_time_check"
        CHECK (
            ("state" IN ('approved_undispatched', 'dispatched', 'rejected'))
            = ("decided_at" IS NOT NULL)
        ),
    CONSTRAINT "assistant_cache_purge_requests_consent_within_window_check"
        CHECK (
            "state" NOT IN ('approved_undispatched', 'dispatched')
            OR ("decided_at" IS NOT NULL AND "decided_at" < "expires_at")
        ),
    CONSTRAINT "assistant_cache_purge_requests_expiry_is_not_a_decision_check"
        CHECK ("state" <> 'expired' OR "decided_by_user_id" IS NULL),
    CONSTRAINT "assistant_cache_purge_requests_approval_names_a_human_check"
        CHECK (
            "state" NOT IN ('approved_undispatched', 'dispatched')
            OR "decided_by_user_id" IS NOT NULL
        ),
    CONSTRAINT "assistant_cache_purge_requests_rejection_names_a_human_check"
        CHECK ("state" <> 'rejected' OR "decided_by_user_id" IS NOT NULL),

    -- WITHDRAWAL. When the connection was revoked while the row waited. It is
    -- not a decision: it has its own time and names nobody.
    "withdrawn_at" timestamptz NULL,
    CONSTRAINT "assistant_cache_purge_requests_withdrawn_has_time_check"
        CHECK (("state" = 'withdrawn') = ("withdrawn_at" IS NOT NULL)),
    CONSTRAINT "assistant_cache_purge_requests_withdrawal_names_nobody_check"
        CHECK ("state" <> 'withdrawn' OR "decided_by_user_id" IS NULL),

    -- THE CLAIM. Set in the statement that moves the row to 'dispatched'.
    "claimed_at" timestamptz NULL,
    CONSTRAINT "assistant_cache_purge_requests_claimed_iff_dispatched_check"
        CHECK (("state" = 'dispatched') = ("claimed_at" IS NOT NULL)),

    -- THE cache_purge_audit ROW the reservation wrote. No FK: a recorded fact.
    "cache_purge_audit_id" uuid NULL,
    CONSTRAINT "assistant_cache_purge_requests_audit_row_when_dispatched_check"
        CHECK ("cache_purge_audit_id" IS NULL OR "state" = 'dispatched'),

    -- TRANSIENT DISPATCH ATTEMPTS. The row stays approved while these move.
    "dispatch_attempts" integer NOT NULL DEFAULT 0
        CONSTRAINT "assistant_cache_purge_requests_dispatch_attempts_check"
        CHECK ("dispatch_attempts" >= 0),
    "last_attempt_at" timestamptz NULL,
    "last_attempt_code" text NULL
        CONSTRAINT "assistant_cache_purge_requests_last_attempt_code_check"
        CHECK ("last_attempt_code" IN (
            'site_unreachable',
            'site_cooldown',
            'site_hourly_cap',
            'site_busy',
            'org_busy',
            'context_unavailable',
            'write_tools_disabled'
        )),

    -- THE OUTCOME. Only a dispatched row has one.
    "outcome" text NULL
        CONSTRAINT "assistant_cache_purge_requests_outcome_check"
        CHECK ("outcome" IN (
            'purged',
            'site_reported_failure',
            'agent_failed',
            'outcome_unknown',
            'not_sent'
        )),
    CONSTRAINT "assistant_cache_purge_requests_outcome_when_dispatched_check"
        CHECK ("outcome" IS NULL OR "state" = 'dispatched'),
    "not_sent_reason" text NULL
        CONSTRAINT "assistant_cache_purge_requests_not_sent_reason_check"
        CHECK ("not_sent_reason" IN (
            'grant_inactive',
            'assistant_paused',
            'organisation_deleted',
            'capability_not_held',
            'site_absent',
            'forbidden_by_context',
            'agent_outdated',
            'dispatch_deadline_passed',
            'transport_pre_send'
        )),
    -- A not-sent outcome has a reason, and only a not-sent outcome has one.
    -- IS NOT DISTINCT FROM, not `=`: with outcome NULL, `outcome = 'not_sent'`
    -- is NULL and the whole CHECK would pass with a reason and no outcome.
    CONSTRAINT "assistant_cache_purge_requests_not_sent_has_reason_check"
        CHECK (("outcome" IS NOT DISTINCT FROM 'not_sent')
               = ("not_sent_reason" IS NOT NULL)),
    -- Anything that was sent names the cache_purge_audit row of its attempt.
    CONSTRAINT "assistant_cache_purge_requests_sent_names_audit_row_check"
        CHECK ("outcome" IS NULL OR "outcome" = 'not_sent'
               OR "cache_purge_audit_id" IS NOT NULL),
    "outcome_at" timestamptz NULL,
    CONSTRAINT "assistant_cache_purge_requests_outcome_has_time_check"
        CHECK (("outcome" IS NULL) = ("outcome_at" IS NULL)),

    -- WHAT THE SITE REPORTED. Hosting-cache identifiers are a closed set of
    -- twelve, one per hosting integration the agent ships (its
    -- includes/integrations/class-<slug>.php basenames). KEEP IN LOCKSTEP with
    -- the Go set that filters the agent's report. NULL means not reported.
    "hosting_caches_cleared" text[] NULL
        CONSTRAINT "assistant_cache_purge_requests_hosting_cleared_check"
        CHECK ("hosting_caches_cleared" <@ ARRAY[
            'cloudflare', 'cloudpanel', 'cloudways', 'gridpane', 'kinsta',
            'rocketnet', 'runcloud', 'siteground', 'spinupwp', 'varnish',
            'wpcloud', 'wpengine'
        ]::text[]),
    "hosting_caches_skipped" text[] NULL
        CONSTRAINT "assistant_cache_purge_requests_hosting_skipped_check"
        CHECK ("hosting_caches_skipped" <@ ARRAY[
            'cloudflare', 'cloudpanel', 'cloudways', 'gridpane', 'kinsta',
            'rocketnet', 'runcloud', 'siteground', 'spinupwp', 'varnish',
            'wpcloud', 'wpengine'
        ]::text[]),
    "origin_only_confirmed" boolean NULL,
    "wpmgr_cdn" text NULL
        CONSTRAINT "assistant_cache_purge_requests_wpmgr_cdn_check"
        CHECK ("wpmgr_cdn" IN ('not_attempted', 'cleared', 'failed', 'not_configured')),
    -- Sanitised in Go before insert, rendered as text to a person, and never
    -- returned to the AI client.
    "site_reported_text" text NULL
        CONSTRAINT "assistant_cache_purge_requests_site_reported_text_check"
        CHECK ("site_reported_text" IS NULL OR length("site_reported_text") <= 512)
);

-- ===========================================================================
-- (2) INDEXES
-- ===========================================================================

CREATE INDEX IF NOT EXISTS "assistant_cache_purge_requests_tenant_idx"
    ON "public"."assistant_cache_purge_requests" ("tenant_id");

CREATE INDEX IF NOT EXISTS "assistant_cache_purge_requests_site_idx"
    ON "public"."assistant_cache_purge_requests" ("site_id");

-- The dispatch scan and the sweeper's one-hour deadline pass. Reserving or
-- closing a row moves it out of this index.
CREATE INDEX IF NOT EXISTS "assistant_cache_purge_requests_dispatch_idx"
    ON "public"."assistant_cache_purge_requests" ("decided_at")
    WHERE "state" = 'approved_undispatched';

-- The expiry sweep, over the only state that can expire.
CREATE INDEX IF NOT EXISTS "assistant_cache_purge_requests_expiry_sweep_idx"
    ON "public"."assistant_cache_purge_requests" ("expires_at")
    WHERE "state" = 'pending';

-- AT MOST ONE WAITING REQUEST PER CONNECTION PER SITE. Per connection, not per
-- site: one connection's waiting request never blocks another connection's.
-- Partial on 'pending', so a decided, withdrawn or expired row blocks nothing.
-- The creation path's ON CONFLICT names exactly these columns and predicate.
CREATE UNIQUE INDEX IF NOT EXISTS "assistant_cache_purge_requests_one_pending_idx"
    ON "public"."assistant_cache_purge_requests"
       ("tenant_id", "site_id", "proposed_by_grant_id")
    WHERE "state" = 'pending';

-- The per-connection caps (waiting count, daily count) and the revoke cascade.
CREATE INDEX IF NOT EXISTS "assistant_cache_purge_requests_grant_created_idx"
    ON "public"."assistant_cache_purge_requests"
       ("proposed_by_grant_id", "created_at" DESC);

-- In flight: dispatched and no outcome yet. The per-site busy check and the
-- reconciler read this.
CREATE INDEX IF NOT EXISTS "assistant_cache_purge_requests_in_flight_idx"
    ON "public"."assistant_cache_purge_requests" ("site_id", "claimed_at")
    WHERE "state" = 'dispatched' AND "outcome" IS NULL;

-- ===========================================================================
-- (3) ROW LEVEL SECURITY
-- ===========================================================================
--
-- ENABLE and FORCE. Three policies: permissive tenant isolation, the
-- RESTRICTIVE site scope every site-keyed sibling carries (the m19 predicate,
-- byte for byte as m132 and m133 apply it), and a FOR SELECT agent policy.

ALTER TABLE "public"."assistant_cache_purge_requests" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."assistant_cache_purge_requests" FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'assistant_cache_purge_requests'
          AND policyname = 'assistant_cache_purge_requests_tenant_isolation'
    ) THEN
        CREATE POLICY "assistant_cache_purge_requests_tenant_isolation"
            ON "public"."assistant_cache_purge_requests"
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'assistant_cache_purge_requests'
          AND policyname = 'assistant_cache_purge_requests_site_scope'
    ) THEN
        CREATE POLICY "assistant_cache_purge_requests_site_scope"
            ON "public"."assistant_cache_purge_requests"
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
    END IF;
END;
$$;

-- The cross-tenant service context (pool.InAgentTx). It admits the three
-- SCANS -- dispatch, expiry sweep, reconciler -- each a plain SELECT returning
-- ids and tenants. Every write runs under a tenant transaction, where tenant
-- isolation admits it. FOR SELECT IS LOAD-BEARING: FOR ALL would put the
-- decision columns of every tenant's requests in reach of the agent context,
-- which also serves requests a customer's plugin drives. See this table's row
-- in db/rls-cross-tenant-policies.txt.
--
-- The scans must stay plain SELECTs. A locking read (FOR UPDATE / SKIP LOCKED)
-- under this policy returns zero rows with no error, because PostgreSQL
-- applies the UPDATE policy to a locking read and none admits app.agent.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'assistant_cache_purge_requests'
          AND policyname = 'assistant_cache_purge_requests_agent'
    ) THEN
        CREATE POLICY "assistant_cache_purge_requests_agent"
            ON "public"."assistant_cache_purge_requests"
            FOR SELECT
            USING (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;

-- ===========================================================================
-- (4) PRIVILEGES: THE FACTS ARE IMMUTABLE, AND THE ROW CANNOT BE DELETED
-- ===========================================================================
--
-- m133 section (5), applied here. SELECT and INSERT are granted explicitly,
-- as m147 does, so the table is usable whatever default privileges the
-- migrating role carries. DELETE and TRUNCATE are revoked: a request's outcome
-- is a state, never an absence, and rows leave only by the tenant or site
-- cascade. UPDATE is revoked at table level FIRST, then granted back on
-- exactly the columns the workflow moves; a table-level UPDATE covers every
-- column and PostgreSQL will not carve one out of it.
--
-- Immutable after insert: id, tenant_id, site_id, proposed_by_grant_id, scope,
-- url, site_label, site_host, grant_label, grant_via, setup_client,
-- digest_nonce, presented_digest, created_at, expires_at.
--
-- apps/api/tests/rls_integration_test.go re-applies these after its blanket
-- test grant, so the integration proofs run against the privileges a real
-- install has.

GRANT SELECT, INSERT ON "public"."assistant_cache_purge_requests" TO "wpmgr_app";

REVOKE DELETE, TRUNCATE ON "public"."assistant_cache_purge_requests" FROM "wpmgr_app";

REVOKE UPDATE ON "public"."assistant_cache_purge_requests" FROM "wpmgr_app";

GRANT UPDATE (
    "state", "decided_at", "decided_by_user_id", "withdrawn_at",
    "claimed_at", "cache_purge_audit_id",
    "dispatch_attempts", "last_attempt_at", "last_attempt_code",
    "outcome", "not_sent_reason", "outcome_at",
    "hosting_caches_cleared", "hosting_caches_skipped",
    "origin_only_confirmed", "wpmgr_cdn", "site_reported_text"
) ON "public"."assistant_cache_purge_requests" TO "wpmgr_app";
