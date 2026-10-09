-- m156: assistant_ability_requests, one AI connection's request to run one
-- approved-write catalogue ability on one site, waiting for a person to
-- approve it. Kind-generic: every write entry in ability_catalogue (m155)
-- uses this one table; wpmgr/page-create is the first (engine slice E2).
--
-- ===========================================================================
-- WHAT THIS TABLE IS
-- ===========================================================================
--
-- site_ability_run on a write entry runs the precheck synchronously, then
-- writes one 'pending' row here and changes nothing on the site. A person who
-- holds the entry's operator_permission on that site approves or declines
-- that one row. Only an approved row is ever sent, by a worker, after the
-- approval commits. No automation approves a row.
--
-- It is m151's shape (assistant_cache_purge_requests), copied device by
-- device: the composite tenant/site foreign key, recorded facts with no
-- foreign key, a closed state set with no DEFAULT, a server-computed
-- presented_digest over the stored facts and a server-random nonce, the
-- window and "names a human" CHECKs, column-privilege immutability of the
-- facts, no DELETE or TRUNCATE for the application role, a one-pending
-- unique index the creation path's ON CONFLICT names, ENABLE + FORCE row
-- security with tenant isolation, the RESTRICTIVE site scope, and a FOR
-- SELECT agent policy for the cross-tenant scans. m151 and m133 carry the
-- long-form argument for each device and it is not repeated here.
--
-- ===========================================================================
-- WHAT IS NEW RELATIVE TO m151
-- ===========================================================================
--
--   a. THE ENTRY APPROVED (amendment W1). entry_id names the ability_catalogue
--      row and entry_sha256 the exact entry bytes the precheck ran against.
--      Both are recorded facts, immutable after insert. The worker compares
--      the catalogue's current entry_sha256 with this one and closes the row
--      not_sent / entry_changed on a mismatch; the reservation statement also
--      refuses a row whose entry hash moved or whose entry is disabled.
--   b. operator_permission (W1). The permission the approver must hold,
--      copied from the entry at creation, so an approval is checked against
--      what the card was built for even if the entry later changes.
--   c. THE EXACT INPUT (amendment R2). input_json is the exact JSON text the
--      agent will receive as `p`, stored as text, never jsonb (jsonb would
--      re-order keys and drop duplicates, and the bytes are what the token's
--      `pd` claim covers). input_sha256 is held to sha256 of those UTF-8 bytes
--      by a CHECK, so the two cannot disagree. 64 KiB cap (engine v4 §4.2).
--   d. THE PRECHECK RESULT. precheck_digest, preview_digest (NULL for an
--      entry with no preview) and base_fingerprint, all bound into the digest
--      and sent back to the agent as `expected`.
--   e. THE CARD FACTS. title_excerpt, editor, post_type, effect_copy,
--      snapshot, card_copy_version. Sanitised and capped in Go.
--   f. STATE CARRIES THE RESULT CLASS. m151 parks every sent row in
--      'dispatched' and puts the result in outcome. Here the result class is
--      the state ('done', 'failed', 'not_sent', 'outcome_unknown') and
--      outcome holds the specific result, paired by one CHECK. The engine
--      resolves an unknown outcome later through the site ledger, which m151
--      never does, so 'outcome_unknown' is a state a row WAITS in (outcome
--      NULL, unknown_since set) before it is resolved or given up on.
--   g. THE DISPATCH DEADLINE is a stored column, dispatch_deadline_at, set
--      by the approving statement from the database clock.
--   h. THE LEDGER REFERENCES. created_post_id, restored, trashed, and the
--      person's undo (undo_state and its times). W3: the agent takes the
--      object id for a revert ONLY from its own ledger row for this request
--      id. created_post_id here is a display and status fact, never the id a
--      revert acts on.
--
-- ===========================================================================
-- id IS THE request_id
-- ===========================================================================
--
-- There is no separate request_id column. id is the value returned to the AI
-- as request_id, sent to the agent as request_id, and keyed by the site
-- ledger, exactly as m151's id is. A second uuid would be a second name for
-- one fact and a place for the two to disagree.
--
-- ===========================================================================
-- THE ONE-PENDING INDEX, AND THE TARGET OF A CREATION
-- ===========================================================================
--
-- At most one waiting request per (tenant, site, connection, ability,
-- target), partial on 'pending' as m151's is, so a decided, withdrawn or
-- expired row blocks nothing. target_key is a GENERATED column, so no caller
-- can write a value that disagrees with the row:
--
--   * a post-targeted entry (target_post_id set): 'post:<id>'. One waiting
--     edit per connection per post per ability (engine v4 §1.4 step 8).
--   * a creation (target_post_id NULL): 'new:<input_sha256>'. A creation has
--     no existing object, and the object it would create is identified by
--     the exact input that describes it. So an identical retry from the same
--     connection on the same site dedupes to the waiting row (Track A dedupe
--     semantics: the AI gets the same request_id back), while a different
--     page is a different request. How many different creations may wait is
--     the Go cap of 5 pending creations per connection per site (content v3
--     §3.8.1 and §4.5: "creation is limited instead by the cap"), which a
--     unique index cannot express.
--
-- Per connection, not per site: one connection's waiting request never
-- blocks another's. Two connections asking to edit one post are serialised
-- downstream, by the per-site dispatch reservation and the agent's per-post
-- atomic claim (Sec-F3), not here.
--
-- ===========================================================================
-- NOT EVERY TERMINAL STATE NAMES A HUMAN, AND THE ROW IS NOT THE DURABLE
-- RECORD OF WHO APPROVED
-- ===========================================================================
--
-- As m151: 'expired' and 'withdrawn' are not decisions and name nobody; no
-- transition guard exists, so the hash-chained audit_log row written with
-- the approval is the durable record of who approved, not this row.
--
-- ===========================================================================
-- THE NULL SWEEP
-- ===========================================================================
--
-- Every CHECK that compares two booleans with `=` builds both sides from
-- NOT NULL columns or IS [NOT] NULL / IS [NOT] DISTINCT FROM. The one CASE
-- (state_matches_outcome) is wrapped in coalesce(..., false), because
-- `outcome IN (...)` is NULL when outcome is NULL and a NULL CHECK passes.
--
-- ===========================================================================
-- THE CASCADE
-- ===========================================================================
--
-- Deleting the site or the tenant deletes its requests, as m151. A request
-- reserves no object storage, and it is not the record of what was done:
-- audit_log is, and the site's own ledger holds the retained copy and the
-- created object. Nothing that must outlive the site dies with it.
--
-- ===========================================================================
-- THE LIFECYCLE LOCK
-- ===========================================================================
--
-- The reservation takes org.LifecycleLockKey for the tenant with
-- pg_try_advisory_xact_lock (TryAssistantRequestXactLock, both arguments as
-- text), treating false as the transient org_busy, exactly as m151's worker.

-- ===========================================================================
-- (1) THE TABLE
-- ===========================================================================

CREATE TABLE IF NOT EXISTS "public"."assistant_ability_requests" (
    "id" uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    "tenant_id" uuid NOT NULL
        REFERENCES "public"."tenants" ("id") ON DELETE CASCADE,
    "site_id"   uuid NOT NULL,
    CONSTRAINT "assistant_ability_requests_site_within_tenant_fkey"
        FOREIGN KEY ("tenant_id", "site_id")
        REFERENCES "public"."sites" ("tenant_id", "id") ON DELETE CASCADE,

    -- WHO ASKED. The mcp_grants row. No FK (m133 DECISION 7).
    "proposed_by_grant_id" uuid NOT NULL,

    -- THE ENTRY APPROVED (W1). Recorded facts, no FK: the catalogue row may
    -- change after this request is created, and this row must keep naming
    -- what the card was built for.
    "entry_id" uuid NOT NULL,
    "entry_sha256" text NOT NULL
        CONSTRAINT "assistant_ability_requests_entry_sha256_shape_check"
        CHECK ("entry_sha256" ~ '^[0-9a-f]{64}$'),
    "ability_name" text NOT NULL
        CONSTRAINT "assistant_ability_requests_ability_name_check"
        CHECK ("ability_name" ~ '^[a-z0-9-]{1,64}/[a-z0-9-]{1,64}$'),
    -- Same shape as ability_catalogue_operator_permission_check (m155).
    "operator_permission" text NOT NULL
        CONSTRAINT "assistant_ability_requests_operator_permission_check"
        CHECK ("operator_permission" ~ '^[a-z]+(\.[a-z_]+){1,3}$'),

    -- THE EXACT INPUT (R2). Text, never jsonb. The hash is held to the bytes.
    "input_json" text NOT NULL
        CONSTRAINT "assistant_ability_requests_input_json_size_check"
        CHECK (octet_length("input_json") BETWEEN 2 AND 65536),
    "input_sha256" text NOT NULL
        CONSTRAINT "assistant_ability_requests_input_sha256_shape_check"
        CHECK ("input_sha256" ~ '^[0-9a-f]{64}$'),
    CONSTRAINT "assistant_ability_requests_input_sha256_matches_check"
        CHECK ("input_sha256" = encode(sha256(convert_to("input_json", 'UTF8')), 'hex')),

    -- THE TARGET. NULL for a creation. target_key is derived, never written.
    "target_post_id" bigint NULL
        CONSTRAINT "assistant_ability_requests_target_post_id_check"
        CHECK ("target_post_id" > 0),
    "target_key" text GENERATED ALWAYS AS (
        CASE WHEN "target_post_id" IS NULL
             THEN 'new:' || "input_sha256"
             ELSE 'post:' || "target_post_id"::text
        END
    ) STORED,

    -- THE PRECHECK RESULT.
    "precheck_digest" text NOT NULL
        CONSTRAINT "assistant_ability_requests_precheck_digest_shape_check"
        CHECK ("precheck_digest" ~ '^[0-9a-f]{64}$'),
    "preview_digest" text NULL
        CONSTRAINT "assistant_ability_requests_preview_digest_shape_check"
        CHECK ("preview_digest" ~ '^[0-9a-f]{64}$'),
    "base_fingerprint" text NOT NULL
        CONSTRAINT "assistant_ability_requests_base_fingerprint_shape_check"
        CHECK ("base_fingerprint" ~ '^[!-~]+$' AND octet_length("base_fingerprint") <= 256),

    -- THE CARD FACTS, which the digest covers.
    "site_label" text NOT NULL
        CONSTRAINT "assistant_ability_requests_site_label_length_check"
        CHECK (char_length("site_label") <= 201),
    "site_host" text NOT NULL
        CONSTRAINT "assistant_ability_requests_site_host_ascii_check"
        CHECK ("site_host" ~ '^[!-~]{1,255}$'),
    "grant_label" text NOT NULL
        CONSTRAINT "assistant_ability_requests_grant_label_check"
        CHECK (length(btrim("grant_label")) > 0
               AND char_length("grant_label") <= 65),
    "grant_via" text NOT NULL
        CONSTRAINT "assistant_ability_requests_grant_via_check"
        CHECK ("grant_via" IN ('token', 'browser_sign_in')),
    "setup_client" text NULL
        CONSTRAINT "assistant_ability_requests_setup_client_shape_check"
        CHECK ("setup_client" IS NULL
               OR ("setup_client" ~ '^[a-z0-9]+(-[a-z0-9]+)*$'
                   AND length("setup_client") <= 64)),
    -- The page title for a creation, the resolved target title for an edit.
    "title_excerpt" text NULL
        CONSTRAINT "assistant_ability_requests_title_excerpt_length_check"
        CHECK (char_length("title_excerpt") <= 201),
    -- The editor or route the precheck resolved (content v3 §3.8.2).
    "editor" text NULL
        CONSTRAINT "assistant_ability_requests_editor_check"
        CHECK ("editor" ~ '^(wordpress_blocks|wordpress_classic|builder:[a-z0-9_-]{1,64})$'),
    -- A WordPress post type key (at most 20 characters in core).
    "post_type" text NULL
        CONSTRAINT "assistant_ability_requests_post_type_check"
        CHECK ("post_type" ~ '^[a-z0-9_-]{1,20}$'),
    -- The entry's effect and undo strategy as shown. Same closed sets as
    -- ability_catalogue_effect_copy_check and _snapshot_check, minus the
    -- snapshot 'none' no write entry may carry (m155, engine v4 §4.4).
    "effect_copy" text NOT NULL
        CONSTRAINT "assistant_ability_requests_effect_copy_check"
        CHECK ("effect_copy" IN ('draft', 'live', 'none')),
    "snapshot" text NOT NULL
        CONSTRAINT "assistant_ability_requests_snapshot_check"
        CHECK ("snapshot" IN (
            'created_post_trash', 'wp_revision', 'vendor_draft_discard',
            'vendor_tree_rewrite', 'post_fields', 'own_attachment_delete',
            'menu_items', 'option_values'
        )),
    "card_copy_version" integer NOT NULL
        CONSTRAINT "assistant_ability_requests_card_copy_version_check"
        CHECK ("card_copy_version" > 0),

    -- THE NONCE AND THE FINGERPRINT.
    "digest_nonce" text NOT NULL
        CONSTRAINT "assistant_ability_requests_digest_nonce_shape_check"
        CHECK ("digest_nonce" ~ '^[0-9a-f]{64}$'),
    "presented_digest" text NOT NULL
        CONSTRAINT "assistant_ability_requests_presented_digest_shape_check"
        CHECK ("presented_digest" ~ '^[0-9a-f]{64}$'),

    -- THE STATE MACHINE. Closed, NOT NULL, NO DEFAULT.
    "state" text NOT NULL
        CONSTRAINT "assistant_ability_requests_state_check"
        CHECK ("state" IN (
            'pending',
            'approved',
            'declined',
            'expired',
            'withdrawn',
            'dispatched',
            'done',
            'failed',
            'not_sent',
            'outcome_unknown'
        )),

    "created_at" timestamptz NOT NULL DEFAULT now(),

    "expires_at" timestamptz NOT NULL,
    CONSTRAINT "assistant_ability_requests_window_is_positive_check"
        CHECK ("expires_at" > "created_at"),

    -- THE DECISION.
    "decided_at" timestamptz NULL,
    "decided_by_user_id" uuid NULL,
    CONSTRAINT "assistant_ability_requests_decided_states_have_time_check"
        CHECK (
            ("state" IN ('approved', 'declined', 'dispatched', 'done', 'failed',
                         'not_sent', 'outcome_unknown'))
            = ("decided_at" IS NOT NULL)
        ),
    CONSTRAINT "assistant_ability_requests_consent_within_window_check"
        CHECK (
            "state" NOT IN ('approved', 'dispatched', 'done', 'failed',
                            'not_sent', 'outcome_unknown')
            OR ("decided_at" IS NOT NULL AND "decided_at" < "expires_at")
        ),
    CONSTRAINT "assistant_ability_requests_approval_names_a_human_check"
        CHECK (
            "state" NOT IN ('approved', 'dispatched', 'done', 'failed',
                            'not_sent', 'outcome_unknown')
            OR "decided_by_user_id" IS NOT NULL
        ),
    CONSTRAINT "assistant_ability_requests_decline_names_a_human_check"
        CHECK ("state" <> 'declined' OR "decided_by_user_id" IS NOT NULL),
    CONSTRAINT "assistant_ability_requests_expiry_is_not_a_decision_check"
        CHECK ("state" <> 'expired' OR "decided_by_user_id" IS NULL),

    -- WITHDRAWAL. The connection was revoked while the row waited.
    "withdrawn_at" timestamptz NULL,
    CONSTRAINT "assistant_ability_requests_withdrawn_has_time_check"
        CHECK (("state" = 'withdrawn') = ("withdrawn_at" IS NOT NULL)),
    CONSTRAINT "assistant_ability_requests_withdrawal_names_nobody_check"
        CHECK ("state" <> 'withdrawn' OR "decided_by_user_id" IS NULL),

    -- THE DISPATCH DEADLINE. Set by the approving statement, and present on
    -- exactly the rows an approval produced.
    "dispatch_deadline_at" timestamptz NULL,
    CONSTRAINT "assistant_ability_requests_deadline_iff_approved_check"
        CHECK (
            ("state" IN ('approved', 'dispatched', 'done', 'failed',
                         'not_sent', 'outcome_unknown'))
            = ("dispatch_deadline_at" IS NOT NULL)
        ),
    CONSTRAINT "assistant_ability_requests_deadline_after_decision_check"
        CHECK ("dispatch_deadline_at" IS NULL OR "dispatch_deadline_at" > "decided_at"),

    -- THE CLAIM. Set by the reservation. Every sent state has one; no state
    -- before the reservation does. 'not_sent' may have one (closed after the
    -- reservation, transport_pre_send) or not (closed before it).
    "claimed_at" timestamptz NULL,
    CONSTRAINT "assistant_ability_requests_sent_states_claimed_check"
        CHECK ("state" NOT IN ('dispatched', 'outcome_unknown', 'done', 'failed')
               OR "claimed_at" IS NOT NULL),
    CONSTRAINT "assistant_ability_requests_unsent_states_unclaimed_check"
        CHECK ("state" NOT IN ('pending', 'approved', 'declined', 'expired', 'withdrawn')
               OR "claimed_at" IS NULL),

    -- TRANSIENT DISPATCH ATTEMPTS. The row stays approved while these move.
    "dispatch_attempts" integer NOT NULL DEFAULT 0
        CONSTRAINT "assistant_ability_requests_dispatch_attempts_check"
        CHECK ("dispatch_attempts" >= 0),
    "last_attempt_at" timestamptz NULL,
    "last_attempt_code" text NULL
        CONSTRAINT "assistant_ability_requests_last_attempt_code_check"
        CHECK ("last_attempt_code" IN (
            'site_unreachable',
            'site_cooldown',
            'site_hourly_cap',
            'site_busy',
            'org_busy',
            'context_unavailable',
            'write_tools_disabled'
        )),

    -- AN UNKNOWN OUTCOME BEING RESOLVED THROUGH THE SITE LEDGER (engine v4
    -- §2.7). unknown_since is when the row entered 'outcome_unknown'; it is
    -- kept when the ledger later resolves the row to done or failed.
    "unknown_since" timestamptz NULL,
    CONSTRAINT "assistant_ability_requests_unknown_has_time_check"
        CHECK ("state" <> 'outcome_unknown' OR "unknown_since" IS NOT NULL),
    CONSTRAINT "assistant_ability_requests_unknown_since_states_check"
        CHECK ("unknown_since" IS NULL
               OR "state" IN ('outcome_unknown', 'done', 'failed')),
    "ledger_checked_at" timestamptz NULL,
    CONSTRAINT "assistant_ability_requests_ledger_checked_when_unknown_check"
        CHECK ("ledger_checked_at" IS NULL OR "unknown_since" IS NOT NULL),

    -- THE OUTCOME. engine v4 §1.5's closed set.
    "outcome" text NULL
        CONSTRAINT "assistant_ability_requests_outcome_check"
        CHECK ("outcome" IN (
            'created',
            'applied',
            'refused',
            'verify_mismatch',
            'failed',
            'outcome_unknown',
            'not_sent'
        )),
    -- The state is the result class; outcome is the specific result. An
    -- 'outcome_unknown' row is either still being resolved (outcome NULL) or
    -- given up on (outcome 'outcome_unknown'). Wrapped in coalesce: see THE
    -- NULL SWEEP.
    CONSTRAINT "assistant_ability_requests_state_matches_outcome_check"
        CHECK (coalesce(
            CASE "state"
                WHEN 'done'            THEN "outcome" IN ('created', 'applied')
                WHEN 'failed'          THEN "outcome" IN ('refused', 'verify_mismatch', 'failed')
                WHEN 'not_sent'        THEN "outcome" = 'not_sent'
                WHEN 'outcome_unknown' THEN "outcome" IS NULL OR "outcome" = 'outcome_unknown'
                ELSE "outcome" IS NULL
            END, false)),
    "outcome_at" timestamptz NULL,
    CONSTRAINT "assistant_ability_requests_outcome_has_time_check"
        CHECK (("outcome" IS NULL) = ("outcome_at" IS NULL)),
    -- The agent's closed result code (conflict, preview_changed,
    -- snapshot_failed, side_effect_detected, ...). Shape only: the vocabulary
    -- belongs to the agent and grows per entry; Go maps it onto its own closed
    -- set before insert and refuses anything else.
    "outcome_code" text NULL
        CONSTRAINT "assistant_ability_requests_outcome_code_shape_check"
        CHECK ("outcome_code" ~ '^[a-z][a-z0-9_]{0,63}$'),
    CONSTRAINT "assistant_ability_requests_outcome_code_with_outcome_check"
        CHECK ("outcome_code" IS NULL OR "outcome" IS NOT NULL),
    "not_sent_reason" text NULL
        CONSTRAINT "assistant_ability_requests_not_sent_reason_check"
        CHECK ("not_sent_reason" IN (
            'grant_inactive',
            'assistant_paused',
            'organisation_deleted',
            'capability_not_held',
            'site_absent',
            'forbidden_by_context',
            'agent_outdated',
            'dispatch_deadline_passed',
            'transport_pre_send',
            'entry_changed',
            'entry_disabled'
        )),
    CONSTRAINT "assistant_ability_requests_not_sent_has_reason_check"
        CHECK (("outcome" IS NOT DISTINCT FROM 'not_sent')
               = ("not_sent_reason" IS NOT NULL)),

    -- THE LEDGER RESULT REFERENCES. Display and status facts; the agent acts
    -- only on its own ledger row (W3).
    "created_post_id" bigint NULL
        CONSTRAINT "assistant_ability_requests_created_post_id_check"
        CHECK ("created_post_id" > 0),
    CONSTRAINT "assistant_ability_requests_created_names_post_check"
        CHECK ("outcome" IS DISTINCT FROM 'created' OR "created_post_id" IS NOT NULL),
    "restored" boolean NULL,
    "trashed" boolean NULL,
    CONSTRAINT "assistant_ability_requests_ledger_refs_when_sent_check"
        CHECK (("created_post_id" IS NULL AND "restored" IS NULL AND "trashed" IS NULL)
               OR "state" IN ('done', 'failed', 'outcome_unknown')),
    -- Sanitised in Go before insert, rendered as text to a person.
    "site_reported_text" text NULL
        CONSTRAINT "assistant_ability_requests_site_reported_text_check"
        CHECK ("site_reported_text" IS NULL OR length("site_reported_text") <= 512),

    -- A PERSON'S UNDO of a done request (content v3 §3.8.4, §5).
    "undo_state" text NULL
        CONSTRAINT "assistant_ability_requests_undo_state_check"
        CHECK ("undo_state" IN (
            'available',
            'in_progress',
            'undone',
            'refused_conflict',
            'refused_published',
            'failed'
        )),
    CONSTRAINT "assistant_ability_requests_undo_only_when_done_check"
        CHECK ("undo_state" IS NULL OR "state" = 'done'),
    "undo_available_until" timestamptz NULL,
    CONSTRAINT "assistant_ability_requests_undo_has_window_check"
        CHECK (("undo_state" IS NULL) = ("undo_available_until" IS NULL)),
    "undo_by_user_id" uuid NULL,
    CONSTRAINT "assistant_ability_requests_undo_names_a_human_check"
        CHECK ((coalesce("undo_state", 'available') <> 'available')
               = ("undo_by_user_id" IS NOT NULL)),
    "undo_started_at" timestamptz NULL,
    CONSTRAINT "assistant_ability_requests_undo_started_has_time_check"
        CHECK ((coalesce("undo_state", 'available') <> 'available')
               = ("undo_started_at" IS NOT NULL)),
    "undo_finished_at" timestamptz NULL,
    CONSTRAINT "assistant_ability_requests_undo_finished_has_time_check"
        CHECK ((coalesce("undo_state", 'available') NOT IN ('available', 'in_progress'))
               = ("undo_finished_at" IS NOT NULL))
);

-- ===========================================================================
-- (2) INDEXES
-- ===========================================================================

CREATE INDEX IF NOT EXISTS "assistant_ability_requests_tenant_idx"
    ON "public"."assistant_ability_requests" ("tenant_id");

-- The per-site queue, and the per-site hourly cap.
CREATE INDEX IF NOT EXISTS "assistant_ability_requests_site_created_idx"
    ON "public"."assistant_ability_requests" ("site_id", "created_at" DESC);

-- The per-connection caps, status by grant, and the revoke cascade.
CREATE INDEX IF NOT EXISTS "assistant_ability_requests_grant_created_idx"
    ON "public"."assistant_ability_requests"
       ("proposed_by_grant_id", "created_at" DESC);

-- The dispatch scan and the deadline sweep.
CREATE INDEX IF NOT EXISTS "assistant_ability_requests_dispatch_idx"
    ON "public"."assistant_ability_requests" ("dispatch_deadline_at")
    WHERE "state" = 'approved';

-- The expiry sweep, over the only state that can expire.
CREATE INDEX IF NOT EXISTS "assistant_ability_requests_expiry_sweep_idx"
    ON "public"."assistant_ability_requests" ("expires_at")
    WHERE "state" = 'pending';

-- In flight: sent with no outcome yet. The per-site busy check, the stale
-- reconciler and the ledger resolution read this.
CREATE INDEX IF NOT EXISTS "assistant_ability_requests_in_flight_idx"
    ON "public"."assistant_ability_requests" ("site_id", "claimed_at")
    WHERE "state" IN ('dispatched', 'outcome_unknown') AND "outcome" IS NULL;

-- AT MOST ONE WAITING REQUEST PER CONNECTION PER SITE PER ABILITY PER TARGET.
-- See the header for the target of a creation. The creation path's ON
-- CONFLICT names exactly these columns and this predicate.
CREATE UNIQUE INDEX IF NOT EXISTS "assistant_ability_requests_one_pending_idx"
    ON "public"."assistant_ability_requests"
       ("tenant_id", "site_id", "proposed_by_grant_id", "ability_name", "target_key")
    WHERE "state" = 'pending';

-- ===========================================================================
-- (3) ROW LEVEL SECURITY (amendment R4)
-- ===========================================================================

ALTER TABLE "public"."assistant_ability_requests" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."assistant_ability_requests" FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'assistant_ability_requests'
          AND policyname = 'assistant_ability_requests_tenant_isolation'
    ) THEN
        CREATE POLICY "assistant_ability_requests_tenant_isolation"
            ON "public"."assistant_ability_requests"
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'assistant_ability_requests'
          AND policyname = 'assistant_ability_requests_site_scope'
    ) THEN
        CREATE POLICY "assistant_ability_requests_site_scope"
            ON "public"."assistant_ability_requests"
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

-- The cross-tenant service context (pool.InAgentTx), FOR SELECT only, for the
-- dispatch scan, the expiry and deadline sweeps, the stale reconciler and the
-- ledger-resolution scan, each a plain SELECT returning ids and tenants.
-- Every write runs under a tenant transaction. FOR SELECT is load-bearing,
-- for m151's reason, and a locking read under it returns zero rows.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'assistant_ability_requests'
          AND policyname = 'assistant_ability_requests_agent'
    ) THEN
        CREATE POLICY "assistant_ability_requests_agent"
            ON "public"."assistant_ability_requests"
            FOR SELECT
            USING (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;

-- ===========================================================================
-- (4) PRIVILEGES: THE FACTS ARE IMMUTABLE, AND THE ROW CANNOT BE DELETED
-- ===========================================================================
--
-- As m151. UPDATE is revoked at table level FIRST, then granted back on
-- exactly the workflow columns.
--
-- Immutable after insert: id, tenant_id, site_id, proposed_by_grant_id,
-- entry_id, entry_sha256, ability_name, operator_permission, input_json,
-- input_sha256, target_post_id, target_key (generated), precheck_digest,
-- preview_digest, base_fingerprint, site_label, site_host, grant_label,
-- grant_via, setup_client, title_excerpt, editor, post_type, effect_copy,
-- snapshot, card_copy_version, digest_nonce, presented_digest, created_at,
-- expires_at.
--
-- apps/api/tests/rls_integration_test.go re-applies these after its blanket
-- test grant, so the integration proofs run against a real install's
-- privileges.

GRANT SELECT, INSERT ON "public"."assistant_ability_requests" TO "wpmgr_app";

REVOKE DELETE, TRUNCATE ON "public"."assistant_ability_requests" FROM "wpmgr_app";

REVOKE UPDATE ON "public"."assistant_ability_requests" FROM "wpmgr_app";

GRANT UPDATE (
    "state", "decided_at", "decided_by_user_id", "withdrawn_at",
    "dispatch_deadline_at", "claimed_at",
    "dispatch_attempts", "last_attempt_at", "last_attempt_code",
    "unknown_since", "ledger_checked_at",
    "outcome", "outcome_at", "outcome_code", "not_sent_reason",
    "created_post_id", "restored", "trashed", "site_reported_text",
    "undo_state", "undo_available_until", "undo_by_user_id",
    "undo_started_at", "undo_finished_at"
) ON "public"."assistant_ability_requests" TO "wpmgr_app";
