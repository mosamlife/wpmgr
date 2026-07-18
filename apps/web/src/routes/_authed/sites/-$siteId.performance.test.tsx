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

import { Route as PerformanceRedirectRoute } from "./$siteId.performance";

// GH #243 — `/sites/$siteId/performance` (the old tab name) must forward to
// the Cache tab instead of 404ing. Mounts the REAL route's `beforeLoad`
// re-attached to a throwaway root (mirrors
// routes/_authed/-welcome.checkout.test.tsx's rationale — `.update()` strips
// the `_authed` pathless-layout prefix so the guard doesn't need mocking),
// plus a `/sites/$siteId/cache` stub so the redirect target is directly
// observable via `router.state.location`.

const SITE_ID = "22222222-2222-2222-2222-222222222222";

function buildRedirectRouter(
  initialPath: string,
  queryClient: ReturnType<typeof createTestQueryClient>,
) {
  const rootRoute = createRootRouteWithContext<RouterContext>()({});
  type UpdateOptions = Parameters<typeof PerformanceRedirectRoute.update>[0];
  const performanceRoute = PerformanceRedirectRoute.update({
    id: "/sites/$siteId/performance",
    path: "/sites/$siteId/performance",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const cacheRoute = createRoute({
    path: "/sites/$siteId/cache",
    getParentRoute: () => rootRoute,
    component: () => <div>Cache tab stub</div>,
  });
  const routeTree = rootRoute.addChildren([performanceRoute, cacheRoute]);
  return createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [initialPath] }),
    context: { queryClient },
  });
}

describe("/sites/$siteId/performance redirect (GH #243)", () => {
  it("redirects to /sites/$siteId/cache for the same site, replacing history", async () => {
    const queryClient = createTestQueryClient();
    const router = buildRedirectRouter(`/sites/${SITE_ID}/performance`, queryClient);

    renderWithProviders(<RouterProvider router={router} />, { queryClient });

    await waitFor(() =>
      expect(router.state.location.pathname).toBe(`/sites/${SITE_ID}/cache`),
    );
    // beforeLoad redirects with replace:true — the old URL must not remain
    // reachable via back-navigation as a live history entry.
    expect(router.history.location.pathname).toBe(`/sites/${SITE_ID}/cache`);
  });

  it("preserves the siteId param exactly (does not redirect to a different site)", async () => {
    const otherSiteId = "33333333-3333-3333-3333-333333333333";
    const queryClient = createTestQueryClient();
    const router = buildRedirectRouter(`/sites/${otherSiteId}/performance`, queryClient);

    renderWithProviders(<RouterProvider router={router} />, { queryClient });

    await waitFor(() =>
      expect(router.state.location.pathname).toBe(`/sites/${otherSiteId}/cache`),
    );
  });
});
