-- ===========================================================================
-- Fleet software census — S2 "measure the fleet"
-- ===========================================================================
-- Answers: for each integration target, how many sites run it, at which
-- versions, and what fraction of the fleet is that?
--
-- READ-ONLY. Every statement is a SELECT. Nothing here writes.
--
--   psql "$APP_DSN"   -v tenant_id=<uuid> -f fleet_software_census.sql   # one org
--   psql "$OWNER_DSN"                     -f fleet_software_census.sql   # fleet-wide
--
-- See README.md in this directory for the RLS/role rules: `sites` is FORCE RLS,
-- so fleet-wide needs BYPASSRLS and the app role alone returns zero rows.
--
-- ---------------------------------------------------------------------------
-- WHAT THE DATA ACTUALLY IS (read before trusting any number this prints)
-- ---------------------------------------------------------------------------
-- There is no normalized plugin/theme table. The whole inventory is one JSONB
-- document per site, sites.components, written by UpdateSiteMetadata
-- (db/query/sites.sql) which REWRITES the document wholesale on every push.
--
-- !! INVENTORY AGE IS NOT MEASURABLE IN THIS SCHEMA. READ THIS BEFORE QUOTING
-- !! ANY "stale" NUMBER THIS SCRIPT PRINTS.
--
-- There is no components_updated_at column. UpdateSiteMetadata does stamp
-- last_seen_at alongside components -- but TouchSiteHeartbeat
-- (db/query/site_connection.sql) ALSO stamps last_seen_at, every 60 seconds,
-- WITHOUT touching components. So last_seen_at is "when the agent last said
-- hello", not "when the inventory was last refreshed".
--
-- The two diverge in the dangerous direction: a site whose 60s heartbeat is
-- alive but whose 30-minute metadata cron has died reports last_seen_at =
-- seconds ago while serving a components document that is arbitrarily old.
-- last_seen_at therefore OVERSTATES inventory freshness and can never
-- understate it. The columns below named *_stale are a LOWER BOUND on staleness
-- and a site absent from them is not thereby fresh.
--
-- Closing this gap needs a schema change (a components_updated_at stamped only
-- by UpdateSiteMetadata). That is a migration, and it is not in this read-only
-- script's scope.
--
-- Shape, built control-plane side in internal/site/service.go buildInventoryPayload:
--   {"plugins":[{"slug","name","version","active",...}],
--    "themes": [{"slug","name","version","active",...}], ...}
--
-- Four properties of that document drive the whole design below:
--
--   1. `slug` for a plugin is the PLUGIN FILE PATH ("elementor/elementor.php"),
--      not a directory slug. The directory is split_part(slug,'/',1). For a
--      theme it is the stylesheet directory ("Divi"), and its case is the
--      theme author's — hence lower() on both sides of every comparison.
--
--   2. Page builders are NOT all plugins. Bricks and Divi ship as THEMES.
--      A plugins-only census reports zero Bricks and zero Divi and is wrong in
--      exactly the direction that flatters the "Gutenberg first" hypothesis.
--      This script matches plugins and themes in one pass.
--
--   3. `active` comes from get_option('active_plugins'), which on MULTISITE
--      does not contain network-activated plugins. A network-activated builder
--      is therefore reported active=false while running on every site in the
--      network. Multisite rows are counted but reported separately, because
--      their `active` flag is not trustworthy.
--
--   4. A site can have a plugins array and no themes array. That site cannot be
--      called Gutenberg — an unseen themes array could hold Bricks or Divi. It
--      is classified 'indeterminate', never folded into the no-builder bucket.
--
-- The single largest correctness rule here: "no builder found" is only a
-- finding for a site whose inventory we actually have. Sites with no usable
-- inventory are counted in their own bucket and are NEVER counted as Gutenberg.
-- Folding them in would inflate the exact number the roadmap is betting on.
-- ===========================================================================

\set ON_ERROR_STOP on
\timing off
\pset pager off

\if :{?tenant_id}
\else
  \set tenant_id ''
\endif

\if :{?stale_days}
\else
  \set stale_days 7
\endif

-- Scope the session to one tenant when a tenant_id was supplied, so this runs
-- as wpmgr_app under sites_tenant_isolation. Empty string = fleet-wide, which
-- needs BYPASSRLS.
SELECT set_config('app.tenant_id', :'tenant_id', false) AS app_tenant_id_set
WHERE :'tenant_id' <> '';

-- ---------------------------------------------------------------------------
-- The shared view of the fleet. Everything below selects from these.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE TEMP VIEW census_fleet AS
SELECT
    s.id,
    s.tenant_id,
    s.multisite,
    s.last_seen_at,
    s.components,
    -- A usable plugins array. '{}' and {"plugins":{...}} are NOT usable;
    -- {"plugins":[]} IS usable and means "reported, no plugins".
    --
    -- COALESCE is load-bearing, not defensive noise. components DEFAULTs to
    -- '{}', so on a never-reported site `components -> 'plugins'` is SQL NULL
    -- and jsonb_typeof(NULL) is NULL — making this expression NULL rather than
    -- false. `WHEN NOT <null>` never matches, so without COALESCE such a site
    -- falls straight through the classification CASE below and lands in the
    -- ELSE branch: Gutenberg. That silently scores every site that has never
    -- reported anything as "runs no page builder" — inflating precisely the
    -- number the roadmap is betting on, using the sites that said nothing.
    -- Caught by the proof harness; see fleet_software_census_proof.sql sites 7 and 8.
    COALESCE(jsonb_typeof(s.components -> 'plugins') = 'array', false) AS has_plugin_inventory,
    COALESCE(jsonb_typeof(s.components -> 'themes')  = 'array', false) AS has_theme_inventory,
    -- Buckets of LAST AGENT CONTACT, not of inventory age. See the header note:
    -- the 60s heartbeat bumps last_seen_at without refreshing components.
    CASE
        WHEN s.last_seen_at IS NULL                       THEN 'never_contacted'
        WHEN s.last_seen_at > now() - interval '24 hours'  THEN 'contact_24h'
        WHEN s.last_seen_at > now() - interval '7 days'    THEN 'contact_week'
        WHEN s.last_seen_at > now() - interval '30 days'   THEN 'contact_month'
        ELSE 'contact_over_30d'
    END AS contact_freshness,
    -- LOWER BOUND on inventory staleness (see header). True here means the
    -- inventory is definitely at least this old; false does NOT mean fresh.
    (s.last_seen_at IS NULL
     OR s.last_seen_at < now() - (:stale_days || ' days')::interval) AS contact_stale,
    s.connection_state
FROM sites s
WHERE s.connection_state <> 'archived'
  -- NULLIF, not a bare :'tenant_id'::uuid. Postgres resolves a literal cast at
  -- parse time, so ''::uuid raises "invalid input syntax for type uuid" even
  -- though the OR short-circuits at runtime. NULLIF('','') is NULL, and
  -- NULL::uuid is legal.
  AND (:'tenant_id' = '' OR s.tenant_id = NULLIF(:'tenant_id', '')::uuid);

-- Integration targets. (target, category, kind, dir_slug) — one row per
-- distributed artifact, because a product can ship as several.
CREATE OR REPLACE TEMP VIEW census_targets AS
SELECT * FROM (VALUES
    -- Page builders (category 'builder' defines the Gutenberg complement).
    ('Elementor',       'builder', 'plugin', 'elementor'),
    ('Elementor',       'builder', 'plugin', 'elementor-pro'),
    ('WPBakery',        'builder', 'plugin', 'js_composer'),
    ('Bricks',          'builder', 'theme',  'bricks'),
    ('Divi',            'builder', 'theme',  'divi'),
    ('Divi',            'builder', 'plugin', 'divi-builder'),
    ('Beaver Builder',  'builder', 'plugin', 'bb-plugin'),
    ('Beaver Builder',  'builder', 'plugin', 'beaver-builder-lite-version'),
    ('Beaver Builder',  'builder', 'theme',  'bb-theme'),
    ('Breakdance',      'builder', 'plugin', 'breakdance'),
    ('Oxygen',          'builder', 'plugin', 'oxygen'),
    -- Also builders. Not integration targets, but they must suppress the
    -- Gutenberg bucket or "no page builder" silently absorbs them.
    ('Avada Fusion',    'builder', 'plugin', 'fusion-builder'),
    ('Brizy',           'builder', 'plugin', 'brizy'),
    ('SiteOrigin',      'builder', 'plugin', 'siteorigin-panels'),
    ('Thrive Architect','builder', 'plugin', 'thrive-visual-editor'),
    ('Themify',         'builder', 'plugin', 'themify-builder'),
    ('Visual Composer', 'builder', 'plugin', 'visualcomposer'),
    ('Live Composer',   'builder', 'plugin', 'live-composer-page-builder'),
    ('WP Page Builder', 'builder', 'plugin', 'wp-page-builder'),
    -- Commerce.
    ('WooCommerce',     'commerce','plugin', 'woocommerce'),
    -- SEO.
    ('Yoast SEO',       'seo',     'plugin', 'wordpress-seo'),
    ('Yoast SEO',       'seo',     'plugin', 'wordpress-seo-premium'),
    ('Rank Math',       'seo',     'plugin', 'seo-by-rank-math'),
    ('Rank Math',       'seo',     'plugin', 'seo-by-rank-math-pro'),
    ('SEOPress',        'seo',     'plugin', 'wp-seopress'),
    ('SEOPress',        'seo',     'plugin', 'wp-seopress-pro'),
    ('AIOSEO',          'seo',     'plugin', 'all-in-one-seo-pack'),
    ('AIOSEO',          'seo',     'plugin', 'all-in-one-seo-pack-pro'),
    -- Custom fields / content modelling.
    ('ACF',             'fields',  'plugin', 'advanced-custom-fields'),
    ('ACF',             'fields',  'plugin', 'advanced-custom-fields-pro'),
    ('Secure Custom Fields','fields','plugin','secure-custom-fields'),
    ('Pods',            'fields',  'plugin', 'pods'),
    ('Meta Box',        'fields',  'plugin', 'meta-box'),
    ('Toolset Types',   'fields',  'plugin', 'types'),
    ('Toolset Types',   'fields',  'plugin', 'toolset-types'),
    ('Carbon Fields',   'fields',  'plugin', 'carbon-fields'),
    ('JetEngine',       'fields',  'plugin', 'jet-engine')
) AS t(target, category, kind, dir_slug);

-- Every installed component on every in-scope site, flattened to one row each.
CREATE OR REPLACE TEMP VIEW census_components AS
SELECT f.id AS site_id, f.tenant_id, f.multisite, f.contact_freshness, f.contact_stale,
       'plugin'::text AS kind,
       lower(split_part(c ->> 'slug', '/', 1)) AS dir_slug,
       c ->> 'version'  AS raw_version,
       COALESCE((c ->> 'active')::boolean, false) AS active
FROM census_fleet f
CROSS JOIN LATERAL jsonb_array_elements(
    CASE WHEN f.has_plugin_inventory THEN f.components -> 'plugins' ELSE '[]'::jsonb END
) AS c
UNION ALL
SELECT f.id, f.tenant_id, f.multisite, f.contact_freshness, f.contact_stale,
       'theme'::text,
       lower(split_part(c ->> 'slug', '/', 1)),
       c ->> 'version',
       COALESCE((c ->> 'active')::boolean, false)
FROM census_fleet f
CROSS JOIN LATERAL jsonb_array_elements(
    CASE WHEN f.has_theme_inventory THEN f.components -> 'themes' ELSE '[]'::jsonb END
) AS c;

-- Matched target hits. `active` is taken at face value except on multisite,
-- where a network-activated plugin reports active=false (see note 3 above).
CREATE OR REPLACE TEMP VIEW census_hits AS
SELECT c.*, t.target, t.category,
       substring(c.raw_version from '^([0-9]+\.[0-9]+)') AS version_minor
FROM census_components c
JOIN census_targets t
  ON t.kind = c.kind AND t.dir_slug = c.dir_slug;

-- ===========================================================================
-- REFUSE ON AN EMPTY SCOPE.
-- A census over zero sites must not print a page of tidy zeroes that reads as
-- "nobody runs Elementor". Most likely causes: fleet-wide run as a NOBYPASSRLS
-- role, or a tenant_id that matches nothing.
-- ===========================================================================
DO $$
DECLARE n bigint;
BEGIN
    SELECT count(*) INTO n FROM census_fleet;
    IF n = 0 THEN
        RAISE EXCEPTION
            'census scope is empty: 0 sites visible. Either the tenant_id matches no site, or this is a fleet-wide run as a role without BYPASSRLS (sites is FORCE RLS). Refusing to report a fleet that does not exist.';
    END IF;
END $$;

\echo ''
\echo '=== 0. Inventory coverage — the honest denominator ==================='
-- Read this table first. Every percentage further down is a fraction of
-- `classifiable`, and this is where you see how much of the fleet that is.
SELECT
    count(*)                                                   AS sites_in_scope,
    count(*) FILTER (WHERE has_plugin_inventory)               AS has_plugin_inventory,
    count(*) FILTER (WHERE NOT has_plugin_inventory)           AS no_usable_inventory,
    count(*) FILTER (WHERE has_plugin_inventory
                       AND NOT has_theme_inventory)            AS plugins_but_no_themes,
    count(*) FILTER (WHERE multisite)                          AS multisite_sites,
    count(*) FILTER (WHERE contact_stale)                           AS stale_sites,
    round(100.0 * count(*) FILTER (WHERE has_plugin_inventory)
          / nullif(count(*), 0), 1)                            AS pct_with_inventory
FROM census_fleet;

\echo ''
\echo '=== 0b. Freshness distribution ======================================='
SELECT contact_freshness,
       connection_state,
       count(*) AS sites,
       round(100.0 * count(*) / nullif(sum(count(*)) OVER (), 0), 1) AS pct
FROM census_fleet
GROUP BY contact_freshness, connection_state
ORDER BY array_position(
    ARRAY['contact_24h','contact_week','contact_month','contact_over_30d',
          'never_contacted'], contact_freshness),
    connection_state;

\echo ''
\echo '=== 1. Builder classification (incl. unknown) ========================'
-- The Gutenberg question. Note the three non-builder buckets are distinct and
-- none of them is silently merged into "Gutenberg".
WITH classified AS (
    SELECT f.id, f.multisite, f.contact_stale,
           CASE
               WHEN NOT f.has_plugin_inventory THEN 'unknown: no inventory'
               WHEN EXISTS (SELECT 1 FROM census_hits h
                            WHERE h.site_id = f.id AND h.category = 'builder'
                              AND h.active)
                    THEN 'builder active'
               WHEN NOT f.has_theme_inventory
                    THEN 'indeterminate: no theme inventory'
               WHEN EXISTS (SELECT 1 FROM census_hits h
                            WHERE h.site_id = f.id AND h.category = 'builder')
                    THEN 'builder installed, not active'
               ELSE 'Gutenberg (no builder present)'
           END AS bucket
    FROM census_fleet f
)
SELECT bucket,
       count(*) AS sites,
       round(100.0 * count(*) / nullif(sum(count(*)) OVER (), 0), 1) AS pct_of_scope,
       count(*) FILTER (WHERE contact_stale)  AS of_which_stale,
       count(*) FILTER (WHERE multisite) AS of_which_multisite
FROM classified
GROUP BY bucket
ORDER BY sites DESC;

\echo ''
\echo '=== 2. Per-target adoption ==========================================='
-- pct_of_classifiable is deliberately NOT a fraction of all sites: a site whose
-- inventory we never received cannot testify for or against any target.
WITH denom AS (
    SELECT count(*) FILTER (WHERE has_plugin_inventory) AS classifiable
    FROM census_fleet
)
SELECT h.category,
       h.target,
       count(DISTINCT h.site_id) FILTER (WHERE h.active)     AS sites_active,
       count(DISTINCT h.site_id) FILTER (WHERE NOT h.active) AS sites_installed_inactive,
       round(100.0 * count(DISTINCT h.site_id) FILTER (WHERE h.active)
             / nullif((SELECT classifiable FROM denom), 0), 1) AS pct_of_classifiable,
       count(DISTINCT h.site_id) FILTER (WHERE h.active AND h.contact_stale)  AS active_but_stale,
       count(DISTINCT h.site_id) FILTER (WHERE h.active AND h.multisite) AS active_on_multisite
FROM census_hits h
GROUP BY h.category, h.target
ORDER BY h.category, sites_active DESC;

\echo ''
\echo '=== 3. Version spread (major.minor) =================================='
-- Grouped to major.minor; per-patch is noise. version_minor NULL means the
-- version string did not parse — the agent writes the literal "unknown" when a
-- plugin header omits Version, so this bucket is real and is shown, not dropped.
SELECT h.target,
       h.dir_slug,
       COALESCE(h.version_minor, '(unparsed)') AS version_minor,
       count(DISTINCT h.site_id) AS sites,
       count(DISTINCT h.site_id) FILTER (WHERE h.contact_stale) AS of_which_stale
FROM census_hits h
WHERE h.active
GROUP BY h.target, h.dir_slug, h.version_minor
ORDER BY h.target, sites DESC, version_minor;

\echo ''
\echo '=== 4. Builder co-installation ======================================='
-- How many sites carry more than one builder. A site with Elementor AND Divi
-- is a migration/hand-off case, and it is why section 1 counts each site once
-- by precedence rather than summing section 2.
WITH per_site AS (
    SELECT site_id, count(DISTINCT target) AS builders
    FROM census_hits WHERE category = 'builder' AND active
    GROUP BY site_id
)
SELECT builders AS active_builders_on_site, count(*) AS sites
FROM per_site GROUP BY builders ORDER BY builders;
