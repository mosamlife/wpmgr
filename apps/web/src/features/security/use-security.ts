import {
  useQuery,
  useMutation,
  useQueryClient,
  type UseQueryResult,
  type UseMutationResult,
} from "@tanstack/react-query";
import {
  getSiteLoginProtection,
  putSiteLoginProtection,
  unblockSiteIp,
  listSiteLoginEvents,
  type SiteLoginProtectionConfig,
  type SiteLoginProtectionConfigUpdate,
  type UnblockIpResult,
  type SiteLoginEvent,
} from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

// S2 — TanStack Query hooks for login protection + login events.
//
// Cache key family: ["security", siteId, ...discriminator]
// Convention mirrors features/errors/use-error-config.ts:
//   - useSecurityConfig   — GET  /sites/:siteId/security/login-protection
//   - useUpdateSecurityConfig — PUT (body = SecurityConfig minus updated_at)
//   - useUnblockIp        — POST /sites/:siteId/security/unblock-ip
//   - useLoginEvents      — GET  /sites/:siteId/security/login-events

export const securityKeys = {
  all: ["security"] as const,
  forSite: (siteId: string) => ["security", siteId] as const,
  config: (siteId: string) => ["security", siteId, "config"] as const,
  events: (siteId: string, status?: 1 | 2 | 3) =>
    ["security", siteId, "events", status ?? "all"] as const,
};

// ---------------------------------------------------------------------------
// useSecurityConfig — GET /sites/:siteId/security/login-protection
// ---------------------------------------------------------------------------

export function useSecurityConfig(
  siteId: string,
): UseQueryResult<SiteLoginProtectionConfig, Error> {
  return useQuery({
    queryKey: securityKeys.config(siteId),
    queryFn: async () => {
      const { data, error } = await getSiteLoginProtection({
        path: { siteId },
      });
      if (error) throw toError(error);
      if (!data) throw new Error("No security config returned");
      return data;
    },
    staleTime: 30_000,
  });
}

// ---------------------------------------------------------------------------
// useUpdateSecurityConfig — PUT /sites/:siteId/security/login-protection
// ---------------------------------------------------------------------------

export function useUpdateSecurityConfig(
  siteId: string,
): UseMutationResult<
  SiteLoginProtectionConfig,
  Error,
  SiteLoginProtectionConfigUpdate
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: SiteLoginProtectionConfigUpdate) => {
      const { data, error } = await putSiteLoginProtection({
        path: { siteId },
        body,
      });
      if (error) throw toError(error);
      if (!data) throw new Error("No config data returned");
      return data;
    },
    onSuccess: (updated) => {
      // Reflect the returned config immediately (incl. any auto-added CIDR).
      queryClient.setQueryData(securityKeys.config(siteId), updated);
      // Invalidate login events — mode change may affect future events display.
      void queryClient.invalidateQueries({
        queryKey: securityKeys.forSite(siteId),
      });
    },
  });
}

// ---------------------------------------------------------------------------
// useUnblockIp — POST /sites/:siteId/security/unblock-ip
// ---------------------------------------------------------------------------

export function useUnblockIp(
  siteId: string,
): UseMutationResult<UnblockIpResult, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (ip: string) => {
      const { data, error } = await unblockSiteIp({
        path: { siteId },
        body: { ip },
      });
      if (error) throw toError(error);
      if (!data) throw new Error("No unblock result returned");
      return data;
    },
    onSuccess: () => {
      // Refresh events so blocked rows may change state.
      void queryClient.invalidateQueries({
        queryKey: securityKeys.forSite(siteId),
      });
    },
  });
}

// ---------------------------------------------------------------------------
// useLoginEvents — GET /sites/:siteId/security/login-events
// ---------------------------------------------------------------------------

export interface UseLoginEventsOptions {
  status?: 1 | 2 | 3;
  limit?: number;
}

export function useLoginEvents(
  siteId: string,
  options: UseLoginEventsOptions = {},
): UseQueryResult<SiteLoginEvent[], Error> {
  return useQuery({
    queryKey: securityKeys.events(siteId, options.status),
    queryFn: async () => {
      const { data, error } = await listSiteLoginEvents({
        path: { siteId },
        query: {
          ...(options.status !== undefined ? { status: options.status } : {}),
          limit: options.limit ?? 100,
        },
      });
      if (error) throw toError(error);
      return data?.items ?? [];
    },
    refetchInterval: 30_000,
  });
}
