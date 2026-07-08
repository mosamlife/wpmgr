import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import type { UseMutationResult } from "@tanstack/react-query";
import type { Me } from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { mockMutationResult, mockQueryResult } from "@/test/query-mocks";
import { authKeys } from "@/features/auth/use-auth";

import { Route as BillingRoute } from "./billing";
import {
  useBilling,
  useCreateBillingCheckout,
  useCreateBillingPortal,
  useCancelBillingSubscription,
  useVerifyRazorpayCheckout,
  type BillingInfo,
  type CheckoutResult,
  type CreateCheckoutVariables,
  type CancelSubscriptionResult,
  type VerifyCheckoutResult,
  type RazorpayCheckoutSuccess,
  type PortalResult,
} from "@/features/billing/use-billing";
import {
  loadRazorpayCheckout,
  type RazorpayCheckoutOptions,
  type RazorpayInstance,
} from "@/features/billing/razorpay-checkout";
import { toast } from "@/components/toast";

// M16 Phase B — Razorpay checkout UI outcome tests.
//
// Mounts the REAL route component/singleton (`Route.options.component`,
// `Route.update(...)`), exactly like
// routes/_authed/admin/accounts/-index.test.tsx and
// routes/_authed/settings/-route.test.tsx: the page reads/writes the URL via
// `Route.useSearch()` / `useNavigate({ from: Route.fullPath })`, both bound
// to this file's own exported `Route` singleton, so the test router below
// re-attaches that EXACT singleton to a throwaway root route rather than
// rendering the component as an inert child of a generic wrapper (which
// would leave `Route.useSearch()` unable to resolve anything).
//
// Only the billing data hooks, the Razorpay Checkout.js loader, and the
// toast helpers are mocked. `useMe()` and `useBillingCheckoutReturn` run for
// real: `useMe()` is pre-seeded into the query cache (mirrors the
// admin-accounts test's own note on why — avoids an accidental real
// `getMe()` network call), and `useBillingCheckoutReturn` is the exact
// mechanism this feature reuses for the Razorpay post-payment "finalizing
// your subscription" poll, so mocking it would test a stub instead of the
// real reuse.

vi.mock("@/features/billing/use-billing", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/billing/use-billing")>();
  return {
    ...actual,
    useBilling: vi.fn(),
    useCreateBillingCheckout: vi.fn(),
    useCreateBillingPortal: vi.fn(),
    useCancelBillingSubscription: vi.fn(),
    useVerifyRazorpayCheckout: vi.fn(),
  };
});

vi.mock("@/features/billing/razorpay-checkout", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("@/features/billing/razorpay-checkout")>();
  return { ...actual, loadRazorpayCheckout: vi.fn() };
});

vi.mock("@/components/toast", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/components/toast")>();
  return {
    ...actual,
    toast: { ...actual.toast, success: vi.fn(), error: vi.fn() },
  };
});

const mockedUseBilling = vi.mocked(useBilling);
const mockedUseCreateBillingCheckout = vi.mocked(useCreateBillingCheckout);
const mockedUseCreateBillingPortal = vi.mocked(useCreateBillingPortal);
const mockedUseCancelBillingSubscription = vi.mocked(useCancelBillingSubscription);
const mockedUseVerifyRazorpayCheckout = vi.mocked(useVerifyRazorpayCheckout);
const mockedLoadRazorpayCheckout = vi.mocked(loadRazorpayCheckout);
const mockedToastSuccess = vi.mocked(toast.success);
const mockedToastError = vi.mocked(toast.error);

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const TENANT_ID = "11111111-1111-1111-1111-111111111111";

const ME_FIXTURE: Me = {
  user: {
    id: "00000000-0000-0000-0000-000000000001",
    email: "owner@wpmgr.test",
    name: "Owner",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  },
  memberships: [
    { user_id: "00000000-0000-0000-0000-000000000001", tenant_id: TENANT_ID, role: "owner" },
  ],
  active_tenant_id: TENANT_ID,
  hosted: true,
};

function billingFixture(overrides: Partial<BillingInfo> = {}): BillingInfo {
  return {
    plan: "free",
    plan_status: "none",
    meters: { sites: { used: 1, limit: 3 } },
    portal_available: false,
    ...overrides,
  };
}

/**
 * Attaches this file's exported `Route` singleton to a throwaway root route
 * and returns a router mounted at `initialPath` — the same post-hoc wiring
 * `routeTree.gen.ts` performs for every file route, scoped to a single
 * collapsed path segment instead of the real `_authed`/`settings` parent
 * chain (see routes/_authed/admin/accounts/-index.test.tsx's identical
 * pattern and its module-doc explanation).
 */
function buildBillingRouter(initialPath: string) {
  const rootRoute = createRootRoute({});
  type UpdateOptions = Parameters<typeof BillingRoute.update>[0];
  const billingRoute = BillingRoute.update({
    id: "/settings/billing",
    path: "/settings/billing",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const routeTree = rootRoute.addChildren([billingRoute]);
  return createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [initialPath] }),
  });
}

function renderBillingPage(initialPath = "/settings/billing") {
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(authKeys.me, ME_FIXTURE);
  const router = buildBillingRouter(initialPath);
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  return router;
}

/**
 * Builds a `mutate` spy that synchronously invokes the caller's `onSuccess`
 * with `result` — simulating a mutation that resolves immediately. Narrow
 * cast at the mock boundary (same pattern as ban-list-panel.test.tsx /
 * add-site-dialog.test.tsx): the mock only implements the 1-arg `onSuccess`
 * callback shape the component actually calls, not TanStack Query's full
 * 4-arg `MutateOptions` signature.
 */
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

/** Same idea as `fireOnSuccess`, but for a mutation whose caller only reads `onSettled`. */
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

// A fake Razorpay Checkout.js constructor: captures the options it was
// constructed with and exposes a spy for `.open()`, so tests can both
// inspect what the component handed Checkout.js and drive its callbacks
// (`handler`, `modal.ondismiss`) as if the operator interacted with the real
// modal.
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

  mockedUseCreateBillingPortal.mockReturnValue(
    mockMutationResult<PortalResult, void>({}),
  );
  mockedUseCancelBillingSubscription.mockReturnValue(
    mockMutationResult<CancelSubscriptionResult, void>({}),
  );
  mockedUseVerifyRazorpayCheckout.mockReturnValue(
    mockMutationResult<VerifyCheckoutResult, RazorpayCheckoutSuccess>({}),
  );
  mockedLoadRazorpayCheckout.mockResolvedValue(FakeRazorpay);

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
// Provider/currency picker — posts the right checkout body
// ---------------------------------------------------------------------------

describe("Payment method picker", () => {
  it("defaults to Stripe: Upgrade posts { tier, provider: 'stripe' } with no currency (non-vacuous: proves the default matches the CP's own default)", async () => {
    const mutateMock = vi.fn();
    mockedUseBilling.mockReturnValue(
      mockQueryResult<BillingInfo | null>({ data: billingFixture() }),
    );
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({ mutate: mutateMock }),
    );

    renderBillingPage();

    const stripeRadio = await screen.findByRole("radio", { name: "Stripe" });
    expect(stripeRadio).toHaveAttribute("aria-checked", "true");
    // The currency picker is Razorpay-only — must not render while Stripe is selected.
    expect(screen.queryByRole("radiogroup", { name: "Currency" })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Upgrade to Starter" }));

    expect(mutateMock).toHaveBeenCalledTimes(1);
    expect(mutateMock).toHaveBeenCalledWith(
      { tier: "starter", provider: "stripe", currency: undefined },
      expect.anything(),
    );
  });

  it("switching to Razorpay reveals a currency choice; selecting INR posts { provider: 'razorpay', currency: 'INR' }", async () => {
    const mutateMock = vi.fn();
    mockedUseBilling.mockReturnValue(
      mockQueryResult<BillingInfo | null>({ data: billingFixture() }),
    );
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({ mutate: mutateMock }),
    );

    renderBillingPage();

    fireEvent.click(await screen.findByRole("radio", { name: "Razorpay" }));
    const currencyGroup = await screen.findByRole("radiogroup", { name: "Currency" });
    expect(currencyGroup).toBeInTheDocument();

    fireEvent.click(screen.getByRole("radio", { name: "INR (₹)" }));
    fireEvent.click(screen.getByRole("button", { name: "Upgrade to Agency" }));

    expect(mutateMock).toHaveBeenCalledWith(
      { tier: "agency", provider: "razorpay", currency: "INR" },
      expect.anything(),
    );
  });
});

// ---------------------------------------------------------------------------
// Stripe path — unregressed redirect behavior
// ---------------------------------------------------------------------------

describe("Stripe checkout", () => {
  it("redirects the browser to the returned Stripe URL (existing behavior, unregressed)", async () => {
    mockedUseBilling.mockReturnValue(
      mockQueryResult<BillingInfo | null>({ data: billingFixture() }),
    );
    const mutateMock = fireOnSuccess<CheckoutResult, CreateCheckoutVariables>({
      url: "https://checkout.stripe.com/session/abc",
    });
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({ mutate: mutateMock }),
    );

    renderBillingPage();

    fireEvent.click(await screen.findByRole("button", { name: "Upgrade to Starter" }));

    expect(window.location.href).toBe("https://checkout.stripe.com/session/abc");
    // The Razorpay loader must never even be consulted on the Stripe path.
    expect(mockedLoadRazorpayCheckout).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// Razorpay path — Checkout.js modal + verify + poll
// ---------------------------------------------------------------------------

describe("Razorpay checkout", () => {
  it("opens the Checkout.js modal with the subscription's key/id/amount/currency, and on handler success calls verify then starts the billing poll", async () => {
    const refetchMock = vi.fn();
    mockedUseBilling.mockReturnValue(
      mockQueryResult<BillingInfo | null>({ data: billingFixture(), refetch: refetchMock }),
    );
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
      mockMutationResult<VerifyCheckoutResult, RazorpayCheckoutSuccess>({ mutate: verifyMutateMock }),
    );

    const router = renderBillingPage();

    fireEvent.click(await screen.findByRole("radio", { name: "Razorpay" }));
    fireEvent.click(screen.getByRole("radio", { name: "INR (₹)" }));
    fireEvent.click(screen.getByRole("button", { name: "Upgrade to Agency" }));

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
    capturedRazorpayOptions!.handler({
      razorpay_payment_id: "pay_1",
      razorpay_subscription_id: "sub_123",
      razorpay_signature: "sig_1",
    });

    expect(verifyMutateMock).toHaveBeenCalledWith(
      {
        razorpay_payment_id: "pay_1",
        razorpay_subscription_id: "sub_123",
        razorpay_signature: "sig_1",
      },
      expect.anything(),
    );

    // The verify call settling flips `?checkout=success` on THIS router (via
    // useNavigate({ from: Route.fullPath })) — proving it reuses the real
    // navigation the Stripe redirect success path uses, not a stand-in.
    await waitFor(() => expect(router.state.location.search.checkout).toBe("success"));

    // That in turn arms the real (unmocked) useBillingCheckoutReturn poll,
    // which refetches billing immediately — the actual source of truth for
    // the plan flip, never the client-side verify response.
    await waitFor(() => expect(refetchMock).toHaveBeenCalled());
    expect(await screen.findByText("Finalizing your subscription…")).toBeInTheDocument();
  });

  it("does nothing (no toast, no navigation) when the operator dismisses the modal without paying", async () => {
    mockedUseBilling.mockReturnValue(
      mockQueryResult<BillingInfo | null>({ data: billingFixture() }),
    );
    const mutateMock = fireOnSuccess<CheckoutResult, CreateCheckoutVariables>({
      razorpay: {
        subscription_id: "sub_123",
        key_id: "rzp_test_456",
        currency: "USD",
        amount: 1500,
      },
    });
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({ mutate: mutateMock }),
    );

    const router = renderBillingPage();

    fireEvent.click(await screen.findByRole("radio", { name: "Razorpay" }));
    fireEvent.click(screen.getByRole("button", { name: "Upgrade to Agency" }));
    await waitFor(() => expect(razorpayOpenMock).toHaveBeenCalledTimes(1));

    capturedRazorpayOptions!.modal!.ondismiss!();

    expect(mockedToastError).not.toHaveBeenCalled();
    expect(mockedToastSuccess).not.toHaveBeenCalled();
    expect(router.state.location.search.checkout).toBeUndefined();
  });

  it("shows a fallback toast when Checkout.js fails to load, instead of throwing inside the click handler", async () => {
    mockedUseBilling.mockReturnValue(
      mockQueryResult<BillingInfo | null>({ data: billingFixture() }),
    );
    const mutateMock = fireOnSuccess<CheckoutResult, CreateCheckoutVariables>({
      razorpay: {
        subscription_id: "sub_123",
        key_id: "rzp_test_456",
        currency: "USD",
        amount: 1500,
      },
    });
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({ mutate: mutateMock }),
    );
    mockedLoadRazorpayCheckout.mockRejectedValue(
      new Error("Could not load the Razorpay checkout script."),
    );

    renderBillingPage();

    fireEvent.click(await screen.findByRole("radio", { name: "Razorpay" }));
    fireEvent.click(screen.getByRole("button", { name: "Upgrade to Agency" }));

    await waitFor(() => expect(mockedToastError).toHaveBeenCalledTimes(1));
    expect(mockedToastError).toHaveBeenCalledWith(
      "Could not open the Razorpay payment window",
      expect.objectContaining({
        description: "Could not load the Razorpay checkout script.",
      }),
    );
    expect(razorpayOpenMock).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// Cancel action (no portal) vs Manage-billing portal link
// ---------------------------------------------------------------------------

describe("Cancel subscription vs Manage billing", () => {
  it("shows Cancel subscription (never Manage billing) when portal_available is false, and posts an EMPTY-body POST /billing/cancel on confirm", async () => {
    mockedUseBilling.mockReturnValue(
      mockQueryResult<BillingInfo | null>({
        data: billingFixture({ plan: "starter", plan_status: "active", portal_available: false }),
      }),
    );
    const cancelMutateAsync = vi.fn(
      (): Promise<CancelSubscriptionResult> => Promise.resolve({ ok: true }),
    );
    mockedUseCancelBillingSubscription.mockReturnValue(
      mockMutationResult<CancelSubscriptionResult, void>({ mutateAsync: cancelMutateAsync }),
    );

    renderBillingPage();

    expect(await screen.findByRole("button", { name: "Cancel subscription" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Manage billing" })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Cancel subscription" }));

    const confirmInput = await screen.findByLabelText(/type/i);
    fireEvent.change(confirmInput, { target: { value: "Starter" } });

    // The trigger button is aria-hidden while the modal Radix dialog is open
    // (see restore-dialog.test.tsx's identical note), so this uniquely
    // resolves to the dialog's own confirm button.
    fireEvent.click(screen.getByRole("button", { name: "Cancel subscription" }));

    await waitFor(() => expect(cancelMutateAsync).toHaveBeenCalledTimes(1));
    // Cancel's wire contract is a bare POST with an EMPTY body — the mutation
    // hook takes no variables at all, unlike checkout/verify which carry a
    // payload; this proves no stray body field was ever wired in.
    expect(cancelMutateAsync).toHaveBeenCalledWith(undefined);

    expect(mockedToastSuccess).toHaveBeenCalledWith(
      "Your subscription will be cancelled at the end of the current billing period",
    );
  });

  it("shows Manage billing (never Cancel subscription) when portal_available is true", async () => {
    mockedUseBilling.mockReturnValue(
      mockQueryResult<BillingInfo | null>({
        data: billingFixture({ plan: "starter", plan_status: "active", portal_available: true }),
      }),
    );
    const portalMutate = fireOnSuccess<PortalResult, void>({
      url: "https://billing.stripe.com/p/session_abc",
    });
    mockedUseCreateBillingPortal.mockReturnValue(
      mockMutationResult<PortalResult, void>({ mutate: portalMutate }),
    );

    renderBillingPage();

    expect(await screen.findByRole("button", { name: "Manage billing" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Cancel subscription" })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Manage billing" }));

    expect(portalMutate).toHaveBeenCalledTimes(1);
    expect(window.location.href).toBe("https://billing.stripe.com/p/session_abc");
  });
});
