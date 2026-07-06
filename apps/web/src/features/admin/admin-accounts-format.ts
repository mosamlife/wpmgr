import type { BillingPlanId } from "@/features/billing/use-billing";

// M16 Phase C1 — pure derivation + formatting logic for the superadmin Billing
// Admin panel (Accounts, Account detail, Revenue). Kept dependency-free (no
// React) so every branch is independently unit-testable, mirroring
// features/billing/billing-status.ts.

// ---------------------------------------------------------------------------
// Money
// ---------------------------------------------------------------------------

/** Format a cent amount as USD ("$12.00"). */
export function formatCents(cents: number): string {
  return new Intl.NumberFormat("en-US", {
    style: "currency",
    currency: "USD",
  }).format(cents / 100);
}

/**
 * The Accounts table MRR column has three distinct renderings per the spec:
 *   - comped accounts show "$0 comped" regardless of the stored mrr_cents
 *   - free-tier or canceled accounts show an em-dash (no billable MRR)
 *   - everything else shows the real dollar amount
 */
export function formatAccountMrr(item: {
  mrr_cents: number;
  plan: string;
  plan_status: string;
}): string {
  if (item.plan_status === "comped") return "$0 comped";
  if (item.plan === "free" || item.plan_status === "canceled") return "–";
  return formatCents(item.mrr_cents);
}

// ---------------------------------------------------------------------------
// Account display status — suspended overrides everything else, per the
// Accounts table Status column spec.
// ---------------------------------------------------------------------------

export type AccountDisplayStatus =
  | "active"
  | "trialing"
  | "past_due"
  | "canceled"
  | "comped"
  | "suspended";

export const ACCOUNT_STATUS_LABEL: Record<AccountDisplayStatus, string> = {
  active: "Active",
  trialing: "Trialing",
  past_due: "Past due",
  canceled: "Canceled",
  comped: "Comped",
  suspended: "Suspended",
};

/**
 * Tailwind palette classes for the status pill. No semantic token exists for
 * "trialing" (blue) or "comped" (violet), so — matching the existing
 * house pattern for one-off admin pills (the superadmin badge and connection
 * state pills in routes/_authed/admin/index.tsx) — those two use the raw
 * Tailwind palette with explicit dark: variants. active/past_due/suspended
 * reuse the shared success/warning/destructive subtle tokens.
 */
export const ACCOUNT_STATUS_BADGE_CLASS: Record<AccountDisplayStatus, string> = {
  active: "bg-success-subtle text-success-subtle-fg",
  trialing: "bg-blue-100 text-blue-800 dark:bg-blue-900/40 dark:text-blue-300",
  past_due: "bg-warning-subtle text-warning-subtle-fg",
  canceled: "bg-muted text-muted-foreground",
  comped: "bg-violet-100 text-violet-800 dark:bg-violet-900/40 dark:text-violet-300",
  suspended: "bg-destructive-subtle text-destructive-subtle-fg",
};

/**
 * Derives the single display status from the raw account fields.
 * `suspended` always wins — an account can be suspended while its underlying
 * subscription is still `active` (e.g. a manual admin suspension), and the
 * operator needs that to be unmissable regardless of billing state.
 */
export function accountDisplayStatus(account: {
  plan_status: string;
  suspended: boolean;
}): AccountDisplayStatus {
  if (account.suspended) return "suspended";
  switch (account.plan_status) {
    case "comped":
      return "comped";
    case "trialing":
      return "trialing";
    case "past_due":
      return "past_due";
    case "canceled":
    case "paused":
    case "none":
      return "canceled";
    default:
      return "active";
  }
}

// ---------------------------------------------------------------------------
// Meters — used/cap coloring shared by the Accounts table chips and the
// account detail "Usage vs limits" card.
// ---------------------------------------------------------------------------

export type MeterTone = "ok" | "warning" | "critical";

/** Raw used/cap percent — NOT clamped, so callers can detect over-cap (>100). */
export function meterPercent(used: number, cap: number): number {
  if (cap <= 0) return 0;
  return (used / cap) * 100;
}

/** Clamped 0-100 integer percent for progress-bar rendering. */
export function meterBarPercent(rawPercent: number): number {
  return Math.min(100, Math.max(0, Math.round(rawPercent)));
}

/** amber at 80%+, red at 100%+, else the calm default. */
export function meterTone(rawPercent: number): MeterTone {
  if (rawPercent >= 100) return "critical";
  if (rawPercent >= 80) return "warning";
  return "ok";
}

/** True when usage has actually exceeded the cap (not just reached it). */
export function isOverCap(used: number, cap: number): boolean {
  return cap > 0 && used > cap;
}

export const METER_TONE_TEXT_CLASS: Record<MeterTone, string> = {
  ok: "text-muted-foreground",
  warning: "text-warning-subtle-fg",
  critical: "text-destructive",
};

// ---------------------------------------------------------------------------
// Idle detection — "90d idle" styling for Last activity
// ---------------------------------------------------------------------------

const IDLE_THRESHOLD_MS = 90 * 24 * 60 * 60 * 1000;

/** True when `lastActivityAt` is null (never active) or more than 90 days old. */
export function isIdle90d(
  lastActivityAt: string | null,
  now: number = Date.now(),
): boolean {
  if (!lastActivityAt) return true;
  const then = Date.parse(lastActivityAt);
  if (Number.isNaN(then)) return true;
  return now - then > IDLE_THRESHOLD_MS;
}

// ---------------------------------------------------------------------------
// Accounts list filters — URL/wire mapping
//
// Wire contract (pinned): GET /api/v1/admin/accounts?search=&status=&plan=
// &near_limit=&has_overrides=&comped=&idle=&sort=&limit=&offset=
//
// The Status multi-select dropdown offers all six display statuses (matching
// the table's Status column exactly), joined into the single `status` param.
// past_due/comped/near-limit/has-overrides/idle-90d also get their own
// one-tap quick-toggle chips per the spec; the past_due and comped chips are
// convenience shortcuts that toggle the same `status` entry (past_due) or the
// dedicated `comped` boolean (since the API exposes comped as its own axis,
// independent of status — an account can be comped while its underlying
// subscription is in any state).
// ---------------------------------------------------------------------------

export const ACCOUNT_STATUS_FILTER_OPTIONS: readonly AccountDisplayStatus[] = [
  "active",
  "trialing",
  "past_due",
  "canceled",
  "suspended",
  "comped",
];

export const ACCOUNT_PLAN_FILTER_OPTIONS: readonly BillingPlanId[] = [
  "free",
  "starter",
  "agency",
  "scale",
];

export type AdminAccountSort =
  | "needs_attention"
  | "mrr_desc"
  | "created_desc"
  | "name_asc";

export const DEFAULT_ACCOUNTS_SORT: AdminAccountSort = "needs_attention";
export const DEFAULT_ACCOUNTS_LIMIT = 25;

export const ACCOUNT_SORT_OPTIONS: ReadonlyArray<{
  value: AdminAccountSort;
  label: string;
}> = [
  { value: "needs_attention", label: "Needs attention" },
  { value: "mrr_desc", label: "MRR (high to low)" },
  { value: "created_desc", label: "Newest" },
  { value: "name_asc", label: "Org name (A to Z)" },
];

export interface AdminAccountsFilters {
  search: string;
  status: AccountDisplayStatus[];
  plan: BillingPlanId[];
  nearLimit: boolean;
  hasOverrides: boolean;
  comped: boolean;
  idle90d: boolean;
  sort: AdminAccountSort;
  limit: number;
  offset: number;
}

export function defaultAccountsFilters(): AdminAccountsFilters {
  return {
    search: "",
    status: [],
    plan: [],
    nearLimit: false,
    hasOverrides: false,
    comped: false,
    idle90d: false,
    sort: DEFAULT_ACCOUNTS_SORT,
    limit: DEFAULT_ACCOUNTS_LIMIT,
    offset: 0,
  };
}

/** Pure — builds the wire query string for GET /api/v1/admin/accounts. */
export function buildAccountsQuery(filters: AdminAccountsFilters): string {
  const params = new URLSearchParams();
  const search = filters.search.trim();
  if (search) params.set("search", search);
  if (filters.status.length > 0) params.set("status", filters.status.join(","));
  if (filters.plan.length > 0) params.set("plan", filters.plan.join(","));
  if (filters.nearLimit) params.set("near_limit", "true");
  if (filters.hasOverrides) params.set("has_overrides", "true");
  if (filters.comped) params.set("comped", "true");
  if (filters.idle90d) params.set("idle", "true");
  params.set("sort", filters.sort);
  params.set("limit", String(filters.limit));
  params.set("offset", String(filters.offset));
  return params.toString();
}

/** Count of active filter axes (search + every toggle/multi-select), for the "Clear filters" pill. */
export function activeAccountsFilterCount(filters: AdminAccountsFilters): number {
  let count = 0;
  if (filters.search.trim()) count += 1;
  if (filters.status.length > 0) count += 1;
  if (filters.plan.length > 0) count += 1;
  if (filters.nearLimit) count += 1;
  if (filters.hasOverrides) count += 1;
  if (filters.comped) count += 1;
  if (filters.idle90d) count += 1;
  return count;
}

// ---------------------------------------------------------------------------
// Revenue page — defensive client-side ordering
//
// The API response order is not contractually pinned; sort defensively so the
// worklist and event feed always read correctly regardless of what the
// backend returns (same defensive-ordering principle used for the fleet
// Audit log after the newest-first display bug).
// ---------------------------------------------------------------------------

export function sortPastDueOldestFirst<T extends { days_past_due: number }>(
  rows: readonly T[],
): T[] {
  return [...rows].sort((a, b) => b.days_past_due - a.days_past_due);
}

export function sortEventsNewestFirst<T extends { at: string }>(
  rows: readonly T[],
): T[] {
  return [...rows].sort((a, b) => Date.parse(b.at) - Date.parse(a.at));
}
