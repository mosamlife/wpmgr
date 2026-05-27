import { createRootRoute, Outlet } from "@tanstack/react-router";

// Root route. The actual chrome (app shell, sidebar, header) is rendered by the
// protected `_authed` layout so that /login stays chrome-free.
export const Route = createRootRoute({
  component: RootComponent,
});

function RootComponent() {
  return <Outlet />;
}
