import type { BillingInfo } from "./use-billing";

// Pure derivation of the Billing page's status banner from `plan_status`.
// Kept dependency-free (no React) so every branch is independently
// unit-testable (see use-billing.test.ts "billing-page state rendering").

export type BillingBannerTone = "warning" | "muted";

export interface BillingBanner {
  tone: BillingBannerTone;
  message: string;
}

/**
 * Format an ISO timestamp as "Jan 12, 2027" for billing dates.
 *
 * Pinned to en-US/UTC rather than the viewer's locale/timezone: a renewal or
 * grace-until date is a contractual calendar boundary set by the payment
 * provider, not a local event time, so it must read identically regardless
 * of where the operator is sitting (and this keeps the formatter
 * deterministic for tests, unlike `toLocaleDateString(undefined, ...)`).
 */
export function formatBillingDate(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return iso;
  return new Intl.DateTimeFormat("en-US", {
    month: "short",
    day: "numeric",
    year: "numeric",
    timeZone: "UTC",
  }).format(date);
}

/**
 * Derives the status banner (if any) for a `plan_status`. Returns `null` for
 * states that need no banner (none, trialing, active, comped).
 */
export function billingBannerFor(
  billing: Pick<BillingInfo, "plan_status" | "grace_until">,
): BillingBanner | null {
  switch (billing.plan_status) {
    case "past_due": {
      const until = billing.grace_until
        ? formatBillingDate(billing.grace_until)
        : null;
      return {
        tone: "warning",
        message: until
          ? `Payment issue. Service continues until ${until}. Update your card to avoid interruption.`
          : "Payment issue. Update your card to avoid interruption.",
      };
    }
    case "canceled":
      return {
        tone: "muted",
        message:
          "Your subscription is canceled. Choose a plan below to restore paid features.",
      };
    case "paused":
      return {
        tone: "muted",
        message:
          "Your subscription is paused. Choose a plan below to resume paid features.",
      };
    case "none":
    case "trialing":
    case "active":
    case "comped":
      return null;
    default:
      return null;
  }
}

/** Human label for a plan_status, used next to the plan name. */
export function planStatusLabel(status: BillingInfo["plan_status"]): string {
  switch (status) {
    case "none":
      return "No plan";
    case "trialing":
      return "Trialing";
    case "active":
      return "Active";
    case "past_due":
      return "Payment issue";
    case "canceled":
      return "Canceled";
    case "paused":
      return "Paused";
    case "comped":
      return "Comped";
    default:
      return status;
  }
}
