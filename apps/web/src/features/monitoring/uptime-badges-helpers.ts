// uptime-badges helpers — extracted from uptime-badges.tsx so that the
// component file exports only components (react-refresh/only-export-components).

import type { UptimeStatus, UptimeSummaryItem } from "@wpmgr/api";

/**
 * Discriminator for the visual up/down/unknown status. The canonical SDK types
 * (`UptimeStatus` / `UptimeSummaryItem`) are objects, not string literals, so
 * the badge component takes this narrow union instead and the callers derive it
 * via the helpers below.
 */
export type UpDown = "up" | "down" | "unknown";

export const statusMeta: Record<
  UpDown,
  { label: string; variant: "success" | "destructive" | "muted" }
> = {
  up: { label: "Up", variant: "success" },
  down: { label: "Down", variant: "destructive" },
  unknown: { label: "Unknown", variant: "muted" },
};

/**
 * Derive the up/down/unknown discriminator from a per-site summary item
 * (dashboard list). If the site has never been probed (no `last_check`) we
 * report "unknown" so the UI doesn't claim a definitive state.
 */
export function statusFromItem(item: UptimeSummaryItem): UpDown {
  if (!item.last_check) return "unknown";
  return item.up ? "up" : "down";
}

/**
 * Derive the up/down/unknown discriminator from a windowed UptimeStatus
 * (site-detail). Same rule as `statusFromItem`: missing `last_check` means we
 * have no probe yet for this site.
 */
export function statusFromStatus(s: UptimeStatus): UpDown {
  if (!s.last_check) return "unknown";
  return s.up ? "up" : "down";
}
