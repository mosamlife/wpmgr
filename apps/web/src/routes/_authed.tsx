import { createFileRoute, Outlet, redirect } from "@tanstack/react-router";

import { AppShell } from "@/components/layout/app-shell";
import { getSession } from "@/lib/session-store";

// Protected pathless layout. Everything nested under `_authed/` requires the
// (stubbed) session; otherwise we bounce to /login. Real auth lands in Phase 5.
export const Route = createFileRoute("/_authed")({
  beforeLoad: () => {
    if (!getSession()) {
      throw redirect({ to: "/login" });
    }
  },
  component: AuthedLayout,
});

function AuthedLayout() {
  return (
    <AppShell>
      <Outlet />
    </AppShell>
  );
}
