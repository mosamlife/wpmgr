import {
  useQuery,
  useMutation,
  useQueryClient,
  type UseQueryResult,
  type UseMutationResult,
} from "@tanstack/react-query";
import {
  listSitePhpErrors,
  silenceSitePhpError,
  type PhpError,
} from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

// ADR-037 Sprint 2 — TanStack Query hooks for the PHP error monitor.
//
// The list endpoint supports `silenced` and `since` filters; the table shows
// unsilenced by default with a toggle to reveal silenced. The mutation flips
// the silenced flag on a (site, md5) row; the agent keeps counting locally
// regardless.

export const errorsKeys = {
  all: ["php-errors"] as const,
  forSite: (siteId: string, silenced: "true" | "false" | "all") =>
    [...errorsKeys.all, "site", siteId, silenced] as const,
};

export interface UsePHPErrorsOptions {
  showSilenced: boolean;
  limit?: number;
}

export function usePHPErrors(
  siteId: string,
  options: UsePHPErrorsOptions,
): UseQueryResult<PhpError[], Error> {
  const silenced = options.showSilenced ? "all" : "false";
  return useQuery({
    queryKey: errorsKeys.forSite(siteId, silenced),
    queryFn: async () => {
      const { data, error } = await listSitePhpErrors({
        path: { siteId },
        query: {
          ...(options.showSilenced ? {} : { silenced: "false" }),
          limit: options.limit ?? 100,
        },
      });
      if (error) throw toError(error);
      return data?.items ?? [];
    },
    refetchInterval: 15_000,
  });
}

export function useSilenceError(
  siteId: string,
): UseMutationResult<
  void,
  Error,
  { md5: string; silenced: boolean }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ md5, silenced }) => {
      const { error } = await silenceSitePhpError({
        path: { siteId, md5 },
        body: { silenced },
      });
      if (error) throw toError(error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: errorsKeys.all,
      });
    },
  });
}
