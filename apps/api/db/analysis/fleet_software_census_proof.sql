-- ===========================================================================
-- Proof harness for fleet_software_census.sql
-- ===========================================================================
-- Seeds thirteen sites covering every classification branch and every
-- inventory-age bucket, PLUS a second "noise" org whose three sites every
-- assertion must exclude, runs the census, asserts the counts, then ROLLS BACK.
-- Nothing is persisted.
--
-- The noise org is load-bearing. Assertions here used to read GLOBAL counts
-- (`SELECT count(*) FROM census_fleet`), which are only correct on a database
-- holding no other sites -- and the census runs fleet-wide as a BYPASSRLS
-- owner, so it sees everything. It passed solely because the dev container has
-- zero sites, and would have mis-asserted on any populated database. Every
-- assertion is now scoped to the fixture tenant, and the noise org exists so
-- that re-globalising one breaks immediately instead of much later on somebody
-- else's database.
--
-- It also proves the census is READ-ONLY by behaviour rather than by grepping
-- its text: every base table in `public` is fingerprinted (rows + content
-- digest) and the transaction's tuple-write counters are captured, before and
-- after the census runs, and any movement fails the harness.
--
--   psql "$OWNER_DSN" -f fleet_software_census_proof.sql
--
-- Run it from THIS DIRECTORY: the \i below is resolved against psql's cwd.
--
-- Requires a database at m121 or later (sites.components_updated_at). Against
-- an older schema the seeds fail on the unknown column, loudly, which is the
-- correct outcome — the census cannot be proven against a schema that lacks the
-- column it now reports on.
--
-- The census's own empty-scope guard is proven separately by running it against
-- an empty scope: it must RAISE and exit non-zero rather than print zeroes.
--
-- Every seeded site is annotated with the bucket it must land in. If you change
-- the classification CASE in the census, this file must move with it.
-- ===========================================================================

\set ON_ERROR_STOP on
\pset pager off

BEGIN;

INSERT INTO tenants (id, name, slug)
VALUES ('c5e75000-0000-4000-8000-0000000c5e75', 'Census Proof Org', 'census-proof-org-s2');

-- ---------------------------------------------------------------------------
-- components_updated_at (m121) is set EXPLICITLY on every seed, including the
-- NULLs. The NULLs are fixtures, not oversights: they are the state every row
-- in a real database is in until its next metadata push, because m121
-- deliberately did not backfill. A proof that only seeded dated rows would
-- never exercise the bucket that dominates a real run.
-- ---------------------------------------------------------------------------

-- 1. Elementor active as a PLUGIN.       -> builder active, inventory_24h
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, components_updated_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-000000000001','c5e75000-0000-4000-8000-0000000c5e75','https://s1.example','s1-elementor','connected', now(), now(), false, 'hello-elementor',
 '{"plugins":[{"slug":"elementor/elementor.php","name":"Elementor","version":"3.21.5","active":true},
              {"slug":"akismet/akismet.php","name":"Akismet","version":"5.3.1","active":false}],
   "themes":[{"slug":"hello-elementor","name":"Hello","version":"3.0.1","active":true}]}'::jsonb);

-- 2. Bricks active as a THEME. A plugins-only census scores this Gutenberg.
--                                        -> builder active, inventory_week
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, components_updated_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-000000000002','c5e75000-0000-4000-8000-0000000c5e75','https://s2.example','s2-bricks','connected', now(), now() - interval '3 days', false, 'bricks',
 '{"plugins":[{"slug":"akismet/akismet.php","name":"Akismet","version":"5.3.1","active":true}],
   "themes":[{"slug":"bricks","name":"Bricks","version":"1.9.2","active":true}]}'::jsonb);

-- 3. Divi, whose stylesheet directory is capital-D "Divi". FULL inventory but
--    NO recorded age — a dated-inventory question this site cannot answer even
--    though its component list is complete.
--                                        -> builder active, age_unknown
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, components_updated_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-000000000003','c5e75000-0000-4000-8000-0000000c5e75','https://s3.example','s3-divi','connected', now(), NULL, false, 'Divi',
 '{"plugins":[],
   "themes":[{"slug":"Divi","name":"Divi","version":"4.23.1","active":true}]}'::jsonb);

-- 4. WPBakery, whose plugin directory is "js_composer".
--                                        -> builder active, inventory_24h
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, components_updated_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-000000000004','c5e75000-0000-4000-8000-0000000c5e75','https://s4.example','s4-wpbakery','connected', now(), now(), false, 'twentytwentyone',
 '{"plugins":[{"slug":"js_composer/js_composer.php","name":"WPBakery","version":"6.13.0","active":true}],
   "themes":[{"slug":"twentytwentyone","name":"TT1","version":"2.0","active":true}]}'::jsonb);

-- 5. No builder at all, full inventory. Also the Woo + Yoast + ACF fixture.
--    Dated 10 days back, so it is PROVABLY stale at the default stale_days=7.
--                                        -> Gutenberg, inventory_month, stale
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, components_updated_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-000000000005','c5e75000-0000-4000-8000-0000000c5e75','https://s5.example','s5-gutenberg','connected', now(), now() - interval '10 days', false, 'twentytwentyfour',
 '{"plugins":[{"slug":"woocommerce/woocommerce.php","name":"WooCommerce","version":"8.1.2","active":true},
              {"slug":"wordpress-seo/wp-seo.php","name":"Yoast SEO","version":"21.5","active":true},
              {"slug":"advanced-custom-fields/acf.php","name":"ACF","version":"6.2.4","active":true}],
   "themes":[{"slug":"twentytwentyfour","name":"TT4","version":"1.1","active":true}]}'::jsonb);

-- 6. Plugins array present, THEMES KEY ABSENT, and its only builder is
--    inactive. Cannot be called Gutenberg because an unseen theme could be
--    Bricks or Divi.
--                                        -> indeterminate: partial inventory
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, components_updated_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-000000000006','c5e75000-0000-4000-8000-0000000c5e75','https://s6.example','s6-nothemes','connected', now(), NULL, false, '',
 '{"plugins":[{"slug":"elementor/elementor.php","name":"Elementor","version":"3.20.0","active":false}]}'::jsonb);

-- 7. Never reported anything.            -> unknown: no inventory, age_unknown
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, components_updated_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-000000000007','c5e75000-0000-4000-8000-0000000c5e75','https://s7.example','s7-empty','pending_enrollment', NULL, NULL, false, '', '{}'::jsonb);

-- 8. Malformed plugins key (the shape sites.sql already guards against).
--                                        -> unknown: no inventory, age_unknown
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, components_updated_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-000000000008','c5e75000-0000-4000-8000-0000000c5e75','https://s8.example','s8-badshape','connected', now(), NULL, false, '',
 '{"plugins":{"unexpected":"shape"}}'::jsonb);

-- 9. MULTISITE with an Elementor reported active=false. On a PRE-#558 agent
--    that may be a network-activated plugin being under-reported; on a post
--    -#558 agent it is genuinely inactive. The census cannot tell which, which
--    is why multisite is reported in its own column and section 7 sizes the
--    exposure.
--                                        -> builder installed, not active
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, components_updated_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-000000000009','c5e75000-0000-4000-8000-0000000c5e75','https://s9.example','s9-multisite','connected', now(), now(), true, 'twentytwentyfour',
 '{"plugins":[{"slug":"elementor/elementor.php","name":"Elementor","version":"3.21.5","active":false}],
   "themes":[]}'::jsonb);

-- 10. Elementor active, inventory recorded 60 days ago.
--                                        -> builder active, over_30d, stale
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, components_updated_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-00000000000a','c5e75000-0000-4000-8000-0000000c5e75','https://s10.example','s10-stale','disconnected', now() - interval '60 days', now() - interval '60 days', false, 'hello-elementor',
 '{"plugins":[{"slug":"elementor/elementor.php","name":"Elementor","version":"3.18.0","active":true}],
   "themes":[]}'::jsonb);

-- 11. Version header missing; the agent writes the literal "unknown".
--                                        -> builder active, (unparsed)
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, components_updated_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-00000000000b','c5e75000-0000-4000-8000-0000000c5e75','https://s11.example','s11-unknownver','connected', now(), now(), false, 'hello-elementor',
 '{"plugins":[{"slug":"elementor/elementor.php","name":"Elementor","version":"unknown","active":true}],
   "themes":[]}'::jsonb);

-- 12. Breakdance installed but switched off. Single-site, so active=false is
--     trustworthy regardless of agent version. Dated 40 days back.
--                                        -> builder installed, not active, stale
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, components_updated_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-00000000000c','c5e75000-0000-4000-8000-0000000c5e75','https://s12.example','s12-inactive','connected', now(), now() - interval '40 days', false, 'twentytwentyfour',
 '{"plugins":[{"slug":"breakdance/plugin.php","name":"Breakdance","version":"1.7.0","active":false}],
   "themes":[{"slug":"twentytwentyfour","name":"TT4","version":"1.1","active":true}]}'::jsonb);

-- 13. THEMES ONLY: Bricks active, PLUGINS KEY ABSENT. This is the mirror of
--     site 6 and it is the regression guard for the classification ORDER. A
--     CASE that tests "no plugin inventory" before testing "builder active"
--     calls this site 'unknown' — discarding a builder we positively observed.
--     A positive finding survives partial inventory; only a negative needs the
--     complete document.
--                                        -> builder active, inventory_24h
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, components_updated_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-00000000000d','c5e75000-0000-4000-8000-0000000c5e75','https://s13.example','s13-themesonly','connected', now(), now() - interval '2 hours', false, 'bricks',
 '{"themes":[{"slug":"bricks","name":"Bricks","version":"1.9.5","active":true}]}'::jsonb);

-- An archived site, which the census must EXCLUDE entirely.
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, components_updated_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-0000000000ff','c5e75000-0000-4000-8000-0000000c5e75','https://sX.example','sX-archived','archived', now(), now(), false, 'x',
 '{"plugins":[{"slug":"elementor/elementor.php","name":"Elementor","version":"3.21.5","active":true}],"themes":[]}'::jsonb);

-- ---------------------------------------------------------------------------
-- NOISE TENANT. A SECOND org whose sites the census WILL see (this runs
-- fleet-wide as a BYPASSRLS owner) and which every assertion below must
-- therefore exclude.
--
-- This exists because the harness previously asserted on GLOBAL counts --
-- `SELECT count(*) FROM census_fleet` and friends -- which only gave the right
-- answer on a database holding no other sites. It passed on the dev container
-- because that container has zero sites, and it would have mis-asserted on any
-- populated database, including a staging restore. Seeding foreign sites makes
-- that failure mode permanent rather than latent: if anyone re-globalises an
-- assertion, these rows break it immediately instead of two months from now on
-- someone else's database.
--
-- Deliberately shaped to break every global count: it adds sites, an active
-- Elementor, an active Bricks theme, an undated inventory and a multisite.
-- ---------------------------------------------------------------------------
INSERT INTO tenants (id, name, slug)
VALUES ('d0125000-0000-4000-8000-0000000d0125', 'Noise Org', 'census-noise-org-s2');

INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, components_updated_at, multisite, active_theme, components) VALUES
('c0000000-0000-0000-0000-000000000001','d0125000-0000-4000-8000-0000000d0125','https://n1.example','n1-elementor','connected', now(), now(), false, 'hello-elementor',
 '{"plugins":[{"slug":"elementor/elementor.php","name":"Elementor","version":"3.21.5","active":true}],
   "themes":[{"slug":"hello-elementor","name":"Hello","version":"3.0.1","active":true}]}'::jsonb),
('c0000000-0000-0000-0000-000000000002','d0125000-0000-4000-8000-0000000d0125','https://n2.example','n2-bricks','connected', now(), NULL, true, 'bricks',
 '{"plugins":[],
   "themes":[{"slug":"bricks","name":"Bricks","version":"1.9.2","active":true}]}'::jsonb),
('c0000000-0000-0000-0000-000000000003','d0125000-0000-4000-8000-0000000d0125','https://n3.example','n3-empty','pending_enrollment', NULL, NULL, false, '', '{}'::jsonb);

-- ---------------------------------------------------------------------------
-- READ-ONLY, PROVEN AT RUNTIME RATHER THAN BY GREPPING THE TEXT.
--
-- The claim "this script writes nothing" was previously supported by
--     grep -inE '^[[:space:]]*(INSERT|UPDATE|DELETE|...)' fleet_software_census.sql
-- which is a check that cannot meaningfully fail. The `^` anchor means a write
-- placed after anything else on its line is invisible to it, and the file
-- mentions those keywords dozens of times in prose regardless, so the exit
-- status was never really testing the property.
--
-- This tests the BEHAVIOUR instead: fingerprint every base table in `public`,
-- run the census, fingerprint again, and require the two to be identical. Row
-- count alone would miss an UPDATE that rewrites a row in place, so each table
-- also carries a content digest.
--
-- Cheap here because the fixture is tiny. It is a proof-harness device and is
-- not something to point at a production-sized database.
-- ---------------------------------------------------------------------------
CREATE TEMP TABLE census_rw_snapshot (
    phase  text,
    tbl    text,
    n      bigint,
    digest text
);

CREATE FUNCTION pg_temp.census_fingerprint(p_phase text) RETURNS bigint AS $fn$
DECLARE
    r        record;
    cnt      bigint;
    dig      text;
    n_tables bigint := 0;
BEGIN
    FOR r IN
        SELECT c.relname
        FROM pg_class c
        JOIN pg_namespace ns ON ns.oid = c.relnamespace
        WHERE ns.nspname = 'public' AND c.relkind = 'r'
        ORDER BY c.relname
    LOOP
        EXECUTE format(
            'SELECT count(*), coalesce(md5(string_agg(t::text, %L ORDER BY t::text)), %L) FROM public.%I t',
            '|', '(empty)', r.relname)
        INTO cnt, dig;
        INSERT INTO census_rw_snapshot VALUES (p_phase, r.relname, cnt, dig);
        n_tables := n_tables + 1;
    END LOOP;

    -- Content digests alone are NOT sufficient, and this was found the hard
    -- way: a planted `UPDATE sites SET last_seen_at = now() WHERE multisite`
    -- passed the digest check silently, because now() is the TRANSACTION START
    -- time and the seeds had already written that exact value -- so the write
    -- really happened and rewrote identical bytes.
    --
    -- pg_stat_xact_user_tables counts TUPLES TOUCHED in the current
    -- transaction, so an UPDATE that changes nothing still increments
    -- n_tup_upd. That closes the identical-value hole the digest cannot see.
    -- Restricted to `public` so this function's own INSERTs into the temp
    -- snapshot table are not counted as writes.
    INSERT INTO census_rw_snapshot
    SELECT p_phase, '__xact_tuples_written__',
           coalesce(sum(n_tup_ins + n_tup_upd + n_tup_del), 0),
           '(tuple counter, not a digest)'
    FROM pg_stat_xact_user_tables
    WHERE schemaname = 'public';

    RETURN n_tables;
END
$fn$ LANGUAGE plpgsql;

SELECT pg_temp.census_fingerprint('before') AS tables_fingerprinted_before;

\echo '### Running census as the OWNER (BYPASSRLS), fleet-wide ###'
\i fleet_software_census.sql

SELECT pg_temp.census_fingerprint('after') AS tables_fingerprinted_after;

-- ---------------------------------------------------------------------------
-- Assertions. Each is the classification the seed comments promise.
-- ---------------------------------------------------------------------------
\echo ''
\echo '### ASSERTIONS ###'
DO $$
DECLARE
    got  bigint;
    fail text := '';
    diff text;
    -- EVERY assertion below is scoped to this tenant. The census runs
    -- fleet-wide as a BYPASSRLS owner, so census_fleet also holds the noise
    -- org's sites and anything else the database happens to contain. Asserting
    -- on global counts made this harness silently conditional on an EMPTY
    -- database -- it passed only because the dev container has zero sites, and
    -- it would have mis-asserted on any populated one.
    proof_tenant constant uuid := 'c5e75000-0000-4000-8000-0000000c5e75';
    noise_tenant constant uuid := 'd0125000-0000-4000-8000-0000000d0125';
BEGIN
    -- The noise org must actually be present, or the scoping is being proven
    -- against nothing and we are back to the empty-database assumption.
    SELECT count(*) INTO got FROM census_fleet WHERE tenant_id = noise_tenant;
    IF got <> 3 THEN fail := fail || format('noise tenant should contribute 3 sites that every assertion must exclude, got %s; ', got); END IF;

    -- ---- READ-ONLY, PROVEN AT RUNTIME -------------------------------------
    -- Every base table in `public`, fingerprinted before and after the census
    -- ran. Any INSERT, UPDATE or DELETE it performed moves a count or a digest.
    SELECT count(*) INTO got FROM census_rw_snapshot WHERE phase = 'before';
    IF got = 0 THEN fail := fail || 'read-only fingerprint covered 0 tables, so it proves nothing; fix it before trusting a pass; '; END IF;

    SELECT count(*) INTO got FROM census_rw_snapshot WHERE phase = 'after';
    IF got = 0 THEN fail := fail || 'read-only fingerprint has no AFTER phase; the census run did not complete; '; END IF;

    SELECT string_agg(format('%s (rows %s->%s, digest %s)',
                             b.tbl, b.n, a.n,
                             CASE WHEN b.digest IS DISTINCT FROM a.digest THEN 'CHANGED' ELSE 'same' END), ', ')
      INTO diff
      FROM census_rw_snapshot b
      JOIN census_rw_snapshot a ON a.tbl = b.tbl AND a.phase = 'after'
     WHERE b.phase = 'before'
       AND (b.n, b.digest) IS DISTINCT FROM (a.n, a.digest);
    IF diff IS NOT NULL THEN
        fail := fail || format('THE CENSUS IS NOT READ-ONLY -- it changed: %s; ', diff);
    END IF;

    SELECT count(*) INTO got FROM census_fleet WHERE tenant_id = proof_tenant;
    IF got <> 13 THEN fail := fail || format('scope should exclude the archived site and be 13, got %s; ', got); END IF;

    -- ---- Inventory-usability flags must be BOOLEAN, never NULL. -------------
    -- These are the regression guard for the three-valued-logic bug:
    -- jsonb_typeof(NULL) is NULL, so without COALESCE a never-reported site
    -- (components = '{}', the column default) has has_plugin_inventory = NULL,
    -- slips past `WHEN NOT ...` and is classified Gutenberg.
    SELECT count(*) INTO got FROM census_fleet WHERE tenant_id = proof_tenant AND has_plugin_inventory IS NULL;
    IF got <> 0 THEN fail := fail || format('has_plugin_inventory must never be NULL, got %s NULL rows; ', got); END IF;

    SELECT count(*) INTO got FROM census_fleet WHERE tenant_id = proof_tenant AND has_theme_inventory IS NULL;
    IF got <> 0 THEN fail := fail || format('has_theme_inventory must never be NULL, got %s NULL rows; ', got); END IF;

    -- Same rule, now for the m121 age flags. inventory_provably_stale is built
    -- from a comparison against a NULLABLE column, so it is the same trap one
    -- table over: without COALESCE it is NULL for every undated site and every
    -- `FILTER (WHERE ...)` silently drops those rows.
    SELECT count(*) INTO got FROM census_fleet WHERE tenant_id = proof_tenant AND inventory_age_known IS NULL;
    IF got <> 0 THEN fail := fail || format('inventory_age_known must never be NULL, got %s NULL rows; ', got); END IF;

    SELECT count(*) INTO got FROM census_fleet WHERE tenant_id = proof_tenant AND inventory_provably_stale IS NULL;
    IF got <> 0 THEN fail := fail || format('inventory_provably_stale must never be NULL, got %s NULL rows; ', got); END IF;

    SELECT count(*) INTO got FROM census_fleet WHERE tenant_id = proof_tenant AND inventory_freshness IS NULL;
    IF got <> 0 THEN fail := fail || format('inventory_freshness must never be NULL, got %s NULL rows; ', got); END IF;

    -- ---- Inventory presence -----------------------------------------------
    SELECT count(*) INTO got FROM census_fleet WHERE tenant_id = proof_tenant AND NOT has_plugin_inventory AND NOT has_theme_inventory;
    IF got <> 2 THEN fail := fail || format('sites with NO usable inventory should be 2 (s7 empty, s8 malformed), got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_fleet
      WHERE tenant_id = proof_tenant AND (has_plugin_inventory OR has_theme_inventory)
        AND NOT (has_plugin_inventory AND has_theme_inventory);
    IF got <> 2 THEN fail := fail || format('PARTIAL inventory should be 2 (s6 plugins-only, s13 themes-only), got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_fleet WHERE tenant_id = proof_tenant AND has_plugin_inventory;
    IF got <> 10 THEN fail := fail || format('plugin_denom should be 10, got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_fleet WHERE tenant_id = proof_tenant AND has_theme_inventory;
    IF got <> 10 THEN fail := fail || format('theme_denom should be 10, got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_fleet WHERE tenant_id = proof_tenant AND (has_plugin_inventory OR has_theme_inventory);
    IF got <> 11 THEN fail := fail || format('either_denom should be 11, got %s; ', got); END IF;

    -- ---- m121 inventory age. The NULL bucket is the point. ----------------
    SELECT count(*) INTO got FROM census_fleet WHERE tenant_id = proof_tenant AND NOT inventory_age_known;
    IF got <> 4 THEN fail := fail || format('age_unknown should be 4 (s3,s6,s7,s8), got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_fleet WHERE tenant_id = proof_tenant AND inventory_age_known;
    IF got <> 9 THEN fail := fail || format('age_known should be 9, got %s; ', got); END IF;

    -- Every undated site must land in the age_unknown bucket and NOWHERE else.
    -- This is the assertion that stops a NULL age being quietly folded into a
    -- freshness bucket, which would report "we do not know" as "it is fresh".
    SELECT count(*) INTO got FROM census_fleet
      WHERE tenant_id = proof_tenant AND NOT inventory_age_known AND inventory_freshness <> 'age_unknown (never recorded)';
    IF got <> 0 THEN fail := fail || format('%s undated site(s) landed in a dated freshness bucket — NULL age is being folded in; ', got); END IF;

    SELECT count(*) INTO got FROM census_fleet
      WHERE tenant_id = proof_tenant AND inventory_age_known AND inventory_freshness = 'age_unknown (never recorded)';
    IF got <> 0 THEN fail := fail || format('%s DATED site(s) landed in age_unknown; ', got); END IF;

    -- An undated site must never be counted as provably stale: we cannot prove
    -- what we never recorded.
    SELECT count(*) INTO got FROM census_fleet
      WHERE tenant_id = proof_tenant AND NOT inventory_age_known AND inventory_provably_stale;
    IF got <> 0 THEN fail := fail || format('%s undated site(s) counted as provably stale; ', got); END IF;

    SELECT count(*) INTO got FROM census_fleet WHERE tenant_id = proof_tenant AND inventory_provably_stale;
    IF got <> 3 THEN fail := fail || format('provably stale (>7d) should be 3 (s5 10d, s10 60d, s12 40d), got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_fleet WHERE tenant_id = proof_tenant AND inventory_freshness = 'inventory_24h';
    IF got <> 5 THEN fail := fail || format('inventory_24h should be 5 (s1,s4,s9,s11,s13), got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_fleet WHERE tenant_id = proof_tenant AND inventory_freshness = 'inventory_week';
    IF got <> 1 THEN fail := fail || format('inventory_week should be 1 (s2), got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_fleet WHERE tenant_id = proof_tenant AND inventory_freshness = 'inventory_month';
    IF got <> 1 THEN fail := fail || format('inventory_month should be 1 (s5), got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_fleet WHERE tenant_id = proof_tenant AND inventory_freshness = 'inventory_over_30d';
    IF got <> 2 THEN fail := fail || format('inventory_over_30d should be 2 (s10,s12), got %s; ', got); END IF;

    -- Inventory age and agent contact are DIFFERENT facts. s3 proves it: the
    -- heartbeat is current, the inventory has no recorded age at all. If these
    -- two ever agree on every row, the census has gone back to reading
    -- last_seen_at as freshness.
    SELECT count(*) INTO got FROM census_fleet
      WHERE tenant_id = proof_tenant AND last_contact_freshness = 'contact_24h' AND NOT inventory_age_known;
    IF got <> 3 THEN fail := fail || format('sites contacted <24h but with UNDATED inventory should be 3 (s3,s6,s8), got %s; ', got); END IF;

    -- ---- Every classification bucket, by exact count. ----------------------
    -- These read census_classified, THE VIEW THE CENSUS ITSELF REPORTS FROM.
    --
    -- An earlier version of this harness re-derived the classification CASE
    -- here in its own CREATE TABLE AS. That made every assertion below a test
    -- of the copy: the census's CASE was edited to a known-wrong ordering and
    -- this file still exited 0, because the copy was still right. Asserting
    -- against the census's own view is the whole point -- if the two ever drift
    -- apart again, the harness is testing nothing.
    SELECT count(*) INTO got FROM census_classified WHERE tenant_id = proof_tenant AND bucket = 'unknown: no inventory';
    IF got <> 2 THEN fail := fail || format('bucket "unknown: no inventory" should be 2 (s7,s8), got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_classified WHERE tenant_id = proof_tenant AND bucket = 'builder active';
    IF got <> 7 THEN fail := fail || format('bucket "builder active" should be 7 (s1,s2,s3,s4,s10,s11,s13), got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_classified WHERE tenant_id = proof_tenant AND bucket = 'indeterminate: partial inventory';
    IF got <> 1 THEN fail := fail || format('bucket "indeterminate" should be 1 (s6), got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_classified WHERE tenant_id = proof_tenant AND bucket = 'builder installed, not active';
    IF got <> 2 THEN fail := fail || format('bucket "installed not active" should be 2 (s9,s12), got %s; ', got); END IF;

    -- The one that matters most: exactly ONE seeded site truly runs no builder.
    SELECT count(*) INTO got FROM census_classified WHERE tenant_id = proof_tenant AND bucket = 'Gutenberg (no builder present)';
    IF got <> 1 THEN fail := fail || format('bucket "Gutenberg" should be EXACTLY 1 (s5 only). Got %s — a site with unknown or partial inventory is being scored as Gutenberg; ', got); END IF;

    -- s13 specifically: a themes-only site with an ACTIVE builder must be
    -- 'builder active', not 'unknown'. This fails if the CASE ever tests
    -- inventory completeness before testing for a positive hit.
    SELECT count(*) INTO got FROM census_classified
      WHERE tenant_id = proof_tenant AND id = 'a0000000-0000-0000-0000-00000000000d' AND bucket = 'builder active';
    IF got <> 1 THEN fail := fail || 'themes-only s13 with active Bricks must classify "builder active", not be discarded as unknown; '; END IF;

    -- ---- Target matching, plugins AND themes ------------------------------
    SELECT count(DISTINCT site_id) INTO got FROM census_hits WHERE tenant_id = proof_tenant AND target='Bricks' AND active;
    IF got <> 2 THEN fail := fail || format('Bricks (theme) active should be 2 (s2,s13), got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_hits WHERE tenant_id = proof_tenant AND target='Divi' AND active;
    IF got <> 1 THEN fail := fail || format('Divi (capital-D theme dir) active should be 1, got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_hits WHERE tenant_id = proof_tenant AND target='WPBakery' AND active;
    IF got <> 1 THEN fail := fail || format('WPBakery (js_composer) active should be 1, got %s; ', got); END IF;

    SELECT count(DISTINCT site_id) INTO got FROM census_hits WHERE tenant_id = proof_tenant AND target='Elementor' AND active;
    IF got <> 3 THEN fail := fail || format('Elementor active should be 3 (s1,s10,s11), got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_hits WHERE tenant_id = proof_tenant AND target='WooCommerce' AND active;
    IF got <> 1 THEN fail := fail || format('WooCommerce active should be 1, got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_hits WHERE tenant_id = proof_tenant AND target='Yoast SEO' AND active;
    IF got <> 1 THEN fail := fail || format('Yoast (wordpress-seo) active should be 1, got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_hits WHERE tenant_id = proof_tenant AND target='ACF' AND active;
    IF got <> 1 THEN fail := fail || format('ACF active should be 1, got %s; ', got); END IF;

    -- ---- Per-target denominators (PR #552 review, thread 1) ---------------
    -- A theme-shipped target must NOT be divided by the plugin denominator. Of
    -- the 13 seeded sites, 10 sent a themes array and 10 sent a plugins array,
    -- but they are not the same 10: s6 is plugins-only and s13 is themes-only.
    -- Dividing Bricks by the plugin denominator would count s6 — a site that
    -- could never have reported a theme — against Bricks' adoption.
    SELECT ships_plugin::int + ships_theme::int * 2 INTO got FROM census_target_kinds WHERE target = 'Bricks';
    IF got <> 2 THEN fail := fail || format('Bricks must be classified theme-only (expected 2 = theme bit), got %s; ', got); END IF;

    SELECT ships_plugin::int + ships_theme::int * 2 INTO got FROM census_target_kinds WHERE target = 'Divi';
    IF got <> 3 THEN fail := fail || format('Divi must be classified plugin+theme (expected 3), got %s; ', got); END IF;

    SELECT ships_plugin::int + ships_theme::int * 2 INTO got FROM census_target_kinds WHERE target = 'Elementor';
    IF got <> 1 THEN fail := fail || format('Elementor must be classified plugin-only (expected 1), got %s; ', got); END IF;

    SELECT ships_plugin::int + ships_theme::int * 2 INTO got FROM census_target_kinds WHERE target = 'Beaver Builder';
    IF got <> 3 THEN fail := fail || format('Beaver Builder must be classified plugin+theme (expected 3), got %s; ', got); END IF;

    -- ---- Zero-adoption targets must be PRINTED, not dropped ---------------
    -- census_adoption is driven from the target list with a LEFT JOIN, so a
    -- target nobody runs still gets a row. An inner join against census_hits
    -- would drop it, and then "nobody runs Oxygen" and "the Oxygen slug is
    -- wrong and never matched anything" become indistinguishable.
    --
    -- These are deliberately NOT tenant-scoped: census_adoption aggregates
    -- fleet-wide by construction, so the properties asserted here are the
    -- structural ones, which hold whatever else the database contains.
    SELECT count(*) INTO got FROM census_adoption;
    IF got <> (SELECT count(DISTINCT target) FROM census_targets) THEN
        fail := fail || format('census_adoption must have exactly one row per configured target (%s), got %s -- a target is being dropped or duplicated; ',
                               (SELECT count(DISTINCT target) FROM census_targets), got);
    END IF;

    -- Oxygen is a configured target that no fixture installs. It must appear.
    SELECT count(*) INTO got FROM census_adoption WHERE target = 'Oxygen';
    IF got <> 1 THEN fail := fail || format('zero-adoption target Oxygen must still appear exactly once, got %s rows; ', got); END IF;

    SELECT sites_active INTO got FROM census_adoption WHERE target = 'Oxygen';
    IF got <> 0 THEN fail := fail || format('Oxygen should have 0 active sites, got %s; ', got); END IF;

    -- ...and it must carry a real denominator, or the 0 is unreadable: "0 of 0"
    -- says nothing, "0 of 10 that could have reported it" is a finding.
    SELECT denom_sites INTO got FROM census_adoption WHERE target = 'Oxygen';
    IF got IS NULL OR got = 0 THEN fail := fail || format('zero-adoption target must still carry its applicable denominator, got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_adoption WHERE denom_sites IS NULL OR ships_as IS NULL OR sites_active IS NULL;
    IF got <> 0 THEN fail := fail || format('%s census_adoption row(s) have NULL where a LEFT JOIN should have COALESCEd to 0; ', got); END IF;

    -- ---- Exclusions and parsing -------------------------------------------
    -- The archived site carried an active Elementor. If it leaked in, the
    -- Elementor count above would be 4.
    SELECT count(*) INTO got FROM census_fleet WHERE tenant_id = proof_tenant AND connection_state='archived';
    IF got <> 0 THEN fail := fail || format('archived sites must be excluded, got %s; ', got); END IF;

    -- The unparsed-version bucket must survive rather than be dropped.
    SELECT count(*) INTO got FROM census_hits
      WHERE tenant_id = proof_tenant AND target='Elementor' AND active AND version_minor IS NULL;
    IF got <> 1 THEN fail := fail || format('one Elementor should have an unparsed version, got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_hits
      WHERE tenant_id = proof_tenant AND target='Elementor' AND active AND version_minor='3.21';
    IF got <> 1 THEN fail := fail || format('one Elementor should group to 3.21, got %s; ', got); END IF;

    -- ---- Multisite exposure (the #558 caveat, sized) ----------------------
    SELECT count(*) INTO got FROM census_fleet WHERE tenant_id = proof_tenant AND multisite;
    IF got <> 1 THEN fail := fail || format('multisite sites should be 1 (s9), got %s; ', got); END IF;

    SELECT count(DISTINCT site_id) INTO got FROM census_hits
      WHERE tenant_id = proof_tenant AND multisite AND category='builder' AND NOT active;
    IF got <> 1 THEN fail := fail || format('multisite sites with an INACTIVE builder should be 1 (s9), got %s; ', got); END IF;

    IF fail <> '' THEN RAISE EXCEPTION 'PROOF FAILED: %', fail; END IF;
    RAISE NOTICE 'ALL ASSERTIONS PASSED';
END $$;

-- ---------------------------------------------------------------------------
-- RLS, ASSERTED — not merely printed. (PR #552 review, thread 2.)
--
-- Read as wpmgr_app: NOSUPERUSER, NOBYPASSRLS, the role every install actually
-- runs as, through the real tenant-isolation policy. A proof that only ever
-- runs as superuser leaves the RLS policies inert.
--
-- These queries hit `sites` DIRECTLY and deliberately not census_fleet. The
-- census's temp views are owned by the role that ran the census (the BYPASSRLS
-- owner) and are not granted to wpmgr_app, so reading one here raises
-- "permission denied for view census_fleet" — verified, not assumed. Were a
-- grant ever added, it would be worse than the error: PostgreSQL defaults views
-- to security_invoker = false, so the view would execute with the OWNER's
-- rights and report the owner's row count while appearing to test the app role.
-- Either way the view proves nothing about RLS. The table does.
--
-- The counts are asserted inside a DO block so a policy or role regression
-- RAISES and psql exits non-zero. Printed counts alone let the script exit 0
-- while showing the wrong numbers, which is a guard that fails open.
-- ---------------------------------------------------------------------------
\echo ''
\echo '### RLS: asserted as wpmgr_app under sites_tenant_isolation ###'
SET LOCAL ROLE wpmgr_app;

SELECT set_config('app.tenant_id', 'c5e75000-0000-4000-8000-0000000c5e75', true) AS scoped;
SELECT current_user AS running_as,
       count(*)     AS sites_visible_to_app_role
FROM sites WHERE connection_state <> 'archived';

DO $$
DECLARE got bigint; fail text := '';
BEGIN
    IF current_user <> 'wpmgr_app' THEN
        RAISE EXCEPTION 'RLS PROOF BROKEN: expected to be running as wpmgr_app, am %. The role switch failed, so nothing below tests RLS.', current_user;
    END IF;

    -- Scoped to the proof tenant: the 13 non-archived seeds are visible.
    PERFORM set_config('app.tenant_id', 'c5e75000-0000-4000-8000-0000000c5e75', true);
    SELECT count(*) INTO got FROM sites WHERE connection_state <> 'archived';
    IF got <> 13 THEN fail := fail || format('wpmgr_app WITH the tenant GUC should see 13 non-archived seeded sites, got %s; ', got); END IF;

    SELECT count(*) INTO got FROM sites;
    IF got <> 14 THEN fail := fail || format('wpmgr_app WITH the tenant GUC should see 14 sites including the archived one, got %s; ', got); END IF;

    -- GUC cleared: tenant isolation must admit NOTHING. If this ever returns a
    -- row, sites_tenant_isolation is not doing its job and every number this
    -- census prints for a single org is suspect.
    PERFORM set_config('app.tenant_id', '', true);
    SELECT count(*) INTO got FROM sites;
    IF got <> 0 THEN fail := fail || format('wpmgr_app with NO tenant GUC must see 0 sites, got %s — tenant isolation is not holding; ', got); END IF;

    IF fail <> '' THEN RAISE EXCEPTION 'RLS PROOF FAILED: %', fail; END IF;
    RAISE NOTICE 'RLS ASSERTIONS PASSED (as %)', current_user;
END $$;

RESET ROLE;

ROLLBACK;

\echo ''
\echo '### ROLLED BACK — nothing persisted ###'
