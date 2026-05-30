import {
  useQuery,
  useMutation,
  useQueryClient,
  type UseQueryResult,
  type UseMutationResult,
} from "@tanstack/react-query";
import {
  getSiteErrorConfig,
  patchSiteErrorConfig,
  type SiteErrorConfig,
  type SiteErrorConfigUpdate,
} from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";
import { errorsKeys } from "./use-errors";

// S1.2 — TanStack Query hooks for the per-site PHP error config.
//
// The error_level is a PHP E_* bitmask. The user-facing toggles control three
// non-fatal groups; unknown bits are preserved via read-modify-write. The
// ignore_md5s list is a full replacement on each PATCH (canonical list).

export const errorConfigKeys = {
  forSite: (siteId: string) => ["errors", "config", siteId] as const,
};

// PHP E_* bitmask constants for the three user-controlled non-fatal groups.
export const E_WARNING = 2;
export const E_USER_WARNING = 512;
export const E_NOTICE = 8;
export const E_USER_NOTICE = 1024;
export const E_DEPRECATED = 8192;
export const E_USER_DEPRECATED = 16384;

export const WARNINGS_MASK = E_WARNING | E_USER_WARNING; // 514
export const NOTICES_MASK = E_NOTICE | E_USER_NOTICE; // 1032
export const DEPRECATIONS_MASK = E_DEPRECATED | E_USER_DEPRECATED; // 24576

/** Extract the three toggle states from a raw error_level bitmask. */
export function bitmaskToToggles(level: number): {
  warnings: boolean;
  notices: boolean;
  deprecations: boolean;
} {
  return {
    warnings: (level & WARNINGS_MASK) === WARNINGS_MASK,
    notices: (level & NOTICES_MASK) === NOTICES_MASK,
    deprecations: (level & DEPRECATIONS_MASK) === DEPRECATIONS_MASK,
  };
}

/** Apply toggle states back onto a base level, preserving unknown bits. */
export function togglesToBitmask(
  base: number,
  toggles: { warnings: boolean; notices: boolean; deprecations: boolean },
): number {
  let level = base;

  // Clear then conditionally set each known group.
  level = level & ~WARNINGS_MASK;
  if (toggles.warnings) level = level | WARNINGS_MASK;

  level = level & ~NOTICES_MASK;
  if (toggles.notices) level = level | NOTICES_MASK;

  level = level & ~DEPRECATIONS_MASK;
  if (toggles.deprecations) level = level | DEPRECATIONS_MASK;

  return level;
}

export function useErrorConfig(
  siteId: string,
): UseQueryResult<SiteErrorConfig, Error> {
  return useQuery({
    queryKey: errorConfigKeys.forSite(siteId),
    queryFn: async () => {
      const { data, error } = await getSiteErrorConfig({
        path: { siteId },
      });
      if (error) throw toError(error);
      if (!data) throw new Error("No config data returned");
      return data;
    },
    staleTime: 30_000,
  });
}

export function useUpdateErrorConfig(
  siteId: string,
): UseMutationResult<SiteErrorConfig, Error, SiteErrorConfigUpdate> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: SiteErrorConfigUpdate) => {
      const { data, error } = await patchSiteErrorConfig({
        path: { siteId },
        body,
      });
      if (error) throw toError(error);
      if (!data) throw new Error("No config data returned");
      return data;
    },
    onSuccess: (updatedConfig) => {
      // Update the config cache immediately with the server-confirmed value.
      queryClient.setQueryData(errorConfigKeys.forSite(siteId), updatedConfig);
      // Invalidate the errors list so ignored errors disappear from the table.
      void queryClient.invalidateQueries({ queryKey: errorsKeys.all });
    },
  });
}
