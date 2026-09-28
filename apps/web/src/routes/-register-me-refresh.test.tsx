import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRouteWithContext,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import type { Me } from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { authKeys } from "@/features/auth/use-auth";
import type { RouterContext } from "@/router";

import { Route as RegisterRoute } from "./register";
import { Route as SettingsRoute } from "./_authed/settings/route";
import { Route as SmtpRoute } from "./_authed/settings/smtp";

// First-run regression: a Me built before a session exists (register,
// verify-email, login, 2FA) omits can_manage_instance_email — the OpenAPI
// text says clients read it from the GET /auth/me that follows. login.tsx
// and 2fa-challenge.tsx already force that fresh fetch; this pins that
// register.tsx does too, so the sole owner of a brand-new install sees the
// Email / SMTP nav item and can reach /settings/smtp without the cache
// having to go stale first.
//
// Exercises the REAL useRegister hook (from use-auth.ts) end to end, mounted
// on register.tsx's own route singleton (same re-attach-to-a-throwaway-root
// pattern as routes/-register.test.tsx). Only the @wpmgr/api boundary is
// mocked: register, getMe, listSocialProviders (register.tsx's beforeLoad
// primes the sign-in-method list) and client.get (the SMTP page's GET
// /api/v1/settings/smtp).

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return {
    ...actual,
    register: vi.fn(),
    getMe: vi.fn(),
    listSocialProviders: vi.fn(),
    client: { ...actual.client, get: vi.fn() },
  };
});

const { register, getMe, listSocialProviders, client } = await import("@wpmgr/api");
const mockedRegister = vi.mocked(register);
const mockedGetMe = vi.mocked(getMe);
const mockedListSocialProviders = vi.mocked(listSocialProviders);
const mockedClientGet = vi.mocked(client.get);

const TENANT_ID = "11111111-1111-1111-1111-111111111111";

// The exact shape the server sends from a Me built before a session exists:
// no can_manage_instance_email field at all.
const BOOTSTRAP_OWNER_NO_CAPABILITY: Me = {
  user: {
    id: "00000000-0000-0000-0000-000000000001",
    email: "owner@wpmgr.test",
    name: "Owner",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  },
  memberships: [
    { user_id: "00000000-0000-0000-0000-000000000001", tenant_id: TENANT_ID, role: "owner" },
  ],
  active_tenant_id: TENANT_ID,
  hosted: false,
};

function buildRegisterRouter(queryClient: ReturnType<typeof createTestQueryClient>) {
  const rootRoute = createRootRouteWithContext<RouterContext>()({});
  type UpdateOptions = Parameters<typeof RegisterRoute.update>[0];
  const registerRoute = RegisterRoute.update({
    id: "/register",
    path: "/register",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const sitesRoute = createRoute({
    path: "/sites",
    getParentRoute: () => rootRoute,
    component: () => <div>Sites stub</div>,
  });
  const routeTree = rootRoute.addChildren([registerRoute, sitesRoute]);
  return createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: ["/register"] }),
    context: { queryClient },
  });
}

beforeEach(() => {
  vi.clearAllMocks();
  mockedListSocialProviders.mockResolvedValue({
    data: { providers: [], sso: false },
    error: undefined,
    response: new Response(),
  });
});

describe("RegisterPage — forces a fresh /auth/me after bootstrap register (can_manage_instance_email regression)", () => {
  it("the Email / SMTP nav item is present and /settings/smtp shows the real form, even though the register response omits the field", async () => {
    mockedRegister.mockResolvedValue({
      data: BOOTSTRAP_OWNER_NO_CAPABILITY,
      error: undefined,
      response: new Response(null, { status: 200 }),
    });
    mockedGetMe.mockResolvedValue({
      data: { ...BOOTSTRAP_OWNER_NO_CAPABILITY, can_manage_instance_email: true },
      error: undefined,
      response: new Response(),
    });
    mockedClientGet.mockResolvedValue({
      data: {
        enabled: false,
        host: "",
        port: 587,
        username: "",
        from_address: "",
        from_name: "",
        tls_mode: "starttls",
        allow_insecure_tls: false,
        password_set: false,
        updated_at: "2026-01-01T00:00:00Z",
      },
      error: undefined,
      response: new Response(),
    });

    const queryClient = createTestQueryClient();
    // Unauthenticated — register.tsx's own beforeLoad redirects to /sites
    // when ensureMe resolves a real session, so seed null (the "no session"
    // convention) to keep the register form reachable without a real getMe
    // call from beforeLoad interfering with the assertion below.
    queryClient.setQueryData(authKeys.me, null);
    const router = buildRegisterRouter(queryClient);
    renderWithProviders(<RouterProvider router={router} />, { queryClient });

    const emailInput = await screen.findByLabelText("Email");
    fireEvent.change(emailInput, { target: { value: "owner@wpmgr.test" } });
    fireEvent.change(screen.getByLabelText("Password"), {
      target: { value: "supersecretpassword" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create account" }));

    await waitFor(() => expect(router.state.location.pathname).toBe("/sites"));

    // The fix: a fresh GET /auth/me actually fired (not just the seeded
    // register response) before the cache is treated as current.
    await waitFor(() => expect(mockedGetMe).toHaveBeenCalledTimes(1));
    expect(queryClient.getQueryData<Me>(authKeys.me)?.can_manage_instance_email).toBe(
      true,
    );

    // The settings nav follows that now-fresh cache.
    const SettingsLayout = SettingsRoute.options.component!;
    renderWithProviders(<SettingsLayout />, {
      queryClient,
      withRouter: true,
      initialPath: "/settings/account",
    });
    const navLink = await screen.findByRole("link", { name: "Email / SMTP" });
    expect(navLink).toHaveAttribute("href", "/settings/smtp");

    // And the SMTP page itself renders the real form, not the "ask your
    // instance administrator" refusal a missing capability produces.
    const SmtpSettingsPage = SmtpRoute.options.component!;
    renderWithProviders(<SmtpSettingsPage />, {
      queryClient,
      withRouter: true,
      initialPath: "/settings/smtp",
    });
    expect(await screen.findByText("SMTP relay")).toBeInTheDocument();
    expect(
      screen.queryByText("Only the instance administrator can change these settings."),
    ).not.toBeInTheDocument();
  });
});
