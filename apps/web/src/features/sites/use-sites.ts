import {
  queryOptions,
  useQuery,
  useMutation,
  useQueryClient,
  type UseQueryResult,
  type UseMutationResult,
} from "@tanstack/react-query";
import {
  client,
  listSites,
  getSite,
  deleteSite,
  createPairingCode,
  setSiteTags,
  refreshSiteScreenshot,
  type Site,
  type SiteList,
  type PairingCode,
  type PairingCodeCreate,
  type ApiError,
} from "@wpmgr/api";

import { toast } from "@/components/toast";

// Server-state hooks for the Sites domain. Built on the generated @wpmgr/api
// SDK (Hey API). Each SDK call returns `{ data, error, response }`; we unwrap
// `data` (throwing on transport/HTTP errors) so TanStack Query manages the
// loading/error/success states.

/** Which connection-state bucket the list should surface. */
export type SitesView = "active" | "archived";

export const sitesKeys = {
  all: ["sites"] as const,
  lists: () => [...sitesKeys.all, "list"] as const,
  list: (tag?: string, view: SitesView = "active", clientId?: string) =>
    [...sitesKeys.lists(), { tag: tag ?? null, view, clientId: clientId ?? null }] as const,
  detail: (id: string) => [...sitesKeys.all, "detail", id] as const,
};

/** A 404 surfaced as a typed error so callers can render a not-found state. */
export class NotFoundError extends Error {
  constructor(message = "Not found") {
    super(message);
    this.name = "NotFoundError";
  }
}

/**
 * Shared query options for the default sites list (active view, no tag, no
 * client filter). Exporting this object lets route loaders prefetch the exact
 * same cache entry that `useSites()` reads, so there is no duplicate request
 * and the component receives already-resolved data on mount.
 *
 * This is intentionally limited to the default-filter case: it is the only
 * call the Sites index page fires on a cold load before any filters are
 * applied.
 */
export const sitesQueryOptions = () =>
  queryOptions({
    queryKey: sitesKeys.list(undefined, "active", undefined),
    queryFn: async () => {
      const { data, error } = await listSites({});
      if (error) throw toError(error);
      return data?.items ?? [];
    },
  });

/**
 * List sites, optionally filtered by a single tag (?tag=) and/or a client
 * UUID (?clientId=). Passing an empty or undefined tag lists all sites.
 *
 * The default ("active") view hides archived sites (the CP omits them unless
 * asked). Pass `view: "archived"` to fetch the archived bucket via
 * `?state=archived` — the Phase 5 "Archived" filter chip drives this.
 */
export function useSites(
  tag?: string,
  options?: { view?: SitesView; clientId?: string },
): UseQueryResult<Site[], Error> {
  const trimmed = tag?.trim() ? tag.trim() : undefined;
  const view: SitesView = options?.view ?? "active";
  const clientId = options?.clientId || undefined;
  return useQuery({
    queryKey: sitesKeys.list(trimmed, view, clientId),
    queryFn: async () => {
      // The archived view passes `?state=archived`. For views that need extra
      // params we call the typed `listSites` with the full query object.
      if (view === "archived") {
        const query: Record<string, string> = { state: "archived" };
        if (trimmed) query.tag = trimmed;
        if (clientId) query.clientId = clientId;
        // The client's response-style generics unwrap a responses-map.
        const { data, error } = await client.get<{ 200: SiteList }>({
          url: "/api/v1/sites",
          query,
        });
        if (error) throw toError(error);
        return data?.items ?? [];
      }
      const { data, error } = await listSites({
        query: {
          ...(trimmed ? { tag: trimmed } : {}),
          ...(clientId ? { clientId } : {}),
        },
      });
      if (error) throw toError(error);
      return data?.items ?? [];
    },
  });
}

export function useSite(siteId: string): UseQueryResult<Site, Error> {
  return useQuery({
    queryKey: sitesKeys.detail(siteId),
    queryFn: async () => {
      const { data, error, response } = await getSite({
        path: { siteId },
      });
      if (response?.status === 404) throw new NotFoundError("Site not found");
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
  });
}

export function useDeleteSite(): UseMutationResult<void, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (siteId: string) => {
      const { error, response } = await deleteSite({ path: { siteId } });
      if (response?.status === 404) throw new NotFoundError("Site not found");
      if (error) throw toError(error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: sitesKeys.all });
    },
  });
}

/**
 * Generate a one-time agent pairing code. The plaintext `code` is returned ONCE
 * and is never retrievable again — callers must surface it immediately. We do
 * NOT cache it. A new pairing code can create a new (pending) site, so we
 * invalidate the sites lists on success.
 */
export function usePairingCode(): UseMutationResult<
  PairingCode,
  Error,
  PairingCodeCreate
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: PairingCodeCreate) => {
      const { data, error } = await createPairingCode({ body });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: sitesKeys.lists() });
    },
  });
}

/**
 * Replace the tag set on a site (PUT tags). Optimistically updates the cached
 * detail entry, then reconciles with the server response and invalidates lists
 * (tag filters may now match differently).
 */
export function useSetSiteTags(): UseMutationResult<
  Site,
  Error,
  { siteId: string; tags: string[] },
  { previous: Site | undefined }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ siteId, tags }) => {
      const { data, error, response } = await setSiteTags({
        path: { siteId },
        body: { tags },
      });
      if (response?.status === 404) throw new NotFoundError("Site not found");
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onMutate: async ({ siteId, tags }) => {
      await queryClient.cancelQueries({ queryKey: sitesKeys.detail(siteId) });
      const previous = queryClient.getQueryData<Site>(
        sitesKeys.detail(siteId),
      );
      if (previous) {
        queryClient.setQueryData<Site>(sitesKeys.detail(siteId), {
          ...previous,
          tags,
        });
      }
      return { previous };
    },
    onError: (_error, { siteId }, context) => {
      if (context?.previous) {
        queryClient.setQueryData(sitesKeys.detail(siteId), context.previous);
      }
    },
    onSuccess: (site) => {
      queryClient.setQueryData(sitesKeys.detail(site.id), site);
    },
    onSettled: (_data, _error, { siteId }) => {
      void queryClient.invalidateQueries({ queryKey: sitesKeys.detail(siteId) });
      void queryClient.invalidateQueries({ queryKey: sitesKeys.lists() });
    },
  });
}

/**
 * Enqueue a fresh screenshot capture for a site. On success the server returns
 * `status: "pending"` immediately; we optimistically patch the site's list and
 * detail cache to show the "capturing" thumbnail state while the job runs, then
 * invalidate both so the completed screenshot appears once the SSE event fires
 * or the next poll wins.
 *
 * GH #187: on self-host, the media-encoder that actually performs the capture
 * is frequently not running, so the job never completes. Without this fix the
 * poll below exhausted SILENTLY, leaving the card stuck on the "capturing"
 * spinner forever with no further feedback — a permanent false-pending state
 * after an honest "Screenshot queued" toast. The poll's terminal branch below
 * now ALWAYS resolves the optimistic state one way or another: either the
 * server confirms ready/failed, or we time out and force the card back to
 * "failed" (clearing the spinner) with a warning toast that names the likely
 * cause. There is no code path where this mutation's UI effects end in
 * silence.
 *
 * Expected non-2xx:
 *   409 — site not enrolled (no agent to take the shot)
 *   500 — screenshot capture failed / not configured on this instance
 *   501 — screenshot feature not configured on this instance
 *   503 — the screenshot worker (media-encoder) is not running
 */
export function useRefreshScreenshot(): UseMutationResult<void, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (siteId: string) => {
      const { error, response } = await refreshSiteScreenshot({
        path: { siteId },
      });
      if (response?.status === 409)
        throw new Error("Site is not enrolled; cannot capture screenshot.");
      if (response?.status === 503)
        throw new Error("The screenshot service isn't running.");
      // 500 (capture failed / worker misconfigured) and 501 (feature not
      // wired on this instance) are both "you can't use this here" from the
      // operator's point of view — one clear message for both.
      if (response?.status === 500 || response?.status === 501)
        throw new Error("Screenshots aren't configured on this server.");
      if (error) throw toError(error);
    },
    onMutate: (siteId: string) => {
      // Optimistic: patch the list cache entry to "pending" so the thumbnail
      // immediately shows the "capturing" state while the job queues.
      const patchSite = (site: Site): Site => ({
        ...site,
        screenshot_status: "pending" as const,
        // Clear the stale URL so the thumbnail does not show an outdated image
        // while the new one is being captured.
        screenshot_url: undefined,
        screenshot_url_2x: undefined,
      });
      // Patch every list cache key that contains this site.
      queryClient.setQueriesData<Site[]>(
        { queryKey: sitesKeys.lists() },
        (prev) => prev?.map((s) => (s.id === siteId ? patchSite(s) : s)),
      );
      // Patch the detail cache if present.
      const prev = queryClient.getQueryData<Site>(sitesKeys.detail(siteId));
      if (prev) {
        queryClient.setQueryData<Site>(sitesKeys.detail(siteId), patchSite(prev));
      }
    },
    onSuccess: (_data, siteId) => {
      void queryClient.invalidateQueries({ queryKey: sitesKeys.detail(siteId) });
      void queryClient.invalidateQueries({ queryKey: sitesKeys.lists() });
      // The POST only ENQUEUES the capture; the worker finishes a few seconds
      // later (cold-start + render). Without a follow-up refetch the card would
      // sit on "capturing" until a manual reload. Poll the list until this
      // site's screenshot flips off "pending" (ready or failed), bounded to
      // ~15 ticks x 3s = 45s so a stuck job can't poll forever.
      let attempts = 0;
      const maxAttempts = 15;
      const poll = async (): Promise<void> => {
        attempts += 1;
        await queryClient.invalidateQueries({ queryKey: sitesKeys.lists() });
        let status: string | undefined;
        for (const [, data] of queryClient.getQueriesData<Site[]>({
          queryKey: sitesKeys.lists(),
        })) {
          const s = data?.find((x) => x.id === siteId);
          if (s) {
            status = s.screenshot_status;
            break;
          }
        }
        if (status !== undefined && status !== "pending") {
          // Terminal state confirmed by the server (ready or failed) — the
          // list/detail cache already reflects it via the invalidation above.
          return;
        }
        if (attempts >= maxAttempts) {
          // Exhausted the poll window with the server still saying "pending"
          // (or never surfacing this site at all). NEVER end silently here:
          // force the optimistic state out of "capturing" so the spinner
          // clears, and tell the operator why.
          const clearStuckPending = (site: Site): Site =>
            site.screenshot_status === "pending"
              ? { ...site, screenshot_status: "failed" as const }
              : site;
          queryClient.setQueriesData<Site[]>(
            { queryKey: sitesKeys.lists() },
            (prevList) =>
              prevList?.map((s) => (s.id === siteId ? clearStuckPending(s) : s)),
          );
          const prevDetail = queryClient.getQueryData<Site>(
            sitesKeys.detail(siteId),
          );
          if (prevDetail) {
            queryClient.setQueryData<Site>(
              sitesKeys.detail(siteId),
              clearStuckPending(prevDetail),
            );
          }
          toast.warning("Screenshot didn't finish", {
            description:
              "If you self-host, make sure the media-encoder service is running.",
          });
          return;
        }
        setTimeout(() => void poll(), 3000);
      };
      setTimeout(() => void poll(), 3000);
    },
  });
}

/** Normalize the generated `Error` body (or anything) into an Error instance. */
function toError(error: unknown): Error {
  if (error instanceof Error) return error;
  if (isApiError(error)) return new Error(error.message);
  return new Error("Request failed");
}

function isApiError(value: unknown): value is ApiError {
  return (
    typeof value === "object" &&
    value !== null &&
    "message" in value &&
    typeof value.message === "string"
  );
}
