import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";
import { authKeys } from "@/features/auth/use-auth";
import type { Me } from "@wpmgr/api";

import { Route as AdminAccountsRoute } from "./index";
import {
  useAdminAccountsList,
  type AdminAccountsListResponse,
} from "@/features/admin/use-admin-accounts";

// Regression test for the Accounts pager bug: `patchSearch`'s literal
// `offset: 0` used to be spread AFTER `...patch`, so it clobbered the
// Next/Previous handlers' own computed `offset` on every single click — the
// pager was permanently stuck on page 1. Fixed by reordering to
// `{ ...prev, offset: 0, ...patch }` (routes/_authed/admin/accounts/index.tsx).
//
// This mounts the REAL route component (not a stand-in), because the bug
// lives in how the component's own `patchSearch` merges search params — a
// test that stubs navigation would test the stub, not the bug. The page
// reads/writes the URL via `Route.useSearch()` / `useNavigate({ from:
// Route.fullPath })`, both bound to the file's own exported `Route`
// singleton, so the test router below re-attaches that EXACT singleton to a
// throwaway root route (mirroring what the generated routeTree.gen.ts does
// at boot via `Route.update({ id, path, getParentRoute })` — see that file's
// `AuthedAdminAccountsIndexRoute` block) rather than rendering the component
// as an inert child of a generic wrapper route, which would leave the
// exported `Route` never `.init()`-ed and `Route.useSearch()` unable to
// resolve anything. See src/test/render.tsx's module doc for why a full app
// routeTree is deliberately not used elsewhere in this suite either.

vi.mock("@/features/admin/use-admin-accounts", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("@/features/admin/use-admin-accounts")>();
  return { ...actual, useAdminAccountsList: vi.fn() };
});

const mockedUseAdminAccountsList = vi.mocked(useAdminAccountsList);

// A large `total` relative to the default page size (25) lets both Next and
// Previous be exercised without needing a second fixture page. `items: []`
// is deliberate — this test proves the URL-search reducer, not row
// rendering, and empty rows sidestep needing a `/admin/accounts/$tenantId`
// child route registered on the throwaway test router (AccountRow links
// there).
const FIXTURE: AdminAccountsListResponse = {
  tiles: { mrr_cents: 590_000, active_subs: 80, past_due_count: 3, accounts_total: 100 },
  items: [],
  total: 100,
  limit: 25,
  offset: 0,
};

/**
 * Attaches the route file's actual exported `Route` singleton to a fresh,
 * isolated root route and returns a router mounted at `initialPath`. This is
 * the same post-hoc wiring `routeTree.gen.ts` performs for every file route
 * (`Route.update({ id, path, getParentRoute })`), just scoped to a throwaway
 * root instead of the real `_authed`/`admin` parent chain — so the `_authed`
 * session guard never runs, matching every other render test in this suite.
 *
 * `Route.update`'s public type only accepts already-declared updatable
 * fields (component/loader/etc), not the creation-time `id`/`path`/
 * `getParentRoute` triple the codegen assigns post hoc — the generated file
 * casts through `any` for this exact call. We route through `unknown`
 * instead so this test file's own `@typescript-eslint/no-explicit-any: error`
 * rule stays clean.
 */
function buildAccountsRouter(initialPath: string) {
  const rootRoute = createRootRoute({});
  type UpdateOptions = Parameters<typeof AdminAccountsRoute.update>[0];
  const accountsRoute = AdminAccountsRoute.update({
    id: "/admin/accounts",
    path: "/admin/accounts",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const routeTree = rootRoute.addChildren([accountsRoute]);
  return createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [initialPath] }),
  });
}

// A minimal but fully valid `Me` — every field the type requires is present,
// so no type assertion is needed to hand it to the query cache.
const ME_FIXTURE: Me = {
  user: {
    id: "00000000-0000-0000-0000-000000000099",
    email: "superadmin@wpmgr.test",
    name: "Superadmin",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    is_superadmin: true,
  },
  memberships: [],
  hosted: true,
};

function renderAccountsPage(initialPath = "/admin/accounts") {
  const queryClient = createTestQueryClient();
  // `useMe()` is a real TanStack Query hook the page calls for the
  // `hosted` banner gate only — pre-seed the cache instead of mocking the
  // whole auth module, so an accidental real `getMe()` network call never
  // fires in this jsdom environment.
  queryClient.setQueryData(authKeys.me, ME_FIXTURE);
  const router = buildAccountsRouter(initialPath);
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  return router;
}

beforeEach(() => {
  mockedUseAdminAccountsList.mockReturnValue(
    mockQueryResult<AdminAccountsListResponse>({ data: FIXTURE }),
  );
});

describe("Admin Accounts pager — patchSearch offset regression", () => {
  it("Next advances the URL search offset to `limit` and Previous returns it to 0 (non-vacuous: the pre-fix spread order always clobbers offset back to 0)", async () => {
    const router = renderAccountsPage();

    const nextButton = await screen.findByRole("button", { name: "Next" });
    expect(router.state.location.search.offset).toBeUndefined();

    fireEvent.click(nextButton);
    await waitFor(() => {
      expect(router.state.location.search.offset).toBe(25);
    });
    // Also visible in the rendered pager text, for good measure.
    expect(await screen.findByText("26-50 of 100")).toBeInTheDocument();

    const prevButton = screen.getByRole("button", { name: "Previous" });
    fireEvent.click(prevButton);
    await waitFor(() => {
      expect(router.state.location.search.offset).toBe(0);
    });
    expect(await screen.findByText("1-25 of 100")).toBeInTheDocument();
  });

  it("applying a filter resets the offset back to page 1 while keeping the filter", async () => {
    const router = renderAccountsPage();

    const nextButton = await screen.findByRole("button", { name: "Next" });
    fireEvent.click(nextButton);
    await waitFor(() => {
      expect(router.state.location.search.offset).toBe(25);
    });

    // "Past due" is a plain toggle chip (not a Radix popover), so it exercises
    // the exact same `patchSearch` path as the Status multi-select filter
    // without needing to drive a portal-rendered menu open in jsdom.
    const pastDueChip = screen.getByRole("button", { name: "Past due" });
    fireEvent.click(pastDueChip);

    await waitFor(() => {
      expect(router.state.location.search.offset).toBe(0);
    });
    expect(router.state.location.search.status).toEqual(["past_due"]);
  });
});
