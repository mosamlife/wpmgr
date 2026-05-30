import {
  useQuery,
  useMutation,
  useQueryClient,
  type UseQueryResult,
  type UseMutationResult,
} from "@tanstack/react-query";
import {
  getSiteLoginBrand,
  putSiteLoginBrand,
  type SiteLoginBrand,
  type SiteLoginBrandUpdate,
} from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

// M14 — TanStack Query hooks for the per-site login whitelabel config.
//
// GET  /api/v1/sites/:siteId/login-brand  -> SiteLoginBrand
// PUT  /api/v1/sites/:siteId/login-brand  -> SiteLoginBrand (stores + pushes to agent)
//
// Cache key family: ["loginBrand", siteId]
// Convention mirrors features/security/use-security.ts.

export const loginBrandKeys = {
  forSite: (siteId: string) => ["loginBrand", siteId] as const,
};

// ---------------------------------------------------------------------------
// useLoginBrand — GET /sites/:siteId/login-brand
// ---------------------------------------------------------------------------

export function useLoginBrand(
  siteId: string,
): UseQueryResult<SiteLoginBrand, Error> {
  return useQuery({
    queryKey: loginBrandKeys.forSite(siteId),
    queryFn: async () => {
      const { data, error } = await getSiteLoginBrand({
        path: { siteId },
      });
      if (error) throw toError(error);
      if (!data) throw new Error("No login brand config returned");
      return data;
    },
    staleTime: 30_000,
  });
}

// ---------------------------------------------------------------------------
// useUpdateLoginBrand — PUT /sites/:siteId/login-brand
// ---------------------------------------------------------------------------

export function useUpdateLoginBrand(
  siteId: string,
): UseMutationResult<SiteLoginBrand, Error, SiteLoginBrandUpdate> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: SiteLoginBrandUpdate) => {
      const { data, error } = await putSiteLoginBrand({
        path: { siteId },
        body,
      });
      if (error) throw toError(error);
      if (!data) throw new Error("No login brand config returned");
      return data;
    },
    onSuccess: (updated) => {
      // Reflect the returned config immediately — no refetch needed.
      queryClient.setQueryData(loginBrandKeys.forSite(siteId), updated);
    },
  });
}
