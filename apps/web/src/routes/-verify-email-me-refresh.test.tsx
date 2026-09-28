import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
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

import { Route as VerifyEmailRoute } from "./verify-email";
import { Route as SettingsRoute } from "./_authed/settings/route";
import { Route as SmtpRoute } from "./_authed/settings/smtp";

// First-run regression: a Me built before a session exists (register,
// verify-email, login, 2FA) omits can_manage_instance_email — the OpenAPI
// text says clients read it from the GET /auth/me that follows. login.tsx
// and 2fa-challenge.tsx already force that fresh fetch; this pins that
// verify-email.tsx does too, so the newly-verified sole owner sees the
// Email / SMTP nav item and can reach /settings/smtp without the cache
// having to go stale first.
//
// Exercises the REAL useVerifyEmail hook (from use-auth.ts) end to end,
// mounted on verify-email.tsx's own route singleton (same
// re-attach-to-a-throwaway-root pattern as routes/-verify-email.test.tsx).
// Only the @wpmgr/api boundary is mocked: client.post (POST
// /auth/verify-email is a hand-rolled Gin route, not in the generated SDK),
// getMe, and client.get (the SMTP page's GET /api/v1/settings/smtp).

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return {
    ...actual,
    getMe: vi.fn(),
    client: { ...actual.client, post: vi.fn(), get: vi.fn() },
  };
});

const { getMe, client } = await import("@wpmgr/api");
const mockedGetMe = vi.mocked(getMe);
const mockedClientPost = vi.mocked(client.post);
const mockedClientGet = vi.mocked(client.get);

const TENANT_ID = "11111111-1111-1111-1111-111111111111";

// The exact shape the server sends from a Me built before a session exists:
// no can_manage_instance_email field at all.
const VERIFIED_OWNER_NO_CAPABILITY: Me = {
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

function buildVerifyEmailRouter(queryClient: ReturnType<typeof createTestQueryClient>) {
  const rootRoute = createRootRouteWithContext<RouterContext>()({});
  type UpdateOptions = Parameters<typeof VerifyEmailRoute.update>[0];
  const verifyEmailRoute = VerifyEmailRoute.update({
    id: "/verify-email",
    path: "/verify-email",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const sitesRoute = createRoute({
    path: "/sites",
    getParentRoute: () => rootRoute,
    component: () => <div>Sites stub</div>,
  });
  const routeTree = rootRoute.addChildren([verifyEmailRoute, sitesRoute]);
  return createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: ["/verify-email?token=abc123"] }),
    context: { queryClient },
  });
}

beforeEach(() => {
  vi.clearAllMocks();
});

describe("VerifyEmailPage — forces a fresh /auth/me after verify (can_manage_instance_email regression)", () => {
  it("the Email / SMTP nav item is present and /settings/smtp shows the real form, even though the verify response omits the field", async () => {
    mockedClientPost.mockResolvedValue({
      data: VERIFIED_OWNER_NO_CAPABILITY,
      error: undefined,
      response: new Response(null, { status: 200 }),
    } as unknown as Awaited<ReturnType<typeof client.post>>);
    mockedGetMe.mockResolvedValue({
      data: { ...VERIFIED_OWNER_NO_CAPABILITY, can_manage_instance_email: true },
      error: undefined,
      response: new Response(),
    } as unknown as Awaited<ReturnType<typeof getMe>>);
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
    } as unknown as Awaited<ReturnType<typeof client.get>>);

    const queryClient = createTestQueryClient();
    const router = buildVerifyEmailRouter(queryClient);
    renderWithProviders(<RouterProvider router={router} />, { queryClient });

    await waitFor(() => expect(router.state.location.pathname).toBe("/sites"));

    // The fix: a fresh GET /auth/me actually fired (not just the seeded
    // verify-email response) before the cache is treated as current.
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
