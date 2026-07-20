import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import type { QueryClient } from "@tanstack/react-query";
import type { Me, Site } from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";
import { authKeys } from "@/features/auth/use-auth";

import { Route as SitesIndexRoute } from "./index";
import { useSites } from "@/features/sites/use-sites";
import { useClients } from "@/features/clients/use-clients";
import { useTags } from "@/features/tags/use-tags";
import { useSitesLiveSync } from "@/features/sites/use-sites-live";
import type { UseSitesOptions } from "@/features/sites/use-sites";

// GH #252: the Sites list page rendered TWO visible "Add site" triggers for
// operators (the PageHeader action AND the toolbar's addSiteSlot). Fixed by
// making the PageHeader the page's single primary-action location (matching
// every other list page's `actions=` primary-action convention, e.g.
// `routes/_authed/settings/tags.tsx`'s "New tag" button) and suppressing the
// toolbar's own trigger. The truly-empty (post-onboarding, zero-sites)
// state had the SAME defect one layer down: NoSitesEmpty defaults its own
// `cta` to another <AddSiteDialog />, so that state is covered here too.
//
// Mounts the REAL route component with the file's own exported `Route`
// singleton re-attached to a throwaway root (mirrors
// routes/_authed/admin/accounts/-index.test.tsx) because the page reads the
// URL via `Route.useSearch()` / `useNavigate({ from: Route.fullPath })`,
// both bound to that exact singleton.

vi.mock("@/features/sites/use-sites", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/sites/use-sites")>();
  return { ...actual, useSites: vi.fn() };
});
vi.mock("@/features/clients/use-clients", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/clients/use-clients")>();
  return { ...actual, useClients: vi.fn() };
});
vi.mock("@/features/tags/use-tags", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/tags/use-tags")>();
  return { ...actual, useTags: vi.fn() };
});
vi.mock("@/features/sites/use-sites-live", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/sites/use-sites-live")>();
  return { ...actual, useSitesLiveSync: vi.fn() };
});

const mockedUseSites = vi.mocked(useSites);
const mockedUseClients = vi.mocked(useClients);
const mockedUseTags = vi.mocked(useTags);
const mockedUseSitesLiveSync = vi.mocked(useSitesLiveSync);

const OWNER_ME: Me = {
  user: {
    id: "00000000-0000-0000-0000-0000000000u1",
    email: "owner@example.com",
    name: "Owner",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  },
  memberships: [{ user_id: "00000000-0000-0000-0000-0000000000u1", tenant_id: "t1", role: "owner" }],
  active_tenant_id: "t1",
  hosted: false,
};

function buildSite(overrides: Partial<Site> = {}): Site {
  return {
    id: "11111111-0000-0000-0000-000000000001",
    tenant_id: "t1",
    url: "https://acme.example.com",
    name: "Acme",
    status: "active",
    wp_version: "6.8",
    php_version: "8.3",
    health_status: "healthy",
    multisite: false,
    tags: [],
    ...overrides,
  } as unknown as Site;
}

/** Re-attaches the route file's real `Route` singleton to a throwaway root,
 *  same technique as routes/_authed/admin/accounts/-index.test.tsx. The
 *  route's `loader` reads `context.queryClient` (to prefetch the default
 *  sites list) exactly like the real app router (src/router.tsx), so the
 *  test router needs the same `context: { queryClient }` wiring or the
 *  loader throws on `undefined.prefetchQuery`. */
function buildSitesRouter(initialPath: string, queryClient: QueryClient) {
  const rootRoute = createRootRoute({});
  type UpdateOptions = Parameters<typeof SitesIndexRoute.update>[0];
  const sitesRoute = SitesIndexRoute.update({
    id: "/sites",
    path: "/sites",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const routeTree = rootRoute.addChildren([sitesRoute]);
  return createRouter({
    routeTree,
    context: { queryClient },
    history: createMemoryHistory({ initialEntries: [initialPath] }),
  });
}

function renderSitesPage(initialPath = "/sites") {
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(authKeys.me, OWNER_ME);
  const router = buildSitesRouter(initialPath, queryClient);
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  return router;
}

beforeEach(() => {
  mockedUseClients.mockReturnValue(mockQueryResult({ data: [] }));
  mockedUseTags.mockReturnValue(mockQueryResult({ data: [] }));
  mockedUseSitesLiveSync.mockReturnValue(undefined);
});

afterEach(() => {
  window.localStorage.clear();
});

// Both tests below wait on the actual "Add site" button(s) as the PRIMARY
// (and only) async anchor, with a generous explicit timeout rather than
// testing-library's 1000ms default. This closes a real flake: RouterProvider's
// first paint is an async microtask (see src/test/render.tsx's module doc),
// and on a busy/contended CI runner that paint can land after the default
// findBy* timeout, well before any application-level slowness. `findByRole`
// on `role="toolbar"` (or, in the empty-state test, the onboarding heading)
// raced that SAME async paint with the SAME default timeout and intermittently
// lost. Reproduced locally by running this file under CPU contention (many
// parallel vitest + CPU-bound processes), which deterministically reproduced
// "Unable to find role=\"toolbar\" and name \"Filter sites\"" (and the
// analogous heading miss in the second test) even though the component logic
// itself is 100% synchronous once mounted (the mocked `useSites` never
// transitions through a pending state, so there is nothing async to wait on
// BELOW the first paint).
//
// `findAllByRole` for the button is deliberately the wait target instead of
// `findByRole`: unlike the singular query, it doesn't throw on >1 match, so a
// REGRESSION of the GH #252 bug (both triggers rendering again) still fails
// the length assertion below with a clear message, rather than failing early
// inside the wait with a less informative "found multiple elements" error.
//
// Once the button wait resolves, the toolbar/heading check that follows is a
// SYNCHRONOUS assertion (no waiting): by the time the button is in the DOM,
// SitesPage has already committed its single render pass, so the toolbar (or
// the onboarding heading) is guaranteed to already be present too. This keeps
// the test's real invariant intact: it still genuinely proves the "sites
// loaded" (or "truly empty") branch rendered, not just that any element
// showed up trivially fast.
const FIND_TIMEOUT = 5000;

describe("Sites page: exactly one 'Add site' trigger for an operator (GH #252)", () => {
  it("list view with sites loaded renders exactly one Add site button (the PageHeader's)", async () => {
    mockedUseSites.mockImplementation((options?: UseSitesOptions) =>
      mockQueryResult<Site[]>({ data: options?.view === "archived" ? [] : [buildSite()] }),
    );

    renderSitesPage();

    const addSiteButtons = await screen.findAllByRole(
      "button",
      { name: /^add site$/i },
      { timeout: FIND_TIMEOUT },
    );

    // Confirms this is genuinely the "sites loaded" branch (toolbar + row
    // list), not the empty/error branch. The toolbar is where the GH #252
    // duplicate used to live, so its presence here is load-bearing, not
    // incidental.
    expect(screen.getByRole("toolbar", { name: "Filter sites" })).toBeInTheDocument();
    expect(screen.getByText("1 site enrolled")).toBeInTheDocument();

    expect(addSiteButtons).toHaveLength(1);
  });

  it("the truly-empty, post-onboarding state renders exactly one Add site trigger (not a second one from NoSitesEmpty's default CTA)", async () => {
    // Mark onboarding already dismissed on this "browser" so SitesPageEmpty
    // renders NoSitesEmpty (not OnboardingWizard, whose CTA is "Continue",
    // not "Add site"; see onboarding-wizard.tsx).
    window.localStorage.setItem("wpmgr.onboarding.completed", "true");

    mockedUseSites.mockImplementation(() => mockQueryResult<Site[]>({ data: [] }));

    renderSitesPage();

    const addSiteButtons = await screen.findAllByRole(
      "button",
      { name: /^add site$/i },
      { timeout: FIND_TIMEOUT },
    );

    // Confirms this is genuinely the truly-empty NoSitesEmpty branch (where
    // the second GH #252 duplicate used to live), not some other branch.
    expect(
      screen.getByRole("heading", { name: "Connect your first WordPress site." }),
    ).toBeInTheDocument();

    expect(addSiteButtons).toHaveLength(1);
  });
});
