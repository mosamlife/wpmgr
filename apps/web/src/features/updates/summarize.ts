// Pure helpers extracted from update-status.tsx and update-tasks-table.tsx to
// clear the react-refresh boundary: files that export both components AND
// non-component values trip the react-refresh/only-export-components rule.
// Surface agents import from here; the originals will remove their copies.

import type { Site, UpdateTask } from "@wpmgr/api";

type TaskStatus = UpdateTask["status"];

/** Count task statuses and derive a 0..total "settled" progress figure. */
export function summarizeTasks(tasks: UpdateTask[]): {
  total: number;
  done: number;
  counts: Record<TaskStatus, number>;
} {
  const counts: Record<TaskStatus, number> = {
    pending: 0,
    running: 0,
    succeeded: 0,
    failed: 0,
    rolled_back: 0,
    skipped: 0,
  };
  for (const task of tasks) counts[task.status] += 1;
  const done =
    counts.succeeded + counts.failed + counts.rolled_back + counts.skipped;
  return { total: tasks.length, done, counts };
}

/** Build a site id -> name lookup from the sites list cache. */
export function siteNameMap(sites: Site[] | undefined): Map<string, string> {
  const map = new Map<string, string>();
  for (const site of sites ?? []) map.set(site.id, site.name);
  return map;
}

// GH #210 — the worst-case rollback failure: an update causes a site-wide
// PHP fatal, so the rollback command is undeliverable (it rides the same
// WordPress request that's fataling), and an agent-side watchdog attempts
// automatic filesystem-level recovery. The backend keeps the existing task
// status enum (failed/rolled_back) and communicates this condition purely
// through the detail/error text, so callers key off content rather than a
// new status value. Every surface that renders a task's outcome should use
// this helper rather than re-deriving the pattern locally, so the condition
// reads consistently (and never as a generic "rollback failed") everywhere.
const SITE_DOWN_RECOVERY_STATUSES = new Set(["failed", "rolled_back"]);
const SITE_DOWN_RECOVERY_PATTERN =
  /site[- ]wide|site is down|not responding|undeliverable|filesystem recovery|automatic recovery|watchdog/i;

/**
 * True when a terminal (failed/rolled_back) task's detail/error text
 * describes the site-wide-fatal + undeliverable-rollback + auto-filesystem-
 * recovery condition, as opposed to an ordinary update failure or rollback.
 */
export function isSiteDownRecovery(
  status: string,
  detail?: string,
  error?: string,
): boolean {
  if (!SITE_DOWN_RECOVERY_STATUSES.has(status)) return false;
  return SITE_DOWN_RECOVERY_PATTERN.test(`${detail ?? ""} ${error ?? ""}`);
}

/** Distinct, actionable label for the site-down-recovery condition. Never
 * collapse this into the generic "Rolled back"/"Failed" copy. */
export const SITE_DOWN_RECOVERY_LABEL = "Site down, recovery attempted";

/** Fallback body copy when the backend detail is empty but the condition is
 * detected from `error` alone. */
export const SITE_DOWN_RECOVERY_FALLBACK_DETAIL =
  "The site went down site-wide during this update. Automatic filesystem recovery was attempted; manual filesystem recovery may be required.";
