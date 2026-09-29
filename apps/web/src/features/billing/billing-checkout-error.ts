// S0.4 (stripe-design-v9.md 3.3, 5.10) — POST /billing/checkout's 409 copy.
//
// Codes read out of the Go handler, not guessed — same idiom as
// `mapApiKeyError` in features/api-keys/use-api-keys.ts and
// `mapDeleteOrgError` in features/orgs/use-orgs.ts:
//   apps/api/internal/billing/checkout.go:84  billing_comped
//   apps/api/internal/billing/checkout.go:86  billing_subscription_exists
//   apps/api/internal/billing/checkout.go:89  billing_subscription_pending
//   apps/api/internal/billing/checkout.go:52   billing_provider_locked (+ details.reason)
//   apps/api/internal/billing/checkout.go:298  billing_checkout_superseded
// details.reason values (checkout.go:44-46): needs_support,
// pending_at_provider, provider_changed.

export type BillingCheckoutErrorCode =
  | "billing_subscription_exists"
  | "billing_comped"
  | "billing_subscription_pending"
  | "billing_provider_locked"
  | "billing_checkout_superseded";

export type BillingProviderLockedReason =
  | "needs_support"
  | "pending_at_provider"
  | "provider_changed";

/** Raised by useCreateBillingCheckout; carries the server's code + reason. */
export class BillingCheckoutError extends Error {
  code?: string;
  reason?: string;
  constructor(message: string, code?: string, reason?: string) {
    super(message);
    this.name = "BillingCheckoutError";
    this.code = code;
    this.reason = reason;
  }
}

function isBillingCheckoutErrorCode(
  code: string,
): code is BillingCheckoutErrorCode {
  return (
    code === "billing_subscription_exists" ||
    code === "billing_comped" ||
    code === "billing_subscription_pending" ||
    code === "billing_provider_locked" ||
    code === "billing_checkout_superseded"
  );
}

export interface BillingCheckoutErrorCopy {
  message: string;
  /** "manage_billing": show/point at the Manage billing action. "start_again": offer to retry the checkout. */
  action?: "manage_billing" | "start_again";
}

function providerLockedMessage(reason: string | undefined): string {
  switch (reason as BillingProviderLockedReason | undefined) {
    case "needs_support":
      return "Contact support to switch.";
    case "pending_at_provider":
      return "Cancel your current subscription first.";
    case "provider_changed":
      return "Your payment method changed in another tab. Reload and try again.";
    default:
      return "Reload and try again.";
  }
}

/**
 * Maps a checkout refusal's code (+ details.reason, for
 * billing_provider_locked) into clear, human copy. `currentPlanLabel` fills
 * the "You already have the X plan" wording for billing_subscription_exists.
 * Falls back to the server's own message for any undocumented code — pure +
 * exported so every documented code is covered by a test without a network
 * call (see billing-checkout-error.test.ts).
 */
export function mapBillingCheckoutError(
  code: string | undefined,
  reason: string | undefined,
  currentPlanLabel: string,
  fallbackMessage: string,
): BillingCheckoutErrorCopy {
  if (!code || !isBillingCheckoutErrorCode(code)) {
    return { message: fallbackMessage || "Could not start checkout." };
  }
  switch (code) {
    case "billing_subscription_exists":
      return {
        message: `You already have the ${currentPlanLabel} plan. Change it in Manage billing.`,
        action: "manage_billing",
      };
    case "billing_comped":
      return {
        message: "Your plan is complimentary. Contact support to change it.",
      };
    case "billing_subscription_pending":
      return {
        message: "You already have a subscription; it may still be activating.",
        action: "manage_billing",
      };
    case "billing_checkout_superseded":
      return {
        message:
          "A newer checkout was started for this workspace. Continue there, or start again.",
        action: "start_again",
      };
    case "billing_provider_locked":
      return { message: providerLockedMessage(reason) };
  }
}
