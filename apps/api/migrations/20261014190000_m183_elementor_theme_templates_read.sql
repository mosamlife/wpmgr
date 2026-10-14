-- m183: the Elementor header and footer template catalogue.
-- Own reads use no approval and no snapshot. The entry hash stays NULL for
-- the boot stamp. Each seed leaves any existing row, including its operator
-- settings, untouched.

INSERT INTO "public"."ability_catalogue" (
    "name", "source", "class", "status", "enabled", "approval_mode",
    "snapshot", "min_agent_version", "limits",
    "title", "description", "usage"
)
SELECT 'wpmgr/elementor-theme-templates', 'wpmgr', 'read', 'admitted', true, 'none',
       'none', '0.61.167',
       '{"builders_enabled":["elementor"],"max_output_bytes":32768}'::jsonb,
       'List Elementor headers and footers',
       'Lists the site''s Elementor Pro header and footer templates with their type, status, whether WPMgr made ' ||
       'each one and where each shows, plus whether Elementor Pro''s Theme Builder is ready, the places a ' ||
       'template can be set to show, and the site''s menus. Changes nothing.',
       'Read this before making or changing a header or footer. pro.theme_builder must be true; pro.problem ' ||
       'says why it is not. Each template lists shows_on, the display rules Elementor Pro stored, such as ' ||
       'include/general for the entire site or exclude/singular/page/42 for everywhere except page 42; ' ||
       'shows_on is null when the rules cannot be read, and shows_on_problem says why. places lists the ' ||
       'rules this site accepts in this version; takes_id means the rule may end with a page or post id. ' ||
       'made_by_wpmgr marks templates WPMgr made; only those, while they are still drafts, can be changed. ' ||
       'Lists stop at 25 entries; templates_total and menus_total count everything. Each menu has its id, ' ||
       'slug, name and item count. To make a header or footer, call wpmgr/page-create with template header ' ||
       'or footer instead of post_type, editor builder:elementor, elementor_format classic, an outline, and ' ||
       'shows_on: at most 20 rules from places, each include/ or exclude/ and the place, with a page or post ' ||
       'id where takes_id allows. Without shows_on the draft shows nowhere. To change one, read it with ' ||
       'wpmgr/page-structure and use wpmgr/page-edit; set_shows_on replaces the whole list. WPMgr makes and ' ||
       'changes drafts only and never publishes one: a person publishes it in Elementor. Headers and footers ' ||
       'need the WPMgr plugin 0.61.167 or later and Elementor Pro.'
WHERE NOT EXISTS (
    SELECT 1 FROM "public"."ability_catalogue" c WHERE c.name = 'wpmgr/elementor-theme-templates'
);
