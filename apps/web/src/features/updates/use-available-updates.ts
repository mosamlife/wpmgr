import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from "@tanstack/react-query";

import { sitesKeys } from "@/features/sites/use-sites";
import type { SiteAvailableUpdates } from "./types";

// Server-state hooks for the per-site "available updates" panel (Track C).
//
// Two endpoints are consumed:
//   GET  /api/v1/sites/{siteId}/updates/available   — list outstanding updates
//   POST /api/v1/sites/{siteId}/updates/refresh     — ask the agent to re-check
//
// Neither endpoint is in the generated @wpmgr/api SDK yet (Track B is shipping
// the OpenAPI regen in parallel). To keep this PR unblocked the hooks call the
// same-origin REST surface directly with `fetch`, matching the credentials /
// content-type contract the generated client uses (`credentials: "include"`,
// `Accept: application/json`). Once Track B merges, swap the inner request
// helpers for the generated `getSiteUpdatesAvailable` / `refreshSiteUpdates`
// operations — keys, error shapes, and return types are deliberately the same.

export const availableUpdatesKeys = {
  all: ["available-updates"] as const,
  detail: (siteId: string) =>
    [...availableUpdatesKeys.all, siteId] as const,
};

/** Polling cadence for the "available updates" panel. 60s feels live enough. */
const AVAILABLE_REFETCH_MS = 60_000;
const AVAILABLE_STALE_MS = 30_000;

class RefreshConflictError extends Error {
  constructor(message = "A refresh is already in progress") {
    super(message);
    this.name = "RefreshConflictError";
  }
}

async function fetchJson<T>(input: string, init?: RequestInit): Promise<T> {
  const res = await fetch(input, {
    credentials: "include",
    headers: { Accept: "application/json", ...(init?.headers ?? {}) },
    ...init,
  });
  if (res.status === 409) throw new RefreshConflictError();
  if (!res.ok) {
    let message = `Request failed (${res.status})`;
    try {
      const body = (await res.json()) as { message?: string };
      if (body?.message) message = body.message;
    } catch {
      // body wasn't JSON; keep the generic message
    }
    throw new Error(message);
  }
  if (res.status === 202 || res.status === 204) {
    return undefined as T;
  }
  return (await res.json()) as T;
}

/**
 * Fetch the list of outstanding updates the agent has reported for a site.
 * Polls in the background so the badge count stays roughly fresh while the
 * page is open; staleTime is shorter than the refetch interval so manual
 * refetch() calls actually hit the network.
 */
export function useAvailableUpdates(
  siteId: string,
): UseQueryResult<SiteAvailableUpdates, Error> {
  return useQuery({
    queryKey: availableUpdatesKeys.detail(siteId),
    queryFn: () =>
      fetchJson<SiteAvailableUpdates>(
        `/api/v1/sites/${encodeURIComponent(siteId)}/updates/available`,
      ),
    staleTime: AVAILABLE_STALE_MS,
    refetchInterval: AVAILABLE_REFETCH_MS,
  });
}

/**
 * Ask the control plane to enqueue an agent "check for updates" probe. The
 * 202 response means "accepted"; the resulting agent push will land via the
 * normal site-update channel and invalidate caches there. We also invalidate
 * the available-updates query so the panel will refetch as soon as the agent
 * reports back.
 */
export function useRefreshSiteUpdates(
  siteId: string,
): UseMutationResult<void, Error, void> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: () =>
      fetchJson<void>(
        `/api/v1/sites/${encodeURIComponent(siteId)}/updates/refresh`,
        { method: "POST" },
      ),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: sitesKeys.detail(siteId) });
      void queryClient.invalidateQueries({
        queryKey: availableUpdatesKeys.detail(siteId),
      });
    },
  });
}

export { RefreshConflictError };
