import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { screen, within } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import type { QueryClient } from "@tanstack/react-query";
import type { FleetAiReadinessSite, Me, Site } from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";
import { authKeys } from "@/features/auth/use-auth";
import { failResult, fleetSite, okResult } from "@/features/ai-readiness/readiness-fixtures";

import { Route as SitesIndexRoute } from "./index";
import { useSites, type UseSitesOptions } from "@/features/sites/use-sites";
import { useClients } from "@/features/clients/use-clients";
import { useTags } from "@/features/tags/use-tags";
import { useSitesLiveSync } from "@/features/sites/use-sites-live";

// The "AI" column on the Sites list and the matching chip on grid cards, driven
// through the real route, a real router and a real QueryClient. Only the
// network edge (the generated SDK) is stubbed. The rollup's shape is
// FleetAiReadiness in packages/openapi-client/src/generated/types.gen.ts, which
// apps/api/internal/aireadiness/dto.go (fleetSiteDTO) writes.
//
// The list view's body is windowed by react-virtuoso, which mounts no rows in
// jsdom's zero-height layout (see the note in gh414-pause-real-tree.test.tsx).
// It is replaced below by a stand-in that renders the header and every row, so
// the table's own column definitions, header and cells run for real.

vi.mock("react-virtuoso", async () => {
  const React = await import("react");
  type Props = {
    data: unknown[];
    fixedHeaderContent?: () => React.ReactNode;
    itemContent: (index: number, item: unknown) => React.ReactNode;
  };
  return {
    TableVirtuoso: ({ data, fixedHeaderContent, itemContent }: Props) =>
      React.createElement(
        "table",
        null,
        React.createElement("thead", null, fixedHeaderContent?.()),
        React.createElement(
          "tbody",
          null,
          data.map((item, i) => React.createElement("tr", { key: i }, itemContent(i, item))),
        ),
      ),
  };
});

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

const getFleetReadiness = vi.fn();
const getFleetAgents = vi.fn();
vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return {
    ...actual,
    getFleetAiReadiness: (...a: unknown[]): unknown => getFleetReadiness(...a),
    getFleetAgentVersions: (...a: unknown[]): unknown => getFleetAgents(...a),
  };
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

const S_READY = "aaaaaaaa-0000-0000-0000-000000000001";
const S_FIX = "bbbbbbbb-0000-0000-0000-000000000002";
const S_UNCHECKED = "cccccccc-0000-0000-0000-000000000003";
const S_WARNED = "dddddddd-0000-0000-0000-000000000004";
const S_BUILDERS = "eeeeeeee-0000-0000-0000-000000000005";

function buildSite(overrides: Partial<Site> = {}): Site {
  return {
    id: S_READY,
    tenant_id: "t1",
    url: "https://ready.example.com",
    name: "Acme Shop",
    status: "active",
    wp_version: "7.1",
    php_version: "8.3",
    health_status: "healthy",
    multisite: false,
    tags: [],
    ...overrides,
  } as unknown as Site;
}

const SITES: Site[] = [
  buildSite({ id: S_READY, name: "Acme Shop", url: "https://acme.example.com" }),
  buildSite({ id: S_FIX, name: "Fixit Ltd", url: "https://fixit.example.com" }),
  buildSite({ id: S_UNCHECKED, name: "Pending Inc", url: "https://pending.example.com" }),
  buildSite({ id: S_WARNED, name: "Beta Co", url: "https://beta.example.com" }),
  buildSite({ id: S_BUILDERS, name: "Builders Ltd", url: "https://builders.example.com" }),
];

const ROLLUP: FleetAiReadinessSite[] = [
  fleetSite({ site_id: S_READY }),
  fleetSite({
    site_id: S_FIX,
    status: "needs_attention",
    fix_count: 2,
    failing: ["wp_version", "content_editing"],
  }),
  fleetSite({ site_id: S_UNCHECKED, status: "incomplete" }),
  fleetSite({ site_id: S_WARNED, warnings: ["mcp_adapter_plugin_active", "elementor_mcp_endpoint_open"] }),
  // A builder version row can only be in `failing` when the builder is too old:
  // one that is installed but not active is not_applicable and is not listed
  // (AIReadinessCheck in packages/openapi/openapi.yaml).
  fleetSite({
    site_id: S_BUILDERS,
    status: "needs_attention",
    fix_count: 2,
    failing: ["elementor_version", "bricks_version"],
  }),
];

function buildSitesRouter(initialPath: string, queryClient: QueryClient) {
  const rootRoute = createRootRoute({});
  type UpdateOptions = Parameters<typeof SitesIndexRoute.update>[0];
  const sitesRoute = SitesIndexRoute.update({
    id: "/sites",
    path: "/sites",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  return createRouter({
    routeTree: rootRoute.addChildren([sitesRoute]),
    context: { queryClient },
    history: createMemoryHistory({ initialEntries: [initialPath] }),
  });
}

function renderSitesPage(initialPath = "/sites") {
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(authKeys.me, OWNER_ME);
  renderWithProviders(<RouterProvider router={buildSitesRouter(initialPath, queryClient)} />, {
    queryClient,
  });
}

const FIND_TIMEOUT = 5000;

beforeEach(() => {
  mockedUseClients.mockReturnValue(mockQueryResult({ data: [] }));
  mockedUseTags.mockReturnValue(mockQueryResult({ data: [] }));
  mockedUseSitesLiveSync.mockReturnValue(undefined);
  mockedUseSites.mockImplementation((options?: UseSitesOptions) =>
    mockQueryResult<Site[]>({ data: options?.view === "archived" ? [] : SITES }),
  );
  getFleetReadiness.mockReset();
  getFleetReadiness.mockResolvedValue(okResult({ sites: ROLLUP }));
  // The Agent column's own rollup is out of scope here; refuse it the way a
  // site collaborator's would be refused.
  getFleetAgents.mockReset();
  getFleetAgents.mockResolvedValue(failResult(403, "insufficient_permission", "your role does not permit this action"));
});

afterEach(() => {
  window.localStorage.clear();
});

/** The row for a site, found by its link text, in either view. */
async function rowFor(name: string): Promise<HTMLElement> {
  const link = await screen.findByRole("link", { name }, { timeout: FIND_TIMEOUT });
  const row = link.closest("tr");
  if (!row) throw new Error(`no table row around ${name}`);
  return row;
}

describe("Sites list, table view: the AI column", () => {
  it("sits right after Agent and is headed AI", async () => {
    renderSitesPage("/sites");
    await rowFor("Acme Shop");
    const headers = screen.getAllByRole("columnheader").map((th) => th.textContent?.trim() ?? "");
    const agent = headers.findIndex((t) => t.startsWith("Agent"));
    expect(agent).toBeGreaterThan(-1);
    expect(headers[agent + 1]).toBe("AI");
  });

  it("shows Ready, '2 to fix' and Not checked, each linking to the site's Content tab", async () => {
    renderSitesPage("/sites");

    const ready = await within(await rowFor("Acme Shop")).findByRole("link", { name: /^Ready/ });
    expect(ready).toHaveTextContent("Ready");
    expect(ready).toHaveAttribute("href", `/sites/${S_READY}/content`);

    const fix = await within(await rowFor("Fixit Ltd")).findByRole("link", { name: /to fix/ });
    expect(fix).toHaveTextContent("2 to fix");
    expect(fix).toHaveAttribute("href", `/sites/${S_FIX}/content`);

    const unchecked = await within(await rowFor("Pending Inc")).findByRole("link", { name: /Not checked/ });
    expect(unchecked).toHaveTextContent("Not checked");
    expect(unchecked).toHaveAttribute("href", `/sites/${S_UNCHECKED}/content`);
  });

  it("lists what to fix, one per line, on hover", async () => {
    renderSitesPage("/sites");
    const fix = await within(await rowFor("Fixit Ltd")).findByRole("link", { name: /to fix/ });
    expect(fix.getAttribute("title")).toBe("WordPress version\nAI page creation");
    expect(fix).toHaveAccessibleDescription("WordPress version. AI page creation");
  });

  it("words a builder's version line as the fix, never as a bare 'version' row", async () => {
    renderSitesPage("/sites");
    const link = await within(await rowFor("Builders Ltd")).findByRole("link", { name: /to fix/ });
    expect(link).toHaveTextContent("2 to fix");
    expect(link.getAttribute("title")).toBe("Elementor needs updating\nBricks needs updating");
    expect(link.getAttribute("title")).not.toMatch(/version/i);
  });

  it("marks an open AI connection point with an amber triangle and names it on hover, leaving Ready alone", async () => {
    renderSitesPage("/sites");
    const row = within(await rowFor("Beta Co"));
    const link = await row.findByRole("link", { name: /^Ready/ });
    expect(link).toHaveTextContent("Ready");
    expect(within(link).getByRole("img", { name: "Open AI connection point" })).toBeInTheDocument();
    expect(link.getAttribute("title")).toContain("Open AI connection point: MCP Adapter plugin");
    expect(link.getAttribute("title")).toContain("Open AI connection point: Elementor switch");

    // And the other rows carry no triangle.
    const readyRow = within(await rowFor("Acme Shop"));
    expect(readyRow.queryByRole("img", { name: "Open AI connection point" })).not.toBeInTheDocument();
  });

  it("shows a dash for a site the rollup does not list, and results for the rest", async () => {
    getFleetReadiness.mockResolvedValue(okResult({ sites: ROLLUP.filter((s) => s.site_id !== S_UNCHECKED) }));
    renderSitesPage("/sites");
    const missing = within(await rowFor("Pending Inc"));
    expect(await missing.findByRole("img", { name: "AI readiness unavailable" })).toBeInTheDocument();
    expect(missing.queryByText("Not checked")).not.toBeInTheDocument();
    expect(await within(await rowFor("Acme Shop")).findByText("Ready")).toBeInTheDocument();
  });

  // 403 insufficient_permission: apps/api/internal/authz/middleware.go.
  it("shows a dash in every cell, and no page error, when the rollup is refused", async () => {
    getFleetReadiness.mockResolvedValue(
      failResult(403, "insufficient_permission", "your role does not permit this action"),
    );
    renderSitesPage("/sites");
    await rowFor("Acme Shop");
    expect(await screen.findAllByRole("img", { name: "AI readiness unavailable" })).toHaveLength(SITES.length);
    expect(screen.queryByText("Ready")).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    // The list itself is untouched.
    for (const site of SITES) expect(screen.getByRole("link", { name: site.name })).toBeInTheDocument();
  });

  it("shows a dash in every cell, and no page error, when the rollup request fails outright", async () => {
    getFleetReadiness.mockRejectedValue(new TypeError("Failed to fetch"));
    renderSitesPage("/sites");
    await rowFor("Acme Shop");
    expect(await screen.findAllByRole("img", { name: "AI readiness unavailable" })).toHaveLength(SITES.length);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("shows no result and no dash while the rollup is still on its way", async () => {
    getFleetReadiness.mockReturnValue(new Promise(() => {}));
    renderSitesPage("/sites");
    const row = within(await rowFor("Acme Shop"));
    expect(row.queryByText("Ready")).not.toBeInTheDocument();
    expect(row.queryByRole("img", { name: "AI readiness unavailable" })).not.toBeInTheDocument();
  });
});

describe("Sites list, grid view: the AI chip", () => {
  async function cardFor(name: string): Promise<HTMLElement> {
    return await screen.findByRole("article", { name }, { timeout: FIND_TIMEOUT });
  }

  it("shows the same states on each card", async () => {
    renderSitesPage("/sites?view=grid");

    const ready = await within(await cardFor("Acme Shop")).findByRole("link", { name: /Ready/ });
    expect(ready).toHaveTextContent("AI");
    expect(ready).toHaveTextContent("Ready");
    expect(ready).toHaveAttribute("href", `/sites/${S_READY}/content`);

    expect(
      await within(await cardFor("Fixit Ltd")).findByRole("link", { name: /to fix/ }),
    ).toHaveTextContent("2 to fix");
    expect(
      await within(await cardFor("Pending Inc")).findByRole("link", { name: /Not checked/ }),
    ).toHaveTextContent("Not checked");
    const warned = within(await cardFor("Beta Co"));
    expect(await warned.findByRole("img", { name: "Open AI connection point" })).toBeInTheDocument();
  });

  it("shows a dash on every card, and no page error, when the rollup is refused", async () => {
    getFleetReadiness.mockResolvedValue(
      failResult(403, "insufficient_permission", "your role does not permit this action"),
    );
    renderSitesPage("/sites?view=grid");
    await cardFor("Acme Shop");
    expect(await screen.findAllByRole("img", { name: "AI readiness unavailable" })).toHaveLength(SITES.length);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});
