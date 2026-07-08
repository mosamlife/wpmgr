import { describe, it, expect, beforeEach, afterEach } from "vitest";

import {
  loadRazorpayCheckout,
  __resetRazorpayCheckoutLoaderForTests,
  type RazorpayConstructor,
} from "./razorpay-checkout";

// Pure loader-logic coverage (no React) — mirrors the pattern used for
// use-billing.ts's `shouldPollCheckoutReturn` (see use-billing.test.ts): the
// loader is dependency-free enough to drive directly against jsdom's real
// `document`/`window`, so every branch (already loaded, load succeeds, load
// fails, repeated calls reuse one script tag) is independently testable
// without mounting any component.

const FAKE_RAZORPAY = {} as unknown as RazorpayConstructor;

function scriptTags(): HTMLScriptElement[] {
  return Array.from(
    document.querySelectorAll('script[src="https://checkout.razorpay.com/v1/checkout.js"]'),
  );
}

beforeEach(() => {
  __resetRazorpayCheckoutLoaderForTests();
  delete (window as unknown as { Razorpay?: unknown }).Razorpay;
  for (const s of scriptTags()) s.remove();
});

afterEach(() => {
  delete (window as unknown as { Razorpay?: unknown }).Razorpay;
  for (const s of scriptTags()) s.remove();
});

describe("loadRazorpayCheckout — already loaded", () => {
  it("resolves immediately with window.Razorpay and never appends a script tag", async () => {
    (window as unknown as { Razorpay?: unknown }).Razorpay = FAKE_RAZORPAY;

    const result = await loadRazorpayCheckout();

    expect(result).toBe(FAKE_RAZORPAY);
    expect(scriptTags()).toHaveLength(0);
  });
});

describe("loadRazorpayCheckout — first-time load", () => {
  it("appends exactly ONE <script> tag pointed at Razorpay's Checkout.js, and resolves with window.Razorpay once it fires 'load'", async () => {
    const pending = loadRazorpayCheckout();

    // A second concurrent call before the first resolves must reuse the same
    // in-flight promise rather than appending a second script tag (the
    // "load once, on demand" contract this loader exists to guarantee).
    const secondPending = loadRazorpayCheckout();
    expect(scriptTags()).toHaveLength(1);

    // Simulate the real script finishing: Razorpay's Checkout.js sets
    // `window.Razorpay` as a side effect of executing, then the browser
    // fires the script's 'load' event.
    (window as unknown as { Razorpay?: unknown }).Razorpay = FAKE_RAZORPAY;
    scriptTags()[0]!.dispatchEvent(new Event("load"));

    const [first, second] = await Promise.all([pending, secondPending]);
    expect(first).toBe(FAKE_RAZORPAY);
    expect(second).toBe(FAKE_RAZORPAY);
  });

  it("rejects (never throws synchronously) when the script fails to load, e.g. blocked by CSP or an ad blocker", async () => {
    const pending = loadRazorpayCheckout();
    const script = scriptTags()[0];
    expect(script).toBeDefined();

    script!.dispatchEvent(new Event("error"));

    await expect(pending).rejects.toThrow(
      "Could not load the Razorpay checkout script.",
    );
  });

  it("allows a fresh attempt (a new script tag) after a prior load failure", async () => {
    const firstAttempt = loadRazorpayCheckout();
    scriptTags()[0]!.dispatchEvent(new Event("error"));
    await expect(firstAttempt).rejects.toThrow();

    // Clean up the failed tag the way a real page would (this loader itself
    // never removes it) before retrying, so the retry's own tag is the only
    // one present.
    for (const s of scriptTags()) s.remove();

    const secondAttempt = loadRazorpayCheckout();
    expect(scriptTags()).toHaveLength(1);
    (window as unknown as { Razorpay?: unknown }).Razorpay = FAKE_RAZORPAY;
    scriptTags()[0]!.dispatchEvent(new Event("load"));

    await expect(secondAttempt).resolves.toBe(FAKE_RAZORPAY);
  });
});
