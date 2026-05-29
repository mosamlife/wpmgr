import {
  useQuery,
  useQueryClient,
  type UseQueryResult,
} from "@tanstack/react-query";

import {
  getBackupSqlInspection,
  type SqlInspection,
} from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

// Server-state hook for the snapshot SQL inspection report.
//
// The endpoint has three terminal shapes:
//   200 — SqlInspection body ready (agent-generated or CP-legacy parser done).
//   202 — Legacy snapshot. CP enqueued a streaming parse; poll the same URL.
//   503 — `inspection_unwired`: snapshot was created by an older agent that
//         didn't ship inspection JSON and the CP fallback isn't wired for this
//         snapshot's storage backend. Not retryable for this snapshot.
//
// We model state so the dialog never has to inspect HTTP status itself:
//   { phase: "loading" }                — first request in flight
//   { phase: "analyzing", elapsedMs }   — 202 + still polling (with elapsedMs
//                                          so the UI can switch its copy after
//                                          30s to the "still analyzing" line)
//   { phase: "ready", report }          — 200 + SqlInspection body
//   { phase: "unwired" }                — 503 inspection_unwired
//   { phase: "error", message }         — any other failure
//
// Polling: refetchInterval=5000ms while analyzing, off once ready/unwired/error.
// React Query handles cancel-on-unmount automatically, so when the dialog
// closes and the hook unmounts the polling stops on its own.

export type SqlInspectionState =
  | { phase: "loading" }
  | { phase: "analyzing"; elapsedMs: number }
  | { phase: "ready"; report: SqlInspection }
  | { phase: "unwired" }
  | { phase: "error"; message: string };

interface QueryShape {
  state: SqlInspectionState;
  /** Monotonic wall-clock anchor for the first 202 we saw; lets the UI
   *  compute elapsed time without re-rendering this hook every second. */
  analyzingSince: number | null;
}

export const sqlInspectionKey = (snapshotId: string) =>
  ["backups", "sql-inspection", snapshotId] as const;

export function useSqlInspection(
  snapshotId: string,
  enabled: boolean,
): UseQueryResult<QueryShape, Error> {
  const queryClient = useQueryClient();

  return useQuery({
    queryKey: sqlInspectionKey(snapshotId),
    enabled,
    // Keep the previous polling result while a fresh poll is in flight so the
    // "Analyzing…" copy doesn't flash back to a skeleton between ticks.
    placeholderData: (prev) => prev,
    queryFn: async (): Promise<QueryShape> => {
      const prev = queryClient.getQueryData<QueryShape>(
        sqlInspectionKey(snapshotId),
      );
      const analyzingSince =
        prev?.state.phase === "analyzing" ? prev.analyzingSince : null;

      const { data, error, response } = await getBackupSqlInspection({
        path: { snapshotId },
      });

      if (response?.status === 202) {
        const since = analyzingSince ?? Date.now();
        return {
          state: { phase: "analyzing", elapsedMs: Date.now() - since },
          analyzingSince: since,
        };
      }

      // The OpenAPI spec for /sql-inspection doesn't enumerate 503 in the
      // generated `Errors` map, so the SDK surfaces it as `error` being
      // undefined and `data` being absent. We probe `response?.status`
      // directly — the SDK's response object is the raw fetch Response.
      if (response?.status === 503) {
        return { state: { phase: "unwired" }, analyzingSince: null };
      }

      if (response?.status === 404) {
        return {
          state: {
            phase: "error",
            message:
              "Snapshot not found. The snapshot may have been deleted while you had this dialog open. Close and reopen from the snapshot list.",
          },
          analyzingSince: null,
        };
      }

      if (error) {
        const e = toError(error);
        return {
          state: { phase: "error", message: e.message },
          analyzingSince: null,
        };
      }

      if (data) {
        return {
          state: { phase: "ready", report: data as SqlInspection },
          analyzingSince: null,
        };
      }

      return {
        state: {
          phase: "error",
          message:
            "Empty response from the inspection endpoint. The control plane returned no body. Retry in a moment; if it persists, check control-plane logs for `inspection`.",
        },
        analyzingSince: null,
      };
    },
    // Poll every 5s while the CP is still parsing. The API echoes
    // Retry-After: 5 for 202 responses; we honor that statically here.
    refetchInterval: (query) => {
      const data = query.state.data;
      if (!data) return false;
      return data.state.phase === "analyzing" ? 5000 : false;
    },
    // No retry on terminal failures — the hook already maps them to a
    // descriptive `error` phase the dialog renders inline.
    retry: false,
  });
}
