// Pure helpers for building the incident detail dialog's client-composed
// "what happened around this incident" timeline (GH #148). The dialog pulls
// from four existing hooks (update runs, backups, activity, PHP errors),
// tags each row with a source kind, and merge-sorts everything newest-first
// around the incident's `[started_at, ended_at]` window. Extracted here so
// the merge/sort/window logic can be unit tested without mounting the
// dialog or any of its four data hooks.

import type {
  UpdateTask,
  BackupSnapshot,
  SiteActivityEvent,
  PhpError,
} from "@wpmgr/api";

export type TimelineSource = "update" | "backup" | "activity" | "php_error";

export interface TimelineItem {
  source: TimelineSource;
  id: string;
  /** ISO timestamp used for windowing and sort order. */
  timestamp: string;
  label: string;
  detail?: string;
}

export interface IncidentWindow {
  start: string;
  /** `null` means the incident is still ongoing (window extends to "now"). */
  end: string | null;
}

/** Context padding either side of the incident window, in milliseconds. */
export const TIMELINE_WINDOW_PADDING_MS = 15 * 60 * 1000; // 15 minutes

// ---------------------------------------------------------------------------
// Per-source mappers — real SDK shapes -> the minimal TimelineItem shape
// ---------------------------------------------------------------------------

function backupKindLabel(kind: BackupSnapshot["kind"]): string {
  switch (kind) {
    case "files":
      return "Files";
    case "db":
      return "Database";
    case "full":
      return "Full";
    default:
      return "Backup";
  }
}

/** Update tasks, already filtered to the site the incident belongs to. */
export function updateTasksToTimeline(tasks: UpdateTask[]): TimelineItem[] {
  return tasks
    .map((task): TimelineItem | null => {
      const timestamp =
        task.finished_at ?? task.started_at ?? task.updated_at ?? task.created_at;
      if (!timestamp) return null;
      const target = `${task.target_type} ${task.target_slug}`;
      let label: string;
      if (task.status === "failed") label = `Failed to update ${target}`;
      else if (task.status === "rolled_back")
        label = `Rolled back ${target} update`;
      else if (task.status === "succeeded") label = `Updated ${target}`;
      else label = `${target} update ${task.status}`;
      const detail =
        task.from_version && task.to_version
          ? `${task.from_version} to ${task.to_version}`
          : undefined;
      return { source: "update", id: task.id, timestamp, label, detail };
    })
    .filter((item): item is TimelineItem => item !== null);
}

export function backupsToTimeline(snapshots: BackupSnapshot[]): TimelineItem[] {
  return snapshots.map((snapshot): TimelineItem => {
    const timestamp = snapshot.finished_at ?? snapshot.created_at;
    const kindLabel = backupKindLabel(snapshot.kind);
    let label: string;
    if (snapshot.status === "completed") label = `${kindLabel} backup completed`;
    else if (snapshot.status === "failed") label = `${kindLabel} backup failed`;
    else label = `${kindLabel} backup ${snapshot.status}`;
    return {
      source: "backup",
      id: snapshot.id,
      timestamp,
      label,
      detail: snapshot.error,
    };
  });
}

export function activityToTimeline(
  events: SiteActivityEvent[],
): TimelineItem[] {
  return events.map((event): TimelineItem => ({
    source: "activity",
    id: event.id,
    timestamp: event.occurred_at,
    label: event.summary,
    detail: event.actor_login || undefined,
  }));
}

export function phpErrorsToTimeline(errors: PhpError[]): TimelineItem[] {
  return errors.map((err): TimelineItem => ({
    source: "php_error",
    id: err.id,
    timestamp: err.last_seen_at,
    label: `PHP ${err.severity}: ${err.message}`,
    detail: `${err.file}:${err.line}`,
  }));
}

// ---------------------------------------------------------------------------
// Window filter + merge-sort
// ---------------------------------------------------------------------------

function withinWindow(
  timestamp: string,
  window: IncidentWindow,
  paddingMs: number,
): boolean {
  const t = Date.parse(timestamp);
  if (!Number.isFinite(t)) return false;
  const startMs = Date.parse(window.start) - paddingMs;
  if (!Number.isFinite(startMs)) return false;
  const endMs = (window.end ? Date.parse(window.end) : Date.now()) + paddingMs;
  return t >= startMs && t <= endMs;
}

export interface MergeIncidentTimelineOptions {
  /** Max items kept per source list after windowing (newest first). Default 5. */
  perSourceLimit?: number;
  /** Max items in the final merged result. Default 20. */
  totalLimit?: number;
  /** Context padding either side of the window, in ms. Default 15 minutes. */
  paddingMs?: number;
}

/**
 * Filter each source list to the incident window (plus context padding),
 * keep the newest `perSourceLimit` per source, then merge and sort the
 * whole set newest-first, capped at `totalLimit`.
 */
export function mergeIncidentTimeline(
  sourceLists: TimelineItem[][],
  window: IncidentWindow,
  options?: MergeIncidentTimelineOptions,
): TimelineItem[] {
  const perSourceLimit = options?.perSourceLimit ?? 5;
  const totalLimit = options?.totalLimit ?? 20;
  const paddingMs = options?.paddingMs ?? TIMELINE_WINDOW_PADDING_MS;

  const merged: TimelineItem[] = [];
  for (const list of sourceLists) {
    const filtered = list
      .filter((item) => withinWindow(item.timestamp, window, paddingMs))
      .sort((a, b) => Date.parse(b.timestamp) - Date.parse(a.timestamp))
      .slice(0, perSourceLimit);
    merged.push(...filtered);
  }

  return merged
    .sort((a, b) => Date.parse(b.timestamp) - Date.parse(a.timestamp))
    .slice(0, totalLimit);
}
