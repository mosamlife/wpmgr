import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRouteWithContext,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { authKeys } from "@/features/auth/use-auth";
import type { RouterContext } from "@/router";
import type { Me } from "@wpmgr/api";

import { Route as AcceptRoute } from "./accept";

// The signed-in caller whose invitation went to another of their addresses.
//
// The 403 handler does two things in one breath: it writes the message AND it
// flips the page to the typed-address form. That makes the message a
// description of a page that no longer exists by the time it is read. It used
// to tell the person to choose "Use a different address" below, which was wrong
// twice over: the button it meant is removed by the very same handler, and its
// label was never those words anyway. Nothing here pins the exact sentence;
// what is pinned is that the sentence cannot point at a control the page has
// just taken away.

const SESSION_EMAIL = "sarah@home.example";

function sessionMe(): Me {
  return {
    user: {
      id: "11111111-1111-1111-1111-111111111111",
      email: SESSION_EMAIL,
      name: "Sarah",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    },
    memberships: [],
  };
}

function buildAcceptRouter(
  initialPath: string,
  queryClient: ReturnType<typeof createTestQueryClient>,
) {
  const rootRoute = createRootRouteWithContext<RouterContext>()({});
  type UpdateOptions = Parameters<typeof AcceptRoute.update>[0];
  const acceptRoute = AcceptRoute.update({
    id: "/accept",
    path: "/accept",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const sitesRoute = createRoute({
    path: "/sites",
    getParentRoute: () => rootRoute,
    component: () => <div>Sites stub</div>,
  });
  const portalRoute = createRoute({
    path: "/portal",
    getParentRoute: () => rootRoute,
    component: () => <div>Portal stub</div>,
  });
  const siteHealthRoute = createRoute({
    path: "/sites/$siteId/health",
    getParentRoute: () => rootRoute,
    component: () => <div>Site health stub</div>,
  });
  const routeTree = rootRoute.addChildren([
    acceptRoute,
    sitesRoute,
    portalRoute,
    siteHealthRoute,
  ]);
  return createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [initialPath] }),
    context: { queryClient },
  });
}

/** Renders /accept with a live session, so the page opens in its signed-in
 *  branch (address fixed to the session's, no password field). */
function renderAcceptPageSignedIn() {
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(authKeys.me, sessionMe());
  const router = buildAcceptRouter("/accept?token=tok-1", queryClient);
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
}

const fetchMock = vi.fn();

beforeEach(() => {
  vi.clearAllMocks();
  // The invite-accept POST is hand-rolled `fetch` (it carries the
  // X-WPMgr-Invite-Accept header), so it is stubbed at the global, not at a
  // generated client fn. Error bodies are empty over the wire today, which is
  // exactly why the page branches on status.
  fetchMock.mockResolvedValue(new Response("{}", { status: 403 }));
  vi.stubGlobal("fetch", fetchMock);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("AcceptPage - invitation addressed to someone else", () => {
  it("does not send the person after a control the same refusal removes", async () => {
    renderAcceptPageSignedIn();

    // Signed-in branch: the address is the session's and the switch is offered.
    const switchLabel = /this invitation was sent to a different address/i;
    expect(await screen.findByText(switchLabel)).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Accept invitation" }));

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      `This invitation was not sent to ${SESSION_EMAIL}`,
    );

    // The refusal flips the page to the typed-address form, which is what takes
    // the switch away. Both halves are asserted together because it is the pair
    // that makes the old copy wrong.
    await waitFor(() => {
      expect(screen.queryByText(switchLabel)).toBeNull();
    });
    expect(alert.textContent?.toLowerCase()).not.toContain(
      "use a different address",
    );

    // Whatever the sentence says to do next has to be doable on the page the
    // person is now on: an address to type and a password to go with it.
    expect(screen.getByLabelText("Email address")).toBeTruthy();
    expect(screen.getByLabelText("Password")).toBeTruthy();
  });

  it("names only controls that are on the page when the message is", async () => {
    renderAcceptPageSignedIn();
    await screen.findByRole("button", { name: "Accept invitation" });
    fireEvent.click(screen.getByRole("button", { name: "Accept invitation" }));

    const alert = await screen.findByRole("alert");
    const message = alert.textContent ?? "";

    // A quoted phrase in an error is read as a label to go and find. If the
    // message quotes anything, that thing has to be findable.
    const quoted = [...message.matchAll(/"([^"]+)"/g)].map((m) => m[1] ?? "");
    for (const label of quoted) {
      expect(
        screen.queryByRole("button", { name: new RegExp(label, "i") }),
        `message quotes "${label}" but no such control is rendered`,
      ).not.toBeNull();
    }
  });
});
