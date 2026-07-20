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

describe("Sites page: exactly one 'Add site' trigger for an operator (GH #252)", () => {
  it("list view with sites loaded renders exactly one Add site button (the PageHeader's)", async () => {
    mockedUseSites.mockImplementation((options?: UseSitesOptions) =>
      mockQueryResult<Site[]>({ data: options?.view === "archived" ? [] : [buildSite()] }),
    );

    renderSitesPage();

    // The page renders asynchronously behind RouterProvider's first paint
    // (see src/test/render.tsx's module doc); the row list itself is
    // virtualized (react-virtuoso), which does not measure/paint rows in
    // jsdom's zero-size layout, so assert on the toolbar landing (proof the
    // "sites loaded" branch, not the empty/error one, is showing) rather
    // than on a specific row's text.
    await screen.findByRole("toolbar", { name: "Filter sites" });
    expect(screen.getByText("1 site enrolled")).toBeInTheDocument();

    const addSiteButtons = screen.getAllByRole("button", { name: /^add site$/i });
    expect(addSiteButtons).toHaveLength(1);
  });

  it("the truly-empty, post-onboarding state renders exactly one Add site trigger (not a second one from NoSitesEmpty's default CTA)", async () => {
    // Mark onboarding already dismissed on this "browser" so SitesPageEmpty
    // renders NoSitesEmpty (not OnboardingWizard, whose CTA is "Continue",
    // not "Add site"; see onboarding-wizard.tsx).
    window.localStorage.setItem("wpmgr.onboarding.completed", "true");

    mockedUseSites.mockImplementation(() => mockQueryResult<Site[]>({ data: [] }));

    renderSitesPage();

    await screen.findByRole("heading", {
      name: "Connect your first WordPress site.",
    });

    const addSiteButtons = screen.getAllByRole("button", { name: /^add site$/i });
    expect(addSiteButtons).toHaveLength(1);
  });
});
