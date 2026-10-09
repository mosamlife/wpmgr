-- m158: let a person undo the draft a failed or interrupted write left behind
-- (GH #826).
--
-- A write that created a post and then failed (verify_mismatch, a refusal
-- after the insert, a lost reply resolved as outcome_unknown) leaves a draft
-- on the site. The agent already undoes such a draft from its own ledger row.
-- m156's assistant_ability_requests_undo_only_when_done_check allowed an undo
-- on 'done' rows only, so the control plane could not record one.
--
-- This file replaces that CHECK with one that also admits a row in 'failed'
-- or 'outcome_unknown' that names a created post:
--
--   undo_state IS NULL
--   OR state = 'done'
--   OR (state IN ('failed', 'outcome_unknown') AND created_post_id IS NOT NULL)
--
-- The new predicate is a strict widening of the old one, so every row that
-- satisfied m156's CHECK satisfies this one. The DO block below proves that
-- by count before the swap and raises if any row would fail, rather than
-- relying on the argument alone. The count runs with FORCE lifted and
-- row_security off, as m146 does, so a policy that would hide rows from the
-- owner raises an error instead of returning a smaller count.
--
-- Every other undo CHECK from m156 is unchanged: undo_has_window still
-- requires undo_available_until whenever undo_state is set, so the statement
-- that begins a recovery undo sets a window too.
--
-- The constraint keeps its m156 name, so code and tests that name it keep
-- working; the name now understates what it admits.
--
-- CONVERGE PATH. None is needed beyond this file. It is a new ordinal: a
-- database that ran m156 holds the old CHECK and this file replaces it; one
-- that has not runs m156 and then this file in the same boot.
--
-- ORDINAL. 20261001020000 sorts after m157 (20261001010000) and before
-- 20261002000000. The label m158 is a name; the ordinal is the order.
--
-- IDEMPOTENT. DROP CONSTRAINT IF EXISTS, then ADD; a re-run converges.

DO $$
DECLARE
    v_total     bigint;
    v_violators bigint;
BEGIN
    PERFORM set_config('lock_timeout', '5s', true);

    ALTER TABLE "public"."assistant_ability_requests" NO FORCE ROW LEVEL SECURITY;
    PERFORM set_config('row_security', 'off', true);

    SELECT count(*) INTO v_total
    FROM "public"."assistant_ability_requests";

    SELECT count(*) INTO v_violators
    FROM "public"."assistant_ability_requests"
    WHERE NOT (
        "undo_state" IS NULL
        OR "state" = 'done'
        OR ("state" IN ('failed', 'outcome_unknown') AND "created_post_id" IS NOT NULL)
    );

    IF v_violators IS NULL OR v_violators <> 0 THEN
        RAISE EXCEPTION 'm158: % of % assistant_ability_requests rows carry an undo_state the new undo CHECK refuses; nothing was changed',
            coalesce(v_violators::text, 'an unknown number'), v_total;
    END IF;

    RAISE NOTICE 'm158: % assistant_ability_requests rows checked, 0 refused by the new undo CHECK', v_total;

    ALTER TABLE "public"."assistant_ability_requests"
        DROP CONSTRAINT IF EXISTS "assistant_ability_requests_undo_only_when_done_check";

    ALTER TABLE "public"."assistant_ability_requests"
        ADD CONSTRAINT "assistant_ability_requests_undo_only_when_done_check"
        CHECK (
            "undo_state" IS NULL
            OR "state" = 'done'
            OR ("state" IN ('failed', 'outcome_unknown') AND "created_post_id" IS NOT NULL)
        );

    PERFORM set_config('row_security', 'on', true);
    ALTER TABLE "public"."assistant_ability_requests" FORCE ROW LEVEL SECURITY;

    IF NOT EXISTS (
        SELECT 1 FROM pg_class
        WHERE oid = 'public.assistant_ability_requests'::regclass
          AND relrowsecurity
          AND relforcerowsecurity
    ) THEN
        RAISE EXCEPTION 'm158: assistant_ability_requests must end with ENABLE and FORCE ROW LEVEL SECURITY';
    END IF;
END;
$$;
