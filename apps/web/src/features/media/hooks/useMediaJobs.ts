import {
  useQuery,
  type UseQueryResult,
} from "@tanstack/react-query";
import { client } from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

import type { ListJobsResponse, MediaJobDetail } from "../types";
import { isTerminalJobState } from "../types";
import { mediaKeys } from "./useMediaAssets";

// Server-state hooks for media jobs (list + single job detail with variants).
//
//   GET /api/v1/sites/:siteId/media/jobs?cursor&limit&state  → { items, next_cursor }
//   GET /api/v1/sites/:siteId/media/jobs/:jobId              → job + variants
//
// SSE drives the live JobsDrawer rows; these hooks back the authoritative
// "Jobs" list and the per-job variant breakdown (the side drawer / job detail).
// The detail query polls only while the job is non-terminal so a re-opened
// drawer converges even if a completion frame was missed.

export function useMediaJobs(
  siteId: string,
  state?: string,
): UseQueryResult<ListJobsResponse, Error> {
  return useQuery({
    queryKey: mediaKeys.jobs(siteId, state),
    queryFn: async () => {
      const query: Record<string, string> = {};
      if (state) query.state = state;
      const { data, error } = await client.get<{ 200: ListJobsResponse }>({
        url: `/api/v1/sites/${encodeURIComponent(siteId)}/media/jobs`,
        query,
      });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
  });
}

export function useMediaJob(
  siteId: string,
  jobId: string | null,
): UseQueryResult<MediaJobDetail, Error> {
  return useQuery({
    queryKey: jobId ? mediaKeys.job(jobId) : [...mediaKeys.all, "job", "none"],
    enabled: jobId !== null,
    queryFn: async () => {
      const { data, error } = await client.get<{ 200: MediaJobDetail }>({
        url: `/api/v1/sites/${encodeURIComponent(siteId)}/media/jobs/${encodeURIComponent(jobId ?? "")}`,
      });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    // Poll while the job is still running; stop once terminal. SSE patches keep
    // it fresh in the common case — this is the missed-frame backstop.
    refetchInterval: (query) => {
      const d = query.state.data;
      if (!d) return false;
      return isTerminalJobState(d.state) ? false : 2000;
    },
  });
}
