import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act, waitFor } from "@testing-library/react";
import type { UseMutationResult } from "@tanstack/react-query";

import { mockMutationResult } from "@/test/query-mocks";
import { toast } from "@/components/toast";

import { useCheckoutFlow } from "./use-checkout-flow";
import {
  useCreateBillingCheckout,
  useVerifyRazorpayCheckout,
  type CheckoutResult,
  type CreateCheckoutVariables,
  type VerifyCheckoutResult,
  type RazorpayCheckoutSuccess,
} from "./use-billing";
import {
  loadRazorpayCheckout,
  type RazorpayCheckoutOptions,
  type RazorpayInstance,
} from "./razorpay-checkout";

// Phase C prep — a direct, hook-only regression lock for `useCheckoutFlow`,
// the exact machinery `PlanTiersGrid` (routes/_authed/settings/billing.tsx)
// calls today and the Phase C post-verify upgrade screen will call next.
// Mirrors ../../routes/_authed/settings/-billing.test.tsx's Stripe/Razorpay
// assertions but at the hook boundary, with no route/page chrome in the way,
// so a future consumer of this hook can trust this file alone as the
// contract without re-deriving it from the billing page's DOM.

vi.mock("./use-billing", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-billing")>();
  return {
    ...actual,
    useCreateBillingCheckout: vi.fn(),
    useVerifyRazorpayCheckout: vi.fn(),
  };
});

vi.mock("./razorpay-checkout", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./razorpay-checkout")>();
  return { ...actual, loadRazorpayCheckout: vi.fn() };
});

vi.mock("@/components/toast", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/components/toast")>();
  return {
    ...actual,
    toast: { ...actual.toast, success: vi.fn(), error: vi.fn() },
  };
});

const mockedUseCreateBillingCheckout = vi.mocked(useCreateBillingCheckout);
const mockedUseVerifyRazorpayCheckout = vi.mocked(useVerifyRazorpayCheckout);
const mockedLoadRazorpayCheckout = vi.mocked(loadRazorpayCheckout);
const mockedToastError = vi.mocked(toast.error);

/** Same pattern as -billing.test.tsx's own helper: a `mutate` spy that
 *  synchronously invokes the caller's `onSuccess` with `result`. Narrow cast
 *  at the mock boundary — the mock only implements the 1-arg `onSuccess`
 *  callback shape the hook actually calls, not TanStack Query's full 4-arg
 *  `MutateOptions` signature. */
function fireOnSuccess<TData, TVariables = unknown>(
  result: TData,
): UseMutationResult<TData, Error, TVariables>["mutate"] {
  const mutate = vi.fn(
    (_vars: TVariables, opts?: { onSuccess?: (result: TData) => void }) => {
      opts?.onSuccess?.(result);
    },
  );
  return mutate as UseMutationResult<TData, Error, TVariables>["mutate"];
}

/** Same idea, but for a mutation whose caller only reads `onSettled`. */
function fireOnSettled<TData, TVariables = unknown>(): UseMutationResult<
  TData,
  Error,
  TVariables
>["mutate"] {
  const mutate = vi.fn(
    (_vars: TVariables, opts?: { onSettled?: () => void }) => {
      opts?.onSettled?.();
    },
  );
  return mutate as UseMutationResult<TData, Error, TVariables>["mutate"];
}

let capturedRazorpayOptions: RazorpayCheckoutOptions | null = null;
const razorpayOpenMock = vi.fn();

class FakeRazorpay implements RazorpayInstance {
  constructor(options: RazorpayCheckoutOptions) {
    capturedRazorpayOptions = options;
  }
  open = razorpayOpenMock;
}

const originalLocation = window.location;

beforeEach(() => {
  vi.clearAllMocks();
  capturedRazorpayOptions = null;
  mockedLoadRazorpayCheckout.mockResolvedValue(FakeRazorpay);
  mockedUseVerifyRazorpayCheckout.mockReturnValue(
    mockMutationResult<VerifyCheckoutResult, RazorpayCheckoutSuccess>({}),
  );

  Object.defineProperty(window, "location", {
    configurable: true,
    writable: true,
    value: { ...originalLocation, href: "" },
  });
});

afterEach(() => {
  Object.defineProperty(window, "location", {
    configurable: true,
    writable: true,
    value: originalLocation,
  });
});

// ---------------------------------------------------------------------------
// Stripe path
// ---------------------------------------------------------------------------

describe("useCheckoutFlow — Stripe path", () => {
  it("defaults to provider 'stripe', posts { tier, provider: 'stripe' } with no currency, and redirects the browser to the returned URL", () => {
    const mutateMock = fireOnSuccess<CheckoutResult, CreateCheckoutVariables>({
      url: "https://checkout.stripe.com/session/abc",
    });
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({ mutate: mutateMock }),
    );
    const onCheckoutSuccess = vi.fn();

    const { result } = renderHook(() => useCheckoutFlow({ onCheckoutSuccess }));

    expect(result.current.provider).toBe("stripe");
    expect(result.current.currency).toBe("USD");

    act(() => {
      result.current.startCheckout("starter");
    });

    expect(mutateMock).toHaveBeenCalledWith(
      { tier: "starter", provider: "stripe", currency: undefined },
      expect.anything(),
    );
    expect(window.location.href).toBe("https://checkout.stripe.com/session/abc");
    // The Razorpay loader must never even be consulted on the Stripe path.
    expect(mockedLoadRazorpayCheckout).not.toHaveBeenCalled();
    expect(onCheckoutSuccess).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// Razorpay path
// ---------------------------------------------------------------------------

describe("useCheckoutFlow — Razorpay path", () => {
  it("posts the selected provider/currency, opens the Checkout.js modal with the subscription's key/id/amount/currency, and on handler success verifies then calls onCheckoutSuccess", async () => {
    const mutateMock = fireOnSuccess<CheckoutResult, CreateCheckoutVariables>({
      razorpay: {
        subscription_id: "sub_123",
        key_id: "rzp_test_456",
        currency: "INR",
        amount: 120000,
      },
    });
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({ mutate: mutateMock }),
    );
    const verifyMutateMock = fireOnSettled<VerifyCheckoutResult, RazorpayCheckoutSuccess>();
    mockedUseVerifyRazorpayCheckout.mockReturnValue(
      mockMutationResult<VerifyCheckoutResult, RazorpayCheckoutSuccess>({
        mutate: verifyMutateMock,
      }),
    );
    const onCheckoutSuccess = vi.fn();

    const { result } = renderHook(() => useCheckoutFlow({ onCheckoutSuccess }));

    act(() => result.current.setProvider("razorpay"));
    act(() => result.current.setCurrency("INR"));
    act(() => result.current.startCheckout("agency"));

    expect(mutateMock).toHaveBeenCalledWith(
      { tier: "agency", provider: "razorpay", currency: "INR" },
      expect.anything(),
    );

    await waitFor(() => expect(mockedLoadRazorpayCheckout).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(razorpayOpenMock).toHaveBeenCalledTimes(1));

    expect(capturedRazorpayOptions).not.toBeNull();
    expect(capturedRazorpayOptions).toMatchObject({
      key: "rzp_test_456",
      subscription_id: "sub_123",
      amount: 120000,
      currency: "INR",
      name: "WPMgr",
    });

    // Simulate Razorpay's Checkout.js calling back into our handler with the
    // Checkout.js onSuccess payload (field names verbatim).
    act(() => {
      capturedRazorpayOptions!.handler({
        razorpay_payment_id: "pay_1",
        razorpay_subscription_id: "sub_123",
        razorpay_signature: "sig_1",
      });
    });

    expect(verifyMutateMock).toHaveBeenCalledWith(
      {
        razorpay_payment_id: "pay_1",
        razorpay_subscription_id: "sub_123",
        razorpay_signature: "sig_1",
      },
      expect.anything(),
    );
    // Verify's onSettled must fire the caller's success hook regardless of
    // verify's own outcome — see the hook's own doc comment.
    expect(onCheckoutSuccess).toHaveBeenCalledTimes(1);
  });

  it("does nothing (no toast, no onCheckoutSuccess) when the operator dismisses the modal without paying", async () => {
    const mutateMock = fireOnSuccess<CheckoutResult, CreateCheckoutVariables>({
      razorpay: { subscription_id: "sub_1", key_id: "rzp_1", currency: "USD", amount: 1500 },
    });
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({ mutate: mutateMock }),
    );
    const onCheckoutSuccess = vi.fn();

    const { result } = renderHook(() => useCheckoutFlow({ onCheckoutSuccess }));
    act(() => result.current.setProvider("razorpay"));
    act(() => result.current.startCheckout("agency"));

    await waitFor(() => expect(razorpayOpenMock).toHaveBeenCalledTimes(1));

    act(() => {
      capturedRazorpayOptions!.modal!.ondismiss!();
    });

    expect(onCheckoutSuccess).not.toHaveBeenCalled();
    expect(mockedToastError).not.toHaveBeenCalled();
  });

  it("shows a fallback toast (never throwing inside the click handler) when Checkout.js fails to load", async () => {
    const mutateMock = fireOnSuccess<CheckoutResult, CreateCheckoutVariables>({
      razorpay: { subscription_id: "sub_1", key_id: "rzp_1", currency: "USD", amount: 1500 },
    });
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({ mutate: mutateMock }),
    );
    mockedLoadRazorpayCheckout.mockRejectedValue(
      new Error("Could not load the Razorpay checkout script."),
    );
    const onCheckoutSuccess = vi.fn();

    const { result } = renderHook(() => useCheckoutFlow({ onCheckoutSuccess }));
    act(() => result.current.setProvider("razorpay"));
    act(() => result.current.startCheckout("agency"));

    await waitFor(() => expect(mockedToastError).toHaveBeenCalledTimes(1));
    expect(mockedToastError).toHaveBeenCalledWith(
      "Could not open the Razorpay payment window",
      expect.objectContaining({
        description: "Could not load the Razorpay checkout script.",
      }),
    );
    expect(razorpayOpenMock).not.toHaveBeenCalled();
    expect(onCheckoutSuccess).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// isStarting / error surface
// ---------------------------------------------------------------------------

describe("useCheckoutFlow — loading/error surface", () => {
  it("isStarting mirrors the checkout mutation's isPending", () => {
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({ isPending: true }),
    );

    const { result } = renderHook(() => useCheckoutFlow({ onCheckoutSuccess: vi.fn() }));

    expect(result.current.isStarting).toBe(true);
  });

  it("surfaces the checkout mutation's error once it has failed", () => {
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({
        isError: true,
        error: new Error("Checkout is temporarily unavailable"),
      }),
    );

    const { result } = renderHook(() => useCheckoutFlow({ onCheckoutSuccess: vi.fn() }));

    expect(result.current.error?.message).toBe("Checkout is temporarily unavailable");
  });

  it("error is null while the checkout mutation has not errored", () => {
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({}),
    );

    const { result } = renderHook(() => useCheckoutFlow({ onCheckoutSuccess: vi.fn() }));

    expect(result.current.isStarting).toBe(false);
    expect(result.current.error).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// initialCurrency — M16 Phase C2's `?currency=` hint (register.tsx / welcome.checkout.tsx)
// ---------------------------------------------------------------------------

describe("useCheckoutFlow — initialCurrency option", () => {
  it("defaults to USD when initialCurrency is omitted (unregressed /settings/billing behavior)", () => {
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({}),
    );

    const { result } = renderHook(() => useCheckoutFlow({ onCheckoutSuccess: vi.fn() }));

    expect(result.current.currency).toBe("USD");
  });

  it("seeds the currency state from initialCurrency when provided", () => {
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({}),
    );

    const { result } = renderHook(() =>
      useCheckoutFlow({ onCheckoutSuccess: vi.fn(), initialCurrency: "INR" }),
    );

    expect(result.current.currency).toBe("INR");
  });

  it("posts the seeded currency once the operator switches to Razorpay, without an explicit setCurrency call", () => {
    const mutateMock = fireOnSuccess<CheckoutResult, CreateCheckoutVariables>({
      razorpay: { subscription_id: "sub_1", key_id: "rzp_1", currency: "INR", amount: 150000 },
    });
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({ mutate: mutateMock }),
    );

    const { result } = renderHook(() =>
      useCheckoutFlow({ onCheckoutSuccess: vi.fn(), initialCurrency: "INR" }),
    );

    act(() => result.current.setProvider("razorpay"));
    act(() => result.current.startCheckout("agency"));

    expect(mutateMock).toHaveBeenCalledWith(
      { tier: "agency", provider: "razorpay", currency: "INR" },
      expect.anything(),
    );
  });
});
