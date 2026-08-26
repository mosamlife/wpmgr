import {
  useQuery,
  useMutation,
  useInfiniteQuery,
  useQueryClient,
  type UseQueryResult,
  type UseMutationResult,
  type UseInfiniteQueryResult,
  type InfiniteData,
} from "@tanstack/react-query";
import {
  getSiteEmailConfig,
  putSiteEmailConfig,
  sendTestEmail,
  syncSiteEmailConfig,
  listEmailProviders,
  getOrgEmailConfig,
  putOrgEmailConfig,
  listSiteEmailLog,
  getSiteEmailLogEntry,
  getSiteEmailStats,
  listFleetEmailLog,
  getFleetEmailStats,
  getFleetEmailDeliverability,
  listSiteEmailSuppression,
  addSiteEmailSuppression,
  deleteSiteEmailSuppression,
  listFleetEmailSuppression,
  addFleetEmailSuppression,
  deleteFleetEmailSuppression,
  resendEmailLog,
  bulkResendEmailLog,
  bulkDeleteEmailLog,
  putSiteEmailWebhookConfig,
  putOrgEmailWebhookConfig,
  listEmailConnections,
  putEmailConnection,
  deleteEmailConnection,
  getEmailNotifySettings,
  putEmailNotifySettings,
  type SiteEmailConfig,
  type PutEmailConfigRequest,
  type EmailTestRequest,
  type EmailTestResult,
  type EmailProviderCatalog,
  type SiteEmailLogEntry,
  type EmailLogList,
  type EmailLogDetail,
  type EmailStats,
  type DeliverabilityReport,
  type EmailSuppressionEntry,
  type EmailSuppressionPage,
  type AddSuppressionRequest,
  type ResendEmailResult,
  type BulkResendResponse,
  type BulkDeleteLogsResponse,
  type PutEmailWebhookConfigRequest,
  type EmailWebhookConfigResponse,
  type EmailConnection,
  type EmailNotifySettings,
  type PutEmailConnectionRequest,
  type PutEmailNotifySettingsRequest,
} from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";
import { toast } from "@/components/toast";

// ---------------------------------------------------------------------------
// Query-key factory
// ---------------------------------------------------------------------------

export const emailKeys = {
  all: ["email"] as const,
  providers: () => [...emailKeys.all, "providers"] as const,
  orgConfig: () => [...emailKeys.all, "org-config"] as const,
  siteConfig: (siteId: string) =>
    [...emailKeys.all, "config", siteId] as const,
  log: (siteId: string, filters: EmailLogFilters) =>
    [...emailKeys.all, "log", siteId, filters] as const,
  logDetail: (siteId: string, logId: string) =>
    [...emailKeys.all, "log", siteId, "detail", logId] as const,
  stats: (siteId: string, range: EmailStatsRange) =>
    [...emailKeys.all, "stats", siteId, range] as const,
  fleetLog: (filters: FleetEmailLogFilters) =>
    [...emailKeys.all, "fleet-log", filters] as const,
  fleetStats: (range: EmailStatsRange) =>
    [...emailKeys.all, "fleet-stats", range] as const,
  fleetDeliverability: (windowDays: number) =>
    [...emailKeys.all, "fleet-deliverability", windowDays] as const,
  // Suppression lists (Phase 4a)
  suppression: (siteId: string, reason?: string) =>
    [...emailKeys.all, "suppression", siteId, reason ?? ""] as const,
  fleetSuppression: (reason?: string) =>
    [...emailKeys.all, "fleet-suppression", reason ?? ""] as const,
  // Connections (m62)
  connections: (siteId: string) =>
    [...emailKeys.all, "connections", siteId] as const,
  // Notify settings (m62)
  notifySettings: () => [...emailKeys.all, "notify-settings"] as const,
} as const;

// ---------------------------------------------------------------------------
// Filter types
// ---------------------------------------------------------------------------

export interface EmailLogFilters {
  status?: "sent" | "failed" | "";
  from?: string;
  to?: string;
  q?: string;
  limit?: number;
}

export interface FleetEmailLogFilters {
  status?: "sent" | "failed" | "";
  from?: string;
  to?: string;
  q?: string;
  limit?: number;
}

export interface EmailStatsRange {
  from?: string;
  to?: string;
}

// ---------------------------------------------------------------------------
// Provider catalog
// ---------------------------------------------------------------------------

/** GET /api/v1/email/providers — static catalog, long stale time. */
export function useProviders(): UseQueryResult<EmailProviderCatalog, Error> {
  return useQuery({
    queryKey: emailKeys.providers(),
    queryFn: async () => {
      const { data, error } = await listEmailProviders();
      if (error) throw toError(error);
      return data;
    },
    staleTime: 5 * 60_000, // provider catalog changes very rarely
  });
}

// ---------------------------------------------------------------------------
// Org-wide email config
// ---------------------------------------------------------------------------

/** GET /api/v1/email/org-config */
export function useOrgEmailConfig(): UseQueryResult<SiteEmailConfig, Error> {
  return useQuery({
    queryKey: emailKeys.orgConfig(),
    queryFn: async () => {
      const { data, error } = await getOrgEmailConfig();
      if (error) throw toError(error);
      return data;
    },
    staleTime: 60_000,
  });
}

/** PUT /api/v1/email/org-config */
export function usePutOrgEmailConfig(): UseMutationResult<
  SiteEmailConfig,
  Error,
  PutEmailConfigRequest
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body) => {
      const { data, error } = await putOrgEmailConfig({ body });
      if (error) throw toError(error);
      return data;
    },
    onSuccess: (updated) => {
      queryClient.setQueryData(emailKeys.orgConfig(), updated);
      toast.success("Saved — pushing to inheriting sites in the background");
    },
    onError: (err) => {
      toast.error("Could not save email config", { description: err.message });
    },
  });
}

// ---------------------------------------------------------------------------
// Per-site email config
// ---------------------------------------------------------------------------

/**
 * GET /api/v1/sites/:siteId/email/config
 *
 * The secret is NEVER returned — only `secret_set` (a boolean). The UI shows a
 * "configured" indicator when secret_set is true and a "Replace" affordance to
 * allow entering a new credential (which is write-only on PUT).
 */
export function useEmailConfig(
  siteId: string,
): UseQueryResult<SiteEmailConfig | null, Error> {
  return useQuery({
    queryKey: emailKeys.siteConfig(siteId),
    queryFn: async () => {
      const { data, error, response } = await getSiteEmailConfig({
        path: { siteId },
      });
      if (error) {
        // A site with no per-site config row AND no org-wide default returns
        // 404 (email_config_not_found). That is the "not configured yet"
        // state, not a failure — surface it as a null config so the panels
        // render their setup forms with empty defaults instead of an error.
        if (response?.status === 404) return null;
        throw toError(error);
      }
      return data;
    },
    staleTime: 60_000,
  });
}

/**
 * PUT /api/v1/sites/:siteId/email/config
 *
 * Nil-sentinel: omitting `secret` preserves the stored credential. To clear,
 * send an empty string. The UI NEVER reads back the secret value — only
 * secret_set from the GET response.
 */
export function usePutEmailConfig(
  siteId: string,
): UseMutationResult<SiteEmailConfig, Error, PutEmailConfigRequest> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body) => {
      const { data, error } = await putSiteEmailConfig({
        path: { siteId },
        body,
      });
      if (error) throw toError(error);
      return data;
    },
    onSuccess: (updated) => {
      queryClient.setQueryData(emailKeys.siteConfig(siteId), updated);
      toast.success("Email settings saved");
    },
    onError: (err) => {
      toast.error("Could not save email settings", {
        description: err.message,
      });
    },
  });
}

// ---------------------------------------------------------------------------
// Test email
// ---------------------------------------------------------------------------

/** POST /api/v1/sites/:siteId/email/test */
export function useTestEmail(
  siteId: string,
): UseMutationResult<EmailTestResult, Error, EmailTestRequest> {
  return useMutation({
    mutationFn: async (body) => {
      const { data, error } = await sendTestEmail({
        path: { siteId },
        body,
      });
      if (error) throw toError(error);
      return data;
    },
  });
}

// ---------------------------------------------------------------------------
// Sync email config to agent
// ---------------------------------------------------------------------------

/** POST /api/v1/sites/:siteId/email/sync */
export function useSyncEmailConfig(
  siteId: string,
): UseMutationResult<EmailTestResult, Error, void> {
  return useMutation({
    mutationFn: async () => {
      const { data, error } = await syncSiteEmailConfig({
        path: { siteId },
      });
      if (error) throw toError(error);
      return data;
    },
    onSuccess: (result) => {
      if (result?.ok) {
        toast.success("Email config synced to site");
      } else {
        toast.error("Could not sync to site", {
          description: result?.detail ?? "The agent reported a failure",
        });
      }
    },
    onError: (err) => {
      toast.error("Could not sync to site", { description: err.message });
    },
  });
}

// ---------------------------------------------------------------------------
// Email log (per-site, infinite/keyset pagination)
// ---------------------------------------------------------------------------

export interface UseEmailLogResult {
  entries: SiteEmailLogEntry[];
  fetchNextPage: UseInfiniteQueryResult<
    InfiniteData<EmailLogList>,
    Error
  >["fetchNextPage"];
  hasNextPage: boolean;
  isFetchingNextPage: boolean;
  isPending: boolean;
  isError: boolean;
  error: Error | null;
  refetch: UseInfiniteQueryResult<
    InfiniteData<EmailLogList>,
    Error
  >["refetch"];
}

/**
 * GET /api/v1/sites/:siteId/email/log (keyset infinite pagination via next_cursor).
 * Bodies are never returned in list responses — only in the detail response.
 */
export function useEmailLog(
  siteId: string,
  filters: EmailLogFilters,
): UseEmailLogResult {
  const result = useInfiniteQuery<
    EmailLogList,
    Error,
    InfiniteData<EmailLogList>,
    ReturnType<typeof emailKeys.log>,
    string | undefined
  >({
    queryKey: emailKeys.log(siteId, filters),
    initialPageParam: undefined,
    getNextPageParam: (lastPage) => lastPage.next_cursor || undefined,
    queryFn: async ({ pageParam }) => {
      const { data, error } = await listSiteEmailLog({
        path: { siteId },
        query: {
          ...(filters.status ? { status: filters.status } : {}),
          ...(filters.from ? { from: filters.from } : {}),
          ...(filters.to ? { to: filters.to } : {}),
          ...(filters.q ? { q: filters.q } : {}),
          limit: filters.limit ?? 50,
          ...(pageParam !== undefined ? { cursor: pageParam } : {}),
        },
      });
      if (error) throw toError(error);
      return data ?? { entries: [], next_cursor: "" };
    },
    staleTime: 30_000,
  });

  const entries = result.data?.pages.flatMap((p) => p.entries) ?? [];

  return {
    entries,
    fetchNextPage: result.fetchNextPage,
    hasNextPage: result.hasNextPage,
    isFetchingNextPage: result.isFetchingNextPage,
    isPending: result.isPending,
    isError: result.isError,
    error: result.error,
    refetch: result.refetch,
  };
}

// ---------------------------------------------------------------------------
// Email log detail (single entry with prev/next navigation)
// ---------------------------------------------------------------------------

/** GET /api/v1/sites/:siteId/email/log/:logId */
export function useEmailLogDetail(
  siteId: string,
  logId: string | null,
): UseQueryResult<EmailLogDetail, Error> {
  return useQuery({
    queryKey: emailKeys.logDetail(siteId, logId ?? ""),
    queryFn: async () => {
      if (!logId) throw new Error("No log entry selected");
      const { data, error } = await getSiteEmailLogEntry({
        path: { siteId, logId },
      });
      if (error) throw toError(error);
      return data;
    },
    enabled: logId !== null,
    staleTime: 60_000,
  });
}

// ---------------------------------------------------------------------------
// Email stats (per-site)
// ---------------------------------------------------------------------------

/** GET /api/v1/sites/:siteId/email/stats?from=&to= */
export function useEmailStats(
  siteId: string,
  range: EmailStatsRange = {},
): UseQueryResult<EmailStats, Error> {
  return useQuery({
    queryKey: emailKeys.stats(siteId, range),
    queryFn: async () => {
      const { data, error } = await getSiteEmailStats({
        path: { siteId },
        query: {
          ...(range.from ? { from: range.from } : {}),
          ...(range.to ? { to: range.to } : {}),
        },
      });
      if (error) throw toError(error);
      return data;
    },
    staleTime: 60_000,
  });
}

// ---------------------------------------------------------------------------
// Fleet email log (org-scope, cross-site)
// ---------------------------------------------------------------------------

export interface UseFleetEmailLogResult {
  entries: SiteEmailLogEntry[];
  fetchNextPage: UseInfiniteQueryResult<
    InfiniteData<EmailLogList>,
    Error
  >["fetchNextPage"];
  hasNextPage: boolean;
  isFetchingNextPage: boolean;
  isPending: boolean;
  isError: boolean;
  error: Error | null;
  refetch: UseInfiniteQueryResult<
    InfiniteData<EmailLogList>,
    Error
  >["refetch"];
}

/** GET /api/v1/email/log (org-scope, keyset paginated) */
export function useFleetEmailLog(
  filters: FleetEmailLogFilters,
): UseFleetEmailLogResult {
  const result = useInfiniteQuery<
    EmailLogList,
    Error,
    InfiniteData<EmailLogList>,
    ReturnType<typeof emailKeys.fleetLog>,
    string | undefined
  >({
    queryKey: emailKeys.fleetLog(filters),
    initialPageParam: undefined,
    getNextPageParam: (lastPage) => lastPage.next_cursor || undefined,
    queryFn: async ({ pageParam }) => {
      const { data, error } = await listFleetEmailLog({
        query: {
          ...(filters.status ? { status: filters.status } : {}),
          ...(filters.from ? { from: filters.from } : {}),
          ...(filters.to ? { to: filters.to } : {}),
          ...(filters.q ? { q: filters.q } : {}),
          limit: filters.limit ?? 50,
          ...(pageParam !== undefined ? { cursor: pageParam } : {}),
        },
      });
      if (error) throw toError(error);
      return data ?? { entries: [], next_cursor: "" };
    },
    staleTime: 30_000,
  });

  const entries = result.data?.pages.flatMap((p) => p.entries) ?? [];

  return {
    entries,
    fetchNextPage: result.fetchNextPage,
    hasNextPage: result.hasNextPage,
    isFetchingNextPage: result.isFetchingNextPage,
    isPending: result.isPending,
    isError: result.isError,
    error: result.error,
    refetch: result.refetch,
  };
}

// ---------------------------------------------------------------------------
// Fleet email stats
// ---------------------------------------------------------------------------

/** GET /api/v1/email/stats (org-scope; site_count populated) */
export function useFleetEmailStats(
  range: EmailStatsRange = {},
): UseQueryResult<EmailStats, Error> {
  return useQuery({
    queryKey: emailKeys.fleetStats(range),
    queryFn: async () => {
      const { data, error } = await getFleetEmailStats({
        query: {
          ...(range.from ? { from: range.from } : {}),
          ...(range.to ? { to: range.to } : {}),
        },
      });
      if (error) throw toError(error);
      return data;
    },
    staleTime: 60_000,
  });
}

// ---------------------------------------------------------------------------
// Fleet email deliverability (per-site reputation table)
// ---------------------------------------------------------------------------

/** GET /api/v1/email/deliverability?window=<days> (org-scope) */
export function useFleetEmailDeliverability(
  windowDays = 30,
): UseQueryResult<DeliverabilityReport, Error> {
  return useQuery({
    queryKey: emailKeys.fleetDeliverability(windowDays),
    queryFn: async () => {
      const { data, error } = await getFleetEmailDeliverability({
        query: { window: windowDays },
      });
      if (error) throw toError(error);
      return data ?? { window_days: windowDays, items: [] };
    },
    staleTime: 60_000,
  });
}

// ---------------------------------------------------------------------------
// Suppression list (per-site, Phase 4a)
// ---------------------------------------------------------------------------

export interface SuppressionFilters {
  reason?: string;
  limit?: number;
}

export interface UseSuppressionResult {
  entries: EmailSuppressionEntry[];
  hasMore: boolean;
  nextCursor: string | undefined;
  isPending: boolean;
  isError: boolean;
  error: Error | null;
  refetch: UseInfiniteQueryResult<
    InfiniteData<EmailSuppressionPage>,
    Error
  >["refetch"];
  fetchNextPage: UseInfiniteQueryResult<
    InfiniteData<EmailSuppressionPage>,
    Error
  >["fetchNextPage"];
  isFetchingNextPage: boolean;
}

/** GET /api/v1/sites/:siteId/email/suppression (keyset paginated) */
export function useSiteEmailSuppression(
  siteId: string,
  filters: SuppressionFilters = {},
): UseSuppressionResult {
  const result = useInfiniteQuery<
    EmailSuppressionPage,
    Error,
    InfiniteData<EmailSuppressionPage>,
    ReturnType<typeof emailKeys.suppression>,
    string | undefined
  >({
    queryKey: emailKeys.suppression(siteId, filters.reason),
    initialPageParam: undefined,
    getNextPageParam: (lastPage) =>
      lastPage.has_more ? (lastPage.next_cursor ?? undefined) : undefined,
    queryFn: async ({ pageParam }) => {
      const { data, error } = await listSiteEmailSuppression({
        path: { siteId },
        query: {
          ...(filters.reason ? { reason: filters.reason } : {}),
          limit: filters.limit ?? 50,
          ...(pageParam !== undefined ? { cursor: pageParam } : {}),
        },
      });
      if (error) throw toError(error);
      return data ?? { entries: [], has_more: false };
    },
    staleTime: 30_000,
  });

  return {
    entries: result.data?.pages.flatMap((p) => p.entries) ?? [],
    hasMore: result.hasNextPage,
    nextCursor: result.data?.pages.at(-1)?.next_cursor ?? undefined,
    isPending: result.isPending,
    isError: result.isError,
    error: result.error,
    refetch: result.refetch,
    fetchNextPage: result.fetchNextPage,
    isFetchingNextPage: result.isFetchingNextPage,
  };
}

/** POST /api/v1/sites/:siteId/email/suppression */
export function useAddSiteEmailSuppression(
  siteId: string,
): UseMutationResult<EmailSuppressionEntry, Error, AddSuppressionRequest> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body) => {
      const { data, error } = await addSiteEmailSuppression({
        path: { siteId },
        body,
      });
      if (error) throw toError(error);
      return data;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: emailKeys.suppression(siteId),
      });
      void queryClient.invalidateQueries({
        queryKey: emailKeys.fleetSuppression(),
      });
      toast.success("Address suppressed");
    },
    onError: (err) => {
      toast.error("Could not add suppression", { description: err.message });
    },
  });
}

/** DELETE /api/v1/sites/:siteId/email/suppression/:suppressionId */
export function useDeleteSiteEmailSuppression(
  siteId: string,
): UseMutationResult<void, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (suppressionId: string) => {
      const { error } = await deleteSiteEmailSuppression({
        path: { siteId, suppressionId },
      });
      if (error) throw toError(error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: emailKeys.suppression(siteId),
      });
      void queryClient.invalidateQueries({
        queryKey: emailKeys.fleetSuppression(),
      });
      toast.success("Suppression removed");
    },
    onError: (err) => {
      toast.error("Could not remove suppression", { description: err.message });
    },
  });
}

// ---------------------------------------------------------------------------
// Suppression list (fleet scope, Phase 4a)
// ---------------------------------------------------------------------------

/** GET /api/v1/email/suppression (org-scope keyset paginated) */
export function useFleetEmailSuppression(
  filters: SuppressionFilters = {},
): UseSuppressionResult {
  const result = useInfiniteQuery<
    EmailSuppressionPage,
    Error,
    InfiniteData<EmailSuppressionPage>,
    ReturnType<typeof emailKeys.fleetSuppression>,
    string | undefined
  >({
    queryKey: emailKeys.fleetSuppression(filters.reason),
    initialPageParam: undefined,
    getNextPageParam: (lastPage) =>
      lastPage.has_more ? (lastPage.next_cursor ?? undefined) : undefined,
    queryFn: async ({ pageParam }) => {
      const { data, error } = await listFleetEmailSuppression({
        query: {
          ...(filters.reason ? { reason: filters.reason } : {}),
          limit: filters.limit ?? 50,
          ...(pageParam !== undefined ? { cursor: pageParam } : {}),
        },
      });
      if (error) throw toError(error);
      return data ?? { entries: [], has_more: false };
    },
    staleTime: 30_000,
  });

  return {
    entries: result.data?.pages.flatMap((p) => p.entries) ?? [],
    hasMore: result.hasNextPage,
    nextCursor: result.data?.pages.at(-1)?.next_cursor ?? undefined,
    isPending: result.isPending,
    isError: result.isError,
    error: result.error,
    refetch: result.refetch,
    fetchNextPage: result.fetchNextPage,
    isFetchingNextPage: result.isFetchingNextPage,
  };
}

/** POST /api/v1/email/suppression (fleet-wide, site_id=null) */
export function useAddFleetEmailSuppression(): UseMutationResult<
  EmailSuppressionEntry,
  Error,
  AddSuppressionRequest
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body) => {
      const { data, error } = await addFleetEmailSuppression({ body });
      if (error) throw toError(error);
      return data;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: emailKeys.fleetSuppression(),
      });
      toast.success("Address suppressed fleet-wide");
    },
    onError: (err) => {
      toast.error("Could not add suppression", { description: err.message });
    },
  });
}

/** DELETE /api/v1/email/suppression/:suppressionId (fleet scope) */
export function useDeleteFleetEmailSuppression(): UseMutationResult<
  void,
  Error,
  string
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (suppressionId: string) => {
      const { error } = await deleteFleetEmailSuppression({
        path: { suppressionId },
      });
      if (error) throw toError(error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: emailKeys.fleetSuppression(),
      });
      toast.success("Suppression removed");
    },
    onError: (err) => {
      toast.error("Could not remove suppression", { description: err.message });
    },
  });
}

// ---------------------------------------------------------------------------
// Log actions — resend + bulk operations (Phase 4a)
// ---------------------------------------------------------------------------

/** Error class for 409 body-not-stored resend rejection */
export class BodyNotStoredError extends Error {
  constructor() {
    super("Email body was not stored — resend unavailable for this message.");
    this.name = "BodyNotStoredError";
  }
}

/** POST /api/v1/sites/:siteId/email/log/:logId/resend */
export function useResendEmail(
  siteId: string,
): UseMutationResult<ResendEmailResult, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (logId: string) => {
      const { data, error, response } = await resendEmailLog({
        path: { siteId, logId },
      });
      if (response?.status === 409) {
        throw new BodyNotStoredError();
      }
      if (error) throw toError(error);
      return data;
    },
    onSuccess: (result) => {
      if (result.ok) {
        // GH #528: `verified` is only meaningful when ok is true. false means
        // wpmgr sent the email but could not confirm the site's copy was
        // still the exact message the operator picked (most commonly: the
        // original send had failed, so there was no delivery id on record to
        // check against). That is not a failure — the send happened — so it
        // gets a calm heads-up, never the error/destructive treatment. The
        // raw `detail` token is never shown here; this is fixed, reviewed
        // copy so the sentence stays legible regardless of what the control
        // plane's `detail` string says.
        if (result.verified === false) {
          toast.warning("Email resent — unconfirmed", {
            description:
              "wpmgr could not confirm the site sent the exact message you selected — usually because the original send had failed, so there was no delivery id to check against. If this site's database was restored recently, verify the delivery before relying on it.",
          });
        } else {
          toast.success("Email queued for resend");
        }
      } else {
        // A null detail means the agent gave no reason. Pass undefined so the
        // toast renders the title alone rather than an empty description line.
        toast.error("Resend failed", { description: result.detail ?? undefined });
      }
      void queryClient.invalidateQueries({
        queryKey: emailKeys.all,
      });
    },
    onError: (err) => {
      if (err instanceof BodyNotStoredError) {
        toast.error("Resend unavailable", {
          description: "Email body was not stored for this message.",
        });
      } else {
        toast.error("Could not resend email", { description: err.message });
      }
    },
  });
}

/** POST /api/v1/sites/:siteId/email/log/bulk-resend */
export function useBulkResendEmail(
  siteId: string,
): UseMutationResult<BulkResendResponse, Error, string[]> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (logIds: string[]) => {
      const { data, error } = await bulkResendEmailLog({
        path: { siteId },
        body: { log_ids: logIds },
      });
      if (error) throw toError(error);
      return data;
    },
    onSuccess: (result) => {
      const succeeded = result.results.filter((r) => r.ok).length;
      const failed = result.results.filter((r) => !r.ok).length;
      // GH #528: per-item `verified` is only meaningful on the items that
      // succeeded. A batch routinely mixes confirmed and unconfirmed sends,
      // so this is never folded into the success/fail count — it is reported
      // as its own number, and the caller narrows table selection to exactly
      // these log_ids so the operator has a concrete way to find them (see
      // email-log-table.tsx's bulkResend.mutate onSuccess).
      const unverified = result.results.filter(
        (r) => r.ok && r.verified === false,
      ).length;
      if (failed === 0 && unverified === 0) {
        toast.success(`${succeeded} email${succeeded !== 1 ? "s" : ""} queued for resend`);
      } else if (failed === 0) {
        toast.warning(
          `${succeeded} email${succeeded !== 1 ? "s" : ""} queued for resend, ${unverified} unconfirmed`,
          {
            description:
              unverified === 1
                ? "wpmgr could not confirm one of them went out as the exact message selected. It stays selected below so you can review it."
                : `wpmgr could not confirm ${unverified} of them went out as the exact message selected. They stay selected below so you can review them.`,
          },
        );
      } else {
        const unverifiedNote =
          unverified > 0
            ? ` (${unverified} of the successes could not be confirmed as the exact message selected)`
            : "";
        toast.error(`${failed} resend${failed !== 1 ? "s" : ""} failed`, {
          description: `${succeeded} succeeded, ${failed} could not be resent${unverifiedNote}`,
        });
      }
      void queryClient.invalidateQueries({ queryKey: emailKeys.all });
    },
    onError: (err) => {
      toast.error("Bulk resend failed", { description: err.message });
    },
  });
}

/** POST /api/v1/sites/:siteId/email/log/bulk-delete */
export function useBulkDeleteEmail(
  siteId: string,
): UseMutationResult<BulkDeleteLogsResponse, Error, string[]> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (logIds: string[]) => {
      const { data, error } = await bulkDeleteEmailLog({
        path: { siteId },
        body: { log_ids: logIds },
      });
      if (error) throw toError(error);
      return data;
    },
    onSuccess: (result) => {
      toast.success(
        `${result.deleted} log entr${result.deleted !== 1 ? "ies" : "y"} deleted`,
      );
      void queryClient.invalidateQueries({ queryKey: emailKeys.all });
    },
    onError: (err) => {
      toast.error("Bulk delete failed", { description: err.message });
    },
  });
}

// ---------------------------------------------------------------------------
// Webhook config mutations (Phase 4b)
// ---------------------------------------------------------------------------

/**
 * PUT /api/v1/sites/:siteId/email/webhook-config
 *
 * Rotate the webhook route token or update the signing key / SES TopicArn
 * allowlist. When rotate_token is true the response contains
 * webhook_route_token (plain token, returned exactly once — surface it to
 * the operator immediately).
 */
export function usePutSiteEmailWebhookConfig(
  siteId: string,
): UseMutationResult<
  EmailWebhookConfigResponse,
  Error,
  PutEmailWebhookConfigRequest
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body) => {
      const { data, error } = await putSiteEmailWebhookConfig({
        path: { siteId },
        body,
      });
      if (error) throw toError(error);
      return data;
    },
    onSuccess: () => {
      // Invalidate the full config query so webhook_url / webhook_signing_key_set
      // are refreshed from the server after the mutation.
      void queryClient.invalidateQueries({
        queryKey: emailKeys.siteConfig(siteId),
      });
      toast.success("Webhook settings saved");
    },
    onError: (err) => {
      toast.error("Could not save webhook settings", {
        description: err.message,
      });
    },
  });
}

/**
 * PUT /api/v1/email/org-config/webhook-config
 *
 * Org-level equivalent of usePutSiteEmailWebhookConfig.
 */
export function usePutOrgEmailWebhookConfig(): UseMutationResult<
  EmailWebhookConfigResponse,
  Error,
  PutEmailWebhookConfigRequest
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body) => {
      const { data, error } = await putOrgEmailWebhookConfig({ body });
      if (error) throw toError(error);
      return data;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: emailKeys.orgConfig(),
      });
      toast.success("Webhook settings saved");
    },
    onError: (err) => {
      toast.error("Could not save webhook settings", {
        description: err.message,
      });
    },
  });
}

// ---------------------------------------------------------------------------
// Named connections (m62 multi-connection)
// ---------------------------------------------------------------------------

/** GET /api/v1/sites/:siteId/email/connections */
export function useEmailConnections(
  siteId: string,
): UseQueryResult<EmailConnection[], Error> {
  return useQuery({
    queryKey: emailKeys.connections(siteId),
    queryFn: async () => {
      const { data, error } = await listEmailConnections({
        path: { siteId },
      });
      if (error) throw toError(error);
      return data?.connections ?? [];
    },
    staleTime: 60_000,
  });
}

/** Error class for 409 connection-referenced delete rejection */
export class ConnectionReferencedError extends Error {
  constructor(message?: string) {
    super(
      message ??
        "This connection is referenced by default_connection, fallback_connection, or a per-FROM mapping and cannot be deleted. Update routing first.",
    );
    this.name = "ConnectionReferencedError";
  }
}

/** PUT /api/v1/sites/:siteId/email/connections/:connKey */
export function usePutEmailConnection(
  siteId: string,
): UseMutationResult<
  EmailConnection,
  Error,
  { connKey: string; body: PutEmailConnectionRequest }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ connKey, body }) => {
      const { data, error } = await putEmailConnection({
        path: { siteId, connKey },
        body,
      });
      if (error) throw toError(error);
      return data;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: emailKeys.connections(siteId),
      });
      // Config query embeds connections, invalidate it too
      void queryClient.invalidateQueries({
        queryKey: emailKeys.siteConfig(siteId),
      });
      toast.success("Connection saved");
    },
    onError: (err) => {
      toast.error("Could not save connection", { description: err.message });
    },
  });
}

/** DELETE /api/v1/sites/:siteId/email/connections/:connKey */
export function useDeleteEmailConnection(
  siteId: string,
): UseMutationResult<void, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (connKey: string) => {
      const { error, response } = await deleteEmailConnection({
        path: { siteId, connKey },
      });
      if (response?.status === 409) {
        throw new ConnectionReferencedError();
      }
      if (error) throw toError(error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: emailKeys.connections(siteId),
      });
      void queryClient.invalidateQueries({
        queryKey: emailKeys.siteConfig(siteId),
      });
      toast.success("Connection deleted");
    },
    onError: (err) => {
      if (err instanceof ConnectionReferencedError) {
        toast.error("Cannot delete connection", { description: err.message });
      } else {
        toast.error("Could not delete connection", { description: err.message });
      }
    },
  });
}

// ---------------------------------------------------------------------------
// Notify settings (m62 alerts + digest)
// ---------------------------------------------------------------------------

/**
 * GET /api/v1/email/notify-settings
 *
 * This endpoint NEVER 404s — it returns defaults when no settings row exists
 * yet (the 0.35.1 lesson applied to the new settings endpoint).
 */
export function useEmailNotifySettings(): UseQueryResult<
  EmailNotifySettings,
  Error
> {
  return useQuery({
    queryKey: emailKeys.notifySettings(),
    queryFn: async () => {
      const { data, error, response } = await getEmailNotifySettings();
      if (error) {
        // Pre-0.36 API: treat 404 as feature-unavailable (return null; hide the card)
        if (response?.status === 404) return null as unknown as EmailNotifySettings;
        throw toError(error);
      }
      return data;
    },
    staleTime: 60_000,
  });
}

/** PUT /api/v1/email/notify-settings */
export function usePutEmailNotifySettings(): UseMutationResult<
  EmailNotifySettings,
  Error,
  PutEmailNotifySettingsRequest
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body) => {
      const { data, error } = await putEmailNotifySettings({ body });
      if (error) throw toError(error);
      return data;
    },
    onSuccess: (updated) => {
      queryClient.setQueryData(emailKeys.notifySettings(), updated);
      toast.success("Notification settings saved");
    },
    onError: (err) => {
      toast.error("Could not save notification settings", {
        description: err.message,
      });
    },
  });
}

// Re-export new types consumed by feature components
export type { EmailConnection, EmailNotifySettings, PutEmailConnectionRequest, PutEmailNotifySettingsRequest };
