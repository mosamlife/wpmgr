import {
  useQuery,
  useMutation,
  useQueryClient,
  type UseQueryResult,
  type UseMutationResult,
} from "@tanstack/react-query";
import {
  listSites,
  getSite,
  deleteSite,
  createPairingCode,
  setSiteTags,
  type Site,
  type PairingCode,
  type PairingCodeCreate,
  type ApiError,
} from "@wpmgr/api";

// Server-state hooks for the Sites domain. Built on the generated @wpmgr/api
// SDK (Hey API). Each SDK call returns `{ data, error, response }`; we unwrap
// `data` (throwing on transport/HTTP errors) so TanStack Query manages the
// loading/error/success states.

export const sitesKeys = {
  all: ["sites"] as const,
  lists: () => [...sitesKeys.all, "list"] as const,
  list: (tag?: string) => [...sitesKeys.lists(), { tag: tag ?? null }] as const,
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
 */
export function useSites(tag?: string): UseQueryResult<Site[], Error> {
  const trimmed = tag?.trim() ? tag.trim() : undefined;
  return useQuery({
    queryKey: sitesKeys.list(trimmed),
    queryFn: async () => {
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
