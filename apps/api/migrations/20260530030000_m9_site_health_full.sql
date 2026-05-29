-- ADR-037 Site-Health-Full (v0.9.14) — full WP_Debug_Data parity.
--
-- The Site-Health-Full agent build adds a 15th category — `wp_native` — that
-- carries the verbatim WP_Debug_Data::debug_data() dump (every section WP
-- populates: wp-core, wp-paths-sizes, wp-dropins, wp-active-theme,
-- wp-parent-theme, wp-themes-inactive, wp-mu-plugins, wp-plugins-active,
-- wp-plugins-inactive, wp-media, wp-server, wp-database, wp-constants,
-- wp-filesystem + every third-party `debug_information` filter contribution
-- from Yoast SEO / WooCommerce / ACF / etc.).
--
-- Structurally NO schema change is required — `agent_diagnostics.category`
-- is a text column and `payload` is JSONB, so a new category string just
-- means a new row per site. This migration is additive in the sense that it
-- updates the documenting COMMENT on the category column and registers the
-- new category string with the WPMgr CP's known-categories whitelist (see
-- apps/api/internal/diagnostics/model.go::ValidCategory).
--
-- Privacy: `wp_native.payload` is the agent's WP_Debug_Data dump AFTER its
-- privacy walker has redacted admin_email / user_email / SMTP credentials /
-- WP salts. The CP relies on the agent-side redaction here — the operator
-- GET handler does not need to re-redact. See the agent's
-- DiagnosticsCommand::redactWpNative() for the denylist.

COMMENT ON COLUMN "public"."agent_diagnostics"."category" IS
  'One of: identity / php / mysql / filesystem / http / cron / themes / '
  'plugins / users / security / https / mail / performance / hosting / '
  'wp_native. The 14 legacy categories are the WPMgr-extra leapfrog '
  'collector; wp_native is the verbatim WP_Debug_Data::debug_data() dump '
  'introduced in agent v0.9.14 (Site-Health-Full).';

-- No data migration needed: existing rows keep their category strings, new
-- agent builds will start inserting wp_native rows on their next daily push.
