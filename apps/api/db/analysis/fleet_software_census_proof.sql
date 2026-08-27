-- ===========================================================================
-- Proof harness for fleet_software_census.sql
-- ===========================================================================
-- Seeds twelve sites covering every classification branch, runs the census,
-- asserts the bucket counts, then ROLLS BACK. Nothing is persisted.
--
--   psql "$OWNER_DSN" -f fleet_software_census_proof.sql
--
-- The census's own guard is proven separately by running it against an empty
-- scope: it must RAISE and exit non-zero rather than print zeroes.
--
-- Every seeded site is annotated with the bucket it must land in. If you change
-- the classification CASE in the census, this file must move with it.
-- ===========================================================================

\set ON_ERROR_STOP on
\pset pager off

BEGIN;

INSERT INTO tenants (id, name, slug)
VALUES ('c5e75000-0000-4000-8000-0000000c5e75', 'Census Proof Org', 'census-proof-org-s2');

-- 1. Elementor active as a PLUGIN.                    -> builder active
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-000000000001','c5e75000-0000-4000-8000-0000000c5e75','https://s1.example','s1-elementor','connected', now(), false, 'hello-elementor',
 '{"plugins":[{"slug":"elementor/elementor.php","name":"Elementor","version":"3.21.5","active":true},
              {"slug":"akismet/akismet.php","name":"Akismet","version":"5.3.1","active":false}],
   "themes":[{"slug":"hello-elementor","name":"Hello","version":"3.0.1","active":true}]}'::jsonb);

-- 2. Bricks active as a THEME. A plugins-only census scores this Gutenberg.
--                                                     -> builder active
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-000000000002','c5e75000-0000-4000-8000-0000000c5e75','https://s2.example','s2-bricks','connected', now(), false, 'bricks',
 '{"plugins":[{"slug":"akismet/akismet.php","name":"Akismet","version":"5.3.1","active":true}],
   "themes":[{"slug":"bricks","name":"Bricks","version":"1.9.2","active":true}]}'::jsonb);

-- 3. Divi, whose stylesheet directory is capital-D "Divi".
--                                                     -> builder active
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-000000000003','c5e75000-0000-4000-8000-0000000c5e75','https://s3.example','s3-divi','connected', now(), false, 'Divi',
 '{"plugins":[],
   "themes":[{"slug":"Divi","name":"Divi","version":"4.23.1","active":true}]}'::jsonb);

-- 4. WPBakery, whose plugin directory is "js_composer".
--                                                     -> builder active
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-000000000004','c5e75000-0000-4000-8000-0000000c5e75','https://s4.example','s4-wpbakery','connected', now(), false, 'twentytwentyone',
 '{"plugins":[{"slug":"js_composer/js_composer.php","name":"WPBakery","version":"6.13.0","active":true}],
   "themes":[{"slug":"twentytwentyone","name":"TT1","version":"2.0","active":true}]}'::jsonb);

-- 5. No builder at all, full inventory. Also the Woo + Yoast fixture.
--                                                     -> Gutenberg
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-000000000005','c5e75000-0000-4000-8000-0000000c5e75','https://s5.example','s5-gutenberg','connected', now(), false, 'twentytwentyfour',
 '{"plugins":[{"slug":"woocommerce/woocommerce.php","name":"WooCommerce","version":"8.1.2","active":true},
              {"slug":"wordpress-seo/wp-seo.php","name":"Yoast SEO","version":"21.5","active":true},
              {"slug":"advanced-custom-fields/acf.php","name":"ACF","version":"6.2.4","active":true}],
   "themes":[{"slug":"twentytwentyfour","name":"TT4","version":"1.1","active":true}]}'::jsonb);

-- 6. Plugins array present, THEMES KEY ABSENT. Cannot be called Gutenberg
--    because an unseen theme could be Bricks or Divi.
--                                                     -> indeterminate
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-000000000006','c5e75000-0000-4000-8000-0000000c5e75','https://s6.example','s6-nothemes','connected', now(), false, '',
 '{"plugins":[{"slug":"elementor/elementor.php","name":"Elementor","version":"3.20.0","active":false}]}'::jsonb);

-- 7. Never reported anything.                         -> unknown: no inventory
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-000000000007','c5e75000-0000-4000-8000-0000000c5e75','https://s7.example','s7-empty','pending_enrollment', NULL, false, '', '{}'::jsonb);

-- 8. Malformed plugins key (the shape sites.sql already guards against).
--                                                     -> unknown: no inventory
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-000000000008','c5e75000-0000-4000-8000-0000000c5e75','https://s8.example','s8-badshape','connected', now(), false, '',
 '{"plugins":{"unexpected":"shape"}}'::jsonb);

-- 9. MULTISITE with a network-activated Elementor, which the agent reports
--    active=false. Real state is "active"; we can only see "installed".
--                                                     -> builder installed, not active
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-000000000009','c5e75000-0000-4000-8000-0000000c5e75','https://s9.example','s9-multisite','connected', now(), true, 'twentytwentyfour',
 '{"plugins":[{"slug":"elementor/elementor.php","name":"Elementor","version":"3.21.5","active":false}],
   "themes":[]}'::jsonb);

-- 10. Elementor active but last contact 60 days ago.  -> builder active, stale
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-00000000000a','c5e75000-0000-4000-8000-0000000c5e75','https://s10.example','s10-stale','disconnected', now() - interval '60 days', false, 'hello-elementor',
 '{"plugins":[{"slug":"elementor/elementor.php","name":"Elementor","version":"3.18.0","active":true}],
   "themes":[]}'::jsonb);

-- 11. Version header missing; the agent writes the literal "unknown".
--                                                     -> builder active, (unparsed)
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-00000000000b','c5e75000-0000-4000-8000-0000000c5e75','https://s11.example','s11-unknownver','connected', now(), false, 'hello-elementor',
 '{"plugins":[{"slug":"elementor/elementor.php","name":"Elementor","version":"unknown","active":true}],
   "themes":[]}'::jsonb);

-- 12. Breakdance installed but switched off.          -> builder installed, not active
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-00000000000c','c5e75000-0000-4000-8000-0000000c5e75','https://s12.example','s12-inactive','connected', now(), false, 'twentytwentyfour',
 '{"plugins":[{"slug":"breakdance/plugin.php","name":"Breakdance","version":"1.7.0","active":false}],
   "themes":[{"slug":"twentytwentyfour","name":"TT4","version":"1.1","active":true}]}'::jsonb);

-- An archived site, which the census must EXCLUDE entirely.
INSERT INTO sites (id, tenant_id, url, name, connection_state, last_seen_at, multisite, active_theme, components) VALUES
('a0000000-0000-0000-0000-0000000000ff','c5e75000-0000-4000-8000-0000000c5e75','https://sX.example','sX-archived','archived', now(), false, 'x',
 '{"plugins":[{"slug":"elementor/elementor.php","name":"Elementor","version":"3.21.5","active":true}],"themes":[]}'::jsonb);

\echo '### Running census as the OWNER (BYPASSRLS), fleet-wide ###'
\i fleet_software_census.sql

-- ---------------------------------------------------------------------------
-- Assertions. Each is the classification the seed comments promise.
-- ---------------------------------------------------------------------------
\echo ''
\echo '### ASSERTIONS ###'
DO $$
DECLARE
    got  bigint;
    fail text := '';
    PROCEDURE_note text;
BEGIN
    SELECT count(*) INTO got FROM census_fleet;
    IF got <> 12 THEN fail := fail || format('scope should exclude the archived site and be 12, got %s; ', got); END IF;

    -- ---- Inventory-usability flags must be BOOLEAN, never NULL. -------------
    -- These four are the regression guard for the three-valued-logic bug:
    -- jsonb_typeof(NULL) is NULL, so without COALESCE a never-reported site
    -- (components = '{}', the column default) has has_plugin_inventory = NULL,
    -- slips past `WHEN NOT ...` and is classified Gutenberg.
    SELECT count(*) INTO got FROM census_fleet WHERE has_plugin_inventory IS NULL;
    IF got <> 0 THEN fail := fail || format('has_plugin_inventory must never be NULL, got %s NULL rows; ', got); END IF;

    SELECT count(*) INTO got FROM census_fleet WHERE has_theme_inventory IS NULL;
    IF got <> 0 THEN fail := fail || format('has_theme_inventory must never be NULL, got %s NULL rows; ', got); END IF;

    SELECT count(*) INTO got FROM census_fleet WHERE NOT has_plugin_inventory;
    IF got <> 2 THEN fail := fail || format('sites with no usable inventory should be 2 (s7 empty, s8 malformed), got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_fleet WHERE has_plugin_inventory AND NOT has_theme_inventory;
    IF got <> 1 THEN fail := fail || format('plugins-but-no-themes should be 1 (s6), got %s; ', got); END IF;

    -- ---- Every classification bucket, by exact count. ----------------------
    -- Re-derives the census CASE. If the CASE changes, this must change too.
    CREATE TEMP TABLE census_proof_buckets ON COMMIT DROP AS
    SELECT f.id,
           CASE
               WHEN NOT f.has_plugin_inventory THEN 'unknown: no inventory'
               WHEN EXISTS (SELECT 1 FROM census_hits h
                            WHERE h.site_id = f.id AND h.category = 'builder' AND h.active)
                    THEN 'builder active'
               WHEN NOT f.has_theme_inventory THEN 'indeterminate: no theme inventory'
               WHEN EXISTS (SELECT 1 FROM census_hits h
                            WHERE h.site_id = f.id AND h.category = 'builder')
                    THEN 'builder installed, not active'
               ELSE 'Gutenberg (no builder present)'
           END AS bucket
    FROM census_fleet f;

    SELECT count(*) INTO got FROM census_proof_buckets WHERE bucket = 'unknown: no inventory';
    IF got <> 2 THEN fail := fail || format('bucket "unknown: no inventory" should be 2 (s7,s8), got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_proof_buckets WHERE bucket = 'builder active';
    IF got <> 6 THEN fail := fail || format('bucket "builder active" should be 6 (s1,s2,s3,s4,s10,s11), got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_proof_buckets WHERE bucket = 'indeterminate: no theme inventory';
    IF got <> 1 THEN fail := fail || format('bucket "indeterminate" should be 1 (s6), got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_proof_buckets WHERE bucket = 'builder installed, not active';
    IF got <> 2 THEN fail := fail || format('bucket "installed not active" should be 2 (s9,s12), got %s; ', got); END IF;

    -- The one that matters most: exactly ONE seeded site truly runs no builder.
    SELECT count(*) INTO got FROM census_proof_buckets WHERE bucket = 'Gutenberg (no builder present)';
    IF got <> 1 THEN fail := fail || format('bucket "Gutenberg" should be EXACTLY 1 (s5 only). Got %s — a site with unknown or partial inventory is being scored as Gutenberg; ', got); END IF;

    SELECT count(*) INTO got FROM census_hits WHERE target='Bricks' AND active;
    IF got <> 1 THEN fail := fail || format('Bricks (theme) active should be 1, got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_hits WHERE target='Divi' AND active;
    IF got <> 1 THEN fail := fail || format('Divi (capital-D theme dir) active should be 1, got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_hits WHERE target='WPBakery' AND active;
    IF got <> 1 THEN fail := fail || format('WPBakery (js_composer) active should be 1, got %s; ', got); END IF;

    SELECT count(DISTINCT site_id) INTO got FROM census_hits WHERE target='Elementor' AND active;
    IF got <> 3 THEN fail := fail || format('Elementor active should be 3 (s1,s10,s11), got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_hits WHERE target='WooCommerce' AND active;
    IF got <> 1 THEN fail := fail || format('WooCommerce active should be 1, got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_hits WHERE target='Yoast SEO' AND active;
    IF got <> 1 THEN fail := fail || format('Yoast (wordpress-seo) active should be 1, got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_hits WHERE target='ACF' AND active;
    IF got <> 1 THEN fail := fail || format('ACF active should be 1, got %s; ', got); END IF;

    -- The archived site carried an active Elementor. If it leaked in, the
    -- Elementor count above would be 4.
    SELECT count(*) INTO got FROM census_fleet WHERE connection_state='archived';
    IF got <> 0 THEN fail := fail || format('archived sites must be excluded, got %s; ', got); END IF;

    -- The unparsed-version bucket must survive rather than be dropped.
    SELECT count(*) INTO got FROM census_hits
      WHERE target='Elementor' AND active AND version_minor IS NULL;
    IF got <> 1 THEN fail := fail || format('one Elementor should have an unparsed version, got %s; ', got); END IF;

    SELECT count(*) INTO got FROM census_hits
      WHERE target='Elementor' AND active AND version_minor='3.21';
    IF got <> 1 THEN fail := fail || format('one Elementor should group to 3.21, got %s; ', got); END IF;

    IF fail <> '' THEN RAISE EXCEPTION 'PROOF FAILED: %', fail; END IF;
    RAISE NOTICE 'ALL ASSERTIONS PASSED';
END $$;

-- ---------------------------------------------------------------------------
-- Same data, but read as wpmgr_app: NOSUPERUSER, NOBYPASSRLS, the role every
-- install actually runs as, through the real tenant-isolation policy. A proof
-- that only ever runs as superuser leaves the RLS policies inert.
-- ---------------------------------------------------------------------------
\echo ''
\echo '### RLS: same scope as wpmgr_app under sites_tenant_isolation ###'
SET LOCAL ROLE wpmgr_app;
SELECT set_config('app.tenant_id', 'c5e75000-0000-4000-8000-0000000c5e75', true) AS scoped;
SELECT current_user AS running_as,
       count(*)     AS sites_visible_to_app_role
FROM sites WHERE connection_state <> 'archived';

\echo ''
\echo '### RLS: wpmgr_app with NO tenant GUC must see nothing ###'
SELECT set_config('app.tenant_id', '', true) AS cleared;
SELECT current_user AS running_as,
       count(*)     AS sites_visible_without_guc
FROM sites;
RESET ROLE;

ROLLBACK;

\echo ''
\echo '### ROLLED BACK — nothing persisted ###'
