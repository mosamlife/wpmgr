import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import type { AbilityRequest, Me } from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";

import { Route as ContentRoute } from "./$siteId.content";

// Field names come from the generated AbilityRequest / ContentEditingState
// (packages/openapi-client/src/generated/types.gen.ts). Error codes asserted
// here are the ones the Go handlers return:
//   ability_request_changed            apps/api/internal/abilityrequest/service.go CodeRequestChanged (409)
//   content_editing_agent_outdated     apps/api/internal/abilityrequest/content_editing.go (409)
//   content_editing_unreachable        same file (503)
// Action keys asserted for audit live in apps/api/internal/audit/audit.go.

const listReqs = vi.fn();
const approveReq = vi.fn();
const declineReq = vi.fn();
const undoReq = vi.fn();
const getEditing = vi.fn();
const enableEditing = vi.fn();
const getInv = vi.fn();

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return {
    ...actual,
    listSiteAbilityRequests: (...a: unknown[]): unknown => listReqs(...a),
    approveAbilityRequest: (...a: unknown[]): unknown => approveReq(...a),
    declineAbilityRequest: (...a: unknown[]): unknown => declineReq(...a),
    undoAbilityRequest: (...a: unknown[]): unknown => undoReq(...a),
    getSiteContentEditing: (...a: unknown[]): unknown => getEditing(...a),
    enableSiteContentEditing: (...a: unknown[]): unknown => enableEditing(...a),
    getSiteContentInventory: (...a: unknown[]): unknown => getInv(...a),
  };
});

const ME_KEY = ["test", "me"] as const;
vi.mock("@/features/auth/use-auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/auth/use-auth")>();
  const { useQuery } = await import("@tanstack/react-query");
  return {
    ...actual,
    useMe: () => useQuery({ queryKey: ME_KEY, queryFn: () => null, staleTime: Infinity }),
  };
});
vi.mock("@/features/sites/use-sites", () => ({
  useSite: () => ({ data: { url: "https://shop.example.com" } }),
}));

const TENANT = "00000000-0000-0000-0000-0000000000aa";
function meWithRole(role: "operator" | "viewer"): Me {
  return {
    user: { id: "u", email: "a@b.test", name: "A" },
    active_tenant_id: TENANT,
    memberships: [{ tenant_id: TENANT, role, tenant_name: "Acme" }],
  } as unknown as Me;
}

const FUTURE = new Date(Date.now() + 10 * 60_000).toISOString();
const PAST = new Date(Date.now() - 10 * 60_000).toISOString();

function input(over: Record<string, unknown> = {}): string {
  return JSON.stringify({
    post_type: "page",
    editor: "wordpress_blocks",
    title: "Spring sale",
    outline: [
      { type: "heading", level: 2, text: "Why now" },
      { type: "paragraph", text: "Big savings." },
      { type: "list", ordered: false, items: ["One", "Two"] },
    ],
    ...over,
  });
}

function req(over: Partial<AbilityRequest> = {}): AbilityRequest {
  return {
    id: "r-1",
    site_id: "site-1",
    ability_name: "wpmgr/page-create",
    input_json: input(),
    title_excerpt: "Spring sale",
    editor: "wordpress_blocks",
    post_type: "page",
    effect_copy: "draft",
    snapshot: "{}",
    site_label: "Shop",
    site_host: "shop.example.com",
    grant_label: "Claude on laptop",
    grant_via: "token",
    setup_client: null,
    card_copy_version: 1,
    presented_digest: "digest-1",
    state: "pending",
    created_at: "2026-09-30T09:00:00Z",
    expires_at: "2026-09-30T10:00:00Z",
    decided_at: null,
    outcome: null,
    outcome_code: null,
    not_sent_reason: null,
    created_post_id: null,
    trashed: null,
    undo_state: null,
    undo_available_until: null,
    ...over,
  };
}

function okList(requests: AbilityRequest[]) {
  return {
    data: { requests, limit: 50, offset: 0 },
    error: undefined,
    response: { status: 200 },
  };
}
function okEditing(enabled: boolean) {
  return {
    data: { site_id: "site-1", enabled },
    error: undefined,
    response: { status: 200 },
  };
}
function fail(status: number, code: string, message: string) {
  return { data: undefined, error: { code, message }, response: { status } };
}

function renderTab(role: "operator" | "viewer" = "operator") {
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(ME_KEY, meWithRole(role));
  const rootRoute = createRootRoute({});
  type UpdateOptions = Parameters<typeof ContentRoute.update>[0];
  const contentRoute = ContentRoute.update({
    id: "/sites/$siteId/content",
    path: "/sites/$siteId/content",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const router = createRouter({
    routeTree: rootRoute.addChildren([contentRoute]),
    history: createMemoryHistory({ initialEntries: ["/sites/site-1/content"] }),
  });
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
}

beforeEach(() => {
  for (const m of [listReqs, approveReq, declineReq, undoReq, getEditing, enableEditing, getInv]) {
    m.mockReset();
  }
  getEditing.mockResolvedValue(okEditing(true));
  listReqs.mockResolvedValue(okList([]));
  getInv.mockResolvedValue({
    data: {
      state: "ok",
      min_agent_version: "0.62.0",
      last_checked_at: null,
      titles_included: true,
      truncated: false,
      next_after_post_id: null,
      pages: [],
    },
    error: undefined,
    response: { status: 200 },
  });
});

async function card() {
  return await screen.findByRole("article", { name: /Create a draft page/ });
}

describe("AI page-creation approval card", () => {
  it("shows the title, editor, nothing-published line, requester and expiry", async () => {
    listReqs.mockResolvedValue(okList([req()]));
    renderTab();
    const c = within(await card());
    expect(c.getByText("Create a draft page · Shop")).toBeInTheDocument();
    expect(c.getByText("WordPress block editor")).toBeInTheDocument();
    expect(c.getByText("Nothing is published. Undo moves the draft to the trash.")).toBeInTheDocument();
    expect(c.getByText("Claude on laptop")).toBeInTheDocument();
    expect(c.getByText(/closes /)).toBeInTheDocument();
    expect(c.getByText("Chosen by the AI")).toBeInTheDocument();
    expect(c.getByText("From the site")).toBeInTheDocument();
  });

  it("names the classic editor", async () => {
    listReqs.mockResolvedValue(
      okList([req({ editor: "wordpress_classic", input_json: input({ editor: "wordpress_classic" }) })]),
    );
    renderTab();
    expect(within(await card()).getByText("Classic editor")).toBeInTheDocument();
  });

  it("renders the outline as text: a <b> in the AI text shows literally and creates no element", async () => {
    listReqs.mockResolvedValue(
      okList([
        req({
          input_json: input({
            title: "<b>Bold title</b>",
            outline: [
              { type: "heading", level: 2, text: "<b>Loud</b> heading" },
              { type: "paragraph", text: "Hello <b>world</b><img src=x onerror=alert(1)>" },
              { type: "list", ordered: true, items: ["<i>first</i>"] },
            ],
          }),
        }),
      ]),
    );
    renderTab();
    const preview = within(await card()).getByTestId("ability-outline");
    expect(preview.textContent).toContain("<b>Bold title</b>");
    expect(preview.textContent).toContain("<b>Loud</b> heading");
    expect(preview.textContent).toContain("Hello <b>world</b><img src=x onerror=alert(1)>");
    expect(preview.textContent).toContain("<i>first</i>");
    expect(preview.querySelector("b, i, img")).toBeNull();
  });

  it("focuses Decline, and Approve sends the presented digest", async () => {
    listReqs.mockResolvedValue(okList([req()]));
    approveReq.mockResolvedValue({ data: req({ state: "approved" }), error: undefined, response: { status: 200 } });
    renderTab();
    const c = within(await card());
    expect(c.getByRole("button", { name: "Decline" })).toHaveFocus();
    fireEvent.click(c.getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(approveReq).toHaveBeenCalledTimes(1));
    expect(approveReq).toHaveBeenCalledWith({
      path: { siteId: "site-1", requestId: "r-1" },
      body: { presented_digest: "digest-1" },
    });
  });

  it("says 'This request changed. Ask the AI again.' on a 409 ability_request_changed", async () => {
    listReqs.mockResolvedValue(okList([req()]));
    approveReq.mockResolvedValue(
      fail(409, "ability_request_changed", "This request changed or was decided while you were reading it. Nothing ran."),
    );
    renderTab();
    const c = within(await card());
    fireEvent.click(c.getByRole("button", { name: "Approve" }));
    expect(await c.findByRole("alert")).toHaveTextContent("This request changed. Ask the AI again.");
  });

  it("disables Approve when the input cannot be shown in full", async () => {
    listReqs.mockResolvedValue(okList([req({ input_json: "{not json" })]));
    renderTab();
    const c = within(await card());
    expect(c.getByRole("button", { name: "Approve" })).toBeDisabled();
    expect(c.getByText(/cannot show this request in full/)).toBeInTheDocument();
  });

  it("decline posts an empty body", async () => {
    listReqs.mockResolvedValue(okList([req()]));
    declineReq.mockResolvedValue({ data: req({ state: "declined" }), error: undefined, response: { status: 200 } });
    renderTab();
    fireEvent.click(within(await card()).getByRole("button", { name: "Decline" }));
    await waitFor(() => expect(declineReq).toHaveBeenCalledTimes(1));
    expect(declineReq).toHaveBeenCalledWith({ path: { siteId: "site-1", requestId: "r-1" }, body: {} });
  });
});

describe("card states", () => {
  const cases: Array<[string, Partial<AbilityRequest>, RegExp]> = [
    ["approved, not started", { state: "approved", decided_at: "2026-09-30T09:05:00Z" }, /Not started yet/],
    ["sent, no outcome yet", { state: "dispatched" }, /WPMgr is creating the draft page/],
    ["outcome unknown", { state: "outcome_unknown" }, /WPMgr is checking whether the draft was created/],
    ["undone", { state: "done", outcome: "created", created_post_id: 7, undo_state: "undone", trashed: true }, /Moved to the trash/],
    ["undo refused, published", { state: "done", outcome: "created", created_post_id: 7, undo_state: "refused_published" }, /has been published since/],
    ["undo refused, edited", { state: "done", outcome: "created", created_post_id: 7, undo_state: "refused_conflict" }, /was edited since/],
    ["undo failed", { state: "done", outcome: "created", created_post_id: 7, undo_state: "failed" }, /could not move the draft to the trash/],
    ["failed, site refused", { state: "failed", outcome: "refused", outcome_code: "created_post_published" }, /The site refused to create the draft page/],
    ["failed, mismatch", { state: "failed", outcome: "verify_mismatch" }, /did not match what you approved/],
    ["failed, other", { state: "failed", outcome: "failed" }, /something went wrong on the site/],
    ["not sent", { state: "not_sent", outcome: "not_sent", not_sent_reason: "agent_outdated" }, /Nothing was sent: the WPMgr plugin on the site is too old/],
    ["declined", { state: "declined", decided_at: "2026-09-30T09:05:00Z" }, /Declined at .* Nothing was created/],
    ["withdrawn", { state: "withdrawn" }, /Withdrawn because its connection was revoked/],
    ["expired", { state: "expired" }, /Closed unanswered at .* Nothing was created/],
  ];
  it.each(cases)("%s", async (_name, over, text) => {
    listReqs.mockResolvedValue(okList([req(over)]));
    renderTab();
    const c = within(await card());
    expect(c.getByText(text)).toBeInTheDocument();
    expect(c.queryByRole("button", { name: "Approve" })).toBeNull();
  });

  it("shows the site's own failure text as a text node", async () => {
    listReqs.mockResolvedValue(
      okList([req({ state: "failed", outcome: "refused", outcome_code: "<b>nope</b>" })]),
    );
    renderTab();
    const c = within(await card());
    expect(c.getByText("The site said: <b>nope</b>")).toBeInTheDocument();
  });

  it("done: 'Draft created', an edit link when the site address is known, and Undo inside the window", async () => {
    listReqs.mockResolvedValue(
      okList([req({ state: "done", outcome: "created", created_post_id: 42, undo_available_until: FUTURE })]),
    );
    undoReq.mockResolvedValue({
      data: req({ state: "done", outcome: "created", created_post_id: 42, undo_state: "undone", trashed: true }),
      error: undefined,
      response: { status: 200 },
    });
    renderTab();
    const c = within(await card());
    expect(c.getByText("Draft created.")).toBeInTheDocument();
    const link = c.getByRole("link", { name: "Edit the draft in WordPress" });
    expect(link).toHaveAttribute("href", "https://shop.example.com/wp-admin/post.php?post=42&action=edit");
    fireEvent.click(c.getByRole("button", { name: "Undo" }));
    await waitFor(() => expect(undoReq).toHaveBeenCalledTimes(1));
    expect(undoReq).toHaveBeenCalledWith({ path: { siteId: "site-1", requestId: "r-1" }, body: {} });
  });

  it("done: no Undo once the window has passed", async () => {
    listReqs.mockResolvedValue(
      okList([req({ state: "done", outcome: "created", created_post_id: 42, undo_available_until: PAST })]),
    );
    renderTab();
    const c = within(await card());
    expect(c.getByText("Draft created.")).toBeInTheDocument();
    expect(c.queryByRole("button", { name: "Undo" })).toBeNull();
  });

  it("lists a waiting request above decided ones", async () => {
    listReqs.mockResolvedValue(
      okList([
        req({ id: "r-old", state: "declined", site_label: "Old" }),
        req({ id: "r-new", state: "pending", site_label: "Waiting" }),
      ]),
    );
    renderTab();
    await screen.findByRole("article", { name: /Waiting/ });
    const articles = screen.getAllByRole("article");
    expect(articles[0]).toHaveAccessibleName(/Waiting/);
  });
});

describe("AI editing switch", () => {
  it("off: says so and offers Turn on to an operator", async () => {
    getEditing.mockResolvedValue(okEditing(false));
    renderTab();
    expect(await screen.findByText("AI page creation is off for this site.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Turn on" })).toBeInTheDocument();
  });

  it("off: no Turn on for a viewer", async () => {
    getEditing.mockResolvedValue(okEditing(false));
    renderTab("viewer");
    expect(await screen.findByText("AI page creation is off for this site.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Turn on" })).toBeNull();
    expect(listReqs).not.toHaveBeenCalled();
  });

  it("Turn on calls enable and shows the on state", async () => {
    getEditing.mockResolvedValue(okEditing(false));
    enableEditing.mockImplementation(() => {
      getEditing.mockResolvedValue(okEditing(true));
      return Promise.resolve(okEditing(true));
    });
    renderTab();
    fireEvent.click(await screen.findByRole("button", { name: "Turn on" }));
    await waitFor(() => expect(enableEditing).toHaveBeenCalledWith({ path: { siteId: "site-1" }, body: {} }));
    expect(await screen.findByText(/AI page creation is on/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Turn on" })).toBeNull();
  });

  it("409 content_editing_agent_outdated asks for the plugin update", async () => {
    getEditing.mockResolvedValue(okEditing(false));
    enableEditing.mockResolvedValue(
      fail(409, "content_editing_agent_outdated", "This site's WPMgr agent must be updated"),
    );
    renderTab();
    fireEvent.click(await screen.findByRole("button", { name: "Turn on" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Update the WPMgr plugin to 0.61.156 or later",
    );
  });

  it("503 says the site could not be reached", async () => {
    getEditing.mockResolvedValue(okEditing(false));
    enableEditing.mockResolvedValue(
      fail(503, "content_editing_unreachable", "The site could not be reached"),
    );
    renderTab();
    fireEvent.click(await screen.findByRole("button", { name: "Turn on" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("could not reach this site");
  });
});
