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
  approveAbilityRequest,
  declineAbilityRequest,
  enableSiteContentEditing,
  getSiteContentEditing,
  listAbilityRequests,
  listSiteAbilityRequests,
  undoAbilityRequest,
  type AbilityRequest,
  type AbilityRequestList,
  type AbilityRequestOrgList,
  type ApiError,
  type ContentEditingState,
} from "@wpmgr/api";

import { aiReadinessKeys } from "@/features/ai-readiness/use-ai-readiness";
import { aiTrustKeys } from "@/features/ai-trust/use-ai-trust";

// AI site-change requests (engine slice E2): the per-site queue, approve,
// decline, undo, and the per-site "AI editing" switch. The routes are all
// under /api/v1/sites/:siteId/ai (apps/api/internal/abilityrequest/handler.go).

export const abilityRequestKeys = {
  all: ["ability-requests"] as const,
  site: (siteId: string) => [...abilityRequestKeys.all, "site", siteId] as const,
  org: () => [...abilityRequestKeys.all, "org"] as const,
  editing: (siteId: string) => ["content-editing", siteId] as const,
};

/** Rows per page. The API accepts up to 100. */
export const ABILITY_PAGE_SIZE = 50;

/** Carries the server's own error `code` (service.go Code* constants, content_editing.go). */
export class AbilityRequestError extends Error {
  readonly code: string;
  readonly status: number | undefined;
  constructor(code: string, message: string, status?: number) {
    super(message);
    this.name = "AbilityRequestError";
    this.code = code;
    this.status = status;
  }
}

/** Server code for a stale presented_digest (service.go CodeRequestChanged). */
export const CODE_REQUEST_CHANGED = "ability_request_changed";
/** content_editing.go: the site's agent is below the page-create floor. */
/** undo.go CodeUndoRetry: the site did not settle the undo; it is open again (HTTP 503). */
export const CODE_UNDO_RETRY = "ability_request_undo_retry";
export const CODE_AGENT_OUTDATED = "content_editing_agent_outdated";

function isApiError(value: unknown): value is ApiError {
  return (
    typeof value === "object" &&
    value !== null &&
    "message" in value &&
    typeof (value as ApiError).message === "string"
  );
}

function toAbilityError(error: unknown, status: number | undefined): AbilityRequestError {
  const apiError = isApiError(error) ? error : null;
  const code =
    apiError?.code ?? (status === 403 ? "forbidden" : status === 404 ? "not_found" : "request_failed");
  return new AbilityRequestError(
    code,
    apiError?.message ?? "The request could not be completed. Try again.",
    status,
  );
}

export function useAbilityRequestPages(
  siteId: string,
  enabled: boolean,
): UseInfiniteQueryResult<InfiniteData<AbilityRequestList, number>, Error> {
  return useInfiniteQuery({
    queryKey: [...abilityRequestKeys.site(siteId), "paged"] as const,
    initialPageParam: 0,
    enabled,
    queryFn: async ({ pageParam }): Promise<AbilityRequestList> => {
      const { data, error, response } = await listSiteAbilityRequests({
        path: { siteId },
        query: { limit: ABILITY_PAGE_SIZE, offset: pageParam },
      });
      const status = response?.status;
      if (error) throw toAbilityError(error, status);
      if (!data) throw new AbilityRequestError("empty_response", "Empty response", status);
      return data;
    },
    getNextPageParam: (last) =>
      last.requests.length >= ABILITY_PAGE_SIZE ? last.offset + last.requests.length : undefined,
    // Pull is the truth: a short poll keeps pending, running and done fresh.
    refetchInterval: 15_000,
  });
}

/**
 * The organisation-wide queue (GET /api/v1/ai/ability-requests, GH #828).
 * `pending_count` is the server's whole-queue figure for the tab badge.
 */
export function useOrgAbilityRequestPages(): UseInfiniteQueryResult<
  InfiniteData<AbilityRequestOrgList, number>,
  Error
> {
  return useInfiniteQuery({
    queryKey: [...abilityRequestKeys.org(), "paged"] as const,
    initialPageParam: 0,
    queryFn: async ({ pageParam }): Promise<AbilityRequestOrgList> => {
      const { data, error, response } = await listAbilityRequests({
        query: { limit: ABILITY_PAGE_SIZE, offset: pageParam },
      });
      const status = response?.status;
      if (error) throw toAbilityError(error, status);
      if (!data) throw new AbilityRequestError("empty_response", "Empty response", status);
      return data;
    },
    getNextPageParam: (last) =>
      last.requests.length >= ABILITY_PAGE_SIZE ? last.offset + last.requests.length : undefined,
    refetchInterval: 30_000,
  });
}

/** The badge count alone: one row, same key family as the list so decisions invalidate it. */
export function useOrgAbilityPendingCount(): UseQueryResult<AbilityRequestOrgList, Error> {
  return useQuery({
    queryKey: [...abilityRequestKeys.org(), "count"] as const,
    queryFn: async (): Promise<AbilityRequestOrgList> => {
      const { data, error, response } = await listAbilityRequests({ query: { limit: 1, offset: 0 } });
      const status = response?.status;
      if (error) throw toAbilityError(error, status);
      if (!data) throw new AbilityRequestError("empty_response", "Empty response", status);
      return data;
    },
    refetchInterval: 30_000,
  });
}

interface DecideVars {
  readonly siteId: string;
  readonly requestId: string;
}

function useAbilityMutation<V extends DecideVars>(
  call: (vars: V) => Promise<{ data?: AbilityRequest; error?: unknown; response?: Response }>,
): UseMutationResult<AbilityRequest, AbilityRequestError, V> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (vars: V): Promise<AbilityRequest> => {
      const { data, error, response } = await call(vars);
      const status = response?.status;
      if (error) throw toAbilityError(error, status);
      if (!data) throw new AbilityRequestError("empty_response", "Empty response", status);
      return data;
    },
    // Settled, not success: a 409 means the row moved under the reader, and
    // the list must show where it went.
    onSettled: (_d, _e, vars) => {
      void qc.invalidateQueries({ queryKey: abilityRequestKeys.site(vars.siteId) });
      void qc.invalidateQueries({ queryKey: abilityRequestKeys.org() });
      // An approval adds a row to AI activity and an undo changes one.
      void qc.invalidateQueries({ queryKey: aiTrustKeys.activity() });
    },
  });
}

export interface ApproveAbilityVars extends DecideVars {
  /** The `presented_digest` the queue returned for this exact row. */
  readonly presentedDigest: string;
}

export function useApproveAbilityRequest() {
  return useAbilityMutation<ApproveAbilityVars>(({ siteId, requestId, presentedDigest }) =>
    approveAbilityRequest({
      path: { siteId, requestId },
      body: { presented_digest: presentedDigest },
    }),
  );
}

export function useDeclineAbilityRequest() {
  return useAbilityMutation<DecideVars>(({ siteId, requestId }) =>
    declineAbilityRequest({ path: { siteId, requestId }, body: {} }),
  );
}

export function useUndoAbilityRequest() {
  return useAbilityMutation<DecideVars>(({ siteId, requestId }) =>
    undoAbilityRequest({ path: { siteId, requestId }, body: {} }),
  );
}

export function useContentEditing(
  siteId: string,
): UseQueryResult<ContentEditingState, AbilityRequestError> {
  return useQuery({
    queryKey: abilityRequestKeys.editing(siteId),
    queryFn: async (): Promise<ContentEditingState> => {
      const { data, error, response } = await getSiteContentEditing({ path: { siteId } });
      const status = response?.status;
      if (error) throw toAbilityError(error, status);
      if (!data) throw new AbilityRequestError("empty_response", "Empty response", status);
      return data;
    },
  });
}

export function useEnableContentEditing(
  siteId: string,
): UseMutationResult<ContentEditingState, AbilityRequestError, void> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (): Promise<ContentEditingState> => {
      const { data, error, response } = await enableSiteContentEditing({
        path: { siteId },
        body: {},
      });
      const status = response?.status;
      if (error) throw toAbilityError(error, status);
      if (!data) throw new AbilityRequestError("empty_response", "Empty response", status);
      return data;
    },
    onSuccess: (data) => {
      qc.setQueryData(abilityRequestKeys.editing(siteId), data);
      void qc.invalidateQueries({ queryKey: abilityRequestKeys.editing(siteId) });
      // The readiness card's "AI page creation" row and the Sites list column
      // read this same switch. Turning it on changes what both show without any
      // site report arriving, so both are asked again here.
      void qc.invalidateQueries({ queryKey: aiReadinessKeys.site(siteId) });
      void qc.invalidateQueries({ queryKey: aiReadinessKeys.fleet() });
      // Turning AI editing on can set the site's AI mode to its default.
      void qc.invalidateQueries({ queryKey: aiTrustKeys.siteMode(siteId) });
    },
  });
}
