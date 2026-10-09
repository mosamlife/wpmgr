import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, screen, waitFor } from "@testing-library/react";
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

function mount() {
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
    history: createMemoryHistory({ initialEntries: [`/connect/ai?${SEARCH}`] }),
    context: { queryClient },
  });
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  return { queryClient };
}

const readBox = () => screen.getByTestId<HTMLInputElement>("ability-box-mcp.ability.read");
const requestBox = () => screen.getByTestId<HTMLInputElement>("ability-box-mcp.ability.request");

function authorizeRequests(): string[] {
  return fetchMock.mock.calls
    .map(([input]) => urlOf(input))
    .filter((url) => url.startsWith(CONSENT_AUTHORIZE_PATH));
}

/** The JSON body of the one approval POST, read from the request itself. */
async function approvalBody(): Promise<{ capabilities?: string[]; consent_ticket?: string }> {
  await waitFor(() => expect(navigateTo).toHaveBeenCalledTimes(1));
  const posts = fetchMock.mock.calls.filter(([input]) => urlOf(input) === CONSENT_APPROVE_PATH);
  expect(posts).toHaveLength(1);
  const raw = posts[0]![1]?.body;
  if (typeof raw !== "string") throw new Error("the approval body was not a JSON string");
  return JSON.parse(raw) as { capabilities?: string[]; consent_ticket?: string };
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

    fireEvent.click(readBox());
    fireEvent.click(requestBox());
    expect(readBox().checked).toBe(false);
    expect(requestBox().checked).toBe(false);

    submitApproval();
    const body = await approvalBody();
    expect(body.capabilities).toEqual(["mcp.sites.read"]);
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

    submitApproval();
    const body = await approvalBody();
    expect(body.capabilities).toEqual(["mcp.sites.read"]);
  });
});
