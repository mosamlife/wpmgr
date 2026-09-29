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
  /**
   * GH #791 — `backup_schedule_runs.attempt_error` (m148): the last failed
   * attempt's error while the run is still `running` and being retried.
   * `error` keeps meaning the final failure reason only. Mirrors
   * `BackupSnapshot`'s `attempt_error` split — see
   * `format-progress.ts`'s `snapshotAttemptError` for why.
   */
  attempt_error: string | null;
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
// useScheduleRuns — GET /api/v1/sites/{siteId}/schedule-runs
// Omitting ?status returns both sets in one response: { upcoming, past }.
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
      // The CP returns { upcoming: [], past: [] } directly — no status filter
      // needed; omitting ?status returns both sets in one response.
      const data = await apiGet<{ upcoming: ScheduleRun[]; past: ScheduleRun[] }>(
        `/api/v1/sites/${encodeURIComponent(siteId)}/schedule-runs`,
      );
      const upcoming = data.upcoming ?? [];
      const past = data.past ?? [];
      return { all: [...upcoming, ...past], upcoming, past };
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
