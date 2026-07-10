import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRouteWithContext,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import type { UseMutationResult } from "@tanstack/react-query";
import type { Me } from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { mockMutationResult } from "@/test/query-mocks";
import type { RouterContext } from "@/router";

import { Route as VerifyEmailRoute } from "./verify-email";
import {
  useVerifyEmail,
  useResendVerification,
  type VerifyEmailResult,
} from "@/features/auth/use-auth";
import { stashPendingPlan, clearPendingPlan } from "@/features/billing/pending-plan";

// M16 Phase C2 — signup-to-premium, the /verify-email half: on a 200
// response carrying `me.desired_plan` on a hosted instance, skip straight to
// /welcome/checkout instead of an empty Sites page. Mounts the REAL route
// component re-attached to a throwaway root (same pattern as
// routes/-register.test.tsx / routes/_authed/settings/-billing.test.tsx).

vi.mock("@/features/auth/use-auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/auth/use-auth")>();
  return { ...actual, useVerifyEmail: vi.fn(), useResendVerification: vi.fn() };
});

const mockedUseVerifyEmail = vi.mocked(useVerifyEmail);
const mockedUseResendVerification = vi.mocked(useResendVerification);

const MINIMAL_ME: Me = {
  user: {
    id: "00000000-0000-0000-0000-000000000001",
    email: "new@wpmgr.test",
    name: "New Owner",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  },
  memberships: [
    {
      user_id: "00000000-0000-0000-0000-000000000001",
      tenant_id: "11111111-1111-1111-1111-111111111111",
      role: "owner",
    },
  ],
  active_tenant_id: "11111111-1111-1111-1111-111111111111",
};

function buildVerifyEmailRouter(
  initialPath: string,
  queryClient: ReturnType<typeof createTestQueryClient>,
) {
  const rootRoute = createRootRouteWithContext<RouterContext>()({});
  type UpdateOptions = Parameters<typeof VerifyEmailRoute.update>[0];
  const verifyEmailRoute = VerifyEmailRoute.update({
    id: "/verify-email",
    path: "/verify-email",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const sitesRoute = createRoute({
    path: "/sites",
    getParentRoute: () => rootRoute,
    component: () => <div>Sites stub</div>,
  });
  const welcomeCheckoutRoute = createRoute({
    path: "/welcome/checkout",
    getParentRoute: () => rootRoute,
    validateSearch: (search: Record<string, unknown>) => search,
    component: () => <div>Welcome checkout stub</div>,
  });
  const routeTree = rootRoute.addChildren([
    verifyEmailRoute,
    sitesRoute,
    welcomeCheckoutRoute,
  ]);
  return createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [initialPath] }),
    context: { queryClient },
  });
}

function renderVerifyEmailPage(initialPath: string) {
  const queryClient = createTestQueryClient();
  const router = buildVerifyEmailRouter(initialPath, queryClient);
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  return router;
}

/** A `mutate` spy that synchronously fires the caller's `onSuccess` with `result`. */
function mutateFiringOnSuccess(
  result: VerifyEmailResult,
): UseMutationResult<VerifyEmailResult, Error, { token: string }>["mutate"] {
  return vi.fn(
    (_vars: { token: string }, opts?: { onSuccess?: (result: VerifyEmailResult) => void }) => {
      opts?.onSuccess?.(result);
    },
  ) as UseMutationResult<VerifyEmailResult, Error, { token: string }>["mutate"];
}

beforeEach(() => {
  vi.clearAllMocks();
  clearPendingPlan();
  mockedUseResendVerification.mockReturnValue(
    mockMutationResult<void, { email: string }>({}),
  );
});

describe("VerifyEmailPage — desired_plan routing on a 200 verify", () => {
  it("navigates to /welcome/checkout?plan=agency when desired_plan is present and the instance is hosted", async () => {
    mockedUseVerifyEmail.mockReturnValue(
      mockMutationResult<VerifyEmailResult, { token: string }>({
        mutate: mutateFiringOnSuccess({
          status: 200,
          me: { ...MINIMAL_ME, hosted: true, desired_plan: "agency" },
        }),
      }),
    );

    const router = renderVerifyEmailPage("/verify-email?token=abc123");

    await waitFor(() => expect(router.state.location.pathname).toBe("/welcome/checkout"));
    expect(router.state.location.search).toMatchObject({ plan: "agency" });
  });

  it("navigates to /sites (unregressed) when there is no desired_plan", async () => {
    mockedUseVerifyEmail.mockReturnValue(
      mockMutationResult<VerifyEmailResult, { token: string }>({
        mutate: mutateFiringOnSuccess({
          status: 200,
          me: { ...MINIMAL_ME, hosted: true },
        }),
      }),
    );

    const router = renderVerifyEmailPage("/verify-email?token=abc123");

    await waitFor(() => expect(router.state.location.pathname).toBe("/sites"));
  });

  it("navigates to /sites when desired_plan is present but the instance is not hosted (self-host safety)", async () => {
    mockedUseVerifyEmail.mockReturnValue(
      mockMutationResult<VerifyEmailResult, { token: string }>({
        mutate: mutateFiringOnSuccess({
          status: 200,
          me: { ...MINIMAL_ME, hosted: false, desired_plan: "agency" },
        }),
      }),
    );

    const router = renderVerifyEmailPage("/verify-email?token=abc123");

    await waitFor(() => expect(router.state.location.pathname).toBe("/sites"));
  });

  it("carries a stashed `?currency=` hint through to /welcome/checkout", async () => {
    stashPendingPlan({ plan: "agency", currency: "INR" });
    mockedUseVerifyEmail.mockReturnValue(
      mockMutationResult<VerifyEmailResult, { token: string }>({
        mutate: mutateFiringOnSuccess({
          status: 200,
          me: { ...MINIMAL_ME, hosted: true, desired_plan: "agency" },
        }),
      }),
    );

    const router = renderVerifyEmailPage("/verify-email?token=abc123");

    await waitFor(() => expect(router.state.location.pathname).toBe("/welcome/checkout"));
    expect(router.state.location.search).toMatchObject({ plan: "agency", currency: "INR" });
  });

  it("still shows the verifying state before the mutation settles (unregressed)", async () => {
    mockedUseVerifyEmail.mockReturnValue(
      mockMutationResult<VerifyEmailResult, { token: string }>({
        mutate: vi.fn(),
        isPending: true,
        isSuccess: false,
        isError: false,
      }),
    );

    renderVerifyEmailPage("/verify-email?token=abc123");

    expect(await screen.findByText("Verifying your email")).toBeInTheDocument();
  });
});
