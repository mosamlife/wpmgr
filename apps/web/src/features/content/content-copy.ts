import type { ContentInventoryRow } from "@wpmgr/api";

// Copy table for the site Content tab. Verdicts and reason codes are OPEN
// strings on the wire (a newer agent can send a word this build does not know),
// so every lookup falls back to UNKNOWN_COPY rather than rendering the raw code.
// Wording is for a site owner. Site-supplied strings never pass through here.

export const UNKNOWN_COPY = "Not available yet";

/** Third column in slice S1: nothing is editable yet. */
export const AI_EDIT_NOT_ON = "Not yet: AI editing isn't switched on";

// route_reason to the plain reason a page would be refused (or handled) once
// AI editing is on. Codes are the closed set in apps/api/internal/content/validate.go.
const REASON_COPY: Record<string, string> = {
  content_column: "Later: text would be published directly",
  unrecognised_builder: "Later: no, builder not confirmed",
  template_may_override: "Later: no, the theme may replace this page's text",
  special_page: "Later: no, this is a special page such as the blog index",
  empty_page: "Later: no, the page has no text yet",
  block_editor_unsupported: "Later: no, block editor pages are not supported",
  vendor_ability: "Later: through the builder's own tools",
  unpublished_draft_exists: "Later: no, an unpublished draft exists",
  ai_draft_pending: "Later: no, an AI draft is waiting for review",
  draft_unreadable: "Later: no, the draft could not be read",
  builder_version_below_floor: "Later: no, the builder is too old",
  builder_version_unverified: "Later: no, the builder version is unverified",
  builder_ability_missing: "Later: no, the builder lacks the needed tools",
  builder_ability_changed: "Later: no, the builder's tools changed",
  ability_owner_mismatch: "Later: no, the builder's tools are not trusted",
  vendor_requires_admin: "Later: no, the builder needs an administrator",
  builder_access_not_granted: "Later: no, builder access is not granted",
  builder_not_supported: "Later: no, this builder is not supported",
  ambiguous_owner: "Later: no, it is unclear which editor owns this page",
};

function lookup(table: Record<string, string>, key: string): string {
  return Object.prototype.hasOwnProperty.call(table, key) ? table[key]! : UNKNOWN_COPY;
}

export function reasonCopy(reason: string): string {
  return lookup(REASON_COPY, reason);
}

const VERDICT_EDITOR_COPY: Record<string, string> = {
  classic: "WordPress (classic)",
  empty: "WordPress (empty page)",
  block_document: "Block editor",
  builder: "A page builder",
  ambiguous: "A page builder",
  unrecognised_builder: "A page builder",
  special_page: "WordPress",
  template_may_override: "WordPress",
};

/** The "Edited with" cell. Names and versions are text nodes at the call site. */
export function editorLabel(row: ContentInventoryRow): {
  name: string;
  version: string | null;
} {
  const ed = row.editor;
  if (ed) {
    return {
      name: ed.display_name ? ed.display_name : "A page builder",
      version: ed.display_name && ed.version ? ed.version : null,
    };
  }
  return { name: lookup(VERDICT_EDITOR_COPY, row.verdict), version: null };
}

const VERDICT_SHARE_COPY: Record<string, string> = {
  classic: "Classic editor",
  empty: "Empty pages",
  block_document: "Block editor",
  builder: "Page builder",
  ambiguous: "Unclear which editor",
  unrecognised_builder: "A page builder, not confirmed",
  special_page: "Special pages",
  template_may_override: "Theme may replace the text",
};

export function verdictShareLabel(verdict: string): string {
  return lookup(VERDICT_SHARE_COPY, verdict);
}

export function pageTitle(row: ContentInventoryRow, titlesIncluded: boolean): string {
  if (titlesIncluded && row.title) return row.title;
  return `Page #${row.post_id}`;
}
