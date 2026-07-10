import { describe, it, expect } from "vitest";
import { waitFor } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRouteWithContext,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import type { RouterContext } from "@/router";

import { Route as BillingShimRoute } from "./billing";

// Compatibility redirect regression lock: POST /billing/checkout's Stripe
// success/cancel URLs are server-fixed to `{publicBaseURL}/billing?checkout=...`
// (apps/api/internal/billing/handler.go), one path segment shorter than this
// app's real route (`/settings/billing`) — this route exists purely to catch
// that return trip and forward it on. See routes/_authed/billing.tsx's
// module doc.

function buildShimRouter(initialPath: string) {
  const rootRoute = createRootRouteWithContext<RouterContext>()({});
  type UpdateOptions = Parameters<typeof BillingShimRoute.update>[0];
  const billingShimRoute = BillingShimRoute.update({
    id: "/billing",
    path: "/billing",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const settingsBillingRoute = createRoute({
    path: "/settings/billing",
    getParentRoute: () => rootRoute,
    component: () => <div>Settings billing stub</div>,
  });
  const routeTree = rootRoute.addChildren([billingShimRoute, settingsBillingRoute]);
  return createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [initialPath] }),
    context: { queryClient: createTestQueryClient() },
  });
}

function renderShim(initialPath: string) {
  const router = buildShimRouter(initialPath);
  renderWithProviders(<RouterProvider router={router} />);
  return router;
}

describe("/_authed/billing compatibility redirect", () => {
  it("forwards /billing?checkout=success to /settings/billing?checkout=success", async () => {
    const router = renderShim("/billing?checkout=success");

    await waitFor(() => expect(router.state.location.pathname).toBe("/settings/billing"));
    expect(router.state.location.search).toMatchObject({ checkout: "success" });
  });

  it("forwards /billing?checkout=cancel to /settings/billing?checkout=cancel", async () => {
    const router = renderShim("/billing?checkout=cancel");

    await waitFor(() => expect(router.state.location.pathname).toBe("/settings/billing"));
    expect(router.state.location.search).toMatchObject({ checkout: "cancel" });
  });

  it("forwards a bare /billing (no checkout param) to /settings/billing with no stray param", async () => {
    const router = renderShim("/billing");

    await waitFor(() => expect(router.state.location.pathname).toBe("/settings/billing"));
    expect(router.state.location.search).not.toHaveProperty("checkout");
  });
});
