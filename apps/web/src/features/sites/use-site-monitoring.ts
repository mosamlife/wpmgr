import { useMutation, useQueryClient, type UseMutationResult } from "@tanstack/react-query";
import {
  pauseSiteMonitoring,
  resumeSiteMonitoring,
  type MonitoringBulkResult,
  type ApiError,
} from "@wpmgr/api";

import { sitesKeys } from "@/features/sites/use-sites";
import { monitoringRequestErrorMessage } from "@/features/sites/monitoring-pause";

// GH #414 phase 4b — the two bulk mutations behind the Sites bulk menu.
//
// Server state, so TanStack Query owns it (never Zustand). Both routes are
// idempotent and return 200 with a per-site report even when some sites were
// refused, so a non-2xx here is a WHOLE-REQUEST failure and nothing else.

/**
 * A named error carrying the server's stable `code`, so the caller can render
 * the right sentence for `request_too_large` / `principal_required` /
 * `too_many_sites` / `resume_at_in_past` instead of one generic failure.
 *
 * Branching on the code rather than the status is what keeps the distinction:
 * `request_too_large` and `resume_at_in_past` are both 422 from the same route
 * and mean entirely different things to the operator.
 */
export class MonitoringRequestError extends Error {
  readonly code: string;
  readonly status: number | undefined;

  constructor(code: string, message: string, status?: number) {
    super(message);
    this.name = "MonitoringRequestError";
    this.code = code;
    this.status = status;
  }
}

/**
 * Turn the generated `{ error, response }` pair into a MonitoringRequestError,
 * preferring our own sentence for a code we know and falling back to the
 * server's own message (never to "Request failed") when we do not.
 */
function toMonitoringError(error: unknown, status: number | undefined): MonitoringRequestError {
  const apiError = isApiError(error) ? error : null;
  const code = apiError?.code ?? (status === 403 ? "principal_required" : "request_failed");
  const known = monitoringRequestErrorMessage(code);
  const message = known ?? apiError?.message ?? "Monitoring could not be changed.";
  return new MonitoringRequestError(code, message, status);
}

function isApiError(value: unknown): value is ApiError {
  return (
    typeof value === "object" &&
    value !== null &&
    "message" in value &&
    typeof (value as ApiError).message === "string"
  );
}

export interface PauseMonitoringVars {
  siteIds: string[];
  reason?: string;
  resumeAt?: string;
}

export interface ResumeMonitoringVars {
  siteIds: string[];
}

/**
 * Pause monitoring on many sites.
 *
 * Invalidates the whole sites tree in `onSuccess` so every list and detail
 * re-pulls the authoritative pause columns. Push is a hint; pull is the truth,
 * and the response's per-site echo is deliberately NOT written into the cache:
 * a refused site would otherwise be patched with state the database never
 * stored.
 */
export function usePauseMonitoring(): UseMutationResult<
  MonitoringBulkResult,
  MonitoringRequestError,
  PauseMonitoringVars
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ siteIds, reason, resumeAt }: PauseMonitoringVars) => {
      const { data, error, response } = await pauseSiteMonitoring({
        body: {
          site_ids: siteIds,
          ...(reason ? { reason } : {}),
          ...(resumeAt ? { resume_at: resumeAt } : {}),
        },
      });
      if (error) throw toMonitoringError(error, response?.status);
      if (!data) throw new MonitoringRequestError("empty_response", "Empty response");
      return data;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: sitesKeys.all });
    },
  });
}

/** Resume monitoring on many sites. Same contract, same invalidation. */
export function useResumeMonitoring(): UseMutationResult<
  MonitoringBulkResult,
  MonitoringRequestError,
  ResumeMonitoringVars
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ siteIds }: ResumeMonitoringVars) => {
      const { data, error, response } = await resumeSiteMonitoring({
        body: { site_ids: siteIds },
      });
      if (error) throw toMonitoringError(error, response?.status);
      if (!data) throw new MonitoringRequestError("empty_response", "Empty response");
      return data;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: sitesKeys.all });
    },
  });
}
