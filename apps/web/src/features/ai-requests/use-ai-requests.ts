import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from "@tanstack/react-query";
import {
  listAssistantRequests,
  listSiteAssistantRequests,
  approveAssistantRequest,
  declineAssistantRequest,
  type AssistantRequest,
  type AssistantRequestList,
  type ApiError,
} from "@wpmgr/api";

// The AI request queue (tracka-cache-purge-design-v7 §2.6, slice W2): the
// global list behind /ai/requests, the site-nested list behind the
// Performance/Cache banner, and approve/decline.
//
// THIS GOES THROUGH @wpmgr/api, UNLIKE features/ai-connections. The
// connections list is hand-shaped Go DTOs with no OpenAPI document (see that
// feature's own use-ai-connections.ts); the AI request queue routes are in
// packages/openapi/openapi.yaml (feat(openapi): document the AI request
// queue, approve and decline), so this is the ordinary generated-client path
// every other domain hook in apps/web follows -- one facade import, no raw
// fetch, no hand-rolled zod schema for a shape the generated types already
// pin.

export const assistantRequestKeys = {
  all: ["ai-requests"] as const,
  list: () => [...assistantRequestKeys.all, "list"] as const,
  site: (siteId: string) => [...assistantRequestKeys.all, "site", siteId] as const,
};

/**
 * A named error carrying the server's own `code`, so a caller can branch on
 * it (e.g. `assistant_request_changed` after a digest mismatch) instead of a
 * generic failure. apps/api/internal/assistantrequest/service.go's `Code*`
 * constants are the source of truth for these values; this class does not
 * invent a vocabulary of its own, it just carries the server's.
 */
export class AssistantRequestError extends Error {
  readonly code: string;
  readonly status: number | undefined;

  constructor(code: string, message: string, status?: number) {
    super(message);
    this.name = "AssistantRequestError";
    this.code = code;
    this.status = status;
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

// The server already writes a complete, approve/decline-page-ready sentence
// into `message` for every refusal this route can give (service.go's
// errRequestChanged, errAssistantPaused, ... each build one). This only
// supplies a fallback for the cases that never reach that code at all -- a
// network failure, or a response the client library could not decode.
function toAssistantRequestError(error: unknown, status: number | undefined): AssistantRequestError {
  const apiError = isApiError(error) ? error : null;
  const code = apiError?.code ?? (status === 403 ? "forbidden" : status === 404 ? "not_found" : "request_failed");
  const message = apiError?.message ?? "The request could not be completed. Try again.";
  return new AssistantRequestError(code, message, status);
}

/**
 * The organisation-wide queue behind `/ai/requests` (and the tab badge).
 * `pending_count` on the response is the count that badge shows -- it is a
 * server-computed fact, never derived client-side by filtering the page in
 * hand, because the page is capped and the pending count is not.
 */
export function useAssistantRequests(): UseQueryResult<AssistantRequestList, Error> {
  return useQuery({
    queryKey: assistantRequestKeys.list(),
    queryFn: async (): Promise<AssistantRequestList> => {
      const { data, error, response } = await listAssistantRequests({});
      // Captured before either narrowing check below: once `error` is ruled
      // out, the generated response type makes `data` unconditionally
      // present, so TS proves the `!data` branch unreachable and would type
      // a `response` read inside it as `never`. `status` is a plain
      // `number | undefined` local, so it carries through both branches.
      const status = response?.status;
      if (error) throw toAssistantRequestError(error, status);
      if (!data) throw new AssistantRequestError("empty_response", "Empty response", status);
      return data;
    },
    // Live data is pulled, not pushed (house rule): there is no SSE channel
    // for this queue, so a short poll is the whole freshness story, not a
    // backstop for one.
    refetchInterval: 30_000,
  });
}

/**
 * The one-site list that feeds the Cache tab's banner. Disabled with no
 * `siteId` rather than firing a request for `undefined` and rendering
 * whatever a malformed URL comes back as.
 */
export function useSiteAssistantRequests(
  siteId: string | undefined,
): UseQueryResult<AssistantRequestList, Error> {
  return useQuery({
    queryKey: assistantRequestKeys.site(siteId ?? ""),
    queryFn: async (): Promise<AssistantRequestList> => {
      const { data, error, response } = await listSiteAssistantRequests({
        path: { siteId: siteId as string },
      });
      const status = response?.status;
      if (error) throw toAssistantRequestError(error, status);
      if (!data) throw new AssistantRequestError("empty_response", "Empty response", status);
      return data;
    },
    enabled: siteId != null && siteId !== "",
    refetchInterval: 30_000,
  });
}

export interface ApproveAssistantRequestVars {
  readonly siteId: string;
  readonly requestId: string;
  /** The `presented_digest` the queue returned for this exact row (§2.6). */
  readonly presentedDigest: string;
}

/**
 * Approves one request. Invalidates BOTH the global queue and this site's
 * list on success, because either one may be the screen an operator is
 * looking at (the badge on /ai and the banner on the Cache tab both read the
 * same underlying rows, from two different routes).
 */
export function useApproveAssistantRequest(): UseMutationResult<
  AssistantRequest,
  AssistantRequestError,
  ApproveAssistantRequestVars
> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async ({
      siteId,
      requestId,
      presentedDigest,
    }: ApproveAssistantRequestVars): Promise<AssistantRequest> => {
      const { data, error, response } = await approveAssistantRequest({
        path: { siteId, requestId },
        body: { presented_digest: presentedDigest },
      });
      const status = response?.status;
      if (error) throw toAssistantRequestError(error, status);
      if (!data) throw new AssistantRequestError("empty_response", "Empty response", status);
      return data;
    },
    onSuccess: (_data, vars) => {
      void qc.invalidateQueries({ queryKey: assistantRequestKeys.all });
      void qc.invalidateQueries({ queryKey: assistantRequestKeys.site(vars.siteId) });
    },
  });
}

export interface DeclineAssistantRequestVars {
  readonly siteId: string;
  readonly requestId: string;
}

/** Declines one request. Same invalidation as approve. */
export function useDeclineAssistantRequest(): UseMutationResult<
  AssistantRequest,
  AssistantRequestError,
  DeclineAssistantRequestVars
> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async ({ siteId, requestId }: DeclineAssistantRequestVars): Promise<AssistantRequest> => {
      const { data, error, response } = await declineAssistantRequest({
        path: { siteId, requestId },
        body: {},
      });
      const status = response?.status;
      if (error) throw toAssistantRequestError(error, status);
      if (!data) throw new AssistantRequestError("empty_response", "Empty response", status);
      return data;
    },
    onSuccess: (_data, vars) => {
      void qc.invalidateQueries({ queryKey: assistantRequestKeys.all });
      void qc.invalidateQueries({ queryKey: assistantRequestKeys.site(vars.siteId) });
    },
  });
}
