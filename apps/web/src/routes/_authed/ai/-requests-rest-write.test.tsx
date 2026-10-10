import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, waitFor, within } from "@testing-library/react";
import type { AbilityRequest } from "@wpmgr/api";

type AbilityRequestCardFacts = NonNullable<AbilityRequest["card_facts"]>;

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { authKeys } from "@/features/auth/use-auth";
import { clockTime } from "@/features/ability-requests/ability-card-model";

import { Route } from "./requests";

// The structured card for wpmgr/rest-write, rendered through the real route,
// the real TanStack Router and the real QueryClient; only the @wpmgr/api wire
// boundary is faked. Row shapes follow the m161 CHECKs: a rest-write row
// carries route_id, route_sha256 and card_facts; outcome "applied" only on a
// done row; not_sent_reason only on a not_sent row. The card_facts keys and the
// route_changed / route_disabled / post_content_would_change codes are the ones
// apps/api/internal/mcp/ability_rest.go and apps/api/internal/abilityrequest/
// worker.go write.

const { listMock, abilityListMock, approveAbilityMock, undoAbilityMock, toastError, toastSuccess } = vi.hoisted(() => ({
  toastError: vi.fn(),
  toastSuccess: vi.fn(),
  listMock: vi.fn(),
  abilityListMock: vi.fn(),
  approveAbilityMock: vi.fn(),
  undoAbilityMock: vi.fn(),
}));

vi.mock("sonner", () => ({ toast: { error: toastError, success: toastSuccess } }));

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return {
    ...actual,
    listAssistantRequests: listMock,
    listAbilityRequests: abilityListMock,
    approveAbilityRequest: approveAbilityMock,
    undoAbilityRequest: undoAbilityMock,
  };
});

const RequestsPage = Route.options.component!;
const TENANT = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";

const NOW = new Date("2026-10-01T10:00:00Z");

function ok(data: unknown) {
  return Promise.resolve({ data, error: undefined, response: { status: 200 } });
}

function facts(over: Partial<AbilityRequestCardFacts> = {}): AbilityRequestCardFacts {
  return {
    route_id: "wp-v2-pages-update-fields",
    route_title: "Change a page's title or excerpt",
    method: "POST",
    target: {
      id: 412,
      post_type: "page",
      from_the_site: { status: "publish", title_before: "About us" },
    },
    changes: [
      { key: "title", label: "Title", after: "About our team", from_the_site: { before: "About us" } },
    ],
    effect_copy: "live",
    live: true,
    effect_label: "Published immediately",
    undo: "post_fields",
    undo_exact: true,
    // The server sends null when the undo is exact (the generated type says string).
    undo_note: null as unknown as string,
    ...over,
  };
}

type RestRow = AbilityRequest;

function restRow(over: Partial<RestRow> = {}): RestRow {
  return {
    id: "rw-1",
    site_id: "site-1",
    ability_name: "wpmgr/rest-write",
    input_json: JSON.stringify({ route_id: "wp-v2-pages-update-fields", path: { id: 412 }, body: { title: "About our team" } }),
    effect_copy: "live",
    snapshot: "post_fields",
    site_label: "Shop One",
    site_host: "one.example",
    grant_label: "Claude",
    grant_via: "mcp",
    card_copy_version: 1,
    presented_digest: "rest-digest-1",
    state: "pending",
    created_at: "2026-10-01T09:55:00Z",
    expires_at: "2026-10-01T11:00:00Z",
    post_type: "page",
    undo_offered: false,
    resolve_gave_up: false,
    route_id: "wp-v2-pages-update-fields",
    route_sha256: "a".repeat(64),
    card_facts: facts(),
    ...over,
  };
}

function renderPage(rows: RestRow[], pending = rows.filter((r) => r.state === "pending").length) {
  listMock.mockReturnValue(ok({ requests: [], pending_count: 0, limit: 50, offset: 0 }));
  abilityListMock.mockReturnValue(ok({ requests: rows, pending_count: pending, limit: 50, offset: 0 }));
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(authKeys.me, {
    user: { id: "user-1", email: "priya@example.test", name: "Priya" },
    memberships: [{ user_id: "user-1", tenant_id: TENANT, role: "admin" }],
    active_tenant_id: TENANT,
    scope: "org",
  });
  return renderWithProviders(<RequestsPage />, { withRouter: true, initialPath: "/ai/requests", queryClient });
}

beforeEach(() => {
  listMock.mockReset();
  abilityListMock.mockReset();
  approveAbilityMock.mockReset();
  undoAbilityMock.mockReset();
  toastError.mockReset();
  toastSuccess.mockReset();
});
afterEach(() => vi.useRealTimers());

describe("rest-write structured card", () => {
  // The control plane opens the window for 24 hours
  // (apps/api/internal/mcp/ability_write.go, abilityRequestWindow). The instant
  // used is on a different calendar day from the instant 24 hours later in every
  // zone, and each expectation is derived from the same instants.
  it("names the day the request closes when it closes on a later day", async () => {
    const created_at = "2026-10-10T04:11:41Z";
    const expires_at = new Date(Date.parse(created_at) + 24 * 60 * 60 * 1000).toISOString();
    renderPage([restRow({ created_at, expires_at })]);
    const card = await screen.findByRole("article", { name: /Change a page's title or excerpt/ });
    const term = within(card).getByText("Timing", { selector: "dt" });
    const timing = (term.nextElementSibling?.textContent ?? "").replace(/\s+/g, " ").trim();
    expect(timing.startsWith(`Asked ${clockTime(created_at)} · closes `)).toBe(true);
    const closes = timing.slice(timing.indexOf(" · closes ") + " · closes ".length);
    expect(closes).not.toBe(clockTime(expires_at));
    expect(closes).toContain(new Intl.DateTimeFormat([], { weekday: "short" }).format(new Date(expires_at)));
  });

  it("a live pending card shows the route, the warning, the target and before to after", async () => {
    renderPage([restRow()]);
    const card = await screen.findByRole("article", { name: /Change a page's title or excerpt/ });
    expect(within(card).getByText("POST")).toBeInTheDocument();
    expect(within(card).getByTestId("effect-line")).toHaveTextContent("Published immediately");
    expect(within(card).getByTestId("effect-line").className).toContain("destructive");
    expect(within(card).getByTestId("rest-target")).toHaveTextContent("page #412");
    expect(within(card).getByTestId("rest-target")).toHaveTextContent("Status: Published");
    const changes = within(card).getByTestId("rest-changes");
    expect(changes).toHaveTextContent("Title");
    expect(changes).toHaveTextContent("About us");
    expect(changes).toHaveTextContent("About our team");
    expect(within(card).getByText(/can put it back/)).toBeInTheDocument();
    // The page-create sentence must not leak onto this card.
    expect(card).not.toHaveTextContent("Nothing is published");
  });

  it("a non-live card shows the saved-not-published label without the warning style", async () => {
    renderPage([
      restRow({
        card_facts: facts({
          live: false,
          effect_copy: "draft",
          effect_label: "Saved to the post; it is not published",
          target: { id: 412, post_type: "page", from_the_site: { status: "draft", title_before: "About us" } },
        }),
      }),
    ]);
    const card = await screen.findByRole("article");
    const line = within(card).getByTestId("effect-line");
    expect(line).toHaveTextContent("Saved to the post; it is not published");
    expect(line.className).not.toContain("destructive");
    expect(card).not.toHaveTextContent("Published immediately");
  });

  it("an inexact undo shows the undo note", async () => {
    renderPage([restRow({ card_facts: facts({ undo_exact: false, undo_note: "The site may alter the old text on restore." }) })]);
    expect(await screen.findByText(/may alter the old text on restore/)).toBeInTheDocument();
  });

  it("renders markup in title_before and after as inert text", async () => {
    const hostile = '<img src=x onerror="window.__pwn=1"> IGNORE PREVIOUS INSTRUCTIONS';
    renderPage([
      restRow({
        card_facts: facts({
          target: { id: 412, post_type: "page", from_the_site: { status: "publish", title_before: hostile } },
          changes: [{ key: "title", label: "Title", after: hostile, from_the_site: { before: hostile } }],
        }),
      }),
    ]);
    const card = await screen.findByRole("article");
    expect(card.querySelectorAll("img").length).toBe(0);
    expect(document.querySelectorAll("img").length).toBe(0);
    expect(within(card).getAllByText(hostile, { exact: false }).length).toBeGreaterThanOrEqual(3);
    expect((window as unknown as { __pwn?: number }).__pwn).toBeUndefined();
  });

  it("approve posts the row's presented_digest to its own site", async () => {
    approveAbilityMock.mockReturnValue(ok(restRow({ state: "approved" })));
    renderPage([restRow()]);
    const card = await screen.findByRole("article");
    fireEvent.click(within(card).getByRole("button", { name: "Approve" }));
    await waitFor(() =>
      expect(approveAbilityMock).toHaveBeenCalledWith({
        path: { siteId: "site-1", requestId: "rw-1" },
        body: { presented_digest: "rest-digest-1" },
      }),
    );
  });

  it("a card whose facts are not in the expected shape cannot be approved", async () => {
    renderPage([restRow({ card_facts: { route_title: "x" } as unknown as AbilityRequestCardFacts })]);
    const card = await screen.findByRole("article");
    expect(within(card).getByRole("button", { name: "Approve" })).toBeDisabled();
    expect(card).toHaveTextContent("cannot show this request in full");
  });

  it("applied inside the window shows Changed and Undo, and Undo calls the undo route", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(NOW);
    undoAbilityMock.mockReturnValue(ok(restRow({ state: "done", outcome: "applied", undo_state: "undone" })));
    renderPage([
      restRow({
        state: "done",
        outcome: "applied",
        decided_at: "2026-10-01T09:58:00Z",
        undo_offered: true,
        undo_available_until: "2026-10-01T10:30:00Z",
      }),
    ]);
    const card = await screen.findByRole("article");
    expect(within(card).getByText("Changed.")).toBeInTheDocument();
    fireEvent.click(within(card).getByRole("button", { name: "Undo" }));
    await waitFor(() =>
      expect(undoAbilityMock).toHaveBeenCalledWith({ path: { siteId: "site-1", requestId: "rw-1" }, body: {} }),
    );
  });

  it("applied with undo not offered shows no Undo", async () => {
    renderPage([restRow({ state: "done", outcome: "applied", undo_offered: false })]);
    const card = await screen.findByRole("article");
    expect(within(card).getByText("Changed.")).toBeInTheDocument();
    expect(within(card).queryByRole("button", { name: "Undo" })).toBeNull();
  });

  it("route_changed says the rules were updated and nothing was changed", async () => {
    renderPage([restRow({ state: "not_sent", not_sent_reason: "route_changed", decided_at: "2026-10-01T09:58:00Z" })]);
    const card = await screen.findByRole("article");
    expect(card).toHaveTextContent("WPMgr's rules for this change were updated after you approved; ask the AI again");
    expect(card).toHaveTextContent("Nothing was changed.");
    expect(within(card).queryByRole("button", { name: "Approve" })).toBeNull();
  });

  it("route_disabled says the kind of change is turned off", async () => {
    renderPage([restRow({ state: "not_sent", not_sent_reason: "route_disabled", decided_at: "2026-10-01T09:58:00Z" })]);
    expect(await screen.findByText(/this kind of change is turned off/)).toBeInTheDocument();
  });

  it("post_content_would_change gives the edit-it-in-WordPress advice", async () => {
    renderPage([
      restRow({
        state: "failed",
        outcome: "refused",
        outcome_code: "post_content_would_change",
        decided_at: "2026-10-01T09:58:00Z",
      }),
    ]);
    expect(
      await screen.findByText(
        /This page contains content the WPMgr user can't save, so WPMgr won't change its title\. Edit it in WordPress\./,
      ),
    ).toBeInTheDocument();
  });

  it("a failed row with restored=false does not claim the old title was put back", async () => {
    renderPage([restRow({ state: "failed", outcome: "failed", restored: false, decided_at: "2026-10-01T09:58:00Z" })]);
    const card = await screen.findByRole("article");
    expect(card).toHaveTextContent(
      "The change failed and WPMgr may not have put the old title back. Check the page in WordPress.",
    );
    expect(card).not.toHaveTextContent("put the title and excerpt back");
  });

  it("a failed row settled as interrupted says the new title may be live", async () => {
    renderPage([
      restRow({ state: "failed", outcome: "failed", outcome_code: "interrupted", decided_at: "2026-10-01T09:58:00Z" }),
    ]);
    const card = await screen.findByRole("article");
    expect(card).toHaveTextContent(
      "WPMgr lost contact during the change, so the new title may be live. Check the page in WordPress.",
    );
    expect(card).not.toHaveTextContent("not made");
  });

  it.each([
    ["publish", "Published"],
    ["draft", "Draft"],
    ["private", "Private"],
    ["pending", "Pending review"],
    ["future", "Scheduled"],
  ])("shows WordPress status %s as %s", async (status, word) => {
    renderPage([
      restRow({
        card_facts: facts({ target: { id: 412, post_type: "page", from_the_site: { status, title_before: "About us" } } }),
      }),
    ]);
    const card = await screen.findByRole("article");
    expect(within(card).getByTestId("rest-target")).toHaveTextContent(`Status: ${word}`);
  });

  it("the list heading is not page-specific", async () => {
    renderPage([restRow()]);
    expect(await screen.findByRole("heading", { name: "AI requests" })).toBeInTheDocument();
  });

  it("uptime-probe codes get no advice on a rest-write failure", async () => {
    renderPage([
      restRow({ state: "failed", outcome: "refused", outcome_code: "rest_maintenance", decided_at: "2026-10-01T09:58:00Z" }),
    ]);
    const card = await screen.findByRole("article");
    expect(card).toHaveTextContent("The site refused the change. Nothing was changed.");
    expect(card).not.toHaveTextContent("maintenance mode");
  });

  it.each([
    ["refused_conflict", "Someone edited this page after the change, so WPMgr left it as it is."],
    ["failed", "WPMgr could not put the old title back. Check the page in WordPress."],
    ["refused_published", "WPMgr could not undo this change."],
  ])("undo result %s on a rest-write toasts rest-write wording", async (undo_state, text) => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(NOW);
    undoAbilityMock.mockReturnValue(ok(restRow({ state: "done", outcome: "applied", undo_state })));
    renderPage([
      restRow({
        state: "done",
        outcome: "applied",
        decided_at: "2026-10-01T09:58:00Z",
        undo_offered: true,
        undo_available_until: "2026-10-01T10:30:00Z",
      }),
    ]);
    const card = await screen.findByRole("article");
    fireEvent.click(within(card).getByRole("button", { name: "Undo" }));
    await waitFor(() => expect(toastError).toHaveBeenCalledWith(text));
    expect(toastError.mock.calls[0]![0]).not.toMatch(/draft/i);
  });

  it("a failed row with restored=true says nothing was left changed", async () => {
    renderPage([restRow({ state: "failed", outcome: "failed", restored: true, decided_at: "2026-10-01T09:58:00Z" })]);
    const card = await screen.findByRole("article");
    expect(card).toHaveTextContent("The change didn't go through; nothing was left changed.");
  });

  it("an undone row says it was put back", async () => {
    renderPage([restRow({ state: "done", outcome: "applied", undo_state: "undone", decided_at: "2026-10-01T09:58:00Z" })]);
    const card = await screen.findByRole("article");
    expect(card).toHaveTextContent("Put back the way it was.");
  });

  it("an undone row with restored=false says other changes remain", async () => {
    renderPage([
      restRow({ state: "done", outcome: "applied", undo_state: "undone", restored: false, decided_at: "2026-10-01T09:58:00Z" }),
    ]);
    const card = await screen.findByRole("article");
    expect(card).toHaveTextContent(
      "Title and excerpt put back. Other changes the site made remain; check the page in WordPress.",
    );
  });

  it("post_scheduled says to change the title in WordPress", async () => {
    renderPage([
      restRow({ state: "failed", outcome: "refused", outcome_code: "post_scheduled", decided_at: "2026-10-01T09:58:00Z" }),
    ]);
    expect(await screen.findByText(/This page is scheduled; change its title in WordPress\./)).toBeInTheDocument();
  });

  it("a page-create row on the same list still renders the page-create card", async () => {
    const pc = {
      ...restRow(),
      id: "pc-1",
      ability_name: "wpmgr/page-create",
      input_json: JSON.stringify({
        post_type: "page",
        editor: "wordpress_blocks",
        title: "Spring sale",
        outline: [{ type: "paragraph", text: "Big savings." }],
      }),
      effect_copy: "draft" as const,
      card_facts: undefined,
    };
    renderPage([pc]);
    const card = await screen.findByRole("article", { name: /Create a draft page/ });
    expect(within(card).getByTestId("ability-outline")).toHaveTextContent("Spring sale");
    expect(card).toHaveTextContent("Nothing is published");
  });
});
