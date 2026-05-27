import { createRouter } from "@tanstack/react-router";
import type { QueryClient } from "@tanstack/react-query";

import { routeTree } from "./routeTree.gen";
import { queryClient } from "@/lib/query-client";

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
  context: { queryClient } satisfies RouterContext,
});

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
