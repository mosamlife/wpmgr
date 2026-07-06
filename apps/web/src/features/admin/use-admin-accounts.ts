import {
  useQuery,
  useMutation,
  useQueryClient,
  keepPreviousData,
  type UseMutationResult,
} from "@tanstack/react-query";
import { client } from "@wpmgr/api";
import type {
  AdminAccountTiles,
  AdminAccountListItem,
  AdminAccountsResponse,
  AdminAccountUsage,
  AdminAccountSubscription,
  AdminAccountTimelineEntry,
  AdminAccountMember,
  AdminAccountSite,
  AdminAccountDetail,
  AdminRevenueTiles,
  AdminPlanDistributionRow,
  AdminCompedRow,
  AdminPastDueRow,
  AdminRecentBillingEvent,
  AdminRevenueResponse,
  AdminCompAccountRequest,
  AdminReasonRequest,
  AdminExtendGraceRequest,
  AdminForceStateRequest,
} from "@wpmgr/api";

import { toast } from "@/components/toast";
import { toError } from "@/features/auth/use-auth";
import { buildAccountsQuery, type AdminAccountsFilters } from "./admin-accounts-format";

// M16 Phase C1 — superadmin Billing Admin Panel hooks.
//
// These call the hand-rolled /api/v1/admin/accounts* and /api/v1/admin/revenue
// endpoints via the shared `client` (same credentials/baseUrl as every
// generated SDK call), NOT the generated SDK operation functions — the ROUTES
// themselves stay hand-rolled Gin (ADR-047: ogen's own server is not mounted
// for /admin/*, because these routes need requireSuperadmin/RLS middleware
// that only Gin carries), so there is no generated `listAdminAccounts` /
// `getAdminAccount` / `compAdminAccount` etc. to call.
//
// The WIRE TYPES below, however, ARE generated: packages/openapi/openapi.yaml
// documents this contract under the `admin-billing` tag, and
// apps/api/internal/admin/billing_contract_test.go enforces, server-side,
// that the hand-rolled Go DTOs in billing_dto.go still match it. Every type
// exported from this file is either a direct re-export of the generated
// `@wpmgr/api` type, or a same-shape alias where this file's
// already-established name differs from the spec's schema name (documented
// inline below) — there is no hand-maintained duplicate struct left to drift.
//
// A prior drift between a hand-maintained interface in this file and
// billing_dto.go (invented nesting like `sites: {used,cap}` instead of the
// real flat `sites_used`/`sites_cap`, `tiles.past_due`/`tiles.total` instead
// of `past_due_count`/`accounts_total`) crashed the whole /admin/accounts
// panel in prod. Importing the generated type instead of hand-declaring one
// turns that class of bug into a compile error — if the backend and the spec
// ever disagree, `billing_contract_test.go` fails first; if this file and the
// spec ever disagree, `pnpm -C apps/web typecheck` fails.
//
// The mutation INPUT types are the one place this file keeps a hand-written
// shape: SetOverridesInput allows `null` to explicitly clear a previously-set
// override (see admin-account-dialogs.tsx, which sends `null` for an emptied
// input), but the OpenAPI code generator (hey-api 0.97.3) strips a schema's
// `nullable: true` on an OPTIONAL property down to plain `?:` in the emitted
// TS type (compare `AdminSetOverridesRequest` in @wpmgr/api, whose
// `sites`/`storage_gb`/`seats` come out as `number | undefined`, not
// `number | null | undefined`) — using it verbatim here would reject a real,
// spec-legal payload. Every other mutation input below IS the generated
// request type directly.

// ---------------------------------------------------------------------------
// Domain types — re-exported from the generated OpenAPI client. View-model
// derivation (usage meters, entitlement display rows, timeline labels) lives
// in admin-accounts-format.ts alongside its other pure derivation helpers.
// ---------------------------------------------------------------------------

export type {
  AdminAccountTiles,
  AdminAccountListItem,
  AdminAccountUsage,
  AdminAccountSubscription,
  AdminAccountTimelineEntry,
  AdminAccountMember,
  AdminAccountSite,
  AdminAccountDetail,
  AdminRevenueTiles,
  AdminPlanDistributionRow,
  AdminCompedRow,
  AdminPastDueRow,
};

/** Alias for the generated `AdminAccountsResponse` — kept under this file's established name (every call site imports it as such). */
export type AdminAccountsListResponse = AdminAccountsResponse;

/** Alias for the generated `AdminRecentBillingEvent` — kept under this file's established name (every call site imports it as such). */
export type AdminRevenueEvent = AdminRecentBillingEvent;

export type { AdminRevenueResponse };

// ---------------------------------------------------------------------------
// Query key factory
// ---------------------------------------------------------------------------

export const adminAccountsKeys = {
  all: ["admin", "accounts"] as const,
  list: (filters: AdminAccountsFilters) =>
    ["admin", "accounts", "list", filters] as const,
  detail: (tenantId: string) => ["admin", "accounts", "detail", tenantId] as const,
};

export const adminRevenueKeys = {
  info: ["admin", "revenue"] as const,
};

/** Invalidates every cache a mutation on one account can affect: its own detail, the accounts list, and the revenue tiles (MRR/counts can shift). */
function invalidateAccountEverywhere(
  qc: ReturnType<typeof useQueryClient>,
  tenantId: string,
) {
  void qc.invalidateQueries({ queryKey: adminAccountsKeys.detail(tenantId) });
  void qc.invalidateQueries({ queryKey: adminAccountsKeys.all });
  void qc.invalidateQueries({ queryKey: adminRevenueKeys.info });
}

// ---------------------------------------------------------------------------
// Queries
// ---------------------------------------------------------------------------

export function useAdminAccountsList(filters: AdminAccountsFilters) {
  return useQuery({
    queryKey: adminAccountsKeys.list(filters),
    queryFn: async (): Promise<AdminAccountsListResponse> => {
      const qs = buildAccountsQuery(filters);
      const r = await client.get({ url: `/api/v1/admin/accounts?${qs}` });
      if (r.error) throw toError(r.error);
      return r.data as AdminAccountsListResponse;
    },
    // Keep the previous page's rows on screen while the next page loads (the
    // pager and filter chips stay interactive instead of flashing a skeleton).
    placeholderData: keepPreviousData,
    staleTime: 30_000,
  });
}

export function useAdminAccountDetail(tenantId: string | undefined) {
  return useQuery({
    queryKey: adminAccountsKeys.detail(tenantId ?? ""),
    queryFn: async (): Promise<AdminAccountDetail> => {
      const r = await client.get({
        url: `/api/v1/admin/accounts/${tenantId}`,
      });
      if (r.error) throw toError(r.error);
      return r.data as AdminAccountDetail;
    },
    enabled: tenantId != null && tenantId.length > 0,
    staleTime: 15_000,
  });
}

export function useAdminRevenue() {
  return useQuery({
    queryKey: adminRevenueKeys.info,
    queryFn: async (): Promise<AdminRevenueResponse> => {
      const r = await client.get({ url: "/api/v1/admin/revenue" });
      if (r.error) throw toError(r.error);
      return r.data as AdminRevenueResponse;
    },
    staleTime: 30_000,
  });
}

// ---------------------------------------------------------------------------
// Mutations — every manual control requires a `reason`, per the spec. Each
// invalidates the affected account's detail + the accounts list + revenue
// tiles in onSuccess so every surface reflects the change immediately.
// ---------------------------------------------------------------------------

/** The POST .../comp body — the generated request type directly (tier accepts the full ladder including "free", matching billing.ValidTier server-side). */
export type CompAccountInput = AdminCompAccountRequest;

export function useAdminCompAccount(
  tenantId: string,
): UseMutationResult<unknown, Error, CompAccountInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (body: CompAccountInput) => {
      const r = await client.post({
        url: `/api/v1/admin/accounts/${tenantId}/comp`,
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (r.error) throw toError(r.error);
      return r.data;
    },
    onSuccess: () => {
      invalidateAccountEverywhere(qc, tenantId);
      toast.success("Account comped");
    },
    onError: (err: Error) =>
      toast.error("Comp failed", { description: err.message }),
  });
}

/** The DELETE .../comp body — the generated request type directly. */
export type RevokeCompInput = AdminReasonRequest;

export function useAdminRevokeComp(
  tenantId: string,
): UseMutationResult<unknown, Error, RevokeCompInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (body: RevokeCompInput) => {
      const r = await client.delete({
        url: `/api/v1/admin/accounts/${tenantId}/comp`,
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (r.error) throw toError(r.error);
      return r.data;
    },
    onSuccess: () => {
      invalidateAccountEverywhere(qc, tenantId);
      toast.success("Comp revoked");
    },
    onError: (err: Error) =>
      toast.error("Revoke comp failed", { description: err.message }),
  });
}

/**
 * The PUT .../overrides body. Hand-kept (NOT the generated
 * `AdminSetOverridesRequest`) — see this file's header comment: the codegen
 * strips `nullable: true` on an optional property, but `null` is a real,
 * spec-legal value here (explicitly clears a previously-set override; an
 * omitted key leaves it untouched; see admin-account-dialogs.tsx).
 */
export interface SetOverridesInput {
  sites?: number | null;
  storage_gb?: number | null;
  seats?: number | null;
  reason: string;
}

export function useAdminSetOverrides(
  tenantId: string,
): UseMutationResult<unknown, Error, SetOverridesInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (body: SetOverridesInput) => {
      const r = await client.put({
        url: `/api/v1/admin/accounts/${tenantId}/overrides`,
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (r.error) throw toError(r.error);
      return r.data;
    },
    onSuccess: () => {
      invalidateAccountEverywhere(qc, tenantId);
      toast.success("Overrides updated");
    },
    onError: (err: Error) =>
      toast.error("Overrides update failed", { description: err.message }),
  });
}

/** The POST .../grace body — the generated request type directly. */
export type ExtendGraceInput = AdminExtendGraceRequest;

export function useAdminExtendGrace(
  tenantId: string,
): UseMutationResult<unknown, Error, ExtendGraceInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (body: ExtendGraceInput) => {
      const r = await client.post({
        url: `/api/v1/admin/accounts/${tenantId}/grace`,
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (r.error) throw toError(r.error);
      return r.data;
    },
    onSuccess: () => {
      invalidateAccountEverywhere(qc, tenantId);
      toast.success("Grace period extended");
    },
    onError: (err: Error) =>
      toast.error("Extend grace failed", { description: err.message }),
  });
}

/** The POST .../suspend and .../restore body — the generated request type directly. */
export type ReasonInput = AdminReasonRequest;

export function useAdminSuspendAccount(
  tenantId: string,
): UseMutationResult<unknown, Error, ReasonInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (body: ReasonInput) => {
      const r = await client.post({
        url: `/api/v1/admin/accounts/${tenantId}/suspend`,
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (r.error) throw toError(r.error);
      return r.data;
    },
    onSuccess: () => {
      invalidateAccountEverywhere(qc, tenantId);
      toast.success("Account suspended");
    },
    onError: (err: Error) =>
      toast.error("Suspend failed", { description: err.message }),
  });
}

export function useAdminRestoreAccount(
  tenantId: string,
): UseMutationResult<unknown, Error, ReasonInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (body: ReasonInput) => {
      const r = await client.post({
        url: `/api/v1/admin/accounts/${tenantId}/restore`,
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (r.error) throw toError(r.error);
      return r.data;
    },
    onSuccess: () => {
      invalidateAccountEverywhere(qc, tenantId);
      toast.success("Account restored");
    },
    onError: (err: Error) =>
      toast.error("Restore failed", { description: err.message }),
  });
}

/** The POST .../state body — the generated request type directly. */
export type ForceStateInput = AdminForceStateRequest;

/** "Force billing state" — reconciles the CP's view of plan/status with the provider only. Never call this to grant service; use comp for that. */
export function useAdminForceBillingState(
  tenantId: string,
): UseMutationResult<unknown, Error, ForceStateInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (body: ForceStateInput) => {
      const r = await client.post({
        url: `/api/v1/admin/accounts/${tenantId}/state`,
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (r.error) throw toError(r.error);
      return r.data;
    },
    onSuccess: () => {
      invalidateAccountEverywhere(qc, tenantId);
      toast.success("Billing state reconciled");
    },
    onError: (err: Error) =>
      toast.error("Reconcile failed", { description: err.message }),
  });
}
