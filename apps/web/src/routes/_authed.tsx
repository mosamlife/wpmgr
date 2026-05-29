import { createFileRoute, Outlet, redirect } from "@tanstack/react-router";

import { AppShell } from "@/components/layout/app-shell";
import { ensureMe } from "@/features/auth/use-auth";
import { BulkActionProvider } from "@/features/sites/bulk-action-drawer";

// Protected pathless layout. Everything nested under `_authed/` requires a real
// session: the guard calls `GET /auth/me` (via the cached `ensureMe`) and, when
// unauthenticated (the query returns null on a 401), redirects to /login while
// remembering where the user was headed.
export const Route = createFileRoute("/_authed")({
  beforeLoad: async ({ context, location }) => {
    const me = await ensureMe(context.queryClient);
    if (!me) {
      throw redirect({
        to: "/login",
        search: { redirect: location.href },
      });
    }
  },
  component: AuthedLayout,
});

function AuthedLayout() {
  // BulkActionProvider wraps the entire AppShell so BOTH the TopBar's
  // notification bell and the route content read the same context. The
  // drawer host is rendered inside the provider and floats over every
  // surface via `position: fixed`. One provider per authenticated session
  // — the drawer host renders at most one drawer at a time.
  return (
    <BulkActionProvider>
      <AppShell>
        <Outlet />
      </AppShell>
    </BulkActionProvider>
  );
}
