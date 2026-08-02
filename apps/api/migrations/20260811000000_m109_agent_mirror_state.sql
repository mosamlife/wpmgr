-- m109 - GH #322: persist what the upstream agent-release mirror actually
-- did, so the fleet Agent column can eventually say WHEN the reference was
-- last confirmed against upstream, instead of a plain green "current" badge
-- computed against a reference that, on a self-hosted install with the
-- mirror on, may itself be hours behind with no signal anywhere of that.
--
-- ONE ROW PER INSTALL, id = 1 enforced by CHECK. The mirror is one per
-- install, not one per tenant: it fetches one public GitHub release and
-- writes one pair of objects into one bucket (see internal/agentupstream;
-- MirrorArgs is deliberately empty of any tenant/site identity for the same
-- reason). A tenant-scoped table would have to invent a tenant for a job
-- that has none, and every tenant on the install would then read a
-- different copy of the same fact.
--
-- Shape and posture follow wordfence_vuln_feed_meta (m79) plus its m102
-- follow-up, the exact structural analogue: a background-job freshness
-- sentinel with a last-attempt timestamp, a last-success timestamp, an
-- outcome, a short reason, and a last_request_at spacing clock.
--
-- NO RLS, and that is deliberate, not an omission:
--   * The table has no tenant_id and holds no tenant data, no PII and no
--     secrets. Every column is a property of this install's own release
--     channel.
--   * instance_settings (m80) DOES carry RLS with an _agent policy, because
--     it stores ENCRYPTED SECRETS and forcing every access through
--     InAgentTx is worth the friction there. Nothing here is a secret:
--     last_attempt_detail is a curated, non-secret string composed by the
--     application (internal/agentmirror.Repo.RecordAttempt), never a
--     wrapped storage error (which could embed a presigned URL's
--     signature).
--   * wordfence_vuln_feed_meta, which carries the same class of data
--     (freshness timestamp, outcome, error string, request-spacing clock)
--     and is likewise surfaced to operators, has no RLS and is read and
--     written via bare pool queries (internal/vuln/repo.go). This table
--     follows that precedent exactly.
--
-- On exposing this row's data on the tenant-scoped GET /api/v1/fleet/agents:
-- every value here is already shared by every tenant on the install, and is
-- describing the ONE reference version every tenant's fleet is classified
-- against. The same payload already carries self_update_enabled, an
-- install-level config value, for the identical reason.
--
-- Idempotent throughout: IF NOT EXISTS + DROP CONSTRAINT IF EXISTS (mirrors
-- m101/m92/m93). updated_at is set in application SQL (now()); there is no
-- trigger (m36 comment).

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."agent_mirror_state" (
        "id" integer PRIMARY KEY DEFAULT 1
            CONSTRAINT "agent_mirror_state_singleton_chk" CHECK ("id" = 1),

        -- Spacing clock: the wall-clock time of the last ACTUAL GitHub
        -- request, whatever the status code (200, 304, 403 and 429 all
        -- spend the slot). Persisted, not merely in-memory, so the manual
        -- check endpoint can answer "not checked, and here is why" from a
        -- replica that is not the one that will work the job.
        "last_request_at" timestamptz,

        -- LAST ATTEMPT. Written for every run that actually executed.
        -- NEVER written while mirroring is disabled entirely (see
        -- internal/agentupstream.MirrorWorker.Work): stamping an attempt for
        -- a run that did nothing would be the same lie in miniature this
        -- feature exists to remove.
        "last_attempt_at"      timestamptz,
        "last_attempt_outcome" text,
        "last_attempt_detail"  text,  -- curated, non-secret, <=200 chars (Go)
        "last_attempt_trigger" text,

        -- LAST SUCCESS (LAST CONFIRMATION). Advances only when this install
        -- established what upstream publishes: mirrored, current, or
        -- unchanged (a 304). This is the ONLY timestamp an operator-facing
        -- "checked N ago" age may ever be computed from, never
        -- last_attempt_at (see the module doc: LAST ATTEMPT vs LAST SUCCESS
        -- is the whole point of this table).
        "last_success_at"      timestamptz,
        "last_success_outcome" text,
        "last_success_version" text,

        -- Last time a NEW release was actually published into this
        -- install's own storage, as distinct from merely confirming the
        -- existing one.
        "last_mirrored_at"      timestamptz,
        "last_mirrored_version" text,

        "updated_at" timestamptz NOT NULL DEFAULT now()
    );
END;
$$;

ALTER TABLE "public"."agent_mirror_state"
    DROP CONSTRAINT IF EXISTS "agent_mirror_state_attempt_outcome_chk";
ALTER TABLE "public"."agent_mirror_state"
    ADD CONSTRAINT "agent_mirror_state_attempt_outcome_chk"
    CHECK ("last_attempt_outcome" IS NULL OR "last_attempt_outcome" IN (
        'mirrored', 'current', 'unchanged', 'rate_limited', 'refused',
        'foreign_channel', 'upstream_unavailable', 'storage_error',
        'not_configured'
    ));

ALTER TABLE "public"."agent_mirror_state"
    DROP CONSTRAINT IF EXISTS "agent_mirror_state_success_outcome_chk";
ALTER TABLE "public"."agent_mirror_state"
    ADD CONSTRAINT "agent_mirror_state_success_outcome_chk"
    CHECK ("last_success_outcome" IS NULL OR "last_success_outcome" IN (
        'mirrored', 'current', 'unchanged'
    ));

ALTER TABLE "public"."agent_mirror_state"
    DROP CONSTRAINT IF EXISTS "agent_mirror_state_trigger_chk";
ALTER TABLE "public"."agent_mirror_state"
    ADD CONSTRAINT "agent_mirror_state_trigger_chk"
    CHECK ("last_attempt_trigger" IS NULL OR "last_attempt_trigger" IN (
        'periodic', 'manual'
    ));

-- Seed the single row so every write is a plain UPDATE ... WHERE id = 1
-- (mirrors m79's wordfence_vuln_feed_meta seed).
DO $$
BEGIN
    INSERT INTO "public"."agent_mirror_state" ("id") VALUES (1)
    ON CONFLICT ("id") DO NOTHING;
END;
$$;
