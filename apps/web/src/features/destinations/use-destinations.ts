// Per-site destination CRUD (ADR-036 P1 storage adapter). The query layer
// invalidates aggressively on every mutation so a successful save reflects on
// the list immediately without forcing a page refresh.

import {
  useQuery,
  useMutation,
  useQueryClient,
  type UseQueryResult,
  type UseMutationResult,
} from "@tanstack/react-query";
import {
  listSiteDestinations,
  createSiteDestination,
  getSiteDestination,
  updateSiteDestination,
  deleteSiteDestination,
  testSiteDestination,
  type SiteDestination,
  type SiteDestinationCreate,
  type SiteDestinationUpdate,
  type SiteDestinationTest,
  type SiteDestinationTestResult,
} from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

export const destinationsKeys = {
  all: ["destinations"] as const,
  list: (siteId: string) => [...destinationsKeys.all, "list", siteId] as const,
  one: (siteId: string, id: string) =>
    [...destinationsKeys.all, "one", siteId, id] as const,
};

export function useDestinations(
  siteId: string,
): UseQueryResult<SiteDestination[], Error> {
  return useQuery({
    queryKey: destinationsKeys.list(siteId),
    queryFn: async () => {
      const { data, error } = await listSiteDestinations({
        path: { siteId },
      });
      if (error) throw toError(error);
      return data?.items ?? [];
    },
    enabled: !!siteId,
  });
}

export function useDestination(
  siteId: string,
  destinationId: string,
): UseQueryResult<SiteDestination, Error> {
  return useQuery({
    queryKey: destinationsKeys.one(siteId, destinationId),
    queryFn: async () => {
      const { data, error } = await getSiteDestination({
        path: { siteId, destinationId },
      });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    enabled: !!siteId && !!destinationId,
  });
}

export function useCreateDestination(
  siteId: string,
): UseMutationResult<SiteDestination, Error, SiteDestinationCreate> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: SiteDestinationCreate) => {
      const { data, error } = await createSiteDestination({
        path: { siteId },
        body,
      });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: destinationsKeys.list(siteId),
      });
    },
  });
}

export function useUpdateDestination(
  siteId: string,
): UseMutationResult<
  SiteDestination,
  Error,
  { destinationId: string; body: SiteDestinationUpdate }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ destinationId, body }) => {
      const { data, error } = await updateSiteDestination({
        path: { siteId, destinationId },
        body,
      });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: (_, vars) => {
      void queryClient.invalidateQueries({
        queryKey: destinationsKeys.list(siteId),
      });
      void queryClient.invalidateQueries({
        queryKey: destinationsKeys.one(siteId, vars.destinationId),
      });
    },
  });
}

export function useDeleteDestination(
  siteId: string,
): UseMutationResult<void, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (destinationId: string) => {
      const { error } = await deleteSiteDestination({
        path: { siteId, destinationId },
      });
      if (error) throw toError(error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: destinationsKeys.list(siteId),
      });
    },
  });
}

export function useTestConnection(
  siteId: string,
): UseMutationResult<SiteDestinationTestResult, Error, SiteDestinationTest> {
  return useMutation({
    mutationFn: async (body: SiteDestinationTest) => {
      const { data, error } = await testSiteDestination({
        path: { siteId },
        body,
      });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
  });
}
