import { describe, it, expect, vi } from "vitest";
import { screen } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import type { Me } from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";

import { Route as ContextRoute } from "./$siteId.context";

// Greptile P1 ("Operator writes are hidden"): `canWrite` on this route was
// computed with `canManage` (owner/admin only), which hid the site-context
// editor and history from an operator-level org member or an operator-role
// site collaborator — both of whom ADR-064 Decision 6's `context.site.write`
// actually permits ("site-scope write additionally requires access to that
// specific site", not "requires admin"). Mirrors
// `-api-keys.role-ceiling.test.tsx`'s pattern exactly: mount the route's own
// exported `Route` on a throwaway one-segment tree (skips the real
// `_authed` guard) with a real `useQuery`-backed `useMe` stand-in, and prove
// the WIRING, not just the permission helper in isolation — a version that
// swapped back to `canManage` would leave every unit test on `canOperate`
// itself green while still shipping this exact bug.

const ME_KEY = ["test", "me"] as const;

vi.mock("@/features/auth/use-auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/auth/use-auth")>();
  const { useQuery } = await import("@tanstack/react-query");
  return {
    ...actual,
    useMe: () => useQuery({ queryKey: ME_KEY, queryFn: () => null, staleTime: Infinity }),
  };
});

vi.mock("@/features/context/effective-context-preview", () => ({
  EffectiveContextPreview: () => null,
}));

vi.mock("@/features/context/site-context-section", () => ({
  SiteContextSection: ({ canWrite }: { canWrite: boolean }) => (
    <div data-testid="site-context-section">canWrite={String(canWrite)}</div>
  ),
}));

vi.mock("@/features/context/site-context-history-section", () => ({
  SiteContextHistorySection: ({ canWrite }: { canWrite: boolean }) => (
    <div data-testid="site-context-history-section">canWrite={String(canWrite)}</div>
  ),
}));

const TENANT = "00000000-0000-0000-0000-0000000000aa";

function meWithRole(role: "owner" | "admin" | "operator" | "viewer"): Me {
  return {
    user: {
      id: "00000000-0000-0000-0000-000000000001",
      email: "viewer@wpmgr.test",
      name: "Viewer",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    },
    active_tenant_id: TENANT,
    memberships: [{ tenant_id: TENANT, role, tenant_name: "Acme" }],
  } as unknown as Me;
}

/** Same recipe as `-api-keys.role-ceiling.test.tsx`'s `buildApiKeysRouter`. */
function buildContextRouter() {
  const rootRoute = createRootRoute({});
  type UpdateOptions = Parameters<typeof ContextRoute.update>[0];
  const contextRoute = ContextRoute.update({
    id: "/sites/$siteId/context",
    path: "/sites/$siteId/context",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const routeTree = rootRoute.addChildren([contextRoute]);
  return createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: ["/sites/site-1/context"] }),
  });
}

async function renderAs(role: "owner" | "admin" | "operator" | "viewer") {
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(ME_KEY, meWithRole(role));
  const router = buildContextRouter();
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  // RouterProvider's first paint is a microtask.
  return screen.findByTestId("site-context-section");
}

// A genuine site-scoped principal: no org membership at all (site access via
// a share, not an org role) — the actual principal the canWriteSiteContext
// fix exists for, per its own Me.scope/Me.role check.
function meSiteScoped(role: Me["role"]): Me {
  return { scope: "site", role, memberships: [] } as unknown as Me;
}

async function renderAsSiteScoped(role: Me["role"]) {
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(ME_KEY, meSiteScoped(role));
  const router = buildContextRouter();
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  return screen.findByTestId("site-context-section");
}

describe("site context tab — canWrite follows Decision 6, not the org-admin ceiling", () => {
  it("an OPERATOR (not owner/admin) sees the editor and history as writable", async () => {
    const section = await renderAs("operator");
    expect(section).toHaveTextContent("canWrite=true");
    expect(screen.getByTestId("site-context-history-section")).toHaveTextContent(
      "canWrite=true",
    );
  });

  // CodeRabbit finding on #566: these two used to share one test, mounting a
  // second tree without unmounting the first — `screen.findByTestId` then
  // has two matching elements in the document at once, which is undefined
  // behaviour for a `getBy*`-style query even on a run where it happens not
  // to throw. Split into separate tests so each gets its own render/cleanup
  // cycle, same as every other case in this file.
  it("an OWNER remains writable (must not regress)", async () => {
    expect(await renderAs("owner")).toHaveTextContent("canWrite=true");
  });

  it("an ADMIN remains writable (must not regress)", async () => {
    expect(await renderAs("admin")).toHaveTextContent("canWrite=true");
  });

  it("a VIEWER is still read-only", async () => {
    const section = await renderAs("viewer");
    expect(section).toHaveTextContent("canWrite=false");
  });

  // Security review finding (F3): every case above is an org-scoped
  // membership role. Nothing rendered this route as a genuine site-scoped
  // collaborator — the very principal the canWriteSiteContext fix exists
  // for — until now.
  it("a genuine SITE-SCOPED collaborator (no org membership) sees the editor as writable", async () => {
    const section = await renderAsSiteScoped("operator");
    expect(section).toHaveTextContent("canWrite=true");
  });

  // Security review finding (F3): role:"client" is a real portal-principal
  // value under scope==="site" (types.gen.ts:939) that reaches this route,
  // not a hypothetical. It resolves to read-only today; this holds it there.
  it("a portal (client) principal under scope==='site' stays read-only", async () => {
    const section = await renderAsSiteScoped("client");
    expect(section).toHaveTextContent("canWrite=false");
  });
});
