import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from "@tanstack/react-query";
import {
  client,
  listAudit,
  verifyAudit,
  type AuditEntry,
  type AuditVerify,
} from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

// Fleet-wide audit log hooks backed by the /api/v1/audit endpoint.
//
// useAudit: paginated listing with optional action-prefix and site_id filters.
//   Uses offset pagination (limit/offset) matching the SDK shape.
//   Each page fetch is a separate query key so the "Load more" affordance
//   appends without invalidating existing pages.
//
// useAuditVerify: calls /api/v1/audit/verify to check the hash-chain integrity
//   of the tenant's audit log. Returns { ok, broken_at }.
//
// useAuditRebaseline: calls /api/v1/audit/integrity/rebaseline to move the
//   integrity anchor to the current chain head after an operator has reviewed
//   and acknowledged a historical break (see AuditIntegrityReport).

export const auditKeys = {
  all: ["audit"] as const,
  lists: () => [...auditKeys.all, "list"] as const,
  list: (params: AuditParams) => [...auditKeys.lists(), params] as const,
  verify: () => [...auditKeys.all, "verify"] as const,
};

export interface AuditParams {
  action: string;
  siteId: string;
  limit: number;
  offset: number;
}

export interface UseAuditResult {
  items: AuditEntry[];
  isPending: boolean;
  isError: boolean;
  /** True while a fetch (initial or refetch) is in flight; drives the Reload button's loading state. */
  isFetching: boolean;
  error: Error | null;
  refetch: UseQueryResult<AuditEntry[], Error>["refetch"];
}

export function useAudit(params: AuditParams): UseAuditResult {
  const result = useQuery<AuditEntry[], Error>({
    queryKey: auditKeys.list(params),
    queryFn: async () => {
      const { data, error } = await listAudit({
        query: {
          limit: params.limit,
          offset: params.offset,
          ...(params.action ? { action: params.action } : {}),
          ...(params.siteId ? { site_id: params.siteId } : {}),
        },
      });
      if (error) throw toError(error);
      return data?.items ?? [];
    },
    // Keep data alive while the user pages; don't discard the previous result
    // when offset/action change so the table does not flash empty.
    placeholderData: (prev) => prev,
  });

  return {
    items: result.data ?? [],
    isPending: result.isPending,
    isError: result.isError,
    isFetching: result.isFetching,
    error: result.error,
    refetch: result.refetch,
  };
}

export function useAuditVerify(): UseQueryResult<AuditVerify, Error> {
  return useQuery<AuditVerify, Error>({
    queryKey: auditKeys.verify(),
    queryFn: async () => {
      const { data, error } = await verifyAudit();
      if (error) throw toError(error);
      return data ?? { ok: true };
    },
    refetchInterval: 60_000,
  });
}

/**
 * POST /api/v1/audit/integrity/rebaseline (owner/admin): moves the chain's
 * trusted anchor to the current head so verification runs forward from that
 * point on. This is the resolution for a historical chain break: the
 * append-only log is never edited, only where verification starts is, and
 * the rebaseline itself is audited server-side.
 *
 * Hand-rolled via the raw `client` (same pattern as the other not-yet-in-the-
 * generated-SDK hooks across the app, e.g. use-hardening.ts / use-scan.ts):
 * the backend is landing this OpenAPI operation and regenerating the SDK in
 * parallel. Migrate this to the generated `postAuditIntegrityRebaseline` fn
 * (re-exported from packages/openapi-client/src/index.ts) once that lands.
 *
 * `verify` is the single source of truth for the badge/dialog state, so on
 * success we invalidate + refetch it rather than trust a hand-typed response
 * body.
 */
export function useAuditRebaseline(): UseMutationResult<void, Error, void> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      const result = await client.post({
        url: "/api/v1/audit/integrity/rebaseline",
      });
      if (result.error) throw toError(result.error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: auditKeys.verify() });
    },
  });
}
