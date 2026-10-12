import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, within } from "@testing-library/react";
import type { AbilityRequest, Me } from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { authKeys } from "@/features/auth/use-auth";
import { siteAiMode } from "@/features/ai-trust/ai-trust-fixtures";
import { autoApproved, pageEditRow, personApproved, waitsBecause } from "@/test/ability-request-rows";

import { AiEditingSection } from "./ai-editing-section";

// An AI page edit under approval tiers. A site's setting can approve a page
// edit with no person deciding (the m174 seed classes wpmgr/page-edit as
// ai_draft, apps/api/migrations/20261010020000_m174_approval_tiers.sql), so its
// card has to say what the other two cards say: "Ran automatically" and what
// allowed it, why it waits when it waits, and the wording of a change nobody
// approved by hand. Rendered through the real router, the real QueryClient and
// the real hooks; only the @wpmgr/api wire boundary is faked. The rows are the
// shapes abilityrequest/approval_dto.go returns, held to m174's CHECKs by
// test/ability-request-rows.ts. Every sentence below is written out in full on
// purpose: a test that imported the copy's constant would pass whatever the
// constant said.

const { editingMock, listSiteMock, siteModeMock } = vi.hoisted(() => ({
  editingMock: vi.fn(),
  listSiteMock: vi.fn(),
  siteModeMock: vi.fn(),
}));

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return {
    ...actual,
    getSiteContentEditing: editingMock,
    listSiteAbilityRequests: listSiteMock,
    getSiteAiMode: siteModeMock,
  };
});

function ok(data: unknown) {
  return Promise.resolve({ data, error: undefined, response: { status: 200 } });
}

const ME = {
  user: { id: "user-1", email: "priya@example.test", name: "Priya" },
  memberships: [],
  scope: "org",
  role: "admin",
} as unknown as Me;

function renderTab(rows: AbilityRequest[]) {
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(authKeys.me, ME);
  editingMock.mockReturnValue(ok({ site_id: "site-1", enabled: true }));
  listSiteMock.mockReturnValue(ok({ requests: rows, limit: 50, offset: 0 }));
  siteModeMock.mockReturnValue(ok(siteAiMode("site-1")));
  return renderWithProviders(<AiEditingSection siteId="site-1" siteUrl="https://example.com" canOperate />, {
    withRouter: true,
    initialPath: "/sites/site-1/content",
    queryClient,
  });
}

beforeEach(() => {
  editingMock.mockReset();
  listSiteMock.mockReset();
  siteModeMock.mockReset();
});

const flat = (el: Element | null | undefined) => (el?.textContent ?? "").replace(/\s+/g, " ").trim();

const DECIDED = "2026-10-01T09:58:00Z";
const SET_AT = "2026-10-09T12:00:00Z";
const DAY = 86_400_000;
const inDays = (n: number) => new Date(Date.now() + n * DAY).toISOString();
// The card writes times and dates in the viewer's own locale; the expected
// words are built the same way so the test holds in any.
const timeOf = (iso: string) => new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
const dateOf = (iso: string) => new Date(iso).toLocaleDateString([], { month: "short", day: "numeric" });

/** A page edit a site's setting approved and the site applied, with its undo open. */
function ranAuto(over: Partial<AbilityRequest> = {}): AbilityRequest {
  return pageEditRow({
    state: "done",
    outcome: "applied",
    decided_at: DECIDED,
    undo_state: "available",
    undo_available_until: inDays(13),
    undo_offered: true,
    ...autoApproved(),
    ...over,
  });
}

/** The same edit, approved by a person. */
function ranByPerson(over: Partial<AbilityRequest> = {}): AbilityRequest {
  return ranAuto({ ...personApproved(), ...over });
}

/** A page edit a setting has approved and WPMgr has not sent yet. */
function allowed(over: Partial<AbilityRequest> = {}): AbilityRequest {
  return pageEditRow({ state: "approved", decided_at: DECIDED, ...autoApproved(), ...over });
}

async function openCard(): Promise<HTMLElement> {
  return screen.findByRole("article", { name: /Change a draft in Elementor/ });
}

/** The value of one row of the card's definition list, or null when the card has no such row. */
function rowValue(card: HTMLElement, label: string): string | null {
  const dt = within(card).queryByText(label, { selector: "dt" });
  return dt === null ? null : flat(dt.nextElementSibling);
}

async function statusOf(row: AbilityRequest): Promise<{ card: HTMLElement; line: HTMLElement }> {
  renderTab([row]);
  const card = await openCard();
  return { card, line: within(card).getByTestId("status-line") };
}

// --- a change a setting approved ------------------------------------------------

describe("a page edit a site's setting approved", () => {
  it("says it ran automatically, when it ran and what allowed it", async () => {
    renderTab([ranAuto()]);
    const card = await openCard();
    expect(flat(within(card).getByTestId("ran-automatically"))).toBe("Ran automatically");
    expect(rowValue(card, "Ran")).toBe(timeOf(DECIDED));
    expect(rowValue(card, "Allowed by")).toBe(`Auto for AI drafts, set by you on ${dateOf(SET_AT)}`);
    // It is still a card a person can act on: the edit is undone from here.
    expect(within(card).getByRole("button", { name: "Undo this change" })).toBeEnabled();
  });

  it("names the person who chose the setting, unless that person is the viewer", async () => {
    renderTab([ranAuto(autoApproved({ set_by_user_id: "user-2", set_by_name: "Marcus" }))]);
    const card = await openCard();
    expect(rowValue(card, "Allowed by")).toBe(`Auto for AI drafts, set by Marcus on ${dateOf(SET_AT)}`);
  });

  it("says a default allowed it when nobody chose the setting", async () => {
    renderTab([
      ranAuto(autoApproved({ source: "launch_default", set_by_user_id: null, set_by_name: null })),
    ]);
    const card = await openCard();
    expect(rowValue(card, "Allowed by")).toBe(`Auto for AI drafts, the default since ${dateOf(SET_AT)}`);
  });

  it("does not say it of a change a person approved", async () => {
    renderTab([ranByPerson()]);
    const card = await openCard();
    expect(within(card).queryByTestId("ran-automatically")).toBeNull();
    expect(rowValue(card, "Ran")).toBeNull();
    expect(rowValue(card, "Allowed by")).toBeNull();
    expect(card).not.toHaveTextContent("Ran automatically");
  });

  it("says nothing of it for a request that has not been approved", async () => {
    renderTab([pageEditRow()]);
    const card = await openCard();
    expect(within(card).queryByTestId("ran-automatically")).toBeNull();
    expect(rowValue(card, "Allowed by")).toBeNull();
  });

  it("does not claim it ran while it has only been allowed, but still says what allowed it", async () => {
    const { card, line } = await statusOf(allowed());
    expect(flat(within(card).getByTestId("ran-automatically"))).toBe("Ran automatically");
    expect(rowValue(card, "Ran")).toBeNull();
    expect(rowValue(card, "Allowed by")).toBe(`Auto for AI drafts, set by you on ${dateOf(SET_AT)}`);
    expect(flat(line)).toBe(
      `Allowed by this site's setting at ${timeOf(DECIDED)}. Not started yet. WPMgr sends it to the site shortly.`,
    );
    expect(card).not.toHaveTextContent("Approved at");
  });

  it("still says Approved at for a change a person approved", async () => {
    const { line } = await statusOf(allowed({ ...personApproved() }));
    expect(flat(line)).toBe(`Approved at ${timeOf(DECIDED)}. Not started yet. WPMgr sends it to the site shortly.`);
  });
});

// --- why a change waits ---------------------------------------------------------

describe("a page edit that waits for a person", () => {
  it.each<[string, NonNullable<AbilityRequest["ask_reason"]>, string]>([
    ["the site is set to ask", "site_mode_ask", "This site is set to ask you every time."],
    ["the connection is set to always ask", "connection_never_auto", "This connection is set to always ask."],
    [
      "the site's setting does not run this kind of change",
      "kind_not_in_mode",
      "This site runs changes to the AI's own drafts only when you allow them.",
    ],
    [
      "WPMgr could not check it in time",
      "not_checked",
      "WPMgr could not check this against the site's setting in time, so it waits for you.",
    ],
  ])("says why it waits when %s", async (_name, reason, line) => {
    renderTab([pageEditRow({ ...waitsBecause(reason) })]);
    const card = await openCard();
    expect(flat(within(card).getByTestId("waiting-because"))).toBe(line);
    expect(rowValue(card, "Waiting because")).toBe(line);
    // Deciding it is unchanged: Decline first, then Approve.
    expect(within(card).getByRole("button", { name: "Decline" })).toHaveFocus();
    expect(within(card).getByRole("button", { name: "Approve" })).toBeEnabled();
  });

  it("still reads as waiting for a person when the reason is one this page does not know", async () => {
    // Built past the fixture's shape check on purpose: a reason a later API adds.
    const unknown = "visible_auto_held" as unknown as NonNullable<AbilityRequest["ask_reason"]>;
    renderTab([{ ...pageEditRow({ ...waitsBecause("site_mode_ask") }), ask_reason: unknown }]);
    const card = await openCard();
    expect(rowValue(card, "Waiting because")).toBe("This change waits for you to approve it.");
  });

  it("adds no row when the server recorded no reason", async () => {
    renderTab([pageEditRow()]);
    const card = await openCard();
    expect(rowValue(card, "Waiting because")).toBeNull();
  });

  it("stops saying it once a person has decided, though the server keeps the reason", async () => {
    renderTab([ranByPerson({ ...waitsBecause("site_mode_ask") })]);
    const card = await openCard();
    expect(rowValue(card, "Waiting because")).toBeNull();
  });
});

// --- what the card says when nobody approved it and it did not finish -----------

describe("the result of a page edit a setting approved", () => {
  const UNKNOWN = "WPMgr could not confirm the result. Check the page in WordPress.";

  it("is not claimed when WPMgr could not confirm it, though the page may be linked", async () => {
    const { card, line } = await statusOf(
      pageEditRow({ state: "outcome_unknown", decided_at: DECIDED, ...autoApproved() }),
    );
    expect(flat(line)).toBe(UNKNOWN);
    expect(line).toHaveAttribute("data-tone", "amber");
    expect(within(card).getByRole("link", { name: "Open in Elementor" })).toBeInTheDocument();
  });

  it("keeps a person's own wording for a change a person approved", async () => {
    const { line } = await statusOf(pageEditRow({ state: "outcome_unknown", decided_at: DECIDED, ...personApproved() }));
    expect(flat(line)).toBe(
      "WPMgr could not confirm whether the change was made. Open the page in Elementor and check it.",
    );
  });

  it("is not claimed when it failed and the page may be changed, and the card turns red", async () => {
    const { card, line } = await statusOf(
      pageEditRow({ state: "failed", outcome: "failed", decided_at: DECIDED, ...autoApproved() }),
    );
    expect(flat(line)).toBe(UNKNOWN);
    expect(card).toHaveClass("border-destructive");
  });

  it("keeps its own words when the site refused before it changed anything, and the card still turns red", async () => {
    const { card, line } = await statusOf(
      pageEditRow({
        state: "failed",
        outcome: "refused",
        outcome_code: "conflict",
        outcome_detail: "editor_open",
        decided_at: DECIDED,
        ...autoApproved(),
      }),
    );
    expect(flat(line)).toBe("Someone has this page open in Elementor. Nothing changed.");
    expect(card).toHaveClass("border-destructive");
  });

  it("keeps its own words when WPMgr put the page back", async () => {
    const { card, line } = await statusOf(
      pageEditRow({
        state: "failed",
        outcome: "verify_mismatch",
        outcome_code: "verify_mismatch",
        restored: true,
        decided_at: DECIDED,
        ...autoApproved(),
      }),
    );
    expect(flat(line)).toBe(
      "Elementor did not save the change exactly as approved, so WPMgr put the page back as it was.",
    );
    expect(card).toHaveClass("border-destructive");
  });

  it("keeps a failure of a change a person approved as it was, with no red border", async () => {
    const { card, line } = await statusOf(
      pageEditRow({ state: "failed", outcome: "failed", decided_at: DECIDED, ...personApproved() }),
    );
    expect(flat(line)).toBe(
      "The change was not made because something went wrong on the site. Open the page in Elementor and check it.",
    );
    expect(card).not.toHaveClass("border-destructive");
  });

  it("does not turn a change that went through red", async () => {
    const { card, line } = await statusOf(ranAuto());
    expect(flat(line)).toBe("Draft changed in Elementor.");
    expect(card).not.toHaveClass("border-destructive");
  });

  it("says the setting changed when it did before the change ran", async () => {
    const { line } = await statusOf(
      pageEditRow({
        state: "not_sent",
        outcome: "not_sent",
        not_sent_reason: "setting_changed",
        decided_at: DECIDED,
        ...autoApproved(),
      }),
    );
    expect(flat(line)).toBe("Not started: this site's setting changed before it ran. Nothing was changed.");
  });

  it("says WPMgr changed how this kind of change is handled when it did before the change ran", async () => {
    const { line } = await statusOf(
      pageEditRow({
        state: "not_sent",
        outcome: "not_sent",
        not_sent_reason: "class_changed",
        decided_at: DECIDED,
        ...autoApproved(),
      }),
    );
    expect(flat(line)).toBe(
      "Not started: WPMgr changed how this kind of change is handled before it ran. Nothing was changed.",
    );
  });

  it("keeps the wording of every other reason a change was not sent", async () => {
    const { line } = await statusOf(
      pageEditRow({
        state: "not_sent",
        outcome: "not_sent",
        not_sent_reason: "site_absent",
        decided_at: DECIDED,
        ...autoApproved(),
      }),
    );
    expect(flat(line)).toBe("Nothing was sent: the site left the connection's scope. Nothing was changed.");
  });
});
