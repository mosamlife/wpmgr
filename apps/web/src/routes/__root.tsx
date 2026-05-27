import { createRootRouteWithContext, Outlet } from "@tanstack/react-router";

import type { RouterContext } from "@/router";

// Root route. The actual chrome (app shell, sidebar, header) is rendered by the
// protected `_authed` layout so that /login stays chrome-free. The router
// context carries the QueryClient so guards/loaders can read server state
// (e.g. the auth session via GET /auth/me).
export const Route = createRootRouteWithContext<RouterContext>()({
  component: RootComponent,
});

function RootComponent() {
  return <Outlet />;
}
