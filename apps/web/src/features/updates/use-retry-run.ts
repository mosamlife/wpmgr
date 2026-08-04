import {
  useMutation,
  useQueryClient,
  type UseMutationResult,
} from "@tanstack/react-query";
import { retryUpdateRun } from "@wpmgr/api";

import { updatesKeys } from "./use-updates";
import type { RetryResult } from "./retry-contract";

// GH #336 retry mutation.
//
//   POST /api/v1/updates/runs/{id}/retry   { task_ids: [...] }
//   200  { run_id?, requested, created, excluded[], warning? }
//
// A retry always creates a NEW run and never mutates the run being viewed,
// which is why the source run's cache is only invalidated (it may now carry
// provenance) and the new run is fetched fresh on navigation.
//
// The 200 is returned even when NOTHING was created: that is a complete
// answer to a well-formed request, and the per-task reasons in `excluded` are
// the point. So this hook resolves with the whole result and never reduces it
// to a boolean success; the caller renders `requested`, `created` and every
// exclusion.

/** A retry the control plane refused, with the status and code intact. */
export class RetryRunError extends Error {
  readonly status: number;
  readonly code: string | undefined;

  constructor(message: string, status: number, code?: string) {
    super(message);
    this.name = "RetryRunError";
    this.status = status;
    this.code = code;
  }
}

/** The `message` field of the generated ApiError body, when there is one. */
function readApiMessage(value: unknown): string | undefined {
  if (typeof value !== "object" || value === null) return undefined;
  if (!("message" in value) || typeof value.message !== "string") {
    return undefined;
  }
  return value.message;
}

function readApiCode(value: unknown): string | undefined {
  if (typeof value !== "object" || value === null) return undefined;
  if (!("code" in value) || typeof value.code !== "string") return undefined;
  return value.code;
}

/**
 * Operator-facing message for a refusal, preferring the control plane's own
 * words. The status fallbacks exist so a bare 403/404/409/422 with no body is
 * still explained rather than surfacing "Request failed".
 */
function retryErrorMessage(status: number, body: unknown): string {
  const message = readApiMessage(body);
  if (message !== undefined && message.trim().length > 0) return message;
  if (status === 401 || status === 403) {
    return "You do not have permission to start updates in this organisation.";
  }
  if (status === 404) {
    return "This run no longer exists, so it cannot be retried.";
  }
  if (status === 409) {
    return "The agent self-update channel is not available on this control plane.";
  }
  if (status === 422) {
    return "None of the selected updates can be requested as they are.";
  }
  return "The retry could not be started.";
}

export interface RetryRunInput {
  /** Task ids from the run being retried. The unit is tasks, never sites. */
  taskIds: string[];
}

/**
 * Retry a set of tasks from an existing run. Resolves with the FULL result
 * (requested / created / excluded / warning) so the caller can render a
 * partial commit.
 */
export function useRetryRun(
  runId: string,
): UseMutationResult<RetryResult, Error, RetryRunInput> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ taskIds }: RetryRunInput) => {
      const { data, error, response } = await retryUpdateRun({
        path: { id: runId },
        body: { task_ids: taskIds },
      });
      if (error) {
        const status = response?.status ?? 0;
        throw new RetryRunError(
          retryErrorMessage(status, error),
          status,
          readApiCode(error),
        );
      }
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: () => {
      // The source run is untouched by design, but it may now carry
      // provenance, and the new run must appear in the list.
      void queryClient.invalidateQueries({ queryKey: updatesKeys.lists() });
      void queryClient.invalidateQueries({
        queryKey: updatesKeys.detail(runId),
      });
    },
  });
}
