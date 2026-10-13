-- Request content is cleared after its own final timestamp has aged fifteen
-- days. The record and original input hash remain; draft edits keep only the
-- outline version and design needed to compile additions to a created page.
ALTER TABLE public.assistant_ability_requests
    ADD COLUMN IF NOT EXISTS content_cleared_at timestamptz,
    ADD COLUMN IF NOT EXISTS retained_draft jsonb;

ALTER TABLE public.assistant_ability_requests
    DROP CONSTRAINT IF EXISTS assistant_ability_requests_input_sha256_matches_check,
    DROP CONSTRAINT IF EXISTS assistant_ability_requests_rest_write_card_check,
    DROP CONSTRAINT IF EXISTS assistant_ability_requests_page_edit_card_check,
    DROP CONSTRAINT IF EXISTS assistant_ability_requests_cleared_content_check;
ALTER TABLE public.assistant_ability_requests
    ADD CONSTRAINT assistant_ability_requests_input_sha256_matches_check
        CHECK (content_cleared_at IS NOT NULL
               OR input_sha256 = encode(sha256(convert_to(input_json, 'UTF8')), 'hex')),
    ADD CONSTRAINT assistant_ability_requests_rest_write_card_check
        CHECK (content_cleared_at IS NOT NULL
               OR ability_name <> 'wpmgr/rest-write' OR card_facts IS NOT NULL),
    ADD CONSTRAINT assistant_ability_requests_page_edit_card_check
        CHECK (content_cleared_at IS NOT NULL
               OR ability_name <> 'wpmgr/page-edit'
               OR coalesce(card_facts ->> 'kind' = 'builder_edit', false)),
    ADD CONSTRAINT assistant_ability_requests_cleared_content_check
        CHECK ((content_cleared_at IS NULL AND retained_draft IS NULL)
               OR (content_cleared_at IS NOT NULL AND input_json = '{}' AND card_facts IS NULL));

-- Resolving unknown outcomes have no outcome timestamp; give-up stamps it.
CREATE OR REPLACE FUNCTION public.ability_request_finished_at(
    p_state text, p_outcome_at timestamptz, p_decided_at timestamptz,
    p_expires_at timestamptz, p_withdrawn_at timestamptz)
RETURNS timestamptz
LANGUAGE sql IMMUTABLE PARALLEL SAFE
SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT CASE p_state
        WHEN 'done' THEN p_outcome_at
        WHEN 'failed' THEN p_outcome_at
        WHEN 'not_sent' THEN p_outcome_at
        WHEN 'outcome_unknown' THEN p_outcome_at
        WHEN 'declined' THEN p_decided_at
        WHEN 'expired' THEN p_expires_at
        WHEN 'withdrawn' THEN p_withdrawn_at
        ELSE NULL
    END
$$;

CREATE OR REPLACE FUNCTION public.ability_request_retained_draft(p_ability text, p_input text)
RETURNS jsonb
LANGUAGE plpgsql IMMUTABLE
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_input jsonb;
BEGIN
    IF p_ability <> 'wpmgr/page-create' THEN
        RETURN NULL;
    END IF;
    v_input := p_input::jsonb;
    RETURN jsonb_build_object('outline_version', v_input -> 'outline_version',
                              'design', v_input -> 'design');
EXCEPTION WHEN data_exception THEN
    RETURN jsonb_build_object('outline_version', NULL, 'design', NULL);
END;
$$;

CREATE OR REPLACE FUNCTION public.ability_request_content_guard()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_finished timestamptz;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.content_cleared_at IS NOT NULL OR NEW.retained_draft IS NOT NULL THEN
            RAISE EXCEPTION 'request content is present at creation' USING ERRCODE = '55000';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.input_sha256 IS DISTINCT FROM OLD.input_sha256 THEN
        RAISE EXCEPTION 'the original input hash is immutable' USING ERRCODE = '55000';
    END IF;
    v_finished := public.ability_request_finished_at(OLD.state, OLD.outcome_at,
                    OLD.decided_at, OLD.expires_at, OLD.withdrawn_at);
    IF OLD.content_cleared_at IS NOT NULL
       OR NEW.content_cleared_at IS NULL
       OR NEW.content_cleared_at > statement_timestamp()
       OR NEW.input_json IS DISTINCT FROM '{}'
       OR NEW.card_facts IS NOT NULL
       OR v_finished IS NULL
       OR v_finished >= statement_timestamp() - interval '15 days'
       OR OLD.undo_state IS NOT DISTINCT FROM 'in_progress'
       OR NEW.retained_draft IS DISTINCT FROM
          public.ability_request_retained_draft(OLD.ability_name, OLD.input_json)
       OR (to_jsonb(NEW) - ARRAY['input_json','card_facts','retained_draft','content_cleared_at','target_key'])
          IS DISTINCT FROM
          (to_jsonb(OLD) - ARRAY['input_json','card_facts','retained_draft','content_cleared_at','target_key']) THEN
        RAISE EXCEPTION 'request content changes only in its final retention transition'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION public.ability_request_content_guard() FROM PUBLIC;
DROP TRIGGER IF EXISTS assistant_ability_requests_content_guard ON public.assistant_ability_requests;
CREATE TRIGGER assistant_ability_requests_content_guard
    BEFORE INSERT OR UPDATE OF input_json, input_sha256, card_facts, retained_draft, content_cleared_at
    ON public.assistant_ability_requests
    FOR EACH ROW EXECUTE FUNCTION public.ability_request_content_guard();

CREATE INDEX IF NOT EXISTS assistant_ability_requests_content_retention_idx
    ON public.assistant_ability_requests
       (public.ability_request_finished_at(state, outcome_at, decided_at, expires_at, withdrawn_at), id)
    WHERE content_cleared_at IS NULL
      AND (state IN ('done','failed','not_sent','declined','expired','withdrawn')
           OR (state = 'outcome_unknown' AND outcome IS NOT NULL))
      AND undo_state IS DISTINCT FROM 'in_progress';

-- The table owner needs the definer's column privileges in both role layouts.
-- The trigger constrains that owner, including when it is also wpmgr_app.
DO $$
DECLARE
    v_owner text;
BEGIN
    SELECT pg_get_userbyid(relowner) INTO STRICT v_owner
    FROM pg_class WHERE oid = 'public.assistant_ability_requests'::regclass;
    EXECUTE format('GRANT SELECT ON public.assistant_ability_requests TO %I', v_owner);
    EXECUTE format('GRANT UPDATE (input_json, card_facts, retained_draft, content_cleared_at) ON public.assistant_ability_requests TO %I', v_owner);
END;
$$;

DROP FUNCTION IF EXISTS public.clear_ability_request_content(integer);
DROP FUNCTION IF EXISTS public.clear_ability_request_content(integer, timestamptz, uuid);
CREATE FUNCTION public.clear_ability_request_content(
    p_limit integer, p_after_finished timestamptz DEFAULT NULL, p_after_id uuid DEFAULT NULL)
RETURNS TABLE (cleared integer, failed integer, examined integer,
               after_finished timestamptz, after_id uuid)
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_count integer := 0;
    v_failed integer := 0;
    v_examined integer := 0;
    v_updated integer;
    v_candidate record;
    v_candidates jsonb;
    v_after_finished timestamptz := coalesce(p_after_finished, '-infinity'::timestamptz);
    v_after_id uuid := coalesce(p_after_id, '00000000-0000-0000-0000-000000000000'::uuid);
    v_prev_agent text := current_setting('app.agent', true);
    v_prev_tenant text := current_setting('app.tenant_id', true);
    v_prev_site_scope text := current_setting('app.site_scope', true);
BEGIN
    IF p_limit IS NULL OR p_limit < 1 OR p_limit > 1000 THEN
        RAISE EXCEPTION 'retention batch limit must be between 1 and 1000' USING ERRCODE = '22023';
    END IF;
    IF (p_after_finished IS NULL) <> (p_after_id IS NULL) THEN
        RAISE EXCEPTION 'retention cursor requires both its timestamp and id' USING ERRCODE = '22023';
    END IF;
    PERFORM set_config('app.site_scope', 'off', true);
    PERFORM set_config('app.agent', 'on', true);

    -- Discovery pages follow the retention index under the SELECT-only agent
    -- policy. Each page is bounded; only that page is grouped by tenant.
    LOOP
        SELECT coalesce(jsonb_agg(to_jsonb(d) ORDER BY d.finished_at, d.id), '[]'::jsonb)
        INTO v_candidates
        FROM (
            SELECT id, tenant_id,
                   public.ability_request_finished_at(state, outcome_at, decided_at, expires_at, withdrawn_at) AS finished_at
            FROM public.assistant_ability_requests
            WHERE content_cleared_at IS NULL
              AND (state IN ('done','failed','not_sent','declined','expired','withdrawn')
                   OR (state = 'outcome_unknown' AND outcome IS NOT NULL))
              AND undo_state IS DISTINCT FROM 'in_progress'
              AND public.ability_request_finished_at(state, outcome_at, decided_at, expires_at, withdrawn_at)
                  < statement_timestamp() - interval '15 days'
              AND (public.ability_request_finished_at(state, outcome_at, decided_at, expires_at, withdrawn_at), id)
                  > (v_after_finished, v_after_id)
            ORDER BY public.ability_request_finished_at(state, outcome_at, decided_at, expires_at, withdrawn_at), id
            LIMIT p_limit - v_count - v_failed
        ) d;
        EXIT WHEN jsonb_array_length(v_candidates) = 0;
        v_examined := v_examined + jsonb_array_length(v_candidates);
        v_after_finished := (v_candidates -> -1 ->> 'finished_at')::timestamptz;
        v_after_id := (v_candidates -> -1 ->> 'id')::uuid;

        FOR v_candidate IN
            SELECT id, tenant_id
            FROM jsonb_to_recordset(v_candidates) AS c(id uuid, tenant_id uuid)
            ORDER BY tenant_id, id
        LOOP
            PERFORM set_config('app.tenant_id', v_candidate.tenant_id::text, true);
            PERFORM set_config('app.agent', 'off', true);
            -- Each candidate has its own rollback scope. A row that cannot be
            -- cleared stays intact while the remaining candidates are processed.
            BEGIN
                WITH eligible AS (
                    SELECT r.id
                    FROM public.assistant_ability_requests r
                    WHERE r.id = v_candidate.id
                      AND r.tenant_id = v_candidate.tenant_id
                      AND r.content_cleared_at IS NULL
                      AND (r.state IN ('done','failed','not_sent','declined','expired','withdrawn')
                           OR (r.state = 'outcome_unknown' AND r.outcome IS NOT NULL))
                      AND r.undo_state IS DISTINCT FROM 'in_progress'
                      AND public.ability_request_finished_at(r.state, r.outcome_at, r.decided_at, r.expires_at, r.withdrawn_at)
                          < statement_timestamp() - interval '15 days'
                    FOR UPDATE SKIP LOCKED
                )
                UPDATE public.assistant_ability_requests r
                SET retained_draft = public.ability_request_retained_draft(r.ability_name, r.input_json),
                    input_json = '{}', card_facts = NULL, content_cleared_at = statement_timestamp()
                FROM eligible e
                WHERE r.id = e.id AND r.tenant_id = v_candidate.tenant_id;
                GET DIAGNOSTICS v_updated = ROW_COUNT;
            EXCEPTION WHEN OTHERS THEN
                v_updated := 0;
                v_failed := v_failed + 1;
            END;
            v_count := v_count + v_updated;

            -- Locking and updates use the candidate's tenant policy. Locked
            -- or changed rows consume neither count; failures consume the budget.
            PERFORM set_config('app.agent', 'on', true);
        END LOOP;
        EXIT WHEN v_count + v_failed >= p_limit;
    END LOOP;
    PERFORM set_config('app.agent', coalesce(v_prev_agent, ''), true);
    PERFORM set_config('app.tenant_id', coalesce(v_prev_tenant, ''), true);
    PERFORM set_config('app.site_scope', coalesce(v_prev_site_scope, ''), true);
    -- The cursor covers every discovered row, including failures and skipped
    -- locks. An empty batch has no cursor; a new sweep starts with NULLs.
    RETURN QUERY SELECT v_count, v_failed, v_examined,
        CASE WHEN v_examined > 0 THEN v_after_finished END,
        CASE WHEN v_examined > 0 THEN v_after_id END;
EXCEPTION WHEN OTHERS THEN
    PERFORM set_config('app.agent', coalesce(v_prev_agent, ''), true);
    PERFORM set_config('app.tenant_id', coalesce(v_prev_tenant, ''), true);
    PERFORM set_config('app.site_scope', coalesce(v_prev_site_scope, ''), true);
    RAISE;
END;
$$;
REVOKE ALL ON FUNCTION public.clear_ability_request_content(integer, timestamptz, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.clear_ability_request_content(integer, timestamptz, uuid) TO wpmgr_app;

-- Agent scope permits cross-tenant reads only. Retention writes use the
-- existing tenant isolation policy under the target tenant's own scope.
DROP POLICY IF EXISTS assistant_ability_requests_agent ON public.assistant_ability_requests;
CREATE POLICY assistant_ability_requests_agent ON public.assistant_ability_requests
    FOR SELECT
    USING (current_setting('app.agent', true) = 'on');
