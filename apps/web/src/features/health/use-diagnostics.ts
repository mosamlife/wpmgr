import {
  useQuery,
  useMutation,
  useQueryClient,
  type UseQueryResult,
  type UseMutationResult,
} from "@tanstack/react-query";
import {
  getSiteDiagnostics,
  refreshSiteDiagnostics,
  type SiteDiagnosticsCard,
} from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

// ADR-037 Sprint 2 — TanStack Query hooks for the site Health tab.
//
// Backed by the generated @wpmgr/api SDK. The diagnostics endpoint returns 14
// cards every call (one per agent category); the UI's HealthTab renders 9 of
// them (PHP / MySQL / Filesystem / HTTP / Cron / Themes / Plugins / Users /
// Security per the headline spec) and groups the rest into a single Hosting
// ribbon up top.

export const diagnosticsKeys = {
  all: ["diagnostics"] as const,
  forSite: (siteId: string) => [...diagnosticsKeys.all, "site", siteId] as const,
};

/** Latest diagnostics by category for a site. */
export function useDiagnostics(
  siteId: string,
): UseQueryResult<SiteDiagnosticsCard[], Error> {
  return useQuery({
    queryKey: diagnosticsKeys.forSite(siteId),
    queryFn: async () => {
      const { data, error } = await getSiteDiagnostics({ path: { siteId } });
      if (error) throw toError(error);
      return data?.items ?? [];
    },
  });
}

/**
 * Trigger an on-demand diagnostics push from the agent. The CP endpoint
 * returns 503 with `diagnostics_refresh_unwired` when the CP->agent commander
 * has not yet been wired — the UI surfaces that as "Re-run is queued for
 * a future release" rather than a generic error.
 */
export function useRefreshDiagnostics(
  siteId: string,
): UseMutationResult<void, Error, void> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      const { error, response } = await refreshSiteDiagnostics({
        path: { siteId },
      });
      if (response?.status === 503) {
        throw new Error("Re-run check is not yet wired");
      }
      if (error) throw toError(error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: diagnosticsKeys.forSite(siteId),
      });
    },
  });
}

/** Return the card for the given category, or `undefined`. */
export function cardFor(
  items: SiteDiagnosticsCard[] | undefined,
  category: string,
): SiteDiagnosticsCard | undefined {
  return items?.find((c) => c.category === category);
}
