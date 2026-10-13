import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, renderHook, screen, waitFor, within } from "@testing-library/react";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import type { FleetAgentVersions, Me, Site } from "@wpmgr/api";

import { authKeys } from "@/features/auth/use-auth";
import { useClients } from "@/features/clients/use-clients";
import { useFleetAgentVersions } from "@/features/fleet/use-fleet-agents";
import { sitesKeys } from "@/features/sites/use-sites";
import { useSitesSelection } from "@/features/sites/use-sites-selection";
import { useSitesLiveSync } from "@/features/sites/use-sites-live";
import type { SitesToolbarProps } from "@/features/sites/sites-toolbar";
import { useTags } from "@/features/tags/use-tags";
import { mockQueryResult } from "@/test/query-mocks";
import { createTestQueryClient, renderWithProviders } from "@/test/render";

import { Route as SitesIndexRoute } from "./index";

type DeleteSiteRequest = { path: { siteId: string } };
type DeleteSiteResult = {
  response: { status: number };
  error?: { code: string; message: string };
};

const { listSitesMock, deleteSiteMock, toastSuccess } = vi.hoisted(() => ({
  listSitesMock: vi.fn(),
  deleteSiteMock: vi.fn<(options: DeleteSiteRequest) => Promise<DeleteSiteResult>>(),
  toastSuccess: vi.fn(),
}));

vi.mock("@wpmgr/api", async (importOriginal) => ({
  ...await importOriginal<typeof import("@wpmgr/api")>(),
  listSites: listSitesMock, deleteSite: deleteSiteMock,
}));
vi.mock("@/components/toast", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/components/toast")>();
  return { ...actual, toast: { ...actual.toast, success: toastSuccess } };
});
vi.mock("@/features/clients/use-clients", async (importOriginal) => ({
  ...await importOriginal<typeof import("@/features/clients/use-clients")>(), useClients: vi.fn(),
}));
vi.mock("@/features/tags/use-tags", async (importOriginal) => ({
  ...await importOriginal<typeof import("@/features/tags/use-tags")>(), useTags: vi.fn(),
}));
vi.mock("@/features/fleet/use-fleet-agents", async (importOriginal) => ({
  ...await importOriginal<typeof import("@/features/fleet/use-fleet-agents")>(), useFleetAgentVersions: vi.fn(),
}));
vi.mock("@/features/sites/use-sites-live", async (importOriginal) => ({
  ...await importOriginal<typeof import("@/features/sites/use-sites-live")>(), useSitesLiveSync: vi.fn(),
}));
// The menu trigger is a plain button to keep Radix focus handling out of this
// route test. The page, selection, dialog, list and delete hooks run unchanged.
vi.mock("@/features/sites/sites-toolbar", async (importOriginal) => ({
  ...await importOriginal<typeof import("@/features/sites/sites-toolbar")>(),
  SitesToolbar: ({ selection, onBulkDelete }: SitesToolbarProps) => (
    <button type="button" onClick={onBulkDelete} disabled={selection.count === 0}>
      Delete selected sites
    </button>
  ),
}));

const OWNER_ME: Me = {
  user: {
    id: "00000000-0000-0000-0000-000000000001", email: "owner@example.test", name: "Owner",
    created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z",
  },
  memberships: [{ user_id: "00000000-0000-0000-0000-000000000001", tenant_id: "t1", role: "owner" }],
  active_tenant_id: "t1", hosted: false,
};

function site(id: string, name: string): Site {
  return {
    id, name, tenant_id: "t1", url: `https://${name.toLowerCase()}.example.test`,
    status: "active", enrolled: true, connection_state: "connected", wp_version: "6.8", php_version: "8.3",
    health_status: "healthy", multisite: false, tags: [], components: { plugins: [], themes: [] },
  } as unknown as Site;
}
const ALPHA = site("11111111-0000-0000-0000-000000000001", "Alpha");
const BRAVO = site("11111111-0000-0000-0000-000000000002", "Bravo");
let backendSites: Site[];

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

function renderPage() {
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(authKeys.me, OWNER_ME);
  const rootRoute = createRootRoute({});
  type UpdateOptions = Parameters<typeof SitesIndexRoute.update>[0];
  const route = SitesIndexRoute.update({
    id: "/sites", path: "/sites", getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const router = createRouter({
    routeTree: rootRoute.addChildren([route]), context: { queryClient },
    history: createMemoryHistory({ initialEntries: ["/sites?view=grid"] }),
  });
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  return { queryClient, router };
}

async function openDelete(...names: string[]) {
  for (const name of names) {
    fireEvent.click(await screen.findByRole("checkbox", { name: `Select ${name}` }, { timeout: 5000 }));
  }
  fireEvent.click(screen.getByRole("button", { name: "Delete selected sites" }));
  const dialog = await screen.findByRole("dialog");
  fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: String(names.length) } });
  return dialog;
}

beforeEach(() => {
  const selection = renderHook(() => useSitesSelection());
  act(() => selection.result.current.clear());
  selection.unmount();
  backendSites = [ALPHA, BRAVO];
  toastSuccess.mockReset();
  listSitesMock.mockReset();
  listSitesMock.mockImplementation(({ query }: { query?: { state?: string } } = {}) => Promise.resolve({
    data: { items: query?.state === "archived" ? [] : backendSites }, response: { status: 200 },
  }));
  deleteSiteMock.mockReset();
  deleteSiteMock.mockImplementation(({ path }: { path: { siteId: string } }) => {
    backendSites = backendSites.filter((item) => item.id !== path.siteId);
    return Promise.resolve({ response: { status: 204 } });
  });
  vi.mocked(useClients).mockReturnValue(mockQueryResult({ data: [] }));
  vi.mocked(useTags).mockReturnValue(mockQueryResult({ data: [] }));
  vi.mocked(useFleetAgentVersions).mockReturnValue(mockQueryResult<FleetAgentVersions>({ data: undefined }));
  vi.mocked(useSitesLiveSync).mockReturnValue(undefined);
});

afterEach(() => { window.localStorage.clear(); });

describe("DS-02 site deletion through the router and QueryClient", () => {
  it("shows the pending label, spinner and duration hint while preventing dismissal or duplicate submits", async () => {
    const request = deferred<{ response: { status: number } }>();
    deleteSiteMock.mockReturnValue(request.promise);
    renderPage();
    const dialog = await openDelete("Alpha");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete site" }));
    const pending = await within(dialog).findByRole("button", { name: "Deleting…" });
    expect(pending).toHaveTextContent("Deleting…");
    expect(pending).toBeDisabled();
    expect(pending.querySelector(".animate-spin")).not.toBeNull();
    expect(within(dialog).getByRole("button", { name: "Keep sites" })).toBeDisabled();
    expect(dialog).toHaveTextContent("This can take a few seconds.");
    fireEvent.keyDown(dialog, { key: "Escape" });
    expect(dialog).toBeInTheDocument();
    expect(deleteSiteMock).toHaveBeenCalledTimes(1);
    backendSites = [BRAVO];
    await act(async () => {
      request.resolve({ response: { status: 204 } });
      await request.promise;
    });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("closes, reports Site deleted and removes the row even while list refetch is pending", async () => {
    const { queryClient, router } = renderPage();
    const dialog = await openDelete("Alpha");
    // Opt-in fault injection makes this regression fail without altering the
    // production hook. Run once with this flag and once without it.
    if (process.env.WPMGR_DS02_PLANT_ROW_DEFECT === "1") {
      vi.spyOn(queryClient, "setQueriesData").mockReturnValue([]);
    }
    const refetch = deferred<{ data: { items: Site[] }; response: { status: number } }>();
    listSitesMock.mockImplementation(() => refetch.promise);
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete site" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await waitFor(() => expect(screen.queryByRole("checkbox", { name: "Select Alpha" })).not.toBeInTheDocument());
    expect(screen.getByRole("checkbox", { name: "Select Bravo" })).toBeInTheDocument();
    expect(toastSuccess).toHaveBeenCalledWith("Site deleted");
    expect(router.state.location.pathname).toBe("/sites");
    for (const [, data] of queryClient.getQueriesData<Site[]>({ queryKey: sitesKeys.lists() })) {
      expect(data?.some((item) => item.id === ALPHA.id)).not.toBe(true);
    }
    await act(async () => {
      refetch.resolve({ data: { items: backendSites }, response: { status: 200 } });
      await refetch.promise;
    });
  });

  it("keeps a failure open with a plain error and the typed confirmation, then permits retry", async () => {
    // The shared HTTP error mapper returns this body for an unexpected 500:
    // apps/api/internal/server/httpx/respond.go.
    deleteSiteMock.mockResolvedValueOnce({ error: { code: "internal_error", message: "internal server error" }, response: { status: 500 } });
    renderPage();
    const dialog = await openDelete("Alpha");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete site" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("The site could not be deleted. Please try again.");
    expect(dialog).toBeInTheDocument();
    expect(within(dialog).getByRole("textbox")).toHaveValue("1");
    expect(within(dialog).getByRole("button", { name: "Delete site" })).toBeEnabled();
    expect(toastSuccess).not.toHaveBeenCalled();
    expect(screen.getByLabelText("Select Alpha")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete site" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(deleteSiteMock).toHaveBeenCalledTimes(2);
    expect(toastSuccess).toHaveBeenCalledWith("Site deleted");
  });

  it("keeps only failed sites selected after a partial bulk failure and never retries a successful delete", async () => {
    deleteSiteMock.mockImplementation(({ path }: { path: { siteId: string } }) => {
      if (path.siteId === BRAVO.id) return Promise.resolve({ error: { code: "internal_error", message: "internal server error" }, response: { status: 500 } });
      backendSites = backendSites.filter((item) => item.id !== path.siteId);
      return Promise.resolve({ response: { status: 204 } });
    });
    renderPage();
    const dialog = await openDelete("Alpha", "Bravo");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete sites" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("The site could not be deleted. Please try again.");
    expect(dialog).toBeInTheDocument();
    expect(dialog).toHaveTextContent("Delete 1 site");
    expect(deleteSiteMock).toHaveBeenCalledTimes(2);
    deleteSiteMock.mockImplementation(({ path }: { path: { siteId: string } }) => {
      backendSites = backendSites.filter((item) => item.id !== path.siteId);
      return Promise.resolve({ response: { status: 204 } });
    });
    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "1" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete site" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(deleteSiteMock.mock.calls.map(([args]) => args.path.siteId)).toEqual([ALPHA.id, BRAVO.id, BRAVO.id]);
  });
});
