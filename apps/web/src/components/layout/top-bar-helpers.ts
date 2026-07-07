// Pure helpers backing the app-shell top bar breadcrumb (see top-bar.tsx).
// Extracted so the routeless-segment / humanize logic can be unit tested
// without mounting the router or the DOM.

export interface Crumb {
  label: string;
  /** Absolute pathname this crumb links to, or null when it has no
   * navigable destination (the leaf, or a known-routeless parent segment). */
  to: string | null;
}

/**
 * Segments that are a path prefix in the URL but have NO navigable index
 * route - only `$id` detail routes exist underneath them (e.g.
 * `routes/_authed/restores/$restoreId.tsx`, there is no `restores/index.tsx`;
 * same for `schedule-runs/$runId.tsx`). Linking to the bare segment 404s
 * (GH #150). These render as plain text instead of a Link.
 *
 * This is a known-routeless allowlist rather than a route-table lookup
 * because the breadcrumb builder is a pure pathname->label mapper with no
 * access to the router's route manifest. Keep this set in sync with
 * `src/routes/_authed/**`: any new parent path segment that has ONLY nested
 * `$param` routes (no sibling `index.tsx`) must be added here too, or its
 * breadcrumb crumb will silently 404 again.
 */
export const ROUTELESS_SEGMENTS = new Set(["restores", "schedule-runs"]);

const TITLES: Record<string, string> = {
  sites: "Sites",
  updates: "Updates",
  backups: "Backups",
  migrations: "Migrations",
  uptime: "Uptime",
  performance: "Performance",
  vulnerabilities: "Vulnerabilities",
  audit: "Audit",
  settings: "Settings",
  alerts: "Alerts",
  "api-keys": "API keys",
  restores: "Restores",
  "schedule-runs": "Schedule runs",
};

export function humanize(segment: string): string {
  if (TITLES[segment]) return TITLES[segment];
  // Path params arrive as raw values (e.g. an ID). Display them in mono via
  // the consumer; here we just pass through.
  return segment;
}

/**
 * Build the breadcrumb crumb list for a pathname. Pure - no router or DOM
 * access - so route registration state never affects this beyond the
 * `ROUTELESS_SEGMENTS` allowlist above.
 */
export function buildBreadcrumbCrumbs(pathname: string): Crumb[] {
  const segments = pathname.split("/").filter(Boolean);
  if (segments.length === 0) return [{ label: "Home", to: null }];
  const crumbs: Crumb[] = [];
  let acc = "";
  segments.forEach((segment, i) => {
    acc += `/${segment}`;
    const isLast = i === segments.length - 1;
    crumbs.push({
      label: humanize(segment),
      to: isLast || ROUTELESS_SEGMENTS.has(segment) ? null : acc,
    });
  });
  return crumbs;
}
