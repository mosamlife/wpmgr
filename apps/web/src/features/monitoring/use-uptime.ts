import {
  useQuery,
  useMutation,
  useQueryClient,
  type UseQueryResult,
  type UseMutationResult,
} from "@tanstack/react-query";
import {
  getSiteUptime,
  getUptimeSummary,
  getAlertConfig,
  putAlertConfig,
  type UptimeStatus,
  type UptimeSummary,
  type AlertConfig,
  type AlertConfigUpdate,
} from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

// Server-state hooks for the M5 uptime-monitoring domain (per-site uptime
// summary + time-series, per-tenant current-status list, tenant downtime alert
// config). Built on the generated @wpmgr/api SDK; each call returns
// `{ data, error, response }` which we unwrap so TanStack Query owns
// loading/error/success. Uptime data is refetched on an interval because the
// probe worker writes new results at ~60s cadence.

/** Selectable aggregation window for the uptime view. */
export type UptimeWindow = "7d" | "30d" | "90d";

export const UPTIME_WINDOWS: ReadonlyArray<{
  value: UptimeWindow;
  label: string;
}> = [
  { value: "7d", label: "7 days" },
  { value: "30d", label: "30 days" },
  { value: "90d", label: "90 days" },
];

export const monitoringKeys = {
  all: ["monitoring"] as const,
  uptime: (siteId: string, window: UptimeWindow) =>
    [...monitoringKeys.all, "uptime", siteId, window] as const,
  summary: () => [...monitoringKeys.all, "summary"] as const,
  alertConfig: () => [...monitoringKeys.all, "alert-config"] as const,
};

/** A 404 surfaced as a typed error so callers can render a not-found state. */
export class NotFoundError extends Error {
  constructor(message = "Not found") {
    super(message);
    this.name = "NotFoundError";
  }
}

/**
 * Fetch a site's windowed uptime status (uptime %, avg latency, TLS expiry,
 * downsampled per-bucket series). Refetches every 60s so the site-detail view
 * tracks fresh probe results without a manual reload.
 */
export function useSiteUptime(
  siteId: string,
  window: UptimeWindow,
): UseQueryResult<UptimeStatus, Error> {
  return useQuery({
    queryKey: monitoringKeys.uptime(siteId, window),
    queryFn: async () => {
      const { data, error, response } = await getSiteUptime({
        path: { siteId },
        query: { window },
      });
      if (response?.status === 404) throw new NotFoundError("Site not found");
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    refetchInterval: 60_000,
  });
}

/**
 * Per-site current status across the tenant. Used to enrich the sites list
 * health column. Returns the raw UptimeSummary; callers can index by site_id
 * for O(1) lookup. Refetches on the same 60s probe cadence.
 */
export function useUptimeSummary(): UseQueryResult<UptimeSummary, Error> {
  return useQuery({
    queryKey: monitoringKeys.summary(),
    queryFn: async () => {
      const { data, error } = await getUptimeSummary();
      if (error) throw toError(error);
      if (!data) return { items: [] } satisfies UptimeSummary;
      return data;
    },
    refetchInterval: 60_000,
  });
}

/**
 * Fetch the tenant's downtime alert config (operator+). Returns `null` on 404
 * (none configured yet) so the editor renders empty defaults rather than an
 * error.
 */
export function useAlertConfig(): UseQueryResult<AlertConfig | null, Error> {
  return useQuery({
    queryKey: monitoringKeys.alertConfig(),
    queryFn: async () => {
      const { data, error, response } = await getAlertConfig();
      if (response?.status === 404) return null;
      if (error) throw toError(error);
      return data ?? null;
    },
  });
}

/**
 * Create or replace the tenant downtime alert config (operator+). Optimistically
 * updates the cached config, rolls back on error, and reconciles + invalidates
 * on settle.
 */
export function usePutAlertConfig(): UseMutationResult<
  AlertConfig,
  Error,
  AlertConfigUpdate,
  { previous: AlertConfig | null | undefined }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: AlertConfigUpdate) => {
      const { data, error, response } = await putAlertConfig({ body });
      if (response?.status === 422) {
        throw toError(error ?? { message: "Validation failed" });
      }
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onMutate: async (body) => {
      await queryClient.cancelQueries({
        queryKey: monitoringKeys.alertConfig(),
      });
      const previous = queryClient.getQueryData<AlertConfig | null>(
        monitoringKeys.alertConfig(),
      );
      if (previous) {
        queryClient.setQueryData<AlertConfig>(monitoringKeys.alertConfig(), {
          ...previous,
          email_recipients:
            body.email_recipients ?? previous.email_recipients,
          webhook_url: body.webhook_url ?? previous.webhook_url,
        });
      }
      return { previous };
    },
    onError: (_error, _body, context) => {
      if (context && context.previous !== undefined) {
        queryClient.setQueryData(
          monitoringKeys.alertConfig(),
          context.previous,
        );
      }
    },
    onSuccess: (config) => {
      queryClient.setQueryData(monitoringKeys.alertConfig(), config);
    },
    onSettled: () => {
      void queryClient.invalidateQueries({
        queryKey: monitoringKeys.alertConfig(),
      });
    },
  });
}
