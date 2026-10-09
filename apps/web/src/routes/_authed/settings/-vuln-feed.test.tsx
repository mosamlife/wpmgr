import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import type { QueryClient } from "@tanstack/react-query";
import { client, type Me } from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { authKeys } from "@/features/auth/use-auth";

import { Route as SettingsLayoutRoute } from "./route";
import { Route as VulnFeedSettingsRoute } from "./vuln-feed";

// GH #361. The vulnerability feed key is instance-wide and follows the same
// authority as the instance email settings, so the owner of a single-
// organisation install can set it from Settings. Settings has an entry beside
// Email / SMTP that follows `me.can_manage_instance_email`, and a page that
// refuses to mount the panel for anyone else.
//
// The real settings layout route and the real page route are re-attached to a
// throwaway root (the technique routes/_authed/sites/-index.test.tsx uses), so
// the test goes through the router's matching, the layout's nav filter and the
// page's own gate. Only the HTTP client is mocked: the feed hooks, the query
// cache and the panel are the real ones.
//
// Request shapes below are the ones the hooks send to the routes in
// apps/api/internal/admin/handler.go (GET /status, PUT and DELETE /key, POST
// /sync under /api/v1/admin/vuln-feed); the response fields are the DTO in
// features/admin/use-admin-vuln-feed.ts.

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return {
    ...actual,
    client: { get: vi.fn(), put: vi.fn(), delete: vi.fn(), post: vi.fn() },
  };
});

const mockedGet = vi.mocked(client.get);
const mockedPut = vi.mocked(client.put);

const TENANT_ID = "00000000-0000-0000-0000-0000000000aa";
const USER_ID = "00000000-0000-0000-0000-000000000002";

const OWNER_ME: Me = {
  user: {
    id: USER_ID,
    email: "owner@wpmgr.test",
    name: "Owner",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  },
  memberships: [{ user_id: USER_ID, tenant_id: TENANT_ID, role: "owner" }],
  active_tenant_id: TENANT_ID,
};

const SUPERADMIN_NO_ORG_ME: Me = {
  user: {
    id: "00000000-0000-0000-0000-000000000001",
    email: "superadmin@wpmgr.test",
    name: "Superadmin",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    is_superadmin: true,
  },
  memberships: [],
};

const STATUS_NOT_CONFIGURED = {
  configured: false,
  source: "none",
  feed_ok: false,
  record_count: 0,
  last_synced: null,
  last_error: "",
  enrichment_available: false,
};

/** What a mocked client call resolves to when the request succeeds. */
function ok(data: unknown) {
  return { data, error: undefined, response: new Response(null, { status: 200 }) };
}

function buildRouter(initialPath: string, queryClient: QueryClient) {
  const rootRoute = createRootRoute({});
  type LayoutUpdate = Parameters<typeof SettingsLayoutRoute.update>[0];
  type PageUpdate = Parameters<typeof VulnFeedSettingsRoute.update>[0];
  const settingsRoute = SettingsLayoutRoute.update({
    id: "/settings",
    path: "/settings",
    getParentRoute: () => rootRoute,
  } as unknown as LayoutUpdate);
  const vulnFeedRoute = VulnFeedSettingsRoute.update({
    id: "/settings/vuln-feed",
    path: "/vuln-feed",
    getParentRoute: () => settingsRoute,
  } as unknown as PageUpdate);
  // A neighbouring settings page to start from, so a click on the nav entry is
  // a real navigation between two matched routes.
  const accountRoute = createRoute({
    getParentRoute: () => settingsRoute,
    path: "/account",
    component: () => <p>Account settings page</p>,
  });
  const routeTree = rootRoute.addChildren([
    settingsRoute.addChildren([accountRoute, vulnFeedRoute]),
  ]);
  return createRouter({
    routeTree,
    context: { queryClient },
    history: createMemoryHistory({ initialEntries: [initialPath] }),
  });
}

function renderSettings(initialPath: string, me: Me) {
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(authKeys.me, me);
  const router = buildRouter(initialPath, queryClient);
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  return router;
}

beforeEach(() => {
  vi.clearAllMocks();
  mockedGet.mockResolvedValue(ok(STATUS_NOT_CONFIGURED) as never);
});

describe("Settings > Vulnerability feed for an owner the server admits (can_manage_instance_email)", () => {
  it("lists the entry beside Email / SMTP and opens the feed settings from it", async () => {
    renderSettings("/settings/account", { ...OWNER_ME, can_manage_instance_email: true });

    const smtp = await screen.findByRole("link", { name: "Email / SMTP" });
    const feed = await screen.findByRole("link", { name: "Vulnerability feed" });
    expect(feed).toHaveAttribute("href", "/settings/vuln-feed");
    // "Beside": the entry directly follows Email / SMTP in the settings nav.
    const labels = screen
      .getAllByRole("link")
      .map((a) => (a.textContent ?? "").trim());
    expect(labels.indexOf("Vulnerability feed")).toBe(labels.indexOf("Email / SMTP") + 1);
    expect(smtp).toHaveAttribute("href", "/settings/smtp");

    fireEvent.click(feed);

    expect(
      await screen.findByRole("heading", { name: "Vulnerability feed" }),
    ).toBeInTheDocument();
    expect(await screen.findByText("Not configured")).toBeInTheDocument();
    expect(mockedGet).toHaveBeenCalledWith({ url: "/api/v1/admin/vuln-feed/status" });
    expect(screen.getByRole("link", { name: "Vulnerability feed" })).toHaveAttribute(
      "aria-current",
      "page",
    );
  });

  it("saves a pasted key through the feed route and clears the field", async () => {
    mockedPut.mockResolvedValue(ok({ ok: true, syncing: true }) as never);

    renderSettings("/settings/vuln-feed", { ...OWNER_ME, can_manage_instance_email: true });

    const input = await screen.findByLabelText("Wordfence Intelligence API key");
    fireEvent.change(input, { target: { value: "wf-key-123" } });
    fireEvent.click(screen.getByRole("button", { name: "Save key" }));

    await waitFor(() => expect(mockedPut).toHaveBeenCalledTimes(1));
    expect(mockedPut).toHaveBeenCalledWith(
      expect.objectContaining({
        url: "/api/v1/admin/vuln-feed/key",
        body: { key: "wf-key-123" },
      }),
    );
    // The key is write-only: the field is emptied once it is saved.
    await waitFor(() => expect(input).toHaveValue(""));
  });
});

describe("Settings > Vulnerability feed for everyone else", () => {
  it("hides the entry from an owner the server does not admit, while org-only entries still show", async () => {
    renderSettings("/settings/account", { ...OWNER_ME, can_manage_instance_email: false });

    // An org-scoped item renders, so the missing entry below is the capability
    // gate and not an empty nav.
    await screen.findByRole("link", { name: "Organisation" });
    expect(screen.queryByRole("link", { name: "Vulnerability feed" })).not.toBeInTheDocument();
  });

  it("says only the instance administrator can change it, and sends no request, when the page is opened by URL", async () => {
    renderSettings("/settings/vuln-feed", { ...OWNER_ME, can_manage_instance_email: false });

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Only the instance administrator can change these settings.",
    );
    expect(screen.queryByLabelText("Wordfence Intelligence API key")).not.toBeInTheDocument();
    expect(mockedGet).not.toHaveBeenCalled();
  });

  it("never defaults open when the response has no can_manage_instance_email field", async () => {
    renderSettings("/settings/vuln-feed", OWNER_ME);

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Only the instance administrator can change these settings.",
    );
    expect(screen.queryByRole("link", { name: "Vulnerability feed" })).not.toBeInTheDocument();
    expect(mockedGet).not.toHaveBeenCalled();
  });
});

describe("Settings > Vulnerability feed for a superadmin with no organisation", () => {
  it("shows the entry and the feed settings, with every org-only entry hidden", async () => {
    renderSettings("/settings/vuln-feed", {
      ...SUPERADMIN_NO_ORG_ME,
      can_manage_instance_email: true,
    });

    expect(await screen.findByText("Not configured")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Vulnerability feed" })).toHaveAttribute(
      "href",
      "/settings/vuln-feed",
    );
    expect(screen.queryByRole("link", { name: "Organisation" })).not.toBeInTheDocument();
    expect(mockedGet).toHaveBeenCalledWith({ url: "/api/v1/admin/vuln-feed/status" });
  });
});
