import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { client } from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

// Restore-run domain hooks — hand-rolled Gin endpoints, not in @wpmgr/api.
// Authenticated via the configured Hey API client (credentials: "include" session cookie).
// Pattern mirrors features/security/use-scan.ts exactly.

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

export type RestoreStatus =
  | "queued"
  | "running"
  | "completed"
  | "failed"
  | "rolled_back";

export interface RestoreRun {
  id: string;
  site_id: string;
  snapshot_id: string;
  mode: string;
  components: string[];
  status: RestoreStatus;
  current_phase: string | null;
  error: string | null;
  triggered_by: string | null;
  triggered_by_email: string | null;
  triggered_by_name: string | null;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
}

export interface RestoreRunEvent {
  id: number;
  phase: string;
  status: string | null;
  message: string | null;
  occurred_at: string;
}

// ---------------------------------------------------------------------------
// Cache key family
// ---------------------------------------------------------------------------

export const restoreKeys = {
  all: ["restores"] as const,
  forSite: (siteId: string) =>
    ["restores", "site", siteId] as const,
  detail: (restoreId: string) =>
    ["restores", "detail", restoreId] as const,
  events: (restoreId: string) =>
    ["restores", "events", restoreId] as const,
};

// ---------------------------------------------------------------------------
// Terminal statuses where polling stops
// ---------------------------------------------------------------------------

const TERMINAL_STATUSES = new Set<RestoreStatus>([
  "completed",
  "failed",
  "rolled_back",
]);

export function isRestoreTerminal(status: RestoreStatus): boolean {
  return TERMINAL_STATUSES.has(status);
}

// ---------------------------------------------------------------------------
// Helper — authenticated GET via the Hey API client (same pattern as use-scan.ts)
// ---------------------------------------------------------------------------

async function apiGet<T>(url: string): Promise<T> {
  const result = await client.get({ url });
  if (result.error !== undefined) throw toError(result.error);
  return result.data as T;
}

// ---------------------------------------------------------------------------
// useRestoreRuns — GET /api/v1/sites/{siteId}/restores
// ---------------------------------------------------------------------------

export function useRestoreRuns(
  siteId: string,
): UseQueryResult<RestoreRun[], Error> {
  return useQuery({
    queryKey: restoreKeys.forSite(siteId),
    queryFn: async () => {
      const data = await apiGet<{ items: RestoreRun[] }>(
        `/api/v1/sites/${encodeURIComponent(siteId)}/restores`,
      );
      return data.items ?? [];
    },
    refetchInterval: (query) => {
      const items = query.state.data ?? [];
      return items.some((r) => !isRestoreTerminal(r.status)) ? 3000 : false;
    },
    enabled: Boolean(siteId),
  });
}

// ---------------------------------------------------------------------------
// useRestoreRun — GET /api/v1/restores/{restoreId}
// ---------------------------------------------------------------------------

export function useRestoreRun(
  restoreId: string,
): UseQueryResult<RestoreRun, Error> {
  return useQuery({
    queryKey: restoreKeys.detail(restoreId),
    queryFn: async () =>
      apiGet<RestoreRun>(
        `/api/v1/restores/${encodeURIComponent(restoreId)}`,
      ),
    enabled: Boolean(restoreId),
    refetchInterval: (query) => {
      const data = query.state.data;
      if (!data) return false;
      return isRestoreTerminal(data.status) ? false : 3000;
    },
  });
}

// ---------------------------------------------------------------------------
// useRestoreEvents — GET /api/v1/restores/{restoreId}/events?after=<lastId>
// Incremental: tracks the last seen event id and only fetches new events.
// Polls every 3s while running; stops once the caller signals terminal.
// ---------------------------------------------------------------------------

export function useRestoreEvents(
  restoreId: string,
  { running }: { running: boolean },
): UseQueryResult<RestoreRunEvent[], Error> {
  return useQuery({
    queryKey: restoreKeys.events(restoreId),
    queryFn: async (ctx) => {
      // Accumulate events across fetches by keeping them in the cache.
      // Each fetch uses ?after=<last id> to fetch only new events and
      // appends them to the existing list.
      const prev: RestoreRunEvent[] =
        ctx.client.getQueryData<RestoreRunEvent[]>(
          restoreKeys.events(restoreId),
        ) ?? [];
      const lastId = prev.length > 0 ? prev[prev.length - 1]!.id : 0;
      const url =
        lastId > 0
          ? `/api/v1/restores/${encodeURIComponent(restoreId)}/events?after=${lastId}`
          : `/api/v1/restores/${encodeURIComponent(restoreId)}/events`;
      const data = await apiGet<{ items: RestoreRunEvent[] }>(url);
      const newEvents = data.items ?? [];
      if (newEvents.length === 0) return prev;
      return [...prev, ...newEvents];
    },
    enabled: Boolean(restoreId),
    refetchInterval: running ? 3000 : false,
    // Retain accumulated events in cache indefinitely (they don't change once written).
    staleTime: 0,
    gcTime: Infinity,
  });
}
