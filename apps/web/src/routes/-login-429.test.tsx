import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, act } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRouteWithContext,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import type { ApiError } from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { authKeys } from "@/features/auth/use-auth";
import type { RouterContext } from "@/router";

import { Route as LoginRoute } from "./login";

// GH #718 Phase 1 -- the login admission gate's 429 too_many_attempts path.
// commits e5151d26 (apps/api, login_gate.go) and 6131dc5c (this app:
// login.tsx's paused-sign-in panel + use-auth.ts's LoginRateLimitedError /
// Retry-After parse) shipped the behaviour with no test referencing
// LoginRateLimitedError, the countdown, the disabled Sign in button, or the
// end-of-pause panel -- reverting the fix left every test green.
//
// Mounts the REAL route (its own `component`, `validateSearch`, the real
// useLogin/LoginRateLimitedError chain) re-attached to a throwaway root, the
// same pattern as routes/-login.test.tsx and
// routes/-register-me-refresh.test.tsx. Only the @wpmgr/api boundary is
// mocked: `login` (the 429/401 responses under test) and
// `listSocialProviders` (login.tsx's beforeLoad primes the sign-in-method
// list before first paint).
//
// Fake timers throughout: login.tsx's countdown is a `window.setInterval`
// tied to `Date.now()` (see the comment above `rateLimitDeadline` in
// login.tsx), so a real 250ms interval would make every "does the countdown
// reach zero" assertion either flaky or slow. `vi.setSystemTime` plus
// `vi.advanceTimersByTimeAsync` under `act` drives it deterministically, the
// same pattern already used against a real interval-driven poll in
// use-sites.test.ts.

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return { ...actual, login: vi.fn(), listSocialProviders: vi.fn() };
});

const { login, listSocialProviders } = await import("@wpmgr/api");
const mockedLogin = vi.mocked(login);
const mockedListSocialProviders = vi.mocked(listSocialProviders);

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
  const stub = (path: string) =>
    createRoute({
      path,
      getParentRoute: () => rootRoute,
      validateSearch: (search: Record<string, unknown>) => search,
      component: () => <div>{path} stub</div>,
    });
  const routeTree = rootRoute.addChildren([
    loginRoute,
    stub("/sites"),
    stub("/portal"),
    stub("/register"),
    stub("/forgot-password"),
    stub("/2fa-challenge"),
  ]);
  return createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [initialPath] }),
    context: { queryClient },
  });
}

function renderLoginPage() {
  const queryClient = createTestQueryClient();
  // Unauthenticated: login.tsx's beforeLoad redirects away when `ensureMe`
  // resolves a session, so seed `null` (fetchMe's "no session" convention).
  queryClient.setQueryData(authKeys.me, null);
  const router = buildLoginRouter("/login", queryClient);
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  return router;
}

/**
 * Waits for the route's first paint (real timers only -- see the note on
 * `submitLogin` below for why fake timers must not be active yet here).
 */
async function waitForForm() {
  await screen.findByLabelText("Email");
}

/**
 * Types, fills and submits the sign-in form, then flushes the mocked
 * `login()` promise + the mutation's onError microtask chain by hand.
 *
 * Deliberately uses no `waitFor`/`findBy*` (see `waitForForm` above):
 * `@testing-library/dom`'s polling relies on a `MutationObserver` callback
 * that jsdom schedules via `queueMicrotask`, which Vitest's fake timers also
 * fake -- so once fake timers are active, `waitFor` never sees the DOM
 * settle and hangs until the whole test times out. Every 429/countdown test
 * below therefore calls `waitForForm()` BEFORE `vi.useFakeTimers()`, then
 * `submitLogin()` after, exactly mirroring the manual
 * `await Promise.resolve()` flush already used against a real
 * interval-driven poll in use-sites.test.ts (no `waitFor` there either).
 * Everything asserted afterwards is a synchronous `getBy*`/`queryBy*`, which
 * needs no observer at all.
 */
async function submitLogin() {
  fireEvent.change(screen.getByLabelText("Email"), {
    target: { value: "sarah@acme.test" },
  });
  fireEvent.change(screen.getByLabelText("Password"), {
    target: { value: "hunter2hunter2" },
  });
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
  });
}

function signInButton() {
  return screen.getByRole("button", { name: "Sign in" });
}

/** The role="status" pause panel's text, or null if it isn't rendered. */
function pauseText() {
  return screen.queryByRole("status")?.textContent ?? null;
}

/** Every role="alert" node's text, joined -- the raw code must never appear here. */
function alertText() {
  return screen
    .queryAllByRole("alert")
    .map((node) => node.textContent)
    .join(" | ");
}

/** A 429 too_many_attempts response, per login_gate.go / openapi.yaml. */
function mock429(headers: Record<string, string>, body: Partial<ApiError>) {
  mockedLogin.mockResolvedValue({
    data: undefined,
    error: body as ApiError,
    response: new Response(null, { status: 429, headers }),
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

afterEach(() => {
  vi.useRealTimers();
});

describe("LoginPage - 429 too_many_attempts (GH #718 Phase 1)", () => {
  it("shows the paused message for the refused scope and disables Sign in", async () => {
    // Header and body deliberately DISAGREE (42 vs 99): the header is the
    // contract (packages/openapi.yaml) and must win over the body restating
    // a different number, per use-auth.ts's clamp chain. A build that reads
    // only the body (or only the default) would show 99 or 30 here instead.
    mock429(
      { "Retry-After": "42" },
      { code: "too_many_attempts", message: "too many sign-in attempts", details: { scope: "pair", retry_after_seconds: 99 } },
    );
    renderLoginPage();
    await waitForForm();
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-09-28T12:00:00Z"));

    await submitLogin();

    expect(pauseText()).toContain("Too many sign-in attempts for this account.");
    expect(pauseText()).toContain("Try again in");
    expect(pauseText()).toContain("42");
    expect(pauseText()).not.toContain("99");
    expect(signInButton()).toBeDisabled();
    // The raw machine code must never reach the DOM.
    expect(alertText()).not.toContain("too_many_attempts");
    expect(document.body.textContent).not.toContain("too_many_attempts");
  });

  it("the network scope gets its own wording", async () => {
    mock429(
      { "Retry-After": "10" },
      { code: "too_many_attempts", message: "x", details: { scope: "network", retry_after_seconds: 10 } },
    );
    renderLoginPage();
    await waitForForm();
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-09-28T12:00:00Z"));

    await submitLogin();

    expect(pauseText()).toContain("Too many sign-in attempts from your network.");
  });

  it("the countdown reaches zero, Sign in re-enables, and the raw code is never shown", async () => {
    mock429(
      { "Retry-After": "2" },
      { code: "too_many_attempts", message: "x", details: { scope: "pair", retry_after_seconds: 2 } },
    );
    renderLoginPage();
    await waitForForm();
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-09-28T12:00:00Z"));

    await submitLogin();
    expect(pauseText()).toContain("Try again in");
    expect(signInButton()).toBeDisabled();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(2100);
    });

    // THE FIX: at 0 the panel swaps to a neutral confirmation instead of
    // unmounting -- unmounting it would let `serverError` show the raw
    // LoginRateLimitedError.message ("too_many_attempts") underneath.
    expect(pauseText()).toContain("You can try again now.");
    expect(signInButton()).not.toBeDisabled();
    expect(alertText()).not.toContain("too_many_attempts");
    expect(document.body.textContent).not.toContain("too_many_attempts");
  });

  it("falls back to the response body's retry_after_seconds when the Retry-After header is missing", async () => {
    mock429(
      {},
      { code: "too_many_attempts", message: "x", details: { scope: "source", retry_after_seconds: 17 } },
    );
    renderLoginPage();
    await waitForForm();
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-09-28T12:00:00Z"));

    await submitLogin();

    expect(pauseText()).toContain("17");
  });

  it.each([
    ["header 0, body 0", { "Retry-After": "0" }, 0],
    ["header negative, body negative", { "Retry-After": "-5" }, -5],
    ["header non-integer, body non-integer", { "Retry-After": "1.5" }, 1.5],
    ["header absurdly large, body absurdly large", { "Retry-After": "999999999" }, 999999999],
    ["header HTTP-date, no body details", { "Retry-After": "Wed, 21 Oct 2026 07:28:00 GMT" }, undefined],
    ["everything missing", {}, undefined],
  ])(
    "clamps to the fixed 30s default when %s",
    async (_label, headers, bodySeconds) => {
      mock429(
        headers,
        bodySeconds === undefined
          ? { code: "too_many_attempts", message: "x" }
          : { code: "too_many_attempts", message: "x", details: { scope: "pair", retry_after_seconds: bodySeconds } },
      );
      renderLoginPage();
      await waitForForm();
      vi.useFakeTimers();
      vi.setSystemTime(new Date("2026-09-28T12:00:00Z"));

      await submitLogin();

      expect(pauseText()).toContain("Try again in");
      expect(pauseText()).toContain("30");
    },
  );

  it("an unrecognised scope reads as the generic wording rather than failing to render", async () => {
    mock429(
      { "Retry-After": "5" },
      { code: "too_many_attempts", message: "x", details: { scope: "bogus", retry_after_seconds: 5 } },
    );
    renderLoginPage();
    await waitForForm();
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-09-28T12:00:00Z"));

    await submitLogin();

    expect(pauseText()).toContain("Too many sign-in attempts.");
  });

  it("a 401 still shows the ordinary wrong-credentials message, not the pause panel", async () => {
    // apps/api/internal/auth/service.go:148,164 -- the code/message a real
    // 401 carries (useLogin ignores both and throws a fixed message, but the
    // mock should still look like the real response).
    mockedLogin.mockResolvedValue({
      data: undefined,
      error: { code: "invalid_credentials", message: "invalid email or password" },
      response: new Response(null, { status: 401 }),
    });
    renderLoginPage();
    await waitForForm();

    await submitLogin();

    expect(screen.queryByRole("status")).toBeNull();
    expect(alertText()).toMatch(/invalid email or password/i);
    expect(signInButton()).not.toBeDisabled();
  });

  it("Forgot password stays usable during the pause", async () => {
    mock429(
      { "Retry-After": "60" },
      { code: "too_many_attempts", message: "x", details: { scope: "pair", retry_after_seconds: 60 } },
    );
    renderLoginPage();
    await waitForForm();
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-09-28T12:00:00Z"));

    await submitLogin();

    expect(signInButton()).toBeDisabled();
    const forgot = screen.getByRole("link", { name: "Forgot password?" });
    expect(forgot).toHaveAttribute("href", "/forgot-password");
    expect(forgot).not.toHaveAttribute("aria-disabled");
  });
});
