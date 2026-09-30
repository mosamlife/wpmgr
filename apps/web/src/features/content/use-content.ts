import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from "@tanstack/react-query";
import {
  getAdminContentFleetReport,
  getSiteContentInventory,
  refreshSiteContentInventory,
  type ContentFleetReport,
  type ContentInventoryPage,
} from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

export const contentKeys = {
  all: ["content"] as const,
  inventory: (siteId: string) => [...contentKeys.all, "inventory", siteId] as const,
  inventoryPage: (siteId: string, after: number | null, editor: string) =>
    [...contentKeys.inventory(siteId), after, editor] as const,
  fleetReport: () => [...contentKeys.all, "fleet-report"] as const,
};

export const INVENTORY_PAGE_SIZE = 25;
export const POLL_MS = 5000;
export const POLL_WINDOW_MS = 2 * 60 * 1000;

export function useContentInventory(
  siteId: string,
  afterPostId: number | null,
  editor: string,
  poll?: { since: string | null; until: number } | null,
): UseQueryResult<ContentInventoryPage, Error> {
  return useQuery({
    // After a queued refresh: poll every 5s until last_checked_at changes or
    // the deadline (2 minutes) passes.
    refetchInterval: (query) => {
      if (!poll || Date.now() >= poll.until) return false;
      const cur = query.state.data?.last_checked_at ?? null;
      return cur !== poll.since ? false : POLL_MS;
    },
    queryKey: contentKeys.inventoryPage(siteId, afterPostId, editor),
    queryFn: async () => {
      const { data, error } = await getSiteContentInventory({
        path: { siteId },
        query: {
          limit: INVENTORY_PAGE_SIZE,
          ...(afterPostId != null ? { after_post_id: afterPostId } : {}),
          ...(editor ? { editor } : {}),
        },
      });
      if (error || !data) throw toError(error);
      return data;
    },
  });
}

export class RefreshRateLimitedError extends Error {
  readonly retryAfterSeconds: number;
  constructor(retryAfterSeconds: number) {
    super(`Checked recently. Try again in ${retryAfterSeconds} seconds.`);
    this.name = "RefreshRateLimitedError";
    this.retryAfterSeconds = retryAfterSeconds;
  }
}

const DEFAULT_RETRY_SECONDS = 30;

export function useRefreshContentInventory(
  siteId: string,
): UseMutationResult<void, Error, void> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      const { error, response } = await refreshSiteContentInventory({
        path: { siteId },
      });
      if (response?.status === 429) {
        const raw = Number(response.headers.get("Retry-After"));
        const secs =
          Number.isInteger(raw) && raw >= 1 && raw <= 3600 ? raw : DEFAULT_RETRY_SECONDS;
        throw new RefreshRateLimitedError(secs);
      }
      if (error) throw toError(error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: contentKeys.inventory(siteId) });
    },
  });
}

export function useContentFleetReport(): UseQueryResult<ContentFleetReport, Error> {
  return useQuery({
    queryKey: contentKeys.fleetReport(),
    queryFn: async () => {
      const { data, error } = await getAdminContentFleetReport();
      if (error || !data) throw toError(error);
      return data;
    },
  });
}
