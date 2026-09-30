import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import type { ContentInventoryPage, ContentInventoryRow, Me } from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";

import { Route as ContentRoute } from "./$siteId.content";

// Field names and states are the generated client's ContentInventoryPage and
// ContentInventoryRow (packages/openapi-client/src/generated/types.gen.ts). The
// 429 comes from RefreshSiteContentInventoryErrors, its Retry-After header from
// the operation's contract.

const getInv = vi.fn();
const refreshInv = vi.fn();

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return {
    ...actual,
    getSiteContentInventory: (...a: unknown[]): unknown => getInv(...a),
    refreshSiteContentInventory: (...a: unknown[]): unknown => refreshInv(...a),
  };
});

const ME_KEY = ["test", "me"] as const;
vi.mock("@/features/auth/use-auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/auth/use-auth")>();
  const { useQuery } = await import("@tanstack/react-query");
  return {
    ...actual,
    useMe: () => useQuery({ queryKey: ME_KEY, queryFn: () => null, staleTime: Infinity }),
  };
});
vi.mock("@/features/sites/use-sites", () => ({
  useSite: () => ({ data: { url: "https://shop.example.com" } }),
}));

const TENANT = "00000000-0000-0000-0000-0000000000aa";
function meWithRole(role: "operator" | "viewer"): Me {
  return {
    user: { id: "u", email: "a@b.test", name: "A" },
    active_tenant_id: TENANT,
    memberships: [{ tenant_id: TENANT, role, tenant_name: "Acme" }],
  } as unknown as Me;
}

function row(over: Partial<ContentInventoryRow>): ContentInventoryRow {
  return {
    post_id: 1,
    post_type: "page",
    post_status: "publish",
    verdict: "classic",
    route_number: 1,
    route_reason: "content_column",
    editor: null,
    title: "About us",
    checked_at: "2026-09-30T09:40:00Z",
    ...over,
  };
}

function page(over: Partial<ContentInventoryPage>): ContentInventoryPage {
  return {
    state: "ok",
    min_agent_version: "0.62.0",
    last_checked_at: "2026-09-30T09:40:00Z",
    titles_included: true,
    truncated: false,
    next_after_post_id: null,
    pages: [row({})],
    ...over,
  };
}

function ok(data: ContentInventoryPage) {
  return { data, error: undefined, response: { status: 200 } };
}

// A site-scoped collaborator has no org membership; Me.scope/Me.role carry the
// share role (the same shape use-auth's canWriteSiteContext reads).
function siteScopedMe(role: "operator" | "viewer"): Me {
  return { scope: "site", role, memberships: [] } as unknown as Me;
}

function renderTab(
  role: "operator" | "viewer" | "site-operator" | "site-viewer" = "operator",
) {
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(
    ME_KEY,
    role === "site-operator"
      ? siteScopedMe("operator")
      : role === "site-viewer"
        ? siteScopedMe("viewer")
        : meWithRole(role),
  );
  const rootRoute = createRootRoute({});
  type UpdateOptions = Parameters<typeof ContentRoute.update>[0];
  const contentRoute = ContentRoute.update({
    id: "/sites/$siteId/content",
    path: "/sites/$siteId/content",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const router = createRouter({
    routeTree: rootRoute.addChildren([contentRoute]),
    history: createMemoryHistory({ initialEntries: ["/sites/site-1/content"] }),
  });
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
}

beforeEach(() => {
  getInv.mockReset();
  refreshInv.mockReset();
});

describe("site Content tab", () => {
  it("renders rows with editor, refusal reason and the S1 fixed answer", async () => {
    getInv.mockResolvedValue(
      ok(
        page({
          pages: [
            row({ post_id: 1, title: "Spring sale", verdict: "builder", route_number: 2, route_reason: "vendor_ability", editor: { integration_id: "beaver", display_name: "Beaver Builder", version: "2.11.2" } }),
            row({ post_id: 2, title: "Home", verdict: "block_document", route_reason: "block_editor_unsupported" }),
          ],
        }),
      ),
    );
    renderTab();
    expect(await screen.findByText("Spring sale")).toBeInTheDocument();
    expect(screen.getByText("Beaver Builder 2.11.2")).toBeInTheDocument();
    expect(screen.getByText("Block editor")).toBeInTheDocument();
    expect(screen.getAllByText("Not yet: AI editing isn't switched on")).toHaveLength(2);
    expect(screen.getByText("Later: no, block editor pages are not supported")).toBeInTheDocument();
  });

  it("hides titles and says why when titles_included is false", async () => {
    getInv.mockResolvedValue(
      ok(page({ titles_included: false, pages: [row({ post_id: 412, title: "Secret title" })] })),
    );
    renderTab("viewer");
    expect(await screen.findByText("Page #412")).toBeInTheDocument();
    expect(screen.queryByText("Secret title")).not.toBeInTheDocument();
    expect(screen.getByText(/titles need operator access/i)).toBeInTheDocument();
  });

  it("shows Not available yet for an unknown verdict and reason", async () => {
    getInv.mockResolvedValue(
      ok(page({ pages: [row({ verdict: "from_the_future", route_reason: "new_reason_code" })] })),
    );
    renderTab();
    await screen.findByText("About us");
    expect(screen.getAllByText("Not available yet")).toHaveLength(2);
    expect(screen.queryByText("new_reason_code")).not.toBeInTheDocument();
  });

  it("renders a site-supplied title as text, not HTML", async () => {
    getInv.mockResolvedValue(
      ok(page({ pages: [row({ title: "<img src=x onerror=alert(1)>" })] })),
    );
    renderTab();
    expect(await screen.findByText("<img src=x onerror=alert(1)>")).toBeInTheDocument();
    expect(document.querySelector("img")).toBeNull();
  });

  it("agent_update_needed names the minimum version", async () => {
    getInv.mockResolvedValue(ok(page({ state: "agent_update_needed", pages: [] })));
    renderTab();
    expect(
      await screen.findByText(/Update the WPMgr plugin on this site to version 0\.62\.0 to see its pages\./),
    ).toBeInTheDocument();
  });

  it("not_connected", async () => {
    getInv.mockResolvedValue(ok(page({ state: "not_connected", pages: [] })));
    renderTab();
    expect(await screen.findByText(/not connected right now/i)).toBeInTheDocument();
  });

  it("never refreshed shows the empty state with a Refresh button", async () => {
    getInv.mockResolvedValue(ok(page({ last_checked_at: null, pages: [] })));
    renderTab();
    expect(await screen.findByText(/have not been checked yet/i)).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: "Refresh" }).length).toBeGreaterThan(0);
  });

  it("shows a loading state, then an error with retry", async () => {
    getInv.mockReturnValue(new Promise(() => {}));
    renderTab();
    expect(await screen.findByRole("status", { name: "Loading pages" })).toBeInTheDocument();
  });

  it("shows an error state when the request fails", async () => {
    getInv.mockResolvedValue({ data: undefined, error: { code: "internal", message: "boom" }, response: { status: 500 } });
    renderTab();
    expect(await screen.findByText("Could not load this site's pages")).toBeInTheDocument();
  });

  it("a 429 on refresh says to try again in N seconds", async () => {
    getInv.mockResolvedValue(ok(page({})));
    refreshInv.mockResolvedValue({
      data: undefined,
      error: { code: "rate_limited", message: "slow down" },
      response: new Response(null, { status: 429, headers: { "Retry-After": "42" } }),
    });
    renderTab();
    await screen.findByText("About us");
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    expect(
      await screen.findByText("Checked recently. Try again in 42 seconds."),
    ).toBeInTheDocument();
  });

  it("shows Refresh to a site-scoped operator (no org membership)", async () => {
    getInv.mockResolvedValue(ok(page({})));
    renderTab("site-operator");
    await screen.findByText("About us");
    expect(screen.getByRole("button", { name: "Refresh" })).toBeInTheDocument();
  });

  it("hides Refresh from a site-scoped viewer", async () => {
    getInv.mockResolvedValue(ok(page({})));
    renderTab("site-viewer");
    await screen.findByText("About us");
    expect(screen.queryByRole("button", { name: "Refresh" })).not.toBeInTheDocument();
  });

  it("hides Refresh from a viewer", async () => {
    getInv.mockResolvedValue(ok(page({})));
    renderTab("viewer");
    await screen.findByText("About us");
    expect(screen.queryByRole("button", { name: "Refresh" })).not.toBeInTheDocument();
  });

  it("maps the editor filter to the API editor param", async () => {
    getInv.mockResolvedValue(ok(page({})));
    renderTab();
    await screen.findByText("About us");
    fireEvent.change(screen.getByLabelText("Show"), { target: { value: "classic" } });
    await waitFor(() =>
      expect(getInv).toHaveBeenLastCalledWith(
        expect.objectContaining({ query: expect.objectContaining({ editor: "classic" }) as unknown }),
      ),
    );
  });

  it("search filters the loaded page by title", async () => {
    getInv.mockResolvedValue(
      ok(page({ pages: [row({ post_id: 1, title: "Spring sale" }), row({ post_id: 2, title: "About us" })] })),
    );
    renderTab();
    await screen.findByText("Spring sale");
    fireEvent.change(screen.getByLabelText(/^Search/), { target: { value: "spring" } });
    expect(screen.queryByText("About us")).not.toBeInTheDocument();
    expect(screen.getByText("Spring sale")).toBeInTheDocument();
  });

  it("a site checked with zero pages says when and that none were found", async () => {
    getInv.mockResolvedValue(ok(page({ pages: [] })));
    renderTab();
    expect(
      await screen.findByText(/WPMgr checked this site .* and found no published pages\./),
    ).toBeInTheDocument();
    expect(screen.queryByText(/have not been checked yet/i)).not.toBeInTheDocument();
  });

  it("the classic filter option is labelled No page builder", async () => {
    getInv.mockResolvedValue(ok(page({})));
    renderTab();
    await screen.findByText("About us");
    expect(screen.getByRole("option", { name: "No page builder" })).toHaveValue("classic");
    expect(screen.queryByRole("option", { name: "WordPress (classic)" })).not.toBeInTheDocument();
  });

  it("shows the truncation note only when truncated is true", async () => {
    getInv.mockResolvedValue(ok(page({ truncated: true })));
    renderTab();
    expect(
      await screen.findByText("Showing the first 5,000 pages WPMgr checked on this site."),
    ).toBeInTheDocument();
  });

  it("does not show the truncation note otherwise", async () => {
    getInv.mockResolvedValue(ok(page({})));
    renderTab();
    await screen.findByText("About us");
    expect(screen.queryByText(/Showing the first 5,000/)).not.toBeInTheDocument();
  });

  it("hints that search covers only the loaded page", async () => {
    getInv.mockResolvedValue(ok(page({})));
    renderTab();
    expect(await screen.findByText("Searches this page of results")).toBeInTheDocument();
  });

  it("after a queued refresh it refetches and no longer claims the list updates when the site replies", async () => {
    getInv.mockResolvedValue(ok(page({})));
    refreshInv.mockResolvedValue({ data: { status: "queued" }, error: undefined, response: { status: 202 } });
    renderTab();
    await screen.findByText("About us");
    const before = getInv.mock.calls.length;
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    expect(await screen.findByText(/refreshes on its own/)).toBeInTheDocument();
    await waitFor(() => expect(getInv.mock.calls.length).toBeGreaterThan(before));
    expect(screen.queryByText(/when the site replies/)).not.toBeInTheDocument();
  });
});
