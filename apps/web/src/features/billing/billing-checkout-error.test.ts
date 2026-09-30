import { describe, it, expect } from "vitest";

import { mapBillingCheckoutError } from "./billing-checkout-error";

// S0.4 (stripe-design-v9.md 3.3, 5.10) — the 409 copy for POST
// /billing/checkout, one case per code the Go handler actually returns
// (apps/api/internal/billing/checkout.go:84,86,89,298; providerLocked at :52).

describe("mapBillingCheckoutError", () => {
  it("billing_subscription_exists names the current plan and offers Manage billing", () => {
    const result = mapBillingCheckoutError(
      "billing_subscription_exists",
      undefined,
      "Agency",
      "fallback",
    );
    expect(result.message).toBe(
      "You already have the Agency plan. Change it in Manage billing.",
    );
    expect(result.action).toBe("manage_billing");
  });

  it("billing_comped points at support and offers no action", () => {
    const result = mapBillingCheckoutError(
      "billing_comped",
      undefined,
      "Starter",
      "fallback",
    );
    expect(result.message).toBe(
      "Your plan is complimentary. Contact support to change it.",
    );
    expect(result.action).toBeUndefined();
  });

  it("billing_subscription_pending offers Manage billing", () => {
    const result = mapBillingCheckoutError(
      "billing_subscription_pending",
      undefined,
      "Starter",
      "fallback",
    );
    expect(result.message).toBe(
      "You already have a subscription; it may still be activating.",
    );
    expect(result.action).toBe("manage_billing");
  });

  it("billing_checkout_superseded offers to start again", () => {
    const result = mapBillingCheckoutError(
      "billing_checkout_superseded",
      undefined,
      "Starter",
      "fallback",
    );
    expect(result.message).toBe(
      "A newer checkout was started for this workspace. Continue there, or start again.",
    );
    expect(result.action).toBe("start_again");
  });

  describe("billing_provider_locked — copy chosen by details.reason", () => {
    it("needs_support", () => {
      expect(
        mapBillingCheckoutError(
          "billing_provider_locked",
          "needs_support",
          "Starter",
          "fallback",
        ).message,
      ).toBe("Contact support to switch.");
    });

    it("pending_at_provider", () => {
      expect(
        mapBillingCheckoutError(
          "billing_provider_locked",
          "pending_at_provider",
          "Starter",
          "fallback",
        ).message,
      ).toBe("Cancel your current subscription first.");
    });

    it("provider_changed", () => {
      expect(
        mapBillingCheckoutError(
          "billing_provider_locked",
          "provider_changed",
          "Starter",
          "fallback",
        ).message,
      ).toBe("Your payment method changed in another tab. Reload and try again.");
    });

    it("an unknown or missing reason falls back to a generic retry message", () => {
      expect(
        mapBillingCheckoutError(
          "billing_provider_locked",
          "some_future_reason",
          "Starter",
          "fallback",
        ).message,
      ).toBe("Reload and try again.");
      expect(
        mapBillingCheckoutError(
          "billing_provider_locked",
          undefined,
          "Starter",
          "fallback",
        ).message,
      ).toBe("Reload and try again.");
    });
  });

  it("falls back to the server's own message for an undocumented code", () => {
    const result = mapBillingCheckoutError(
      "billing_invalid_currency",
      undefined,
      "Starter",
      "currency must be INR",
    );
    expect(result.message).toBe("currency must be INR");
    expect(result.action).toBeUndefined();
  });

  it("falls back to a generic message when both code and server message are absent", () => {
    const result = mapBillingCheckoutError(undefined, undefined, "Starter", "");
    expect(result.message).toBe("Could not start checkout.");
  });
});
