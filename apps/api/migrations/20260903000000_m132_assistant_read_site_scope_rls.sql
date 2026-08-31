-- m132 - ADR-061 A11, Wave 1.2: give every table the assistant can READ the
-- RESTRICTIVE app.site_scope policy that 39 other site-keyed tables already
-- carry.
--
-- This migration CORRECTS NOTHING. It edits no applied migration and re-runs no
-- backfill. It is not an m114/m115-shaped repair. It is additive: 22 new
-- policies on 22 tables, and not one existing object is dropped or altered.
--
-- CONVERGE PATH. None is owed. Every statement is a CREATE POLICY guarded by a
-- pg_policies existence check, so the file is idempotent and a database in any
-- state converges by applying this file and nothing else. There is no operator
-- step. A database that has already been hand-patched with a policy of the same
-- name keeps its own and this file skips it -- which is the one case worth
-- knowing about, because the skip is silent; see PROVING IT.
--
-- ===========================================================================
-- WHY IT EXISTS
-- ===========================================================================
--
-- m19 gave 21 site-keyed tables a RESTRICTIVE site-scope policy. m50, m72, m94,
-- m99, m112, m113/m114 and m122 each added more as their tables landed. The set
-- grew by accretion, one migration per feature, and nothing ever asserted that
-- the set was COMPLETE. There is no invariant test in this repository that says
-- "every site-keyed table carries a site-scope policy" -- verified by the
-- absence of any query over information_schema.columns in apps/api/tests. So
-- the gap was never visible as a gap; it was only ever visible one table at a
-- time, when someone happened to look.
--
-- m112 is what that costs. Four tables in the email domain shipped without the
-- policy, so the database refused another TENANT and had no opinion about
-- another SITE. Three review rounds found seven privilege-escalation doors and
-- closed each one in a handler before the fourth round asked why they kept
-- appearing. The answer was that the database had no opinion.
--
-- The assistant surface makes the same gap materially worse, because it turns a
-- latent database property into a reachable one. m131 seated eight read
-- capability groups; the tool registry widens inside that set with no further
-- schema work, by design. Every tool that widens into it reads these tables. A
-- site-scoped assistant grant that reaches a table in this file today is
-- refused only by whatever the handler remembered to check.
--
-- ===========================================================================
-- DECISION 1: THESE POLICIES ARE INERT TODAY, AND THAT IS NOT A DEFECT
-- ===========================================================================
--
-- READ THIS BEFORE CONCLUDING THAT THIS FILE SECURES ANYTHING ON ITS OWN.
--
-- app.site_scope is written in exactly one place in the production tree:
-- InScopedTenantTx at apps/api/internal/db/db.go:582. Nothing else sets it.
-- Every other transaction wrapper -- InTenantTx (db.go:278), InTenantTxAsUser
-- (db.go:355), InAgentTx (db.go:429), InRumIngestLookupTx -- leaves it unset,
-- and with it unset the first disjunct of every predicate below is TRUE and the
-- policy is a tautology.
--
-- The assistant's own scope resolution does NOT currently route through the
-- scoped helper. ResolveScopeSites (apps/api/internal/mcp/repo.go:494) runs
-- under r.pool.InTenantTx and takes a uuid.UUID rather than a principal, so it
-- cannot route on scope even in principle. That signature change is ADR-061
-- A11 item 2 and is Wave 1.3; it is deliberately NOT in this file, because a
-- migration and the Go change that depends on it are two commits in that order.
--
-- So: on the day this applies, these 22 policies subtract nothing on any path
-- the assistant takes. They become load-bearing the moment 1.3 lands, and 1.3
-- is impossible without them -- a scoped helper cannot engage policies that do
-- not exist. That is the whole ordering argument and it is why this file comes
-- first.
--
-- WHAT THIS FILE DOES BUY TODAY: the collaborator path. site_shares-derived
-- principals already reach InScopedTenantTx through RunTenantTx (db.go:772), so
-- for THOSE principals these policies engage on application. That is a real
-- narrowing on a real path, and it is the reason the proofs below are runnable
-- now rather than after 1.3.
--
-- ===========================================================================
-- DECISION 2: THE PREDICATE IS COPIED FROM m19 CHARACTER FOR CHARACTER
-- ===========================================================================
--
-- The exemplar is sites_site_scope at
-- apps/api/migrations/20260531050000_m19_orgs_sharing.sql:330. Consistency with
-- it outranks any improvement, and one specific detail is why.
--
--     nullif(current_setting('app.allowed_site_ids', true), <empty string>)
--
-- The nullif is not defensive noise and it is not cosmetic. It is the entire
-- fail-closed property, and it is ADR-061 A11 item 3 -- "a scope of site with an
-- empty allowlist resolves to zero sites, not all sites", which the ADR names as
-- the single most likely place for a fail-open default to survive review.
--
-- Trace it. An empty allowlist arrives as the empty string, because
-- InScopedTenantTx joins a zero-length slice (db.go:603). Then:
--
--   nullif(<empty string>, <empty string>)  -> NULL
--   string_to_array(NULL, ',')              -> NULL
--   site_id = ANY (NULL::uuid[])            -> NULL
--   FALSE OR NULL                           -> NULL
--
-- and a USING clause that evaluates to NULL does not admit the row. Zero rows.
-- Verified directly against PostgreSQL 16.4 rather than reasoned about:
--
--   SELECT <a uuid> = ANY (string_to_array(nullif('', ''), ',')::uuid[]);
--     empty   -> NULL   (deny)
--     match   -> true
--     nomatch -> false
--
-- The two plausible "improvements" both make this worse:
--
--   * Dropping the nullif. string_to_array(<empty string>, ',') returns a
--     one-element array holding the empty string, and casting that to uuid[]
--     raises 22P02 invalid input syntax. Fail-closed by exception rather than
--     by row count: every read on the path becomes a 500, and the property
--     stops being provable as a row count.
--   * coalesce(..., ARRAY[]::uuid[]). ANY over an empty array is FALSE, which
--     is also zero rows -- but it is a SECOND shape for the same invariant, and
--     the next person to audit this has to prove two predicates equivalent
--     instead of diffing 22 identical ones against m19.
--
-- The property is worth restating precisely because it is counter-intuitive:
-- the empty allowlist denies via NULL, not via FALSE. A reviewer checking for
-- FALSE finds NULL and may read it as an accident. It is not.
--
-- ===========================================================================
-- DECISION 3: SINGLE "FOR ALL" POLICY, NOT THE m112 READ/WRITE SPLIT
-- ===========================================================================
--
-- m112 split its policies into _read / _insert / _update / _delete because the
-- email domain has an INHERITING row: site_id IS NULL means "the organisation
-- default, which every site without its own config actually sends mail with",
-- and a site-scoped collaborator must be able to READ it (a shipped feature)
-- while never being able to WRITE it (the seven doors).
--
-- No table in this file has that shape. Checked, not assumed: the only
-- occurrences of "site_id IS NULL" as a meaningful predicate in
-- apps/api/db/query/**.sql are in site_email.sql, and the only such occurrences
-- in db/schema.sql are the m112 tables plus their partial unique indexes. The
-- perf, RUM, object-cache, database-health, vulnerability and activity queries
-- contain none.
--
-- Six tables here nonetheless have a NULLABLE site_id: site_app_alert_state,
-- site_cache_stats, site_events, site_perf_config, site_security_policy, and
-- (excluded, see DECISION 4) site_file_manager. For those, the m19 predicate
-- denies a NULL-site_id row to a site-scoped principal, because NULL = ANY(...)
-- is NULL. That is the deliberate choice and it is fail-closed. It is safe here
-- precisely because no read path inherits such a row the way the email domain
-- does; if one is ever added, it needs the m112 split and a new ordinal, not an
-- edit to this file.
--
-- ===========================================================================
-- DECISION 4: WHAT IS DELIBERATELY NOT IN THIS FILE
-- ===========================================================================
--
-- Eight site-keyed tables with RLS enabled and no site-scope policy are left
-- alone, each for a stated reason. Silence here would read as an oversight of
-- the same kind this migration exists to close.
--
--   api_keys, invitations, pairing_codes   Credential and enrolment tables. No
--                                          read capability names them and no
--                                          assistant tool can reach them. A
--                                          restrictive policy on a credential
--                                          table is a change to the auth path,
--                                          which is its own review.
--
--   site_shares                            THE ALLOWLIST ITSELF. app.
--                                          allowed_site_ids is derived from
--                                          this table, so scoping it by that
--                                          GUC is circular: a principal would
--                                          be filtered by a list computed from
--                                          rows the filter hides. Whatever the
--                                          right answer is, it is not this
--                                          predicate, and it is not a line item
--                                          inside a migration about reads.
--
--   email_alert_state,                     Email domain. Not one of m131's
--   email_webhook_events                   eight read groups. m112 owns this
--                                          domain and split its policies for
--                                          reasons that apply here too; folding
--                                          two more tables in under the simple
--                                          shape would be the wrong shape.
--
--   file_transfers                         Agent transfer plumbing, not a read
--                                          surface. Reached by the agent under
--                                          InAgentTx, where site_scope is unset
--                                          regardless.
--
--   site_file_manager                      Would belong to mcp.content.read,
--                                          which m131 DECISION 3 seats as
--                                          deliberately UNREACHABLE: no tool
--                                          requires it and nothing can grant
--                                          it. Adding the policy would be
--                                          harmless and would also be the first
--                                          half of a content surface nobody has
--                                          reviewed. It lands with that work.
--
-- ===========================================================================
-- DECISION 5: SEVEN MORE TABLES ARE OWED, AND WHY THEY ARE NOT HERE
-- ===========================================================================
--
-- This file does NOT complete the gap. Seven further site-keyed tables carry no
-- site-scope policy and ARE assistant-readable, two of them named verbatim in
-- m131's own group descriptions:
--
--   site_file_baseline        m77:41   "changed core files" (mcp.security.read)
--   site_managed_files        m77:152  file integrity        (mcp.security.read)
--   site_security_bans        m76:174  "failed login attempts"
--   site_security_hardening_config     m76:38   security posture
--   site_media_assets         m23:30   "media library waste" (performance)
--   media_optimization_jobs   m23:132  media waste           (performance)
--   site_media_settings       m25:34   media waste           (performance)
--
-- plus media_variant_results (m23:232), an INDIRECT child keyed only by
-- job_id + tenant_id, which needs the subquery form m19 uses for
-- backup_manifest_entries rather than the predicate below.
--
-- All eight have site_id (or a parent that does) and RLS enabled. They are
-- omitted for one mechanical reason, and it is worth stating because it is the
-- reason the gap was invisible in the first place:
--
--     NONE OF THESE EIGHT TABLES EXISTS IN apps/api/db/schema.sql AT ALL.
--
-- Verified: a CREATE TABLE grep for the set over db/schema.sql returns 0.
-- The rule of thumb is that schema.sql lags the migrations on POLICIES -- 46
-- policies over 26 tables there against 59 over 39 in the migrations. It is
-- worse than that: it lags on WHOLE TABLES. An audit that builds its list of
-- site-keyed tables from schema.sql, as the first pass of this work did, cannot
-- see these eight and reports the gap as closed when it is not.
--
-- Adding their policies here while schema.sql lacks their DDL would put
-- CREATE POLICY statements against undefined relations into sqlc's input and
-- break generation. So the second increment is: backfill the eight CREATE TABLE
-- definitions into schema.sql, then a new ordinal carrying their policies --
-- seven of this file's shape and one of m19's indirect-child shape. It is NOT
-- an edit to this file, which will have applied.
--
-- ===========================================================================
-- DECISION 6: LOCK DURATION
-- ===========================================================================
--
-- CREATE POLICY takes ACCESS EXCLUSIVE on its table, but holds it only for the
-- catalogue write: there is no table scan, no rewrite and no constraint
-- validation anywhere in this file. Duration is proportional to the number of
-- policies (22), not to any table's row count -- which matters, because
-- rum_events_raw and site_uptime_probes-class tables are the largest in this
-- schema and a row-proportional statement on them would be an outage.
--
-- The runner wraps this file in ONE transaction and a transaction cannot weaken
-- its own lock, so all 22 ACCESS EXCLUSIVE locks are held together until the
-- file commits. That is the real cost: a brief total stall on 22 tables, not 22
-- brief stalls. It is still milliseconds of catalogue work, and it is the same
-- shape m19 already took on 21 tables at once.
--
-- The trap this file avoids: an unbatched backfill or an ADD CONSTRAINT ...
-- VALIDATE alongside these policies would hold every one of those locks for the
-- duration of the scan. There is no DML in this file for exactly that reason.
--
-- ===========================================================================
-- DECISION 7: THE CROSS-TENANT LEDGER GAINS NO ROWS
-- ===========================================================================
--
-- apps/api/db/rls-cross-tenant-policies.txt is unchanged by this migration and
-- that is correct, not an omission. scripts/check-rls-cross-tenant.sh:46-48
-- excludes RESTRICTIVE policies by construction and names site_scope as the
-- reason: a restrictive policy can only ever narrow the row set, never grant,
-- so it cannot be a cross-tenant grant and has nothing to record.
--
-- The same script states its own blind spot at line 79: it is blind to a MISSING
-- or WEAKENED restrictive policy, by construction. So the guard passing over
-- this migration is not evidence that this migration is right. The proofs
-- below are.
--
-- ===========================================================================
-- PROVING IT
-- ===========================================================================
--
-- Every policy here is created inside an IF NOT EXISTS guard, which means a
-- policy that already exists under the same name is SILENTLY KEPT. Asserting
-- "a row exists in pg_policies with that name" therefore proves nothing about
-- what this file created -- that is the exact mistake m114 shipped, where
-- dropping AS RESTRICTIVE passed the whole suite because a permissive policy is
-- OR-combined and GRANTS instead of subtracting.
--
-- The proof obligations are semantic, per table, as the wpmgr_app role
-- (rolsuper=f, rolbypassrls=f), through InScopedTenantTx:
--
--   1. a principal allowed site A reads A's rows and not B's;
--   2. a principal with an EMPTY allowlist reads ZERO rows;
--   3. a principal on the plain helper reads exactly what it reads today;
--   4. cross-tenant isolation is unchanged.
--
-- Obligation 3 is the one that would take the product down if it failed, and it
-- is the reason the predicate's first disjunct is a tautology rather than a
-- check on some new column.
--
-- ===========================================================================
-- THE POLICIES
-- ===========================================================================
--
-- Grouped by the m131 read capability that reaches them. The predicate is
-- byte-identical in all 22; only the table name changes.

-- ---------------------------------------------------------------------------
-- mcp.performance.read - page speed and caching
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_perf_config'
          AND policyname = 'site_perf_config_site_scope'
    ) THEN
        CREATE POLICY "site_perf_config_site_scope" ON "public"."site_perf_config"
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

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_cache_stats'
          AND policyname = 'site_cache_stats_site_scope'
    ) THEN
        CREATE POLICY "site_cache_stats_site_scope" ON "public"."site_cache_stats"
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

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_cache_hit_ratio_history'
          AND policyname = 'site_cache_hit_ratio_history_site_scope'
    ) THEN
        CREATE POLICY "site_cache_hit_ratio_history_site_scope" ON "public"."site_cache_hit_ratio_history"
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

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_object_cache_config'
          AND policyname = 'site_object_cache_config_site_scope'
    ) THEN
        CREATE POLICY "site_object_cache_config_site_scope" ON "public"."site_object_cache_config"
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

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_object_cache_stats_history'
          AND policyname = 'site_object_cache_stats_history_site_scope'
    ) THEN
        CREATE POLICY "site_object_cache_stats_history_site_scope" ON "public"."site_object_cache_stats_history"
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

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'cache_purge_audit'
          AND policyname = 'cache_purge_audit_site_scope'
    ) THEN
        CREATE POLICY "cache_purge_audit_site_scope" ON "public"."cache_purge_audit"
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

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'rucss_jobs'
          AND policyname = 'rucss_jobs_site_scope'
    ) THEN
        CREATE POLICY "rucss_jobs_site_scope" ON "public"."rucss_jobs"
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

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'rucss_results'
          AND policyname = 'rucss_results_site_scope'
    ) THEN
        CREATE POLICY "rucss_results_site_scope" ON "public"."rucss_results"
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

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'font_results'
          AND policyname = 'font_results_site_scope'
    ) THEN
        CREATE POLICY "font_results_site_scope" ON "public"."font_results"
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

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'font_transcode_results'
          AND policyname = 'font_transcode_results_site_scope'
    ) THEN
        CREATE POLICY "font_transcode_results_site_scope" ON "public"."font_transcode_results"
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

-- ---------------------------------------------------------------------------
-- mcp.performance.read - real user page speed (RUM)
--
-- rum_events_raw is among the highest-row-count tables in this schema. See
-- DECISION 6: CREATE POLICY does not scan it.
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'rum_events_raw'
          AND policyname = 'rum_events_raw_site_scope'
    ) THEN
        CREATE POLICY "rum_events_raw_site_scope" ON "public"."rum_events_raw"
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

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'rum_rollup_hourly'
          AND policyname = 'rum_rollup_hourly_site_scope'
    ) THEN
        CREATE POLICY "rum_rollup_hourly_site_scope" ON "public"."rum_rollup_hourly"
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

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'rum_rollup_daily'
          AND policyname = 'rum_rollup_daily_site_scope'
    ) THEN
        CREATE POLICY "rum_rollup_daily_site_scope" ON "public"."rum_rollup_daily"
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

-- ---------------------------------------------------------------------------
-- mcp.performance.read - database size and bloat
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_db_size_history'
          AND policyname = 'site_db_size_history_site_scope'
    ) THEN
        CREATE POLICY "site_db_size_history_site_scope" ON "public"."site_db_size_history"
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

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_db_scan_results'
          AND policyname = 'site_db_scan_results_site_scope'
    ) THEN
        CREATE POLICY "site_db_scan_results_site_scope" ON "public"."site_db_scan_results"
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

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_db_clean_results'
          AND policyname = 'site_db_clean_results_site_scope'
    ) THEN
        CREATE POLICY "site_db_clean_results_site_scope" ON "public"."site_db_clean_results"
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

-- ---------------------------------------------------------------------------
-- mcp.security.read - is it exposed or tampered with
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_vulnerabilities'
          AND policyname = 'site_vulnerabilities_site_scope'
    ) THEN
        CREATE POLICY "site_vulnerabilities_site_scope" ON "public"."site_vulnerabilities"
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

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_security_policy'
          AND policyname = 'site_security_policy_site_scope'
    ) THEN
        CREATE POLICY "site_security_policy_site_scope" ON "public"."site_security_policy"
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

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_security_policy_groups'
          AND policyname = 'site_security_policy_groups_site_scope'
    ) THEN
        CREATE POLICY "site_security_policy_groups_site_scope" ON "public"."site_security_policy_groups"
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

-- ---------------------------------------------------------------------------
-- mcp.activity.read - what changed, and how it went
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_events'
          AND policyname = 'site_events_site_scope'
    ) THEN
        CREATE POLICY "site_events_site_scope" ON "public"."site_events"
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

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_connection_history'
          AND policyname = 'site_connection_history_site_scope'
    ) THEN
        CREATE POLICY "site_connection_history_site_scope" ON "public"."site_connection_history"
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

-- ---------------------------------------------------------------------------
-- mcp.uptime.read - is it up, and has it been
--
-- The site_uptime_* tables already carry the policy (m19, m99). This is the one
-- table in that family that does not.
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_app_alert_state'
          AND policyname = 'site_app_alert_state_site_scope'
    ) THEN
        CREATE POLICY "site_app_alert_state_site_scope" ON "public"."site_app_alert_state"
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
