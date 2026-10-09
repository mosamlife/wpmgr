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

// WHERE A SIGN-IN LANDS WHEN SOMEONE ARRIVED WITH A DEEP LINK THAT CARRIES A
// QUERY STRING.
//
// An AI app's browser sign-in opens /connect/ai?response_type=code&client_id=...
// for a person who is not signed in. The _authed guard sends them to
// /login?redirect=<that whole address>, and after they sign in they have to be
// returned to exactly that address, query and all, or the request the app made
// is lost and the app waits for a callback that never comes.
//
// These tests mount the REAL /login and /2fa-challenge routes (their own
// component, validateSearch and beforeLoad, re-attached to a throwaway root) and
// a /connect/ai stub that parses its search with the REAL consent route's own
// schema, so a hand-back that mangles the query is judged by the parser the
// consent screen actually reads it with. Only the SDK's network functions are
// replaced; useLogin, the 2FA hooks, the QueryClient and the router are real.

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

const originalLocation = window.location;

beforeEach(() => {
  vi.clearAllMocks();
  mockedProviders.mockResolvedValue({
    data: { providers: [], sso: false },
    error: undefined,
    response: new Response(),
  } as unknown as Awaited<ReturnType<typeof listSocialProviders>>);
  mockedLogin.mockResolvedValue({
    data: ME,
    error: undefined,
    response: new Response(null, { status: 200 }),
  } as unknown as Awaited<ReturnType<typeof login>>);
  mockedGetMe.mockResolvedValue({
    data: ME,
    error: undefined,
    response: new Response(null, { status: 200 }),
  } as unknown as Awaited<ReturnType<typeof getMe>>);
  mockedClientPost.mockResolvedValue({
    data: { me: ME },
    error: undefined,
    response: new Response(null, { status: 200 }),
  } as unknown as Awaited<ReturnType<typeof client.post>>);
  // A document navigation (an off-site href the router would hand to the
  // browser) writes `href` here, so a test can see that none happened.
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
  it("returns to a query-bearing deep link after a password sign-in", async () => {
    const router = mount(encode(DEEP_LINK), null);

    await signInWithPassword();

    await waitFor(() => expect(router.state.location.pathname).toBe("/connect/ai"));
    // All seven parameters, equal. A hand-back that kept the path and dropped
    // or reshaped the query would put the person on a screen that cannot
    // identify the request.
    expect(await landedSearch()).toEqual(EXPECTED_SEARCH);
  });

  it("sends an already-signed-in visitor straight to a query-bearing deep link", async () => {
    const router = mount(encode(DEEP_LINK), ME);

    await waitFor(() => expect(router.state.location.pathname).toBe("/connect/ai"));
    expect(await landedSearch()).toEqual(EXPECTED_SEARCH);
  });

  it("returns to a query-bearing deep link through the 2FA challenge", async () => {
    const router = mount(encode2fa(DEEP_LINK), null);

    await answerTotp();

    await waitFor(() => expect(router.state.location.pathname).toBe("/connect/ai"));
    expect(await landedSearch()).toEqual(EXPECTED_SEARCH);
  });

  it("sends an already-signed-in visitor on the 2FA page to a query-bearing deep link", async () => {
    const router = mount(encode2fa(DEEP_LINK), ME);

    await waitFor(() => expect(router.state.location.pathname).toBe("/connect/ai"));
    expect(await landedSearch()).toEqual(EXPECTED_SEARCH);
  });

  it("still serves a plain deep link, and the sites list when there is none", async () => {
    // The over-fire arm: the helper must not turn every redirect into
    // something exotic. A path with no query lands on that path; no
    // ?redirect= at all lands on the sites list.
    const plain = mount(encode("/settings"), ME);
    await screen.findByText("Settings stub");
    expect(plain.state.location.pathname).toBe("/settings");
  });

  it("falls back to the sites list after a password sign-in with no deep link", async () => {
    mount("/login", null);
    await signInWithPassword();
    await screen.findByText("Sites stub");
  });
});

describe("a sign-in redirect never leaves the origin", () => {
  // Each of these is attacker-chosen: ?redirect= is the one search parameter on
  // the sign-in pages that a link can set. Each must end on the sites list, and
  // none may reach the browser as a document navigation.
  const HOSTILE = [
    "//evil.example/x",
    "/\\evil.example",
    "https://evil.example/steal",
    "javascript:alert(1)",
  ] as const;

  type Site = {
    name: string;
    start: (target: string) => Promise<unknown>;
  };
  const SITES: readonly Site[] = [
    {
      name: "login, already signed in",
      start: async (target) => mount(encode(target), ME),
    },
    {
      name: "login, password sign-in",
      start: async (target) => {
        const router = mount(encode(target), null);
        await signInWithPassword();
        return router;
      },
    },
    {
      name: "2FA, already signed in",
      start: async (target) => mount(encode2fa(target), ME),
    },
    {
      name: "2FA, after the code is accepted",
      start: async (target) => {
        const router = mount(encode2fa(target), null);
        await answerTotp();
        return router;
      },
    },
  ];

  for (const site of SITES) {
    for (const hostile of HOSTILE) {
      it(`${site.name}: ${hostile} lands on the sites list`, async () => {
        await site.start(hostile);

        // Both in one retry loop, so a failure names the hazard rather than the
        // symptom: the router hands an absolute URL to window.location as a
        // document navigation, and a page that never reaches the sites list
        // because it was sent off-site would otherwise only say "not found".
        await waitFor(() => {
          expect(window.location.href).toBe("");
          expect(screen.queryByText("Sites stub")).not.toBeNull();
        });
      });
    }
  }
});
