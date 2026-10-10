import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
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

// What the Disconnect confirmation on the Sites list tells the operator.
// Disconnect only revokes the agent's access (POST /api/v1/sites/{id}/revoke,
// connection_handler.go): the site stays in the default list, labelled as
// disconnected, with its history kept, and can then be reconnected or
// archived. Archiving is a separate action with its own confirmation, so this
// one must not say the site "is archived".
//
// Mounts the REAL route component (the file's own `Route` singleton re-attached
// to a throwaway root, the technique -index.search-sort.test.tsx uses) and
// opens the dialog from the grid card's Disconnect action. The page's data
// hooks are stubbed like that file does; the dialog text under test is the
// page's own.
//
// The row menu is the one other piece replaced. Opening a Dialog from a Radix
// DropdownMenuItem recurses to "Maximum call stack size exceeded" under jsdom
// and never settles (see the note in -siteId-pause.test.tsx), so the card's
// actions are a single button that calls the same `onDisconnect(site)` the real
// menu item calls. Whether the real item calls it is site-row-actions.test.tsx.

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
vi.mock("@/features/sites/site-row-actions", () => ({
  SiteRowActions: ({
    site,
    onDisconnect,
  }: {
    site: Site;
    onDisconnect?: (site: Site) => void;
  }) =>
    onDisconnect ? (
      <button type="button" onClick={() => onDisconnect(site)}>
        Disconnect {site.name}
      </button>
    ) : null,
}));

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
    enrolled: true,
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

function renderSitesPage(initialPath: string) {
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(authKeys.me, OWNER_ME);
  const router = buildSitesRouter(initialPath, queryClient);
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  return router;
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
});

describe("Sites page: the Disconnect confirmation", () => {
  it("says the site stays in the list as disconnected with its history kept, not that it is archived", async () => {
    renderSitesPage("/sites?view=grid");

    fireEvent.click(
      await screen.findByRole(
        "button",
        { name: "Disconnect Acme" },
        { timeout: FIND_TIMEOUT },
      ),
    );

    const dialog = await screen.findByRole(
      "dialog",
      {},
      { timeout: FIND_TIMEOUT },
    );
    expect(dialog).toHaveTextContent("Disconnect acme.example.com");
    expect(dialog).toHaveTextContent(
      "Backups and monitoring stop. The site stays in your sites list as disconnected, with its full history kept. You can reconnect it or archive it later.",
    );
    expect(dialog).not.toHaveTextContent(/is archived/i);
  });
});
