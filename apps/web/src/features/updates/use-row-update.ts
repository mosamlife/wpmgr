import { useCallback, useEffect, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import type { UpdateItem, UpdateRunCreate, UpdateTask } from "@wpmgr/api";

import {
  useCreateUpdateRun,
  useRunEventStream,
  useUpdateRun,
} from "./use-updates";
import { availableUpdatesKeys } from "./use-available-updates";
import type {
  AvailableUpdateItem,
  CoreUpdate,
  SiteAvailableUpdates,
} from "./types";

// Per-row update state machine for the AvailableUpdatesCard. Wraps the bulk
// update endpoint (POST /api/v1/updates) into a single-item flow and projects
// the run's SSE-driven task status back down to one row.
//
// Lifecycle:
//   idle      — no run started for this row
//   starting  — POST /updates in flight
//   pending   — run created, task queued, no agent activity yet
//   running   — agent is doing work
//   succeeded — task settled OK; row fades after a beat
//   failed    — task settled with an error; Retry available
//   rolled_back — guard tripped; Retry available
//   skipped   — task no-op'd (e.g. nothing to do)
//
// Reuses `useRunEventStream` so we do NOT open a second SSE channel per row;
// each call to the hook subscribes to its run's events, and the cache patches
// drive a shared `useUpdateRun` query that the hook reads from.

export type RowUpdateState =
  | "idle"
  | "starting"
  | "pending"
  | "running"
  | "succeeded"
  | "failed"
  | "rolled_back"
  | "skipped";

export interface RowUpdate {
  state: RowUpdateState;
  /** Best-effort progress detail. Populated by the agent's task.detail field. */
  progress?: string;
  /** Error message when state === "failed" or "rolled_back". */
  error?: string;
  /** The run id once POST /updates has resolved (for "View logs" links). */
  runId: string | null;
  /** The matched task id, or null until the run report contains one. */
  taskId: string | null;
  /** True while the POST /updates request is in flight. */
  isStarting: boolean;
  /** Start the update for this row. */
  trigger: () => Promise<void>;
  /** Reset the row to "idle" and start again. Used by failed/rolled_back. */
  retry: () => Promise<void>;
}

interface UseRowUpdateOptions {
  /** Optional callback fired when the row's run starts. */
  onStart?: (runId: string) => void;
  /** Optional callback fired when the task reaches a terminal state. */
  onSettled?: (state: RowUpdateState) => void;
}

function findTask(
  tasks: UpdateTask[] | undefined,
  item: AvailableUpdateItem,
): UpdateTask | undefined {
  return tasks?.find(
    (t) => t.target_type === item.type && t.target_slug === item.slug,
  );
}

/** Map a UpdateTask.status onto our RowUpdateState union. */
function projectStatus(
  task: UpdateTask | undefined,
  runId: string | null,
  starting: boolean,
): RowUpdateState {
  if (starting) return "starting";
  if (!runId) return "idle";
  if (!task) return "pending";
  switch (task.status) {
    case "pending":
      return "pending";
    case "running":
      return "running";
    case "succeeded":
      return "succeeded";
    case "failed":
      return "failed";
    case "rolled_back":
      return "rolled_back";
    case "skipped":
      return "skipped";
    default:
      return "pending";
  }
}

/**
 * Drive a single-row update against the shared bulk updates endpoint. The hook
 * is stateful (it owns the `runId`) so the row remembers its run across SSE
 * frames; the caller doesn't need to thread the runId through props.
 */
export function useRowUpdate(
  siteId: string,
  item: AvailableUpdateItem,
  options?: UseRowUpdateOptions,
): RowUpdate {
  const queryClient = useQueryClient();
  const [runId, setRunId] = useState<string | null>(null);
  const terminalNotifiedFor = useRef<string | null>(null);
  const create = useCreateUpdateRun();
  const onSettledRef = useRef(options?.onSettled);
  const onStartRef = useRef(options?.onStart);
  useEffect(() => {
    onSettledRef.current = options?.onSettled;
    onStartRef.current = options?.onStart;
  }, [options?.onSettled, options?.onStart]);

  // Pull the run from the cache once we have an id. Poll while the run is
  // not yet completed: SSE is the primary signal but the underlying agent
  // task can finish in well under a second (e.g. a tiny plugin bump), which
  // means the EventSource may subscribe AFTER the terminal transition was
  // published to the in-process hub — the SSE handler will flush a current-
  // state snapshot on connect, but if any link in that chain is flaky the
  // row would stay "Queued" forever. A 2 s poll is the safety net that the
  // browser ALWAYS observes the terminal state within a couple of seconds.
  const { data: run } = useUpdateRun(runId ?? "", {
    poll: true,
    enabled: Boolean(runId),
  });
  useRunEventStream(runId ?? "", { enabled: Boolean(runId) });

  const task = findTask(run?.tasks, item);
  const state = projectStatus(task, runId, create.isPending);

  // Notify caller once per terminal transition AND scrub the item from the
  // available-updates cache on success (optimistic collapse).
  useEffect(() => {
    if (!runId) return;
    if (
      state !== "succeeded" &&
      state !== "failed" &&
      state !== "rolled_back" &&
      state !== "skipped"
    ) {
      return;
    }
    if (terminalNotifiedFor.current === runId) return;
    terminalNotifiedFor.current = runId;
    onSettledRef.current?.(state);
    if (state === "succeeded" && (item.type === "plugin" || item.type === "theme")) {
      queryClient.setQueryData<SiteAvailableUpdates>(
        availableUpdatesKeys.detail(siteId),
        (prev) => {
          if (!prev) return prev;
          return {
            ...prev,
            items: prev.items.filter(
              (i) => !(i.type === item.type && i.slug === item.slug),
            ),
          };
        },
      );
    }
  }, [runId, state, item.type, item.slug, queryClient, siteId]);

  const trigger = useCallback(async () => {
    // Reset any previous attempt so the state machine starts fresh.
    terminalNotifiedFor.current = null;
    setRunId(null);

    const body: UpdateRunCreate = {
      site_ids: [siteId],
      items: [
        {
          type: item.type,
          slug: item.slug,
          version: item.new_version,
        },
      ],
      dry_run: false,
    };
    const created = await create.mutateAsync(body);
    setRunId(created.id);
    onStartRef.current?.(created.id);
  }, [create, item.new_version, item.slug, item.type, siteId]);

  return {
    state,
    progress: task?.detail,
    error: task?.error,
    runId,
    taskId: task?.id ?? null,
    isStarting: create.isPending,
    trigger,
    retry: trigger,
  };
}

/**
 * Build the bulk-update POST body for "Update all" / "Update selected".
 * Single-item version of this lives inline in `useRowUpdate.trigger`.
 */
export function buildBulkBody(
  siteId: string,
  items: AvailableUpdateItem[],
  includeCore: { new_version: string } | null,
): UpdateRunCreate {
  const wireItems: UpdateItem[] = [];
  if (includeCore) {
    wireItems.push({
      type: "core",
      version: includeCore.new_version,
    });
  }
  for (const it of items) {
    wireItems.push({
      type: it.type,
      slug: it.slug,
      version: it.new_version,
    });
  }
  return {
    site_ids: [siteId],
    items: wireItems,
    dry_run: false,
  };
}

/**
 * Variant of `useRowUpdate` for the WordPress core row. Mirrors the
 * plugin/theme state machine but matches the task by `target_type === "core"`
 * instead of (type, slug) — core tasks ship with slug "core" on the wire,
 * but a separate hook keeps the matching logic clean.
 */
export function useCoreRowUpdate(
  siteId: string,
  coreUpdate: CoreUpdate,
): RowUpdate {
  const [runId, setRunId] = useState<string | null>(null);
  const create = useCreateUpdateRun();
  // Poll alongside SSE — see comment on the plugin/theme variant above.
  const { data: run } = useUpdateRun(runId ?? "", {
    poll: true,
    enabled: Boolean(runId),
  });
  useRunEventStream(runId ?? "", { enabled: Boolean(runId) });

  const task = run?.tasks?.find((t) => t.target_type === "core");
  const state: RowUpdateState = !runId
    ? create.isPending
      ? "starting"
      : "idle"
    : !task
      ? "pending"
      : task.status === "pending"
        ? "pending"
        : task.status === "running"
          ? "running"
          : task.status === "succeeded"
            ? "succeeded"
            : task.status === "failed"
              ? "failed"
              : task.status === "rolled_back"
                ? "rolled_back"
                : "skipped";

  const trigger = useCallback(async () => {
    setRunId(null);
    const created = await create.mutateAsync({
      site_ids: [siteId],
      items: [
        {
          type: "core",
          version: coreUpdate.new_version,
        },
      ],
      dry_run: false,
    });
    setRunId(created.id);
  }, [coreUpdate.new_version, create, siteId]);

  return {
    state,
    progress: task?.detail,
    error: task?.error,
    runId,
    taskId: task?.id ?? null,
    isStarting: create.isPending,
    trigger,
    retry: trigger,
  };
}
