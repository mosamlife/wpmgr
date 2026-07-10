import { describe, it, expect } from "vitest";

import { resolveCheckoutTier, shouldDropIntoSitesAfterCheckout } from "./welcome.checkout";

// Pure-logic coverage for welcome.checkout.tsx's two exported helpers — no
// React/router needed (mirrors use-billing.test.ts's own
// `shouldPollCheckoutReturn` pattern).

describe("resolveCheckoutTier — URL then stash precedence", () => {
  it("prefers the URL's own plan when both are present", () => {
    expect(resolveCheckoutTier("starter", "scale")).toBe("starter");
  });

  it("falls back to the stash when the URL carries no plan", () => {
    expect(resolveCheckoutTier(undefined, "scale")).toBe("scale");
  });

  it("returns undefined when neither is present", () => {
    expect(resolveCheckoutTier(undefined, undefined)).toBeUndefined();
  });
});

describe("shouldDropIntoSitesAfterCheckout — the Razorpay success-to-Sites edge", () => {
  it("is false before any checkout has succeeded (status undefined)", () => {
    expect(
      shouldDropIntoSitesAfterCheckout({ status: undefined, wasFinalizing: false, isFinalizing: false }),
    ).toBe(false);
  });

  it("is false on the very first render even if isFinalizing starts false (no prior finalizing edge yet)", () => {
    expect(
      shouldDropIntoSitesAfterCheckout({ status: "success", wasFinalizing: false, isFinalizing: false }),
    ).toBe(false);
  });

  it("is false while still finalizing", () => {
    expect(
      shouldDropIntoSitesAfterCheckout({ status: "success", wasFinalizing: true, isFinalizing: true }),
    ).toBe(false);
  });

  it("is true exactly on the finalizing-to-done edge once a checkout succeeded", () => {
    expect(
      shouldDropIntoSitesAfterCheckout({ status: "success", wasFinalizing: true, isFinalizing: false }),
    ).toBe(true);
  });
});
