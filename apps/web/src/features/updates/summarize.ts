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
