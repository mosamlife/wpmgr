import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { client } from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

// Schedule-run domain hooks — hand-rolled Gin endpoints, NOT in the ogen spec.
// Authenticated via the configured Hey API client (credentials:"include" session
// cookie). Pattern mirrors use-restores.ts exactly.

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

export type ScheduleRunStatus =
  | "scheduled"
  | "queued"
  | "running"
  | "completed"
  | "failed"
  | "skipped"
  | "canceled";

export interface ScheduleRun {
  id: string;
  tenant_id: string;
  site_id: string;
  schedule_id: string;
  snapshot_id: string | null;
  scheduled_for: string; // RFC 3339 UTC
  status: ScheduleRunStatus;
  kind: string;
  error: string | null;
  triggered_by: string | null;
  triggered_by_email: string | null;
  triggered_by_name: string | null;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
  updated_at: string;
}

// ---------------------------------------------------------------------------
// Cache key family
// ---------------------------------------------------------------------------

export const scheduleKeys = {
  all: ["schedule-runs"] as const,
  forSite: (siteId: string) =>
    ["schedule-runs", "site", siteId] as const,
  detail: (runId: string) =>
    ["schedule-runs", "detail", runId] as const,
};

// ---------------------------------------------------------------------------
// Terminal statuses where polling stops
// ---------------------------------------------------------------------------

const TERMINAL_STATUSES = new Set<ScheduleRunStatus>([
  "completed",
  "failed",
  "skipped",
  "canceled",
]);

export function isScheduleRunTerminal(status: ScheduleRunStatus): boolean {
  return TERMINAL_STATUSES.has(status);
}

// ---------------------------------------------------------------------------
// Helper — authenticated GET via the Hey API client
// ---------------------------------------------------------------------------

async function apiGet<T>(url: string): Promise<T> {
  const result = await client.get({ url });
  if (result.error !== undefined) throw toError(result.error);
  return result.data as T;
}

// ---------------------------------------------------------------------------
// useScheduleRuns — GET /api/v1/sites/{siteId}/schedule-runs?status=both
// Returns { upcoming, past } splits from the full list.
// ---------------------------------------------------------------------------

export interface UseScheduleRunsResult {
  upcoming: ScheduleRun[];
  past: ScheduleRun[];
  all: ScheduleRun[];
}

export function useScheduleRuns(
  siteId: string,
): UseQueryResult<UseScheduleRunsResult, Error> {
  return useQuery({
    queryKey: scheduleKeys.forSite(siteId),
    queryFn: async () => {
      const data = await apiGet<{ items: ScheduleRun[] }>(
        `/api/v1/sites/${encodeURIComponent(siteId)}/schedule-runs?status=both`,
      );
      const items = data.items ?? [];
      const now = Date.now();
      const upcoming = items.filter(
        (r) =>
          !isScheduleRunTerminal(r.status) ||
          new Date(r.scheduled_for).getTime() > now,
      );
      const past = items.filter(
        (r) =>
          isScheduleRunTerminal(r.status) &&
          new Date(r.scheduled_for).getTime() <= now,
      );
      return { all: items, upcoming, past };
    },
    refetchInterval: (query) => {
      const items = query.state.data?.all ?? [];
      return items.some((r) => !isScheduleRunTerminal(r.status)) ? 3000 : false;
    },
    enabled: Boolean(siteId),
  });
}

// ---------------------------------------------------------------------------
// useScheduleRun — GET /api/v1/schedule-runs/{runId}
// ---------------------------------------------------------------------------

export function useScheduleRun(
  runId: string,
): UseQueryResult<ScheduleRun, Error> {
  return useQuery({
    queryKey: scheduleKeys.detail(runId),
    queryFn: async () =>
      apiGet<ScheduleRun>(
        `/api/v1/schedule-runs/${encodeURIComponent(runId)}`,
      ),
    enabled: Boolean(runId),
    refetchInterval: (query) => {
      const data = query.state.data;
      if (!data) return false;
      return isScheduleRunTerminal(data.status) ? false : 3000;
    },
  });
}
