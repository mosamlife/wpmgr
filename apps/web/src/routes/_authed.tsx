import { createFileRoute, Outlet, redirect } from "@tanstack/react-router";

import { AppShell } from "@/components/layout/app-shell";
import {
  ensureMe,
  useMe,
  hasOrg,
  isSuperadmin,
  isSuperadminAllowedPath,
} from "@/features/auth/use-auth";
import { BulkActionProvider } from "@/features/sites/bulk-action-drawer";
import { NoOrgScreen } from "@/features/orgs/no-org-screen";

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
    // Portal users (role==="client") never belong in the agency shell. Redirect
    // them to /portal BEFORE the NoOrgScreen check so they don't see onboarding.
    // This is the reverse gate: /portal/route.tsx handles the agency->portal
    // direction; this handles the portal->agency direction.
    if (me.role === "client") {
      throw redirect({ to: "/portal" });
    }
    // A superadmin who belongs to no organisation has nothing to manage, so
    // keep them inside the Admin area and out of the tenant-scoped shell
    // (which would 403 / bounce them to the create-org screen). They still reach
    // /admin and any future /admin/* routes, PLUS their own per-user account
    // settings (profile + 2FA/security) so they can secure their own login.
    // A superadmin who also belongs to an organisation is not held here: that
    // organisation's sites are theirs to open like any other member's.
    if (isSuperadmin(me) && !isSuperadminAllowedPath(me, location.pathname)) {
      throw redirect({ to: "/admin" });
    }
  },
  component: AuthedLayout,
});

/**
 * Returns true when the user has genuinely zero access: no org memberships
 * AND no active tenant. This is distinct from a site-scoped collaborator
 * (active_tenant_id is set but not present in memberships) — those users
 * still have the "Shared with me" area and must NOT see the onboarding screen.
 */
function useHasNoOrg(): boolean {
  const { data: me } = useMe();
  if (!me) return false;
  // A superadmin with no organisation is held in /admin by the guard above, so
  // never show them the create-organisation onboarding screen. One who belongs
  // to an organisation is not organisation-less either way.
  if (isSuperadmin(me)) return false;
  return !hasOrg(me);
}

function AuthedLayout() {
  const noOrg = useHasNoOrg();

  // BulkActionProvider wraps the entire AppShell so BOTH the TopBar's
  // notification bell and the route content read the same context. The
  // drawer host is rendered inside the provider and floats over every
  // surface via `position: fixed`. One provider per authenticated session
  // — the drawer host renders at most one drawer at a time.
  //
  // When the user has no org membership and no active tenant, render the
  // onboarding screen inside the shell so the top-bar (user menu, org
  // switcher) remains reachable. The <Outlet /> is suppressed in this state
  // — tenant-scoped routes would return 403 and look broken.
  return (
    <BulkActionProvider>
      <AppShell>
        {noOrg ? <NoOrgScreen /> : <Outlet />}
      </AppShell>
    </BulkActionProvider>
  );
}
