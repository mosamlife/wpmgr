import { createRouter } from "@tanstack/react-router";

import { routeTree } from "./routeTree.gen";

// Central router instance built from the generated route tree.
export const router = createRouter({
  routeTree,
  defaultPreload: "intent",
  scrollRestoration: true,
});

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
