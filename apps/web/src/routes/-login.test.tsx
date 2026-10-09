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
import { mockMutationResult } from "@/test/query-mocks";
import { authKeys } from "@/features/auth/use-auth";
import type { RouterContext } from "@/router";

import { Route as LoginRoute } from "./login";
import { useLogin, useResendVerification, type LoginResult } from "@/features/auth/use-auth";
import { listSocialProviders } from "@wpmgr/api";

// The sign-in page, mounting the REAL route (its own `component`,
// `validateSearch` and `beforeLoad`) re-attached to a throwaway root, the same
// pattern as -register.test.tsx. Three behaviours are pinned here:
//
//   2.14 a refusal that only a verification link can clear offers that link.
//        The callback sends no mail on the status-gate codes, and nothing else
//        in the product will send one either, so without the control on this
//        panel the refusal is a dead end.
//
//   2.29 the alternative sign-in methods are part of first paint, not injected
//        into the page a moment later above the SSO button.
//
//   2.31 the ?redirect= deep link survives the provider handshake.

vi.mock("@/features/auth/use-auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/auth/use-auth")>();
  return { ...actual, useLogin: vi.fn(), useResendVerification: vi.fn() };
});

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return { ...actual, listSocialProviders: vi.fn() };
});

const mockedUseLogin = vi.mocked(useLogin);
const mockedUseResendVerification = vi.mocked(useResendVerification);
const mockedListSocialProviders = vi.mocked(listSocialProviders);

type ProvidersBody = { providers: ("google" | "github")[]; sso: boolean };

/** Answers /auth/social/providers the way the generated client would. */
function methodsResolving(body: ProvidersBody) {
  return vi
    .fn()
    .mockResolvedValue({ data: body, error: undefined, response: new Response() });
}

function buildLoginRouter(
  initialPath: string,
  queryClient: ReturnType<typeof createTestQueryClient>,
) {
  const rootRoute = createRootRouteWithContext<RouterContext>()({});
  type UpdateOptions = Parameters<typeof LoginRoute.update>[0];
  const loginRoute = LoginRoute.update({
    id: "/login",
    path: "/login",
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
  const registerRoute = createRoute({
    path: "/register",
    getParentRoute: () => rootRoute,
    validateSearch: (search: Record<string, unknown>) => search,
    component: () => <div>Register stub</div>,
  });
  const forgotRoute = createRoute({
    path: "/forgot-password",
    getParentRoute: () => rootRoute,
    component: () => <div>Forgot stub</div>,
  });
  const challengeRoute = createRoute({
    path: "/2fa-challenge",
    getParentRoute: () => rootRoute,
    validateSearch: (search: Record<string, unknown>) => search,
    component: () => <div>Challenge stub</div>,
  });
  const routeTree = rootRoute.addChildren([
    loginRoute,
    sitesRoute,
    portalRoute,
    registerRoute,
    forgotRoute,
    challengeRoute,
  ]);
  return createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [initialPath] }),
    context: { queryClient },
  });
}

function renderLoginPage(initialPath = "/login") {
  const queryClient = createTestQueryClient();
  // Unauthenticated: login.tsx's beforeLoad redirects away when `ensureMe`
  // resolves a session, so seed `null` (fetchMe's "no session" convention).
  queryClient.setQueryData(authKeys.me, null);
  const router = buildLoginRouter(initialPath, queryClient);
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  return router;
}

const originalLocation = window.location;

beforeEach(() => {
  vi.clearAllMocks();
  mockedUseLogin.mockReturnValue(
    mockMutationResult<LoginResult, { email: string; password: string }>({}),
  );
  mockedUseResendVerification.mockReturnValue(
    mockMutationResult<void, { email: string }>({}),
  );
  mockedListSocialProviders.mockImplementation(
    methodsResolving({ providers: ["google", "github"], sso: true }),
  );
  Object.defineProperty(window, "location", {
    configurable: true,
    writable: true,
    value: { ...originalLocation, href: "" },
  });
});

afterEach(() => {
  Object.defineProperty(window, "location", {
    configurable: true,
    writable: true,
    value: originalLocation,
  });
});

// ---------------------------------------------------------------------------
// 2.29: the alternative methods are there when the page is
// ---------------------------------------------------------------------------

describe("LoginPage - alternative sign-in methods are part of first paint", () => {
  it("has every provider button and the SSO button in the first painted DOM", async () => {
    renderLoginPage("/login");
    // The first assertion is async because RouterProvider resolves its initial
    // match in a microtask. Everything after it is deliberately synchronous:
    // that is the whole point. If the method list were still in flight at
    // paint, these buttons would arrive in a later commit and push whatever is
    // below them down the page.
    await screen.findByLabelText("Email");

    expect(screen.getByRole("button", { name: "Sign in with Google" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Sign in with GitHub" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Sign in with SSO" })).toBeTruthy();
  });

  it("does not promise single sign-on above a page that may not offer it", async () => {
    // The description is the first thing read and sits above the form. It used
    // to name SSO on every install, including the default one where no issuer
    // is configured and no SSO button renders.
    mockedListSocialProviders.mockImplementation(
      methodsResolving({ providers: [], sso: false }),
    );
    renderLoginPage("/login");
    await screen.findByLabelText("Email");

    expect(screen.queryByText(/single sign-on/i)).toBeNull();
    expect(screen.getByText("Use your email and password.")).toBeTruthy();
  });

  it("renders no SSO button when the install has no issuer", async () => {
    mockedListSocialProviders.mockImplementation(
      methodsResolving({ providers: ["google"], sso: false }),
    );
    renderLoginPage("/login");
    await screen.findByLabelText("Email");

    expect(screen.getByRole("button", { name: "Sign in with Google" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Sign in with SSO" })).toBeNull();
  });

  it("paints the page even when the method list never answers", async () => {
    // Sign-in is the one page that has to work when things are broken. The
    // prefetch is bounded, so a hanging providers endpoint costs the buttons,
    // never the form.
    mockedListSocialProviders.mockImplementation(
      vi.fn().mockReturnValue(new Promise(() => {})),
    );
    renderLoginPage("/login");

    expect(await screen.findByLabelText("Email", undefined, { timeout: 4000 })).toBeTruthy();
    // And nothing stands in for the buttons meanwhile. A placeholder here would
    // be a block that appears and then collapses on the commonest install,
    // which configures no provider and no issuer, so the resolved render is
    // nothing at all.
    expect(screen.queryByTestId("sign-in-methods-placeholder")).toBeNull();
    expect(screen.queryByRole("button", { name: /Sign in with/ })).toBeNull();
  }, 10_000);

  it("renders no block at all on an install that offers neither", async () => {
    mockedListSocialProviders.mockImplementation(
      methodsResolving({ providers: [], sso: false }),
    );
    renderLoginPage("/login");
    await screen.findByLabelText("Email");

    expect(screen.queryByText("or")).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// 2.31: the deep link survives the handshake
// ---------------------------------------------------------------------------

describe("LoginPage - the ?redirect= deep link reaches the provider handshake", () => {
  it("carries the target into the social start URL", async () => {
    renderLoginPage("/login?redirect=%2Fsites%2Fabc%2Fbackups");
    await screen.findByLabelText("Email");

    fireEvent.click(screen.getByRole("button", { name: "Sign in with Google" }));

    expect(window.location.href).toBe(
      "/auth/social/google/start?redirect=%2Fsites%2Fabc%2Fbackups",
    );
  });

  it("starts a plain handshake when there is no deep link", async () => {
    renderLoginPage("/login");
    await screen.findByLabelText("Email");

    fireEvent.click(screen.getByRole("button", { name: "Sign in with GitHub" }));

    expect(window.location.href).toBe("/auth/social/github/start");
  });

  it("refuses to hand an off-site target to the handshake", async () => {
    renderLoginPage("/login?redirect=https%3A%2F%2Fevil.example%2Fsteal");
    await screen.findByLabelText("Email");

    fireEvent.click(screen.getByRole("button", { name: "Sign in with Google" }));

    expect(window.location.href).toBe("/auth/social/google/start");
  });

  // THE SIBLING USE OF THE SAME PARAMETER. beforeLoad sends a visitor who
  // already has a session wherever ?redirect= says, from the same attacker
  // chosen string, on the page whose whole job is to be linked to.
  //
  // Asserted on what beforeLoad THROWS rather than on where the test router
  // ends up: the redirect target is the thing under test, and a memory history
  // with a handful of stub routes quietly resolves an unmatched target to
  // something harmless, which would make this pass either way.
  describe("the redirect a signed-in visitor is sent to", () => {
    async function redirectTargetFor(redirect: string | undefined) {
      const queryClient = createTestQueryClient();
      queryClient.setQueryData(authKeys.me, {
        user: { id: "u1", email: "sarah@acme.test" },
        memberships: [],
        role: "owner",
      });
      // beforeLoad takes the full router argument; a test supplies the two
      // fields it reads.
      const beforeLoad = LoginRoute.options.beforeLoad as (
        arg: unknown,
      ) => Promise<void>;
      try {
        await beforeLoad({ context: { queryClient }, search: { redirect } });
      } catch (thrown) {
        return (thrown as { options?: { to?: string } }).options?.to;
      }
      throw new Error("a visitor with a session must be redirected off /login");
    }

    it("honours a same-origin deep link", async () => {
      expect(await redirectTargetFor("/sites/abc/backups")).toBe("/sites/abc/backups");
    });

    it("falls back to /sites with no deep link", async () => {
      expect(await redirectTargetFor(undefined)).toBe("/sites");
    });

    it("refuses to send them anywhere off this origin", async () => {
      for (const off of [
        "https://evil.example/steal",
        "//evil.example/steal",
        "/\\evil.example/steal",
      ]) {
        expect(await redirectTargetFor(off), off).toBe("/sites");
      }
    });
  });
});

// ---------------------------------------------------------------------------
// 2.14: a refusal that mail can clear offers the mail
// ---------------------------------------------------------------------------

describe("LoginPage - social refusals offer the recovery they need", () => {
  it("offers to send the verification email a pending account never received", async () => {
    const resend = vi.fn(
      (_vars: { email: string }, opts?: { onSuccess?: () => void }) => {
        opts?.onSuccess?.();
        return Promise.resolve();
      },
    );
    mockedUseResendVerification.mockReturnValue(
      mockMutationResult<void, { email: string }>({
        mutateAsync: resend as unknown as ReturnType<
          typeof mockMutationResult<void, { email: string }>
        >["mutateAsync"],
      }),
    );

    renderLoginPage("/login?social_error=email_not_verified");
    const emailInput = await screen.findByLabelText("Email");

    const send = screen.getByRole("button", { name: "Send the verification email" });
    // Nothing to send to yet: the callback deliberately does not tell the page
    // which address was refused, so the address comes from the form.
    expect((send as HTMLButtonElement).disabled).toBe(true);

    fireEvent.change(emailInput, { target: { value: "pending@wpmgr.test" } });
    await waitFor(() => expect((send as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(send);

    expect(resend).toHaveBeenCalledWith(
      { email: "pending@wpmgr.test" },
      expect.anything(),
    );
    await screen.findByText(/Verification email sent/i);
  });

  it("offers no resend for a disabled account", async () => {
    renderLoginPage("/login?social_error=account_disabled");
    await screen.findByLabelText("Email");

    expect(screen.getByRole("alert").textContent).toMatch(/disabled/i);
    expect(
      screen.queryByRole("button", { name: "Send the verification email" }),
    ).toBeNull();
  });

  it("keeps the deep link the refusal came back with", async () => {
    // socialFail returns both, so recovering by password still lands where the
    // shared link was pointing.
    renderLoginPage("/login?social_error=email_not_verified&redirect=%2Fsites%2Fabc");
    await screen.findByLabelText("Email");

    fireEvent.click(screen.getByRole("button", { name: "Sign in with Google" }));
    expect(window.location.href).toBe(
      "/auth/social/google/start?redirect=%2Fsites%2Fabc",
    );
  });
});
