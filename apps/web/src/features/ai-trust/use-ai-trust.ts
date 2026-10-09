import {
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
  type InfiniteData,
  type UseInfiniteQueryResult,
  type UseMutationResult,
  type UseQueryResult,
} from "@tanstack/react-query";
import {
  getAiConnectionUsage,
  getSiteAiMode,
  listAiActivity,
  putAiConnectionAuto,
  putSiteAiMode,
  type AiActivityPage,
  type AiAuto,
  type AiConnectionAuto,
  type AiConnectionUsage,
  type AiMode,
  type ApiError,
  type SiteAiMode,
} from "@wpmgr/api";

// AI trust: how much an AI connection may change on its own. A site's mode
// (GET/PUT /api/v1/sites/:siteId/ai/mode), a connection's switch and usage
// (/api/v1/ai/connections/:grantId/usage and /auto), and the feed of every
// approved AI change (/api/v1/ai/activity). Only a signed-in person can loosen
// any of it; the server enforces that, and these hooks only report its answer.

export type ActivityFilter =
  | "all"
  | "ran_automatically"
  | "approved_by_person"
  | "failed_or_unknown"
  | "undone";

export interface ActivityFilters {
  readonly filter: ActivityFilter;
  readonly siteId?: string;
  readonly grantId?: string;
}

export const aiTrustKeys = {
  all: ["ai-trust"] as const,
  siteMode: (siteId: string) => [...aiTrustKeys.all, "site-mode", siteId] as const,
  usage: (grantId: string) => [...aiTrustKeys.all, "usage", grantId] as const,
  activity: () => [...aiTrustKeys.all, "activity"] as const,
  activityList: (f: ActivityFilters) =>
    [...aiTrustKeys.activity(), f.filter, f.siteId ?? null, f.grantId ?? null] as const,
};

/** Rows per activity page. The API accepts up to 100. */
export const ACTIVITY_PAGE_SIZE = 50;

/** Refusal codes from the AI-trust routes (AiControlRefusalCode in the generated client). */
export const CODE_STALE_VERSION = "stale_version";
export const CODE_PAUSED = "paused";
export const CODE_AGENT_OUTDATED = "agent_outdated";

/** Carries the server's own `code`, the HTTP status and the envelope's `details`. */
export class AiTrustError extends Error {
  readonly code: string;
  readonly status: number | undefined;
  readonly details: Readonly<Record<string, unknown>> | null;
  constructor(code: string, message: string, status?: number, details?: Record<string, unknown> | null) {
    super(message);
    this.name = "AiTrustError";
    this.code = code;
    this.status = status;
    this.details = details ?? null;
  }
}

/**
 * 409 `stale_version`: the mode changed after the caller read it, so nothing
 * was saved. `details` names the mode and version as they now stand.
 */
export class StaleSiteModeError extends AiTrustError {
  readonly currentMode: AiMode | null;
  readonly currentVersion: number | null;
  constructor(message: string, details: Record<string, unknown> | null) {
    super(CODE_STALE_VERSION, message, 409, details);
    this.name = "StaleSiteModeError";
    const mode = details?.["mode"];
    const version = details?.["version"];
    this.currentMode = mode === "ask" || mode === "ai_drafts" || mode === "full" ? mode : null;
    this.currentVersion = typeof version === "number" && Number.isInteger(version) ? version : null;
  }
}

function isApiError(value: unknown): value is ApiError {
  return (
    typeof value === "object" &&
    value !== null &&
    "message" in value &&
    typeof (value as ApiError).message === "string"
  );
}

export function toAiTrustError(error: unknown, status: number | undefined): AiTrustError {
  const apiError = isApiError(error) ? error : null;
  const details = apiError?.details ?? null;
  if (status === 409 && apiError?.code === CODE_STALE_VERSION) {
    return new StaleSiteModeError(apiError.message, details);
  }
  const code =
    apiError?.code ?? (status === 403 ? "forbidden" : status === 404 ? "not_found" : "request_failed");
  return new AiTrustError(code, apiError?.message ?? "The request could not be completed. Try again.", status, details);
}

export function useSiteAiMode(siteId: string, enabled = true): UseQueryResult<SiteAiMode, AiTrustError> {
  return useQuery({
    queryKey: aiTrustKeys.siteMode(siteId),
    enabled,
    queryFn: async (): Promise<SiteAiMode> => {
      const { data, error, response } = await getSiteAiMode({ path: { siteId } });
      const status = response?.status;
      if (error) throw toAiTrustError(error, status);
      if (!data) throw new AiTrustError("empty_response", "Empty response", status);
      return data;
    },
  });
}

export interface PutSiteModeVars {
  readonly mode: "ask" | "ai_drafts";
  /** The `version` the card last read; the server refuses a stale one. */
  readonly version: number;
}

export function usePutSiteAiMode(
  siteId: string,
): UseMutationResult<SiteAiMode, AiTrustError, PutSiteModeVars> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async ({ mode, version }: PutSiteModeVars): Promise<SiteAiMode> => {
      const { data, error, response } = await putSiteAiMode({
        path: { siteId },
        body: { mode, version },
      });
      const status = response?.status;
      if (error) throw toAiTrustError(error, status);
      if (!data) throw new AiTrustError("empty_response", "Empty response", status);
      return data;
    },
    onSuccess: (data) => {
      qc.setQueryData(aiTrustKeys.siteMode(siteId), data);
    },
    // A refusal can mean the mode moved under the reader (stale_version), so
    // the card re-reads after every failure as well.
    onError: () => {
      void qc.invalidateQueries({ queryKey: aiTrustKeys.siteMode(siteId) });
    },
  });
}

export function useAiConnectionUsage(
  grantId: string,
  enabled = true,
): UseQueryResult<AiConnectionUsage, AiTrustError> {
  return useQuery({
    queryKey: aiTrustKeys.usage(grantId),
    enabled,
    queryFn: async (): Promise<AiConnectionUsage> => {
      const { data, error, response } = await getAiConnectionUsage({ path: { grantId } });
      const status = response?.status;
      if (error) throw toAiTrustError(error, status);
      if (!data) throw new AiTrustError("empty_response", "Empty response", status);
      return data;
    },
    // Counts move as the connection works; a slow poll keeps the hour current.
    refetchInterval: 60_000,
  });
}

export function usePutAiConnectionAuto(
  grantId: string,
): UseMutationResult<AiConnectionAuto, AiTrustError, AiAuto> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (aiAuto: AiAuto): Promise<AiConnectionAuto> => {
      const { data, error, response } = await putAiConnectionAuto({
        path: { grantId },
        body: { ai_auto: aiAuto },
      });
      const status = response?.status;
      if (error) throw toAiTrustError(error, status);
      if (!data) throw new AiTrustError("empty_response", "Empty response", status);
      return data;
    },
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: aiTrustKeys.usage(grantId) });
    },
  });
}

export function useAiActivityPages(
  filters: ActivityFilters,
  refetchInterval: number,
): UseInfiniteQueryResult<InfiniteData<AiActivityPage, string | null>, AiTrustError> {
  return useInfiniteQuery({
    queryKey: aiTrustKeys.activityList(filters),
    initialPageParam: null as string | null,
    queryFn: async ({ pageParam }): Promise<AiActivityPage> => {
      const { data, error, response } = await listAiActivity({
        query: {
          filter: filters.filter,
          ...(filters.siteId ? { site_id: filters.siteId } : {}),
          ...(filters.grantId ? { grant_id: filters.grantId } : {}),
          limit: ACTIVITY_PAGE_SIZE,
          ...(pageParam ? { cursor: pageParam } : {}),
        },
      });
      const status = response?.status;
      if (error) throw toAiTrustError(error, status);
      if (!data) throw new AiTrustError("empty_response", "Empty response", status);
      return data as AiActivityPage;
    },
    getNextPageParam: (last) => last.next_cursor ?? undefined,
    refetchInterval,
  });
}
