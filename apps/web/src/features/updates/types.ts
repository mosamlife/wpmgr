// Track C local types for the "available updates" panel. These shapes mirror
// the wire contract documented in the Updates feature brief (Track A backend +
// Track B OpenAPI). When Track B's OpenAPI regeneration lands, callers can swap
// imports of these names to come from `@wpmgr/api` with one find/replace —
// the field names are deliberately identical.
//
// Until then, keeping these here lets the React UI compile without blocking on
// the schema PR.

/** Per-component availability hint. Mirrors WP's wp_get_*_updates() entries. */
export type AvailableUpdate = {
  new_version: string;
  package?: string | null;
  tested?: string | null;
  requires_php?: string | null;
};

/** WordPress core update advisory. */
export type CoreUpdate = {
  new_version: string;
  current_version: string;
};

/** A single plugin/theme row in the "updates available" payload. */
export type AvailableUpdateItem = {
  type: "plugin" | "theme";
  slug: string;
  name: string;
  version: string;
  new_version: string;
  active: boolean;
  package?: string | null;
  tested?: string | null;
  requires_php?: string | null;
};

/** GET /api/v1/sites/{siteId}/updates/available response. */
export type SiteAvailableUpdates = {
  site_id: string;
  core_update: CoreUpdate | null;
  items: AvailableUpdateItem[];
  /** ISO-8601 timestamp of when the agent last refreshed availability. */
  as_of: string | null;
};

/** Stable identity key for the multi-select Set. */
export function itemKey(item: { type: AvailableUpdateItem["type"]; slug: string }): string {
  return `${item.type}:${item.slug}`;
}

/** Special key reserved for the core row in the selection set. */
export const CORE_KEY = "core:core" as const;
