import { useEffect, useRef } from "react";
import {
  useQuery,
  useMutation,
  useQueryClient,
  type QueryClient,
  type UseQueryResult,
  type UseMutationResult,
} from "@tanstack/react-query";
import {
  createUpdateRun,
  listUpdateRuns,
  getUpdateRun,
  type UpdateRun,
  type UpdateRunCreate,
  type UpdateTask,
  type UpdateEvent,
  type ApiError,
} from "@wpmgr/api";

// Server-state hooks for the Updates (bulk update runs) domain. Built on the
// generated @wpmgr/api SDK; each call returns `{ data, error, response }` which
// we unwrap so TanStack Query manages loading/error/success.
//
// Live progress for a single run comes over Server-Sent Events. The generated
// fetch client does not model SSE, so `useRunEventStream` uses the browser
// `EventSource` directly against the same-origin Vite proxy (/api/...), and
// PATCHES the run-detail query cache as task-status deltas arrive. If the
// EventSource errors (proxy/browser unsupported), it falls back to short-poll
// refetching the detail query until the run completes.

export const updatesKeys = {
  all: ["updates"] as const,
  lists: () => [...updatesKeys.all, "list"] as const,
  list: () => [...updatesKeys.lists()] as const,
  detail: (id: string) => [...updatesKeys.all, "detail", id] as const,
};

/** A 404 surfaced as a typed error so callers can render a not-found state. */
export class NotFoundError extends Error {
  constructor(message = "Not found") {
    super(message);
    this.name = "NotFoundError";
  }
}

/** List recent update runs for the current tenant. */
export function useUpdateRuns(): UseQueryResult<UpdateRun[], Error> {
  return useQuery({
    queryKey: updatesKeys.list(),
    queryFn: async () => {
      const { data, error } = await listUpdateRuns({});
      if (error) throw toError(error);
      return data?.items ?? [];
    },
  });
}

/**
 * Fetch a single run with its tasks. `enablePolling` turns on background
 * refetching (used as the SSE fallback) and stops once the run completes.
 * `enabled` lets callers gate the query while they're still resolving a runId
 * (e.g. the per-row update flow on the site detail page).
 */
export function useUpdateRun(
  runId: string,
  options?: { poll?: boolean; enabled?: boolean },
): UseQueryResult<UpdateRun, Error> {
  const poll = options?.poll ?? false;
  const enabled = options?.enabled ?? true;
  return useQuery({
    queryKey: updatesKeys.detail(runId),
    queryFn: async () => {
      const { data, error, response } = await getUpdateRun({
        path: { runId },
      });
      if (response?.status === 404) throw new NotFoundError("Update run not found");
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    enabled: enabled && runId !== "",
    // Poll every 2s while enabled AND the run is not yet completed.
    refetchInterval: (query) =>
      poll && query.state.data?.status !== "completed" ? 2000 : false,
  });
}

/**
 * Start a bulk update run (operator+). On success, seeds the detail cache with
 * the freshly created run (including its initial tasks) and invalidates the
 * list so the new run appears.
 */
export function useCreateUpdateRun(): UseMutationResult<
  UpdateRun,
  Error,
  UpdateRunCreate
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: UpdateRunCreate) => {
      const { data, error, response } = await createUpdateRun({ body });
      if (response?.status === 422) {
        throw toError(error ?? { message: "Validation failed" });
      }
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: (run) => {
      queryClient.setQueryData(updatesKeys.detail(run.id), run);
      void queryClient.invalidateQueries({ queryKey: updatesKeys.lists() });
    },
  });
}

/**
 * Apply a single UpdateEvent delta to a cached run, returning the next run.
 * Updates the matching task in place (or appends if unseen) and patches the
 * run-level status. Exported for unit/e2e reuse.
 */
export function applyEvent(run: UpdateRun, event: UpdateEvent): UpdateRun {
  const tasks = run.tasks ?? [];
  let matched = false;
  const nextTasks: UpdateTask[] = tasks.map((task) => {
    if (task.id !== event.task_id) return task;
    matched = true;
    return {
      ...task,
      status: event.status,
      ...(event.from_version !== undefined
        ? { from_version: event.from_version }
        : {}),
      ...(event.to_version !== undefined
        ? { to_version: event.to_version }
        : {}),
      ...(event.detail !== undefined ? { detail: event.detail } : {}),
    };
  });
  if (!matched) {
    nextTasks.push({
      id: event.task_id,
      run_id: event.run_id,
      tenant_id: run.tenant_id,
      site_id: event.site_id,
      target_type: event.target_type,
      target_slug: event.target_slug,
      status: event.status,
      from_version: event.from_version,
      to_version: event.to_version,
      detail: event.detail,
      created_at: run.created_at,
      updated_at: new Date().toISOString(),
    });
  }
  return { ...run, status: event.run_status, tasks: nextTasks };
}

/** Patch a single SSE event into the run-detail query cache. */
function patchEvent(
  queryClient: QueryClient,
  runId: string,
  event: UpdateEvent,
): void {
  queryClient.setQueryData<UpdateRun>(updatesKeys.detail(runId), (prev) =>
    prev ? applyEvent(prev, event) : prev,
  );
}

export type RunStreamState = "connecting" | "live" | "polling" | "closed";

/**
 * Subscribe to the live SSE stream for a run and patch deltas into the query
 * cache. Returns the current transport state for UI affordances. On the first
 * EventSource error we close it and signal "polling" so the caller can enable
 * the `useUpdateRun({ poll: true })` fallback. The stream is closed on unmount
 * and once the run reaches a terminal ("completed") state.
 *
 * EventSource is same-origin (/api/v1/updates/{runId}/events) so the session
 * cookie flows automatically; no auth wiring is needed here.
 */
export function useRunEventStream(
  runId: string,
  options?: { enabled?: boolean; onState?: (state: RunStreamState) => void },
): void {
  const queryClient = useQueryClient();
  const enabled = options?.enabled ?? true;
  const onState = options?.onState;
  const onStateRef = useRef(onState);

  // Keep the latest callback in a ref so the stream effect doesn't re-run (and
  // tear down the EventSource) just because the parent passed a new closure.
  useEffect(() => {
    onStateRef.current = onState;
  }, [onState]);

  useEffect(() => {
    if (!enabled) return;
    if (typeof EventSource === "undefined") {
      onStateRef.current?.("polling");
      return;
    }

    let closed = false;
    const url = `/api/v1/updates/${encodeURIComponent(runId)}/events`;
    const source = new EventSource(url, { withCredentials: true });

    onStateRef.current?.("connecting");

    source.onopen = () => {
      if (!closed) onStateRef.current?.("live");
    };

    // The CP emits NAMED `event: task` frames (see apps/api/internal/update
    // handler.go writeEvent). `EventSource.onmessage` only fires for the
    // default unnamed `message` event, so a named-event stream silently
    // delivers nothing — which was the v0.9.0 bug: rows stayed "Queued"
    // forever even though the run had succeeded server-side. We listen on
    // the named event AND keep onmessage as a defensive fallback in case the
    // wire ever changes back to unnamed frames.
    const handleFrame = (msg: MessageEvent<string>) => {
      if (closed) return;
      let event: UpdateEvent;
      try {
        event = JSON.parse(msg.data) as UpdateEvent;
      } catch {
        return; // ignore heartbeats / malformed frames
      }
      patchEvent(queryClient, runId, event);
      if (event.run_status === "completed") {
        closed = true;
        source.close();
        onStateRef.current?.("closed");
      }
    };
    source.addEventListener("task", handleFrame as EventListener);
    source.onmessage = handleFrame;

    source.onerror = () => {
      // EventSource auto-reconnects, but a hard failure (proxy/unsupported)
      // means we should stop relying on it and let polling take over.
      if (closed) return;
      closed = true;
      source.close();
      onStateRef.current?.("polling");
    };

    return () => {
      closed = true;
      source.removeEventListener("task", handleFrame as EventListener);
      source.close();
    };
  }, [enabled, runId, queryClient]);
}

/** Normalize the generated `Error` body (or anything) into an Error instance. */
function toError(error: unknown): Error {
  if (error instanceof Error) return error;
  if (isApiError(error)) return new Error(error.message);
  return new Error("Request failed");
}

function isApiError(value: unknown): value is ApiError {
  return (
    typeof value === "object" &&
    value !== null &&
    "message" in value &&
    typeof value.message === "string"
  );
}
