import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRouteWithContext,
  createRoute,
  createRouter,
  RouterProvider,
  useSearch,
} from "@tanstack/react-router";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { authKeys } from "@/features/auth/use-auth";
import type { RouterContext } from "@/router";
import { client, getMe, listSocialProviders, login, type Me } from "@wpmgr/api";

import { Route as LoginRoute } from "./login";
import { Route as TwoFaRoute } from "./2fa-challenge";
import { Route as ConnectAiRoute } from "./_authed/connect.ai";

// WHERE THE SIGN-IN SCREENS SEND SOMEONE WHO ARRIVED WITH A ?redirect= ADDRESS.
//
// Two kinds of test live here, and the titles say which.
//
//   "pin: ..." holds behaviour in place. Every pin also passes against the
//   two-factor screen as it was before the narrowing that the regression tests
//   below are about (main's 2fa-challenge.tsx), so a pin does not reproduce a
//   defect. It goes red if a later change breaks what already works.
//
//   "the two-factor screen returns only to same-origin paths" holds the
//   regression tests. Each of those FAILS without the narrowing, ending on the
//   router's Not Found screen, and passes with it.
//
// The pins around a deep link exist for one case: an AI app's browser sign-in
// opens /connect/ai?response_type=code&client_id=... for a person who is not
// signed in. The _authed guard sends them to /login?redirect=<that whole
// address>, and after they sign in they have to be returned to exactly that
// address, query and all, or the request the app made is lost and the app waits
// for a callback that never comes.
//
// These tests mount the REAL /login and /2fa-challenge routes (their own
// component, validateSearch and beforeLoad, re-attached to a throwaway root) and
// a /connect/ai stub that parses its search with the REAL consent route's own
// schema, so a hand-back that mangles the query is judged by the parser the
// consent screen actually reads it with. Only the SDK's network functions are
// replaced; useLogin, the 2FA hooks, the QueryClient and the router are real.
// The router runs on an in-memory history, so these tests show where the app
// routes a value, not what a browser's address bar does with it.

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return {
    ...actual,
    login: vi.fn(),
    getMe: vi.fn(),
    listSocialProviders: vi.fn(),
    client: { ...actual.client, post: vi.fn() },
  };
});

const mockedLogin = vi.mocked(login);
const mockedGetMe = vi.mocked(getMe);
const mockedProviders = vi.mocked(listSocialProviders);
const mockedClientPost = vi.mocked(client.post);

const ME = {
  user: { id: "u1", email: "sarah@acme.test" },
  memberships: [],
  role: "owner",
} as unknown as Me;

// The seven parameters a standard MCP client sends, in the order it sends them,
// with the values a real one has: a base64url challenge and state, a loopback
// redirect, and a space separated scope list. Nothing here is all digits, which
// the search parser would read as a number.
const AUTHORIZE: readonly (readonly [string, string])[] = [
  ["response_type", "code"],
  ["client_id", "c1-abc"],
  ["code_challenge", "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"],
  ["code_challenge_method", "S256"],
  ["state", "a~b-_c"],
  ["redirect_uri", "http://localhost:61695/callback"],
  ["scope", "mcp:read mcp:site mcp:cache"],
];

const DEEP_LINK =
  "/connect/ai?" + AUTHORIZE.map(([k, v]) => `${k}=${encodeURIComponent(v)}`).join("&");
const EXPECTED_SEARCH = Object.fromEntries(AUTHORIZE);

const encode = (target: string) => `/login?redirect=${encodeURIComponent(target)}`;
const encode2fa = (target: string) =>
  `/2fa-challenge?challenge=c1&totp=true&redirect=${encodeURIComponent(target)}`;

function buildRouter(
  initialPath: string,
  queryClient: ReturnType<typeof createTestQueryClient>,
) {
  const rootRoute = createRootRouteWithContext<RouterContext>()({});
  type LoginUpdate = Parameters<typeof LoginRoute.update>[0];
  type TwoFaUpdate = Parameters<typeof TwoFaRoute.update>[0];
  const loginRoute = LoginRoute.update({
    id: "/login",
    path: "/login",
    getParentRoute: () => rootRoute,
  } as unknown as LoginUpdate);
  const twoFaRoute = TwoFaRoute.update({
    id: "/2fa-challenge",
    path: "/2fa-challenge",
    getParentRoute: () => rootRoute,
  } as unknown as TwoFaUpdate);
  const stub = (path: string, label: string) =>
    createRoute({
      path,
      getParentRoute: () => rootRoute,
      component: () => <div>{label}</div>,
    });
  const connectAiRoute = createRoute({
    path: "/connect/ai",
    getParentRoute: () => rootRoute,
    // The consent route's OWN parser, not a copy of it.
    validateSearch: ConnectAiRoute.options.validateSearch,
    component: function ConnectAiStub() {
      const search = useSearch({ strict: false });
      return <pre data-testid="connect-ai-search">{JSON.stringify(search)}</pre>;
    },
  });
  const routeTree = rootRoute.addChildren([
    loginRoute,
    twoFaRoute,
    connectAiRoute,
    stub("/sites", "Sites stub"),
    stub("/portal", "Portal stub"),
    stub("/settings", "Settings stub"),
    stub("/register", "Register stub"),
    stub("/forgot-password", "Forgot stub"),
  ]);
  return createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [initialPath] }),
    context: { queryClient },
  });
}

function mount(initialPath: string, session: Me | null) {
  const queryClient = createTestQueryClient();
  // Seeded so the guards decide from it: `null` is fetchMe's "no session".
  queryClient.setQueryData(authKeys.me, session);
  const router = buildRouter(initialPath, queryClient);
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  return router;
}

async function signInWithPassword() {
  fireEvent.change(await screen.findByLabelText("Email"), {
    target: { value: "sarah@acme.test" },
  });
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: "correct horse" } });
  fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
}

async function answerTotp() {
  fireEvent.change(await screen.findByLabelText("Authentication code"), {
    target: { value: "123456" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Verify" }));
}

async function landedSearch(): Promise<unknown> {
  const node = await screen.findByTestId("connect-ai-search");
  return JSON.parse(node.textContent ?? "null") as unknown;
}

// The four places a ?redirect= value is read: the sign-in page and the
// two-factor page, each for a visitor who already has a session (the route's
// beforeLoad) and for one who has just finished signing in.
type Site = {
  name: string;
  /** The address the visitor opens, for the ?redirect= value under test. */
  open: (target: string) => string;
  /** Whether they hold a session when they open it. */
  session: Me | null;
  /** What they do on the page before the app navigates, if anything. */
  act?: () => Promise<void>;
};

const LOGIN_SIGNED_IN: Site = { name: "login, already signed in", open: encode, session: ME };
const LOGIN_PASSWORD: Site = {
  name: "login, password sign-in",
  open: encode,
  session: null,
  act: signInWithPassword,
};
const TWO_FA_SIGNED_IN: Site = {
  name: "2FA, already signed in",
  open: encode2fa,
  session: ME,
};
const TWO_FA_CODE: Site = {
  name: "2FA, after the code is accepted",
  open: encode2fa,
  session: null,
  act: answerTotp,
};

async function visit(site: Site, target: string) {
  mount(site.open(target), site.session);
  await site.act?.();
}

async function expectSitesList() {
  // Both in one retry loop: nothing may have navigated the document (the tests
  // below replace window.location, and a write to its href shows up here), and
  // the sites list must be what is on screen.
  await waitFor(() => {
    expect(window.location.href).toBe("");
    expect(screen.queryByText("Sites stub")).not.toBeNull();
  });
}

const originalLocation = window.location;

beforeEach(() => {
  vi.clearAllMocks();
  mockedProviders.mockResolvedValue({
    data: { providers: [], sso: false },
    error: undefined,
    response: new Response(),
  });
  mockedLogin.mockResolvedValue({
    data: ME,
    error: undefined,
    response: new Response(null, { status: 200 }),
  });
  mockedGetMe.mockResolvedValue({
    data: ME,
    error: undefined,
    response: new Response(null, { status: 200 }),
  });
  mockedClientPost.mockResolvedValue({
    data: { me: ME },
    error: undefined,
    response: new Response(null, { status: 200 }),
  });
  // A write to window.location.href (a document navigation) lands here, so a
  // test can see that none happened.
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

describe("sign-in returns to a deep link that carries a query string", () => {
  // PINS. Each of these passes on main as well as on this branch (checked by
  // running this file against main's 2fa-challenge.tsx): a deep link with a
  // query string reaches /connect/ai with every parameter intact. They hold
  // that in place, and they are the over-fire arm of the regression tests
  // further down, because a narrowing that sent every address to the sites list
  // would fail them.
  it("pin: returns to a query-bearing deep link after a password sign-in", async () => {
    const router = mount(encode(DEEP_LINK), null);

    await signInWithPassword();

    await waitFor(() => expect(router.state.location.pathname).toBe("/connect/ai"));
    // All seven parameters, equal. A hand-back that kept the path and dropped
    // or reshaped the query would put the person on a screen that cannot
    // identify the request.
    expect(await landedSearch()).toEqual(EXPECTED_SEARCH);
  });

  it("pin: sends an already-signed-in visitor straight to a query-bearing deep link", async () => {
    const router = mount(encode(DEEP_LINK), ME);

    await waitFor(() => expect(router.state.location.pathname).toBe("/connect/ai"));
    expect(await landedSearch()).toEqual(EXPECTED_SEARCH);
  });

  it("pin: returns to a query-bearing deep link through the 2FA challenge", async () => {
    const router = mount(encode2fa(DEEP_LINK), null);

    await answerTotp();

    await waitFor(() => expect(router.state.location.pathname).toBe("/connect/ai"));
    expect(await landedSearch()).toEqual(EXPECTED_SEARCH);
  });

  it("pin: sends an already-signed-in visitor on the 2FA page to a query-bearing deep link", async () => {
    const router = mount(encode2fa(DEEP_LINK), ME);

    await waitFor(() => expect(router.state.location.pathname).toBe("/connect/ai"));
    expect(await landedSearch()).toEqual(EXPECTED_SEARCH);
  });

  it("pin: sends an already-signed-in visitor to a plain deep link", async () => {
    // A path with no query lands on that path.
    const plain = mount(encode("/settings"), ME);
    await screen.findByText("Settings stub");
    expect(plain.state.location.pathname).toBe("/settings");
  });

  it("pin: falls back to the sites list after a password sign-in with no deep link", async () => {
    mount("/login", null);
    await signInWithPassword();
    await screen.findByText("Sites stub");
  });
});

// ?redirect= is the one search parameter on the sign-in screens that a link
// gets to choose. A screen follows it only when it is a path on this origin
// (sameOriginPath) and otherwise goes to the sites list.
//
// The first group starts with a slash and then names a host; the second group
// does not start with a slash at all.
const HOST_AFTER_SLASH = ["//evil.example/x", "/\\evil.example"] as const;
const NOT_A_PATH = ["https://evil.example/steal", "javascript:alert(1)"] as const;

describe("the two-factor screen returns only to same-origin paths", () => {
  // REGRESSION TESTS. Without the narrowing, each of these ends on the router's
  // Not Found screen instead of the sites list, at both places the two-factor
  // screen reads the value. The over-fire arm is the first group of pins above.
  for (const site of [TWO_FA_SIGNED_IN, TWO_FA_CODE]) {
    for (const value of HOST_AFTER_SLASH) {
      it(`${site.name}: ${value} lands on the sites list`, async () => {
        await visit(site, value);
        await expectSitesList();
      });
    }
  }
});

describe("a ?redirect= that is not a path on this origin lands on the sites list", () => {
  // PINS. These end on the sites list on main as well as on this branch: the
  // sign-in page already narrowed the value, and on the two-factor screen the
  // values in the second group already ended there. They hold that in place,
  // and they are what would catch either screen loosening later.
  const CASES: readonly { site: Site; values: readonly string[] }[] = [
    { site: LOGIN_SIGNED_IN, values: [...HOST_AFTER_SLASH, ...NOT_A_PATH] },
    { site: LOGIN_PASSWORD, values: [...HOST_AFTER_SLASH, ...NOT_A_PATH] },
    { site: TWO_FA_SIGNED_IN, values: NOT_A_PATH },
    { site: TWO_FA_CODE, values: NOT_A_PATH },
  ];

  for (const { site, values } of CASES) {
    for (const value of values) {
      it(`pin: ${site.name}: ${value} lands on the sites list`, async () => {
        await visit(site, value);
        await expectSitesList();
      });
    }
  }
});
