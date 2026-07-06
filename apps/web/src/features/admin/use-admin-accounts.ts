import {
  useQuery,
  useMutation,
  useQueryClient,
  keepPreviousData,
  type UseMutationResult,
} from "@tanstack/react-query";
import { client } from "@wpmgr/api";

import { toast } from "@/components/toast";
import { toError } from "@/features/auth/use-auth";
import type { BillingPlanId, BillingPlanStatus } from "@/features/billing/use-billing";
import { buildAccountsQuery, type AdminAccountsFilters } from "./admin-accounts-format";

// M16 Phase C1 — superadmin Billing Admin Panel hooks.
//
// These call the hand-rolled /api/v1/admin/accounts* and /api/v1/admin/revenue
// endpoints. Like use-admin.ts and use-admin-vuln-feed.ts, they are NOT in the
// generated OpenAPI SDK (superadmin-only; the control plane is landing this
// contract in parallel with this web surface) — hand-rolled via the shared
// `client` (same credentials/baseUrl as every generated SDK call). Swap to
// generated `listAdminAccounts` / `getAdminAccount` / `getAdminRevenue` /
// `compAdminAccount` etc. (re-exported from packages/openapi-client/src/index.ts)
// once the SDK regenerates against the landed contract.
//
// IMPORTANT: the types below MUST track apps/api/internal/admin/billing_dto.go
// field-for-field (names, nesting, optionality) until these endpoints move
// into OpenAPI and this file is deleted in favor of a generated client. A
// prior drift between this file and billing_dto.go (invented nesting like
// `sites: {used,cap}` instead of the real flat `sites_used`/`sites_cap`,
// `tiles.past_due`/`tiles.total` instead of `past_due_count`/`accounts_total`)
// crashed the whole /admin/accounts panel in prod — read billing_dto.go, not
// memory, before touching this file again. A future task should add
// /admin/accounts* to packages/openapi/openapi.yaml so the client here is
// generated instead of hand-rolled.
// ---------------------------------------------------------------------------
// Domain types — wire DTOs only. View-model derivation (usage meters,
// entitlement display rows, timeline labels) lives in admin-accounts-format.ts
// alongside its other pure derivation helpers.
// ---------------------------------------------------------------------------

export interface AdminAccountTiles {
  mrr_cents: number;
  active_subs: number;
  past_due_count: number;
  accounts_total: number;
}

export interface AdminAccountListItem {
  tenant_id: string;
  org_name: string;
  org_slug: string;
  owner_email?: string;
  plan: string;
  plan_status: string;
  suspended_at?: string;
  has_overrides: boolean;
  mrr_cents: number;
  sites_used: number;
  sites_cap: number;
  storage_used_bytes_approx: number;
  storage_cap_bytes: number;
  near_limit: boolean;
  created_at: string;
  last_activity?: string;
}

export interface AdminAccountsListResponse {
  tiles: AdminAccountTiles;
  items: AdminAccountListItem[];
  total: number;
  limit: number;
  offset: number;
}

export interface AdminAccountEntitlementValues {
  probe_interval_floor_sec: number;
  backup_cadence_floor_seconds: number;
  incremental_backups: boolean;
  client_portal: boolean;
}

export interface AdminAccountUsage {
  sites: { used: number; cap: number };
  storage_bytes_approx: { used: number; cap: number };
  seats_used: number;
  restore_volume_bytes_approx: number;
  entitlements: AdminAccountEntitlementValues;
}

export interface AdminAccountSubscription {
  provider?: string;
  provider_customer_id?: string;
  provider_subscription_id?: string;
  dashboard_url?: string;
  current_period_end?: string;
  cancel_at_period_end: boolean;
  grace_until?: string;
  comp_reason?: string;
  last_billing_event_at?: string;
  stale: boolean;
}

export interface AdminAccountTimelineEntry {
  source: "billing_event" | "audit";
  occurred_at: string;
  kind: string;
  actor_type?: string;
  actor_id?: string;
  metadata?: Record<string, unknown>;
}

export interface AdminAccountMember {
  id: string;
  email: string;
  name: string;
  role: string;
  status: string;
  email_verified: boolean;
  last_login_at?: string;
  member_since: string;
}

export interface AdminAccountSite {
  id: string;
  url: string;
  connection_state: string;
  created_at: string;
}

export interface AdminAccountDetail {
  tenant_id: string;
  org_name: string;
  org_slug: string;
  owner_email?: string;
  plan: string;
  plan_status: string;
  mrr_cents: number;
  created_at: string;
  suspended_at?: string;
  suspended_reason?: string;
  usage: AdminAccountUsage;
  subscription: AdminAccountSubscription;
  timeline: AdminAccountTimelineEntry[];
  members: AdminAccountMember[];
  sites: AdminAccountSite[];
}

export interface AdminRevenueTiles {
  mrr_cents: number;
  mrr_past_due_cents: number;
  active_subs: number;
  trialing_subs: number;
  past_due_count: number;
  past_due_at_risk_cents: number;
  new_this_month: number;
  canceled_this_month: number;
}

export interface AdminPlanDistributionRow {
  plan: string;
  count: number;
  mrr_cents: number;
}

export interface AdminCompedRow {
  count: number;
  hypothetical_value_cents: number;
}

export interface AdminPastDueRow {
  tenant_id: string;
  org_name: string;
  org_slug: string;
  owner_email?: string;
  amount_cents: number;
  days_past_due: number;
  grace_until?: string;
  last_payment_failed_at?: string;
}

export interface AdminRevenueEvent {
  id: string;
  occurred_at: string;
  org_name?: string;
  org_slug?: string;
  tenant_id?: string;
  kind: string;
  provider: string;
}

export interface AdminRevenueResponse {
  tiles: AdminRevenueTiles;
  plan_distribution: AdminPlanDistributionRow[];
  comped: AdminCompedRow;
  past_due: AdminPastDueRow[];
  recent_events: AdminRevenueEvent[];
  last_webhook_received_at?: string;
}

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

export interface CompAccountInput {
  tier: BillingPlanId;
  reason: string;
}

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

export interface RevokeCompInput {
  reason: string;
}

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

export interface SetOverridesInput {
  /**
   * `number` sets an override, `null` explicitly clears a previously-set
   * override, and an omitted key leaves that limit untouched — the endpoint
   * signature marks every field optional (`sites?`), which reads as a
   * partial-update PUT rather than a full-replace, so untouched rows are
   * simply never sent.
   */
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

export interface ExtendGraceInput {
  until: string;
  reason: string;
}

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

export interface ReasonInput {
  reason: string;
}

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

export interface ForceStateInput {
  plan: BillingPlanId;
  plan_status: BillingPlanStatus;
  reason: string;
}

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
