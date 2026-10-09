import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, within, fireEvent } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRouteWithContext,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
  useSearch,
} from "@tanstack/react-router";
import type { AbilityRequest, AssistantRequest, Me } from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { assertDbShape, pageCreateRow } from "@/test/ability-request-rows";
import { authKeys } from "@/features/auth/use-auth";
import { abilityRequestKeys } from "@/features/ability-requests/use-ability-requests";
import { DEEP_LINK_MAX_PAGES, useDeepLink } from "@/features/ai-requests/use-deep-link";
import type { RouterContext } from "@/router";

import { Route as AuthedRoute } from "../../_authed";
import { Route } from "./requests";

// THE LINK AN AI HANDS A PERSON (design §7 layer 1, §8.9 "Deep link").
//
// `approval_url` is `<base>/ai/requests?request=<id>` (apps/api/internal/mcp/
// ability_decide.go, approvalURL). These tests open that address on the REAL
// route: the pathless /_authed layout's real beforeLoad (only its component is
// swapped for a bare Outlet, as -connect.ai.signed-out.test.tsx does), the
// requests route attached the way routeTree.gen.ts attaches it, so its own
// validateSearch runs, the real hooks and cards, and the real QueryClient. Only
// the @wpmgr/api wire boundary is replaced. Row shapes come from
// src/test/ability-request-rows.ts, which holds them to the states the database
// allows.
//
// What a link must do: scroll to the named card and focus Decline on it (the
// safe choice), instead of focusing the first waiting card; show a decided or
// closed request with its own status line; say so when nothing the person can
// see matches, or when their role cannot read the requests; and make no claim
// while a queue is still loading or has failed.

const { cacheListMock, abilityListMock, declineMock } = vi.hoisted(() => ({
  cacheListMock: vi.fn(),
  abilityListMock: vi.fn(),
  declineMock: vi.fn(),
}));

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return {
    ...actual,
    listAssistantRequests: cacheListMock,
    listAbilityRequests: abilityListMock,
    declineAbilityRequest: declineMock,
  };
});

type AbilityRequestCardFacts = NonNullable<AbilityRequest["card_facts"]>;

const TENANT = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const ME = {
  user: { id: "user-1", email: "priya@example.test", name: "Priya" },
  memberships: [{ user_id: "user-1", tenant_id: TENANT, role: "admin" }],
  active_tenant_id: TENANT,
  scope: "org",
  role: "admin",
} as unknown as Me;

const uuid = (n: number) => `00000000-0000-4000-8000-${String(n).padStart(12, "0")}`;
const NEWER = uuid(1);
const TARGET = uuid(2);
const MISSING = uuid(999);

// The exact sentences of design §8.9, written out here rather than imported, so
// a drift in the constants fails this file.
const NOT_FOUND_COPY =
  "We could not find that request. It may belong to another organisation. Check which organisation you are signed in to.";
const NO_PERMISSION_COPY = "You can see AI activity but only an operator can decide this request.";

function ok(data: unknown) {
  return Promise.resolve({ data, error: undefined, response: { status: 200 } });
}
function fail(status: number, code: string) {
  return Promise.resolve({ data: undefined, error: { code, message: code }, response: { status } });
}

// The organisation queue the fake serves, newest first, paged by limit/offset.
let abilityRows: AbilityRequest[] = [];

function serveAbility() {
  abilityListMock.mockImplementation(({ query }: { query: { limit: number; offset: number } }) =>
    ok({
      requests: abilityRows.slice(query.offset, query.offset + query.limit),
      pending_count: abilityRows.filter((r) => r.state === "pending").length,
      limit: query.limit,
      offset: query.offset,
    }),
  );
}

function pending(id: string, siteLabel: string): AbilityRequest {
  return pageCreateRow({ id, site_label: siteLabel, site_id: `site-${siteLabel}` });
}

function restFacts(): AbilityRequestCardFacts {
  return {
    route_id: "wp-v2-pages-update-fields",
    route_title: "Change a page's title or excerpt",
    method: "POST",
    target: { id: 412, post_type: "page", from_the_site: { status: "draft", title_before: "About us" } },
    changes: [{ key: "title", label: "Title", after: "About our team", from_the_site: { before: "About us" } }],
    effect_copy: "draft",
    live: false,
    effect_label: "Saved as a draft change",
    undo: "post_fields",
    undo_exact: true,
    undo_note: null as unknown as string,
  };
}

function restWriteRow(over: Partial<AbilityRequest> = {}): AbilityRequest {
  const row: AbilityRequest = {
    id: TARGET,
    site_id: "site-rest",
    ability_name: "wpmgr/rest-write",
    input_json: JSON.stringify({ route_id: "wp-v2-pages-update-fields", path: { id: 412 }, body: { title: "About our team" } }),
    effect_copy: "draft",
    snapshot: "post_fields",
    site_label: "Shop Rest",
    site_host: "rest.example",
    grant_label: "Claude",
    grant_via: "token",
    card_copy_version: 1,
    presented_digest: "d".repeat(64),
    state: "pending",
    created_at: "2026-10-01T09:55:00Z",
    expires_at: "2026-10-01T11:00:00Z",
    post_type: "page",
    undo_offered: false,
    resolve_gave_up: false,
    route_id: "wp-v2-pages-update-fields",
    route_sha256: "a".repeat(64),
    card_facts: restFacts(),
    ...over,
  };
  assertDbShape(row);
  return row;
}

function LoginStub() {
  const search = useSearch({ strict: false });
  return <pre data-testid="login-search">{JSON.stringify(search)}</pre>;
}

function mount(initialPath: string, session: Me | null = ME) {
  const queryClient = createTestQueryClient();
  // Seeded so the guard decides from it: `null` is fetchMe's "no session".
  queryClient.setQueryData(authKeys.me, session);

  const rootRoute = createRootRouteWithContext<RouterContext>()({});
  type AuthedUpdate = Parameters<typeof AuthedRoute.update>[0];
  type RequestsUpdate = Parameters<typeof Route.update>[0];
  const authedRoute = AuthedRoute.update({
    id: "/_authed",
    getParentRoute: () => rootRoute,
    component: Outlet,
  } as unknown as AuthedUpdate);
  const requestsRoute = Route.update({
    id: "/ai/requests",
    path: "/ai/requests",
    getParentRoute: () => authedRoute,
  } as unknown as RequestsUpdate);
  const loginRoute = createRoute({
    path: "/login",
    getParentRoute: () => rootRoute,
    validateSearch: (search: Record<string, unknown>) => search,
    component: LoginStub,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([authedRoute.addChildren([requestsRoute]), loginRoute]),
    history: createMemoryHistory({ initialEntries: [initialPath] }),
    context: { queryClient },
  });
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  return { router, queryClient };
}

let scrollSpy: ReturnType<typeof vi.fn>;

beforeEach(() => {
  vi.clearAllMocks();
  abilityRows = [];
  cacheListMock.mockReset();
  abilityListMock.mockReset();
  declineMock.mockReset();
  cacheListMock.mockReturnValue(ok({ requests: [], pending_count: 0, limit: 50, offset: 0 }));
  serveAbility();
  // jsdom has no scrollIntoView. The spy records which element was scrolled to.
  scrollSpy = vi.fn();
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, writable: true, value: scrollSpy });
});
afterEach(() => {
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

const articleOf = (name: string) => screen.findByRole("article", { name });
const declineIn = (card: HTMLElement) => within(card).getByRole("button", { name: "Decline" });

describe("arriving by the link to a request that waits", () => {
  it("scrolls to the named card and focuses its Decline button, not the first waiting card's", async () => {
    abilityRows = [pending(NEWER, "Shop A"), pending(TARGET, "Shop B")];
    mount(`/ai/requests?request=${TARGET}`);

    const target = await articleOf("Create a draft page · Shop B");
    const other = await articleOf("Create a draft page · Shop A");
    await waitFor(() => expect(declineIn(target)).toHaveFocus());
    expect(declineIn(other)).not.toHaveFocus();
    // Scrolled to the named card, and to no other.
    expect(scrollSpy.mock.contexts).toEqual([target]);
  });

  it("does the same for a structured card (a change to an existing page)", async () => {
    abilityRows = [pending(NEWER, "Shop A"), restWriteRow({ id: TARGET, site_label: "Shop B" })];
    mount(`/ai/requests?request=${TARGET}`);

    const target = await articleOf("Change a page's title or excerpt · Shop B");
    await waitFor(() => expect(declineIn(target)).toHaveFocus());
    expect(scrollSpy.mock.contexts).toEqual([target]);
  });

  it("is the Decline button that acts: pressing it declines the named request and no other", async () => {
    abilityRows = [pending(NEWER, "Shop A"), pending(TARGET, "Shop B")];
    declineMock.mockReturnValue(ok({ ...abilityRows[1]!, state: "declined", decided_at: "2026-10-01T09:58:00Z" }));
    mount(`/ai/requests?request=${TARGET}`);

    const target = await articleOf("Create a draft page · Shop B");
    await waitFor(() => expect(declineIn(target)).toHaveFocus());
    fireEvent.click(document.activeElement as HTMLElement);

    await waitFor(() => expect(declineMock).toHaveBeenCalledTimes(1));
    expect(declineMock).toHaveBeenCalledWith({ path: { siteId: "site-Shop B", requestId: TARGET }, body: {} });
  });

  it("leaves the first waiting card with the default focus when there is no link", async () => {
    // The over-fire arm: the same page without a link behaves as before.
    abilityRows = [pending(NEWER, "Shop A"), pending(TARGET, "Shop B")];
    mount("/ai/requests");

    const first = await articleOf("Create a draft page · Shop A");
    await waitFor(() => expect(declineIn(first)).toHaveFocus());
    expect(scrollSpy).not.toHaveBeenCalled();
    expect(screen.queryByTestId("deep-link-not-found")).not.toBeInTheDocument();
    expect(screen.queryByTestId("deep-link-no-permission")).not.toBeInTheDocument();
  });

  it("acts once: answering the card does not pull the page back to it", async () => {
    abilityRows = [pending(NEWER, "Shop A"), pending(TARGET, "Shop B")];
    const { queryClient } = mount(`/ai/requests?request=${TARGET}`);
    const target = await articleOf("Create a draft page · Shop B");
    await waitFor(() => expect(declineIn(target)).toHaveFocus());
    expect(scrollSpy).toHaveBeenCalledTimes(1);

    // Someone else answers it; the next read of the queue shows it declined.
    abilityRows = [
      abilityRows[0]!,
      { ...abilityRows[1]!, state: "declined", decided_at: "2026-10-01T09:58:00Z" },
    ];
    await queryClient.invalidateQueries({ queryKey: abilityRequestKeys.org() });

    await waitFor(() => expect(within(target).getByText(/^Declined at /)).toBeInTheDocument());
    expect(within(target).queryByRole("button", { name: "Decline" })).not.toBeInTheDocument();
    expect(scrollSpy).toHaveBeenCalledTimes(1);
    expect(target).not.toHaveFocus();
  });
});

describe("arriving by the link to a request already answered or closed", () => {
  it("shows the card's own status line and puts focus on the card, which has nothing to decide", async () => {
    abilityRows = [
      pending(NEWER, "Shop A"),
      pageCreateRow({
        id: TARGET,
        site_label: "Shop B",
        state: "done",
        outcome: "created",
        created_post_id: 7,
        decided_at: "2026-10-01T09:58:00Z",
      }),
    ];
    mount(`/ai/requests?request=${TARGET}`);

    const target = await articleOf("Create a draft page · Shop B");
    await waitFor(() => expect(target).toHaveFocus());
    expect(within(target).getByText("Draft created.")).toBeInTheDocument();
    expect(within(target).queryByRole("button", { name: "Decline" })).not.toBeInTheDocument();
    expect(within(target).queryByRole("button", { name: "Approve" })).not.toBeInTheDocument();
    expect(scrollSpy.mock.contexts).toEqual([target]);
    // The first waiting card does not take the focus from it.
    expect(declineIn(await articleOf("Create a draft page · Shop A"))).not.toHaveFocus();
  });

  it("says a closed request was closed unanswered and that nothing was changed", async () => {
    abilityRows = [restWriteRow({ id: TARGET, site_label: "Shop B", state: "expired" })];
    mount(`/ai/requests?request=${TARGET}`);

    const target = await articleOf("Change a page's title or excerpt · Shop B");
    await waitFor(() => expect(target).toHaveFocus());
    expect(within(target).getByText(/^Closed unanswered at .+\. Nothing was changed\.$/)).toBeInTheDocument();
    expect(within(target).queryByRole("button", { name: "Decline" })).not.toBeInTheDocument();
  });
});

describe("a link that names nothing the person can see", () => {
  it("says it could not find the request, in the design's words, and focuses no card", async () => {
    abilityRows = [pending(NEWER, "Shop A")];
    mount(`/ai/requests?request=${MISSING}`);

    const notice = await screen.findByTestId("deep-link-not-found");
    expect(notice.textContent).toBe(NOT_FOUND_COPY);
    expect(notice).toHaveAttribute("role", "alert");
    expect(screen.queryByTestId("deep-link-no-permission")).not.toBeInTheDocument();
    // Another request's Decline must not be the default focus for a link that
    // named something else.
    const other = await articleOf("Create a draft page · Shop A");
    expect(declineIn(other)).not.toHaveFocus();
    expect(scrollSpy).not.toHaveBeenCalled();
  });

  it("makes no claim while the queue is still being read", async () => {
    abilityListMock.mockReturnValue(new Promise(() => {}));
    mount(`/ai/requests?request=${MISSING}`);

    await screen.findByTestId("org-ability-requests");
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByTestId("deep-link-not-found")).not.toBeInTheDocument();
    expect(screen.queryByTestId("deep-link-no-permission")).not.toBeInTheDocument();
  });

  it("makes no claim when a queue failed to load, which shows its own error", async () => {
    abilityListMock.mockReturnValue(fail(500, "internal"));
    mount(`/ai/requests?request=${MISSING}`);

    expect(await screen.findByText("Could not load AI requests.")).toBeInTheDocument();
    expect(screen.queryByTestId("deep-link-not-found")).not.toBeInTheDocument();
    expect(screen.queryByTestId("deep-link-no-permission")).not.toBeInTheDocument();
  });

  it("says only an operator can decide it when the person's role cannot read the requests", async () => {
    // GET /ai/ability-requests answers 403 without site.content.edit
    // (apps/api/internal/abilityrequest/handler.go, the route's RequirePermission).
    abilityListMock.mockReturnValue(fail(403, "forbidden"));
    mount(`/ai/requests?request=${MISSING}`);

    const notice = await screen.findByTestId("deep-link-no-permission");
    expect(notice.textContent).toBe(NO_PERMISSION_COPY);
    expect(notice).toHaveAttribute("role", "alert");
    expect(screen.queryByTestId("deep-link-not-found")).not.toBeInTheDocument();
  });

  it("still finds a request in the cache-clear queue when the other queue is closed to the person", async () => {
    abilityListMock.mockReturnValue(fail(403, "forbidden"));
    const purge: AssistantRequest = {
      id: TARGET,
      site_id: "site-purge",
      scope: "url",
      url: "https://shop.example/blog/spring-sale/",
      site_label: "Shop Purge",
      site_host: "shop.example",
      grant_label: "Claude",
      grant_via: "oauth",
      setup_client: "Claude Code",
      presented_digest: "digest-abc",
      state: "pending",
      created_at: "2026-09-29T09:41:00Z",
      expires_at: "2026-09-30T09:41:00Z",
      decided_at: null,
      decided_by_user_id: null,
      decided_by_name: null,
      decided_by_account_deleted: false,
      withdrawn_at: null,
      claimed_at: null,
      dispatch_attempts: 0,
      last_attempt_at: null,
      last_attempt_code: null,
      outcome: null,
      not_sent_reason: null,
      outcome_at: null,
      hosting_caches_cleared: null,
      hosting_caches_skipped: null,
      origin_only_confirmed: null,
      wpmgr_cdn: null,
      site_reported_text: null,
    };
    cacheListMock.mockReturnValue(ok({ requests: [purge], pending_count: 1, limit: 50, offset: 0 }));
    mount(`/ai/requests?request=${TARGET}`);

    const card = await screen.findByRole("article", { name: "Clear one page on Shop Purge" });
    await waitFor(() => expect(declineIn(card)).toHaveFocus());
    expect(screen.queryByTestId("deep-link-not-found")).not.toBeInTheDocument();
    expect(screen.queryByTestId("deep-link-no-permission")).not.toBeInTheDocument();
  });
});

describe("a request on a later page", () => {
  it("reads on until it finds the card, then focuses it", async () => {
    // A full first page of answered requests, so the queue has a second page.
    const answered = Array.from({ length: 50 }, (_, i) =>
      pageCreateRow({
        id: uuid(100 + i),
        site_label: `Old ${i}`,
        state: "declined",
        decided_at: "2026-10-01T09:58:00Z",
      }),
    );
    abilityRows = [...answered, pending(TARGET, "Shop B")];
    mount(`/ai/requests?request=${TARGET}`);

    const target = await articleOf("Create a draft page · Shop B");
    await waitFor(() => expect(declineIn(target)).toHaveFocus());
    expect(abilityListMock).toHaveBeenCalledWith({ query: { limit: 50, offset: 50 } });
    expect(screen.queryByTestId("deep-link-not-found")).not.toBeInTheDocument();
  });
});

describe("the search for a request stops", () => {
  function Probe({ id }: { id: string }) {
    return <p data-testid="probe">{useDeepLink(id).kind}</p>;
  }

  it("after the page cap of a queue that never ends, and then says it was not found", async () => {
    // Every page is full and holds nothing named MISSING, for as long as it is asked.
    abilityListMock.mockImplementation(({ query }: { query: { limit: number; offset: number } }) =>
      ok({
        requests: Array.from({ length: query.limit }, (_, i) =>
          pageCreateRow({
            id: uuid(10_000 + query.offset + i),
            state: "declined",
            decided_at: "2026-10-01T09:58:00Z",
          }),
        ),
        pending_count: 0,
        limit: query.limit,
        offset: query.offset,
      }),
    );
    renderWithProviders(<Probe id={MISSING} />);

    await waitFor(() => expect(screen.getByTestId("probe").textContent).toBe("not_found"));
    const pagedReads = abilityListMock.mock.calls.filter(
      ([arg]) => (arg as { query: { limit: number } }).query.limit === 50,
    );
    expect(pagedReads).toHaveLength(DEEP_LINK_MAX_PAGES);
  });
});

describe("a link that is not a short string", () => {
  it.each([
    ["longer than any id", "x".repeat(100)],
    ["a number", "12345"],
  ])("is dropped when it is %s, and the page behaves as with no link", async (_label, value) => {
    abilityRows = [pending(NEWER, "Shop A"), pending(TARGET, "Shop B")];
    mount(`/ai/requests?request=${value}`);

    const first = await articleOf("Create a draft page · Shop A");
    await waitFor(() => expect(declineIn(first)).toHaveFocus());
    expect(screen.queryByTestId("deep-link-not-found")).not.toBeInTheDocument();
    expect(scrollSpy).not.toHaveBeenCalled();
  });
});

describe("a signed-out visitor who opens the link", () => {
  it("is sent to sign in with the whole address, request included, and sees no request", async () => {
    abilityRows = [pending(TARGET, "Shop B")];
    const { router } = mount(`/ai/requests?request=${TARGET}`, null);

    await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
    const search = JSON.parse((await screen.findByTestId("login-search")).textContent ?? "null") as {
      redirect?: string;
    };
    expect(search.redirect).toBeDefined();
    const back = new URL(search.redirect!, "http://app.test");
    expect(back.pathname).toBe("/ai/requests");
    expect(back.searchParams.get("request")).toBe(TARGET);
    // Nothing of the tenant was asked for or shown.
    expect(abilityListMock).not.toHaveBeenCalled();
    expect(screen.queryByRole("article")).not.toBeInTheDocument();
  });

  it("is not sent anywhere when signed in (the over-fire arm of the above)", async () => {
    abilityRows = [pending(TARGET, "Shop B")];
    const { router } = mount(`/ai/requests?request=${TARGET}`);

    await articleOf("Create a draft page · Shop B");
    expect(router.state.location.pathname).toBe("/ai/requests");
  });
});
