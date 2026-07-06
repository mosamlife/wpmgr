import { describe, it, expect } from "vitest";

import {
  shouldPollCheckoutReturn,
  isCheckoutTier,
  billingKeys,
  type CheckoutPollSnapshot,
} from "./use-billing";

// Pure-logic coverage for the checkout-return polling decision and the
// checkout-tier type guard. Mirrors the pattern in use-site-connection.test.ts
// (no React/QueryClient — the full mutation lifecycle is integration-level).

// ---------------------------------------------------------------------------
// shouldPollCheckoutReturn — the checkout-return polling logic
// ---------------------------------------------------------------------------

describe("shouldPollCheckoutReturn", () => {
  const active: CheckoutPollSnapshot = { plan: "starter", plan_status: "active" };
  const free: CheckoutPollSnapshot = { plan: "free", plan_status: "none" };

  it("keeps polling when there is no current snapshot yet (billing hasn't loaded)", () => {
    expect(
      shouldPollCheckoutReturn({ elapsedMs: 0, baseline: free, current: null }),
    ).toBe(true);
  });

  it("keeps polling while the current snapshot still matches the pre-checkout baseline", () => {
    expect(
      shouldPollCheckoutReturn({
        elapsedMs: 4000,
        baseline: free,
        current: { ...free },
      }),
    ).toBe(true);
  });

  it("stops polling once the plan changed from baseline", () => {
    expect(
      shouldPollCheckoutReturn({ elapsedMs: 4000, baseline: free, current: active }),
    ).toBe(false);
  });

  it("stops polling once only plan_status changed (e.g. trialing -> active) even if plan id is stable", () => {
    const trialing: CheckoutPollSnapshot = { plan: "starter", plan_status: "trialing" };
    expect(
      shouldPollCheckoutReturn({ elapsedMs: 4000, baseline: trialing, current: active }),
    ).toBe(false);
  });

  it("stops polling once the default 30s budget elapses, even with no observed change", () => {
    expect(
      shouldPollCheckoutReturn({ elapsedMs: 30_000, baseline: free, current: free }),
    ).toBe(false);
  });

  it("respects a custom budget", () => {
    expect(
      shouldPollCheckoutReturn({
        elapsedMs: 5_000,
        baseline: free,
        current: free,
        budgetMs: 4_000,
      }),
    ).toBe(false);
  });

  it("stops (rather than polling forever) when there is no baseline to compare against", () => {
    // No baseline captured (e.g. billing was never cached before the redirect)
    // but we do have a current snapshot — nothing to compare, so don't spin.
    expect(
      shouldPollCheckoutReturn({ elapsedMs: 2000, baseline: null, current: free }),
    ).toBe(false);
  });

  it("elapsed just under budget still polls; at-or-over budget stops (boundary check)", () => {
    expect(
      shouldPollCheckoutReturn({ elapsedMs: 29_999, baseline: free, current: free }),
    ).toBe(true);
    expect(
      shouldPollCheckoutReturn({ elapsedMs: 30_001, baseline: free, current: free }),
    ).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// isCheckoutTier — the type guard gating which tiers can hit /checkout
// ---------------------------------------------------------------------------

describe("isCheckoutTier", () => {
  it("accepts starter, agency, scale", () => {
    expect(isCheckoutTier("starter")).toBe(true);
    expect(isCheckoutTier("agency")).toBe(true);
    expect(isCheckoutTier("scale")).toBe(true);
  });

  it("rejects free — downgrading to free happens via the billing portal, not checkout", () => {
    expect(isCheckoutTier("free")).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// billingKeys — cache key shape
// ---------------------------------------------------------------------------

describe("billingKeys", () => {
  it("info key is namespaced under 'billing'", () => {
    const key = billingKeys.info();
    expect(key[0]).toBe("billing");
    expect(key).toContain("info");
  });
});
