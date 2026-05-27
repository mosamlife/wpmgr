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
  type Site,
  type ApiError,
} from "@wpmgr/api";

// Server-state hooks for the Sites domain. Built on the generated @wpmgr/api
// SDK (Hey API). Each SDK call returns `{ data, error, response }`; we unwrap
// `data` (throwing on transport/HTTP errors) so TanStack Query manages the
// loading/error/success states.

export const sitesKeys = {
  all: ["sites"] as const,
  list: () => [...sitesKeys.all, "list"] as const,
  detail: (id: string) => [...sitesKeys.all, "detail", id] as const,
};

/** A 404 surfaced as a typed error so callers can render a not-found state. */
export class NotFoundError extends Error {
  constructor(message = "Not found") {
    super(message);
    this.name = "NotFoundError";
  }
}

export function useSites(): UseQueryResult<Site[], Error> {
  return useQuery({
    queryKey: sitesKeys.list(),
    queryFn: async () => {
      const { data, error } = await listSites();
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
