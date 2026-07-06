import type { BillingPlanId } from "./use-billing";

// The four hosted-SaaS tiers, in ascending order. Presentational catalog
// data (marketing copy) for the pricing cards — independent of the backend's
// internal entitlement ladder (apps/api/internal/billing/entitlements.go),
// which governs enforcement, not display copy.

export interface PlanCatalogEntry {
  id: BillingPlanId;
  name: string;
  priceLabel: string;
  sitesLimit: number;
  storageLabel: string;
  cadenceLabel: string;
}

export const PLAN_CATALOG: readonly PlanCatalogEntry[] = [
  {
    id: "free",
    name: "Free",
    priceLabel: "$0/mo",
    sitesLimit: 3,
    storageLabel: "Bring your own storage",
    cadenceLabel: "5 minute checks",
  },
  {
    id: "starter",
    name: "Starter",
    priceLabel: "$15/mo",
    sitesLimit: 10,
    storageLabel: "50 GB storage",
    cadenceLabel: "Daily backups",
  },
  {
    id: "agency",
    name: "Agency",
    priceLabel: "$59/mo",
    sitesLimit: 50,
    storageLabel: "250 GB storage",
    cadenceLabel: "Hourly backups",
  },
  {
    id: "scale",
    name: "Scale",
    priceLabel: "$169/mo",
    sitesLimit: 200,
    storageLabel: "1 TB storage",
    cadenceLabel: "Hourly backups",
  },
] as const;

/** Ascending tier order — index is the rank used by `isDowngrade`. */
export const PLAN_ORDER: readonly BillingPlanId[] = [
  "free",
  "starter",
  "agency",
  "scale",
];

export function planRank(id: BillingPlanId): number {
  return PLAN_ORDER.indexOf(id);
}

export function planCatalogEntry(id: BillingPlanId): PlanCatalogEntry | undefined {
  return PLAN_CATALOG.find((p) => p.id === id);
}

export function planLabel(id: string): string {
  const known = PLAN_CATALOG.find((p) => p.id === id);
  if (known) return known.name;
  return id.length > 0 ? id.charAt(0).toUpperCase() + id.slice(1) : id;
}

/** Whether switching from `current` to `target` would be a downgrade. */
export function isDowngrade(current: BillingPlanId, target: BillingPlanId): boolean {
  return planRank(target) < planRank(current);
}

/**
 * Server-guarded downgrade note: true when the tenant's current site usage
 * exceeds the target tier's site limit, so the client-side button disables
 * pre-emptively. The server enforces this regardless (the same
 * CheckSiteCreate site-cap gate a downgrade eventually resolves into) — this
 * is a UX guard, not the source of truth.
 */
export function exceedsPlanLimit(usedSites: number, target: PlanCatalogEntry): boolean {
  return usedSites > target.sitesLimit;
}
