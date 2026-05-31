import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { client } from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

// Shared-with-me hook — hand-rolled endpoint, not in @wpmgr/api.
// GET /api/v1/shared-with-me -> { items: SharedSite[] }

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

export type SharedSiteRole = "viewer" | "operator" | "admin";

export interface SharedSite {
  id: string;
  site_id: string;
  user_id: string;
  role: SharedSiteRole;
  expires_at?: string | null;
  created_at: string;
  granted_by?: string | null;
  /** Included by the backend for display. */
  site_name?: string;
  site_url?: string;
  org_name?: string;
  org_id?: string;
}

// ---------------------------------------------------------------------------
// Cache key family
// ---------------------------------------------------------------------------

export const sharedWithMeKeys = {
  all: ["shared-with-me"] as const,
  list: () => ["shared-with-me", "list"] as const,
};

// ---------------------------------------------------------------------------
// useSharedWithMe — GET /api/v1/shared-with-me
// ---------------------------------------------------------------------------

export function useSharedWithMe(): UseQueryResult<SharedSite[], Error> {
  return useQuery({
    queryKey: sharedWithMeKeys.list(),
    queryFn: async () => {
      const result = await client.get({ url: "/api/v1/shared-with-me" });
      if (result.error !== undefined) throw toError(result.error);
      const data = result.data as { items: SharedSite[] };
      return data.items ?? [];
    },
    staleTime: 2 * 60_000,
  });
}
