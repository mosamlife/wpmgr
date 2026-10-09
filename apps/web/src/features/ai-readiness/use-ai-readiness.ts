import { useCallback, useEffect, useMemo, useState } from "react";
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from "@tanstack/react-query";
import {
  getFleetAiReadiness,
  getSiteAiReadiness,
  refreshSiteAiReadiness,
  type AiReadinessRefreshResult,
  type ApiError,
  type FleetAiReadiness,
  type SiteAiReadiness,
} from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

import type { AiReadinessRollup } from "./readiness-cell-model";
import { REFRESH_FORBIDDEN, REFRESH_UNREACHABLE } from "./readiness-copy";

// AI readiness hooks. The routes are
// GET  /api/v1/sites/{siteId}/ai/readiness
// GET  /api/v1/fleet/ai-readiness
// POST /api/v1/sites/{siteId}/ai/readiness/refresh
// (apps/api/internal/aireadiness/handler.go). Errors are the control plane's
// flat {code, message} envelope (apps/api/internal/server/httpx/respond.go).

export const aiReadinessKeys = {
  all: ["ai-readiness"] as const,
  site: (siteId: string) => [...aiReadinessKeys.all, "site", siteId] as const,
  fleet: () => [...aiReadinessKeys.all, "fleet"] as const,
};

/** "Check again" refetches the card this often, at most MAX_POLLS times. */
export const POLL_MS = 15_000;
export const MAX_POLLS = 8;

function isApiError(value: unknown): value is ApiError {
  return (
    typeof value === "object" &&
    value !== null &&
    "message" in value &&
    typeof (value as ApiError).message === "string"
  );
}

/** A failed card load. `status` lets the card tell "no such site" from a fault. */
export class AiReadinessLoadError extends Error {
  readonly status: number | undefined;
  constructor(message: string, status: number | undefined) {
    super(message);
    this.name = "AiReadinessLoadError";
    this.status = status;
  }
}

/**
 * What a site's readiness was computed from, as far as the site's own reports
 * go. These two timestamps move when new results land (a metadata report, a
 * tool-list read) and not otherwise. They do not move when the AI page creation
 * switch flips: turning it on refreshes the card and the rollup itself
 * (useEnableContentEditing).
 */
function resultsStamp(r: Pick<SiteAiReadiness, "metadata_as_of" | "abilities_as_of">): string {
  return `${r.metadata_as_of ?? ""}|${r.abilities_as_of ?? ""}`;
}

export function useSiteAiReadiness(siteId: string): UseQueryResult<SiteAiReadiness, Error> {
  const queryClient = useQueryClient();
  return useQuery({
    queryKey: aiReadinessKeys.site(siteId),
    queryFn: async (): Promise<SiteAiReadiness> => {
      const { data, error, response } = await getSiteAiReadiness({ path: { siteId } });
      if (error || !data) {
        const status = response?.status;
        throw new AiReadinessLoadError(
          isApiError(error) ? error.message : toError(error).message,
          status,
        );
      }
      // The fleet rollup is computed from the same facts. "Check again" marks it
      // stale once, when the request is accepted, which is before any results
      // exist; a rollup read after that is current only until results land.
      // Whenever a read finds newer results than the last one did, whether a
      // poll, a refocus or a return to the tab, the rollup is stale again.
      const previous = queryClient.getQueryData<SiteAiReadiness>(aiReadinessKeys.site(siteId));
      if (previous && resultsStamp(previous) !== resultsStamp(data)) {
        void queryClient.invalidateQueries({ queryKey: aiReadinessKeys.fleet() });
      }
      return data;
    },
  });
}

export function useFleetAiReadiness(): UseQueryResult<FleetAiReadiness, Error> {
  return useQuery({
    queryKey: aiReadinessKeys.fleet(),
    queryFn: async (): Promise<FleetAiReadiness> => {
      const { data, error } = await getFleetAiReadiness();
      if (error || !data) throw toError(error);
      return data;
    },
    staleTime: 60_000,
  });
}

/**
 * The fleet rollup as the Sites list consumes it. A refused or failed rollup is
 * "unavailable", never a thrown error: the page's primary content is the sites
 * list, so a missing rollup costs every cell its answer and nothing else. Data
 * already in hand wins over a later failed refetch.
 */
export function useAiReadinessRollup(): AiReadinessRollup {
  const query = useFleetAiReadiness();
  const { data, isError } = query;
  return useMemo<AiReadinessRollup>(() => {
    if (data) {
      return { state: "ready", bySite: new Map(data.sites.map((s) => [s.site_id, s])) };
    }
    return isError ? { state: "unavailable" } : { state: "loading" };
  }, [data, isError]);
}

export type RefreshFailureKind = "unreachable" | "forbidden" | "other";

/** A refused "Check again". `kind` selects the card's message. */
export class AiReadinessRefreshError extends Error {
  readonly kind: RefreshFailureKind;
  readonly status: number | undefined;
  constructor(kind: RefreshFailureKind, message: string, status?: number) {
    super(message);
    this.name = "AiReadinessRefreshError";
    this.kind = kind;
    this.status = status;
  }
}

/** 409 `site_unreachable` (service.go RequestRefresh): not enrolled, or the agent has gone quiet. */
export const CODE_SITE_UNREACHABLE = "site_unreachable";

const NETWORK_FAILURE_MESSAGE = "Check your connection and try again.";

export function toRefreshError(error: unknown, status: number | undefined): AiReadinessRefreshError {
  const api = isApiError(error) ? error : null;
  if (status === 409 && api?.code === CODE_SITE_UNREACHABLE) {
    return new AiReadinessRefreshError("unreachable", REFRESH_UNREACHABLE, status);
  }
  if (status === 403) {
    return new AiReadinessRefreshError("forbidden", REFRESH_FORBIDDEN, status);
  }
  return new AiReadinessRefreshError(
    "other",
    api?.message ?? NETWORK_FAILURE_MESSAGE,
    status,
  );
}

export function useRefreshAiReadiness(
  siteId: string,
): UseMutationResult<AiReadinessRefreshResult, AiReadinessRefreshError, void> {
  const queryClient = useQueryClient();
  return useMutation<AiReadinessRefreshResult, AiReadinessRefreshError, void>({
    mutationFn: async () => {
      let result: Awaited<ReturnType<typeof refreshSiteAiReadiness>>;
      try {
        result = await refreshSiteAiReadiness({ path: { siteId }, body: {} });
      } catch {
        // The request never produced a response (offline, DNS, CORS).
        throw new AiReadinessRefreshError("other", NETWORK_FAILURE_MESSAGE);
      }
      const { data, error, response } = result;
      if (error || !data) throw toRefreshError(error, response?.status);
      return data;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: aiReadinessKeys.site(siteId) });
      void queryClient.invalidateQueries({ queryKey: aiReadinessKeys.fleet() });
    },
  });
}

/**
 * A bounded refetch window. `start()` arms it; it then calls `refetch` every
 * `intervalMs`, at most `maxPolls` times, and stops on its own. Calling
 * `start()` again begins a fresh window. There is no way to leave it running:
 * the loop ends when the count is spent or the component unmounts.
 */
export function useBoundedPolling(
  refetch: () => unknown,
  intervalMs: number = POLL_MS,
  maxPolls: number = MAX_POLLS,
): { start: () => void; polling: boolean } {
  const [span, setSpan] = useState({ armed: false, done: 0 });

  useEffect(() => {
    if (!span.armed || span.done >= maxPolls) return;
    const id = setTimeout(() => {
      void refetch();
      setSpan((s) => ({ ...s, done: s.done + 1 }));
    }, intervalMs);
    return () => clearTimeout(id);
  }, [span, refetch, intervalMs, maxPolls]);

  const start = useCallback(() => setSpan({ armed: true, done: 0 }), []);
  return { start, polling: span.armed && span.done < maxPolls };
}
