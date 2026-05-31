import {
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
  type Site,
  type SiteList,
  type PairingCode,
  type PairingCodeCreate,
  type ApiError,
} from "@wpmgr/api";

// Server-state hooks for the Sites domain. Built on the generated @wpmgr/api
// SDK (Hey API). Each SDK call returns `{ data, error, response }`; we unwrap
// `data` (throwing on transport/HTTP errors) so TanStack Query manages the
// loading/error/success states.

/** Which connection-state bucket the list should surface. */
export type SitesView = "active" | "archived";

export const sitesKeys = {
  all: ["sites"] as const,
  lists: () => [...sitesKeys.all, "list"] as const,
  list: (tag?: string, view: SitesView = "active") =>
    [...sitesKeys.lists(), { tag: tag ?? null, view }] as const,
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
 * List sites, optionally filtered by a single tag (?tag=). Passing an empty or
 * undefined tag lists all sites.
 *
 * The default ("active") view hides archived sites (the CP omits them unless
 * asked). Pass `view: "archived"` to fetch the archived bucket via
 * `?state=archived` — the Phase 5 "Archived" filter chip drives this.
 */
export function useSites(
  tag?: string,
  options?: { view?: SitesView },
): UseQueryResult<Site[], Error> {
  const trimmed = tag?.trim() ? tag.trim() : undefined;
  const view: SitesView = options?.view ?? "active";
  return useQuery({
    queryKey: sitesKeys.list(trimmed, view),
    queryFn: async () => {
      // The archived view passes `?state=archived`, a query param the generated
      // ListSitesData type does not declare yet (Phase 5 backend addition). For
      // that view we call the shared `client` directly so we can attach the
      // param without churning the generated client; the active view stays on
      // the typed `listSites` wrapper.
      if (view === "archived") {
        const query: Record<string, string> = { state: "archived" };
        if (trimmed) query.tag = trimmed;
        // The client's response-style generics unwrap a responses-map (the
        // generated SDK passes `{ <status>: Body }`). We mirror that so `data`
        // types as SiteList rather than being collapsed to its value union.
        const { data, error } = await client.get<{ 200: SiteList }>({
          url: "/api/v1/sites",
          query,
        });
        if (error) throw toError(error);
        return data?.items ?? [];
      }
      const { data, error } = await listSites(
        trimmed ? { query: { tag: trimmed } } : {},
      );
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
