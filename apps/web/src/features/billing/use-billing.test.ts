import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, act } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";

// `client.post` is mocked at the `@wpmgr/api` wire boundary (never the hook
// module itself — see use-site-connection.test.ts's identical note), so the
// "wire contract" describe block below exercises useCancelBillingSubscription's
// real mutationFn, including its `when: "now"` body-spread branching.
const { postMock } = vi.hoisted(() => ({ postMock: vi.fn() }));
vi.mock("@wpmgr/api", () => ({ client: { post: postMock } }));

import {
  shouldPollCheckoutReturn,
  isCheckoutTier,
  billingKeys,
  useCancelBillingSubscription,
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

// ---------------------------------------------------------------------------
// useCancelBillingSubscription — wire contract (real hook against a faked
// transport, GH #745 Stripe S0 review). Cancel now's whole safety property is
// that its body reads `{ when: "now" }` — an empty body sends a period-end
// cancel instead (see cancel.go:118-120 for what the server does with each),
// so this proves the mutationFn's own body-spread branch, not just the
// component that calls it.
// ---------------------------------------------------------------------------

function makeQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
}

function wrapperFor(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return createElement(QueryClientProvider, { client: qc }, children);
  };
}

describe("useCancelBillingSubscription — wire contract", () => {
  beforeEach(() => {
    postMock.mockReset();
    postMock.mockResolvedValue({ data: { ok: true }, error: undefined, response: { status: 200 } });
  });

  it("posts { when: 'now' } in the body for Cancel now", async () => {
    const { result } = renderHook(() => useCancelBillingSubscription(), {
      wrapper: wrapperFor(makeQueryClient()),
    });

    await act(async () => {
      await result.current.mutateAsync({ when: "now" });
    });

    expect(postMock).toHaveBeenCalledTimes(1);
    expect(postMock).toHaveBeenCalledWith(
      expect.objectContaining({
        url: "/api/v1/billing/cancel",
        body: { when: "now" },
      }),
    );
  });

  it("sends no body for a period-end cancel (over-fire guard: 'when: now' must not leak into the default path)", async () => {
    const { result } = renderHook(() => useCancelBillingSubscription(), {
      wrapper: wrapperFor(makeQueryClient()),
    });

    await act(async () => {
      await result.current.mutateAsync(undefined);
    });

    expect(postMock).toHaveBeenCalledTimes(1);
    const call = postMock.mock.calls[0][0] as { url: string; body?: unknown };
    expect(call.url).toBe("/api/v1/billing/cancel");
    expect(call.body).toBeUndefined();
  });
});
