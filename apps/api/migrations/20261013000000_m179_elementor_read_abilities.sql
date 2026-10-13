-- m179: the Elementor widget catalogue and compact widget control schema.
-- Own reads use no approval and no snapshot. The entry hash stays NULL for
-- the boot stamp. Each seed leaves any existing row, including its operator
-- settings, untouched.

INSERT INTO "public"."ability_catalogue" (
    "name", "source", "class", "status", "enabled", "approval_mode",
    "snapshot", "min_agent_version", "limits",
    "title", "description", "usage"
)
SELECT 'wpmgr/elementor-widgets', 'wpmgr', 'read', 'admitted', true, 'none',
       'none', '0.61.166',
       '{"builders_enabled":["elementor"]}'::jsonb,
       'List Elementor widgets',
       'Lists the Elementor widgets registered on the site, with their names, titles, categories and whether ' ||
       'they are Pro or atomic. Changes nothing.',
       'Read the site''s registered widgets before choosing one. Each catalogue entry returns name, title, ' ||
       'categories, pro and atomic. Atomic widgets are listed but cannot be used yet. Use ' ||
       'wpmgr/elementor-widget-schema to read a widget''s controls before choosing settings.'
WHERE NOT EXISTS (
    SELECT 1 FROM "public"."ability_catalogue" c WHERE c.name = 'wpmgr/elementor-widgets'
);

INSERT INTO "public"."ability_catalogue" (
    "name", "source", "class", "status", "enabled", "approval_mode",
    "snapshot", "min_agent_version", "limits",
    "title", "description", "usage"
)
SELECT 'wpmgr/elementor-widget-schema', 'wpmgr', 'read', 'admitted', true, 'none',
       'none', '0.61.166',
       '{"builders_enabled":["elementor"],"max_output_bytes":32768}'::jsonb,
       'Read an Elementor widget''s controls',
       'Reads a compact control schema for one registered Elementor widget, with sections, control ids and ' ||
       'the details needed to choose settings. The answer is capped at 32 KiB. Changes nothing.',
       'Choose a widget by its name from wpmgr/elementor-widgets. The compact schema returns a skeleton of ' ||
       'sections and control ids, then named controls with their type, option keys, default, responsive window, ' ||
       'group family and activator, and repeater fields. The schema answer is capped at 32 KiB. ' ||
       'Atomic widgets are listed but cannot be used yet.'
WHERE NOT EXISTS (
    SELECT 1 FROM "public"."ability_catalogue" c WHERE c.name = 'wpmgr/elementor-widget-schema'
);
