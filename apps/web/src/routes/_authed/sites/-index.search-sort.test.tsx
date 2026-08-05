import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
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
import { useSites, type UseSitesOptions } from "@/features/sites/use-sites";
import { useClients } from "@/features/clients/use-clients";
import { useTags } from "@/features/tags/use-tags";
import { useSitesLiveSync } from "@/features/sites/use-sites-live";

// GH #349. Search and order on the Sites list are SERVER-side axes.
//
// THE BUG: `useSites()` asked for no limit, the control plane defaulted to 50
// ordered by created_at DESC, and the page then filtered those 50 in the
// browser. Above 50 sites an agency searched only their newest 50 and were
// told, with no hint of truncation, that nothing else matched. There was no
// way to order the list at all.
//
// THE FIX: `q` and `sort` go to the server, so the rows that arrive ARE the
// best matches in the requested order. Connection status and agent freshness
// stay client-side (see the comment above `visibleSites` for why), which is
// exactly why the request now asks for the contract maximum rather than
// leaving the implicit 50 in place.
//
// Every test in this file fails against the pre-change route.

vi.mock("@/features/sites/use-sites", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("@/features/sites/use-sites")>();
  return { ...actual, useSites: vi.fn() };
});
vi.mock("@/features/clients/use-clients", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("@/features/clients/use-clients")>();
  return { ...actual, useClients: vi.fn() };
});
vi.mock("@/features/tags/use-tags", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("@/features/tags/use-tags")>();
  return { ...actual, useTags: vi.fn() };
});
vi.mock("@/features/sites/use-sites-live", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("@/features/sites/use-sites-live")>();
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
  memberships: [
    {
      user_id: "00000000-0000-0000-0000-0000000000u1",
      tenant_id: "t1",
      role: "owner",
    },
  ],
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

/** The options the ACTIVE-view list call was made with, most recent last. */
function listCalls(): UseSitesOptions[] {
  return mockedUseSites.mock.calls
    .map(([options]) => options)
    .filter((options): options is UseSitesOptions => options?.view === "active");
}

function latestListCall(): UseSitesOptions {
  const calls = listCalls();
  const last = calls.at(-1);
  if (!last) throw new Error("the page never asked for the active sites list");
  return last;
}

const FIND_TIMEOUT = 5000;

beforeEach(() => {
  mockedUseClients.mockReturnValue(mockQueryResult({ data: [] }));
  mockedUseTags.mockReturnValue(mockQueryResult({ data: [] }));
  mockedUseSitesLiveSync.mockReturnValue(undefined);
  mockedUseSites.mockImplementation((options?: UseSitesOptions) =>
    mockQueryResult<Site[]>({
      data: options?.view === "archived" ? [] : [buildSite()],
    }),
  );
});

afterEach(() => {
  window.localStorage.clear();
  vi.clearAllMocks();
});

describe("T5: the Sites list searches on the SERVER (GH #349)", () => {
  it("sends the URL's `q` to the API", async () => {
    renderSitesPage("/sites?q=iacop");

    await screen.findByRole("toolbar", { name: "Filter sites" }, {
      timeout: FIND_TIMEOUT,
    });

    await waitFor(() => {
      expect(latestListCall().q).toBe("iacop");
    });
  });

  it("does NOT filter the returned rows in the browser: a row the server sent is a row the operator sees", async () => {
    // The server matched this site on something the row does not spell out.
    // Its name, url and tags contain no "iacop" at all, so the pre-change
    // page's own filter over name/url/tags dropped it and rendered the
    // filtered-empty state instead. The whole point of moving the axis is
    // that the server's answer IS the answer.
    //
    // Grid view, because the list view's body is windowed by react-virtuoso,
    // which renders no rows at all in jsdom's zero-height layout.
    mockedUseSites.mockImplementation((options?: UseSitesOptions) =>
      mockQueryResult<Site[]>({
        data:
          options?.view === "archived"
            ? []
            : [
                buildSite({
                  id: "s-server-match",
                  name: "Northwind",
                  url: "https://northwind.test",
                  tags: [],
                }),
              ],
      }),
    );

    renderSitesPage("/sites?q=iacop&view=grid");

    expect(
      await screen.findByText("Northwind", {}, { timeout: FIND_TIMEOUT }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("status", {
        name: "No sites match the current filters",
      }),
    ).not.toBeInTheDocument();
  });

  it("counts what the server returned, not a second helping of the same filter", async () => {
    // The subline is the other place the old client-side filter showed
    // through: it counted the rows that survived it. With `q` server-side the
    // page must report the server's answer as-is.
    mockedUseSites.mockImplementation((options?: UseSitesOptions) =>
      mockQueryResult<Site[]>({
        data:
          options?.view === "archived"
            ? []
            : [
                buildSite({ id: "a", name: "Northwind", url: "https://nw.test" }),
                buildSite({ id: "b", name: "Contoso", url: "https://ct.test" }),
              ],
      }),
    );

    renderSitesPage("/sites?q=iacop");

    expect(
      await screen.findByText("2 matching sites", {}, { timeout: FIND_TIMEOUT }),
    ).toBeInTheDocument();
  });

  it("keeps typing responsive: the URL updates on the keystroke even though the request is debounced", async () => {
    const router = renderSitesPage("/sites");

    await screen.findByRole("toolbar", { name: "Filter sites" }, {
      timeout: FIND_TIMEOUT,
    });

    fireEvent.change(screen.getByLabelText("Search sites"), {
      target: { value: "acme" },
    });

    await waitFor(() => {
      expect(router.state.location.search).toMatchObject({ q: "acme" });
    });
    await waitFor(() => {
      expect(latestListCall().q).toBe("acme");
    });
  });
});

describe("T4: the order control is URL-controlled (GH #349)", () => {
  it("reads the applied order back out of the URL", async () => {
    renderSitesPage("/sites?sort=name");

    expect(
      await screen.findByRole(
        "button",
        { name: /Order sites, currently Name \(A to Z\)/ },
        { timeout: FIND_TIMEOUT },
      ),
    ).toBeInTheDocument();
  });

  it("shows the list's real default when the URL says nothing", async () => {
    renderSitesPage("/sites");

    expect(
      await screen.findByRole(
        "button",
        { name: /Order sites, currently Newest first/ },
        { timeout: FIND_TIMEOUT },
      ),
    ).toBeInTheDocument();
  });

  it("writes the chosen order to the URL and sends it to the API", async () => {
    const router = renderSitesPage("/sites");

    const trigger = await screen.findByRole(
      "button",
      { name: /Order sites/ },
      { timeout: FIND_TIMEOUT },
    );
    fireEvent.pointerDown(
      trigger,
      new PointerEvent("pointerdown", { bubbles: true, button: 0 }),
    );

    const option = await screen.findByRole("menuitemradio", {
      name: "Recently active",
    });
    fireEvent.click(option);

    await waitFor(() => {
      expect(router.state.location.search).toMatchObject({ sort: "-last_seen" });
    });
    await waitFor(() => {
      expect(latestListCall().sort).toBe("-last_seen");
    });
  });

  it("marks the applied order as checked, so the menu says which one is on", async () => {
    renderSitesPage("/sites?sort=-name");

    const trigger = await screen.findByRole(
      "button",
      { name: /Order sites/ },
      { timeout: FIND_TIMEOUT },
    );
    fireEvent.pointerDown(
      trigger,
      new PointerEvent("pointerdown", { bubbles: true, button: 0 }),
    );

    const checked = await screen.findByRole("menuitemradio", { checked: true });
    expect(checked).toHaveTextContent("Name (Z to A)");
  });

  it("falls back to the default rather than breaking the page on a bogus URL value", async () => {
    // The server 422s an unrecognised order. A hand-edited or stale link must
    // not send one, and must not blow up the route either.
    renderSitesPage("/sites?sort=by-vibes");

    await screen.findByRole(
      "button",
      { name: /Order sites, currently Newest first/ },
      { timeout: FIND_TIMEOUT },
    );
    expect(latestListCall().sort).toBeUndefined();
  });

  it("survives 'Clear filters': an order is how you read the list, not a filter on it", async () => {
    const router = renderSitesPage("/sites?q=acme&sort=name");

    const clear = await screen.findByRole(
      "button",
      { name: /Clear 1 active filter/ },
      { timeout: FIND_TIMEOUT },
    );
    fireEvent.click(clear);

    await waitFor(() => {
      expect(router.state.location.search).not.toHaveProperty("q");
    });
    expect(router.state.location.search).toMatchObject({ sort: "name" });
    // And the control still says so, rather than quietly snapping back to the
    // default while the URL claims otherwise.
    expect(
      screen.getByRole("button", { name: /Order sites, currently Name \(A to Z\)/ }),
    ).toBeInTheDocument();
    await waitFor(() => {
      expect(latestListCall().sort).toBe("name");
    });
  });
});
