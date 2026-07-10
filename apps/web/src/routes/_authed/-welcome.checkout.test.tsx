import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRouteWithContext,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import type { Me } from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { mockMutationResult, mockQueryResult } from "@/test/query-mocks";
import { authKeys } from "@/features/auth/use-auth";
import type { RouterContext } from "@/router";

import { Route as WelcomeCheckoutRoute } from "./welcome.checkout";
import {
  useBilling,
  useCreateBillingCheckout,
  useVerifyRazorpayCheckout,
  type BillingInfo,
  type CheckoutResult,
  type CreateCheckoutVariables,
  type VerifyCheckoutResult,
  type RazorpayCheckoutSuccess,
} from "@/features/billing/use-billing";
import { loadRazorpayCheckout } from "@/features/billing/razorpay-checkout";
import { readPendingPlan, stashPendingPlan, clearPendingPlan } from "@/features/billing/pending-plan";

// M16 Phase C2 — the /welcome/checkout post-verify screen. Mounts the REAL
// route (`beforeLoad` + `component`) re-attached to a throwaway root, exactly
// like routes/_authed/settings/-billing.test.tsx (whose module doc explains
// why: `Route.useSearch()` is bound to this file's own exported `Route`
// singleton). Only the billing data hooks + Razorpay loader are mocked;
// `useBillingCheckoutReturn` runs for real (same rationale as -billing.test.tsx).

vi.mock("@/features/billing/use-billing", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/billing/use-billing")>();
  return {
    ...actual,
    useBilling: vi.fn(),
    useCreateBillingCheckout: vi.fn(),
    useVerifyRazorpayCheckout: vi.fn(),
  };
});

vi.mock("@/features/billing/razorpay-checkout", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/billing/razorpay-checkout")>();
  return { ...actual, loadRazorpayCheckout: vi.fn() };
});

const mockedUseBilling = vi.mocked(useBilling);
const mockedUseCreateBillingCheckout = vi.mocked(useCreateBillingCheckout);
const mockedUseVerifyRazorpayCheckout = vi.mocked(useVerifyRazorpayCheckout);
const mockedLoadRazorpayCheckout = vi.mocked(loadRazorpayCheckout);

const TENANT_ID = "11111111-1111-1111-1111-111111111111";

const HOSTED_OWNER_ME: Me = {
  user: {
    id: "00000000-0000-0000-0000-000000000001",
    email: "owner@wpmgr.test",
    name: "Owner",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  },
  memberships: [{ user_id: "00000000-0000-0000-0000-000000000001", tenant_id: TENANT_ID, role: "owner" }],
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
 * Attaches welcome.checkout.tsx's own `Route` (beforeLoad + component) to a
 * throwaway root, plus a `/sites` stub so a real redirect/navigate resolves
 * and is observable via `router.state.location`.
 */
function buildWelcomeCheckoutRouter(
  initialPath: string,
  queryClient: ReturnType<typeof createTestQueryClient>,
) {
  const rootRoute = createRootRouteWithContext<RouterContext>()({});
  type UpdateOptions = Parameters<typeof WelcomeCheckoutRoute.update>[0];
  const welcomeCheckoutRoute = WelcomeCheckoutRoute.update({
    id: "/welcome/checkout",
    path: "/welcome/checkout",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const sitesRoute = createRoute({
    path: "/sites",
    getParentRoute: () => rootRoute,
    component: () => <div>Sites stub</div>,
  });
  const routeTree = rootRoute.addChildren([welcomeCheckoutRoute, sitesRoute]);
  return createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [initialPath] }),
    context: { queryClient },
  });
}

function renderWelcomeCheckout(initialPath: string, me: Me | null = HOSTED_OWNER_ME) {
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(authKeys.me, me);
  const router = buildWelcomeCheckoutRouter(initialPath, queryClient);
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  return router;
}

beforeEach(() => {
  vi.clearAllMocks();
  clearPendingPlan();
  mockedUseBilling.mockReturnValue(mockQueryResult<BillingInfo | null>({ data: billingFixture() }));
  mockedUseVerifyRazorpayCheckout.mockReturnValue(
    mockMutationResult<VerifyCheckoutResult, RazorpayCheckoutSuccess>({}),
  );
  mockedLoadRazorpayCheckout.mockResolvedValue(
    class {
      open = vi.fn();
      constructor() {}
    },
  );
});

describe("WelcomeCheckoutPage — self-host / resolution guards (beforeLoad)", () => {
  it("redirects to /sites when the instance is not hosted", async () => {
    const mutateMock = vi.fn();
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({ mutate: mutateMock }),
    );

    const router = renderWelcomeCheckout("/welcome/checkout?plan=agency", {
      ...HOSTED_OWNER_ME,
      hosted: false,
    });

    await waitFor(() => expect(router.state.location.pathname).toBe("/sites"));
    // Self-host safety must block BEFORE any checkout is ever created.
    expect(mutateMock).not.toHaveBeenCalled();
  });

  it("redirects to /sites when no plan is resolvable (no ?plan= and no stash)", async () => {
    const mutateMock = vi.fn();
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({ mutate: mutateMock }),
    );

    const router = renderWelcomeCheckout("/welcome/checkout");

    await waitFor(() => expect(router.state.location.pathname).toBe("/sites"));
    expect(mutateMock).not.toHaveBeenCalled();
  });

  it("resolves the plan from the stash when the URL carries no ?plan=", async () => {
    stashPendingPlan({ plan: "scale" });
    const mutateMock = vi.fn();
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({ mutate: mutateMock }),
    );

    renderWelcomeCheckout("/welcome/checkout");

    expect(
      await screen.findByRole("heading", { name: "Complete your Scale subscription" }),
    ).toBeInTheDocument();
    await waitFor(() =>
      expect(mutateMock).toHaveBeenCalledWith(
        { tier: "scale", provider: "stripe", currency: undefined },
        expect.anything(),
      ),
    );
  });

  it("the URL's ?plan= wins over a conflicting stash", async () => {
    stashPendingPlan({ plan: "scale" });
    const mutateMock = vi.fn();
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({ mutate: mutateMock }),
    );

    renderWelcomeCheckout("/welcome/checkout?plan=starter");

    expect(
      await screen.findByRole("heading", { name: "Complete your Starter subscription" }),
    ).toBeInTheDocument();
  });
});

describe("WelcomeCheckoutPage — auto-starts checkout once on mount", () => {
  it("auto-calls startCheckout(tier) with the default provider (Stripe) and renders the picker + plan summary", async () => {
    const mutateMock = vi.fn();
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({ mutate: mutateMock }),
    );

    renderWelcomeCheckout("/welcome/checkout?plan=agency");

    expect(
      await screen.findByRole("heading", { name: "Complete your Agency subscription" }),
    ).toBeInTheDocument();
    expect(screen.getByText("Agency")).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "Stripe" })).toHaveAttribute("aria-checked", "true");

    await waitFor(() => expect(mutateMock).toHaveBeenCalledTimes(1));
    expect(mutateMock).toHaveBeenCalledWith(
      { tier: "agency", provider: "stripe", currency: undefined },
      expect.anything(),
    );
  });

  it("clears the pending-plan stash once checkout has started", async () => {
    stashPendingPlan({ plan: "agency", currency: "INR" });
    const mutateMock = vi.fn();
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({ mutate: mutateMock }),
    );

    renderWelcomeCheckout("/welcome/checkout?plan=agency");
    await screen.findByRole("heading", { name: "Complete your Agency subscription" });

    await waitFor(() => expect(mutateMock).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(readPendingPlan()).toBeNull());
  });

  it("does not auto-start a second checkout on a re-render", async () => {
    const mutateMock = vi.fn();
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({ mutate: mutateMock }),
    );

    renderWelcomeCheckout("/welcome/checkout?plan=agency");
    await screen.findByRole("heading", { name: "Complete your Agency subscription" });
    await waitFor(() => expect(mutateMock).toHaveBeenCalledTimes(1));

    // Switching provider triggers a re-render of the same mounted component;
    // the auto-start effect must not fire again.
    fireEvent.click(screen.getByRole("radio", { name: "Razorpay" }));
    await waitFor(() =>
      expect(screen.getByRole("radio", { name: "Razorpay" })).toHaveAttribute(
        "aria-checked",
        "true",
      ),
    );

    expect(mutateMock).toHaveBeenCalledTimes(1);
  });
});

describe("WelcomeCheckoutPage — Skip for now", () => {
  it("clears the stash and navigates to /sites without ever starting a checkout call from the click", async () => {
    const mutateMock = vi.fn();
    mockedUseCreateBillingCheckout.mockReturnValue(
      mockMutationResult<CheckoutResult, CreateCheckoutVariables>({ mutate: mutateMock }),
    );

    const router = renderWelcomeCheckout("/welcome/checkout?plan=agency");
    await screen.findByRole("heading", { name: "Complete your Agency subscription" });
    // Let the auto-start effect fire first (matches production timing) before skipping.
    await waitFor(() => expect(mutateMock).toHaveBeenCalledTimes(1));

    fireEvent.click(screen.getByRole("button", { name: "Skip for now" }));

    await waitFor(() => expect(router.state.location.pathname).toBe("/sites"));
    expect(readPendingPlan()).toBeNull();
    // Skip must never itself trigger an additional checkout-creation call.
    expect(mutateMock).toHaveBeenCalledTimes(1);
  });
});
