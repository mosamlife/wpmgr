import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, screen, fireEvent, waitFor } from "@testing-library/react";
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

// GH #414 adversarial-review finding 3 — a viewer (no operate permission).
const VIEWER_ME: Me = {
  user: {
    id: "00000000-0000-0000-0000-0000000000v1",
    email: "viewer@example.com",
    name: "Viewer",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  },
  memberships: [{ user_id: "00000000-0000-0000-0000-0000000000v1", tenant_id: "t1", role: "viewer" }],
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

function renderSitesPage(initialPath = "/sites", me: Me = OWNER_ME) {
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(authKeys.me, me);
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

// GH #414 adversarial-review finding 3 — `index.tsx`'s `operate` gate
// (`const operate = canOperate(me)`) is the ONLY thing standing between a
// viewer and a working pause/resume control: `SiteRowActions` carries no
// permission check of its own, it gates on connection state plus callback
// presence (`onPauseMonitoring`/`onResumeMonitoring`). Forcing `operate` to
// `true` at index.tsx:159 was previously undetected because nothing rendered
// the real page tree for a non-operator and looked for the row menu item.
// Grid view (`?view=grid`) is used deliberately: the default table view's
// TableVirtuoso never mounts rows in jsdom (see
// features/sites/gh414-pause-real-tree.test.tsx's file header), but
// SiteRowActions is the exact same shared component in both views.
describe("Sites page: pause/resume control gated behind operate permission (GH #414 adversarial-review finding 3)", () => {
  it("a viewer (non-operator) gets no Pause monitoring control in the row menu", async () => {
    mockedUseSites.mockImplementation((options?: UseSitesOptions) =>
      mockQueryResult<Site[]>({
        data:
          options?.view === "archived"
            ? []
            : [buildSite({ enrolled: true, health_status: "healthy" })],
      }),
    );

    renderSitesPage("/sites?view=grid", VIEWER_ME);

    const moreActionsButton = await screen.findByRole(
      "button",
      { name: /more actions for acme/i },
      { timeout: FIND_TIMEOUT },
    );
    fireEvent.keyDown(moreActionsButton, { key: "Enter" });
    await screen.findByRole("menu");

    expect(
      screen.queryByRole("menuitem", { name: /pause monitoring/i }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("menuitem", { name: /resume monitoring/i }),
    ).not.toBeInTheDocument();
  });

  it("an owner (operator) DOES get the Pause monitoring control in the same row menu (sanity control)", async () => {
    mockedUseSites.mockImplementation((options?: UseSitesOptions) =>
      mockQueryResult<Site[]>({
        data:
          options?.view === "archived"
            ? []
            : [buildSite({ enrolled: true, health_status: "healthy" })],
      }),
    );

    renderSitesPage("/sites?view=grid", OWNER_ME);

    const moreActionsButton = await screen.findByRole(
      "button",
      { name: /more actions for acme/i },
      { timeout: FIND_TIMEOUT },
    );
    fireEvent.keyDown(moreActionsButton, { key: "Enter" });

    expect(
      await screen.findByRole("menuitem", { name: /pause monitoring/i }),
    ).toBeInTheDocument();
  });
});

// GH #338. "Show archived" with nothing archived swapped the whole page for the
// first-run screen ("Connect your first WordPress site.") and removed the chip
// that gets back, because an empty archived list was read as "this tenant has no
// sites at all". The operator in the report had 24 sites and saw a page that
// said they had none, with no control on it that led anywhere.
//
// Server state is mocked at the hook boundary like every test above: the
// archived bucket is the second `useSites` call, told apart by `options.view`
// (use-sites.ts maps view "archived" to `?state=archived`; packages/openapi/
// openapi.yaml, operationId listSites).
//
// The grid view keeps the cards mountable under jsdom. Cards are found by their
// name link, which every role sees.
function activeSites(count: number): Site[] {
  return Array.from({ length: count }, (_, i) =>
    buildSite({
      id: `00000000-0000-0000-0000-${String(i + 1).padStart(12, "0")}`,
      name: `Site ${i + 1}`,
      url: `https://site-${i + 1}.example.com`,
    }),
  );
}

function mockBuckets(active: Site[], archived: Site[]): void {
  mockedUseSites.mockImplementation((options?: UseSitesOptions) =>
    mockQueryResult<Site[]>({
      data: options?.view === "archived" ? archived : active,
    }),
  );
}

/** The page heading renders in every branch, so it is the neutral thing to wait
 *  on: whatever branch the page chose, the assertions after it are synchronous
 *  and fail with a message about that branch rather than with a timeout. */
async function pageReady(): Promise<void> {
  await screen.findByRole(
    "heading",
    { name: "Sites" },
    { timeout: FIND_TIMEOUT },
  );
}

describe("Sites page: an empty archived view is not the first-run screen (GH #338)", () => {
  beforeEach(() => {
    // Onboarding already dismissed on this browser, so the first-run screen is
    // the one the reporter saw, "Connect your first WordPress site.", and not
    // the onboarding wizard.
    window.localStorage.setItem("wpmgr.onboarding.completed", "true");
  });

  it("says there are no archived sites and keeps the way back when nothing is archived", async () => {
    mockBuckets(activeSites(24), []);

    renderSitesPage("/sites?view=grid&archived=true");
    await pageReady();

    expect(
      screen.queryByRole("heading", {
        name: "Connect your first WordPress site.",
      }),
    ).not.toBeInTheDocument();
    expect(
      screen.getByRole("status", { name: "No archived sites" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Showing archived" }),
    ).toHaveAttribute("aria-pressed", "true");
    expect(
      screen.getByRole("button", { name: "Back to active sites" }),
    ).toBeInTheDocument();
  });

  it("the archived toggle on that page goes back to the active sites", async () => {
    mockBuckets(activeSites(24), []);

    const router = renderSitesPage("/sites?view=grid&archived=true");
    await pageReady();
    fireEvent.click(screen.getByRole("button", { name: "Showing archived" }));

    expect(
      await screen.findByRole("link", { name: "Site 1" }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("status", { name: "No archived sites" }),
    ).not.toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Show archived" }),
    ).toHaveAttribute("aria-pressed", "false");
    expect(router.state.location.search).not.toHaveProperty("archived");
  });

  it("'Back to active sites' in the empty state does the same", async () => {
    mockBuckets(activeSites(24), []);

    const router = renderSitesPage("/sites?view=grid&archived=true");
    await pageReady();
    fireEvent.click(
      screen.getByRole("button", { name: "Back to active sites" }),
    );

    expect(
      await screen.findByRole("link", { name: "Site 1" }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("status", { name: "No archived sites" }),
    ).not.toBeInTheDocument();
    expect(router.state.location.search).not.toHaveProperty("archived");
  });

  it("switching to archived adds a history entry, so the browser Back button returns to the active list", async () => {
    mockBuckets(activeSites(3), []);

    const router = renderSitesPage("/sites?view=grid");
    await pageReady();
    fireEvent.click(screen.getByRole("button", { name: "Show archived" }));
    await screen.findByRole("status", { name: "No archived sites" });

    act(() => router.history.back());

    expect(
      await screen.findByRole("link", { name: "Site 1" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Show archived" }),
    ).toHaveAttribute("aria-pressed", "false");
    expect(router.state.location.search).not.toHaveProperty("archived");
  });

  it("gives a viewer the way back as well, though a viewer has no archived toggle", async () => {
    mockBuckets(activeSites(2), []);

    renderSitesPage("/sites?view=grid&archived=true", VIEWER_ME);
    await pageReady();

    expect(
      screen.getByRole("status", { name: "No archived sites" }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /archived$/i }),
    ).not.toBeInTheDocument();

    fireEvent.click(
      screen.getByRole("button", { name: "Back to active sites" }),
    );
    expect(
      await screen.findByRole("link", { name: "Site 1" }),
    ).toBeInTheDocument();
  });

  // The honest cases the new state must not swallow.

  it("still lists archived sites when there are some", async () => {
    mockBuckets(activeSites(2), [
      buildSite({
        id: "00000000-0000-0000-0000-0000000000a1",
        name: "Retired",
        url: "https://retired.example.com",
      }),
    ]);

    renderSitesPage("/sites?view=grid&archived=true");

    expect(
      await screen.findByRole("link", { name: "Retired" }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("status", { name: "No archived sites" }),
    ).not.toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Showing archived" }),
    ).toHaveAttribute("aria-pressed", "true");
  });

  it("keeps the filter message when a filter, not the archive, emptied the list", async () => {
    mockBuckets(activeSites(2), []);

    renderSitesPage("/sites?view=grid&archived=true&q=zzz");
    await pageReady();

    expect(
      screen.getByRole("status", { name: "No sites match the current filters" }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("status", { name: "No archived sites" }),
    ).not.toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Showing archived" }),
    ).toBeInTheDocument();
  });

  it("still shows the first-run screen for a tenant with no sites at all", async () => {
    // The positive control for the absence asserted above: the heading that must
    // be gone from the archived view is the same one this state renders.
    mockBuckets([], []);

    renderSitesPage("/sites?view=grid");

    expect(
      await screen.findByRole(
        "heading",
        { name: "Connect your first WordPress site." },
        { timeout: FIND_TIMEOUT },
      ),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("status", { name: "No archived sites" }),
    ).not.toBeInTheDocument();
  });
});

// GH #568. The Status filter could not select Paused or Archived. Its options
// were the connection states of the rows already loaded: a pause is a separate
// field on the row (monitoring_paused_at), not a connection state, and archived
// sites are a separate server list that the active view never loads.
//
// Paused is now a Monitoring filter (Active / Paused) beside Status, and
// operators are always offered Archived in Status. Archived is the same switch
// as the "Show archived" chip: both write the one `archived` search param.

/** A connected site, so the Status menu has a real state to offer. */
function connectedSite(overrides: Partial<Site> = {}): Site {
  return buildSite({ enrolled: true, health_status: "healthy", ...overrides });
}

/** What the server sends for the archived list: connection_state "archived". */
function archivedSite(overrides: Partial<Site> = {}): Site {
  return {
    ...buildSite(overrides),
    connection_state: "archived",
  } as unknown as Site;
}

const LIVE = connectedSite({
  id: "00000000-0000-0000-0000-0000000000b1",
  name: "Live",
  url: "https://live.example.com",
});
const FROZEN = connectedSite({
  id: "00000000-0000-0000-0000-0000000000b2",
  name: "Frozen",
  url: "https://frozen.example.com",
  monitoring_paused_at: "2026-10-01T09:00:00Z",
});
const RETIRED = archivedSite({
  id: "00000000-0000-0000-0000-0000000000b3",
  name: "Retired",
  url: "https://retired.example.com",
});

/** An array-valued search param as TanStack Router writes it into a URL. */
function arrayParam(...values: string[]): string {
  return encodeURIComponent(JSON.stringify(values));
}

function openMenu(trigger: HTMLElement): void {
  fireEvent.pointerDown(
    trigger,
    new PointerEvent("pointerdown", { bubbles: true, button: 0 }),
  );
}

/** The filter menus stay open across a toggle by design, and while one is open
 *  the rest of the page is hidden from assistive technology. Close it the way a
 *  person would, so what the test reads next is the page as they see it. */
async function closeMenu(): Promise<void> {
  fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape" });
  await waitFor(() => {
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });
}

async function chooseFromMenu(
  triggerName: string,
  optionName: string,
): Promise<void> {
  const trigger = await screen.findByRole(
    "button",
    { name: triggerName },
    { timeout: FIND_TIMEOUT },
  );
  openMenu(trigger);
  fireEvent.click(
    await screen.findByRole("menuitemcheckbox", { name: optionName }),
  );
  await closeMenu();
}

describe("Sites page: the Monitoring filter (GH #568)", () => {
  it("choosing Monitoring: Paused lists only the paused site", async () => {
    mockBuckets([LIVE, FROZEN], []);

    const router = renderSitesPage("/sites?view=grid");
    // Positive control: before the filter both sites are listed.
    expect(
      await screen.findByText(
        "2 sites enrolled, 1 paused",
        {},
        { timeout: FIND_TIMEOUT },
      ),
    ).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Live" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Frozen" })).toBeInTheDocument();

    await chooseFromMenu("Filter by monitoring", "Paused");

    expect(await screen.findByText("1 matching site")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Frozen" })).toBeInTheDocument();
    expect(
      screen.queryByRole("link", { name: "Live" }),
    ).not.toBeInTheDocument();
    expect(router.state.location.search).toMatchObject({
      monitoring: ["Paused"],
    });
  });

  it("choosing Monitoring: Active hides only the paused site", async () => {
    mockBuckets([LIVE, FROZEN], []);

    renderSitesPage("/sites?view=grid");
    await screen.findByRole(
      "link",
      { name: "Frozen" },
      { timeout: FIND_TIMEOUT },
    );

    await chooseFromMenu("Filter by monitoring", "Active");

    expect(await screen.findByText("1 matching site")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Live" })).toBeInTheDocument();
    expect(
      screen.queryByRole("link", { name: "Frozen" }),
    ).not.toBeInTheDocument();
  });

  it("reads the Monitoring filter back out of the URL", async () => {
    mockBuckets([LIVE, FROZEN], []);

    renderSitesPage(`/sites?view=grid&monitoring=${arrayParam("Paused")}`);

    expect(
      await screen.findByRole(
        "link",
        { name: "Frozen" },
        { timeout: FIND_TIMEOUT },
      ),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("link", { name: "Live" }),
    ).not.toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Clear 1 active filter" }),
    ).toBeInTheDocument();
  });

  it("'Clear filters' resets Monitoring along with the other axes", async () => {
    mockBuckets([LIVE, FROZEN], []);

    const router = renderSitesPage(
      `/sites?view=grid&monitoring=${arrayParam("Paused")}`,
    );
    await screen.findByRole(
      "link",
      { name: "Frozen" },
      { timeout: FIND_TIMEOUT },
    );

    fireEvent.click(screen.getByRole("button", { name: "Clear 1 active filter" }));

    expect(
      await screen.findByRole("link", { name: "Live" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Frozen" })).toBeInTheDocument();
    expect(router.state.location.search).not.toHaveProperty("monitoring");
  });

  it("says which filter left nothing to show when no site is paused", async () => {
    // Nothing is paused, so Monitoring: Paused matches no row. The page must
    // call that a filter result and name the filter, not render an empty grid.
    mockBuckets([LIVE], []);

    renderSitesPage(`/sites?view=grid&monitoring=${arrayParam("Paused")}`);

    const empty = await screen.findByRole(
      "status",
      { name: "No sites match the current filters" },
      { timeout: FIND_TIMEOUT },
    );
    expect(empty).toHaveTextContent("monitoring:Paused");
  });
});

describe("Sites page: Status offers Archived to operators (GH #568)", () => {
  beforeEach(() => {
    window.localStorage.setItem("wpmgr.onboarding.completed", "true");
  });

  it("an operator with only active sites loaded can choose Archived", async () => {
    mockBuckets([LIVE, FROZEN], []);

    const router = renderSitesPage("/sites?view=grid");
    await screen.findByRole(
      "link",
      { name: "Live" },
      { timeout: FIND_TIMEOUT },
    );

    await chooseFromMenu("Filter by status", "Archived");

    await waitFor(() => {
      expect(router.state.location.search).toMatchObject({ archived: true });
    });
    // The page now asks for the archived list: the mock answers it with none.
    expect(
      await screen.findByRole("status", { name: "No archived sites" }),
    ).toBeInTheDocument();
  });

  it("a viewer is not offered Archived", async () => {
    mockBuckets([LIVE, FROZEN], []);

    renderSitesPage("/sites?view=grid", VIEWER_ME);
    openMenu(
      await screen.findByRole(
        "button",
        { name: "Filter by status" },
        { timeout: FIND_TIMEOUT },
      ),
    );

    // Positive control: the menu is open and lists the real state.
    expect(
      await screen.findByRole("menuitemcheckbox", { name: "Connected" }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("menuitemcheckbox", { name: "Archived" }),
    ).not.toBeInTheDocument();
  });

  it("choosing Archived drops a Status selection made against the active list", async () => {
    mockBuckets([LIVE, FROZEN], []);

    const router = renderSitesPage(
      `/sites?view=grid&status=${arrayParam("Connected")}`,
    );
    await screen.findByRole(
      "link",
      { name: "Live" },
      { timeout: FIND_TIMEOUT },
    );

    await chooseFromMenu("Filter by status", "Archived");

    await waitFor(() => {
      expect(router.state.location.search).toMatchObject({ archived: true });
    });
    expect(router.state.location.search).not.toHaveProperty("status");
  });

  it("ticks Archived while the archived list is showing, and unticking it goes back with a history entry", async () => {
    mockBuckets([LIVE, FROZEN], [RETIRED]);

    const router = renderSitesPage("/sites?view=grid&archived=true");
    await screen.findByRole(
      "link",
      { name: "Retired" },
      { timeout: FIND_TIMEOUT },
    );

    openMenu(screen.getByRole("button", { name: "Filter by status" }));
    const archivedOption = await screen.findByRole("menuitemcheckbox", {
      name: "Archived",
    });
    expect(archivedOption).toHaveAttribute("aria-checked", "true");
    fireEvent.click(archivedOption);
    await closeMenu();

    expect(
      await screen.findByRole("link", { name: "Live" }),
    ).toBeInTheDocument();
    expect(router.state.location.search).not.toHaveProperty("archived");

    act(() => router.history.back());

    expect(
      await screen.findByRole("link", { name: "Retired" }),
    ).toBeInTheDocument();
    expect(router.state.location.search).toMatchObject({ archived: true });
  });

  it("the Show archived chip and the Status entry are one control", async () => {
    mockBuckets([LIVE, FROZEN], [RETIRED]);

    renderSitesPage("/sites?view=grid");
    fireEvent.click(
      await screen.findByRole(
        "button",
        { name: "Show archived" },
        { timeout: FIND_TIMEOUT },
      ),
    );
    await screen.findByRole("link", { name: "Retired" });

    openMenu(screen.getByRole("button", { name: "Filter by status" }));
    expect(
      await screen.findByRole("menuitemcheckbox", { name: "Archived" }),
    ).toHaveAttribute("aria-checked", "true");
  });

  it("'Show all' in the Status menu leaves the archived list", async () => {
    mockBuckets([LIVE, FROZEN], [RETIRED]);

    const router = renderSitesPage("/sites?view=grid&archived=true");
    await screen.findByRole(
      "link",
      { name: "Retired" },
      { timeout: FIND_TIMEOUT },
    );

    openMenu(screen.getByRole("button", { name: "Filter by status" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Show all" }));
    await closeMenu();

    expect(
      await screen.findByRole("link", { name: "Live" }),
    ).toBeInTheDocument();
    expect(router.state.location.search).not.toHaveProperty("archived");
  });
});
