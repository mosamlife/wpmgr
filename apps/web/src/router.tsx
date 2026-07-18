import { createRouter } from "@tanstack/react-router";
import type { QueryClient } from "@tanstack/react-query";

import { routeTree } from "./routeTree.gen";
import { queryClient } from "@/lib/query-client";
import { NotFoundPage } from "@/components/layout/not-found-page";

// Router context shape. The QueryClient is threaded through so route guards
// (`beforeLoad`) and loaders can read/seed server state — notably the auth
// session via `GET /auth/me`.
export interface RouterContext {
  queryClient: QueryClient;
}

// Central router instance built from the generated route tree.
export const router = createRouter({
  routeTree,
  defaultPreload: "intent",
  scrollRestoration: true,
  // GH #243 — branded 404 for any unmatched path (stale/mistyped deep link,
  // a removed route) instead of TanStack Router's bare default fallback.
  // notFoundMode "root" is load-bearing: NotFoundPage mounts its own AppShell,
  // and the default "fuzzy" mode would render it INSIDE the deepest matched
  // layout (nested shell + duplicate command palette) on partially-matched
  // dead paths like /sites/{id}/{removed-tab}.
  defaultNotFoundComponent: NotFoundPage,
  notFoundMode: "root",
  context: { queryClient } satisfies RouterContext,
});

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
