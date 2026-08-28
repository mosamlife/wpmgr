-- ===========================================================================
-- Fleet software census — S2 "measure the fleet"
-- ===========================================================================
-- Answers: for each integration target, how many sites run it, at which
-- versions, and what fraction of the fleet is that?
--
-- READ-ONLY, and that is PROVEN AT RUNTIME rather than asserted here. The proof
-- harness fingerprints every base table in `public` (row count plus a content
-- digest) and the transaction's tuple-write counters, before and after this
-- script runs, and fails if anything moved. Do not replace that with a grep for
-- write keywords: the anchored grep this file used to advertise reported CLEAN
-- with a live `UPDATE sites` planted in it, because the write followed another
-- statement on its line.
--
-- What it DOES do to the session, none of it persistent: creates TEMP views, and
-- sets app.tenant_id when a tenant_id is supplied. Both vanish on disconnect.
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
-- Shape, built control-plane side in internal/site/service.go buildInventoryPayload:
--   {"plugins":[{"slug","name","version","active",...}],
--    "themes": [{"slug","name","version","active",...}], ...}
--
-- Five properties of that document drive the whole design below.
--
--   1. `slug` for a plugin is the PLUGIN FILE PATH ("elementor/elementor.php"),
--      not a directory slug. The directory is split_part(slug,'/',1). For a
--      theme it is the stylesheet directory ("Divi"), and its case is the
--      theme author's — hence lower() on both sides of every comparison.
--
--   2. Page builders are NOT all plugins. Bricks and Divi ship as THEMES.
--      A plugins-only census reports zero Bricks and zero Divi and is wrong in
--      exactly the direction that flatters the "Gutenberg first" hypothesis.
--      This script matches plugins and themes in one pass, and — see section 4
--      — gives a theme-shipped target a THEME denominator, because a site that
--      never sent a themes array cannot testify about Bricks either way.
--
--   3. INVENTORY AGE IS sites.components_updated_at (m121, GH #553). Read the
--      next block; it is the single most misreadable number this script prints.
--
--   4. `active` ON MULTISITE DEPENDS ON THE AGENT VERSION. Read the block after
--      that one.
--
--   5. A site can have a plugins array and no themes array, or the reverse.
--      A POSITIVE finding from partial inventory is still true — an active
--      Bricks theme is an active builder whether or not we also got the plugin
--      list. A NEGATIVE finding is not: "no builder present" requires BOTH
--      arrays, because the missing one could hold Bricks or Divi. The
--      classification CASE in section 3 is ordered on exactly that asymmetry.
--
-- ---------------------------------------------------------------------------
-- INVENTORY AGE: components_updated_at, AND WHY MOST OF IT IS NULL TODAY
-- ---------------------------------------------------------------------------
--
-- m121 (20260823000000) added sites.components_updated_at, stamped by
-- UpdateSiteMetadata — the only statement in the tree that writes
-- sites.components — so the inventory now carries its own age.
--
-- WHAT THE TIMESTAMP MEANS, EXACTLY. It is now() evaluated ON THE CONTROL PLANE
-- at the moment it PERSISTED the inventory document. It is NOT the instant the
-- agent walked the plugin and theme list on the WordPress host. The agent's
-- metadata payload carries no collection timestamp, so the collection instant is
-- simply not knowable here, and m121 named the column components_updated_at
-- rather than components_collected_at precisely so this script cannot quietly
-- claim otherwise (m121 DECISION 2).
--
-- The gap between the two is one agent push — normally a single HTTP round trip,
-- and NOT small when it matters: a queued or retried push, a clock-skewed host,
-- or any future store-and-forward path widens it without warning. So every
-- freshness number below is "how long since WE RECORDED this", which is a lower
-- bound on "how long since the agent LOOKED". Like every other error in this
-- script it runs one way: the inventory can be older than it reports here, never
-- newer.
--
-- This replaces last_seen_at, which was never inventory age: TouchSiteHeartbeat
-- (db/query/site_connection.sql) bumps last_seen_at every 60 seconds WITHOUT
-- touching components, so it overstated freshness and could never understate
-- it. last_seen_at still appears below, in section 2 only, answering the
-- different and still-useful question "is the agent alive at all".
--
-- !! m121 DELIBERATELY DID NOT BACKFILL. Every row that existed when it applied
-- !! has components_updated_at = NULL, and NULL means EXACTLY "we have never
-- !! recorded when this inventory was collected". A site leaves that state only
-- !! on its next metadata push (agent-driven, ~30 minute cron). So a census run
-- !! soon after m121 is dominated by NULL, and that is a true statement about
-- !! our knowledge, not a defect to be tidied away.
--
-- Therefore NULL age is ITS OWN BUCKET everywhere in this script. It is never
-- folded into "fresh", never folded into "stale", and never silently dropped by
-- a three-valued-logic filter. The two derived flags are deliberately asymmetric:
--
--   inventory_age_known       true iff components_updated_at IS NOT NULL.
--   inventory_provably_stale  true ONLY when the stamp exists AND is older than
--                             :stale_days. It is FALSE for a NULL-age site —
--                             so `NOT inventory_provably_stale` does NOT mean
--                             fresh, it means "fresh or unknown". Every report
--                             below that shows a stale count shows the
--                             age-unknown count beside it for that reason.
--
-- Section 0 exists so no number below can be quoted without its denominator.
--
-- ---------------------------------------------------------------------------
-- MULTISITE `active` IS A MIXTURE UNTIL THE FLEET UPDATES
-- ---------------------------------------------------------------------------
--
-- On multisite, a network-activated plugin does not appear in
-- get_option('active_plugins'); it lives in
-- get_site_option('active_sitewide_plugins'), a different option with a
-- different shape. The agent originally read only the first, so a builder
-- network-activated across a multisite reported active=false on every site in
-- the network — invisible on exactly the installs where it mattered most.
--
-- PR #558 fixed the agent: MetadataCommand::plugins() now unions both sources.
-- But the fix is IN THE AGENT, and agents update on their own schedule. A fleet
-- measured today is a MIXTURE of both behaviours, and nothing in the components
-- document records which agent version wrote it. So for multisite rows:
--
--   active=true  is trustworthy in both versions (no false positives).
--   active=false is trustworthy ONLY from a post-#558 agent. From an older one
--                it may be a network-activated plugin being under-reported.
--
-- The error therefore runs one way: multisite builder adoption is a LOWER
-- BOUND, and it will rise as agents update without anyone installing anything.
-- Single-site rows are unaffected — active_plugins is complete there.
--
-- Section 7 prints the multisite exposure so the size of that caveat is a
-- number and not an adjective. Sections 3 and 4 keep multisite counts in their
-- own columns rather than merging them into a single adoption figure.
--
-- ---------------------------------------------------------------------------
-- The single largest correctness rule
-- ---------------------------------------------------------------------------
-- "No builder found" is only a finding for a site whose inventory we actually
-- have. Sites with no usable inventory are counted in their own bucket and are
-- NEVER counted as Gutenberg. Folding them in would inflate the exact number
-- the roadmap is betting on, using the sites that said nothing.
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
    s.components_updated_at,
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

    -- ---- INVENTORY AGE (m121). NULL is a bucket, not a rounding error. -----
    -- True iff we have ever recorded when this inventory was collected. Every
    -- freshness figure in this script is a fraction of the sites where this is
    -- true, and section 0 prints that denominator first for that reason.
    (s.components_updated_at IS NOT NULL) AS inventory_age_known,
    CASE
        WHEN s.components_updated_at IS NULL                            THEN 'age_unknown (never recorded)'
        WHEN s.components_updated_at > now() - interval '24 hours'      THEN 'inventory_24h'
        WHEN s.components_updated_at > now() - interval '7 days'        THEN 'inventory_week'
        WHEN s.components_updated_at > now() - interval '30 days'       THEN 'inventory_month'
        ELSE 'inventory_over_30d'
    END AS inventory_freshness,
    -- PROVABLY stale: the stamp exists AND is older than :stale_days. COALESCE
    -- to false for a NULL stamp, so this flag never carries SQL NULL into a
    -- filter. Read it with inventory_age_known beside it: false here means
    -- "fresh OR unknown", never "fresh".
    COALESCE(
        s.components_updated_at < now() - (:stale_days || ' days')::interval,
        false) AS inventory_provably_stale,

    -- ---- AGENT LIVENESS. A DIFFERENT QUESTION. Section 2 only. ------------
    -- last_seen_at is bumped by the 60s heartbeat and says nothing about when
    -- the inventory was refreshed. Kept because "the agent is gone" is worth
    -- knowing on its own; never used as an inventory age.
    CASE
        WHEN s.last_seen_at IS NULL                        THEN 'never_contacted'
        WHEN s.last_seen_at > now() - interval '24 hours'  THEN 'contact_24h'
        WHEN s.last_seen_at > now() - interval '7 days'    THEN 'contact_week'
        WHEN s.last_seen_at > now() - interval '30 days'   THEN 'contact_month'
        ELSE 'contact_over_30d'
    END AS last_contact_freshness,
    s.connection_state
FROM sites s
WHERE s.connection_state <> 'archived'
  -- NULLIF, not a bare :'tenant_id'::uuid. Postgres resolves a literal cast at
  -- parse time, so ''::uuid raises "invalid input syntax for type uuid" even
  -- though the OR short-circuits at runtime. NULLIF('','') is NULL, and
  -- NULL::uuid is legal.
  AND (:'tenant_id' = '' OR s.tenant_id = NULLIF(:'tenant_id', '')::uuid);

-- Integration targets. (target, category, kind, dir_slug) — one row per
-- distributed artifact, because a product can ship as several. `kind` is also
-- what decides each target's denominator in section 4.
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

-- Which artifact kinds each TARGET ships as. Divi is a theme AND a plugin;
-- Bricks is a theme only; Elementor is a plugin only. Section 4 uses this to
-- pick a denominator per target instead of one global one.
CREATE OR REPLACE TEMP VIEW census_target_kinds AS
SELECT target,
       bool_or(kind = 'plugin') AS ships_plugin,
       bool_or(kind = 'theme')  AS ships_theme
FROM census_targets
GROUP BY target;

-- Every installed component on every in-scope site, flattened to one row each.
CREATE OR REPLACE TEMP VIEW census_components AS
SELECT f.id AS site_id, f.tenant_id, f.multisite,
       f.inventory_freshness, f.inventory_provably_stale, f.inventory_age_known,
       'plugin'::text AS kind,
       lower(split_part(c ->> 'slug', '/', 1)) AS dir_slug,
       c ->> 'version'  AS raw_version,
       COALESCE((c ->> 'active')::boolean, false) AS active
FROM census_fleet f
CROSS JOIN LATERAL jsonb_array_elements(
    CASE WHEN f.has_plugin_inventory THEN f.components -> 'plugins' ELSE '[]'::jsonb END
) AS c
UNION ALL
SELECT f.id, f.tenant_id, f.multisite,
       f.inventory_freshness, f.inventory_provably_stale, f.inventory_age_known,
       'theme'::text,
       lower(split_part(c ->> 'slug', '/', 1)),
       c ->> 'version',
       COALESCE((c ->> 'active')::boolean, false)
FROM census_fleet f
CROSS JOIN LATERAL jsonb_array_elements(
    CASE WHEN f.has_theme_inventory THEN f.components -> 'themes' ELSE '[]'::jsonb END
) AS c;

-- Matched target hits. On multisite, `active=false` is only trustworthy from a
-- post-#558 agent; see the header. Multisite is carried through so every
-- report can separate it rather than average it in.
CREATE OR REPLACE TEMP VIEW census_hits AS
SELECT c.*, t.target, t.category,
       substring(c.raw_version from '^([0-9]+\.[0-9]+)') AS version_minor
FROM census_components c
JOIN census_targets t
  ON t.kind = c.kind AND t.dir_slug = c.dir_slug;

-- One bucket per site. THIS VIEW IS THE CLASSIFICATION -- section 3 reports it
-- and the proof harness asserts against it. It is a view rather than a CTE
-- inside section 3 specifically so the harness can test THIS expression
-- instead of a copy: an earlier harness re-derived the CASE in its own
-- CREATE TABLE AS, so editing the census's CASE changed nothing the assertions
-- could see and the bucket assertions passed against a regression. A guard that
-- cannot see the thing it guards is not a guard.
--
-- Ordered on the positive/negative asymmetry of property 5 in the header:
-- an ACTIVE builder found in partial inventory is a fact; "no builder" is only
-- a fact when BOTH arrays arrived. None of the three non-builder buckets is
-- silently merged into Gutenberg.
CREATE OR REPLACE TEMP VIEW census_classified AS
SELECT f.id, f.tenant_id, f.multisite, f.inventory_provably_stale, f.inventory_age_known,
       CASE
           WHEN NOT f.has_plugin_inventory AND NOT f.has_theme_inventory
                THEN 'unknown: no inventory'
           -- A positive hit is trustworthy even from one array.
           WHEN EXISTS (SELECT 1 FROM census_hits h
                        WHERE h.site_id = f.id AND h.category = 'builder'
                          AND h.active)
                THEN 'builder active'
           -- Below here every branch is a NEGATIVE claim, so it needs the
           -- complete document.
           WHEN NOT (f.has_plugin_inventory AND f.has_theme_inventory)
                THEN 'indeterminate: partial inventory'
           WHEN EXISTS (SELECT 1 FROM census_hits h
                        WHERE h.site_id = f.id AND h.category = 'builder')
                THEN 'builder installed, not active'
           ELSE 'Gutenberg (no builder present)'
       END AS bucket
FROM census_fleet f;

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
\echo '======================================================================'
\echo '=== 0. CONFIDENCE — read this before quoting any number below      ==='
\echo '======================================================================'
-- Every figure is computed, none is written into the prose. The point of this
-- section is that "42% run Elementor" and "42% of the 12% of sites that have
-- reported" are different claims, and only the second one is available here.
SELECT format(
  'SCOPE %s sites. INVENTORY present on %s (%s%%) — that is the denominator for every adoption number. INVENTORY AGE known on only %s (%s%%); the other %s have never recorded when their inventory was collected (m121 added the column with no backfill, so a site stays unknown until its next metadata push) and appear in their own age_unknown bucket, never folded into fresh or stale. MULTISITE %s site(s) in scope: their active=false is a lower bound until every agent carries PR #558.',
  (SELECT count(*) FROM census_fleet),
  (SELECT count(*) FROM census_fleet WHERE has_plugin_inventory OR has_theme_inventory),
  (SELECT round(100.0 * count(*) FILTER (WHERE has_plugin_inventory OR has_theme_inventory)
                / nullif(count(*), 0), 1) FROM census_fleet),
  (SELECT count(*) FROM census_fleet WHERE inventory_age_known),
  (SELECT round(100.0 * count(*) FILTER (WHERE inventory_age_known)
                / nullif(count(*), 0), 1) FROM census_fleet),
  (SELECT count(*) FROM census_fleet WHERE NOT inventory_age_known),
  (SELECT count(*) FROM census_fleet WHERE multisite)
) AS read_this_first;

\echo ''
\echo '--- 0b. The denominators, as counts ---------------------------------'
SELECT m.ord,
       m.metric,
       m.sites,
       round(100.0 * m.sites / nullif((SELECT count(*) FROM census_fleet), 0), 1) AS pct_of_scope,
       m.meaning
FROM (
    SELECT 1, 'sites in scope',
           (SELECT count(*) FROM census_fleet),
           'non-archived, within the tenant scope given'
    UNION ALL SELECT 2, 'inventory present (classifiable)',
           (SELECT count(*) FROM census_fleet WHERE has_plugin_inventory OR has_theme_inventory),
           'has a plugins array, a themes array, or both'
    UNION ALL SELECT 3, 'inventory COMPLETE (both arrays)',
           (SELECT count(*) FROM census_fleet WHERE has_plugin_inventory AND has_theme_inventory),
           'only these can support a NEGATIVE finding such as "no builder"'
    UNION ALL SELECT 4, 'inventory PARTIAL (one array only)',
           (SELECT count(*) FROM census_fleet
             WHERE (has_plugin_inventory OR has_theme_inventory)
               AND NOT (has_plugin_inventory AND has_theme_inventory)),
           'positive findings still count; "no builder" cannot be concluded'
    UNION ALL SELECT 5, 'no usable inventory',
           (SELECT count(*) FROM census_fleet WHERE NOT has_plugin_inventory AND NOT has_theme_inventory),
           'never reported, or a malformed components document'
    UNION ALL SELECT 6, 'inventory age KNOWN',
           (SELECT count(*) FROM census_fleet WHERE inventory_age_known),
           'components_updated_at IS NOT NULL — denominator for ALL freshness'
    UNION ALL SELECT 7, 'inventory age UNKNOWN',
           (SELECT count(*) FROM census_fleet WHERE NOT inventory_age_known),
           'm121 did not backfill; clears on each site next metadata push'
    UNION ALL SELECT 8, 'inventory PROVABLY stale',
           (SELECT count(*) FROM census_fleet WHERE inventory_provably_stale),
           'dated AND older than the stale_days threshold'
    UNION ALL SELECT 9, 'multisite',
           (SELECT count(*) FROM census_fleet WHERE multisite),
           'active=false is a lower bound on pre-#558 agents'
) AS m(ord, metric, sites, meaning)
ORDER BY m.ord;

\echo ''
\echo '=== 1. Inventory freshness (components_updated_at, m121) ============='
-- age_unknown is a real category and is listed FIRST, not sorted away.
-- Expect it to dominate on any run soon after m121.
SELECT inventory_freshness,
       count(*) AS sites,
       round(100.0 * count(*) / nullif(sum(count(*)) OVER (), 0), 1) AS pct_of_scope,
       count(*) FILTER (WHERE has_plugin_inventory OR has_theme_inventory) AS of_which_have_inventory
FROM census_fleet
GROUP BY inventory_freshness
ORDER BY array_position(
    ARRAY['age_unknown (never recorded)', 'inventory_24h', 'inventory_week',
          'inventory_month', 'inventory_over_30d'], inventory_freshness);

\echo ''
\echo '=== 2. Agent contact (last_seen_at) — A DIFFERENT QUESTION ==========='
-- "Is the agent alive", not "how old is the inventory". The 60s heartbeat bumps
-- last_seen_at without refreshing components, so a site can be contact_24h and
-- inventory_over_30d at the same time. Do NOT quote this as freshness.
SELECT last_contact_freshness,
       connection_state,
       count(*) AS sites,
       count(*) FILTER (WHERE NOT inventory_age_known) AS of_which_inventory_age_unknown
FROM census_fleet
GROUP BY last_contact_freshness, connection_state
ORDER BY array_position(
    ARRAY['contact_24h','contact_week','contact_month','contact_over_30d',
          'never_contacted'], last_contact_freshness),
    connection_state;

\echo ''
\echo '=== 3. Builder classification (incl. unknown) ========================'
-- Reads census_classified, defined above with the reasoning for its ordering.
SELECT bucket,
       count(*) AS sites,
       round(100.0 * count(*) / nullif(sum(count(*)) OVER (), 0), 1) AS pct_of_scope,
       count(*) FILTER (WHERE inventory_provably_stale)  AS of_which_provably_stale,
       count(*) FILTER (WHERE NOT inventory_age_known)   AS of_which_age_unknown,
       count(*) FILTER (WHERE multisite)                 AS of_which_multisite
FROM census_classified
GROUP BY bucket
ORDER BY sites DESC;

\echo ''
\echo '=== 4. Per-target adoption ==========================================='
-- THE DENOMINATOR IS PER TARGET, AND IT IS PRINTED.
--
-- A site that sent a plugins array but no themes array cannot produce a Bricks
-- hit, so counting it in the Bricks denominator understates Bricks — the exact
-- bias finding 2 in the header is about, arriving through the back door. Each
-- target is therefore divided by the sites that could have reported it:
--   plugin-only target (Elementor)  -> sites with a plugins array
--   theme-only target  (Bricks)     -> sites with a themes array
--   both (Divi, Beaver Builder)     -> sites with either array
-- denom_sites is a column so the reader can see which one was used.
WITH denom AS (
    SELECT count(*) FILTER (WHERE has_plugin_inventory)                        AS plugin_denom,
           count(*) FILTER (WHERE has_theme_inventory)                         AS theme_denom,
           count(*) FILTER (WHERE has_plugin_inventory OR has_theme_inventory)  AS either_denom
    FROM census_fleet
),
per_target AS (
    SELECT h.category,
           h.target,
           count(DISTINCT h.site_id) FILTER (WHERE h.active)                    AS sites_active,
           count(DISTINCT h.site_id) FILTER (WHERE NOT h.active)                AS sites_installed_inactive,
           count(DISTINCT h.site_id)                                            AS sites_installed_any,
           count(DISTINCT h.site_id) FILTER (WHERE h.active AND h.inventory_provably_stale) AS active_but_provably_stale,
           count(DISTINCT h.site_id) FILTER (WHERE h.active AND NOT h.inventory_age_known)  AS active_age_unknown,
           count(DISTINCT h.site_id) FILTER (WHERE h.multisite)                 AS on_multisite
    FROM census_hits h
    GROUP BY h.category, h.target
)
SELECT p.category,
       p.target,
       CASE WHEN k.ships_plugin AND k.ships_theme THEN 'plugin+theme'
            WHEN k.ships_theme                    THEN 'theme'
            ELSE                                       'plugin' END AS ships_as,
       p.sites_active,
       p.sites_installed_inactive,
       CASE WHEN k.ships_plugin AND k.ships_theme THEN d.either_denom
            WHEN k.ships_theme                    THEN d.theme_denom
            ELSE                                       d.plugin_denom END AS denom_sites,
       round(100.0 * p.sites_active
             / nullif(CASE WHEN k.ships_plugin AND k.ships_theme THEN d.either_denom
                           WHEN k.ships_theme                    THEN d.theme_denom
                           ELSE                                       d.plugin_denom END, 0), 1) AS pct_of_denom,
       p.active_but_provably_stale,
       p.active_age_unknown,
       p.on_multisite
FROM per_target p
JOIN census_target_kinds k ON k.target = p.target
CROSS JOIN denom d
ORDER BY p.category, p.sites_active DESC;

\echo ''
\echo '=== 5. Version spread (major.minor) =================================='
-- Grouped to major.minor; per-patch is noise. version_minor NULL means the
-- version string did not parse — the agent writes the literal "unknown" when a
-- plugin header omits Version, so this bucket is real and is shown, not dropped.
SELECT h.target,
       h.dir_slug,
       COALESCE(h.version_minor, '(unparsed)') AS version_minor,
       count(DISTINCT h.site_id) AS sites,
       count(DISTINCT h.site_id) FILTER (WHERE h.inventory_provably_stale) AS of_which_provably_stale,
       count(DISTINCT h.site_id) FILTER (WHERE NOT h.inventory_age_known)  AS of_which_age_unknown
FROM census_hits h
WHERE h.active
GROUP BY h.target, h.dir_slug, h.version_minor
ORDER BY h.target, sites DESC, version_minor;

\echo ''
\echo '=== 6. Builder co-installation ======================================='
-- How many sites carry more than one builder. A site with Elementor AND Divi
-- is a migration/hand-off case, and it is why section 3 counts each site once
-- by precedence rather than summing section 4.
WITH per_site AS (
    SELECT site_id, count(DISTINCT target) AS builders
    FROM census_hits WHERE category = 'builder' AND active
    GROUP BY site_id
)
SELECT builders AS active_builders_on_site, count(*) AS sites
FROM per_site GROUP BY builders ORDER BY builders;

\echo ''
\echo '=== 7. Multisite exposure — the size of the #558 caveat =============='
-- How much of the answer could still move when agents update. Builder adoption
-- on multisite is a LOWER BOUND on any pre-#558 agent, and nothing in the
-- components document says which agent version wrote it, so this is the
-- exposure, not an error estimate.
SELECT
    (SELECT count(*) FROM census_fleet WHERE multisite)                       AS multisite_sites,
    (SELECT count(*) FROM census_fleet)                                        AS sites_in_scope,
    (SELECT round(100.0 * count(*) FILTER (WHERE multisite) / nullif(count(*), 0), 1)
       FROM census_fleet)                                                      AS pct_multisite,
    (SELECT count(DISTINCT site_id) FROM census_hits
      WHERE multisite AND category = 'builder' AND NOT active)                 AS multisite_builder_inactive,
    'each multisite site above with an INACTIVE builder may be network-activated and under-reported by a pre-#558 agent'
                                                                               AS how_to_read;
