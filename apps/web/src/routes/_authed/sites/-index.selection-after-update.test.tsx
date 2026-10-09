import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  act,
  fireEvent,
  renderHook,
  screen,
  waitFor,
} from "@testing-library/react";
import {
  Outlet,
  RouterProvider,
  createMemoryHistory,
  createRoute,
  createRootRoute,
  createRouter,
} from "@tanstack/react-router";
import type { QueryClient } from "@tanstack/react-query";
import type { Me, Site, UpdateRun } from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";
import { authKeys } from "@/features/auth/use-auth";
import { CommandPalette } from "@/features/command/command-palette";
import { useClients } from "@/features/clients/use-clients";
import { useSitesLiveSync } from "@/features/sites/use-sites-live";
import { useSitesSelection } from "@/features/sites/use-sites-selection";
import { useTags } from "@/features/tags/use-tags";

import { Route as SitesIndexRoute } from "./index";

// GH #742. The Sites selection is a module-level singleton, so a selection made
// for a bulk update outlived the page: the operator submitted "Update plugins",
// landed on the run page, came back through the sidebar (which carries no
// filters) and found "N sites selected" with no card checked in view. The next
// bulk action, delete included, would have run on ids nobody could see.
//
// Everything below runs the REAL Sites route, the REAL update wizard and the
// REAL `useSites` / `useCreateUpdateRun` hooks through a real memory router and a
// real QueryClient. Only the two generated SDK calls the flow makes are
// replaced, at the same boundary update-wizard.test.tsx already uses:
//
//   listSites       GET  /api/v1/sites     200 { items: Site[] }; the archived
//                                          bucket is `?state=archived`
//                                          (packages/openapi/openapi.yaml,
//                                          operationId listSites)
//   createUpdateRun POST /api/v1/updates   201 UpdateRun, 422 Error
//                                          (apps/api/internal/update/handler.go,
//                                          `c.JSON(http.StatusCreated, &out)`)
//
// The 422 fixture is the real code `no_target_sites` that CreateRun returns
// when no enrolled site matches (apps/api/internal/update/service.go, a
// domain.Validation, mapped to 422 by domain.HTTPStatus in
// apps/api/internal/domain/errors.go).
//
// The grid view (`?view=grid`) is used on purpose: the default table view is a
// virtualised list that never mounts a row under jsdom (see
// features/sites/gh414-pause-real-tree.test.tsx). The selection singleton is
// the same one in both views.
const { listSitesMock, createUpdateRunMock } = vi.hoisted(() => ({
  listSitesMock: vi.fn(),
  createUpdateRunMock: vi.fn(),
}));

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return {
    ...actual,
    listSites: listSitesMock,
    createUpdateRun: createUpdateRunMock,
  };
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

// cmdk lives inside a Radix dialog, which observes its content box, and cmdk
// scrolls the active item into view. jsdom has neither (src/test/setup.ts holds
// no global stubs on purpose), so they are stubbed here, as
// features/command/command-palette.test.tsx does.
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
vi.stubGlobal("ResizeObserver", ResizeObserverStub);
Element.prototype.scrollIntoView = function scrollIntoView() {};

const OWNER_ME: Me = {
  user: {
    id: "00000000-0000-0000-0000-0000000000u1",
    email: "owner@example.com",
    name: "Owner",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  },
  memberships: [
    { user_id: "00000000-0000-0000-0000-0000000000u1", tenant_id: "t1", role: "owner" },
  ],
  active_tenant_id: "t1",
  hosted: false,
};

const RUN_ID = "7b6f0f1e-3d0a-4f55-9d44-5c1f0a2a0b01";

function buildSite(id: string, name: string): Site {
  return {
    id,
    tenant_id: "t1",
    url: `https://${name.toLowerCase()}.example.com`,
    name,
    status: "active",
    enrolled: true,
    wp_version: "6.8",
    php_version: "8.3",
    health_status: "healthy",
    multisite: false,
    tags: [],
    components: {
      plugins: [
        {
          slug: "woocommerce",
          name: "WooCommerce",
          version: "8.0.0",
          available_update: { new_version: "8.1.0" },
        },
      ],
      themes: [],
    },
  } as unknown as Site;
}

const SITES: Site[] = [
  buildSite("site-a", "Alpha"),
  buildSite("site-b", "Bravo"),
  buildSite("site-c", "Charlie"),
];

function createdRun(): UpdateRun {
  return {
    id: RUN_ID,
    tenant_id: "t1",
    status: "pending",
    dry_run: true,
    created_at: "2026-10-09T10:00:00Z",
    updated_at: "2026-10-09T10:00:00Z",
  };
}

/** Reads the singleton from outside the Sites page, the way the command palette does. */
function SelectionProbe() {
  const { count } = useSitesSelection();
  return <p data-testid="selection-count">{count}</p>;
}

/**
 * The shell around the page: a probe and the real command palette beside the
 * Outlet, which is where `AppShell` mounts both in the app. The palette stays
 * closed (it is a modal that would hide the page from the accessibility tree)
 * until a test presses the stand-in button.
 */
function Shell() {
  const [paletteOpen, setPaletteOpen] = useState(false);
  return (
    <>
      <button type="button" onClick={() => setPaletteOpen(true)}>
        Open palette
      </button>
      <SelectionProbe />
      <Outlet />
      <CommandPalette open={paletteOpen} onClose={() => setPaletteOpen(false)} />
    </>
  );
}

function buildRouter(
  initialPath: string,
  queryClient: QueryClient,
  options: { runPageLoader?: () => Promise<void> } = {},
) {
  const rootRoute = createRootRoute({ component: Shell });
  type UpdateOptions = Parameters<typeof SitesIndexRoute.update>[0];
  const sitesRoute = SitesIndexRoute.update({
    id: "/sites",
    path: "/sites",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  // The real run page opens an EventSource and polls, none of which this flow
  // is about. A stand-in at the real path is enough: the wizard navigates to
  // `/updates/$runId`, and what matters is that Sites unmounts when it lands.
  const runRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/updates/$runId",
    loader: options.runPageLoader,
    component: () => <p>Run page</p>,
  });
  const elsewhereRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/elsewhere",
    component: () => <p>Somewhere else</p>,
  });
  return createRouter({
    routeTree: rootRoute.addChildren([sitesRoute, runRoute, elsewhereRoute]),
    context: { queryClient },
    history: createMemoryHistory({ initialEntries: [initialPath] }),
    // Keep the page being left on screen for as long as a navigation is
    // pending, instead of swapping to a pending component after a second.
    defaultPendingMs: Number.POSITIVE_INFINITY,
  });
}

type TestRouter = ReturnType<typeof buildRouter>;

function renderApp(
  initialPath = "/sites?view=grid",
  options: { runPageLoader?: () => Promise<void> } = {},
): TestRouter {
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(authKeys.me, OWNER_ME);
  const router = buildRouter(initialPath, queryClient, options);
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  return router;
}

/** Empties the module-level singleton so one test never inherits another's. */
function resetSelection(): void {
  const { result, unmount } = renderHook(() => useSitesSelection());
  act(() => result.current.clear());
  unmount();
}

const FIND_TIMEOUT = 5000;

function selectionCount(): string {
  return screen.getByTestId("selection-count").textContent ?? "";
}

async function selectSites(...names: string[]): Promise<void> {
  for (const name of names) {
    fireEvent.click(
      await screen.findByRole(
        "checkbox",
        { name: `Select ${name}` },
        { timeout: FIND_TIMEOUT },
      ),
    );
  }
  // The toolbar swaps modes through an exit animation, so the bulk controls
  // arrive a moment after its label changes: wait for one of them.
  await screen.findByRole(
    "button",
    { name: `Run backup on ${names.length} ${names.length === 1 ? "site" : "sites"}` },
    { timeout: FIND_TIMEOUT },
  );
  expect(selectionCount()).toBe(String(names.length));
}

/** Opens the wizard from the bulk toolbar and ticks the one plugin with an update. */
async function openWizardAndPickAPlugin(siteCount: number): Promise<void> {
  fireEvent.click(
    screen.getByRole("button", {
      name: `Update plugins on ${siteCount} sites`,
    }),
  );
  fireEvent.click(await screen.findByLabelText(/woocommerce/i));
}

function submitButton(): HTMLElement {
  return screen.getByRole("button", { name: /preview 1 update/i });
}

/** The same move the sidebar's Sites link makes: `to: "/sites"`, no search. */
async function backToSitesBySidebar(router: TestRouter): Promise<void> {
  await act(async () => {
    await router.navigate({ to: "/sites" });
  });
}

beforeEach(() => {
  resetSelection();
  listSitesMock.mockReset();
  listSitesMock.mockImplementation(
    ({ query }: { query?: { state?: string } } = {}) =>
      Promise.resolve({
        data: { items: query?.state === "archived" ? [] : SITES },
        error: undefined,
        response: { status: 200 },
      }),
  );
  createUpdateRunMock.mockReset();
  createUpdateRunMock.mockResolvedValue({
    data: createdRun(),
    error: undefined,
    response: { status: 201 },
  });
  vi.mocked(useClients).mockReturnValue(mockQueryResult({ data: [] }));
  vi.mocked(useTags).mockReturnValue(mockQueryResult({ data: [] }));
  vi.mocked(useSitesLiveSync).mockReturnValue(undefined);
});

afterEach(() => {
  window.localStorage.clear();
});

describe("Sites page: a selection does not outlive the visit it was made in (GH #742)", () => {
  it("a submitted bulk update leaves no selection behind when the operator comes back by the sidebar", async () => {
    const router = renderApp();

    await selectSites("Alpha", "Bravo");
    await openWizardAndPickAPlugin(2);
    fireEvent.click(submitButton());

    await screen.findByText("Run page", {}, { timeout: FIND_TIMEOUT });
    expect(router.state.location.pathname).toBe(`/updates/${RUN_ID}`);
    expect(createUpdateRunMock).toHaveBeenCalledTimes(1);

    await backToSitesBySidebar(router);

    // Wait for the page's toolbar, whichever mode it is in, then judge the mode
    // with an assertion, so a regression reads "action" and not a timeout.
    const toolbar = await screen.findByRole(
      "toolbar",
      {},
      { timeout: FIND_TIMEOUT },
    );
    // The idle toolbar is back: its filter controls are the thing a leftover
    // selection hid, and "N sites selected" is the thing it showed instead.
    expect(toolbar).toHaveAttribute("data-mode", "idle");
    expect(
      screen.getByRole("searchbox", { name: "Search sites" }),
    ).toBeInTheDocument();
    expect(screen.queryByText(/sites? selected/)).not.toBeInTheDocument();
    expect(selectionCount()).toBe("0");
  });

  it("the run being created consumes the selection while the page is still on screen, however long the navigation takes", async () => {
    // The route the wizard navigates to holds its loader open, so Sites stays
    // mounted. The unmount clear below cannot be what empties the selection
    // here: only the wizard reporting its submitted run can.
    let releaseRunPage!: () => void;
    const runPageLoader = () =>
      new Promise<void>((resolve) => {
        releaseRunPage = resolve;
      });
    renderApp("/sites?view=grid", { runPageLoader });

    await selectSites("Alpha", "Bravo");
    await openWizardAndPickAPlugin(2);
    fireEvent.click(submitButton());

    await waitFor(() => expect(createUpdateRunMock).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(selectionCount()).toBe("0"));

    // Still on Sites, with the run page not yet shown. The router has already
    // moved its location to the run, so what is on screen is the evidence.
    expect(
      screen.getByRole("region", { name: "Sites grid" }),
    ).toBeInTheDocument();
    expect(screen.queryByText("Run page")).not.toBeInTheDocument();
    expect(await screen.findByRole("toolbar")).toHaveAttribute(
      "data-mode",
      "idle",
    );

    await act(async () => {
      releaseRunPage();
    });
    await screen.findByText("Run page", {}, { timeout: FIND_TIMEOUT });
  });

  it("cancelling the wizard leaves the selection exactly as it was", async () => {
    renderApp();

    await selectSites("Alpha", "Bravo");
    await openWizardAndPickAPlugin(2);
    fireEvent.click(screen.getByRole("button", { name: "Close" }));

    await waitFor(() =>
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument(),
    );
    expect(createUpdateRunMock).not.toHaveBeenCalled();
    expect(selectionCount()).toBe("2");
    expect(
      screen.getByRole("toolbar", { name: "Bulk actions" }),
    ).toBeInTheDocument();
    expect(screen.getByText("sites selected")).toBeInTheDocument();
  });

  it("a refused run leaves the selection and the open wizard alone so the operator can retry", async () => {
    createUpdateRunMock.mockResolvedValue({
      data: undefined,
      error: {
        code: "no_target_sites",
        message: "no enrolled sites matched the selection",
      },
      response: { status: 422 },
    });
    const router = renderApp();

    await selectSites("Alpha", "Bravo");
    await openWizardAndPickAPlugin(2);
    fireEvent.click(submitButton());

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "no enrolled sites matched the selection",
    );
    expect(createUpdateRunMock).toHaveBeenCalledTimes(1);
    expect(router.state.location.pathname).toBe("/sites");
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(selectionCount()).toBe("2");
  });

  it("leaving Sites with sites ticked, and no bulk action at all, still drops the selection", async () => {
    const router = renderApp();

    await selectSites("Alpha", "Bravo");
    await act(async () => {
      await router.navigate({ to: "/elsewhere" });
    });
    await screen.findByText("Somewhere else");

    // Nothing but the singleton remembers it: the page that owned it is gone.
    expect(selectionCount()).toBe("0");

    await backToSitesBySidebar(router);
    expect(
      await screen.findByRole("toolbar", {}, { timeout: FIND_TIMEOUT }),
    ).toHaveAttribute("data-mode", "idle");
    expect(selectionCount()).toBe("0");
  });

  it("changing a filter keeps the selection: the clear is for leaving the page, not for re-rendering it", async () => {
    // The over-clear guard. A filter change navigates within the same route, so
    // the page stays mounted and a selection made across filters must survive
    // (index.tsx, CRITICAL INVARIANT: filtering a selected site out of view
    // keeps it in the bulk target).
    const router = renderApp();

    await selectSites("Alpha", "Bravo");
    await act(async () => {
      await router.navigate({
        to: "/sites",
        search: { view: "grid", q: "charlie" },
      });
    });
    await waitFor(() =>
      expect(router.state.location.search).toMatchObject({ q: "charlie" }),
    );

    expect(selectionCount()).toBe("2");
    expect(
      screen.getByRole("toolbar", { name: "Bulk actions" }),
    ).toBeInTheDocument();
  });
});

describe("Command palette: selected-site actions exist only while the Sites page owns the selection (GH #742)", () => {
  it("offers 'Run on selected' on Sites, and no longer once the operator has left it", async () => {
    // "Run backup on N sites" runs on whatever the singleton holds, from any
    // page. A selection left behind by Sites would have been offered on every
    // other page, over sites the operator cannot see from there.
    const router = renderApp();

    await selectSites("Alpha", "Bravo");
    fireEvent.click(screen.getByRole("button", { name: "Open palette" }));
    // Positive control: the group IS offered while the selection is live.
    expect(await screen.findByText("Run on selected (2)")).toBeInTheDocument();
    expect(screen.getByText("Run backup on 2 sites")).toBeInTheDocument();
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    await waitFor(() =>
      expect(screen.queryByText("Run on selected (2)")).not.toBeInTheDocument(),
    );

    await act(async () => {
      await router.navigate({ to: "/elsewhere" });
    });
    await screen.findByText("Somewhere else");

    fireEvent.click(screen.getByRole("button", { name: "Open palette" }));
    await screen.findByText("Run on all");
    expect(screen.queryByText(/Run on selected/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Run backup on \d+ sites/)).not.toBeInTheDocument();
  });
});
