// GH #272 — tri-state uptime badge for the Sites list/grid.
//
// `Site.up` (`@wpmgr/api`) maps from the API's `UptimeUp *bool`: `true` after
// an explicit successful probe, `false` after an explicit failed probe, and
// `null`/`undefined` when the site has never been probed (or a probe result
// has not synced yet). A prior version of the Sites badge only special-cased
// the literal `false`, so `null`/`undefined` fell through to a green "Up" —
// a fully-down, never-probed site could read as healthy during an incident.
//
// This helper is the single source of truth for that mapping so the grid
// (site-card.tsx) and the table (sites-table.tsx) can't drift apart again.
// It intentionally mirrors the fleet dashboard's unknown handling
// (features/fleet/uptime-status.ts STATUS_COLOR_CLASS.unknown = muted) for
// visual consistency across the app, but stays a dedicated helper rather
// than reusing features/monitoring/uptime-badges-helpers.ts — that helper's
// `statusFromItem`/`statusFromStatus` key off a different field
// (`last_check`) for a different data shape (UptimeSummaryItem/UptimeStatus)
// and changing its `up` fallback would affect unrelated monitoring callers.

import type { StatusTone } from "@/components/status/status-dot";

export type SiteUpDown = "up" | "down" | "unknown";

export interface SiteUptimeBadge {
  status: SiteUpDown;
  /** Human-readable label ("Up" / "Down" / "Unknown"). */
  label: string;
  /** Semantic tone for StatusDot/StatusChip-style consumers. */
  tone: StatusTone;
}

/**
 * Derive the tri-state uptime badge from `Site.up`.
 *
 * The core invariant: the badge is green ("up") ONLY when there is an
 * explicit "up" probe result. `null` and `undefined` both mean "we don't
 * know" and must render as a neutral "Unknown", never as "Up".
 */
export function siteUptimeBadge(up: boolean | null | undefined): SiteUptimeBadge {
  if (up === true) return { status: "up", label: "Up", tone: "success" };
  if (up === false) return { status: "down", label: "Down", tone: "destructive" };
  return { status: "unknown", label: "Unknown", tone: "muted" };
}

/**
 * Text-color class for callers (sites-table.tsx) that color plain text
 * instead of rendering a `StatusDot`. Keyed off `status` (not re-deriving
 * from `up`) so it always agrees with `siteUptimeBadge`.
 */
export function siteUptimeTextClass(status: SiteUpDown): string {
  switch (status) {
    case "up":
      return "text-[var(--color-foreground)]";
    case "down":
      return "text-[var(--color-destructive)]";
    case "unknown":
      return "text-[var(--color-muted-foreground)]";
  }
}
