import { describe, it, expect } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRouteWithContext,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
  useSearch,
} from "@tanstack/react-router";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { authKeys } from "@/features/auth/use-auth";
import type { RouterContext } from "@/router";
import type { Me } from "@wpmgr/api";

import { Route as AuthedRoute } from "../_authed";
import { Route as ConnectAiRoute } from "./connect.ai";

// A SIGNED-OUT VISITOR WHO OPENS THE CONSENT SCREEN IS SENT TO SIGN IN WITH THE
// WHOLE ADDRESS, QUERY INCLUDED.
//
// An AI app's browser sign-in opens /connect/ai?response_type=code&client_id=...
// This is the half of the round trip that keeps that address: the pathless
// _authed layout's beforeLoad sends a visitor without a session to
// /login?redirect=<where they were headed>. Its counterpart, bringing them back
// to it after they sign in, is -login.deep-link.test.tsx in the parent folder.
//
// The REAL _authed beforeLoad runs (only its layout component is swapped for a
// bare Outlet so the app shell and its queries stay out of it), in front of a
// /connect/ai stub that uses the real consent route's own search parser.

const QUERY =
  "response_type=code&client_id=c1-abc" +
  "&code_challenge=E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM&code_challenge_method=S256" +
  "&state=a~b-_c&redirect_uri=http%3A%2F%2Flocalhost%3A61695%2Fcallback" +
  "&scope=mcp%3Aread%20mcp%3Asite%20mcp%3Acache";

const ME = {
  user: { id: "u1", email: "sarah@acme.test" },
  memberships: [],
  role: "owner",
} as unknown as Me;

function mount(initialPath: string, session: Me | null) {
  const queryClient = createTestQueryClient();
  // Seeded so the guard decides from it: `null` is fetchMe's "no session".
  queryClient.setQueryData(authKeys.me, session);

  const rootRoute = createRootRouteWithContext<RouterContext>()({});
  type AuthedUpdate = Parameters<typeof AuthedRoute.update>[0];
  const authedRoute = AuthedRoute.update({
    id: "/_authed",
    getParentRoute: () => rootRoute,
    component: Outlet,
  } as unknown as AuthedUpdate);
  const connectAiRoute = createRoute({
    path: "/connect/ai",
    getParentRoute: () => authedRoute,
    validateSearch: ConnectAiRoute.options.validateSearch,
    component: () => <div>Consent screen stub</div>,
  });
  const loginRoute = createRoute({
    path: "/login",
    getParentRoute: () => rootRoute,
    validateSearch: (search: Record<string, unknown>) => search,
    component: function LoginStub() {
      const search = useSearch({ strict: false });
      return <pre data-testid="login-search">{JSON.stringify(search)}</pre>;
    },
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([authedRoute.addChildren([connectAiRoute]), loginRoute]),
    history: createMemoryHistory({ initialEntries: [initialPath] }),
    context: { queryClient },
  });
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  return router;
}

describe("/connect/ai for a signed-out visitor", () => {
  it("is sent to /login with redirect equal to the full href", async () => {
    const router = mount(`/connect/ai?${QUERY}`, null);

    await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
    const search = JSON.parse(
      (await screen.findByTestId("login-search")).textContent ?? "null",
    ) as { redirect?: string };

    // The redirect must come back as the same request: the path and every one
    // of the seven parameters, equal once parsed. Compared as parameters rather
    // than as raw text so a harmless re-encoding by the router does not fail a
    // test about whether the request survived.
    expect(search.redirect).toBeDefined();
    const target = new URL(search.redirect!, "http://app.test");
    expect(target.pathname).toBe("/connect/ai");
    expect(Object.fromEntries(target.searchParams)).toEqual(
      Object.fromEntries(new URLSearchParams(QUERY)),
    );
  });

  it("leaves a signed-in visitor on the consent screen", async () => {
    // The over-fire arm: the same address, with a session, is not redirected.
    // Without it the first test would also pass against a guard that sent
    // everyone to /login.
    const router = mount(`/connect/ai?${QUERY}`, ME);

    await screen.findByText("Consent screen stub");
    expect(router.state.location.pathname).toBe("/connect/ai");
  });
});
