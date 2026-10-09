import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRouteWithContext,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
} from "@tanstack/react-router";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";
import { authKeys } from "@/features/auth/use-auth";
import { useSites } from "@/features/sites/use-sites";
import { Sidebar } from "@/components/layout/sidebar";
import { ShellContext, type ShellState } from "@/components/layout/app-shell-context";
import type { RouterContext } from "@/router";
import type { Me, Site } from "@wpmgr/api";

import { Route as AuthedRoute } from "../_authed";
import { Route as AdminRoute } from "./admin/route";

// GH #434. A SUPERADMIN WHO ALSO BELONGS TO AN ORGANISATION MUST BE ABLE TO USE
// THAT ORGANISATION.
//
// The reporter made their account a superadmin (self-hosters have to, to set the
// vulnerability feed key) and then could not open their own sites: every page
// outside the admin area sent them back to /admin, and the "Back to Sites"
// button in the admin sidebar pointed at /sites, so it looped.
//
// What runs here is the REAL pathless `_authed` layout guard (its own
// beforeLoad), the REAL `/admin` layout gate (its own beforeLoad), the REAL
// Sidebar, the real router on an in-memory history and a real QueryClient whose
// GET /auth/me answer is seeded. Nothing is mocked except the Sites list the
// sidebar badge reads. Only the page bodies are stubs, because what is under
// test is where the guard sends someone and what the navigation offers them,
// not what the Sites or Admin pages draw.
//
// A test titled "pin:" passes on main as well as with the fix: it holds
// behaviour that must not change. Every other test FAILS on main, and the
// titles say which half of the bug each one is: the guard that sends the
// account to /admin, or the sidebar link that leads back to it.

vi.mock("@/features/sites/use-sites", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/sites/use-sites")>();
  return { ...actual, useSites: vi.fn() };
});

const mockedSites = vi.mocked(useSites);

const TENANT_ID = "00000000-0000-0000-0000-0000000000aa";
const USER_ID = "00000000-0000-0000-0000-000000000001";

const SHELL: ShellState = {
  collapsed: false,
  toggleCollapsed: () => {},
  mobileOpen: false,
  setMobileOpen: () => {},
};

function buildMe(overrides: {
  superadmin: boolean;
  memberships: boolean;
  activeTenant: boolean;
}): Me {
  return {
    user: {
      id: USER_ID,
      email: "operator@wpmgr.test",
      name: "Operator",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
      is_superadmin: overrides.superadmin,
    },
    memberships: overrides.memberships
      ? [{ user_id: USER_ID, tenant_id: TENANT_ID, role: "owner" }]
      : [],
    ...(overrides.activeTenant ? { active_tenant_id: TENANT_ID } : {}),
  };
}

// A superadmin who owns an organisation: the reporter's account.
const SUPERADMIN_WITH_ORG = buildMe({ superadmin: true, memberships: true, activeTenant: true });
// A superadmin with no organisation at all: the account the redirect was written for.
const SUPERADMIN_NO_ORG = buildMe({ superadmin: true, memberships: false, activeTenant: false });
// A superadmin who reaches an organisation only through a share: no membership
// row, but the server reports that organisation as active.
const SUPERADMIN_ACTIVE_ONLY = buildMe({ superadmin: true, memberships: false, activeTenant: true });
// An ordinary member.
const MEMBER = buildMe({ superadmin: false, memberships: true, activeTenant: true });

type AuthedUpdate = Parameters<typeof AuthedRoute.update>[0];
type AdminUpdate = Parameters<typeof AdminRoute.update>[0];

function ShellWithSidebar() {
  return (
    <ShellContext.Provider value={SHELL}>
      <Sidebar />
      <main>
        <Outlet />
      </main>
    </ShellContext.Provider>
  );
}

function mount(initialPath: string, me: Me) {
  const queryClient = createTestQueryClient();
  // Seeded, so the guard (ensureMe) and the sidebar (useMe) read the same
  // answer from the same cache entry, as they do in the app.
  queryClient.setQueryData(authKeys.me, me);

  const rootRoute = createRootRouteWithContext<RouterContext>()({});
  const authedRoute = AuthedRoute.update({
    id: "/_authed",
    getParentRoute: () => rootRoute,
    component: ShellWithSidebar,
  } as unknown as AuthedUpdate);
  const adminRoute = AdminRoute.update({
    id: "/admin",
    path: "/admin",
    getParentRoute: () => authedRoute,
    component: Outlet,
  } as unknown as AdminUpdate);
  const adminIndexRoute = createRoute({
    path: "/",
    getParentRoute: () => adminRoute,
    component: () => <h1>Admin console page</h1>,
  });
  const stub = (path: string, heading: string) =>
    createRoute({
      path,
      getParentRoute: () => authedRoute,
      component: () => <h1>{heading}</h1>,
    });

  const router = createRouter({
    routeTree: rootRoute.addChildren([
      authedRoute.addChildren([
        adminRoute.addChildren([adminIndexRoute]),
        stub("/sites", "Sites page"),
        stub("/settings/security", "Security page"),
        stub("/settings/organization", "Organisation page"),
      ]),
      createRoute({
        path: "/login",
        getParentRoute: () => rootRoute,
        component: () => <h1>Sign in page</h1>,
      }),
    ]),
    history: createMemoryHistory({ initialEntries: [initialPath] }),
    context: { queryClient },
  });
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  return router;
}

const primaryNav = () => screen.findByRole("navigation", { name: /primary/i });
const link = (name: string) => screen.queryByRole("link", { name });

beforeEach(() => {
  vi.clearAllMocks();
  mockedSites.mockReturnValue(mockQueryResult<Site[]>({ data: [] }));
});

describe("a superadmin who belongs to an organisation", () => {
  it("opens that organisation's Sites page instead of being sent to /admin", async () => {
    const router = mount("/sites", SUPERADMIN_WITH_ORG);

    await screen.findByRole("heading", { name: "Sites page" });
    expect(router.state.location.pathname).toBe("/sites");
  });

  it("is not pinned to /admin when the organisation reaches them only as the active one", async () => {
    const router = mount("/sites", SUPERADMIN_ACTIVE_ONLY);

    await screen.findByRole("heading", { name: "Sites page" });
    expect(router.state.location.pathname).toBe("/sites");
  });

  it("is offered 'Back to Sites' in the admin console, and it lands on /sites", async () => {
    const router = mount("/admin", SUPERADMIN_WITH_ORG);

    await screen.findByRole("heading", { name: "Admin console page" });
    const back = await screen.findByRole("link", { name: "Back to Sites" });
    expect(back).toHaveAttribute("href", "/sites");

    fireEvent.click(back);

    await screen.findByRole("heading", { name: "Sites page" });
    expect(router.state.location.pathname).toBe("/sites");
  });

  it("gets the ordinary navigation plus an 'Admin console' entry outside the admin area", async () => {
    mount("/sites", SUPERADMIN_WITH_ORG);

    await screen.findByRole("heading", { name: "Sites page" });
    await primaryNav();
    // The tenant navigation is what is on screen...
    expect(link("Settings")).toBeInTheDocument();
    expect(link("Accounts")).not.toBeInTheDocument();
    // ...with one way into the admin console, which goes to /admin.
    expect(link("Admin console")).toHaveAttribute("href", "/admin");
    // And nothing points back at the page they are already on.
    expect(link("Back to Sites")).not.toBeInTheDocument();
  });

  it("pin: gets the admin navigation, not the tenant navigation, inside the admin area", async () => {
    mount("/admin", SUPERADMIN_WITH_ORG);

    await screen.findByRole("heading", { name: "Admin console page" });
    await primaryNav();
    expect(link("Accounts")).toHaveAttribute("href", "/admin/accounts");
    expect(link("Settings")).not.toBeInTheDocument();
    expect(link("Admin console")).not.toBeInTheDocument();
  });

  it("can go to the admin console from the Sites page and come back", async () => {
    const router = mount("/sites", SUPERADMIN_WITH_ORG);

    await screen.findByRole("heading", { name: "Sites page" });
    fireEvent.click(await screen.findByRole("link", { name: "Admin console" }));
    await screen.findByRole("heading", { name: "Admin console page" });
    expect(router.state.location.pathname).toBe("/admin");

    fireEvent.click(await screen.findByRole("link", { name: "Back to Sites" }));
    await screen.findByRole("heading", { name: "Sites page" });
    expect(router.state.location.pathname).toBe("/sites");
  });
});

describe("a superadmin who belongs to no organisation", () => {
  it("pin: is still held in the admin area when they open /sites", async () => {
    const router = mount("/sites", SUPERADMIN_NO_ORG);

    await screen.findByRole("heading", { name: "Admin console page" });
    expect(router.state.location.pathname).toBe("/admin");
  });

  it("is not shown a 'Back to Sites' link, because it could only lead back to /admin", async () => {
    mount("/admin", SUPERADMIN_NO_ORG);

    await screen.findByRole("heading", { name: "Admin console page" });
    await primaryNav();
    // The rest of the admin navigation is there, so this is not an empty nav.
    expect(link("Accounts")).toHaveAttribute("href", "/admin/accounts");
    expect(link("Back to Sites")).not.toBeInTheDocument();
    expect(link("Admin console")).not.toBeInTheDocument();
  });

  it("pin: keeps their own security settings reachable", async () => {
    const router = mount("/settings/security", SUPERADMIN_NO_ORG);

    await screen.findByRole("heading", { name: "Security page" });
    expect(router.state.location.pathname).toBe("/settings/security");
  });

  it("pin: still keeps them out of organisation pages", async () => {
    const router = mount("/settings/organization", SUPERADMIN_NO_ORG);

    await screen.findByRole("heading", { name: "Admin console page" });
    expect(router.state.location.pathname).toBe("/admin");
  });
});

describe("a user who is not a superadmin", () => {
  it("pin: opens the Sites page and is offered no admin console", async () => {
    const router = mount("/sites", MEMBER);

    await screen.findByRole("heading", { name: "Sites page" });
    await primaryNav();
    expect(router.state.location.pathname).toBe("/sites");
    expect(link("Settings")).toBeInTheDocument();
    expect(link("Admin console")).not.toBeInTheDocument();
  });

  it("pin: is sent from /admin to /sites by the admin area's own gate", async () => {
    const router = mount("/admin", MEMBER);

    await screen.findByRole("heading", { name: "Sites page" });
    expect(router.state.location.pathname).toBe("/sites");
  });
});
