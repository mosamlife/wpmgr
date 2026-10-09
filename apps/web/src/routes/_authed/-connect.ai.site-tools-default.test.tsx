import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRouteWithContext,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import type { RouterContext } from "@/router";
import { useSites } from "@/features/sites/use-sites";
import { useTags } from "@/features/tags/use-tags";
import {
  consentKeys,
  navigateTo,
  CONSENT_APPROVE_PATH,
  CONSENT_AUTHORIZE_PATH,
} from "@/features/mcp-consent/use-consent";

import { parseConsentContext } from "@/features/mcp-consent/consent-context";

import { Route as ConnectAiRoute } from "./connect.ai";

// THE SITE-TOOLS DEFAULT, THROUGH THE REAL ROUTE.
//
// An app that asks for site tools (mcp:site) gets the two site-tools choices
// opened ticked, and the person can clear either one. These tests mount the
// route's own component (its search parsing, its authorize query, its skeleton,
// the screen and the approval request) and stub only the edges: `fetch` for the
// two OAuth endpoints, the two list hooks the page also reads, and the browser
// navigation that follows an approval. What an approval sends is read from the
// body of the real POST, not from a prop.
//
// The authorize endpoint is answered late on purpose, so the screen is built
// from a context that arrived after the first render, which is how it arrives
// in production.

vi.mock("@/features/sites/use-sites", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/sites/use-sites")>();
  return { ...actual, useSites: vi.fn() };
});

vi.mock("@/features/tags/use-tags", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/tags/use-tags")>();
  return { ...actual, useTags: vi.fn() };
});

vi.mock("@/features/mcp-consent/use-consent", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/mcp-consent/use-consent")>();
  return { ...actual, navigateTo: vi.fn() };
});

// The wire shape of the authorize response. The paths come from use-consent.ts
// (CONSENT_AUTHORIZE_PATH, CONSENT_APPROVE_PATH); the field names are the ones
// consentWireSchema parses, which mirror apps/api/internal/mcp/dto.go.
function wire(ticket: string, over: Record<string, unknown> = {}) {
  return {
    client_id: "c_route",
    client_name_unverified: "Route Test Client",
    identity_verified: false,
    redirect_uri: "https://x.example/cb",
    redirect_host: "x.example",
    scopes: ["mcp:read", "mcp:site"],
    grant_lifetime_days: 90,
    state: "s1",
    code_challenge: "cc",
    code_challenge_method: "S256",
    consent_ticket: ticket,
    conferrable_capabilities: [
      { name: "mcp.sites.read", effect: "read" },
      { name: "mcp.ability.read", effect: "read" },
      { name: "mcp.ability.request", effect: "request" },
    ],
    ...over,
  };
}

// The seven reads the server lists for mcp:read (scopeCapabilities in
// apps/api/internal/mcp/policy.go), written out here rather than taken from the
// dashboard's vocabulary, and the full offer for a request that holds mcp:site.
const ALL_READS = [
  "mcp.activity.read",
  "mcp.backups.read",
  "mcp.diagnostics.read",
  "mcp.performance.read",
  "mcp.security.read",
  "mcp.sites.read",
  "mcp.uptime.read",
];
const FULL_OFFER = [
  ...ALL_READS.map((name) => ({ name, effect: "read" })),
  { name: "mcp.ability.read", effect: "read" },
  { name: "mcp.ability.request", effect: "request" },
];

const APPROVED = {
  grant_id: "g1",
  code: "code-1",
  redirect_uri: "https://x.example/cb",
  state: "s1",
};

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

const SEARCH =
  "response_type=code&client_id=c_route" +
  "&redirect_uri=https%3A%2F%2Fx.example%2Fcb" +
  "&scope=mcp%3Aread%20mcp%3Asite&state=s1&code_challenge=cc&code_challenge_method=S256";

/** The address a fetch call was made to, whichever form it was handed in. */
function urlOf(input: RequestInfo | URL): string {
  if (typeof input === "string") return input;
  if (input instanceof URL) return input.href;
  return input.url;
}

let authorizeAnswers: (() => Promise<Response>)[];
const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
  (input) => {
    const url = urlOf(input);
    if (url.startsWith(CONSENT_AUTHORIZE_PATH)) {
      const next = authorizeAnswers.shift();
      // Loud, never a silent default: an authorize request this test did not plan
      // for is a test bug, not a response to invent.
      if (next === undefined) return Promise.reject(new Error(`unplanned authorize request: ${url}`));
      return next();
    }
    if (url === CONSENT_APPROVE_PATH) return Promise.resolve(jsonResponse(APPROVED));
    return Promise.reject(new Error(`unplanned request: ${url}`));
  },
);

/** An authorize answer that arrives only when `release` is called. */
function lateAnswer(body: unknown) {
  let release!: () => void;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  return {
    release,
    answer: async () => {
      await gate;
      return jsonResponse(body);
    },
  };
}

function mount(search: string = SEARCH) {
  const queryClient = createTestQueryClient();
  const rootRoute = createRootRouteWithContext<RouterContext>()({});
  type UpdateOptions = Parameters<typeof ConnectAiRoute.update>[0];
  const connectAiRoute = ConnectAiRoute.update({
    id: "/connect/ai",
    path: "/connect/ai",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const router = createRouter({
    routeTree: rootRoute.addChildren([connectAiRoute]),
    history: createMemoryHistory({ initialEntries: [`/connect/ai?${search}`] }),
    context: { queryClient },
  });
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  return { queryClient, router };
}

const readBox = () => screen.getByTestId<HTMLInputElement>("ability-box-mcp.ability.read");
const requestBox = () => screen.getByTestId<HTMLInputElement>("ability-box-mcp.ability.request");

function authorizeRequests(): string[] {
  return fetchMock.mock.calls
    .map(([input]) => urlOf(input))
    .filter((url) => url.startsWith(CONSENT_AUTHORIZE_PATH));
}

/** The JSON body of the one approval POST, read from the request itself. */
async function approvalBody(): Promise<{
  capabilities?: string[];
  consent_ticket?: string;
  client_id?: string;
  name?: string;
}> {
  await waitFor(() => expect(navigateTo).toHaveBeenCalledTimes(1));
  const posts = fetchMock.mock.calls.filter(([input]) => urlOf(input) === CONSENT_APPROVE_PATH);
  expect(posts).toHaveLength(1);
  const raw = posts[0]![1]?.body;
  if (typeof raw !== "string") throw new Error("the approval body was not a JSON string");
  return JSON.parse(raw) as {
    capabilities?: string[];
    consent_ticket?: string;
    client_id?: string;
    name?: string;
  };
}

function submitApproval() {
  fireEvent.submit(screen.getByTestId("consent-approve").closest("form")!);
}

beforeEach(() => {
  authorizeAnswers = [];
  fetchMock.mockClear();
  vi.mocked(navigateTo).mockClear();
  vi.stubGlobal("fetch", fetchMock);
  vi.mocked(useSites).mockReturnValue({
    data: [],
    isPending: false,
  } as unknown as ReturnType<typeof useSites>);
  vi.mocked(useTags).mockReturnValue({ data: [] } as unknown as ReturnType<typeof useTags>);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("/connect/ai, site tools asked for", () => {
  it("shows the skeleton first, then opens both site tools ticked, and approving sends both", async () => {
    const late = lateAnswer(wire("ticket-1"));
    authorizeAnswers = [late.answer];
    mount();

    // The context has not arrived: a skeleton, and nothing to approve or tick.
    await screen.findByRole("status", { name: "Loading the connection request" });
    expect(screen.queryByTestId("consent-approve")).toBeNull();
    expect(screen.queryByTestId("consent-site-capability")).toBeNull();
    expect(authorizeRequests()).toHaveLength(1);

    // It arrives, and the boxes are ticked.
    late.release();
    await screen.findByTestId("consent-site-capability");
    expect(readBox().checked).toBe(true);
    expect(requestBox().checked).toBe(true);

    submitApproval();
    const body = await approvalBody();
    expect(body.capabilities).toEqual([
      "mcp.sites.read",
      "mcp.ability.read",
      "mcp.ability.request",
    ]);
    expect(body.consent_ticket).toBe("ticket-1");
  });

  it("sends neither site tool when the person clears both before approving", async () => {
    const late = lateAnswer(wire("ticket-1"));
    authorizeAnswers = [late.answer];
    mount();
    await screen.findByRole("status", { name: "Loading the connection request" });
    late.release();
    await screen.findByTestId("consent-site-capability");
    expect(readBox().checked).toBe(true);
    expect(requestBox().checked).toBe(true);

    // Clearing "see what the site can do" clears "ask for changes" with it.
    fireEvent.click(readBox());
    expect(readBox().checked).toBe(false);
    expect(requestBox().checked).toBe(false);

    submitApproval();
    const body = await approvalBody();
    expect(body.capabilities).toEqual(["mcp.sites.read"]);
  });

  it("ticking ask for changes while see what the site can do is clear ticks both, and the approval sends both", async () => {
    const late = lateAnswer(wire("ticket-1"));
    authorizeAnswers = [late.answer];
    mount();
    await screen.findByRole("status", { name: "Loading the connection request" });
    late.release();
    await screen.findByTestId("consent-site-capability");

    // Clear the read, which clears the request; then tick only the request.
    fireEvent.click(readBox());
    expect(readBox().checked).toBe(false);
    expect(requestBox().checked).toBe(false);
    fireEvent.click(requestBox());
    expect(requestBox().checked).toBe(true);
    expect(readBox().checked).toBe(true);

    submitApproval();
    const body = await approvalBody();
    expect(body.capabilities).toEqual([
      "mcp.sites.read",
      "mcp.ability.read",
      "mcp.ability.request",
    ]);
  });

  it("does not call the untouched screen read-only, and says so again once ask for changes is cleared", async () => {
    // With site tools ticked at open, "This connection is read-only." would be
    // untrue on a screen nobody has touched. The paragraph follows the ticks.
    const late = lateAnswer(wire("ticket-1"));
    authorizeAnswers = [late.answer];
    mount();
    await screen.findByRole("status", { name: "Loading the connection request" });
    late.release();
    await screen.findByTestId("consent-site-capability");

    const header = screen.getByRole("heading", { level: 1 }).parentElement!;
    expect(header).toHaveTextContent(
      "Something is asking to read your fleet, and to ask for changes to it, through this dashboard.",
    );
    const cannotChange = () => screen.getByTestId("consent-cannot-change");
    expect(cannotChange()).toHaveTextContent(
      "Beyond reading, the only thing it can do is ask to make changes through the site's tools. That only creates a request, and nothing runs until a person approves it in WPMgr.",
    );
    expect(cannotChange()).not.toHaveTextContent(/read-only/i);

    fireEvent.click(requestBox());
    expect(requestBox().checked).toBe(false);
    expect(cannotChange()).toHaveTextContent("This connection is read-only.");
    expect(cannotChange()).not.toHaveTextContent(/Beyond reading/);
  });

  // OWNER RULING 2026-10-09: A PRESET CHANGES ONLY THE READ ROWS. These go
  // through the real route with all seven reads on offer, so Read everything
  // really widens the reads, and read what the approval sends from the POST body.
  it("opens on Just the basics, not Custom, with both site tools ticked", async () => {
    const late = lateAnswer(wire("ticket-1", { conferrable_capabilities: FULL_OFFER }));
    authorizeAnswers = [late.answer];
    mount();
    await screen.findByRole("status", { name: "Loading the connection request" });
    late.release();
    await screen.findByTestId("consent-site-capability");

    expect(readBox().checked).toBe(true);
    expect(requestBox().checked).toBe(true);
    expect(screen.getByRole("button", { name: "Just the basics", pressed: true })).toBeTruthy();
    expect(screen.queryByTestId("preset-custom")).toBeNull();
    expect(screen.getByText("See which sites are in scope, and no other read.")).toBeTruthy();
    expect(screen.getByTestId("consent-read-capability").textContent ?? "").not.toMatch(
      /your own set|you have changed|clears them/i,
    );
  });

  it("keeps both site tools ticked when Read everything is pressed, and the approval carries them", async () => {
    const late = lateAnswer(wire("ticket-1", { conferrable_capabilities: FULL_OFFER }));
    authorizeAnswers = [late.answer];
    mount();
    await screen.findByRole("status", { name: "Loading the connection request" });
    late.release();
    await screen.findByTestId("consent-site-capability");

    fireEvent.click(screen.getByRole("button", { name: "Read everything" }));
    expect(screen.getByRole("button", { name: "Read everything", pressed: true })).toBeTruthy();
    expect(readBox().checked).toBe(true);
    expect(requestBox().checked).toBe(true);

    submitApproval();
    const body = await approvalBody();
    expect([...(body.capabilities ?? [])].sort()).toEqual(
      [...ALL_READS, "mcp.ability.read", "mcp.ability.request"].sort(),
    );
  });

  it("keeps both site tools ticked when Just the basics is pressed after Read everything", async () => {
    const late = lateAnswer(wire("ticket-1", { conferrable_capabilities: FULL_OFFER }));
    authorizeAnswers = [late.answer];
    mount();
    await screen.findByRole("status", { name: "Loading the connection request" });
    late.release();
    await screen.findByTestId("consent-site-capability");

    fireEvent.click(screen.getByRole("button", { name: "Read everything" }));
    fireEvent.click(screen.getByRole("button", { name: "Just the basics" }));
    expect(screen.getByRole("button", { name: "Just the basics", pressed: true })).toBeTruthy();
    expect(readBox().checked).toBe(true);
    expect(requestBox().checked).toBe(true);

    submitApproval();
    const body = await approvalBody();
    expect([...(body.capabilities ?? [])].sort()).toEqual([
      "mcp.ability.read",
      "mcp.ability.request",
      "mcp.sites.read",
    ]);
  });

  it("does not put back a site tool the person cleared when a shortcut is pressed", async () => {
    const late = lateAnswer(wire("ticket-1", { conferrable_capabilities: FULL_OFFER }));
    authorizeAnswers = [late.answer];
    mount();
    await screen.findByRole("status", { name: "Loading the connection request" });
    late.release();
    await screen.findByTestId("consent-site-capability");

    fireEvent.click(requestBox());
    expect(requestBox().checked).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "Read everything" }));
    expect(requestBox().checked).toBe(false);
    expect(readBox().checked).toBe(true);

    submitApproval();
    const body = await approvalBody();
    expect([...(body.capabilities ?? [])].sort()).toEqual(
      [...ALL_READS, "mcp.ability.read"].sort(),
    );
  });

  it("does not send the request after the read is cleared and ticked again", async () => {
    const late = lateAnswer(wire("ticket-1"));
    authorizeAnswers = [late.answer];
    mount();
    await screen.findByRole("status", { name: "Loading the connection request" });
    late.release();
    await screen.findByTestId("consent-site-capability");

    fireEvent.click(readBox());
    fireEvent.click(readBox());
    expect(readBox().checked).toBe(true);
    expect(requestBox().checked).toBe(false);

    submitApproval();
    const body = await approvalBody();
    expect(body.capabilities).toEqual(["mcp.sites.read", "mcp.ability.read"]);
  });

  it("keeps a cleared box cleared when a refreshed context arrives behind the open screen", async () => {
    // Two authorize answers: the first, late, and the second for the refetch.
    // The second differs in the consent ticket (so the refetched context is a
    // NEW object, which is what a careless "set the ticks when the context
    // changes" would react to) and in the lifetime it states, which is on screen
    // and is how this test knows the screen has really been re-rendered with it.
    // The query cache tells its observers on a later tick than the refetch
    // resolves, so asserting on the ticks before the screen shows the new
    // context would prove nothing.
    const late = lateAnswer(wire("ticket-1"));
    authorizeAnswers = [
      late.answer,
      () => Promise.resolve(jsonResponse(wire("ticket-2", { grant_lifetime_days: 30 }))),
    ];
    const { queryClient } = mount();

    await screen.findByRole("status", { name: "Loading the connection request" });
    late.release();
    await screen.findByTestId("consent-site-capability");
    expect(screen.getByTestId("consent-duration-expiry")).toHaveTextContent("90 days");
    expect(readBox().checked).toBe(true);
    expect(requestBox().checked).toBe(true);

    // The person takes off "ask for changes".
    fireEvent.click(requestBox());
    expect(requestBox().checked).toBe(false);

    // A refreshed context arrives while the screen is open.
    await act(async () => {
      await queryClient.refetchQueries({ queryKey: consentKeys.all });
    });
    expect(authorizeRequests()).toHaveLength(2);
    expect(authorizeAnswers).toHaveLength(0);
    await waitFor(() =>
      expect(screen.getByTestId("consent-duration-expiry")).toHaveTextContent("30 days"),
    );

    // The tick the person took off has not come back, and the other is as it was.
    expect(requestBox().checked).toBe(false);
    expect(readBox().checked).toBe(true);

    // And the approval carries the refreshed ticket, which proves the screen is
    // built on the new context rather than still on the first one.
    submitApproval();
    const body = await approvalBody();
    expect(body.consent_ticket).toBe("ticket-2");
    expect(body.capabilities).toEqual(["mcp.sites.read", "mcp.ability.read"]);
  });
});

describe("/connect/ai, site tools not asked for", () => {
  it("shows no site-tools box and sends the fleet read alone", async () => {
    // The server lists the site-tools capabilities in this payload even though
    // the scopes do not include mcp:site, so only the scope can be keeping them
    // out of the request.
    const late = lateAnswer(wire("ticket-1", { scopes: ["mcp:read"] }));
    authorizeAnswers = [late.answer];
    mount();
    await screen.findByRole("status", { name: "Loading the connection request" });
    late.release();
    await screen.findByTestId("consent-approve");
    expect(screen.queryByTestId("consent-site-capability")).toBeNull();
    expect(screen.queryByTestId("ability-capability-box")).toBeNull();
    // A request for reading alone is described, and worded, as reading alone.
    expect(screen.getByRole("heading", { level: 1 }).parentElement!).toHaveTextContent(
      "Something is asking to read your fleet through this dashboard.",
    );
    expect(screen.getByTestId("consent-cannot-change")).toHaveTextContent(
      "This connection is read-only.",
    );

    submitApproval();
    const body = await approvalBody();
    expect(body.capabilities).toEqual(["mcp.sites.read"]);
  });
});

// A DIFFERENT AUTHORIZE REQUEST IS A DIFFERENT SCREEN. The screen keeps what the
// person ticks and types and works out its opening ticks once, so when the
// browser's back and forward buttons hand the route a context that is already
// cached for another request, the mounted screen must not carry the first
// request's ticks, name or sentences into the second. The second request is put
// in the cache before the move, which is exactly the state back and forward
// leave, and the fetch stub refuses any authorize request it was not told about,
// so a screen that went to the network instead would fail loudly.
describe("/connect/ai, a different authorize request", () => {
  const PARAMS_B = {
    response_type: "code",
    client_id: "c_second",
    redirect_uri: "https://other.example/cb",
    scope: "mcp:read mcp:cache",
    state: "s2",
    code_challenge: "cc2",
    code_challenge_method: "S256",
  };
  const SEARCH_B =
    "response_type=code&client_id=c_second&redirect_uri=https%3A%2F%2Fother.example%2Fcb" +
    "&scope=mcp%3Aread%20mcp%3Acache&state=s2&code_challenge=cc2&code_challenge_method=S256";
  const CACHE_PURGE = { name: "mcp.cache.purge", effect: "request" };

  const cacheBox = () =>
    within(screen.getByTestId("consent-cache-capability")).getByRole<HTMLInputElement>("checkbox");
  const nameInput = () => screen.getByLabelText<HTMLInputElement>("Name this connection");
  const cannotChange = () => screen.getByTestId("consent-cannot-change");

  it("mounts a fresh screen for the second request, and again for the first when the person goes back", async () => {
    const wireA = wire("ticket-a", {
      scopes: ["mcp:read", "mcp:site", "mcp:cache"],
      conferrable_capabilities: [...FULL_OFFER, CACHE_PURGE],
    });
    const wireB = wire("ticket-b", {
      client_id: "c_second",
      client_name_unverified: "Second Client",
      redirect_uri: "https://other.example/cb",
      redirect_host: "other.example",
      scopes: ["mcp:read", "mcp:cache"],
      conferrable_capabilities: [{ name: "mcp.sites.read", effect: "read" }, CACHE_PURGE],
    });
    const late = lateAnswer(wireA);
    authorizeAnswers = [late.answer];
    const { queryClient, router } = mount();
    await screen.findByRole("status", { name: "Loading the connection request" });
    late.release();
    await screen.findByTestId("consent-site-capability");

    // The first screen. It opens with the site tools ticked and the cache clear
    // clear. The person ticks the cache clear and renames the connection.
    expect(nameInput().value).toBe("Route Test Client");
    expect(cacheBox().checked).toBe(false);
    fireEvent.click(cacheBox());
    fireEvent.change(nameInput(), { target: { value: "Renamed on the first screen" } });
    expect(cacheBox().checked).toBe(true);
    expect(cannotChange()).toHaveTextContent(
      "Beyond reading, the only things it can do are ask to clear the site cache and ask to make changes through the site's tools.",
    );

    // The second request, already cached, as it is on the way back and forth.
    queryClient.setQueryData(consentKeys.authorize(PARAMS_B), parseConsentContext(wireB));
    act(() => {
      router.history.push(`/connect/ai?${SEARCH_B}`);
    });
    await waitFor(() =>
      expect(screen.getByTestId("consent-redirect-host")).toHaveTextContent("other.example"),
    );
    expect(authorizeRequests()).toHaveLength(1);

    // Its ticks, its name and its sentences are the second request's own.
    expect(screen.queryByTestId("consent-site-capability")).toBeNull();
    expect(cacheBox().checked).toBe(false);
    expect(nameInput().value).toBe("Second Client");
    expect(cannotChange()).toHaveTextContent("This connection is read-only.");
    expect(cannotChange()).not.toHaveTextContent(/clear the site cache/);

    // And back to the first: a fresh screen again, not the one the person
    // ticked and renamed.
    act(() => {
      router.history.back();
    });
    await waitFor(() =>
      expect(screen.getByTestId("consent-redirect-host")).toHaveTextContent("x.example"),
    );
    expect(authorizeRequests()).toHaveLength(1);
    expect(readBox().checked).toBe(true);
    expect(requestBox().checked).toBe(true);
    expect(cacheBox().checked).toBe(false);
    expect(nameInput().value).toBe("Route Test Client");
    expect(cannotChange()).toHaveTextContent(
      "Beyond reading, the only thing it can do is ask to make changes through the site's tools.",
    );

    // What goes to the server is the first request's own, with no cache clear.
    submitApproval();
    const body = await approvalBody();
    expect(body.client_id).toBe("c_route");
    expect(body.consent_ticket).toBe("ticket-a");
    expect(body.name).toBe("Route Test Client");
    expect([...(body.capabilities ?? [])].sort()).toEqual([
      "mcp.ability.read",
      "mcp.ability.request",
      "mcp.sites.read",
    ]);
  });

  // The over-fire arm of the key, that it is the request and not the consent
  // ticket, is the earlier test in this file where a refreshed context for the
  // SAME request brings a new ticket and the person's cleared tick survives.
});
