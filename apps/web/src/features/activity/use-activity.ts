import {
  useQuery,
  type UseQueryResult,
} from "@tanstack/react-query";
import {
  listSiteActivity,
  verifySiteActivity,
  type SiteActivityEvent,
  type ActivityVerifyResult,
} from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

// ADR-037 Sprint 3 — TanStack Query hooks for the WordPress activity log.
//
// useActivity drives the table (filtered, newest first). useActivityVerify
// feeds the top integrity badge: a server-side recomputation of the whole hash
// chain, returning either "verified" or the seq of the first tamper point.

export type SeverityFilter = "all" | "high" | "medium" | "low";

export const activityKeys = {
  all: ["activity"] as const,
  forSite: (
    siteId: string,
    filters: ActivityFilters,
  ) => [...activityKeys.all, "site", siteId, filters] as const,
  verify: (siteId: string) =>
    [...activityKeys.all, "verify", siteId] as const,
};

export interface ActivityFilters {
  severity: SeverityFilter;
  objectType: string;
  actorLogin: string;
}

export function useActivity(
  siteId: string,
  filters: ActivityFilters,
): UseQueryResult<SiteActivityEvent[], Error> {
  return useQuery({
    queryKey: activityKeys.forSite(siteId, filters),
    queryFn: async () => {
      const { data, error } = await listSiteActivity({
        path: { siteId },
        query: {
          ...(filters.severity !== "all"
            ? { severity: filters.severity }
            : {}),
          ...(filters.objectType ? { object_type: filters.objectType } : {}),
          ...(filters.actorLogin ? { actor_login: filters.actorLogin } : {}),
          limit: 200,
        },
      });
      if (error) throw toError(error);
      return data?.items ?? [];
    },
    refetchInterval: 30_000,
  });
}

export function useActivityVerify(
  siteId: string,
): UseQueryResult<ActivityVerifyResult, Error> {
  return useQuery({
    queryKey: activityKeys.verify(siteId),
    queryFn: async () => {
      const { data, error } = await verifySiteActivity({ path: { siteId } });
      if (error) throw toError(error);
      return data ?? { valid: true, total: 0 };
    },
    refetchInterval: 60_000,
  });
}
